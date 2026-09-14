// 本文件验证 Anthropic Messages 最终 JSON 和 SSE 事件编码。
package messages

import (
	"encoding/json"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

// TestStreamEncoderEmitsMessageStartAndText 验证流式文本产生 Anthropic 标准事件。
func TestStreamEncoderEmitsMessageStartAndText(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
	text := llm.TextContent{Text: "hello"}
	partial := &llm.AssistantMessage{Content: []llm.Content{text}, StopReason: llm.StopReasonPending}
	final := &llm.AssistantMessage{Content: []llm.Content{text}, StopReason: llm.StopReasonStop}
	events := []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{ResponseID: "msg-1", StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "hello", Partial: partial},
		{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: "hello", Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: final},
	}
	encoded := encodeStreamEvents(t, encoder, events)
	if len(encoded) != 6 { // message_start, content_block_start, content_block_delta, content_block_stop, message_delta, message_stop
		t.Fatalf("event count = %d, want 6", len(encoded))
	}
	if encoded[0].Name != "message_start" || encoded[5].Name != "message_stop" {
		t.Fatalf("events = %v", encoded)
	}
}

// TestStreamEncoderEmitsToolUse 验证流式工具调用按 Anthropic 增量格式输出。
func TestStreamEncoderEmitsToolUse(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
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
	if len(encoded) != 7 {
		t.Fatalf("event count = %d, want 7", len(encoded))
	}
	if encoded[0].Name != "message_start" || encoded[6].Name != "message_stop" {
		t.Fatalf("events = %v", encoded)
	}
	startBlock := decodeEventData(t, encoded[1])
	content := startBlock["content_block"].(map[string]any)
	if content["type"] != "tool_use" || content["name"] != "lookup" {
		t.Fatalf("content_block = %#v", content)
	}
	stopBlock := decodeEventData(t, encoded[4])
	if stopBlock["type"] != "content_block_stop" {
		t.Fatalf("stop block type = %s", stopBlock["type"])
	}
}

// TestStreamEncoderHoldsThinkingForLateSignature 的测试动机是上游实测帧序
// thinking_end → toolcall_* → thinking_signature：thinking 块必须挂起等待
// 隔块的尾随签名，签名到达时补发 signature_delta 再收尾。
func TestStreamEncoderHoldsThinkingForLateSignature(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
	call := llm.ToolCall{ID: "call-1", Name: "lookup", Arguments: json.RawMessage(`{"city":"Shanghai"}`)}
	partial := &llm.AssistantMessage{
		Content:    []llm.Content{llm.ThinkingContent{Thinking: "inspect"}, call},
		StopReason: llm.StopReasonPending,
	}
	final := &llm.AssistantMessage{
		Content:    []llm.Content{llm.ThinkingContent{Thinking: "inspect", ThinkingSignature: "sig"}, call},
		StopReason: llm.StopReasonToolUse,
	}
	encoded := encodeStreamEvents(t, encoder, []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventThinkingStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventThinkingDelta, ContentIndex: 0, Delta: "inspect", Partial: partial},
		{Type: llm.ResponseEventThinkingEnd, ContentIndex: 0, Content: "inspect", Partial: partial},
		{Type: llm.ResponseEventToolCallStart, ContentIndex: 1, ToolCallID: "call-1", ToolName: "lookup", Partial: partial},
		{Type: llm.ResponseEventToolCallDelta, ContentIndex: 1, ToolCallID: "call-1", Delta: `{"city":"Shanghai"}`, Partial: partial},
	})
	// thinking 块挂起期间 tool_use 正常推进，此刻共 5 个事件且没有块收尾。
	if len(encoded) != 5 {
		t.Fatalf("events before signature = %d, want 5", len(encoded))
	}
	partial.Content[0] = llm.ThinkingContent{Thinking: "inspect", ThinkingSignature: "sig"}
	encoded = append(encoded, encodeStreamEvents(t, encoder, []llm.ResponseEvent{
		{Type: llm.ResponseEventThinkingSignature, ContentIndex: 0, Delta: "sig", Partial: partial},
		{Type: llm.ResponseEventToolCallEnd, ContentIndex: 1, ToolCall: &call, Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonToolUse, Message: final},
	})...)
	names := make([]string, 0, len(encoded))
	for _, event := range encoded {
		names = append(names, event.Name)
	}
	want := []string{
		"message_start", "content_block_start", "content_block_delta",
		"content_block_start", "content_block_delta",
		"content_block_delta", "content_block_stop",
		"content_block_stop", "message_delta", "message_stop",
	}
	if len(names) != len(want) {
		t.Fatalf("event names = %v, want %v", names, want)
	}
	for index, name := range want {
		if names[index] != name {
			t.Fatalf("event[%d] = %q, want %q (all: %v)", index, names[index], name, names)
		}
	}
	signatureDelta := decodeEventData(t, encoded[5])
	delta := signatureDelta["delta"].(map[string]any)
	if delta["type"] != "signature_delta" || delta["signature"] != "sig" || signatureDelta["index"] != float64(0) {
		t.Fatalf("signature delta = %#v", signatureDelta)
	}
	// 签名不再就地关块：挂起的思考块由流终止时的 flush 统一收尾，
	// 因此 index=1 的工具块先于 index=0 的思考块 stop。
	toolStop := decodeEventData(t, encoded[6])
	if toolStop["index"] != float64(1) {
		t.Fatalf("tool stop block = %#v", toolStop)
	}
	thinkingStop := decodeEventData(t, encoded[7])
	if thinkingStop["index"] != float64(0) || thinkingStop["content_block"] != nil {
		t.Fatalf("thinking stop block = %#v", thinkingStop)
	}
}

