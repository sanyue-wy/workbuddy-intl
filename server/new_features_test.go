package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ===========================================================================
// 1. 熔断器按凭据隔离测试
// ===========================================================================

func TestBreakerManager_CredentialIsolationAndLRU(t *testing.T) {
	bm := NewBreakerManager(3) // 容量为 3

	key1 := "sk-ant-key-user-11111111"
	key2 := "sk-ant-key-user-22222222"
	key3 := "sk-ant-key-user-33333333"
	key4 := "sk-ant-key-user-44444444"

	b1 := bm.GetBreaker(key1)
	b2 := bm.GetBreaker(key2)
	b3 := bm.GetBreaker(key3)

	if b1 == b2 || b2 == b3 {
		t.Fatal("不同 API Key 必须获取独立的 CircuitBreaker 实例")
	}

	// 模拟 key1 收到 401 触发致命封锁
	resp401 := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
	}
	body401 := []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
	b1.RecordResponse(resp401, body401)

	// key1 应被封锁
	if err := b1.CheckAllowed(); err == nil {
		t.Fatal("key1 应进入 AccountBlocked 状态")
	}

	// key2 应完全不受影响
	if err := b2.CheckAllowed(); err != nil {
		t.Fatalf("key2 凭据应完全不受 key1 的 401 影响，实际得到错误: %v", err)
	}

	// 验证 LRU 淘汰：访问 key1 使其变为最近使用，然后加入 key4，key2 应被淘汰
	_ = bm.GetBreaker(key1)
	_ = bm.GetBreaker(key4)

	stats := bm.Stats()
	if len(stats) > 3 {
		t.Fatalf("LRU 容量应限制为 3, 实际: %d", len(stats))
	}

	// 验证凭据哈希键计算
	h := sha256.Sum256([]byte(key1))
	expectedKey1Hash := hex.EncodeToString(h[:8])
	if CredentialKey(key1) != expectedKey1Hash {
		t.Fatalf("CredentialKey 必须为 SHA-256 前 8 字节 (16 字符 hex), 预期 %s 实际 %s", expectedKey1Hash, CredentialKey(key1))
	}

	// 验证掩码
	masked := MaskAPIKey(key1)
	if !strings.HasPrefix(masked, "sk-a") || !strings.HasSuffix(masked, "1111") {
		t.Fatalf("掩码格式不符合预期: %s", masked)
	}
}

