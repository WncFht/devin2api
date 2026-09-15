// 本文件定义 Anthropic Messages API 请求 JSON 到中间 LLM 模型的转换。
package messages

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
)

// Request 是 Anthropic Messages 请求中本适配器支持的字段集合。
type Request struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	System   json.RawMessage `json:"system,omitempty"`
	// MaxTokens 用指针区分「未提供」与「显式 <=0」：后者是被丢弃的
	// 客户端输入，需要进 Dropped 可观测。
	MaxTokens     *int            `json:"max_tokens"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    json.RawMessage `json:"tool_choice,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

// Message 是 Anthropic 消息条目。
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Tool 是 Anthropic 工具定义。
type Tool struct {
	// Type 缺省/为 "custom" 时是客户端 function 工具；bash_*/text_editor_*
	// 等客户端执行类型同样转发（无 input_schema 时按 {"type":"object"} 占位）；
	// web_search_*/web_fetch_*/code_execution_* 等服务端托管类型不转发。
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicRequestFields 是 DecodeRequest 已消费的顶层字段；其余字段
// （thinking/service_tier/context_management/mcp_servers 等）上游没有
// 对应物，记入 Dropped 透出而不是静默吞掉。
var anthropicRequestFields = map[string]bool{
	"model": true, "messages": true, "system": true, "max_tokens": true,
	"tools": true, "tool_choice": true, "stream": true, "temperature": true,
	"top_p": true, "top_k": true, "stop_sequences": true, "metadata": true,
}

// AdaptedRequest 是 Anthropic 请求转换后的中间请求和生成选项。
type AdaptedRequest struct {
	Context llm.RequestMessages
	Options RequestOptions
}

// RequestOptions 保存不属于对话历史的生成控制参数。
type RequestOptions struct {
	Stream bool
}

// DecodeRequest 将 Anthropic Messages JSON 请求转换为中间请求。
func DecodeRequest(data []byte) (AdaptedRequest, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&request); err != nil {
		return AdaptedRequest{}, fmt.Errorf("decode anthropic request: %w", err)
	}
	if decoder.More() {
		// 顶层 JSON 后还有内容说明 body 不是单个请求对象——多半
		// 是客户端 bug 或代理误拼接，静默忽略会掩盖截断/串包。
		return AdaptedRequest{}, errors.New("anthropic request has trailing data after JSON body")
	}
	if request.Model == "" {
		return AdaptedRequest{}, errors.New("anthropic request model is required")
	}
	if len(request.Messages) == 0 {
		return AdaptedRequest{}, errors.New("anthropic request messages are required")
	}

	context := llm.RequestMessages{Model: request.Model}
	context.Dropped = append(context.Dropped, common.UnconsumedFields(data, anthropicRequestFields)...)
	if request.MaxTokens != nil {
		if *request.MaxTokens > 0 {
			context.MaxTokens = request.MaxTokens
		} else {
			context.Dropped = append(context.Dropped, "field:max_tokens")
		}
	}
	context.Temperature = request.Temperature
	context.TopP = request.TopP
	if request.TopK != nil {
		if *request.TopK > 0 {
			context.TopK = request.TopK
		} else {
			context.Dropped = append(context.Dropped, "field:top_k")
		}
	}
	context.StopSequences = request.StopSequences
	toolChoice, disableParallel, err := common.ParseAnthropicToolChoice(request.ToolChoice)
	if err != nil {
		return AdaptedRequest{}, err
	}
	context.ToolChoice = toolChoice
	context.DisableParallelToolCalls = disableParallel
	if len(bytes.TrimSpace(request.Metadata)) > 0 {
		var metadata struct {
			UserID string `json:"user_id"`
		}
		if json.Unmarshal(request.Metadata, &metadata) == nil {
			context.SessionKey = metadata.UserID
		}
	}
	if len(bytes.TrimSpace(request.System)) > 0 && !bytes.Equal(bytes.TrimSpace(request.System), []byte("null")) {
		if err := appendSystem(&context, request.System); err != nil {
			return AdaptedRequest{}, err
		}
	}
	if err := appendMessages(&context, request.Messages); err != nil {
		return AdaptedRequest{}, err
	}
	for _, tool := range request.Tools {
		if !clientExecutedToolType(tool.Type) {
			// server tool（web_search_*/web_fetch_*/code_execution_* 等）
			// 由供应商托管执行，上游 Devin 无对应物，转发只会制造废工具。
			context.Dropped = append(context.Dropped, "tool:"+tool.Type)
			continue
		}
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		context.Tools = append(context.Tools, llm.ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	// 孤儿 tool result 在 IR 校验前统一降级为 USER 文本——校验要求
	// ToolCallID/ToolName 非空，而孤儿字段本来就是缺的。
	context.DemoteOrphanToolResults()
	if err := context.Validate(); err != nil {
		return AdaptedRequest{}, fmt.Errorf("validate adapted request: %w", err)
	}

	return AdaptedRequest{
		Context: context,
		Options: RequestOptions{
			Stream: request.Stream,
		},
	}, nil
}

// clientToolTypePrefixes 是 Anthropic 客户端执行工具的 type 形态：
// bash/text_editor/computer/memory 由调用方环境执行（Claude Code 的本地
// 工具就是这种），客户端不带 input_schema——按 {"type":"object"} 透传让
// 模型照常发起调用，参数由客户端按类型版本的既定 schema 解释。
var clientToolTypePrefixes = []string{
	"bash_", "text_editor_", "computer_", "memory_", "str_replace_based_edit_tool",
}

// clientExecutedToolType 判断 tool.type 是否客户端可执行：空/custom 是
// 普通 function 工具；已知客户端类型前缀放行；其余视为服务端托管工具。
func clientExecutedToolType(toolType string) bool {
	if toolType == "" || toolType == "custom" {
		return true
	}
	for _, prefix := range clientToolTypePrefixes {
		if strings.HasPrefix(toolType, prefix) {
			return true
		}
	}
	return false
}

// appendSystem 把 system 字段（字符串或块数组）并入 SystemPrompt。
func appendSystem(context *llm.RequestMessages, raw json.RawMessage) error {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		context.SystemPrompt = text
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return fmt.Errorf("decode anthropic system: %w", err)
	}
	for _, part := range parts {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(part, &block); err != nil {
			return err
		}
		if block.Type != "text" {
			context.Dropped = append(context.Dropped, "system_block:"+block.Type)
			continue
		}
		if context.SystemPrompt != "" && block.Text != "" {
			context.SystemPrompt += "\n"
		}
		context.SystemPrompt += block.Text
	}
	return nil
}

// appendMessages 顺序解码消息流；toolNames 随 assistant tool_use 增量登记
// id→name，后续 tool_result 直查，替代逐条 findToolNameByToolUseID 回扫。
func appendMessages(context *llm.RequestMessages, messages []Message) error {
	toolNames := make(map[string]string)
	for index, message := range messages {
		if err := appendMessage(context, message, toolNames); err != nil {
			return fmt.Errorf("message[%d]: %w", index, err)
		}
	}
	return nil
}

// appendMessage 按 role 把单条消息解码进会话。
func appendMessage(context *llm.RequestMessages, message Message, toolNames map[string]string) error {
	switch message.Role {
	case "user", "system":
		// Claude Code 在消息流中间插入 role:system 的途中注入（agent 列表、
		// task reminder、system notification）。内容位置敏感——解码为
		// UserMessage 保持时序，不能折叠进系统提示词。
		messages, err := decodeAnthropicUserMessages(context, message.Content, toolNames)
		if err != nil {
			return err
		}
		if len(messages) == 0 && len(bytes.TrimSpace(message.Content)) > 0 {
			// content:[] 的消息不该凭空消失：与 content:null 同策落成
			// 空文本占位，保住轮次结构，同时记账可见。
			context.Dropped = append(context.Dropped, "empty_message:"+message.Role)
			messages = []llm.Message{llm.UserMessage{
				Content:     []llm.Content{llm.TextContent{Text: ""}},
				TimestampMS: time.Now().UnixMilli(),
			}}
		}
		context.Messages = append(context.Messages, messages...)
	case "assistant":
		content, err := decodeAssistantContent(context, message.Content, toolNames)
		if err != nil {
			return err
		}
		if len(content) == 0 && len(bytes.TrimSpace(message.Content)) > 0 {
			context.Dropped = append(context.Dropped, "empty_message:assistant")
		}
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
	default:
		context.Dropped = append(context.Dropped, "role:"+message.Role)
	}
	return nil
}

// decodeAnthropicUserMessages 把 Anthropic user 消息 content 拆分为一个或多个中间消息。
// tool_result 内容块会生成独立的 llm.ToolResultMessage。
func decodeAnthropicUserMessages(context *llm.RequestMessages, raw json.RawMessage, toolNames map[string]string) ([]llm.Message, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []llm.Message{llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: ""}},
			TimestampMS: time.Now().UnixMilli(),
		}}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Message{llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: text}},
			TimestampMS: time.Now().UnixMilli(),
		}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode user content: %w", err)
	}

	var result []llm.Message
	var currentUserContent []llm.Content
	flushUser := func() {
		if len(currentUserContent) == 0 {
			return
		}
		result = append(result, llm.UserMessage{
			Content:     currentUserContent,
			TimestampMS: time.Now().UnixMilli(),
		})
		currentUserContent = nil
	}

	for index, part := range parts {
		var header struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		switch header.Type {
		case "text":
			currentUserContent = append(currentUserContent, llm.TextContent{Text: header.Text})
		case "image":
			image, err := common.DecodeImagePart(part)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			currentUserContent = append(currentUserContent, image)
		case "document", "file":
			// 文档块上游没有对应通道，内容必然丢；静默丢弃会让模型在
			// 缺上下文下回答而无人察觉，落占位文本至少让缺失可见。
			context.Dropped = append(context.Dropped, "user_block:"+header.Type)
			currentUserContent = append(currentUserContent, llm.TextContent{
				Text: "[content omitted: " + header.Type + " block not supported]",
			})
		case "tool_result":
			// tool_use_id 缺失或对不上前置调用的结果先按原样进 IR；
			// 解码尾的 DemoteOrphanToolResults 统一降级为 USER 文本。
			flushUser()
			tool, err := decodeToolResult(context, header.ToolUseID, header.Content, header.IsError, toolNames)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			result = append(result, tool)
		default:
			context.Dropped = append(context.Dropped, "user_block:"+header.Type)
		}
	}
	flushUser()
	return result, nil
}

