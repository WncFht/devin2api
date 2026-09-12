// 本文件实现进程内 HTTP 代理运行指标的原子计数器。
//
// Package obs 提供常驻零成本的运行观测：请求计数、错误分类、
// 流式占比和收发字节量。全部用 atomic 实现，热路径无锁。
// 参考 ccLoad 的 httpProxyRuntimeMetrics 形态——同量级代理不需要
// Prometheus 整套 exposition，JSON 快照直接喂给面板。
package obs

import (
	"sync/atomic"
	"time"
)

// Metrics 是 /v1/* 请求的运行计数器集合。
type Metrics struct {
	active      atomic.Int64
	completed   atomic.Uint64
	rejected    atomic.Uint64 // 鉴权/并发拒绝，未进入处理管线
	okResponses atomic.Uint64
	clientErrs  atomic.Uint64
	serverErrs  atomic.Uint64
	streaming   atomic.Uint64
	buffered    atomic.Uint64
	reqBytes    atomic.Uint64
	respBytes   atomic.Uint64
	startedAt   time.Time
}

// NewMetrics 创建以启动时刻为起点的指标集合。
func NewMetrics() *Metrics {
	return &Metrics{startedAt: time.Now()}
}

// Request 是一次请求生命周期的观测句柄，begin/finish 成对使用。
type Request struct {
	metrics  *Metrics
	observed bool
	stream   bool
	reqBytes uint64
}

// Begin 计入一个新的活跃请求，返回生命周期句柄。
func (m *Metrics) Begin() *Request {
	m.active.Add(1)
	return &Request{metrics: m}
}

// Observe 在请求体读取完成后记录方向与请求大小。
func (r *Request) Observe(streaming bool, requestBodyBytes int) {
	r.observed = true
	r.stream = streaming
	if requestBodyBytes > 0 {
		r.reqBytes = uint64(requestBodyBytes)
	}
}

// Finish 在请求结束时按最终状态归类计数；responseBodyBytes 为下发字节数。
func (r *Request) Finish(status, responseBodyBytes int) {
	if r == nil || r.metrics == nil {
		return
	}
	m := r.metrics
	m.active.Add(-1)
	m.completed.Add(1)
	if r.observed {
		if r.stream {
			m.streaming.Add(1)
		} else {
			m.buffered.Add(1)
		}
	}
	m.reqBytes.Add(r.reqBytes)
	if responseBodyBytes > 0 {
		m.respBytes.Add(uint64(responseBodyBytes))
	}
	switch {
	case status >= 500:
		m.serverErrs.Add(1)
	case status >= 400:
		m.clientErrs.Add(1)
	default:
		m.okResponses.Add(1)
	}
}

// Reject 计入一个在进入处理管线前被拒的请求（鉴权失败/并发上限）。
func (m *Metrics) Reject() {
	m.rejected.Add(1)
}

// Snapshot 返回全部计数的即时快照，供 JSON 序列化给面板或 /statsz。
func (m *Metrics) Snapshot() map[string]any {
	return map[string]any{
		"uptime_seconds":         int64(time.Since(m.startedAt).Seconds()),
		"active_requests":        m.active.Load(),
		"completed_requests":     m.completed.Load(),
		"rejected_requests":      m.rejected.Load(),
		"ok_responses":           m.okResponses.Load(),
		"client_error_responses": m.clientErrs.Load(),
		"server_error_responses": m.serverErrs.Load(),
		"streaming_requests":     m.streaming.Load(),
		"non_streaming_requests": m.buffered.Load(),
		"request_body_bytes":     m.reqBytes.Load(),
		"response_body_bytes":    m.respBytes.Load(),
	}
}
