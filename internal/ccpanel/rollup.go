// rollup 把 index.jsonl 增量聚合成 (10分钟槽 × 入口api × 模型) 的格子，
// 支撑 /dashboard/{summary,metrics,stats} 与筛选项端点按任意时间窗求和。
//
// 与 debuglog 内置 usageAggregator 的分工：后者服务旧面板，固定维度 +
// 8 天细粒度；本结构面向移植面板的 range 契约（ccLoad 按时间窗任意切片），
// 覆盖索引全生命周期（256MB 上限 ≈ 两周），格子数被天然约束在万级。
// 增量扫经 debuglog.ScanIndex：每次查询只解析新追加的行。
package ccpanel

import (
	"sort"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// rollupSlotSeconds 是格子时间槽宽度；10 分钟对齐 usageAggregator 的桶，
// 误差边界 ±10 分钟（天界对齐的预设 range 不受此误差影响）。
const rollupSlotSeconds = 600

type cellKey struct {
	slot  int64  // unix 秒，600 对齐
	api   string // 入口协议原文：anthropic/openai-chat/openai-responses/responses-ws
	model string // 生效模型（Model 退化 RequestedModel）
	kh    string // index.jsonl 的 key_hash：auth_token_id 过滤与 api_token 数据范围
}

// cellTotals 是单格子的累计计数；延迟只留和与样本数，查询期算均值。
// 计数口径与 ccLoad 对齐：ok=2xx，gone=499（客户端断连），limited=429
// （error 的子集，健康块染蓝用）；NG 后缀=非 499 行合计（metrics/token
// 统计的计数口径），无后缀 token 字段含 499 行（summary/stats 口径）。
type cellTotals struct {
	requests     int64
	ok           int64
	gone         int64
	limited      int64
	nDur         int64 // duration>0 行数（stats avg_duration 分母，含 499）
	nDurOK       int64 // 2xx && duration>0（metrics duration_count）
	inTok        int64
	outTok       int64
	cacheRead    int64
	cacheWrite   int64
	inTokNG      int64
	outTokNG     int64
	cacheReadNG  int64
	cacheWriteNG int64
	sumDurMS     int64 // duration>0 行 duration 和（含 499）
	sumDurOKMS   int64 // 2xx 行 duration 和
	// streaming && 2xx && fbt>0：stats/health/metrics 的 TTFT 口径
	sumFirstOKMS int64
	nFirstOK     int64
	// streaming 行 fbt 和与样本数（不限状态）：token 统计 stream_avg_ttfb
	sumFirstStreamMS int64
	nFirstStream     int64
	// 非 streaming 行 duration 和与行数（不限状态）：token 统计 non_stream_avg_rt
	sumDurNonStreamMS int64
	nNonStream        int64
	// 非 499 行按 stream 拆分：token 统计 stream_count/non_stream_count
	nStreamNG    int64
	nNonStreamNG int64
}

// recentPoint 是 recent 环的一条记录；model/kh 供 per-model/per-token
// recent_rpm，gone 让 RPM 口径剔除客户端断连（ccLoad status_code != 499 同款）。
type recentPoint struct {
	end   int64
	model string
	kh    string
	gone  bool
}

// modelLast 记录单 (模型,key_hash) 的最近时刻（毫秒戳）：req* 是最近
// 非 499 行，okAt 是最近 2xx 行——ccLoad last_request_*/last_success_at
// 的投影源；kh 维让 api_token 范围查询不漏其他令牌的行。
type modelLast struct {
	reqAt     int64
	reqStatus int
	reqResult string
	okAt      int64
}

// lastKey 是 last 表的键：模型 + key_hash。
type lastKey struct {
	model string
	kh    string
}

type rollup struct {
	mu     sync.Mutex
	offset int64
	cells  map[cellKey]*cellTotals
	models map[string]struct{}
	codes  map[int]struct{}
	last   map[lastKey]*modelLast
	// recent 是最近 61s 内请求的完成时刻（unix 秒），给 recent_rpm 用；
	// 与 usageAggregator 的 starts 窗口同一口径。
	recent []recentPoint
}

func newRollup() *rollup {
	return &rollup{
		cells:  make(map[cellKey]*cellTotals),
		models: make(map[string]struct{}),
		codes:  make(map[int]struct{}),
		last:   make(map[lastKey]*modelLast),
	}
}

// add 把一条索引行计入格子。须在 mu 下调用。
func (r *rollup) add(e debuglog.IndexEntry) {
	started, err := time.Parse(time.RFC3339Nano, e.StartedAt)
	if err != nil {
		return
	}
	model := e.Model
	if model == "" {
		model = e.RequestedModel
	}
	key := cellKey{slot: started.Unix() / rollupSlotSeconds * rollupSlotSeconds, api: e.API, model: model, kh: e.KeyHash}
	c := r.cells[key]
	if c == nil {
		c = &cellTotals{}
		r.cells[key] = c
	}
	c.requests++
	ok2xx := e.StatusCode >= 200 && e.StatusCode < 300
	gone := e.StatusCode == 499
	if ok2xx {
		c.ok++
		c.sumDurOKMS += e.DurationMS
		if e.DurationMS > 0 {
			c.nDurOK++
		}
	}
	if gone {
		c.gone++
	} else {
		c.inTokNG += e.InputTokens
		c.outTokNG += e.OutputTokens
		c.cacheReadNG += e.CacheReadTokens
		c.cacheWriteNG += e.CacheWriteTokens
		if e.Stream {
			c.nStreamNG++
		} else {
			c.nNonStreamNG++
		}
	}
	if e.StatusCode == 429 {
		c.limited++
	}
	c.inTok += e.InputTokens
	c.outTok += e.OutputTokens
	c.cacheRead += e.CacheReadTokens
	c.cacheWrite += e.CacheWriteTokens
	if e.DurationMS > 0 {
		c.nDur++
		c.sumDurMS += e.DurationMS
	}
	if e.Stream && ok2xx && e.FirstUpstreamMS != nil && *e.FirstUpstreamMS > 0 {
		c.sumFirstOKMS += *e.FirstUpstreamMS
		c.nFirstOK++
	}
	if e.Stream && e.FirstUpstreamMS != nil {
		c.sumFirstStreamMS += *e.FirstUpstreamMS
		c.nFirstStream++
	}
	if !e.Stream {
		c.nNonStream++
		c.sumDurNonStreamMS += e.DurationMS
	}
	if model != "" {
		r.models[model] = struct{}{}
	}
	if e.StatusCode != 0 {
		r.codes[e.StatusCode] = struct{}{}
	}
	startedMS := started.UnixMilli()
	lk := lastKey{model: model, kh: e.KeyHash}
	l := r.last[lk]
	if l == nil {
		l = &modelLast{}
		r.last[lk] = l
	}
	if !gone && startedMS >= l.reqAt {
		l.reqAt = startedMS
		l.reqStatus = e.StatusCode
		l.reqResult = e.Result
	}
	if ok2xx && startedMS >= l.okAt {
		l.okAt = startedMS
	}
	// recent 环形：条目按完成序近似到达，超水位时裁到最新完成时刻前 61s。
	end := started.Unix() + e.DurationMS/1000
	r.recent = append(r.recent, recentPoint{end: end, model: model, kh: e.KeyHash, gone: gone})
	if len(r.recent) > 4096 {
		cut := end - 61
		keep := r.recent[:0]
		for _, s := range r.recent {
			if s.end >= cut {
				keep = append(keep, s)
			}
		}
		r.recent = keep
	}
}

// refresh 把索引新追加的行并入格子；读失败时保留旧偏移，下次查询重试。
func (r *rollup) refresh(m *debuglog.Manager) {
	if m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	next, err := m.ScanIndex(r.offset, func() {
		r.cells = make(map[cellKey]*cellTotals)
		r.models = make(map[string]struct{})
		r.codes = make(map[int]struct{})
		r.last = make(map[lastKey]*modelLast)
		r.recent = nil
	}, r.add)
	if err != nil {
		return
	}
	r.offset = next
}

// eachCell 刷新后遍历 [since,until) 时间窗内的格子；cb 不得持有 cell 指针。
func (r *rollup) eachCell(m *debuglog.Manager, since, until time.Time, cb func(cellKey, cellTotals)) {
	r.refresh(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, c := range r.cells {
		at := key.slot
		if at+rollupSlotSeconds <= since.Unix() || at >= until.Unix() {
			continue
		}
		cb(key, *c)
	}
}

// modelSet 返回出现过的模型名集合（排序）；kh 非空时只数该 key_hash
// 产生过流量的模型（api_token 身份的数据范围收敛）。
func (r *rollup) modelSet(m *debuglog.Manager, kh string) []string {
	if kh != "" {
		set := map[string]struct{}{}
		r.eachCell(m, time.Unix(0, 0), time.Now().Add(time.Hour), func(key cellKey, _ cellTotals) {
			if key.kh == kh && key.model != "" {
				set[key.model] = struct{}{}
			}
		})
		out := make([]string, 0, len(set))
		for name := range set {
			out = append(out, name)
		}
		sort.Strings(out)
		return out
	}
	r.refresh(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.models))
	for m := range r.models {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// statusCodeSet 返回出现过的状态码集合（排序）。
func (r *rollup) statusCodeSet(m *debuglog.Manager) []int {
	r.refresh(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]int, 0, len(r.codes))
	for c := range r.codes {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

// recentRPM 返回最近 60 秒内完成的非 499 请求数；model/kh 非空时分别按
// 生效模型、key_hash 过滤（stats 行级 recent_rpm 与 per-token RPM 用）。
func (r *rollup) recentRPM(m *debuglog.Manager, model, kh string) float64 {
	r.refresh(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := time.Now().Unix() - 60
	var n int64
	for _, s := range r.recent {
		if s.end > cut && !s.gone && (model == "" || s.model == model) && (kh == "" || s.kh == kh) {
			n++
		}
	}
	return float64(n)
}

// addCells 返回 a+b 的逐字段和。
func addCells(a, b cellTotals) cellTotals {
	a.requests += b.requests
	a.ok += b.ok
	a.gone += b.gone
	a.limited += b.limited
	a.nDur += b.nDur
	a.nDurOK += b.nDurOK
	a.inTok += b.inTok
	a.outTok += b.outTok
	a.cacheRead += b.cacheRead
	a.cacheWrite += b.cacheWrite
	a.inTokNG += b.inTokNG
	a.outTokNG += b.outTokNG
	a.cacheReadNG += b.cacheReadNG
	a.cacheWriteNG += b.cacheWriteNG
	a.sumDurMS += b.sumDurMS
	a.sumDurOKMS += b.sumDurOKMS
	a.sumFirstOKMS += b.sumFirstOKMS
	a.nFirstOK += b.nFirstOK
	a.sumFirstStreamMS += b.sumFirstStreamMS
	a.nFirstStream += b.nFirstStream
	a.sumDurNonStreamMS += b.sumDurNonStreamMS
	a.nNonStream += b.nNonStream
	a.nStreamNG += b.nStreamNG
	a.nNonStreamNG += b.nNonStreamNG
	return a
}

// lastByModel 返回各模型的最近时刻快照（started_at 毫秒戳）；kh 非空时
// 只看该令牌的行，为空时跨令牌取每模型最新（admin 全量视图口径）。
func (r *rollup) lastByModel(m *debuglog.Manager, kh string) map[string]modelLast {
	r.refresh(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]modelLast, len(r.last))
	for k, v := range r.last {
		if kh != "" && k.kh != kh {
			continue
		}
		cur := out[k.model]
		if v.reqAt >= cur.reqAt {
			cur.reqAt = v.reqAt
			cur.reqStatus = v.reqStatus
			cur.reqResult = v.reqResult
		}
		if v.okAt >= cur.okAt {
			cur.okAt = v.okAt
		}
		out[k.model] = cur
	}
	return out
}
