package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LimitClass 描述上游响应的限流/拒绝分级性质
type LimitClass int

const (
	ClassSuccess      LimitClass = iota
	ClassRetryable               // 瞬时限流/过载 (普通 429 / 529)，可带抖动退避
	ClassRefused                 // 确定性硬墙 (带 unified-reset 的 429)，必须休眠至 ResetsAt
	ClassWAFBlocked              // WAF / Cloudflare 边缘人机验证 (HTML 403)，执行中度冷却避风
	ClassAccountFatal            // 账号/凭据致命异常 (401 / 封号 403)，永久熔断防止风险扩散
	ClassNotRetryable            // 业务参数非法 (400)，不重试直接回传
)

func (c LimitClass) String() string {
	switch c {
	case ClassSuccess:
		return "Success"
	case ClassRetryable:
		return "Retryable"
	case ClassRefused:
		return "Refused"
	case ClassWAFBlocked:
		return "WAFBlocked"
	case ClassAccountFatal:
		return "AccountFatal"
	case ClassNotRetryable:
		return "NotRetryable"
	default:
		return "Unknown"
	}
}

// Classification 响应分类与避风决策结果
type Classification struct {
	Class     LimitClass
	RetrySpan time.Duration
	ResetsAt  time.Time
	ErrType   string
	Reason    string
}

// BreakerState 熔断器状态枚举
type BreakerState int

const (
	StateClosed BreakerState = iota
	StateOpen
	StateHalfOpen
	StateAccountBlocked
)

func (s BreakerState) String() string {
	switch s {
	case StateClosed:
		return "Closed"
	case StateOpen:
		return "Open"
	case StateHalfOpen:
		return "Half-Open"
	case StateAccountBlocked:
		return "Account-Blocked"
	default:
		return "Unknown"
	}
}

// BreakerStats 熔断器监控指标快照
type BreakerStats struct {
	State          string    `json:"state"`
	Consecutive429 int       `json:"consecutive_429"`
	OpenUntil      time.Time `json:"open_until,omitempty"`
	BlockedReason  string    `json:"blocked_reason,omitempty"`
	TotalRequests  uint64    `json:"total_requests"`
	TotalTripped   uint64    `json:"total_tripped"`
}

// CredentialBreakerStats 按凭据划分的熔断器状态快照
type CredentialBreakerStats struct {
	Key            string    `json:"key"`
	MaskedKey      string    `json:"masked_key"`
	State          string    `json:"state"`
	Consecutive429 int       `json:"consecutive_429"`
	OpenUntil      time.Time `json:"open_until,omitempty"`
	BlockedReason  string    `json:"blocked_reason,omitempty"`
	TotalRequests  uint64    `json:"total_requests"`
	TotalTripped   uint64    `json:"total_tripped"`
}

// CredentialKey 用 x-api-key 的 SHA-256 前 8 字节（16位十六进制）作为凭据隔离键
func CredentialKey(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		trimmed = "anonymous"
	}
	h := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(h[:8])
}

// MaskAPIKey 生成用于管理面板展示的脱敏 API Key
func MaskAPIKey(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if trimmed == "" {
		return "anonymous"
	}
	n := len(trimmed)
	if n > 8 {
		return trimmed[:4] + "…" + trimmed[n-4:]
	}
	return "********"
}

// ---------------------------------------------------------------------------
// 去相关抖动退避器 (Decorrelated Jitter Backoff)
// ---------------------------------------------------------------------------

// Backoff 带硬下限的去相关抖动退避器
// 算法公式: v = min(Ceil, max(Floor, Base + rand(0, 3*prev - Base)))
type Backoff struct {
	Base  time.Duration
	Floor time.Duration
	Ceil  time.Duration
	prev  time.Duration
	r     *rand.Rand
	mu    sync.Mutex
}

// NewBackoff 创建去相关抖动退避器
func NewBackoff(base, floor, ceil time.Duration, seed int64) *Backoff {
	if base <= 0 {
		base = 400 * time.Millisecond
	}
	if floor <= 0 {
		floor = 250 * time.Millisecond
	}
	if ceil <= 0 {
		ceil = 30 * time.Second
	}
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	return &Backoff{
		Base:  base,
		Floor: floor,
		Ceil:  ceil,
		prev:  base,
		r:     rand.New(rand.NewSource(seed)),
	}
}

