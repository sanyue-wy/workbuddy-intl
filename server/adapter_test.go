package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ===========================================================================
// 1. 请求体转换 (TransformRequest) 测试
// ===========================================================================

func TestWorkBuddyAdapter_TransformRequest_InjectMode(t *testing.T) {
	wbCfg := WorkBuddyConfig{
		Enabled:   true,
		Mode:      "workbuddy",
		ChannelID: "test-channel-999",
		DeviceID:  "device-abc-123",
		WrapMode:  "inject",
	}
	adapter := NewWorkBuddyAdapter(wbCfg)

	cfg := &Config{
		ModelMapping: map[string]string{
			"claude-3-7-sonnet": "wb-claude-3-7-sonnet-v1",
		},
	}

	reqBody := []byte(`{
		"model": "claude-3-7-sonnet",
		"messages": [{"role": "user", "content": "Hello WorkBuddy"}],
		"max_tokens": 1024,
		"stream": true
	}`)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	outBytes, err := adapter.TransformRequest(req, reqBody, cfg)
	if err != nil {
		t.Fatalf("TransformRequest failed: %v", err)
	}

	var res map[string]interface{}
	if err := json.Unmarshal(outBytes, &res); err != nil {
		t.Fatalf("Unmarshal output failed: %v", err)
	}

	// 验证模型重映射
	if res["model"] != "wb-claude-3-7-sonnet-v1" {
		t.Errorf("Expected model mapped to wb-claude-3-7-sonnet-v1, got %v", res["model"])
	}

	// 验证注入字段
	if res["channel_id"] != "test-channel-999" {
		t.Errorf("Expected channel_id to be test-channel-999, got %v", res["channel_id"])
	}
	if res["device_id"] != "device-abc-123" {
		t.Errorf("Expected device_id to be device-abc-123, got %v", res["device_id"])
	}
	if res["session_id"] == nil || res["session_id"] == "" {
		t.Errorf("Expected session_id to be generated")
	}
}

