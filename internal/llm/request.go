// 本文件定义与具体模型供应商无关的请求上下文、消息、内容块和工具。
//
// Package llm 定义与具体模型供应商无关的请求消息和响应事件。
//
//	agent loop 的语义等价，不是 Provider 请求结构等价
package llm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
)

// MessageRole 标识一条中间消息在对话中的角色。
type MessageRole string

const (
	MessageRoleUser       MessageRole = "user"
	MessageRoleAssistant  MessageRole = "assistant"
	MessageRoleToolResult MessageRole = "toolResult"
)

// ContentType 标识一个消息内容块的种类。
type ContentType string

const (
	ContentTypeText     ContentType = "text"
	ContentTypeThinking ContentType = "thinking"
	ContentTypeImage    ContentType = "image"
	ContentTypeToolCall ContentType = "toolCall"
	// ContentTypeServerToolResult 是服务端托管工具的执行结果块（响应方向
	// 专属）：与对应的 Server ToolCall 一起出现在 AssistantMessage.Content
	// 里，按 ToolCallID 配对。请求方向不会由任何解码器产出——托管工具的
	// 回放走普通 call+ToolResultMessage 对，结果正文已被渲染成文本。
	ContentTypeServerToolResult ContentType = "serverToolResult"
)

// RequestMessages 是发送给任意供应商适配器的完整请求上下文。
type RequestMessages struct {
	// Model 是调用方指定的模型标识；空表示由适配器使用默认配置。
	Model string
	// SystemPrompt 是独立于普通消息历史的系统提示词。
	SystemPrompt string
	// Messages 是按时间顺序排列、可跨供应商重放的完整对话历史。
	Messages []Message
	// Tools 是本次请求允许模型调用的工具定义。
	Tools []ToolDefinition
	// MaxTokens 是可选的输出 token 上限；nil 表示使用供应商默认。
	MaxTokens *int
	// Temperature 是可选的采样温度；nil 表示使用供应商默认。
	Temperature *float64
	// TopP 是可选的 nucleus 采样参数；nil 表示使用供应商默认。
	TopP *float64
	// TopK 是可选的 top-k 采样参数；nil 表示使用供应商默认。
	TopK *int
	// StopSequences 是可选的停止序列列表。
	StopSequences []string
	// ToolChoice 是调用方对工具调用行为的偏好；nil 表示交给模型自选。
	ToolChoice *ToolChoice
	// DisableParallelToolCalls 为 true 时要求模型不要在一轮内发起多个并行工具调用。
	// Devin 上游接受但会忽略该标记（实测并行调用照常发出），适配器仅做形状透传。
	DisableParallelToolCalls bool
	// Seed 是可选的采样种子；nil 表示由供应商随机。
	Seed *int64
	// SessionKey 是调用方提供的会话标识（如 user / prompt_cache_key /
	// metadata.user_id），适配器可据此为同一对话派生稳定的上游会话 ID。
	// 空表示调用方未提供。
	SessionKey string
	// ServerSearch 非空表示本请求已被前端判定为「服务端托管搜索侧请求」
	//（如 Claude Code 的 WebSearch 专用请求：tools 只含 web_search_* 变体）。
	// 适配器不走 GetChatMessage 主路径，改为代调上游搜索 RPC 并合成
	// 一次完整的工具调用响应流。
	ServerSearch *ServerSearchRequest
	// Dropped 记录请求解码与规范化时被丢弃/降级的下游字段
	//（"kind:detail"），供调试日志透出——「解码即过滤」的静默面
	// 需要可观测。
	Dropped []string
}

// 会话亲和种子消费的 marker 前缀（Dropped 子集）：客户端声明的能力/
// 行为面——cache_control 断点类型与 anthropic-beta flag 改变上游特性，
// 同会话键下声明漂移应换 lane。发射端（api 解码器、app 头后处理）与
// 消费端（devin 适配器 sessionSeed）共用这组常量防止字面量漂移；
// 其余 Dropped marker 是逐请求修复/降级痕迹（field:* 另受 collectDropped
// 调试门控），进种子会让亲和依赖调试开关或逐轮抖动。
const (
	MarkerCacheControl  = "cache_control:"
	MarkerAnthropicBeta = "anthropic_beta:"
)

