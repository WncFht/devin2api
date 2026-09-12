// Package dashboard 实现管理面板：模型列表、价格筛选、账户用量。
// password 为空时无需登录直接进入；非空时走 session cookie。
package dashboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/go-chi/chi/v5"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"google.golang.org/protobuf/proto"
)

const (
	clientName    = "windsurf"
	clientVersion = "1.48.2"
	// seatUserStatusPath 与 Windsurf 官方 / WindsurfAPI 一致的 JSON Connect 路径。
	// 生成的 connect 包名 ExaSeatManagementPb_SeatManagementService 在上游会 404。
	seatUserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"
)

// Handler 是面板 HTTP 处理器。
type Handler struct {
	password      string
	baseURL       string
	token         string
	apiClient     devinprotoconnect.ApiServerServiceClient
	httpClient    *http.Client
	baseTransport http.RoundTripper
	sessionMu     sync.RWMutex
	sessionTokens map[string]time.Time

	// 面板数据缓存：模型目录、供应商列表、模型状态均不经常变化，缓存可显著降低上游压力。
	cacheMu             sync.RWMutex
	cacheTTL            time.Duration
	modelsCache         []map[string]any
	modelsExpiry        time.Time
	providersCache      []map[string]any
	providersExpiry     time.Time
	modelStatusesCache  []map[string]any
	modelStatusesExpiry time.Time

	// metrics 是 HTTP 代理运行计数器快照源；nil 时 stats 端点只返回日志侧数据。
	metrics *obs.Metrics
	// debugManager 暴露日志管道自身指标（丢弃数、活跃目录数）。
	debugManager *debuglog.Manager
}

// New 创建面板处理器。password 为空表示开放访问。proxy 为可选代理地址。
// forceHTTP1 为 true 时强制 HTTP/1.1，与 adapter 保持一致的连接模型。
// metrics/debugManager 允许为 nil（对应功能未启用）。
func New(password, baseURL, token, proxy string, forceHTTP1 bool, metrics *obs.Metrics, debugManager *debuglog.Manager) *Handler {
	base, err := httpproxy.NewTransport(proxy, forceHTTP1)
	if err != nil {
		// 代理配置错误时回退到默认 transport，保证面板仍可尝试工作。
		base = http.DefaultTransport.(*http.Transport).Clone()
	}
	transport := &authTransport{base: base, token: token}
	// 面板可能遇到上游长时思考/排队，超时与 ResponseHeaderTimeout 对齐。
	httpClient := &http.Client{Transport: transport, Timeout: 610 * time.Second}
	return &Handler{
		password:      password,
		baseURL:       strings.TrimRight(baseURL, "/"),
		token:         token,
		apiClient:     devinprotoconnect.NewApiServerServiceClient(httpClient, baseURL),
		httpClient:    httpClient,
		baseTransport: base,
		sessionTokens: make(map[string]time.Time),
		cacheTTL:      5 * time.Minute,
		metrics:       metrics,
		debugManager:  debugManager,
	}
}

// Register 将面板路由注册到 mux。有 token 即可启用；密码仅控制是否登录。
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
}) {
	mux.Get("/panel", h.servePanel)
	mux.Post("/panel/login", h.handleLogin)
	mux.Get("/panel/api", h.apiIndex)
	mux.Get("/panel/api/status", h.apiStatus)
	mux.Get("/panel/api/models", h.apiModels)
	mux.Get("/panel/api/stats", h.apiStats)
	mux.Get("/panel/api/requests", h.apiRequests)
	mux.Get("/panel/api/requests/export", h.apiExportRequests)
	mux.Get("/panel/api/requests/active", h.apiActiveRequests)
	mux.Get("/panel/api/requests/{dir}", h.apiRequestDetail)
	mux.Get("/panel/api/requests/{dir}/merged", h.apiMergedResponse)
	mux.Get("/panel/api/requests/{dir}/file/*", h.apiRequestFile)
	mux.Post("/panel/api/requests/{dir}/abort", h.apiAbortRequest)
	mux.Get("/panel/api/logs", h.apiProcessLog)
	mux.Get("/panel/api/quota", h.apiQuota)
	mux.Get("/panel/api/usage", h.apiUsage)
	mux.Post("/panel/api/debug/toggle", h.apiDebugToggle)
}

