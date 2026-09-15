package ccpanel

import (
	"net/http"
	"sort"
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

// protocolStat 对应 ccLoad 的 ClientProtocolStats / AuthTypeStats（字段同名）。
type protocolStat struct {
	ClientProtocol           string  `json:"client_protocol,omitempty"`
	AuthType                 string  `json:"auth_type,omitempty"`
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

// dashboardSummary 实现 ccLoad 的 /dashboard/summary：按入口协议与认证
// 类型（恒 api_key）分组的用量卡片数据。
func (h *Handler) dashboardSummary(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since, until, rangeName := resolveRange(r, now)
	prices := h.panel.CatalogPrices(r.Context())

	byProtocol := map[string]*protocolStat{}
	var grand protocolStat
	h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
		proto := clientProtocol(key.api)
		stat := byProtocol[proto]
		if stat == nil {
			stat = &protocolStat{ClientProtocol: proto}
			byProtocol[proto] = stat
		}
		cost := cellCost(key, c, prices)
		for _, dst := range []*protocolStat{stat, &grand} {
			dst.TotalRequests += c.requests
			dst.ErrorRequests += c.failures
			dst.SuccessRequests += c.requests - c.failures
			dst.TotalInputTokens += c.inTok
			dst.TotalOutputTokens += c.outTok
			dst.TotalCacheReadTokens += c.cacheRead
			dst.TotalCacheCreationTokens += c.cacheWrite
			dst.TotalCost += cost
			dst.EffectiveCost += cost
		}
	})

	protocols := make(map[string]protocolStat, len(byProtocol))
	for k, v := range byProtocol {
		protocols[k] = *v
	}
	// 认证类型卡：本服务只有一种下游凭据形态，全部流量归 api_key。
	grand.AuthType = "api_key"
	byAuth := map[string]protocolStat{}
	if grand.TotalRequests > 0 {
		byAuth["api_key"] = grand
	}

	duration := until.Sub(since).Seconds()
	if duration < 1 {
		duration = 1
	}
	respondOK(w, map[string]any{
		"total_requests":     grand.TotalRequests,
		"success_requests":   grand.SuccessRequests,
		"error_requests":     grand.ErrorRequests,
		"range":              rangeName,
		"duration_seconds":   duration,
		"rpm_stats":          h.rpmStats(since, until),
		"is_today":           rangeName == "" || rangeName == "today",
		"by_client_protocol": protocols,
		"by_auth_type":       byAuth,
	})
}

// rpmStats 由 10 分钟格子推导 RPM/QPS 估计：peak 取单槽请求数/10 分钟，
// recent 取 recent 环的真实 60s 计数。
func (h *Handler) rpmStats(since, until time.Time) map[string]any {
	var total, peakSlot int64
	h.ru.eachCell(h.debug, since, until, func(_ cellKey, c cellTotals) {
		total += c.requests
		if c.requests > peakSlot {
			peakSlot = c.requests
		}
	})
	minutes := until.Sub(since).Minutes()
	if minutes < 1 {
		minutes = 1
	}
	peakRPM := float64(peakSlot) / (rollupSlotSeconds / 60)
	avgRPM := float64(total) / minutes
	recent := h.ru.recentRPM(h.debug)
	return map[string]any{
		"peak_rpm":   peakRPM,
		"peak_qps":   peakRPM / 60,
		"avg_rpm":    avgRPM,
		"avg_qps":    avgRPM / 60,
		"recent_rpm": recent,
		"recent_qps": recent / 60,
	}
}

