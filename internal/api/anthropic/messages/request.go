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
	// 以下为实测上游接受的可选透传位：strict 是 Anthropic 工具顶层字段，
	// annotations 是 MCP 形态的工具注解（只消费 readOnlyHint），
	// server_name/attribution_field_names 同名直传。
	Strict      bool `json:"strict,omitempty"`
	Annotations *struct {
		ReadOnlyHint bool `json:"readOnlyHint,omitempty"`
	} `json:"annotations,omitempty"`
	ServerName            string   `json:"server_name,omitempty"`
	AttributionFieldNames []string `json:"attribution_field_names,omitempty"`
	// 服务端搜索工具（web_search_*）声明携带的域过滤参数；工具本体不
	// 转发，参数喂给 Flow A 侧请求短路的搜索 RPC。
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	BlockedDomains []string `json:"blocked_domains,omitempty"`
	// CacheControl 只作 marker 记账（断点存在性是客户端能力信号），
	// 不透传上游。
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
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
// collectDropped 为 true 时对请求体做二次全量扫描收集顶层未消费字段
// （field:* 标记）；为 false 跳过——Dropped 的唯一读者是 debuglog 请求
// 投影，debug 关时整棵字段树白建。其余 Dropped 写入点都在低频分支，
// 不随该开关门控。
func DecodeRequest(data []byte, collectDropped bool) (AdaptedRequest, error) {
	var request Request
	// 整包 Unmarshal 直接按字节切词，比流式 Decoder 省掉读缓冲的
	// 倍增拷贝（205KB 体实测 ~3x 快、alloc ~1/3）；尾随垃圾同样报错。
	if err := json.Unmarshal(data, &request); err != nil {
		return AdaptedRequest{}, fmt.Errorf("decode anthropic request: %w", err)
	}
	if request.Model == "" {
		return AdaptedRequest{}, errors.New("anthropic request model is required")
	}
	if len(request.Messages) == 0 {
		return AdaptedRequest{}, errors.New("anthropic request messages are required")
	}

	context := llm.RequestMessages{Model: request.Model}
	if collectDropped {
		context.Dropped = append(context.Dropped, common.UnconsumedFields(data, anthropicRequestFields)...)
	}
	context.MaxTokens = common.PositiveIntOrDrop(request.MaxTokens, &context.Dropped, "field:max_tokens")
	context.Temperature = request.Temperature
	context.TopP = request.TopP
	context.TopK = common.PositiveIntOrDrop(request.TopK, &context.Dropped, "field:top_k")
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
	if !common.JSONBlank(request.System) {
		if err := appendSystem(&context, request.System); err != nil {
			return AdaptedRequest{}, err
		}
	}
	if err := appendMessages(&context, request.Messages); err != nil {
		return AdaptedRequest{}, err
	}
	droppedTools := make(map[string]bool)
	for _, tool := range request.Tools {
		common.MarkCacheControl(tool.CacheControl, &context.Dropped)
		if !clientExecutedToolType(tool.Type) {
			// server tool（web_search_*/web_fetch_*/code_execution_* 等）
			// 由供应商托管执行，上游 Devin 无对应物，转发只会制造废工具。
			context.Dropped = append(context.Dropped, "tool:"+tool.Type)
			droppedTools[tool.Name] = true
			continue
		}
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		definition := llm.ToolDefinition{
			Name:                  tool.Name,
			Description:           tool.Description,
			InputSchema:           schema,
			Strict:                tool.Strict,
			ServerName:            tool.ServerName,
			AttributionFieldNames: tool.AttributionFieldNames,
		}
		if tool.Annotations != nil {
			definition.ReadOnlyHint = tool.Annotations.ReadOnlyHint
		}
		context.Tools = append(context.Tools, definition)
	}
	// Claude Code 的 WebSearch 是专用侧请求：tools 只含 web_search_* 变体
	// 且 tool_choice 允许或指名搜索。判定标记置位后由适配器在打
	// GetChatMessage 之前整体短路成一次托管搜索（见 devin.runServerSearch）；
	// 末条 user 文本抽不出查询时不短路，退回通用翻译路径。
	if isServerSearchRequest(request.Tools, context.ToolChoice) {
		if query := serverSearchQuery(context.Messages); query != "" {
			var allowed, blocked []string
			for _, tool := range request.Tools {
				allowed = append(allowed, tool.AllowedDomains...)
				blocked = append(blocked, tool.BlockedDomains...)
			}
			context.ServerSearch = &llm.ServerSearchRequest{
				Query:          query,
				AllowedDomains: allowed,
				BlockedDomains: blocked,
			}
		}
	}
	// tool_choice 指名了被丢的服务端工具时降级为 auto：名字在声明表
	// 之外会被适配器按「指名不存在的工具」打 400，而客户端的本意只是
	// 「用它声明过的搜索」——auto 保留模型在剩余工具里的选择权。从未
	// 声明过的名字不降级，留给上游/适配器的指名校验报错。
	common.DemoteDroppedToolChoice(&context, droppedTools)
	// 相邻 assistant 回合先合并（与 chat/responses 两面同走 IR 层共享
	// 实现）：客户端发连续 assistant 消息时 wire 上的假回合边界会
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

// isServerSearchRequest 判定「WebSearch 专用侧请求」：tools 整表都是
// web_search_* 服务端托管变体，且 tool_choice 缺省/auto/any 或指名的
// 正是这批搜索工具。tool_choice=none 或指名非搜索工具不算——那仍是
// 普通请求，只是恰好没声明客户端工具。
func isServerSearchRequest(tools []Tool, choice *llm.ToolChoice) bool {
	if len(tools) == 0 {
		return false
	}
	for _, tool := range tools {
		if tool.Type != "web_search" && !strings.HasPrefix(tool.Type, "web_search_") {
			return false
		}
	}
	if choice == nil {
		return true
	}
	switch choice.Mode {
	case llm.ToolChoiceAuto, llm.ToolChoiceRequired:
		return true
	case llm.ToolChoiceNamed:
		for _, tool := range tools {
			if tool.Name == choice.ToolName {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// ccSearchQueryPrefix 是 Claude Code WebSearch 侧请求 user 消息的固定
// 模板前缀。
const ccSearchQueryPrefix = "Perform a web search for the query: "

// serverSearchQuery 取末条 user 消息的末尾非空文本作搜索查询；命中
// CC 模板前缀时剥离。模板漂移（前缀缺席）时整段文本仍是可用查询。
func serverSearchQuery(messages []llm.Message) string {
	for index := len(messages) - 1; index >= 0; index-- {
		user, ok := messages[index].(llm.UserMessage)
		if !ok {
			continue
		}
		for block := len(user.Content) - 1; block >= 0; block-- {
			text, ok := user.Content[block].(llm.TextContent)
			if !ok || strings.TrimSpace(text.Text) == "" {
				continue
			}
			return strings.TrimSpace(strings.TrimPrefix(text.Text, ccSearchQueryPrefix))
		}
	}
	return ""
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
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if err := json.Unmarshal(part, &block); err != nil {
			return err
		}
		common.MarkCacheControl(block.CacheControl, &context.Dropped)
		if block.Type != "text" {
			context.Dropped = append(context.Dropped, "system_block:"+block.Type)
			continue
		}
		context.SystemPrompt = common.AppendSystemPrompt(context.SystemPrompt, block.Text)
	}
	return nil
}

// appendMessages 顺序解码消息流。
func appendMessages(context *llm.RequestMessages, messages []Message) error {
	for index, message := range messages {
		if err := appendMessage(context, message); err != nil {
			return fmt.Errorf("message[%d]: %w", index, err)
		}
	}
	return nil
}

// appendMessage 按 role 把单条消息解码进会话。
func appendMessage(context *llm.RequestMessages, message Message) error {
	switch message.Role {
	case "user", "system":
		// Claude Code 在消息流中间插入 role:system 的途中注入（agent 列表、
		// task reminder、system notification）。内容位置敏感——解码为
		// UserMessage 保持时序，不能折叠进系统提示词。
		messages, err := decodeAnthropicUserMessages(context, message.Content)
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
		messages, err := decodeAssistantContent(context, message.Content)
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			if len(bytes.TrimSpace(message.Content)) > 0 {
				context.Dropped = append(context.Dropped, "empty_message:assistant")
			}
			messages = []llm.Message{llm.AssistantMessage{TimestampMS: time.Now().UnixMilli()}}
		}
		context.Messages = append(context.Messages, messages...)
	default:
		// 未知 role 不静默丢——整条降级为 USER 文本保住内容，
		// 与 responses/chat 两面前端同口径。
		context.Dropped = append(context.Dropped, "role:"+message.Role)
		context.Messages = append(context.Messages, common.DemotedRoleMessage(message.Role, message.Content))
	}
	return nil
}

// decodeAnthropicUserMessages 把 Anthropic user 消息 content 拆分为一个或多个中间消息。
// tool_result 内容块会生成独立的 llm.ToolResultMessage。
func decodeAnthropicUserMessages(context *llm.RequestMessages, raw json.RawMessage) ([]llm.Message, error) {
	if common.JSONBlank(raw) {
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
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			ToolUseID    string          `json:"tool_use_id"`
			Content      json.RawMessage `json:"content"`
			IsError      bool            `json:"is_error"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		common.MarkCacheControl(header.CacheControl, &context.Dropped)
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
			// 上游 documents 通道实测可用（claude-5/gpt-5.6 系 supportsDocuments
			// 能力位，历史轮回放也接受）；file_id 等无法解析的形态按请求错误拒绝。
			document, err := common.DecodeDocumentPart(part)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			currentUserContent = append(currentUserContent, document)
		case "tool_result":
			// tool_use_id 缺失或对不上前置调用的结果先按原样进 IR；
			// 解码尾的 DemoteOrphanToolResults 统一降级为 USER 文本。
			flushUser()
			tool, err := decodeToolResult(context, header.ToolUseID, header.Content, header.IsError)
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

// decodeAssistantContent 解码 assistant 消息为消息序列：text/thinking/
// tool_use 聚进当前 assistant 内容；server_tool_use 是服务端托管调用块
// （Server 标记的 ToolCall，同样留在 assistant 内容里）；*_tool_result
// 是服务端已完成执行的结果块——回放 wire 上结果须走 TOOL prompt 与调用
// 配对，故在该处截断 assistant 段、拆出独立 ToolResultMessage。
func decodeAssistantContent(context *llm.RequestMessages, raw json.RawMessage) ([]llm.Message, error) {
	if common.JSONBlank(raw) {
		return []llm.Message{llm.AssistantMessage{
			Content:     []llm.Content{llm.TextContent{Text: ""}},
			TimestampMS: time.Now().UnixMilli(),
		}}, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Message{llm.AssistantMessage{
			Content:     []llm.Content{llm.TextContent{Text: text}},
			TimestampMS: time.Now().UnixMilli(),
		}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode assistant content: %w", err)
	}
	var messages []llm.Message
	content := make([]llm.Content, 0, len(parts))
	flush := func() {
		if len(content) == 0 {
			return
		}
		messages = append(messages, llm.AssistantMessage{Content: content, TimestampMS: time.Now().UnixMilli()})
		content = nil
	}
	for index, part := range parts {
		var header struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			Thinking     string          `json:"thinking"`
			Signature    string          `json:"signature"`
			Data         string          `json:"data"`
			ID           string          `json:"id"`
			Name         string          `json:"name"`
			Input        json.RawMessage `json:"input"`
			ToolUseID    string          `json:"tool_use_id"`
			Content      json.RawMessage `json:"content"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		common.MarkCacheControl(header.CacheControl, &context.Dropped)
		switch header.Type {
		case "text":
			content = append(content, llm.TextContent{Text: header.Text})
		case "thinking":
			content = append(content, llm.ThinkingContent{
				Thinking:      header.Thinking,
				Signature:     header.Signature,
				SignatureType: guessSignatureType(header.Signature),
			})
		case "redacted_thinking":
			// redacted 块的 data 是密封思考体；在 Devin wire 上对应 signature+redacted 标记。
			content = append(content, llm.ThinkingContent{
				Signature:     header.Data,
				SignatureType: guessSignatureType(header.Data),
				Redacted:      true,
			})
		case "tool_use":
			args, custom := common.NormalizeToolArguments(header.Input)
			content = append(content, llm.ToolCall{ID: header.ID, Name: header.Name, Arguments: args, Custom: custom})
		case "server_tool_use":
			args, custom := common.NormalizeToolArguments(header.Input)
			content = append(content, llm.ToolCall{ID: header.ID, Name: header.Name, Arguments: args, Custom: custom, Server: true})
		default:
			if strings.HasSuffix(header.Type, "_tool_result") {
				flush()
				messages = append(messages, decodeServerToolResult(header.Type, header.ToolUseID, header.Content))
				continue
			}
			context.Dropped = append(context.Dropped, "assistant_block:"+header.Type)
		}
	}
	flush()
	return messages, nil
}

// decodeServerToolResult 把 assistant 流内嵌的 *_tool_result 块拆成
// ToolResultMessage。web_search_tool_result 的 content 是
// web_search_result 条目数组——逐条取 title/url 渲成清单，Anthropic
// 侧的锚点字段（encrypted_content/page_age）对上游无意义且体积大，
// 剥离。错误形态 content 是 {"type":"..._error","error_code":...}
// 对象。其余托管结果变体按紧凑 JSON 原文转文本，模型按字段自行消费。
func decodeServerToolResult(blockType, toolUseID string, raw json.RawMessage) llm.ToolResultMessage {
	result := llm.ToolResultMessage{ToolCallID: toolUseID, TimestampMS: time.Now().UnixMilli()}
	trimmed := bytes.TrimSpace(raw)
	text := ""
	if len(trimmed) > 0 {
		if trimmed[0] == '{' {
			var failure struct {
				Type      string `json:"type"`
				ErrorCode string `json:"error_code"`
			}
			if json.Unmarshal(trimmed, &failure) == nil && strings.HasSuffix(failure.Type, "_error") {
				result.IsError = true
				if failure.ErrorCode == "" {
					failure.ErrorCode = "unknown_error"
				}
				text = blockType + ": " + failure.ErrorCode
			}
		}
		if !result.IsError && blockType == "web_search_tool_result" && trimmed[0] == '[' {
			var entries []struct {
				Type  string `json:"type"`
				Title string `json:"title"`
				URL   string `json:"url"`
			}
			if json.Unmarshal(trimmed, &entries) == nil {
				var list strings.Builder
				count := 0
				for _, entry := range entries {
					if entry.Type != "web_search_result" || entry.URL == "" {
						continue
					}
					count++
					fmt.Fprintf(&list, "\n%d. %s — %s", count, entry.Title, entry.URL)
				}
				if count == 0 {
					text = "web search returned no results"
				} else {
					text = "Search results:" + list.String()
				}
			}
		}
		if text == "" && !result.IsError {
			var compact bytes.Buffer
			if json.Compact(&compact, trimmed) == nil {
				text = compact.String()
			} else {
				text = string(trimmed)
			}
		}
	}
	result.Content = []llm.Content{llm.TextContent{Text: text}}
	return result
}

// decodeToolResult 把 tool_result 块解码为 ToolResultMessage；tool_use_id
// 缺失或对不上已知调用时按原样进 IR，由解码尾的
// DemoteOrphanToolResults 降级。
func decodeToolResult(context *llm.RequestMessages, toolUseID string, raw json.RawMessage, isError bool) (llm.ToolResultMessage, error) {
	content, err := decodeAnthropicContent(context, raw)
	if err != nil {
		return llm.ToolResultMessage{}, err
	}
	if len(content) == 0 {
		content = []llm.Content{llm.TextContent{Text: ""}}
	}
	return llm.ToolResultMessage{
		ToolCallID:  toolUseID,
		Content:     content,
		IsError:     isError,
		TimestampMS: time.Now().UnixMilli(),
	}, nil
}

// decodeAnthropicContent 把原始 JSON 解码为 text / image 内容块。
func decodeAnthropicContent(context *llm.RequestMessages, raw json.RawMessage) ([]llm.Content, error) {
	if common.JSONBlank(raw) {
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
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
			Resource     *struct {
				URI      string `json:"uri"`
				MIMEType string `json:"mimeType"`
				Text     string `json:"text"`
				Blob     string `json:"blob"`
			} `json:"resource"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		common.MarkCacheControl(header.CacheControl, &context.Dropped)
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
			// resource 字段缺席时与其他不识块同口径——记 Dropped 加占位文本。
			if header.Resource == nil {
				context.Dropped = append(context.Dropped, "content_block:"+header.Type)
				content = append(content, llm.TextContent{
					Text: "[content omitted: " + header.Type + " block not supported]",
				})
				continue
			}
			switch {
			case header.Resource.Text != "":
				content = append(content, llm.TextContent{Text: header.Resource.Text})
			case header.Resource.Blob != "" && strings.HasPrefix(header.Resource.MIMEType, "image/"):
				content = append(content, llm.ImageContent{Data: header.Resource.Blob, MIMEType: header.Resource.MIMEType})
			case header.Resource.Blob != "":
				// 非图 blob 走文档通道（上游 documents 实测可读），URI 作文件名。
				content = append(content, llm.DocumentContent{Data: header.Resource.Blob, MIMEType: header.Resource.MIMEType, Filename: header.Resource.URI})
			default:
				content = append(content, llm.TextContent{Text: "[resource: " + header.Resource.URI + "]"})
			}
		case "document", "file":
			// tool_result 内容里的文档块与 user 层同通道上行。
			document, err := common.DecodeDocumentPart(part)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			content = append(content, document)
		default:
			// tool_result 内无法投到 IR 的块只记 Dropped 加占位文本，
			// 让缺失对模型可见而不是静默丢上下文。
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
