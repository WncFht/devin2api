// 本文件验证 Devin 请求字段映射和响应增量聚合。
package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
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

// stalledDevinResponseReceiver 模拟上游建立后不再产出任何帧的挂死流：
// Receive 阻塞至 release 关闭，供静默看门狗与取消路径的测试使用。
type stalledDevinResponseReceiver struct {
	release chan struct{}
}

// Receive 阻塞到 release 关闭再报告流结束。
func (receiver *stalledDevinResponseReceiver) Receive() bool {
	<-receiver.release
	return false
}

// Msg 没有可返回的帧。
func (receiver *stalledDevinResponseReceiver) Msg() *devinproto.GetChatMessageResponse { return nil }

// Err 模拟正常 EOF。
func (receiver *stalledDevinResponseReceiver) Err() error { return nil }

// hangAfterReceiver 先按脚本发帧、发完后阻塞在 release 上：
// 模拟「内容到齐但传输不收尾」或「只发零事件帧续命」的退化流。
type hangAfterReceiver struct {
	responses []*devinproto.GetChatMessageResponse
	index     int
	current   *devinproto.GetChatMessageResponse
	release   chan struct{}
}

// Receive 发完脚本帧后阻塞到 release 关闭。
func (receiver *hangAfterReceiver) Receive() bool {
	if receiver.index >= len(receiver.responses) {
		<-receiver.release
		return false
	}
	receiver.current = receiver.responses[receiver.index]
	receiver.index++
	return true
}

// Msg 返回最近一次成功读取的帧。
func (receiver *hangAfterReceiver) Msg() *devinproto.GetChatMessageResponse {
	return receiver.current
}

// Err 模拟正常 EOF。
func (receiver *hangAfterReceiver) Err() error { return nil }

// heartbeatAfterReceiver 发完脚本帧后无限续发同一心跳帧（零事件活性帧），
// 模拟「上游有帧流动但永不产出内容」的退化流——静默看门狗被帧到达喂活，
// 只能由无进度期限兜底。
type heartbeatAfterReceiver struct {
	responses []*devinproto.GetChatMessageResponse
	index     int
	current   *devinproto.GetChatMessageResponse
	heartbeat *devinproto.GetChatMessageResponse
}

// Receive 先发脚本帧，之后无限报告心跳帧。
func (receiver *heartbeatAfterReceiver) Receive() bool {
	if receiver.index < len(receiver.responses) {
		receiver.current = receiver.responses[receiver.index]
		receiver.index++
		return true
	}
	receiver.current = receiver.heartbeat
	return true
}

// Msg 返回最近一次成功读取的帧。
func (receiver *heartbeatAfterReceiver) Msg() *devinproto.GetChatMessageResponse {
	return receiver.current
}

// Err 模拟正常 EOF。
func (receiver *heartbeatAfterReceiver) Err() error { return nil }

func TestBuildRequestMapsLoopMessages(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "system",
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ThinkingContent{Thinking: "think", ThinkingSignature: "sig"},
				llm.ToolCall{ID: "call-1", Name: "exec", Arguments: json.RawMessage(`{"command":"ls"}`)},
			}},
			llm.ToolResultMessage{ToolCallID: "call-1", IsError: true, Content: []llm.Content{llm.TextContent{Text: "failed"}}},
		},
		Tools: []llm.ToolDefinition{
			{Name: "exec", Description: "run", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "read", Description: "read file", InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`)},
		},
	}
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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

// TestBuildRequestMergesParallelToolCalls 验证一个助手回合的文本与多个
// 并行 tool call 合并为单条 prompt（真实 chisel 客户端的 wire 形态），
// 对应结果按 call 序紧随其后，不产生相邻 SYSTEM 消息。
func TestBuildRequestMergesParallelToolCalls(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "check both"}}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.TextContent{Text: "Reading both files."},
				llm.ToolCall{ID: "call-a", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
				llm.ToolCall{ID: "call-b", Name: "read", Arguments: json.RawMessage(`{"path":"b"}`)},
			}},
			llm.ToolResultMessage{ToolCallID: "call-a", Content: []llm.Content{llm.TextContent{Text: "a-body"}}},
			llm.ToolResultMessage{ToolCallID: "call-b", Content: []llm.Content{llm.TextContent{Text: "b-body"}}},
		},
	}
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	prompts := converted.GetChatMessagePrompts()
	if len(prompts) != 4 {
		t.Fatalf("message count = %d, want 4 (user, merged assistant, result, result)", len(prompts))
	}
	assistant := prompts[1]
	if assistant.GetSource() != assistantSource {
		t.Fatalf("assistant source = %v", assistant.GetSource())
	}
	if assistant.GetPrompt() != "Reading both files." || len(assistant.GetToolCalls()) != 2 {
		t.Fatalf("merged assistant prompt = %#v", assistant)
	}
	if assistant.GetToolCalls()[0].GetId() != "call-a" || assistant.GetToolCalls()[1].GetId() != "call-b" {
		t.Fatalf("tool call order = %q, %q", assistant.GetToolCalls()[0].GetId(), assistant.GetToolCalls()[1].GetId())
	}
	for index, want := range []string{"call-a", "call-b"} {
		if prompts[2+index].GetToolCallId() != want {
			t.Fatalf("result %d tool_call_id = %q, want %q", index, prompts[2+index].GetToolCallId(), want)
		}
	}
}

// TestBuildRequestReportsRepairs 验证请求投影的静默修复逐类计数：分组式
// call/result 历史被重排、孤立结果降级、空 assistant 丢弃、历史图剥离，
// 加上 sanitize 命中——合计即落进 meta.json 的 repairs 总量。
func TestBuildRequestReportsRepairs(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "You are Claude Code, Anthropic's official CLI for Claude.",
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "earlier turn with a screenshot"},
				llm.ImageContent{Data: "AAAA", MIMEType: "image/png"},
			}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{"path":"a"}`)},
			}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ToolCall{ID: "call-2", Name: "read", Arguments: json.RawMessage(`{"path":"b"}`)},
			}},
			llm.ToolResultMessage{ToolCallID: "call-1", Content: []llm.Content{llm.TextContent{Text: "a-body"}}},
			llm.ToolResultMessage{ToolCallID: "call-2", Content: []llm.Content{llm.TextContent{Text: "b-body"}}},
			llm.ToolResultMessage{ToolCallID: "call-lost", Content: []llm.Content{llm.TextContent{Text: "orphan"}}},
			llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: ""}}},
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "next step"}}},
		},
	}
	var sanitizeHits map[string]int
	request, sanitizeHits = sanitizeRequest(request)
	// 孤儿结果降级发生在 IR 层（解码尾），这里手动补一遍以模拟
	// 真实管线到达 buildRequest 前的形态。
	request.DemoteOrphanToolResults()
	converted, repairs, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	repairs.SanitizeHits = sanitizeHits
	if repairs.ReorderedPrompts != 2 {
		t.Fatalf("reordered = %d, want 2", repairs.ReorderedPrompts)
	}
	if repairs.DroppedEmptyAssistant != 1 {
		t.Fatalf("dropped empty assistant = %d, want 1", repairs.DroppedEmptyAssistant)
	}
	if repairs.OmittedHistoryImages != 1 {
		t.Fatalf("omitted images = %d, want 1", repairs.OmittedHistoryImages)
	}
	if repairs.SanitizeHits["a1-cc-full"] != 1 {
		t.Fatalf("sanitize hits = %#v, want a1-cc-full:1", repairs.SanitizeHits)
	}
	if repairs.Total() != 5 {
		t.Fatalf("total = %d, want 5", repairs.Total())
	}
	// 孤立结果被降级为 USER 文本保住内容，其余 prompt 数量不变。
	prompts := converted.GetChatMessagePrompts()
	if len(prompts) != 7 {
		t.Fatalf("prompts = %d, want 7", len(prompts))
	}
	if prompts[5].GetSource() != devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER ||
		!strings.Contains(prompts[5].GetPrompt(), "original call lost") {
		t.Fatalf("demoted prompt = %v %q", prompts[5].GetSource(), prompts[5].GetPrompt())
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
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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

// TestConnectErrorPassthrough 验证 Connect 错误分类后 code + message 原样保留。
func TestConnectErrorPassthrough(t *testing.T) {
	err := llm.Classify(connect.NewError(connect.CodeInvalidArgument, errors.New("model does not support images")))
	if err == nil || !strings.Contains(err.Error(), "invalid_argument") || !strings.Contains(err.Error(), "model does not support images") {
		t.Fatalf("Classify = %v", err)
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
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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
			llm.ToolResultMessage{ToolCallID: "tc1", Content: []llm.Content{llm.TextContent{Text: "file content"}}},
		},
	}
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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
	decoder := newResponseDecoder("model", nil, nil, nil)
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
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaThinking: proto.String("think")})
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("answer")})
	events := decoder.decode(&devinproto.GetChatMessageResponse{DeltaSignature: proto.String("sig")})
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

