package ccpanel

import (
	"net/http"
	"strconv"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// resolveRange 复刻 ccLoad PaginationParams.GetTimeRange 的口径：
// range=today|yesterday|day_before_yesterday|this_week|last_week|
// this_month|last_month|custom（custom 用 start_time/end_time 毫秒戳，
// 非法时回落 today；end 超现在截到现在）。
func resolveRange(r *http.Request, now time.Time) (since, until time.Time, rangeName string) {
	rangeName = r.URL.Query().Get("range")
	loc := now.Local()
	beginDay := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	}
	endDay := func(t time.Time) time.Time {
		return beginDay(t).AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	beginWeek := func(t time.Time) time.Time {
		// 周一为一周起点（ccLoad beginningOfWeek 同口径）。
		d := beginDay(t)
		offset := (int(d.Weekday()) + 6) % 7
		return d.AddDate(0, 0, -offset)
	}
	beginMonth := func(t time.Time) time.Time {
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
	}
	switch rangeName {
	case "custom":
		startMS, _ := strconv.ParseInt(r.URL.Query().Get("start_time"), 10, 64)
		endMS, _ := strconv.ParseInt(r.URL.Query().Get("end_time"), 10, 64)
		s, u := time.UnixMilli(startMS), time.UnixMilli(endMS)
		if startMS > 0 && endMS > 0 && s.Before(now) && u.After(s) {
			if u.After(now) {
				u = now
			}
			return s, u, rangeName
		}
		return beginDay(now), now, rangeName
	case "yesterday":
		y := now.AddDate(0, 0, -1)
		return beginDay(y), endDay(y), rangeName
	case "day_before_yesterday":
		d := now.AddDate(0, 0, -2)
		return beginDay(d), endDay(d), rangeName
	case "this_week":
		return beginWeek(now), now, rangeName
	case "last_week":
		lw := now.AddDate(0, 0, -7)
		return beginWeek(lw), beginWeek(lw).AddDate(0, 0, 7).Add(-time.Nanosecond), rangeName
	case "this_month":
		return beginMonth(now), now, rangeName
	case "last_month":
		lm := now.AddDate(0, -1, 0)
		return beginMonth(lm), beginMonth(now).Add(-time.Nanosecond), rangeName
	default: // today 与未知值同口径
		return beginDay(loc), now, "today"
	}
}

// endpointStat 是 /dashboard/summary 按入口端点分组的用量行；
// api 取日志行的原值（anthropic/openai-chat/openai-responses/responses-ws）。
type endpointStat struct {
	API                      string  `json:"api,omitempty"`
	TotalRequests            int64   `json:"total_requests"`
	SuccessRequests          int64   `json:"success_requests"`
	ErrorRequests            int64   `json:"error_requests"`
	TotalInputTokens         int64   `json:"total_input_tokens,omitempty"`
	TotalOutputTokens        int64   `json:"total_output_tokens,omitempty"`
	TotalCacheReadTokens     int64   `json:"total_cache_read_tokens,omitempty"`
	TotalCacheCreationTokens int64   `json:"total_cache_creation_tokens,omitempty"`
	TotalCost                float64 `json:"total_cost,omitempty"`
	EffectiveCost            float64 `json:"effective_cost"`
}

// dashboardSummary 实现 /dashboard/summary：按入口端点（日志行 api
// 字段）分组的用量卡片。口径对齐 ccLoad GetClientProtocolStats：
// success=2xx、error=非2xx非499、total=success+error，token/成本求和
// 含 499 行；api_token 身份收敛到自己的 key_hash 行。
func (h *Handler) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := time.Now()
	since, until, rangeName := resolveRange(r, now)
	isToday := rangeName == "today"
	scope, excluded := h.queryScope(r)
	prices := h.CatalogPrices(ctx)

	byAPI := map[string]*endpointStat{}
	var grand endpointStat
	if !excluded {
		h.eachCell(ctx, since, until, scope, func(key store.LogCellKey, c store.LogCellTotals) {
			stat := byAPI[key.API]
			if stat == nil {
				stat = &endpointStat{API: key.API}
				byAPI[key.API] = stat
			}
			cost := cellCost(key, c, prices)
			for _, dst := range []*endpointStat{stat, &grand} {
				dst.TotalRequests += c.Requests - c.Gone
				dst.SuccessRequests += c.OK
				dst.ErrorRequests += c.Requests - c.OK - c.Gone
				dst.TotalInputTokens += c.InTok
				dst.TotalOutputTokens += c.OutTok
				dst.TotalCacheReadTokens += c.CacheRead
				dst.TotalCacheCreationTokens += c.CacheWrite
				dst.TotalCost += cost
				dst.EffectiveCost += cost
			}
		})
	}

	apis := make(map[string]endpointStat, len(byAPI))
	for k, v := range byAPI {
		apis[k] = *v
	}

	duration := until.Sub(since).Seconds()
	if duration < 1 {
		duration = 1
	}
	rpm := zeroRPMStats()
	if !excluded {
		rpm = h.rpmStatsFiltered(ctx, since, until, scope, isToday, "")
	}
	respondOK(w, map[string]any{
		"total_requests":   grand.TotalRequests,
		"success_requests": grand.SuccessRequests,
		"error_requests":   grand.ErrorRequests,
		"range":            rangeName,
		"duration_seconds": duration,
		"rpm_stats":        rpm,
		"is_today":         isToday,
		"by_api":           apis,
	})
}

