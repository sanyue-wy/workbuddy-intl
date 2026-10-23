package server

import (
	"context"
	"net"
	"net/http"
	"time"
)

// NewOptimizedClient 构建面向 WorkBuddy 国际版高并发、低延迟 AI 流式转发的专属 HTTP/2 客户端
func NewOptimizedClient(cfg *Config) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second, // 建连超时 10s
		KeepAlive: 30 * time.Second, // TCP 层探活保活周期 30s
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			// 开启 TCP_NODELAY 禁用 Nagle 算法，消除小数据包合并等待延迟
			if tcpConn, ok := conn.(*net.TCPConn); ok {
				_ = tcpConn.SetNoDelay(true)
				_ = tcpConn.SetKeepAlive(true)
				_ = tcpConn.SetKeepAlivePeriod(30 * time.Second)
				_ = tcpConn.SetReadBuffer(64 * 1024)  // 优化 socket 接收缓冲
				_ = tcpConn.SetWriteBuffer(64 * 1024) // 优化 socket 发送缓冲
			}
			return conn, nil
		},
		ForceAttemptHTTP2:     true,                                                   // 强制启用 HTTP/2 多路复用 ALPN 协商
		MaxIdleConns:          cfg.MaxIdleConns,                                       // 全局最大空闲长连接池容量 (默认 200)
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,                                // 单目标上游最大空闲长连接 (默认 50)
		MaxConnsPerHost:       100,                                                    // 允许高并发连接上限
		IdleConnTimeout:       90 * time.Second,                                       // 空闲长连接在连接池保留时长 90s
		ResponseHeaderTimeout: time.Duration(cfg.ResponseHeaderTimeout) * time.Second, // 等待上游首字节响应头超时 (防挂死，默认 60s)
		TLSHandshakeTimeout:   10 * time.Second,                                       // TLS 握手超时 10s
		ExpectContinueTimeout: 1 * time.Second,
		// 关键：禁用传输层自动 gzip 压缩，防止中间代理因为等待 gzip 块而破坏 SSE 打字机流式实时性
		DisableCompression: true,
		WriteBufferSize:    32 * 1024, // 用户空间写缓冲区，降低内核系统调用开销
		ReadBufferSize:     32 * 1024, // 用户空间读缓冲区，针对 SSE 帧优化
	}

	return &http.Client{
		Transport: transport,
		// 关键：流式长连接必须为 0 超时，防止长文本推理因超时被 Go 底层强行腰斩
		Timeout: 0,
	}
}
