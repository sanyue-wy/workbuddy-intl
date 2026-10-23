package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// WarmerStats 连接池预热与心跳保活器运行状态统计
type WarmerStats struct {
	TotalWarmups uint64 `json:"total_warmups"`
	SuccessCount uint64 `json:"success_count"`
	FailCount    uint64 `json:"fail_count"`
	LastWarmTime string `json:"last_warm_time"`
	LastRTTMs    int64  `json:"last_rtt_ms"`
	Concurrency  int    `json:"concurrency"`
	IntervalSec  int    `json:"interval_sec"`
	Status       string `json:"status"`
}

// ConnectionWarmer 实现 WorkBuddy 国际版上游 HTTP/2 长连接启动预热与后台保活心跳
type ConnectionWarmer struct {
	client      *http.Client
	upstreamURL string
	interval    time.Duration
	concurrency int
	stopCh      chan struct{}
	stopped     bool
	mu          sync.Mutex
	wg          sync.WaitGroup

	// 指标统计
	totalWarmups     uint64
	successCount     uint64
	failCount        uint64
	lastWarmTime     atomic.Pointer[time.Time]
	lastRTTNs        int64
	lastActivityNano int64
}

// NewConnectionWarmer 构造连接预热与保活器
func NewConnectionWarmer(client *http.Client, upstreamURL string, interval time.Duration, concurrency int) *ConnectionWarmer {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	w := &ConnectionWarmer{
		client:      client,
		upstreamURL: upstreamURL,
		interval:    interval,
		concurrency: concurrency,
		stopCh:      make(chan struct{}),
	}
	now := time.Now()
	w.lastWarmTime.Store(&now)
	w.RecordActivity()
	return w
}

// RecordActivity 记录收到真实客户端请求的活跃时间戳
func (w *ConnectionWarmer) RecordActivity() {
	atomic.StoreInt64(&w.lastActivityNano, time.Now().UnixNano())
}

// LastActivity 获取最近一次真实请求时间
func (w *ConnectionWarmer) LastActivity() time.Time {
	n := atomic.LoadInt64(&w.lastActivityNano)
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// Start 启动后台异步启动预热与周期心跳保活
func (w *ConnectionWarmer) Start(ctx context.Context) {
	// 1. 立即并发执行首次启动预热（消除终端用户首次提问的 DNS + TCP + TLS 握手延时）
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.WarmOnce(ctx, "启动首次预热")
	}()

	// 2. 周期性探活，防止 NAT 网关或上游 LB 超时关闭空闲长连接
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		ticker := time.NewTicker(w.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				// 最近 5 分钟没有真实请求时暂停心跳
				lastAct := w.LastActivity()
				if !lastAct.IsZero() && time.Since(lastAct) > 5*time.Minute {
					continue
				}
				w.WarmOnce(context.Background(), "周期心跳保活")
			case <-w.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop 优雅停止保活协程并等待在途探测结束
func (w *ConnectionWarmer) Stop() {
	w.mu.Lock()
	if !w.stopped {
		w.stopped = true
		close(w.stopCh)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

// WarmOnce 对上游执行一轮并发预热探测
func (w *ConnectionWarmer) WarmOnce(ctx context.Context, reason string) {
	atomic.AddUint64(&w.totalWarmups, 1)

	var wg sync.WaitGroup
	for i := 0; i < w.concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			if err := w.warmSingle(ctx, reason, workerID); err != nil {
				log.Printf("[Warmer Debug] 上游连接预热握手失败 (%s#%d): %v", reason, workerID, err)
			}
		}(i)
	}
	wg.Wait()

	now := time.Now()
	w.lastWarmTime.Store(&now)
}

// warmSingle 执行单条轻量级长连接探针握手
func (w *ConnectionWarmer) warmSingle(ctx context.Context, reason string, workerID int) error {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// 探测目标优先拼接 /v1/messages，确保与业务路由使用完全一致的 Host 与 TLS SNI
	targetURL, err := url.JoinPath(w.upstreamURL, "v1/messages")
	if err != nil {
		targetURL = w.upstreamURL
	}

	req, err := http.NewRequestWithContext(reqCtx, "OPTIONS", targetURL, nil)
	if err != nil {
		atomic.AddUint64(&w.failCount, 1)
		return err
	}

	// 注入轻量官方探针头，保证对齐客户端指纹并协商 HTTP/2
	req.Header.Set("User-Agent", "claude-code/2.1.306")
	req.Header.Set("x-app", "cli")
	req.Header.Set("Accept", "*/*")

	start := time.Now()
	resp, err := w.client.Do(req)
	rtt := time.Since(start)
	atomic.StoreInt64(&w.lastRTTNs, rtt.Nanoseconds())

	if err != nil {
		atomic.AddUint64(&w.failCount, 1)
		return fmt.Errorf("探针通信失败: %w", err)
	}
	defer resp.Body.Close()

	// 必须将 Body 读完并关闭，促使底层将 TCP/TLS 连接回收至 Transport 的空闲连接池 (idleConn)
	_, _ = io.Copy(io.Discard, resp.Body)
	atomic.AddUint64(&w.successCount, 1)

	log.Printf("[Warmer OK] 上游 HTTP/2 连接池预热成功 (%s#%d) <- 状态码: %d | RTT: %v",
		reason, workerID, resp.StatusCode, rtt)
	return nil
}

// Stats 返回当前预热器的运行统计快照
func (w *ConnectionWarmer) Stats() WarmerStats {
	var lastTimeStr string
	if t := w.lastWarmTime.Load(); t != nil {
		lastTimeStr = t.Format(time.RFC3339)
	}

	status := "active"
	w.mu.Lock()
	if w.stopped {
		status = "stopped"
	} else {
		lastAct := w.LastActivity()
		if !lastAct.IsZero() && time.Since(lastAct) > 5*time.Minute {
			status = "idle"
		}
	}
	w.mu.Unlock()

	return WarmerStats{
		TotalWarmups: atomic.LoadUint64(&w.totalWarmups),
		SuccessCount: atomic.LoadUint64(&w.successCount),
		FailCount:    atomic.LoadUint64(&w.failCount),
		LastWarmTime: lastTimeStr,
		LastRTTMs:    atomic.LoadInt64(&w.lastRTTNs) / int64(time.Millisecond),
		Concurrency:  w.concurrency,
		IntervalSec:  int(w.interval / time.Second),
		Status:       status,
	}
}
