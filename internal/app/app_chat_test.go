// 本文件验证 /v1/chat/completions 路由能被正确解码、适配和编码。
package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/leookun/devin-2api/internal/config"
	"github.com/leookun/devin-2api/internal/llm"
)

// TestChatCompletionsHandlerStreamsSSE 验证 chat 流式返回 data-only SSE。
func TestChatCompletionsHandlerStreamsSSE(t *testing.T) {
	final := &llm.AssistantMessage{ResponseID: "chat-1", ResponseModel: "gpt-test", Content: []llm.Content{llm.TextContent{Text: "hello"}}, StopReason: llm.StopReasonStop}
	fake := &fakeAdapter{events: []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{ResponseID: "chat-1", StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: &llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}, StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "hello", Partial: &llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}, StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: "hello", Partial: &llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}, StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final},
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("Content-Type = %q", response.Header().Get("Content-Type"))
	}
	body := response.Body.String()
	if !strings.Contains(body, `data: {`) || !strings.Contains(body, `data: [DONE]`) {
		t.Fatalf("body missing expected data: %s", body)
	}
	if len(fake.lastRequest.Messages) != 1 {
		t.Fatalf("adapter message count = %d, want 1", len(fake.lastRequest.Messages))
	}
}

// TestChatCompletionsHandlerReturnsJSON 验证 chat 非流式返回完整 JSON。
func TestChatCompletionsHandlerReturnsJSON(t *testing.T) {
	fake := &fakeAdapter{events: []llm.ResponseEvent{{
		Type:   llm.ResponseEventDone,
		Reason: llm.StopReasonStop,
		Message: &llm.AssistantMessage{ResponseID: "chat-1", ResponseModel: "gpt-test", StopReason: llm.StopReasonStop},
	}}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]}`))
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", response.Code, response.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "chat.completion" {
		t.Fatalf("object = %v, want chat.completion", parsed["object"])
	}
}
