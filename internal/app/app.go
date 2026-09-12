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
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/go-chi/chi/v5"
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
	apiKey string
	// concurrency 限制同时处理的 /v1/* 请求数。
	concurrency chan struct{}
	// metrics 是常驻运行计数器；始终可用，供面板和进程日志消费。
	metrics *obs.Metrics
	// version 是构建注入的版本标识，healthz 透出供排障定位运行构建。
	version string
	// startedAt 是应用创建时间，供 healthz 报 uptime。
	startedAt time.Time
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
	application.apiKey = apiKey
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
		protected.Use(application.concurrencyMiddleware)
		protected.Use(application.apiKeyMiddleware)
		protected.Get("/v1/models", application.listModels)
		protected.Get("/v1/models/{model}", application.getModel)
		protected.Get("/v1/responses", application.createResponsesWebSocket)
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
	})
}

// listModels 返回 OpenAI 兼容的 GET /v1/models 列表。
func (application *App) listModels(writer http.ResponseWriter, request *http.Request) {
	if application.adapter == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "provider adapter is not configured")
		return
	}
	models, err := application.adapter.ListModels(request.Context())
	if err != nil {
		writeJSONError(writer, http.StatusBadGateway, err.Error())
		return
	}
	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		created := m.Created
		if created == 0 {
			created = time.Now().Unix()
		}
		ownedBy := m.OwnedBy
		if ownedBy == "" {
			ownedBy = "devin"
		}
		entry := map[string]any{
			"id": m.ID, "object": "model", "created": created, "owned_by": ownedBy,
		}
		// 非 OpenAI 标准字段，供面板/网关按能力做请求前 gate。
		entry["supports_images"] = m.SupportsImages
		entry["supports_tool_calls"] = m.SupportsToolCalls
		entry["supports_parallel_tool_calls"] = m.SupportsParallelToolCalls
		entry["supports_thinking"] = m.SupportsThinking
		entry["preserve_thinking"] = m.PreserveThinking
		if m.ContextTokens > 0 {
			entry["context_tokens"] = m.ContextTokens
		}
		if m.MaxOutputTokens > 0 {
			entry["max_output_tokens"] = m.MaxOutputTokens
		}
		data = append(data, entry)
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
}

// getModel 返回 OpenAI 兼容的 GET /v1/models/{model}。
func (application *App) getModel(writer http.ResponseWriter, request *http.Request) {
	if application.adapter == nil {
		writeJSONError(writer, http.StatusServiceUnavailable, "provider adapter is not configured")
		return
	}
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
			created := m.Created
			if created == 0 {
				created = time.Now().Unix()
			}
			ownedBy := m.OwnedBy
			if ownedBy == "" {
				ownedBy = "devin"
			}
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": m.ID, "object": "model", "created": created, "owned_by": ownedBy,
				"supports_images":              m.SupportsImages,
				"supports_tool_calls":          m.SupportsToolCalls,
				"supports_parallel_tool_calls": m.SupportsParallelToolCalls,
				"supports_thinking":            m.SupportsThinking,
				"preserve_thinking":            m.PreserveThinking,
				"context_tokens":               m.ContextTokens,
				"max_output_tokens":            m.MaxOutputTokens,
			})
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

// concurrencyMiddleware 限制同时处理的 /v1/* 请求数，避免上游阻塞时资源耗尽。
func (application *App) concurrencyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case application.concurrency <- struct{}{}:
			defer func() { <-application.concurrency }()
			next.ServeHTTP(writer, request)
		default:
			application.metrics.Reject()
			slog.Warn("request rejected", "reason", "concurrency_limit", "path", request.URL.Path, "client_ip", clientIP(request))
			writeJSONError(writer, http.StatusServiceUnavailable, "server is busy, please try again later")
		}
	})
}

