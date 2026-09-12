// 本文件定义 OpenAI Responses 请求 JSON 到中间 LLM 模型的转换。
//
// Package responses 定义 OpenAI Responses HTTP 协议与中间 LLM 模型之间的编解码。
package responses

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

// Request 是 OpenAI Responses 请求中本适配器支持的字段集合。
type Request struct {
	// Model 是请求使用的模型标识。
	Model string `json:"model"`
	// Instructions 是独立于 input 的系统提示词。
	Instructions string `json:"instructions,omitempty"`
	// Input 是字符串或 Responses input item 数组。
	Input json.RawMessage `json:"input"`
	// Tools 是 OpenAI function 工具定义。
	Tools []Tool `json:"tools,omitempty"`
	// Stream 表示是否请求流式响应。
	Stream bool `json:"stream,omitempty"`
	// MaxOutputTokens 是可选的输出 token 上限。
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// Temperature 是可选的采样温度。
	Temperature *float64 `json:"temperature,omitempty"`
	// PreviousResponseID 是调用方提供的上游响应关联标识。本代理无服务端
	// 响应存储（store=false），该字段被解析但忽略——客户端应回退到
	// 携带完整历史。
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// TopP 是可选的 nucleus 采样参数。
	TopP *float64 `json:"top_p,omitempty"`
	// User 是可选的调用方用户标识。
	User string `json:"user,omitempty"`
	// PromptCacheKey 是可选的调用方缓存键。
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// ToolChoice 控制工具调用行为："auto"/"none"/"required" 或 function 对象。
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	// ParallelToolCalls 为 false 时禁止并行工具调用。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// responsesRequestFields 是 DecodeRequest 已消费的顶层字段；其余字段
// （reasoning/store/service_tier/include/previous_response_id 等）上游
// 没有对应物，记入 Dropped 透出而不是静默吞掉。
var responsesRequestFields = map[string]bool{
	"model": true, "instructions": true, "input": true, "tools": true,
	"stream": true, "max_output_tokens": true, "temperature": true,
	"top_p": true, "user": true, "prompt_cache_key": true,
	"tool_choice": true, "parallel_tool_calls": true,
}

// Tool 是 OpenAI Responses function 工具定义。
type Tool struct {
	// Type 固定为 function。
	Type string `json:"type"`
	// Name 是工具名称。
	Name string `json:"name"`
	// Description 是工具用途说明。
	Description string `json:"description,omitempty"`
	// Parameters 是工具输入 JSON Schema。
	Parameters json.RawMessage `json:"parameters"`
}

// AdaptedRequest 是 OpenAI 请求转换后的中间请求和生成选项。
type AdaptedRequest struct {
	// Context 是供应商无关的完整对话上下文。
	Context llm.RequestMessages
	// Options 是本次生成所需的协议选项。
	Options RequestOptions
}

// RequestOptions 保存不属于对话历史的生成控制参数。
type RequestOptions struct {
	// Stream 表示调用方是否请求流式响应。
	Stream bool
	// MaxOutputTokens 是可选的输出 token 上限。
	MaxOutputTokens *int
	// Temperature 是可选的采样温度。
	Temperature *float64
	// PreviousResponseID 是调用方提供的上游响应关联标识。
	PreviousResponseID string
}

// DecodeRequest 将 OpenAI Responses JSON 请求转换为中间请求。
func DecodeRequest(data []byte) (AdaptedRequest, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&request); err != nil {
		return AdaptedRequest{}, fmt.Errorf("decode responses request: %w", err)
	}
	if request.Model == "" {
		return AdaptedRequest{}, errors.New("responses request model is required")
	}

	context := llm.RequestMessages{Model: request.Model, SystemPrompt: request.Instructions}
	context.Dropped = append(context.Dropped, common.UnconsumedFields(data, responsesRequestFields)...)
	if request.MaxOutputTokens != nil && *request.MaxOutputTokens > 0 {
		context.MaxTokens = request.MaxOutputTokens
	}
	context.Temperature = request.Temperature
	context.TopP = request.TopP
	context.SessionKey = request.PromptCacheKey
	if context.SessionKey == "" {
		context.SessionKey = request.User
	}
	toolChoice, err := common.ParseOpenAIToolChoice(request.ToolChoice)
	if err != nil {
		return AdaptedRequest{}, err
	}
	context.ToolChoice = toolChoice
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		context.DisableParallelToolCalls = true
	}
	if err := appendInputMessages(&context, request.Input); err != nil {
		return AdaptedRequest{}, err
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			context.Dropped = append(context.Dropped, "tool:"+tool.Type)
			continue
		}
		schema := tool.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		context.Tools = append(context.Tools, llm.ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	if err := context.Validate(); err != nil {
		return AdaptedRequest{}, fmt.Errorf("validate adapted request: %w", err)
	}
	return AdaptedRequest{
		Context: context,
		Options: RequestOptions{
			Stream:             request.Stream,
			MaxOutputTokens:    request.MaxOutputTokens,
			Temperature:        request.Temperature,
			PreviousResponseID: request.PreviousResponseID,
		},
	}, nil
}

