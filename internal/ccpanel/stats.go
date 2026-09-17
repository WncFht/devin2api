package ccpanel

import (
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// statsEntry 对应 ccLoad model.StatsEntry 的渠道列收缩版：stats 页按模型
// 聚合的行（模型维=生效模型，cellKey.model 同口径）。本服务单上游无渠道维。
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
// recent 环等无格子键的辅助结构沿用同一范围。
type statScope struct {
	kh        string
	api       string
	model     string
	modelLike string
}

// queryScope 把一次统计查询的数据范围折成格子谓词 + statScope。
// 范围来源两类：api_token 身份（强制只看自己的行）与 query 筛选
// （api、auth_token_id、model、model_like）。excluded=true 表示条件
// 不可能命中（auth_token_id 查无令牌、或与 api_token 身份冲突），
// 调用方直接回空集。
func (h *Handler) queryScope(r *http.Request) (match func(cellKey) bool, scope statScope, excluded bool) {
	q := r.URL.Query()
	if id := identityFrom(r); id.Role == "api_token" {
		scope.kh = id.KeyHash
		if scope.kh == "" {
			return nil, scope, true
		}
	}
	if raw := strings.TrimSpace(q.Get("auth_token_id")); raw != "" {
		tkh := ""
		if tid, err := strconv.ParseInt(raw, 10, 64); err == nil && h.tokens != nil {
			if t, ok := h.tokens.Get(tid); ok {
				tkh = t.KeyHash()
			}
		}
		if tkh == "" || (scope.kh != "" && tkh != scope.kh) {
			return nil, scope, true
		}
		scope.kh = tkh
	}
	scope.api = strings.TrimSpace(q.Get("api"))
	scope.model = strings.TrimSpace(q.Get("model"))
	scope.modelLike = strings.TrimSpace(q.Get("model_like"))
	if scope.kh == "" && scope.api == "" && scope.model == "" && scope.modelLike == "" {
		return nil, scope, false
	}
	return func(k cellKey) bool {
		if scope.kh != "" && k.kh != scope.kh {
			return false
		}
		if scope.api != "" && k.api != scope.api {
			return false
		}
		if scope.model != "" && k.model != scope.model {
			return false
		}
		if scope.modelLike != "" && !strings.Contains(k.model, scope.modelLike) {
			return false
		}
		return true
	}, scope, false
}

// dashboardStats 实现 /dashboard|/admin/stats：
// {stats:[StatsEntry per model], duration_seconds, rpm_stats, is_today}。
// success/error/total/499 口径与 ccLoad GetStats SQL 逐条对齐
// （success=2xx，error=非2xx非499，total=非499）。
func (h *Handler) dashboardStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since, until, rangeName := resolveRange(r, now)
	isToday := rangeName == "today"
	duration := until.Sub(since).Seconds()
	if duration < 1 {
		duration = 1
	}
	match, scope, excluded := h.queryScope(r)
	// recentBlock 聚合短窗（10s/60s）指标：req 非 499；tps 是生成速率
	// （Σ输出 ÷ Σ生成时长，同表格速度列口径）；ttfb 沿用格子口径；
	// cache_pct 同缓存命中列。recent 环容量 61s，覆盖两个窗。
	recentBlock := func(sec int64) map[string]any {
		a := h.ru.recentWindow(h.debug, sec, scope.api, scope.model, scope.modelLike, scope.kh)
		out := map[string]any{"requests": a.req}
		if a.req > 0 {
			out["rpm"] = float64(a.req) * 60 / float64(sec)
		}
		if a.genMS > 0 {
			out["tps"] = float64(a.outTok) * 1000 / float64(a.genMS)
		}
		if a.nFirst > 0 {
			out["ttfb_s"] = float64(a.firstMS) / float64(a.nFirst) / 1000
		}
		if a.nDur > 0 {
			out["dur_s"] = float64(a.durMS) / float64(a.nDur) / 1000
		}
		if d := a.inTok + a.crTok + a.cwTok; d > 0 {
			out["cache_pct"] = float64(a.crTok) * 100 / float64(d)
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
	prices := h.CatalogPrices(r.Context())

	type modelAgg struct {
		t    cellTotals
		cost float64
		peak int64 // 单槽非 499 峰值（per-model peak_rpm 的分子）
	}
	aggs := map[string]*modelAgg{}
	h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
		if match != nil && !match(key) {
			return
		}
		a := aggs[key.model]
		if a == nil {
			a = &modelAgg{}
			aggs[key.model] = a
		}
		a.t = addCells(a.t, c)
		a.cost += cellCost(key, c, prices)
		if n := c.requests - c.gone; n > a.peak {
			a.peak = n
		}
	})

	last := h.ru.lastByModel(h.debug, scope.kh)
	models := make([]string, 0, len(aggs))
	for m := range aggs {
		models = append(models, m)
	}
	sort.Strings(models)

	perModel := h.healthTimelines(since, until, isToday, match, prices)

	entries := make([]statsEntry, 0, len(models))
	for _, m := range models {
		a := aggs[m]
		e := statsEntry{
			Model:          m,
			Success:        a.t.ok,
			Error:          a.t.requests - a.t.ok - a.t.gone,
			Total:          a.t.requests - a.t.gone,
			HealthTimeline: perModel[m],
		}
		if a.t.nFirstOK > 0 {
			v := float64(a.t.sumFirstOKMS) / float64(a.t.nFirstOK) / 1000
			e.AvgFirstByteTimeSeconds = &v
		}
		if a.t.nDur > 0 {
			v := float64(a.t.sumDurMS) / float64(a.t.nDur) / 1000
			e.AvgDurationSeconds = &v
		}
		if l, ok := last[m]; ok {
			if l.okAt > 0 {
				at := l.okAt
				e.LastSuccessAt = &at
				e.LastSuccessID = &at // 日志行 id 即 started_at 毫秒戳（S2 投影口径）
			}
			if l.reqAt > 0 {
				at := l.reqAt
				status := l.reqStatus
				e.LastRequestAt = &at
				e.LastRequestID = &at
				e.LastRequestStatus = &status
				e.LastRequestMessage = l.reqResult
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
			if v := h.ru.recentRPM(h.debug, m, scope.kh); v > 0 {
				e.RecentRPM = &v
				if e.PeakRPM == nil || *e.PeakRPM < v {
					e.PeakRPM = &v
				}
			}
		}
		if a.t.inTok > 0 {
			e.TotalInputTokens = &a.t.inTok
		}
		if a.t.outTok > 0 {
			e.TotalOutputTokens = &a.t.outTok
		}
		if a.t.cacheRead > 0 {
			e.TotalCacheReadTokens = &a.t.cacheRead
		}
		if a.t.cacheWrite > 0 {
			e.TotalCacheWriteTokens = &a.t.cacheWrite
		}
		if a.t.sumGenMS > 0 {
			e.GenMS = &a.t.sumGenMS
		}
		if a.cost > 0 {
			e.TotalCost = &a.cost
			e.EffectiveCost = &a.cost
		}
		entries = append(entries, e)
	}
	respond(entries, h.rpmStatsFiltered(since, until, match, isToday, scope.model, scope.kh))
}

// healthTimelines 复刻 ccLoad fillHealthTimeline 的 per-model 部分：
// isToday 取最近 4h 按 5min×48 桶，否则按 range/48 桶。
// 格子分辨率 10min：今日档一个格子跨两个桶，按重叠秒数比例分摊计数
// （成功率/均值不变，计数为区间估计）。单上游无渠道聚合时间线。
func (h *Handler) healthTimelines(since, until time.Time, isToday bool, match func(cellKey) bool, prices map[string]CatalogPrice) map[string][]healthPoint {
	const numBuckets = 48
	var healthStart time.Time
	var bucketSec int64
	if isToday {
		bucketSec = 5 * 60
		healthStart = until.Add(-4 * time.Hour)
		if healthStart.Before(since) {
			healthStart = since
		}
	} else {
		bucketSec = int64(until.Sub(since).Seconds()) / numBuckets
		if bucketSec < 1 {
			bucketSec = 1
		}
		healthStart = since
	}
	startUnix := healthStart.Unix()

	type fBucket struct {
		succ, err, lim                 float64
		inT, outT, cr, cw, cost        float64
		durSum, durN, firstSum, firstN float64
	}
	perModelF := map[string]*[numBuckets]fBucket{}
	h.ru.eachCell(h.debug, healthStart, until, func(key cellKey, c cellTotals) {
		if match != nil && !match(key) {
			return
		}
		fb := perModelF[key.model]
		if fb == nil {
			fb = &[numBuckets]fBucket{}
			perModelF[key.model] = fb
		}
		cellStart, cellEnd := key.slot, key.slot+rollupSlotSeconds
		cost := cellCostNG(key, c, prices)
		i0 := int((cellStart - startUnix) / bucketSec)
		i1 := int((cellEnd - 1 - startUnix) / bucketSec)
		for i := i0; i <= i1 && i < numBuckets; i++ {
			if i < 0 {
				continue
			}
			bs := startUnix + int64(i)*bucketSec
			ov := min(cellEnd, bs+bucketSec) - max(cellStart, bs)
			if ov <= 0 {
				continue
			}
			share := float64(ov) / rollupSlotSeconds
			b := &fb[i]
			b.succ += float64(c.ok) * share
			b.err += float64(c.requests-c.ok-c.gone) * share
			b.lim += float64(c.limited) * share
			b.inT += float64(c.inTokNG) * share
			b.outT += float64(c.outTokNG) * share
			b.cr += float64(c.cacheReadNG) * share
			b.cw += float64(c.cacheWriteNG) * share
			b.cost += cost * share
			b.durSum += float64(c.sumDurOKMS) * share
			b.durN += float64(c.nDurOK) * share
			b.firstSum += float64(c.sumFirstOKMS) * share
			b.firstN += float64(c.nFirstOK) * share
		}
	})

	finalize := func(i int, b fBucket) healthPoint {
		p := healthPoint{
			Ts:          time.Unix(startUnix+int64(i)*bucketSec, 0),
			SuccessRate: -1,
			Success:     int64(math.Round(b.succ)),
			Error:       int64(math.Round(b.err)),
			RateLimited: int64(math.Round(b.lim)),
		}
		if p.Success+p.Error > 0 {
			p.SuccessRate = float64(p.Success) / float64(p.Success+p.Error)
		}
		if b.durN > 0 {
			p.AvgDuration = b.durSum / b.durN / 1000
		}
		if b.firstN > 0 {
			p.AvgFirstByteTime = b.firstSum / b.firstN / 1000
		}
		p.InputTokens = int64(math.Round(b.inT))
		p.OutputTokens = int64(math.Round(b.outT))
		p.CacheReadTokens = int64(math.Round(b.cr))
		p.CacheWriteTokens = int64(math.Round(b.cw))
		p.Cost = b.cost
		p.EffectiveCost = b.cost
		return p
	}

	perModel := make(map[string][]healthPoint, len(perModelF))
	for m, fb := range perModelF {
		pts := make([]healthPoint, numBuckets)
		for i := range pts {
			pts[i] = finalize(i, fb[i])
		}
		perModel[m] = pts
	}
	return perModel
}

// rpmStatsFiltered 由 10 分钟格子推导 RPM/QPS：计数口径非 499
// （ccLoad GetRPMStats 的 WHERE status_code != 499），peak 取单槽
// 峰值折算分钟速率。recent_rpm 仅 isToday 有效，取 recent 环的真实
// 60s 计数，并按 ccLoad 口径把 peak 抬到不低于 recent（格子折算的
// 峰值会低估瞬时峰值）；recentModel/kh 分别按模型与令牌收敛计数。
func (h *Handler) rpmStatsFiltered(since, until time.Time, match func(cellKey) bool, isToday bool, recentModel, kh string) map[string]any {
	var total, peak int64
	h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
		if match != nil && !match(key) {
			return
		}
		n := c.requests - c.gone
		total += n
		if n > peak {
			peak = n
		}
	})
	minutes := until.Sub(since).Minutes()
	if minutes < 1 {
		minutes = 1
	}
	peakRPM := float64(peak) / (rollupSlotSeconds / 60)
	avgRPM := float64(total) / minutes
	recent := 0.0
	if isToday {
		recent = h.ru.recentRPM(h.debug, recentModel, kh)
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
	match, _, excluded := h.queryScope(r)
	set := map[string]struct{}{}
	if !excluded {
		h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
			if key.model == "" {
				return
			}
			if match != nil && !match(key) {
				return
			}
			set[key.model] = struct{}{}
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