// apiStats 返回代理自身运行指标：请求计数、错误分类、流式占比、字节量，
// 以及调试日志管道自观测数据（丢弃数、活跃目录数）。
func (h *Handler) apiStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	payload := map[string]any{}
	if h.metrics != nil {
		payload["http"] = h.metrics.Snapshot()
	}
	if h.debugManager != nil {
		payload["debuglog"] = h.debugManager.Stats()
		payload["usage"] = h.debugManager.UsageStats()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// apiIndex 是自描述端点：面向 agent 的面板 API 目录与调试工作流说明。
// 让初次接触的调用方无需读代码即可发现检索入口与日志布局。
func (h *Handler) apiIndex(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service": "devin-2api",
		"auth":    "dashboard.password 非空时可用 cookie 会话或 Authorization: Bearer <密码>",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/panel/api/status", "description": "账户/套餐/容量/渠道/模型状态告警"},
			{"method": "GET", "path": "/panel/api/models", "description": "模型目录含能力位与价格"},
			{"method": "GET", "path": "/panel/api/stats", "description": "进程运行指标（RPM/QPS/goroutine/内存/GC/CPU）+ 60 分钟逐分钟趋势 + 日志管道自观测 + index 聚合用量"},
			{"method": "GET", "path": "/panel/api/usage", "description": "index.jsonl 聚合：今日/窗口累计、按模型/按 key、错误阶段、7 天逐小时趋势、p50/p95/p99、目录价估算成本"},
			{"method": "GET", "path": "/panel/api/requests?limit=&offset=&q=&status_class=&result=&model=&error_stage=&since=", "description": "最近请求（新在前）；q 子串或结构化过滤，has_more 提示窗口外仍有历史"},
			{"method": "GET", "path": "/panel/api/requests/export?format=json|csv&筛选参数同上", "description": "导出筛选后的请求摘要（CSV 或 JSONL）"},
			{"method": "GET", "path": "/panel/api/requests/active", "description": "进行中请求活快照：阶段状态、模型、已下发字节、已写文件、丢弃数"},
			{"method": "GET", "path": "/panel/api/requests/{dir}", "description": "单请求 meta.json + 文件清单"},
			{"method": "GET", "path": "/panel/api/requests/{dir}/merged", "description": "把 06-http-response.jsonl 的 SSE 帧合并成可读的最终响应"},
			{"method": "GET", "path": "/panel/api/requests/{dir}/file/{name}", "description": "读取请求目录内文件（顶层或 attachments/），超 4MB 截断"},
			{"method": "POST", "path": "/panel/api/requests/{dir}/abort", "description": "中断进行中请求（取消 ctx）；无活跃请求时 404"},
			{"method": "GET", "path": "/panel/api/logs?offset=", "description": "进程 stderr 日志尾部；offset>0 增量拉取，响应带 next_offset"},
			{"method": "GET", "path": "/panel/api/quota", "description": "配额历史快照（logs/quota.jsonl）+ 按燃烧速率外推的耗尽时间"},
			{"method": "POST", "path": "/panel/api/debug/toggle", "description": "请求日志运行时开关；body {\"enabled\":true|false}"},
		},
		"debug_workflow": []string{
			"每个 /v1/* 响应带 X-Request-Id 头（=调试目录名）；错误体含 debug_ref 与 stage 字段",
			"凭 dir 调 /panel/api/requests/{dir} 拿 meta 与文件清单，再逐个 file/ 读取",
			"也可直接读磁盘 logs/index.jsonl（每完成请求一行摘要）与 logs/{dir}/（meta.json、01-06 阶段文件、error.json、attachments/）",
		},
	})
}

// requestsFetchCap 是请求列表单次扫描的索引行数上限；
// 过滤与分页在这批记录内进行，更早历史用 grep 查 index.jsonl 原文件。
const requestsFetchCap = 2000

