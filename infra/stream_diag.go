package infra

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// StreamDiagnostics 流诊断结构
// 跟踪流式请求中的字节数、块数等指标
type StreamDiagnostics struct {
	mu             sync.RWMutex
	BytesSent      int64             // 已发送的字节数
	BytesReceived  int64             // 已接收的字节数
	ChunksSent     int               // 已发送的块数
	ChunksReceived int               // 已接收的块数
	StartedAt      time.Time         // 流开始时间
	FirstChunkAt   time.Time         // 首块到达时间（零值表示尚未到达）
	LastChunkAt    time.Time         // 最后块到达时间
	HTTPStatus     int               // HTTP 状态码
	Headers        map[string]string // 响应头快照
}

// 诊断关注的 HTTP 响应头名称
var diagHeaders = []string{
	"cf-ray",
	"cf-cache-status",
	"x-openrouter-provider",
	"x-openrouter-model",
	"x-openrouter-id",
	"x-request-id",
	"x-vercel-id",
	"via",
	"server",
}

// NewStreamDiagnostics 创建流诊断实例
func NewStreamDiagnostics() *StreamDiagnostics {
	return &StreamDiagnostics{
		StartedAt: time.Now(),
		Headers:   make(map[string]string),
	}
}

// Record 记录流数据（增量：每次调用增加计数器）
func (d *StreamDiagnostics) Record(sent int64, received int64) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if sent > 0 {
		d.BytesSent += sent
		d.ChunksSent++
	}
	if received > 0 {
		d.BytesReceived += received
		d.ChunksReceived++
		if d.FirstChunkAt.IsZero() {
			d.FirstChunkAt = time.Now()
		}
		d.LastChunkAt = time.Now()
	}
}

// RecordSent 记录发送数据
func (d *StreamDiagnostics) RecordSent(bytes int64) {
	d.Record(bytes, 0)
}

// RecordReceived 记录接收数据
func (d *StreamDiagnostics) RecordReceived(bytes int64) {
	d.Record(0, bytes)
}

// SetHTTPStatus 设置 HTTP 状态码
func (d *StreamDiagnostics) SetHTTPStatus(status int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.HTTPStatus = status
}

// CaptureHeader 捕获诊断关注的响应头
func (d *StreamDiagnostics) CaptureHeader(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	lowerKey := strings.ToLower(key)
	for _, h := range diagHeaders {
		if lowerKey == h {
			// 截断长值（最多 120 字符）
			if len(value) > 120 {
				value = value[:120]
			}
			d.Headers[lowerKey] = value
			return
		}
	}
}

// CaptureHeaders 批量捕获响应头
func (d *StreamDiagnostics) CaptureHeaders(headers map[string]string) {
	for k, v := range headers {
		d.CaptureHeader(k, v)
	}
}

// Elapsed 返回自开始以来的持续时间
func (d *StreamDiagnostics) Elapsed() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return time.Since(d.StartedAt)
}

// TTFB 返回首字节到达时间（Time To First Byte）
// 如果首块尚未到达，返回 0
func (d *StreamDiagnostics) TTFB() time.Duration {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.FirstChunkAt.IsZero() {
		return 0
	}
	return d.FirstChunkAt.Sub(d.StartedAt)
}

// Summary 返回诊断摘要字符串
func (d *StreamDiagnostics) Summary() string {
	d.mu.RLock()
	defer d.mu.RUnlock()

	parts := []string{
		fmt.Sprintf("sent=%d bytes(%d chunks)", d.BytesSent, d.ChunksSent),
		fmt.Sprintf("recv=%d bytes(%d chunks)", d.BytesReceived, d.ChunksReceived),
		fmt.Sprintf("elapsed=%v", time.Since(d.StartedAt).Round(time.Millisecond)),
	}

	if !d.FirstChunkAt.IsZero() {
		parts = append(parts, fmt.Sprintf("ttfb=%v", d.FirstChunkAt.Sub(d.StartedAt).Round(time.Millisecond)))
	}

	if d.HTTPStatus > 0 {
		parts = append(parts, fmt.Sprintf("status=%d", d.HTTPStatus))
	}

	if len(d.Headers) > 0 {
		headerParts := make([]string, 0, len(d.Headers))
		for k, v := range d.Headers {
			headerParts = append(headerParts, k+"="+v)
		}
		parts = append(parts, "upstream=["+strings.Join(headerParts, " ")+"]")
	}

	return strings.Join(parts, " ")
}

// Reset 重置诊断计数器（保留开始时间）
func (d *StreamDiagnostics) Reset() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.BytesSent = 0
	d.BytesReceived = 0
	d.ChunksSent = 0
	d.ChunksReceived = 0
	d.FirstChunkAt = time.Time{}
	d.LastChunkAt = time.Time{}
	d.HTTPStatus = 0
	d.Headers = make(map[string]string)
	d.StartedAt = time.Now()
}
