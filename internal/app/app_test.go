// 本文件验证 app 能把 OpenAI HTTP 请求交给 adapter，并按顺序输出 Responses SSE。
package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

type fakeAdapter struct {
	lastRequest llm.RequestMessages
	events      []llm.ResponseEvent
}

func (fake *fakeAdapter) Stream(_ context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	fake.lastRequest = request
	return &fakeStream{events: fake.events}, nil
}

func (fake *fakeAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "gpt-test", Created: 1, OwnedBy: "test"}}, nil
}

type fakeStream struct {
	events []llm.ResponseEvent
	index  int
}

func (stream *fakeStream) Recv(_ context.Context) (llm.ResponseEvent, error) {
	if stream.index >= len(stream.events) {
		return llm.ResponseEvent{}, io.EOF
	}
	event := stream.events[stream.index]
	stream.index++
	return event, nil
}

// concurrentFakeAdapter 为每个 model 返回独立的事件流，用于并发隔离测试。
type concurrentFakeAdapter struct {
	mu     sync.Mutex
	events map[string][]llm.ResponseEvent
}

func (c *concurrentFakeAdapter) Stream(_ context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	c.mu.Lock()
	events := c.events[request.Model]
	c.mu.Unlock()
	return &fakeStream{events: events}, nil
}

func (c *concurrentFakeAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "model-a", Created: 1, OwnedBy: "test"}, {ID: "model-b", Created: 1, OwnedBy: "test"}, {ID: "model-c", Created: 1, OwnedBy: "test"}}, nil
}

// TestResponsesHandlerStreamsOrderedEvents 验证请求路由和 SSE 事件顺序。
func TestResponsesHandlerStreamsOrderedEvents(t *testing.T) {
	final := &llm.AssistantMessage{
		ResponseID:    "resp-1",
		ResponseModel: "gpt-test",
		Content:       []llm.Content{llm.TextContent{Text: "hello"}},
		StopReason:    llm.StopReasonStop,
	}
	partial := &llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}, StopReason: llm.StopReasonPending}
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{ResponseID: "resp-1", StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "hello", Partial: partial},
		{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: "hello", Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final},
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if strings.Index(body, "response.created") > strings.Index(body, "response.output_item.added") || strings.Index(body, "response.output_text.delta") > strings.Index(body, "response.completed") {
		t.Fatalf("events are out of order: %s", body)
	}
	for _, expected := range []string{"response.in_progress", "response.content_part.added", "response.content_part.done", "response.output_item.done", `"output":[{"content"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body = %s, want %s", body, expected)
		}
	}
	if len(fake.lastRequest.Messages) != 1 {
		t.Fatalf("adapter message count = %d, want 1", len(fake.lastRequest.Messages))
	}
}

// TestResponsesHandlerReturnsJSONForNonStream 验证非流式请求返回最终 JSON。
func TestResponsesHandlerReturnsJSONForNonStream(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventDone,
		Reason: llm.StopReasonStop,
		Message: &llm.AssistantMessage{
			ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop,
		},
	}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"object":"response"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// TestResponsesHandlerConsumesAllAssistantRounds 的测试动机是保证非流式模式消费完整事件流并返回最后一轮助手内容。
func TestResponsesHandlerConsumesAllAssistantRounds(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonToolUse, Message: &llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "first"}}, ResponseModel: "gpt-test", StopReason: llm.StopReasonToolUse}},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "last"}}, ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}},
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"text":"last"`) || strings.Contains(response.Body.String(), `"text":"first"`) {
		t.Fatalf("body = %s, want only last assistant round", response.Body.String())
	}
}

