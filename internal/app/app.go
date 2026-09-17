// 本文件定义 HTTP 应用、chi 路由和供应商适配器的串联逻辑。
//
// Package app 负责组装 HTTP 路由并连接 API 编解码与供应商适配器。
package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/modelreg"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/randid"
)

// PanelRegistrar 描述面板路由注册所需的最小能力。
type PanelRegistrar interface {
	Register(mux interface {
		Get(pattern string, handlerFn http.HandlerFunc)
		Post(pattern string, handlerFn http.HandlerFunc)
		Put(pattern string, handlerFn http.HandlerFunc)
		Patch(pattern string, handlerFn http.HandlerFunc)
		Delete(pattern string, handlerFn http.HandlerFunc)
	})
}

const (
	// readHeaderTimeout 是防止慢速请求头连接长期占用资源的内部策略。
	readHeaderTimeout = 60 * time.Second
	// readTimeout 限制请求体读取总时长，防止慢速客户端长期占用连接。
	readTimeout = 120 * time.Second
	// writeTimeout 是响应级绝对 deadline，会无差别砍断超过时长的正常 SSE
	// 长流（长 thinking + 长输出可超过 30 分钟）。流的生命周期由应用层
	// 更精确的机制管理：上游静默看门狗（120s）、SSE 保活、客户端 ctx 取消；
	// 慢读客户端的背压挂起由并发槽上限兜底。故不设写超时。
	writeTimeout = 0
	// idleTimeout 是 keep-alive 连接两次请求之间的内部空闲策略。
	idleTimeout = 360 * time.Second
	// defaultMaxConcurrency 是默认同时处理的 /v1/* 请求数上限。
	defaultMaxConcurrency = 1024
)

// App 保存 HTTP 应用依赖和服务配置。
type App struct {
	// adapter 是供应商无关请求与上游协议之间的适配器。
	adapter adapter.Adapter
	// serverConfig 是 HTTP 服务运行配置。
	serverConfig config.ServerConfig
	// debugManager 为每次兼容 API 请求创建独立的写盘日志。
	debugManager *debuglog.Manager
	// ccPanel 是可选的管理面板（ccLoad 契约）处理器；nil 表示不启用面板。
	ccPanel PanelRegistrar
	// apiKey 是 auth.api_key 的运行时值：不再是 /v1 准入旁路——启动与
	// reload 时它作为种子写成普通令牌行（见 main.seedConfigAPIKey）；
	// 这里保留运行时值供面板探活当凭据用。apiKeyMu 保护它：reload 热换。
	apiKeyMu sync.RWMutex
	apiKey   string
	// tokens 是下游 auth token 仓（移植面板的多 key 体系），/v1 准入的
	// 唯一判定源：仓空即开放模式；nil 表示未接线（测试装配），按开放处理。
	tokens *authtoken.Store
	// models 是模型注册表覆盖层（停用开关与重定向）；nil 表示无注册表，
	// 模型名直通 adapter 别名解析。
	models *modelreg.Store
	// tokenCostFn 把一次请求的 token 用量折成美元（目录价口径），
	// 供 token 费用窗口记账；nil 时成本记 0。
	tokenCostFn func(model string, input, output, cacheRead, cacheWrite int64) float64
	// concurrencyLimit 是同时处理的 /v1/* 请求数上限（配置 reload 热换值）；
	// concurrencyInUse 是已占槽数。chan cap 换 CAS 计数器——获取本来就是
	// 非阻塞 try（满了 429），原子计数语义等价且上限可变。
	concurrencyLimit atomic.Int64
	concurrencyInUse atomic.Int64
	// wsConns 限制下游 WebSocket 连接数。连接占用 fd+goroutine，与上游并发
	// 槽分开计量——空闲长连接不该烧并发额度；槽按轮次在 WS 循环里获取。
	wsConns chan struct{}
	// metrics 是常驻运行计数器；始终可用，供面板和进程日志消费。
	metrics *obs.Metrics
	// version 是构建注入的版本标识，healthz 透出供排障定位运行构建。
	version string
	// startedAt 是应用创建时间，供 healthz 报 uptime。
	startedAt time.Time
	// draining 置位后并发槽获取点转为快速 503：进程即将退出，
	// 下游网关应立即换路重试，而不是把请求塞进一个要退出的实例。
	draining atomic.Bool
	// inflight 跟踪占用并发槽的请求与 WS 轮次，供优雅退出等待排空。
	// 不用 sync.WaitGroup：排空期 listener 保持开启，新请求仍会 Add——
	// counter 归零与 waiter 唤醒之间存在调度窗口，窗口内 Add(1) 触发
	// "sync: WaitGroup is reused before previous Wait has returned" panic，
	// 会把正在排空的进程整段炸掉、掐死在途流。
	inflight drainTracker
}

// New 创建一个使用指定供应商适配器的 HTTP 应用。
func New(providerAdapter adapter.Adapter, serverConfig config.ServerConfig, debugManager *debuglog.Manager) *App {
	application := &App{
		adapter:      providerAdapter,
		serverConfig: serverConfig,
		debugManager: debugManager,
		wsConns:      make(chan struct{}, wsMaxConnections),
		metrics:      obs.NewMetrics(),
		startedAt:    time.Now(),
	}
	application.concurrencyLimit.Store(int64(normalizeMaxConcurrency(serverConfig.MaxConcurrency)))
	return application
}

// normalizeMaxConcurrency 归一并发上限：0/负值按默认上限处理。
func normalizeMaxConcurrency(limit int) int {
	if limit <= 0 {
		return defaultMaxConcurrency
	}
	return limit
}

// SetMaxConcurrency 热换 /v1 并发槽上限（配置 reload 路径）。缩容到在途
// 数以下时新 acquire 全拒直到自然排空——正是目标语义，无存量迁移问题。
func (application *App) SetMaxConcurrency(limit int) {
	application.concurrencyLimit.Store(int64(normalizeMaxConcurrency(limit)))
}