func TestWorkBuddyAdapter_TransformRequest_EnvelopeMode(t *testing.T) {
	wbCfg := WorkBuddyConfig{
		Enabled:   true,
		Mode:      "workbuddy",
		ChannelID: "desktop-intl-prod",
		DeviceID:  "dev-777",
		WrapMode:  "envelope",
	}
	adapter := NewWorkBuddyAdapter(wbCfg)

	reqBody := []byte(`{"model": "claude-3-5-sonnet", "messages": [{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))

	outBytes, err := adapter.TransformRequest(req, reqBody, nil)
	if err != nil {
		t.Fatalf("TransformRequest envelope failed: %v", err)
	}

	var envelope map[string]interface{}
	if err := json.Unmarshal(outBytes, &envelope); err != nil {
		t.Fatalf("Unmarshal envelope failed: %v", err)
	}

	if envelope["channel_id"] != "desktop-intl-prod" {
		t.Errorf("Expected envelope channel_id desktop-intl-prod, got %v", envelope["channel_id"])
	}
	if envelope["protocol_standard"] != "anthropic_v1" {
		t.Errorf("Expected protocol_standard anthropic_v1, got %v", envelope["protocol_standard"])
	}
	payload, ok := envelope["request_payload"].(map[string]interface{})
	if !ok {
		t.Fatalf("request_payload missing or not a map")
	}
	if payload["model"] != "claude-3-5-sonnet" {
		t.Errorf("Expected inner model claude-3-5-sonnet, got %v", payload["model"])
	}
}

// ===========================================================================
// 2. 请求头清洗与伪装 (TransformHeaders) 测试
// ===========================================================================

func TestWorkBuddyAdapter_TransformHeaders(t *testing.T) {
	wbCfg := WorkBuddyConfig{
		Enabled:      true,
		SessionToken: "wb_jwt_token_abcdef123456",
		Cookie:       "session_id=s123; auth=token456",
		ChannelID:    "intl-test",
		DeviceID:     "device-001",
		CustomOrigin: "https://intl.workbuddy.ai",
		UserAgent:    "WorkBuddy-Custom-Agent/1.0",
		ExtraHeaders: map[string]string{
			"x-custom-tenant": "enterprise-intl",
		},
	}
	adapter := NewWorkBuddyAdapter(wbCfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	// 模拟客户端携带的暴露头和旧 token
	req.Header.Set("x-api-key", "client-inbound-key")
	req.Header.Set("Authorization", "Bearer old-client-token")
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	req.Header.Set("Via", "1.1 varnish")

	adapter.TransformHeaders(req, nil)

	// 1. 验证负向代理头已被清除
	if req.Header.Get("X-Forwarded-For") != "" {
		t.Errorf("X-Forwarded-For should be cleared, got: %s", req.Header.Get("X-Forwarded-For"))
	}
	if req.Header.Get("Via") != "" {
		t.Errorf("Via should be cleared, got: %s", req.Header.Get("Via"))
	}
	if req.Header.Get("x-api-key") != "" {
		t.Errorf("x-api-key should be stripped")
	}

	// 2. 验证真实凭据注入
	expectedAuth := "Bearer wb_jwt_token_abcdef123456"
	if req.Header.Get("Authorization") != expectedAuth {
		t.Errorf("Expected Authorization %s, got %s", expectedAuth, req.Header.Get("Authorization"))
	}
	if req.Header.Get("x-session-token") != "wb_jwt_token_abcdef123456" {
		t.Errorf("Expected x-session-token to be set")
	}
	if req.Header.Get("Cookie") != "session_id=s123; auth=token456" {
		t.Errorf("Expected Cookie to be set")
	}

	// 3. 验证浏览器与原生指纹
	if req.Header.Get("Origin") != "https://intl.workbuddy.ai" {
		t.Errorf("Expected Origin https://intl.workbuddy.ai, got %s", req.Header.Get("Origin"))
	}
	if req.Header.Get("User-Agent") != "WorkBuddy-Custom-Agent/1.0" {
		t.Errorf("Expected User-Agent WorkBuddy-Custom-Agent/1.0, got %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("x-custom-tenant") != "enterprise-intl" {
		t.Errorf("Expected extra header x-custom-tenant to be set")
	}
}

// ===========================================================================
// 3. SSE 双向流式转换 (TransformSSE) 测试
// ===========================================================================

func TestWorkBuddyAdapter_TransformSSE_CustomUpstreamStream(t *testing.T) {
	adapter := NewWorkBuddyAdapter(DefaultWorkBuddyConfig())

	// 模拟非纯净的上游 WorkBuddy 异构 SSE 流
	upstreamSSE := strings.Join([]string{
		": ping keepalive",
		"event: wb_message",
		`data: {"code":0,"data":{"text":"Hello"}}`,
		"",
		"event: wb_message",
		`data: {"code":0,"data":{"text":" World!"}}`,
		"",
		"event: done",
		`data: {"done": true}`,
		"",
	}, "\n")

	var outputBuf bytes.Buffer
	rec := httptest.NewRecorder()

	err := adapter.TransformSSE(context.Background(), strings.NewReader(upstreamSSE), &outputBuf, rec)
	if err != nil {
		t.Fatalf("TransformSSE failed: %v", err)
	}

	outStr := outputBuf.String()

	// 验证标准 Anthropic SSE 事件序列
	if !strings.Contains(outStr, "event: message_start") {
		t.Errorf("Expected output to contain event: message_start")
	}
	if !strings.Contains(outStr, "event: content_block_start") {
		t.Errorf("Expected output to contain event: content_block_start")
	}
	if !strings.Contains(outStr, `event: content_block_delta`) {
		t.Errorf("Expected output to contain event: content_block_delta")
	}
	if !strings.Contains(outStr, `"text":"Hello"`) {
		t.Errorf("Expected output to contain Hello chunk")
	}
	if !strings.Contains(outStr, `"text":" World!"`) {
		t.Errorf("Expected output to contain World! chunk")
	}
	if !strings.Contains(outStr, "event: content_block_stop") {
		t.Errorf("Expected output to contain event: content_block_stop")
	}
	if !strings.Contains(outStr, "event: message_delta") {
		t.Errorf("Expected output to contain event: message_delta")
	}
	if !strings.Contains(outStr, "event: message_stop") {
		t.Errorf("Expected output to contain event: message_stop")
	}
}

func TestWorkBuddyAdapter_TransformSSE_OpenAICompatibleStream(t *testing.T) {
	adapter := NewWorkBuddyAdapter(DefaultWorkBuddyConfig())

	// 模拟上游返回 OpenAI / DeepSeek 风格的流式分块
	openAISSE := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Deep"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"Seek"}}]}`,
		"",
		`data: [DONE]`,
		"",
	}, "\n")

	var outputBuf bytes.Buffer
	err := adapter.TransformSSE(context.Background(), strings.NewReader(openAISSE), &outputBuf, nil)
	if err != nil {
		t.Fatalf("TransformSSE OpenAI-compat failed: %v", err)
	}

	outStr := outputBuf.String()
	if !strings.Contains(outStr, `"text":"Deep"`) {
		t.Errorf("Expected chunk Deep, got: %s", outStr)
	}
	if !strings.Contains(outStr, `"text":"Seek"`) {
		t.Errorf("Expected chunk Seek, got: %s", outStr)
	}
	if !strings.Contains(outStr, "event: message_stop") {
		t.Errorf("Expected message_stop after [DONE]")
	}
}

