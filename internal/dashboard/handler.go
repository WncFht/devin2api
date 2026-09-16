// Package dashboard 实现管理面板：模型列表、价格筛选、账户用量。
// password 为空时无需登录直接进入；非空时走 session cookie。
package dashboard

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/randid"
	"github.com/WncFht/devin2api/internal/upstream"

	"local/devinproto/devinprotoconnect"
)

// Handler 是面板 HTTP 处理器。
type Handler struct {
	// authMu 保护 password/passwordHash：配置 reload 会运行时换值。
	authMu   sync.RWMutex
	password string
	// passwordHash 是面板密码的 SHA-256：比较走定长哈希，既不向
	// ConstantTimeCompare 泄漏长度，也与 apiKeyMiddleware 的口径一致。
	passwordHash [32]byte
	baseURL      string
	// tokenFunc 每次求值返回当前上游凭据——adapter 的 unauthenticated
	// 自愈更新 token 后面板跟随新值，不缓存启动时的静态快照。
	tokenFunc     func() string
	apiClient     devinprotoconnect.ApiServerServiceClient
	baseTransport http.RoundTripper
	sessionMu     sync.RWMutex
	sessionTokens map[string]time.Time
	// recentTokens 是最近见过的上游凭据（token 自愈轮换会换新）：
	// maskToken 按这个集合脱敏，旧请求目录里的历史 token 字面值也罩住。
	tokenMu      sync.Mutex
	recentTokens []string
	// loginFailures 按客户端 IP 记录连续登录失败与锁定期——面板是
	// 唯一持密码的端点，爆破代价要抬高。
	loginFailures map[string]*loginFail

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

	// metrics 是 HTTP 代理运行计数器快照源；nil 时 stats 端点只返回日志侧数据。
	metrics *obs.Metrics
	// debugManager 暴露日志管道自身指标（丢弃数、活跃目录数）。
	debugManager *debuglog.Manager
	// version 是运行中二进制的构建版本，由 main 经 SetVersion 注入。
	version string
	// gateStats 返回速率闸门快照；nil 时 stats 不输出 gate 段。
	gateStats func() devin.GateStats
	// aliasesFunc 返回当前生效的模型别名映射；nil 时 status 不做缺席校验。
	aliasesFunc func() map[string]string
	// configOps 挂配置自省与热重载端点；nil 时两个端点 404。
	configOps *ConfigOps
}

// ConfigOps 是面板配置端点的操作面：Reload 重读并热应用配置文件，
// Current 返回脱敏后的生效配置视图。实现由装配层（main）提供——
// 热应用要跨 adapter/应用/日志管理器多方协调，不属于面板自身职责。
type ConfigOps struct {
	Reload  func() (*ConfigReloadReport, error)
	Current func() map[string]any
}

// ConfigReloadReport 是一次热重载的结果：applied 是已生效的变更字段，
// requiresRestart 是改了但要重启才生效的字段（listen/transport 固化项）。
type ConfigReloadReport struct {
	At              string   `json:"at"`
	Applied         []string `json:"applied"`
	RequiresRestart []string `json:"requires_restart,omitempty"`
}

// New 创建面板处理器。password 为空表示开放访问。proxy 为可选代理地址。
// forceHTTP1 为 true 时强制 HTTP/1.1，与 adapter 保持一致的连接模型。
// tokenFunc 每次求值返回当前上游凭据（与 adapter 的自愈共用同一来源）；
// nil 视为恒空凭据。metrics/debugManager 允许为 nil（对应功能未启用）。
func New(password, baseURL string, tokenFunc func() string, proxy string, forceHTTP1 bool, metrics *obs.Metrics, debugManager *debuglog.Manager) (*Handler, error) {
	if tokenFunc == nil {
		tokenFunc = func() string { return "" }
	}
	base, err := httpproxy.NewTransport(proxy, forceHTTP1)
	if err != nil {
		return nil, fmt.Errorf("proxy transport: %w", err)
	}
	transport := upstream.NewBasicAuthTransportFunc(base, tokenFunc)
	// 面板可能遇到上游长时思考/排队，超时与 ResponseHeaderTimeout 对齐。
	httpClient := &http.Client{Transport: transport, Timeout: 610 * time.Second}
	// baseURL 归一化一次，Connect 客户端与 fetchUserStatus 用同一形态——
	// 尾随斜杠的 devin.base_url 会让 Connect 调用路径出 "//"。
	trimmedURL := strings.TrimRight(baseURL, "/")
	return &Handler{
		password:      password,
		passwordHash:  sha256.Sum256([]byte(password)),
		baseURL:       trimmedURL,
		tokenFunc:     tokenFunc,
		apiClient:     devinprotoconnect.NewApiServerServiceClient(httpClient, trimmedURL, connect.WithSendGzip()),
		baseTransport: base,
		sessionTokens: make(map[string]time.Time),
		loginFailures: make(map[string]*loginFail),
		cacheTTL:      5 * time.Minute,
		metrics:       metrics,
		debugManager:  debugManager,
	}, nil
}

