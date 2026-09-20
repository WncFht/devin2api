// 本文件验证 app 能把 OpenAI HTTP 请求交给 adapter，并按顺序输出 Responses SSE。
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/store"
)

// openTokenDB 开一个临时 sqlite 库给令牌仓用。
func openTokenDB(t *testing.T) *store.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// newTokenStore 建一个以 plain 为令牌的下游仓并接到 application——
// /v1 准入的唯一判定源是令牌仓（无凭据旁路），要 401 场景就得仓内
// 有行。plain 为空串时种的是匿名通道行（无凭据请求按它准入）。
func newTokenStore(t *testing.T, plain string) *authtoken.Store {
	t.Helper()
	store, err := authtoken.New(openTokenDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Ensure(plain, &authtoken.Token{Description: "test", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	return store
}

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
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{}, openTokenDB(t))
	application := New(fake, config.ServerConfig{Listen: ":0"}, manager)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	ref := response.Header().Get("X-Request-Id")
	if ref == "" {
		t.Fatal("missing X-Request-Id header")
	}
	<-manager.Drained(ref)
	detail, err := manager.Detail(context.Background(), ref)
	if err != nil {
		t.Fatalf("Detail(%q): %v", ref, err)
	}
	present := map[string]bool{}
	for _, f := range detail.Files {
		present[f.Name] = true
	}
	for _, name := range []string{"meta.json", "01-http-request.json", "02-request-messages.json", "05-response-events.jsonl", "06-http-response.jsonl"} {
		if !present[name] {
			t.Errorf("%s missing from files %v", name, detail.Files)
		}
	}
	if !strings.Contains(string(detail.Meta), `"result": "completed"`) || !strings.Contains(string(detail.Meta), `"provider": "devin"`) {
		t.Fatalf("meta = %s", detail.Meta)
	}
}

// TestPrematureEndTurnFlagged 的测试动机是观测「工具结果之后模型纯文本
// end_turn」的可疑收尾：结构合法但实测存在模型声称继续动作后直接 EOS
// 的故障形态，meta/index 需要可检索的标记来统计真实频率。
func TestPrematureEndTurnFlagged(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventDone,
		Reason: llm.StopReasonStop,
		Message: &llm.AssistantMessage{
			ResponseModel: "gpt-test", StopReason: llm.StopReasonStop,
			Content: []llm.Content{llm.TextContent{Text: "done"}},
		},
	}}}
	root := filepath.Join(t.TempDir(), "logs")
	st := openTokenDB(t)
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{}, st)
	application := New(fake, config.ServerConfig{Listen: ":0"}, manager)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"run ls"}]},
		{"type":"function_call","call_id":"call-1","name":"exec","arguments":"{}"},
		{"type":"function_call_output","call_id":"call-1","output":"file.txt"}
	]}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	<-manager.Drained(response.Header().Get("X-Request-Id"))
	meta, _, _, err := manager.ReadFile(context.Background(), response.Header().Get("X-Request-Id"), "meta.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"premature_end_turn": true`) {
		t.Fatalf("meta = %s, want premature_end_turn", meta)
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 || !rows[0].PrematureEndTurn {
		t.Fatalf("log row = %+v, want premature_end_turn", rows)
	}
}