// TestResponseDecoderSynthesizesThinkingForBareSignature 的测试动机是 openai 体制
// 上游只有签名没有思考正文：合成块必须走 start/end 完整生命周期，编码器从
// Partial 边界取签名；发 thinking_signature 会与边界读取叠加翻倍并让 item 悬挂。
func TestResponseDecoderSynthesizesThinkingForBareSignature(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaSignature:     proto.String("sig"),
		DeltaSignatureType: proto.String("openai"),
	})
	want := []llm.ResponseEventType{llm.ResponseEventThinkingStart, llm.ResponseEventThinkingEnd}
	if len(events) != len(want) {
		t.Fatalf("bare signature events = %#v, want start+end", events)
	}
	for index, eventType := range want {
		if events[index].Type != eventType || events[index].ContentIndex != 0 {
			t.Fatalf("event[%d] = %#v, want %q at index 0", index, events[index], eventType)
		}
	}
	thinking := events[1].Partial.Content[0].(llm.ThinkingContent)
	if thinking.ThinkingSignature != "sig" || thinking.SignatureType != "openai" {
		t.Fatalf("synthesized thinking = %#v", thinking)
	}
	// 后续裸签名帧按 merge 路径并入同一合成块。
	events = decoder.decode(&devinproto.GetChatMessageResponse{DeltaSignature: proto.String("2")})
	if len(events) != 1 || events[0].Type != llm.ResponseEventThinkingSignature || events[0].Delta != "2" {
		t.Fatalf("second signature events = %#v, want thinking_signature delta", events)
	}
	thinking = events[0].Partial.Content[0].(llm.ThinkingContent)
	if thinking.ThinkingSignature != "sig2" {
		t.Fatalf("merged synthesized thinking = %#v", thinking)
	}
}

// TestResponseDecoderAggregatesToolArgumentFragments 的测试动机是保证事件保留原始增量，同时最终工具调用具有完整参数。
func TestResponseDecoderAggregatesToolArgumentFragments(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
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

// TestResponseDecoderBindsLateToolCallID 的测试动机是首帧缺 id 的退化形态：
// 合成占位发出 ToolCallStart 后真实 id 晚到，事件侧必须继续用占位 id
// （客户端已按它对账），而最终消息的内容块要回填真 id 供下轮回放。
func TestResponseDecoderBindsLateToolCallID(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	first := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
		Name: proto.String("exec"), ArgumentsJson: proto.String(`{"command":"`),
	}}})
	if len(first) == 0 || first[0].Type != llm.ResponseEventToolCallStart || first[0].ToolCallID != "call_0" {
		t.Fatalf("first events = %#v, want tool start with placeholder call_0", first)
	}
	second := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
		Id: proto.String("real-id"), ArgumentsJson: proto.String(`ls"}`),
	}}})
	for _, event := range second {
		if event.ToolCallID != "call_0" {
			t.Fatalf("delta ToolCallID = %q, want pinned placeholder call_0", event.ToolCallID)
		}
	}
	call := decoder.partial.Content[0].(llm.ToolCall)
	if call.ID != "real-id" {
		t.Fatalf("partial call id = %q, want backfilled real-id", call.ID)
	}
	decoder.decode(&devinproto.GetChatMessageResponse{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL.Enum()})
	done := decoder.finish(nil)
	final := done[len(done)-1].Message.Content[0].(llm.ToolCall)
	if final.ID != "real-id" || string(final.Arguments) != `{"command":"ls"}` {
		t.Fatalf("final call = %#v", final)
	}
}

// TestResponseDecoderSplitsSecondIdlessCall 验证带新名字的后续无 id 帧开启
// 新调用而不是并入上一个——无 id 续帧按位置归并，但名字不同就是另一次调用。
func TestResponseDecoderSplitsSecondIdlessCall(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
		Id: proto.String("a"), Name: proto.String("exec"),
	}}})
	second := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
		Name: proto.String("read"),
	}}})
	if len(second) != 1 || second[0].Type != llm.ResponseEventToolCallStart || second[0].ToolCallID != "call_1" {
		t.Fatalf("second events = %#v, want new tool start call_1", second)
	}
	if len(decoder.tools) != 2 {
		t.Fatalf("tools = %d, want 2 separate calls", len(decoder.tools))
	}
}

// TestResponseDecoderUnwrapsCustomToolArguments 验证 custom 声明工具的
// wire 包装形态：start 即标记 Custom（下游 item kind 是 custom_tool_call），
// {"input":"<原文>"} 片段流中不产生 delta，结束帧解包成 freeform 原文。
func TestResponseDecoderUnwrapsCustomToolArguments(t *testing.T) {
	decoder := newResponseDecoder("model", nil, map[string]bool{"apply_patch": true}, nil)
	decoder.start()
	first := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{Id: proto.String("apply_patch_0"), Name: proto.String("apply_patch")}}})
	if len(first) != 1 || first[0].Type != llm.ResponseEventToolCallStart {
		t.Fatalf("first events = %#v, want tool start", first)
	}
	if call := first[0].Partial.Content[0].(llm.ToolCall); !call.Custom {
		t.Fatalf("wrapped call Custom = false at start, want true")
	}
	second := decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{ArgumentsJson: proto.String(`{"input": "*** Begin Patch`)}}})
	if len(second) != 0 {
		t.Fatalf("wrapped fragment emitted %d events, want buffered", len(second))
	}
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{ArgumentsJson: proto.String(`\n*** End Patch"}`)}}})
	decoder.decode(&devinproto.GetChatMessageResponse{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL.Enum()})
	events := decoder.finish(nil)
	var call llm.ToolCall
	var sawFullDelta bool
	for _, event := range events {
		if event.Type == llm.ResponseEventToolCallDelta && event.Delta == "*** Begin Patch\n*** End Patch" {
			sawFullDelta = true
		}
		if event.Type == llm.ResponseEventToolCallEnd && event.ToolCall != nil {
			call = *event.ToolCall
		}
	}
	if !sawFullDelta {
		t.Fatalf("no unwrapped input delta emitted for wrapped call")
	}
	if !call.Custom || string(call.Arguments) != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("call = %#v, want custom with raw patch text", call)
	}
}

// TestResponseDecoderConsumesUsageAfterStopReason 的测试动机是匹配 Devin 在停止原因后发送最终 token 统计帧的真实顺序。
func TestResponseDecoderConsumesUsageAfterStopReason(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
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
	stream := &responseStream{frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("requested-model", nil, nil, nil), gate: newRateGate(GateConfig{}, nil, "")}
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

// TestServerToolContinuationPairsAllCalls 的测试动机是钉住多跳托管续轮
// 的两处回归：换流时本跳尾帧（toolcall_end/server_tool_result）必须照常
// 下发，且续轮 wire 要为前序各跳已回答的 Server 调用补齐结果——漏发
// 会把未配对调用裸发上行，被上游以 invalid_argument 拒收。
func TestServerToolContinuationPairsAllCalls(t *testing.T) {
	ctx := context.Background()
	functionCall := devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL
	stopPattern := devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN
	call := func(id, arguments string) *devinproto.GetChatMessageResponse {
		return &devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
			Id: proto.String(id), Name: proto.String("web_search"), ArgumentsJson: proto.String(arguments)}}}
	}
	hops := [][]*devinproto.GetChatMessageResponse{
		{call("c0", `{"query":"first"}`), {StopReason: functionCall.Enum()}},
		{call("c1", `{"query":"second"}`), {StopReason: functionCall.Enum()}},
		{{DeltaText: proto.String("final answer")}, {StopReason: stopPattern.Enum()}},
	}
	newDecoder := func() *responseDecoder {
		return newResponseDecoder("model", nil, nil, map[string]bool{"web_search": true})
	}
	var resultSets [][]llm.ToolResultMessage
	hop := 0
	stream := &responseStream{
		frames:  pumpUpstream(ctx, &fakeDevinResponseReceiver{responses: hops[0]}),
		cancel:  func() {},
		decoder: newDecoder(),
		gate:    newRateGate(GateConfig{}, nil, ""),
		search: func(_ context.Context, query string, _, _ []string, _ uint32) (webSearchOutcome, error) {
			return webSearchOutcome{
				results: []llm.WebSearchResult{{Title: "t-" + query, URL: "https://example.com/" + query}},
				summary: "summary " + query,
			}, nil
		},
	}
	stream.extend = func(_ string, extra []llm.Message, seed []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
		var results []llm.ToolResultMessage
		for _, message := range extra {
			if result, ok := message.(llm.ToolResultMessage); ok {
				results = append(results, result)
			}
		}
		resultSets = append(resultSets, results)
		hop++
		decoder := newDecoder()
		decoder.start()
		decoder.partial.Content = append([]llm.Content(nil), seed...)
		return pumpUpstream(ctx, &fakeDevinResponseReceiver{responses: hops[hop]}), func() {}, decoder, nil
	}

	var events []llm.ResponseEvent
	var done *llm.AssistantMessage
	for {
		event, err := stream.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
		if event.Type == llm.ResponseEventDone {
			done = event.Message
		}
	}
	if done == nil {
		t.Fatal("stream ended without done event")
	}
	var results, callEnds int
	for _, event := range events {
		switch event.Type {
		case llm.ResponseEventServerToolResult:
			results++
		case llm.ResponseEventToolCallEnd:
			callEnds++
		}
	}
	if results != 2 || callEnds != 2 {
		t.Fatalf("server_tool_result=%d toolcall_end=%d, want 2 each (tail frames must reach the client on hop switch)", results, callEnds)
	}
	if len(resultSets) != 2 {
		t.Fatalf("continuations = %d, want 2", len(resultSets))
	}
	// 第二跳的续轮必须携带两个调用的结果——首跳结果已在 partial 中，
	// 不带会让 assistant 上的 c0 调用在 wire 上无配对结果。
	if len(resultSets[1]) != 2 || resultSets[1][0].ToolCallID != "c0" || resultSets[1][1].ToolCallID != "c1" {
		t.Fatalf("second continuation results = %#v, want c0+c1", resultSets[1])
	}
}

