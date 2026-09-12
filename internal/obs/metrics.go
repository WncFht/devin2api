// 本文件实现进程内 HTTP 代理运行指标的原子计数器。
//
// Package obs 提供常驻零成本的运行观测：请求计数、错误分类、
// 流式占比和收发字节量。全部用 atomic 实现，热路径无锁。
// 参考 ccLoad 的 httpProxyRuntimeMetrics 形态——同量级代理不需要
// Prometheus 整套 exposition，JSON 快照直接喂给面板。
package obs

import (
	"sync"
	"sync/atomic"
	"time"
)

// trendBuckets 是趋势环形缓冲的分钟桶数：保留最近 60 分钟。
const trendBuckets = 60

// minuteBucket 是一分钟内的请求聚合，供趋势图使用。
type minuteBucket struct {
	minute   int64 // unix 分钟戳
	requests uint64
	errors   uint64 // 4xx/5xx 与管线前拒绝
}

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
	// bucketsMu 保护 buckets；分钟桶写入低频，普通 mutex 足够。
	bucketsMu sync.Mutex
	buckets   [trendBuckets]minuteBucket
	// procMu 保护 CPU 采样状态：cpu_percent 由相邻两次快照的 rusage 差得出。
	procMu         sync.Mutex
	lastCPUSeconds float64
	lastCPUAt      time.Time
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
	m.recordBucket(status >= 400)
}

// Reject 计入一个在进入处理管线前被拒的请求（鉴权失败/并发上限）。
func (m *Metrics) Reject() {
	m.rejected.Add(1)
	m.recordBucket(true)
}

// recordBucket 把一次请求归入当前分钟桶；桶满时循环覆盖最旧数据。
func (m *Metrics) recordBucket(isError bool) {
	minute := time.Now().Unix() / 60
	m.bucketsMu.Lock()
	defer m.bucketsMu.Unlock()
	index := int(minute % trendBuckets)
	if m.buckets[index].minute != minute {
		m.buckets[index] = minuteBucket{minute: minute}
	}
	m.buckets[index].requests++
	if isError {
		m.buckets[index].errors++
	}
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
		"trend_minutes":          m.trend(),
		"rates":                  m.rates(),
		"process":                m.process(),
	}
}

// rates 从分钟桶派生 RPM/QPS（ccLoad RPMStats 同款：current/peak/avg + QPS）。
// current 是进行中的当前分钟计数；avg 覆盖分钟环内窗口；peak 是历史单分钟峰值。
func (m *Metrics) rates() map[string]any {
	now := time.Now().Unix()
	minute := now / 60
	m.bucketsMu.Lock()
	snapshot := m.buckets
	m.bucketsMu.Unlock()
	var window, peak, current uint64
	for _, bucket := range snapshot {
		if bucket.minute == 0 {
			continue
		}
		window += bucket.requests
		if bucket.requests > peak {
			peak = bucket.requests
		}
		if bucket.minute == minute {
			current = bucket.requests
		}
	}
	// avg 除以实际覆盖的分钟数（未满 60 分钟按已运行时长计，避免启动初期被稀释）。
	elapsed := int64(time.Since(m.startedAt)/time.Minute) + 1
	if elapsed > trendBuckets {
		elapsed = trendBuckets
	}
	// QPS 用当前分钟已计请求 ÷ 本分钟已过秒数；首秒内按 1 秒防除零。
	secondsIntoMinute := now%60 + 1
	return map[string]any{
		"rpm_current": current,
		"rpm_peak":    peak,
		"rpm_avg":     float64(window) / float64(elapsed),
		"qps_current": float64(current) / float64(secondsIntoMinute),
	}
}

// trend 返回最近 60 分钟的逐分钟请求/错误数（旧→新，含零值分钟），
// 供面板直接画 sparkline，无需客户端再聚合。
func (m *Metrics) trend() []map[string]any {
	current := time.Now().Unix() / 60
	m.bucketsMu.Lock()
	snapshot := m.buckets
	m.bucketsMu.Unlock()
	out := make([]map[string]any, 0, trendBuckets)
	for minute := current - trendBuckets + 1; minute <= current; minute++ {
		bucket := snapshot[int(minute%trendBuckets)]
		point := map[string]any{"minute": minute * 60, "requests": uint64(0), "errors": uint64(0)}
		if bucket.minute == minute {
			point["requests"] = bucket.requests
			point["errors"] = bucket.errors
		}
		out = append(out, point)
	}
	return out
}
