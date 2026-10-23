package server

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ===========================================================================
// 1. WorkBuddy 专用通道配置与数据结构
// ===========================================================================

// WorkBuddyConfig 定义 WorkBuddy 国际版专用通道适配配置
type WorkBuddyConfig struct {
	Enabled      bool              `json:"enabled"`
	Mode         string            `json:"mode"`          // "workbuddy", "direct", "auto"
	SessionToken string            `json:"session_token"` // 从网页端或客户端提取的 JWT / Session Token
	Cookie       string            `json:"cookie"`        // 从网页端提取的完整 Cookie 字符串
	ChannelID    string            `json:"channel_id"`    // 上游内部通道 ID，默认 "intl-desktop-v2"
	DeviceID     string            `json:"device_id"`     // 设备唯一标识指纹
	CustomOrigin string            `json:"custom_origin"` // 伪装的 Origin/Referer 地址
	UserAgent    string            `json:"user_agent"`    // 客户端伪装 UA
	UpstreamPath string            `json:"upstream_path"` // 自定义上游接口路径，为空时透传
	WrapMode     string            `json:"wrap_mode"`     // 载荷包装策略: "inject" (注入字段), "envelope" (外层包裹), "openai" (OpenAI兼容)
	ExtraHeaders map[string]string `json:"extra_headers"` // 额外自定义伪装请求头
}

// DefaultWorkBuddyConfig 返回生产级 WorkBuddy 默认配置
func DefaultWorkBuddyConfig() WorkBuddyConfig {
	return WorkBuddyConfig{
		Enabled:      false,
		Mode:         "workbuddy",
		ChannelID:    "intl-desktop-v2",
		CustomOrigin: "https://intl.workbuddy.ai",
		UserAgent:    "WorkBuddy-Client/2.4.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36",
		WrapMode:     "inject",
		ExtraHeaders: make(map[string]string),
	}
}

// ChannelAdapter 通道适配器抽象接口（参考 dsh-our-free-model 多通道规范）
type ChannelAdapter interface {
	Name() string
	IsEnabled() bool
	Match(req *http.Request) bool
	TransformRequest(req *http.Request, rawBody []byte, cfg *Config) ([]byte, error)
	TransformHeaders(req *http.Request, cfg *Config)
	TransformSSE(ctx context.Context, src io.Reader, dst io.Writer, flusher http.Flusher) error
}

// ===========================================================================
// 2. WorkBuddyAdapter 核心实现
// ===========================================================================

// WorkBuddyAdapter 实现 ChannelAdapter 接口，处理 WorkBuddy 国际版专用网关双向转换
type WorkBuddyAdapter struct {
	wbCfg WorkBuddyConfig
	mu    sync.RWMutex
}

// NewWorkBuddyAdapter 创建适配器实例
func NewWorkBuddyAdapter(cfg WorkBuddyConfig) *WorkBuddyAdapter {
	if cfg.ChannelID == "" {
		cfg.ChannelID = "intl-desktop-v2"
	}
	if cfg.CustomOrigin == "" {
		cfg.CustomOrigin = "https://intl.workbuddy.ai"
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = "WorkBuddy-Client/2.4.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36"
	}
	if cfg.WrapMode == "" {
		cfg.WrapMode = "inject"
	}
	return &WorkBuddyAdapter{
		wbCfg: cfg,
	}
}

// Name 返回适配器名称
func (a *WorkBuddyAdapter) Name() string {
	return "WorkBuddy-Intl-Adapter"
}

// IsEnabled 检查通道是否启用
func (a *WorkBuddyAdapter) IsEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.wbCfg.Enabled
}

// Match 判断当前 HTTP 请求是否归属本通道处理
func (a *WorkBuddyAdapter) Match(req *http.Request) bool {
	if !a.IsEnabled() {
		return false
	}
	p := strings.TrimSuffix(req.URL.Path, "/")
	return p == "/v1/messages" || p == "/messages" || p == "/v1/messages/count_tokens"
}