// TestPrematureEndTurnNotFlaggedForUserInput 验证普通用户输入后的正常
// 收尾不触发标记——标记只统计工具结果结尾的可疑形态。
func TestPrematureEndTurnNotFlaggedForUserInput(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventDone,
		Reason: llm.StopReasonStop,
		Message: &llm.AssistantMessage{
			ResponseModel: "gpt-test", StopReason: llm.StopReasonStop,
			Content: []llm.Content{llm.TextContent{Text: "done"}},
		},
	}}}
	root := filepath.Join(t.TempDir(), "logs")
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{}, openTokenDB(t))
	application := New(fake, config.ServerConfig{Listen: ":0"}, manager)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	<-manager.Drained(response.Header().Get("X-Request-Id"))
	meta, _, _, err := manager.ReadFile(context.Background(), response.Header().Get("X-Request-Id"), "meta.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(meta), "premature_end_turn") {
		t.Fatalf("meta = %s, want no premature_end_turn", meta)
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
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{}, openTokenDB(t))
	application := New(fake, config.ServerConfig{Listen: ":0"}, manager)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	dir := response.Header().Get("X-Request-Id")
	<-manager.Drained(dir)
	meta, _, _, err := manager.ReadFile(context.Background(), dir, "meta.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), `"result": "failed"`) {
		t.Fatalf("meta = %s", meta)
	}
	errorLog, _, _, err := manager.ReadFile(context.Background(), dir, "error.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(errorLog), `"stage": "http_stream"`) {
		t.Fatalf("error = %s", errorLog)
	}
}

// TestResponsesHandlerRejectsMissingAPIKey 验证仓内有令牌时未提供密钥的
// /v1/* 请求返回 401（匿名通道未开——仓内唯一的行不是匿名行）。
func TestResponsesHandlerRejectsMissingAPIKey(t *testing.T) {
	fake := &fakeAdapter{}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(newTokenStore(t, "secret-key"), nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"type":"authentication_error"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// TestResponsesHandlerRejectsInvalidAPIKey 验证错误密钥无法通过鉴权。
func TestResponsesHandlerRejectsInvalidAPIKey(t *testing.T) {
	fake := &fakeAdapter{}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(newTokenStore(t, "secret-key"), nil)
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
	application.SetAuthTokens(newTokenStore(t, "secret-key"), nil)
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
	application.SetAuthTokens(newTokenStore(t, "secret-key"), nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "secret-key")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerHealthIsUnprotected 验证 /healthz 不受令牌仓保护。
func TestResponsesHandlerHealthIsUnprotected(t *testing.T) {
	application := New(&fakeAdapter{}, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(newTokenStore(t, "secret-key"), nil)
	request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerOpenModeWhenTokenStoreEmpty 验证令牌仓为空时 /v1
// 不校验凭据（开放模式）：无凭据请求直接放行。
func TestResponsesHandlerOpenModeWhenTokenStoreEmpty(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}}}}
	store, err := authtoken.New(openTokenDB(t))
	if err != nil {
		t.Fatal(err)
	}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(store, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}

// TestResponsesHandlerAnonymousChannelAdmits 验证匿名通道行：无凭据请求
// 按该行准入；坏凭据仍 401（匿名行只兜「没带凭据」，不豁免「带错凭据」）。
func TestResponsesHandlerAnonymousChannelAdmits(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(newTokenStore(t, ""), nil)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("no-credential status = %d, want 200: %s", response.Code, response.Body.String())
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	bad.Header.Set("Authorization", "Bearer wrong-key")
	badResponse := httptest.NewRecorder()
	application.Router().ServeHTTP(badResponse, bad)
	if badResponse.Code != http.StatusUnauthorized {
		t.Fatalf("bad-credential status = %d, want 401", badResponse.Code)
	}
}

// TestResponsesHandlerTokenRPMLimitReturns429 验证令牌 max_rpm 在准入链
// 上执行：超过分钟桶上限的请求按 token_limit 429 拒绝。
func TestResponsesHandlerTokenRPMLimitReturns429(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}}}}
	store, err := authtoken.New(openTokenDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Ensure("secret-key", &authtoken.Token{Description: "t", IsActive: true, MaxRPM: 1}); err != nil {
		t.Fatal(err)
	}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(store, nil)

	first := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	first.Header.Set("Authorization", "Bearer secret-key")
	firstResponse := httptest.NewRecorder()
	application.Router().ServeHTTP(firstResponse, first)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200: %s", firstResponse.Code, firstResponse.Body.String())
	}

	second := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	second.Header.Set("Authorization", "Bearer secret-key")
	secondResponse := httptest.NewRecorder()
	application.Router().ServeHTTP(secondResponse, second)
	if secondResponse.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429: %s", secondResponse.Code, secondResponse.Body.String())
	}
	if !strings.Contains(secondResponse.Body.String(), "rate limit") {
		t.Fatalf("body = %s, want rate-limit message", secondResponse.Body.String())
	}
}

