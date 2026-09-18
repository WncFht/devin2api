package ccpanel

import (
	"hash/fnv"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/store"
)

// activeRequest 是 ccLoad ActiveRequest 的 wire 形状收缩版；本服务无多
// 上游，api 取 logs 表入口端点原值，upstream_protocol 留空
// （上游恒为 devin）。
type activeRequest struct {
	ID               int64  `json:"id"`
	Model            string `json:"model"`
	ClientIP         string `json:"client_ip"`
	StartTime        int64  `json:"start_time"`
	Streaming        bool   `json:"is_streaming"`
	API              string `json:"api,omitempty"`
	UpstreamProtocol string `json:"upstream_protocol,omitempty"`
	APIKeyUsed       string `json:"api_key_used,omitempty"`
	// Account 是已选定服务本请求的上游账号（号池 lane 名），
	// AccountSwitches 是至今的 failover 换号次数；与 logs 表同名同源。
	Account             string  `json:"account,omitempty"`
	AccountSwitches     int     `json:"account_switches,omitempty"`
	TokenID             int64   `json:"token_id,omitempty"`
	BaseURL             string  `json:"base_url,omitempty"`
	BytesReceived       int64   `json:"bytes_received,omitempty"`
	ClientFirstByteTime float64 `json:"client_first_byte_time,omitempty"`
	CostMultiplier      float64 `json:"cost_multiplier"`
	UpstreamWebsocket   bool    `json:"upstream_websocket,omitempty"`
	DebugLogAvailable   bool    `json:"debug_log_available,omitempty"`
	UpstreamStatus      string  `json:"upstream_status"`
	Abortable           bool    `json:"abortable,omitempty"`
	// Class 是令牌声明的请求类（fg/bg）——闸门分级准入语义随请求携带。
	Class string `json:"class,omitempty"`
}

// activeRequestID 把目录名映射成正 int64——移植前端的请求 id 是数字，
// 我方目录名是唯一字符串，FNV-1a 取正即稳定一一对应。
func activeRequestID(dir string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(dir))
	return int64(h.Sum64() & (1<<63 - 1))
}

// adminActiveRequests 实现 GET /admin/active-requests：进行中请求快照，
// 信封外加 active_request_title_enabled（本服务无标题生成能力，恒 false）。
func (h *Handler) adminActiveRequests(w http.ResponseWriter, _ *http.Request) {
	out := []activeRequest{}
	for _, ar := range h.debug.ActiveRequests() {
		model := ar.ResolvedModel
		if model == "" {
			model = ar.Model
		}
		status := "requesting"
		switch {
		case ar.State == debuglog.StateWaitingUpstream && ar.Retries > 0:
			status = "retrying"
		case ar.State == debuglog.StateReceivingUpstream || ar.State == debuglog.StateStreamingClient:
			status = "receiving"
		}
		row := activeRequest{
			ID:                activeRequestID(ar.Dir),
			Model:             model,
			ClientIP:          ar.Meta.ClientIP,
			StartTime:         ar.StartedAt.UnixMilli(),
			Streaming:         ar.Meta.Stream,
			API:               ar.Meta.API,
			APIKeyUsed:        ar.Meta.KeyHash,
			Account:           ar.Account,
			AccountSwitches:   ar.AccountSwitches,
			BaseURL:           h.BaseURL(),
			BytesReceived:     ar.ClientBytes,
			CostMultiplier:    1,
			UpstreamWebsocket: ar.Meta.API == "responses-ws",
			DebugLogAvailable: true,
			UpstreamStatus:    status,
			Abortable:         ar.Abortable,
			Class:             ar.Meta.Class,
		}
		if ar.FirstUpstreamMS != nil {
			row.ClientFirstByteTime = float64(*ar.FirstUpstreamMS) / 1000
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime < out[j].StartTime })
	// ccLoad 在信封外多带一个 active_request_title_enabled 顶层字段；
	// 本服务无标题生成能力，恒 false。
	titleEnabled := false
	writeEnvelope(w, http.StatusOK, apiResponse{
		Success: true, Data: out, Count: intPtr(len(out)),
		ActiveRequestTitleEnabled: &titleEnabled,
	})
}

// adminAbortActiveRequest 实现 POST /admin/active-requests/{id}/abort：
// id 是目录名哈希，反查活跃目录后走 debug.Abort 取消上游 ctx。
func (h *Handler) adminAbortActiveRequest(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid request_id")
		return
	}
	for _, ar := range h.debug.ActiveRequests() {
		if activeRequestID(ar.Dir) == id {
			if h.debug.Abort(ar.Dir) {
				respondOK(w, map[string]any{"aborted": true})
				return
			}
			respondError(w, http.StatusNotFound, "active request not found or not abortable")
			return
		}
	}
	respondError(w, http.StatusNotFound, "active request not found or not abortable")
}

