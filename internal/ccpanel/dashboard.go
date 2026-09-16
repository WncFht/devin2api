package ccpanel

import (
	"net/http"
	"strconv"
	"time"

	"github.com/WncFht/devin2api/internal/dashboard"
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
// api 取 index.jsonl 的原值（anthropic/openai-chat/openai-responses/responses-ws）。
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

// dashboardSummary 实现 /dashboard/summary：按入口端点（index.jsonl api
// 字段）分组的用量卡片。口径对齐 ccLoad GetClientProtocolStats：
// success=2xx、error=非2xx非499、total=success+error，token/成本求和
// 含 499 行；api_token 身份收敛到自己的 key_hash 行。
func (h *Handler) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since, until, rangeName := resolveRange(r, now)
	isToday := rangeName == "today"
	match, kh, excluded := h.queryScope(r)
	prices := h.panel.CatalogPrices(r.Context())

	byAPI := map[string]*endpointStat{}
	var grand endpointStat
	if !excluded {
		h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
			if match != nil && !match(key) {
				return
			}
			stat := byAPI[key.api]
			if stat == nil {
				stat = &endpointStat{API: key.api}
				byAPI[key.api] = stat
			}
			cost := cellCost(key, c, prices)
			for _, dst := range []*endpointStat{stat, &grand} {
				dst.TotalRequests += c.requests - c.gone
				dst.SuccessRequests += c.ok
				dst.ErrorRequests += c.requests - c.ok - c.gone
				dst.TotalInputTokens += c.inTok
				dst.TotalOutputTokens += c.outTok
				dst.TotalCacheReadTokens += c.cacheRead
				dst.TotalCacheCreationTokens += c.cacheWrite
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
		rpm = h.rpmStatsFiltered(since, until, match, isToday, "", kh)
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

// dashboardMetrics 实现 /dashboard/metrics：按 bucket_min 聚合的时间桶点列，
// models 键为模型名（本服务无多上游渠道，模型即最细维度）。口径对齐
// ccLoad AggregateRangeWithFilter：success=2xx、error=非2xx非499，
// token/成本只计非 499 行（NG 字段），均值样本为 2xx 且时值>0 的行。
// 无论有无数据都补出 [since,until] 对齐 bucket 边界的满序列（metrics_finalize
// 同款），前端按点位对齐多序列；格子粒度 10 分钟，更小的 bucket 抬到 10。
func (h *Handler) dashboardMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since, until, _ := resolveRange(r, now)
	bucketMin, _ := strconv.Atoi(r.URL.Query().Get("bucket_min"))
	if bucketMin <= 0 {
		bucketMin = 5
	}
	if bucketMin < 10 {
		bucketMin = 10
	}
	bucketSec := int64(bucketMin) * 60
	match, _, excluded := h.queryScope(r)
	prices := h.panel.CatalogPrices(r.Context())

	type bucketAgg struct {
		total     cellTotals
		cost      float64
		byModel   map[string]*cellTotals
		modelCost map[string]float64
	}
	buckets := map[int64]*bucketAgg{}
	if !excluded {
		h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
			if match != nil && !match(key) {
				return
			}
			// 整格归入槽起点所在桶：bucket 不是 10 分钟倍数时边界有
			// ±10 分钟错位（格子分辨率下限），前端常用档位均为整倍数。
			b := key.slot / bucketSec * bucketSec
			a := buckets[b]
			if a == nil {
				a = &bucketAgg{byModel: map[string]*cellTotals{}, modelCost: map[string]float64{}}
				buckets[b] = a
			}
			cost := cellCostNG(key, c, prices)
			a.cost += cost
			mt := a.byModel[key.model]
			if mt == nil {
				mt = &cellTotals{}
				a.byModel[key.model] = mt
			}
			*mt = addCells(*mt, c)
			a.modelCost[key.model] += cost
			a.total = addCells(a.total, c)
		})
	}

	fill := func(p *metricPoint, t cellTotals, cost float64) {
		p.Success = t.ok
		p.Error = t.requests - t.ok - t.gone
		p.FirstByteSampleCount = t.nFirstOK
		p.DurationSampleCount = t.nDurOK
		p.InputTokens = t.inTokNG
		p.OutputTokens = t.outTokNG
		p.CacheReadTokens = t.cacheReadNG
		p.CacheCreationTokens = t.cacheWriteNG
		if t.nFirstOK > 0 {
			v := float64(t.sumFirstOKMS) / float64(t.nFirstOK) / 1000
			p.AvgFirstByteTimeSeconds = &v
		}
		if t.nDurOK > 0 {
			v := float64(t.sumDurOKMS) / float64(t.nDurOK) / 1000
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
					Success:             t.ok,
					Error:               t.requests - t.ok - t.gone,
					InputTokens:         t.inTokNG,
					OutputTokens:        t.outTokNG,
					CacheReadTokens:     t.cacheReadNG,
					CacheCreationTokens: t.cacheWriteNG,
				}
				if t.nFirstOK > 0 {
					v := float64(t.sumFirstOKMS) / float64(t.nFirstOK) / 1000
					mc.AvgFirstByteTimeSeconds = &v
				}
				if t.nDurOK > 0 {
					v := float64(t.sumDurOKMS) / float64(t.nDurOK) / 1000
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
	respondOK(w, points)
}

// cellCost 按目录价折算单格成本（含 499 行的 token 口径，summary/stats/
// token 覆盖用）；无目录价的模型贡献 0。
// 与旧面板 usage 页同口径：cache_write 按 input 价计费（目录无独立价格维）。
func cellCost(key cellKey, c cellTotals, prices map[string]dashboard.CatalogPrice) float64 {
	return tokenCost(key.model, c.inTok, c.outTok, c.cacheRead, c.cacheWrite, prices)
}

// cellCostNG 与 cellCost 同式，但取非 499 行的 token 口径（metrics/health 用）。
func cellCostNG(key cellKey, c cellTotals, prices map[string]dashboard.CatalogPrice) float64 {
	return tokenCost(key.model, c.inTokNG, c.outTokNG, c.cacheReadNG, c.cacheWriteNG, prices)
}

// tokenCost 是目录价折算公式：prompt 侧 input+cache_write 按 input 价、
// cache_read 按 cached 价、output 按 output 价，目录单位是 USD/百万 token。
func tokenCost(model string, in, out, cacheRead, cacheWrite int64, prices map[string]dashboard.CatalogPrice) float64 {
	p, ok := prices[model]
	if !ok {
		return 0
	}
	return (float64(in+cacheWrite)*p.Input + float64(cacheRead)*p.Cached + float64(out)*p.Output) / 1e6
}

// dashboardModels 实现 /dashboard/models 与 /admin/models：
// 日志里出现过的模型 + 出现过的状态码。
// api_token 身份收敛到自己产生过流量的模型。
func (h *Handler) dashboardModels(w http.ResponseWriter, r *http.Request) {
	respondOK(w, map[string]any{
		"models":       h.ru.modelSet(h.debug, identityFrom(r).KeyHash),
		"status_codes": h.ru.statusCodeSet(h.debug),
	})
}