// TestResponseDecoderRejectsEOFWithoutStopReason 的测试动机是防止把上游截断伪装成
// 正常结束：Devin 的正常收尾必带 stopReason 帧，干净 EOF 却缺它说明流被截断。
func TestResponseDecoderRejectsEOFWithoutStopReason(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("partial")})
	event := decoder.finish(nil)[0]
	if event.Type != llm.ResponseEventError || event.Error == nil || event.Error.ErrorMessage != "devin stream ended without stop reason" {
		t.Fatalf("event = %#v, want truncation error", event)
	}
}

// TestResponseDecoderRejectsEmptyNormalEOF 的测试动机是避免把未产生任何内容的异常空流误报为成功。
func TestResponseDecoderRejectsEmptyNormalEOF(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	event := decoder.finish(nil)[0]
	if event.Type != llm.ResponseEventError || event.Error == nil || event.Error.ErrorMessage != "devin stream ended without generated content" {
		t.Fatalf("event = %#v, want empty-stream error", event)
	}
}

// TestResponseDecoderCompletesPartialWithThinking 验证 STOP_REASON_PARTIAL 不吞掉已生成的思考/文本。
func TestResponseDecoderCompletesPartialWithThinking(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
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
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, st)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/responses"})
	request := &devinproto.GetChatMessageRequest{
		Metadata: &devinproto.ExaCodeiumCommonPb_Metadata{ApiKey: proto.String("secret-token"), F: proto.String("fingerprint")},
		Prompt:   proto.String("hello"),
	}
	recordProtoJSON(recorder, "03-devin-request.json", request)
	recordProtoJSON(recorder, "04-devin-response.jsonl", &devinproto.GetChatMessageResponse{DeltaText: proto.String("world")})
	recorder.Complete(debuglog.Completion{})
	<-manager.Drained(recorder.Dir())

	requestLog, _, _, err := manager.ReadFile(recorder.Dir(), "03-devin-request.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestLog), "secret-token") || strings.Contains(string(requestLog), "fingerprint") {
		t.Fatalf("request log contains credentials: %s", requestLog)
	}
	responseLog, _, _, err := manager.ReadFile(recorder.Dir(), "04-devin-response.jsonl")
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
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "token", Model: "model"})
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
	first, _, err := buildRequest(base, Config{}, callBinding{Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	// 同一会话追加了新消息：前缀不变，trajectory/cascade 必须稳定。
	base.Messages = append(base.Messages,
		llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "answer"}}},
		llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "follow up"}}},
	)
	second, _, err := buildRequest(base, Config{}, callBinding{Token: "token", Model: "model"})
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
	first, _, err := buildRequest(makeRequest("original first message"), Config{}, callBinding{Token: "token", Model: "model"})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := buildRequest(makeRequest("[summary of compacted history]"), Config{}, callBinding{Token: "token", Model: "model"})
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
	binding := callBinding{Token: "token", Model: "model"}
	first, _, err := buildRequest(makeRequest("session-1", "task A"), Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := buildRequest(makeRequest("session-2", "task A"), Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if first.GetTrajectoryReference().GetTrajectoryId() == second.GetTrajectoryReference().GetTrajectoryId() {
		t.Fatalf("distinct session keys must not share a trajectory")
	}
	// 无 SessionKey 的客户端退回内容哈希：不同首条消息仍自然分散。
	third, _, err := buildRequest(makeRequest("", "task B"), Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	fourth, _, err := buildRequest(makeRequest("", "task C"), Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if third.GetTrajectoryReference().GetTrajectoryId() == fourth.GetTrajectoryReference().GetTrajectoryId() {
		t.Fatalf("keyless distinct conversations must not share a trajectory")
	}
}

// TestIsTransientConnectError 验证传输断裂（含 connect.Error 包装形态）
// 可重试、上游语义拒绝不重试：上游 unavailable 实测是确定性语义错误
// （router 直连、未开放端点），文案 "try again later" 是固定模板，
// 重试永远得到同样失败。
func TestIsTransientConnectError(t *testing.T) {
	retryable := map[string]error{
		"bare unexpected EOF": io.ErrUnexpectedEOF,
		// 线上实测形态：connect-go 把传输断裂包成 connect.Error——
		// RoundTrip 断 → unavailable 包 EOF；envelope 截断 →
		// invalid_argument 包 "protocol error: ..."；tcp 重置 →
		// unavailable 包 *net.OpError。
		"unavailable wrapping EOF": connect.NewError(connect.CodeUnavailable, io.ErrUnexpectedEOF),
		"incomplete envelope": connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("protocol error: incomplete envelope: %w", io.ErrUnexpectedEOF)),
		// 帧体截断：connect-go 不 %w 包 io.EOF，只能靠 protocol error: 措辞。
		"payload truncated": connect.NewError(connect.CodeInvalidArgument,
			errors.New("protocol error: promised 1024 bytes in enveloped message, got 100 bytes")),
		// 垃圾 flag 字节：connect-go 报 CodeInternal 而非 InvalidArgument。
		"invalid envelope flags": connect.NewError(connect.CodeInternal,
			errors.New("protocol error: invalid envelope flags 3")),
		"tcp reset": connect.NewError(connect.CodeUnavailable,
			&net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}),
		"mid-stream clean EOF": connect.NewError(connect.CodeUnknown, io.EOF),
		// http2 RST_STREAM：connect-go 把尾缀 code 映成语义 code
		// （wrapIfRSTError），映射出的 unavailable/internal/
		// resource_exhausted 与真实语义无关——全是传输断裂。
		"rst refused stream": connect.NewError(connect.CodeUnavailable,
			errors.New("stream error: stream ID 1; REFUSED_STREAM; received from peer")),
		"rst internal error": connect.NewError(connect.CodeInternal,
			errors.New("stream error: stream ID 3; INTERNAL_ERROR; received from peer")),
		"rst enhance your calm": connect.NewError(connect.CodeResourceExhausted,
			errors.New("bandwidth exhausted: stream error: stream ID 5; ENHANCE_YOUR_CALM; received from peer")),
		// GOAWAY 不走 wrapIfRSTError，建连期以 unavailable 外皮透出。
		"goaway": connect.NewError(connect.CodeUnavailable,
			errors.New(`http2: server sent GOAWAY and closed the connection; LastStreamID=9, ErrCode=NO_ERROR, debug=""`)),
		// h1 连接池（force_http1）：复用到对端已关闭的空闲连接时报
		// errServerClosedIdle——失败发生在字节写出之前，重试安全。
		"h1 idle conn closed": connect.NewError(connect.CodeUnavailable,
			errors.New("http: server closed idle connection")),
	}
	for name, err := range retryable {
		if !isTransientConnectError(err) {
			t.Fatalf("%s must be retryable: %v", name, err)
		}
	}
	semantic := map[string]error{
		"unavailable template": connect.NewError(connect.CodeUnavailable, errors.New("try later")),
		"permission denied":    connect.NewError(connect.CodePermissionDenied, errors.New("blocked")),
		"invalid argument":     connect.NewError(connect.CodeInvalidArgument, errors.New("bad request")),
		"rate limited":         connect.NewError(connect.CodeResourceExhausted, errors.New("rate limit")),
	}
	for name, err := range semantic {
		if isTransientConnectError(err) {
			t.Fatalf("%s must not be retried: %v", name, err)
		}
	}
}

// TestResponseStreamYieldsErrorBeforeStart 的测试动机是：上游在产出任何内容
// 前失败时，首个对外事件必须是 error 而不是 start——否则 HTTP 层在 start
// 时已提交 200，真实错误状态码无法回传，下游网关会把请求级错误误判为
// 渠道故障并冷却整个渠道。
func TestResponseStreamYieldsErrorBeforeStart(t *testing.T) {
	stream := &responseStream{
		gate:    newRateGate(GateConfig{}, nil, ""),
		frames:  pumpUpstream(context.Background(), &errorDevinResponseReceiver{err: connect.NewError(connect.CodePermissionDenied, errors.New("blocked by content policy"))}),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
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
	stream := &responseStream{frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil), gate: newRateGate(GateConfig{}, nil, "")}
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

// TestResponseStreamFailsOnUpstreamStall 的测试动机是：上游建立后无限静默
// （半开连接、上游挂死）时看门狗必须把请求按传输错误收尾，而不是干等
// 客户端超时或主动断开。
func TestResponseStreamFailsOnUpstreamStall(t *testing.T) {
	defer func(timeout time.Duration) { upstreamStallTimeout = timeout }(upstreamStallTimeout)
	upstreamStallTimeout = 20 * time.Millisecond
	receiver := &stalledDevinResponseReceiver{release: make(chan struct{})}
	defer close(receiver.release)
	stream := &responseStream{gate: newRateGate(GateConfig{}, nil, ""), frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil)}
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != llm.ResponseEventError || event.Error == nil || !strings.Contains(event.Error.ErrorMessage, "stalled") {
		t.Fatalf("event = %#v, want upstream stall error", event)
	}
	if _, err := stream.Recv(context.Background()); err != io.EOF {
		t.Fatalf("after stall Recv err = %v, want io.EOF", err)
	}
}

// TestResponseStreamTailGraceFinishesAfterStopReason 的测试动机是：上游
// 发完 stopReason 后语义内容已齐，若传输层不收尾（connect-go 排空 body
// 等 EOF 时上游挂住连接），短宽限后必须按正常 EOF 完成而不是干等
// 120s 静默看门狗再把完整响应拖成 stall 错误。
func TestResponseStreamTailGraceFinishesAfterStopReason(t *testing.T) {
	defer func(d time.Duration) { upstreamTailGrace = d }(upstreamTailGrace)
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	upstreamTailGrace = 20 * time.Millisecond
	upstreamStallTimeout = 10 * time.Second
	receiver := &hangAfterReceiver{release: make(chan struct{}), responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("done")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	defer close(receiver.release)
	stream := &responseStream{frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil), gate: newRateGate(GateConfig{}, nil, "")}
	for i := 0; i < 16; i++ {
		event, err := stream.Recv(context.Background())
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			t.Fatalf("tail grace emitted error event %#v, want clean finish", event.Error)
		}
	}
	t.Fatal("stream did not finish cleanly within tail grace")
}