// apiRequests 返回 index.jsonl 中的最近请求（新的在前），供面板列表和
// agent 检索。?limit=&offset= 分页；过滤走结构化参数
// ?q= 子串、?status_class=2xx|4xx|5xx、?result=、?model=、?error_stage=、?since=RFC3339。
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
	offset, _ := strconv.Atoi(params.Get("offset"))
	limit, _ := strconv.Atoi(params.Get("limit"))
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
	_ = json.NewEncoder(w).Encode(map[string]any{
		"requests": entries,
		"total":    total,
		"offset":   offset,
		"limit":    limit,
		"has_more": result.HasMore,
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
		Result:      get("result"),
		Model:       get("model"),
		ErrorStage:  get("error_stage"),
	}
	if since := get("since"); since != "" {
		if parsed, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = parsed
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
	defer out.Flush()
	_, _ = out.WriteString("dir,started_at,method,path,api,model,requested_model,response_model,status,result,duration_ms,first_upstream_ms,first_client_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,total_tokens,stream,key_hash,client_request_id,error_stage\n")
	for _, e := range entries {
		firstUpstream, firstClient := "", ""
		if e.FirstUpstreamMS != nil {
			firstUpstream = strconv.FormatInt(*e.FirstUpstreamMS, 10)
		}
		if e.FirstClientMS != nil {
			firstClient = strconv.FormatInt(*e.FirstClientMS, 10)
		}
		_, _ = fmt.Fprintf(out, "%s,%s,%s,%s,%s,%s,%s,%s,%d,%s,%d,%s,%s,%d,%d,%d,%d,%d,%d,%v,%s,%s,%s\n",
			e.Dir, e.StartedAt, e.Method, csvEscape(e.Path), e.API, e.Model, e.RequestedModel, e.ResponseModel,
			e.StatusCode, e.Result, e.DurationMS, firstUpstream, firstClient,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
			e.Stream, e.KeyHash, e.ClientRequestID, e.ErrorStage)
	}
}

// csvEscape 转义含逗号/引号/换行的字段。
func csvEscape(s string) string {
	if !strings.ContainsAny(s, ",\"\n") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// apiUsage 返回 index.jsonl 聚合快照，并按模型目录价附估算成本。
// 价格是 catalog 标价（$/1M tokens），est_cost 为参考值而非上游账单。
func (h *Handler) apiUsage(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		_, _ = w.Write([]byte(`{"disabled":true}`))
		return
	}
	snap := h.debugManager.UsageStats()
	prices := h.modelPriceMap(r.Context())
	var totalCost float64
	models := make([]map[string]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		row := map[string]any{
			"name": m.Name, "requests": m.Requests, "errors": m.Errors, "disconnected": m.Disconnected,
			"input_tokens": m.Input, "output_tokens": m.Output,
			"cache_read_tokens": m.CacheRead, "cache_write_tokens": m.CacheWrite,
			"reasoning_tokens": m.Reasoning, "total_tokens": m.TotalTokens,
			"avg_duration_ms": m.AvgDuration, "avg_ttfb_ms": m.AvgTTFB,
			"success_rate": m.SuccessRate, "last_result": m.LastResult, "last_at": m.LastAt,
		}
		if p, ok := prices[m.Name]; ok {
			cost := (float64(m.Input)*p.input + float64(m.CacheRead)*p.cached + float64(m.Output)*p.output) / 1e6
			row["est_cost"] = cost
			totalCost += cost
		}
		models = append(models, row)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"snapshot":      snap,
		"models":        models,
		"est_cost":      totalCost,
		"cost_basis":    "catalog price per 1M tokens (estimate, not invoice)",
		"price_missing": len(prices) == 0,
	})
}

// modelPriceMap 从模型目录缓存取 uid → 三类 token 单价（$/1M）。
func (h *Handler) modelPriceMap(ctx context.Context) map[string]struct {
	input, cached, output float64
} {
	type price struct{ input, cached, output float64 }
	out := map[string]price{}
	models, err := h.cachedModels(ctx)
	if err != nil {
		return nil
	}
	for _, m := range models {
		uid, _ := m["uid"].(string)
		if uid == "" {
			continue
		}
		out[uid] = price{
			input:  floatAny(m["price_input"]),
			cached: floatAny(m["price_cached"]),
			output: floatAny(m["price_output"]),
		}
	}
	return out
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

// apiMergedResponse 把请求目录内 06-http-response.jsonl 的 SSE 帧合并成
// 可读的最终响应文本（ccLoad merge-debug-response 同款），原始帧仍可读。
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
	_ = json.NewEncoder(w).Encode(mergeStreamEvents(data))
}

