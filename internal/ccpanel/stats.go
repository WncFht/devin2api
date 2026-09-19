package ccpanel

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// statsEntry 对应 ccLoad model.StatsEntry 的渠道列收缩版：stats 页按模型
// 聚合的行（模型维=生效模型，LogCellKey.Model 同口径）。本服务单上游无渠道维。
type statsEntry struct {
	Model                   string   `json:"model"`
	Success                 int64    `json:"success"`
	Error                   int64    `json:"error"`
	Total                   int64    `json:"total"`
	AvgFirstByteTimeSeconds *float64 `json:"avg_first_byte_time_seconds,omitempty"`
	AvgDurationSeconds      *float64 `json:"avg_duration_seconds,omitempty"`
	LastSuccessAt           *int64   `json:"last_success_at,omitempty"`
	LastSuccessID           *int64   `json:"last_success_id,omitempty"`
	LastRequestAt           *int64   `json:"last_request_at,omitempty"`
	LastRequestID           *int64   `json:"last_request_id,omitempty"`
	LastRequestStatus       *int     `json:"last_request_status,omitempty"`
	LastRequestMessage      string   `json:"last_request_message,omitempty"`
	PeakRPM                 *float64 `json:"peak_rpm,omitempty"`
	AvgRPM                  *float64 `json:"avg_rpm,omitempty"`
	RecentRPM               *float64 `json:"recent_rpm,omitempty"`
	TotalInputTokens        *int64   `json:"total_input_tokens,omitempty"`
	TotalOutputTokens       *int64   `json:"total_output_tokens,omitempty"`
	TotalCacheReadTokens    *int64   `json:"total_cache_read_input_tokens,omitempty"`
	TotalCacheWriteTokens   *int64   `json:"total_cache_creation_input_tokens,omitempty"`
	TotalCost               *float64 `json:"total_cost,omitempty"`
	EffectiveCost           *float64 `json:"effective_cost,omitempty"`
	// GenMS 是生成时长合计毫秒（stream 扣首字、其余全时长）——窗口
	// TPS = Σ输出 ÷ Σgen 的分母，同表格速度列口径。
	GenMS          *int64        `json:"gen_ms,omitempty"`
	HealthTimeline []healthPoint `json:"health_timeline,omitempty"`
}

// healthPoint 对应 ccLoad model.HealthPoint（健康指示块的单点）。
type healthPoint struct {
	Ts               time.Time `json:"ts"`
	SuccessRate      float64   `json:"rate"` // -1 表示无数据
	Success          int64     `json:"success"`
	Error            int64     `json:"error"`
	RateLimited      int64     `json:"rate_limited"`
	AvgFirstByteTime float64   `json:"avg_first_byte_time"`
	AvgDuration      float64   `json:"avg_duration"`
	InputTokens      int64     `json:"input_tokens"`
	OutputTokens     int64     `json:"output_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_creation_tokens"`
	Cost             float64   `json:"cost"`
	EffectiveCost    float64   `json:"effective_cost"`
}

// statScope 是一次统计查询收敛后的过滤值：kh 为限定 key_hash（api_token
// 身份或 auth_token_id 参数命中时），api/model/modelLike 来自 query——
// 与 store.LogScope 一一对应，经 logScope() 下推到 SQL。account 是
// 上游账号 lane 名（读侧折叠口径），P2 逐号统计的支点，v1 无填充方。
type statScope struct {
	kh        string
	api       string
	model     string
	modelLike string
	account   string
}

// queryScope 把一次统计查询的数据范围折成 statScope。
// 范围来源两类：api_token 身份（强制只看自己的行）与 query 筛选
// （api、auth_token_id、model、model_like）。excluded=true 表示条件
// 不可能命中（auth_token_id 查无令牌、或与 api_token 身份冲突），
// 调用方直接回空集。
func (h *Handler) queryScope(r *http.Request) (scope statScope, excluded bool) {
	q := r.URL.Query()
	if scope.kh, excluded = h.scopeKeyHash(r); excluded {
		return scope, true
	}
	scope.api = strings.TrimSpace(q.Get("api"))
	scope.model = strings.TrimSpace(q.Get("model"))
	scope.modelLike = strings.TrimSpace(q.Get("model_like"))
	return scope, false
}

