package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ProxyHandler 核心反向代理处理器（专为 WorkBuddy 国际版 Anthropic / Claude 协议深度定制）
type ProxyHandler struct {
	cfg            *Config
	client         *http.Client
	warmer         *ConnectionWarmer
	pacer          *Pacer
	breaker        *CircuitBreaker
	breakerManager *BreakerManager
	models         *ModelManager
	adapter        *WorkBuddyAdapter
	mux            *http.ServeMux
}

// NewProxyHandler 创建 WorkBuddy 国际版纯净代理处理器实例
func NewProxyHandler(cfg *Config) *ProxyHandler {
	// 动态使配置生效：缓冲池大小与日志等级
	SetStreamBufferSizeKB(cfg.BufferSizeKB)
	SetLogLevel(cfg.LogLevel)

	if cfg.EnableFingerprint {
		log.Printf("[WorkBuddy-Intl WARN] enable_fingerprint 已启用：伪造客户端指纹可能违反上游服务条款 (Terms of Service)")
	}

	client := NewOptimizedClient(cfg)
	pacer := NewPacer(cfg)

	var breakerManager *BreakerManager
	var breaker *CircuitBreaker
	if cfg.EnableCircuitBreaker {
		breakerManager = NewBreakerManager(1000)
		breaker = breakerManager.GetBreaker(cfg.APIKey)
	}

	models := NewModelManager(client, cfg.UpstreamURL, cfg.APIKey, 1*time.Hour)
	models.Start(context.Background())

	var warmer *ConnectionWarmer
	if cfg.EnableWarmup {
		warmer = NewConnectionWarmer(client, cfg.UpstreamURL, time.Duration(cfg.WarmupIntervalSeconds)*time.Second, cfg.WarmupConcurrency)
		warmer.Start(context.Background())
	}

	adapter := NewWorkBuddyAdapter(cfg.WorkBuddy)

	p := &ProxyHandler{
		cfg:            cfg,
		client:         client,
		warmer:         warmer,
		pacer:          pacer,
		breaker:        breaker,
		breakerManager: breakerManager,
		models:         models,
		adapter:        adapter,
		mux:            http.NewServeMux(),
	}
	p.RegisterRoutes(p.mux)
	return p
}

// ServeHTTP 实现标准 http.Handler 接口，并执行入站令牌安全校验
func (p *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	cfg := p.getConfig()
	if cfg.InboundToken != "" {
		path := r.URL.Path
		if strings.HasPrefix(path, "/v1") || strings.HasPrefix(path, "/api") ||
			strings.HasPrefix(path, "/messages") || strings.HasPrefix(path, "/models") ||
			strings.HasPrefix(path, "/stealth") || strings.HasPrefix(path, "/perf") {
			token := r.Header.Get("X-Proxy-Token")
			if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.InboundToken)) != 1 {
				if strings.HasPrefix(path, "/v1") || strings.HasPrefix(path, "/messages") || strings.HasPrefix(path, "/models") {
					w.Header().Set("Content-Type", "application/json; charset=utf-8")
					w.WriteHeader(http.StatusUnauthorized)
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"type": "error",
						"error": map[string]interface{}{
							"type":    "authentication_error",
							"message": "invalid or missing X-Proxy-Token",
						},
					})
					return
				}
				http.Error(w, "Unauthorized: invalid or missing X-Proxy-Token", http.StatusUnauthorized)
				return
			}
		}
	}

	p.mux.ServeHTTP(w, r)
}

// Pacer 获取起搏器实例
func (p *ProxyHandler) Pacer() *Pacer {
	return p.pacer
}

// Breaker 获取默认自适应熔断器实例
func (p *ProxyHandler) Breaker() *CircuitBreaker {
	if p.breakerManager != nil {
		cfg := p.getConfig()
		return p.breakerManager.GetBreaker(cfg.APIKey)
	}
	return p.breaker
}

// BreakerManager 获取凭据隔离熔断管理器
func (p *ProxyHandler) BreakerManager() *BreakerManager {
	return p.breakerManager
}