// MaxConcurrency 返回当前生效的并发上限（归一后的值）。
func (application *App) MaxConcurrency() int {
	return int(application.concurrencyLimit.Load())
}

// Metrics 返回常驻运行计数器，供面板 stats 端点读取。
func (application *App) Metrics() *obs.Metrics {
	return application.metrics
}

// SetAuthTokens 注入下游令牌仓与成本折算函数；应在 Router 之前调用。
func (application *App) SetAuthTokens(tokens *authtoken.Store, costFn func(model string, input, output, cacheRead, cacheWrite int64) float64) {
	application.tokens = tokens
	application.tokenCostFn = costFn
}

// SetModelRegistry 注入模型注册表；应在 Router 之前调用。
func (application *App) SetModelRegistry(models *modelreg.Store) {
	application.models = models
}

// SetAPIKey 记录 auth.api_key 的运行时值（面板探活凭据用）；应在
// Router/HTTPServer 之前调用。/v1 准入不读它——凭据准入全走令牌仓。
func (application *App) SetAPIKey(apiKey string) {
	application.apiKeyMu.Lock()
	application.apiKey = apiKey
	application.apiKeyMu.Unlock()
}

// APIKey 返回 auth.api_key 的运行时值（配置热重载后为新值）；
// 移植面板的探活用它读取凭据。
func (application *App) APIKey() string {
	application.apiKeyMu.RLock()
	defer application.apiKeyMu.RUnlock()
	return application.apiKey
}

// SetCCPanel 注入管理面板（ccLoad 契约）处理器。
func (application *App) SetCCPanel(d PanelRegistrar) {
	application.ccPanel = d
}

// SetVersion 记录构建版本，由 main 通过 -ldflags -X 注入。
func (application *App) SetVersion(version string) {
	application.version = version
}

// Router 返回应用的 chi HTTP 路由。
func (application *App) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", application.health)
	// ccLoad 行为：裸根重定向到面板首页（静态页自身做登录门）。
	router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/web/index.html", http.StatusFound)
	})
	router.Group(func(protected chi.Router) {
		// request-id 最先挂上：连同鉴权/并发拒绝在内的所有 /v1/* 响应
		// 都需要 Anthropic 形态的请求 ID。
		protected.Use(requestIDMiddleware)
		// 先鉴权再占并发槽：未携带 key 的洪水请求不应消耗稀缺并发额度。
		protected.Use(application.apiKeyMiddleware)
		// WebSocket 升级单独一组：连接是长生命周期的，不能在 middleware 里
		// 整连接持并发槽——槽由 WS 循环按轮次获取/释放，连接数另有 wsConns 上限。
		protected.Group(func(ws chi.Router) {
			ws.Get("/v1/responses", application.createResponsesWebSocket)
		})
		// /v1/models 是元数据读，不占并发槽：慢目录拉取下一条轻量
		// 请求不该烧槽位到 ReadTimeout；上游侧由目录 singleflight 与
		// 失败冷却自保。
		protected.Get("/v1/models", application.listModels)
		protected.Get("/v1/models/{model}", application.getModel)
		// chi 不允许在同一 mux 上先注册路由再 Use——并发闸门单独开一组，
		// 组内 Use 先于路由注册，组外的 models/WS 不受它约束。
		protected.Group(func(gated chi.Router) {
			gated.Use(application.concurrencyMiddleware)
			gated.Post("/v1/responses", application.createResponses)
			gated.Post("/v1/chat/completions", application.createChatCompletions)
			gated.Post("/v1/messages", application.createMessages)
		})
	})
	if application.ccPanel != nil {
		// gzip 只压 /admin|/dashboard 的 JSON 响应（见 gzipPanelMiddleware
		// 的判定）；/v1 的 SSE/WS 不在该子树内。
		application.ccPanel.Register(router.With(gzipPanelMiddleware))
	}
	return router
}

// HTTPServer 创建带有应用路由和超时配置的 HTTP 服务。
func (application *App) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              application.serverConfig.Listen,
		Handler:           application.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    1 << 20,
	}
}

func (application *App) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	// 除存活信号外带版本与运行概况：健康检查同时也是排障入口，
	// 让调用方不碰面板就能确认「跑的是哪一版、日志是否开着」。
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"status":         "ok",
		"version":        application.version,
		"uptime_seconds": int64(time.Since(application.startedAt).Seconds()),
		"debug_logging":  application.debugManager.Enabled(),
		// 排空期 healthz 仍应答——部署脚本靠 version+draining 区分
		// 「旧实例还在排」与「新实例已接管」；active_requests 让部署
		// 能挑空闲窗口 kickstart，排空期 503 少砸到真实请求。
		"draining":        application.draining.Load(),
		"active_requests": application.metrics.Active(),
		// pid 让部署脚本区分「应答的是交接进程还是托管新实例」——
		// 交接期间 version 两边相同，只有 pid 能确认切换终态。
		"pid": os.Getpid(),
	})
}

// modelEntry 把目录条目投影为 OpenAI /v1/models 形状；列表与详情端点
// 共用同一份字段集，避免两处漂移。非 OpenAI 标准字段供面板/网关按能力
// 做请求前 gate（含 is_model_router：router uid 直连上游会被拒）。
// modelCreatedFallback 是目录缺 created 字段时全部模型共用的兜底时间戳
// （进程启动时刻）：逐请求取 time.Now() 会让同一模型的 created 逐次漂移。
var modelCreatedFallback = time.Now().Unix()