// TestResponseStreamNoProgressWatchdog 的测试动机是：上游只发零事件帧
// （latency 活性帧/元数据帧）续命时，静默看门狗被帧到达重置、永不判死，
// 「无内容进度」期限必须兜底收尾——pre-content 尚可整体重发，
// post-content 按传输错误收场。
func TestResponseStreamNoProgressWatchdog(t *testing.T) {
	defer func(d time.Duration) { upstreamNoProgressTimeout = d }(upstreamNoProgressTimeout)
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	upstreamNoProgressTimeout = 30 * time.Millisecond
	upstreamStallTimeout = 10 * time.Second
	meta := func() *devinproto.GetChatMessageResponse {
		return &devinproto.GetChatMessageResponse{
			MessageId: proto.String("m"), RequestId: proto.String("r"),
			Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{ModelUid: proto.String("m")},
		}
	}
	receiver := &hangAfterReceiver{release: make(chan struct{}),
		responses: []*devinproto.GetChatMessageResponse{meta(), meta(), meta()}}
	defer close(receiver.release)
	stream := &responseStream{frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil), gate: newRateGate(GateConfig{}, nil, "")}
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != llm.ResponseEventError || event.Error == nil ||
		!strings.Contains(event.Error.ErrorMessage, "no progress") {
		t.Fatalf("event = %#v, want no-progress error", event)
	}
}

// TestResponseStreamNoProgressWatchdogAfterContent 的测试动机是钉住无进度
// 看门狗的跨 Recv 生命周期：首个内容事件下发后看门狗必须仍在岗——消费方
// 活跃等待期间零事件帧续命照样触发收尾（旧实现 Recv 返回即停表，首事件
// 后看门狗实质失效，退化的活性帧流会让请求无限挂起）。
func TestResponseStreamNoProgressWatchdogAfterContent(t *testing.T) {
	defer func(d time.Duration) { upstreamNoProgressTimeout = d }(upstreamNoProgressTimeout)
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	upstreamNoProgressTimeout = 30 * time.Millisecond
	upstreamStallTimeout = 10 * time.Second
	receiver := &heartbeatAfterReceiver{
		responses: []*devinproto.GetChatMessageResponse{{DeltaText: proto.String("hi")}},
		heartbeat: &devinproto.GetChatMessageResponse{
			Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{ModelUid: proto.String("m")},
		},
	}
	stream := &responseStream{frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil), gate: newRateGate(GateConfig{}, nil, "")}
	// 先排空首批内容事件（start/text_start/text_delta），此后只剩心跳帧。
	for drained := false; !drained; {
		event, err := stream.Recv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		drained = event.Type == llm.ResponseEventTextDelta
	}
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != llm.ResponseEventError || event.Error == nil ||
		!strings.Contains(event.Error.ErrorMessage, "no progress") {
		t.Fatalf("event = %#v, want no-progress error after first content", event)
	}
}

// TestResponseStreamResumesAfterStall 的测试动机是钉住 post-commit 截断
// 续传的对外形态：内容已下发后上游被静默看门狗杀死时不再直接报错，
// 而是回显已产出内容 + "continue" 重发续传——客户端先收到在飞块的
// end 接缝（干净块边界），续流内容开新块，全程只有一个 start。
func TestResponseStreamResumesAfterStall(t *testing.T) {
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	upstreamStallTimeout = 20 * time.Millisecond
	ctx := context.Background()
	receiver := &hangAfterReceiver{release: make(chan struct{}), responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("partial ")},
		{DeltaText: proto.String("text")},
	}}
	defer close(receiver.release)
	var cause string
	var extra []llm.Message
	var seed []llm.Content
	stream := &responseStream{
		frames:  pumpUpstream(ctx, receiver),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
	}
	stream.extend = func(resumeCause string, messages []llm.Message, seedContent []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
		cause, extra, seed = resumeCause, messages, seedContent
		decoder := newResponseDecoder("model", nil, nil, nil)
		decoder.start()
		decoder.partial.Content = append([]llm.Content(nil), seedContent...)
		return pumpUpstream(ctx, &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
			{DeltaText: proto.String("continued")},
			{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		}}), func() {}, decoder, nil
	}
	var types []llm.ResponseEventType
	var done *llm.AssistantMessage
	for {
		event, err := stream.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			t.Fatalf("unexpected error event: %#v", event.Error)
		}
		types = append(types, event.Type)
		if event.Type == llm.ResponseEventDone {
			done = event.Message
		}
	}
	want := []llm.ResponseEventType{
		llm.ResponseEventStart,
		llm.ResponseEventTextStart, llm.ResponseEventTextDelta, llm.ResponseEventTextDelta,
		llm.ResponseEventTextEnd, // 接缝：截断块干净收口
		llm.ResponseEventTextStart, llm.ResponseEventTextDelta, llm.ResponseEventTextEnd,
		llm.ResponseEventDone,
	}
	if !slices.Equal(types, want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	if !strings.HasPrefix(cause, "resume: ") || !strings.Contains(cause, "stalled") {
		t.Fatalf("resume cause = %q, want resume: ...stalled...", cause)
	}
	// 续传 wire 形态：assistant 回显携带物化后的半截文本，追加 "continue"。
	if len(extra) != 2 {
		t.Fatalf("extra = %#v, want [assistant echo, continue]", extra)
	}
	assistant, ok := extra[0].(llm.AssistantMessage)
	if !ok || len(assistant.Content) != 1 {
		t.Fatalf("extra[0] = %#v, want assistant echo with one block", extra[0])
	}
	if text, ok := assistant.Content[0].(llm.TextContent); !ok || text.Text != "partial text" {
		t.Fatalf("echoed block = %#v, want materialized partial text", assistant.Content[0])
	}
	user, ok := extra[1].(llm.UserMessage)
	if !ok || len(user.Content) != 1 || user.Content[0].(llm.TextContent).Text != "continue" {
		t.Fatalf("extra[1] = %#v, want continue user message", extra[1])
	}
	if len(seed) != 1 || seed[0].(llm.TextContent).Text != "partial text" {
		t.Fatalf("seed = %#v, want materialized content", seed)
	}
	if done == nil || done.StopReason != llm.StopReasonStop || len(done.Content) != 2 {
		t.Fatalf("done = %#v, want stop with two text blocks", done)
	}
}

// TestResponseStreamDoesNotResumeInFlightToolCall 的测试动机是钉住续传
// 的拒绝边界：在飞工具调用的 arguments 是截断 JSON，回传会被上游参数
// 校验拒掉、丢弃又让客户端已见调用与上游历史分叉——只能按错误透传。
func TestResponseStreamDoesNotResumeInFlightToolCall(t *testing.T) {
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	upstreamStallTimeout = 20 * time.Millisecond
	receiver := &hangAfterReceiver{release: make(chan struct{}), responses: []*devinproto.GetChatMessageResponse{
		{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
			Id: proto.String("c0"), Name: proto.String("shell"), ArgumentsJson: proto.String(`{"cmd":`),
		}}},
	}}
	defer close(receiver.release)
	resumed := false
	stream := &responseStream{
		frames:  pumpUpstream(context.Background(), receiver),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
		extend: func(_ string, _ []llm.Message, _ []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
			resumed = true
			return nil, nil, nil, errors.New("must not resume an in-flight tool call")
		},
	}
	for {
		event, err := stream.Recv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			if !strings.Contains(event.Error.ErrorMessage, "stalled") {
				t.Fatalf("error = %q, want stall error", event.Error.ErrorMessage)
			}
			break
		}
	}
	if resumed {
		t.Fatal("must not resume with an in-flight tool call")
	}
}

// TestResponseStreamResumeAttemptsCapped 的测试动机是钉住续传预算：
// 每次续传都把整段上下文重发再计费一遍，续上的流再被杀死时不能无限
// 滚上游配额——触顶后按 stall 错误透传。
func TestResponseStreamResumeAttemptsCapped(t *testing.T) {
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	defer func(n int) { maxStreamResumes = n }(maxStreamResumes)
	upstreamStallTimeout = 20 * time.Millisecond
	maxStreamResumes = 1
	release := make(chan struct{})
	defer close(release)
	first := &hangAfterReceiver{release: release, responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("x")},
	}}
	resumes := 0
	stream := &responseStream{
		frames:  pumpUpstream(context.Background(), first),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
	}
	stream.extend = func(_ string, _ []llm.Message, seedContent []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
		resumes++
		decoder := newResponseDecoder("model", nil, nil, nil)
		decoder.start()
		decoder.partial.Content = append([]llm.Content(nil), seedContent...)
		// 续上的流再次挂死：第二次 stall 应命中预算上限而非再续。
		return pumpUpstream(context.Background(), &hangAfterReceiver{release: release}), func() {}, decoder, nil
	}
	for {
		event, err := stream.Recv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			if !strings.Contains(event.Error.ErrorMessage, "stalled") {
				t.Fatalf("error = %q, want stall error", event.Error.ErrorMessage)
			}
			break
		}
	}
	if resumes != 1 {
		t.Fatalf("resumes = %d, want exactly 1 (capped)", resumes)
	}
}