// Next 获取下一次重试/冷却退避时长
func (b *Backoff) Next() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()

	hi := b.prev * 3
	if hi > b.Ceil {
		hi = b.Ceil
	}
	if hi < b.Base {
		hi = b.Base
	}
	span := float64(hi - b.Base)
	v := b.Base + time.Duration(b.r.Float64()*span)
	if v < b.Floor {
		v = b.Floor
	}
	if v > b.Ceil {
		v = b.Ceil
	}
	b.prev = v
	return v
}

// Reset 在调用成功后重置退避基线
func (b *Backoff) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prev = b.Base
}

// ---------------------------------------------------------------------------
// CircuitBreaker 自适应熔断与避风器
// ---------------------------------------------------------------------------

// CircuitBreaker 针对 Anthropic 国际版协议专门定制的分级自适应熔断避风器
type CircuitBreaker struct {
	state          BreakerState
	openUntil      time.Time
	consecutive429 int
	blockedReason  string
	backoff        *Backoff
	mu             sync.RWMutex

	// 统计指标
	totalRequests uint64
	totalTripped  uint64
}

// NewCircuitBreaker 创建自适应熔断器
func NewCircuitBreaker() *CircuitBreaker {
	return &CircuitBreaker{
		state:   StateClosed,
		backoff: NewBackoff(500*time.Millisecond, 300*time.Millisecond, 60*time.Second, time.Now().UnixNano()),
	}
}

// CheckAllowed 检查当前请求是否允许通过熔断保护
func (cb *CircuitBreaker) CheckAllowed() error {
	// 读锁快路径：闭合健康状态下 0 互斥锁争用，确保高并发建连阶段零排队
	cb.mu.RLock()
	if cb.state == StateClosed {
		cb.mu.RUnlock()
		atomic.AddUint64(&cb.totalRequests, 1)
		return nil
	}
	cb.mu.RUnlock()

	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.totalRequests++
	now := time.Now()

	switch cb.state {
	case StateAccountBlocked:
		if !cb.openUntil.IsZero() && now.After(cb.openUntil) {
			cb.state = StateHalfOpen
			cb.consecutive429 = 0
			cb.openUntil = time.Time{}
			log.Printf("[CircuitBreaker] 账号熔断 30 分钟锁定到期，自动转入 Half-Open 试探状态")
			return nil
		}
		remain := time.Duration(0)
		if !cb.openUntil.IsZero() {
			remain = cb.openUntil.Sub(now).Round(time.Second)
			if remain < time.Second {
				remain = time.Second
			}
		}
		return fmt.Errorf("上游 API 凭据失效或账号被风控拒绝 (%s)，已触发安全熔断保护（剩余冷却 %v），请检查 API Key 或账号状态", cb.blockedReason, remain)

	case StateOpen:
		if now.Before(cb.openUntil) {
			remain := cb.openUntil.Sub(now).Round(time.Second)
			if remain < time.Second {
				remain = time.Second
			}
			return fmt.Errorf("上游服务触发限流/熔断保护中，请等待 %v 后重试", remain)
		}
		// 冷却期结束，转入 Half-Open 状态放行单个试探请求
		cb.state = StateHalfOpen
		log.Printf("[CircuitBreaker] 冷却期结束，熔断器进入 Half-Open 试探状态")
		return nil

	case StateHalfOpen, StateClosed:
		return nil
	}

	return nil
}

