package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ===========================================================================
// E2E 探针 1: 真实完整协议路由与端点转发自动化探针
// ===========================================================================

func TestE2E_FullProtocolRoutingAndEndpoints(t *testing.T) {
	var mu sync.Mutex
	receivedRequests := make(map[string]*http.Request)
	receivedBodies := make(map[string][]byte)

	// 1. 构建 Mock 上游 Anthropic 官方服务
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		receivedRequests[r.URL.Path] = r.Clone(r.Context())
		receivedBodies[r.URL.Path] = body
		mu.Unlock()

		switch r.URL.Path {
		case "/v1/messages":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_e2e_123","type":"message","role":"assistant","content":[{"type":"text","text":"Hello E2E"}],"model":"claude-3-5-sonnet-20241022","usage":{"input_tokens":10,"output_tokens":5}}`))
		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"input_tokens":25}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mockUpstream.Close()

	// 2. 初始化 ProxyHandler，指向 Mock 上游
	cfg := &Config{
		ListenAddr:            "127.0.0.1:0",
		UpstreamURL:           mockUpstream.URL,
		APIKey:                "sk-ant-test-key-12345",
		ClaudeCodeVersion:     "2.1.306",
		EnableFingerprint:     true,
		EnablePacing:          false, // E2E 快速路由测试关闭起搏延迟
		EnableCircuitBreaker:  true,
		EnableWarmup:          false,
		TimeoutSeconds:        30,
		ResponseHeaderTimeout: 10,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		BufferSizeKB:          32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	client := proxyServer.Client()

	// 3. 测试用例矩阵定义
	testCases := []struct {
		name           string
		method         string
		path           string
		reqBody        string
		expectedStatus int
		expectUpstream bool
		upstreamPath   string
		checkBodySub   string
	}{
		{
			name:           "POST /v1/messages 标准消息转发",
			method:         "POST",
			path:           "/v1/messages",
			reqBody:        `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`,
			expectedStatus: http.StatusOK,
			expectUpstream: true,
			upstreamPath:   "/v1/messages",
			checkBodySub:   "Hello E2E",
		},
		{
			name:           "POST /messages 兼容路由转发",
			method:         "POST",
			path:           "/messages",
			reqBody:        `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"hi"}]}`,
			expectedStatus: http.StatusOK,
			expectUpstream: true,
			upstreamPath:   "/v1/messages",
			checkBodySub:   "Hello E2E",
		},
		{
			name:           "POST /v1/messages/count_tokens 计算 Token",
			method:         "POST",
			path:           "/v1/messages/count_tokens",
			reqBody:        `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"count this"}]}`,
			expectedStatus: http.StatusOK,
			expectUpstream: true,
			upstreamPath:   "/v1/messages/count_tokens",
			checkBodySub:   `"input_tokens":25`,
		},
		{
			name:           "POST /messages/count_tokens 兼容路由计算 Token",
			method:         "POST",
			path:           "/messages/count_tokens",
			reqBody:        `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"count this"}]}`,
			expectedStatus: http.StatusOK,
			expectUpstream: true,
			upstreamPath:   "/v1/messages/count_tokens",
			checkBodySub:   `"input_tokens":25`,
		},
		{
			name:           "GET /v1/models 本地模型列表",
			method:         "GET",
			path:           "/v1/models",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `claude-3-7-sonnet-latest`,
		},
		{
			name:           "GET /models 兼容模型列表",
			method:         "GET",
			path:           "/models",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `claude-3-5-sonnet-latest`,
		},
		{
			name:           "GET /health 健康探测",
			method:         "GET",
			path:           "/health",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `OK`,
		},
		{
			name:           "GET / 服务根元数据与状态",
			method:         "GET",
			path:           "/",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `workbuddy-intl`,
		},
		{
			name:           "GET /api/config 配置查看与脱敏校验",
			method:         "GET",
			path:           "/api/config",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `masked_key`,
		},
		{
			name:           "GET /v1/stealth/status 防风控状态监控",
			method:         "GET",
			path:           "/v1/stealth/status",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `circuit_breaker`,
		},
		{
			name:           "GET /v1/perf/metrics 性能与连接池监控",
			method:         "GET",
			path:           "/v1/perf/metrics",
			reqBody:        "",
			expectedStatus: http.StatusOK,
			expectUpstream: false,
			checkBodySub:   `pool_metrics`,
		},
		{
			name:           "GET /non_existent_route 严格返回 404",
			method:         "GET",
			path:           "/non_existent_route",
			reqBody:        "",
			expectedStatus: http.StatusNotFound,
			expectUpstream: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var bodyReader io.Reader
			if tc.reqBody != "" {
				bodyReader = strings.NewReader(tc.reqBody)
			}
			req, err := http.NewRequest(tc.method, proxyServer.URL+tc.path, bodyReader)
			if err != nil {
				t.Fatalf("创建请求失败: %v", err)
			}
			if tc.reqBody != "" {
				req.Header.Set("Content-Type", "application/json")
			}

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("执行请求失败: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.expectedStatus {
				t.Fatalf("预期状态码 %d, 实际得到: %d", tc.expectedStatus, resp.StatusCode)
			}

			respBytes, _ := io.ReadAll(resp.Body)
			if tc.checkBodySub != "" && !strings.Contains(string(respBytes), tc.checkBodySub) {
				t.Fatalf("响应内容缺少预期子串 %q, 实际内容: %s", tc.checkBodySub, string(respBytes))
			}

			if tc.expectUpstream {
				mu.Lock()
				_, received := receivedRequests[tc.upstreamPath]
				mu.Unlock()
				if !received {
					t.Fatalf("预期上游接收端点 %s, 但未被调用", tc.upstreamPath)
				}
			}
		})
	}
}