// IsSeedMarker 判定 Dropped marker 是否进会话亲和种子（sessionSeed）。
func IsSeedMarker(marker string) bool {
	return strings.HasPrefix(marker, MarkerCacheControl) ||
		strings.HasPrefix(marker, MarkerAnthropicBeta)
}

// ServerSearchRequest 是一次服务端托管搜索的完整参数。Query 已从客户端
// 消息里抽取；Allowed/BlockedDomains 来自工具声明（上游只支持单域字面量
// 与结果侧过滤，多域语义由适配器展开）。
type ServerSearchRequest struct {
	Query          string
	AllowedDomains []string
	BlockedDomains []string
}

// RequestRepairs 是一次请求投影为上游 wire 格式时发生的静默修复计数，
// 与响应方向的 AssistantMessageDiagnostic 同族（诊断，不改变主结果）。
// 协议翻译层的修复（调用-结果配对重排、空助手消息丢弃、历史图片剥离、
// 策略指纹改写）本身合法，但上游协议漂移排障必须能回答
// 「代理对这次请求动过什么」。全零时整个字段不落盘。
// 孤儿 tool result 的降级发生在 IR 层（DemoteOrphanToolResults），不进
// wire——它的审计走 RequestMessages.Dropped 逐条标记而非这里的计数。
type RequestRepairs struct {
	// ReorderedPrompts 是 call→result 配对重排中改变位置的 prompt 数。
	ReorderedPrompts int `json:"reordered_prompts,omitempty"`
	// DroppedEmptyAssistant 是被跳过的空助手消息数（上游见空回复会退化）。
	DroppedEmptyAssistant int `json:"dropped_empty_assistant,omitempty"`
	// OmittedHistoryImages 是被改写为文本占位的历史图片数（上游只收当前轮图片）。
	OmittedHistoryImages int `json:"omitted_history_images,omitempty"`
	// DroppedDuplicateTools 是按名去重时被丢弃的重复工具声明数
	//（上游 tools[] 重名直接 invalid_argument）。
	DroppedDuplicateTools int `json:"dropped_duplicate_tools,omitempty"`
	// SanitizeHits 是上游内容策略指纹改写按规则 id 的命中计数。
	SanitizeHits map[string]int `json:"sanitize_hits,omitempty"`
	// SchemaRefDropped 是工具 schema 归一化剥掉的本地 $ref 键数
	// （解不开/循环引用与深度保险丝截断——上游对 $ref 确定性拒绝，
	// 剥键保兄弟约束是语义漂移，必须可对账）。
	SchemaRefDropped int `json:"schema_ref_dropped,omitempty"`
}

// Total 返回全部修复动作的合计次数，供日志索引汇总成单字段。
func (repairs RequestRepairs) Total() int {
	total := repairs.ReorderedPrompts +
		repairs.DroppedEmptyAssistant + repairs.OmittedHistoryImages +
		repairs.DroppedDuplicateTools + repairs.SchemaRefDropped
	for _, hits := range repairs.SanitizeHits {
		total += hits
	}
	return total
}

// ToolChoiceMode 标识客户端要求的工具调用模式。
type ToolChoiceMode string

const (
	// ToolChoiceAuto 由模型自行决定是否调用工具（默认行为）。
	ToolChoiceAuto ToolChoiceMode = "auto"
	// ToolChoiceNone 禁止模型调用工具。
	ToolChoiceNone ToolChoiceMode = "none"
	// ToolChoiceRequired 强制模型本轮必须调用工具（任一工具）。
	ToolChoiceRequired ToolChoiceMode = "required"
	// ToolChoiceNamed 强制模型调用 ToolName 指定的工具。
	ToolChoiceNamed ToolChoiceMode = "named"
)

