// 本文件是请求日志相关的面板端点：列表筛选/导出、单请求详情与文件读取、
// 进行中快照与中断、SSE 合并视图、进程日志尾部。
package dashboard

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// requestsFetchCap 是请求列表单次扫描的索引行数上限；
// 过滤与分页在这批记录内进行，更早历史用 grep 查 index.jsonl 原文件。
const requestsFetchCap = 2000

// apiRequests 返回 index.jsonl 中的最近请求（新的在前），供面板列表和
// agent 检索。?limit=&offset= 分页；过滤走结构化参数
// ?q= 子串、?status=499|!200|>=400|4xx（逗号 OR）、?status_class=2xx|4xx|5xx、
// ?result=、?model=、?error_stage=、?since=/?until=RFC3339 时间窗。
func (h *Handler) apiRequests(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		_, _ = w.Write([]byte(`{"requests":[],"disabled":true}`))
		return
	}
	result := h.debugManager.ListRequests(requestsFetchCap, parseRequestFilter(r.URL.Query()))
	entries := result.Entries
	total := len(entries)
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset > 0 {
		if offset >= total {
			entries = nil
		} else {
			entries = entries[offset:]
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	payload := map[string]any{
		"requests": entries,
		"total":    total,
		"offset":   offset,
		"limit":    limit,
		"has_more": result.HasMore,
	}
	// 管线前拒绝不进 index——用户在请求页找 503/429 时天然扑空，
	// 把拒绝事件环捎在列表响应里，前端据此提示「去系统页看」。
	if h.metrics != nil {
		payload["rejects"] = h.metrics.Rejects()
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// matrixEntry 是健康矩阵用的条目投影：只带分桶（started_at）与归因/悬停
// （归因口径、状态码计数、均耗时/均 TTFB）所需字段。完整 IndexEntry 约
// 30 个字段，投影把单条载荷压小一个量级。
type matrixEntry struct {
	StartedAt       string `json:"started_at"`
	Model           string `json:"model,omitempty"`
	RequestedModel  string `json:"requested_model,omitempty"`
	StatusCode      int    `json:"status_code"`
	Result          string `json:"result"`
	ErrorStage      string `json:"error_stage,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	FirstUpstreamMS *int64 `json:"first_upstream_ms,omitempty"`
	RateLimited     bool   `json:"rate_limited,omitempty"`
}

// apiRequestMatrix 给概览健康矩阵提供紧凑条目：窗口内条目不分页，
// 扫描上限直接用满 requestsFetchCap——走 /requests?limit=500 的列表
// 口径在高流量下盖不满 30 分钟分桶窗口。
func (h *Handler) apiRequestMatrix(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		_, _ = w.Write([]byte(`{"entries":[],"disabled":true}`))
		return
	}
	filter := parseRequestFilter(r.URL.Query())
	result := h.debugManager.ListRequests(requestsFetchCap, filter)
	entries := make([]matrixEntry, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, matrixEntry{
			StartedAt:       e.StartedAt,
			Model:           e.Model,
			RequestedModel:  e.RequestedModel,
			StatusCode:      e.StatusCode,
			Result:          e.Result,
			ErrorStage:      e.ErrorStage,
			DurationMS:      e.DurationMS,
			FirstUpstreamMS: e.FirstUpstreamMS,
			RateLimited:     e.RateLimited,
		})
	}
	// 截断判定不同于列表的 has_more（后者只说文件比尾部窗大）：窗口内
	// 条目打满扫描上限，或尾部窗最早一行仍晚于 since（尾部边界落在
	// 请求窗口内部，窗内可能有条目根本没被读到），才算覆盖不完整。
	truncated := len(result.Entries) >= requestsFetchCap
	if !truncated && result.HasMore && !filter.Since.IsZero() {
		if tailStart, err := time.Parse(time.RFC3339Nano, result.IndexTailStart); err == nil {
			truncated = tailStart.After(filter.Since)
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"entries":   entries,
		"total":     len(entries),
		"truncated": truncated,
	})
}

// parseRequestFilter 从查询串构建结构化筛选；q 为子串，其余为精确条件。
func parseRequestFilter(params map[string][]string) debuglog.RequestFilter {
	get := func(key string) string {
		if values := params[key]; len(values) > 0 {
			return values[0]
		}
		return ""
	}
	filter := debuglog.RequestFilter{
		Query:       get("q"),
		StatusClass: get("status_class"),
		Status:      get("status"),
		Result:      get("result"),
		Model:       get("model"),
		ErrorStage:  get("error_stage"),
	}
	if since := get("since"); since != "" {
		if parsed, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = parsed
		}
	}
	if until := get("until"); until != "" {
		if parsed, err := time.Parse(time.RFC3339, until); err == nil {
			filter.Until = parsed
		}
	}
	return filter
}

// apiExportRequests 把筛选后的请求摘要导出为 JSON 数组或 CSV。
func (h *Handler) apiExportRequests(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	if h.debugManager == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"debug log disabled"}`))
		return
	}
	entries := h.debugManager.ListRequests(requestsFetchCap, parseRequestFilter(r.URL.Query())).Entries
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="requests.csv"`)
		writeRequestsCSV(w, entries)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

// writeRequestsCSV 把请求摘要写成 CSV；指针字段用空串表示缺失。
func writeRequestsCSV(w http.ResponseWriter, entries []debuglog.IndexEntry) {
	out := bufio.NewWriter(w)
	defer func() { _ = out.Flush() }()
	_, _ = out.WriteString("dir,started_at,method,path,api,model,requested_model,response_model,status,result,duration_ms,first_upstream_ms,first_client_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,total_tokens,stream,key_hash,client_request_id,error_stage,retries\n")
	for _, e := range entries {
		firstUpstream, firstClient := "", ""
		if e.FirstUpstreamMS != nil {
			firstUpstream = strconv.FormatInt(*e.FirstUpstreamMS, 10)
		}
		if e.FirstClientMS != nil {
			firstClient = strconv.FormatInt(*e.FirstClientMS, 10)
		}
		_, _ = fmt.Fprintf(out, "%s,%s,%s,%s,%s,%s,%s,%s,%d,%s,%d,%s,%s,%d,%d,%d,%d,%d,%d,%v,%s,%s,%s,%d\n",
			csvEscape(e.Dir), csvEscape(e.StartedAt), csvEscape(e.Method), csvEscape(e.Path),
			csvEscape(e.API), csvEscape(e.Model), csvEscape(e.RequestedModel), csvEscape(e.ResponseModel),
			e.StatusCode, csvEscape(e.Result), e.DurationMS, firstUpstream, firstClient,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
			e.Stream, csvEscape(e.KeyHash), csvEscape(e.ClientRequestID), csvEscape(e.ErrorStage), e.Retries)
	}
}

// csvEscape 转义含逗号/引号/换行的字段；以 = + - @ 开头的值前加单引号，
// 防止客户端可控字段（client_request_id）在 Excel 里被当公式执行。
func csvEscape(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		s = "'" + s
	}
	if !strings.ContainsAny(s, ",\"\n") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// apiMergedResponse 把请求目录内 06-http-response.jsonl 的 SSE 帧合并成
// 可读的最终响应文本（同类调试面板的响应合并同款），原始帧仍可读。
func (h *Handler) apiMergedResponse(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"debug log disabled"}`))
		return
	}
	data, _, _, err := h.debugManager.ReadFile(chi.URLParam(r, "dir"), "06-http-response.jsonl")
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"response stream file not found"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(mergeStreamEvents(h.maskToken(data)))
}