// adminListAuthTokens 实现 GET /admin/auth-tokens：令牌表 + range 时叠加
// duration/rpm/is_today 全局统计，并用时间窗聚合同名覆盖各令牌的累计
// 字段（ccLoad HandleListAuthTokens + GetAuthTokenStatsInRange 语义：
// 覆盖值来自 logs 范围聚合而非令牌持久计数，范围外无数据的令牌清零）。
func (h *Handler) adminListAuthTokens(w http.ResponseWriter, r *http.Request) {
	var list []*authtoken.Token
	if h.tokens != nil {
		list = h.tokens.List()
	}
	tokens := make([]authtoken.View, 0, len(list))
	for _, t := range list {
		tokens = append(tokens, t.API())
	}
	data := map[string]any{
		"tokens":   tokens,
		"is_today": false,
	}
	rangeParam := strings.TrimSpace(r.URL.Query().Get("range"))
	if rangeParam == "" || rangeParam == "all" {
		respondOK(w, data)
		return
	}
	since, until, name := resolveRange(r, time.Now())
	isToday := name == "today"
	duration := until.Sub(since).Seconds()
	if duration < 1 {
		duration = 1
	}
	data["duration_seconds"] = duration
	data["is_today"] = isToday
	data["rpm_stats"] = h.rpmStatsFiltered(r.Context(), since, until, statScope{}, isToday, "")

	// 时间窗覆盖：按 key_hash 聚合格子，逐令牌覆盖累计字段。
	// 口径对齐 GetAuthTokenStatsInRange：success/failure 计数非 499，
	// token/成本求和含 499 行，TTFB/RT 均值含全部状态（stream 取 fbt
	// 样本、non-stream 取 duration 样本），stream/non_stream 计数非 499。
	// 开放模式（空仓）的请求无凭据可关联，自然不落入任何令牌。
	prices := h.CatalogPrices(r.Context())
	type tokenAgg struct {
		t    store.LogCellTotals
		cost float64
		peak int64 // 单槽非 499 峰值（peak_rpm 的分子，折算分钟速率）
	}
	byKH := map[string]*tokenAgg{}
	h.eachCell(r.Context(), since, until, statScope{}, func(key store.LogCellKey, c store.LogCellTotals) {
		if key.KeyHash == "" {
			return
		}
		a := byKH[key.KeyHash]
		if a == nil {
			a = &tokenAgg{}
			byKH[key.KeyHash] = a
		}
		a.t = addCells(a.t, c)
		a.cost += cellCost(key, c, prices)
		if n := c.Requests - c.Gone; n > a.peak {
			a.peak = n
		}
	})
	for i, t := range list {
		ov := &tokens[i]
		a := byKH[t.KeyHash()]
		if a == nil {
			a = &tokenAgg{} // 范围内无数据：清零覆盖（ccLoad 同款语义）
		}
		ov.SuccessCount = a.t.OK
		ov.FailureCount = a.t.Requests - a.t.OK - a.t.Gone
		ov.PromptTokensTotal = a.t.InTok
		ov.CompletionTokensTotal = a.t.OutTok
		ov.CacheReadTokensTotal = a.t.CacheRead
		ov.CacheCreationTokensTotal = a.t.CacheWrite
		ov.TotalCostUSD = a.cost
		ov.EffectiveCostUSD = a.cost
		ov.StreamAvgTTFB = 0
		if a.t.NFirstStream > 0 {
			ov.StreamAvgTTFB = float64(a.t.SumFirstStreamMS) / float64(a.t.NFirstStream) / 1000
		}
		ov.NonStreamAvgRT = 0
		if a.t.NNonStream > 0 {
			ov.NonStreamAvgRT = float64(a.t.SumDurNonStreamMS) / float64(a.t.NNonStream) / 1000
		}
		ov.StreamCount = a.t.NStreamNG
		ov.NonStreamCount = a.t.NNonStreamNG
		ov.PeakRPM = float64(a.peak) / (rollupSlotSeconds / 60)
		ov.AvgRPM = float64(ov.SuccessCount+ov.FailureCount) * 60 / duration
		ov.RecentRPM = 0
		if isToday {
			ov.RecentRPM = h.recentRPM(r.Context(), "", t.KeyHash())
			if ov.PeakRPM < ov.RecentRPM {
				ov.PeakRPM = ov.RecentRPM
			}
		}
	}
	respondOK(w, data)
}

// adminModelPricing 实现 GET /admin/model-pricing?model=：
// 目录价投影成 ccLoad 的 pricing 形状；无目录价的模型 found=false。
func (h *Handler) adminModelPricing(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		respondError(w, http.StatusBadRequest, "missing model id")
		return
	}
	prices := h.CatalogPrices(r.Context())
	p, found := prices[model]
	pricing := map[string]any{
		"input_price":  p.Input,
		"output_price": p.Output,
	}
	if p.Cached > 0 {
		pricing["cache_read_price"] = p.Cached
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	respondOK(w, map[string]any{
		"model":   model,
		"found":   found,
		"pricing": pricing,
	})
}

