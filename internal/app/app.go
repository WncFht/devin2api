// 本文件定义 HTTP 应用、chi 路由和供应商适配器的串联逻辑。
//
// Package app 负责组装 HTTP 路由并连接 API 编解码与供应商适配器。
package app

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/randid"
)

// DashboardRegistrar 描述面板路由注册所需的最小能力。
type DashboardRegistrar interface {
	Register(mux interface {
		Get(pattern string, handlerFn http.HandlerFunc)
		Post(pattern string, handlerFn http.HandlerFunc)
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
	// dashboard 是可选的管理面板处理器；nil 表示不启用面板。
	dashboard DashboardRegistrar
	// apiKey 是可选的 OpenAI 兼容接口访问密钥；为空则不校验。
	// apiKeyMu 保护它：配置 reload 会运行时换值。
	apiKeyMu sync.RWMutex
	apiKey   string
	// concurrency 限制同时处理的 /v1/* 请求数。
	concurrency chan struct{}
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
	limit := serverConfig.MaxConcurrency
	if limit <= 0 {
		limit = defaultMaxConcurrency
	}
	return &App{
		adapter:      providerAdapter,
		serverConfig: serverConfig,
		debugManager: debugManager,
		concurrency:  make(chan struct{}, limit),
		wsConns:      make(chan struct{}, wsMaxConnections),
		metrics:      obs.NewMetrics(),
		startedAt:    time.Now(),
	}
}

// Metrics 返回常驻运行计数器，供面板 stats 端点读取。
func (application *App) Metrics() *obs.Metrics {
	return application.metrics
}

// SetAPIKey 设置 OpenAI 兼容接口的访问密钥；应在 Router/HTTPServer 之前调用。
func (application *App) SetAPIKey(apiKey string) {
	application.apiKeyMu.Lock()
	application.apiKey = apiKey
	application.apiKeyMu.Unlock()
}

// SetDashboard 注入管理面板处理器。
func (application *App) SetDashboard(d DashboardRegistrar) {
	application.dashboard = d
}

// SetVersion 记录构建版本，由 main 通过 -ldflags -X 注入。
func (application *App) SetVersion(version string) {
	application.version = version
}

// Router 返回应用的 chi HTTP 路由。
func (application *App) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", application.health)
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
		protected.Use(application.concurrencyMiddleware)
		protected.Get("/v1/models", application.listModels)
		protected.Get("/v1/models/{model}", application.getModel)
		protected.Post("/v1/responses", application.createResponses)
		protected.Post("/v1/chat/completions", application.createChatCompletions)
		protected.Post("/v1/messages", application.createMessages)
	})
	if application.dashboard != nil {
		application.dashboard.Register(router)
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
	return map[string]any{
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
	for _, m := range models {
		if m.ID == id {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(modelEntry(m))
			return
		}
	}
	writeJSONError(writer, http.StatusNotFound, fmt.Sprintf("model %q not found", id), "invalid_request_error")
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
func (application *App) BeginDrain() { application.draining.Store(true) }

// WaitDrain 阻塞到在途并发槽清空或 ctx 超时；超时返回错误，调用方负责强制 Close。
func (application *App) WaitDrain(ctx context.Context) error {
	return application.inflight.Wait(ctx)
}

// noteReject 统一记录一次管线前拒绝：分原因计数、事件环与进程日志同源。
// 这类请求没有调试目录与 index 行——计数/事件环供面板查，slog 行是唯一
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
	select {
	case application.concurrency <- struct{}{}:
		release = func() {
			<-application.concurrency
			application.inflight.Done()
		}
		return "", 0, release
	default:
		return obs.RejectConcurrencyLimit, http.StatusTooManyRequests, release
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

// apiKeyMiddleware 校验 OpenAI 兼容接口的 API Key。
// 支持标准 Authorization: Bearer <key> 与兼容头 X-Api-Key: <key>。
func (application *App) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		application.apiKeyMu.RLock()
		expected := application.apiKey
		application.apiKeyMu.RUnlock()
		if strings.TrimSpace(expected) == "" {
			next.ServeHTTP(writer, request)
			return
		}

		var provided string
		if auth := request.Header.Get("Authorization"); auth != "" {
			const prefix = "Bearer "
			if strings.HasPrefix(auth, prefix) {
				provided = strings.TrimSpace(auth[len(prefix):])
			}
		}
		if provided == "" {
			if key := request.Header.Get("X-Api-Key"); key != "" {
				provided = strings.TrimSpace(key)
			}
		}
		if provided == "" {
			application.noteReject(obs.RejectMissingAPIKey, request, http.StatusUnauthorized)
			writeAuthError(writer, "Missing API key")
			return
		}

		expectedHash := sha256.Sum256([]byte(expected))
		providedHash := sha256.Sum256([]byte(provided))
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			application.noteReject(obs.RejectInvalidAPIKey, request, http.StatusUnauthorized)
			writeAuthError(writer, "Invalid API key")
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
	recorder := application.debugManager.Start(debuglog.RequestMeta{
		Method:          request.Method,
		Path:            request.URL.Path,
		API:             api,
		ClientIP:        clientIP(request),
		UserAgent:       request.UserAgent(),
		KeyHash:         requestCredentialHash(request),
		ClientRequestID: clientRequestID(request),
	})
	// reqCtx 供面板 Abort 主动中断：cancel 挂到 recorder 上，
	// Complete 时 recorder 自动解除挂接，defer cancel 兜底释放。
	// WithCancelCause 让中断原因沿 ctx 链传到事件泵/上游 Recv——
	// 客户端看到的错误是「aborted via panel」而非裸 context.Canceled。
	reqCtx, cancel := context.WithCancelCause(request.Context())
	defer cancel(nil)
	recorder.SetAbort(func() {
		cancel(fmt.Errorf("aborted via panel request abort: %w", context.Canceled))
	})
	// Stripe Request-Id 模式：本地请求 id（即调试目录名）写进响应头，
	// agent 拿到后可直接查 index.jsonl 或 /panel/api/requests/{dir}。
	// 头部在首个字节写出时才提交，因此流式请求与中途错误同样生效。
	if ref := debugRef(recorder); ref != "" {
		writer.Header().Set("X-Request-Id", ref)
	}
	completion := debuglog.Completion{StatusCode: http.StatusInternalServerError, Result: "failed"}
	startedAt := time.Now()
	responseBytes := 0
	defer func() {
		recorder.Complete(completion)
		reqMetrics.Finish(completion.StatusCode, responseBytes, completion.Result)
		if completion.PrematureEndTurn {
			slog.Warn("premature end_turn", "dir", debugRef(recorder), "model", completion.Model)
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
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPRead, http.StatusRequestEntityTooLarge, fmt.Errorf("request payload exceeds the %d MiB limit", tooLarge.Limit>>20))
			return
		}
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPRead, http.StatusBadRequest, fmt.Errorf("read request: %w", err))
		return
	}
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
	body, err = protocol.EncodeFinal(message)
	if err != nil {
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPEncode, http.StatusInternalServerError, err)
		return
	}
	if err := out.writeContent(body); err != nil {
		completion.Result = "disconnected"
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
// 读 index.jsonl 计数时每个命中都要这样复核。
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
func requestCredentialHash(request *http.Request) string {
	credential := ""
	if auth := request.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		credential = strings.TrimSpace(auth[len("Bearer "):])
	} else if key := request.Header.Get("X-Api-Key"); key != "" {
		credential = strings.TrimSpace(key)
	}
	return hashCredential(credential)
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
	dir := filepath.Base(recorder.DirectoryPath())
	if dir == "." {
		return ""
	}
	return dir
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
	// 进程日志只出白名单信号 + 脱敏摘要；完整原文留在请求目录的 error.json。
	slog.Warn("request failed", "stage", stage, "status", status, "error", obs.Diagnostic(err))
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