func modelEntry(m adapter.ModelInfo) map[string]any {
	created := m.Created
	if created == 0 {
		created = modelCreatedFallback
	}
	ownedBy := m.OwnedBy
	if ownedBy == "" {
		ownedBy = "devin"
	}
	entry := map[string]any{
		"id": m.ID, "object": "model", "created": created, "owned_by": ownedBy,
		"supports_images":              m.SupportsImages,
		"supports_tool_calls":          m.SupportsToolCalls,
		"supports_parallel_tool_calls": m.SupportsParallelToolCalls,
		"supports_thinking":            m.SupportsThinking,
		"preserve_thinking":            m.PreserveThinking,
		"is_model_router":              m.IsModelRouter,
		"context_tokens":               m.ContextTokens,
		"max_output_tokens":            m.MaxOutputTokens,
	}
	// alias_of 标记该 id 是客户端别名：请求会被改写到目标 uid 运行。
	if m.AliasOf != "" {
		entry["alias_of"] = m.AliasOf
	}
	return entry
}

// listModels 返回 OpenAI 兼容的 GET /v1/models 列表。
func (application *App) listModels(writer http.ResponseWriter, request *http.Request) {
	models, err := application.adapter.ListModels(request.Context())
	if err != nil {
		writeUpstreamCatalogError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelEntry(m))
	}
	data = application.mergeAliases(data)
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
}

// getModel 返回 OpenAI 兼容的 GET /v1/models/{model}。
func (application *App) getModel(writer http.ResponseWriter, request *http.Request) {
	id := chi.URLParam(request, "model")
	if id == "" {
		writeJSONError(writer, http.StatusBadRequest, "model id is required", "invalid_request_error")
		return
	}
	models, err := application.adapter.ListModels(request.Context())
	if err != nil {
		writeUpstreamCatalogError(writer, err)
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		data = append(data, modelEntry(m))
	}
	for _, entry := range application.mergeAliases(data) {
		if entry["id"] == id {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(entry)
			return
		}
	}
	writeJSONError(writer, http.StatusNotFound, fmt.Sprintf("model %q not found", id), "invalid_request_error")
}

// aliasProvider 解出可选的别名映射来源；目前只有 devin adapter 实现，
// 与 dashboard.SetAliasesFunc 的注入方式保持一致，接口不强求。
type aliasProvider interface {
	Aliases() map[string]string
}

// mergeAliases 把 devin.aliases 并入模型列表投影：别名撞名真实目录条目时
// 给该条目标注 alias_of（名字仍可达，但请求会被改写为 alias_of 的 uid——
// 不标注的话目录在说谎）；目录缺席的纯别名补一条合成条目，能力位抄目标
// 模型，发现端（/model 选择器、模型列表）才能看到并正确认知别名。
func (application *App) mergeAliases(data []map[string]any) []map[string]any {
	provider, ok := application.adapter.(aliasProvider)
	if !ok {
		return data
	}
	aliases := provider.Aliases()
	if len(aliases) == 0 {
		return data
	}
	byID := make(map[string]map[string]any, len(data))
	for _, entry := range data {
		if id, ok := entry["id"].(string); ok {
			byID[id] = entry
		}
	}
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	slices.Sort(names)
	capabilityKeys := []string{
		"supports_images", "supports_tool_calls",
		"supports_parallel_tool_calls", "supports_thinking", "preserve_thinking",
	}
	for _, name := range names {
		target := strings.TrimSpace(aliases[name])
		if target == "" {
			continue
		}
		base, hasTarget := byID[target]
		if entry, ok := byID[name]; ok {
			// 影子名：条目仍挂目录原位，但能力位以实际跑的目标为准——
			// 请求这个名字得到的是 target 的行为，原模型的能力位是说谎。
			entry["alias_of"] = target
			if hasTarget {
				for _, key := range capabilityKeys {
					if value, ok := base[key]; ok {
						entry[key] = value
					}
				}
			}
			continue
		}
		entry := map[string]any{
			"id": name, "object": "model", "created": modelCreatedFallback,
			"owned_by": "alias", "alias_of": target,
		}
		if hasTarget {
			for _, key := range capabilityKeys {
				if value, ok := base[key]; ok {
					entry[key] = value
				}
			}
		}
		data = append(data, entry)
	}
	return data
}

// writeUpstreamCatalogError 上报模型目录拉取失败：除 429（透传限流语义
// + Retry-After，客户端按语义退避）外一律 502——目录失败是上游责任，
// 上游的 4xx 方言（unauthenticated 等）漏给客户端会被误当成自身凭据错。
func writeUpstreamCatalogError(writer http.ResponseWriter, err error) {
	failure := llm.Classify(err)
	status := common.HTTPStatus(failure)
	if status != http.StatusTooManyRequests && status < 500 {
		status = http.StatusBadGateway
	}
	if failure.RetryAfterSeconds > 0 {
		writer.Header().Set("Retry-After", strconv.Itoa(failure.RetryAfterSeconds))
	}
	writeJSONError(writer, status, err.Error(), common.OpenAIErrorType(failure))
}

func writeJSONError(writer http.ResponseWriter, status int, message string, errType string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": errType, "code": nil, "param": nil},
	})
}

func writeAuthError(writer http.ResponseWriter, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "unauthenticated", "code": nil, "param": nil},
	})
}

// writeRateLimitError 返回 429 + Retry-After：并发溢出是「本地过载」不是
// 服务端故障，503 会让下游网关误判渠道故障并冷却。
func writeRateLimitError(writer http.ResponseWriter, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Retry-After", "1")
	writer.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "rate_limit_error", "code": "rate_limit_exceeded", "param": nil},
	})
}

// writeDrainingError 在优雅退出排空期返回 503 + Retry-After：实例即将退出，
// 下游应立即换路重试；Refused 连接与挂在半路的流都换成一个可行动的错误。
func writeDrainingError(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Retry-After", "1")
	writer.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": "server is draining for restart; retry the request", "type": "server_error", "code": "server_draining", "param": nil},
	})
}

