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
}

// cellTotals 是单格子的累计计数；延迟只留和与样本数，查询期算均值。
type cellTotals struct {
	requests   int64
	failures   int64 // Result != completed（含断连/中止/失败）
	inTok      int64
	outTok     int64
	cacheRead  int64
	cacheWrite int64
	sumDurMS   int64
	sumFirstMS int64
	nFirst     int64
}

type rollup struct {
	mu     sync.Mutex
	offset int64
	cells  map[cellKey]*cellTotals
	models map[string]struct{}
	codes  map[int]struct{}
	// recent 是最近 61s 内请求的完成时刻（unix 秒），给 recent_rpm 用；
	// 与 usageAggregator 的 starts 窗口同一口径。
	recent []int64
}

func newRollup() *rollup {
	return &rollup{
		cells:  make(map[cellKey]*cellTotals),
		models: make(map[string]struct{}),
		codes:  make(map[int]struct{}),
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
	key := cellKey{slot: started.Unix() / rollupSlotSeconds * rollupSlotSeconds, api: e.API, model: model}
	c := r.cells[key]
	if c == nil {
		c = &cellTotals{}
		r.cells[key] = c
	}
	c.requests++
	if e.Result != "completed" {
		c.failures++
	}
	c.inTok += e.InputTokens
	c.outTok += e.OutputTokens
	c.cacheRead += e.CacheReadTokens
	c.cacheWrite += e.CacheWriteTokens
	c.sumDurMS += e.DurationMS
	if e.FirstUpstreamMS != nil {
		c.sumFirstMS += *e.FirstUpstreamMS
		c.nFirst++
	}
	if model != "" {
		r.models[model] = struct{}{}
	}
	if e.StatusCode != 0 {
		r.codes[e.StatusCode] = struct{}{}
	}
	// recent 环形：条目按完成序近似到达，超水位时裁到最新完成时刻前 61s。
	end := started.Unix() + e.DurationMS/1000
	r.recent = append(r.recent, end)
	if len(r.recent) > 4096 {
		cut := end - 61
		keep := r.recent[:0]
		for _, s := range r.recent {
			if s >= cut {
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

// modelSet 返回出现过的模型名集合（排序）。
func (r *rollup) modelSet(m *debuglog.Manager) []string {
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

// recentRPM 返回最近 60 秒内完成的请求数。
func (r *rollup) recentRPM(m *debuglog.Manager) float64 {
	r.refresh(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	cut := time.Now().Unix() - 60
	var n int64
	for _, s := range r.recent {
		if s > cut {
			n++
		}
	}
	return float64(n)
}

// clientProtocol 把入口 api 标识映射成 ccLoad 的 client_protocol 键：
// anthropic 直连、openai-chat 是 OpenAI 兼容、responses 系（含 WS 轮次）
// 是 Codex 前端。空 api 的行（早期索引）归入 "unknown"。
func clientProtocol(api string) string {
	switch api {
	case "anthropic":
		return "anthropic"
	case "openai-chat":
		return "openai"
	case "openai-responses", "responses-ws":
		return "codex"
	default:
		return "unknown"
	}
}