// ToolChoice 是供应商无关的工具调用偏好。
// Anthropic 的 {"type":"any"} 在本层归一为 ToolChoiceRequired——
// Devin 上游的 option_name 合法值是 none/auto/required，"any" 会被拒绝。
type ToolChoice struct {
	// Mode 是归一化后的调用模式。
	Mode ToolChoiceMode
	// ToolName 是 ToolChoiceNamed 模式下要求调用的工具名。
	ToolName string
}

// Message 是用户、助手或工具结果消息的统一接口。
// 契约：生产方一律以值形态存进切片（UserMessage，而非 *UserMessage）。
// 值接收者方法让指针形态同样满足接口，但 DemoteOrphanToolResults 与
// MergeAdjacentAssistantTurns 的 type switch 只认值形态——混入指针会
// 过 Validate 却被正规化静默跳过。
type Message interface {
	// Role 返回消息在对话中的角色。
	Role() MessageRole
	// Validate 检查消息是否满足中间层约束。
	Validate() error
}

// Content 是文字、思考、图片或工具调用内容块的统一接口。
type Content interface {
	// ContentType 返回内容块的种类。
	ContentType() ContentType
	// Validate 检查内容块是否满足中间层约束。
	Validate() error
}

// TextContent 表示普通文字内容块。
type TextContent struct {
	// Text 是向用户展示或作为上下文重放的文字。
	Text string
}

// ContentType 返回文字内容类型。
func (TextContent) ContentType() ContentType { return ContentTypeText }

// Validate 检查文字内容块。
func (TextContent) Validate() error { return nil }

// ThinkingContent 表示模型的思考或推理内容块。
type ThinkingContent struct {
	// Thinking 是可见的思考内容；加密思考场景下可以为空。
	Thinking string
	// ThinkingSignature 是供应商签名或加密后的不透明载荷，重放时应原样保留。
	ThinkingSignature string
	// SignatureType 是签名载荷的格式标识（Devin 上游 signature_type：
	// sealed/anthropic/openai）。签名的解析规则由它决定——openai 型签名
	// 是序列化的 Responses reasoning item，其余是不透明 blob。重放时必须
	// 随签名原样回传，实测错配触发上游 invalid_argument。
	SignatureType string
	// Redacted 表示思考正文已被供应商隐藏，签名中可能保存可重放载荷。
	Redacted bool
}

// ContentType 返回思考内容类型。
func (ThinkingContent) ContentType() ContentType { return ContentTypeThinking }

// Validate 检查思考内容块。
func (content ThinkingContent) Validate() error {
	if content.Redacted && content.ThinkingSignature == "" {
		return errors.New("redacted thinking content requires a signature")
	}
	return nil
}

// ImageContent 表示以 base64 编码传递的图片附件。
type ImageContent struct {
	// Data 是不含 data URL 前缀的 base64 图片数据。
	Data string
	// MIMEType 是图片的媒体类型，例如 image/png。
	MIMEType string
}

// ContentType 返回图片内容类型。
func (ImageContent) ContentType() ContentType { return ContentTypeImage }

// Validate 检查图片内容块。
func (content ImageContent) Validate() error {
	if content.Data == "" {
		return errors.New("image data is required")
	}
	if content.MIMEType == "" {
		return errors.New("image MIME type is required")
	}
	return nil
}

// ToolCall 表示助手发起的一次工具调用。
type ToolCall struct {
	// ID 是供应商分配的调用标识，用于关联后续工具结果。
	ID string
	// Name 是要调用的工具名称。
	Name string
	// Arguments 是模型增量拼接完成后的 JSON 参数对象。
	Arguments json.RawMessage
	// Custom 为 true 时 Arguments 不是 JSON 对象而是供应商原文
	//（Devin invalid_json_str/is_custom_tool_call：custom/freeform 工具
	// 的参数体本来就不是 JSON，如 apply_patch 的补丁文本）。请求方向
	// 客户端回灌的畸形 JSON 参数也按此保留原文，不吞成 {}。
	Custom bool
	// Server 为 true 时这是服务端托管工具的调用（如 OpenAI web_search）：
	// 客户端不执行任何东西，由代理代调上游专用 RPC 并把结果作为
	// ServerToolResult 块随同一响应下发。编码器据此把调用渲染成各协议的
	// 托管形态（anthropic server_tool_use / responses web_search_call）。
	Server bool
}

