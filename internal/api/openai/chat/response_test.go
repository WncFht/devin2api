// 本文件验证 OpenAI Chat Completions 最终 JSON 和 SSE chunk 编码。
package chat

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/leookun/devin-2api/internal/llm"
)

// TestStreamEncoderEmitsRoleAndText 验证流式文本产生 role chunk 和 content delta。
func TestStreamEncoderEmitsRoleAndText(t *testing.T) {
	encoder := NewStreamEncoder("gpt-test", false)
	text := llm.TextContent{Text: "final answer"}
	partial := &llm.AssistantMessage{Content: []llm.Content{text}, StopReason: llm.StopReasonPending}
	final := &llm.AssistantMessage{Content: []llm.Content{text}, StopReason: llm.StopReasonStop}
	events := []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "final ", Partial: partial},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "answer", Partial: partial},
		{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: "final answer", Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final},
	}
	encoded := encodeStreamEvents(t, encoder, events)
	if len(encoded) != 5 { // role, "final ", "answer", finish, [DONE]
		t.Fatalf("event count = %d, want 5", len(encoded))
	}
	first := decodeEventData(t, encoded[0])
	if first["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"] != "assistant" {
		t.Fatalf("first event role missing: %v", first)
	}
	if strings.Contains(string(encoded[0].Data), `"content":`) {
		t.Fatalf("first chunk should not contain content: %s", encoded[0].Data)
	}
	if decodeEventData(t, encoded[1])["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"] != "final " {
		t.Fatalf("second chunk content wrong")
	}
}

// TestStreamEncoderEmitsToolCalls 验证流式工具调用按 OpenAI Chat 增量格式输出。
func TestStreamEncoderEmitsToolCalls(t *testing.T) {
	encoder := NewStreamEncoder("gpt-test", false)
	call := llm.ToolCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"city":"Shanghai"}`)}
	partial := &llm.AssistantMessage{Content: []llm.Content{call}, StopReason: llm.StopReasonPending}
	final := &llm.AssistantMessage{Content: []llm.Content{call}, StopReason: llm.StopReasonToolUse}
	events := []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventToolCallStart, ContentIndex: 0, ToolCallID: "call-1", ToolName: "lookup", Partial: partial},
		{Type: llm.ResponseEventToolCallDelta, ContentIndex: 0, ToolCallID: "call-1", Delta: `{"city":"`, Partial: partial},
		{Type: llm.ResponseEventToolCallDelta, ContentIndex: 0, ToolCallID: "call-1", Delta: `Shanghai"}`, Partial: partial},
		{Type: llm.ResponseEventToolCallEnd, ContentIndex: 0, ToolCall: &call, Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonToolUse, Message: final},
	}
	encoded := encodeStreamEvents(t, encoder, events)
	if len(encoded) != 6 { // role, tool start, arg delta x2, finish, [DONE]
		t.Fatalf("event count = %d, want 6", len(encoded))
	}
	firstTool := decodeEventData(t, encoded[1])
	toolCall := firstTool["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if toolCall["function"].(map[string]any)["name"] != "lookup" {
		t.Fatalf("tool call name = %v", toolCall["function"].(map[string]any)["name"])
	}
}

// TestEncodeResponseFinal 验证非流式最终 JSON 结构和用量缓存字段。
func TestEncodeResponseFinal(t *testing.T) {
	final := &llm.AssistantMessage{
		ResponseID:    "chatcmpl-1",
		ResponseModel: "gpt-test",
		Content:       []llm.Content{llm.TextContent{Text: "hello"}},
		StopReason:    llm.StopReasonStop,
		Usage:         llm.Usage{Input: 10, Output: 5, CacheRead: 3, TotalTokens: 18},
	}
	body, err := EncodeResponse(final)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["object"] != "chat.completion" || parsed["model"] != "gpt-test" {
		t.Fatalf("response = %#v", parsed)
	}
	usage := parsed["usage"].(map[string]any)
	details := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(3) {
		t.Fatalf("cached_tokens = %v", details["cached_tokens"])
	}
}

func encodeStreamEvents(t *testing.T, encoder *StreamEncoder, events []llm.ResponseEvent) []SSEEvent {
	t.Helper()
	var encoded []SSEEvent
	for _, event := range events {
		batch, err := encoder.Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, batch...)
	}
	return encoded
}

func decodeEventData(t *testing.T, event SSEEvent) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}
