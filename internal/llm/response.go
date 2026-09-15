// 本文件定义助手聚合响应、用量、诊断信息和增量响应事件。
//
// agent loop 的语义等价，不是 Provider 响应结构等价
package llm

import (
	"encoding/json"
	"errors"
	"fmt"
)

// StopReason 表示模型生成停止的原因。
type StopReason string

const (
	StopReasonPending StopReason = "pending"
	StopReasonStop    StopReason = "stop"
	// StopReasonStopSequence 表示生成被客户端提供的停止序列截断。
	// Devin 上游不执行 stop_patterns，由适配器在解码层本地截断。
	StopReasonStopSequence StopReason = "stopSequence"
	StopReasonLength       StopReason = "length"
	StopReasonToolUse      StopReason = "toolUse"
	// StopReasonContentFilter 表示上游内容过滤器结束了生成。
	StopReasonContentFilter StopReason = "contentFilter"
	StopReasonError         StopReason = "error"
	StopReasonAborted       StopReason = "aborted"
)

// AssistantMessage 表示供应商无关的助手消息，也是最终响应消息。
type AssistantMessage struct {
	// Content 是助手生成的文字、思考和工具调用内容块。
	Content []Content
	// API 是生成消息所使用的上游 API 协议标识。
	API string
	// Provider 是生成消息的模型供应商标识。
	Provider string
	// Model 是请求时选择的模型标识。
	Model string
	// ResponseModel 是供应商在响应中返回的实际模型标识。
	ResponseModel string
	// ResponseID 是供应商分配的响应标识，可用于延续会话或诊断。
	ResponseID string
	// OutputID 是供应商分配的 output item 标识（Devin 上游 outputId，
	// OpenAI 形态是 msg_*）。重放历史时随该消息原样回传 wire
	// output_id——跨 provider 的 output item 身份锚点。
	OutputID string
	// UpstreamRequestID 是上游服务为本次调用分配的追踪标识
	//（Devin Connect 的 request_id），报障时可直接提供给上游。
	UpstreamRequestID string
	// Diagnostics 是转换或流式处理过程中收集的非主响应诊断信息。
	Diagnostics []AssistantMessageDiagnostic
	// Usage 是本条响应累计的 token 与费用用量。
	Usage Usage
	// StopReason 是当前或最终生成状态。
	StopReason StopReason
	// StopSequence 是 StopReasonStopSequence 时实际命中的停止序列。
	StopSequence string
	// ErrorMessage 是生成失败或中止时的可读错误信息。
	ErrorMessage string
	// Failure 是生产侧携带的分类记录；为 nil 时消费方按 ErrorMessage
	// 文本兜底分类（见 FailureOf）。
	Failure *Failure
	// DebugRef 是本代理侧的请求日志引用（调试目录名），由 app 层在错误
	// 事件下发前注入——客户端/agent 可凭它直接定位完整证据链。
	DebugRef string
	// TimestampMS 是创建消息时的 Unix 毫秒时间戳。
	TimestampMS int64
}

// Role 返回助手角色。
func (AssistantMessage) Role() MessageRole { return MessageRoleAssistant }

// Validate 检查助手消息。
func (message AssistantMessage) Validate() error {
	if err := validateContent(message.Content, ContentTypeText, ContentTypeThinking, ContentTypeToolCall, ContentTypeServerToolResult); err != nil {
		return err
	}
	if message.StopReason != "" && !message.StopReason.valid() {
		return fmt.Errorf("invalid stop reason %q", message.StopReason)
	}
	return message.Usage.Validate()
}

// Usage 保存一次助手响应或工具执行的累计用量。
type Usage struct {
	// Input 是输入 token 数。
	Input int64
	// Output 是输出 token 数。
	Output int64
	// CacheRead 是从提示缓存中读取的 token 数。
	CacheRead int64
	// CacheWrite 是写入提示缓存的 token 数。
	CacheWrite int64
	// Reasoning 是输出 token 中属于推理的子集；nil 表示供应商未提供。
	// 当前无生产者：Devin usage 不拆分推理 token，为报告
	// reasoning_tokens 的上游预留；下游编码器（OpenAI usage 输出、
	// index 索引列）已就位，生产者接上即通。
	Reasoning *int64
	// TotalTokens 是供应商报告或适配器计算的总 token 数。
	TotalTokens int64
	// Costs 是上游帧报告的权威计费读数；nil 表示上游未上报。
	Costs *UpstreamCosts
}