// drainTracker 计数在途并发槽并给排空等待方一个完成信号。
// 全部状态迁移在 mu 下进行：WaitGroup 版本里「counter 归零」与
// 「waiter 真正返回」是两步，其间新来的 Add 即 panic；这里归零与
// 唤醒是同一临界区内的原子动作，任何时序的 Add 都安全。
type drainTracker struct {
	mu      sync.Mutex
	count   int
	drained chan struct{} // 非 nil 表示有排空等待方；计数归零时关闭并置 nil
}

// Add 占用一个并发槽。允许在排空等待进行中调用——等待方会在
// 计数再次归零时才被唤醒，语义与「Add 先于 draining 检查」一致。
func (tracker *drainTracker) Add() {
	tracker.mu.Lock()
	tracker.count++
	tracker.mu.Unlock()
}

// Done 释放一个并发槽；计数归零且有等待方时唤醒。
func (tracker *drainTracker) Done() {
	tracker.mu.Lock()
	tracker.count--
	if tracker.count == 0 && tracker.drained != nil {
		close(tracker.drained)
		tracker.drained = nil
	}
	tracker.mu.Unlock()
}

// Wait 阻塞到计数归零或 ctx 截止。
func (tracker *drainTracker) Wait(ctx context.Context) error {
	tracker.mu.Lock()
	if tracker.count == 0 {
		tracker.mu.Unlock()
		return nil
	}
	if tracker.drained == nil {
		tracker.drained = make(chan struct{})
	}
	done := tracker.drained
	tracker.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BeginDrain 进入排空态：新请求快速 503，在途请求继续跑完。
// listener 保持开启由调用方控制——http.Server.Shutdown 会先关 listener 再
// 等在途连接，排空期整段变成 connection refused；这里改为排空结束才 Close。
// 适配器实现可选 BeginDrain 接口时同步通知：devin 保温调度是后台上游生产者，
// 排空语义是「不再制造新上游工作」，不只「不再接收新下游请求」——进程排空
// 可能持续数分钟，ping 若照发会把冷掉的缓存又焐热，白烧上游配额。
func (application *App) BeginDrain() {
	application.draining.Store(true)
	if drainer, ok := application.adapter.(interface{ BeginDrain() }); ok {
		drainer.BeginDrain()
	}
}

// WaitDrain 阻塞到在途并发槽清空或 ctx 超时；超时返回错误，调用方负责强制 Close。
func (application *App) WaitDrain(ctx context.Context) error {
	return application.inflight.Wait(ctx)
}

// noteReject 统一记录一次管线前拒绝：分原因计数、事件环与进程日志同源。
// 这类请求没有调试记录与 logs 行——计数/事件环供面板查，slog 行是唯一
// 跨重启留存的足迹（部署后查排空期拒绝就靠它）。
func (application *App) noteReject(reason obs.RejectReason, request *http.Request, status int) {
	event := obs.RejectEvent{
		Status:    status,
		Path:      request.URL.Path,
		IP:        clientIP(request),
		KeyHash:   requestCredentialHash(request),
		UserAgent: request.UserAgent(),
	}
	application.metrics.Reject(reason, event)
	slog.Warn("request rejected",
		"reason", string(reason), "status", status, "path", request.URL.Path,
		"client_ip", event.IP, "key_hash", event.KeyHash, "ua", event.UserAgent)
}

// admitTurn 做一次 /v1 轮次准入：inflight.Add 先行（排空等待才能覆盖所有
// 已进入的请求），再查排空标记与并发槽。reason 非空即被拒。
// 返回的 release 无论成败都必须调用；拒绝路径要在写完拒绝帧之后才调——
// release 里的 inflight.Done 放行 WaitDrain，排空中的进程随时退出，
// 写晚了的拒绝通知客户端根本收不到。HTTP 与 WS 两条入口共用这段计数
// 纪律，各自只负责按自己的 wire 渲染拒绝。
func (application *App) admitTurn() (reason obs.RejectReason, status int, release func()) {
	application.inflight.Add()
	release = application.inflight.Done
	if application.draining.Load() {
		return obs.RejectDraining, http.StatusServiceUnavailable, release
	}
	limit := application.concurrencyLimit.Load()
	for {
		cur := application.concurrencyInUse.Load()
		if cur >= limit {
			return obs.RejectConcurrencyLimit, http.StatusTooManyRequests, release
		}
		if application.concurrencyInUse.CompareAndSwap(cur, cur+1) {
			release = func() {
				application.concurrencyInUse.Add(-1)
				application.inflight.Done()
			}
			return "", 0, release
		}
	}
}

// concurrencyMiddleware 限制同时处理的 /v1/* 请求数，避免上游阻塞时资源耗尽。
func (application *App) concurrencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		reason, status, release := application.admitTurn()
		if reason != "" {
			application.noteReject(reason, request, status)
			if reason == obs.RejectDraining {
				writeDrainingError(writer)
			} else {
				writeRateLimitError(writer, "server is busy, please try again later")
			}
			release()
			return
		}
		defer release()
		next.ServeHTTP(writer, request)
	})
}

// requestIDMiddleware 给每个 /v1/* 响应发 req_ 前缀的请求 ID。
// Claude Code 只把 req_ 前缀的 request-id 认作 Anthropic 第一方响应：
// 缺失或非 req_ 形态时它把响应当作中间人代理产物——流式失败后会再做
// 一次非流式探测请求，且重试预算与超时参数都按更保守的档取。
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("request-id", randid.Prefixed("req_"))
		next.ServeHTTP(writer, request)
	})
}

// presentedCredential 提取请求携带的下游凭据：Bearer 优先，X-Api-Key 兜底。
func presentedCredential(request *http.Request) string {
	if auth := request.Header.Get("Authorization"); auth != "" {
		const prefix = "Bearer "
		if strings.HasPrefix(auth, prefix) {
			return strings.TrimSpace(auth[len(prefix):])
		}
	}
	if key := request.Header.Get("X-Api-Key"); key != "" {
		return strings.TrimSpace(key)
	}
	return ""
}