// ===========================================================================
// 4. 端到端转发集成测试 (End-to-End Adapter Routing)
// ===========================================================================

func TestWorkBuddyAdapter_EndToEndProxy(t *testing.T) {
	// 1. 模拟 WorkBuddy 上游服务端点
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 校验头部已被伪装
		if r.Header.Get("x-workbuddy-channel") != "test-channel" {
			http.Error(w, "missing x-workbuddy-channel", http.StatusBadRequest)
			return
		}
		if !strings.Contains(r.Header.Get("Authorization"), "Bearer mock-session-token") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// 读取上游收到的请求体
		var reqMap map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&reqMap)

		if reqMap["session_id"] == nil {
			http.Error(w, "missing session_id", http.StatusBadRequest)
			return
		}

		// 返回模拟的 WorkBuddy 流式事件
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		_, _ = w.Write([]byte("data: {\"text\":\"WorkBuddy\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: {\"text\":\" Response\"}\n\n"))
		flusher.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstreamServer.Close()

	// 2. 初始化带 Adapter 的 ProxyHandler
	cfg := DefaultConfig()
	cfg.UpstreamURL = upstreamServer.URL
	cfg.InboundToken = "test-secret"
	cfg.WorkBuddy = WorkBuddyConfig{
		Enabled:      true,
		Mode:         "workbuddy",
		SessionToken: "mock-session-token",
		ChannelID:    "test-channel",
		WrapMode:     "inject",
	}

	proxy := NewProxyHandler(&cfg)

	// 3. 构造下游客户端发送的标准 Anthropic 请求
	clientPayload := `{"model":"claude-3-7-sonnet","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(clientPayload))
	req.Header.Set("X-Proxy-Token", "test-secret")
	req.Header.Set("x-api-key", "test-secret")
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("Expected 200 OK, got %d, body: %s", resp.StatusCode, string(body))
	}

	respBody, _ := io.ReadAll(resp.Body)
	respStr := string(respBody)

	// 验证客户端收到的是标准 Anthropic SSE
	if !strings.Contains(respStr, "event: message_start") {
		t.Errorf("Expected standard Anthropic message_start, got: %s", respStr)
	}
	if !strings.Contains(respStr, `"text":"WorkBuddy"`) {
		t.Errorf("Expected text chunk WorkBuddy")
	}
	if !strings.Contains(respStr, "event: message_stop") {
		t.Errorf("Expected message_stop at the end")
	}
}