// ContentType 返回工具调用内容类型。
func (ToolCall) ContentType() ContentType { return ContentTypeToolCall }

// Validate 检查工具调用。
func (call ToolCall) Validate() error {
	if call.ID == "" {
		return errors.New("tool call ID is required")
	}
	if call.Name == "" {
		return errors.New("tool call name is required")
	}
	if call.Custom {
		return nil
	}
	if !IsJSONObject(call.Arguments) {
		return errors.New("tool call arguments must be a JSON object")
	}
	return nil
}

// UserMessage 表示一条用户消息。
type UserMessage struct {
	// Content 是用户提交的文字和图片内容块。
	Content []Content
	// TimestampMS 是创建消息时的 Unix 毫秒时间戳。
	TimestampMS int64
}

// Role 返回用户角色。
func (UserMessage) Role() MessageRole { return MessageRoleUser }

// Validate 检查用户消息。
func (message UserMessage) Validate() error {
	return validateContent(message.Content, ContentTypeText, ContentTypeImage)
}

// ToolResultMessage 表示一次工具调用的执行结果。
type ToolResultMessage struct {
	// ToolCallID 是本结果所对应的工具调用标识——wire 上凭它配对，
	// 工具名不上行（上游 prompt 只带 call id + 正文），故不存。
	ToolCallID string
	// Content 是返回给模型的文字和图片内容块。
	Content []Content
	// IsError 表示工具执行是否失败。
	IsError bool
	// TimestampMS 是创建消息时的 Unix 毫秒时间戳。
	TimestampMS int64
}

// Role 返回工具结果角色。
func (ToolResultMessage) Role() MessageRole { return MessageRoleToolResult }

// Validate 检查工具结果消息。
func (message ToolResultMessage) Validate() error {
	if message.ToolCallID == "" {
		return errors.New("tool result call ID is required")
	}
	if err := validateContent(message.Content, ContentTypeText, ContentTypeImage); err != nil {
		return err
	}
	return nil
}

// WebSearchResult 是一条服务端搜索命中的标准化结果。
type WebSearchResult struct {
	// Title 是命中页面的标题。
	Title string
	// URL 是命中页面的地址。
	URL string
	// Summary 是上游返回的结果摘要正文。
	Summary string
}

// ServerToolResult 是服务端托管工具的执行结果内容块（响应方向专属），
// 与同一消息内的 Server ToolCall 按 ToolCallID 配对。
type ServerToolResult struct {
	// ToolCallID 是本结果所对应的托管工具调用标识。
	ToolCallID string
	// ToolName 是托管工具名（如 web_search），供编码器选择结果块形态。
	ToolName string
	// Results 是结构化的搜索命中列表；当前仅搜索类托管工具填充。
	Results []WebSearchResult
	// Text 是结果的可读正文（上游合成的摘要），回放与兜底渲染共用。
	Text string
	// IsError 表示托管执行失败（搜索 RPC 失败、参数缺失等）。
	IsError bool
	// ErrorCode 是失败时的稳定错误码（供 anthropic
	// web_search_tool_result_error 形态使用）。
	ErrorCode string
}

// ContentType 返回服务端工具结果内容类型。
func (ServerToolResult) ContentType() ContentType { return ContentTypeServerToolResult }

// Validate 检查服务端工具结果块。
func (result ServerToolResult) Validate() error {
	if result.ToolCallID == "" {
		return errors.New("server tool result call ID is required")
	}
	if result.ToolName == "" {
		return errors.New("server tool result tool name is required")
	}
	return nil
}