// authenticate 判定下游凭据，令牌仓是唯一判定源：哈希命中→(token,true)
// ——空明文凭据命中匿名通道行亦然；仓空（或未接线）→(nil,true) 开放
// 模式；其余→false。apiKeyMiddleware 与 createCompletion 共用——WS
// 轮次的内层请求不经过 middleware，准入在 createCompletion 里必须能
// 独立重演这套判定。
func (application *App) authenticate(credential string) (*authtoken.Token, bool) {
	if application.tokens == nil {
		return nil, true
	}
	if t, ok := application.tokens.Resolve(credential); ok {
		return t, true
	}
	if application.tokens.Empty() {
		return nil, true
	}
	return nil, false
}

// apiKeyMiddleware 校验 OpenAI 兼容接口的下游凭据：有效 auth token
// （含匿名通道行）才放行；仓空时（开放模式）不校验。
func (application *App) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		provided := presentedCredential(request)
		if _, ok := application.authenticate(provided); !ok {
			if provided == "" {
				application.noteReject(obs.RejectMissingAPIKey, request, http.StatusUnauthorized)
				writeAuthError(writer, "Missing API key")
			} else {
				application.noteReject(obs.RejectInvalidAPIKey, request, http.StatusUnauthorized)
				writeAuthError(writer, "Invalid API key")
			}
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func (application *App) createResponses(writer http.ResponseWriter, request *http.Request) {
	application.createCompletion(writer, request, "openai-responses", decodeResponsesRequest, responsesProtocol{})
}

func (application *App) createChatCompletions(writer http.ResponseWriter, request *http.Request) {
	application.createCompletion(writer, request, "openai-chat", decodeChatRequest, chatProtocol{})
}

func (application *App) createMessages(writer http.ResponseWriter, request *http.Request) {
	application.createCompletion(writer, request, "anthropic", decodeAnthropicRequest, anthropicProtocol{})
}

func (application *App) createCompletion(
	writer http.ResponseWriter,
	request *http.Request,
	api string,
	decoder decodeRequestFunc,
	protocol protocolEncoder,
) {
	reqMetrics := application.metrics.Begin()
	// reqCtx 供面板 Abort 主动中断：cancel 挂到 recorder 上，
	// Complete 时 recorder 自动解除挂接，defer cancel 兜底释放。
	// WithCancelCause 让中断原因沿 ctx 链传到事件泵/上游 Recv——
	// 客户端看到的错误是「aborted via panel」而非裸 context.Canceled。
	reqCtx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	completion := debuglog.Completion{StatusCode: http.StatusInternalServerError, Result: "failed"}
	startedAt := time.Now()
	responseBytes := 0
	// authTok 是本次请求解析到的下游令牌（开放模式为 nil——空仓无凭据
	// 准入，无行可归因）；tokenAcquired 标记并发槽已占，defer 据此配对
	// Release；tokenBlocked 标记准入拒绝——被拒请求不进令牌统计
	//（ccLoad 在代理层前就返回）。
	var authTok *authtoken.Token
	tokenAcquired := false
	tokenBlocked := false
	// recorder 在请求体读成后才创建：连完整请求都没到达的读失败
	//（超时/断连/对端 RST）不产生调试记录与 logs 行——它们与鉴权、
	// 并发、排空拒绝同口径，是唯一痕迹在 http.rejects 里的管线前拒绝。
	var recorder *debuglog.Recorder
	startRecorder := func() {
		recorder = application.debugManager.Start(debuglog.RequestMeta{
			Method:          request.Method,
			Path:            request.URL.Path,
			API:             api,
			ClientIP:        clientIP(request),
			UserAgent:       request.UserAgent(),
			KeyHash:         requestCredentialHash(request),
			ClientRequestID: clientRequestID(request),
		})
		recorder.SetAbort(func() {
			cancel(fmt.Errorf("aborted via panel request abort: %w", context.Canceled))
		})
		// Stripe Request-Id 模式：本地调试身份 <dir> 写进响应头，agent
		// 拿到后可查 logs 表或 /admin/debug-logs/{id}（{id} 是 logs 表主键）。
		// 头部在首个字节写出时才提交，因此流式请求与中途错误同样生效。
		if ref := debugRef(recorder); ref != "" {
			writer.Header().Set("X-Request-Id", ref)
		}
	}
	defer func() {
		recorder.Complete(completion)
		reqMetrics.Finish(completion.StatusCode, responseBytes, completion.Result)
		if authTok != nil && !tokenBlocked {
			// 令牌统计回写（ccLoad updateTokenStats 同口径：499 跳过、
			// token/费用只记 2xx）；FirstByteSec 取自上游首字节标记。
			res := authtoken.Result{
				StatusCode:       completion.StatusCode,
				Stream:           completion.Stream,
				DurationSec:      time.Since(startedAt).Seconds(),
				InputTokens:      completion.Usage.Input,
				OutputTokens:     completion.Usage.Output,
				CacheReadTokens:  completion.Usage.CacheRead,
				CacheWriteTokens: completion.Usage.CacheWrite,
			}
			if firstMS := recorder.FirstUpstreamMS(); firstMS > 0 {
				res.FirstByteSec = float64(firstMS) / 1000
			}
			if application.tokenCostFn != nil {
				model := completion.Model
				if model == "" {
					model = completion.RequestedModel
				}
				res.CostUSD = application.tokenCostFn(model, res.InputTokens, res.OutputTokens, res.CacheReadTokens, res.CacheWriteTokens)
			}
			application.tokens.AddResult(authTok.ID, res)
		}
		if tokenAcquired {
			application.tokens.Release(authTok.ID)
		}
		slog.Info("request",
			"api", api, "method", request.Method, "path", request.URL.Path,
			"status", completion.StatusCode, "result", completion.Result,
			"duration_ms", time.Since(startedAt).Milliseconds(),
			"model", completion.Model, "requested_model", completion.RequestedModel,
			"stream", completion.Stream, "client_ip", clientIP(request),
			"upstream_request_id", completion.UpstreamRequestID,
			"input_tokens", completion.Usage.Input, "output_tokens", completion.Usage.Output,
			"cache_read_tokens", completion.Usage.CacheRead)
	}()

	// 图片 base64 会显著放大 JSON；与常见 IDE 多图请求对齐到 32MiB。
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 32<<20))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			// 字节超限按 PayloadTooLarge 报 413：下游网关按 4xx 归类为
			// 客户端可修正错误。不贴 context_length_exceeded——这里量的
			// 是字节不是 token，上游的 ContextTooLong 由归一链另行覆盖。
			// ≥32MiB 的载荷是真实到达的请求，留调试记录供容量排障。
			startRecorder()
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPRead, http.StatusRequestEntityTooLarge, fmt.Errorf("request payload exceeds the %d MiB limit", tooLarge.Limit>>20))
			return
		}
		// 读体失败（超时/截断）按 504 下发而非 400：4xx 在 Claude Code
		// 等客户端是不可重试错误、直接杀掉轮次（subagent 连根死），
		// 5xx 才进标准重试预算——请求没读完是传输抖动，不是客户端
		// 可修正的错误。
		application.noteReject(obs.RejectHTTPRead, request, http.StatusGatewayTimeout)
		completion.StatusCode = http.StatusGatewayTimeout
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusGatewayTimeout)
		_, _ = writer.Write(protocol.EncodeError(fmt.Errorf("read request: %w", err), ""))
		return
	}
	startRecorder()
	if recorder != nil {
		// 投影会对 body 再做一次 generic unmarshal；recorder 为 nil 时
		// WriteJSON 是 no-op，参数表达式却仍会求值——必须在外层门控。
		recorder.WriteJSON(debuglog.StageHTTPRequest, httpRequestProjection(request, body))
	}
	// collectDropped 门控解码期对请求体的二次全量扫描（顶层未消费字段
	// 收集）——Dropped 的唯一读者是 02 投影，recorder 为 nil 时纯烧 CPU。
	messages, options, err := decoder(body, recorder != nil)
	if err != nil {
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPDecode, http.StatusBadRequest, err)
		return
	}
	// 显式亲和头恒赢于 body 提取的 SessionKey：头是调用方的意图声明，
	// 号池会话绑定与 trajectory 谱系都以它为种子。
	if key := common.SessionKeyFromHeader(request.Header); key != "" {
		messages.SessionKey = key
	}
	completion.Model = messages.Model
	completion.RequestedModel = messages.Model
	completion.Stream = options.Stream
	reqMetrics.Observe(options.Stream, len(body))
	recorder.SetModel(messages.Model)
	if recorder != nil {
		// 02 投影必须就地求值、不能推迟到日志 worker：adapter 的
		// sanitizeRequest 会原地改写 messages 的共享 slice——推迟读
		// 既会数据竞争，也会把「客户端原文」记成改写后内容。
		recorder.WriteJSON(debuglog.StageRequestMessages, debuglog.RequestMessagesProjection(messages))
	}
	// 令牌准入（ccLoad RequireAPIAuth/enforceTokenLimits 同序）：先占并发槽，
	// 再查模型白名单、RPM 窗口，最后查费用窗口。HTTP 路径上 apiKeyMiddleware
	// 已验过凭据，这里是幂等复核；WS 轮次的内层请求不走 middleware，靠它兜底。
	if tok, ok := application.authenticate(presentedCredential(request)); ok {
		authTok = tok
	} else {
		// HTTP 路径上 middleware 已放行，走到这里失败只可能是令牌在请求
		// 处理中被删/停用/过期，或 WS 会话建立后状态翻转——按 401 收尾。
		tokenBlocked = true
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageTokenLimit, http.StatusUnauthorized, errors.New("invalid or expired api token"))
		return
	}
	if authTok != nil {
		// 匿名通道请求不带凭据，recorder 采样不到 key_hash——拿到令牌后
		// 回填，index/meta/进行中行才把匿名流量归到该行。
		recorder.SetKeyHash(authTok.KeyHash())
		active, limit, ok := application.tokens.Acquire(authTok.ID)
		if !ok {
			tokenBlocked = true
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageTokenLimit, http.StatusTooManyRequests, fmt.Errorf("token concurrency limit exceeded: %d active of %d limit", active, limit))
			return
		}
		tokenAcquired = true
		if !authTok.IsModelAllowed(messages.Model) {
			tokenBlocked = true
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageTokenLimit, http.StatusForbidden, fmt.Errorf("model '%s' is not allowed for this token", messages.Model))
			return
		}
		if used, limit, ok := application.tokens.AllowRPM(authTok.ID); !ok {
			tokenBlocked = true
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageTokenLimit, http.StatusTooManyRequests, fmt.Errorf("token rate limit exceeded: %d of %d requests per minute", used, limit))
			return
		}
		if used, limit, window, exceeded := application.tokens.CostLimitState(authTok.ID); exceeded {
			tokenBlocked = true
			windowName := map[string]string{"5h": "5h", "daily": "Daily", "weekly": "Weekly", "monthly": "Monthly", "total": "Total"}[window]
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageTokenLimit, http.StatusTooManyRequests, fmt.Errorf("%s cost limit exceeded: $%.2f used of $%.2f limit", windowName, float64(used)/1e6, float64(limit)/1e6))
			return
		}
	}
	// 模型注册表准入（ccLoad 渠道 ModelEntry 同义，本服务为全局覆盖层）：
	// 停用按 404 收尾——对客户端的语义是本网关不提供该模型；redirect_model
	// 改写请求模型名，下游走 adapter 别名解析落到最终上游 uid。
	// RequestedModel 保持客户端原名，logs 表同时留两段身份。
	// 不设 tokenBlocked：请求本身合法，按失败回写令牌统计（ccLoad 同口径）。
	if application.models != nil {
		if entry, ok := application.models.Lookup(messages.Model); ok {
			if entry.Disabled {
				completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageModelDisabled, http.StatusNotFound, fmt.Errorf("model '%s' is disabled", messages.Model))
				return
			}
			if entry.RedirectModel != "" {
				messages.Model = entry.RedirectModel
				completion.Model = entry.RedirectModel
			}
		}
	}
	ctx := debuglog.WithRecorder(reqCtx, recorder)
	if options.Stream {
		application.streamCompletion(ctx, writer, recorder, protocol, messages, options, &completion, &responseBytes)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	out := &streamWriter{writer: writer, recorder: recorder}
	if flusher, ok := writer.(http.Flusher); ok {
		out.flusher = flusher
		// "\n" 心跳只发给 OpenAI 系（Codex 约 30s 无字节弃连）。
		// Anthropic 非流式在上游思考窗口保持静默：Anthropic SDK 系客户端
		// 容忍分钟级首字等待，而任何提前写出的字节都把状态提交为 200，
		// 之后的失败只能以「200 + 错误体」下发——Claude Code 把它判为
		// malformed response 并终止整轮，不可重试。
		if api != "anthropic" {
			out.heartbeat = []byte("\n")
		}
	}
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	items := startStreamPump(streamCtx, application.adapter, messages, recorder)
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()
	message, err := collectPumpedMessage(streamCtx, out, items, ticker)
	if err != nil {
		failure := llm.Classify(err)
		noteRetryAfter(recorder, failure)
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || streamCtx.Err() != nil {
			completion.Result = "disconnected"
			// 未提交时按 499（nginx 约定的客户端关闭）入账——断连不该
			// 记成 500 污染 server_error 聚合；心跳已提交 200 的按线上实况记。
			if !out.committed {
				completion.StatusCode = 499
			} else {
				completion.StatusCode = http.StatusOK
			}
			recorder.WriteError(debuglog.ErrStageClientDisconnected, err)
			return
		}
		if out.committed {
			// 心跳已把状态提交为 200，错误只能以协议错误体下发——
			// 前导 \n 是合法 JSON 空白，客户端解析出 error 字段。
			completion.StatusCode = http.StatusOK
			if writeErr := out.writeContent(protocol.EncodeError(err, debugRef(recorder))); writeErr != nil {
				completion.Result = "disconnected"
				recorder.WriteError(debuglog.ErrStageClientDisconnected, writeErr)
				return
			}
			recorder.WriteError(debuglog.ErrStageResponseEvent, err)
			return
		}
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageResponseEvent, common.HTTPStatus(failure), err)
		return
	}
	updateCompletionIdentity(&completion, messages, message)
	body, err = protocol.EncodeFinal(message, strings.TrimSpace(messages.Model), options)
	if err != nil {
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPEncode, http.StatusInternalServerError, err)
		return
	}
	// committed 必须在写出前采样：write 内部先置位再写，写失败后
	// 再读已区分不出「此前心跳已提交 200」与「首个字节就没发出去」。
	wasCommitted := out.committed
	if err := out.writeContent(body); err != nil {
		completion.Result = "disconnected"
		if !wasCommitted {
			completion.StatusCode = 499
		} else {
			completion.StatusCode = http.StatusOK
		}
		recorder.WriteError(debuglog.ErrStageClientDisconnected, err)
		return
	}
	responseBytes += out.bytes
	recorder.AppendJSONL(debuglog.StageHTTPResponse, "response", json.RawMessage(body))
	completion.StatusCode = http.StatusOK
	completion.Result = "completed"
}

