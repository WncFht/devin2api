// Package ccpanel 内嵌移植自 ccLoad（MIT，作者 caidaoli）的管理面板，
// 按其原路径契约挂在主 mux 上：/web/* 静态资源、/login|/logout、
// /public/*、/dashboard/*、/admin/*。它是本服务唯一的管理面板——原
// /panel 时代的后端能力（上游客户端、目录缓存、密码与爆破账本、
// token 脱敏、配置自省、配额采样）已并入本包。
//
// 鉴权：移植前端把 dashboard.password 本身当 Bearer token（登录接口
// 返回 token=密码），配合按 IP 的爆破账本；无会话表、跨重启不掉线。
package ccpanel

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
	"github.com/WncFht/devin2api/internal/obs"
)

// Handler 提供面板的全部路由与后端服务。
type Handler struct {
	// authMu 保护 password/passwordHash：配置 reload 会运行时换值。
	authMu   sync.RWMutex
	password string
	// passwordHash 是面板密码的 SHA-256：比较走定长哈希，既不向
	// ConstantTimeCompare 泄漏长度，也与 apiKeyMiddleware 的口径一致。
	passwordHash [32]byte
	// loginMu 保护 loginFailures：按客户端 IP 记录连续登录失败与锁定期——
	// 面板是唯一持密码的端点，爆破代价要抬高。
	loginMu       sync.RWMutex
	loginFailures map[string]*loginFail
	// recentTokens 是最近见过的上游凭据（token 自愈轮换会换新）：
	// maskToken 按这个集合脱敏，旧请求目录里的历史 token 字面值也罩住。
	tokenMu      sync.Mutex
	recentTokens []string

	// tokenFunc 每次求值返回当前上游凭据——adapter 的 unauthenticated
	// 自愈更新 token 后面板跟随新值，不缓存启动时的静态快照。
	tokenFunc func() string
	// upstreamPtr 持有当前生效的上游调用束（connect client、裸 transport
	// 与归一化 baseURL 固化在同一份 base_url/proxy/force_http1 上）：
	// endpoint 配置热应用时 SetUpstream 整体重建、原子换指针，
	// 在途调用持旧引用跑完。New 之后恒非 nil。
	upstreamPtr atomic.Pointer[panelUpstream]

	// 面板数据缓存：模型目录、供应商列表、模型状态均不经常变化，缓存可显著降低上游压力。
	// 每个缓存各持一把锁——拉取上游发生在写锁内（锁内复查把并发 miss 收敛成
	// 单次 RPC），共用一把会让一个慢接口（上限 610s）堵住无关缓存的读。
	cacheTTL     time.Duration
	modelsMu     sync.RWMutex
	modelsCache  []map[string]any
	modelsExpiry time.Time
	// modelsFetch 非空表示有目录拉取在途（singleflight 的 done channel）；
	// 由 modelsMu 保护，关闭即完成信号。
	modelsFetch         chan struct{}
	providersMu         sync.RWMutex
	providersCache      []map[string]any
	providersExpiry     time.Time
	modelStatusesMu     sync.RWMutex
	modelStatusesCache  []map[string]any
	modelStatusesExpiry time.Time

	// quotaMu/quotaCancel 管配额采样协程生命周期：SetQuotaInterval
	// cancel 旧协程按新间隔重起（配置 reload 热路径）。quotaInterval
	// 记最近一次请求的周期，供设置页回读。
	quotaMu       sync.Mutex
	quotaCancel   context.CancelFunc
	quotaInterval time.Duration

	// debug 是 index.jsonl 与请求目录的读取入口。
	debug *debuglog.Manager
	// metrics 是进程级运行计数器（runtime-metrics 端点）。
	metrics *obs.Metrics
	// gateStats 返回速率闸门快照；nil 时 runtime-metrics 不投 gate 组。
	gateStats func() devin.GateStats
	// configOps 挂配置自省与热重载端点；nil 时两个端点 404。
	configOps *ConfigOps
	// maxConcurrencyFunc 返回 /v1 管线的全局并发上限运行时值
	// （配置 reload 后为新值），投影到 runtime-metrics 的 max_concurrency。
	maxConcurrencyFunc func() int
	// aliasesFunc 返回模型别名表（注册表落地前的静态种子）。
	aliasesFunc func() map[string]string
	// tokens 是下游令牌仓；nil 时 api_token 登录与令牌端点不可用。
	tokens *authtoken.Store
	// models 是模型注册表仓；nil 时 /admin/model-registry 返回 503。
	models *modelreg.Store
	// settings 是运行时设置键仓（panel-settings.json）；nil 时 /admin/settings
	// 返回空表。
	settings *PanelSettings
	// probeHandler 是应用根路由（含 /v1 管线），模型探活经它发进程内
	// 真实请求；nil 时 /admin/model-test 返回 503。
	probeHandler http.Handler
	// masterKeyFunc 返回当前生效的 auth.api_key（热重载后为新值），
	// 探活鉴权与令牌页主密钥卡共用。
	masterKeyFunc func() string
	// warmStats 返回前缀保温簿记快照；nil 时 runtime-metrics 不投 warm 组。
	warmStats func() devin.WarmStats

	versionMu sync.RWMutex
	version   string
	startedAt time.Time

	staticEntries sync.Map
	// ru 是 index.jsonl 的增量聚合立方体：(10分钟槽 × 入口api × 模型)，
	// 支撑 /dashboard/{summary,metrics,stats} 的任意时间窗查询。
	ru *rollup
}

