// 本文件把 index.jsonl 的请求摘要在内存中聚合成多维统计，供面板消费。
//
// 设计要点：
//   - 启动时回放 index.jsonl 尾部（上限 usageReplayTailBytes），进程重启不丢历史口径；
//   - 请求完成时 appendIndex 顺带累加，热路径只有一次 mutex 下的计数更新；
//   - 延迟用定长蓄水池（最近 usageSampleCapacity 条）算 p50/p95/p99，
//     比均值更能暴露上游长尾；
//   - 天聚合按本地时区；小时环保留最近 usageHourBuckets 小时。
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
	// usageHourBuckets 是逐小时趋势保留的桶数（7 天）。
	usageHourBuckets = 24 * 7
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
	RateLimited  int64 `json:"rate_limited"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
	CacheWrite   int64 `json:"cache_write_tokens"`
	Reasoning    int64 `json:"reasoning_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	// GenMS 是 completed 请求的首帧后生成毫秒累计（duration−TTFB），
	// 前端用 output_tokens/gen_ms 求 decode 速率（tok/s）。
	GenMS int64 `json:"gen_ms,omitempty"`
}

// add 把一条索引行计入累计。
func (t *usageTotals) add(e IndexEntry) {
	t.Requests++
	switch {
	case e.StatusCode >= 400 || e.Result == "failed":
		t.Errors++
	case e.Result == "disconnected" || e.Result == "aborted":
		t.Disconnected++
	}
	if e.StatusCode == 429 {
		t.RateLimited++
	}
	t.InputTokens += e.InputTokens
	t.OutputTokens += e.OutputTokens
	t.CacheRead += e.CacheReadTokens
	t.CacheWrite += e.CacheWriteTokens
	t.Reasoning += e.ReasoningTokens
	t.TotalTokens += e.TotalTokens
	// 只计正常完成且有首帧时间戳的请求：失败/断连的耗时段包含
	// 非生成分量，混入会拉低均速。
	if e.Result == "completed" && e.FirstUpstreamMS != nil {
		t.GenMS += max(e.DurationMS-*e.FirstUpstreamMS, 0)
	}
}

// usageHourCap 是单小时桶内保留的延迟样本上限；超出后循环覆盖最旧样本。
const usageHourCap = 1024

// usageHourBucket 是一小时内的请求/token 聚合，供趋势图。
// 计数字段与 usageTotals 对齐：面板按时间范围选择器截一段桶求和，
// 即可得到该窗口的完整卡片数据（含断连/缓存写/推理 token）。
type usageHourBucket struct {
	hour         int64 // unix 小时戳
	requests     int64
	errors       int64
	disconnected int64
	rateLimited  int64 // status_code==429
	input        int64
	output       int64
	cacheRead    int64
	cacheWrite   int64
	reasoning    int64
	genMS        int64   // completed 请求的首帧后生成毫秒累计（均速分子分母）
	durs         []int64 // duration_ms 样本（环形，上限 usageHourCap）
	durHead      int
	ttfbs        []int64 // first_upstream_ms 样本
	ttfbHead     int
}

// pushSample 向容量受限的样本切片追加；满后原地覆盖最旧值。
func pushSample(samples *[]int64, head *int, v int64) {
	if len(*samples) < usageHourCap {
		*samples = append(*samples, v)
		return
	}
	(*samples)[*head] = v
	*head = (*head + 1) % usageHourCap
}

// usageHourPoint 是输出给面板的小时数据点。
type usageHourPoint struct {
	Hour         int64 `json:"hour"` // unix 秒
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	Disconnected int64 `json:"disconnected"`
	RateLimited  int64 `json:"rate_limited"`
	Input        int64 `json:"input_tokens"`
	Output       int64 `json:"output_tokens"`
	AvgDur       int64 `json:"avg_duration_ms"`
	DurP95       int64 `json:"duration_p95_ms"`
	AvgTTFB      int64 `json:"avg_ttfb_ms"`
	TTFBP95      int64 `json:"ttfb_p95_ms"`
	// CacheRead/CacheWrite/Reasoning/GenMS 供前端按任意时间范围求和，
	// 再派生缓存命中率与 decode 均速。
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
	Reasoning  int64 `json:"reasoning_tokens"`
	GenMS      int64 `json:"gen_ms,omitempty"`
}

// dimensionAgg 是按模型或 key 哈希聚合的行。
type dimensionAgg struct {
	Name         string `json:"name"`
	Requests     int64  `json:"requests"`
	Errors       int64  `json:"errors"`
	Disconnected int64  `json:"disconnected"`
	RateLimited  int64  `json:"rate_limited"`
	Input        int64  `json:"input_tokens"`
	Output       int64  `json:"output_tokens"`
	CacheRead    int64  `json:"cache_read_tokens"`
	CacheWrite   int64  `json:"cache_write_tokens"`
	Reasoning    int64  `json:"reasoning_tokens"`
	TotalTokens  int64  `json:"total_tokens"`
	// GenMS 是 completed 请求的首帧后生成毫秒累计，供前端算均速。
	GenMS       int64   `json:"gen_ms,omitempty"`
	SumDuration int64   `json:"-"`
	TTFBSamples int64   `json:"-"`
	SumTTFB     int64   `json:"-"`
	LastResult  string  `json:"last_result,omitempty"`
	LastStatus  int     `json:"last_status,omitempty"`
	LastAt      string  `json:"last_at,omitempty"`
	AvgDuration float64 `json:"avg_duration_ms"`
	AvgTTFB     float64 `json:"avg_ttfb_ms"`
	SuccessRate float64 `json:"success_rate"`
}

