// 本文件把 index.jsonl 的请求摘要在内存中聚合成多维统计，供面板消费。
//
// 设计要点：
//   - 启动时回放 index.jsonl 尾部（上限 usageReplayTailBytes），进程重启不丢历史口径；
//   - 请求完成时 appendIndex 顺带累加，热路径只有一次 mutex 下的计数更新；
//   - 延迟用定长蓄水池（最近 usageSampleCapacity 条）算 p50/p95/p99，
//     比均值更能暴露上游长尾；
//   - 天聚合按本地时区；10 分钟环保留最近 usageMinBuckets 个桶（8 天）。
package debuglog

import (
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// usageReplayTailBytes 是启动回放读取 index.jsonl 的尾部上限。
	usageReplayTailBytes = 64 << 20
	// usageSampleCapacity 是延迟蓄水池容量（最近 N 条完成请求）。
	usageSampleCapacity = 4096
	// usageMinBuckets 是细粒度趋势保留的 10 分钟桶数（8 天）。
	usageMinBuckets = 6 * 24 * 8
	// usageMaxDays 是天聚合输出的天数上限。
	usageMaxDays = 31
)

// usageTotals 是一组请求的基础累计量。
type usageTotals struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`       // status>=400 或 result=failed
	Disconnected int64 `json:"disconnected"` // 含 aborted
	// RateLimited 是上游返回 429 的次数。本地并发拒绝在 recorder 创建前
	// 返回、不进 index，故此处纯为上游限流语义。
	RateLimited int64 `json:"rate_limited"`
	// ClientFaults/UpstreamFaults 是 errorOwner 归因计数：调用方责任
	//（断连/中断/请求体阶段失败）与服务端责任（上游错误与代理自身
	// 失败）分列——SLA 口径只把后者算作失分。
	ClientFaults   int64 `json:"client_faults"`
	UpstreamFaults int64 `json:"upstream_faults"`
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
	CacheRead      int64 `json:"cache_read_tokens"`
	CacheWrite     int64 `json:"cache_write_tokens"`
	Reasoning      int64 `json:"reasoning_tokens"`
	TotalTokens    int64 `json:"total_tokens"`
	// GenMS/GenOut 是 decode 速率的分母分子：只累计「可信流式」条目
	// （见 decodeWindow），前端用 gen_tokens/gen_ms 求 tok/s。
	GenMS  int64 `json:"gen_ms,omitempty"`
	GenOut int64 `json:"gen_tokens,omitempty"`
}

// maxPlausibleDecodeTPS 是单条请求表面 decode 速率的上限（tok/s）。
// 上游（swe-2 走 Devin 协议）常把整段结果集中在末帧突发下发，
// duration−TTFB≈0 除出的速率是测量伪影而非真实速度；当前模型真实
// decode 峰值约 300+，取 400 作为可信边界。
const maxPlausibleDecodeTPS = 400

// decodeWindow 返回该条目的（输出 token 数, 生成毫秒）；无首帧时间戳、
// 无输出 token 或表面速率超过物理上限（突发下发）时不进速率统计。
func decodeWindow(e IndexEntry) (int64, int64, bool) {
	if e.Result != "completed" || e.FirstUpstreamMS == nil || e.OutputTokens <= 0 {
		return 0, 0, false
	}
	gen := max(e.DurationMS-*e.FirstUpstreamMS, 0)
	if gen <= 0 || e.OutputTokens*1000 > maxPlausibleDecodeTPS*gen {
		return 0, 0, false
	}
	return e.OutputTokens, gen, true
}

// ErrorOwner 把一条索引记录按失败责任归因（对齐 sub2api 的 error_owner +
// is_business_limited 双标记，压缩成单维三值）。面板经 matrix 条目的
// owner 字段直接消费，JS 不再复刻这份判定。
//   - "client"：客户端断连/面板中断，或请求体读取与解码阶段的失败——
//     还没碰到上游，责任在调用方；
//   - "business_limited"：429（本地闩快败或上游限流）——配额动作不是
//     服务质量故障，SLA 分母剔除；
//   - "upstream"：其余失败（上游 5xx/语义错误/transport 断裂/代理自身
//     编码失败）——SLA 口径里唯一算失分的类别；
//   - ""：非失败请求。
//
// isRateLimited 判定索引行是否被限流语义终结：HTTP 429（上游真拒或本地
// 闸门快败），或 200+流内错误事件下发的限流——后者靠 index 的
// rate_limited 标记认出（recorder 在记录错误时按文案语义置位）。
func isRateLimited(e IndexEntry) bool {
	return e.StatusCode == 429 || e.RateLimited
}

// 判定只用索引字段（result/status/error_stage/rate_limited），回放旧索引
// 行同样可归类——旧行无 rate_limited 字段，流内限流仍按 upstream 归。
func ErrorOwner(e IndexEntry) string {
	if isRateLimited(e) {
		return "business_limited"
	}
	if e.Result == "disconnected" || e.Result == "aborted" {
		return "client"
	}
	if e.StatusCode < 400 && e.Result != "failed" {
		return ""
	}
	if e.ErrorStage == ErrStageHTTPRead || e.ErrorStage == ErrStageHTTPDecode {
		return "client"
	}
	return "upstream"
}

// add 把一条索引行计入累计。
func (t *usageTotals) add(e IndexEntry) {
	t.Requests++
	// 断连/中止先按结果归类：预提交断连的错误状态码（500/499）是
	// 「没写出去」的占位而非服务端失分，按状态码先判会把断连误计为
	// Errors 且永远到不了 Disconnected 分支。
	switch {
	case e.Result == "disconnected" || e.Result == "aborted":
		t.Disconnected++
	case e.StatusCode >= 400 || e.Result == "failed":
		t.Errors++
	}
	switch ErrorOwner(e) {
	case "client":
		t.ClientFaults++
	case "upstream":
		t.UpstreamFaults++
	}
	if isRateLimited(e) {
		t.RateLimited++
	}
	t.InputTokens += e.InputTokens
	t.OutputTokens += e.OutputTokens
	t.CacheRead += e.CacheReadTokens
	t.CacheWrite += e.CacheWriteTokens
	t.Reasoning += e.ReasoningTokens
	t.TotalTokens += e.TotalTokens
	// 只计可信流式条目：失败/断连的耗时段含非生成分量，突发下发的
	// 表面速率是伪影，混入都会污染均速。
	if out, gen, ok := decodeWindow(e); ok {
		t.GenMS += gen
		t.GenOut += out
	}
}

// usageMinSampleCap 是单桶内保留的延迟样本上限；超出后循环覆盖最旧样本。
const usageMinSampleCap = 256

// usageMinBucket 是一个 10 分钟窗口内的请求/token 聚合，供趋势图。
// 计数面直接内嵌 usageTotals：面板按时间范围选择器截一段桶求和，
// 即可得到该窗口的完整卡片数据（含断连/缓存写/推理 token）。
type usageMinBucket struct {
	at int64 // 桶起点 unix 秒（600s 对齐）
	usageTotals
	durs     []int64 // duration_ms 样本（环形，上限 usageMinSampleCap）
	durHead  int
	ttfbs    []int64 // first_upstream_ms 样本
	ttfbHead int
}

// pushSample 向容量受限的样本切片追加；满后原地覆盖最旧值。
func pushSample(samples *[]int64, head *int, v int64) {
	if len(*samples) < usageMinSampleCap {
		*samples = append(*samples, v)
		return
	}
	(*samples)[*head] = v
	*head = (*head + 1) % usageMinSampleCap
}

// usageMinPoint 是输出给面板的 10 分钟粒度数据点。
// 计数面内嵌 usageTotals（与 usageMinBucket 同型，整块拷贝成点），
// 附带每桶样本算出的延迟摘要。
type usageMinPoint struct {
	At int64 `json:"at"` // 桶起点 unix 秒
	usageTotals
	AvgDur  int64 `json:"avg_duration_ms"`
	DurP95  int64 `json:"duration_p95_ms"`
	AvgTTFB int64 `json:"avg_ttfb_ms"`
	TTFBP95 int64 `json:"ttfb_p95_ms"`
}

// dimensionAgg 是按模型或 key 哈希聚合的行。
// 计数面内嵌 usageTotals，本 struct 只保留维度特有的延迟/分位数/末态字段。
type dimensionAgg struct {
	Name string `json:"name"`
	usageTotals
	SumDuration int64   `json:"-"`
	TTFBSamples int64   `json:"-"`
	SumTTFB     int64   `json:"-"`
	LastResult  string  `json:"last_result,omitempty"`
	LastStatus  int     `json:"last_status,omitempty"`
	LastAt      string  `json:"last_at,omitempty"`
	AvgDuration float64 `json:"avg_duration_ms"`
	AvgTTFB     float64 `json:"avg_ttfb_ms"`
	SuccessRate float64 `json:"success_rate"`
	// SLASuccessRate 是服务端口径成功率：分母剔除客户端责任与 429
	// 限流条目，剩余请求中 upstream 失分占比取反——回答「服务本身
	// 可靠吗」而不是「客户端有没有正确使用」。
	SLASuccessRate float64 `json:"sla_success_rate"`
	// InTok/OutTok 分位数描述该模型的请求体量分布：计费与上下文窗口
	// 占用都跟长度强相关，均值会掩盖长尾（见 dimensionSampleCapacity）。
	InTokP50  int64 `json:"input_p50,omitempty"`
	InTokP95  int64 `json:"input_p95,omitempty"`
	OutTokP50 int64 `json:"output_p50,omitempty"`
	OutTokP95 int64 `json:"output_p95,omitempty"`
	// inTokSamples/outTokSamples 是 token 数蓄水池（最近 N 条），
	// 序列化维度行时经 stats() 折叠成上面的分位数字段。
	inTokSamples  *sampleRing `json:"-"`
	outTokSamples *sampleRing `json:"-"`
}

// dimensionSampleCapacity 是单维度行 token 样本蓄水池容量。
const dimensionSampleCapacity = 2048

// latencyStats 是蓄水池算出的延迟分布。
type latencyStats struct {
	Samples int64 `json:"samples"`
	P50     int64 `json:"p50"`
	P90     int64 `json:"p90"`
	P95     int64 `json:"p95"`
	P99     int64 `json:"p99"`
	Avg     int64 `json:"avg"`
	Max     int64 `json:"max"`
}

// usageDayRow 是单日聚合。
type usageDayRow struct {
	Date string `json:"date"`
	usageTotals
}

// rateLimitEvent 是一次 429 的采样：发生时刻（≈请求完成时刻）、模型、
// 以及该时刻前 60s 内启动的请求数。stage 区分来源——rate_gate 是本地
// 闸门快败（RPM 读数为到达速率，含被拒），其余是上游真 429（RPM 近似
// 上游实际收到的发送速率）；上游行的 RPM 峰值即限流阈值观测下界。
type rateLimitEvent struct {
	At    int64  `json:"at"` // unix 秒
	Model string `json:"model,omitempty"`
	RPM   int64  `json:"rpm"` // At 前 60s 内启动的请求数（含本请求）
	Stage string `json:"stage,omitempty"`
}

// rateLimitEventCap 是限流事件环形保留条数。
const rateLimitEventCap = 256

// startsCap 触发 starts 窗口压缩的条数上限。
const startsCap = 4096

// UsageSnapshot 是聚合结果的完整快照。
type UsageSnapshot struct {
	WindowStart string          `json:"window_start"` // 回放窗口最早一条的时间
	Entries     int64           `json:"entries"`      // 参与聚合的索引行数
	Today       usageTotals     `json:"today"`
	Window      usageTotals     `json:"window"`
	Days        []usageDayRow   `json:"days"`   // 新在前
	Points      []usageMinPoint `json:"points"` // 旧到新，10 分钟粒度，含零值桶
	Models      []dimensionAgg  `json:"models"`
	Keys        []dimensionAgg  `json:"keys"`
	// ModelDays 是 模型×自然日 的 totals 矩阵，面板的时间范围选择器
	// 用它把模型表过滤到所选窗口（维度行的其它字段只在全窗口下可得）。
	ModelDays   map[string]map[string]usageTotals `json:"model_days,omitempty"`
	ErrorStages map[string]int64                  `json:"error_stages"`
	Duration    latencyStats                      `json:"duration"`
	TTFB        latencyStats                      `json:"ttfb"`
	// RateLimitEvents 是最近的上游 429 采样（旧到新），供面板推算限流阈值。
	RateLimitEvents []rateLimitEvent `json:"rate_limit_events,omitempty"`
}

// sampleRing 是定长延迟蓄水池：写满后循环覆盖最旧样本。
type sampleRing struct {
	vals []int64
	head int
	size int
}

func newSampleRing(capacity int) *sampleRing {
	return &sampleRing{vals: make([]int64, capacity)}
}

func (r *sampleRing) push(v int64) {
	r.vals[r.head] = v
	r.head = (r.head + 1) % len(r.vals)
	if r.size < len(r.vals) {
		r.size++
	}
}

// stats 返回样本的分位数摘要；无样本时返回零值。
func (r *sampleRing) stats() latencyStats {
	if r == nil || r.size == 0 {
		return latencyStats{}
	}
	sorted := make([]int64, r.size)
	copy(sorted, r.vals[:r.size])
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	pick := func(q float64) int64 {
		idx := int(q * float64(len(sorted)-1))
		return sorted[idx]
	}
	return latencyStats{
		Samples: int64(r.size),
		P50:     pick(0.50),
		P90:     pick(0.90),
		P95:     pick(0.95),
		P99:     pick(0.99),
		Avg:     sum / int64(len(sorted)),
		Max:     sorted[len(sorted)-1],
	}
}

// usageAggregator 持有全部聚合状态；add 在 appendIndex 内被调，快照被面板读取。
type usageAggregator struct {
	mu          sync.Mutex
	windowStart time.Time
	entries     int64
	total       usageTotals
	days        map[string]*usageTotals
	mins        [usageMinBuckets]usageMinBucket
	// ttfbSamples/durationSamples 蓄水池分别覆盖 first_upstream_ms 与 duration_ms。
	ttfbSamples     *sampleRing
	durationSamples *sampleRing
	perModel        map[string]*dimensionAgg
	perModelDay     map[string]map[string]*usageTotals
	perKey          map[string]*dimensionAgg
	errStages       map[string]int64
	// starts 是最近请求的启动时间戳（秒，按完成序追加、近似时序），
	// 用于在 429 到达时刻回看前 60s 的发送速率。
	// 在途请求完成前不入索引，速率读数略偏低。
	starts   []int64
	rlEvents []rateLimitEvent
}

func newUsageAggregator() *usageAggregator {
	return &usageAggregator{
		days:            make(map[string]*usageTotals),
		ttfbSamples:     newSampleRing(usageSampleCapacity),
		durationSamples: newSampleRing(usageSampleCapacity),
		perModel:        make(map[string]*dimensionAgg),
		perModelDay:     make(map[string]map[string]*usageTotals),
		perKey:          make(map[string]*dimensionAgg),
		errStages:       make(map[string]int64),
	}
}

// add 把一条已完成请求的索引行计入所有聚合维度。
func (a *usageAggregator) add(e IndexEntry) {
	started, err := time.Parse(time.RFC3339Nano, e.StartedAt)
	if err != nil {
		started = time.Now()
	}
	day := started.Local().Format("2006-01-02")
	slot := started.Unix() / 600

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.entries == 0 || started.Before(a.windowStart) {
		a.windowStart = started
	}
	a.entries++
	a.total.add(e)
	dayTotals := a.days[day]
	if dayTotals == nil {
		dayTotals = &usageTotals{}
		a.days[day] = dayTotals
	}
	dayTotals.add(e)

	idx := int(slot % usageMinBuckets)
	// 环形槽按 usageMinBuckets（8 天）回绕：回放窗口内的旧条目会撞上
	// 当前桶的槽位，直接重置会把已计数据连同桶一起抹掉。槽位冲突只
	// 保留较新 slot 的桶——更旧条目的桶更新丢弃（total/天/维度仍照常
	// 累计），新 slot 则重置过期桶。
	if a.mins[idx].at < slot*600 {
		a.mins[idx] = usageMinBucket{at: slot * 600}
	}
	if a.mins[idx].at == slot*600 {
		a.mins[idx].add(e)
		pushSample(&a.mins[idx].durs, &a.mins[idx].durHead, e.DurationMS)
		if e.FirstUpstreamMS != nil {
			pushSample(&a.mins[idx].ttfbs, &a.mins[idx].ttfbHead, *e.FirstUpstreamMS)
		}
	}

	a.durationSamples.push(e.DurationMS)
	if e.FirstUpstreamMS != nil {
		a.ttfbSamples.push(*e.FirstUpstreamMS)
	}

	model := e.Model
	if model == "" {
		model = e.RequestedModel
	}
	if model != "" {
		agg := a.perModel[model]
		if agg == nil {
			agg = &dimensionAgg{
				Name:          model,
				inTokSamples:  newSampleRing(dimensionSampleCapacity),
				outTokSamples: newSampleRing(dimensionSampleCapacity),
			}
			a.perModel[model] = agg
		}
		agg.addEntry(e)
		dayMap := a.perModelDay[model]
		if dayMap == nil {
			dayMap = make(map[string]*usageTotals)
			a.perModelDay[model] = dayMap
		}
		dayTotals2 := dayMap[day]
		if dayTotals2 == nil {
			dayTotals2 = &usageTotals{}
			dayMap[day] = dayTotals2
		}
		dayTotals2.add(e)
	}
	if e.KeyHash != "" {
		agg := a.perKey[e.KeyHash]
		if agg == nil {
			agg = &dimensionAgg{Name: e.KeyHash}
			a.perKey[e.KeyHash] = agg
		}
		agg.addEntry(e)
	}
	if e.ErrorStage != "" {
		a.errStages[e.ErrorStage]++
	}

	a.starts = append(a.starts, started.Unix())
	// starts 超容时裁到 61s 窗口内——更早的样本不可能再参与任何
	// 未来 429 的速率计算。回放大索引时同理安全：条目按完成序到达。
	if len(a.starts) > startsCap {
		cut := started.Unix() - 61
		keep := a.starts[:0]
		for _, s := range a.starts {
			if s >= cut {
				keep = append(keep, s)
			}
		}
		a.starts = keep
	}
	if isRateLimited(e) {
		end := started.Unix() + e.DurationMS/1000
		var rpm int64
		for _, s := range a.starts {
			if s > end-60 && s <= end {
				rpm++
			}
		}
		model := e.Model
		if model == "" {
			model = e.RequestedModel
		}
		a.rlEvents = append(a.rlEvents, rateLimitEvent{At: end, Model: model, RPM: rpm, Stage: e.ErrorStage})
		if len(a.rlEvents) > rateLimitEventCap {
			a.rlEvents = a.rlEvents[len(a.rlEvents)-rateLimitEventCap:]
		}
	}
}

// addEntry 把请求计入一个维度行：基础计数走内嵌的 usageTotals，
// 本方法只补维度特有的延迟和、token 蓄水池与末态快照。
func (d *dimensionAgg) addEntry(e IndexEntry) {
	d.add(e)
	d.SumDuration += e.DurationMS
	if e.FirstUpstreamMS != nil {
		d.TTFBSamples++
		d.SumTTFB += *e.FirstUpstreamMS
	}
	if d.inTokSamples != nil {
		d.inTokSamples.push(e.InputTokens)
	}
	if d.outTokSamples != nil && e.OutputTokens > 0 {
		d.outTokSamples.push(e.OutputTokens)
	}
	d.LastResult = e.Result
	d.LastStatus = e.StatusCode
	d.LastAt = e.StartedAt
}

// finish 在快照时补齐派生字段（均值、成功率）。
func (d *dimensionAgg) finish() {
	if d.Requests > 0 {
		d.AvgDuration = float64(d.SumDuration) / float64(d.Requests)
	}
	if d.TTFBSamples > 0 {
		d.AvgTTFB = float64(d.SumTTFB) / float64(d.TTFBSamples)
	}
	if total := d.Requests; total > 0 {
		d.SuccessRate = float64(total-d.Errors-d.Disconnected) / float64(total)
	}
	// SLA 口径：分母只留「服务端承诺内」的请求——客户端责任与限流
	// 条目整体剔除，upstream 失分占比取反。
	if slable := d.Requests - d.ClientFaults - d.RateLimited; slable > 0 {
		d.SLASuccessRate = float64(slable-d.UpstreamFaults) / float64(slable)
	}
	if st := d.inTokSamples.stats(); st.Samples > 0 {
		d.InTokP50, d.InTokP95 = st.P50, st.P95
	}
	if st := d.outTokSamples.stats(); st.Samples > 0 {
		d.OutTokP50, d.OutTokP95 = st.P50, st.P95
	}
}

// snapshot 返回全部聚合的深拷贝视图。
func (a *usageAggregator) snapshot() UsageSnapshot {
	a.mu.Lock()
	defer a.mu.Unlock()

	todayKey := time.Now().Local().Format("2006-01-02")
	snap := UsageSnapshot{
		WindowStart: a.windowStart.Format(time.RFC3339),
		Entries:     a.entries,
		Window:      a.total,
		ErrorStages: make(map[string]int64, len(a.errStages)),
		Duration:    a.durationSamples.stats(),
		TTFB:        a.ttfbSamples.stats(),
	}
	for k, v := range a.errStages {
		snap.ErrorStages[k] = v
	}
	if today, ok := a.days[todayKey]; ok {
		snap.Today = *today
	}

	keys := make([]string, 0, len(a.days))
	for k := range a.days {
		keys = append(keys, k)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	if len(keys) > usageMaxDays {
		keys = keys[:usageMaxDays]
	}
	for _, k := range keys {
		snap.Days = append(snap.Days, usageDayRow{Date: k, usageTotals: *a.days[k]})
	}
	if len(a.perModelDay) > 0 {
		keep := make(map[string]bool, len(keys))
		for _, k := range keys {
			keep[k] = true
		}
		snap.ModelDays = make(map[string]map[string]usageTotals, len(a.perModelDay))
		for model, dayMap := range a.perModelDay {
			out := make(map[string]usageTotals, len(dayMap))
			for d, t := range dayMap {
				if keep[d] {
					out[d] = *t
				}
			}
			if len(out) > 0 {
				snap.ModelDays[model] = out
			}
		}
	}

	current := time.Now().Unix() / 600
	snap.Points = make([]usageMinPoint, 0, usageMinBuckets)
	for s := current - usageMinBuckets + 1; s <= current; s++ {
		bucket := a.mins[int(s%usageMinBuckets)]
		point := usageMinPoint{At: s * 600}
		if bucket.at == s*600 {
			point.usageTotals = bucket.usageTotals
			point.AvgDur, point.DurP95 = sampleSummary(bucket.durs)
			point.AvgTTFB, point.TTFBP95 = sampleSummary(bucket.ttfbs)
		}
		snap.Points = append(snap.Points, point)
	}

	snap.Models = sortedAggs(a.perModel)
	snap.Keys = sortedAggs(a.perKey)
	snap.RateLimitEvents = append([]rateLimitEvent(nil), a.rlEvents...)
	return snap
}

// latencySummary 只返回全局延迟分位数两行——/panel/api/stats 的 1Hz
// 轮询只消费 usage.ttfb/usage.duration，而全量 snapshot 要遍历 1152
// 个分钟桶各排序一份样本副本、再按模型与 key 各排 2048 样本环、
// 深拷贝 per-model-day，为两行数据做数百 KB 的活不划算。
func (a *usageAggregator) latencySummary() map[string]latencyStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return map[string]latencyStats{
		"duration": a.durationSamples.stats(),
		"ttfb":     a.ttfbSamples.stats(),
	}
}

// sampleSummary 返回样本的均值与 p95；空样本返回零值。
func sampleSummary(samples []int64) (avg, p95 int64) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := make([]int64, len(samples))
	copy(sorted, samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return sum / int64(len(sorted)), sorted[int(0.95*float64(len(sorted)-1))]
}

// replayLines 把 index.jsonl 的快照字节逐行解析并计入聚合，返回成功行数。
// 快照边界由调用方划定：NewManager 的回放协程在 manager.mutex 下
// tailRead——appendIndex 的「写文件+入账」与快照天然互斥，快照内行
// 由本函数统一入账，快照外行由实时路径自计，任一行恰入账一次。
func (a *usageAggregator) replayLines(data []byte) int64 {
	var parsed int64
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry IndexEntry
		if json.Unmarshal([]byte(line), &entry) == nil {
			a.add(entry)
			parsed++
		}
	}
	return parsed
}

// sortedAggs 把维度 map 转成按请求数降序的行切片。
func sortedAggs(m map[string]*dimensionAgg) []dimensionAgg {
	out := make([]dimensionAgg, 0, len(m))
	for _, agg := range m {
		agg.finish()
		out = append(out, *agg)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Name < out[j].Name
	})
	return out
}
