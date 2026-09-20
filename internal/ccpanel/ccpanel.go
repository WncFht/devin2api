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
	"encoding/hex"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/selfupdate"
	"github.com/WncFht/devin2api/internal/store"
)

// Handler 提供面板的全部路由与后端服务。
type Handler struct {
	// authMu 保护密码相关字段：配置 reload 与面板内轮换都会运行时换值。
	authMu sync.RWMutex
	// filePassword 是 config.yaml 的 dashboard.password 原值（启动与
	// reload 写入）：它是应急回落层——DB 覆盖存在时不服役。
	filePassword string
	// dbPasswordHash/dbPasswordSet 是 runtime_state["dashboard.password_hash"]
	// 的内存镜像：面板内轮换的落点，存在即压过文件值（overlay 语义，
	// 不是双凭据——轮换才能把人踢出去）。
	dbPasswordHash [32]byte
	dbPasswordSet  bool
	// passwordHash 是生效哈希：dbPasswordSet 取覆盖，否则
	// sha256(filePassword)。比较走定长哈希，既不向 ConstantTimeCompare
	// 泄漏长度，也与 apiKeyMiddleware 的口径一致。
	passwordHash [32]byte
	// passwordSet 是「面板是否有密码」的生效判定——DB 覆盖下文件
	// 可能为空但面板仍有密码，旧的 password=="" 判据不够用。
	passwordSet bool
	// loginMu 保护 loginFailures：按客户端 IP 记录连续登录失败与锁定期——
	// 面板是唯一持密码的端点，爆破代价要抬高。
	loginMu       sync.RWMutex
	loginFailures map[string]*loginFail
	// recentTokens 是最近见过的上游凭据（token 自愈轮换会换新）：
	// maskToken 按这个集合脱敏，旧请求目录里的历史 token 字面值也罩住。
	// seedTokens 是启动时播种的常驻集合（config 声明的账号凭据 +
	// upstream_accounts 仓行）：重启后 recentTokens 环是空的，旧调试
	// 目录里的凭据字面值仍须罩住——它无容量上限、常驻不淘汰。
	tokenMu      sync.Mutex
	recentTokens []string
	seedTokens   map[string]struct{}

	// tokenFunc 每次求值返回当前上游凭据——adapter 的 unauthenticated
	// 自愈更新 token 后面板跟随新值，不缓存启动时的静态快照。
	// 号池下它是首号 lane 的凭据源：seat/状态类上游调用 MVP 绑首号。
	tokenFunc func() string
	// upstreamPtr 持有当前生效的上游调用束（connect client、裸 transport
	// 与归一化 baseURL 固化在同一份 base_url/proxy/force_http1 上）：
	// endpoint 配置热应用时 SetUpstream 整体重建、原子换指针，
	// 在途调用持旧引用跑完。New 之后恒非 nil。
	upstreamPtr atomic.Pointer[panelUpstream]

	// 面板数据缓存：模型目录、供应商列表、模型状态均不经常变化，缓存
	// 可显著降低上游压力；usage/status 两页快照同理。统一走 ttlCache——
	// 每个缓存各持一把锁，拉取永远锁外（旧实现把写锁横在 ≤610s 的 RPC
	// 上，一个慢接口会堵死同缓存全部读，锁内等待也不吃 ctx）。
	modelsCache        ttlCache[[]map[string]any]
	providersCache     ttlCache[[]map[string]any]
	modelStatusesCache ttlCache[[]map[string]any]
	// usageCache 是 UsageStats 聚合快照（logs 表十几条聚合查询）：
	// 面板轮询语义容忍秒级陈旧，SWR 让页面扇出的并发请求只吃一趟计算。
	usageCache ttlCache[store.UsageSnapshot]
	// statusCache 是 StatusReport 六路上游 RPC 并行聚合的快照，
	// 耗时≈最慢一路 RTT（实测 ~1s）——quota 页每次加载/轮询各付一趟。
	statusCache ttlCache[map[string]any]

	// quota 是配额采样子系统（协程生命周期/身份投影/落库重放/轮次
	// 心跳，实现见 quota.go）；quotaOnce 兜底字面量构造的 Handler
	// （测试绕开 New）在首个配额调用点懒挂。
	quotaOnce sync.Once
	quota     *quotaSampler

	// debug 是请求目录的读取入口（logs 表行查询走 store）。
	debug *debuglog.Manager
	// store 是 SQLite 持久层：日志行查询/聚合与配额样本读写都走它。
	// nil 时停采、日志与配额端点降级为空——与无 debug manager 的
	// 口径一致。
	store *store.Store
	// statsCache 缓存 /admin|/dashboard/stats 的聚合结果：一轮是
	// 格子扫描 + recentWindow×2 + lastByModel + recentRPM 的查询组，
	// 面板轮询重放同一查询，命中时跳过全部聚合。
	statsCache *statsCache
	// metrics 是进程级运行计数器（runtime-metrics 端点）。
	metrics *obs.Metrics
	// history 是进程指标历史环（runtime-metrics/history 端点数据源）：
	// StartMetricsHistory 起的采样协程单写，admin 读侧短锁拷出。
	history metricsHistory
	// pool 是面板对号池的全部依赖：遥测一次 Snapshot 取齐（读侧每
	// 请求一次求值，拿到的是同一时间切面而非逐方法拼出的混合切面），
	// 动作口各自独立可缺席。nil 视为无池：gate/warm/detached/accounts
	// 组缺席、配额采样退回单号匿名、排空跳过闸门冲刷。
	pool *PoolDeps
	// configOps 挂配置自省与热重载端点；nil 时两个端点 404。
	configOps *ConfigOps
	// accountOps 挂 /admin/accounts 账号 CRUD 与行操作面；nil 时
	// 该族端点 503。
	accountOps *accounts.AccountOps
	// maxConcurrencyFunc 返回 /v1 管线的全局并发上限运行时值
	// （配置 reload 后为新值），投影到 runtime-metrics 的 max_concurrency。
	maxConcurrencyFunc func() int
	// tokens 是下游令牌仓；nil 时 api_token 登录与令牌端点不可用。
	tokens *authtoken.Store
	// models 是模型注册表仓；nil 时 /admin/model-registry 返回 503。
	models *modelreg.Store
	// settings 是运行时设置键仓（settings 表）；nil 时 /admin/settings
	// 返回空表。
	settings *PanelSettings
	// probeHandler 是应用根路由（含 /v1 管线），模型探活经它发进程内
	// 真实请求；nil 时 /admin/model-test 返回 503。
	probeHandler http.Handler
	// probeToken 是模型探活的明文凭据：仓非空且没有匿名通道可用时
	// 由面板自铸一条 "panel: probe" 令牌，明文只留在内存（仓里只有
	// 哈希）。令牌被删后下一次探活自动重铸。
	probeTokenMu sync.Mutex
	probeToken   string
	// updateOps 是自更新服务（/admin/update*）；nil 时该族端点 501。
	updateOps *selfupdate.Service

	versionMu sync.RWMutex
	version   string
	startedAt time.Time

	staticEntries sync.Map
}

