// 本文件验证 Devin 请求字段映射和响应增量聚合。
package devin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	devinproto "local/devinproto"

	"connectrpc.com/connect"
	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"google.golang.org/protobuf/proto"
)

// fakeDevinResponseReceiver 为 responseStream 测试提供确定顺序的 protobuf 帧。
type fakeDevinResponseReceiver struct {
	// responses 是等待消费的响应帧。
	responses []*devinproto.GetChatMessageResponse
	// index 是下一次 Receive 尝试读取的位置。
	index int
	// current 是最近一次成功读取的响应帧。
	current *devinproto.GetChatMessageResponse
}

// Receive 前进到下一帧。
func (receiver *fakeDevinResponseReceiver) Receive() bool {
	if receiver.index >= len(receiver.responses) {
		return false
	}
	receiver.current = receiver.responses[receiver.index]
	receiver.index++
	return true
}

// Msg 返回最近一次成功读取的帧。
func (receiver *fakeDevinResponseReceiver) Msg() *devinproto.GetChatMessageResponse {
	return receiver.current
}

// Err 模拟正常 EOF。
func (receiver *fakeDevinResponseReceiver) Err() error { return nil }

// errorDevinResponseReceiver 模拟上游零帧即以错误终止的流。
type errorDevinResponseReceiver struct {
	err error
}

// Receive 直接报告流结束。
func (receiver *errorDevinResponseReceiver) Receive() bool { return false }

// Msg 没有可返回的帧。
func (receiver *errorDevinResponseReceiver) Msg() *devinproto.GetChatMessageResponse { return nil }

// Err 返回流终止错误。
func (receiver *errorDevinResponseReceiver) Err() error { return receiver.err }

func TestBuildRequestMapsLoopMessages(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "system",
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ThinkingContent{Thinking: "think", ThinkingSignature: "sig"},
				llm.ToolCall{ID: "call-1", Name: "exec", Arguments: json.RawMessage(`{"command":"ls"}`)},
			}},
			llm.ToolResultMessage{ToolCallID: "call-1", ToolName: "exec", IsError: true, Content: []llm.Content{llm.TextContent{Text: "failed"}}},
		},
		Tools: []llm.ToolDefinition{
			{Name: "exec", Description: "run", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "read", Description: "read file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	wantPrompt := "system\n\n# tools descriptions\n<tool name=\"exec\">\n1. run\n</tool>\n<tool name=\"read\">\n1. read file\n</tool>"
	if converted.GetPrompt() != wantPrompt || converted.GetChatModelUid() != "model" {
		t.Fatalf("top-level request = %#v", converted)
	}
	// 助手轮无文本：只有 thinking + 工具调用 → 单条调用消息，thinking/签名挂在其上。
	if len(converted.GetChatMessagePrompts()) != 3 {
		t.Fatalf("message count = %d, want 3", len(converted.GetChatMessagePrompts()))
	}
	toolCallMsg := converted.GetChatMessagePrompts()[1]
	if toolCallMsg.GetSource() != devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM {
		t.Fatalf("assistant tool call source = %v", toolCallMsg.GetSource())
	}
	if toolCallMsg.GetThinking() != "think" || toolCallMsg.GetSignature() != "sig" || len(toolCallMsg.GetToolCalls()) != 1 {
		t.Fatalf("assistant tool call prompt = %#v", toolCallMsg)
	}
	if toolCallMsg.Prompt != nil {
		t.Fatalf("tool call prompt must omit the prompt field, got %q", toolCallMsg.GetPrompt())
	}
	historicalCall := toolCallMsg.GetToolCalls()[0]
	if historicalCall.GetName() != "exec" || historicalCall.GetArgumentsJson() != `{"command":"ls"}` {
		t.Fatalf("historical tool call = %#v", historicalCall)
	}
	toolResult := converted.GetChatMessagePrompts()[2]
	if toolResult.GetToolCallId() != "call-1" || !toolResult.GetToolResultIsError() {
		t.Fatalf("tool result = %#v", toolResult)
	}
	if len(converted.GetTools()) != 2 || converted.GetTools()[0].GetName() != "exec" || converted.GetTools()[1].GetName() != "read" {
		t.Fatalf("tools = %#v", converted.GetTools())
	}
	if converted.GetTools()[0].GetDescription() != "exec" || converted.GetTools()[1].GetDescription() != "read" {
		t.Fatalf("sanitized descriptions = %q/%q", converted.GetTools()[0].GetDescription(), converted.GetTools()[1].GetDescription())
	}
	if len(converted.GetMetadata().GetF()) != 732 {
		t.Fatalf("fingerprint length = %d, want 732", len(converted.GetMetadata().GetF()))
	}
	if converted.GetMetadata().GetExtensionVersion() != "3000.2.17" || converted.GetMetadata().GetIdeVersion() != "3000.2.17" {
		t.Fatalf("client versions = %q/%q, want 3000.2.17", converted.GetMetadata().GetExtensionVersion(), converted.GetMetadata().GetIdeVersion())
	}
	if converted.GetRequestType() != devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE {
		t.Fatalf("request type = %v, want CASCADE", converted.GetRequestType())
	}
	if converted.GetCascadeId() == "" || converted.GetExecutionId() == "" {
		t.Fatalf("cascade/execution IDs = %q/%q, want non-empty", converted.GetCascadeId(), converted.GetExecutionId())
	}
	trajectory := converted.GetTrajectoryReference()
	if trajectory == nil || trajectory.GetTrajectoryId() == "" {
		t.Fatalf("trajectory reference = %#v, want ID", trajectory)
	}
	if trajectory.GetTrajectoryType() != devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE {
		t.Fatalf("trajectory type = %v, want CASCADE", trajectory.GetTrajectoryType())
	}
	if trajectory.GetStepType() != devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT {
		t.Fatalf("trajectory step type = %v, want USER_INPUT", trajectory.GetStepType())
	}
	if converted.GetPlannerMode() != devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT {
		t.Fatalf("planner mode = %v, want DEFAULT", converted.GetPlannerMode())
	}
	if converted.ProviderSource != nil {
		t.Fatalf("provider source = %v, want absent", converted.GetProviderSource())
	}
	if converted.GetConfiguration().GetMaxNewlines() != 400 {
		t.Fatalf("max newlines = %d, want 400", converted.GetConfiguration().GetMaxNewlines())
	}
}

// TestBuildRequestAggregatesThinkingBlocks 验证一条 assistant 消息的多个
// thinking 块按序拼接、签名取最后非空；纯 redacted 块（无可见文本）也生成
// wire 上的签名回放。
func TestBuildRequestAggregatesThinkingBlocks(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ThinkingContent{Thinking: "part-1", ThinkingSignature: "sig-1"},
				llm.ThinkingContent{Thinking: "part-2", ThinkingSignature: "sig-2"},
				llm.TextContent{Text: "answer"},
			}},
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "again"}}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ThinkingContent{ThinkingSignature: "sealed-x", Redacted: true},
				llm.ToolCall{ID: "call-1", Name: "exec", Arguments: json.RawMessage(`{}`)},
			}},
		},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	prompts := converted.GetChatMessagePrompts()
	textMsg := prompts[1]
	if textMsg.GetThinking() != "part-1\npart-2" || textMsg.GetSignature() != "sig-2" {
		t.Fatalf("multi-block thinking prompt = %#v", textMsg)
	}
	redactedCall := prompts[3]
	if !redactedCall.GetThinkingRedacted() || redactedCall.GetSignature() != "sealed-x" || redactedCall.GetThinking() != "" {
		t.Fatalf("redacted-only prompt = %#v", redactedCall)
	}
}

