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
	inflight sync.WaitGroup
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
	})
}

// modelEntry 把目录条目投影为 OpenAI /v1/models 形状；列表与详情端点
// 共用同一份字段集，避免两处漂移。非 OpenAI 标准字段供面板/网关按能力
// 做请求前 gate（含 is_model_router：router uid 直连上游会被拒）。
func modelEntry(m adapter.ModelInfo) map[string]any {
	created := m.Created
	if created == 0 {
		created = time.Now().Unix()
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
		writeJSONError(writer, http.StatusBadGateway, err.Error())
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
		writeJSONError(writer, http.StatusBadRequest, "model id is required")
		return
	}
	models, err := application.adapter.ListModels(request.Context())
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, err.Error())
		return
	}
	for _, m := range models {
		if m.ID == id {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(modelEntry(m))
			return
		}
	}
	writeJSONError(writer, http.StatusNotFound, fmt.Sprintf("model %q not found", id))
}

func writeJSONError(writer http.ResponseWriter, status int, message string) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{"message": message, "type": "invalid_request_error", "code": nil, "param": nil},
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

// BeginDrain 进入排空态：新请求快速 503，在途请求继续跑完。
// listener 保持开启由调用方控制——http.Server.Shutdown 会先关 listener 再
// 等在途连接，排空期整段变成 connection refused；这里改为排空结束才 Close。
func (application *App) BeginDrain() { application.draining.Store(true) }

// Draining 报告是否处于排空态，供 healthz 透出。
func (application *App) Draining() bool { return application.draining.Load() }

// WaitDrain 阻塞到在途并发槽清空或 ctx 超时；超时返回错误，调用方负责强制 Close。
func (application *App) WaitDrain(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		application.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// concurrencyMiddleware 限制同时处理的 /v1/* 请求数，避免上游阻塞时资源耗尽。
func (application *App) concurrencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// Add 先于 draining 检查：排空等待才能覆盖所有已经进入的请求，
		// 被拒绝的请求瞬时 Done，不占排空时间。
		application.inflight.Add(1)
		defer application.inflight.Done()
		if application.draining.Load() {
			application.noteReject(obs.RejectDraining, request, http.StatusServiceUnavailable)
			writeDrainingError(writer)
			return
		}
		select {
		case application.concurrency <- struct{}{}:
			defer func() { <-application.concurrency }()
			next.ServeHTTP(writer, request)
		default:
			application.noteReject(obs.RejectConcurrencyLimit, request, http.StatusTooManyRequests)
			writeRateLimitError(writer, "server is busy, please try again later")
		}
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
	api := "openai-responses"
	if _, isWS := writer.(*wsResponseWriter); isWS {
		api = "responses-ws"
	}
	application.createCompletion(writer, request, api, decodeResponsesRequest, responsesProtocol{})
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
			completion.StatusCode = http.StatusRequestEntityTooLarge
			writeLoggedError(writer, recorder, protocol, "http_read", completion.StatusCode, fmt.Errorf("request payload exceeds the %d MiB limit", tooLarge.Limit>>20))
			return
		}
		completion.StatusCode = http.StatusBadRequest
		writeLoggedError(writer, recorder, protocol, "http_read", completion.StatusCode, fmt.Errorf("read request: %w", err))
		return
	}
	if recorder != nil {
		// 投影会对 body 再做一次 generic unmarshal；recorder 为 nil 时
		// WriteJSON 是 no-op，参数表达式却仍会求值——必须在外层门控。
		recorder.WriteJSON("01-http-request.json", httpRequestProjection(request, body))
	}
	messages, options, err := decoder(body)
	if err != nil {
		completion.StatusCode = http.StatusBadRequest
		writeLoggedError(writer, recorder, protocol, "http_decode", completion.StatusCode, err)
		return
	}
	completion.Model = messages.Model
	completion.RequestedModel = messages.Model
	completion.Stream = options.Stream
	reqMetrics.Observe(options.Stream, len(body))
	recorder.SetModel(messages.Model)
	if recorder != nil {
		recorder.WriteJSON("02-request-messages.json", debuglog.RequestMessagesProjection(messages))
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
		noteRetryAfter(recorder, err.Error())
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			completion.Result = "disconnected"
			recorder.WriteError("client_disconnected", err)
			return
		}
		if out.committed {
			// 心跳已把状态提交为 200，错误只能以协议错误体下发——
			// 前导 \n 是合法 JSON 空白，客户端解析出 error 字段。
			completion.StatusCode = http.StatusOK
			if writeErr := out.writeContent(protocol.EncodeError(err, debugRef(recorder))); writeErr != nil {
				completion.Result = "disconnected"
				recorder.WriteError("client_disconnected", writeErr)
				return
			}
			recorder.WriteError("response_event", err)
			return
		}
		completion.StatusCode = mapProviderErrorStatus(err)
		writeLoggedError(writer, recorder, protocol, "response_event", completion.StatusCode, err)
		return
	}
	updateCompletionIdentity(&completion, messages, message)
	body, err = protocol.EncodeFinal(message)
	if err != nil {
		completion.StatusCode = http.StatusInternalServerError
		writeLoggedError(writer, recorder, protocol, "http_encode", completion.StatusCode, err)
		return
	}
	if err := out.writeContent(body); err != nil {
		completion.Result = "disconnected"
		recorder.WriteError("client_disconnected", err)
		return
	}
	responseBytes += out.bytes
	recorder.AppendJSONL("06-http-response.jsonl", "response", json.RawMessage(body))
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