// TestResponsesHandlerTokenCost5hLimitReturns429 验证 5h 锚定窗口超额按
// token_limit 429 拒绝，错误信息带窗口名。
func TestResponsesHandlerTokenCost5hLimitReturns429(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop}}}}
	store, err := authtoken.New(openTokenDB(t))
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := store.Ensure("secret-key", &authtoken.Token{
		Description: "t", IsActive: true, MaxConcurrency: 5, Cost5hLimitMicroUSD: 1_000_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Ensure 返回快照：用量字段须经 Update 落回仓内才参与准入判定。
	tok.Cost5hAnchor = time.Now().UnixMilli()
	tok.Cost5hUsedMicroUSD = 1_000_000
	if err := store.Update(tok); err != nil {
		t.Fatal(err)
	}

	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	application.SetAuthTokens(store, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("Authorization", "Bearer secret-key")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "5h cost limit") {
		t.Fatalf("body = %s, want 5h window named", response.Body.String())
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
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(blockedRoot, debuglog.RetentionPolicy{}, nil))
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

// TestStreamRateLimitDeliversSSE429 验证 OpenAI 流式面的限流契约：
// pre-stream 429 以「200 + 错误事件」下发——Codex 对 HTTP 429 一律终止
// （codex-rs retry_429 硬编码 false），只把流内错误事件当可重试信号；
// error.code=rate_limit_exceeded + "try again in Ns" 让它睡到 reset
// 时刻再重试。确定性错误仍走真实 HTTP 状态（见上个用例）。
func TestStreamRateLimitDeliversSSE429(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventError,
		Reason: llm.StopReasonError,
		Error:  &llm.AssistantMessage{ErrorMessage: "resource_exhausted: Reached overall message rate limit. Please try again later. Your limit will reset in 30 seconds."},
	}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want committed 200: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, want := range []string{"response.failed", `"status":429`, `"rate_limit_exceeded"`, "try again in"} {
		if !strings.Contains(body, want) {
			t.Fatalf("error event missing %s: %s", want, body)
		}
	}
}

// TestStreamRateLimitAnthropicKeepsHTTPStatus 是对照组：同样的限流在
// Anthropic 面必须保留 429 状态——Claude Code 按 HTTP 状态码与
// unified-reset 头睡眠重试，提交 200 会让失败被判为不可重试的
// malformed response。
func TestStreamRateLimitAnthropicKeepsHTTPStatus(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventError,
		Reason: llm.StopReasonError,
		Error:  &llm.AssistantMessage{ErrorMessage: "resource_exhausted: Reached overall message rate limit. Please try again later. Your limit will reset in 30 seconds."},
	}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header on 429")
	}
}