// dashboardStats 实现 /dashboard|/admin/stats：
// {stats:[StatsEntry per model], duration_seconds, rpm_stats, is_today}。
// success/error/total/499 口径与 ccLoad GetStats SQL 逐条对齐
// （success=2xx，error=非2xx非499，total=非499）。
func (h *Handler) dashboardStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	since, until, rangeName := resolveRange(r, now)
	isToday := rangeName == "today"
	duration := until.Sub(since).Seconds()
	if duration < 1 {
		duration = 1
	}
	scope, excluded := h.queryScope(r)
	// recentBlock 聚合短窗（10s/60s）指标：req 非 499；tps 是生成速率
	// （Σ输出 ÷ Σ生成时长，同表格速度列口径）；ttfb 沿用格子口径；
	// cache_pct 同缓存命中列。
	recentBlock := func(sec int64) map[string]any {
		// excluded（筛选条件不可能命中）时连查询都不发：scope.kh 为空会
		// 返回全局真实计数，与 stats=[]/rpm=0 的排空口径自相矛盾。
		var a store.LogRecentAgg
		if !excluded {
			a = h.recentWindow(ctx, sec, scope)
		}
		out := map[string]any{"requests": a.Req}
		if a.Req > 0 {
			out["rpm"] = float64(a.Req) * 60 / float64(sec)
		}
		if a.GenMS > 0 {
			out["tps"] = float64(a.OutTok) * 1000 / float64(a.GenMS)
		}
		if a.NFirst > 0 {
			out["ttfb_s"] = float64(a.FirstMS) / float64(a.NFirst) / 1000
		}
		if a.NDur > 0 {
			out["dur_s"] = float64(a.DurMS) / float64(a.NDur) / 1000
		}
		if d := a.InTok + a.CrTok + a.CwTok; d > 0 {
			out["cache_pct"] = float64(a.CrTok) * 100 / float64(d)
		}
		return out
	}
	respond := func(stats []statsEntry, rpm map[string]any) {
		respondOK(w, map[string]any{
			"stats":            stats,
			"duration_seconds": duration,
			"rpm_stats":        rpm,
			"is_today":         isToday,
			"recent":           map[string]any{"s10": recentBlock(10), "s60": recentBlock(60)},
		})
	}
	if excluded {
		respond([]statsEntry{}, zeroRPMStats())
		return
	}
	prices := h.CatalogPrices(ctx)

	type modelAgg struct {
		t    store.LogCellTotals
		cost float64
		peak int64 // 单槽非 499 峰值（per-model peak_rpm 的分子）
	}
	// 一遍格子扫描喂三类累加器：per-model 聚合、全局 rpm total/peak、
	// 健康时间线桶（healthStart 下界由 covers 判——等价于独立扫描
	// eachCell(healthStart,until) 的重叠计入口径）。
	health := newHealthBuckets(since, until, isToday, prices)
	var rpmTotal, rpmPeak int64
	aggs := map[string]*modelAgg{}
	h.eachCell(ctx, since, until, scope, func(key store.LogCellKey, c store.LogCellTotals) {
		a := aggs[key.Model]
		if a == nil {
			a = &modelAgg{}
			aggs[key.Model] = a
		}
		a.t = addCells(a.t, c)
		a.cost += cellCost(key, c, prices)
		n := c.Requests - c.Gone
		if n > a.peak {
			a.peak = n
		}
		rpmTotal += n
		if n > rpmPeak {
			rpmPeak = n
		}
		if health.covers(key.Slot) {
			health.add(key, c)
		}
	})

	last := h.lastByModel(ctx, scope.kh)
	models := make([]string, 0, len(aggs))
	for m := range aggs {
		models = append(models, m)
	}
	sort.Strings(models)

	perModel := health.finalize()

	var recentByModel map[string]float64
	if isToday {
		recentByModel = h.recentRPMByModel(ctx, scope.kh)
	}
	entries := make([]statsEntry, 0, len(models))
	for _, m := range models {
		a := aggs[m]
		e := statsEntry{
			Model:          m,
			Success:        a.t.OK,
			Error:          a.t.Requests - a.t.OK - a.t.Gone,
			Total:          a.t.Requests - a.t.Gone,
			HealthTimeline: perModel[m],
		}
		if a.t.NFirstOK > 0 {
			v := float64(a.t.SumFirstOKMS) / float64(a.t.NFirstOK) / 1000
			e.AvgFirstByteTimeSeconds = &v
		}
		if a.t.NDur > 0 {
			v := float64(a.t.SumDurMS) / float64(a.t.NDur) / 1000
			e.AvgDurationSeconds = &v
		}
		if l, ok := last[m]; ok {
			if l.OKAt > 0 {
				at := l.OKAt
				id := l.OKID
				e.LastSuccessAt = &at
				e.LastSuccessID = &id // 日志行 id 现为 logs 表自增主键（旧为 started_at 毫秒戳）
			}
			if l.ReqAt > 0 {
				at := l.ReqAt
				id := l.ReqID
				status := l.ReqStatus
				e.LastRequestAt = &at
				e.LastRequestID = &id
				e.LastRequestStatus = &status
				e.LastRequestMessage = l.ReqResult
			}
		}
		if a.peak > 0 {
			v := float64(a.peak) / (rollupSlotSeconds / 60)
			e.PeakRPM = &v
		}
		if e.Total > 0 {
			v := float64(e.Total) * 60 / duration
			e.AvgRPM = &v
		}
		if isToday {
			if v := recentByModel[m]; v > 0 {
				e.RecentRPM = &v
				if e.PeakRPM == nil || *e.PeakRPM < v {
					e.PeakRPM = &v
				}
			}
		}
		if a.t.InTok > 0 {
			e.TotalInputTokens = &a.t.InTok
		}
		if a.t.OutTok > 0 {
			e.TotalOutputTokens = &a.t.OutTok
		}
		if a.t.CacheRead > 0 {
			e.TotalCacheReadTokens = &a.t.CacheRead
		}
		if a.t.CacheWrite > 0 {
			e.TotalCacheWriteTokens = &a.t.CacheWrite
		}
		if a.t.SumGenMS > 0 {
			e.GenMS = &a.t.SumGenMS
		}
		if a.cost > 0 {
			e.TotalCost = &a.cost
			e.EffectiveCost = &a.cost
		}
		entries = append(entries, e)
	}
	respond(entries, h.rpmStatsFiltered(ctx, since, until, scope, isToday, scope.model, rpmTotal, rpmPeak))
}