// latencyStats 是蓄水池算出的延迟分布。
type latencyStats struct {
	Samples int64 `json:"samples"`
	P50     int64 `json:"p50"`
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

// rateLimitEvent 是一次上游 429 的采样：发生时刻（≈请求完成时刻）、模型、
// 以及该时刻前 60s 内启动的请求数。多次采样的 RPM 峰值即上游
// 限流阈值（每分钟请求数）的观测下界——被限说明已触线。
type rateLimitEvent struct {
	At    int64  `json:"at"` // unix 秒
	Model string `json:"model,omitempty"`
	RPM   int64  `json:"rpm"` // At 前 60s 内启动的请求数（含本请求）
}

// rateLimitEventCap 是限流事件环形保留条数。
const rateLimitEventCap = 256

// startsCap 触发 starts 窗口压缩的条数上限。
const startsCap = 4096

// UsageSnapshot 是聚合结果的完整快照。
type UsageSnapshot struct {
	WindowStart string           `json:"window_start"` // 回放窗口最早一条的时间
	Entries     int64            `json:"entries"`      // 参与聚合的索引行数
	Today       usageTotals      `json:"today"`
	Window      usageTotals      `json:"window"`
	Days        []usageDayRow    `json:"days"`  // 新在前
	Hours       []usageHourPoint `json:"hours"` // 旧到新，含零值小时
	Models      []dimensionAgg   `json:"models"`
	Keys        []dimensionAgg   `json:"keys"`
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
	hours       [usageHourBuckets]usageHourBucket
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
	hour := started.Unix() / 3600

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

	idx := int(hour % usageHourBuckets)
	if a.hours[idx].hour != hour {
		a.hours[idx] = usageHourBucket{hour: hour}
	}
	a.hours[idx].requests++
	if e.StatusCode >= 400 || e.Result == "failed" {
		a.hours[idx].errors++
	}
	if e.Result == "disconnected" || e.Result == "aborted" {
		a.hours[idx].disconnected++
	}
	if e.StatusCode == 429 {
		a.hours[idx].rateLimited++
	}
	a.hours[idx].input += e.InputTokens
	a.hours[idx].output += e.OutputTokens
	a.hours[idx].cacheRead += e.CacheReadTokens
	a.hours[idx].cacheWrite += e.CacheWriteTokens
	a.hours[idx].reasoning += e.ReasoningTokens
	if e.Result == "completed" && e.FirstUpstreamMS != nil {
		a.hours[idx].genMS += max(e.DurationMS-*e.FirstUpstreamMS, 0)
	}
	pushSample(&a.hours[idx].durs, &a.hours[idx].durHead, e.DurationMS)
	if e.FirstUpstreamMS != nil {
		pushSample(&a.hours[idx].ttfbs, &a.hours[idx].ttfbHead, *e.FirstUpstreamMS)
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
			agg = &dimensionAgg{Name: model}
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
	if e.StatusCode == 429 {
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
		a.rlEvents = append(a.rlEvents, rateLimitEvent{At: end, Model: model, RPM: rpm})
		if len(a.rlEvents) > rateLimitEventCap {
			a.rlEvents = a.rlEvents[len(a.rlEvents)-rateLimitEventCap:]
		}
	}
}

// addEntry 把请求计入一个维度行。
func (d *dimensionAgg) addEntry(e IndexEntry) {
	d.Requests++
	switch {
	case e.StatusCode >= 400 || e.Result == "failed":
		d.Errors++
	case e.Result == "disconnected" || e.Result == "aborted":
		d.Disconnected++
	}
	if e.StatusCode == 429 {
		d.RateLimited++
	}
	d.Input += e.InputTokens
	d.Output += e.OutputTokens
	d.CacheRead += e.CacheReadTokens
	d.CacheWrite += e.CacheWriteTokens
	d.Reasoning += e.ReasoningTokens
	d.TotalTokens += e.TotalTokens
	d.SumDuration += e.DurationMS
	if e.FirstUpstreamMS != nil {
		d.TTFBSamples++
		d.SumTTFB += *e.FirstUpstreamMS
	}
	if e.Result == "completed" && e.FirstUpstreamMS != nil {
		d.GenMS += max(e.DurationMS-*e.FirstUpstreamMS, 0)
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

	current := time.Now().Unix() / 3600
	snap.Hours = make([]usageHourPoint, 0, usageHourBuckets)
	for h := current - usageHourBuckets + 1; h <= current; h++ {
		bucket := a.hours[int(h%usageHourBuckets)]
		point := usageHourPoint{Hour: h * 3600}
		if bucket.hour == h {
			point.Requests = bucket.requests
			point.Errors = bucket.errors
			point.Disconnected = bucket.disconnected
			point.RateLimited = bucket.rateLimited
			point.Input = bucket.input
			point.Output = bucket.output
			point.CacheRead = bucket.cacheRead
			point.CacheWrite = bucket.cacheWrite
			point.Reasoning = bucket.reasoning
			point.GenMS = bucket.genMS
			point.AvgDur, point.DurP95 = sampleSummary(bucket.durs)
			point.AvgTTFB, point.TTFBP95 = sampleSummary(bucket.ttfbs)
		}
		snap.Hours = append(snap.Hours, point)
	}

	snap.Models = sortedAggs(a.perModel)
	snap.Keys = sortedAggs(a.perKey)
	snap.RateLimitEvents = append([]rateLimitEvent(nil), a.rlEvents...)
	return snap
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

// replayIndex 启动时回放 index.jsonl 尾部重建聚合，返回成功解析的行数。
// 文件缺失（首次运行）不是错误。
func (a *usageAggregator) replayIndex(path string) int64 {
	data, err := tailRead(path, usageReplayTailBytes)
	if err != nil {
		return 0
	}
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