func appendInputMessages(context *llm.RequestMessages, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		context.Messages = append(context.Messages, llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: text}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode responses input: %w", err)
	}
	// reasoning item 在 input 里位于它所属输出项（assistant message /
	// function_call）之前：summary 文本先缓冲，挂到紧随其后的 assistant
	// 产出上。encrypted_content 为 sealed.* 时是我们自己发出的上游签名，
	// 随思考回放在 wire 上交给上游；外来不透明载荷不可解，忽略。
	var pending pendingReasoning
	// toolNames 随解码增量登记 function_call/custom_tool_call 的
	// call_id→name，output item 按 id 直查，替代逐条 findToolName 回扫。
	toolNames := make(map[string]string)
	for index, item := range items {
		if err := appendInputItem(context, item, &pending, toolNames); err != nil {
			return fmt.Errorf("input[%d]: %w", index, err)
		}
	}
	return nil
}

// pendingReasoning 缓冲 reasoning item 的 summary 文本与可回放签名。
type pendingReasoning struct {
	texts         []string
	signature     string
	signatureType string
}

// consumePendingThinking 取出累积的 reasoning summary，作为 ThinkingContent 前置块。
// 只有签名没有可见文本时按 redacted 处理，与上游的 sealed 表示一致。
func consumePendingThinking(pending *pendingReasoning) []llm.Content {
	if len(pending.texts) == 0 && pending.signature == "" {
		return nil
	}
	block := llm.ThinkingContent{
		Thinking:          strings.Join(pending.texts, "\n"),
		ThinkingSignature: pending.signature,
		SignatureType:     pending.signatureType,
		Redacted:          len(pending.texts) == 0,
	}
	*pending = pendingReasoning{}
	return []llm.Content{block}
}

// classifyReasoningSignature 识别回放进 input 的 encrypted_content 属于哪种
// 上游签名体制：sealed.* 与序列化 reasoning item 数组（openai 型）都是我们
// 自己下发过的形态，原样回放；其余外来不透明载荷不可解，丢弃。
func classifyReasoningSignature(encrypted string) (signature, signatureType string, keep bool) {
	switch {
	case strings.HasPrefix(encrypted, "sealed."):
		return encrypted, "sealed", true
	case isOpenAIReasoningSignature(encrypted):
		return encrypted, "openai", true
	default:
		return "", "", false
	}
}

// isOpenAIReasoningSignature 判断载荷是否为 openai 型签名——上游 signature
// 字段的实测形态是序列化 Responses reasoning item 数组。下行时我们把整个
// blob 原样放进 encrypted_content，回放时按同一形态识别。
func isOpenAIReasoningSignature(blob string) bool {
	trimmed := strings.TrimSpace(blob)
	if !strings.HasPrefix(trimmed, "[") {
		return false
	}
	var items []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(trimmed), &items) != nil || len(items) == 0 {
		return false
	}
	return items[0].Type == "reasoning"
}