// apiKeyMiddleware 校验 OpenAI 兼容接口的 API Key。
// 支持标准 Authorization: Bearer <key> 与兼容头 X-Api-Key: <key>。
func (application *App) apiKeyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.TrimSpace(application.apiKey) == "" {
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
			application.metrics.Reject()
			slog.Warn("request rejected", "reason", "missing_api_key", "path", request.URL.Path, "client_ip", clientIP(request))
			writeAuthError(writer, "Missing API key")
			return
		}

		expectedHash := sha256.Sum256([]byte(application.apiKey))
		providedHash := sha256.Sum256([]byte(provided))
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			application.metrics.Reject()
			slog.Warn("request rejected", "reason", "invalid_api_key", "path", request.URL.Path, "client_ip", clientIP(request), "key_hash", hashCredential(provided))
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
	reqCtx, cancel := context.WithCancel(request.Context())
	defer cancel()
	recorder.SetAbort(cancel)
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

	if application.adapter == nil {
		completion.StatusCode = http.StatusServiceUnavailable
		writeLoggedError(writer, recorder, "provider_configuration", completion.StatusCode, errors.New("provider adapter is not configured"))
		return
	}
	// 图片 base64 会显著放大 JSON；与常见 IDE 多图请求对齐到 32MiB。
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 32<<20))
	if err != nil {
		completion.StatusCode = http.StatusBadRequest
		writeLoggedError(writer, recorder, "http_read", completion.StatusCode, fmt.Errorf("read request: %w", err))
		return
	}
	recorder.WriteJSON("01-http-request.json", httpRequestProjection(request, body))
	messages, options, err := decoder(body)
	if err != nil {
		completion.StatusCode = http.StatusBadRequest
		writeLoggedError(writer, recorder, "http_decode", completion.StatusCode, err)
		return
	}
	completion.Model = messages.Model
	completion.RequestedModel = messages.Model
	completion.Stream = options.Stream
	reqMetrics.Observe(options.Stream, len(body))
	recorder.SetModel(messages.Model)
	recorder.WriteJSON("02-request-messages.json", debuglog.RequestMessagesProjection(messages))
	ctx := debuglog.WithRecorder(reqCtx, recorder)
	if options.Stream {
		application.streamCompletion(ctx, writer, recorder, protocol, messages, options, &completion, &responseBytes)
		return
	}
	stream, err := application.adapter.Stream(ctx, messages)
	if err != nil {
		completion.StatusCode = mapProviderErrorStatus(err)
		writeLoggedError(writer, recorder, "provider_stream", completion.StatusCode, err)
		return
	}
	message, err := collectFinalMessage(ctx, stream, recorder)
	if err != nil {
		completion.StatusCode = mapProviderErrorStatus(err)
		writeLoggedError(writer, recorder, "response_event", completion.StatusCode, err)
		return
	}
	updateCompletionIdentity(&completion, messages, message)
	body, err = protocol.EncodeFinal(message)
	if err != nil {
		completion.StatusCode = http.StatusInternalServerError
		writeLoggedError(writer, recorder, "http_encode", completion.StatusCode, err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	if _, err := writer.Write(body); err != nil {
		completion.Result = "disconnected"
		recorder.WriteError("client_disconnected", err)
		return
	}
	recorder.NoteClientLatency()
	recorder.AddClientBytes(int64(len(body)))
	responseBytes += len(body)
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
// （docs/2026-09-12-premature-endturn.md），记入日志供统计真实频率。
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
	for _, header := range []string{"X-Request-Id", "X-Session-Id"} {
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
			"accept":       request.Header.Get("Accept"),
			"content_type": request.Header.Get("Content-Type"),
			"user_agent":   request.Header.Get("User-Agent"),
		},
		"body": parsedBody,
	}
}

func writeLoggedError(writer http.ResponseWriter, recorder *debuglog.Recorder, stage string, status int, err error) {
	recorder.WriteError(stage, err)
	// 进程日志只出白名单信号 + 脱敏摘要；完整原文留在请求目录的 error.json。
	slog.Warn("request failed", "stage", stage, "status", status, "error", obs.Diagnostic(err))
	message := err.Error()
	errorType := common.OpenAIErrorType(message)
	// 客户端可修正的错误用 invalid_request_error，便于 IDE 直接展示。
	if status == http.StatusBadRequest ||
		strings.Contains(message, "does not support image") ||
		strings.Contains(message, "invalid_argument") ||
		strings.HasPrefix(message, "invalid_argument:") {
		errorType = "invalid_request_error"
		if status >= 500 {
			status = http.StatusBadRequest
		}
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	// 透传完整 message，不改写上游文案；stage 标明失败发生在哪一层，
	// debug_ref 是本地调试目录名，agent 凭它一次调用即可拿到全部证据。
	payload := map[string]any{
		"message": message,
		"type":    errorType,
		"code":    common.ErrorCode(message),
		"param":   nil,
		"stage":   stage,
	}
	if ref := debugRef(recorder); ref != "" {
		payload["debug_ref"] = ref
	}
	response := map[string]any{"error": payload}
	_ = json.NewEncoder(writer).Encode(response)
	recorder.AppendJSONL("06-http-response.jsonl", "error", response)
}

// mapProviderErrorStatus 将上游/适配器错误映射为合适的 HTTP 状态，message 仍原样透传。
// Connect 编码的上游错误交给 common.HTTPStatus；本地适配器产生的错误先按内容匹配。
func mapProviderErrorStatus(err error) int {
	if err == nil {
		return http.StatusBadGateway
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "does not support image"),
		strings.Contains(msg, "file_id images"),
		strings.Contains(msg, "only data URL"),
		strings.Contains(msg, "validate Devin request"),
		strings.Contains(msg, "validate adapted request"):
		return http.StatusBadRequest
	default:
		return common.HTTPStatus(msg)
	}
}