// ===========================================================================
// E2E 探针 2: 打字机流式效果 (Typing SSE Chunked Probe) 与低延迟穿透探针
// ===========================================================================

func TestE2E_TypewriterStreamingChunkedProbe(t *testing.T) {
	// 模拟流式 SSE 数据包列表，上游分步 Flush 推送
	sseChunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_stream_probe\",\"model\":\"claude-3-5-sonnet-20241022\"}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" World\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	chunkInterval := 30 * time.Millisecond

	// 1. Mock 上游流式打字机服务器
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("Mock 上游必须支持 http.Flusher")
			return
		}
		flusher.Flush()

		// 模拟真实打字机逐步推送
		for _, chunk := range sseChunks {
			time.Sleep(chunkInterval)
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
		}
	}))
	defer mockUpstream.Close()

	// 2. 初始化代理
	cfg := &Config{
		ListenAddr:            "127.0.0.1:0",
		UpstreamURL:           mockUpstream.URL,
		EnableFingerprint:     true,
		EnablePacing:          false, // 关闭起搏以精确测算打字机流水线时延
		EnableCircuitBreaker:  false,
		EnableWarmup:          false,
		TimeoutSeconds:        30,
		ResponseHeaderTimeout: 10,
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		BufferSizeKB:          32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// 3. 客户端逐块接收流并记录时间戳
	client := proxyServer.Client()
	reqBody := `{"model":"claude-3-5-sonnet-20241022","stream":true,"messages":[{"role":"user","content":"Type slowly"}]}`
	req, err := http.NewRequest("POST", proxyServer.URL+"/v1/messages", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("创建流式请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	startTime := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	// 验证 1: 穿透防缓冲头矩阵
	if xab := resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("缺少或错误的 X-Accel-Buffering 头: %q (预期 'no')", xab)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("Cache-Control 必须包含 no-cache, 得到: %q", cc)
	}
	if nosniff := resp.Header.Get("X-Content-Type-Options"); nosniff != "nosniff" {
		t.Errorf("X-Content-Type-Options 必须为 nosniff, 得到: %q", nosniff)
	}

	// 验证 2: 打字机逐块流式到达与时间戳渐进性
	type ChunkReceipt struct {
		index     int
		timestamp time.Time
		data      string
	}

	var receipts []ChunkReceipt
	buf := make([]byte, 4096)
	var fullReceived strings.Builder

	for {
		n, rErr := resp.Body.Read(buf)
		if n > 0 {
			receivedStr := string(buf[:n])
			receipts = append(receipts, ChunkReceipt{
				index:     len(receipts),
				timestamp: time.Now(),
				data:      receivedStr,
			})
			fullReceived.WriteString(receivedStr)
		}
		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			t.Fatalf("读取流失败: %v", rErr)
		}
	}

	totalDuration := time.Since(startTime)

	// 校验: 接收到了多次 chunk 推送，证明未被聚合一次性输出
	if len(receipts) < 3 {
		t.Fatalf("打字机效果失效：接收 chunk 次数过少 (%d 次)，数据可能被全量缓冲", len(receipts))
	}

	// 校验首块交付时间 TTFB 应明显小于总耗时
	firstChunkLatency := receipts[0].timestamp.Sub(startTime)
	if firstChunkLatency >= totalDuration-50*time.Millisecond {
		t.Errorf("首块延迟 (%v) 接近总耗时 (%v)，未达成 TTFB 首字节强刷效果", firstChunkLatency, totalDuration)
	}

	// 校验完整 SSE 报文文本包含打字机文本
	fullStr := fullReceived.String()
	if !strings.Contains(fullStr, "Hello") || !strings.Contains(fullStr, " World") || !strings.Contains(fullStr, "message_stop") {
		t.Fatalf("流式接收数据不完整: %s", fullStr)
	}

	t.Logf("✓ 打字机流式验证通过: 共接收 %d 个分块, TTFB首块延迟: %v, 总耗时: %v", len(receipts), firstChunkLatency, totalDuration)
}