// UpstreamCosts 是上游计费读数（Devin GetChatMessageResponse 帧上的
// credit_cost 系字段）。CreditCost 是单请求口径的可加总成本，聚合层
// 对它求和；Committed* 系是账户侧快照/累计读数，逐请求保留原值，
// 跨请求求和没有语义。
type UpstreamCosts struct {
	CreditCost                    int64
	CommittedCreditCost           int64
	CommittedAcuCost              float64
	CommittedQuotaCostBasisPoints int64
	CommittedOverageCostCents     int64
}

// Validate 检查用量字段均为非负值。
func (usage Usage) Validate() error {
	if usage.Input < 0 || usage.Output < 0 || usage.CacheRead < 0 || usage.CacheWrite < 0 || usage.TotalTokens < 0 {
		return errors.New("usage values cannot be negative")
	}
	if usage.Reasoning != nil && *usage.Reasoning < 0 {
		return errors.New("reasoning usage cannot be negative")
	}
	return nil
}

// AssistantMessageDiagnostic 保存不改变主响应结果的诊断记录。
type AssistantMessageDiagnostic struct {
	// Type 是诊断事件的稳定类型标识。
	Type string
	// TimestampMS 是产生诊断时的 Unix 毫秒时间戳。
	TimestampMS int64
	// Details 是可选的结构化诊断详情。
	Details json.RawMessage
}

// ResponseEventType 标识响应流中的增量事件种类。
type ResponseEventType string

const (
	ResponseEventStart         ResponseEventType = "start"
	ResponseEventTextStart     ResponseEventType = "text_start"
	ResponseEventTextDelta     ResponseEventType = "text_delta"
	ResponseEventTextEnd       ResponseEventType = "text_end"
	ResponseEventThinkingStart ResponseEventType = "thinking_start"
	ResponseEventThinkingDelta ResponseEventType = "thinking_delta"
	ResponseEventThinkingEnd   ResponseEventType = "thinking_end"
	// ResponseEventThinkingSignature 是思考块结束后才到达的签名增量
	//（Devin 上游把签名作为尾随帧发送）。ContentIndex 指向已结束块。
	ResponseEventThinkingSignature ResponseEventType = "thinking_signature"
	ResponseEventToolCallStart     ResponseEventType = "toolcall_start"
	ResponseEventToolCallDelta     ResponseEventType = "toolcall_delta"
	ResponseEventToolCallEnd       ResponseEventType = "toolcall_end"
	// ResponseEventServerToolResult 是服务端托管工具结果到达事件：
	// 结果块已按 ContentIndex 追加进 Partial，编码器据此把对应的
	// 托管调用项（server_tool_use / web_search_call）渲染完结。
	ResponseEventServerToolResult ResponseEventType = "server_tool_result"
	ResponseEventDone             ResponseEventType = "done"
	ResponseEventError            ResponseEventType = "error"
)

// ResponseEvent 是供应商无关的助手响应增量事件。
type ResponseEvent struct {
	// Type 是本事件的种类。
	Type ResponseEventType
	// ContentIndex 是本事件对应的助手内容块下标。
	ContentIndex int
	// Delta 是本次新增的文字、思考或尚未完成的工具参数 JSON 片段。
	Delta string
	// Content 是文字或思考内容块结束时的完整内容。
	Content string
	// Partial 是处理本事件后累计形成的助手消息，包含当前累计用量。
	Partial *AssistantMessage
	// ToolCallID 是工具调用开始和参数增量事件关联的稳定调用标识。
	ToolCallID string
	// ToolName 是工具调用开始事件声明的工具名称。
	ToolName string
	// ToolCall 是工具调用结束时已经解析完成的调用对象。
	ToolCall *ToolCall
	// ServerResult 是服务端托管工具的执行结果（ResponseEventServerToolResult）。
	ServerResult *ServerToolResult
	// Reason 是完成或失败事件的停止原因。
	Reason StopReason
	// Message 是正常完成时的最终助手消息。
	Message *AssistantMessage
	// Error 是失败或中止时的最终错误助手消息。
	Error *AssistantMessage
}

