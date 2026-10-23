package server

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// AppVersion 由 main 注入，供管理面板展示
var AppVersion = "dev"

//go:embed web/index.html
var webFS embed.FS

var startedAt = time.Now()

// ---------------------------------------------------------------------------
// 日志等级与环形缓冲：支持 debug/info/warn/error 过滤，同时写到原输出与内存供面板查看
// ---------------------------------------------------------------------------

type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "debug"
	case LogLevelInfo:
		return "info"
	case LogLevelWarn:
		return "warn"
	case LogLevelError:
		return "error"
	default:
		return "info"
	}
}

var (
	currentLogLevel   = LogLevelInfo
	currentLogLevelMu sync.RWMutex
)

// ParseLogLevel 解析日志等级字符串
func ParseLogLevel(level string) LogLevel {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return LogLevelDebug
	case "info":
		return LogLevelInfo
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

// SetLogLevel 设置当前运行时的全局日志输出等级
func SetLogLevel(level string) {
	currentLogLevelMu.Lock()
	defer currentLogLevelMu.Unlock()
	currentLogLevel = ParseLogLevel(level)
}

// GetLogLevel 获取当前生效的日志等级
func GetLogLevel() LogLevel {
	currentLogLevelMu.RLock()
	defer currentLogLevelMu.RUnlock()
	return currentLogLevel
}

func detectLogLevel(text string) LogLevel {
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "ERROR") || strings.Contains(upper, "CRITICAL") || strings.Contains(upper, "FATAL") {
		return LogLevelError
	}
	if strings.Contains(upper, "WARN") {
		return LogLevelWarn
	}
	if strings.Contains(upper, "DEBUG") {
		return LogLevelDebug
	}
	return LogLevelInfo
}

type logLine struct {
	Seq  uint64 `json:"seq"`
	Time string `json:"time"`
	Text string `json:"text"`
}

type logRing struct {
	mu    sync.Mutex
	lines []logLine
	max   int
	seq   uint64
	out   io.Writer
	part  bytes.Buffer
}

var ring = &logRing{max: 500, out: os.Stderr}

// InstallLogRing 接管标准 log 输出（在 main 中尽早调用）
func InstallLogRing() {
	ring.out = log.Writer()
	log.SetOutput(ring)
}

func (r *logRing) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.part.Write(p)
	minLevel := GetLogLevel()

	for {
		b := r.part.Bytes()
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			break
		}
		lineStr := string(b[:i])
		r.part.Next(i + 1)

		if detectLogLevel(lineStr) >= minLevel {
			_, _ = r.out.Write([]byte(lineStr + "\n"))
			r.seq++
			r.lines = append(r.lines, logLine{
				Seq:  r.seq,
				Time: time.Now().Format("15:04:05"),
				Text: lineStr,
			})
		}
	}
	if len(r.lines) > r.max {
		r.lines = append([]logLine(nil), r.lines[len(r.lines)-r.max:]...)
	}
	return len(p), nil
}

func (r *logRing) since(seq uint64) []logLine {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logLine, 0, 64)
	for _, l := range r.lines {
		if l.Seq > seq {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 请求统计
// ---------------------------------------------------------------------------

type modelStat struct {
	Model    string  `json:"model"`
	Requests uint64  `json:"requests"`
	Errors   uint64  `json:"errors"`
	AvgMs    float64 `json:"avg_ms"`
	totalMs  float64
}

type reqStats struct {
	mu       sync.Mutex
	total    uint64
	errors   uint64
	bytes    int64
	totalMs  float64
	byModel  map[string]*modelStat
	byStatus map[int]uint64
	recent   []recentReq
}

type recentReq struct {
	Time   string `json:"time"`
	Path   string `json:"path"`
	Model  string `json:"model"`
	Status int    `json:"status"`
	Ms     int64  `json:"ms"`
}

var stats = &reqStats{byModel: map[string]*modelStat{}, byStatus: map[int]uint64{}}

func recordRequest(path, model string, status int, dur time.Duration, n int64) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	ms := float64(dur.Milliseconds())
	stats.total++
	stats.totalMs += ms
	stats.bytes += n
	stats.byStatus[status]++
	isErr := status >= 400 || status == 0
	if isErr {
		stats.errors++
	}
	if model == "" {
		model = "(unknown)"
	}
	m := stats.byModel[model]
	if m == nil {
		m = &modelStat{Model: model}
		stats.byModel[model] = m
	}
	m.Requests++
	m.totalMs += ms
	m.AvgMs = m.totalMs / float64(m.Requests)
	if isErr {
		m.Errors++
	}
	stats.recent = append(stats.recent, recentReq{time.Now().Format("15:04:05"), path, model, status, dur.Milliseconds()})
	if len(stats.recent) > 30 {
		stats.recent = stats.recent[len(stats.recent)-30:]
	}
}

func (s *reqStats) snapshot() map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	models := make([]modelStat, 0, len(s.byModel))
	for _, m := range s.byModel {
		models = append(models, *m)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Requests > models[j].Requests })
	st := map[string]uint64{}
	for k, v := range s.byStatus {
		st[strconv.Itoa(k)] = v
	}
	avg := 0.0
	if s.total > 0 {
		avg = s.totalMs / float64(s.total)
	}
	recent := make([]recentReq, len(s.recent))
	for i := range s.recent {
		recent[len(s.recent)-1-i] = s.recent[i]
	}
	return map[string]interface{}{
		"total": s.total, "errors": s.errors, "bytes": s.bytes, "avg_ms": avg,
		"by_model": models, "by_status": st, "recent": recent,
	}
}