func TestCircuitBreaker_AccountBlockedAutoHalfOpen(t *testing.T) {
	cb := NewCircuitBreaker()

	// 触发 401 封锁
	resp401 := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
	}
	body401 := []byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`)
	cb.RecordResponse(resp401, body401)

	if cb.State() != StateAccountBlocked {
		t.Fatalf("预期状态为 StateAccountBlocked, 实际: %v", cb.State())
	}

	// 检查当前不允许通行
	if err := cb.CheckAllowed(); err == nil {
		t.Fatal("AccountBlocked 未到期前不允许通过")
	}

	// 模拟 30 分钟过去
	cb.mu.Lock()
	cb.openUntil = time.Now().Add(-1 * time.Second)
	cb.mu.Unlock()

	// 自动转为 Half-Open
	if err := cb.CheckAllowed(); err != nil {
		t.Fatalf("30分钟到期后应自动转为 Half-Open 允许试探请求, 得到错误: %v", err)
	}
	if cb.State() != StateHalfOpen {
		t.Fatalf("当前状态应为 Half-Open, 实际: %v", cb.State())
	}
}

// ===========================================================================
// 2. 配置生效测试 (buffer_size_kb, log_level, timeout_seconds)
// ===========================================================================

func TestBufferSizeKB_Configurable(t *testing.T) {
	SetStreamBufferSizeKB(64)
	if GetStreamBufferSize() != 64*1024 {
		t.Fatalf("预期缓冲大小 64KB, 实际 %d", GetStreamBufferSize())
	}

	bufPtr := GetStreamBuffer()
	if len(*bufPtr) != 64*1024 {
		t.Fatalf("切片容量预期 64KB, 实际 %d", len(*bufPtr))
	}
	PutStreamBuffer(bufPtr)

	// 恢复默认 32KB
	SetStreamBufferSizeKB(32)
	if GetStreamBufferSize() != 32*1024 {
		t.Fatalf("预期缓冲大小 32KB, 实际 %d", GetStreamBufferSize())
	}
}

func TestLogLevel_Filtering(t *testing.T) {
	SetLogLevel("warn")
	if GetLogLevel() != LogLevelWarn {
		t.Fatalf("预期日志等级 warn, 实际 %v", GetLogLevel())
	}

	if detectLogLevel("[WorkBuddy-Intl Info] 正常日志") >= LogLevelWarn {
		t.Fatal("info 日志在 warn 级别下应被过滤")
	}
	if detectLogLevel("[WorkBuddy-Intl WARN] 警告日志") < LogLevelWarn {
		t.Fatal("warn 日志在 warn 级别下应予保留")
	}
	if detectLogLevel("[WorkBuddy-Intl Error] 错误日志") < LogLevelWarn {
		t.Fatal("error 日志在 warn 级别下应予保留")
	}

	SetLogLevel("info")
}

func TestStreamingIdleTimeout_ResetOnChunks(t *testing.T) {
	// 模拟流式上游：每次 chunk 间隔 50ms，共发 3 个 chunk (总耗时 > 100ms)
	// 设置总超时/空闲超时为 100ms：
	// 若首包后改为空闲超时，每次有数据到来就刷新空闲时钟，请求顺利完成！
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		for i := 0; i < 3; i++ {
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write([]byte("event: ping\ndata: {}\n\n"))
			flusher.Flush()
		}
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		TimeoutSeconds:       1, // 1秒空闲超时
		BufferSizeKB:         32,
		EnableCircuitBreaker: false,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	ts := httptest.NewServer(proxy)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("流式请求失败: %v", err)
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(bodyBytes), "event: ping") {
		t.Fatalf("未收到流式 chunk 内容: %s", string(bodyBytes))
	}
}

// ===========================================================================
// 3. MaxBytesReader 限制请求体大小测试
// ===========================================================================

func TestMaxBodyMB_LimitAndErrorFormat(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"ok"}`))
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		MaxBodyMB:            1, // 限制 1MB
		BufferSizeKB:         32,
		EnableCircuitBreaker: false,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	ts := httptest.NewServer(proxy)
	defer ts.Close()

	// 构造 2MB 超大请求体
	hugeBody := bytes.Repeat([]byte("A"), 2*1024*1024)
	req, _ := http.NewRequest("POST", ts.URL+"/v1/messages", bytes.NewReader(hugeBody))
	req.Header.Set("Content-Type", "application/json")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("超限请求体预期 413, 实际得到: %d", resp.StatusCode)
	}

	respBytes, _ := io.ReadAll(resp.Body)
	var errResp struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBytes, &errResp); err != nil {
		t.Fatalf("413 响应必须为 Anthropic 标准错误 JSON: %v", err)
	}
	if errResp.Error.Type != "invalid_request_error" || !strings.Contains(errResp.Error.Message, "too large") {
		t.Fatalf("错误响应格式不符合 Anthropic 规范: %s", string(respBytes))
	}
}

// ===========================================================================
// 4. 模型映射定点替换测试 (保留字段顺序与格式)
// ===========================================================================