// TestValidateImagesForModelRejectsGLM 验证无视觉模型带图时返回可读错误（透传给客户端）。
func TestValidateImagesForModelRejectsGLM(t *testing.T) {
	request := llm.RequestMessages{
		Model: "glm-5-2",
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
			llm.TextContent{Text: "see"},
			llm.ImageContent{Data: "AAAA", MIMEType: "image/png"},
		}}},
	}
	a := &Adapter{}
	err := a.validateImagesForModel(request, "glm-5-2")
	if err == nil {
		t.Fatal("expected error for glm-5-2 + image")
	}
	if !strings.Contains(err.Error(), "does not support image") {
		t.Fatalf("error = %v, want does not support image", err)
	}
	if err := a.validateImagesForModel(request, "swe-1-7"); err != nil {
		t.Fatalf("swe-1-7 should allow images: %v", err)
	}
	if err := a.validateImagesForModel(llm.RequestMessages{Model: "glm-5-2", Messages: []llm.Message{
		llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}},
	}}, "glm-5-2"); err != nil {
		t.Fatalf("text-only glm should pass: %v", err)
	}
}

// TestValidateImagesUsesCatalog 验证目录缓存的 supports_images 优先于前缀启发式。
func TestValidateImagesUsesCatalog(t *testing.T) {
	request := llm.RequestMessages{
		Model: "future-vision",
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
			llm.ImageContent{Data: "AAAA", MIMEType: "image/png"},
		}}},
	}
	a := &Adapter{models: []adapter.ModelInfo{
		{ID: "glm-9-vision", SupportsImages: true},
		{ID: "swe-3-text", SupportsImages: false},
	}}
	// 目录声明支持图片时，即使名字像无视觉模型也放行。
	if err := a.validateImagesForModel(request, "glm-9-vision"); err != nil {
		t.Fatalf("catalog vision model should pass: %v", err)
	}
	// 目录声明不支持时直接拒绝。
	if err := a.validateImagesForModel(request, "swe-3-text"); err == nil {
		t.Fatal("catalog non-vision model should be rejected")
	}
}