// RecordResponse 根据上游响应状态码、响应头与响应体内容更新熔断器状态
func (cb *CircuitBreaker) RecordResponse(resp *http.Response, bodyBytes []byte) {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	// 1. 网络异常/断连处理 (resp == nil)
	if resp == nil {
		if cb.state == StateHalfOpen {
			cb.state = StateOpen
			delay := cb.backoff.Next()
			cb.openUntil = time.Now().Add(delay)
			cb.totalTripped++
			log.Printf("[CircuitBreaker WARN] Half-Open 试探网络失败，重回 Open 状态 (冷却 %v)", delay)
		}
		return
	}

	// 2. 成功响应 (2xx)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		cb.consecutive429 = 0
		cb.backoff.Reset()
		if cb.state == StateHalfOpen {
			cb.state = StateClosed
			log.Printf("[CircuitBreaker INFO] Half-Open 试探成功，熔断器恢复 Closed 正常状态")
		}
		return
	}

	// 3. 分级研判异常响应
	classification := ClassifyResponse(resp.StatusCode, resp.Header, bodyBytes)

	switch classification.Class {
	case ClassAccountFatal:
		// 账号被封禁或凭据彻底无效：触发账号级熔断（30分钟后自动转为 Half-Open）
		cb.state = StateAccountBlocked
		cb.blockedReason = fmt.Sprintf("%s: %s", classification.ErrType, classification.Reason)
		cb.openUntil = time.Now().Add(30 * time.Minute)
		cb.totalTripped++
		log.Printf("[CircuitBreaker CRITICAL] 触发账号级熔断（30分钟后自动转为 Half-Open）: %s", cb.blockedReason)

	case ClassRefused:
		// 命中硬墙配额上限：精确休眠至上游重置时间戳 ResetsAt
		cb.state = StateOpen
		cb.consecutive429++
		cb.totalTripped++
		if !classification.ResetsAt.IsZero() {
			cb.openUntil = classification.ResetsAt
			log.Printf("[CircuitBreaker WARN] 命中硬墙配额上限，熔断锁定至上游重置时刻: %v (原因: %s)",
				classification.ResetsAt.Format(time.RFC3339), classification.Reason)
		} else {
			delay := cb.backoff.Next()
			cb.openUntil = time.Now().Add(delay)
			log.Printf("[CircuitBreaker WARN] 命中配额拒绝，熔断冷却 %v", delay)
		}

	case ClassWAFBlocked:
		// 命中 Cloudflare / Edge WAF 人机校验，中度冷却 15s 避免烧 IP
		cb.state = StateOpen
		cb.openUntil = time.Now().Add(15 * time.Second)
		cb.totalTripped++
		log.Printf("[CircuitBreaker WARN] 检测到边缘 WAF 人机校验阻断，开启 15s 避风冷却")

	case ClassRetryable:
		// 瞬时限流 (429) 或 Anthropic 过载 (529)
		cb.state = StateOpen
		cb.consecutive429++
		cb.totalTripped++

		var delay time.Duration
		if classification.RetrySpan > 0 {
			delay = classification.RetrySpan
		} else {
			delay = cb.backoff.Next()
		}

		cb.openUntil = time.Now().Add(delay)
		log.Printf("[CircuitBreaker WARN] 触发避风熔断 (连续异常: %d, 熔断时长: %v, 原因: %s)",
			cb.consecutive429, delay, classification.Reason)

	case ClassNotRetryable:
		// 400 业务错误，不改变熔断状态
	}
}

// Reset 手动强制重置熔断器至正常 Closed 状态
func (cb *CircuitBreaker) Reset() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.state = StateClosed
	cb.consecutive429 = 0
	cb.blockedReason = ""
	cb.openUntil = time.Time{}
	cb.backoff.Reset()
}

// State 返回当前熔断器状态
func (cb *CircuitBreaker) State() BreakerState {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.state
}

// Consecutive429 返回当前连续 429 计数
func (cb *CircuitBreaker) Consecutive429() int {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.consecutive429
}

// BlockedReason 返回账号熔断原因
func (cb *CircuitBreaker) BlockedReason() string {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.blockedReason
}

// OpenUntil 返回当前熔断解除时间
func (cb *CircuitBreaker) OpenUntil() time.Time {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.openUntil
}

// Stats 返回熔断器运行快照
func (cb *CircuitBreaker) Stats() BreakerStats {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return BreakerStats{
		State:          cb.state.String(),
		Consecutive429: cb.consecutive429,
		OpenUntil:      cb.openUntil,
		BlockedReason:  cb.blockedReason,
		TotalRequests:  cb.totalRequests,
		TotalTripped:   cb.totalTripped,
	}
}

// ---------------------------------------------------------------------------
// BreakerManager 按凭据隔离的熔断管理器 (LRU 上限)
// ---------------------------------------------------------------------------

// BreakerManager 按凭据隔离维护 CircuitBreaker，并加 LRU 上限
type BreakerManager struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*lruBreakerNode
	head     *lruBreakerNode // most recently used
	tail     *lruBreakerNode // least recently used
}

type lruBreakerNode struct {
	key       string
	maskedKey string
	breaker   *CircuitBreaker
	prev      *lruBreakerNode
	next      *lruBreakerNode
}

// NewBreakerManager 创建带 LRU 淘汰上限的凭据熔断管理器
func NewBreakerManager(capacity int) *BreakerManager {
	if capacity <= 0 {
		capacity = 1000
	}
	return &BreakerManager{
		capacity: capacity,
		items:    make(map[string]*lruBreakerNode),
	}
}