// SetVersion 记录构建版本，stats/index 端点透出，供排障辨认运行中的二进制。
func (h *Handler) SetVersion(version string) {
	h.version = version
}

// SetPassword 运行时更换面板密码（配置 reload 热路径）。换密码的运维
// 语义是踢人——旧密码签出的会话一并吊销，否则最长还能挂 24h。
func (h *Handler) SetPassword(password string) {
	h.authMu.Lock()
	h.password = password
	h.passwordHash = sha256.Sum256([]byte(password))
	h.authMu.Unlock()
	h.sessionMu.Lock()
	h.sessionTokens = make(map[string]time.Time)
	h.sessionMu.Unlock()
}

// SetGateStats 注入速率闸门快照源。
func (h *Handler) SetGateStats(fn func() devin.GateStats) {
	h.gateStats = fn
}

// SetAliasesFunc 注入当前别名映射源，供 status 端点做目录缺席校验。
func (h *Handler) SetAliasesFunc(fn func() map[string]string) {
	h.aliasesFunc = fn
}

// SetConfigOps 注入配置自省与热重载操作面。
func (h *Handler) SetConfigOps(ops ConfigOps) {
	h.configOps = &ops
}

// passwordSnapshot 返回密码与哈希的一致性快照。
func (h *Handler) passwordSnapshot() (string, [32]byte) {
	h.authMu.RLock()
	defer h.authMu.RUnlock()
	return h.password, h.passwordHash
}

// Register 将面板路由注册到 mux。有 token 即可启用；密码仅控制是否登录。
// mux 形参与 app.DashboardRegistrar 的契约一致（本面板只用 Get/Post）。
func (h *Handler) Register(mux interface {
	Get(pattern string, handlerFn http.HandlerFunc)
	Post(pattern string, handlerFn http.HandlerFunc)
	Put(pattern string, handlerFn http.HandlerFunc)
	Patch(pattern string, handlerFn http.HandlerFunc)
	Delete(pattern string, handlerFn http.HandlerFunc)
}) {
	mux.Get("/panel", h.servePanel)
	mux.Post("/panel/login", h.handleLogin)
	mux.Get("/panel/static/*", h.serveStatic)
	mux.Get("/panel/api", h.apiIndex)
	mux.Get("/panel/api/status", h.apiStatus)
	mux.Get("/panel/api/models", h.apiModels)
	mux.Get("/panel/api/stats", h.apiStats)
	mux.Get("/panel/api/requests", h.apiRequests)
	mux.Get("/panel/api/requests/matrix", h.apiRequestMatrix)
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
	mux.Get("/panel/api/config", h.apiConfigCurrent)
	mux.Post("/panel/api/config/reload", h.apiConfigReload)
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
		// /stats 走 1Hz 轮询且前端只读 ttfb/duration 两行——全量
		// UsageStats 快照（数百 KB JSON + 全桶排序）留给 /usage。
		payload["usage"] = h.debugManager.UsageLatency()
	}
	if h.gateStats != nil {
		payload["gate"] = h.gateStats()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// apiConfigCurrent 返回脱敏后的生效配置视图（文件键名与 config.yaml 一致，
// token/api_key/password 以 sha256 前缀代替明文）。实现见 main 的装配。
func (h *Handler) apiConfigCurrent(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	if h.configOps == nil || h.configOps.Current == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.configOps.Current())
}