// TestConnectErrorPassthrough 验证 Connect 错误 message 原样保留。
func TestConnectErrorPassthrough(t *testing.T) {
	err := connectError(connect.NewError(connect.CodeInvalidArgument, errors.New("model does not support images")))
	if err == nil || !strings.Contains(err.Error(), "invalid_argument") || !strings.Contains(err.Error(), "model does not support images") {
		t.Fatalf("connectError = %v", err)
	}
}

// TestBuildRequestOmitsHistoricalImages 验证多轮里只有最新用户消息挂 Images，历史图改占位。
func TestBuildRequestOmitsHistoricalImages(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "see this"},
				llm.ImageContent{Data: "AAAA", MIMEType: "image/png"},
			}},
			llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "ok"}}},
			llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "and this"},
				llm.ImageContent{Data: "BBBB", MIMEType: "image/jpeg"},
			}},
		},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	prompts := converted.GetChatMessagePrompts()
	if len(prompts) != 3 {
		t.Fatalf("prompts = %d, want 3", len(prompts))
	}
	if len(prompts[0].GetImages()) != 0 {
		t.Fatalf("history images = %#v, want empty", prompts[0].GetImages())
	}
	if !strings.Contains(prompts[0].GetPrompt(), "[Image omitted from history]") {
		t.Fatalf("history prompt = %q, want image placeholder", prompts[0].GetPrompt())
	}
	if len(prompts[2].GetImages()) != 1 || prompts[2].GetImages()[0].GetBase64Data() != "BBBB" {
		t.Fatalf("latest images = %#v, want BBBB", prompts[2].GetImages())
	}
	if strings.Contains(prompts[2].GetPrompt(), "[Image omitted from history]") {
		t.Fatalf("latest prompt should keep real image, got %q", prompts[2].GetPrompt())
	}
}

// TestBuildRequestAttachesImagesInSameTurn 验证同一轮中 UserMessage(image) + ToolResultMessage 都挂图片。
// Anthropic 客户端常把 image 和 tool_result 放在同一条 user 消息里，解码后拆成两条；
// 旧逻辑仅挂最后一条，导致图片丢失。
func TestBuildRequestAttachesImagesInSameTurn(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{
			llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "ok"}}},
			llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "see this"},
				llm.ImageContent{Data: "AAAA", MIMEType: "image/png"},
			}},
			llm.ToolResultMessage{ToolCallID: "tc1", ToolName: "read", Content: []llm.Content{llm.TextContent{Text: "file content"}}},
		},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	prompts := converted.GetChatMessagePrompts()
	// prompts: [assistant, user(image), tool_result]
	if len(prompts) != 3 {
		t.Fatalf("prompts = %d, want 3", len(prompts))
	}
	// user 消息在 assistant 之后，属于当前轮，图片应保留
	if len(prompts[1].GetImages()) != 1 || prompts[1].GetImages()[0].GetBase64Data() != "AAAA" {
		t.Fatalf("current-turn user images = %#v, want AAAA", prompts[1].GetImages())
	}
	if strings.Contains(prompts[1].GetPrompt(), "[Image omitted from history]") {
		t.Fatalf("current-turn prompt should keep real image, got %q", prompts[1].GetPrompt())
	}
}

// TestBuildRequestWithoutToolsKeepsPromptUnchanged 的测试动机是确保工具转换不会污染纯文本请求。
func TestBuildRequestWithoutToolsKeepsPromptUnchanged(t *testing.T) {
	request := llm.RequestMessages{SystemPrompt: "system", Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}}}}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetPrompt() != "system" {
		t.Fatalf("prompt = %q, want unchanged system prompt", converted.GetPrompt())
	}
	if len(converted.GetTools()) != 0 {
		t.Fatalf("tools = %#v, want none", converted.GetTools())
	}
}

// TestBuildRequestIgnoresEmptyToolDescriptions 的测试动机是避免没有说明文本的工具生成空提示章节。
func TestBuildRequestIgnoresEmptyToolDescriptions(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "system\n",
		Tools: []llm.ToolDefinition{
			{Name: "empty", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "read", Description: "  read a file  ", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	want := "system\n\n# tools descriptions\n<tool name=\"read\">\n1. read a file\n</tool>"
	if converted.GetPrompt() != want {
		t.Fatalf("prompt = %q, want %q", converted.GetPrompt(), want)
	}
}

// TestResponseDecoderMapsOneFrameToOrderedEvents 的测试动机是明确一个 Devin protobuf 帧可以包含多个 loop 语义。
func TestResponseDecoderMapsOneFrameToOrderedEvents(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	events := decoder.start()
	if len(events) != 1 || events[0].Type != llm.ResponseEventStart {
		t.Fatalf("start events = %#v", events)
	}
	events = decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaThinking:  proto.String("think"),
		DeltaSignature: proto.String("sig"),
		DeltaText:      proto.String("answer"),
		DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
			Id: proto.String("call"), Name: proto.String("exec"), ArgumentsJson: proto.String(`{"command":"ls"}`),
		}},
	})
	want := []llm.ResponseEventType{
		llm.ResponseEventThinkingStart,
		llm.ResponseEventThinkingDelta,
		llm.ResponseEventThinkingEnd,
		llm.ResponseEventTextStart,
		llm.ResponseEventTextDelta,
		llm.ResponseEventTextEnd,
		llm.ResponseEventToolCallStart,
		llm.ResponseEventToolCallDelta,
	}
	if len(events) != len(want) {
		t.Fatalf("event count = %d, want %d: %#v", len(events), len(want), events)
	}
	for index, eventType := range want {
		if events[index].Type != eventType {
			t.Fatalf("event[%d] = %q, want %q", index, events[index].Type, eventType)
		}
	}
	partial := events[len(events)-1].Partial
	if partial == nil || len(partial.Content) != 3 {
		t.Fatalf("partial = %#v, want three content blocks", partial)
	}
	thinking := partial.Content[0].(llm.ThinkingContent)
	if thinking.Thinking != "think" || thinking.ThinkingSignature != "sig" {
		t.Fatalf("thinking = %#v", thinking)
	}
}