func updateCompletionIdentity(completion *debuglog.Completion, messages llm.RequestMessages, message *llm.AssistantMessage) {
	if message == nil {
		return
	}
	completion.Provider = message.Provider
	completion.UpstreamRequestID = message.UpstreamRequestID
	completion.Usage = message.Usage
	completion.ResponseModel = message.ResponseModel
	completion.PrematureEndTurn = prematureEndTurn(messages, message)
	// 上游声明的模型与实际下发的 uid 不一致时记错配——路由/重定向排障信号。
	if message.ResponseModel != "" && message.Model != "" && message.ResponseModel != message.Model {
		completion.ModelMismatch = true
	}
	if message.ResponseModel != "" {
		completion.Model = message.ResponseModel
	} else if message.Model != "" {
		completion.Model = message.Model
	}
}

// prematureEndTurn 标记疑似提前收轮：末条输入是 tool_result、响应无
// toolCall 却声明 STOP——形似「宣告要做事却直接结束」。该形态结构上
// 合法（可能真是最终答复），但实测存在模型声称继续动作后直接 EOS 的
// 故障模式（notes/archive/2026-09-12-premature-endturn.md）。这是候选
// 信号而非判定：任务正常收官（末轮 tool_result → 总结文本 → STOP）
// 形状完全相同，只能靠语义（宣告式 vs 总结式）或会话是否终结来区分，
// 读 logs 表计数时每个命中都要这样复核。
func prematureEndTurn(messages llm.RequestMessages, message *llm.AssistantMessage) bool {
	if message.StopReason != llm.StopReasonStop || len(messages.Messages) == 0 {
		return false
	}
	for _, block := range message.Content {
		if block.ContentType() == llm.ContentTypeToolCall {
			return false
		}
	}
	_, ok := messages.Messages[len(messages.Messages)-1].(llm.ToolResultMessage)
	return ok
}

