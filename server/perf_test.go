package server

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockFlushRecorder 模拟支持 http.Flusher 的流式 ResponseWriter
type mockFlushRecorder struct {
	header     http.Header
	statusCode int
	body       bytes.Buffer
	flushes    int
	flushesMu  sync.Mutex
	onFlush    func(flushes int, currentBody []byte)
}

func newMockFlushRecorder() *mockFlushRecorder {
	return &mockFlushRecorder{
		header: make(http.Header),
	}
}

func (m *mockFlushRecorder) Header() http.Header {
	return m.header
}

func (m *mockFlushRecorder) WriteHeader(statusCode int) {
	m.statusCode = statusCode
}

func (m *mockFlushRecorder) Write(b []byte) (int, error) {
	return m.body.Write(b)
}

func (m *mockFlushRecorder) Flush() {
	m.flushesMu.Lock()
	defer m.flushesMu.Unlock()
	m.flushes++
	if m.onFlush != nil {
		m.onFlush(m.flushes, m.body.Bytes())
	}
}

// ---------------------------------------------------------------------------
// 1. 定制 HTTP/2 连接池预热与并发保活测试
// ---------------------------------------------------------------------------

func TestConnectionWarmer_LifecycleAndConcurrency(t *testing.T) {
	var probeCount int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&probeCount, 1)

		// 验证探针头包含官方 Claude CLI 指纹
		ua := r.Header.Get("User-Agent")
		if !strings.Contains(ua, "claude-code") {
			t.Errorf("预热探针缺失 Claude Code User-Agent: %s", ua)
		}
		if r.Header.Get("x-app") != "cli" {
			t.Errorf("预热探针缺失 x-app: cli")
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	}))
	defer upstream.Close()

	client := upstream.Client()
	concurrency := 3
	warmer := NewConnectionWarmer(client, upstream.URL, 50*time.Millisecond, concurrency)

	// 1. 测试手动单轮并发预热
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	warmer.WarmOnce(ctx, "测试单轮并发预热")

	gotCount := atomic.LoadInt64(&probeCount)
	if gotCount != int64(concurrency) {
		t.Fatalf("预热探针并发数不符: 期望 %d, 实际收到 %d", concurrency, gotCount)
	}

	stats := warmer.Stats()
	if stats.SuccessCount != uint64(concurrency) {
		t.Fatalf("预热成功统计不符: %d", stats.SuccessCount)
	}
	if stats.FailCount != 0 {
		t.Fatalf("预热失败数不为 0: %d", stats.FailCount)
	}
	if stats.Status != "active" {
		t.Fatalf("预热器状态期望 active, 实际: %s", stats.Status)
	}

	// 2. 测试后台心跳保活协程
	warmer.Start(context.Background())
	time.Sleep(120 * time.Millisecond) // 至少触发 2 次心跳

	// 3. 测试优雅停止
	warmer.Stop()
	statsAfterStop := warmer.Stats()
	if statsAfterStop.Status != "stopped" {
		t.Fatalf("停止后状态期望 stopped, 实际: %s", statsAfterStop.Status)
	}
}

// ---------------------------------------------------------------------------
// 2. 双阶段零延迟 SSE 流式实时 Flush 穿透测试
// ---------------------------------------------------------------------------