// apiActiveRequests 返回仍在进行中的请求快照：已耗时、丢弃数、
// 已落盘文件清单——请求未结束就能检查它收到过什么（ccLoad 同款）。
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
		"text":      string(data),
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
		"text":        string(data),
		"next_offset": next,
	})
}

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.password != "" && !h.isAuthenticated(r) {
		_, _ = w.Write([]byte(loginPage))
		return
	}
	_, _ = w.Write([]byte(dashboardPage))
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	if h.password == "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"open":true}`))
		return
	}
	password := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(password), []byte(h.password)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"密码错误"}`))
		return
	}
	sessionID := generateSessionID()
	h.sessionMu.Lock()
	h.sessionTokens[sessionID] = time.Now().Add(24 * time.Hour)
	h.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "devin_panel_session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400,
	})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (h *Handler) isAuthenticated(r *http.Request) bool {
	if h.password == "" {
		return true
	}
	// Agent 友好：除 session cookie 外，允许直接用 Bearer 密码访问 API，
	// 省去先登录拿 cookie 的交互步骤（curl -H 'Authorization: Bearer <密码>'）。
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		if subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, "Bearer ")), []byte(h.password)) == 1 {
			return true
		}
	}
	cookie, err := r.Cookie("devin_panel_session")
	if err != nil {
		return false
	}
	h.sessionMu.RLock()
	expiry, ok := h.sessionTokens[cookie.Value]
	h.sessionMu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		h.sessionMu.Lock()
		delete(h.sessionTokens, cookie.Value)
		h.sessionMu.Unlock()
		return false
	}
	return true
}

func (h *Handler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.isAuthenticated(r) {
		return true
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"未授权"}`))
	return false
}

