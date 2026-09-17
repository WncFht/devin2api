// 本文件定义 OpenAI Chat Completions 请求 JSON 到中间 LLM 模型的转换。
package chat

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
)

// Request 是 OpenAI Chat Completions 请求中本适配器支持的字段集合。
type Request struct {
	Model      string          `json:"model"`
	Messages   []Message       `json:"messages"`
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	// Functions 与 FunctionCall 是 2023-06 前的旧版 function-calling
	// 形态：functions 与 tools 并存时两边都收，function_call 只在
	// tool_choice 缺席时兜底为工具选择。
	Functions           []FunctionTool  `json:"functions,omitempty"`
	FunctionCall        json.RawMessage `json:"function_call,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *StreamOptions  `json:"stream_options,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	TopK                *int            `json:"top_k,omitempty"`
	Seed                *int64          `json:"seed,omitempty"`
	User                string          `json:"user,omitempty"`
	PromptCacheKey      string          `json:"prompt_cache_key,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	N                   *int            `json:"n,omitempty"`
}

// Message 是 Chat Completions 消息条目。
type Message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	// FunctionCall 是旧版（2023-06 前）function-calling 形态的助手调用；
	// 与 tool_calls 互斥，解码时合成 call id 转成 ToolCall。
	FunctionCall *FunctionCall `json:"function_call,omitempty"`
	// Name 是旧版 role:"function" 结果消息携带的函数名。
	Name string `json:"name,omitempty"`
	// ReasoningContent 是 DeepSeek 系/部分代理回传思考文本的约定字段；
	// 解码进 ThinkingContent，客户端回灌历史时思考不会静默丢失。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// ToolCall 是助手消息中的工具调用（也用于流式增量）。
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall 是工具调用的函数部分。
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool 是 OpenAI Chat function 工具定义。
type Tool struct {
	Type     string       `json:"type"`
	Function FunctionTool `json:"function"`
}

// FunctionTool 是 function 工具详情。
type FunctionTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// StreamOptions 是流式额外选项。
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage,omitempty"`
}

// chatRequestFields 是 DecodeRequest 已消费的顶层字段；其余字段
// （reasoning_effort/store/service_tier/metadata/response_format 等）
// 上游没有对应物，记入 Dropped 透出而不是静默吞掉。
var chatRequestFields = map[string]bool{
	"model": true, "messages": true, "tools": true, "tool_choice": true,
	"functions": true, "function_call": true,
	"stream": true, "stream_options": true, "max_tokens": true,
	"max_completion_tokens": true, "temperature": true, "top_p": true,
	"stop": true, "top_k": true, "seed": true, "user": true,
	"prompt_cache_key": true, "parallel_tool_calls": true, "n": true,
}

// AdaptedRequest 是 Chat 请求转换后的中间请求和生成选项。
type AdaptedRequest struct {
	Context llm.RequestMessages
	Options RequestOptions
}

// RequestOptions 保存不属于对话历史的生成控制参数。
type RequestOptions struct {
	Stream       bool
	IncludeUsage bool
}