// healthNumBuckets 是健康时间线的桶数（ccLoad fillHealthTimeline
// 同款 48 点列）。
const healthNumBuckets = 48

// healthBuckets 把与 [healthStart,until) 重叠的格子按重叠秒数份额
// 分摊进 48 桶，finalize 出 per-model 时间线——累加器形态供宿主
// 扫描一遍喂多个聚合，语义与原 eachCell 独立扫描一致：格子是全
// 槽聚合，跨桶界按比例分账（成功率/均值不变，计数为区间估计）。
// isToday 取最近 4h 按 5min 桶，否则按 range/48 桶。
type healthBuckets struct {
	startUnix int64
	bucketSec int64
	prices    map[string]CatalogPrice
	perModel  map[string][]healthBucketCell
}

// healthBucketCell 是单桶的份额累加（float 因为分摊产生小数）。
type healthBucketCell struct {
	succ, err, lim                 float64
	inT, outT, cr, cw, cost        float64
	durSum, durN, firstSum, firstN float64
}

func newHealthBuckets(since, until time.Time, isToday bool, prices map[string]CatalogPrice) *healthBuckets {
	var healthStart time.Time
	var bucketSec int64
	if isToday {
		bucketSec = 5 * 60
		healthStart = until.Add(-4 * time.Hour)
		if healthStart.Before(since) {
			healthStart = since
		}
	} else {
		bucketSec = int64(until.Sub(since).Seconds()) / healthNumBuckets
		if bucketSec < 1 {
			bucketSec = 1
		}
		healthStart = since
	}
	return &healthBuckets{
		startUnix: healthStart.Unix(),
		bucketSec: bucketSec,
		prices:    prices,
		perModel:  map[string][]healthBucketCell{},
	}
}

// covers 报告格子是否与 [healthStart,until) 重叠——复刻 eachCell 的
// 下界判据（slot+slotSec>since）；上界 slot<until 由宿主扫描的
// [since,until) 区间天然满足。
func (b *healthBuckets) covers(slot int64) bool {
	return slot+rollupSlotSeconds > b.startUnix
}

// add 把一个重叠格子按桶界份额分摊进各桶。
func (b *healthBuckets) add(key store.LogCellKey, c store.LogCellTotals) {
	fb := b.perModel[key.Model]
	if fb == nil {
		fb = make([]healthBucketCell, healthNumBuckets)
		b.perModel[key.Model] = fb
	}
	cellStart, cellEnd := key.Slot, key.Slot+rollupSlotSeconds
	cost := cellCostNG(key, c, b.prices)
	i0 := int((cellStart - b.startUnix) / b.bucketSec)
	i1 := int((cellEnd - 1 - b.startUnix) / b.bucketSec)
	for i := i0; i <= i1 && i < healthNumBuckets; i++ {
		if i < 0 {
			continue
		}
		bs := b.startUnix + int64(i)*b.bucketSec
		ov := min(cellEnd, bs+b.bucketSec) - max(cellStart, bs)
		if ov <= 0 {
			continue
		}
		share := float64(ov) / rollupSlotSeconds
		cell := &fb[i]
		cell.succ += float64(c.OK) * share
		cell.err += float64(c.Requests-c.OK-c.Gone) * share
		cell.lim += float64(c.Limited) * share
		cell.inT += float64(c.InTokNG) * share
		cell.outT += float64(c.OutTokNG) * share
		cell.cr += float64(c.CacheReadNG) * share
		cell.cw += float64(c.CacheWriteNG) * share
		cell.cost += cost * share
		cell.durSum += float64(c.SumDurOKMS) * share
		cell.durN += float64(c.NDurOK) * share
		cell.firstSum += float64(c.SumFirstOKMS) * share
		cell.firstN += float64(c.NFirstOK) * share
	}
}