// TestResponseDecoderMergesLateSignature 的测试动机是保证正文之后的
// 尾随签名帧合并回上一个思考块，而不是落成独立的空思考块。
func TestResponseDecoderMergesLateSignature(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaThinking: proto.String("think")})
	events := decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("answer")})
	events = decoder.decode(&devinproto.GetChatMessageResponse{DeltaSignature: proto.String("sig")})
	if len(events) != 1 || events[0].Type != llm.ResponseEventThinkingSignature {
		t.Fatalf("late signature events = %#v, want single thinking_signature", events)
	}
	if events[0].ContentIndex != 0 || events[0].Delta != "sig" {
		t.Fatalf("signature event = %#v", events[0])
	}
	thinking := events[0].Partial.Content[0].(llm.ThinkingContent)
	if thinking.Thinking != "think" || thinking.ThinkingSignature != "sig" {
		t.Fatalf("merged thinking = %#v", thinking)
	}
}

// TestResponseDecoderAggregatesToolArgumentFragments 的测试动机是保证事件保留原始增量，同时最终工具调用具有完整参数。
func TestResponseDecoderAggregatesToolArgumentFragments(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	decoder.start()
	first := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{Id: proto.String("call"), Name: proto.String("exec")}}})
	if len(first) != 1 || first[0].Type != llm.ResponseEventToolCallStart {
		t.Fatalf("first events = %#v, want tool start with tool call", first)
	}
	if first[0].ToolCallID != "call" || first[0].ToolName != "exec" {
		t.Fatalf("start tool identity = %q/%q", first[0].ToolCallID, first[0].ToolName)
	}
	second := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{ArgumentsJson: proto.String(`{"command":"`)}}})
	if err := second[0].Validate(); err != nil {
		t.Fatalf("incomplete tool delta Validate() error = %v", err)
	}
	partialCall := second[0].Partial.Content[0].(llm.ToolCall)
	if string(partialCall.Arguments) != `{}` {
		t.Fatalf("incomplete partial arguments = %s, want {}", partialCall.Arguments)
	}
	third := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{ArgumentsJson: proto.String(`ls"}`)}}})
	stopEvents := decoder.decode(&devinproto.GetChatMessageResponse{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL.Enum()})
	if second[0].ToolCallID != "call" || third[0].ToolCallID != "call" {
		t.Fatalf("tool delta IDs = %q, %q", second[0].ToolCallID, third[0].ToolCallID)
	}
	if second[0].Delta != `{"command":"` || third[0].Delta != `ls"}` {
		t.Fatalf("tool deltas = %q, %q", second[0].Delta, third[0].Delta)
	}
	if len(stopEvents) != 0 {
		t.Fatalf("stop events = %#v, want no final event before EOF", stopEvents)
	}
	events := decoder.finish(nil)
	done := events[len(events)-1]
	if done.Type != llm.ResponseEventDone || done.Message == nil {
		t.Fatalf("done event = %#v", done)
	}
	call := done.Message.Content[0].(llm.ToolCall)
	if string(call.Arguments) != `{"command":"ls"}` {
		t.Fatalf("arguments = %s", call.Arguments)
	}
	if done.Message.StopReason != llm.StopReasonToolUse {
		t.Fatalf("stop reason = %q", done.Message.StopReason)
	}
}