// ToolDefinition 定义模型可以调用的一个工具。
type ToolDefinition struct {
	// Name 是工具的稳定名称。
	Name string
	// Description 是提供给模型的工具用途说明。
	Description string
	// InputSchema 是描述工具输入对象的 JSON Schema。
	InputSchema json.RawMessage
	// Custom 表示客户端按 freeform/custom 语义声明的工具（Codex apply_patch）：
	// 参数体是原文而非 JSON。上游 is_custom_tool 声明通道实测确定性 unknown，
	// 这类工具在 wire 上包装成单字符串参数的 function 声明（InputSchema 即
	// 包装 schema），响应侧按此标记把 {"input":"<原文>"} 解包回原文。
	Custom bool
	// Server 表示这是服务端托管语义的声明（OpenAI {"type":"web_search"}）：
	// 客户端期待代理/上游执行而非本地执行。wire 上按普通 function 声明
	// 下发（诱饵 schema 驱动模型表达调用意图），响应侧由适配器代执行。
	Server bool
	// 以下为上游 ChatToolDefinition 实测接受的可选透传位（2026-09-15
	// 字段二分确认；is_custom_tool/computer_use_config 同批实测被拒，
	// 永远不透传）。
	// Strict 对应上游 strict。
	Strict bool
	// ReadOnlyHint 对应上游 read_only_hint（Anthropic annotations 直传）。
	ReadOnlyHint bool
	// ServerName 对应上游 server_name（MCP 归属标记）。
	ServerName string
	// AttributionFieldNames 对应上游 attribution_field_names。
	AttributionFieldNames []string
}

// toolNameCharset 是上游实测接受的工具名字符集（a.b、mcp::x、中文名
// 均被拒且只回模糊 internal error）。本地校验把这类失败变成可读的 400。
var toolNameCharset = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Validate 检查工具定义。
func (tool ToolDefinition) Validate() error {
	if tool.Name == "" {
		return errors.New("tool name is required")
	}
	if !toolNameCharset.MatchString(tool.Name) {
		return fmt.Errorf("tool name %q contains characters outside [A-Za-z0-9_-] which upstream rejects", tool.Name)
	}
	if !IsJSONObject(tool.InputSchema) {
		return errors.New("tool input schema must be a JSON object")
	}
	return nil
}

// Validate 检查完整请求上下文。
func (request RequestMessages) Validate() error {
	for index, message := range request.Messages {
		if message == nil {
			return fmt.Errorf("message %d is nil", index)
		}
		if err := message.Validate(); err != nil {
			return fmt.Errorf("message %d (%s): %w", index, message.Role(), err)
		}
	}
	for index, tool := range request.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("tool %d: %w", index, err)
		}
	}
	if request.ToolChoice != nil {
		switch request.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
		case ToolChoiceNamed:
			if request.ToolChoice.ToolName == "" {
				return errors.New("tool_choice named mode requires a tool name")
			}
		default:
			return fmt.Errorf("invalid tool_choice mode %q", request.ToolChoice.Mode)
		}
	}
	return nil
}

// DemoteOrphanToolResults 把前面没有可消化 call 的孤儿 ToolResultMessage
// 原位降级为 UserMessage。上游配对是位置性的：call→result 按序消化、
// 不校验 tool_call_id——「id 指向不存在 call 但前面还有未消化 call」的
// 结果上游照常接受；只有先于一切 call 出现（或 call 已被更早的结果消化完）
// 的结果才被拒，降级保住结果内容让整单可继续。
// 缺失调用 id（ToolCallID 为空）的结果 wire 上无法携带配对键，一并降级——
// 留着也过不了 Validate 的必填约束。必须在 Validate 之前调用。
// 每处降级在 Dropped 留 missing_tool_call_id / unmatched_tool_call_id:<id> 标记。
func (request *RequestMessages) DemoteOrphanToolResults() {
	// pending 计「尚未被 result 消化」的前置 call 数：每个保留的 result
	// 按到达顺序消化一个，消化完再来的 result 才是孤儿。
	pending := 0
	for index, message := range request.Messages {
		switch message := message.(type) {
		case AssistantMessage:
			for _, block := range message.Content {
				if _, ok := block.(ToolCall); ok {
					pending++
				}
			}
		case ToolResultMessage:
			if message.ToolCallID != "" && pending > 0 {
				pending--
				continue
			}
			if message.ToolCallID == "" {
				request.Dropped = append(request.Dropped, "missing_tool_call_id")
			} else {
				request.Dropped = append(request.Dropped, "unmatched_tool_call_id:"+message.ToolCallID)
			}
			slog.Warn("demoted orphan tool result to user text", "tool_call_id", message.ToolCallID)
			// 前缀块带 \n：wire 投影把多条 text 内容块直接拼接，
			// 拆成两块才有「标记行 + 原文」的分行效果。
			request.Messages[index] = UserMessage{
				Content:     append([]Content{TextContent{Text: "[tool result, original call lost]\n"}}, message.Content...),
				TimestampMS: message.TimestampMS,
			}
		}
	}
}