// apiConfigReload 重读配置文件并热应用；校验失败 422 且旧配置继续服役。
// 返回 applied（已生效）与 requires_restart（要重启才生效）两组字段名，
// 让调用方明确知道哪些改动仍在 pending。
func (h *Handler) apiConfigReload(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	if h.configOps == nil || h.configOps.Reload == nil {
		http.NotFound(w, r)
		return
	}
	report, err := h.configOps.Reload()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
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
			{"method": "GET", "path": "/panel/api/status", "description": "账户/套餐/容量/渠道/模型状态告警 + devin.aliases 校验（alias_targets_absent 目标缺席 / alias_shadows_catalog 遮蔽真 uid）"},
			{"method": "GET", "path": "/panel/api/models", "description": "模型目录含能力位与价格"},
			{"method": "GET", "path": "/panel/api/stats", "description": "进程运行指标（RPM/QPS/goroutine/内存/GC/CPU）+ 60 分钟逐 30 秒趋势 + http.rejects 管线前拒绝（分原因计数+最近事件，不进索引）+ 日志管道自观测 + index 聚合用量 + gate 速率闸门状态（闩态/计数/令牌/排队 + events 闩迁移事件环）"},
			{"method": "GET", "path": "/panel/api/config", "description": "脱敏后的生效配置视图（token/api_key/password 以 sha256 前缀代替）；stale=true 表示文件在最后一次加载后被修改"},
			{"method": "POST", "path": "/panel/api/config/reload", "description": "重读 config.yaml 并热应用；返回 applied/requires_restart 两组字段名；校验失败 422 旧配置继续服役"},
			{"method": "GET", "path": "/panel/api/usage", "description": "index.jsonl 聚合：今日/窗口累计、model_days 模型×日矩阵（供面板时间范围选择器）、按模型/按 key、错误阶段、8 天 10 分钟粒度趋势（含缓存命中率与均速原料）、p50/p95/p99、目录价估算成本"},
			{"method": "GET", "path": "/panel/api/requests?limit=&offset=&q=&status=&status_class=&result=&model=&error_stage=&since=&until=", "description": "最近请求（新在前）；q 子串或结构化过滤；status 表达式 499/!200/>=400/4xx 逗号 OR；since/until 钉时间窗；has_more 提示窗口外仍有历史；rejects 捎带管线前拒绝事件环（不进索引）"},
			{"method": "GET", "path": "/panel/api/requests/matrix?since=", "description": "健康矩阵紧凑条目：只投影分桶与归因所需字段，不分页（扫描上限 2000）；truncated 为真表示 since 窗口覆盖不完整"},
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
	password, _ := h.passwordSnapshot()
	// ?v= 版本戳让 HTML 引用的资源随发版必然换新 URL；模块 import 走
	// serveStatic 的 ETag 条件请求，覆盖 import 链注不进版本戳的部分。
	v := h.version
	if v == "" {
		v = "dev"
	}
	if authed, _ := h.isAuthenticated(r); password != "" && !authed {
		_, _ = w.Write([]byte(strings.ReplaceAll(loginPage, "__VERSION__", v)))
		return
	}
	_, _ = w.Write([]byte(strings.ReplaceAll(dashboardPage, "__VERSION__", v)))
}

// staticEntry 缓存资源名 → {etag, gzip 预压缩体}：内容随二进制固定，
// 按名惰性算一次。ETag+no-cache 代替 ?v= 版本戳之外给模块导入（静态路径
// 注不进 ?v=）提供一致性保证——条件请求 304 使再验证零成本。
// gzip 只服务 ≥1KB 的文本资源：echarts 630KB 经 tailnet 远程访问面板时
// 体积差距明显；小于阈值时压缩头开销比省的字节还多。
type staticEntry struct {
	etag string
	gz   []byte // nil 表示不值得压缩
}

var staticEntries sync.Map

func gzipBody(body []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(body); err != nil {
		return nil
	}
	if err := gw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
}

// serveStatic 下发 static/ 内嵌的前端资源；内容随二进制固定。
// private：响应需鉴权，不允许共享缓存存储；no-cache+ETag：每次加载再验证。
// 例外是 panel.css：登录页与它共享同一套设计令牌，设了密码（登录页
// 唯一会出现的场景）时若拦它，登录页会裸成浏览器默认样式。
func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(strings.TrimPrefix(chi.URLParam(r, "*"), "/"))
	if name == "." || strings.HasPrefix(name, "..") || strings.HasPrefix(name, "/") {
		http.NotFound(w, r)
		return
	}
	if name != "panel.css" && !h.requireAuth(w, r) {
		return
	}
	body, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	entryV, ok := staticEntries.Load(name)
	entry, _ := entryV.(*staticEntry)
	if !ok {
		sum := sha256.Sum256(body)
		entry = &staticEntry{etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
		if len(body) >= 1024 {
			entry.gz = gzipBody(body)
		}
		staticEntries.Store(name, entry)
	}
	w.Header().Set("Content-Type", staticContentType(name))
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", entry.etag)
	// 响应体随客户端 Accept-Encoding 变体——无论 200 还是 304 都要声明。
	w.Header().Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if entry.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(entry.gz)
		return
	}
	_, _ = w.Write(body)
}

