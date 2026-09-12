// 本文件验证 OpenAI Responses 请求能保留中间模型需要的上下文语义。
package responses

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

// TestDecodeRequestBuildsConversationContext 验证系统提示词、多模态输入、工具和工具结果的保留。
func TestDecodeRequestBuildsConversationContext(t *testing.T) {
	data := []byte(`{
  "model": "gpt-test",
  "instructions": "你是一个谨慎的助手。",
  "input": [
    {"type":"message","role":"user","content":[
      {"type":"input_text","text":"读取这个文件"},
      {"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}
    ]},
    {"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{\"path\":\"a.txt\"}"},
    {"type":"function_call_output","call_id":"call-1","output":"内容"}
  ],
  "tools": [{"type":"function","name":"read_file","description":"读取文件","parameters":{"type":"object"}}]
}`)

	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	if request.Context.Model != "gpt-test" {
		t.Fatalf("Model = %q, want gpt-test", request.Context.Model)
	}
	if request.Context.SystemPrompt != "你是一个谨慎的助手。" {
		t.Fatalf("SystemPrompt = %q", request.Context.SystemPrompt)
	}
	if len(request.Context.Messages) != 3 {
		t.Fatalf("message count = %d, want 3", len(request.Context.Messages))
	}
	if _, ok := request.Context.Messages[0].(llm.UserMessage); !ok {
		t.Fatalf("message[0] type = %T, want llm.UserMessage", request.Context.Messages[0])
	}
	if _, ok := request.Context.Messages[1].(llm.AssistantMessage); !ok {
		t.Fatalf("message[1] type = %T, want llm.AssistantMessage", request.Context.Messages[1])
	}
	if _, ok := request.Context.Messages[2].(llm.ToolResultMessage); !ok {
		t.Fatalf("message[2] type = %T, want llm.ToolResultMessage", request.Context.Messages[2])
	}
	if len(request.Context.Tools) != 1 || request.Context.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %#v", request.Context.Tools)
	}
	if err := request.Context.Validate(); err != nil {
		t.Fatalf("context validation error = %v", err)
	}
}

// TestDecodeRequestAcceptsStringInput 验证紧凑字符串输入会转换为用户文字消息。
func TestDecodeRequestAcceptsStringInput(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"model":"gpt-test","input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(request.Context.Messages))
	}
	message := request.Context.Messages[0].(llm.UserMessage)
	content := message.Content[0].(llm.TextContent)
	if content.Text != "hello" {
		t.Fatalf("text = %q, want hello", content.Text)
	}
}

// TestDecodeRequestAcceptsImageURLObject 验证 IDE 常见的 image_url 对象形态可解码。
func TestDecodeRequestAcceptsImageURLObject(t *testing.T) {
	data := []byte(`{
  "model":"gpt-test",
  "input":[{"role":"user","content":[
    {"type":"input_text","text":"see"},
    {"type":"input_image","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
  ]}]
}`)
	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	user := request.Context.Messages[0].(llm.UserMessage)
	if len(user.Content) != 2 {
		t.Fatalf("content count = %d, want 2", len(user.Content))
	}
	img, ok := user.Content[1].(llm.ImageContent)
	if !ok {
		t.Fatalf("content[1] = %T, want ImageContent", user.Content[1])
	}
	if img.MIMEType != "image/png" || img.Data == "" || strings.HasPrefix(img.Data, "data:") {
		t.Fatalf("image = %#v", img)
	}
}

// TestDecodeRequestAcceptsChatCompletionsImagePart 验证 type=image_url 的 Chat 风格 part。
func TestDecodeRequestAcceptsChatCompletionsImagePart(t *testing.T) {
	data := []byte(`{
  "model":"gpt-test",
  "input":[{"role":"user","content":[
    {"type":"text","text":"see"},
    {"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}
  ]}]
}`)
	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	user := request.Context.Messages[0].(llm.UserMessage)
	if _, ok := user.Content[1].(llm.ImageContent); !ok {
		t.Fatalf("content[1] = %#v, want ImageContent", user.Content[1])
	}
}

// TestDecodeRequestRejectsInvalidToolArguments 验证适配器调用前会拒绝非对象工具参数。
func TestDecodeRequestRejectsInvalidToolArguments(t *testing.T) {
	data := []byte(`{"model":"gpt-test","input":[{"type":"function_call","call_id":"call-1","name":"tool","arguments":"[]"}]}`)
	if _, err := DecodeRequest(data); err == nil {
		t.Fatal("DecodeRequest() error = nil, want invalid arguments error")
	}
}

// TestDecodeRequestRetainsRawSchema 验证工具 schema 会以原始 JSON 保留。
func TestDecodeRequestRetainsRawSchema(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"model":"gpt-test","tools":[{"type":"function","name":"tool","parameters":{"type":"object","additionalProperties":false}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(request.Context.Tools[0].InputSchema, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema = %#v", schema)
	}
}

// TestDecodeRequestAcceptsMessageWithoutType 的测试动机是覆盖 OpenAI 官方示例和 SDK 发送的 role 加 content 简写。
func TestDecodeRequestAcceptsMessageWithoutType(t *testing.T) {
	data := []byte(`{
  "model": "glm-5.2",
  "input": [{
    "role": "user",
    "content": [{"type":"input_text","text":"hello"}]
  }],
  "stream": true
}`)
	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(request.Context.Messages))
	}
	message, ok := request.Context.Messages[0].(llm.UserMessage)
	if !ok {
		t.Fatalf("message type = %T, want llm.UserMessage", request.Context.Messages[0])
	}
	text, ok := message.Content[0].(llm.TextContent)
	if !ok || text.Text != "hello" {
		t.Fatalf("message content = %#v", message.Content)
	}
}