// generateDeviceID 若未配置设备 ID 则生成拟真的设备十六进制指纹
func generateDeviceID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// generateSessionID 生成会话 UUID v4
func generateSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40 // Version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// TransformRequest 将客户端的标准 Anthropic 请求转换为 WorkBuddy 内部网关格式
func (a *WorkBuddyAdapter) TransformRequest(req *http.Request, rawBody []byte, cfg *Config) ([]byte, error) {
	a.mu.RLock()
	wb := a.wbCfg
	a.mu.RUnlock()

	if len(rawBody) == 0 {
		return rawBody, nil
	}

	// 1. 解构标准 Anthropic 请求体
	var parsed map[string]interface{}
	if err := json.Unmarshal(rawBody, &parsed); err != nil {
		return nil, fmt.Errorf("解析客户端 Anthropic 请求体失败: %w", err)
	}

	// 2. 模型定点重映射 (例如 claude-3-7-sonnet -> workbuddy-claude-3-7-sonnet)
	if m, ok := parsed["model"].(string); ok && cfg != nil && len(cfg.ModelMapping) > 0 {
		if mapped, exists := cfg.ModelMapping[m]; exists && mapped != "" {
			parsed["model"] = mapped
		}
	}

	// 3. 准备会话特征元数据
	deviceID := wb.DeviceID
	if deviceID == "" {
		deviceID = generateDeviceID()
	}
	sessionID := generateSessionID()

	// 4. 根据 WrapMode 执行载荷转换
	switch strings.ToLower(wb.WrapMode) {
	case "envelope":
		// Case A: 外层 RPC 包装格式 (常见于前端桌面客户端与微服务网关交互)
		envelope := map[string]interface{}{
			"session_id":        sessionID,
			"channel_id":        wb.ChannelID,
			"device_id":         deviceID,
			"client_version":    "2.4.0",
			"timestamp":         time.Now().UnixMilli(),
			"request_payload":   parsed,
			"protocol_standard": "anthropic_v1",
		}
		return json.Marshal(envelope)

	case "openai":
		// Case B: 上游网关运行 OpenAI 兼容协议（类似 dsh-our-free-model 的 chat/completions 桥接）
		openAIPayload := map[string]interface{}{
			"model":       parsed["model"],
			"stream":      parsed["stream"],
			"max_tokens":  parsed["max_tokens"],
			"temperature": parsed["temperature"],
			"messages":    convertAnthropicMessagesToOpenAI(parsed),
		}
		if sID, ok := parsed["session_id"].(string); ok && sID != "" {
			openAIPayload["session_id"] = sID
		} else {
			openAIPayload["session_id"] = sessionID
		}
		return json.Marshal(openAIPayload)

	case "inject":
		fallthrough
	default:
		// Case C: 顶层无缝字段注入（保持标准 Anthropic 字段，追加 WorkBuddy 上游网关审计/路由字段）
		parsed["session_id"] = sessionID
		parsed["channel_id"] = wb.ChannelID
		parsed["device_id"] = deviceID
		if _, ok := parsed["client_version"]; !ok {
			parsed["client_version"] = "2.4.0"
		}
		return json.Marshal(parsed)
	}
}

// convertAnthropicMessagesToOpenAI 将 Anthropic 消息结构转换为 OpenAI 兼容列表
func convertAnthropicMessagesToOpenAI(anthropicReq map[string]interface{}) []map[string]interface{} {
	var result []map[string]interface{}

	// 处理 system prompt
	if sys, ok := anthropicReq["system"].(string); ok && strings.TrimSpace(sys) != "" {
		result = append(result, map[string]interface{}{
			"role":    "system",
			"content": sys,
		})
	}

	// 处理 messages 列表
	if rawMsgs, ok := anthropicReq["messages"].([]interface{}); ok {
		for _, m := range rawMsgs {
			if msgMap, ok := m.(map[string]interface{}); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// 如果 content 是 []interface{} 块结构，拼接纯文本
				if blocks, ok := content.([]interface{}); ok {
					var textParts []string
					for _, b := range blocks {
						if bMap, ok := b.(map[string]interface{}); ok {
							if t, _ := bMap["type"].(string); t == "text" {
								if textVal, ok := bMap["text"].(string); ok {
									textParts = append(textParts, textVal)
								}
							}
						}
					}
					content = strings.Join(textParts, "\n")
				}

				result = append(result, map[string]interface{}{
					"role":    role,
					"content": content,
				})
			}
		}
	}
	return result
}