// clientIP 提取下游客户端地址；WebSocket 内部请求已透传 RemoteAddr。
func clientIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

// clientRequestID 提取客户端自带的关联 ID，供其事后按自己的 ID 反查日志。
// 只认常见关联头；长度截断防止异常大的头放大日志体积。
func clientRequestID(request *http.Request) string {
	for _, header := range []string{"X-Request-Id", "X-Session-Id", "X-Client-Request-Id"} {
		if value := strings.TrimSpace(request.Header.Get(header)); value != "" {
			if len(value) > 128 {
				return value[:128]
			}
			return value
		}
	}
	return ""
}

// requestCredentialHash 计算请求携带凭据的短哈希用于按 key 关联日志；
// 未携带凭据时返回空串。永远不落明文——SHA-256 前 8 字节。
// auth token 的 sha256(明文)[:16] 与其 KeyHash 同值，日志行据此关联令牌。
func requestCredentialHash(request *http.Request) string {
	return hashCredential(presentedCredential(request))
}

func hashCredential(credential string) string {
	if credential == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(credential))
	return hex.EncodeToString(sum[:8])
}

// debugRef 返回本请求的调试目录名作为跨接口关联引用；
// 未启用调试日志（nil recorder）或异常路径时为空串。
func debugRef(recorder *debuglog.Recorder) string {
	return recorder.Dir()
}

