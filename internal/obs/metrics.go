// 本文件实现进程内 HTTP 代理运行指标的原子计数器。
//
// Package obs 提供常驻零成本的运行观测：请求计数、错误分类、
// 流式占比和收发字节量。全部用 atomic 实现，热路径无锁。
// 参考同类代理的运行指标形态——同量级代理不需要
// Prometheus 整套 exposition，JSON 快照直接喂给面板。
package obs

import (
	"sync"
	"sync/atomic"
	"time"
)

// trendBucketSecs 是趋势桶粒度（秒）；trendBuckets 覆盖最近 trendWindowMinutes 分钟。
const (
	trendBucketSecs    = 10
	trendWindowMinutes = 60
	trendBuckets       = 60 * trendWindowMinutes / trendBucketSecs
)

// spanBucket 是一个 10 秒窗口内的请求聚合，供趋势图使用。
type spanBucket struct {
	at       int64 // 桶起点 unix 秒（10s 对齐）
	requests uint64
	errors   uint64 // 4xx/5xx、管线前拒绝与未正常完成的已提交流（disconnected/aborted/流内失败）
}

// RejectReason 是管线前拒绝的分类；值即透出到面板与日志的标识串。
type RejectReason string

const (
	// RejectDraining 是排空期拒绝：进程即将退出，新请求 503 + Retry-After。
	RejectDraining RejectReason = "draining"
	// RejectConcurrencyLimit 是并发槽溢出：/v1/* 请求槽与 WS 轮次槽共用此分类。
	RejectConcurrencyLimit RejectReason = "concurrency_limit"
	// RejectWSConnectionLimit 是 WS 连接级准入溢出（连接槽与请求槽分开计量）。
	RejectWSConnectionLimit RejectReason = "ws_connection_limit"
	// RejectMissingAPIKey 是未携带凭据的 401。
	RejectMissingAPIKey RejectReason = "missing_api_key"
	// RejectInvalidAPIKey 是凭据不匹配的 401。
	RejectInvalidAPIKey RejectReason = "invalid_api_key"
	// RejectHTTPRead 是请求体读取失败（超时/断连）：完整请求从未到达，
	// 与鉴权/并发拒绝同口径——不产生调试目录，只留 rejected 行与环事件。
	RejectHTTPRead RejectReason = "http_read"
)

// RejectLabel 是拒绝分类与面板显示名的有序对——数组下发而非 map，
// 保住表格行的稳定排序（Go map 序随机，逐次刷新会闪动）。
type RejectLabel struct {
	Reason string `json:"reason"`
	Label  string `json:"label"`
}

// rejectLabels 是拒绝分类的面板显示名：词汇与标签同文件定义，新增分类
// 漏补标签立刻可见；JS 侧不再维护镜像表，未知 reason 回退显示原值。
var rejectLabels = []RejectLabel{
	{string(RejectDraining), "排空"},
	{string(RejectConcurrencyLimit), "并发上限"},
	{string(RejectWSConnectionLimit), "WS连接上限"},
	{string(RejectMissingAPIKey), "缺API Key"},
	{string(RejectInvalidAPIKey), "错API Key"},
	{string(RejectHTTPRead), "读体失败"},
}