// Models 获取动态模型管理器
func (p *ProxyHandler) Models() *ModelManager {
	return p.models
}

func (p *ProxyHandler) getConfig() *Config {
	if p.cfg != nil {
		return p.cfg
	}
	c := GetConfig()
	return &c
}

// Close 释放代理处理器持有的后台资源（连接预热协程、模型刷新协程等）
func (p *ProxyHandler) Close() {
	if p.warmer != nil {
		p.warmer.Stop()
	}
	if p.models != nil {
		p.models.Stop()
	}
}

// RegisterRoutes 注册纯净 Anthropic / Claude 协议路由
func (p *ProxyHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", p.handleRoot)
	mux.HandleFunc("/health", p.handleHealth)
	mux.HandleFunc("/v1/messages", p.handleAnthropicMessages)
	mux.HandleFunc("/messages", p.handleAnthropicMessages)
	mux.HandleFunc("/v1/messages/count_tokens", p.handleCountTokens)
	mux.HandleFunc("/messages/count_tokens", p.handleCountTokens)
	mux.HandleFunc("/v1/models", p.handleModels)
	mux.HandleFunc("/models", p.handleModels)
	mux.HandleFunc("/v1/stealth/status", p.handleStealthStatus)
	mux.HandleFunc("/stealth/status", p.handleStealthStatus)
	mux.HandleFunc("/v1/perf/metrics", p.handlePerfMetrics)
	mux.HandleFunc("/perf/metrics", p.handlePerfMetrics)
	mux.HandleFunc("/api/config", p.handleConfigAPI)
	p.registerAdminRoutes(mux)
}

func (p *ProxyHandler) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cfg := p.getConfig()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	statusMap := map[string]interface{}{
		"status":      "running",
		"service":     "workbuddy-intl (WorkBuddy International Edition - Claude Core)",
		"protocol":    "anthropic",
		"upstream":    cfg.UpstreamURL,
		"version":     cfg.ClaudeCodeVersion,
		"fingerprint": cfg.EnableFingerprint,
		"pacing":      cfg.EnablePacing,
		"warmup":      cfg.EnableWarmup,
		"docs":        "https://docs.anthropic.com/en/api/messages",
	}

	if p.breakerManager != nil {
		statusMap["circuit_breaker"] = p.breakerManager.OverallStats()
	} else if p.breaker != nil {
		statusMap["circuit_breaker"] = p.breaker.Stats()
	}
	if p.pacer != nil && p.pacer.GCRA() != nil {
		statusMap["gcra"] = p.pacer.GCRA().Stats()
	}
	if p.warmer != nil {
		statusMap["warmer"] = p.warmer.Stats()
	}
	statusMap["pool_metrics"] = GetPoolMetrics()

	_ = json.NewEncoder(w).Encode(statusMap)
}

func (p *ProxyHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (p *ProxyHandler) handlePerfMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	res := map[string]interface{}{
		"status":       "ok",
		"pool_metrics": GetPoolMetrics(),
	}
	if p.warmer != nil {
		res["warmer"] = p.warmer.Stats()
	}
	_ = json.NewEncoder(w).Encode(res)
}

func (p *ProxyHandler) handleStealthStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	res := map[string]interface{}{
		"status": "healthy",
	}
	if p.breakerManager != nil {
		res["circuit_breaker"] = p.breakerManager.OverallStats()
		res["breakers"] = p.breakerManager.Stats()
	} else if p.breaker != nil {
		res["circuit_breaker"] = p.breaker.Stats()
	}
	if p.pacer != nil {
		if g := p.pacer.GCRA(); g != nil {
			res["gcra"] = g.Stats()
		}
		if j := p.pacer.Jitter(); j != nil {
			cnt, dur := j.Stats()
			res["jitter"] = map[string]interface{}{
				"sample_count": cnt,
				"total_wait":   dur.String(),
			}
		}
	}
	_ = json.NewEncoder(w).Encode(res)
}

