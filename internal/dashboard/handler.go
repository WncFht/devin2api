// Package dashboard 实现管理面板：模型列表、价格筛选、账户用量。
// password 为空时无需登录直接进入；非空时走 session cookie。
package dashboard

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/upstream"

	"local/devinproto/devinprotoconnect"
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
	// version 是运行中二进制的构建版本，由 main 经 SetVersion 注入。
	version string
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
	transport := upstream.NewBasicAuthTransport(base, token)
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

// SetVersion 记录构建版本，stats/index 端点透出，供排障辨认运行中的二进制。
func (h *Handler) SetVersion(version string) {
	h.version = version
}

// Register 将面板路由注册到 mux。有 token 即可启用；密码仅控制是否登录。
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
}) {
	mux.Get("/panel", h.servePanel)
	mux.Post("/panel/login", h.handleLogin)
	mux.Get("/panel/static/{name}", h.serveStatic)
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
	payload := map[string]any{"version": h.version}
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
		"version": h.version,
		"auth":    "dashboard.password 非空时可用 cookie 会话或 Authorization: Bearer <密码>",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/panel/api/status", "description": "账户/套餐/容量/渠道/模型状态告警"},
			{"method": "GET", "path": "/panel/api/models", "description": "模型目录含能力位与价格"},
			{"method": "GET", "path": "/panel/api/stats", "description": "进程运行指标（RPM/QPS/goroutine/内存/GC/CPU）+ 60 分钟逐 30 秒趋势 + 日志管道自观测 + index 聚合用量"},
			{"method": "GET", "path": "/panel/api/usage", "description": "index.jsonl 聚合：今日/窗口累计、model_days 模型×日矩阵（供面板时间范围选择器）、按模型/按 key、错误阶段、8 天 10 分钟粒度趋势（含缓存命中率与均速原料）、p50/p95/p99、目录价估算成本"},
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

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if h.password != "" && !h.isAuthenticated(r) {
		_, _ = w.Write([]byte(loginPage))
		return
	}
	_, _ = w.Write([]byte(dashboardPage))
}

// staticAssets 是 vendored 前端库（uPlot 等），随二进制 go:embed 打包，
// 面板离线可用，不依赖 CDN。
var staticAssets = map[string]struct {
	contentType string
	body        []byte
}{
	"uplot.iife.min.js": {"text/javascript; charset=utf-8", uplotJS},
	"uplot.min.css":     {"text/css; charset=utf-8", uplotCSS},
}

// serveStatic 下发 vendored 前端资源；内容随二进制固定，按天缓存。
func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	asset, ok := staticAssets[chi.URLParam(r, "name")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(asset.body)
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