// staticContentType 按扩展名给面板资源定 MIME；未识别类型按二进制流下发。
func staticContentType(name string) string {
	switch path.Ext(name) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// loginFail 记录单个来源 IP 的连续登录失败状态。
type loginFail struct {
	// fails 是上次锁定以来的连续失败次数。
	fails int
	// lockedUntil 是锁定截止时间；到期前失败的请求直接 429。
	lockedUntil time.Time
	// lastSeen 是最近一次失败时刻：闲置超过一个锁定周期的条目计数
	// 清零并可被机会清扫——爆破流量不走成功路径也能被回收。
	lastSeen time.Time
}

const (
	// loginMaxFails 是触发锁定的连续失败次数。
	loginMaxFails = 5
	// loginLockout 是达到失败上限后的锁定时长。
	loginLockout = 10 * time.Minute
	// sessionSweepThreshold 是触发机会清扫的 session 数量水位。
	sessionSweepThreshold = 64
)

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	// 所有分支都回 JSON——错误路径此前漏设 Content-Type。
	w.Header().Set("Content-Type", "application/json")
	password, passwordHash := h.passwordSnapshot()
	if password == "" {
		_, _ = w.Write([]byte(`{"ok":true,"open":true}`))
		return
	}
	ip := remoteIP(r)
	provided := sha256.Sum256([]byte(r.FormValue("password")))
	if subtle.ConstantTimeCompare(provided[:], passwordHash[:]) != 1 {
		if h.noteLoginFailure(ip) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"登录尝试过多，请稍后再试"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"密码错误"}`))
		return
	}
	// 密码正确时即使 IP 在锁定期内也放行并清零——锁定只为抬高爆破
	// 代价，真用户记对密码不应被挡在门外。
	h.sessionMu.Lock()
	delete(h.loginFailures, ip)
	now := time.Now()
	if len(h.sessionTokens) > sessionSweepThreshold {
		// 机会清扫：session 只增不扫会缓慢累积，登录是低频事件，
		// 顺手把过期条目与失效失败记录清掉。
		for id, expiry := range h.sessionTokens {
			if now.After(expiry) {
				delete(h.sessionTokens, id)
			}
		}
		h.sweepLoginFailures(now)
	}
	sessionID, err := randid.Hex(32)
	if err != nil {
		h.sessionMu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"会话创建失败"}`))
		return
	}
	h.sessionTokens[sessionID] = now.Add(24 * time.Hour)
	h.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "devin_panel_session",
		Value:    sessionID,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   86400,
		// Lax 挡住跨站 POST 登录/请求携带 cookie 的 CSRF 面，
		// 同站导航不受影响。
		SameSite: http.SameSiteLaxMode,
	})
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// sweepLoginFailures 清掉锁定已过期且闲置超过一个锁定周期的失败条目。
// 仍在锁定中或近期仍有失败活动的条目保留。调用方须持有 sessionMu。
func (h *Handler) sweepLoginFailures(now time.Time) {
	for key, state := range h.loginFailures {
		if !now.Before(state.lockedUntil) && now.Sub(state.lastSeen) > loginLockout {
			delete(h.loginFailures, key)
		}
	}
}