// MergeAdjacentAssistantTurns 合并连续的 AssistantMessage：部分客户端历史
// 把一个模型回合铺平成多条相邻 assistant 消息，逐条放行会让 wire 上产生
// 假的回合边界、抬高宣告处 EOS 概率（issue #2；机制见
// notes/archive/2026-09-12-premature-endturn.md）。连续 assistant 消息必属
// 同一回合——回合边界永远由 user/tool_result 消息分隔。
func (request *RequestMessages) MergeAdjacentAssistantTurns() {
	messages := request.Messages
	merged := make([]Message, 0, len(messages))
	for _, message := range messages {
		assistant, ok := message.(AssistantMessage)
		if !ok || len(merged) == 0 {
			merged = append(merged, message)
			continue
		}
		last, ok := merged[len(merged)-1].(AssistantMessage)
		if !ok {
			merged = append(merged, message)
			continue
		}
		// 两段相邻文本之间补换行：convertMessage 对多块 TextContent 无分隔
		// 直连，不补会把回合内两条 message 的正文粘连。拼装走新切片——
		// append 进 last.Content 的备用 cap 可能写进与 assistant.Content
		// 共享的底层数组（解码器子切片），先污染后复制。
		content := make([]Content, 0, len(last.Content)+1+len(assistant.Content))
		content = append(content, last.Content...)
		if len(last.Content) > 0 && len(assistant.Content) > 0 {
			_, prevText := last.Content[len(last.Content)-1].(TextContent)
			_, nextText := assistant.Content[0].(TextContent)
			if prevText && nextText {
				content = append(content, TextContent{Text: "\n"})
			}
		}
		last.Content = append(content, assistant.Content...)
		if assistant.OutputID != "" {
			last.OutputID = assistant.OutputID
		}
		// 回合如何结束由末段说了算——前段的 stop/length 是铺平留下的
		// 中途状态；末段带 ToolCall 时归 toolUse。Diagnostics 是累计
		// 记录，两段全保留。
		last.StopReason = assistant.StopReason
		last.Diagnostics = slices.Concat(last.Diagnostics, assistant.Diagnostics)
		for _, block := range assistant.Content {
			if _, isCall := block.(ToolCall); isCall {
				last.StopReason = StopReasonToolUse
				break
			}
		}
		merged[len(merged)-1] = last
	}
	request.Messages = merged
}

// validateContent 检查内容块非空、类型在白名单内且各自合法。allowed 只有
// 两三个枚举值，线性比较替代逐消息建 map——几百条历史消息就是几百次小
// 分配，全在每请求热路径上。
func validateContent(content []Content, allowed ...ContentType) error {
	for index, block := range content {
		if block == nil {
			return fmt.Errorf("content block %d is nil", index)
		}
		contentType := block.ContentType()
		if !slices.Contains(allowed, contentType) {
			return fmt.Errorf("content block %d has disallowed type %q", index, contentType)
		}
		if err := block.Validate(); err != nil {
			return fmt.Errorf("content block %d (%s): %w", index, contentType, err)
		}
	}
	return nil
}

// IsJSONObject 判定 value 是否为 JSON 对象（{} 含）。首字节预筛 + json.Valid
// 扫描，不为建树分配——非对象/非法文本/null 均返回 false。
// 各协议前端与适配器共用的参数体检定。
func IsJSONObject(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}