func (h *Handler) apiStatus(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	// 面板聚合多个上游调用，给足时间避免单个慢接口拖垮整体；
	// 与 ResponseHeaderTimeout 对齐，允许上游长时思考/排队。
	ctx, cancel := context.WithTimeout(r.Context(), 610*time.Second)
	defer cancel()

	result := map[string]any{}
	var resultMu sync.Mutex

	// 正确路径：JSON Connect SeatManagement GetUserStatus（Bearer + metadata.api_key）
	if user, plan, planInfo, err := h.fetchUserStatus(ctx); err != nil {
		resultMu.Lock()
		result["user_status_error"] = err.Error()
		resultMu.Unlock()
	} else {
		resultMu.Lock()
		if user != nil {
			result["user"] = user
		}
		if plan != nil {
			result["plan_status"] = plan
		}
		if planInfo != nil {
			result["plan_info"] = planInfo
		}
		resultMu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		capResp, err := h.apiClient.CheckChatCapacity(ctx, connect.NewRequest(&devinproto.CheckChatCapacityRequest{
			Metadata: buildMetadata(h.token),
		}))
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["capacity_error"] = err.Error()
			return
		}
		result["capacity"] = map[string]any{
			"has_capacity":    capResp.Msg.GetHasCapacity(),
			"message":         capResp.Msg.GetMessage(),
			"active_sessions": capResp.Msg.GetActiveSessions(),
		}
	}()

	go func() {
		defer wg.Done()
		statusResp, err := h.apiClient.GetStatus(ctx, connect.NewRequest(&devinproto.GetStatusRequest{
			Metadata: buildMetadata(h.token),
		}))
		resultMu.Lock()
		defer resultMu.Unlock()
		if err != nil {
			result["status_error"] = err.Error()
			return
		}
		st := statusResp.Msg.GetStatus()
		result["ide_status"] = map[string]any{
			"level":   shortEnum(st.GetLevel().String(), "STATUS_LEVEL_"),
			"message": st.GetMessage(),
		}
		result["show_review_prompt"] = statusResp.Msg.GetShowReviewPrompt()
	}()

	go func() {
		defer wg.Done()
		statuses := h.cachedModelStatuses(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if statuses != nil {
			result["model_statuses"] = statuses
		}
	}()

	go func() {
		defer wg.Done()
		providers := h.cachedProviders(ctx)
		resultMu.Lock()
		defer resultMu.Unlock()
		if providers != nil {
			result["providers"] = providers
		}
	}()

	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// fetchUserStatus 调用官方 seat_management JSON Connect 路径。
func (h *Handler) fetchUserStatus(ctx context.Context) (user, plan, planInfo map[string]any, err error) {
	bodyObj := map[string]any{
		"metadata": map[string]any{
			"api_key":           h.token,
			"extension_name":    clientName,
			"extension_version": clientVersion,
			"ide_name":          clientName,
			"ide_version":       clientVersion,
			"locale":            "en",
			"os":                "windows",
		},
	}
	payload, err := json.Marshal(bodyObj)
	if err != nil {
		return nil, nil, nil, err
	}
	url := h.baseURL + seatUserStatusPath
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+h.token)

	// 使用不带 Basic 改写的 client，避免 authTransport 覆盖 Bearer；但复用代理 transport。
	// 与 ResponseHeaderTimeout 对齐，允许上游长时思考/排队。
	client := &http.Client{Timeout: 610 * time.Second, Transport: h.baseTransport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, fmt.Errorf("GetUserStatus HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, nil, nil, fmt.Errorf("decode GetUserStatus: %w", err)
	}
	us, _ := root["userStatus"].(map[string]any)
	if us == nil {
		us, _ = root["user_status"].(map[string]any)
	}
	if us == nil {
		return nil, nil, nil, fmt.Errorf("GetUserStatus: empty userStatus")
	}

	user = map[string]any{
		"name":                strAny(us["name"]),
		"email":               strAny(us["email"]),
		"pro":                 boolAny(us["pro"]),
		"user_id":             strAny(us["userId"], us["user_id"]),
		"team_id":             strAny(us["teamId"], us["team_id"]),
		"teams_tier":          shortEnum(strAny(us["teamsTier"], us["teams_tier"]), "TEAMS_TIER_"),
		"used_prompt_credits": numAny(us["userUsedPromptCredits"], us["user_used_prompt_credits"]),
		"used_flow_credits":   numAny(us["userUsedFlowCredits"], us["user_used_flow_credits"]),
		"max_premium_chat":    numAny(us["maxNumPremiumChatMessages"], us["max_num_premium_chat_messages"]),
	}

	ps, _ := us["planStatus"].(map[string]any)
	if ps == nil {
		ps, _ = us["plan_status"].(map[string]any)
	}
	if ps != nil {
		plan = map[string]any{
			"available_prompt_credits": numAny(ps["availablePromptCredits"], ps["available_prompt_credits"]),
			"available_flow_credits":   numAny(ps["availableFlowCredits"], ps["available_flow_credits"]),
			"available_flex_credits":   numAny(ps["availableFlexCredits"], ps["available_flex_credits"]),
			"used_flex_credits":        numAny(ps["usedFlexCredits"], ps["used_flex_credits"]),
			"used_flow_credits":        numAny(ps["usedFlowCredits"], ps["used_flow_credits"]),
			"used_prompt_credits":      numAny(ps["usedPromptCredits"], ps["used_prompt_credits"]),
			"daily_quota_remaining":    numAny(ps["dailyQuotaRemainingPercent"], ps["daily_quota_remaining_percent"]),
			"weekly_quota_remaining":   numAny(ps["weeklyQuotaRemainingPercent"], ps["weekly_quota_remaining_percent"]),
			"daily_quota_reset":        numAny(ps["dailyQuotaResetAtUnix"], ps["daily_quota_reset_at_unix"]),
			"weekly_quota_reset":       numAny(ps["weeklyQuotaResetAtUnix"], ps["weekly_quota_reset_at_unix"]),
			"acu_consumed":             numAny(ps["acuConsumed"], ps["acu_consumed"]),
			"acu_limit":                numAny(ps["acuLimit"], ps["acu_limit"]),
			"overage_balance_micros":   numAny(ps["overageBalanceMicros"], ps["overage_balance_micros"]),
			"plan_start":               strAny(ps["planStart"], ps["plan_start"]),
			"plan_end":                 strAny(ps["planEnd"], ps["plan_end"]),
		}
		pi, _ := ps["planInfo"].(map[string]any)
		if pi == nil {
			pi, _ = ps["plan_info"].(map[string]any)
		}
		if pi != nil {
			plan["plan_name"] = strAny(pi["planName"], pi["plan_name"])
			plan["monthly_prompt_credits"] = numAny(pi["monthlyPromptCredits"], pi["monthly_prompt_credits"])
			plan["monthly_flow_credits"] = numAny(pi["monthlyFlowCredits"], pi["monthly_flow_credits"])
			plan["billing_strategy"] = shortEnum(strAny(pi["billingStrategy"], pi["billing_strategy"]), "BILLING_STRATEGY_")
			plan["is_teams"] = boolAny(pi["isTeams"], pi["is_teams"])
			plan["is_enterprise"] = boolAny(pi["isEnterprise"], pi["is_enterprise"])
			plan["can_buy_more"] = boolAny(pi["canBuyMoreCredits"], pi["can_buy_more_credits"])
			plan["has_paid_features"] = boolAny(pi["hasPaidFeatures"], pi["has_paid_features"])
		}
	}

	// 顶层 planInfo（部分响应会挂在 root）
	if top, ok := root["planInfo"].(map[string]any); ok {
		planInfo = map[string]any{
			"plan_name":                 strAny(top["planName"], top["plan_name"]),
			"monthly_prompt_credits":    numAny(top["monthlyPromptCredits"], top["monthly_prompt_credits"]),
			"monthly_flow_credits":      numAny(top["monthlyFlowCredits"], top["monthly_flow_credits"]),
			"billing_strategy":          shortEnum(strAny(top["billingStrategy"], top["billing_strategy"]), "BILLING_STRATEGY_"),
			"is_teams":                  boolAny(top["isTeams"], top["is_teams"]),
			"is_enterprise":             boolAny(top["isEnterprise"], top["is_enterprise"]),
			"has_paid_features":         boolAny(top["hasPaidFeatures"], top["has_paid_features"]),
			"max_premium_chat_messages": numAny(top["maxNumPremiumChatMessages"], top["max_num_premium_chat_messages"]),
		}
	} else if plan != nil {
		if name, ok := plan["plan_name"]; ok {
			planInfo = map[string]any{
				"plan_name":              name,
				"monthly_prompt_credits": plan["monthly_prompt_credits"],
				"monthly_flow_credits":   plan["monthly_flow_credits"],
				"billing_strategy":       plan["billing_strategy"],
				"is_teams":               plan["is_teams"],
				"is_enterprise":          plan["is_enterprise"],
				"has_paid_features":      plan["has_paid_features"],
			}
		}
	}
	return user, plan, planInfo, nil
}

func (h *Handler) apiModels(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	// 模型目录可能较大，给足时间并复用缓存；与 ResponseHeaderTimeout 对齐。
	ctx, cancel := context.WithTimeout(r.Context(), 610*time.Second)
	defer cancel()

	models, err := h.cachedModels(ctx)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprintf(w, `{"error":"%s"}`, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"models": models})
}