// TestResponseDecoderConsumesUsageAfterStopReason 的测试动机是匹配 Devin 在停止原因后发送最终 token 统计帧的真实顺序。
func TestResponseDecoderConsumesUsageAfterStopReason(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("complete")})
	stopEvents := decoder.decode(&devinproto.GetChatMessageResponse{
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
	})
	if len(stopEvents) != 0 || decoder.finished {
		t.Fatalf("stop frame events = %#v, finished = %v; want continued upstream consumption", stopEvents, decoder.finished)
	}
	usageEvents := decoder.decode(&devinproto.GetChatMessageResponse{Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{
		InputTokens: proto.Uint64(167), OutputTokens: proto.Uint64(61), CacheReadTokens: proto.Uint64(12195),
	}})
	if len(usageEvents) != 0 {
		t.Fatalf("usage frame events = %#v, want metadata-only frame", usageEvents)
	}
	events := decoder.finish(nil)
	done := events[len(events)-1]
	if done.Type != llm.ResponseEventDone || done.Reason != llm.StopReasonStop || done.Message == nil {
		t.Fatalf("done event = %#v", done)
	}
	usage := done.Message.Usage
	if usage.Input != 167 || usage.Output != 61 || usage.CacheRead != 12195 || usage.CacheWrite != 0 || usage.TotalTokens != 12423 {
		t.Fatalf("usage = %#v, want captured Devin totals", usage)
	}
}

// TestResponseStreamReadsUsageFrameAfterStopReason 的测试动机是保证 transport 不会因 stop 帧提前停止读取后续 usage 帧。
func TestResponseStreamReadsUsageFrameAfterStopReason(t *testing.T) {
	receiver := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("complete"), MessageId: proto.String("message-1")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		{Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{
			ModelUid: proto.String("actual-model"), InputTokens: proto.Uint64(167), OutputTokens: proto.Uint64(61), CacheReadTokens: proto.Uint64(12195),
		}},
		{},
	}}
	stream := &responseStream{upstream: receiver, decoder: newResponseDecoder("requested-model", nil)}
	var done llm.ResponseEvent
	for {
		event, err := stream.Recv(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventDone {
			done = event
		}
	}
	if receiver.index != len(receiver.responses) {
		t.Fatalf("read frame count = %d, want %d", receiver.index, len(receiver.responses))
	}
	if done.Message == nil || done.Message.ResponseID != "message-1" || done.Message.ResponseModel != "actual-model" {
		t.Fatalf("done message identity = %#v", done.Message)
	}
	if done.Message.Usage.TotalTokens != 12423 {
		t.Fatalf("done usage = %#v", done.Message.Usage)
	}
}

// TestResponseDecoderRejectsEOFWithoutStopReason 的测试动机是防止把上游截断伪装成
// 正常结束：Devin 的正常收尾必带 stopReason 帧，干净 EOF 却缺它说明流被截断。
func TestResponseDecoderRejectsEOFWithoutStopReason(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("partial")})
	event := decoder.finish(nil)[0]
	if event.Type != llm.ResponseEventError || event.Error == nil || event.Error.ErrorMessage != "Devin stream ended without stop reason" {
		t.Fatalf("event = %#v, want truncation error", event)
	}
}

// TestResponseDecoderRejectsEmptyNormalEOF 的测试动机是避免把未产生任何内容的异常空流误报为成功。
func TestResponseDecoderRejectsEmptyNormalEOF(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	decoder.start()
	event := decoder.finish(nil)[0]
	if event.Type != llm.ResponseEventError || event.Error == nil || event.Error.ErrorMessage != "Devin stream ended without generated content" {
		t.Fatalf("event = %#v, want empty-stream error", event)
	}
}

// TestResponseDecoderCompletesPartialWithThinking 验证 STOP_REASON_PARTIAL 不吞掉已生成的思考/文本。
func TestResponseDecoderCompletesPartialWithThinking(t *testing.T) {
	decoder := newResponseDecoder("model", nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaThinking: proto.String("think")})
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("hello"), StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_PARTIAL.Enum()})
	events := decoder.finish(nil)
	done := events[len(events)-1]
	if done.Type != llm.ResponseEventDone || done.Reason != llm.StopReasonLength || done.Message == nil {
		t.Fatalf("done event = %#v, want done with length", done)
	}
	if done.Message.Content[0].(llm.ThinkingContent).Thinking != "think" {
		t.Fatalf("thinking missing or wrong: %#v", done.Message.Content)
	}
	if done.Message.Content[1].(llm.TextContent).Text != "hello" {
		t.Fatalf("text missing or wrong: %#v", done.Message.Content)
	}
}

func TestMapStopReason(t *testing.T) {
	cases := []struct {
		input devinproto.ExaCodeiumCommonPb_StopReason
		want  llm.StopReason
	}{
		{devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_TOKENS, llm.StopReasonLength},
		{devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_INCOMPLETE, llm.StopReasonLength},
		{devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_PARTIAL, llm.StopReasonLength},
		{devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL, llm.StopReasonToolUse},
		{devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_ERROR, llm.StopReasonError},
		{devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN, llm.StopReasonStop},
	}
	for _, testCase := range cases {
		if got := mapStopReason(testCase.input); got != testCase.want {
			t.Fatalf("mapStopReason(%v) = %q, want %q", testCase.input, got, testCase.want)
		}
	}
}