// TestResponsesHandlerWritesStageLogs 的测试动机是保证 HTTP 边界和中间响应事件可以按一次请求完整回放。
func TestResponsesHandlerWritesStageLogs(t *testing.T) {
	final := &llm.AssistantMessage{
		Provider: "devin", ResponseID: "resp-1", ResponseModel: "model", StopReason: llm.StopReasonStop,
	}
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final}}}
	root := filepath.Join(t.TempDir(), "logs")
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root, 0, 0))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var requestDirs []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			requestDirs = append(requestDirs, entry)
		}
	}
	if len(requestDirs) != 1 {
		t.Fatalf("request directory count = %d, want 1", len(requestDirs))
	}
	entries = requestDirs
	directory := filepath.Join(root, entries[0].Name())
	for _, name := range []string{"meta.json", "01-http-request.json", "02-request-messages.json", "05-response-events.jsonl", "06-http-response.jsonl"} {
		if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	meta, err := os.ReadFile(filepath.Join(directory, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"result": "completed"`) || !strings.Contains(string(meta), `"provider": "devin"`) {
		t.Fatalf("meta = %s", meta)
	}
}

// TestResponsesHandlerMarksStreamError 的测试动机是避免已输出失败 SSE 的请求被误记为成功。
func TestResponsesHandlerMarksStreamError(t *testing.T) {
	failed := &llm.AssistantMessage{Provider: "devin", StopReason: llm.StopReasonError, ErrorMessage: "upstream failed"}
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventStart},
		{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: failed},
	}}
	root := filepath.Join(t.TempDir(), "logs")
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root, 0, 0))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, entries[0].Name())
	meta, err := os.ReadFile(filepath.Join(directory, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"result": "failed"`) {
		t.Fatalf("meta = %s", meta)
	}
	errorLog, err := os.ReadFile(filepath.Join(directory, "error.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(errorLog), `"stage": "http_stream"`) {
		t.Fatalf("error = %s", errorLog)
	}
}

// TestResponsesHandlerRejectsMissingAPIKey 验证未提供密钥时 /v1/* 返回 401。
func TestResponsesHandlerRejectsMissingAPIKey(t *testing.T) {
	fake := &fakeAdapter{}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAPIKey("secret-key")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"type":"unauthenticated"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// TestResponsesHandlerRejectsInvalidAPIKey 验证错误密钥无法通过鉴权。
func TestResponsesHandlerRejectsInvalidAPIKey(t *testing.T) {
	fake := &fakeAdapter{}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAPIKey("secret-key")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Authorization", "Bearer wrong-key")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerAcceptsBearerAPIKey 验证 Authorization: Bearer <key> 通用格式可用。
func TestResponsesHandlerAcceptsBearerAPIKey(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAPIKey("secret-key")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret-key")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerAcceptsXApiKeyHeader 验证兼容头 X-Api-Key 也可用。
func TestResponsesHandlerAcceptsXApiKeyHeader(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAPIKey("secret-key")
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "secret-key")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerHealthIsUnprotected 验证 /healthz 不受 API Key 保护。
func TestResponsesHandlerHealthIsUnprotected(t *testing.T) {
	application := New(&fakeAdapter{}, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAPIKey("secret-key")
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerIgnoresLogInitializationFailure 的测试动机是保证诊断写盘故障不会改变兼容 API 的业务结果。
func TestResponsesHandlerIgnoresLogInitializationFailure(t *testing.T) {
	final := &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "model", StopReason: llm.StopReasonStop}
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final}}}
	blockedRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedRoot, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(blockedRoot, 0, 0))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestConcurrentResponsesDoNotInterleave 验证高并发下每个请求的响应流互不串扰。
func TestConcurrentResponsesDoNotInterleave(t *testing.T) {
	models := []string{"model-a", "model-b", "model-c"}
	events := make(map[string][]llm.ResponseEvent)
	for _, m := range models {
		events[m] = []llm.ResponseEvent{{
			Type:   llm.ResponseEventDone,
			Reason: llm.StopReasonStop,
			Message: &llm.AssistantMessage{
				ResponseID:    "resp-" + m,
				ResponseModel: m,
				Content:       []llm.Content{llm.TextContent{Text: "unique-response-for-" + m}},
				StopReason:    llm.StopReasonStop,
			},
		}}
	}
	fake := &concurrentFakeAdapter{events: events}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for _, m := range models {
			wg.Add(1)
			go func(model string) {
				defer wg.Done()
				body := fmt.Sprintf(`{"model":"%s","input":"hi"}`, model)
				request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				response := httptest.NewRecorder()
				application.Router().ServeHTTP(response, request)
				if response.Code != http.StatusOK {
					t.Errorf("status = %d for model %s: %s", response.Code, model, response.Body.String())
					return
				}
				want := "unique-response-for-" + model
				bodyStr := response.Body.String()
				if !strings.Contains(bodyStr, want) {
					t.Errorf("response for %s missing %q: %s", model, want, bodyStr)
				}
				// 同时确认没有其它 model 的标记串入。
				for _, other := range models {
					if other == model {
						continue
					}
					if strings.Contains(bodyStr, "unique-response-for-"+other) {
						t.Errorf("response for %s contains marker of %s: %s", model, other, bodyStr)
					}
				}
			}(m)
		}
	}
	wg.Wait()
}

// gatedStream 在首个 Recv 前阻塞在 gate 上，用来模拟上游长静默。
type gatedStream struct {
	gate   chan struct{}
	events []llm.ResponseEvent
	index  int
}

// Recv 首次调用等待 gate 关闭，之后按序返回事件。
func (stream *gatedStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	if stream.index == 0 {
		select {
		case <-stream.gate:
		case <-ctx.Done():
			return llm.ResponseEvent{}, ctx.Err()
		}
	}
	if stream.index >= len(stream.events) {
		return llm.ResponseEvent{}, io.EOF
	}
	event := stream.events[stream.index]
	stream.index++
	return event, nil
}

// gatedAdapter 返回阻塞在 gate 上的流，用于保活测试。
type gatedAdapter struct {
	stream *gatedStream
}

// Stream 返回预设的阻塞流。
func (fake *gatedAdapter) Stream(context.Context, llm.RequestMessages) (llm.ResponseStream, error) {
	return fake.stream, nil
}

// ListModels 返回空模型目录。
func (fake *gatedAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return nil, nil
}

// TestStreamKeepsAliveDuringUpstreamSilence 的测试动机是上游长思考期间
// 连接不能保持完全静默：Codex 约 30s 无数据弃连，网关也有空闲超时。
// SSE 注释行是合法的保活手段，客户端解析器会忽略。
func TestStreamKeepsAliveDuringUpstreamSilence(t *testing.T) {
	original := keepaliveInterval
	keepaliveInterval = 20 * time.Millisecond
	defer func() { keepaliveInterval = original }()

	final := &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}
	gate := make(chan struct{})
	stream := &gatedStream{gate: gate, events: []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{ResponseID: "resp-1", StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final},
	}}
	application := New(&gatedAdapter{stream: stream}, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		application.Router().ServeHTTP(response, request)
		close(done)
	}()
	// 上游静默 3 个保活周期后才放行首批事件。
	time.Sleep(3 * keepaliveInterval)
	close(gate)
	<-done
	if !strings.Contains(response.Body.String(), ": keepalive") {
		t.Fatalf("body missing SSE keepalive comments: %q", response.Body.String())
	}
}