// adminRuntimeMetrics 实现 GET /admin/runtime-metrics：把 obs 快照与
// debuglog 自观测投影成 ccLoad 的 process/http_proxy/logs 分组形状；
// 另投 gate/rejects/rates/trend/debuglog/usage/warm 组承接旧面板
// stats 端点的排障口径。responses_websocket 组本服务无会话仓，给零值。
func (h *Handler) adminRuntimeMetrics(w http.ResponseWriter, r *http.Request) {
	snap := map[string]any{}
	if h.metrics != nil {
		snap = h.metrics.Snapshot()
	}
	proc, _ := snap["process"].(map[string]any)
	getU64 := func(m map[string]any, k string) uint64 {
		switch v := m[k].(type) {
		case uint64:
			return v
		case uint32:
			return uint64(v)
		case int64:
			return uint64(v)
		case int:
			return uint64(v)
		}
		return 0
	}
	getI64 := func(m map[string]any, k string) int64 {
		v, _ := m[k].(int64)
		return v
	}
	getF64 := func(m map[string]any, k string) float64 {
		v, _ := m[k].(float64)
		return v
	}
	getInt := func(m map[string]any, k string) int {
		v, _ := m[k].(int)
		return v
	}
	// cpu_seconds 是 user+system 合计（rusage 采样不拆），全部归 user
	// 一栏——总量正确，拆分字段留 0。
	process := map[string]any{
		"uptime_seconds":           int64(time.Since(h.startedAt).Seconds()),
		"concurrency_slots_in_use": getI64(snap, "active_requests"),
		"max_concurrency":          h.maxConcurrency(),
		"goroutines":               getInt(proc, "goroutines"),
		"cpu_usage_percent":        getF64(proc, "cpu_percent"),
		"cpu_user_seconds":         getF64(proc, "cpu_seconds"),
		"cpu_system_seconds":       0.0,
		// rss_bytes/rss_current_bytes 是瞬时 RSS（linux 取 /proc/self/statm
		// 常驻页口径），随真实占用起伏；max_rss_bytes 保留 ru_maxrss
		// 只涨不降的峰值水印。无瞬时数据源的平台两字段为 0。
		"rss_bytes":           getU64(proc, "rss_current_bytes"),
		"rss_current_bytes":   getU64(proc, "rss_current_bytes"),
		"max_rss_bytes":       getU64(proc, "max_rss_bytes"),
		"heap_alloc_bytes":    getU64(proc, "heap_alloc_bytes"),
		"heap_sys_bytes":      getU64(proc, "heap_sys_bytes"),
		"gc_count":            getU64(proc, "num_gc"),
		"gc_pause_total_ns":   uint64(getF64(proc, "gc_pause_total_ms") * 1e6),
		"gc_cpu_percent":      getF64(proc, "gc_cpu_fraction") * 100,
		"sse_framing_repairs": 0,
	}
	httpProxy := map[string]any{
		"active_requests":        getI64(snap, "active_requests"),
		"completed_requests":     getU64(snap, "completed_requests"),
		"non_error_responses":    getU64(snap, "ok_responses"),
		"client_error_responses": getU64(snap, "client_error_responses"),
		"server_error_responses": getU64(snap, "server_error_responses"),
		"streaming_requests":     getU64(snap, "streaming_requests"),
		"non_streaming_requests": getU64(snap, "non_streaming_requests"),
		"request_body_bytes":     getU64(snap, "request_body_bytes"),
		"response_body_bytes":    getU64(snap, "response_body_bytes"),
	}
	data := map[string]any{
		"process":             process,
		"http_proxy":          httpProxy,
		"responses_websocket": map[string]any{},
	}
	if stats := h.debug.Stats(); stats != nil {
		data["logs"] = map[string]any{
			"backlog_entries":            stats["queued_log_events"],
			"queue_capacity_entries":     stats["queue_capacity"],
			"dropped_entries":            stats["dropped_log_events"],
			"persistence_failed_entries": stats["io_errors"],
			"pending_bytes":              stats["pending_bytes"],
			"pending_bytes_cap":          stats["pending_bytes_cap"],
		}
		// debuglog 组是全量自观测（含 last_bind_failure 监听争夺取证、
		// 保留策略回显）；logs 组只是 ccLoad 契约的四键投影。
		data["debuglog"] = stats
	}
	// rates/trend 是 Snapshot 原生键（RPM/QPS、60 分钟 10s 桶）；
	// rejects 是管线前拒绝的分原因计数与最近事件环——它们不产生调试
	// 目录，这里是唯一透出点。
	if h.metrics != nil {
		data["rates"] = snap["rates"]
		data["trend"] = snap["trend_minutes"]
		data["rejects"] = h.metrics.Rejects()
	}
	// usage 组只投全局延迟分位数两行（ttfb/duration）；全量聚合视图
	// 在 /admin/usage——轮询端点不背全桶排序的成本。
	if h.store != nil {
		if lat, err := h.store.LogLatency(r.Context()); err == nil {
			data["usage"] = lat
		} else {
			slog.Warn("ccpanel: log latency query failed", "error", err)
		}
	}
	// gate 组是速率闸门快照（闩态/配额/排队 + events 闩迁移事件环）。
	if h.gateStats != nil {
		data["gate"] = h.gateStats()
	}
	// warm 组投前缀保温簿记；hit_rate 由 hits/(hits+misses) 派生，
	// cr=0 的 ping 不计入 misses（簿记侧口径），故命中率只反映真实命中。
	if h.warmStats != nil {
		data["warm"] = warmStatsView(h.warmStats())
	}
	// detached 组投脱钩完成缓存簿记：泵终局（finished_*）、移除原因
	// 与孤儿浪费（orphans/orphan_completed）在盘上 04 标记行之外
	// 没有其它观测面；首 lane 快照是后兼容形态。
	if h.detachedStats != nil {
		data["detached"] = h.detachedStats()
	}
	// accounts 组是号池逐账号视图：每号的闸门/保温/脱钩缓存/池侧
	// 状态各自透出——顶层 gate/warm/detached 仍是首 lane 快照
	//（前端后兼容），逐号排障看这里。
	if h.accountGateStats != nil || h.accountWarmStats != nil || h.accountLaneStates != nil || h.accountDetachedStats != nil {
		gates := map[string]devin.GateStats{}
		if h.accountGateStats != nil {
			gates = h.accountGateStats()
		}
		warms := map[string]devin.WarmStats{}
		if h.accountWarmStats != nil {
			warms = h.accountWarmStats()
		}
		laneStates := map[string]devin.LaneState{}
		if h.accountLaneStates != nil {
			laneStates = h.accountLaneStates()
		}
		detacheds := map[string]devin.DetachedStats{}
		if h.accountDetachedStats != nil {
			detacheds = h.accountDetachedStats()
		}
		accounts := make(map[string]any, len(gates)+len(warms)+len(laneStates)+len(detacheds))
		entry := func(name string) map[string]any {
			if e, ok := accounts[name].(map[string]any); ok {
				return e
			}
			e := map[string]any{}
			accounts[name] = e
			return e
		}
		for name, gate := range gates {
			entry(name)["gate"] = gate
		}
		for name, warm := range warms {
			// warm 只在确有该号簿记时投：两次快照之间 ApplyConfigs
			// 换过 lane 集合的话，缺席渲染成 enabled:false 会误读。
			entry(name)["warm"] = warmStatsView(warm)
		}
		for name, state := range laneStates {
			entry(name)["lane"] = state
		}
		for name, detached := range detacheds {
			entry(name)["detached"] = detached
		}
		data["accounts"] = accounts
	}
	respondOK(w, data)
}