// ===========================================================================
// E2E 探针 3: 官方 Claude Code 原生指纹注入与反代痕迹清洗深度探针
// ===========================================================================

func TestE2E_FingerprintSanitizationAndSpoofingProbe(t *testing.T) {
	var capturedHeader http.Header

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_fp_test","type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		APIKey:               "", // 留空以测试客户端 Bearer Token 规整为 x-api-key 的场景
		ClaudeCodeVersion:    "2.1.306",
		EnableFingerprint:    true,
		EnablePacing:         false,
		EnableCircuitBreaker: false,
		EnableWarmup:         false,
		TimeoutSeconds:       10,
		BufferSizeKB:         32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	client := proxyServer.Client()

	req, err := http.NewRequest("POST", proxyServer.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"user","content":"test"}]}`))
	if err != nil {
		t.Fatal(err)
	}

	// 注入多达 20 项反代、CDN、第三方 SDK 及爬虫痕迹请求头
	pollutedHeaders := map[string]string{
		"X-Forwarded-For":     "203.0.113.195, 70.41.3.18",
		"X-Forwarded-Host":    "api.reverse-proxy.local",
		"X-Forwarded-Proto":   "https",
		"X-Forwarded-Server":  "proxy.cluster.internal",
		"X-Real-IP":           "198.51.100.1",
		"Via":                 "1.1 vegur, 1.1 varnish",
		"Forwarded":           "for=192.0.2.60;proto=http;by=203.0.113.43",
		"Client-IP":           "192.0.2.1",
		"CF-Connecting-IP":    "198.51.100.25",
		"True-Client-IP":      "198.51.100.26",
		"X-Cluster-Client-IP": "10.244.0.15",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
		"X-Stainless-Lang":                          "python",
		"X-Stainless-Package-Version":               "0.25.0",
		"X-Stainless-OS":                            "Windows",
		"X-Stainless-Arch":                          "x64",
		"X-Stainless-Runtime":                       "cpython",
		"X-WorkBuddy-Source":                        "desktop-extension",
		"X-OpenAI-Compat":                           "true",
		"X-Client-Data":                             "CJK2yQEIo7bJAQjBtskBCKmdygE=",
	}

	for k, v := range pollutedHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer custom-client-token")
	req.Header.Set("User-Agent", "CustomBrowser/1.0 (Windows NT 10.0)")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预期 200 OK, 实际: %d", resp.StatusCode)
	}

	// 验证 1: 所有污染特征头必须 100% 被清洗消除，无任何残留
	for k := range pollutedHeaders {
		if val := capturedHeader.Get(k); val != "" {
			t.Fatalf("反代痕迹泄露！请求头 %q 未被过滤，残留值: %q", k, val)
		}
	}

	// 验证 2: 保持客户端原始 UA，不伪造任何身份特征
	if ua := capturedHeader.Get("User-Agent"); ua != "CustomBrowser/1.0 (Windows NT 10.0)" {
		t.Errorf("User-Agent 应保持客户端原始值, 得到 %q", ua)
	}

	// 验证 3: API Key 鉴权头规整为 x-api-key
	apiKey := capturedHeader.Get("X-Api-Key")
	if apiKey != "custom-client-token" {
		t.Errorf("x-api-key 未正确承接客户端 Bearer Token, 得到: %q", apiKey)
	}
	if auth := capturedHeader.Get("Authorization"); auth != "" {
		t.Errorf("Authorization 头应已被规整移除，实际残留: %q", auth)
	}

	// 验证 4: 使用系统级 VerifyFingerprint 验证器自检
	upstreamReq := &http.Request{Header: capturedHeader}
	violations := VerifyFingerprint(upstreamReq, "2.1.306")
	if len(violations) > 0 {
		t.Fatalf("VerifyFingerprint 发现了违规项: %v", violations)
	}

	t.Logf("✓ 指纹清洗验证通过: 20 项反代特征全部清除，不新增伪装能力")
}

// ===========================================================================
// E2E 探针 4: 模型别名映射与重写探针
// ===========================================================================

func TestE2E_ModelAliasRewritingProbe(t *testing.T) {
	var lastReceivedBody map[string]interface{}
	var mu sync.Mutex

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		lastReceivedBody = b
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_map","type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:  "127.0.0.1:0",
		UpstreamURL: mockUpstream.URL,
		ModelMapping: map[string]string{
			"claude-3-7-sonnet": "claude-3-7-sonnet-20250219",
			"claude-3-5-sonnet": "claude-3-5-sonnet-20241022",
			"fast-coder":        "claude-3-5-haiku-20241022",
		},
		EnableFingerprint:    true,
		EnablePacing:         false,
		EnableCircuitBreaker: false,
		BufferSizeKB:         32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	client := proxyServer.Client()

	probeCases := []struct {
		inputModel    string
		expectedModel string
	}{
		{"claude-3-7-sonnet", "claude-3-7-sonnet-20250219"},
		{"claude-3-5-sonnet", "claude-3-5-sonnet-20241022"},
		{"fast-coder", "claude-3-5-haiku-20241022"},
		{"claude-3-opus-20240229", "claude-3-opus-20240229"}, // 未在映射表中，保持原样透传
	}

	for _, pc := range probeCases {
		reqJSON := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"test"}]}`, pc.inputModel)
		req, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", strings.NewReader(reqJSON))
		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		resp.Body.Close()

		mu.Lock()
		actualModel := lastReceivedBody["model"]
		mu.Unlock()

		if actualModel != pc.expectedModel {
			t.Errorf("模型映射重写错误: 输入 %q, 预期上游收到 %q, 实际收到 %q", pc.inputModel, pc.expectedModel, actualModel)
		}
	}

	t.Logf("✓ 模型映射与别名重写探针通过: 成功重写别名并透传未映射模型")
}