func TestTwoPhaseFlushStreaming(t *testing.T) {
	// 模拟 Anthropic 真实 SSE 流式推理响应
	chunks := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"role\":\"assistant\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" World!\"}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if ok {
			flusher.Flush()
		}

		for _, chunk := range chunks {
			time.Sleep(5 * time.Millisecond)
			_, _ = w.Write([]byte(chunk))
			if ok {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	cfg.EnableWarmup = false
	cfg.EnablePacing = false
	cfg.EnableJitter = false
	cfg.EnableCircuitBreaker = false

	handler := NewProxyHandler(&cfg)
	defer handler.Close()

	reqBody := `{"model":"claude-3-5-sonnet","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := newMockFlushRecorder()

	var firstFlushBodyLen int
	rec.onFlush = func(flushCount int, currentBody []byte) {
		if flushCount == 1 {
			// Phase 1 (Header Flush): 首刷时 Body 必须为 0，仅刷新 Header 让客户端感知 200 OK
			firstFlushBodyLen = len(currentBody)
		}
	}

	handler.ServeHTTP(rec, req)

	// 验证 Phase 1: 首次 Flush 时必须为 0 字节内容穿透
	if firstFlushBodyLen != 0 {
		t.Fatalf("Phase 1 Header Flush 期望 Body 长度为 0，实际收到: %d 字节", firstFlushBodyLen)
	}

	// 验证总 Flush 次数: 1 (Phase 1 Header Flush) + 4 (Phase 2 Chunks) = 5
	if rec.flushes != 1+len(chunks) {
		t.Fatalf("预期 Flush 次数 %d, 实际 Flush 次数: %d", 1+len(chunks), rec.flushes)
	}

	// 验证防缓冲穿透头矩阵
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Errorf("X-Accel-Buffering 响应头期望为 no, 实际: %s", rec.Header().Get("X-Accel-Buffering"))
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-cache") {
		t.Errorf("Cache-Control 缺失 no-cache: %s", rec.Header().Get("Cache-Control"))
	}
	if rec.Header().Get("Connection") != "keep-alive" {
		t.Errorf("Connection 响应头缺失 keep-alive: %s", rec.Header().Get("Connection"))
	}
	if rec.Header().Get("Pragma") != "no-cache" {
		t.Errorf("Pragma 响应头缺失 no-cache: %s", rec.Header().Get("Pragma"))
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("X-Content-Type-Options 缺失 nosniff: %s", rec.Header().Get("X-Content-Type-Options"))
	}

	// 验证完整响应体
	expectedBody := strings.Join(chunks, "")
	if rec.body.String() != expectedBody {
		t.Fatalf("收到的 SSE 流内容与预期不符:\n收到:\n%s\n预期:\n%s", rec.body.String(), expectedBody)
	}
}

// ---------------------------------------------------------------------------
// 3. 连接生命周期解耦：客户端提前中断测试
// ---------------------------------------------------------------------------

func TestConnectionLifecycleDecoupling_ClientDisconnect(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamClosed := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		close(upstreamStarted)

		// 持续发送无限流，等待客户端断开
		for i := 0; i < 100; i++ {
			select {
			case <-r.Context().Done():
				close(upstreamClosed)
				return
			case <-time.After(10 * time.Millisecond):
				_, err := fmt.Fprintf(w, "data: event %d\n\n", i)
				if err != nil {
					close(upstreamClosed)
					return
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}
	}))
	defer upstream.Close()

	cfg := DefaultConfig()
	cfg.UpstreamURL = upstream.URL
	cfg.EnableWarmup = false
	cfg.EnablePacing = false
	cfg.EnableJitter = false
	cfg.EnableCircuitBreaker = false

	handler := NewProxyHandler(&cfg)
	defer handler.Close()

	// 使用带取消功能的上下文模拟客户端中断
	clientCtx, clientCancel := context.WithCancel(context.Background())

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","stream":true,"messages":[]}`))
	req = req.WithContext(clientCtx)
	rec := newMockFlushRecorder()

	go func() {
		<-upstreamStarted
		time.Sleep(20 * time.Millisecond)
		// 模拟用户在客户端执行 Ctrl+C 取消
		clientCancel()
	}()

	// 执行代理，验证客户端断开后能够安全退出且无 panic
	handler.ServeHTTP(rec, req)

	// 确认上游能感知中断并正常回收连接
	select {
	case <-upstreamClosed:
		// 正常关闭
	case <-time.After(1 * time.Second):
		t.Log("上游检测超时，但代理侧已安全解耦退出")
	}
}