// TestResponseStreamResumesSilentEOF 的测试动机是钉住静默截断的续传：
// 干净 EOF 无 stopReason 与传输断裂同级——上游帧序正常收尾必带
// stopReason，缺它就是应用层截断，按同一套回显续传而不是报错。
func TestResponseStreamResumesSilentEOF(t *testing.T) {
	ctx := context.Background()
	first := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("cut")},
	}}
	var cause string
	stream := &responseStream{
		frames:  pumpUpstream(ctx, first),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
	}
	stream.extend = func(resumeCause string, _ []llm.Message, seedContent []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
		cause = resumeCause
		decoder := newResponseDecoder("model", nil, nil, nil)
		decoder.start()
		decoder.partial.Content = append([]llm.Content(nil), seedContent...)
		return pumpUpstream(ctx, &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
			{DeltaText: proto.String(" rest")},
			{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		}}), func() {}, decoder, nil
	}
	var text strings.Builder
	var done *llm.AssistantMessage
	for {
		event, err := stream.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			t.Fatalf("unexpected error event: %#v", event.Error)
		}
		text.WriteString(event.Delta)
		if event.Type == llm.ResponseEventDone {
			done = event.Message
		}
	}
	if cause != "resume: devin stream ended without stop reason" {
		t.Fatalf("resume cause = %q, want silent-truncation cause", cause)
	}
	if text.String() != "cut rest" {
		t.Fatalf("text = %q, want cut rest", text.String())
	}
	if done == nil || done.StopReason != llm.StopReasonStop || len(done.Content) != 2 {
		t.Fatalf("done = %#v, want stop with two text blocks", done)
	}
}

// TestResponseStreamResumeStripsPartialThinkingSignature 的测试动机是
// 钉住在飞 thinking 块的回显形态：截断点的签名是残片，回传可能被
// 上游验签拒掉——回显剥成无签名 thinking（实测接受），种子内容保留
// 原样保住客户端已见事件与 partial 的一致性。
func TestResponseStreamResumeStripsPartialThinkingSignature(t *testing.T) {
	defer func(d time.Duration) { upstreamStallTimeout = d }(upstreamStallTimeout)
	upstreamStallTimeout = 20 * time.Millisecond
	ctx := context.Background()
	receiver := &hangAfterReceiver{release: make(chan struct{}), responses: []*devinproto.GetChatMessageResponse{
		{DeltaThinking: proto.String("thinking so far"), DeltaSignature: proto.String("sigfrag")},
	}}
	defer close(receiver.release)
	var extra []llm.Message
	var seed []llm.Content
	stream := &responseStream{
		frames:  pumpUpstream(ctx, receiver),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
	}
	stream.extend = func(_ string, messages []llm.Message, seedContent []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
		extra, seed = messages, seedContent
		decoder := newResponseDecoder("model", nil, nil, nil)
		decoder.start()
		decoder.partial.Content = append([]llm.Content(nil), seedContent...)
		return pumpUpstream(ctx, &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
			{DeltaText: proto.String("answer")},
			{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		}}), func() {}, decoder, nil
	}
	var sawThinkingEnd bool
	for {
		event, err := stream.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			t.Fatalf("unexpected error event: %#v", event.Error)
		}
		if event.Type == llm.ResponseEventThinkingEnd {
			sawThinkingEnd = true
		}
	}
	if !sawThinkingEnd {
		t.Fatal("seam must close the in-flight thinking block")
	}
	assistant, ok := extra[0].(llm.AssistantMessage)
	if !ok || len(assistant.Content) == 0 {
		t.Fatalf("extra[0] = %#v, want assistant echo", extra[0])
	}
	thinking, ok := assistant.Content[len(assistant.Content)-1].(llm.ThinkingContent)
	if !ok {
		t.Fatalf("echoed last block = %#v, want thinking", assistant.Content[len(assistant.Content)-1])
	}
	if thinking.ThinkingSignature != "" || thinking.SignatureType != "" {
		t.Fatalf("echoed thinking signature = %q/%q, want stripped", thinking.ThinkingSignature, thinking.SignatureType)
	}
	seeded, ok := seed[len(seed)-1].(llm.ThinkingContent)
	if !ok || seeded.ThinkingSignature != "sigfrag" {
		t.Fatalf("seeded thinking = %#v, want original signature kept", seed[len(seed)-1])
	}
}

// TestResponseStreamReleasesStartOnHoldTimeout 的测试动机是：上游建流后
// 长时间静默时，扣留的 start 必须先行下发——否则客户端在 ~30s 无数据
// 处弃连（中间网关只在首个协议事件后才向客户端放通字节），一条本来
// 能成功的长思考流被掐死。
func TestResponseStreamReleasesStartOnHoldTimeout(t *testing.T) {
	defer func(timeout time.Duration) { startHoldTimeout = timeout }(startHoldTimeout)
	startHoldTimeout = 20 * time.Millisecond
	receiver := &stalledDevinResponseReceiver{release: make(chan struct{})}
	defer close(receiver.release)
	stream := &responseStream{gate: newRateGate(GateConfig{}, nil, ""), frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil)}
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != llm.ResponseEventStart {
		t.Fatalf("event = %q, want start released on hold timeout", event.Type)
	}
	if !stream.startReleased {
		t.Fatal("startReleased = false after hold-timeout release")
	}
}

// TestResponseStreamStopsOnContextCancel 验证等待上游帧期间客户端 ctx
// 取消能立即结束流，而不是挂在阻塞的 Receive 上等看门狗超时。
func TestResponseStreamStopsOnContextCancel(t *testing.T) {
	receiver := &stalledDevinResponseReceiver{release: make(chan struct{})}
	defer close(receiver.release)
	stream := &responseStream{gate: newRateGate(GateConfig{}, nil, ""), frames: pumpUpstream(context.Background(), receiver), cancel: func() {}, decoder: newResponseDecoder("model", nil, nil, nil)}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	event, err := stream.Recv(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if event.Type != llm.ResponseEventError || event.Error == nil || !strings.Contains(event.Error.ErrorMessage, "canceled") {
		t.Fatalf("event = %#v, want cancellation error", event)
	}
}

// TestBuildRequestToolChoiceMapping 验证 tool_choice 映射到上游 oneof：
// required/none 走 option_name，named 走 tool_name，auto 缺省不发。
func TestBuildRequestToolChoiceMapping(t *testing.T) {
	binding := callBinding{Token: "token", Model: "model"}
	request := llm.RequestMessages{Messages: []llm.Message{
		llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}},
	}}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceRequired}
	converted, _, err := buildRequest(request, Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice().GetOptionName() != "required" {
		t.Fatalf("tool_choice = %#v, want option_name=required", converted.GetToolChoice())
	}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceNone}
	converted, _, err = buildRequest(request, Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice().GetOptionName() != "none" {
		t.Fatalf("tool_choice = %#v, want option_name=none", converted.GetToolChoice())
	}

	// 指名调用要求工具在 tools 表内：先声明 read_file 再指名。
	request.Tools = []llm.ToolDefinition{{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: "read_file"}
	converted, _, err = buildRequest(request, Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice().GetToolName() != "read_file" {
		t.Fatalf("tool_choice = %#v, want tool_name=read_file", converted.GetToolChoice())
	}

	request.ToolChoice = &llm.ToolChoice{Mode: llm.ToolChoiceAuto}
	converted, _, err = buildRequest(request, Config{}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetToolChoice() != nil {
		t.Fatalf("auto tool_choice should be omitted, got %#v", converted.GetToolChoice())
	}

	request.ToolChoice = nil
	request.DisableParallelToolCalls = true
	converted, _, err = buildRequest(request, Config{}, binding)
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
	decoder := newResponseDecoder("model", []string{"STOP"}, nil, nil)
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("hello STOP world")})
	var text string
	for _, event := range events {
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
	decoder := newResponseDecoder("model", []string{"XYZ"}, nil, nil)
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

// TestResponseDecoderStopSequenceRuneBoundary 验证 holdback 安全窗不劈开
// 多字节 rune：safe 是字节下界，落在 UTF-8 序列中间时下发半个 rune 会被
// 编码端替成 U+FFFD，残续字节再产一个——客户端永久丢字符。
func TestResponseDecoderStopSequenceRuneBoundary(t *testing.T) {
	decoder := newResponseDecoder("model", []string{"STOP"}, nil, nil)
	decoder.start()
	var emitted string
	for _, event := range decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("ab中文cd")}) {
		if event.Type == llm.ResponseEventTextDelta {
			emitted += event.Delta
		}
	}
	// "文" 的末字节在旧字节下界内：只能下发到最近的 rune 起点。
	if emitted != "ab中" || strings.ContainsRune(emitted, '\uFFFD') {
		t.Fatalf("emitted = %q, want %q without replacement char", emitted, "ab中")
	}
	// 文字块收尾时残续字节随尾部完整冲刷，不丢字符。
	for _, event := range decoder.decode(&devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
		Id: proto.String("c"), Name: proto.String("exec"), ArgumentsJson: proto.String(`{}`),
	}}}) {
		if event.Type == llm.ResponseEventTextDelta {
			emitted += event.Delta
		}
	}
	if emitted != "ab中文cd" {
		t.Fatalf("total emitted = %q, want %q", emitted, "ab中文cd")
	}
}