// noteLoginFailure 把一次密码校验失败计入 IP 账本（表单登录与 Bearer
// 认证共用），返回该 IP 当前是否处于锁定期；顺带按水位机会清扫过期
// 条目——纯爆破流量不走成功路径，失败条目只增不扫会无界增长。
func (h *Handler) noteLoginFailure(ip string) bool {
	h.sessionMu.Lock()
	defer h.sessionMu.Unlock()
	now := time.Now()
	state := h.loginFailures[ip]
	if state == nil {
		state = &loginFail{}
		h.loginFailures[ip] = state
	}
	if now.Before(state.lockedUntil) {
		state.lastSeen = now
		return true
	}
	// 距上次失败超过一个锁定周期视为新一波尝试：陈旧计数跨时间
	// 累积会把低频手滑误算成爆破。
	if now.Sub(state.lastSeen) > loginLockout {
		state.fails = 0
	}
	state.lastSeen = now
	state.fails++
	if state.fails >= loginMaxFails {
		state.fails = 0
		state.lockedUntil = now.Add(loginLockout)
	}
	if len(h.loginFailures) > sessionSweepThreshold {
		h.sweepLoginFailures(now)
	}
	return false
}

// isAuthenticated 判定请求是否已认证；locked 报告来源 IP 是否处于登录
// 锁定期——Bearer 失败与表单登录共用同一 IP 账本，只守 login 端点等于
// 把全速穷举通道留给 Bearer；authed 为真时 locked 无意义（锁定只抬高
// 爆破代价，持有有效会话/正确凭据的真用户不被挡）。
func (h *Handler) isAuthenticated(r *http.Request) (authed, locked bool) {
	password, passwordHash := h.passwordSnapshot()
	if password == "" {
		return true, false
	}
	// Agent 友好：除 session cookie 外，允许直接用 Bearer 密码访问 API，
	// 省去先登录拿 cookie 的交互步骤（curl -H 'Authorization: Bearer <密码>'）。
	if auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		var bearerOK bool
		bearerOK, locked = h.checkPasswordCredential(auth, passwordHash, remoteIP(r))
		if bearerOK {
			return true, false
		}
	}
	cookie, err := r.Cookie("devin_panel_session")
	if err != nil {
		return false, locked
	}
	h.sessionMu.RLock()
	expiry, ok := h.sessionTokens[cookie.Value]
	h.sessionMu.RUnlock()
	if !ok {
		return false, locked
	}
	if time.Now().After(expiry) {
		h.sessionMu.Lock()
		delete(h.sessionTokens, cookie.Value)
		h.sessionMu.Unlock()
		return false, locked
	}
	return true, false
}

// checkPasswordCredential 校验单份密码凭据（Bearer 头或登录表单的明文），
// 成功清该 IP 的失败账本，失败计入账本并报告是否已进入锁定期。
// isAuthenticated 的 Bearer 分支与移植面板的 CheckPanel* 共用同一口径。
func (h *Handler) checkPasswordCredential(provided string, passwordHash [32]byte, ip string) (ok, locked bool) {
	sum := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(sum[:], passwordHash[:]) == 1 {
		// 与表单登录同口径：正确凭据清掉该 IP 的失败账本。
		h.sessionMu.RLock()
		_, hasEntry := h.loginFailures[ip]
		h.sessionMu.RUnlock()
		if hasEntry {
			h.sessionMu.Lock()
			delete(h.loginFailures, ip)
			h.sessionMu.Unlock()
		}
		return true, false
	}
	return false, h.noteLoginFailure(ip)
}

// CheckPanelBearer 校验 Authorization: Bearer 头中的密码凭据，供移植面板
// （ccLoad 契约的 /admin、/dashboard 路由）复用同一密码与同一 IP 爆破账本；
// 不发 cookie、不查 session 表——移植前端把密码本身当 Bearer token 用。
func (h *Handler) CheckPanelBearer(r *http.Request) (authed, locked bool) {
	password, passwordHash := h.passwordSnapshot()
	if password == "" {
		return true, false
	}
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false, false
	}
	return h.checkPasswordCredential(auth, passwordHash, remoteIP(r))
}

// CheckPanelPassword 校验登录表单提交的明文密码（移植面板 /login 用）；
// 语义同 handleLogin 的密码分支：正确密码在锁定期内也放行并清账本。
func (h *Handler) CheckPanelPassword(pw string, r *http.Request) (ok, locked bool) {
	password, passwordHash := h.passwordSnapshot()
	if password == "" {
		return true, false
	}
	return h.checkPasswordCredential(pw, passwordHash, remoteIP(r))
}

func (h *Handler) requireAuth(w http.ResponseWriter, r *http.Request) bool {
	authed, locked := h.isAuthenticated(r)
	if authed {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	if locked {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"登录尝试过多，请稍后再试"}`))
		return false
	}
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"未授权"}`))
	return false
}