func (p *ProxyHandler) handleConfigAPI(w http.ResponseWriter, r *http.Request) {
	cfg := p.getConfig()
	safeKey := ""
	if len(cfg.APIKey) > 8 {
		safeKey = cfg.APIKey[:4] + "...." + cfg.APIKey[len(cfg.APIKey)-4:]
	} else if len(cfg.APIKey) > 0 {
		safeKey = "********"
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"listen_addr":         cfg.ListenAddr,
		"upstream_url":        cfg.UpstreamURL,
		"api_key_set":         cfg.APIKey != "",
		"masked_key":          safeKey,
		"protocol":            "anthropic",
		"claude_code_version": cfg.ClaudeCodeVersion,
		"model_mapping":       cfg.ModelMapping,
		"log_level":           cfg.LogLevel,
		"timeout_seconds":     cfg.TimeoutSeconds,
		"max_body_mb":         cfg.MaxBodyMB,
		"enable_fingerprint":  cfg.EnableFingerprint,
		"enable_pacing":       cfg.EnablePacing,
		"rate_per_sec":        cfg.RatePerSec,
		"burst_capacity":      cfg.BurstCapacity,
		"enable_jitter":       cfg.EnableJitter,
		"enable_circuit_brk":  cfg.EnableCircuitBreaker,
		"enable_warmup":       cfg.EnableWarmup,
		"workbuddy_enabled":   cfg.WorkBuddy.Enabled,
		"workbuddy_mode":      cfg.WorkBuddy.Mode,
	})
}

func (p *ProxyHandler) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if p.models != nil {
		_, _ = w.Write(p.models.GetModelsJSON())
		return
	}

	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"data":     DefaultOfflineModels,
		"has_more": false,
	})
}

func (p *ProxyHandler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	p.forwardAnthropicPath(w, r, "/v1/messages/count_tokens")
}

func (p *ProxyHandler) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	p.forwardAnthropicPath(w, r, "/v1/messages")
}

func isHopByHopHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "connection" ||
		lower == "keep-alive" ||
		lower == "transfer-encoding" ||
		lower == "upgrade" ||
		lower == "content-length" ||
		lower == "trailer" ||
		strings.HasPrefix(lower, "proxy-") {
		return true
	}
	return false
}

func isMessagesEndpoint(path string) bool {
	p := strings.TrimSuffix(path, "/")
	return p == "/v1/messages" || p == "/messages"
}

