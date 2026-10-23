package server

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"
)

// DefaultBufferSize 流式传输单块缓冲区默认大小（32KB）
// 足够容纳网络单次突发帧，并在高并发下保持极低的内存占用与 0 内存逃逸
const DefaultBufferSize = 32 * 1024

// PoolMetrics 记录全局内存池获取、归还与吞吐统计
type PoolMetrics struct {
	StreamGets uint64 `json:"stream_gets"`
	StreamPuts uint64 `json:"stream_puts"`
	BufferGets uint64 `json:"buffer_gets"`
	BufferPuts uint64 `json:"buffer_puts"`
}

var (
	metricStreamGets uint64
	metricStreamPuts uint64
	metricBufferGets uint64
	metricBufferPuts uint64
)

var (
	streamBufferSize int64 = DefaultBufferSize
	streamBufferPool       = sync.Pool{
		New: func() any {
			sz := atomic.LoadInt64(&streamBufferSize)
			if sz <= 0 {
				sz = DefaultBufferSize
			}
			b := make([]byte, sz)
			return &b
		},
	}
)

// SetStreamBufferSizeKB 设置流式传输单块缓冲区大小（单位：KB）
func SetStreamBufferSizeKB(kb int) {
	if kb <= 0 {
		kb = 32
	}
	atomic.StoreInt64(&streamBufferSize, int64(kb*1024))
	streamBufferPool = sync.Pool{
		New: func() any {
			b := make([]byte, kb*1024)
			return &b
		},
	}
}

// GetStreamBufferSize 获取当前流式单块缓冲区字节大小
func GetStreamBufferSize() int {
	return int(atomic.LoadInt64(&streamBufferSize))
}

// GetStreamBuffer 从池中借出 32KB 切片指针（零 GC 分配）
func GetStreamBuffer() *[]byte {
	atomic.AddUint64(&metricStreamGets, 1)
	return streamBufferPool.Get().(*[]byte)
}

// PutStreamBuffer 归还切片指针
func PutStreamBuffer(b *[]byte) {
	if b != nil {
		atomic.AddUint64(&metricStreamPuts, 1)
		streamBufferPool.Put(b)
	}
}

// bufferPool 针对请求体解析、JSON 重写与转换的 bytes.Buffer 池
var bufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

// GetBuffer 从池中获取 bytes.Buffer 实例并重置（零 GC 分配）
func GetBuffer() *bytes.Buffer {
	atomic.AddUint64(&metricBufferGets, 1)
	buf := bufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	return buf
}

// PutBuffer 归还 bytes.Buffer
// 若单个 buffer 扩容超过 1MB 则直接丢弃，交由 GC 回收大对象，防止池被巨型 buffer 污染
func PutBuffer(buf *bytes.Buffer) {
	if buf == nil {
		return
	}
	if buf.Cap() > 1024*1024 {
		return
	}
	atomic.AddUint64(&metricBufferPuts, 1)
	buf.Reset()
	bufferPool.Put(buf)
}

// GetPoolMetrics 获取当前内存池生命周期运行统计快照
func GetPoolMetrics() PoolMetrics {
	return PoolMetrics{
		StreamGets: atomic.LoadUint64(&metricStreamGets),
		StreamPuts: atomic.LoadUint64(&metricStreamPuts),
		BufferGets: atomic.LoadUint64(&metricBufferGets),
		BufferPuts: atomic.LoadUint64(&metricBufferPuts),
	}
}

// ResetPoolMetrics 重置内存池统计计数器（主要用于基准测试或重置统计）
func ResetPoolMetrics() {
	atomic.StoreUint64(&metricStreamGets, 0)
	atomic.StoreUint64(&metricStreamPuts, 0)
	atomic.StoreUint64(&metricBufferGets, 0)
	atomic.StoreUint64(&metricBufferPuts, 0)
}

// DrainAndClose 零分配优雅消耗并关闭 Body，促成 HTTP/2 连接安全回收到 Transport 空闲连接池
func DrainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	bufPtr := GetStreamBuffer()
	defer PutStreamBuffer(bufPtr)
	buf := (*bufPtr)[:4096] // 借用 4KB 缓冲消耗残留
	_, _ = io.CopyBuffer(io.Discard, io.LimitReader(body, 4096), buf)
	_ = body.Close()
}