// dashboardPasswordHashKey 是面板密码覆盖行在 runtime_state 的键：
// 值是 sha256 的 hex——仓内不落明文凭据（与 auth_tokens 同口径）。
const dashboardPasswordHashKey = "dashboard.password_hash"

// Deps 是 New 的装配入口：依赖一次给全，调用方不再背「哪个 Set* 先
// 调」的顺序知识（旧接口里 store 必须先于 SetQuotaInterval 注入，否则
// 采样协程按起跑时的 nil 句柄定生死）。各项缺席语义在字段注释标注。
// 真正迟绑定的操作面保留 setter——settings/probeHandler/configOps/
// accountOps 的构造依赖面板自身（方法值或根路由），只能后挂。
type Deps struct {
	// Password 为空表示开放访问。
	Password   string
	BaseURL    string
	Proxy      string // 可选代理地址
	ForceHTTP1 bool   // 强制 HTTP/1.1，与 adapter 保持一致的连接模型
	// TokenFunc 每次求值返回当前上游凭据（与 adapter 的自愈共用同一
	// 来源）；nil 视为恒空凭据。
	TokenFunc func() string
	// Metrics/Debug 允许为 nil（对应端点降级为空数据）。
	Metrics *obs.Metrics
	Debug   *debuglog.Manager
	// Store 是 SQLite 持久层：日志行查询/聚合与配额样本读写都走它；
	// nil 时停采、日志与配额端点降级为空。
	Store *store.Store
	// Tokens 是下游令牌仓；nil 时 api_token 登录与令牌端点不可用。
	Tokens *authtoken.Store
	// Models 是模型注册表仓；nil 时 /admin/model-registry 返回 503。
	Models *modelreg.Store
	// MaxConcurrencyFunc 返回 /v1 管线的全局并发上限运行时值（配置
	// reload 后为新值），投影到 runtime-metrics 的 max_concurrency；
	// nil 按 0（无限制）透出。
	MaxConcurrencyFunc func() int
	// Pool 是号池接口（遥测 + 动作口）；nil 视为无池——
	// gate/warm/detached/accounts 组缺席、配额采样退回单号匿名、
	// 排空跳过闸门冲刷、脱钩逐出与配额回灌静默跳过。
	Pool *PoolDeps
}

