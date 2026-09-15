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
)

// activeRequest 是 ccLoad ActiveRequest 的 wire 形状；本服务无多上游，
// 渠道字段恒为合成渠道，upstream_protocol 留空（上游恒为 devin）。
type activeRequest struct {
	ID                  int64   `json:"id"`
	Model               string  `json:"model"`
	ClientIP            string  `json:"client_ip"`
	StartTime           int64   `json:"start_time"`
	Streaming           bool    `json:"is_streaming"`
	ChannelID           int64   `json:"channel_id,omitempty"`
	ChannelName         string  `json:"channel_name,omitempty"`
	ClientProtocol      string  `json:"client_protocol,omitempty"`
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
			ChannelID:         synthChannelID,
			ChannelName:       synthChannelName,
			ClientProtocol:    clientProtocol(ar.Meta.API),
			APIKeyUsed:        ar.Meta.KeyHash,
			BaseURL:           h.baseURL,
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

// synthChannelConfig 返回合成渠道的 ccLoad Config 形状——本服务只有一条
// 上游，渠道页未移植，此投影只为满足前端对渠道下拉/详情的引用。
func (h *Handler) synthChannelConfig(r *http.Request) map[string]any {
	models := make([]map[string]any, 0)
	for _, name := range h.channelModelNames(r) {
		models = append(models, map[string]any{"model": name})
	}
	return map[string]any{
		"id":                      synthChannelID,
		"name":                    synthChannelName,
		"auth_type":               "api_key",
		"protocol_transform_mode": "auto",
		"urls":                    []map[string]any{{"url": h.baseURL}},
		"priority":                0,
		"rpm_limit":               0,
		"max_concurrency":         0,
		"enabled":                 true,
		"models":                  models,
		"cost_multiplier":         1.0,
		"daily_cost_limit":        0.0,
	}
}

// adminListChannels 实现 GET /admin/channels：恒返回单元素合成渠道表。
func (h *Handler) adminListChannels(w http.ResponseWriter, r *http.Request) {
	respondOKCount(w, []map[string]any{h.synthChannelConfig(r)}, 1)
}

// adminGetChannel 实现 GET /admin/channels/{id}：只认合成渠道 id=1。
func (h *Handler) adminGetChannel(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "id") != strconv.Itoa(synthChannelID) {
		respondError(w, http.StatusNotFound, "channel not found")
		return
	}
	respondOK(w, h.synthChannelConfig(r))
}

// adminChannelKeys 实现 GET /admin/channels/{id}/keys：合成一条上游 key 行。
// api_key 不落明文（上游凭据不出面板），给脱敏占位串。
func (h *Handler) adminChannelKeys(w http.ResponseWriter, r *http.Request) {
	if chi.URLParam(r, "id") != strconv.Itoa(synthChannelID) {
		respondError(w, http.StatusNotFound, "channel not found")
		return
	}
	respondOKCount(w, []map[string]any{{
		"id":                   1,
		"channel_id":           synthChannelID,
		"key_index":            0,
		"api_key":              "***",
		"note":                 "upstream credential",
		"key_strategy":         "sequential",
		"disabled":             false,
		"cost_multiplier":      1.0,
		"cooldown_until":       0,
		"cooldown_duration_ms": 0,
		"created_at":           h.startedAt.Format(time.RFC3339),
		"updated_at":           h.startedAt.Format(time.RFC3339),
	}}, 1)
}

// adminListSettings 实现 GET /admin/settings：S6 之前无运行时设置项，
// 返回空表（前端只要求数组）。
func (h *Handler) adminListSettings(w http.ResponseWriter, _ *http.Request) {
	respondOK(w, []any{})
}

// adminListAuthTokens 实现 GET /admin/auth-tokens：S4 之前恒空表，
// 带 range 时同样回 rpm/duration 统计字段，前端不区分。
func (h *Handler) adminListAuthTokens(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"tokens":   []any{},
		"is_today": false,
	}
	if rangeParam := strings.TrimSpace(r.URL.Query().Get("range")); rangeParam != "" && rangeParam != "all" {
		since, until, name := resolveRange(r, time.Now())
		duration := until.Sub(since).Seconds()
		if duration < 1 {
			duration = 1
		}
		data["duration_seconds"] = duration
		data["is_today"] = name == "" || name == "today"
		data["rpm_stats"] = h.rpmStats(since, until)
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
	prices := h.panel.CatalogPrices(r.Context())
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
		"max_concurrency":          h.maxConcurrency,
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
	respondOK(w, data)
}