func (p *ProxyHandler) forwardAnthropicPath(w http.ResponseWriter, r *http.Request, targetEndpoint string) {
	start := time.Now()
	cfg := p.getConfig()
	var (
		statModel  string
		statStatus int
		statBytes  int64
	)
	defer func() { recordRequest(targetEndpoint, statModel, statStatus, time.Since(start), statBytes) }()

	// 记录真实客户端请求到达，唤醒/维持连接预热心跳
	if p.warmer != nil {
		p.warmer.RecordActivity()
	}

	// 提取当前请求对应的凭据
	credKey := strings.TrimSpace(r.Header.Get("x-api-key"))
	if credKey == "" {
		auth := r.Header.Get("Authorization")
		if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			credKey = strings.TrimSpace(auth[7:])
		}
	}
	if credKey == "" {
		credKey = cfg.APIKey
	}

	var reqBreaker *CircuitBreaker
	if p.breakerManager != nil {
		reqBreaker = p.breakerManager.GetBreaker(credKey)
	} else if p.breaker != nil {
		reqBreaker = p.breaker
	}

	// 1. 检查凭据熔断状态 (429/401 避风状态保护)
	if reqBreaker != nil {
		if err := reqBreaker.CheckAllowed(); err != nil {
			state := reqBreaker.State()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			if state == StateAccountBlocked {
				statStatus = http.StatusForbidden
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"type": "error",
					"error": map[string]interface{}{
						"type":    "authentication_error",
						"message": err.Error(),
					},
				})
			} else {
				// StateOpen: 注入规范的 Retry-After 头
				until := reqBreaker.OpenUntil()
				retrySec := int(time.Until(until).Seconds())
				if retrySec < 1 {
					retrySec = 1
				}
				w.Header().Set("Retry-After", strconv.Itoa(retrySec))
				statStatus = http.StatusTooManyRequests
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"type": "error",
					"error": map[string]interface{}{
						"type":    "rate_limit_error",
						"message": err.Error(),
					},
				})
			}
			return
		}
	}

	// 2. 防风控起搏器排队（仅作用于 /v1/messages，不作用于 count_tokens 等端点）
	if p.pacer != nil && isMessagesEndpoint(targetEndpoint) {
		if err := p.pacer.Pace(r.Context()); err != nil {
			http.Error(w, "客户端连接在起搏排队等待中提前取消", http.StatusRequestTimeout)
			return
		}
	}

	// 3. 限制请求体大小 (max_body_mb)
	maxMB := cfg.MaxBodyMB
	if maxMB <= 0 {
		maxMB = 32
	}
	r.Body = http.MaxBytesReader(w, r.Body, int64(maxMB)*1024*1024)

	// 预处理 Anthropic 请求体（模型定点映射替换，借用 sync.Pool 缓冲）
	reqBuf := GetBuffer()
	defer PutBuffer(reqBuf)
	body, err := ProcessAnthropicRequestIntoBuffer(r, cfg, reqBuf)
	if err != nil {
		if errors.Is(err, ErrBodyTooLarge) {
			statStatus = http.StatusRequestEntityTooLarge
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "invalid_request_error",
					"message": fmt.Sprintf("request body too large, maximum allowed size is %dMB", maxMB),
				},
			})
			return
		}
		statStatus = http.StatusBadRequest
		http.Error(w, fmt.Sprintf("解析请求体失败: %v", err), http.StatusBadRequest)
		return
	}
	reqMeta := InspectAnthropicRequest(body)
	statModel = reqMeta.Model

	// WorkBuddy 通道适配层：执行载荷包装与注入
	if p.adapter != nil && p.adapter.IsEnabled() {
		transformed, tErr := p.adapter.TransformRequest(r, body, cfg)
		if tErr != nil {
			log.Printf("[WorkBuddy-Intl Adapter] TransformRequest 警告: %v", tErr)
		} else {
			body = transformed
		}
	}

	// 4. 拼接上游目标 URL
	targetURL, err := url.JoinPath(cfg.UpstreamURL, targetEndpoint)
	if err != nil {
		http.Error(w, "拼接上游端点地址失败", http.StatusInternalServerError)
		return
	}

	// 5. 设置单次请求超时控制 (timeout_seconds)
	timeoutSec := cfg.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = 180
	}
	timeoutDur := time.Duration(timeoutSec) * time.Second

	reqCtx, cancelReq := context.WithCancel(r.Context())
	defer cancelReq()

	timer := time.AfterFunc(timeoutDur, func() {
		cancelReq()
	})
	defer timer.Stop()

	outReq, err := http.NewRequestWithContext(reqCtx, r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		http.Error(w, fmt.Sprintf("创建转发请求失败: %v", err), http.StatusInternalServerError)
		return
	}

	// 6. 复制客户端请求头（过滤逐跳头与 Host）
	for k, vv := range r.Header {
		if isHopByHopHeader(k) || strings.EqualFold(k, "host") {
			continue
		}
		for _, v := range vv {
			outReq.Header.Add(k, v)
		}
	}

	// 7. 处理指纹改写与 API Key 注入
	if p.adapter != nil && p.adapter.IsEnabled() {
		p.adapter.TransformHeaders(outReq, cfg)
	} else {
		ApplyFingerprint(outReq, cfg)
	}

	log.Printf("[WorkBuddy-Intl] 转发 %s %s -> %s (Content-Length: %d)", r.Method, r.URL.Path, targetURL, len(body))

	// 8. 发送上游请求
	resp, err := p.client.Do(outReq)
	if err != nil {
		log.Printf("[WorkBuddy-Intl Error] 请求上游通信失败: %v", err)
		if reqBreaker != nil {
			reqBreaker.RecordResponse(nil, nil)
		}
		if reqCtx.Err() == context.Canceled {
			statStatus = http.StatusGatewayTimeout
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusGatewayTimeout)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"type": "error",
				"error": map[string]interface{}{
					"type":    "timeout_error",
					"message": fmt.Sprintf("request timed out after %v", timeoutDur),
				},
			})
			return
		}
		statStatus = http.StatusBadGateway
		http.Error(w, fmt.Sprintf("上游服务通信失败: %v", err), http.StatusBadGateway)
		return
	}
	defer DrainAndClose(resp.Body)
	statStatus = resp.StatusCode

	// 9. 动态自适应限流反馈：解析上游 RateLimit 响应头同步至 GCRA 削峰器
	if p.pacer != nil && p.pacer.GCRA() != nil {
		p.pacer.GCRA().UpdateFromHeaders(resp.Header)
	}

	// 10. 异常状态响应分类与熔断器记录 (>= 400)
	if resp.StatusCode >= 400 {
		errBuf := GetBuffer()
		defer PutBuffer(errBuf)
		_, _ = io.Copy(errBuf, resp.Body)
		errBytes := errBuf.Bytes()

		if reqBreaker != nil {
			reqBreaker.RecordResponse(resp, errBytes)
		}

		// 透传错误响应头（严格过滤所有逐跳头）
		for k, vv := range resp.Header {
			if isHopByHopHeader(k) {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(errBytes)
		return
	}

	// 成功响应重置熔断计数器与退避状态
	if reqBreaker != nil {
		reqBreaker.RecordResponse(resp, nil)
	}

	// 11. 复制上游成功响应头（严格过滤逐跳头）并注入穿透防缓冲指令
	for k, vv := range resp.Header {
		if isHopByHopHeader(k) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-cache, no-transform, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	flusher, isFlusher := w.(http.Flusher)

	// 【Phase 1: Header Immediate Flush】立即提交响应头并强刷
	w.WriteHeader(resp.StatusCode)
	if isFlusher {
		flusher.Flush()
	}

	// 【Phase 2: Chunk Instant Passthrough】借用流式缓冲区，流式请求收到首字节后改为空闲超时
	bufPtr := GetStreamBuffer()
	defer PutStreamBuffer(bufPtr)
	buf := *bufPtr

	isStream := reqMeta.Stream || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")

	// WorkBuddy 专用 SSE Bridge：非纯净流双向转换
	if p.adapter != nil && p.adapter.IsEnabled() && isStream {
		if err := p.adapter.TransformSSE(reqCtx, resp.Body, w, flusher); err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
				log.Printf("[WorkBuddy-Intl SSEBridge] 流式转换中断: %v", err)
			}
		}
		log.Printf("[WorkBuddy-Intl Done] %s %s <- 状态码: %d | (SSE Bridge Transcoded) | 耗时: %v",
			r.Method, r.URL.Path, resp.StatusCode, time.Since(start))
		return
	}

	var totalBytes int64

	for {
		select {
		case <-r.Context().Done():
			log.Printf("[WorkBuddy-Intl Info] 客户端连接提前终止: %v", r.Context().Err())
			return
		case <-reqCtx.Done():
			log.Printf("[WorkBuddy-Intl Error] 请求传输超时 (%v)", timeoutDur)
			return
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if isStream {
				// 流式请求在收到首字节后改为空闲超时：每次成功读取数据块后刷新超时时钟
				timer.Reset(timeoutDur)
			}
			written, writeErr := w.Write(buf[:n])
			totalBytes += int64(written)
			if isFlusher {
				flusher.Flush()
			}
			if writeErr != nil {
				log.Printf("[WorkBuddy-Intl Info] 客户端提前中断连接: %v", writeErr)
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF && readErr != context.Canceled {
				log.Printf("[WorkBuddy-Intl Error] 读取上游响应失败: %v", readErr)
			}
			break
		}
	}

	statBytes = totalBytes
	log.Printf("[WorkBuddy-Intl Done] %s %s <- 状态码: %d | 传输字节: %d | 耗时: %v",
		r.Method, r.URL.Path, resp.StatusCode, totalBytes, time.Since(start))
}