// TestRecordProtoJSONRedactsMetadata 的测试动机是确保 Devin 原始请求可诊断但不会写出 token 和设备指纹。
func TestRecordProtoJSONRedactsMetadata(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	recorder := debuglog.NewManager(root, debuglog.RetentionPolicy{}).Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/responses"})
	request := &devinproto.GetChatMessageRequest{
		Metadata: &devinproto.ExaCodeiumCommonPb_Metadata{ApiKey: proto.String("secret-token"), F: proto.String("fingerprint")},
		Prompt:   proto.String("hello"),
	}
	recordProtoJSON(recorder, "03-devin-request.json", request)
	recordProtoJSON(recorder, "04-devin-response.jsonl", &devinproto.GetChatMessageResponse{DeltaText: proto.String("world")})
	recorder.Complete(debuglog.Completion{})

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, entries[0].Name())
	requestLog, err := os.ReadFile(filepath.Join(directory, "03-devin-request.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestLog), "secret-token") || strings.Contains(string(requestLog), "fingerprint") {
		t.Fatalf("request log contains credentials: %s", requestLog)
	}
	responseLog, err := os.ReadFile(filepath.Join(directory, "04-devin-response.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(responseLog), `"deltaText":"world"`) {
		t.Fatalf("response log = %s", responseLog)
	}
	if strings.Contains(string(responseLog), `"seq":`) || strings.Contains(string(responseLog), `"data":`) {
		t.Fatalf("raw protobuf response must not use an event envelope: %s", responseLog)
	}
}

func TestBuildRequestForwardsSamplingParams(t *testing.T) {
	maxTokens := 4096
	temperature := 0.2
	topP := 0.8
	topK := 10
	seed := int64(42)
	request := llm.RequestMessages{
		SystemPrompt:  "system",
		MaxTokens:     &maxTokens,
		Temperature:   &temperature,
		TopP:          &topP,
		TopK:          &topK,
		Seed:          &seed,
		StopSequences: []string{"STOP"},
		Messages:      []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	configuration := converted.GetConfiguration()
	if configuration.GetMaxTokens() != 4096 || configuration.GetTemperature() != 0.2 ||
		configuration.GetTopP() != 0.8 || configuration.GetTopK() != 10 ||
		configuration.GetSeed() != 42 {
		t.Fatalf("configuration = %#v", configuration)
	}
	if len(configuration.GetStopPatterns()) != 1 || configuration.GetStopPatterns()[0] != "STOP" {
		t.Fatalf("stop patterns = %v", configuration.GetStopPatterns())
	}
}

func TestBuildRequestDefaultSamplingParams(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "system",
		Messages:     []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
	}
	converted, err := buildRequest(request, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	configuration := converted.GetConfiguration()
	if configuration.GetMaxTokens() != 128000 || configuration.GetTemperature() != 1 ||
		configuration.GetTopP() != 0.95 || configuration.GetTopK() != 40 {
		t.Fatalf("default configuration = %#v", configuration)
	}
}

func TestDeriveSessionIDsStableForSamePrefix(t *testing.T) {
	base := llm.RequestMessages{
		SystemPrompt: "system",
		SessionKey:   "user-1",
		Messages:     []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "task"}}}},
	}
	first, err := buildRequest(base, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	// 同一会话追加了新消息：前缀不变，trajectory/cascade 必须稳定。
	base.Messages = append(base.Messages,
		llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "answer"}}},
		llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "follow up"}}},
	)
	second, err := buildRequest(base, Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetTrajectoryReference().GetTrajectoryId() != second.GetTrajectoryReference().GetTrajectoryId() ||
		first.GetCascadeId() != second.GetCascadeId() {
		t.Fatalf("session IDs changed across turns of the same conversation")
	}
	if first.GetExecutionId() == second.GetExecutionId() {
		t.Fatalf("execution ID must stay unique per request")
	}
}