// RejectEvent 是一次管线前拒绝的采样：请求未读体即被拒，没有调试目录；
// logs 留存行由 debuglog 侧同步直写（见 Reject），事件环是它的实时采样面。
type RejectEvent struct {
	At        int64  `json:"at"`
	Reason    string `json:"reason"`
	Status    int    `json:"status"`
	Path      string `json:"path,omitempty"`
	IP        string `json:"ip,omitempty"`
	KeyHash   string `json:"key_hash,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
}

// rejectEventCap 是拒绝事件环形保留条数；拒绝是边缘路径，256 条足够回溯一次洪峰。
const rejectEventCap = 256

// Metrics 是 /v1/* 请求的运行计数器集合。
type Metrics struct {
	active      atomic.Int64
	completed   atomic.Uint64
	rejected    atomic.Uint64 // 鉴权/并发/排空拒绝，未进入处理管线
	okResponses atomic.Uint64
	clientErrs  atomic.Uint64
	serverErrs  atomic.Uint64
	streaming   atomic.Uint64
	buffered    atomic.Uint64
	reqBytes    atomic.Uint64
	respBytes   atomic.Uint64
	startedAt   time.Time
	// rejectsMu 保护 rejectCounts 与 rejectRing；拒绝低频，单锁足够。
	rejectsMu    sync.Mutex
	rejectCounts map[RejectReason]uint64
	rejectRing   [rejectEventCap]RejectEvent
	rejectHead   int
	rejectSize   int
	// bucketsMu 保护 buckets；趋势桶写入低频，普通 mutex 足够。
	bucketsMu sync.Mutex
	buckets   [trendBuckets]spanBucket
	// procMu 保护 CPU 采样状态：cpu_percent 由相邻两次快照的 rusage 差得出。
	procMu         sync.Mutex
	lastCPUSeconds float64
	lastCPUAt      time.Time
	// 监听归属看门狗（main 的 watchListenOwnership）每拍写入：
	// 最近一次扫描发现的外部持有进程数，与最近一次非零扫描的 unix 秒。
	foreignListenHolders  atomic.Int64
	foreignListenLastSeen atomic.Int64
}

// NewMetrics 创建以启动时刻为起点的指标集合。
func NewMetrics() *Metrics {
	return &Metrics{startedAt: time.Now(), rejectCounts: make(map[RejectReason]uint64)}
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

// Active 返回当前在途请求数；healthz 透出供部署脚本挑空闲窗口重启。
func (m *Metrics) Active() int64 {
	return m.active.Load()
}

// Observe 在请求体读取完成后记录方向与请求大小。
func (r *Request) Observe(streaming bool, requestBodyBytes int) {
	r.observed = true
	r.stream = streaming
	if requestBodyBytes > 0 {
		r.reqBytes = uint64(requestBodyBytes)
	}
}

// Finish 在请求结束时归类计数；responseBodyBytes 为下发字节数。
// 状态计数按 HTTP status 归类（回答「返回了什么状态」）；分钟趋势桶另把
// result 非 completed 的请求计入 errors——SSE 提交 200 后断连/中止/流内失败
// 虽然对客户端是 200，对运营信号是失败（回答「请求有没有正常跑完」）。
func (r *Request) Finish(status, responseBodyBytes int, result string) {
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
	m.recordBucket(status >= 400 || (result != "" && result != "completed"))
}

// Reject 计入一个在进入处理管线前被拒的请求。reason 分类落到计数与
// 事件环上，是拒绝的实时面；同一拒绝另由 debuglog.NoteReject 同步直写
// 一条 log_source=rejected 的 logs 留存行（跨重启可检索的足迹，默认
// 视图剔除）——留存行绕开全局队列，其写库失败时这次拒绝只剩本计数与
// stderr WARN，失败数经 rejects.insert_failed 透出。
func (m *Metrics) Reject(reason RejectReason, ev RejectEvent) {
	m.rejected.Add(1)
	m.recordBucket(true)
	ev.At = time.Now().Unix()
	ev.Reason = string(reason)
	m.rejectsMu.Lock()
	m.rejectCounts[reason]++
	m.rejectRing[m.rejectHead] = ev
	m.rejectHead = (m.rejectHead + 1) % rejectEventCap
	if m.rejectSize < rejectEventCap {
		m.rejectSize++
	}
	m.rejectsMu.Unlock()
}

// recordBucket 把一次请求归入当前 10 秒桶；桶满时循环覆盖最旧数据。
func (m *Metrics) recordBucket(isError bool) {
	m.recordBucketAt(time.Now().Unix(), isError)
}

// recordBucketAt 把一次请求归入 at（unix 秒）对齐的 10 秒桶。
func (m *Metrics) recordBucketAt(at int64, isError bool) {
	at = at / trendBucketSecs * trendBucketSecs
	m.bucketsMu.Lock()
	defer m.bucketsMu.Unlock()
	index := int(at / trendBucketSecs % trendBuckets)
	if m.buckets[index].at != at {
		m.buckets[index] = spanBucket{at: at}
	}
	m.buckets[index].requests++
	if isError {
		m.buckets[index].errors++
	}
}

// SeedTrend 用一条历史请求预热趋势桶（数据来自 logs 表启动回放）。
// finishedAt 是请求完成时刻（与 Finish 实时归桶同口径：按完成而非开始时刻）；
// 落在 60 分钟窗口外的条目丢弃。
func (m *Metrics) SeedTrend(finishedAt time.Time, isError bool) {
	at := finishedAt.Unix()
	if at < time.Now().Unix()-trendWindowMinutes*60 {
		return
	}
	m.recordBucketAt(at, isError)
}

// NoteForeignListenHolders 记录一轮监听归属扫描发现的外部持有进程数；
// 非零时刷新 last_seen——「当前有野进程占位」与「曾经见过」两个口径分开。
func (m *Metrics) NoteForeignListenHolders(n int) {
	m.foreignListenHolders.Store(int64(n))
	if n > 0 {
		m.foreignListenLastSeen.Store(time.Now().Unix())
	}
}

// Snapshot 是 Metrics 全部计数的即时快照：字段即 JSON 键，直接序列化
// 给面板与 /statsz——消费侧按字段取数，键名写错在编译期爆炸而非静默零值。
type Snapshot struct {
	UptimeSeconds        int64           `json:"uptime_seconds"`
	ActiveRequests       int64           `json:"active_requests"`
	CompletedRequests    uint64          `json:"completed_requests"`
	RejectedRequests     uint64          `json:"rejected_requests"`
	OKResponses          uint64          `json:"ok_responses"`
	ClientErrorResponses uint64          `json:"client_error_responses"`
	ServerErrorResponses uint64          `json:"server_error_responses"`
	StreamingRequests    uint64          `json:"streaming_requests"`
	NonStreamingRequests uint64          `json:"non_streaming_requests"`
	RequestBodyBytes     uint64          `json:"request_body_bytes"`
	ResponseBodyBytes    uint64          `json:"response_body_bytes"`
	TrendMinutes         []TrendPoint    `json:"trend_minutes"`
	Rates                Rates           `json:"rates"`
	Process              ProcessSnapshot `json:"process"`
	Rejects              Rejects         `json:"rejects"`
	// 监听归属看门狗：未开 reuseport（或未跑看门狗）时恒为 0——
	// 没有扫描数据源不伪造「安全」，0 只表示「最近一轮没发现」。
	ForeignListenHolders  int64 `json:"foreign_listen_holders"`
	ForeignListenLastSeen int64 `json:"foreign_listen_holders_last_seen"`
}

// TrendPoint 是趋势环一个 10 秒桶的请求/错误数，面板直接铺 sparkline。
type TrendPoint struct {
	At       int64  `json:"at"`
	Requests uint64 `json:"requests"`
	Errors   uint64 `json:"errors"`
}

// Rates 是从趋势桶派生的 RPM/QPS 读数：current/peak 按自然分钟合并
// 相邻桶取值，avg 覆盖趋势环内窗口。
type Rates struct {
	RPMCurrent uint64  `json:"rpm_current"`
	RPMPeak    uint64  `json:"rpm_peak"`
	RPMAvg     float64 `json:"rpm_avg"`
	QPSCurrent float64 `json:"qps_current"`
}

// Rejects 是管线前拒绝的分原因计数、最近事件环（新在前）与面板标签表。
type Rejects struct {
	ByReason map[string]uint64 `json:"by_reason"`
	Recent   []RejectEvent     `json:"recent"`
	Labels   []RejectLabel     `json:"labels"`
}

// Snapshot 返回全部计数的即时快照，供 JSON 序列化给面板或 /statsz。
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		UptimeSeconds:         int64(time.Since(m.startedAt).Seconds()),
		ActiveRequests:        m.active.Load(),
		CompletedRequests:     m.completed.Load(),
		RejectedRequests:      m.rejected.Load(),
		OKResponses:           m.okResponses.Load(),
		ClientErrorResponses:  m.clientErrs.Load(),
		ServerErrorResponses:  m.serverErrs.Load(),
		StreamingRequests:     m.streaming.Load(),
		NonStreamingRequests:  m.buffered.Load(),
		RequestBodyBytes:      m.reqBytes.Load(),
		ResponseBodyBytes:     m.respBytes.Load(),
		TrendMinutes:          m.trend(),
		Rates:                 m.rates(),
		Process:               m.process(),
		Rejects:               m.Rejects(),
		ForeignListenHolders:  m.foreignListenHolders.Load(),
		ForeignListenLastSeen: m.foreignListenLastSeen.Load(),
	}
}

// Rejects 返回管线前拒绝的分原因计数与最近事件（新在前），供 stats 快照
// 与 /admin/logs 复用同一份数据。
// 计数是进程内存值，重启清零；跨重启痕迹是 logs 表 log_source=rejected
// 行与 stderr.log 的 "request rejected" 行（reason 字段与这里同源）。
func (m *Metrics) Rejects() Rejects {
	m.rejectsMu.Lock()
	byReason := make(map[string]uint64, len(m.rejectCounts))
	for reason, n := range m.rejectCounts {
		byReason[string(reason)] = n
	}
	recent := make([]RejectEvent, 0, m.rejectSize)
	for i := 1; i <= m.rejectSize; i++ {
		recent = append(recent, m.rejectRing[(m.rejectHead-i+rejectEventCap)%rejectEventCap])
	}
	m.rejectsMu.Unlock()
	return Rejects{ByReason: byReason, Recent: recent, Labels: rejectLabels}
}

// rates 从 10 秒桶派生 RPM/QPS（同类代理 RPM 统计同款：current/peak/avg + QPS）。
// current/peak 先按自然分钟合并相邻桶再取值，语义与分钟粒度时代一致；
// avg 覆盖趋势环内窗口。
func (m *Metrics) rates() Rates {
	now := time.Now().Unix()
	m.bucketsMu.Lock()
	snapshot := m.buckets
	m.bucketsMu.Unlock()
	var window uint64
	var minAt int64
	perMinute := map[int64]uint64{}
	// 槽位复用不即时逐出：桶只在新请求落到同槽位时被覆盖，>60min 空闲后
	// 环里全是旧数据——与 trend() 同口径加窗口谓词，过窗桶不计入
	// current/peak/avg，否则空闲代理会一直报幻影 RPM。
	windowStart := (now/trendBucketSecs - trendBuckets + 1) * trendBucketSecs
	for _, bucket := range snapshot {
		if bucket.at == 0 || bucket.at < windowStart || bucket.at > now {
			continue
		}
		window += bucket.requests
		perMinute[bucket.at/60] += bucket.requests
		if minAt == 0 || bucket.at < minAt {
			minAt = bucket.at
		}
	}
	var peak uint64
	for _, v := range perMinute {
		if v > peak {
			peak = v
		}
	}
	current := perMinute[now/60]
	// avg 分母取「进程运行分钟数」与「最早非空桶覆盖分钟数」的较大者：
	// 前者保证无预热时口径不变（空转时段照样稀释），后者覆盖 logs 表
	// 回放预热场景——窗口数据比进程老，否则 avg 会被放大几十倍。
	elapsed := int64(time.Since(m.startedAt)/time.Minute) + 1
	if minAt != 0 && (now-minAt)/60+1 > elapsed {
		elapsed = (now-minAt)/60 + 1
	}
	if elapsed > trendWindowMinutes {
		elapsed = trendWindowMinutes
	}
	// QPS 用当前分钟已计请求 ÷ 本分钟已过秒数；首秒内按 1 秒防除零。
	secondsIntoMinute := now%60 + 1
	return Rates{
		RPMCurrent: current,
		RPMPeak:    peak,
		RPMAvg:     float64(window) / float64(elapsed),
		QPSCurrent: float64(current) / float64(secondsIntoMinute),
	}
}

// trend 返回最近 60 分钟的逐 10 秒请求/错误数（旧→新，含零值桶），
// 供面板直接画 sparkline，无需客户端再聚合。
func (m *Metrics) trend() []TrendPoint {
	current := time.Now().Unix() / trendBucketSecs
	m.bucketsMu.Lock()
	snapshot := m.buckets
	m.bucketsMu.Unlock()
	out := make([]TrendPoint, 0, trendBuckets)
	for slot := current - trendBuckets + 1; slot <= current; slot++ {
		bucket := snapshot[int(slot%trendBuckets)]
		point := TrendPoint{At: slot * trendBucketSecs}
		if bucket.at == slot*trendBucketSecs {
			point.Requests = bucket.requests
			point.Errors = bucket.errors
		}
		out = append(out, point)
	}
	return out
}