// Validate 检查响应事件与其事件种类所需字段一致。
func (event ResponseEvent) Validate() error {
	switch event.Type {
	case ResponseEventStart:
		return requirePartial(event)
	case ResponseEventTextStart, ResponseEventThinkingStart:
		return requireIndexedPartial(event)
	case ResponseEventToolCallStart:
		if err := requireIndexedPartial(event); err != nil {
			return err
		}
		if event.ToolCallID == "" {
			return errors.New("tool call start event requires a tool call ID")
		}
		if event.ToolName == "" {
			return errors.New("tool call start event requires a tool name")
		}
		return nil
	case ResponseEventTextDelta, ResponseEventThinkingDelta, ResponseEventThinkingSignature:
		if err := requireIndexedPartial(event); err != nil {
			return err
		}
		if event.Delta == "" {
			return errors.New("delta event requires a delta")
		}
		return nil
	case ResponseEventToolCallDelta:
		if err := requireIndexedPartial(event); err != nil {
			return err
		}
		if event.ToolCallID == "" {
			return errors.New("tool call delta event requires a tool call ID")
		}
		return nil
	case ResponseEventTextEnd, ResponseEventThinkingEnd:
		return requireIndexedPartial(event)
	case ResponseEventToolCallEnd:
		if err := requireIndexedPartial(event); err != nil {
			return err
		}
		if event.ToolCall == nil {
			return errors.New("tool call end event requires a tool call")
		}
		return event.ToolCall.Validate()
	case ResponseEventServerToolResult:
		if err := requireIndexedPartial(event); err != nil {
			return err
		}
		if event.ServerResult == nil {
			return errors.New("server tool result event requires a result")
		}
		return event.ServerResult.Validate()
	case ResponseEventDone:
		switch event.Reason {
		case StopReasonStop, StopReasonStopSequence, StopReasonLength, StopReasonToolUse, StopReasonContentFilter:
		default:
			return fmt.Errorf("invalid done reason %q", event.Reason)
		}
		if event.Message == nil {
			return errors.New("done event requires a final message")
		}
		return event.Message.Validate()
	case ResponseEventError:
		if event.Reason != StopReasonError && event.Reason != StopReasonAborted {
			return fmt.Errorf("invalid error reason %q", event.Reason)
		}
		if event.Error == nil {
			return errors.New("error event requires a final error message")
		}
		return event.Error.Validate()
	default:
		return fmt.Errorf("unknown response event type %q", event.Type)
	}
}

// requirePartial 校验事件携带 partial 消息且其自身合法。
func requirePartial(event ResponseEvent) error {
	if event.Partial == nil {
		return fmt.Errorf("%s event requires a partial message", event.Type)
	}
	return event.Partial.Validate()
}

// requireIndexedPartial 只检查 ContentIndex 合法与 Partial 在场，不深校验消息体：
// Partial 是同一累计对象逐帧复用，逐帧走完整 Validate 是 O(帧数×内容块) 的浪费；
// 深度契约由 start/done/error 边界事件覆盖，那里才是编码器读取完整语义的位置。
func requireIndexedPartial(event ResponseEvent) error {
	if event.ContentIndex < 0 {
		return fmt.Errorf("%s event requires a non-negative content index", event.Type)
	}
	if event.Partial == nil {
		return fmt.Errorf("%s event requires a partial message", event.Type)
	}
	return nil
}

// valid 报告停止原因是否为已定义枚举值。
func (reason StopReason) valid() bool {
	switch reason {
	case StopReasonPending, StopReasonStop, StopReasonStopSequence, StopReasonLength,
		StopReasonToolUse, StopReasonContentFilter, StopReasonError, StopReasonAborted:
		return true
	default:
		return false
	}
}