// TestDeriveSessionIDSSurvivesCompaction 验证带 SessionKey 的会话在压缩改写
// 首条消息后仍得到同一 trajectory/cascade ID——SessionKey 即会话契约。
func TestDeriveSessionIDSSurvivesCompaction(t *testing.T) {
	makeRequest := func(text string) llm.RequestMessages {
		return llm.RequestMessages{
			SystemPrompt: "system",
			SessionKey:   "session-1",
			Messages:     []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: text}}}},
		}
	}
	first, err := buildRequest(makeRequest("original first message"), Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRequest(makeRequest("[summary of compacted history]"), Config{BaseURL: "https://example.com", Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if first.GetTrajectoryReference().GetTrajectoryId() != second.GetTrajectoryReference().GetTrajectoryId() ||
		first.GetCascadeId() != second.GetCascadeId() {
		t.Fatalf("session IDs must survive compaction for keyed sessions")
	}
}

func TestDeriveSessionIDSDifferAcrossConversations(t *testing.T) {
	makeRequest := func(key, text string) llm.RequestMessages {
		return llm.RequestMessages{
			SystemPrompt: "system",
			SessionKey:   key,
			Messages:     []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: text}}}},
		}
	}
	cfg := Config{BaseURL: "https://example.com", Token: "token", Model: "model"}
	first, err := buildRequest(makeRequest("session-1", "task A"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := buildRequest(makeRequest("session-2", "task A"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetTrajectoryReference().GetTrajectoryId() == second.GetTrajectoryReference().GetTrajectoryId() {
		t.Fatalf("distinct session keys must not share a trajectory")
	}
	// 无 SessionKey 的客户端退回内容哈希：不同首条消息仍自然分散。
	third, err := buildRequest(makeRequest("", "task B"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	fourth, err := buildRequest(makeRequest("", "task C"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if third.GetTrajectoryReference().GetTrajectoryId() == fourth.GetTrajectoryReference().GetTrajectoryId() {
		t.Fatalf("keyless distinct conversations must not share a trajectory")
	}
}

// TestIsTransientConnectError 验证只对纯传输错误重试：上游 unavailable
// 实测是确定性语义错误（router 直连、未开放端点），重试永远得到同样失败。
func TestIsTransientConnectError(t *testing.T) {
	if !isTransientConnectError(io.ErrUnexpectedEOF) {
		t.Fatal("unexpected EOF must be retryable")
	}
	if isTransientConnectError(connect.NewError(connect.CodeUnavailable, errors.New("try later"))) {
		t.Fatal("unavailable must not be retried")
	}
	if isTransientConnectError(connect.NewError(connect.CodePermissionDenied, errors.New("blocked"))) {
		t.Fatal("permission_denied must not be retried")
	}
	if isTransientConnectError(connect.NewError(connect.CodeInvalidArgument, errors.New("bad request"))) {
		t.Fatal("invalid_argument must not be retried")
	}
}

// TestResponseStreamYieldsErrorBeforeStart 的测试动机是：上游在产出任何内容
// 前失败时，首个对外事件必须是 error 而不是 start——否则 HTTP 层在 start
// 时已提交 200，真实错误状态码无法回传，下游网关会把请求级错误误判为
// 渠道故障并冷却整个渠道。
func TestResponseStreamYieldsErrorBeforeStart(t *testing.T) {
	stream := &responseStream{
		upstream: &errorDevinResponseReceiver{err: connect.NewError(connect.CodePermissionDenied, errors.New("blocked by content policy"))},
		decoder:  newResponseDecoder("model", nil),
	}
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != llm.ResponseEventError {
		t.Fatalf("first event = %q, want error", event.Type)
	}
	if event.Error == nil || !strings.Contains(event.Error.ErrorMessage, "permission_denied") {
		t.Fatalf("error message = %#v", event.Error)
	}
}

// TestResponseStreamStartsBeforeFirstContent 的测试动机是保证正常流中
// start 仍是第一个事件，仅在上游内容就绪时才随首批事件下发。
func TestResponseStreamStartsBeforeFirstContent(t *testing.T) {
	receiver := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{InputTokens: proto.Uint64(1)}},
		{DeltaText: proto.String("hi")},
	}}
	stream := &responseStream{upstream: receiver, decoder: newResponseDecoder("model", nil)}
	first, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first.Type != llm.ResponseEventStart {
		t.Fatalf("first event = %q, want start", first.Type)
	}
	second, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if second.Type != llm.ResponseEventTextStart {
		t.Fatalf("second event = %q, want text_start", second.Type)
	}
}

// TestBuildRequestToolChoiceMapping 验证 tool_choice 映射到上游 oneof：
// required/none 走 option_name，named 走 tool_name，auto 缺省不发。
func TestBuildRequestToolChoiceMapping(t *testing.T) {
	cfg := Config{BaseURL: "https://example.com", Token: "token", Model: "model"}
	request := llm.RequestMessages{Messages: []llm.Message{
		llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}},
	}}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceRequired}
	converted, err := buildRequest(request, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice().GetOptionName() != "required" {
		t.Fatalf("tool_choice = %#v, want option_name=required", converted.GetToolChoice())
	}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceNone}
	converted, err = buildRequest(request, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice().GetOptionName() != "none" {
		t.Fatalf("tool_choice = %#v, want option_name=none", converted.GetToolChoice())
	}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: "read_file"}
	converted, err = buildRequest(request, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice().GetToolName() != "read_file" {
		t.Fatalf("tool_choice = %#v, want tool_name=read_file", converted.GetToolChoice())
	}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	converted, err = buildRequest(request, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice() != nil {
		t.Fatalf("auto tool_choice should be omitted, got %#v", converted.GetToolChoice())
	}

	request.ToolChoice = nil
	request.DisableParallelToolCalls = true
	converted, err = buildRequest(request, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !converted.GetDisableParallelToolCalls() {
		t.Fatal("disable_parallel_tool_calls not set")
	}
}

// TestResponseDecoderLocalStopSequence 验证上游不执行 stop_patterns 时
// 解码层本地截断：命中处关闭文字块，剩余上游帧只更新用量。
func TestResponseDecoderLocalStopSequence(t *testing.T) {
	decoder := newResponseDecoder("model", []string{"STOP"})
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("hello STOP world")})
	var types []llm.ResponseEventType
	var text string
	for _, event := range events {
		types = append(types, event.Type)
		if event.Type == llm.ResponseEventTextDelta {
			text += event.Delta
		}
	}
	if text != "hello " {
		t.Fatalf("emitted text = %q, want %q", text, "hello ")
	}
	last := events[len(events)-1]
	if last.Type != llm.ResponseEventTextEnd || last.Content != "hello " {
		t.Fatalf("last event = %#v, want text_end with truncated content", last)
	}
	// 截断后上游继续吐的帧不再产生对外事件。
	if later := decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String(" more")}); len(later) != 0 {
		t.Fatalf("post-truncation events = %#v, want none", later)
	}
	done := decoder.finish(nil)
	if len(done) != 1 || done[0].Type != llm.ResponseEventDone {
		t.Fatalf("finish events = %#v, want done", done)
	}
	if done[0].Reason != llm.StopReasonStopSequence || done[0].Message.StopSequence != "STOP" {
		t.Fatalf("done = %#v, want stopSequence reason with matched pattern", done[0])
	}
}