func (h *Handler) cachedModels(ctx context.Context) ([]map[string]any, error) {
	h.cacheMu.RLock()
	if h.modelsCache != nil && time.Now().Before(h.modelsExpiry) {
		cached := h.modelsCache
		h.cacheMu.RUnlock()
		return cached, nil
	}
	h.cacheMu.RUnlock()

	// CLI 版响应与 Cascade 版模型表一致，并多出 subagent_default_model_uid 等字段。
	resp, err := h.apiClient.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: buildMetadata(h.token),
	}))
	if err != nil {
		return nil, err
	}

	var models []map[string]any
	for _, c := range resp.Msg.GetClientModelConfigs() {
		uid := c.GetModelUid()
		if uid == "" && c.GetModelOrAlias() != nil {
			uid = c.GetModelOrAlias().GetModelUid()
		}
		costTier := "unspecified"
		switch c.GetModelCostTier() {
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_FREE:
			costTier = "free"
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_LOW:
			costTier = "low"
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_MEDIUM:
			costTier = "medium"
		case devinproto.ExaCodeiumCommonPb_ModelCostTier_ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_HIGH:
			costTier = "high"
		}

		mult := c.GetCreditMultiplier()
		multKnown := mult != 0 || costTier == "free"
		pricingType := shortEnum(c.GetPricingType().String(), "MODEL_PRICING_TYPE_")
		provider := shortEnum(c.GetProvider().String(), "MODEL_PROVIDER_")
		apiProvider := shortEnum(c.GetApiProvider().String(), "API_PROVIDER_")

		m := map[string]any{
			"uid":                 uid,
			"label":               c.GetLabel(),
			"description":         c.GetDescription(),
			"cost_tier":           costTier,
			"credit_multiplier":   mult,
			"multiplier_known":    multKnown,
			"pricing_type":        pricingType,
			"provider":            provider,
			"api_provider":        apiProvider,
			"max_tokens":          c.GetMaxTokens(),
			"disabled":            c.GetDisabled(),
			"is_premium":          c.GetIsPremium(),
			"is_beta":             c.GetIsBeta(),
			"is_new":              c.GetIsNew(),
			"is_recommended":      c.GetIsRecommended(),
			"supports_images":     c.GetSupportsImages(),
			"is_capacity_limited": c.GetIsCapacityLimited(),
			"supports_legacy":     c.GetSupportsLegacy(),
		}
		if modelInfo := c.GetModelInfo(); modelInfo != nil {
			m["context_tokens"] = modelInfo.GetMaxTokens()
			m["max_output_tokens"] = modelInfo.GetMaxOutputTokens()
			m["is_model_router"] = modelInfo.GetIsModelRouter()
			if features := modelInfo.GetModelFeatures(); features != nil {
				m["supports_tool_calls"] = features.GetSupportsToolCalls()
				m["supports_parallel_tool_calls"] = features.GetSupportsParallelToolCalls()
				m["supports_thinking"] = features.GetSupportsThinking()
				m["preserve_thinking"] = features.GetPreserveThinking()
				m["interleave_thinking"] = features.GetInterleaveThinking()
			}
		}

		var dims []map[string]any
		var inputPrice, cachedPrice, outputPrice float64
		var hasInput, hasCached, hasOutput bool
		for _, d := range c.GetModelDimensions() {
			dim := map[string]any{
				"label":       d.GetLabel(),
				"value":       d.GetValue(),
				"min":         d.GetMinRange(),
				"max":         d.GetMaxRange(),
				"denominator": d.GetDenominator(),
				"kind":        shortEnum(d.GetKind().String(), "MODEL_DIMENSION_KIND_"),
				"info":        d.GetInfo(),
			}
			dims = append(dims, dim)
			switch strings.ToLower(d.GetLabel()) {
			case "input":
				inputPrice, hasInput = float64(d.GetValue()), true
			case "cached input", "cached_input", "cache read", "cache_read":
				cachedPrice, hasCached = float64(d.GetValue()), true
			case "output":
				outputPrice, hasOutput = float64(d.GetValue()), true
			}
		}
		if len(dims) > 0 {
			m["dimensions"] = dims
		}
		if hasInput {
			m["price_input"] = inputPrice
		}
		if hasCached {
			m["price_cached"] = cachedPrice
		}
		if hasOutput {
			m["price_output"] = outputPrice
		}

		if ps := c.GetPromoStatus(); ps != nil && ps.GetIsActive() {
			promo := map[string]any{"active": true, "label": ps.GetLabel()}
			if ed := ps.GetEndDate(); ed != nil && ed.GetSeconds() != 0 {
				promo["end_date"] = time.Unix(ed.GetSeconds(), int64(ed.GetNanos())).UTC().Format(time.RFC3339)
			}
			m["promo"] = promo
		}
		if fs := c.GetFastStatus(); fs != nil && fs.GetIsActive() {
			m["fast"] = map[string]any{"active": true, "tooltip": fs.GetTooltip()}
		}
		if fm := c.GetModelFamilyMetadata(); fm != nil {
			m["family"] = fm.GetModelFamilyLabel()
			m["is_default_in_family"] = fm.GetIsDefaultModelInFamily() || c.GetIsDefaultModelInFamily()
		}
		if dr := c.GetDisabledReason(); dr != nil {
			m["disabled_reason"] = dr.GetShortReason()
			m["disabled_description"] = dr.GetDescription()
		}
		if c.GetBetaWarningMessage() != "" {
			m["beta_warning"] = c.GetBetaWarningMessage()
		}
		models = append(models, m)
	}

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	// 请求期间可能有其他 goroutine 已写入缓存，不覆盖更热数据。
	if h.modelsCache != nil && time.Now().Before(h.modelsExpiry) {
		return h.modelsCache, nil
	}
	h.modelsCache = models
	h.modelsExpiry = time.Now().Add(h.cacheTTL)
	return models, nil
}