// metricPoint/metricChannel 对应 ccLoad 的 MetricPoint/ChannelMetric：
// channels 按模型名键控（本服务无多上游渠道，模型即最细维度）。
type metricChannel struct {
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
	Ts                      time.Time                `json:"ts"`
	Success                 int64                    `json:"success"`
	Error                   int64                    `json:"error"`
	AvgFirstByteTimeSeconds *float64                 `json:"avg_first_byte_time_seconds,omitempty"`
	AvgDurationSeconds      *float64                 `json:"avg_duration_seconds,omitempty"`
	TotalCost               *float64                 `json:"total_cost,omitempty"`
	EffectiveCost           *float64                 `json:"effective_cost,omitempty"`
	FirstByteSampleCount    int64                    `json:"first_byte_count,omitempty"`
	DurationSampleCount     int64                    `json:"duration_count,omitempty"`
	InputTokens             int64                    `json:"input_tokens,omitempty"`
	OutputTokens            int64                    `json:"output_tokens,omitempty"`
	CacheReadTokens         int64                    `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens     int64                    `json:"cache_creation_tokens,omitempty"`
	Channels                map[string]metricChannel `json:"channels,omitempty"`
}

// dashboardMetrics 实现 /dashboard/metrics：按 bucket_min 聚合的时间桶点列，
// channels 键为模型名。model 参数按生效模型精确过滤。
func (h *Handler) dashboardMetrics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	since, until, _ := resolveRange(r, now)
	bucketMin, _ := strconv.Atoi(r.URL.Query().Get("bucket_min"))
	if bucketMin <= 0 {
		bucketMin = 5
	}
	// 格子粒度 10 分钟：小于它的 bucket 直接抬到 10，避免输出
	// 大量空桶（10 分钟桶已是存储分辨率下限）。
	if bucketMin < 10 {
		bucketMin = 10
	}
	bucketSec := int64(bucketMin) * 60
	modelFilter := r.URL.Query().Get("model")
	prices := h.panel.CatalogPrices(r.Context())

	type bucketAgg struct {
		total     cellTotals
		cost      float64
		byModel   map[string]*cellTotals
		modelCost map[string]float64
	}
	buckets := map[int64]*bucketAgg{}
	h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
		if modelFilter != "" && key.model != modelFilter {
			return
		}
		b := key.slot / bucketSec * bucketSec
		a := buckets[b]
		if a == nil {
			a = &bucketAgg{byModel: map[string]*cellTotals{}, modelCost: map[string]float64{}}
			buckets[b] = a
		}
		cost := cellCost(key, c, prices)
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

	ts := make([]int64, 0, len(buckets))
	for b := range buckets {
		ts = append(ts, b)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	points := make([]metricPoint, 0, len(ts))
	for _, b := range ts {
		a := buckets[b]
		p := metricPoint{
			Ts:                   time.Unix(b, 0),
			Success:              a.total.requests - a.total.failures,
			Error:                a.total.failures,
			FirstByteSampleCount: a.total.nFirst,
			DurationSampleCount:  a.total.requests,
			InputTokens:          a.total.inTok,
			OutputTokens:         a.total.outTok,
			CacheReadTokens:      a.total.cacheRead,
			CacheCreationTokens:  a.total.cacheWrite,
		}
		if a.total.nFirst > 0 {
			v := float64(a.total.sumFirstMS) / float64(a.total.nFirst) / 1000
			p.AvgFirstByteTimeSeconds = &v
		}
		if a.total.requests > 0 {
			v := float64(a.total.sumDurMS) / float64(a.total.requests) / 1000
			p.AvgDurationSeconds = &v
		}
		if a.cost > 0 {
			p.TotalCost = &a.cost
			p.EffectiveCost = &a.cost
		}
		if len(a.byModel) > 0 {
			p.Channels = make(map[string]metricChannel, len(a.byModel))
			for model, t := range a.byModel {
				mc := metricChannel{
					Success:             t.requests - t.failures,
					Error:               t.failures,
					InputTokens:         t.inTok,
					OutputTokens:        t.outTok,
					CacheReadTokens:     t.cacheRead,
					CacheCreationTokens: t.cacheWrite,
				}
				if t.nFirst > 0 {
					v := float64(t.sumFirstMS) / float64(t.nFirst) / 1000
					mc.AvgFirstByteTimeSeconds = &v
				}
				if t.requests > 0 {
					v := float64(t.sumDurMS) / float64(t.requests) / 1000
					mc.AvgDurationSeconds = &v
				}
				if c := a.modelCost[model]; c > 0 {
					mc.TotalCost = &c
					mc.EffectiveCost = &c
				}
				p.Channels[model] = mc
			}
		}
		points = append(points, p)
	}
	respondOK(w, points)
}

// addCells 返回 a+b 的逐字段和。
func addCells(a, b cellTotals) cellTotals {
	a.requests += b.requests
	a.failures += b.failures
	a.inTok += b.inTok
	a.outTok += b.outTok
	a.cacheRead += b.cacheRead
	a.cacheWrite += b.cacheWrite
	a.sumDurMS += b.sumDurMS
	a.sumFirstMS += b.sumFirstMS
	a.nFirst += b.nFirst
	return a
}

// cellCost 按目录价折算单格成本；无目录价的模型贡献 0。
// 与旧面板 usage 页同口径：cache_write 按 input 价计费（目录无独立价格维）。
func cellCost(key cellKey, c cellTotals, prices map[string]dashboard.CatalogPrice) float64 {
	p, ok := prices[key.model]
	if !ok {
		return 0
	}
	return (float64(c.inTok+c.cacheWrite)*p.Input + float64(c.cacheRead)*p.Cached + float64(c.outTok)*p.Output) / 1e6
}

// dashboardModels 实现 /dashboard/models 与 /admin/models：
// 日志里出现过的模型 + 合成渠道 + 出现过的状态码。
func (h *Handler) dashboardModels(w http.ResponseWriter, _ *http.Request) {
	respondOK(w, map[string]any{
		"models":       h.ru.modelSet(h.debug),
		"channels":     []map[string]any{{"id": synthChannelID, "name": synthChannelName}},
		"status_codes": h.ru.statusCodeSet(h.debug),
	})
}

// channelFilterOptions 实现 /admin/channels/filter-options 与
// /dashboard/channels/filter-options：单渠道名 + 注册表模型名表。
func (h *Handler) channelFilterOptions(w http.ResponseWriter, r *http.Request) {
	models := map[string]struct{}{}
	for _, m := range h.channelModelNames(r) {
		models[m] = struct{}{}
	}
	for _, m := range h.ru.modelSet(h.debug) {
		models[m] = struct{}{}
	}
	list := make([]string, 0, len(models))
	for m := range models {
		list = append(list, m)
	}
	sort.Strings(list)
	respondOK(w, map[string]any{
		"channel_names": []string{synthChannelName},
		"models":        list,
	})
}

// channelModelNames 返回合成渠道对外的模型名表：别名 ∪ 上游目录 uid。
func (h *Handler) channelModelNames(r *http.Request) []string {
	set := map[string]struct{}{}
	if h.aliasesFunc != nil {
		for name := range h.aliasesFunc() {
			set[name] = struct{}{}
		}
	}
	for _, uid := range h.panel.ModelUIDs(r.Context()) {
		set[uid] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for m := range set {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}