// prematureEndTurn 识别可疑的正常收尾：请求最后一条输入是工具结果，
// 模型却以无工具调用的 end_turn 结束。该形态结构上合法（可能真是
// 最终答复），但实测存在模型声称继续动作后直接 EOS 的故障模式
// （notes/archive/2026-09-12-premature-endturn.md），记入日志供统计真实频率。
// prematureEndTurn 标记疑似提前收轮：末条输入是 tool_result、响应无
// toolCall 却声明 STOP——形似「宣告要做事却直接结束」。这是候选信号
// 而非判定：任务正常收官（末轮 tool_result → 总结文本 → STOP）形状完全
// 相同，只能靠语义（宣告式 vs 总结式）或会话是否终结来区分，读
// index.jsonl 计数时每个命中都要这样复核。
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
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

func httpRequestProjection(request *http.Request, body []byte) map[string]any {
	var parsedBody any
	if err := json.Unmarshal(body, &parsedBody); err != nil {
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

func writeLoggedError(writer http.ResponseWriter, recorder *debuglog.Recorder, protocol protocolEncoder, stage string, status int, err error) {
	recorder.WriteError(stage, err)
	// 进程日志只出白名单信号 + 脱敏摘要；完整原文留在请求目录的 error.json。
	slog.Warn("request failed", "stage", stage, "status", status, "error", obs.Diagnostic(err))
	message := err.Error()
	// 客户端可修正的错误统一报 invalid_request_error（两个协议对该语义
	// 同名），便于 IDE 直接展示；状态码同样压回 4xx。
	clientFixable := status == http.StatusBadRequest ||
		status == http.StatusRequestEntityTooLarge ||
		strings.Contains(message, "does not support image") ||
		strings.Contains(message, "invalid_argument")
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
		if resetAt, ok := common.RateLimitReset(message, time.Now()); ok {
			wait := int(math.Ceil(time.Until(resetAt).Seconds()))
			writer.Header().Set("Retry-After", strconv.Itoa(wait))
			writer.Header().Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(resetAt.Unix(), 10))
		}
	}
	noteRetryAfter(recorder, message)
	writer.WriteHeader(status)
	// 错误体按客户端协议成形：/v1/messages 必须回 Anthropic 信封，
	// 否则 Claude Code 解析不出 error 字段。stage 标明失败发生在哪一层，
	// debug_ref 是本地调试目录名，agent 凭它一次调用即可拿到全部证据。
	body := protocol.EncodeHTTPError(httpError{
		Message: message, ClientFixable: clientFixable,
		Stage: stage, DebugRef: debugRef(recorder),
	})
	_, _ = writer.Write(body)
	recorder.AppendJSONL("06-http-response.jsonl", "error", json.RawMessage(body))
}

// noteRetryAfter 把上游限流文案里的 reset 秒数记进请求日志——无论它最终
// 走 Retry-After 头（未提交 429）还是已提交后的错误体下发，索引里都有可查的
// 结构化 hint，grep/聚合不必再解析文案。
func noteRetryAfter(recorder *debuglog.Recorder, message string) {
	if seconds, ok := common.RetryAfterSeconds(message); ok {
		recorder.SetRetryAfter(seconds)
	}
}

// mapProviderErrorStatus 将上游/适配器错误映射为合适的 HTTP 状态，message 仍原样透传。
// Connect 编码的上游错误交给 common.HTTPStatus；本地适配器产生的错误先按内容匹配。
// 客户端取消映射 499（nginx 约定）、上游超时 504：客户端主动断开计成
// 502 会污染指标并让网关误判渠道故障。
func mapProviderErrorStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout
	}
	if errors.Is(err, context.Canceled) {
		return 499
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "does not support image"),
		strings.Contains(msg, "file_id images"),
		strings.Contains(msg, "only data URL"),
		strings.Contains(msg, "validate Devin request"),
		strings.Contains(msg, "validate adapted request"):
		return http.StatusBadRequest
	case strings.Contains(msg, "context deadline exceeded"):
		// 上游 ctx 错误可能在事件层被展平成字符串，errors.Is 已接不到。
		return http.StatusGatewayTimeout
	case strings.Contains(msg, "context canceled"):
		return 499
	default:
		return common.HTTPStatus(msg)
	}
}