// TestResponseDecoderStopSequenceAcrossDeltas 验证跨帧停止序列：
// 第一帧尾部的疑似前缀不下发，第二帧补全后立即截断。
func TestResponseDecoderStopSequenceAcrossDeltas(t *testing.T) {
	decoder := newResponseDecoder("model", []string{"XYZ"})
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("abc XY")})
	var emitted string
	for _, event := range events {
		if event.Type == llm.ResponseEventTextDelta {
			emitted += event.Delta
		}
	}
	// "XY" 可能是 "XYZ" 的不完整前缀，只能下safe发窗口内部分。
	if emitted != "abc " {
		t.Fatalf("first delta emitted = %q, want %q", emitted, "abc ")
	}
	events = decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("Z tail")})
	emitted = ""
	var endContent string
	for _, event := range events {
		if event.Type == llm.ResponseEventTextDelta {
			emitted += event.Delta
		}
		if event.Type == llm.ResponseEventTextEnd {
			endContent = event.Content
		}
	}
	if emitted != "" || endContent != "abc " {
		t.Fatalf("after match: emitted=%q end=%q, want no extra delta, content 'abc '", emitted, endContent)
	}
	done := decoder.finish(nil)
	if done[0].Reason != llm.StopReasonStopSequence {
		t.Fatalf("reason = %q, want stopSequence", done[0].Reason)
	}
}

// TestResponseDecoderNoStopMatchFlushesTail 验证未命中时保留的尾部
// 在文字块关闭时随最后一个 delta 全部下发。
func TestResponseDecoderNoStopMatchFlushesTail(t *testing.T) {
	decoder := newResponseDecoder("model", []string{"STOP"})
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("hi")})
	if len(events) != 1 || events[0].Type != llm.ResponseEventTextStart {
		t.Fatalf("events = %#v, want only text_start (tail withheld)", events)
	}
	// 工具调用帧触发文字块收尾，尾部 "hi" 随 delta 下发。
	events = decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
		Id: proto.String("c"), Name: proto.String("exec"), ArgumentsJson: proto.String(`{}`),
	}}})
	var emitted, endContent string
	for _, event := range events {
		if event.Type == llm.ResponseEventTextDelta {
			emitted += event.Delta
		}
		if event.Type == llm.ResponseEventTextEnd {
			endContent = event.Content
		}
	}
	if emitted != "hi" || endContent != "hi" {
		t.Fatalf("tail flush: delta=%q content=%q, want 'hi'", emitted, endContent)
	}
	decoder.decode(&devinproto.GetChatMessageResponse{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL.Enum()})
	done := decoder.finish(nil)
	last := done[len(done)-1]
	if last.Type != llm.ResponseEventDone || last.Reason != llm.StopReasonToolUse {
		t.Fatalf("done = %#v, want toolUse", last)
	}
}

func TestRepairLeakedXMLArguments(t *testing.T) {
	raw := `<parameter name="command">ls -la</parameter><antml:parameter name="path">/tmp</antml:parameter>`
	repaired, ok := repairLeakedXMLArguments(raw)
	if !ok {
		t.Fatal("expected repair to succeed")
	}
	var args map[string]string
	if err := json.Unmarshal(repaired, &args); err != nil {
		t.Fatalf("repaired args not JSON: %v", err)
	}
	if args["command"] != "ls -la" || args["path"] != "/tmp" {
		t.Fatalf("args = %v", args)
	}
	if _, ok := repairLeakedXMLArguments(`{"command":"ls"}`); ok {
		t.Fatal("plain JSON must not be treated as leaked XML")
	}
	if _, ok := repairLeakedXMLArguments(`garbage`); ok {
		t.Fatal("no tags → no repair")
	}
}