// TestResponseDecoderNoStopMatchFlushesTail 验证未命中时保留的尾部
// 在文字块关闭时随最后一个 delta 全部下发。
func TestResponseDecoderNoStopMatchFlushesTail(t *testing.T) {
	decoder := newResponseDecoder("model", []string{"STOP"}, nil, nil)
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

// TestResponseDecoderStoresSignatureTypeAndOutputID 验证 signature_type 与
// output_id 随帧落进中间模型：回放时缺 signature_type 实测触发上游
// invalid_argument，output_id 是 OpenAI 侧 message item 的真实 id。
func TestResponseDecoderStoresSignatureTypeAndOutputID(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaThinking:      proto.String("think"),
		DeltaSignature:     proto.String("sig-payload"),
		DeltaSignatureType: proto.String("anthropic"),
		OutputId:           proto.String("msg_123"),
	})
	if len(events) == 0 {
		t.Fatal("expected thinking events")
	}
	thinking, ok := decoder.partial.Content[0].(llm.ThinkingContent)
	if !ok {
		t.Fatalf("content[0] = %T, want ThinkingContent", decoder.partial.Content[0])
	}
	if thinking.SignatureType != "anthropic" || thinking.ThinkingSignature != "sig-payload" {
		t.Fatalf("thinking = %#v", thinking)
	}
	if decoder.partial.OutputID != "msg_123" {
		t.Fatalf("output id = %q, want msg_123", decoder.partial.OutputID)
	}
}

// TestResponseDecoderLateSignatureSynthesizesBlock 验证思考块缺席时签名帧
// 不被丢弃：openai 体制无 deltaThinking，签名是唯一思考产物，必须合成
// 空块（start+end 一对，签名经共享 Partial 传达）让 /v1/responses 下游
// 拿得到 reasoning item。
func TestResponseDecoderLateSignatureSynthesizesBlock(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	events := decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaSignature:     proto.String(`[{"id":"rs_9","type":"reasoning","encrypted_content":"gAAA"}]`),
		DeltaSignatureType: proto.String("openai"),
	})
	var sawStart, sawEnd bool
	for _, event := range events {
		if event.Type == llm.ResponseEventThinkingStart {
			sawStart = true
		}
		if event.Type == llm.ResponseEventThinkingEnd {
			sawEnd = true
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("events = %#v, want thinking_start + thinking_end", events)
	}
	thinking, ok := decoder.partial.Content[0].(llm.ThinkingContent)
	if !ok || thinking.SignatureType != "openai" || thinking.ThinkingSignature == "" {
		t.Fatalf("content[0] = %#v", decoder.partial.Content[0])
	}
}

// TestResponseDecoderCustomToolCall 验证 is_custom_tool_call + invalid_json_str
// 把非 JSON 参数原文（如补丁文本）透传为 Custom 调用，而不是吞成 {}。
func TestResponseDecoderCustomToolCall(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
			Id:               proto.String("call-1"),
			Name:             proto.String("apply_patch"),
			IsCustomToolCall: proto.Bool(true),
			InvalidJsonStr:   proto.String("*** Begin Patch\n+hello"),
		}},
	})
	decoder.decode(&devinproto.GetChatMessageResponse{
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL.Enum(),
	})
	events := decoder.finish(nil)
	var done llm.ResponseEvent
	for _, event := range events {
		if event.Type == llm.ResponseEventDone {
			done = event
		}
	}
	if done.Message == nil {
		t.Fatal("missing done message")
	}
	call, ok := done.Message.Content[0].(llm.ToolCall)
	if !ok {
		t.Fatalf("content[0] = %T, want ToolCall", done.Message.Content[0])
	}
	if !call.Custom || string(call.Arguments) != "*** Begin Patch\n+hello" {
		t.Fatalf("tool call = %#v", call)
	}
}

// TestBuildRequestReplaysSignatureMetadata 验证签名三件套（signature +
// signature_type + output_id）整体回填到 assistant prompt。
func TestBuildRequestReplaysSignatureMetadata(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}},
			llm.AssistantMessage{
				OutputID: "msg_42",
				Content: []llm.Content{
					llm.ThinkingContent{Thinking: "t", ThinkingSignature: "sig", SignatureType: "anthropic"},
					llm.TextContent{Text: "answer"},
				},
			},
		},
	}
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "t", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	prompt := converted.GetChatMessagePrompts()[1]
	if prompt.GetSignature() != "sig" || prompt.GetSignatureType() != "anthropic" || prompt.GetOutputId() != "msg_42" {
		t.Fatalf("assistant prompt = %#v", prompt)
	}
}

// TestBuildRequestCustomToolCallUsesInvalidJSONStr 验证 Custom 调用经
// invalid_json_str + is_custom_tool_call 回传，非 JSON 原文不进 arguments_json。
func TestBuildRequestCustomToolCallUsesInvalidJSONStr(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{
			llm.AssistantMessage{Content: []llm.Content{
				llm.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage("*** Begin Patch"), Custom: true},
			}},
		},
	}
	converted, _, err := buildRequest(request, Config{}, callBinding{Token: "t", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	call := converted.GetChatMessagePrompts()[0].GetToolCalls()[0]
	if !call.GetIsCustomToolCall() || call.GetInvalidJsonStr() != "*** Begin Patch" || call.ArgumentsJson != nil {
		t.Fatalf("custom tool call wire = %#v", call)
	}
}

// TestBuildRequestRejectsNamedToolChoiceOutsideTools 验证指名不存在工具的
// tool_choice 在本地报可读错误——上游对此只回模糊流内 invalid_argument。
func TestBuildRequestRejectsNamedToolChoiceOutsideTools(t *testing.T) {
	request := llm.RequestMessages{
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
		Tools: []llm.ToolDefinition{
			{Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		ToolChoice: &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: "missing_tool"},
	}
	_, _, err := buildRequest(request, Config{}, callBinding{Token: "t", Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "missing_tool") {
		t.Fatalf("err = %v, want named tool_choice rejection", err)
	}
}

// TestPairToolCallsWithResultsConsumesDuplicateID 验证重复 call-id 按位置
// 绑定：同 id 的第二个调用不再复用同一份结果（上游实测容忍重复 id）。
func TestPairToolCallsWithResultsConsumesDuplicateID(t *testing.T) {
	assistant := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
	tool := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
	callPrompt := func() *devinproto.ExaChatPb_ChatMessagePrompt {
		return &devinproto.ExaChatPb_ChatMessagePrompt{
			Source:    assistant.Enum(),
			ToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{Id: proto.String("dup"), Name: proto.String("x")}},
		}
	}
	resultPrompt := &devinproto.ExaChatPb_ChatMessagePrompt{
		Source: tool.Enum(), ToolCallId: proto.String("dup"), Prompt: proto.String("r"),
	}
	out, _ := pairToolCallsWithResults([]*devinproto.ExaChatPb_ChatMessagePrompt{callPrompt(), callPrompt(), resultPrompt})
	if len(out) != 3 {
		t.Fatalf("paired prompts = %d, want 3", len(out))
	}
	if out[0].GetSource() != assistant || out[1].GetSource() != tool || out[2].GetSource() != assistant {
		t.Fatalf("expected call,result,call ordering, got %#v", out)
	}
}

// TestPairToolCallsWithResultsKeepsDuplicateResults 验证同 id 的多份结果
// 按到达顺序配对且都不丢：旧实现 byID 单值存取，R1 被 R2 覆盖后按 id 判
// 已消费，先到的结果被静默丢弃。
func TestPairToolCallsWithResultsKeepsDuplicateResults(t *testing.T) {
	assistant := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
	tool := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
	callPrompt := &devinproto.ExaChatPb_ChatMessagePrompt{
		Source:    assistant.Enum(),
		ToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{Id: proto.String("dup"), Name: proto.String("x")}},
	}
	result1 := &devinproto.ExaChatPb_ChatMessagePrompt{Source: tool.Enum(), ToolCallId: proto.String("dup"), Prompt: proto.String("r1")}
	result2 := &devinproto.ExaChatPb_ChatMessagePrompt{Source: tool.Enum(), ToolCallId: proto.String("dup"), Prompt: proto.String("r2")}
	out, _ := pairToolCallsWithResults([]*devinproto.ExaChatPb_ChatMessagePrompt{callPrompt, result1, result2})
	if len(out) != 3 {
		t.Fatalf("paired prompts = %d, want 3 (no result may be dropped)", len(out))
	}
	if out[0] != callPrompt || out[1] != result1 || out[2] != result2 {
		t.Fatalf("expected call,result1,result2 order, got %#v", out)
	}
}

// TestPairToolCallsWithResultsPairsAcrossInterveningPrompts 钉 G2：结果与
// 其 call 之间隔着用户插话（wire 形如 USER→SYSTEM(toolCalls)→USER→TOOL）
// 时按 toolCallId 前移配对——旧实现只在 call 紧邻段内配对，滞留的 TOOL
// prompt 原样上行即 invalid_argument。
func TestPairToolCallsWithResultsPairsAcrossInterveningPrompts(t *testing.T) {
	assistant := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM
	user := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER
	tool := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
	call := func(id string) *devinproto.ExaChatPb_ChatMessagePrompt {
		return &devinproto.ExaChatPb_ChatMessagePrompt{
			Source:    assistant.Enum(),
			ToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{Id: proto.String(id), Name: proto.String("x")}},
		}
	}
	result := func(id, text string) *devinproto.ExaChatPb_ChatMessagePrompt {
		return &devinproto.ExaChatPb_ChatMessagePrompt{Source: tool.Enum(), ToolCallId: proto.String(id), Prompt: proto.String(text)}
	}
	interrupt := &devinproto.ExaChatPb_ChatMessagePrompt{Source: user.Enum(), Prompt: proto.String("hold on")}

	// 单 call 被一条 user 插话与结果隔开：结果前移紧跟 call，插话原序后移。
	t.Run("single", func(t *testing.T) {
		callA, resultA := call("a"), result("a", "rA")
		out, moved := pairToolCallsWithResults([]*devinproto.ExaChatPb_ChatMessagePrompt{callA, interrupt, resultA})
		want := []*devinproto.ExaChatPb_ChatMessagePrompt{callA, resultA, interrupt}
		if len(out) != len(want) {
			t.Fatalf("paired prompts = %d, want %d", len(out), len(want))
		}
		for index := range want {
			if out[index] != want[index] {
				t.Fatalf("out[%d] = %#v, want callA,resultA,interrupt order", index, out)
			}
		}
		if moved == 0 {
			t.Fatal("moved = 0, want reordered count > 0")
		}
	})

	// 两 call 夹一条插话、结果成组在尾：rA 前移归 callA，rB 原位本已紧邻 callB。
	t.Run("grouped", func(t *testing.T) {
		callA, callB := call("a"), call("b")
		resultA, resultB := result("a", "rA"), result("b", "rB")
		out, _ := pairToolCallsWithResults([]*devinproto.ExaChatPb_ChatMessagePrompt{callA, interrupt, callB, resultA, resultB})
		want := []*devinproto.ExaChatPb_ChatMessagePrompt{callA, resultA, interrupt, callB, resultB}
		if len(out) != len(want) {
			t.Fatalf("paired prompts = %d, want %d", len(out), len(want))
		}
		for index := range want {
			if out[index] != want[index] {
				t.Fatalf("out[%d] = %#v, want callA,resultA,interrupt,callB,resultB", index, out)
			}
		}
	})

	// 无 call 认领的孤儿 result 与插话一起原位保留，不丢消息。
	t.Run("orphan", func(t *testing.T) {
		callA, orphan := call("a"), result("ghost", "r?")
		out, _ := pairToolCallsWithResults([]*devinproto.ExaChatPb_ChatMessagePrompt{orphan, callA, interrupt})
		want := []*devinproto.ExaChatPb_ChatMessagePrompt{orphan, callA, interrupt}
		for index := range want {
			if out[index] != want[index] {
				t.Fatalf("out[%d] = %#v, want orphan,callA,interrupt", index, out)
			}
		}
	})
}

