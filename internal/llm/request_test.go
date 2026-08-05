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
						Text:          "我先读取配置。",
						TextSignature: "text-signature",
					},
					ToolCall{
						ID:               "call-1",
						Name:             "read_file",
						Arguments:        json.RawMessage(`{"path":"config.json"}`),
						ThoughtSignature: "provider-thought-signature",
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
				Details:        json.RawMessage(`{"path":"config.json"}`),
				AddedToolNames: []string{"write_file"},
				TimestampMS:    3,
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