// DecodeRequest 将 OpenAI Chat Completions JSON 请求转换为中间请求。
// collectDropped 为 true 时对请求体做二次全量扫描收集顶层未消费字段
// （field:* 标记）；为 false 跳过——Dropped 的唯一读者是 debuglog 请求
// 投影，debug 关时整棵字段树白建。其余 Dropped 写入点都在低频分支，
// 不随该开关门控。
func DecodeRequest(data []byte, collectDropped bool) (AdaptedRequest, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&request); err != nil {
		return AdaptedRequest{}, fmt.Errorf("decode chat request: %w", err)
	}
	if decoder.More() {
		// 顶层 JSON 后还有内容说明 body 不是单个请求对象——多半
		// 是客户端 bug 或代理误拼接，静默忽略会掩盖截断/串包。
		return AdaptedRequest{}, errors.New("chat request has trailing data after JSON body")
	}
	if request.Model == "" {
		return AdaptedRequest{}, errors.New("chat request model is required")
	}
	if len(request.Messages) == 0 {
		return AdaptedRequest{}, errors.New("chat request messages are required")
	}

	context := llm.RequestMessages{Model: request.Model}
	if collectDropped {
		context.Dropped = append(context.Dropped, common.UnconsumedFields(data, chatRequestFields)...)
	}
	// max_completion_tokens 优先于 max_tokens（OpenAI 语义）；选中的指针
	// 非正时静默丢弃会让调用方以为上限已生效——记 Dropped 透出。
	maxTokensValue := request.MaxCompletionTokens
	droppedMaxTokens := "field:max_completion_tokens"
	if maxTokensValue == nil {
		maxTokensValue = request.MaxTokens
		droppedMaxTokens = "field:max_tokens"
	}
	context.MaxTokens = common.PositiveIntOrDrop(maxTokensValue, &context.Dropped, droppedMaxTokens)
	context.Temperature = request.Temperature
	context.TopP = request.TopP
	context.TopK = common.PositiveIntOrDrop(request.TopK, &context.Dropped, "field:top_k")
	context.Seed = request.Seed
	// 上游 CASCADE 通道只支持单次补全：num_completions>1 会中途崩流，
	// 本地尽早拒绝比打到上游更可读。
	if request.N != nil && *request.N > 1 {
		return AdaptedRequest{}, errors.New("chat request n > 1 is not supported by this provider")
	}
	toolChoice, err := common.ParseOpenAIToolChoice(request.ToolChoice, &context.Dropped)
	if err != nil {
		return AdaptedRequest{}, err
	}
	context.ToolChoice = toolChoice
	// 请求级 function_call 是 tool_choice 的旧版前身（"auto"/"none"
	// 字符串或 {"name":X} 对象）；tool_choice 在场时以它为准。
	if context.ToolChoice == nil {
		if choice, ok := parseLegacyFunctionCall(request.FunctionCall); ok {
			context.ToolChoice = choice
		} else {
			context.Dropped = append(context.Dropped, "field:function_call")
		}
	}
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		context.DisableParallelToolCalls = true
	}
	if !common.JSONBlank(request.Stop) {
		var stops []string
		if err := json.Unmarshal(request.Stop, &stops); err != nil {
			var single string
			if json.Unmarshal(request.Stop, &single) == nil && single != "" {
				stops = []string{single}
			}
		}
		if stops == nil {
			// stop 为数字/对象等不识形态，不生效不能静默吞。
			context.Dropped = append(context.Dropped, "field:stop")
		} else {
			context.StopSequences = stops
		}
	}
	context.SessionKey = common.SessionKey(request.PromptCacheKey, request.User)
	// callIDs 登记已发出的全部调用 id（含 tool_call 真 id 与旧版
	// function_call 的合成 id）：function_call 没有 id 字段，要造
	// call_function_N 序数 id，造之前必须确认不与既有 id 撞车。
	// functionIDs 是旧版形态的 name→合成 id 映射：role:"function"
	// 结果消息按函数名而非 call id 对账。
	callIDs := make(map[string]struct{})
	functionIDs := make(map[string]string)
	if err := appendMessages(&context, request.Messages, callIDs, functionIDs); err != nil {
		return AdaptedRequest{}, err
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			context.Dropped = append(context.Dropped, "tool:"+tool.Type)
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		context.Tools = append(context.Tools, llm.ToolDefinition{
			Name:        tool.Function.Name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	// functions 是旧版工具声明形态：与 tools 同构，直接并入。
	for _, fn := range request.Functions {
		schema := fn.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		context.Tools = append(context.Tools, llm.ToolDefinition{
			Name:        fn.Name,
			Description: fn.Description,
			InputSchema: schema,
		})
	}
	// 相邻 assistant 回合先合并（与 responses/anthropic 两面同走 IR 层
	// 共享实现）：客户端发连续 assistant 消息时 wire 上的假回合边界会
	// 抬高提前 EOS 概率。
	context.MergeAdjacentAssistantTurns()
	// 孤儿 tool result 在 IR 校验前统一降级为 USER 文本——校验要求
	// ToolCallID 非空，而孤儿的调用 id 本来就是缺的。
	context.DemoteOrphanToolResults()
	if err := context.Validate(); err != nil {
		return AdaptedRequest{}, &llm.Failure{Code: "invalid_argument", Message: "validate adapted request: " + err.Error(), Cause: err}
	}

	return AdaptedRequest{
		Context: context,
		Options: RequestOptions{
			Stream:       request.Stream,
			IncludeUsage: request.StreamOptions != nil && request.StreamOptions.IncludeUsage,
		},
	}, nil
}

// appendMessages 逐条解码 messages 数组并保序追加进会话。
func appendMessages(context *llm.RequestMessages, messages []Message, callIDs map[string]struct{}, functionIDs map[string]string) error {
	for index, message := range messages {
		if err := appendMessage(context, message, callIDs, functionIDs); err != nil {
			return fmt.Errorf("message[%d]: %w", index, err)
		}
	}
	return nil
}

// appendMessage 按 role 把单条消息解码进会话。
func appendMessage(context *llm.RequestMessages, message Message, callIDs map[string]struct{}, functionIDs map[string]string) error {
	switch message.Role {
	case "system", "developer":
		content, err := common.DecodeContent(message.Content, &context.Dropped)
		if err != nil {
			return err
		}
		if len(content) == 0 {
			// content 在场但解不出内容块（空数组/全部 part 不识）：
			// 对 SystemPrompt 无贡献，记 Dropped 让缺失可对账——
			// 与 anthropic/responses 面同口径。
			context.Dropped = append(context.Dropped, "empty_message:"+message.Role)
		}
		text := common.ContentText(content)
		context.SystemPrompt = common.AppendSystemPrompt(context.SystemPrompt, text)
	case "user":
		content, err := decodeUserContent(context, message.Content)
		if err != nil {
			return err
		}
		context.Messages = append(context.Messages, llm.UserMessage{
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
	case "assistant":
		content, err := decodeAssistantContent(context, message, callIDs, functionIDs)
		if err != nil {
			return err
		}
		if len(content) == 0 {
			// content 缺席/null/空数组且无 tool_calls/function_call/
			// reasoning：助手消息什么都没贡献，wire 端会按
			// DroppedEmptyAssistant 丢弃——记 Dropped 对齐 anthropic 面口径。
			context.Dropped = append(context.Dropped, "empty_message:assistant")
		}
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
	case "tool":
		// tool_call_id 缺失或对不上前置调用的结果先按原样进 IR；
		// 解码尾的 DemoteOrphanToolResults 统一降级为 USER 文本。
		content, err := common.DecodeContent(message.Content, &context.Dropped)
		if err != nil {
			return err
		}
		context.Messages = append(context.Messages, llm.ToolResultMessage{
			ToolCallID:  message.ToolCallID,
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
	case "function":
		// 旧版工具结果：没有 call id，凭 name 对回对应 function_call
		// 的合成 id；对不上说明历史里没有该调用，造孤儿 id 交给
		// DemoteOrphanToolResults 降级成文本而不是 400 整单。
		content, err := common.DecodeContent(message.Content, &context.Dropped)
		if err != nil {
			return err
		}
		id := functionIDs[message.Name]
		if id == "" {
			context.Dropped = append(context.Dropped, "unmatched_function_name:"+message.Name)
			id = "call_function_unmatched_" + message.Name
		}
		context.Messages = append(context.Messages, llm.ToolResultMessage{
			ToolCallID:  id,
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
	default:
		// 未知 role 不静默丢——整条降级为 USER 文本保住内容，
		// 与 responses/anthropic 两面前端同口径。
		context.Dropped = append(context.Dropped, "role:"+message.Role)
		context.Messages = append(context.Messages, common.DemotedRoleMessage(message.Role, message.Content))
	}
	return nil
}

// decodeUserContent 解码 user 消息内容；空/null 归一为空文本块。
// content 在场但解不出内容块（空数组/全部 part 不识）时同样落成
// 空文本占位保住轮次，并记 empty_message:user——与 anthropic 面同口径。
func decodeUserContent(context *llm.RequestMessages, raw json.RawMessage) ([]llm.Content, error) {
	if common.JSONBlank(raw) {
		return []llm.Content{llm.TextContent{Text: ""}}, nil
	}
	content, err := common.DecodeContent(raw, &context.Dropped)
	if err != nil {
		return nil, err
	}
	if len(content) == 0 {
		context.Dropped = append(context.Dropped, "empty_message:user")
		return []llm.Content{llm.TextContent{Text: ""}}, nil
	}
	return content, nil
}

// decodeAssistantContent 解码 assistant 消息的正文与 tool_calls
// （含旧版 function_call 单字段形态）。
func decodeAssistantContent(context *llm.RequestMessages, message Message, callIDs map[string]struct{}, functionIDs map[string]string) ([]llm.Content, error) {
	var content []llm.Content
	if !common.JSONBlank(message.Content) {
		decoded, err := common.DecodeContent(message.Content, &context.Dropped)
		if err != nil {
			return nil, err
		}
		content = append(content, decoded...)
	}
	if message.ReasoningContent != "" {
		content = append(content, llm.ThinkingContent{Thinking: message.ReasoningContent})
	}
	for _, call := range message.ToolCalls {
		if call.Type != "" && call.Type != "function" {
			context.Dropped = append(context.Dropped, "tool_call:"+call.Type)
			continue
		}
		args, custom := common.NormalizeToolArguments(json.RawMessage(call.Function.Arguments))
		callIDs[call.ID] = struct{}{}
		content = append(content, llm.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: args,
			Custom:    custom,
		})
	}
	if call := message.FunctionCall; call != nil {
		// 旧版 function_call 没有 call id：按「已登记调用数」合成
		// call_function_N 序数 id，name→id 登记进 functionIDs 供
		// function 角色结果消息对账。
		var callID string
		for i := len(callIDs); ; i++ {
			callID = fmt.Sprintf("call_function_%d", i)
			if _, taken := callIDs[callID]; !taken {
				break
			}
		}
		callIDs[callID] = struct{}{}
		functionIDs[call.Name] = callID
		args, custom := common.NormalizeToolArguments(json.RawMessage(call.Arguments))
		content = append(content, llm.ToolCall{
			ID:        callID,
			Name:      call.Name,
			Arguments: args,
			Custom:    custom,
		})
	}
	return content, nil
}

// parseLegacyFunctionCall 解析请求级 function_call 旧字段：
// "auto"/"none" 字符串或 {"name":X} 对象；空/null 输入返回 (nil, true)
// 表示字段缺席，其余不识形态返回 ok=false 由调用方记 dropped。
func parseLegacyFunctionCall(raw json.RawMessage) (*llm.ToolChoice, bool) {
	if common.JSONBlank(raw) {
		return nil, true
	}
	var mode string
	if json.Unmarshal(raw, &mode) == nil {
		switch mode {
		case "", "auto":
			return nil, true
		case "none":
			return &llm.ToolChoice{Mode: llm.ToolChoiceNone}, true
		}
		return nil, false
	}
	var named struct {
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &named) == nil && named.Name != "" {
		return &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: named.Name}, true
	}
	return nil, false
}