// ---------------------------------------------------------------------------
// 管理接口与面板
// ---------------------------------------------------------------------------

// adminGuard 只允许本机访问，并拒绝跨站请求（防止任意网页借浏览器调用本地代理）
func adminGuard(w http.ResponseWriter, r *http.Request) bool {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		http.Error(w, "管理接口仅允许本机访问", http.StatusForbidden)
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		if u, err := url.Parse(o); err != nil || u.Host != r.Host {
			http.Error(w, "拒绝跨站请求", http.StatusForbidden)
			return false
		}
	}
	return true
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func (p *ProxyHandler) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/ui", p.handleUI)
	mux.HandleFunc("/ui/", p.handleUI)
	mux.HandleFunc("/api/overview", p.handleOverview)
	mux.HandleFunc("/api/logs", p.handleLogs)
	mux.HandleFunc("/api/breaker/reset", p.handleBreakerReset)
	mux.HandleFunc("/api/model/test", p.handleModelTest)
}

func (p *ProxyHandler) handleUI(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "面板资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	_, _ = w.Write(b)
}

func (p *ProxyHandler) handleOverview(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	cfg := p.getConfig()
	masked := ""
	if n := len(cfg.APIKey); n > 8 {
		masked = cfg.APIKey[:4] + "…" + cfg.APIKey[n-4:]
	} else if n > 0 {
		masked = "********"
	}
	maskedToken := ""
	if n := len(cfg.InboundToken); n > 6 {
		maskedToken = cfg.InboundToken[:2] + "…" + cfg.InboundToken[n-2:]
	} else if n > 0 {
		maskedToken = "******"
	}

	maskedSessionToken := ""
	if n := len(cfg.WorkBuddy.SessionToken); n > 8 {
		maskedSessionToken = cfg.WorkBuddy.SessionToken[:4] + "…" + cfg.WorkBuddy.SessionToken[n-4:]
	} else if n > 0 {
		maskedSessionToken = "********"
	}

	maskedCookie := ""
	if n := len(cfg.WorkBuddy.Cookie); n > 12 {
		maskedCookie = cfg.WorkBuddy.Cookie[:6] + "… (length=" + strconv.Itoa(n) + ")"
	} else if n > 0 {
		maskedCookie = "********"
	}

	res := map[string]interface{}{
		"version":              AppVersion,
		"uptime_sec":           int(time.Since(startedAt).Seconds()),
		"listen_addr":          cfg.ListenAddr,
		"upstream_url":         cfg.UpstreamURL,
		"api_key_set":          cfg.APIKey != "",
		"masked_key":           masked,
		"inbound_token_set":    cfg.InboundToken != "",
		"masked_inbound_token": maskedToken,
		"workbuddy_adapter": map[string]interface{}{
			"enabled":              cfg.WorkBuddy.Enabled,
			"mode":                 cfg.WorkBuddy.Mode,
			"channel_id":           cfg.WorkBuddy.ChannelID,
			"wrap_mode":            cfg.WorkBuddy.WrapMode,
			"session_token_set":    cfg.WorkBuddy.SessionToken != "",
			"masked_session_token": maskedSessionToken,
			"cookie_set":           cfg.WorkBuddy.Cookie != "",
			"masked_cookie":        maskedCookie,
			"custom_origin":        cfg.WorkBuddy.CustomOrigin,
		},
		"model_mapping":        cfg.ModelMapping,
		"log_level":            cfg.LogLevel,
		"timeout_seconds":      cfg.TimeoutSeconds,
		"max_body_mb":          cfg.MaxBodyMB,
		"buffer_size_kb":       cfg.BufferSizeKB,
		"pacing":               cfg.EnablePacing,
		"fingerprint":          cfg.EnableFingerprint,
		"rate_per_sec":         cfg.RatePerSec,
		"burst_capacity":       cfg.BurstCapacity,
		"jitter":               cfg.EnableJitter,
		"breaker_on":           p.breakerManager != nil || p.breaker != nil,
		"warmup":               cfg.EnableWarmup,
		"pool_metrics":         GetPoolMetrics(),
		"stats":                stats.snapshot(),
		"config": map[string]interface{}{
			"listen_addr":             cfg.ListenAddr,
			"upstream_url":            cfg.UpstreamURL,
			"api_key_set":             cfg.APIKey != "",
			"masked_key":              masked,
			"inbound_token_set":       cfg.InboundToken != "",
			"masked_inbound_token":    maskedToken,
			"claude_code_version":     cfg.ClaudeCodeVersion,
			"log_level":               cfg.LogLevel,
			"timeout_seconds":         cfg.TimeoutSeconds,
			"max_body_mb":             cfg.MaxBodyMB,
			"buffer_size_kb":          cfg.BufferSizeKB,
			"max_idle_conns":          cfg.MaxIdleConns,
			"max_idle_conns_per_host": cfg.MaxIdleConnsPerHost,
			"response_header_timeout": cfg.ResponseHeaderTimeout,
			"enable_warmup":           cfg.EnableWarmup,
			"warmup_interval_seconds": cfg.WarmupIntervalSeconds,
			"warmup_concurrency":      cfg.WarmupConcurrency,
			"enable_fingerprint":      cfg.EnableFingerprint,
			"enable_pacing":           cfg.EnablePacing,
			"rate_per_sec":            cfg.RatePerSec,
			"burst_capacity":          cfg.BurstCapacity,
			"enable_jitter":           cfg.EnableJitter,
			"jitter_median_ms":        cfg.JitterMedianMs,
			"jitter_p95_ms":           cfg.JitterP95Ms,
			"enable_circuit_breaker":  cfg.EnableCircuitBreaker,
			"model_mapping":           cfg.ModelMapping,
		},
	}

	if p.models != nil {
		res["models"] = p.models.GetModels()
	} else {
		res["models"] = DefaultOfflineModels
	}

	if p.breakerManager != nil {
		res["breakers"] = p.breakerManager.Stats()
		res["breaker"] = p.breakerManager.OverallStats()
	} else if p.breaker != nil {
		res["breaker"] = p.breaker.Stats()
	}

	if p.pacer != nil && p.pacer.GCRA() != nil {
		res["gcra"] = p.pacer.GCRA().Stats()
	}
	if p.warmer != nil {
		res["warmer"] = p.warmer.Stats()
	}
	writeJSON(w, res)
}