// TestDecodeRequestAttachesReasoningSummary 验证 reasoning item 的 summary
// 文本挂到紧随其后的 assistant 产出上（ThinkingContent 前置块）。
func TestDecodeRequestAttachesReasoningSummary(t *testing.T) {
	data := []byte(`{
  "model": "gpt-test",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
    {"type":"reasoning","summary":[{"type":"summary_text","text":"计划：先读文件再改"}]},
    {"type":"message","role":"assistant","content":[{"type":"output_text","text":"好的"}]},
    {"type":"reasoning","summary":[{"type":"summary_text","text":"需要调用 read_file"}],"encrypted_content":"sealed.v1.xyz"},
    {"type":"function_call","call_id":"call-1","name":"read_file","arguments":"{}"},
    {"type":"function_call_output","call_id":"call-1","output":"内容"}
  ]
}`)
	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 4 {
		t.Fatalf("message count = %d, want 4", len(request.Context.Messages))
	}
	assistant := request.Context.Messages[1].(llm.AssistantMessage)
	thinking, ok := assistant.Content[0].(llm.ThinkingContent)
	if !ok || thinking.Thinking != "计划：先读文件再改" {
		t.Fatalf("assistant content[0] = %#v, want ThinkingContent", assistant.Content[0])
	}
	call := request.Context.Messages[2].(llm.AssistantMessage)
	thinking, ok = call.Content[0].(llm.ThinkingContent)
	if !ok || thinking.Thinking != "需要调用 read_file" {
		t.Fatalf("function_call content[0] = %#v, want ThinkingContent", call.Content[0])
	}
	if thinking.ThinkingSignature != "sealed.v1.xyz" {
		t.Fatalf("thinking signature = %q, want sealed.v1.xyz replay", thinking.ThinkingSignature)
	}
	if _, ok := call.Content[1].(llm.ToolCall); !ok {
		t.Fatalf("function_call content[1] = %#v, want ToolCall", call.Content[1])
	}
}

// TestDecodeRequestDropsOrphanReasoning 验证 reasoning 后无 assistant 产出时
// 缓冲不会挂到后续 user 消息上。
func TestDecodeRequestDropsOrphanReasoning(t *testing.T) {
	data := []byte(`{
  "model": "gpt-test",
  "input": [
    {"type":"reasoning","summary":[{"type":"summary_text","text":"orphan"}]},
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
  ]
}`)
	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	user := request.Context.Messages[0].(llm.UserMessage)
	if len(user.Content) != 1 {
		t.Fatalf("user content = %#v, want single text block", user.Content)
	}
}

// TestDecodeRequestToleratesOrphanToolOutput 验证压缩丢失 function_call 后
// 孤立的 function_call_output 用兜底名放行，不整请求失败。
func TestDecodeRequestToleratesOrphanToolOutput(t *testing.T) {
	data := []byte(`{
  "model": "gpt-test",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
    {"type":"function_call_output","call_id":"call-gone","output":"残留结果"}
  ]
}`)
	request, err := DecodeRequest(data)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := request.Context.Messages[1].(llm.ToolResultMessage)
	if !ok {
		t.Fatalf("message[1] type = %T, want ToolResultMessage", request.Context.Messages[1])
	}
	if tool.ToolCallID != "call-gone" || tool.ToolName != "tool" {
		t.Fatalf("tool result = %#v", tool)
	}
}

// TestDecodeRequestIgnoresUnsupportedExtensions 的测试动机是确保上游新增字段和类型不会阻断可识别的对话内容。
func TestDecodeRequestIgnoresUnsupportedExtensions(t *testing.T) {
	request, err := DecodeRequest([]byte(`{
  "model":"model",
  "client_metadata":{"client":"codex"},
  "input":[
    {"type":"additional_tools","role":"developer","tools":[{"name":"unknown"}]},
    {"content":"untyped extension"},
    {"type":"message","role":"future_role","content":"ignored"},
    {"type":"message","role":"user","content":[
      {"type":"future_content","value":"ignored"},
      {"type":"input_text","text":"hello"}
    ]}
  ],
  "tools":[
    {"type":"web_search_preview"},
    {"type":"function","name":"known","parameters":{"type":"object"}}
  ]
}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(request.Context.Messages))
	}
	message, ok := request.Context.Messages[0].(llm.UserMessage)
	if !ok {
		t.Fatalf("message type = %T, want llm.UserMessage", request.Context.Messages[0])
	}
	if len(message.Content) != 1 || message.Content[0].(llm.TextContent).Text != "hello" {
		t.Fatalf("message content = %#v", message.Content)
	}
	if len(request.Context.Tools) != 1 || request.Context.Tools[0].Name != "known" {
		t.Fatalf("tools = %#v", request.Context.Tools)
	}
}