// TestStreamImmediateErrorReturnsHTTPStatus 的测试动机是上游在产出任何内容
// 前失败时，HTTP 状态必须是真实错误码：下游网关据此区分请求级错误与渠道
// 故障，已提交的 200 + SSE error 会被误判并触发渠道冷却。
func TestStreamImmediateErrorReturnsHTTPStatus(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventError,
		Reason: llm.StopReasonError,
		Error:  &llm.AssistantMessage{ErrorMessage: "permission_denied: blocked by content policy"},
	}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
}

// TestStreamPromptTooLongDeliversSSE413 验证上下文超长错误的 Codex 契约：
// 裸 HTTP 413 会让下游网关物化成错误响应，Codex 永远收不到
// response.failed。正确形态是提交 200 + SSE error 事件（顶层 status=413、
// error.code=context_length_exceeded），客户端据此自动压缩重试。
func TestStreamPromptTooLongDeliversSSE413(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventError,
		Reason: llm.StopReasonError,
		Error:  &llm.AssistantMessage{ErrorMessage: "invalid_argument: The prompt is too long for this model"},
	}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed 200: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "response.failed") {
		t.Fatalf("missing response.failed event: %s", body)
	}
	if !strings.Contains(body, `"status":413`) || !strings.Contains(body, `"context_length_exceeded"`) {
		t.Fatalf("error event missing 413/context_length_exceeded: %s", body)
	}
}

// TestStreamMidStreamErrorCarriesHTTPStatus 的测试动机是已提交 200 之后到达的
// 错误事件必须携带顶层 status 字段，让下游网关按真实语义分类。
func TestStreamMidStreamErrorCarriesHTTPStatus(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{ResponseID: "resp-1", StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventError, Reason: llm.StopReasonError,
			Error: &llm.AssistantMessage{ErrorMessage: "unavailable: connection reset"}},
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed 200: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"status":502`) {
		t.Fatalf("error event missing top-level status: %s", response.Body.String())
	}
}

// TestRequestIDHeaderAndDebugRef 验证响应头与错误体都携带本地调试目录名，
// agent 无需猜测即可定位完整日志。
func TestRequestIDHeaderAndDebugRef(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type: llm.ResponseEventError, Reason: llm.StopReasonError,
		Error: &llm.AssistantMessage{ErrorMessage: "invalid_argument: broken"},
	}}}
	root := filepath.Join(t.TempDir(), "logs")
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root, 0, 0))

	// 成功前即失败：上游首个事件就是错误 → 非 200 HTTP 错误响应。
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("X-Request-Id", "agent-corr-1")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)

	dir := response.Header().Get("X-Request-Id")
	if dir == "" {
		t.Fatal("missing X-Request-Id header")
	}
	if _, err := os.Stat(filepath.Join(root, dir)); err != nil {
		t.Fatalf("X-Request-Id %q does not map to a log dir: %v", dir, err)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"debug_ref":"`+dir+`"`) || !strings.Contains(body, `"stage":"response_event"`) {
		t.Fatalf("error body missing debug_ref/stage: %s", body)
	}
	indexData, readErr := os.ReadFile(filepath.Join(root, "index.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	index := string(indexData)
	if !strings.Contains(index, `"client_request_id":"agent-corr-1"`) || !strings.Contains(index, `"error_stage":"response_event"`) {
		t.Fatalf("index missing correlation fields: %s", index)
	}
}

// TestStreamErrorCarriesDebugRef 验证已提交 200 的流式错误事件内嵌 debug_ref。
func TestStreamErrorCarriesDebugRef(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{ResponseID: "resp-1", StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventError, Reason: llm.StopReasonError,
			Error: &llm.AssistantMessage{ErrorMessage: "upstream exploded"}},
	}}
	root := filepath.Join(t.TempDir(), "logs")
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root, 0, 0))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	dir := response.Header().Get("X-Request-Id")
	if dir == "" || !strings.Contains(response.Body.String(), `"debug_ref":"`+dir+`"`) {
		t.Fatalf("stream error missing debug_ref: header=%q body=%s", dir, response.Body.String())
	}
}