func TestReplaceModelFixedPoint_PreservesFieldOrderAndFormat(t *testing.T) {
	mapping := map[string]string{
		"claude-3-7-sonnet": "claude-3-7-sonnet-latest",
	}

	// 构造包含自定义字段顺序、换行与空格的原始 JSON
	rawJSON := `   {
  "stream": true,
  "temperature": 0.7,
  "model": "claude-3-7-sonnet",
  "max_tokens": 4096,
  "messages": [
    {"role": "user", "content": "hello"}
  ]
}`

	newBytes, newModel, replaced := ReplaceModelFixedPoint([]byte(rawJSON), mapping)
	if !replaced {
		t.Fatal("预期命中模型映射替换")
	}
	if newModel != "claude-3-7-sonnet-latest" {
		t.Fatalf("重写模型错误: %s", newModel)
	}

	newStr := string(newBytes)
	// 验证 "claude-3-7-sonnet" 被替换
	if !strings.Contains(newStr, `"claude-3-7-sonnet-latest"`) {
		t.Fatal("未找到替换后的模型名")
	}

	// 验证字段顺序与原始格式未被打乱 (stream 依然在 temperature 前面，model 依然在 max_tokens 前面)
	idxStream := strings.Index(newStr, `"stream"`)
	idxTemp := strings.Index(newStr, `"temperature"`)
	idxModel := strings.Index(newStr, `"model"`)
	idxMaxTokens := strings.Index(newStr, `"max_tokens"`)
	idxMessages := strings.Index(newStr, `"messages"`)

	if !(idxStream < idxTemp && idxTemp < idxModel && idxModel < idxMaxTokens && idxMaxTokens < idxMessages) {
		t.Fatalf("定点替换破坏了原始 JSON 字段顺序: %s", newStr)
	}

	// 验证未命中的模型保持完全不变
	unmappedJSON := `{"max_tokens":100,"model":"unknown-custom-model","stream":false}`
	outBytes, _, replaced2 := ReplaceModelFixedPoint([]byte(unmappedJSON), mapping)
	if replaced2 {
		t.Fatal("未配置映射的模型不应被替换")
	}
	if string(outBytes) != unmappedJSON {
		t.Fatalf("未映射的 JSON 必须 100%% 保持字节相同: 得到 %s", string(outBytes))
	}
}

// ===========================================================================
// 5. 起搏与抖动：默认关闭且只作用于 /v1/messages
// ===========================================================================

func TestPacer_DefaultFalseAndEndpointScoping(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EnablePacing != false || cfg.EnableJitter != false {
		t.Fatalf("起搏与抖动默认必须为 false")
	}

	if !isMessagesEndpoint("/v1/messages") || !isMessagesEndpoint("/messages") {
		t.Fatal("/v1/messages 应判定为 messages 端点")
	}
	if isMessagesEndpoint("/v1/messages/count_tokens") || isMessagesEndpoint("/messages/count_tokens") {
		t.Fatal("count_tokens 不应判定为 messages 端点")
	}
}

// ===========================================================================
// 6. 指纹改写默认关闭与合规调整测试
// ===========================================================================