// TransformHeaders 清洗客户端代理特征，注入 WorkBuddy 账号真实鉴权凭据与浏览器指纹
func (a *WorkBuddyAdapter) TransformHeaders(req *http.Request, cfg *Config) {
	a.mu.RLock()
	wb := a.wbCfg
	a.mu.RUnlock()

	// 1. 彻底清除入站鉴权与反代外露头（防止泄露给 WorkBuddy 上游或被风控判定为代理）
	req.Header.Del("x-api-key")
	req.Header.Del("Authorization")
	req.Header.Del("X-Forwarded-For")
	req.Header.Del("X-Forwarded-Host")
	req.Header.Del("X-Forwarded-Proto")
	req.Header.Del("X-Real-IP")
	req.Header.Del("Via")
	req.Header.Del("Client-IP")
	req.Header.Del("CF-Connecting-IP")
	req.Header.Del("True-Client-IP")

	// 2. 注入提取出的 WorkBuddy 账号凭据 (Session Token / JWT)
	if strings.TrimSpace(wb.SessionToken) != "" {
		token := strings.TrimSpace(wb.SessionToken)
		if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Set("Authorization", token)
		}
		// 部分内部上游会校验专有头
		req.Header.Set("x-session-token", token)
	} else if cfg != nil && cfg.APIKey != "" {
		// 回退使用通用配置的 API Key
		req.Header.Set("x-api-key", cfg.APIKey)
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	// 3. 注入提取出的 Cookie 串
	if strings.TrimSpace(wb.Cookie) != "" {
		req.Header.Set("Cookie", strings.TrimSpace(wb.Cookie))
	}

	// 4. 伪装 WorkBuddy 官方渠道元数据
	channelID := wb.ChannelID
	if channelID == "" {
		channelID = "intl-desktop-v2"
	}
	req.Header.Set("x-workbuddy-channel", channelID)
	req.Header.Set("x-workbuddy-client", "intl-desktop")
	req.Header.Set("x-workbuddy-version", "2.4.0")

	deviceID := wb.DeviceID
	if deviceID == "" {
		deviceID = generateDeviceID()
	}
	req.Header.Set("x-workbuddy-device-id", deviceID)

	// 5. 伪装 Origin 与 Referer 防御跨域/来源检查
	origin := wb.CustomOrigin
	if origin == "" {
		origin = "https://intl.workbuddy.ai"
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")

	// 6. 伪装原生客户端与高版本浏览器 User-Agent 指纹
	ua := wb.UserAgent
	if ua == "" {
		ua = "WorkBuddy-Client/2.4.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/130.0.0.0 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("sec-ch-ua", `"Chromium";v="130", "WorkBuddy";v="2", "Not?A_Brand";v="99"`)
	req.Header.Set("sec-ch-ua-mobile", "?0")
	req.Header.Set("sec-ch-ua-platform", `"Windows"`)
	req.Header.Set("sec-fetch-dest", "empty")
	req.Header.Set("sec-fetch-mode", "cors")
	req.Header.Set("sec-fetch-site", "same-site")

	// 7. 注入额外用户自定义伪装头
	for k, v := range wb.ExtraHeaders {
		if strings.TrimSpace(k) != "" && strings.TrimSpace(v) != "" {
			req.Header.Set(k, v)
		}
	}
}

// ===========================================================================
// 3. 高性能响应流双向转换 (SSE Bridge / Transcoder)
// ===========================================================================

// SSEBridgeState 维护转换 Anthropic 标准流的状态机
type SSEBridgeState struct {
	messageID    string
	model        string
	hasSentStart bool
	hasSentBlock bool
	hasSentStop  bool
	blockIndex   int
	outputTokens int
}

// NewSSEBridgeState 创建流式状态机
func NewSSEBridgeState(model string) *SSEBridgeState {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	msgID := "msg_wb_" + hex.EncodeToString(b)
	if model == "" {
		model = "claude-3-7-sonnet"
	}
	return &SSEBridgeState{
		messageID:  msgID,
		model:      model,
		blockIndex: 0,
	}
}

// emitMessageStart 发射 message_start 事件
func (s *SSEBridgeState) emitMessageStart(dst io.Writer) error {
	if s.hasSentStart {
		return nil
	}
	s.hasSentStart = true

	payload := map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            s.messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         s.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}
	data, _ := json.Marshal(payload)
	_, err := fmt.Fprintf(dst, "event: message_start\ndata: %s\n\n", data)
	return err
}

// emitContentBlockStart 发射 content_block_start 事件
func (s *SSEBridgeState) emitContentBlockStart(dst io.Writer) error {
	if s.hasSentBlock {
		return nil
	}
	s.hasSentBlock = true

	payload := map[string]interface{}{
		"type":  "content_block_start",
		"index": s.blockIndex,
		"content_block": map[string]interface{}{
			"type": "text",
			"text": "",
		},
	}
	data, _ := json.Marshal(payload)
	_, err := fmt.Fprintf(dst, "event: content_block_start\ndata: %s\n\n", data)
	return err
}

// emitContentBlockDelta 发射文本增量事件
func (s *SSEBridgeState) emitContentBlockDelta(dst io.Writer, text string) error {
	if text == "" {
		return nil
	}
	s.outputTokens += (len(text) + 3) / 4 // 简易粗略 Token 估算

	payload := map[string]interface{}{
		"type":  "content_block_delta",
		"index": s.blockIndex,
		"delta": map[string]interface{}{
			"type": "text_delta",
			"text": text,
		},
	}
	data, _ := json.Marshal(payload)
	_, err := fmt.Fprintf(dst, "event: content_block_delta\ndata: %s\n\n", data)
	return err
}

// emitStopSequence 发射流结束序列 (content_block_stop -> message_delta -> message_stop)
func (s *SSEBridgeState) emitStopSequence(dst io.Writer) error {
	if s.hasSentStop {
		return nil
	}
	s.hasSentStop = true

	// 1. content_block_stop
	blockStop, _ := json.Marshal(map[string]interface{}{
		"type":  "content_block_stop",
		"index": s.blockIndex,
	})
	if _, err := fmt.Fprintf(dst, "event: content_block_stop\ndata: %s\n\n", blockStop); err != nil {
		return err
	}

	// 2. message_delta
	msgDelta, _ := json.Marshal(map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   "end_turn",
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": s.outputTokens,
		},
	})
	if _, err := fmt.Fprintf(dst, "event: message_delta\ndata: %s\n\n", msgDelta); err != nil {
		return err
	}

	// 3. message_stop
	msgStop, _ := json.Marshal(map[string]interface{}{
		"type": "message_stop",
	})
	if _, err := fmt.Fprintf(dst, "event: message_stop\ndata: %s\n\n", msgStop); err != nil {
		return err
	}

	return nil
}