func buildMetadata(token string) *devinproto.ExaCodeiumCommonPb_Metadata {
	fingerprint, _ := randomHex(32)
	return &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(token),
		ExtensionName:    proto.String(clientName),
		ExtensionVersion: proto.String(clientVersion),
		IdeName:          proto.String(clientName),
		IdeVersion:       proto.String(clientVersion),
		Locale:           proto.String("en"),
		Os:               proto.String("win"),
		F:                proto.String(fingerprint),
	}
}

func (h *Handler) cachedProviders(ctx context.Context) []map[string]any {
	h.cacheMu.RLock()
	if h.providersCache != nil && time.Now().Before(h.providersExpiry) {
		cached := h.providersCache
		h.cacheMu.RUnlock()
		return cached
	}
	h.cacheMu.RUnlock()

	providerResp, err := h.apiClient.GetModelProviders(ctx, connect.NewRequest(&devinproto.GetModelProvidersRequest{}))
	if err != nil {
		return nil
	}
	var providers []map[string]any
	for _, p := range providerResp.Msg.GetModelProviders() {
		providers = append(providers, map[string]any{
			"provider":     shortEnum(p.GetProvider().String(), "MODEL_PROVIDER_"),
			"display_name": p.GetDisplayName(),
		})
	}

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.providersCache != nil && time.Now().Before(h.providersExpiry) {
		return h.providersCache
	}
	h.providersCache = providers
	h.providersExpiry = time.Now().Add(h.cacheTTL)
	return providers
}

