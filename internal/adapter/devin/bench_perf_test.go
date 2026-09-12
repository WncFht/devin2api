// 性能基准：请求编码路径（sanitize/工具 schema/buildRequest）与
// responseStream.Recv 的逐帧开销。
package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
	"google.golang.org/protobuf/proto"
)

// BenchmarkSanitizeText 测量规则预筛在「干净长文本」上的成本：
// ToLower 一次 + ~30 个 trigger 的 Contains 全扫。
func BenchmarkSanitizeText(b *testing.B) {
	// 模拟一个真实客户端 system prompt：干净、大、无 trigger。
	text := strings.Repeat("You are a helpful coding assistant. The user asked to refactor the module. ", 2000)
	b.Logf("text bytes = %d", len(text))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sanitizeUpstreamText(text, true)
	}
}

// BenchmarkSanitizeRequest 测量整条请求历史的 sanitize 成本。
func BenchmarkSanitizeRequest(b *testing.B) {
	var messages []llm.Message
	for i := 0; i < 200; i++ {
		messages = append(messages,
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: strings.Repeat("please refactor the function and explain. ", 20)}}},
			llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: strings.Repeat("here is the refactored code and notes. ", 30)}}},
		)
	}
	request := llm.RequestMessages{
		SystemPrompt: strings.Repeat("You are an AI coding assistant. Follow instructions. ", 50),
		Messages:     messages,
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = sanitizeRequest(request)
	}
}

// benchToolSchema 是带 $defs/$ref 与注解的典型工具 schema。
var benchToolSchema = json.RawMessage(`{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "$defs": {"path": {"type": "string", "description": "file path"}},
  "type": "object",
  "title": "Edit file", "description": "edit a file",
  "properties": {
    "path": {"$ref": "#/$defs/path"},
    "old": {"type": "string", "description": "old text"},
    "new": {"type": "string", "description": "new text"},
    "items": {"type": "array", "items": {"$ref": "#/$defs/path"}}
  },
  "required": ["path", "old", "new"]
}`)

// BenchmarkConvertTool 测量单工具 schema 的 strip+normalize 双程成本。
func BenchmarkConvertTool(b *testing.B) {
	tool := llm.ToolDefinition{Name: "edit_file", Description: "Edit a file", InputSchema: benchToolSchema}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := convertToolDefinition(tool); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildRequestLongHistory 测量长历史 + 多工具调用配对的 buildRequest 成本。
// 历史含 N 轮 assistant tool_call + tool result（配对的交错成本在 wire 层）。
func BenchmarkBuildRequestLongHistory(b *testing.B) {
	var messages []llm.Message
	for i := 0; i < 100; i++ {
		messages = append(messages,
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: fmt.Sprintf("task %d", i)}}},
			llm.AssistantMessage{Content: []llm.Content{
				llm.ThinkingContent{Thinking: strings.Repeat("thinking ", 50), ThinkingSignature: "sealed.v1.x", SignatureType: "sealed"},
				llm.ToolCall{ID: fmt.Sprintf("call_%d", i), Name: "exec", Arguments: json.RawMessage(`{"command":"ls"}`)},
			}},
			llm.ToolResultMessage{ToolCallID: fmt.Sprintf("call_%d", i), ToolName: "exec", Content: []llm.Content{llm.TextContent{Text: strings.Repeat("output ", 100)}}},
		)
	}
	request := llm.RequestMessages{
		SystemPrompt: strings.Repeat("sys ", 500),
		Messages:     messages,
		Tools: []llm.ToolDefinition{
			{Name: "exec", Description: "run a command", InputSchema: benchToolSchema},
			{Name: "read_file", Description: "read a file", InputSchema: benchToolSchema},
		},
	}
	cfg := Config{BaseURL: "https://example.com", Token: "t", Model: "m"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := buildRequest(request, cfg); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRecvDeltaStream 测量 responseStream.Recv 每帧开销（含逐帧 timer 分配）。
func BenchmarkRecvDeltaStream(b *testing.B) {
	const frames = 2000
	responses := make([]*devinproto.GetChatMessageResponse, 0, frames+2)
	for i := 0; i < frames; i++ {
		responses = append(responses, &devinproto.GetChatMessageResponse{
			DeltaText: proto.String("chunk of streaming text "),
		})
	}
	responses = append(responses,
		&devinproto.GetChatMessageResponse{
			StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
		},
		&devinproto.GetChatMessageResponse{
			Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{
				InputTokens:  proto.Uint64(10),
				OutputTokens: proto.Uint64(20),
			},
		},
	)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		receiver := &fakeDevinResponseReceiver{responses: responses}
		stream := &responseStream{
			frames:   pumpUpstream(context.Background(), receiver),
			cancel:   func() {},
			decoder:  newResponseDecoder("m", nil),
			recorder: nil,
		}
		for {
			if _, err := stream.Recv(context.Background()); err != nil {
				break
			}
		}
	}
}