// TransformSSE 双向流式转换器：高效拦截非纯净上游 SSE，转换为标准 Anthropic SSE 事件流
func (a *WorkBuddyAdapter) TransformSSE(ctx context.Context, src io.Reader, dst io.Writer, flusher http.Flusher) error {
	reader := bufio.NewReaderSize(src, 32*1024)
	state := NewSSEBridgeState("claude-3-7-sonnet")

	var currentEvent string
	var currentDataLines []string

	flushDownstream := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				break
			}
			return err
		}

		trimmed := strings.TrimRight(line, "\r\n")

		// 遇到空行表示一个完整 SSE 事件块结束
		if trimmed == "" {
			if len(currentDataLines) > 0 {
				fullData := strings.Join(currentDataLines, "\n")
				if err := processUpstreamSSEEvent(ctx, currentEvent, fullData, state, dst); err != nil {
					return err
				}
				flushDownstream()
			}
			currentEvent = ""
			currentDataLines = currentDataLines[:0]
			if err == io.EOF {
				break
			}
			continue
		}

		// 注释行 (心跳包 / ping)
		if strings.HasPrefix(trimmed, ":") {
			continue
		}

		if strings.HasPrefix(trimmed, "event:") {
			currentEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		} else if strings.HasPrefix(trimmed, "data:") {
			currentDataLines = append(currentDataLines, strings.TrimPrefix(trimmed, "data:"))
		}

		if err == io.EOF {
			// 处理末尾残留事件
			if len(currentDataLines) > 0 {
				fullData := strings.Join(currentDataLines, "\n")
				_ = processUpstreamSSEEvent(ctx, currentEvent, fullData, state, dst)
				flushDownstream()
			}
			break
		}
	}

	// 流结束时确保补齐 Anthropic 规范结束帧
	if err := state.emitStopSequence(dst); err != nil {
		return err
	}
	flushDownstream()

	return nil
}