func TestFingerprint_DefaultFalseAndNoSpoofing(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.EnableFingerprint != false {
		t.Fatalf("指纹改写默认必须为 false")
	}

	req, _ := http.NewRequest("POST", "http://localhost/v1/messages", nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 Custom Client")
	req.Header.Set("x-forwarded-for", "1.1.1.1")
	req.Header.Set("x-custom-proxy-hop", "proxy")

	// 开启指纹清洗
	cfg.EnableFingerprint = true
	ApplyFingerprint(req, &cfg)

	// 验证仅清洗转发头，不伪造任何身份头
	if req.Header.Get("x-forwarded-for") != "" {
		t.Fatal("x-forwarded-for 必须被清除")
	}
	if req.Header.Get("User-Agent") != "Mozilla/5.0 Custom Client" {
		t.Fatalf("客户端原始 User-Agent 应原样保留，不得伪造: 实际 %s", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("x-app") != "" {
		t.Fatalf("不得伪造注入 x-app 头")
	}
	if req.Header.Get("x-anthropic-billing-header") != "" {
		t.Fatalf("不得伪造注入 x-anthropic-billing-header")
	}
}

// ===========================================================================
// 7. inbound_token 鉴权与回环绑定校验测试
// ===========================================================================

func TestInboundToken_AuthAndLoopbackEnforcement(t *testing.T) {
	// 回环地址校验
	if !IsLoopbackAddr("127.0.0.1:8787") || !IsLoopbackAddr("localhost:8787") || !IsLoopbackAddr("[::1]:8787") {
		t.Fatal("127.0.0.1, localhost, [::1] 必须研判为回环地址")
	}
	if IsLoopbackAddr("0.0.0.0:8787") || IsLoopbackAddr(":8787") || IsLoopbackAddr("192.168.1.100:8787") {
		t.Fatal("0.0.0.0, :8787, 局域网 IP 不得研判为回环地址")
	}

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_ok"}`))
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		InboundToken:         "secret-proxy-token-123456",
		BufferSizeKB:         32,
		EnableCircuitBreaker: false,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	ts := httptest.NewServer(proxy)
	defer ts.Close()

	// 1. 未携带 X-Proxy-Token 访问 /v1/messages -> 401
	req1, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := ts.Client().Do(req1)
	if err != nil {
		t.Fatal(err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未携带令牌预期 401, 得到 %d", resp1.StatusCode)
	}

	// 2. 携带错误令牌 -> 401
	req2, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Proxy-Token", "wrong-token")
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误令牌预期 401, 得到 %d", resp2.StatusCode)
	}

	// 3. 携带正确令牌 -> 200
	req3, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Proxy-Token", "secret-proxy-token-123456")
	resp3, err := ts.Client().Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("正确令牌预期 200, 得到 %d", resp3.StatusCode)
	}
}

// ===========================================================================
// 8. 逐跳头 (Hop-by-hop) 过滤测试
// ===========================================================================

func TestHopByHopHeaders_FilteredOnSuccessAndError(t *testing.T) {
	if !isHopByHopHeader("Connection") || !isHopByHopHeader("Keep-Alive") ||
		!isHopByHopHeader("Transfer-Encoding") || !isHopByHopHeader("Upgrade") ||
		!isHopByHopHeader("Proxy-Authenticate") || !isHopByHopHeader("Content-Length") {
		t.Fatal("必须将所有标准逐跳头与 Proxy-* 头识别为 Hop-by-hop")
	}

	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 上游注入多种逐跳头
		w.Header().Set("Connection", "close")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("Proxy-Custom-Header", "bad")
		w.Header().Set("X-Valid-Header", "preserved")

		if strings.Contains(r.URL.Path, "err") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"bad"}`))
		} else {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:           "127.0.0.1:0",
		UpstreamURL:          mockUpstream.URL,
		BufferSizeKB:         32,
		EnableCircuitBreaker: false,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	ts := httptest.NewServer(proxy)
	defer ts.Close()

	// 校验成功响应逐跳头过滤
	reqOk, _ := http.NewRequest("POST", ts.URL+"/v1/messages", strings.NewReader(`{"model":"claude-3-5-sonnet","messages":[]}`))
	reqOk.Header.Set("Content-Type", "application/json")
	respOk, err := ts.Client().Do(reqOk)
	if err != nil {
		t.Fatal(err)
	}
	defer respOk.Body.Close()

	if respOk.Header.Get("Proxy-Custom-Header") != "" {
		t.Fatal("成功响应未过滤 Proxy-* 逐跳头")
	}
	if respOk.Header.Get("X-Valid-Header") != "preserved" {
		t.Fatal("正常业务头应被保留")
	}
}

// ===========================================================================
// 9. 连接预热 5 分钟无真实请求暂停心跳测试
// ===========================================================================

func TestConnectionWarmer_PauseHeartbeatWhenIdle(t *testing.T) {
	client := &http.Client{}
	warmer := NewConnectionWarmer(client, "http://localhost", 50*time.Millisecond, 1)

	// 初始状态活跃
	if warmer.Stats().Status != "active" {
		t.Fatalf("初始状态应为 active")
	}

	// 模拟超过 5 分钟无活动
	warmer.mu.Lock()
	warmer.lastActivityNano = time.Now().Add(-6 * time.Minute).UnixNano()
	warmer.mu.Unlock()

	if warmer.Stats().Status != "idle" {
		t.Fatalf("超过 5 分钟无请求状态应为 idle, 实际: %s", warmer.Stats().Status)
	}

	// 真实请求到达记录活跃，立刻恢复 active
	warmer.RecordActivity()
	if warmer.Stats().Status != "active" {
		t.Fatalf("请求到达后应恢复 active")
	}
}

// ===========================================================================
// 10. 统一端口 8787 与动态模型列表测试
// ===========================================================================

func TestModels_UnifiedPortAndDynamicRefresh(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ListenAddr != "127.0.0.1:8780" {
		t.Fatalf("默认端口必须统一为 8780: %s", cfg.ListenAddr)
	}

	// 验证离线模型不硬编码旧日期
	for _, m := range DefaultOfflineModels {
		if strings.Contains(m.ID, "2024") || strings.Contains(m.ID, "2025") {
			t.Fatalf("默认在售模型 ID 不应硬编码具体日期后缀: %s", m.ID)
		}
	}

	// 模拟上游模型列表接口
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-next-gen","type":"model","display_name":"Claude Next Gen"}]}`))
	}))
	defer mockUpstream.Close()

	client := &http.Client{}
	mm := NewModelManager(client, mockUpstream.URL, "test-key", 1*time.Hour)
	defer mm.Stop()

	// 刷新模型
	if err := mm.Refresh(context.Background()); err != nil {
		t.Fatalf("模型刷新失败: %v", err)
	}

	jsonBytes := mm.GetModelsJSON()
	if !strings.Contains(string(jsonBytes), "claude-next-gen") {
		t.Fatalf("动态刷新后的模型未进入缓存: %s", string(jsonBytes))
	}
}

