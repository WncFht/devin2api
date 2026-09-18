// 本文件验证 OpenAI Chat Completions 请求能保留系统提示、多模态输入、工具和工具结果。
package chat

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

// TestDecodeRequestBuildsConversationContext 验证系统提示词、多模态输入、工具和工具结果的保留。
func TestDecodeRequestBuildsConversationContext(t *testing.T) {
	data := []byte(`{
  "model": "gpt-test",
  "messages": [
    {"role": "system", "content": "你是一个谨慎的助手。"},
    {"role": "user", "content": [
      {"type": "text", "text": "读取这个文件"},
      {"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0KGgo="}}
    ]},
    {"role": "assistant", "content": null, "tool_calls": [{"id": "call-1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}}]},
    {"role": "tool", "tool_call_id": "call-1", "content": "内容"}
  ],
  "tools": [{"type": "function", "function": {"name": "read_file", "description": "读取文件", "parameters": {"type": "object"}}}]
}`)

	request, err := DecodeRequest(data, true)
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
	user := request.Context.Messages[0].(llm.UserMessage)
	if _, ok := user.Content[1].(llm.ImageContent); !ok {
		t.Fatalf("user content[1] = %T, want ImageContent", user.Content[1])
	}
	if err := request.Context.Validate(); err != nil {
		t.Fatalf("context validation error = %v", err)
	}
}

// TestDecodeRequestAcceptsPlainString 验证简短字符串输入会转换为用户文字消息。
func TestDecodeRequestAcceptsPlainString(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hello"}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(request.Context.Messages))
	}
	message := request.Context.Messages[0].(llm.UserMessage)
	if message.Content[0].(llm.TextContent).Text != "hello" {
		t.Fatalf("message content = %#v", message.Content)
	}
}

// TestDecodeRequestAcceptsFunctionCallArguments 验证工具调用参数按 JSON 对象保留。
func TestDecodeRequestAcceptsFunctionCallArguments(t *testing.T) {
	request, err := DecodeRequest([]byte(`{"model":"gpt-test","messages":[{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]}]}`), true)
	if err != nil {
		t.Fatal(err)
	}
	assistant := request.Context.Messages[0].(llm.AssistantMessage)
	call := assistant.Content[0].(llm.ToolCall)
	if call.Name != "read_file" {
		t.Fatalf("tool call name = %q", call.Name)
	}
	if !json.Valid(call.Arguments) || !strings.Contains(string(call.Arguments), `"path"`) {
		t.Fatalf("tool call arguments = %q", call.Arguments)
	}
}

// TestDecodeRequestAcceptsLegacyFunctionDialect 验证 2023-06 前的
// function-calling 形态：functions 声明、assistant function_call
// 合成 id、role:"function" 结果按 name 对账、请求级 function_call 选择。
func TestDecodeRequestAcceptsLegacyFunctionDialect(t *testing.T) {
	data := []byte(`{
	  "model": "gpt-test",
	  "messages": [
	    {"role": "user", "content": "读文件"},
	    {"role": "assistant", "content": null, "function_call": {"name": "read_file", "arguments": "{\"path\":\"a.txt\"}"}},
	    {"role": "function", "name": "read_file", "content": "内容"}
	  ],
	  "functions": [{"name": "read_file", "description": "读取文件", "parameters": {"type": "object"}}],
	  "function_call": {"name": "read_file"}
	}`)

	request, err := DecodeRequest(data, true)
	if err != nil {
		t.Fatal(err)
	}
	assistant := request.Context.Messages[1].(llm.AssistantMessage)
	call, ok := assistant.Content[0].(llm.ToolCall)
	if !ok || call.Name != "read_file" {
		t.Fatalf("assistant content[0] = %#v", assistant.Content[0])
	}
	result := request.Context.Messages[2].(llm.ToolResultMessage)
	if result.ToolCallID != call.ID {
		t.Fatalf("tool result = %#v, want call id %q", result, call.ID)
	}
	if len(request.Context.Tools) != 1 || request.Context.Tools[0].Name != "read_file" {
		t.Fatalf("tools = %#v", request.Context.Tools)
	}
	if request.Context.ToolChoice == nil ||
		request.Context.ToolChoice.Mode != llm.ToolChoiceNamed ||
		request.Context.ToolChoice.ToolName != "read_file" {
		t.Fatalf("ToolChoice = %#v", request.Context.ToolChoice)
	}
	if err := request.Context.Validate(); err != nil {
		t.Fatalf("context validation error = %v", err)
	}
}