// decodeAssistantContent 解码 assistant 消息的 text/thinking/tool_use 块。
func decodeAssistantContent(context *llm.RequestMessages, raw json.RawMessage, toolNames map[string]string) ([]llm.Content, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []llm.Content{llm.TextContent{Text: ""}}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Content{llm.TextContent{Text: text}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode assistant content: %w", err)
	}
	content := make([]llm.Content, 0, len(parts))
	for index, part := range parts {
		var header struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Data      string          `json:"data"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		switch header.Type {
		case "text":
			content = append(content, llm.TextContent{Text: header.Text})
		case "thinking":
			content = append(content, llm.ThinkingContent{
				Thinking:          header.Thinking,
				ThinkingSignature: header.Signature,
				SignatureType:     guessSignatureType(header.Signature),
			})
		case "redacted_thinking":
			// redacted 块的 data 是密封思考体；在 Devin wire 上对应 signature+redacted 标记。
			content = append(content, llm.ThinkingContent{
				ThinkingSignature: header.Data,
				SignatureType:     guessSignatureType(header.Data),
				Redacted:          true,
			})
		case "tool_use":
			args, custom := common.NormalizeToolArguments(header.Input)
			toolNames[header.ID] = header.Name
			content = append(content, llm.ToolCall{ID: header.ID, Name: header.Name, Arguments: args, Custom: custom})
		default:
			context.Dropped = append(context.Dropped, "assistant_block:"+header.Type)
		}
	}
	return content, nil
}

// decodeToolResult 把 tool_result 块解码为 ToolResultMessage；tool_use_id
// 缺失或对不上已知调用时 ToolCallID/ToolName 留空，由解码尾的
// DemoteOrphanToolResults 降级。
func decodeToolResult(context *llm.RequestMessages, toolUseID string, raw json.RawMessage, isError bool, toolNames map[string]string) (llm.ToolResultMessage, error) {
	name := toolNames[toolUseID]
	content, err := decodeAnthropicContent(context, raw)
	if err != nil {
		return llm.ToolResultMessage{}, err
	}
	if len(content) == 0 {
		content = []llm.Content{llm.TextContent{Text: ""}}
	}
	return llm.ToolResultMessage{
		ToolCallID:  toolUseID,
		ToolName:    name,
		Content:     content,
		IsError:     isError,
		TimestampMS: time.Now().UnixMilli(),
	}, nil
}

// decodeAnthropicContent 把原始 JSON 解码为 text / image 内容块。
func decodeAnthropicContent(context *llm.RequestMessages, raw json.RawMessage) ([]llm.Content, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return []llm.Content{llm.TextContent{Text: ""}}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Content{llm.TextContent{Text: text}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode content: %w", err)
	}
	content := make([]llm.Content, 0, len(parts))
	for index, part := range parts {
		var header struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Resource *struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
				Blob     string `json:"blob"`
			} `json:"resource"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		switch header.Type {
		case "text":
			content = append(content, llm.TextContent{Text: header.Text})
		case "image":
			image, err := common.DecodeImagePart(part)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			content = append(content, image)
		case "resource":
			// MCP tool_result 的 resource 块：text 直接展开；blob 按图片或占位降级。
			if header.Resource == nil {
				continue
			}
			switch {
			case header.Resource.Text != "":
				content = append(content, llm.TextContent{Text: header.Resource.Text})
			case header.Resource.Blob != "" && strings.HasPrefix(header.Resource.MIMEType, "image/"):
				content = append(content, llm.ImageContent{Data: header.Resource.Blob, MIMEType: header.Resource.MIMEType})
			default:
				content = append(content, llm.TextContent{Text: "[resource: " + header.Resource.URI + "]"})
			}
		default:
			// tool_result 内无法投到 IR 的块（document 等）只记 Dropped、
			// 不进内容——模型会在不知道有内容被省略的情况下作答；与
			// user 层 document/file 的占位约定一致，让缺失可见。
			context.Dropped = append(context.Dropped, "content_block:"+header.Type)
			content = append(content, llm.TextContent{
				Text: "[content omitted: " + header.Type + " block not supported]",
			})
		}
	}
	return content, nil
}

// guessSignatureType 给回放的思考签名标注上游 signature_type。形态分类
// 与 responses 前端共用 common.ClassifySignatureType——signature_type 是
// 上游体制属性而非入口协议属性，跨前端回放的 openai 体制签名（序列化
// reasoning item blob）若标成 anthropic 会触发上游 invalid_argument。
// 其余不透明 blob 按 anthropic 体制标注（走 Anthropic 协议的签名要么来自
// 本代理的 claude 模型，要么来自真实 Anthropic API，两边都是 anthropic
// 体制）。缺类型实测被上游容忍，标错类型才会 invalid_argument。
func guessSignatureType(signature string) string {
	if signatureType := common.ClassifySignatureType(signature); signatureType != "" {
		return signatureType
	}
	if signature == "" {
		return ""
	}
	return "anthropic"
}
