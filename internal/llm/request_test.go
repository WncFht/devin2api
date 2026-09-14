// 本文件验证请求上下文、消息内容和工具参数的中间层校验行为。
package llm

import (
	"encoding/json"
	"testing"
)

func TestRequestMessagesSupportsProviderIndependentHistory(t *testing.T) {
	reasoningTokens := int64(8)
	request := RequestMessages{
		SystemPrompt: "你是一个谨慎的编程助手。",
		Messages: []Message{
			UserMessage{
				Content: []Content{
					TextContent{Text: "读取配置并解释图片。"},
					ImageContent{Data: "iVBORw0KGgo=", MIMEType: "image/png"},
				},
				TimestampMS: 1,
			},
			AssistantMessage{
				Content: []Content{
					ThinkingContent{
						Thinking:          "需要先读取文件。",
						ThinkingSignature: "thinking-signature",
					},
					TextContent{
						Text: "我先读取配置。",
					},
					ToolCall{
						ID:        "call-1",
						Name:      "read_file",
						Arguments: json.RawMessage(`{"path":"config.json"}`),
					},
				},
				API:      "anthropic-messages",
				Provider: "anthropic",
				Model:    "claude-test",
				Usage: Usage{
					Input:       20,
					Output:      12,
					Reasoning:   &reasoningTokens,
					TotalTokens: 32,
				},
				StopReason:  StopReasonToolUse,
				TimestampMS: 2,
			},
			ToolResultMessage{
				ToolCallID: "call-1",
				ToolName:   "read_file",
				Content: []Content{
					TextContent{Text: `{"debug":true}`},
					ImageContent{Data: "iVBORw0KGgo=", MIMEType: "image/png"},
				},
				TimestampMS: 3,
			},
		},
		Tools: []ToolDefinition{
			{
				Name:        "read_file",
				Description: "读取文件内容",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"properties":{"path":{"type":"string"}},
					"required":["path"]
				}`),
			},
		},
	}

	if err := request.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestRequestMessagesRejectsInvalidToolArguments(t *testing.T) {
	request := RequestMessages{
		Messages: []Message{
			AssistantMessage{
				Content: []Content{
					ToolCall{ID: "call-1", Name: "read_file", Arguments: json.RawMessage(`{"path":`)},
				},
				StopReason: StopReasonToolUse,
			},
		},
	}

	if err := request.Validate(); err == nil {
		t.Fatal("Validate() error = nil, want invalid tool arguments error")
	}
}

// DemoteOrphanToolResults 的位置语义：result 先于同 id call（或未出现
// 的 call、缺失 id）都降级为 UserMessage 文本并留 Dropped 标记；
// 正常配对的结果不动。降级后消息整体过 Validate。
func TestDemoteOrphanToolResults(t *testing.T) {
	request := RequestMessages{
		Messages: []Message{
			// 孤儿：调用来得更晚（压缩/乱序）——按位置判孤儿。
			ToolResultMessage{ToolCallID: "call-late", ToolName: "read", Content: []Content{TextContent{Text: "early"}}},
			AssistantMessage{Content: []Content{
				ToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{}`)},
				ToolCall{ID: "call-late", Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
			// 正常配对：call-1 已在前置助手消息出现。
			ToolResultMessage{ToolCallID: "call-1", ToolName: "read", Content: []Content{TextContent{Text: "ok"}}},
			// 孤儿：调用不存在。
			ToolResultMessage{ToolCallID: "call-gone", ToolName: "", Content: []Content{TextContent{Text: "lost"}}},
			// 孤儿：id 缺失。
			ToolResultMessage{Content: []Content{TextContent{Text: "noid"}}},
		},
	}
	request.DemoteOrphanToolResults()

	if _, ok := request.Messages[0].(UserMessage); !ok {
		t.Fatalf("result-before-call not demoted: %T", request.Messages[0])
	}
	if _, ok := request.Messages[2].(ToolResultMessage); !ok {
		t.Fatalf("matched result was demoted: %T", request.Messages[2])
	}
	for _, index := range []int{3, 4} {
		demoted, ok := request.Messages[index].(UserMessage)
		if !ok {
			t.Fatalf("message %d not demoted: %T", index, request.Messages[index])
		}
		text, ok := demoted.Content[0].(TextContent)
		if !ok || text.Text != "[tool result, original call lost]\n" {
			t.Fatalf("message %d prefix = %#v", index, demoted.Content[0])
		}
	}
	want := []string{"unmatched_tool_call_id:call-late", "unmatched_tool_call_id:call-gone", "missing_tool_call_id"}
	got := request.Dropped
	if len(got) != len(want) {
		t.Fatalf("dropped = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dropped[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("post-demote Validate() error = %v", err)
	}
}