// TestStreamEncoderEmitsEverySignatureFragment 钉住上游把签名拆成多帧的
// 形态：每个 thinking_signature 事件都必须发 signature_delta——首个分片
// 就关块会让客户端只累积到前缀，下轮回放截断签名被上游拒。
func TestStreamEncoderEmitsEverySignatureFragment(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
	partial := &llm.AssistantMessage{
		Content:    []llm.Content{llm.ThinkingContent{Thinking: "inspect"}},
		StopReason: llm.StopReasonPending,
	}
	withSig := &llm.AssistantMessage{
		Content:    []llm.Content{llm.ThinkingContent{Thinking: "inspect", ThinkingSignature: "AAABBB"}},
		StopReason: llm.StopReasonStop,
	}
	encoded := encodeStreamEvents(t, encoder, []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventThinkingStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventThinkingDelta, ContentIndex: 0, Delta: "inspect", Partial: partial},
		{Type: llm.ResponseEventThinkingEnd, ContentIndex: 0, Content: "inspect", Partial: partial},
		{Type: llm.ResponseEventThinkingSignature, ContentIndex: 0, Delta: "AAA", Partial: withSig},
		{Type: llm.ResponseEventThinkingSignature, ContentIndex: 0, Delta: "BBB", Partial: withSig},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: withSig},
	})
	var signatures []string
	for _, event := range encoded {
		delta, ok := decodeEventData(t, event)["delta"].(map[string]any)
		if ok && delta["type"] == "signature_delta" {
			signatures = append(signatures, delta["signature"].(string))
		}
	}
	if len(signatures) != 2 || signatures[0] != "AAA" || signatures[1] != "BBB" {
		t.Fatalf("signature deltas = %v, want [AAA BBB]", signatures)
	}
}