// TestResponseStreamReopensBeforeContent 验证首内容帧前的瞬时传输错误
// 触发一次整体重发：客户端不可见任何事件，重发无可见副作用。
func TestResponseStreamReopensBeforeContent(t *testing.T) {
	reopened := false
	second := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := &responseStream{
		frames:  pumpUpstream(context.Background(), &errorDevinResponseReceiver{err: io.ErrUnexpectedEOF}),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
		reopen: func(cause error, _ bool) (<-chan upstreamFrame, context.CancelFunc, error) {
			reopened = true
			return pumpUpstream(context.Background(), second), func() {}, nil
		},
		newDecoder: func() *responseDecoder { return newResponseDecoder("model", nil, nil, nil) },
	}
	event, err := stream.Recv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !reopened {
		t.Fatal("expected reopen before first content")
	}
	if event.Type != llm.ResponseEventStart {
		t.Fatalf("first event = %q, want start", event.Type)
	}
}

// TestResponseStreamDoesNotReopenAfterContent 验证内容已开始流动后失败
// 直接透传为 error 事件——整体重发会把已下发内容重复一遍。
func TestResponseStreamDoesNotReopenAfterContent(t *testing.T) {
	first := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
	}}
	reopened := false
	stream := &responseStream{
		frames:  pumpUpstream(context.Background(), first),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
		reopen: func(cause error, _ bool) (<-chan upstreamFrame, context.CancelFunc, error) {
			reopened = true
			return nil, nil, cause
		},
		newDecoder: func() *responseDecoder { return newResponseDecoder("model", nil, nil, nil) },
	}
	// 先消费 start/text 事件，之后 EOF 无 stopReason → 报截断错误而非重试。
	for {
		event, err := stream.Recv(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			break
		}
	}
	if reopened {
		t.Fatal("must not reopen after content flowed")
	}
}

// TestReloadToken 验证 unauthenticated 自愈：TokenSource 拿到非空且
// 不同的新凭据才更新；同 token 或空值视为自愈失败。
func TestReloadToken(t *testing.T) {
	adapter := &Adapter{token: "old"}
	adapter.config.Identity.TokenSource = func() string { return "old" }
	if adapter.reloadToken() {
		t.Fatal("same token must not count as reload")
	}
	adapter.config.Identity.TokenSource = func() string { return "" }
	if adapter.reloadToken() {
		t.Fatal("empty token must not count as reload")
	}
	adapter.config.Identity.TokenSource = func() string { return "new" }
	if !adapter.reloadToken() || adapter.currentToken() != "new" {
		t.Fatal("expected token reload to swap credentials")
	}
}

// TestResponseStreamContinuesEmptyEndTurn 验证上游正常 stop 但零内容时
// 以追加 "continue" 的形态整体重发一次（空 end_turn 是实测退化形态）。
func TestResponseStreamContinuesEmptyEndTurn(t *testing.T) {
	first := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	second := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	continued := false
	stream := &responseStream{
		frames:  pumpUpstream(context.Background(), first),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
		reopen: func(cause error, continueEmpty bool) (<-chan upstreamFrame, context.CancelFunc, error) {
			if !continueEmpty {
				return nil, nil, cause
			}
			continued = true
			return pumpUpstream(context.Background(), second), func() {}, nil
		},
		newDecoder: func() *responseDecoder { return newResponseDecoder("model", nil, nil, nil) },
	}
	var text strings.Builder
	for {
		event, err := stream.Recv(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventError {
			t.Fatalf("unexpected error event: %#v", event.Error)
		}
		if event.Type == llm.ResponseEventDone {
			if event.Message == nil || event.Message.StopReason != llm.StopReasonStop {
				t.Fatalf("done = %#v", event.Message)
			}
		}
		text.WriteString(event.Delta)
	}
	if !continued {
		t.Fatal("expected empty-end-turn continuation retry")
	}
	if text.String() != "hi" {
		t.Fatalf("text = %q, want hi", text.String())
	}
}

// TestResponseStreamEmptyEndTurnSurfacesWithoutRetry 验证无 reopen 能力时
// 空轮按原样放行——续传是 best-effort 优化，不能变成必依赖路径。
func TestResponseStreamEmptyEndTurnSurfacesWithoutRetry(t *testing.T) {
	first := &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := &responseStream{
		frames:  pumpUpstream(context.Background(), first),
		cancel:  func() {},
		decoder: newResponseDecoder("model", nil, nil, nil),
		gate:    newRateGate(GateConfig{}, nil, ""),
	}
	var done *llm.ResponseEvent
	for {
		event, err := stream.Recv(context.Background())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == llm.ResponseEventDone {
			done = &event
		}
	}
	if done == nil || done.Message == nil || done.Message.StopReason != llm.StopReasonStop {
		t.Fatalf("done = %#v", done)
	}
}

// TestDecoderToEncoderReplayContract 钉住一轮真实 agent 循环的跨请求契约：
// 上游产出 thinking+签名+tool_call → llm.AssistantMessage → 下一轮请求
// 回放时 signature/signature_type/output_id、call↔result 邻接配对全部保真。
// 这是 CPA 多轮会话事故（签名丢失/乱序配对）在我们链路上的对应防回归点。
func TestDecoderToEncoderReplayContract(t *testing.T) {
	// 第一拍：上游帧 → AssistantMessage（decoder 输出，不带 finish 错误）。
	decoder := newResponseDecoder("swe-2-max", nil, nil, nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaThinking: proto.String("need to read the file"),
	})
	decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaSignature:     proto.String("sealed.v1.abc"),
		DeltaSignatureType: proto.String("sealed"),
		OutputId:           proto.String("msg_1"),
	})
	decoder.decode(&devinproto.GetChatMessageResponse{
		DeltaText: proto.String("checking"),
		DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
			Id: proto.String("read_file_0"), Name: proto.String("read_file"),
			ArgumentsJson: proto.String(`{"path":"a.txt"}`),
		}},
	})
	decoder.decode(&devinproto.GetChatMessageResponse{
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL.Enum(),
	})
	var done *llm.AssistantMessage
	for _, event := range decoder.finish(nil) {
		if event.Type == llm.ResponseEventDone {
			done = event.Message
		}
	}
	if done == nil {
		t.Fatal("decoder produced no done message")
	}

	// 第二拍：回放消息序列进 wire——user, assistant(text+thinking+call), tool result, user。
	converted, _, err := buildRequest(llm.RequestMessages{
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "read a.txt"}}},
			*done,
			llm.ToolResultMessage{
				ToolCallID: "read_file_0",
				Content:    []llm.Content{llm.TextContent{Text: "file body"}},
			},
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "what did you find?"}}},
		},
	}, Config{}, callBinding{Token: "token", Model: "swe-2-max"})
	if err != nil {
		t.Fatal(err)
	}
	prompts := converted.GetChatMessagePrompts()
	if len(prompts) != 4 {
		t.Fatalf("prompt count = %d, want 4 (user, assistant, result, user)", len(prompts))
	}
	assistant := prompts[1]
	if assistant.GetPrompt() != "checking" {
		t.Fatalf("assistant text prompt = %q", assistant.GetPrompt())
	}
	if assistant.GetThinking() != "need to read the file" ||
		assistant.GetSignature() != "sealed.v1.abc" ||
		assistant.GetSignatureType() != "sealed" ||
		assistant.GetOutputId() != "msg_1" {
		t.Fatalf("assistant replay metadata = %#v", assistant)
	}
	if len(assistant.GetToolCalls()) != 1 || assistant.GetToolCalls()[0].GetId() != "read_file_0" ||
		assistant.GetToolCalls()[0].GetArgumentsJson() != `{"path":"a.txt"}` {
		t.Fatalf("assistant merged tool calls = %#v", assistant)
	}
	result := prompts[2]
	if result.GetSource() != devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL ||
		result.GetToolCallId() != "read_file_0" || result.GetPrompt() != "file body" {
		t.Fatalf("result prompt = %#v", result)
	}
	// 契约核心：携带 call 的 prompt 与 result 必须邻接（grouped 形态上游 invalid_argument）。
	if prompts[1].GetSource() != assistantSource || prompts[2].GetSource() != devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL {
		t.Fatalf("call/result not adjacent: %#v", prompts)
	}
}