// TestAnthropicBetaHeaderEmitsSeedMarker 验证 anthropic-beta 头按 flag
// 记入 Dropped marker（排序去重的规范集合）——声明的特性面改变上游
// 行为，marker 进 sessionSeed 影响 lane 亲和。
func TestAnthropicBetaHeaderEmitsSeedMarker(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "claude-test", StopReason: llm.StopReasonStop}},
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-test","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Add("anthropic-beta", "flag-b, flag-a")
	request.Header.Add("anthropic-beta", "flag-a")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	want := []string{"anthropic_beta:flag-a", "anthropic_beta:flag-b"}
	var got []string
	for _, marker := range fake.lastRequest.Dropped {
		if strings.HasPrefix(marker, "anthropic_beta:") {
			got = append(got, marker)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("beta markers = %v, want %v (dropped = %v)", got, want, fake.lastRequest.Dropped)
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
	st := openTokenDB(t)
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{}, st)
	application := New(fake, config.ServerConfig{Listen: ":0"}, manager)

	// 成功前即失败：上游首个事件就是错误 → 非 200 HTTP 错误响应。
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	request.Header.Set("X-Request-Id", "agent-corr-1")
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)

	dir := response.Header().Get("X-Request-Id")
	if dir == "" {
		t.Fatal("missing X-Request-Id header")
	}
	<-manager.Drained(dir)
	if _, err := manager.Detail(context.Background(), dir); err != nil {
		t.Fatalf("X-Request-Id %q does not map to a debug dir: %v", dir, err)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"debug_ref":"`+dir+`"`) || !strings.Contains(body, `"stage":"response_event"`) {
		t.Fatalf("error body missing debug_ref/stage: %s", body)
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 || rows[0].ClientRequestID != "agent-corr-1" || rows[0].ErrorStage != "response_event" {
		t.Fatalf("log row missing correlation fields: %+v", rows)
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
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root, debuglog.RetentionPolicy{}, nil))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	dir := response.Header().Get("X-Request-Id")
	if dir == "" || !strings.Contains(response.Body.String(), `"debug_ref":"`+dir+`"`) {
		t.Fatalf("stream error missing debug_ref: header=%q body=%s", dir, response.Body.String())
	}
}

// failingAdapter 在 Stream 阶段直接返回错误，用于验证适配器建连失败
// 时的 HTTP 归一（状态码 / Retry-After / 上游排障字段）。
type failingAdapter struct {
	err error
}

func (fake *failingAdapter) Stream(context.Context, llm.RequestMessages) (llm.ResponseStream, error) {
	return nil, fake.err
}

func (fake *failingAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "gpt-test", Created: 1, OwnedBy: "test"}}, nil
}

// TestResponsesHandlerMapsRateLimitError 验证上游 resource_exhausted 归一为
// 429，且限流文案里的 reset 秒数翻成 Retry-After 头、trace ID 进错误体。
func TestResponsesHandlerMapsRateLimitError(t *testing.T) {
	fake := &failingAdapter{err: errors.New(
		"resource_exhausted: rate limited. Your limit will reset in 30 seconds. (trace ID: abc123)")}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "30" {
		t.Fatalf("Retry-After = %q, want 30", got)
	}
	body := response.Body.String()
	for _, expected := range []string{`"type":"rate_limit_error"`, `"upstream_trace_id":"abc123"`, `"retry_after":30`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("body = %s, want %s", body, expected)
		}
	}
}

// TestResponsesHandlerZeroResetHintWritesNoRetryAfter 验证显式 0 秒 reset
// hint（"reset in 0 seconds"——刚过桶界、新桶已爆、无追加罚）的 429 不写
// Retry-After/unified-reset 头：写 0 等于叫客户端立刻重试撞新桶。
func TestResponsesHandlerZeroResetHintWritesNoRetryAfter(t *testing.T) {
	fake := &failingAdapter{err: errors.New(
		"resource_exhausted: rate limited. Your limit will reset in 0 seconds. (trace ID: abc123)")}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want absent", got)
	}
	if got := response.Header().Get("anthropic-ratelimit-unified-reset"); got != "" {
		t.Fatalf("unified-reset = %q, want absent", got)
	}
}

// blockingAdapter 的 Stream 在 release 关闭前不返回，用于占住并发槽。
type blockingAdapter struct {
	release chan struct{}
}

func (blocking *blockingAdapter) Stream(ctx context.Context, _ llm.RequestMessages) (llm.ResponseStream, error) {
	select {
	case <-blocking.release:
		return nil, errors.New("released")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (blocking *blockingAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "gpt-test", Created: 1, OwnedBy: "test"}}, nil
}

// TestConcurrencyOverflowReturns429 验证并发槽打满时溢出请求收到
// 429 + Retry-After，而不是 503——503 会让下游网关误判渠道故障。
func TestConcurrencyOverflowReturns429(t *testing.T) {
	blocking := &blockingAdapter{release: make(chan struct{})}
	application := New(blocking, config.ServerConfig{Listen: ":0", MaxConcurrency: 1}, nil)

	held := make(chan struct{})
	go func() {
		defer close(held)
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
		application.Router().ServeHTTP(httptest.NewRecorder(), request)
	}()
	// 等第一个请求占住槽：阻塞式 Stream 没有占槽信号，用轮询 metrics 太绕，
	// 直接短暂等待后探测——溢出分支是 select-default，不占槽立即返回。
	time.Sleep(50 * time.Millisecond)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After header")
	}
	if !strings.Contains(response.Body.String(), `"type":"rate_limit_error"`) {
		t.Fatalf("body = %s", response.Body.String())
	}
	close(blocking.release)
	<-held
}

// TestDrainTrackerLateAddDuringWait 复现原 WaitGroup 实现的 panic 窗口：
// 排空等待方已武装、计数归零唤醒之间，排空期仍开着的 listener 放进来的
// 新请求会迟到 Add——tracker 必须容忍这个时序而不是 panic。
func TestDrainTrackerLateAddDuringWait(t *testing.T) {
	var tracker drainTracker
	tracker.Add()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waited := make(chan error, 1)
	go func() { waited <- tracker.Wait(ctx) }()

	// 等等待方武装 drained（确定时序，不依赖调度延迟）。
	for i := 0; i < 1000; i++ {
		tracker.mu.Lock()
		armed := tracker.drained != nil
		tracker.mu.Unlock()
		if armed {
			break
		}
		runtime.Gosched()
	}
	tracker.mu.Lock()
	armed := tracker.drained != nil
	tracker.mu.Unlock()
	if !armed {
		t.Fatal("waiter did not arm drained channel")
	}

	tracker.Done() // 计数归零 → 等待方被唤醒
	tracker.Add()  // 唤醒后、Wait 返回前的迟到 Add：WaitGroup 在此窗口 panic
	tracker.Done()

	if err := <-waited; err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	// 计数再次归零后 Wait 应立即返回。
	if err := tracker.Wait(ctx); err != nil {
		t.Fatalf("Wait after drain = %v, want nil", err)
	}
}

// TestDrainTrackerWaitTimeout 验证 ctx 截止路径返回错误而不是泄漏等待。
func TestDrainTrackerWaitTimeout(t *testing.T) {
	var tracker drainTracker
	tracker.Add()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := tracker.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait = %v, want DeadlineExceeded", err)
	}
	tracker.Done()
}

// errBody 是读取即失败的请求体——模拟客户端断连/超时导致的读失败。
type errBody struct{ err error }

func (b errBody) Read([]byte) (int, error) { return 0, b.err }
func (b errBody) Close() error             { return nil }

// zeroBody 是无限零字节源——413 路径只需要真实字节数越过 32MiB 上限，
// 内容本身永远不会被解析。
type zeroBody struct{}

func (zeroBody) Read(p []byte) (int, error) { return len(p), nil }
func (zeroBody) Close() error               { return nil }

// blockedStreamAdapter 的 Stream 挂起直到 ctx 取消——模拟客户端在上游
// 建流期间断连，或 WS 轮次期间入队帧持续积压。
type blockedStreamAdapter struct{ entered chan struct{} }

func (b *blockedStreamAdapter) Stream(ctx context.Context, _ llm.RequestMessages) (llm.ResponseStream, error) {
	close(b.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

func (b *blockedStreamAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return nil, nil
}

func rejectCount(application *App, reason obs.RejectReason) uint64 {
	return application.metrics.Rejects().ByReason[string(reason)]
}

// TestReadFailureRejectedWithoutDir 验证请求体读取失败（非超限）按管线前
// 拒绝入账：504（可重试档，见 handler 注释）+ rejects 计数 + 一条
// log_source=rejected 留存行；不产生调试目录——完整请求从未到达，
// 与鉴权/并发拒绝同口径。
func TestReadFailureRejectedWithoutDir(t *testing.T) {
	st := openTokenDB(t)
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, st)
	t.Cleanup(func() { manager.Close() })
	application := New(&fakeAdapter{}, config.ServerConfig{Listen: ":0"}, manager)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", errBody{err: errors.New("read: connection reset by peer")})
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)

	if response.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504", response.Code)
	}
	if got := rejectCount(application, obs.RejectHTTPRead); got != 1 {
		t.Fatalf("http_read rejects = %d, want 1", got)
	}
	dirs, err := st.DebugDirs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 0 {
		t.Fatalf("read failure produced debug dirs %v", dirs)
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{LogSource: "rejected"})
	if err != nil {
		t.Fatalf("SearchLogs rejected: %v", err)
	}
	if len(rows) != 1 || rows[0].StatusCode != http.StatusGatewayTimeout ||
		rows[0].Result != "rejected" || rows[0].ErrorStage != debuglog.ErrStagePrePipeline ||
		rows[0].ErrorMessage != string(obs.RejectHTTPRead) || rows[0].API != "openai-responses" {
		t.Fatalf("rejected log row = %+v", rows)
	}
	// 默认日志视图看不到 rejected 行——留存检索须显式取。
	if n, _, err := st.SearchLogs(context.Background(), store.LogQuery{}); err != nil || len(n) != 0 {
		t.Fatalf("默认视图含 rejected 行: %v err=%v", n, err)
	}
}

// TestRequestTooLargeKeepsDebugDir 验证 ≥32MiB 的真实载荷保留调试记录：
// 413 是请求真实到达后的拒绝（不是管线前），X-Request-Id 与调试记录都在，
// rejects 计数不应增长。
func TestRequestTooLargeKeepsDebugDir(t *testing.T) {
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, openTokenDB(t))
	t.Cleanup(func() { manager.Close() })
	application := New(&fakeAdapter{}, config.ServerConfig{Listen: ":0"}, manager)

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", io.LimitReader(zeroBody{}, (32<<20)+1))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", response.Code)
	}
	ref := response.Header().Get("X-Request-Id")
	if ref == "" {
		t.Fatal("413 response missing X-Request-Id debug ref")
	}
	<-manager.Drained(ref)
	if _, err := manager.Detail(context.Background(), ref); err != nil {
		t.Fatalf("debug dir %s missing: %v", ref, err)
	}
	if got := rejectCount(application, obs.RejectHTTPRead); got != 0 {
		t.Fatalf("413 counted as pipeline reject (%d); it must keep debug evidence", got)
	}
}

// TestClientDisconnectRecords499 验证未提交响应前的断连按 499+disconnected
// 入账而不是 500+failed——断连是客户端责任，不能污染 server_error 聚合。
func TestClientDisconnectRecords499(t *testing.T) {
	st := openTokenDB(t)
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, st)
	t.Cleanup(func() { manager.Close() })
	fake := &blockedStreamAdapter{entered: make(chan struct{})}
	application := New(fake, config.ServerConfig{Listen: ":0"}, manager)

	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-test","input":"hi"}`)).WithContext(ctx)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		application.Router().ServeHTTP(response, request)
		close(done)
	}()
	<-fake.entered
	cancel()
	<-done
	<-manager.Drained(response.Header().Get("X-Request-Id"))

	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(rows))
	}
	entry := rows[0]
	if entry.StatusCode != 499 || entry.Result != "disconnected" {
		t.Fatalf("log row = status %d result %q, want 499/disconnected", entry.StatusCode, entry.Result)
	}
	if entry.ErrorStage != debuglog.ErrStageClientDisconnected {
		t.Fatalf("error_stage = %q, want client_disconnected", entry.ErrorStage)
	}
}