// metricPoint/metricModel 对应 ccLoad 的 MetricPoint/ChannelMetric：
// models 按模型名键控（本服务无多上游渠道，模型即最细维度）。
type metricModel struct {
	Success                 int64    `json:"success"`
	Error                   int64    `json:"error"`
	AvgFirstByteTimeSeconds *float64 `json:"avg_first_byte_time_seconds,omitempty"`
	AvgDurationSeconds      *float64 `json:"avg_duration_seconds,omitempty"`
	TotalCost               *float64 `json:"total_cost,omitempty"`
	EffectiveCost           *float64 `json:"effective_cost,omitempty"`
	InputTokens             int64    `json:"input_tokens,omitempty"`
	OutputTokens            int64    `json:"output_tokens,omitempty"`
	CacheReadTokens         int64    `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens     int64    `json:"cache_creation_tokens,omitempty"`
}

type metricPoint struct {
	Ts                      time.Time              `json:"ts"`
	Success                 int64                  `json:"success"`
	Error                   int64                  `json:"error"`
	AvgFirstByteTimeSeconds *float64               `json:"avg_first_byte_time_seconds,omitempty"`
	AvgDurationSeconds      *float64               `json:"avg_duration_seconds,omitempty"`
	TotalCost               *float64               `json:"total_cost,omitempty"`
	EffectiveCost           *float64               `json:"effective_cost,omitempty"`
	FirstByteSampleCount    int64                  `json:"first_byte_count,omitempty"`
	DurationSampleCount     int64                  `json:"duration_count,omitempty"`
	InputTokens             int64                  `json:"input_tokens,omitempty"`
	OutputTokens            int64                  `json:"output_tokens,omitempty"`
	CacheReadTokens         int64                  `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens     int64                  `json:"cache_creation_tokens,omitempty"`
	Models                  map[string]metricModel `json:"models,omitempty"`
}

// dashboardMetrics 实现 /dashboard/metrics：按 bucket_sec 聚合的时间桶点列，
// models 键为模型名（本服务无多上游渠道，模型即最细维度）。口径对齐
// ccLoad AggregateRangeWithFilter：success=2xx、error=非2xx非499，
// token/成本只计非 499 行（NG 字段），均值样本为 2xx 且时值>0 的行。
// 无论有无数据都补出 [since,until] 对齐 bucket 边界的满序列（metrics_finalize
// 同款），前端按点位对齐多序列；格子直接按桶宽出槽（下限 10s、上限 1d），
// 点数超 maxMetricPoints 时桶宽上抬到整齐档，生效值经 X-Bucket-Sec 回传。
func (h *Handler) dashboardMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since, until, _ := resolveRange(r, now)
	bucketSec, _ := strconv.ParseInt(r.URL.Query().Get("bucket_sec"), 10, 64)
	if bucketSec <= 0 {
		bucketSec = rollupSlotSeconds
	}
	bucketSec = clampMetricBucket(bucketSec, until.Unix()-since.Unix())
	scope, excluded := h.queryScope(r)
	prices := h.CatalogPrices(r.Context())

	// totalReqs 是范围内命中行的总请求数（非 499，同 summary.total_requests
	// 与图表 requestCount 口径），经 X-Debug-Total 头给趋势页页脚——边界
	// 半格也计入，比前端对满序列求和更贴近查询窗口真实值。
	var totalReqs int64
	type bucketAgg struct {
		total     store.LogCellTotals
		cost      float64
		byModel   map[string]*store.LogCellTotals
		modelCost map[string]float64
	}
	buckets := map[int64]*bucketAgg{}
	if !excluded {
		h.eachCellSec(r.Context(), bucketSec, since, until, scope, func(key store.LogCellKey, c store.LogCellTotals) {
			totalReqs += c.Requests - c.Gone
			// 槽宽即桶宽，格子槽起点直接是桶号。
			a := buckets[key.Slot]
			if a == nil {
				a = &bucketAgg{byModel: map[string]*store.LogCellTotals{}, modelCost: map[string]float64{}}
				buckets[key.Slot] = a
			}
			cost := cellCostNG(key, c, prices)
			a.cost += cost
			mt := a.byModel[key.Model]
			if mt == nil {
				mt = &store.LogCellTotals{}
				a.byModel[key.Model] = mt
			}
			*mt = addCells(*mt, c)
			a.modelCost[key.Model] += cost
			a.total = addCells(a.total, c)
		})
	}

	fill := func(p *metricPoint, t store.LogCellTotals, cost float64) {
		p.Success = t.OK
		p.Error = t.Requests - t.OK - t.Gone
		p.FirstByteSampleCount = t.NFirstOK
		p.DurationSampleCount = t.NDurOK
		p.InputTokens = t.InTokNG
		p.OutputTokens = t.OutTokNG
		p.CacheReadTokens = t.CacheReadNG
		p.CacheCreationTokens = t.CacheWriteNG
		if t.NFirstOK > 0 {
			v := float64(t.SumFirstOKMS) / float64(t.NFirstOK) / 1000
			p.AvgFirstByteTimeSeconds = &v
		}
		if t.NDurOK > 0 {
			v := float64(t.SumDurOKMS) / float64(t.NDurOK) / 1000
			p.AvgDurationSeconds = &v
		}
		if cost > 0 {
			p.TotalCost = &cost
			p.EffectiveCost = &cost
		}
	}

	// 满序列：范围端点各自截到 bucket 边界，逐桶吐点，空桶给零值点。
	start := since.Unix() / bucketSec * bucketSec
	end := until.Unix() / bucketSec * bucketSec
	points := make([]metricPoint, 0, (end-start)/bucketSec+1)
	for b := start; b <= end; b += bucketSec {
		p := metricPoint{Ts: time.Unix(b, 0)}
		if a := buckets[b]; a != nil {
			fill(&p, a.total, a.cost)
			p.Models = make(map[string]metricModel, len(a.byModel))
			for model, t := range a.byModel {
				mc := metricModel{
					Success:             t.OK,
					Error:               t.Requests - t.OK - t.Gone,
					InputTokens:         t.InTokNG,
					OutputTokens:        t.OutTokNG,
					CacheReadTokens:     t.CacheReadNG,
					CacheCreationTokens: t.CacheWriteNG,
				}
				if t.NFirstOK > 0 {
					v := float64(t.SumFirstOKMS) / float64(t.NFirstOK) / 1000
					mc.AvgFirstByteTimeSeconds = &v
				}
				if t.NDurOK > 0 {
					v := float64(t.SumDurOKMS) / float64(t.NDurOK) / 1000
					mc.AvgDurationSeconds = &v
				}
				if c := a.modelCost[model]; c > 0 {
					mc.TotalCost = &c
					mc.EffectiveCost = &c
				}
				p.Models[model] = mc
			}
		}
		points = append(points, p)
	}
	w.Header().Set("X-Debug-Total", strconv.FormatInt(totalReqs, 10))
	w.Header().Set("X-Bucket-Sec", strconv.FormatInt(bucketSec, 10))
	respondOK(w, points)
}