func (h *Handler) cachedModelStatuses(ctx context.Context) []map[string]any {
	h.cacheMu.RLock()
	if h.modelStatusesCache != nil && time.Now().Before(h.modelStatusesExpiry) {
		cached := h.modelStatusesCache
		h.cacheMu.RUnlock()
		return cached
	}
	h.cacheMu.RUnlock()

	modelStatusResp, err := h.apiClient.GetModelStatuses(ctx, connect.NewRequest(&devinproto.GetModelStatusesRequest{
		Metadata: buildMetadata(h.token),
	}))
	if err != nil {
		return nil
	}
	var statuses []map[string]any
	for _, s := range modelStatusResp.Msg.GetModelStatusInfos() {
		statuses = append(statuses, map[string]any{
			"model":  shortEnum(s.GetModel().String(), "MODEL_"),
			"status": shortEnum(s.GetStatus().String(), "MODEL_STATUS_"),
		})
	}

	h.cacheMu.Lock()
	defer h.cacheMu.Unlock()
	if h.modelStatusesCache != nil && time.Now().Before(h.modelStatusesExpiry) {
		return h.modelStatusesCache
	}
	h.modelStatusesCache = statuses
	h.modelStatusesExpiry = time.Now().Add(h.cacheTTL)
	return statuses
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	// ApiServer Connect 客户端沿用 Basic token-token；Seat 单独走 Bearer。
	if clone.Header.Get("Authorization") == "" {
		clone.Header.Set("Authorization", "Basic "+t.token+"-"+t.token)
	}
	return t.base.RoundTrip(clone)
}

func randomHex(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func generateSessionID() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func shortEnum(full, prefix string) string {
	if full == "" {
		return ""
	}
	for _, p := range []string{
		"ExaCodeiumCommonPb_ModelProvider_MODEL_PROVIDER_",
		"ExaCodeiumCommonPb_APIProvider_API_PROVIDER_",
		"ExaCodeiumCommonPb_ModelPricingType_MODEL_PRICING_TYPE_",
		"ExaCodeiumCommonPb_ModelCostTier_MODEL_COST_TIER_",
		"ExaCodeiumCommonPb_ModelDimensionKind_MODEL_DIMENSION_KIND_",
		"ExaCodeiumCommonPb_StatusLevel_STATUS_LEVEL_",
		"ExaCodeiumCommonPb_ModelStatus_MODEL_STATUS_",
		"ExaCodeiumCommonPb_TeamsTier_TEAMS_TIER_",
		"ExaCodeiumCommonPb_BillingStrategy_BILLING_STRATEGY_",
		"ExaCodeiumCommonPb_Model_",
		"MODEL_PROVIDER_", "API_PROVIDER_", "MODEL_PRICING_TYPE_", "MODEL_COST_TIER_",
		"MODEL_DIMENSION_KIND_", "STATUS_LEVEL_", "MODEL_STATUS_", "TEAMS_TIER_", "BILLING_STRATEGY_",
		"MODEL_",
		prefix,
	} {
		if p == "" {
			continue
		}
		if idx := strings.Index(full, p); idx >= 0 {
			return full[idx+len(p):]
		}
	}
	if i := strings.LastIndex(full, "_"); i >= 0 && i+1 < len(full) {
		return full[i+1:]
	}
	return full
}

func strAny(vals ...any) string {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case string:
			if t != "" {
				return t
			}
		case json.Number:
			return t.String()
		case float64:
			return fmt.Sprintf("%.0f", t)
		default:
			s := fmt.Sprint(t)
			if s != "" && s != "<nil>" {
				return s
			}
		}
	}
	return ""
}

func boolAny(vals ...any) bool {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case bool:
			return t
		case string:
			return t == "true" || t == "1"
		}
	}
	return false
}

func numAny(vals ...any) any {
	for _, v := range vals {
		if v == nil {
			continue
		}
		switch t := v.(type) {
		case float64, float32, int, int32, int64, json.Number:
			return t
		case string:
			if t != "" {
				return t
			}
		}
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
