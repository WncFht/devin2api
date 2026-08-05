// 本文件定义 HTTP 应用、chi 路由和供应商适配器的串联逻辑。
//
// Package app 负责组装 HTTP 路由并连接 API 编解码与供应商适配器。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/leookun/devin-2api/internal/adapter"
	"github.com/leookun/devin-2api/internal/api/openai/responses"
	"github.com/leookun/devin-2api/internal/config"
	"github.com/leookun/devin-2api/internal/debuglog"
	"github.com/leookun/devin-2api/internal/llm"
)

const (
	// readHeaderTimeout 是防止慢速请求头连接长期占用资源的内部策略。
	readHeaderTimeout = 60 * time.Second
	// idleTimeout 是 keep-alive 连接两次请求之间的内部空闲策略。
	idleTimeout = 360 * time.Second
)

// App 保存 HTTP 应用依赖和服务配置。
type App struct {
	// adapter 是供应商无关请求与上游协议之间的适配器。
	adapter adapter.Adapter
	// serverConfig 是 HTTP 服务运行配置。
	serverConfig config.ServerConfig
	// debugManager 为每次兼容 API 请求创建独立的写盘日志。
	debugManager *debuglog.Manager
}

// New 创建一个使用指定供应商适配器的 HTTP 应用。
func New(providerAdapter adapter.Adapter, serverConfig config.ServerConfig, debugManager *debuglog.Manager) *App {
	return &App{adapter: providerAdapter, serverConfig: serverConfig, debugManager: debugManager}
}

// Router 返回应用的 chi HTTP 路由。
func (application *App) Router() http.Handler {
	router := chi.NewRouter()
	router.Get("/healthz", application.health)
	router.Post("/v1/responses", application.createResponses)
	return router
}

// HTTPServer 创建带有应用路由和超时配置的 HTTP 服务。
func (application *App) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              application.serverConfig.Listen,
		Handler:           application.Router(),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
	}
}

func (application *App) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write([]byte(`{"status":"ok"}` + "\n"))
}

func (application *App) createResponses(writer http.ResponseWriter, request *http.Request) {
	recorder := application.debugManager.Start(debuglog.RequestMeta{Method: request.Method, Path: request.URL.Path})
	completion := debuglog.Completion{StatusCode: http.StatusInternalServerError, Result: "failed"}
	defer func() { recorder.Complete(completion) }()

	if application.adapter == nil {
		completion.StatusCode = http.StatusServiceUnavailable
		writeLoggedError(writer, recorder, "provider_configuration", completion.StatusCode, errors.New("provider adapter is not configured"))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, 8<<20))
	if err != nil {
		completion.StatusCode = http.StatusBadRequest
		writeLoggedError(writer, recorder, "http_read", completion.StatusCode, fmt.Errorf("read request: %w", err))
		return
	}
	recorder.WriteJSON("01-http-request.json", httpRequestProjection(request, body))
	adapted, err := responses.DecodeRequest(body)
	if err != nil {
		completion.StatusCode = http.StatusBadRequest
		writeLoggedError(writer, recorder, "http_decode", completion.StatusCode, err)
		return
	}
	completion.Model = adapted.Options.Model
	completion.Stream = adapted.Options.Stream
	recorder.WriteJSON("02-request-messages.json", debuglog.RequestMessagesProjection(adapted.Context))
	ctx := debuglog.WithRecorder(request.Context(), recorder)
	stream, err := application.adapter.Stream(ctx, adapted.Context)
	if err != nil {
		completion.StatusCode = http.StatusBadGateway
		writeLoggedError(writer, recorder, "provider_stream", completion.StatusCode, err)
		return
	}
	if adapted.Options.Stream {
		completion.StatusCode = http.StatusOK
		message, streamErr := writeSSE(ctx, writer, stream, recorder, adapted.Options.Model)
		updateCompletionIdentity(&completion, message)
		if streamErr != nil {
			if errors.Is(streamErr, context.Canceled) || errors.Is(streamErr, context.DeadlineExceeded) {
				completion.Result = "disconnected"
				recorder.WriteError("client_disconnected", streamErr)
			} else {
				recorder.WriteError("http_stream", streamErr)
			}
			return
		}
		completion.Result = "completed"
		return
	}
	message, err := collectFinalMessage(ctx, stream, recorder)
	if err != nil {
		completion.StatusCode = http.StatusBadGateway
		writeLoggedError(writer, recorder, "response_event", completion.StatusCode, err)
		return
	}
	updateCompletionIdentity(&completion, message)
	body, err = responses.EncodeResponse(message)
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
	recorder.AppendJSONL("06-http-response.jsonl", "response", json.RawMessage(body))
	completion.StatusCode = http.StatusOK
	completion.Result = "completed"
}