func (p *ProxyHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	since, _ := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	writeJSON(w, map[string]interface{}{"lines": ring.since(since)})
}

func (p *ProxyHandler) handleBreakerReset(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "需要 POST", http.StatusMethodNotAllowed)
		return
	}
	key := r.URL.Query().Get("key")
	if p.breakerManager != nil {
		p.breakerManager.Reset(key)
		log.Printf("[Admin] 凭据熔断器已手动重置 (key: %s)", key)
	} else if p.breaker != nil {
		p.breaker.Reset()
		log.Printf("[Admin] 熔断器已手动重置")
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (p *ProxyHandler) handleModelTest(w http.ResponseWriter, r *http.Request) {
	if !adminGuard(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "需要 POST", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	model := strings.TrimSpace(body.Model)
	if model == "" {
		model = strings.TrimSpace(r.URL.Query().Get("model"))
	}
	if model == "" {
		http.Error(w, "缺少 model 参数", http.StatusBadRequest)
		return
	}

	cfg := p.getConfig()
	testPayload := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "ping"},
		},
	}
	jsonBytes, _ := json.Marshal(testPayload)

	targetURL, err := url.JoinPath(cfg.UpstreamURL, "/v1/messages/count_tokens")
	if err != nil {
		writeJSON(w, map[string]interface{}{"status": 500, "latency_ms": 0, "error": err.Error()})
		return
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(jsonBytes))
	if err != nil {
		writeJSON(w, map[string]interface{}{"status": 500, "latency_ms": 0, "error": err.Error()})
		return
	}

	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("x-api-key", cfg.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	t0 := time.Now()
	resp, err := p.client.Do(req)
	latency := time.Since(t0).Milliseconds()
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"status":     502,
			"latency_ms": latency,
			"error":      err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	writeJSON(w, map[string]interface{}{
		"status":     resp.StatusCode,
		"latency_ms": latency,
	})
}

// modelFromBody 读取请求体中的 model 字段（仅用于统计）
func modelFromBody(b []byte) string {
	return strings.TrimSpace(InspectAnthropicRequest(b).Model)
}
