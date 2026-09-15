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

// DemoteOrphanToolResults 的位置语义：上游按位置按序消化 call→result、
// 不校验 id——result 前面还有未消化 call 时保留为 TOOL（哪怕 id 对不上），
// 先于一切 call 出现或 call 已被消化完的才降级为 UserMessage 文本并留
// Dropped 标记；缺失 id 的 result wire 上无法携带配对键，同样降级。
// 降级后消息整体过 Validate。
func TestDemoteOrphanToolResults(t *testing.T) {
	request := RequestMessages{
		Messages: []Message{
			// 孤儿：任何 call 之前出现的结果。
			ToolResultMessage{ToolCallID: "call-early", Content: []Content{TextContent{Text: "early"}}},
			AssistantMessage{Content: []Content{
				ToolCall{ID: "call-1", Name: "read", Arguments: json.RawMessage(`{}`)},
				ToolCall{ID: "call-2", Name: "read", Arguments: json.RawMessage(`{}`)},
			}},
			// 正常配对：消化 call-1。
			ToolResultMessage{ToolCallID: "call-1", Content: []Content{TextContent{Text: "ok"}}},
			// id 对不上但还有未消化的 call-2：上游按位置接受，保留 TOOL 原样。
			ToolResultMessage{ToolCallID: "call-mismatch", Content: []Content{TextContent{Text: "positional"}}},
			// 孤儿：pending 已消化完。
			ToolResultMessage{ToolCallID: "call-gone", Content: []Content{TextContent{Text: "lost"}}},
			// 孤儿：id 缺失。
			ToolResultMessage{Content: []Content{TextContent{Text: "noid"}}},
		},
	}
	request.DemoteOrphanToolResults()

	if _, ok := request.Messages[0].(UserMessage); !ok {
		t.Fatalf("result-before-any-call not demoted: %T", request.Messages[0])
	}
	for _, index := range []int{2, 3} {
		if _, ok := request.Messages[index].(ToolResultMessage); !ok {
			t.Fatalf("consumable result was demoted: %T", request.Messages[index])
		}
	}
	for _, index := range []int{0, 4, 5} {
		demoted, ok := request.Messages[index].(UserMessage)
		if !ok {
			t.Fatalf("message %d not demoted: %T", index, request.Messages[index])
		}
		text, ok := demoted.Content[0].(TextContent)
		if !ok || text.Text != "[tool result, original call lost]\n" {
			t.Fatalf("message %d prefix = %#v", index, demoted.Content[0])
		}
	}
	want := []string{"unmatched_tool_call_id:call-early", "unmatched_tool_call_id:call-gone", "missing_tool_call_id"}
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

// MergeAdjacentAssistantTurns 验证相邻 assistant 消息合并为一条回合消息：
// 文本块之间补换行、OutputID 取最后非空、含 ToolCall 时 StopReason 归
// tool_use；被 user/tool_result 分隔的 assistant 不合并。
func TestMergeAdjacentAssistantTurns(t *testing.T) {
	request := RequestMessages{
		Messages: []Message{
			UserMessage{Content: []Content{TextContent{Text: "问"}}},
			AssistantMessage{
				Content:  []Content{TextContent{Text: "先读"}},
				OutputID: "msg_a",
			},
			AssistantMessage{Content: []Content{
				TextContent{Text: "再改"},
				ToolCall{ID: "c1", Name: "edit", Arguments: json.RawMessage(`{}`)},
			}},
			ToolResultMessage{ToolCallID: "c1", Content: []Content{TextContent{Text: "done"}}},
			AssistantMessage{
				Content:  []Content{TextContent{Text: "收尾"}},
				OutputID: "msg_c",
			},
		},
	}
	request.MergeAdjacentAssistantTurns()

	if len(request.Messages) != 4 {
		t.Fatalf("messages = %d, want 4", len(request.Messages))
	}
	merged, ok := request.Messages[1].(AssistantMessage)
	if !ok {
		t.Fatalf("message[1] = %T, want AssistantMessage", request.Messages[1])
	}
	if len(merged.Content) != 4 {
		t.Fatalf("merged content = %#v, want text+\\n+text+call", merged.Content)
	}
	if sep, ok := merged.Content[1].(TextContent); !ok || sep.Text != "\n" {
		t.Fatalf("content[1] = %#v, want newline separator", merged.Content[1])
	}
	if merged.OutputID != "msg_a" || merged.StopReason != StopReasonToolUse {
		t.Fatalf("merged = %#v, want OutputID msg_a + toolUse", merged)
	}
	last, ok := request.Messages[3].(AssistantMessage)
	if !ok || len(last.Content) != 1 {
		t.Fatalf("message[3] = %#v, want separate assistant turn", request.Messages[3])
	}
}