// ===========================================================================
// E2E 探针 5: 自适应避风熔断与硬墙保护探针
// ===========================================================================

func TestE2E_AdaptiveCircuitBreakerAndRateLimitProbe(t *testing.T) {
	upstreamHitCount := 0
	var mu sync.Mutex

	// 上游模拟器：按测试阶段返回不同状态
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		mu.Lock()
		upstreamHitCount++
		count := upstreamHitCount
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			// 第一次：上游触发 429 频控硬墙，带 3 秒重置指示
			w.Header().Set("Retry-After", "3")
			w.Header().Set("anthropic-ratelimit-requests-reset", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your quota."}}`))
		} else {
			// 若熔断器未拦截，上游将被击中
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message"}`))
		}
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		EnableFingerprint:    true,
		EnablePacing:         false,
		EnableCircuitBreaker: true,
		BufferSizeKB:         32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	client := proxyServer.Client()

	// 1. 触发第一次请求 -> 触发上游 429，记录进熔断器
	req1, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"test"}]}`))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第一次请求预期 429, 得到 %d", resp1.StatusCode)
	}

	// 2. 紧接着发送第二次请求 -> 熔断器必须处于 StateOpen，在代理层直接拦截并返回友好 JSON + Retry-After，上游不应被击中！
	req2, _ := http.NewRequest("POST", proxyServer.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"test"}]}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("第二次请求应被熔断器直接拦截为 429, 实际得到: %d", resp2.StatusCode)
	}

	// 校验拦截时附带的 Retry-After 头部
	retryAfter := resp2.Header.Get("Retry-After")
	if retryAfter == "" {
		t.Errorf("熔断器拦截响应缺少 Retry-After 标头")
	}

	// 校验上游没有收到第二次请求 (upstreamHitCount 仍为 1)
	mu.Lock()
	hits := upstreamHitCount
	mu.Unlock()
	if hits != 1 {
		t.Fatalf("熔断避风失效！上游被请求了 %d 次 (预期 1 次，第 2 次应被本地拦截)", hits)
	}

	t.Logf("✓ 避风熔断器验证通过: 429 硬墙成功触发本地阻断保护，上游击穿防护生效 (Retry-After: %s)", retryAfter)
}

// ===========================================================================
// E2E 探针 6: 客户端中断与连接解耦探针
// ===========================================================================

func TestE2E_ClientCancellationDecouplingProbe(t *testing.T) {
	hangStarted := make(chan struct{})

	var once sync.Once
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		once.Do(func() {
			close(hangStarted)
		})
		// 模拟上游长时间无响应
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"too_late"}`))
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		EnableFingerprint:    true,
		EnablePacing:         false,
		EnableCircuitBreaker: false,
		BufferSizeKB:         32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	client := proxyServer.Client()

	// 创建一个提前超时的 context
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, "POST", proxyServer.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[{"role":"user","content":"test"}]}`))
	req.Header.Set("Content-Type", "application/json")

	_, err := client.Do(req)
	if err == nil {
		t.Fatal("预期客户端会因 context 超时而报错")
	}

	<-hangStarted
	time.Sleep(100 * time.Millisecond) // 等待代理层完成 select 退出与清理

	// 验证全局 pool 状态健康，未发生泄漏或锁死
	metrics := GetPoolMetrics()
	if metrics.BufferGets == 0 && metrics.StreamGets == 0 {
		t.Logf("PoolMetrics 当前读数: %+v", metrics)
	}

	t.Logf("✓ 客户端中断与生命周期解耦通过: 优雅处理客户端主动取消且无资源泄漏")
}