// apiActiveRequests 返回仍在进行中的请求快照：已耗时、丢弃数、
// 已落盘文件清单——请求未结束就能检查它收到过什么（同类实现同款）。
func (h *Handler) apiActiveRequests(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	active := []debuglog.ActiveRequest{}
	if h.debugManager != nil {
		active = h.debugManager.ActiveRequests()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"active": active})
}

// apiRequestDetail 返回单个请求目录的 meta.json 与文件清单。
func (h *Handler) apiRequestDetail(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"debug log disabled"}`))
		return
	}
	detail, err := h.debugManager.Detail(chi.URLParam(r, "dir"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"request log not found or already cleaned"}`))
		return
	}
	// meta.json 含 client.user_agent 等自由文本，写路径键名脱敏
	// 覆盖不到值内 token，读路径按字面值再兜底一遍。
	detail.Meta = json.RawMessage(h.maskToken(detail.Meta))
	_ = json.NewEncoder(w).Encode(detail)
}

// apiRequestFile 返回请求目录内单个文件的内容；JSON/JSONL 原文回传，
// 由前端按需美化。大小超上限时截断并标记 truncated。
func (h *Handler) apiRequestFile(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"debug log disabled"}`))
		return
	}
	data, total, truncated, err := h.debugManager.ReadFile(chi.URLParam(r, "dir"), chi.URLParam(r, "*"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"file not found"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":      chi.URLParam(r, "*"),
		"size":      total,
		"truncated": truncated,
		"text":      string(h.maskToken(data)),
	})
}

// apiAbortRequest 中断一个仍在进行中的请求（取消其 ctx，客户端看到连接断开）。
// 给 agent 提供中止卡死请求的手段；已完结或不存在的目录返回 404。
func (h *Handler) apiAbortRequest(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil || !h.debugManager.Abort(chi.URLParam(r, "dir")) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no active request for dir"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"aborted": true})
}

// apiProcessLog 返回进程 stderr 日志尾部（slog 行），支持 ?offset= 增量拉取。
func (h *Handler) apiProcessLog(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"debug log disabled"}`))
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	data, next, err := h.debugManager.ReadProcessLog(offset)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"process log unavailable"}`))
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"text":        string(h.maskToken(data)),
		"next_offset": next,
	})
}

// apiDebugToggle 运行时切换请求日志开关；body {"enabled":bool}。
func (h *Handler) apiDebugToggle(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"debug log disabled at startup"}`))
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"body must be {\"enabled\":bool}"}`))
		return
	}
	h.debugManager.SetEnabled(*body.Enabled)
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": h.debugManager.Enabled()})
}