// TestDecodeRequestTrailingData 验证顶层 JSON 后的尾随内容报错而非静默忽略——
// 与 anthropic 面的 decoder.More() 检查对齐。
func TestDecodeRequestTrailingData(t *testing.T) {
	data := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":"hi"}]} extra`)
	if _, err := DecodeRequest(data, true); err == nil {
		t.Fatal("trailing data should error")
	}
}

// TestDecodeRequestMergesAdjacentAssistants 验证客户端发来的连续 assistant
// 消息合并为一个回合——与 responses 面同一实现（IR 层共享），防止 wire 上
// 出现假回合边界。
func TestDecodeRequestMergesAdjacentAssistants(t *testing.T) {
	data := []byte(`{"model":"gpt-test","messages":[
		{"role":"assistant","content":"第一段"},
		{"role":"assistant","content":"第二段","tool_calls":[{"id":"call-1","type":"function","function":{"name":"read","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call-1","content":"结果"},
		{"role":"assistant","content":"下一回合"}
	]}`)
	request, err := DecodeRequest(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 3 {
		t.Fatalf("message count = %d, want 3 (merged)", len(request.Context.Messages))
	}
	merged, ok := request.Context.Messages[0].(llm.AssistantMessage)
	if !ok || merged.StopReason != llm.StopReasonToolUse {
		t.Fatalf("message[0] = %#v, want merged assistant with toolUse", request.Context.Messages[0])
	}
	if _, ok := request.Context.Messages[1].(llm.ToolResultMessage); !ok {
		t.Fatalf("message[1] = %T, want ToolResultMessage", request.Context.Messages[1])
	}
	if _, ok := request.Context.Messages[2].(llm.AssistantMessage); !ok {
		t.Fatalf("message[2] = %T, want separate assistant turn", request.Context.Messages[2])
	}
}

// TestDecodeRequestEmptyContent 验证空 content 口径与 anthropic 面对齐：
// user 的 content:[] 落成空文本占位并记 empty_message:user；assistant
// 完全无产出（无 content 无 tool_calls）记 empty_message:assistant。
func TestDecodeRequestEmptyContent(t *testing.T) {
	data := []byte(`{"model":"gpt-test","messages":[
		{"role":"user","content":[]},
		{"role":"assistant"},
		{"role":"user","content":"hi"}
	]}`)
	request, err := DecodeRequest(data, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Context.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(request.Context.Messages))
	}
	user := request.Context.Messages[0].(llm.UserMessage)
	if text, ok := user.Content[0].(llm.TextContent); !ok || text.Text != "" {
		t.Fatalf("empty user placeholder = %#v", user.Content)
	}
	dropped := fmt.Sprint(request.Context.Dropped)
	for _, want := range []string{"empty_message:user", "empty_message:assistant"} {
		if !strings.Contains(dropped, want) {
			t.Fatalf("dropped = %v, want %s", request.Context.Dropped, want)
		}
	}
}