func httpRequestProjection(request *http.Request, body []byte) map[string]any {
	// body 以 RawMessage 原样交给日志 worker：请求 goroutine 不做全量
	// unmarshal 建树——worker 侧 rawNeedsSanitize 预筛后，干净 body 直接
	// 落盘，含敏感键/图片才走完整脱敏（语义与旧的全量投影一致）。
	// 非 JSON body 退化为字符串（此时 decode 必然 400，只为留证）。
	var parsedBody any = json.RawMessage(body)
	if !json.Valid(body) {
		parsedBody = string(body)
	}
	return map[string]any{
		"method": request.Method,
		"path":   request.URL.Path,
		"headers": map[string]string{
			"accept":              request.Header.Get("Accept"),
			"content_type":        request.Header.Get("Content-Type"),
			"user_agent":          request.Header.Get("User-Agent"),
			"x_client_request_id": request.Header.Get("X-Client-Request-Id"),
		},
		"body": parsedBody,
	}
}

// writeLoggedError 写出错误响应并返回实际下发的状态码——clientFixable
// 的 ≥500 会压成 400，调用方应记返回值而非入参，否则索引口径「服务端
// 错误」与客户端口径「请求错误」错配，按状态码归因会误伤。
func writeLoggedError(writer http.ResponseWriter, recorder *debuglog.Recorder, protocol protocolEncoder, stage string, status int, err error) int {
	failure := llm.Classify(err)
	recorder.WriteError(stage, err)
	// 进程日志只出白名单信号 + 脱敏摘要；完整原文留在该请求调试记录的 error.json。
	// stage 是捕获点（本函数被哪层错误出口调用），error_stage 是归原点
	//（logs 表的同名列）——闸门拒绝会在 provider_stream 出口被捕获，
	// 但归原点是 rate_gate；两层都写出来排障时才不会读岔。
	originStage, _ := recorder.FirstError()
	if originStage == "" {
		originStage = stage
	}
	logAttrs := []any{"stage", stage, "error_stage", originStage, "status", status, "error", obs.Diagnostic(err)}
	if failure.LocalGate {
		// 本地闸门快败是主动整形而非故障：压测/超额期每分钟几十条，
		// WARN 级别会把真正的异常淹掉。
		slog.Info("request failed", logAttrs...)
	} else {
		slog.Warn("request failed", logAttrs...)
	}
	// 客户端可修正的错误统一报 invalid_request_error（两个协议对该语义
	// 同名），便于 IDE 直接展示；状态码同样压回 4xx。
	clientFixable := status == http.StatusBadRequest ||
		status == http.StatusRequestEntityTooLarge ||
		failure.ClientFixable
	if clientFixable && status >= 500 {
		status = http.StatusBadRequest
	}
	writer.Header().Set("Content-Type", "application/json")
	// 上游限流文案里的 reset hint 是唯一可行动信号——翻成标准
	// Retry-After 头 + Anthropic 统一限流重置时刻，客户端/网关才能
	// 按语义退避而不是猜。unified-reset 给的是绝对时刻：Claude Code
	// 对 429 优先按它睡到重置点（上限 6h），分钟级限流也能扛过
	// 整个重试预算。注意：非 429 状态的 Retry-After 不可超过 60s——
	// Claude Code 对超长的非限流 Retry-After 直接终止整轮。
	if status == http.StatusTooManyRequests {
		recorder.SetRateLimited()
		if resetAt, ok := failure.RateLimitReset(time.Now()); ok {
			// 显式 0 秒 hint（刚过桶界、新桶已爆、无追加罚）解出
			// resetAt=now——等价于「无退避指导」，不写头保持原状。
			if wait := int(math.Ceil(time.Until(resetAt).Seconds())); wait > 0 {
				writer.Header().Set("Retry-After", strconv.Itoa(wait))
				writer.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(resetAt.Unix(), 10))
			}
		}
	}
	noteRetryAfter(recorder, failure)
	writer.WriteHeader(status)
	// 错误体按客户端协议成形：/v1/messages 必须回 Anthropic 信封，
	// 否则 Claude Code 解析不出 error 字段。stage 标明失败发生在哪一层，
	// debug_ref 是本地调试目录名，agent 凭它一次调用即可拿到全部证据。
	body := protocol.EncodeHTTPError(httpError{
		Failure: failure, ClientFixable: clientFixable,
		Stage: stage, DebugRef: debugRef(recorder),
	})
	_, _ = writer.Write(body)
	recorder.AppendJSONL(debuglog.StageHTTPResponse, "error", json.RawMessage(body))
	return status
}

// noteRetryAfter 把限流记录的 reset 秒数记进请求日志——无论它最终
// 走 Retry-After 头（未提交 429）还是已提交后的错误体下发，索引里都有可查的
// 结构化 hint，grep/聚合不必再解析文案。
func noteRetryAfter(recorder *debuglog.Recorder, failure *llm.Failure) {
	if failure != nil && failure.RetryAfterSeconds > 0 {
		recorder.SetRetryAfter(failure.RetryAfterSeconds)
	}
}