// ===========================================================================
// 11. 管理面板参数与模型测试接口
// ===========================================================================

func TestAdminAPI_ReadOnlyConfigAndModelTest(t *testing.T) {
	mockUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages/count_tokens" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"input_tokens": 10}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mockUpstream.Close()

	cfg := &Config{
		ListenAddr:     "127.0.0.1:0",
		UpstreamURL:    mockUpstream.URL,
		APIKey:         "sk-ant-admin-test-key-9999",
		InboundToken:   "my-proxy-token",
		LogLevel:       "debug",
		TimeoutSeconds: 120,
		MaxBodyMB:      16,
		BufferSizeKB:   32,
	}

	proxy := NewProxyHandler(cfg)
	defer proxy.Close()

	ts := httptest.NewServer(proxy)
	defer ts.Close()

	client := ts.Client()

	// 1. GET /api/overview 校验只读脱敏配置 (携带 X-Proxy-Token)
	reqOverview, _ := http.NewRequest("GET", ts.URL+"/api/overview", nil)
	reqOverview.Header.Set("X-Proxy-Token", "my-proxy-token")
	respOverview, err := client.Do(reqOverview)
	if err != nil {
		t.Fatal(err)
	}
	defer respOverview.Body.Close()

	if respOverview.StatusCode != http.StatusOK {
		t.Fatalf("预期 200 OK, 得到 %d", respOverview.StatusCode)
	}

	var ovData struct {
		Config map[string]interface{} `json:"config"`
		Models []ModelInfo            `json:"models"`
	}
	_ = json.NewDecoder(respOverview.Body).Decode(&ovData)

	if ovData.Config["log_level"] != "debug" {
		t.Fatalf("只读参数未正确反映 log_level: %v", ovData.Config["log_level"])
	}
	if ovData.Config["masked_key"] == "sk-ant-admin-test-key-9999" {
		t.Fatalf("API Key 未脱敏泄露！")
	}

	// 2. POST /api/model/test 校验测试模型接口 (携带 X-Proxy-Token)
	testReqBody := `{"model":"claude-3-7-sonnet"}`
	reqTest, _ := http.NewRequest("POST", ts.URL+"/api/model/test", strings.NewReader(testReqBody))
	reqTest.Header.Set("Content-Type", "application/json")
	reqTest.Header.Set("X-Proxy-Token", "my-proxy-token")
	respTest, err := client.Do(reqTest)
	if err != nil {
		t.Fatal(err)
	}
	defer respTest.Body.Close()

	var testResult struct {
		Status    int   `json:"status"`
		LatencyMs int64 `json:"latency_ms"`
	}
	_ = json.NewDecoder(respTest.Body).Decode(&testResult)

	if testResult.Status != http.StatusOK {
		t.Fatalf("模型连通性测试预期 200, 实际: %d", testResult.Status)
	}
	if testResult.LatencyMs < 0 {
		t.Fatalf("延迟必须为非负数")
	}
}
