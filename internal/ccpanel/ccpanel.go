// Package ccpanel 内嵌移植自 ccLoad（MIT，作者 caidaoli）的管理面板，
// 按其原路径契约挂在主 mux 上：/web/* 静态资源、/login|/logout、
// /public/*、/dashboard/*、/admin/*。与旧版 /panel 共存，互不占路。
//
// 鉴权委托给 dashboard.Handler：移植前端把 dashboard.password 本身当
// Bearer token（登录接口返回 token=密码），复用同一 IP 爆破账本，
// 无会话表、跨重启不掉线。
package ccpanel

import (
	"net/http"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/dashboard"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/obs"
)

// synthChannelID 是合成渠道的固定 ID：移植前端的渠道下拉、日志行
// 渠道跳转、test-key 模态都要求至少一个渠道存在，本服务只有一条
// 上游，统一投影成 id=1 name="devin-2api"。
const synthChannelID = 1
const synthChannelName = "devin-2api"

// Handler 提供移植面板的全部路由。
type Handler struct {
	// panel 复用旧面板的密码快照、爆破账本与模型目录缓存。
	panel *dashboard.Handler
	// debug 是 index.jsonl 与请求目录的读取入口。
	debug *debuglog.Manager
	// metrics 是进程级运行计数器（runtime-metrics 端点）。
	metrics *obs.Metrics
	// baseURL 是当前上游地址，投影到合成渠道的 base_url。
	baseURL string
	// maxConcurrency 是 /v1 管线的全局并发上限（0=无限制），
	// 投影到 runtime-metrics 的 max_concurrency。
	maxConcurrency int
	// aliasesFunc 返回模型别名表（注册表落地前的静态种子）。
	aliasesFunc func() map[string]string

	versionMu sync.RWMutex
	version   string
	startedAt time.Time

	staticEntries sync.Map
	// ru 是 index.jsonl 的增量聚合立方体：(10分钟槽 × 入口api × 模型)，
	// 支撑 /dashboard/{summary,metrics,stats} 的任意时间窗查询。
	ru *rollup
}

// New 创建移植面板处理器。panel 为鉴权与目录委托对象，不得为 nil；
// debug/metrics 可为 nil（对应端点降级为空数据）。
func New(panel *dashboard.Handler, debug *debuglog.Manager, metrics *obs.Metrics, baseURL string, maxConcurrency int) *Handler {
	return &Handler{
		panel:          panel,
		debug:          debug,
		metrics:        metrics,
		baseURL:        baseURL,
		maxConcurrency: maxConcurrency,
		startedAt:      time.Now(),
		ru:             newRollup(),
	}
}

// SetVersion 记录构建版本（__VERSION__ 替换与 /public/version 用）。
func (h *Handler) SetVersion(version string) {
	h.versionMu.Lock()
	h.version = version
	h.versionMu.Unlock()
}

// Version 返回当前版本号。
func (h *Handler) Version() string {
	h.versionMu.RLock()
	defer h.versionMu.RUnlock()
	return h.version
}

// SetAliasesFunc 注入别名表读取函数。
func (h *Handler) SetAliasesFunc(fn func() map[string]string) {
	h.aliasesFunc = fn
}

// Register 把移植面板路由挂到 mux。/web、/login、/logout、/public 为
// 公开路径（页面自身在浏览器侧做登录门）；/dashboard、/admin 需 Bearer。
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
	Put(pattern string, handlerFn http.HandlerFunc)
	Patch(pattern string, handlerFn http.HandlerFunc)
	Delete(pattern string, handlerFn http.HandlerFunc)
}) {
	mux.Get("/web/*", h.serveStatic)
	mux.Post("/login", h.handleLogin)
	mux.Post("/logout", h.handleLogout)

	mux.Get("/public/version", h.publicVersion)
	mux.Get("/public/protocols", h.publicProtocols)

	mux.Get("/dashboard/session", h.withAuth(h.dashboardSession))
	mux.Get("/dashboard/summary", h.withAuth(h.dashboardSummary))
	mux.Get("/dashboard/metrics", h.withAuth(h.dashboardMetrics))
	mux.Get("/dashboard/models", h.withAuth(h.dashboardModels))
	mux.Get("/dashboard/channels/filter-options", h.withAuth(h.channelFilterOptions))

	mux.Get("/admin/active-requests", h.withAuth(h.adminActiveRequests))
	mux.Post("/admin/active-requests/{id}/abort", h.withAuth(h.adminAbortActiveRequest))
	mux.Get("/admin/channels", h.withAuth(h.adminListChannels))
	mux.Get("/admin/channels/filter-options", h.withAuth(h.channelFilterOptions))
	mux.Get("/admin/channels/{id}", h.withAuth(h.adminGetChannel))
	mux.Get("/admin/channels/{id}/keys", h.withAuth(h.adminChannelKeys))
	mux.Get("/admin/settings", h.withAuth(h.adminListSettings))
	mux.Get("/admin/auth-tokens", h.withAuth(h.adminListAuthTokens))
	mux.Get("/admin/models", h.withAuth(h.dashboardModels))
	mux.Get("/admin/model-pricing", h.withAuth(h.adminModelPricing))
	mux.Get("/admin/runtime-metrics", h.withAuth(h.adminRuntimeMetrics))
}