// TestDecodeRequestSkipsFieldScan 验证 collectDropped=false 时跳过顶层
// 未消费字段扫描（debuglog 关闭的生产路径），其余 Dropped 记账不受影响。
func TestDecodeRequestSkipsFieldScan(t *testing.T) {
	data := []byte(`{"model":"gpt-test","messages":[{"role":"user","content":[]}],"store":true,"reasoning_effort":"high"}`)
	withScan, err := DecodeRequest(data, true)
	if err != nil {
		t.Fatal(err)
	}
	withoutScan, err := DecodeRequest(data, false)
	if err != nil {
		t.Fatal(err)
	}
	droppedOn := fmt.Sprint(withScan.Context.Dropped)
	for _, want := range []string{"field:store", "field:reasoning_effort"} {
		if !strings.Contains(droppedOn, want) {
			t.Fatalf("collectDropped=true dropped = %v, want %s", withScan.Context.Dropped, want)
		}
	}
	for _, marker := range withoutScan.Context.Dropped {
		if strings.HasPrefix(marker, "field:") {
			t.Fatalf("collectDropped=false still collected field marker %q", marker)
		}
	}
	found := false
	for _, marker := range withoutScan.Context.Dropped {
		if marker == "empty_message:user" {
			found = true
		}
	}
	if !found {
		t.Fatalf("collectDropped=false dropped = %v, want empty_message:user", withoutScan.Context.Dropped)
	}
}

// TestDecodeRequestDemotesDroppedToolChoice 验证 openai 面与 anthropic 面
// 同口径：tool_choice 指名「声明过但投影丢弃」的工具（非 function 类型
// 条目）时降为 auto + 记 tool_choice:<name>，而不是让「指名不存在的
// 工具」校验 400 掉整单；从未声明的名字保持指名留给下游报错。
func TestDecodeRequestDemotesDroppedToolChoice(t *testing.T) {
	tools := `"tools":[
		{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}},
		{"type":"web_search","name":"search_web"}
	]`
	base := `"model":"gpt-test","messages":[{"role":"user","content":"hi"}]`

	// 声明过但被丢：指名降为 auto。
	dropped, err := DecodeRequest([]byte(`{`+base+`,`+tools+`,
		"tool_choice":{"type":"function","function":{"name":"search_web"}}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if dropped.Context.ToolChoice == nil || dropped.Context.ToolChoice.Mode != llm.ToolChoiceAuto {
		t.Fatalf("dropped-name ToolChoice = %#v, want auto", dropped.Context.ToolChoice)
	}
	droppedMarkers := fmt.Sprint(dropped.Context.Dropped)
	if !strings.Contains(droppedMarkers, "tool_choice:search_web") {
		t.Fatalf("dropped = %v, want tool_choice:search_web", dropped.Context.Dropped)
	}

	// 同名指名经旧版 function_call 兜底进来同样降级。
	legacy, err := DecodeRequest([]byte(`{`+base+`,`+tools+`,
		"function_call":{"name":"search_web"}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Context.ToolChoice == nil || legacy.Context.ToolChoice.Mode != llm.ToolChoiceAuto {
		t.Fatalf("legacy function_call ToolChoice = %#v, want auto", legacy.Context.ToolChoice)
	}

	// 从未声明的名字：保持指名，交给指名校验报错。
	ghost, err := DecodeRequest([]byte(`{`+base+`,`+tools+`,
		"tool_choice":{"type":"function","function":{"name":"ghost_tool"}}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if ghost.Context.ToolChoice == nil ||
		ghost.Context.ToolChoice.Mode != llm.ToolChoiceNamed ||
		ghost.Context.ToolChoice.ToolName != "ghost_tool" {
		t.Fatalf("never-declared ToolChoice = %#v, want named ghost_tool", ghost.Context.ToolChoice)
	}

	// 幸存工具：指名原样通过。
	survived, err := DecodeRequest([]byte(`{`+base+`,`+tools+`,
		"tool_choice":{"type":"function","function":{"name":"read_file"}}}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if survived.Context.ToolChoice == nil ||
		survived.Context.ToolChoice.Mode != llm.ToolChoiceNamed ||
		survived.Context.ToolChoice.ToolName != "read_file" {
		t.Fatalf("surviving ToolChoice = %#v, want named read_file", survived.Context.ToolChoice)
	}
}