func appendInputItem(context *llm.RequestMessages, raw json.RawMessage, pending *pendingReasoning, toolNames map[string]string) error {
	var header struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fmt.Errorf("decode input item: %w", err)
	}
	if header.Type == "" && header.Role != "" {
		header.Type = "message"
	}
	switch header.Type {
	case "reasoning":
		var item struct {
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		for _, part := range item.Summary {
			if part.Type == "summary_text" && part.Text != "" {
				pending.texts = append(pending.texts, part.Text)
			}
		}
		if signature, signatureType, keep := classifyReasoningSignature(item.EncryptedContent); keep {
			pending.signature = signature
			pending.signatureType = signatureType
		} else if item.EncryptedContent != "" {
			// 外来不透明载荷不可解，记录而不透传。
			context.Dropped = append(context.Dropped, "reasoning:encrypted_content")
		}
		return nil
	case "message":
		return appendMessageItem(context, raw, header.Role, pending)
	case "function_call":
		var item struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		arguments := json.RawMessage(item.Arguments)
		toolNames[item.CallID] = item.Name
		content := append(consumePendingThinking(pending),
			llm.ToolCall{ID: item.CallID, Name: item.Name, Arguments: arguments})
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			StopReason:  llm.StopReasonToolUse,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	case "custom_tool_call":
		// freeform 工具调用的 input 是原文不是 JSON（如 apply_patch 补丁），
		// 走 Custom 通道原样上行到 invalid_json_str。
		var item struct {
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			Input  string `json:"input"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		toolNames[item.CallID] = item.Name
		content := append(consumePendingThinking(pending),
			llm.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Input), Custom: true})
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			StopReason:  llm.StopReasonToolUse,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	case "function_call_output", "custom_tool_call_output":
		// reasoning 与产出之间插入结果项 → reasoning 成孤儿，丢弃缓冲。
		*pending = pendingReasoning{}
		// 客户端对调用 ID 字段名有四种植法（call_id 是规范，其余来自
		// Chat 习惯/驼峰序列化/id 即调用 id 的实现），按序兼容取第一个非空。
		var item struct {
			CallID      string          `json:"call_id"`
			ToolCallID  string          `json:"tool_call_id"`
			CallIDCamel string          `json:"callId"`
			ID          string          `json:"id"`
			Output      json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		callID := item.CallID
		if callID == "" {
			callID = item.ToolCallID
		}
		if callID == "" {
			callID = item.CallIDCamel
		}
		if callID == "" {
			callID = item.ID
		}
		content, err := decodeToolOutput(item.Output)
		if err != nil {
			return err
		}
		toolName := toolNames[callID]
		if toolName == "" {
			// 压缩后的历史可能丢掉对应的 function_call；对齐 Anthropic
			// 解码路径的兜底名，避免整请求失败。
			context.Dropped = append(context.Dropped, "unmatched_call_id:"+callID)
			toolName = "tool"
		}
		context.Messages = append(context.Messages, llm.ToolResultMessage{
			ToolCallID:  callID,
			ToolName:    toolName,
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	default:
		// tool_search_output / mcp_* 等服务端工具产物没有对应中间类型；
		// 静默丢弃会丢上下文，降级为 USER 文本保住内容。
		context.Dropped = append(context.Dropped, "item:"+header.Type)
		context.Messages = append(context.Messages, llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: "[input item type=" + header.Type + "]\n" + string(raw)}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	}
}

// decodeToolOutput 解码 function_call_output/custom_tool_call_output 的
// output：字符串直接成文本；part 数组（可含 input_image——实测上游
// tool_result 图像子通道有效）按消息内容解码；其余 JSON 原样转文本。
func decodeToolOutput(raw json.RawMessage) ([]llm.Content, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Content{llm.TextContent{Text: text}}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("function call output is required")
	}
	if trimmed[0] == '[' {
		content, err := common.DecodeContent(raw)
		if err == nil && len(content) > 0 {
			return content, nil
		}
	}
	return []llm.Content{llm.TextContent{Text: string(raw)}}, nil
}

func appendMessageItem(context *llm.RequestMessages, raw json.RawMessage, role string, pending *pendingReasoning) error {
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		context.Dropped = append(context.Dropped, "message_role:"+role)
		return nil
	}
	var item struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	content, err := common.DecodeContent(item.Content)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return nil
	}
	switch role {
	case "user":
		// 非 assistant 产出介入 → 缓冲的 reasoning 成孤儿，丢弃。
		*pending = pendingReasoning{}
		context.Messages = append(context.Messages, llm.UserMessage{Content: content, TimestampMS: time.Now().UnixMilli()})
	case "assistant":
		content = append(consumePendingThinking(pending), content...)
		assistant := llm.AssistantMessage{Content: content, TimestampMS: time.Now().UnixMilli()}
		if strings.HasPrefix(item.ID, "msg_") {
			// msg_* 是 OpenAI 侧 message item 的真实标识，上游 output_id
			// 回放用同一个值。
			assistant.OutputID = item.ID
		}
		context.Messages = append(context.Messages, assistant)
	case "system", "developer":
		*pending = pendingReasoning{}
		text := common.ContentText(content)
		if context.SystemPrompt != "" && text != "" {
			context.SystemPrompt += "\n"
		}
		context.SystemPrompt += text
	}
	return nil
}
