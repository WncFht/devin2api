// 本文件验证 app 能把 OpenAI HTTP 请求交给 adapter，并按顺序输出 Responses SSE。
package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leookun/devin-2api/internal/config"
	"github.com/leookun/devin-2api/internal/debuglog"
	"github.com/leookun/devin-2api/internal/llm"
)

type fakeAdapter struct {
	lastRequest llm.RequestMessages
	events      []llm.ResponseEvent
}

func (fake *fakeAdapter) Stream(_ context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	fake.lastRequest = request
	return &fakeStream{events: fake.events}, nil
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
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root))
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
	if len(entries) != 1 {
		t.Fatalf("request directory count = %d, want 1", len(entries))
	}
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
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: failed}}}
	root := filepath.Join(t.TempDir(), "logs")
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(root))
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

// TestResponsesHandlerIgnoresLogInitializationFailure 的测试动机是保证诊断写盘故障不会改变兼容 API 的业务结果。
func TestResponsesHandlerIgnoresLogInitializationFailure(t *testing.T) {
	final := &llm.AssistantMessage{ResponseID: "resp-1", ResponseModel: "model", StopReason: llm.StopReasonStop}
	fake := &fakeAdapter{events: []llm.ResponseEvent{{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final}}}
	blockedRoot := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedRoot, []byte("occupied"), 0o600); err != nil {
		t.Fatal(err)
	}
	application := New(fake, config.ServerConfig{Listen: ":0"}, debuglog.NewManager(blockedRoot))
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"model","input":"hi"}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
}
