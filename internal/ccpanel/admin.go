package ccpanel

import (
	"encoding/json"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/authtoken"
)

// activeRequest 是 ccLoad ActiveRequest 的 wire 形状收缩版；本服务无多
// 上游，api 取 index.jsonl 入口端点原值，upstream_protocol 留空
// （上游恒为 devin）。
type activeRequest struct {
	ID                  int64   `json:"id"`
	Model               string  `json:"model"`
	ClientIP            string  `json:"client_ip"`
	StartTime           int64   `json:"start_time"`
	Streaming           bool    `json:"is_streaming"`
	API                 string  `json:"api,omitempty"`
	UpstreamProtocol    string  `json:"upstream_protocol,omitempty"`
	APIKeyUsed          string  `json:"api_key_used,omitempty"`
	TokenID             int64   `json:"token_id,omitempty"`
	BaseURL             string  `json:"base_url,omitempty"`
	BytesReceived       int64   `json:"bytes_received,omitempty"`
	ClientFirstByteTime float64 `json:"client_first_byte_time,omitempty"`
	CostMultiplier      float64 `json:"cost_multiplier"`
	UpstreamWebsocket   bool    `json:"upstream_websocket,omitempty"`
	DebugLogAvailable   bool    `json:"debug_log_available,omitempty"`
	UpstreamStatus      string  `json:"upstream_status"`
	Abortable           bool    `json:"abortable,omitempty"`
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
		case ar.State == "waiting_upstream" && ar.Retries > 0:
			status = "retrying"
		case ar.State == "receiving_upstream" || ar.State == "streaming_client":
			status = "receiving"
		}
		row := activeRequest{
			ID:                activeRequestID(ar.Dir),
			Model:             model,
			ClientIP:          ar.Meta.ClientIP,
			StartTime:         ar.StartedAt.UnixMilli(),
			Streaming:         ar.Meta.API == "responses-ws",
			API:               ar.Meta.API,
			APIKeyUsed:        ar.Meta.KeyHash,
			BaseURL:           h.BaseURL(),
			BytesReceived:     ar.ClientBytes,
			CostMultiplier:    1,
			UpstreamWebsocket: ar.Meta.API == "responses-ws",
			DebugLogAvailable: true,
			UpstreamStatus:    status,
			Abortable:         ar.Abortable,
		}
		if ar.FirstUpstreamMS != nil {
			row.ClientFirstByteTime = float64(*ar.FirstUpstreamMS) / 1000
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartTime < out[j].StartTime })
	// ccLoad 在信封外多带一个 active_request_title_enabled 顶层字段；
	// 本服务无标题生成能力，恒 false。
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"success":                      true,
		"data":                         out,
		"error":                        "",
		"count":                        len(out),
		"active_request_title_enabled": false,
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
	// 主密钥（config auth.api_key）投影成只读元信息：令牌页把「当前
	// 哪些凭据能过 /v1」一次说全；key_hash 与 index.jsonl 同口径，
	// 供按哈希对照日志行。无明文、不可经此 API 改写。
	if h.masterKeyFunc != nil {
		mk := map[string]any{"configured": false}
		if key := strings.TrimSpace(h.masterKeyFunc()); key != "" {
			hash := authtoken.HashToken(key)
			mk["configured"] = true
			mk["key_hash"] = hash[:16]
		}
		data["master_key"] = mk
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
	data["rpm_stats"] = h.rpmStatsFiltered(since, until, nil, isToday, "", "")

	// 时间窗覆盖：按 key_hash 聚合格子，逐令牌覆盖累计字段。
	// 口径对齐 GetAuthTokenStatsInRange：success/failure 计数非 499，
	// token/成本求和含 499 行，TTFB/RT 均值含全部状态（stream 取 fbt
	// 样本、non-stream 取 duration 样本），stream/non_stream 计数非 499。
	// master key/开放模式的行无对应令牌，自然不落入任何令牌。
	prices := h.CatalogPrices(r.Context())
	type tokenAgg struct {
		t    cellTotals
		cost float64
		peak int64 // 单槽非 499 峰值（peak_rpm 的分子，折算分钟速率）
	}
	byKH := map[string]*tokenAgg{}
	h.ru.eachCell(h.debug, since, until, func(key cellKey, c cellTotals) {
		if key.kh == "" {
			return
		}
		a := byKH[key.kh]
		if a == nil {
			a = &tokenAgg{}
			byKH[key.kh] = a
		}
		a.t = addCells(a.t, c)
		a.cost += cellCost(key, c, prices)
		if n := c.requests - c.gone; n > a.peak {
			a.peak = n
		}
	})
	for i, t := range list {
		ov := &tokens[i]
		a := byKH[t.KeyHash()]
		if a == nil {
			a = &tokenAgg{} // 范围内无数据：清零覆盖（ccLoad 同款语义）
		}
		ov.SuccessCount = a.t.ok
		ov.FailureCount = a.t.requests - a.t.ok - a.t.gone
		ov.PromptTokensTotal = a.t.inTok
		ov.CompletionTokensTotal = a.t.outTok
		ov.CacheReadTokensTotal = a.t.cacheRead
		ov.CacheCreationTokensTotal = a.t.cacheWrite
		ov.TotalCostUSD = a.cost
		ov.EffectiveCostUSD = a.cost
		ov.StreamAvgTTFB = 0
		if a.t.nFirstStream > 0 {
			ov.StreamAvgTTFB = float64(a.t.sumFirstStreamMS) / float64(a.t.nFirstStream) / 1000
		}
		ov.NonStreamAvgRT = 0
		if a.t.nNonStream > 0 {
			ov.NonStreamAvgRT = float64(a.t.sumDurNonStreamMS) / float64(a.t.nNonStream) / 1000
		}
		ov.StreamCount = a.t.nStreamNG
		ov.NonStreamCount = a.t.nNonStreamNG
		ov.PeakRPM = float64(a.peak) / (rollupSlotSeconds / 60)
		ov.AvgRPM = float64(ov.SuccessCount+ov.FailureCount) * 60 / duration
		ov.RecentRPM = 0
		if isToday {
			ov.RecentRPM = h.ru.recentRPM(h.debug, "", t.KeyHash())
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
// debuglog 自观测投影成 ccLoad 的 process/http_proxy/logs 分组形状。
// responses_websocket 组本服务无会话仓，给零值。
func (h *Handler) adminRuntimeMetrics(w http.ResponseWriter, _ *http.Request) {
	snap := map[string]any{}
	if h.metrics != nil {
		snap = h.metrics.Snapshot()
	}
	proc, _ := snap["process"].(map[string]any)
	getU64 := func(m map[string]any, k string) uint64 {
		switch v := m[k].(type) {
		case uint64:
			return v
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
		"rss_bytes":                getU64(proc, "max_rss_bytes"),
		"max_rss_bytes":            getU64(proc, "max_rss_bytes"),
		"heap_alloc_bytes":         getU64(proc, "heap_alloc_bytes"),
		"heap_sys_bytes":           getU64(proc, "heap_sys_bytes"),
		"gc_count":                 getU64(proc, "num_gc"),
		"gc_pause_total_ns":        uint64(getF64(proc, "gc_pause_total_ms") * 1e6),
		"gc_cpu_percent":           getF64(proc, "gc_cpu_fraction") * 100,
		"sse_framing_repairs":      0,
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
		}
	}
	// warm 组投前缀保温簿记；hit_rate 由 hits/(hits+misses) 派生，
	// cr=0 的 ping 不计入 misses（簿记侧口径），故命中率只反映真实命中。
	if h.warmStats != nil {
		warm := h.warmStats()
		pingTotal := warm.PingHits + warm.PingMisses
		hitRate := 0.0
		if pingTotal > 0 {
			hitRate = float64(warm.PingHits) / float64(pingTotal) * 100
		}
		data["warm"] = map[string]any{
			"enabled":        warm.Enabled,
			"entries":        warm.Entries,
			"promoted":       warm.Promoted,
			"suspects":       warm.Suspects,
			"retained_bytes": warm.RetainedBytes,
			"pings_sent":     warm.PingsSent,
			"ping_hits":      warm.PingHits,
			"ping_misses":    warm.PingMisses,
			"ping_hit_rate":  hitRate,
			"ping_skips":     warm.PingSkips,
			"ping_errors":    warm.PingErrors,
			"retired":        warm.Retired,
		}
	}
	respondOK(w, data)
}