// New 按 Deps 创建面板处理器。
func New(d Deps) (*Handler, error) {
	tokenFunc := d.TokenFunc
	if tokenFunc == nil {
		tokenFunc = func() string { return "" }
	}
	up, err := newPanelUpstream(d.BaseURL, d.Proxy, d.ForceHTTP1, tokenFunc)
	if err != nil {
		return nil, err
	}
	h := &Handler{
		filePassword:       d.Password,
		passwordHash:       sha256.Sum256([]byte(d.Password)),
		passwordSet:        d.Password != "",
		tokenFunc:          tokenFunc,
		loginFailures:      make(map[string]*loginFail),
		metrics:            d.Metrics,
		debug:              d.Debug,
		store:              d.Store,
		tokens:             d.Tokens,
		models:             d.Models,
		maxConcurrencyFunc: d.MaxConcurrencyFunc,
		pool:               d.Pool,
		statsCache:         newStatsCache(),
		startedAt:          time.Now(),
	}
	h.quota = &quotaSampler{h: h}
	h.modelsCache = newTTLCache(catalogCacheTTL, false, h.fetchModels)
	h.providersCache = newTTLCache(catalogCacheTTL, false, h.fetchProviders)
	h.modelStatusesCache = newTTLCache(catalogCacheTTL, false, h.fetchModelStatuses)
	h.usageCache = newTTLCache(usageCacheTTL, true, func(ctx context.Context) (store.UsageSnapshot, error) {
		return h.store.UsageStats(ctx)
	})
	h.statusCache = newTTLCache(statusCacheTTL, false, func(ctx context.Context) (map[string]any, error) {
		// 610s 对齐原 adminStatus 语义：WithoutCancel 剥掉请求取消后，
		// 上游长思考/排队仍由这个上限兜底。
		fetchCtx, cancel := context.WithTimeout(ctx, 610*time.Second)
		defer cancel()
		return h.StatusReport(fetchCtx), nil
	})
	h.upstreamPtr.Store(up)
	h.refreshPasswordOverride()
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

// SetPassword 更新文件侧密码并重算生效凭据（配置 reload 热路径）：
// DB 覆盖存在时新文件值只做回落层，不夺回服役位。换密码的运维语义
// 是踢人——前端拿旧 Bearer 立即 401。
func (h *Handler) SetPassword(password string) {
	h.authMu.Lock()
	h.filePassword = password
	h.resolvePasswordLocked()
	h.authMu.Unlock()
}

// resolvePasswordLocked 按 overlay 语义重算生效凭据：DB 覆盖行存在
// 即压过文件值，无行回落 sha256(filePassword)。调用方须持 authMu。
func (h *Handler) resolvePasswordLocked() {
	if h.dbPasswordSet {
		h.passwordHash = h.dbPasswordHash
		h.passwordSet = true
		return
	}
	h.passwordHash = sha256.Sum256([]byte(h.filePassword))
	h.passwordSet = h.filePassword != ""
}

// refreshPasswordOverride 重读 runtime_state["dashboard.password_hash"]
// 刷新内存镜像。boot 与认证未命中时各调一次：后者让「sqlite3 删行/改行」
// 成为不重启的应急恢复通道（面板自身写行时内存已同步，成功路径不付
// 这趟 IO）。读失败/行非法一律保留现状——宁可按旧态服役也不让一次
// IO 抖动把管理员锁在门外。
func (h *Handler) refreshPasswordOverride() {
	if h.store == nil {
		return
	}
	raw, ok, err := h.store.GetState(context.Background(), dashboardPasswordHashKey)
	if err != nil {
		slog.Warn("ccpanel: read dashboard.password_hash failed, keeping current password state", "error", err)
		return
	}
	h.authMu.Lock()
	defer h.authMu.Unlock()
	if !ok {
		h.dbPasswordSet = false
		h.dbPasswordHash = [32]byte{}
		h.resolvePasswordLocked()
		return
	}
	sum, derr := hex.DecodeString(strings.TrimSpace(raw))
	if derr != nil || len(sum) != sha256.Size {
		slog.Warn("ccpanel: dashboard.password_hash malformed, keeping current password state")
		return
	}
	copy(h.dbPasswordHash[:], sum)
	h.dbPasswordSet = true
	h.resolvePasswordLocked()
}

// PasswordSource 返回生效密码来源：db=runtime_state 覆盖、file=config.yaml、
// open=两层皆空（开放面板）。provenance 投影用。
func (h *Handler) PasswordSource() string {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	switch {
	case h.dbPasswordSet:
		return "db"
	case h.filePassword != "":
		return "file"
	default:
		return "open"
	}
}

// passwordSnapshot 返回「是否有密码」与生效哈希的一致性快照。
func (h *Handler) passwordSnapshot() (bool, [32]byte) {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.passwordSet, h.passwordHash
}

// PoolDeps 是面板对号池的全部依赖：Snapshot 一次取齐 gate/warm/
// detached/逐账号状态/别名/逐号凭据源——读侧每请求一次求值，拿到的
// 是同一时间切面而非旧接口逐方法拼出的混合切面；TokenFuncs 给的是
// 活句柄，脱敏环与配额采样按需重读。三个动作口独立可缺席（nil 跳过）：
// EvictDetached 按来源调试目录逐出脱钩完成缓存条目（active-requests
// abort 在 Abort 返回 true 后补调，收口 abort-after-detach 残留窗），
// FlushGates 在排空起点以短 ctx 把各 lane 闸门窗口行重放缓冲做最后
// 一轮同步落库（best-effort），NoteQuota 把一次成功配额探测的日/周
// 剩余百分比回灌池侧降权簿记。
type PoolDeps struct {
	Snapshot      func() devin.PoolSnapshot
	EvictDetached func(dir string)
	FlushGates    func(ctx context.Context)
	NoteQuota     func(name string, dailyRemainingPct, weeklyRemainingPct float64)
}

// poolSnapshot 取一次号池遥测；无池返回零值与 false——各消费组按
// 「无池」缺席，与旧逐字段 nil-func 分支同语义。
func (h *Handler) poolSnapshot() (devin.PoolSnapshot, bool) {
	if h.pool == nil || h.pool.Snapshot == nil {
		return devin.PoolSnapshot{}, false
	}
	return h.pool.Snapshot(), true
}

// quotaSub 返回配额采样子系统；字面量构造的 Handler（测试绕开 New）
// 在首个配额调用点懒挂。
func (h *Handler) quotaSub() *quotaSampler {
	h.quotaOnce.Do(func() {
		if h.quota == nil {
			h.quota = &quotaSampler{h: h}
		}
	})
	return h.quota
}

// NoteUpstreamTokens 把一批已知上游凭据字面值登记进常驻脱敏集合
// （seedTokens）：装配层启动时用 config 声明的账号凭据与
// upstream_accounts 仓的存量行播种——重启前写入的旧调试目录里
// 的凭据字面量不能依赖「本进程见过」的 recentTokens 环兜底。
func (h *Handler) NoteUpstreamTokens(tokens ...string) {
	h.tokenMu.Lock()
	defer h.tokenMu.Unlock()
	if h.seedTokens == nil {
		h.seedTokens = make(map[string]struct{}, len(tokens))
	}
	for _, token := range tokens {
		if token != "" {
			h.seedTokens[token] = struct{}{}
		}
	}
}

// SeedCredentialMasks 给常驻脱敏集合播种：config 声明的账号 token 与
// upstream_accounts 仓的存量行——重启后 recentTokens 环是空的，旧调试
// 目录里的凭据字面值照样罩得住。行内 token 含脱敏哈希形态也无妨（明文
// 位不命中就不替换）。哪些字段承载可脱敏凭据的知识归脱敏集合的所有者。
func (h *Handler) SeedCredentialMasks(ctx context.Context, cfg *config.Config, db *store.Store) {
	var seeds []string
	for _, acc := range cfg.Devin.Accounts {
		seeds = append(seeds, acc.Token)
	}
	if rows, err := db.ListAccounts(ctx); err == nil {
		for _, row := range rows {
			seeds = append(seeds, row.Token)
		}
	} else {
		// 面板侧凭据失去脱敏登记——调试 payload 里这些 token 可能以明文
		// 露面。静默吞掉会让降级无迹可查，按惯例留 WARN。
		slog.Warn("debuglog: upstream account token seeds unavailable, panel credentials will not be masked", "error", err)
	}
	h.NoteUpstreamTokens(seeds...)
}

// SetConfigOps 注入配置自省与热重载操作面（/admin/config*）。
// 迟绑定：Reload 闭包回调面板自身（SetQuotaInterval/SetPassword），
// 装配层只能先建面板再挂操作面。
func (h *Handler) SetConfigOps(ops ConfigOps) {
	h.configOps = &ops
}

// SetAccountOps 注入 /admin/accounts 账号操作面（读写跨 store 行、
// config 声明集与 devinPool 热应用协调，实现由装配层提供）。
// 迟绑定：ops 的构造经 settingsStore，后者依赖面板的配额句柄。
func (h *Handler) SetAccountOps(ops accounts.AccountOps) {
	h.accountOps = &ops
}

// maxConcurrency 返回全局并发上限的运行时值；未注入 getter 时按 0
// （无限制）透出。
func (h *Handler) maxConcurrency() int {
	if h.maxConcurrencyFunc == nil {
		return 0
	}
	return h.maxConcurrencyFunc()
}

// SetSettingsStore 注入运行时设置键仓（/admin/settings 用）。
// 迟绑定：PanelSettings 构造依赖面板的 QuotaInterval/SetQuotaInterval
// 方法值，只能后挂。
func (h *Handler) SetSettingsStore(s *PanelSettings) {
	h.settings = s
}

// SetProbeHandler 注入应用根路由；模型探活在进程内 ServeHTTP，走与外部
// 请求完全相同的鉴权/准入/重定向/上游路径。迟绑定：根路由要等
// app.HTTPServer() 装配完才存在。
func (h *Handler) SetProbeHandler(handler http.Handler) {
	h.probeHandler = handler
}

// panelRoute 是路由表的一行：method+pattern 是 chi 挂载键，handler 是含
// 鉴权包裹的最终形态。docPath/doc 均非空时该端点进 /admin/api 自描述
// 目录——docPath 可与挂载 pattern 不同（目录里给带 query 提示的展示形）。
type panelRoute struct {
	method  string
	pattern string
	handler http.HandlerFunc
	docPath string
	doc     string
}

// routes 是面板路由的唯一事实表：Register 按它挂载，/admin/api 目录按它
// 生成——新增端点只登记一处，挂载集与自描述文档不会漂移。
// /admin 段按目录展示顺序排（status/accounts/logs 等排障链路在前）；
// /web、/login、/logout、/public 为公开路径（页面自身在浏览器侧做登录门），
// /dashboard、/admin 需 Bearer。
func (h *Handler) routes() []panelRoute {
	A := h.withAuth
	W := h.withWebAuth
	return []panelRoute{
		{http.MethodGet, "/web/*", h.serveStatic, "", ""},
		{http.MethodPost, "/login", h.handleLogin, "", ""},
		{http.MethodPost, "/logout", h.handleLogout, "", ""},
		{http.MethodGet, "/public/version", h.publicVersion, "", ""},
		{http.MethodGet, "/public/protocols", h.publicProtocols, "", ""},

		{http.MethodGet, "/dashboard/session", W(h.dashboardSession), "", ""},
		{http.MethodGet, "/dashboard/summary", W(h.dashboardSummary), "", ""},
		{http.MethodGet, "/dashboard/metrics", W(h.dashboardMetrics), "", ""},
		{http.MethodGet, "/dashboard/logs", W(h.dashboardLogs), "", ""},
		{http.MethodGet, "/dashboard/logs/bootstrap", W(h.dashboardLogsBootstrap), "", ""},
		{http.MethodGet, "/dashboard/stats", W(h.dashboardStats), "", ""},
		{http.MethodGet, "/dashboard/stats/filter-options", W(h.dashboardStatsFilterOptions), "", ""},
		{http.MethodGet, "/dashboard/models", W(h.dashboardModels), "", ""},

		{http.MethodGet, "/admin/status", A(h.adminStatus), "/admin/status",
			"账户/套餐/容量/渠道/模型状态告警 + devin.aliases 校验（alias_targets_absent 目标缺席 / alias_shadows_catalog 遮蔽真 uid）"},
		{http.MethodGet, "/admin/models", A(h.dashboardModels), "/admin/models",
			"模型目录含能力位与价格"},
		{http.MethodGet, "/admin/runtime-metrics", A(h.adminRuntimeMetrics), "/admin/runtime-metrics",
			"进程运行指标（RPM/QPS/goroutine/内存/GC/CPU）+ http.rejects 管线前拒绝（分原因计数+最近事件，不进索引）+ 日志管道自观测 + gate 速率闸门状态 + warm 前缀保温簿记"},
		{http.MethodGet, "/admin/runtime-metrics/history", A(h.adminRuntimeMetricsHistory), "/admin/runtime-metrics/history?minutes=",
			"进程指标历史环：30s 一拍的堆/RSS/goroutine/CPU/在途与累计吞吐/日志写积压/闩态（内存 480 点≈4h，重启归零；minutes 缺省 60 上限 240）"},
		{http.MethodGet, "/admin/accounts", A(h.adminAccounts), "/admin/accounts",
			"号池账号聚合视图：source(config|panel|tombstoned)+credential+disabled+token_sha+priority/max_rpm/notes+lane/gate/warm 快照+inflight+quota 摘要+usage{rpm_now,tps_now,ttfb_avg,ttfb_p50,ttfb_p90,cache_rate,today{requests,success_rate,tokens}}"},
		{http.MethodPost, "/admin/accounts", A(h.adminCreateAccount), "/admin/accounts",
			"建号 {name, token?|credentials_file?|credentials_content?, disabled?, verify?, priority?, max_rpm?, notes?}；verify 先探测凭据失败 400 不建行；file 与 content 互斥；整表校验失败 400，重名/墓碑名 409"},
		{http.MethodPut, "/admin/accounts/{name}", A(h.adminUpdateAccount), "/admin/accounts/{name}",
			"改凭据/停启用/元数据 {token?,credentials_file?,credentials_content?,disabled?,priority?,max_rpm?,notes?} 指针语义；file 与 content 互斥；config 名首写自动建覆盖行；tombstoned 409 须先 restore"},
		{http.MethodDelete, "/admin/accounts/{name}", A(h.adminDeleteAccount), "/admin/accounts/{name}",
			"config 名置墓碑（可 restore，覆盖保留复活）；panel 名物理删"},
		{http.MethodPost, "/admin/accounts/{name}/restore", A(h.adminRestoreAccount), "/admin/accounts/{name}/restore",
			"墓碑还活：deleted=0 重推回池；活号 409"},
		{http.MethodPost, "/admin/accounts/{name}/clear-cooldown", A(h.adminClearAccountCooldown), "/admin/accounts/{name}/clear-cooldown",
			"清池侧两档冷却（auth+unhealthy）立即回候选；不动 gate 闩与 last_failure 证据"},
		{http.MethodPost, "/admin/accounts/{name}/quota/refresh", A(h.adminRefreshAccountQuota), "/admin/accounts/{name}/quota/refresh",
			"即采一次该号配额（不经 lane；disabled 可刷 tombstoned 404）；502 上游失败"},
		{http.MethodPost, "/admin/accounts/{name}/test", A(h.adminTestAccount), "/admin/accounts/{name}/test",
			"凭据连通性探测：恒 200 {ok,latency_ms,user?,plan?,error?}；成功顺带配额信号回灌+清冷却；名不在生效集/tombstoned/凭据不可解 404"},
		{http.MethodGet, "/admin/accounts/cli-credentials", A(h.adminCLICredentials), "/admin/accounts/cli-credentials",
			"Devin CLI 凭证发现链探针 {available,path,parsable,suggested_name}；不回传内容"},
		{http.MethodGet, "/admin/accounts/export", A(h.adminExportAccounts), "/admin/accounts/export?format=yaml|json",
			"生效账号集（非墓碑）整批导出为可移植文件：yaml 默认、?format=json；credentials_file 型内联成 credentials_content；tombstoned 不导"},
		{http.MethodPost, "/admin/accounts/import", A(h.adminImportAccounts), "/admin/accounts/import",
			"批量 upsert：body 是 {accounts:[...]} 或裸列表（yaml/json 皆可，形状同 export）；每条是该名期望全态——新名建行/config 名建覆盖行/墓碑名复活；任一非法整批 400 不落库"},
		{http.MethodGet, "/admin/config", A(h.adminConfigCurrent), "/admin/config",
			"脱敏后的生效配置视图（token/api_key/password 以 sha256 前缀代替）；stale=true 表示文件在最后一次加载后被修改"},
		{http.MethodPost, "/admin/config/reload", A(h.adminConfigReload), "/admin/config/reload",
			"重读 config.yaml 并热应用；返回 applied/requires_restart 两组字段名；校验失败 422 旧配置继续服役"},
		{http.MethodGet, "/admin/usage", A(h.adminUsage), "/admin/usage",
			"logs 表聚合：今日/窗口累计、model_days 模型×日矩阵、按模型/按 key、错误阶段、10 分钟粒度趋势、p50/p95/p99、目录价估算成本"},
		{http.MethodGet, "/admin/logs", A(h.dashboardLogs), "/admin/logs?limit=&offset=&q=&status=&status_class=&result=&model=&error_stage=&since=&until=",
			"最近请求（新在前）；q 子串（含 error_message）或结构化过滤；status 表达式 499/!200/>=400/4xx 逗号 OR；since/until 钉时间窗；has_more 提示尾部窗外仍有更早历史，rejects 附管线前拒绝环（401/429 不进索引）"},
		{http.MethodGet, "/admin/logs/matrix", A(h.adminLogsMatrix), "/admin/logs/matrix?since=",
			"健康矩阵紧凑条目：只投影分桶与归因所需字段，不分页（扫描上限 2000）；truncated 为真表示 since 窗口覆盖不完整"},
		{http.MethodGet, "/admin/logs/export", A(h.adminLogsExport), "/admin/logs/export?format=json|csv&筛选参数同上",
			"导出筛选后的请求摘要（CSV 或 JSON 数组）；触及扫描上限带 X-Truncated: true"},
		{http.MethodGet, "/admin/logs/bootstrap", A(h.dashboardLogsBootstrap), "/admin/logs/bootstrap",
			"日志页筛选初始化：模型清单、状态码观察值等一次拉齐"},
		{http.MethodGet, "/admin/stats", A(h.dashboardStats), "/admin/stats?range=",
			"面板统计聚合（rpm_stats/按模型/按令牌用量等，dashboardStats 同形）"},
		{http.MethodGet, "/admin/stats/filter-options", A(h.dashboardStatsFilterOptions), "/admin/stats/filter-options",
			"stats 页筛选项候选（模型名等）"},
		{http.MethodGet, "/admin/metrics", A(h.dashboardMetrics), "/admin/metrics",
			"dashboardMetrics 同形：概要计数与速率"},
		{http.MethodGet, "/admin/active-requests", A(h.adminActiveRequests), "/admin/active-requests",
			"进行中请求活快照：阶段状态、模型、已下发字节、已写文件、丢弃数"},
		{http.MethodGet, "/admin/active-requests/{id}/debug-log", A(h.adminActiveRequestDebugLog), "/admin/active-requests/{id}/debug-log",
			"进行中请求的调试投影（目录已建即按 debug-logs/{id} 口径投影）"},
		{http.MethodGet, "/admin/debug-logs/{id}", A(h.adminDebugLog), "/admin/debug-logs/{id}",
			"单请求 meta.json + 文件清单；id 是日志行自增 id（迁移前的 started_at 毫秒戳链接仍可解析）"},
		{http.MethodGet, "/admin/debug-logs/{id}/merged", A(h.adminDebugLogMerged), "/admin/debug-logs/{id}/merged",
			"把 06-http-response.jsonl 的 SSE 帧合并成可读的最终响应（reasoning/content/tools）"},
		{http.MethodPost, "/admin/debug-logs/merged-response", A(h.adminMergedResponse), "/admin/debug-logs/merged-response",
			"上传体合并版：body {\"resp_body\"}（前端可 gzip），与 GET merged 共用同一合并器"},
		{http.MethodGet, "/admin/debug-logs/{id}/file/*", A(h.adminDebugLogFile), "/admin/debug-logs/{id}/file/{name}",
			"读取请求目录内文件（顶层或 attachments/），超 4MB 截断；?raw=1 原样回字节（CSP sandbox + nosniff）"},
		{http.MethodPost, "/admin/active-requests/{id}/abort", A(h.adminAbortActiveRequest), "/admin/active-requests/{id}/abort",
			"中断进行中请求（取消 ctx）；无活跃请求时 404"},
		{http.MethodGet, "/admin/process-log", A(h.adminProcessLog), "/admin/process-log?offset=",
			"进程 stderr 日志尾部；offset>0 增量拉取，响应带 next_offset"},
		{http.MethodGet, "/admin/quota", A(h.adminQuota), "/admin/quota",
			"配额历史快照（quota_samples 表）+ 按燃烧速率外推的耗尽时间"},
		{http.MethodGet, "/admin/settings", A(h.adminListSettings), "/admin/settings",
			"运行时设置全表：键、当前值、默认、是否有面板覆盖"},
		{http.MethodGet, "/admin/settings/{key}", A(h.adminGetSetting), "/admin/settings/{key}",
			"单个运行时设置（含覆盖来源标记）"},
		{http.MethodPut, "/admin/settings/{key}", A(h.adminUpdateSetting), "/admin/settings/{key}",
			"运行时设置覆盖（debug.enabled/保留策略等），body {\"value\": \"...\"}；对 config.yaml 恒赢"},
		{http.MethodPost, "/admin/settings/{key}/reset", A(h.adminResetSetting), "/admin/settings/{key}/reset",
			"删除该键的面板覆盖，回落 config.yaml/默认值"},
		{http.MethodPost, "/admin/settings/batch", A(h.adminBatchUpdateSettings), "/admin/settings/batch",
			"批量设置覆盖，body {\"key\": \"value\", ...}"},
		{http.MethodPut, "/admin/dashboard/password", A(h.adminUpdateDashboardPassword), "/admin/dashboard/password",
			"面板密码轮换（dashboard.password 的 DB 覆盖层）：body {\"password\"} 非空→sha256 入 runtime_state 压过文件值并回 token 续会话；空→清覆盖回落文件值（应急找回层）"},
		{http.MethodGet, "/admin/auth-tokens", A(h.adminListAuthTokens), "/admin/auth-tokens?range=",
			"下游令牌表 + range 内时间窗聚合统计（覆盖累计字段）；行含 anonymous 标记匿名通道"},
		{http.MethodPost, "/admin/auth-tokens", A(h.adminCreateAuthToken), "/admin/auth-tokens",
			"创建下游令牌（并发槽/RPM/5h|日|周|月费用窗口/模型白名单），明文仅此一次返回；anonymous=true 建匿名通道行（无凭据准入，不返回明文）"},
		{http.MethodPut, "/admin/auth-tokens/{id}", A(h.adminUpdateAuthToken), "/admin/auth-tokens/{id}",
			"更新令牌（启用/各窗口限额/max_rpm/白名单等）"},
		{http.MethodDelete, "/admin/auth-tokens/{id}", A(h.adminDeleteAuthToken), "/admin/auth-tokens/{id}",
			"删除令牌（幂等）"},
		{http.MethodGet, "/admin/model-registry", A(h.adminModelRegistry), "/admin/model-registry",
			"模型注册表：启用/停用、redirect_model（别名解析前改写）、覆盖标记；各行 catalog 字段透出目录价"},
		{http.MethodPut, "/admin/model-registry", A(h.adminPutModel), "/admin/model-registry",
			"写注册条目（启用/禁用/redirect_model）"},
		{http.MethodDelete, "/admin/model-registry", A(h.adminDeleteModel), "/admin/model-registry",
			"删注册条目"},
		{http.MethodGet, "/admin/model-pricing", A(h.adminModelPricing), "/admin/model-pricing?model=",
			"单模型目录价投影（found=false 表示无目录价）"},
		{http.MethodPost, "/admin/model-test", A(h.adminModelTest), "/admin/model-test",
			"模型连通性探针（结果记 log_source=manual_test 的日志行）"},
		{http.MethodPost, "/admin/model-chat", A(h.adminModelChat), "/admin/model-chat",
			"面板内对话式模型测试（同 manual_test 归因）"},
		{http.MethodPost, "/admin/update/check", A(h.adminUpdateCheck), "/admin/update/check?force=1",
			"解析 GitHub 最新 release tag 对比当前版本（20min 进程内缓存，?force=1 绕过）"},
		{http.MethodGet, "/admin/update/status", A(h.adminUpdateStatus), "/admin/update/status",
			"自更新状态：supported/unit/current/rollback_available + 在途记录 {phase,from,to,error,since,pid}（重启窗口内旧/编排/新实例应答同一 db 记录）"},
		{http.MethodPost, "/admin/update", A(h.adminUpdateStart), "/admin/update",
			"零停机自更新：body {\"tag\"} 可省取最新 release；202 后台推进（下载→sha256 校验→编排进程换名+接管+重启），轮询 /admin/update/status；在途 409、非托管/Windows 501"},
		{http.MethodPost, "/admin/update/rollback", A(h.adminUpdateRollback), "/admin/update/rollback",
			"用 <bindir>/devin-2api.backup 做对称换回（免下载，.backup 换成被替换版）；缺席 404、在途 409"},
		{http.MethodGet, "/admin/api", A(h.adminAPIIndex), "", ""},
	}
}

// Register 把 routes 表挂到 mux（method→mux 动词映射直译）。
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
	Put(pattern string, handlerFn http.HandlerFunc)
	Patch(pattern string, handlerFn http.HandlerFunc)
	Delete(pattern string, handlerFn http.HandlerFunc)
}) {
	mount := map[string]func(string, http.HandlerFunc){
		http.MethodGet:    mux.Get,
		http.MethodPost:   mux.Post,
		http.MethodPut:    mux.Put,
		http.MethodPatch:  mux.Patch,
		http.MethodDelete: mux.Delete,
	}
	for _, rt := range h.routes() {
		mount[rt.method](rt.pattern, rt.handler)
	}
}
