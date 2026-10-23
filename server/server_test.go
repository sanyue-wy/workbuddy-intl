package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// 1. 配置与内存池测试
// ---------------------------------------------------------------------------

func TestLoadConfigStripsBOM(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	raw := "\ufeff{\"listen_addr\":\"127.0.0.1:9999\",\"upstream_url\":\"https://api.anthropic.com\"}"
	if err := os.WriteFile(path, []byte(raw), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("带 BOM 配置文件必须能正常加载, 得到错误: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:9999" {
		t.Fatalf("预期 listen_addr 为 127.0.0.1:9999, 实际为 %q", cfg.ListenAddr)
	}
}

func TestBufferPool(t *testing.T) {
	buf := GetBuffer()
	if buf == nil {
		t.Fatal("GetBuffer 返回 nil")
	}
	buf.WriteString("workbuddy-intl-test")
	if buf.String() != "workbuddy-intl-test" {
		t.Fatalf("预期 'workbuddy-intl-test', 实际 %q", buf.String())
	}
	PutBuffer(buf)

	buf2 := GetBuffer()
	if buf2.Len() != 0 {
		t.Fatalf("从池复用的 buffer 应该自动 Reset, 实际长度 %d", buf2.Len())
	}
	PutBuffer(buf2)

	// 测试超过 1MB 丢弃机制
	bigBuf := GetBuffer()
	bigBuf.Grow(2 * 1024 * 1024)
	PutBuffer(bigBuf)
}

func TestStreamBufferPool(t *testing.T) {
	bufPtr := GetStreamBuffer()
	if bufPtr == nil || len(*bufPtr) != DefaultBufferSize {
		t.Fatalf("预期 buffer 大小 %d", DefaultBufferSize)
	}
	(*bufPtr)[0] = 0xAA
	PutStreamBuffer(bufPtr)
}

// ---------------------------------------------------------------------------
// 2. 精细化 GCRA 漏桶平滑削峰测试
// ---------------------------------------------------------------------------

func TestGCRA_CapacityClamping(t *testing.T) {
	// 验证 capacity < 2 会被自动提升至 2，防止形成 CV=0 的机械周期指纹
	g := NewGCRA(1.0, 1)
	if g.capacity != 2 {
		t.Fatalf("capacity 必须强制提升至 >= 2, 实际为 %d", g.capacity)
	}
}

func TestGCRA_ReserveAndCost(t *testing.T) {
	gcra := NewGCRA(10.0, 3) // 10 req/s, capacity 3

	now := time.Now()
	w1 := gcra.Reserve(now)
	w2 := gcra.Reserve(now)
	w3 := gcra.Reserve(now)

	if w1 != 0 || w2 != 0 || w3 != 0 {
		t.Fatalf("前 3 次突发应无需等待: w1=%v, w2=%v, w3=%v", w1, w2, w3)
	}

	w4 := gcra.Reserve(now)
	if w4 <= 0 {
		t.Fatalf("第 4 次突发应需要等待，但得到: %v", w4)
	}

	// 验证加权成本 ReserveCost
	wCost := gcra.ReserveCost(now, 2.0)
	if wCost <= w4 {
		t.Fatalf("加权成本 ReserveCost 应产生更大的等待时长: wCost=%v, w4=%v", wCost, w4)
	}
}

func TestGCRA_Refund(t *testing.T) {
	const rate = 10.0
	const reserved = 10.0
	const actual = 2.0

	// 不退款
	gNoRefund := NewGCRA(rate, 2)
	now := time.Now()
	gNoRefund.ReserveCost(now, reserved)
	tatA := gNoRefund.tat

	// 执行退款
	gRefund := NewGCRA(rate, 2)
	gRefund.ReserveCost(now, reserved)
	gRefund.Refund(reserved, actual)
	tatB := gRefund.tat

	if tatB >= tatA {
		t.Fatalf("Refund 应该使 TAT 回拨: tatB=%v, tatA=%v", tatB, tatA)
	}
}

func TestGCRA_UpdateFromHeaders(t *testing.T) {
	g := NewGCRA(5.0, 3)
	initialTAT := g.tat

	// 模拟剩余额度充足 (remaining=50)
	h := http.Header{}
	h.Set("anthropic-ratelimit-requests-remaining", "50")
	g.UpdateFromHeaders(h)
	if g.tat != initialTAT {
		t.Fatal("剩余额度充足时不应额外惩罚 TAT")
	}

	// 模拟剩余额度告急 (remaining=1)
	h.Set("anthropic-ratelimit-requests-remaining", "1")
	g.UpdateFromHeaders(h)
	if g.tat <= initialTAT {
		t.Fatalf("剩余额度告急时应推迟 TAT: initial=%v, updated=%v", initialTAT, g.tat)
	}
}

// ---------------------------------------------------------------------------
// 3. 拟人化抖动 (对数正态 + OU 过程 + 方差补偿) 测试
// ---------------------------------------------------------------------------

func TestJitter_VarianceCompensationAndQuantiles(t *testing.T) {
	const medianMs = 200.0
	const p95Ms = 600.0
	const floorMs = 30.0
	const ceilMs = 2500.0

	for _, ouNoise := range []float64{0.0, 0.15, 0.20} {
		j := NewJitter(medianMs, p95Ms, floorMs, ceilMs, ouNoise, 12345)
		const nSamples = 30000
		samples := make([]float64, 0, nSamples)
		for i := 0; i < nSamples; i++ {
			d := j.Sample()
			ms := float64(d) / float64(time.Millisecond)
			if ms < floorMs || ms > ceilMs {
				t.Fatalf("采样延迟 %v 越界 [%v, %v]", ms, floorMs, ceilMs)
			}
			samples = append(samples, ms)
		}

		sort.Float64s(samples)
		p50 := samples[int(0.50*float64(len(samples)-1))]
		p95 := samples[int(0.95*float64(len(samples)-1))]

		// 验证中位数 (P50) 靠近 200ms
		if math.Abs(p50-medianMs) > 25.0 {
			t.Errorf("ou=%.2f: P50=%.1fms, 预期接近 %.1fms", ouNoise, p50, medianMs)
		}

		// 验证 95分位 (P95) 在方差补偿后不膨胀，落在目标附近
		if p95 < p95Ms*0.85 || p95 > p95Ms*1.15 {
			t.Errorf("ou=%.2f: P95=%.1fms, 预期接近 %.1fms", ouNoise, p95, p95Ms)
		}
	}
}

// ---------------------------------------------------------------------------
// 4. 去相关抖动退避器测试
// ---------------------------------------------------------------------------

func TestBackoff_DecorrelatedJitter(t *testing.T) {
	b := NewBackoff(300*time.Millisecond, 200*time.Millisecond, 10*time.Second, 42)

	prev := time.Duration(0)
	for i := 0; i < 10; i++ {
		next := b.Next()
		if next < b.Floor {
			t.Fatalf("退避值 %v 低于硬下限 Floor %v", next, b.Floor)
		}
		if next > b.Ceil {
			t.Fatalf("退避值 %v 高于硬上限 Ceil %v", next, b.Ceil)
		}
		prev = next
	}
	_ = prev

	b.Reset()
	if b.prev != b.Base {
		t.Fatalf("Reset 后 prev 应该重置为 Base: %v != %v", b.prev, b.Base)
	}
}

// ---------------------------------------------------------------------------
// 5. 响应分级研判与熔断测试
// ---------------------------------------------------------------------------

func TestClassifyResponse_Hierarchy(t *testing.T) {
	// 1. 401 Unauthorized
	c401 := ClassifyResponse(http.StatusUnauthorized, nil, []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	if c401.Class != ClassAccountFatal {
		t.Fatalf("401 应研判为 ClassAccountFatal, 实际: %v", c401.Class)
	}

	// 2. 403 Account Disabled / Blocked
	c403Blocked := ClassifyResponse(http.StatusForbidden, nil, []byte(`{"type":"error","error":{"type":"permission_error","message":"Your organization has been disabled"}}`))
	if c403Blocked.Class != ClassAccountFatal {
		t.Fatalf("封号 403 应研判为 ClassAccountFatal, 实际: %v", c403Blocked.Class)
	}

	// 3. 403 Cloudflare WAF Challenge (HTML)
	c403WAF := ClassifyResponse(http.StatusForbidden, nil, []byte(`<!DOCTYPE html><html><head><title>Just a moment...</title></head><body>Verify you are human</body></html>`))
	if c403WAF.Class != ClassWAFBlocked {
		t.Fatalf("Cloudflare WAF 403 应研判为 ClassWAFBlocked, 实际: %v", c403WAF.Class)
	}

	// 4. 429 Unified Hard Wall Rejected
	hUnified := http.Header{}
	hUnified.Set("anthropic-ratelimit-unified-status", "rejected")
	futureReset := time.Now().Add(60 * time.Second).Unix()
	hUnified.Set("anthropic-ratelimit-unified-reset", fmt.Sprintf("%d", futureReset))
	c429Unified := ClassifyResponse(http.StatusTooManyRequests, hUnified, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"quota exceeded"}}`))
	if c429Unified.Class != ClassRefused {
		t.Fatalf("统一配额硬墙 429 应研判为 ClassRefused, 实际: %v", c429Unified.Class)
	}
	if c429Unified.ResetsAt.Unix() != futureReset {
		t.Fatalf("ResetsAt 应该匹配响应头时间戳: %v", c429Unified.ResetsAt)
	}

	// 5. 429 Retryable with Retry-After
	hRetry := http.Header{}
	hRetry.Set("Retry-After", "5")
	c429Retry := ClassifyResponse(http.StatusTooManyRequests, hRetry, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limit"}}`))
	if c429Retry.Class != ClassRetryable || c429Retry.RetrySpan != 5*time.Second {
		t.Fatalf("常规 429 应研判为 ClassRetryable 并解析 Retry-After 5s, 实际: %v, span: %v", c429Retry.Class, c429Retry.RetrySpan)
	}

	// 6. 529 Anthropic Overloaded
	c529 := ClassifyResponse(529, nil, []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	if c529.Class != ClassRetryable {
		t.Fatalf("529 应研判为 ClassRetryable, 实际: %v", c529.Class)
	}

	// 7. 400 Bad Request
	c400 := ClassifyResponse(http.StatusBadRequest, nil, []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages is required"}}`))
	if c400.Class != ClassNotRetryable {
		t.Fatalf("400 业务错误应研判为 ClassNotRetryable, 实际: %v", c400.Class)
	}
}

func TestCircuitBreaker_Lifecycle(t *testing.T) {
	cb := NewCircuitBreaker()
	if err := cb.CheckAllowed(); err != nil {
		t.Fatalf("初始状态应允许通行, 实际: %v", err)
	}

	// 模拟 429 限流
	resp429 := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"1"}},
	}
	cb.RecordResponse(resp429, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))

	if cb.State() != StateOpen {
		t.Fatalf("429 后熔断器应进入 StateOpen, 实际: %v", cb.State())
	}
	if err := cb.CheckAllowed(); err == nil {
		t.Fatal("StateOpen 期间应拦截请求")
	}

	// 重置
	cb.Reset()
	if err := cb.CheckAllowed(); err != nil {
		t.Fatalf("重置后应允许通行, 实际: %v", err)
	}

	// 模拟 401 封禁触发 AccountBlocked
	resp401 := &http.Response{StatusCode: http.StatusUnauthorized}
	cb.RecordResponse(resp401, []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`))
	if cb.State() != StateAccountBlocked {
		t.Fatalf("401 应导致 StateAccountBlocked, 实际: %v", cb.State())
	}
	if err := cb.CheckAllowed(); err == nil || !strings.Contains(err.Error(), "安全熔断保护") {
		t.Fatalf("AccountBlocked 状态应拦截请求并给出风险提示, 实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 6. 指纹对齐、负向清洗与 VerifyFingerprint 测试
// ---------------------------------------------------------------------------

func TestApplyAndVerifyFingerprint(t *testing.T) {
	req, _ := http.NewRequest("POST", "http://localhost/v1/messages", nil)
	// 注入脏数据与负向头
	req.Header.Set("x-forwarded-for", "1.2.3.4")
	req.Header.Set("x-real-ip", "1.2.3.4")
	req.Header.Set("via", "1.1 reverse-proxy")
	req.Header.Set("client-ip", "1.2.3.4")
	req.Header.Set("cf-connecting-ip", "1.2.3.4")
	req.Header.Set("anthropic-dangerous-direct-browser-access", "true")
	req.Header.Set("x-stainless-lang", "python")
	req.Header.Set("x-stainless-os", "windows")
	req.Header.Set("x-workbuddy-custom", "legacy")
	req.Header.Set("x-openai-dummy", "discard")

	cfg := &Config{
		EnableFingerprint: true,
		ClaudeCodeVersion: "2.1.306",
		APIKey:            "sk-ant-api03-test-secret-key-12345",
	}

	ApplyFingerprint(req, cfg)

	// 使用严格的验证器检查
	violations := VerifyFingerprint(req, "2.1.306")
	if len(violations) > 0 {
		t.Fatalf("指纹验证失败，存在以下违规项: %v", violations)
	}

	// 校验反向代理头已被彻底清洗
	if req.Header.Get("x-forwarded-for") != "" {
		t.Fatalf("x-forwarded-for 应已被清除, 实际为 %s", req.Header.Get("x-forwarded-for"))
	}
	if req.Header.Get("x-real-ip") != "" {
		t.Fatalf("x-real-ip 应已被清除, 实际为 %s", req.Header.Get("x-real-ip"))
	}
	if req.Header.Get("x-api-key") != "sk-ant-api03-test-secret-key-12345" {
		t.Fatalf("x-api-key 注入失败, 实际为 %s", req.Header.Get("x-api-key"))
	}
}

// ---------------------------------------------------------------------------
// 7. 模型映射与请求体处理测试
// ---------------------------------------------------------------------------

func TestProcessAnthropicRequest_ModelMapping(t *testing.T) {
	cfg := &Config{
		ModelMapping: map[string]string{
			"claude-3-7-sonnet": "claude-3-7-sonnet-20250219",
		},
	}

	rawJSON := `{"model":"claude-3-7-sonnet","messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequest("POST", "http://localhost/v1/messages", bytes.NewReader([]byte(rawJSON)))

	newBody, err := ProcessAnthropicRequest(req, cfg)
	if err != nil {
		t.Fatalf("处理请求失败: %v", err)
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(newBody, &parsed); err != nil {
		t.Fatalf("解析重写后的 JSON 失败: %v", err)
	}

	if parsed["model"] != "claude-3-7-sonnet-20250219" {
		t.Fatalf("模型映射重写失败, 预期 claude-3-7-sonnet-20250219, 实际: %v", parsed["model"])
	}
}

// ---------------------------------------------------------------------------
// 8. 综合端点与防风控代理链路测试
// ---------------------------------------------------------------------------

func TestProxyEndpoints_FullStealth(t *testing.T) {
	// 模拟官方 Anthropic 上游
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-forwarded-for") != "" {
			t.Errorf("上游收到了泄露的反代头 x-forwarded-for")
		}

		// 返回模拟的 RateLimit 响应头
		w.Header().Set("anthropic-ratelimit-requests-remaining", "45")
		w.Header().Set("anthropic-ratelimit-requests-limit", "50")

		switch r.URL.Path {
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-7-sonnet-latest","type":"model"}]}`))

		case "/v1/messages/count_tokens":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"input_tokens": 42}`))

		case "/v1/messages":
			bodyBytes, _ := io.ReadAll(r.Body)
			if strings.Contains(string(bodyBytes), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
				_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"delta\":{\"text\":\"Hello\"}}\n\n"))
				_, _ = w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","content":[{"type":"text","text":"Hi"}]}`))
			}

		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          upstream.URL,
		ClaudeCodeVersion:    "2.1.306",
		EnableFingerprint:    true,
		EnablePacing:         true,
		RatePerSec:           50.0,
		BurstCapacity:        5,
		EnableJitter:         true,
		JitterMedianMs:       10,
		JitterP95Ms:          30,
		EnableCircuitBreaker: true,
		EnableWarmup:         false,
	}

	handler := NewProxyHandler(cfg)
	defer handler.Close()

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// 1. 验证 /v1/stealth/status 监控端点
	reqStatus := httptest.NewRequest("GET", "/v1/stealth/status", nil)
	recStatus := httptest.NewRecorder()
	mux.ServeHTTP(recStatus, reqStatus)
	if recStatus.Code != http.StatusOK {
		t.Fatalf("预期 /v1/stealth/status 返回 200, 实际: %d", recStatus.Code)
	}
	if !strings.Contains(recStatus.Body.String(), `"circuit_breaker"`) ||
		!strings.Contains(recStatus.Body.String(), `"gcra"`) {
		t.Fatalf("stealth 状态端点缺少关键监控字段: %s", recStatus.Body.String())
	}

	// 2. 验证 /v1/messages/count_tokens 转发与指纹
	reqCount := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-3-7-sonnet","messages":[{"role":"user","content":"test"}]}`))
	reqCount.Header.Set("Content-Type", "application/json")
	reqCount.Header.Set("x-forwarded-for", "9.9.9.9") // 模拟客户端传入反代头
	recCount := httptest.NewRecorder()
	mux.ServeHTTP(recCount, reqCount)

	if recCount.Code != http.StatusOK {
		t.Fatalf("预期 count_tokens 返回 200, 实际: %d", recCount.Code)
	}

	// 3. 验证熔断触发时的 Anthropic JSON 错误封装与 Retry-After
	handler.Breaker().RecordResponse(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"3"}},
	}, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`))

	reqBlocked := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-3-7-sonnet","messages":[{"role":"user","content":"test"}]}`))
	recBlocked := httptest.NewRecorder()
	mux.ServeHTTP(recBlocked, reqBlocked)

	if recBlocked.Code != http.StatusTooManyRequests {
		t.Fatalf("熔断期间预期返回 429, 实际: %d", recBlocked.Code)
	}
	if recBlocked.Header().Get("Retry-After") == "" {
		t.Fatal("熔断 429 应包含 Retry-After 头")
	}
	if !strings.Contains(recBlocked.Body.String(), "rate_limit_error") {
		t.Fatalf("熔断错误应包装为 Anthropic 标准错误格式: %s", recBlocked.Body.String())
	}
}