// GetBreaker 根据请求使用的 API Key 获取或创建其专属的 CircuitBreaker（LRU 更新）
func (bm *BreakerManager) GetBreaker(apiKey string) *CircuitBreaker {
	key := CredentialKey(apiKey)
	masked := MaskAPIKey(apiKey)

	bm.mu.Lock()
	defer bm.mu.Unlock()

	if node, exists := bm.items[key]; exists {
		bm.moveToHead(node)
		return node.breaker
	}

	cb := NewCircuitBreaker()
	node := &lruBreakerNode{
		key:       key,
		maskedKey: masked,
		breaker:   cb,
	}

	bm.items[key] = node
	bm.addToHead(node)

	if len(bm.items) > bm.capacity {
		bm.evictTail()
	}

	return cb
}

func (bm *BreakerManager) addToHead(node *lruBreakerNode) {
	node.next = bm.head
	node.prev = nil
	if bm.head != nil {
		bm.head.prev = node
	}
	bm.head = node
	if bm.tail == nil {
		bm.tail = node
	}
}

func (bm *BreakerManager) removeNode(node *lruBreakerNode) {
	if node.prev != nil {
		node.prev.next = node.next
	} else {
		bm.head = node.next
	}
	if node.next != nil {
		node.next.prev = node.prev
	} else {
		bm.tail = node.prev
	}
}

func (bm *BreakerManager) moveToHead(node *lruBreakerNode) {
	if bm.head == node {
		return
	}
	bm.removeNode(node)
	bm.addToHead(node)
}

func (bm *BreakerManager) evictTail() {
	if bm.tail == nil {
		return
	}
	oldTail := bm.tail
	bm.removeNode(oldTail)
	delete(bm.items, oldTail.key)
}

// Reset 重置所有或指定 key 的熔断器状态
func (bm *BreakerManager) Reset(key string) {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	if key == "" || key == "all" {
		for _, node := range bm.items {
			node.breaker.Reset()
		}
		return
	}

	if node, exists := bm.items[key]; exists {
		node.breaker.Reset()
	}
}

// Stats 列出所有活跃凭据（按最近使用排序）的熔断状态
func (bm *BreakerManager) Stats() []CredentialBreakerStats {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	res := make([]CredentialBreakerStats, 0, len(bm.items))
	curr := bm.head
	for curr != nil {
		st := curr.breaker.Stats()
		res = append(res, CredentialBreakerStats{
			Key:            curr.key,
			MaskedKey:      curr.maskedKey,
			State:          st.State,
			Consecutive429: st.Consecutive429,
			OpenUntil:      st.OpenUntil,
			BlockedReason:  st.BlockedReason,
			TotalRequests:  st.TotalRequests,
			TotalTripped:   st.TotalTripped,
		})
		curr = curr.next
	}
	return res
}

// OverallStats 返回聚合熔断统计
func (bm *BreakerManager) OverallStats() BreakerStats {
	bm.mu.Lock()
	defer bm.mu.Unlock()

	var totalReq, totalTrip uint64
	var max429 int
	state := StateClosed.String()
	var openUntil time.Time
	var blockedReason string

	curr := bm.head
	for curr != nil {
		st := curr.breaker.Stats()
		totalReq += st.TotalRequests
		totalTrip += st.TotalTripped
		if st.Consecutive429 > max429 {
			max429 = st.Consecutive429
		}
		if st.State == StateAccountBlocked.String() {
			state = st.State
			blockedReason = st.BlockedReason
		} else if st.State == StateOpen.String() && state != StateAccountBlocked.String() {
			state = st.State
			if st.OpenUntil.After(openUntil) {
				openUntil = st.OpenUntil
			}
		}
		curr = curr.next
	}

	return BreakerStats{
		State:          state,
		Consecutive429: max429,
		OpenUntil:      openUntil,
		BlockedReason:  blockedReason,
		TotalRequests:  totalReq,
		TotalTripped:   totalTrip,
	}
}

// ---------------------------------------------------------------------------
// 响应分类器 (ClassifyResponse)
// ---------------------------------------------------------------------------