// TestResolveModelAlias 钉住别名匹配优先级：精确 > 大小写折叠 > "*" 兜底，
// 全未命中原样返回。
func TestResolveModelAlias(t *testing.T) {
	aliases := map[string]string{
		"swe-2": "swe-2-max",
		"GLM":   "glm-5-2",
		"*":     "fallback-uid",
	}
	cases := []struct{ in, want string }{
		{"swe-2", "swe-2-max"},    // 精确
		{"glm", "glm-5-2"},        // 折叠命中 GLM
		{"SWE-2", "swe-2-max"},    // 折叠命中（精确未中）
		{"other", "fallback-uid"}, // "*" 兜底
	}
	for _, c := range cases {
		if got := ResolveModelAlias(aliases, c.in); got != c.want {
			t.Fatalf("ResolveModelAlias(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := ResolveModelAlias(map[string]string{"swe-2": "swe-2-max"}, "other"); got != "other" {
		t.Fatalf("no wildcard: ResolveModelAlias = %q, want passthrough", got)
	}
	if got := ResolveModelAlias(nil, "x"); got != "x" {
		t.Fatalf("nil aliases: ResolveModelAlias = %q, want passthrough", got)
	}
}

// stubModelConfigsClient 只实现 GetCliModelConfigs——接口其余方法经内嵌
// 类型兜底（本测试不会触达）。calls 记上游调用次数，release 闸门让全部
// 并发等待者挂上 fetch 后才放行拉取。
type stubModelConfigsClient struct {
	devinprotoconnect.ApiServerServiceClient
	calls   atomic.Int32
	release chan struct{}
}

func (stub *stubModelConfigsClient) GetCliModelConfigs(ctx context.Context, _ *connect.Request[devinproto.GetCliModelConfigsRequest]) (*connect.Response[devinproto.GetCliModelConfigsResponse], error) {
	stub.calls.Add(1)
	select {
	case <-stub.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return connect.NewResponse(&devinproto.GetCliModelConfigsResponse{}), nil
}

// TestListModelsSingleflight 验证并发 miss 收敛为单次上游拉取：N 个并发
// 调用共享同一个 fetch，拉取方提交缓存后等待者走复查路径拿到同一份结果。
func TestListModelsSingleflight(t *testing.T) {
	stub := &stubModelConfigsClient{release: make(chan struct{})}
	a := &Adapter{modelsCacheTTL: time.Minute}
	a.linkPtr.Store(&upstreamLink{api: stub})
	const waiters = 8
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	start := make(chan struct{})
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := a.ListModels(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	// 等拉取方真正进入上游调用，再留一小段让其余等待者全部挂上 fetch。
	for stub.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(stub.release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("ListModels() error = %v", err)
	}
	if got := stub.calls.Load(); got != 1 {
		t.Fatalf("GetCliModelConfigs calls = %d, want 1", got)
	}
}

// TestListModelsWaiterCancel 验证等待方吃自己的 ctx：拉取还挂着时
// 断连的等待者立即退出，不陪跑到拉取结束；拉取方自身不受影响。
func TestListModelsWaiterCancel(t *testing.T) {
	stub := &stubModelConfigsClient{release: make(chan struct{})}
	a := &Adapter{modelsCacheTTL: time.Minute}
	a.linkPtr.Store(&upstreamLink{api: stub})
	fetcherDone := make(chan error, 1)
	go func() {
		_, err := a.ListModels(context.Background())
		fetcherDone <- err
	}()
	for stub.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := a.ListModels(waiterCtx)
		waiterDone <- err
	}()
	// 等等待者挂上 fetch 再取消它的 ctx。
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter ListModels() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled waiter did not exit")
	}
	close(stub.release)
	if err := <-fetcherDone; err != nil {
		t.Fatalf("fetcher ListModels() error = %v", err)
	}
}

// TestServerToolMixedTurnKeepsDone 钉住混合回合的收尾语义：同一跳里客户端
// 调用与托管调用并存时，代理执行托管搜索、把结果事件插进尾帧，但 Done
// 必须保留——客户端调用还等客户端执行，续轮责任不在代理。
func TestServerToolMixedTurnKeepsDone(t *testing.T) {
	ctx := context.Background()
	functionCall := devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL
	call := func(id, name, arguments string) *devinproto.GetChatMessageResponse {
		return &devinproto.GetChatMessageResponse{DeltaToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
			Id: proto.String(id), Name: proto.String(name), ArgumentsJson: proto.String(arguments)}}}
	}
	decoder := newResponseDecoder("model", nil, nil, map[string]bool{"web_search": true})
	continued := false
	stream := &responseStream{
		frames: pumpUpstream(ctx, &fakeDevinResponseReceiver{responses: []*devinproto.GetChatMessageResponse{
			call("c0", "web_search", `{"query":"q"}`),
			call("c1", "get_weather", `{"city":"sh"}`),
			{StopReason: functionCall.Enum()},
		}}),
		cancel:  func() {},
		decoder: decoder,
		gate:    newRateGate(GateConfig{}, nil, ""),
		search: func(_ context.Context, query string, _, _ []string, _ uint32) (webSearchOutcome, error) {
			return webSearchOutcome{summary: "ans " + query}, nil
		},
		extend: func(_ string, _ []llm.Message, _ []llm.Content) (<-chan upstreamFrame, context.CancelFunc, *responseDecoder, error) {
			continued = true
			return nil, nil, nil, errors.New("must not continue a mixed turn")
		},
	}
	var events []llm.ResponseEvent
	for {
		event, err := stream.Recv(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if continued {
		t.Fatal("mixed turn triggered a continuation")
	}
	last := events[len(events)-1]
	if last.Type != llm.ResponseEventDone || last.Reason != llm.StopReasonToolUse {
		t.Fatalf("last event = %v, want Done(tool_use)", last.Type)
	}
	var serverResults, callEnds int
	for _, event := range events {
		switch event.Type {
		case llm.ResponseEventServerToolResult:
			serverResults++
			if event.ServerResult.ToolCallID != "c0" {
				t.Fatalf("server result for %q, want c0", event.ServerResult.ToolCallID)
			}
		case llm.ResponseEventToolCallEnd:
			callEnds++
		}
	}
	if serverResults != 1 || callEnds != 2 {
		t.Fatalf("serverResults=%d callEnds=%d, want 1 and 2", serverResults, callEnds)
	}
}

// TestResponseDecoderReadsCreditCostFrame 钉住帧顶层计费字段的解码：上游
// 在末帧携带 credit_cost/committed_* 快照（当前实测模型均未上报，只能靠
// 单测固定解码面），任一字段在场即建 Usage.Costs。
func TestResponseDecoderReadsCreditCostFrame(t *testing.T) {
	decoder := newResponseDecoder("model", nil, nil, nil)
	decoder.start()
	decoder.decode(&devinproto.GetChatMessageResponse{DeltaText: proto.String("ok")})
	decoder.decode(&devinproto.GetChatMessageResponse{
		StopReason:                    devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
		CreditCost:                    proto.Int32(42),
		CommittedCreditCost:           proto.Int32(1000),
		CommittedAcuCost:              proto.Float64(1.5),
		CommittedQuotaCostBasisPoints: proto.Int64(700),
		CommittedOverageCostCents:     proto.Int64(3),
	})
	events := decoder.finish(nil)
	done := events[len(events)-1]
	costs := done.Message.Usage.Costs
	if costs == nil {
		t.Fatal("done usage missing costs")
	}
	if costs.CreditCost != 42 || costs.CommittedCreditCost != 1000 ||
		costs.CommittedAcuCost != 1.5 || costs.CommittedQuotaCostBasisPoints != 700 ||
		costs.CommittedOverageCostCents != 3 {
		t.Fatalf("costs = %#v", costs)
	}
}
