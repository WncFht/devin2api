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

	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/dashboard"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
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
	// tokens 是下游令牌仓；nil 时 api_token 登录与令牌端点不可用。
	tokens *authtoken.Store
	// models 是模型注册表仓；nil 时 /admin/model-registry 返回 503。
	models *modelreg.Store
	// settings 是运行时设置键仓（panel-settings.json）；nil 时 /admin/settings
	// 返回空表。
	settings *PanelSettings

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

// SetTokenStore 注入下游令牌仓（api_token 登录与 /admin/auth-tokens 用）。
func (h *Handler) SetTokenStore(s *authtoken.Store) {
	h.tokens = s
}

// SetModelRegistry 注入模型注册表仓（/admin/model-registry 与渠道模型
// 清单投影用）。
func (h *Handler) SetModelRegistry(s *modelreg.Store) {
	h.models = s
}

// SetSettingsStore 注入运行时设置键仓（/admin/settings 用）。
func (h *Handler) SetSettingsStore(s *PanelSettings) {
	h.settings = s
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

	mux.Get("/dashboard/session", h.withWebAuth(h.dashboardSession))
	mux.Get("/dashboard/summary", h.withWebAuth(h.dashboardSummary))
	mux.Get("/dashboard/metrics", h.withWebAuth(h.dashboardMetrics))
	mux.Get("/dashboard/logs", h.withWebAuth(h.dashboardLogs))
	mux.Get("/dashboard/logs/bootstrap", h.withWebAuth(h.dashboardLogsBootstrap))
	mux.Get("/dashboard/stats", h.withWebAuth(h.dashboardStats))
	mux.Get("/dashboard/stats/filter-options", h.withWebAuth(h.dashboardStatsFilterOptions))
	mux.Get("/dashboard/models", h.withWebAuth(h.dashboardModels))
	mux.Get("/dashboard/channels", h.withWebAuth(h.dashboardChannels))
	mux.Get("/dashboard/channels/filter-options", h.withWebAuth(h.channelFilterOptions))

	mux.Get("/admin/active-requests", h.withAuth(h.adminActiveRequests))
	mux.Get("/admin/active-requests/{id}/debug-log", h.withAuth(h.adminActiveRequestDebugLog))
	mux.Post("/admin/active-requests/{id}/abort", h.withAuth(h.adminAbortActiveRequest))
	mux.Get("/admin/logs", h.withAuth(h.dashboardLogs))
	mux.Get("/admin/logs/bootstrap", h.withAuth(h.dashboardLogsBootstrap))
	mux.Post("/admin/debug-logs/merged-response", h.withAuth(h.adminMergedResponse))
	mux.Get("/admin/debug-logs/{id}", h.withAuth(h.adminDebugLog))
	mux.Get("/admin/metrics", h.withAuth(h.dashboardMetrics))
	mux.Get("/admin/stats", h.withAuth(h.dashboardStats))
	mux.Get("/admin/stats/filter-options", h.withAuth(h.dashboardStatsFilterOptions))
	mux.Get("/admin/channels", h.withAuth(h.adminListChannels))
	mux.Get("/admin/channels/filter-options", h.withAuth(h.channelFilterOptions))
	mux.Get("/admin/channels/{id}", h.withAuth(h.adminGetChannel))
	mux.Get("/admin/channels/{id}/keys", h.withAuth(h.adminChannelKeys))
	mux.Get("/admin/settings", h.withAuth(h.adminListSettings))
	mux.Get("/admin/settings/{key}", h.withAuth(h.adminGetSetting))
	mux.Put("/admin/settings/{key}", h.withAuth(h.adminUpdateSetting))
	mux.Post("/admin/settings/{key}/reset", h.withAuth(h.adminResetSetting))
	mux.Post("/admin/settings/batch", h.withAuth(h.adminBatchUpdateSettings))
	mux.Post("/admin/update/check", h.withAuth(h.adminUpdateCheck))
	mux.Get("/admin/auth-tokens", h.withAuth(h.adminListAuthTokens))
	mux.Post("/admin/auth-tokens", h.withAuth(h.adminCreateAuthToken))
	mux.Put("/admin/auth-tokens/{id}", h.withAuth(h.adminUpdateAuthToken))
	mux.Delete("/admin/auth-tokens/{id}", h.withAuth(h.adminDeleteAuthToken))
	mux.Get("/admin/models", h.withAuth(h.dashboardModels))
	mux.Get("/admin/model-registry", h.withAuth(h.adminModelRegistry))
	mux.Put("/admin/model-registry", h.withAuth(h.adminPutModel))
	mux.Delete("/admin/model-registry", h.withAuth(h.adminDeleteModel))
	mux.Get("/admin/model-pricing", h.withAuth(h.adminModelPricing))
	mux.Get("/admin/runtime-metrics", h.withAuth(h.adminRuntimeMetrics))
}