// warmStatsView 把一条 lane 的保温簿记投影成 runtime-metrics 的 warm
// 组形状；hit_rate 由 hits/(hits+misses) 派生，cr=0 的 ping 不计入
// misses（簿记侧口径），故命中率只反映真实命中。顶层 warm 组与
// accounts 组内每号的 warm 共用同一投影。
func warmStatsView(warm devin.WarmStats) map[string]any {
	pingTotal := warm.PingHits + warm.PingMisses
	hitRate := 0.0
	if pingTotal > 0 {
		hitRate = float64(warm.PingHits) / float64(pingTotal) * 100
	}
	return map[string]any{
		"enabled":        warm.Enabled,
		"entries":        warm.Entries,
		"promoted":       warm.Promoted,
		"demoted":        warm.Demoted,
		"suspects":       warm.Suspects,
		"retained_bytes": warm.RetainedBytes,
		"pings_sent":     warm.PingsSent,
		"ping_hits":      warm.PingHits,
		"ping_misses":    warm.PingMisses,
		"ping_hit_rate":  hitRate,
		"ping_skips":     warm.PingSkips,
		"ping_errors":    warm.PingErrors,
		"retired":        warm.Retired,
		// 死因分账与 ping 燃烧账：churn 构成与保温座位成本的观测面
		//（miss=全前缀重灌走估计、hit=上游实报 cache_read，口径见
		// WarmStats 注释）。
		"retired_by_cause":           warm.RetiredByCause,
		"ping_miss_prefill_tokens":   warm.PingMissPrefillTokens,
		"ping_hit_cache_read_tokens": warm.PingHitCacheReadTokens,
	}
}