// processUpstreamSSEEvent 解析单帧上游数据，识别并桥接文本增量
func processUpstreamSSEEvent(ctx context.Context, eventName string, data string, state *SSEBridgeState, dst io.Writer) error {
	trimmedData := strings.TrimSpace(data)
	if trimmedData == "" {
		return nil
	}

	// 1. 拦截标准结束标记
	if trimmedData == "[DONE]" || eventName == "done" || eventName == "wb_complete" {
		return state.emitStopSequence(dst)
	}

	// 2. 尝试判断是否已经是合规的 Anthropic SSE 原生事件
	if strings.HasPrefix(trimmedData, `{"type":"message_start"`) ||
		strings.HasPrefix(trimmedData, `{"type":"content_block_delta"`) ||
		strings.HasPrefix(trimmedData, `{"type":"content_block_stop"`) ||
		strings.HasPrefix(trimmedData, `{"type":"message_stop"`) {
		// 上游已是纯净 Anthropic 原生 SSE，直接透传事件
		if eventName != "" {
			_, _ = fmt.Fprintf(dst, "event: %s\n", eventName)
		}
		_, err := fmt.Fprintf(dst, "data: %s\n\n", trimmedData)
		return err
	}

	// 3. 上游非纯净 JSON 格式解析
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(trimmedData), &raw); err != nil {
		// 非 JSON 纯文本流兜底处理
		if !state.hasSentStart {
			_ = state.emitMessageStart(dst)
		}
		if !state.hasSentBlock {
			_ = state.emitContentBlockStart(dst)
		}
		return state.emitContentBlockDelta(dst, trimmedData)
	}

	// 4. 提取增量文本内容
	textChunk := extractTextChunkFromRaw(raw)
	if textChunk != "" {
		if !state.hasSentStart {
			if err := state.emitMessageStart(dst); err != nil {
				return err
			}
		}
		if !state.hasSentBlock {
			if err := state.emitContentBlockStart(dst); err != nil {
				return err
			}
		}
		if err := state.emitContentBlockDelta(dst, textChunk); err != nil {
			return err
		}
	}

	// 5. 检查上游是否包含结束指示 (如 finish_reason 或 done 标志)
	if checkUpstreamFinished(raw) {
		return state.emitStopSequence(dst)
	}

	return nil
}

// extractTextChunkFromRaw 从异构 JSON 数据结构中智能提取文本片段
func extractTextChunkFromRaw(raw map[string]interface{}) string {
	// Case 1: WorkBuddy 专有 RPC 结构 {"text": "...", ...} 或 {"data": {"text": "..."}}
	if t, ok := raw["text"].(string); ok {
		return t
	}
	if d, ok := raw["data"].(map[string]interface{}); ok {
		if t, ok := d["text"].(string); ok {
			return t
		}
		if c, ok := d["content"].(string); ok {
			return c
		}
	}

	// Case 2: OpenAI 兼容格式 {"choices": [{"delta": {"content": "..."}}]}
	if choices, ok := raw["choices"].([]interface{}); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]interface{}); ok {
			if delta, ok := first["delta"].(map[string]interface{}); ok {
				if c, ok := delta["content"].(string); ok {
					return c
				}
				// 兼容 reasoning_content 思维链字段
				if r, ok := delta["reasoning_content"].(string); ok {
					return r
				}
			}
		}
	}

	// Case 3: 自研 delta 结构 {"delta": {"text": "..."}}
	if delta, ok := raw["delta"].(map[string]interface{}); ok {
		if t, ok := delta["text"].(string); ok {
			return t
		}
		if c, ok := delta["content"].(string); ok {
			return c
		}
	}

	return ""
}

// checkUpstreamFinished 判断异构上游事件中是否已完成输出
func checkUpstreamFinished(raw map[string]interface{}) bool {
	if done, ok := raw["done"].(bool); ok && done {
		return true
	}
	if choices, ok := raw["choices"].([]interface{}); ok && len(choices) > 0 {
		if first, ok := choices[0].(map[string]interface{}); ok {
			if fr, ok := first["finish_reason"].(string); ok && fr != "" {
				return true
			}
		}
	}
	return false
}