func writeSSE(ctx context.Context, writer http.ResponseWriter, stream llm.ResponseStream, recorder *debuglog.Recorder, model string) (*llm.AssistantMessage, error) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming response writer does not support flushing")
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	encoder := responses.NewStreamEncoder(model)
	var latest *llm.AssistantMessage
	for {
		event, err := receiveEvent(ctx, stream, recorder)
		if errors.Is(err, io.EOF) {
			return latest, nil
		}
		if err != nil {
			return latest, err
		}
		latest = eventMessage(event, latest)
		encodedEvents, err := encoder.Encode(event)
		if err != nil {
			return latest, err
		}
		for _, encoded := range encodedEvents {
			if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", encoded.Name, encoded.Data); err != nil {
				return latest, err
			}
			recorder.AppendJSONL("06-http-response.jsonl", encoded.Name, json.RawMessage(encoded.Data))
			flusher.Flush()
		}
		if event.Type == llm.ResponseEventError {
			if event.Error != nil && event.Error.ErrorMessage != "" {
				return latest, errors.New(event.Error.ErrorMessage)
			}
			return latest, errors.New("response stream returned an error event")
		}
	}
}

func collectFinalMessage(ctx context.Context, stream llm.ResponseStream, recorder *debuglog.Recorder) (*llm.AssistantMessage, error) {
	var final *llm.AssistantMessage
	for {
		event, err := receiveEvent(ctx, stream, recorder)
		if errors.Is(err, io.EOF) {
			if final == nil {
				return nil, errors.New("response stream ended without a final message")
			}
			return final, nil
		}
		if err != nil {
			return nil, err
		}
		switch event.Type {
		case llm.ResponseEventDone:
			if event.Message == nil {
				return nil, errors.New("done event has no final message")
			}
			final = event.Message
		case llm.ResponseEventError:
			if event.Error == nil {
				return nil, errors.New("error event has no error message")
			}
			return nil, errors.New(event.Error.ErrorMessage)
		}
	}
}

func receiveEvent(ctx context.Context, stream llm.ResponseStream, recorder *debuglog.Recorder) (llm.ResponseEvent, error) {
	event, err := stream.Recv(ctx)
	if err == nil {
		recorder.AppendJSONL("05-response-events.jsonl", string(event.Type), debuglog.ResponseEventProjection(event))
	}
	return event, err
}

func eventMessage(event llm.ResponseEvent, fallback *llm.AssistantMessage) *llm.AssistantMessage {
	if event.Message != nil {
		return event.Message
	}
	if event.Error != nil {
		return event.Error
	}
	if event.Partial != nil {
		return event.Partial
	}
	return fallback
}

func updateCompletionIdentity(completion *debuglog.Completion, message *llm.AssistantMessage) {
	if message == nil {
		return
	}
	completion.Provider = message.Provider
	if message.ResponseModel != "" {
		completion.Model = message.ResponseModel
	} else if message.Model != "" {
		completion.Model = message.Model
	}
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
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	response := map[string]any{"error": map[string]string{"message": err.Error(), "type": "server_error"}}
	_ = json.NewEncoder(writer).Encode(response)
	recorder.AppendJSONL("06-http-response.jsonl", "error", response)
}