// finalize 把分摊桶折算成 per-model 点列；只有 add 到格子的模型
// 出列，与原独立扫描同口径。
func (b *healthBuckets) finalize() map[string][]healthPoint {
	finalize := func(i int, bc healthBucketCell) healthPoint {
		p := healthPoint{
			Ts:          time.Unix(b.startUnix+int64(i)*b.bucketSec, 0),
			SuccessRate: -1,
			Success:     int64(math.Round(bc.succ)),
			Error:       int64(math.Round(bc.err)),
			RateLimited: int64(math.Round(bc.lim)),
		}
		if p.Success+p.Error > 0 {
			p.SuccessRate = float64(p.Success) / float64(p.Success+p.Error)
		}
		if bc.durN > 0 {
			p.AvgDuration = bc.durSum / bc.durN / 1000
		}
		if bc.firstN > 0 {
			p.AvgFirstByteTime = bc.firstSum / bc.firstN / 1000
		}
		p.InputTokens = int64(math.Round(bc.inT))
		p.OutputTokens = int64(math.Round(bc.outT))
		p.CacheReadTokens = int64(math.Round(bc.cr))
		p.CacheWriteTokens = int64(math.Round(bc.cw))
		p.Cost = bc.cost
		p.EffectiveCost = bc.cost
		return p
	}

	perModel := make(map[string][]healthPoint, len(b.perModel))
	for m, fb := range b.perModel {
		pts := make([]healthPoint, healthNumBuckets)
		for i := range pts {
			pts[i] = finalize(i, fb[i])
		}
		perModel[m] = pts
	}
	return perModel
}

// rpmStatsFiltered 把宿主扫描预积出的 total/peak（非 499 计数口径，
// 单槽峰值）折算成 rpm_stats 响应件——格子扫描由调用方一遍完成，
// 不再为 total/peak 单独重扫。recent_rpm 仅 isToday 有效，取最近
// 60s 的真实完成计数，并按 ccLoad 口径把 peak 抬到不低于 recent
// （格子折算的峰值会低估瞬时峰值）；recentModel/scope.kh 分别按
// 模型与令牌收敛。
func (h *Handler) rpmStatsFiltered(ctx context.Context, since, until time.Time, scope statScope, isToday bool, recentModel string, total, peak int64) map[string]any {
	minutes := until.Sub(since).Minutes()
	if minutes < 1 {
		minutes = 1
	}
	peakRPM := float64(peak) / (rollupSlotSeconds / 60)
	avgRPM := float64(total) / minutes
	recent := 0.0
	if isToday {
		recent = h.recentRPM(ctx, recentModel, scope.kh)
		if peakRPM < recent {
			peakRPM = recent
		}
	}
	return map[string]any{
		"peak_rpm":   peakRPM,
		"peak_qps":   peakRPM / 60,
		"avg_rpm":    avgRPM,
		"avg_qps":    avgRPM / 60,
		"recent_rpm": recent,
		"recent_qps": recent / 60,
	}
}

// zeroRPMStats 返回全零 rpm_stats（筛选排空时的响应件）。
func zeroRPMStats() map[string]any {
	return map[string]any{
		"peak_rpm": 0.0, "peak_qps": 0.0,
		"avg_rpm": 0.0, "avg_qps": 0.0,
		"recent_rpm": 0.0, "recent_qps": 0.0,
	}
}

// dashboardStatsFilterOptions 实现 /dashboard|/admin/stats/filter-options：
// 范围内出现过的模型名表（auth_token_id 查无令牌等排空场景为空表）。
func (h *Handler) dashboardStatsFilterOptions(w http.ResponseWriter, r *http.Request) {
	since, until, _ := resolveRange(r, time.Now())
	scope, excluded := h.queryScope(r)
	set := map[string]struct{}{}
	if !excluded {
		h.eachCell(r.Context(), since, until, scope, func(key store.LogCellKey, _ store.LogCellTotals) {
			if key.Model != "" {
				set[key.Model] = struct{}{}
			}
		})
	}
	models := make([]string, 0, len(set))
	for m := range set {
		models = append(models, m)
	}
	sort.Strings(models)
	respondOK(w, map[string]any{
		"models": models,
	})
}