// ClassifyResponse 深度对齐 Anthropic 错误特征，进行精准分级分类
func ClassifyResponse(statusCode int, h http.Header, body []byte) Classification {
	now := time.Now()
	errType := parseAnthropicErrorType(body)
	errMessage := parseAnthropicErrorMessage(body)

	// 1. 检查 401 Unauthorized
	if statusCode == http.StatusUnauthorized {
		return Classification{
			Class:   ClassAccountFatal,
			ErrType: firstNonEmpty(errType, "authentication_error"),
			Reason:  firstNonEmpty(errMessage, "invalid api key or credentials"),
		}
	}

	// 2. 检查 403 Forbidden
	if statusCode == http.StatusForbidden {
		// 检查是否为 Cloudflare / WAF 拦截页面 (HTML 载荷)
		if looksLikeHTML(body) {
			return Classification{
				Class:   ClassWAFBlocked,
				ErrType: "waf_challenge",
				Reason:  "Cloudflare / Edge WAF interstitial challenge detected",
			}
		}

		// 检查是否为账号封禁 / 权限彻底拒绝
		lowerMsg := strings.ToLower(errMessage)
		if strings.Contains(lowerMsg, "disabled") ||
			strings.Contains(lowerMsg, "blocked") ||
			strings.Contains(lowerMsg, "banned") ||
			strings.Contains(lowerMsg, "violat") ||
			errType == "permission_error" {
			return Classification{
				Class:   ClassAccountFatal,
				ErrType: firstNonEmpty(errType, "permission_error"),
				Reason:  firstNonEmpty(errMessage, "account suspended, disabled or permission revoked"),
			}
		}

		// 默认 403 视为账号鉴权失败
		return Classification{
			Class:   ClassAccountFatal,
			ErrType: firstNonEmpty(errType, "forbidden"),
			Reason:  firstNonEmpty(errMessage, "forbidden request"),
		}
	}

	// 3. 检查 429 Too Many Requests
	if statusCode == http.StatusTooManyRequests {
		// A. 优先检查 Anthropic 统一配额头 (anthropic-ratelimit-unified-*)
		if h != nil {
			unifiedStatus := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-status"))
			if unifiedStatus == "rejected" {
				rawReset := strings.TrimSpace(h.Get("anthropic-ratelimit-unified-reset"))
				if rawReset != "" {
					if sec, err := strconv.ParseInt(rawReset, 10, 64); err == nil {
						resetTime := time.Unix(sec, 0)
						if resetTime.After(now) {
							return Classification{
								Class:    ClassRefused,
								ResetsAt: resetTime,
								ErrType:  firstNonEmpty(errType, "rate_limit_error"),
								Reason:   "hard quota limit reached, locked until reset timestamp",
							}
						}
					}
				}
			}
		}

		// B. 检查通用 Retry-After 响应头
		var retrySpan time.Duration
		if h != nil {
			if ra := strings.TrimSpace(h.Get("Retry-After")); ra != "" {
				if sec, err := strconv.Atoi(ra); err == nil && sec > 0 {
					retrySpan = time.Duration(sec) * time.Second
				} else if t, err := http.ParseTime(ra); err == nil && t.After(now) {
					retrySpan = t.Sub(now)
				}
			}
		}

		return Classification{
			Class:     ClassRetryable,
			RetrySpan: retrySpan,
			ErrType:   firstNonEmpty(errType, "rate_limit_error"),
			Reason:    firstNonEmpty(errMessage, "rate limited"),
		}
	}

	// 4. 检查 529 Anthropic Overloaded (上游特定负载过高)
	if statusCode == 529 {
		return Classification{
			Class:   ClassRetryable,
			ErrType: "overloaded_error",
			Reason:  firstNonEmpty(errMessage, "anthropic api overloaded"),
		}
	}

	// 5. 检查 500, 502, 503, 504 网关错误
	if statusCode >= 500 && statusCode <= 504 {
		return Classification{
			Class:   ClassRetryable,
			ErrType: "server_error",
			Reason:  fmt.Sprintf("upstream gateway error %d", statusCode),
		}
	}

	// 6. 其余 4xx 业务错误 (如 400 Bad Request)
	if statusCode >= 400 && statusCode < 500 {
		return Classification{
			Class:   ClassNotRetryable,
			ErrType: firstNonEmpty(errType, "invalid_request_error"),
			Reason:  firstNonEmpty(errMessage, "invalid request parameters"),
		}
	}

	return Classification{
		Class: ClassSuccess,
	}
}

// parseAnthropicErrorType 解析响应体中的 error.type
func parseAnthropicErrorType(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error.Type != "" {
		return env.Error.Type
	}
	return env.Type
}

// parseAnthropicErrorMessage 解析响应体中的 error.message
func parseAnthropicErrorMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var env struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return ""
	}
	if env.Error.Message != "" {
		return env.Error.Message
	}
	return env.Message
}

func looksLikeHTML(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	n := len(b)
	if n > 512 {
		n = 512
	}
	s := strings.ToLower(strings.TrimSpace(string(b[:n])))
	return strings.HasPrefix(s, "<!doctype html") || strings.HasPrefix(s, "<html")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ErrBreakerTripped 熔断异常
var ErrBreakerTripped = errors.New("circuit breaker tripped")
