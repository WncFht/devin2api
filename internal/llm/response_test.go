// 本文件验证响应事件协议、最终消息和工具参数增量行为。
package llm

import (
	"encoding/json"
	"testing"
)

func TestResponseEventProtocolCarriesPartialUsageAndFinalMessage(t *testing.T) {
	partial := &AssistantMessage{
		Content: []Content{TextContent{Text: "正在处理"}},
		Usage: Usage{
			Input:       10,
			Output:      2,
			TotalTokens: 12,
		},
		StopReason: StopReasonPending,
	}
	toolCall := &ToolCall{
		ID:        "call-1",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"config.json"}`),
	}
	final := &AssistantMessage{
		Content:    []Content{*toolCall},
		Usage:      Usage{Input: 10, Output: 8, TotalTokens: 18},
		StopReason: StopReasonToolUse,
	}

	events := []ResponseEvent{
		{Type: ResponseEventStart, Partial: partial},
		{Type: ResponseEventTextStart, ContentIndex: 0, Partial: partial},
		{Type: ResponseEventTextDelta, ContentIndex: 0, Delta: "处理中", Partial: partial},
		{Type: ResponseEventTextEnd, ContentIndex: 0, Content: "正在处理", Partial: partial},
		{Type: ResponseEventThinkingStart, ContentIndex: 1, Partial: partial},
		{Type: ResponseEventThinkingDelta, ContentIndex: 1, Delta: "需要工具", Partial: partial},
		{Type: ResponseEventThinkingEnd, ContentIndex: 1, Content: "需要工具", Partial: partial},
		{Type: ResponseEventToolCallStart, ContentIndex: 2, ToolCallID: toolCall.ID, ToolName: toolCall.Name, Partial: partial},
		{Type: ResponseEventToolCallDelta, ContentIndex: 2, ToolCallID: toolCall.ID, Delta: `{"path":`, Partial: partial},
		{Type: ResponseEventToolCallEnd, ContentIndex: 2, ToolCall: toolCall, Partial: partial},
		{Type: ResponseEventDone, Reason: StopReasonToolUse, Message: final},
	}

	for _, event := range events {
		if err := event.Validate(); err != nil {
			t.Errorf("%s Validate() error = %v", event.Type, err)
		}
	}
}

func TestResponseEventRejectsDoneWithoutFinalMessage(t *testing.T) {
	event := ResponseEvent{Type: ResponseEventDone, Reason: StopReasonStop}
	if err := event.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want missing final message error")
	}
}

// TestResponseEventAllowsEmptyToolCallDelta 的测试动机是允许供应商发送空参数片段，同时保留调用关联标识。
func TestResponseEventAllowsEmptyToolCallDelta(t *testing.T) {
	partial := &AssistantMessage{StopReason: StopReasonPending}
	event := ResponseEvent{
		Type:         ResponseEventToolCallDelta,
		ContentIndex: 0,
		ToolCallID:   "call-1",
		Delta:        "",
		Partial:      partial,
	}

	if err := event.Validate(); err != nil {
		t.Fatalf("Validate() error = %v, want empty tool call delta to be valid", err)
	}
}

// TestResponseEventRejectsToolCallDeltaWithoutID 的测试动机是避免并行工具参数流失去稳定关联。
func TestResponseEventRejectsToolCallDeltaWithoutID(t *testing.T) {
	event := ResponseEvent{
		Type:         ResponseEventToolCallDelta,
		ContentIndex: 0,
		Delta:        `{}`,
		Partial:      &AssistantMessage{StopReason: StopReasonPending},
	}
	if err := event.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want missing tool call ID error")
	}
}