// TestEncodeResponseFinal 验证非流式最终 JSON 结构和缓存用量字段。
func TestEncodeResponseFinal(t *testing.T) {
	final := &llm.AssistantMessage{
		ResponseID:    "msg-1",
		ResponseModel: "claude-test",
		Content:       []llm.Content{llm.TextContent{Text: "hello"}},
		StopReason:    llm.StopReasonStop,
		Usage:         llm.Usage{Input: 10, Output: 5, CacheRead: 3, CacheWrite: 2},
	}
	body, err := EncodeResponse(final)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["type"] != "message" || parsed["model"] != "claude-test" {
		t.Fatalf("response = %#v", parsed)
	}
	usage := parsed["usage"].(map[string]any)
	if usage["cache_read_input_tokens"] != float64(3) || usage["cache_creation_input_tokens"] != float64(2) {
		t.Fatalf("usage = %#v", usage)
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

// TestStreamEncoderEmitsError 验证流式错误生成 event: error。
func TestStreamEncoderEmitsError(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
	failed := &llm.AssistantMessage{Provider: "devin", StopReason: llm.StopReasonError, ErrorMessage: "permission_denied: not allowed"}
	event := llm.ResponseEvent{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: failed}
	encoded, err := encoder.Encode(event)
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
	if len(encoded) != 1 {
		t.Fatalf("event count = %d, want 1", len(encoded))
	}
	if encoded[0].Name != "error" {
		t.Fatalf("event name = %q, want error", encoded[0].Name)
	}
	data := decodeEventData(t, encoded[0])
	if data["type"] != "error" {
		t.Fatalf("type = %v", data["type"])
	}
	errObj, ok := data["error"].(map[string]any)
	if !ok {
		t.Fatalf("missing error object: %v", data)
	}
	if errObj["message"] != "permission_denied: not allowed" {
		t.Fatalf("error.message = %v", errObj["message"])
	}
	if errObj["type"] != "invalid_request_error" {
		t.Fatalf("error.type = %v, want invalid_request_error", errObj["type"])
	}
	// 错误后再次编码应因流已结束而失败。
	if _, err := encoder.Encode(event); err == nil {
		t.Fatal("encoding after error should fail")
	}
}

// TestStreamEncoderSignatureReadyAtThinkingEnd 覆盖签名随 thinking_end
// 一次到齐的路径（decodeLateSignature 合成块的 Start+End 序列即是此形态）：
// 规范客户端只从 signature_delta 累积签名，必须先补 delta 再收尾。
func TestStreamEncoderSignatureReadyAtThinkingEnd(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
	partial := &llm.AssistantMessage{
		Content:    []llm.Content{llm.ThinkingContent{Thinking: "inspect", ThinkingSignature: "sig"}},
		StopReason: llm.StopReasonPending,
	}
	encoded := encodeStreamEvents(t, encoder, []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventThinkingStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventThinkingDelta, ContentIndex: 0, Delta: "inspect", Partial: partial},
		{Type: llm.ResponseEventThinkingEnd, ContentIndex: 0, Content: "inspect", Partial: partial},
	})
	// message_start + block_start + thinking_delta + signature_delta + block_stop。
	if len(encoded) != 5 {
		t.Fatalf("events = %d, want 5", len(encoded))
	}
	if encoded[3].Name != "content_block_delta" || encoded[4].Name != "content_block_stop" {
		t.Fatalf("tail events = %q, %q", encoded[3].Name, encoded[4].Name)
	}
	delta := decodeEventData(t, encoded[3])["delta"].(map[string]any)
	if delta["type"] != "signature_delta" || delta["signature"] != "sig" {
		t.Fatalf("signature delta = %#v", delta)
	}
}

// TestStreamEncoderRedactedThinkingDeferredStart 覆盖开块即知 redacted 的
// 路径：spec 的 redacted_thinking 是 start 一次性带 data 的完整块，start
// 应推迟到签名就绪随 data 一起下发。
func TestStreamEncoderRedactedThinkingDeferredStart(t *testing.T) {
	encoder := NewStreamEncoder("claude-test")
	partial := &llm.AssistantMessage{
		Content: []llm.Content{llm.ThinkingContent{
			Thinking: "hidden", ThinkingSignature: "sealed-payload", Redacted: true,
		}},
		StopReason: llm.StopReasonPending,
	}
	encoded := encodeStreamEvents(t, encoder, []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventThinkingStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventThinkingEnd, ContentIndex: 0, Partial: partial},
	})
	// message_start + deferred block_start{redacted_thinking,data} + block_stop。
	if len(encoded) != 3 {
		t.Fatalf("events = %d, want 3", len(encoded))
	}
	start := decodeEventData(t, encoded[1])
	block := start["content_block"].(map[string]any)
	if start["type"] != "content_block_start" || block["type"] != "redacted_thinking" || block["data"] != "sealed-payload" {
		t.Fatalf("deferred start = %#v", start)
	}
	stop := decodeEventData(t, encoded[2])
	if stop["type"] != "content_block_stop" || stop["content_block"] != nil {
		t.Fatalf("stop = %#v", stop)
	}
}