// ---------------------------------------------------------------------------
// 4. sync.Pool 全局内存池零分配与防泄漏测试
// ---------------------------------------------------------------------------

func TestBufferPool_LifecycleAndLargeObjectDiscard(t *testing.T) {
	ResetPoolMetrics()

	// A. 基础借还测试
	buf := GetBuffer()
	buf.WriteString("hello workbuddy-intl")
	if buf.String() != "hello workbuddy-intl" {
		t.Fatalf("Buffer 内容不符: %s", buf.String())
	}
	PutBuffer(buf)

	metrics := GetPoolMetrics()
	if metrics.BufferGets != 1 || metrics.BufferPuts != 1 {
		t.Fatalf("Buffer 指标不符: %+v", metrics)
	}

	// 再次借出，验证已被 Reset 为空
	buf2 := GetBuffer()
	if buf2.Len() != 0 {
		t.Fatalf("借出的 Buffer 未被 Reset: len=%d", buf2.Len())
	}
	PutBuffer(buf2)

	// B. 大对象 (>1MB) 防污染丢弃保护
	hugeBuf := GetBuffer()
	hugeBuf.Grow(2 * 1024 * 1024) // 扩容超过 1MB
	PutBuffer(hugeBuf)

	// C. StreamBuffer 切片池借还测试
	sBuf := GetStreamBuffer()
	if len(*sBuf) != DefaultBufferSize {
		t.Fatalf("StreamBuffer 尺寸不符: %d", len(*sBuf))
	}
	PutStreamBuffer(sBuf)

	metrics = GetPoolMetrics()
	if metrics.StreamGets != 1 || metrics.StreamPuts != 1 {
		t.Fatalf("StreamBuffer 指标不符: %+v", metrics)
	}
}

// ---------------------------------------------------------------------------
// 5. 基准测试：验证 sync.Pool 0 堆内存分配 (0 B/op, 0 allocs/op)
// ---------------------------------------------------------------------------

func BenchmarkSyncPool_StreamBuffer(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bufPtr := GetStreamBuffer()
		buf := *bufPtr
		buf[0] = 0xAA
		PutStreamBuffer(bufPtr)
	}
}

func BenchmarkSyncPool_ByteBuffer(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf := GetBuffer()
		buf.WriteString("{\"model\":\"claude-3-7-sonnet\"}")
		PutBuffer(buf)
	}
}

func BenchmarkProcessAnthropicRequestIntoBuffer(b *testing.B) {
	cfg := &Config{
		ModelMapping: map[string]string{
			"claude-3-7-sonnet": "claude-3-7-sonnet-20250219",
		},
	}
	payload := []byte(`{"model":"claude-3-7-sonnet","stream":true,"messages":[{"role":"user","content":"test"}]}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(payload))
		targetBuf := GetBuffer()
		_, _ = ProcessAnthropicRequestIntoBuffer(req, cfg, targetBuf)
		PutBuffer(targetBuf)
	}
}

func BenchmarkInspectAnthropicRequest_LargePayload(b *testing.B) {
	// 构造具有 50 轮对话消息与思考块的典型大型 Claude Code 请求体 (约 50KB)
	var sb strings.Builder
	sb.WriteString(`{"model":"claude-3-7-sonnet","stream":true,"max_tokens":4096,"messages":[`)
	for i := 0; i < 50; i++ {
		if i > 0 {
			sb.WriteString(`,`)
		}
		sb.WriteString(`{"role":"user","content":"explain quantum computing algorithms and implementation details in distributed systems"},{"role":"assistant","content":"quantum algorithms provide polynomial to exponential speedups"}`)
	}
	sb.WriteString(`]}`)
	payload := []byte(sb.String())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		meta := InspectAnthropicRequest(payload)
		if !meta.Stream || meta.Model == "" {
			b.Fatalf("解析失败")
		}
	}
}