// New 创建面板处理器。password 为空表示开放访问。proxy 为可选代理地址。
// forceHTTP1 为 true 时强制 HTTP/1.1，与 adapter 保持一致的连接模型。
// tokenFunc 每次求值返回当前上游凭据（与 adapter 的自愈共用同一来源）；
// nil 视为恒空凭据。metrics/debug 允许为 nil（对应端点降级为空数据）。
func New(password, baseURL string, tokenFunc func() string, proxy string, forceHTTP1 bool, metrics *obs.Metrics, debug *debuglog.Manager) (*Handler, error) {
	if tokenFunc == nil {
		tokenFunc = func() string { return "" }
	}
	up, err := newPanelUpstream(baseURL, proxy, forceHTTP1, tokenFunc)
	if err != nil {
		return nil, err
	}
	h := &Handler{
		password:      password,
		passwordHash:  sha256.Sum256([]byte(password)),
		tokenFunc:     tokenFunc,
		loginFailures: make(map[string]*loginFail),
		cacheTTL:      5 * time.Minute,
		metrics:       metrics,
		debug:         debug,
		startedAt:     time.Now(),
		ru:            newRollup(),
	}
	h.upstreamPtr.Store(up)
	return h, nil
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

// SetPassword 运行时更换面板密码（配置 reload 热路径）。换密码的运维
// 语义是踢人——前端拿旧 Bearer 立即 401。
func (h *Handler) SetPassword(password string) {
	h.authMu.Lock()
	h.password = password
	h.passwordHash = sha256.Sum256([]byte(password))
	h.authMu.Unlock()
}

// passwordSnapshot 返回密码与哈希的一致性快照。
func (h *Handler) passwordSnapshot() (string, [32]byte) {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.password, h.passwordHash
}

// SetGateStats 注入速率闸门快照源（runtime-metrics 的 gate 组）。
func (h *Handler) SetGateStats(fn func() devin.GateStats) {
	h.gateStats = fn
}

// SetConfigOps 注入配置自省与热重载操作面（/admin/config*）。
func (h *Handler) SetConfigOps(ops ConfigOps) {
	h.configOps = &ops
}

// SetAliasesFunc 注入别名表读取函数。
func (h *Handler) SetAliasesFunc(fn func() map[string]string) {
	h.aliasesFunc = fn
}

// SetMaxConcurrencyFunc 注入 /v1 并发上限读取函数。
func (h *Handler) SetMaxConcurrencyFunc(fn func() int) {
	h.maxConcurrencyFunc = fn
}

// maxConcurrency 返回全局并发上限的运行时值；未注入 getter 时按 0
// （无限制）透出。
func (h *Handler) maxConcurrency() int {
	if h.maxConcurrencyFunc == nil {
		return 0
	}
	return h.maxConcurrencyFunc()
}

// SetTokenStore 注入下游令牌仓（api_token 登录与 /admin/auth-tokens 用）。
func (h *Handler) SetTokenStore(s *authtoken.Store) {
	h.tokens = s
}

// SetModelRegistry 注入模型注册表仓（/admin/model-registry 用）。
func (h *Handler) SetModelRegistry(s *modelreg.Store) {
	h.models = s
}

// SetSettingsStore 注入运行时设置键仓（/admin/settings 用）。
func (h *Handler) SetSettingsStore(s *PanelSettings) {
	h.settings = s
}

// SetProbeHandler 注入应用根路由；模型探活在进程内 ServeHTTP，走与外部
// 请求完全相同的鉴权/准入/重定向/上游路径。
func (h *Handler) SetProbeHandler(handler http.Handler) {
	h.probeHandler = handler
}

// SetMasterKeyFunc 注入 auth.api_key 读取函数。
func (h *Handler) SetMasterKeyFunc(fn func() string) {
	h.masterKeyFunc = fn
}

// SetWarmStats 注入前缀保温簿记读取函数（/admin/runtime-metrics 的 warm 组）。
func (h *Handler) SetWarmStats(fn func() devin.WarmStats) {
	h.warmStats = fn
}

// Register 把面板路由挂到 mux。/web、/login、/logout、/public 为
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

	mux.Get("/admin/active-requests", h.withAuth(h.adminActiveRequests))
	mux.Get("/admin/active-requests/{id}/debug-log", h.withAuth(h.adminActiveRequestDebugLog))
	mux.Post("/admin/active-requests/{id}/abort", h.withAuth(h.adminAbortActiveRequest))
	mux.Get("/admin/logs", h.withAuth(h.dashboardLogs))
	mux.Get("/admin/logs/bootstrap", h.withAuth(h.dashboardLogsBootstrap))
	mux.Get("/admin/logs/export", h.withAuth(h.adminLogsExport))
	mux.Get("/admin/logs/matrix", h.withAuth(h.adminLogsMatrix))
	mux.Post("/admin/debug-logs/merged-response", h.withAuth(h.adminMergedResponse))
	mux.Get("/admin/debug-logs/{id}", h.withAuth(h.adminDebugLog))
	mux.Get("/admin/debug-logs/{id}/file/*", h.withAuth(h.adminDebugLogFile))
	mux.Get("/admin/debug-logs/{id}/merged", h.withAuth(h.adminDebugLogMerged))
	mux.Get("/admin/metrics", h.withAuth(h.dashboardMetrics))
	mux.Get("/admin/stats", h.withAuth(h.dashboardStats))
	mux.Get("/admin/stats/filter-options", h.withAuth(h.dashboardStatsFilterOptions))
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
	mux.Post("/admin/model-test", h.withAuth(h.adminModelTest))
	mux.Post("/admin/model-chat", h.withAuth(h.adminModelChat))
	mux.Put("/admin/model-registry", h.withAuth(h.adminPutModel))
	mux.Delete("/admin/model-registry", h.withAuth(h.adminDeleteModel))
	mux.Get("/admin/model-pricing", h.withAuth(h.adminModelPricing))
	mux.Get("/admin/runtime-metrics", h.withAuth(h.adminRuntimeMetrics))
	mux.Get("/admin/quota", h.withAuth(h.adminQuota))
	mux.Get("/admin/status", h.withAuth(h.adminStatus))
	mux.Get("/admin/api", h.withAuth(h.adminAPIIndex))
	mux.Get("/admin/config", h.withAuth(h.adminConfigCurrent))
	mux.Post("/admin/config/reload", h.withAuth(h.adminConfigReload))
	mux.Get("/admin/process-log", h.withAuth(h.adminProcessLog))
	mux.Get("/admin/usage", h.withAuth(h.adminUsage))
}