// maxMetricPoints 是 metrics 满序列的点数上限；metricBucketSteps 是点数
// 超限时桶宽上取的整齐档（秒），保证间隔标签始终是整秒/整分/整时。
const maxMetricPoints = 2880

var metricBucketSteps = []int64{10, 15, 20, 30, 60, 120, 180, 300, 600, 900, 1200, 1800, 3600, 7200, 10800, 21600, 43200, 86400}

// clampMetricBucket 把请求桶宽收进 [10s,1d]，并在点数超上限时上抬到
// 能放下的最小整齐档。
func clampMetricBucket(bucketSec, spanSec int64) int64 {
	if bucketSec < 10 {
		bucketSec = 10
	}
	if bucketSec > 86400 {
		bucketSec = 86400
	}
	if spanSec/bucketSec+1 > maxMetricPoints {
		bucketSec = spanSec/maxMetricPoints + 1
		for _, s := range metricBucketSteps {
			if s >= bucketSec {
				bucketSec = s
				break
			}
		}
	}
	return bucketSec
}

// cellCost 按目录价折算单格成本（含 499 行的 token 口径，summary/stats/
// token 覆盖用）；无目录价的模型贡献 0。
// 与旧面板 usage 页同口径：cache_write 按 input 价计费（目录无独立价格维）。
func cellCost(key store.LogCellKey, c store.LogCellTotals, prices map[string]CatalogPrice) float64 {
	return TokenCost(c.InTok, c.OutTok, c.CacheRead, c.CacheWrite, prices[key.Model])
}

// cellCostNG 与 cellCost 同式，但取非 499 行的 token 口径（metrics/health 用）。
func cellCostNG(key store.LogCellKey, c store.LogCellTotals, prices map[string]CatalogPrice) float64 {
	return TokenCost(c.InTokNG, c.OutTokNG, c.CacheReadNG, c.CacheWriteNG, prices[key.Model])
}

// dashboardModels 实现 /dashboard/models 与 /admin/models：
// 日志里出现过的模型 + 出现过的状态码。
// api_token 身份收敛到自己产生过流量的模型。
func (h *Handler) dashboardModels(w http.ResponseWriter, r *http.Request) {
	respondOK(w, map[string]any{
		"models":       h.modelSet(r.Context(), identityFrom(r).KeyHash),
		"status_codes": h.statusCodeSet(r.Context()),
	})
}
