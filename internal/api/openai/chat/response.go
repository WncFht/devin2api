// 本文件定义 OpenAI Chat Completions 最终 JSON 和 SSE chunk 编码。
package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/randid"
)

// SSEEvent 别名共用的事件类型，保留包内引用的可读性。
type SSEEvent = common.SSEEvent

// StreamEncoder 保存一次 Chat Completions 流的协议状态。
type StreamEncoder struct {
	model        string
	responseID   string
	createdAt    int64
	includeUsage bool
	// textStarted/thinkingStarted 按 llm ContentIndex 记录块开闭：不同下标
	// 的同类块可以交错（thinking 块之间夹 text），单 bool 会把第二个块的
	// delta 误判成「未 start」。
	textStarted     map[int]bool
	thinkingStarted map[int]bool
	toolCalls       []*toolCallState
	// toolByContent 按 llm ContentIndex 索引工具状态。ContentIndex 是
	// partial.Content 的全局块下标（text/thinking/toolCall 混排），
	// 与 tool_calls 输出序号 state.index 不是一套编号——查找必须走这张表，
	// 不能拿序号比下标。
	toolByContent map[int]*toolCallState
	finished      bool
	finalUsage    llm.Usage
}

type toolCallState struct {
	index int
	id    string
	name  string
}

// NewStreamEncoder 为一次 Chat Completions 流创建编码状态。
func NewStreamEncoder(model string, includeUsage bool) *StreamEncoder {
	return &StreamEncoder{
		model:           model,
		responseID:      randid.Prefixed("chatcmpl-"),
		createdAt:       time.Now().Unix(),
		includeUsage:    includeUsage,
		textStarted:     map[int]bool{},
		thinkingStarted: map[int]bool{},
		toolByContent:   map[int]*toolCallState{},
	}
}

// EncodeResponse 把最终助手消息编码为非流式 Chat Completions JSON。
// model 是回显给客户端的模型名（请求原文，可能是别名）；为空时
// 回落到上游声明的 actual uid 再到解析后的请求 uid。
func EncodeResponse(message *llm.AssistantMessage, model string) ([]byte, error) {
	if message == nil {
		return nil, errors.New("response message is nil")
	}
	if model == "" {
		model = message.ResponseModel
	}
	if model == "" {
		model = message.Model
	}
	if model == "" {
		model = "devin"
	}
	messageObj := messageToChat(message)
	response := map[string]any{
		"id":      randid.Prefixed("chatcmpl-"),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       messageObj,
			"finish_reason": finishReason(message.StopReason),
		}},
		"usage": chatUsage(message.Usage),
	}
	return json.Marshal(response)
}

// Encode 把一个中间响应事件展开为零个或多个有序 Chat Completions SSE chunk。
func (encoder *StreamEncoder) Encode(event llm.ResponseEvent) ([]SSEEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("validate response event: %w", err)
	}
	if encoder.finished {
		return nil, errors.New("chat completion stream is already done")
	}
	switch event.Type {
	case llm.ResponseEventStart:
		return encoder.start(), nil
	case llm.ResponseEventTextStart:
		return encoder.startText(event), nil
	case llm.ResponseEventTextDelta:
		return encoder.textDelta(event)
	case llm.ResponseEventTextEnd:
		return encoder.endText(event)
	case llm.ResponseEventThinkingStart:
		return encoder.startThinking(event), nil
	case llm.ResponseEventThinkingDelta:
		return encoder.thinkingDelta(event)
	case llm.ResponseEventThinkingEnd:
		return encoder.endThinking(event)
	case llm.ResponseEventThinkingSignature:
		// Chat Completions 没有签名概念，思考签名只影响 Anthropic/Responses 形态。
		return nil, nil
	case llm.ResponseEventToolCallStart:
		return encoder.startToolCall(event), nil
	case llm.ResponseEventToolCallDelta:
		return encoder.toolCallDelta(event)
	case llm.ResponseEventToolCallEnd:
		return encoder.endToolCall(event)
	case llm.ResponseEventDone:
		return encoder.finish(event), nil
	case llm.ResponseEventError:
		return encoder.failed(event), nil
	default:
		panic(fmt.Sprintf("validated event type %q has no encoder arm", event.Type))
	}
}

// start 发流的首个 chunk：只带 role=assistant 的 delta。
func (encoder *StreamEncoder) start() []SSEEvent {
	return []SSEEvent{encoder.chunk([]chatChoice{{
		Delta: chatDelta{Role: "assistant"},
	}}, nil)}
}

// startText 标记文字块已开；Chat 流没有独立的块开始帧。
// 后续所有块级事件（delta/end/signature）都依赖对应 *_start 前置——
// 解码器契约保证该顺序，缺失即解码器 bug，各 handler 显式报错而非
// 自动补或静默丢弃，与另两个协议编码器一致。
func (encoder *StreamEncoder) startText(event llm.ResponseEvent) []SSEEvent {
	encoder.textStarted[event.ContentIndex] = true
	return nil
}

// textDelta 下发一段正文增量。
func (encoder *StreamEncoder) textDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	if !encoder.textStarted[event.ContentIndex] {
		return nil, fmt.Errorf("text delta at content index %d without text_start", event.ContentIndex)
	}
	return []SSEEvent{encoder.chunk([]chatChoice{{
		Delta: chatDelta{Content: event.Delta},
	}}, nil)}, nil
}

// endText 关闭文字块；Chat 流没有块结束帧，仅复位状态。
func (encoder *StreamEncoder) endText(event llm.ResponseEvent) ([]SSEEvent, error) {
	if !encoder.textStarted[event.ContentIndex] {
		return nil, fmt.Errorf("text end at content index %d without text_start", event.ContentIndex)
	}
	delete(encoder.textStarted, event.ContentIndex)
	return nil, nil
}

// startThinking 标记思考块已开。
// OpenAI Chat Completions 没有官方 reasoning 字段。
// 这里参考 DeepSeek 等厂商的约定，用 choices[0].delta.reasoning_content 输出思考。
func (encoder *StreamEncoder) startThinking(event llm.ResponseEvent) []SSEEvent {
	encoder.thinkingStarted[event.ContentIndex] = true
	return nil
}

// thinkingDelta 下发一段思考增量为 reasoning_content。
func (encoder *StreamEncoder) thinkingDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	if !encoder.thinkingStarted[event.ContentIndex] {
		return nil, fmt.Errorf("thinking delta at content index %d without thinking_start", event.ContentIndex)
	}
	return []SSEEvent{encoder.chunk([]chatChoice{{
		Delta: chatDelta{ReasoningContent: event.Delta},
	}}, nil)}, nil
}

// endThinking 关闭思考块，仅复位状态。
func (encoder *StreamEncoder) endThinking(event llm.ResponseEvent) ([]SSEEvent, error) {
	if !encoder.thinkingStarted[event.ContentIndex] {
		return nil, fmt.Errorf("thinking end at content index %d without thinking_start", event.ContentIndex)
	}
	delete(encoder.thinkingStarted, event.ContentIndex)
	return nil, nil
}

// startToolCall 登记工具调用状态并发带 id/name 的 tool_calls 首帧。
func (encoder *StreamEncoder) startToolCall(event llm.ResponseEvent) []SSEEvent {
	state := &toolCallState{index: len(encoder.toolCalls), id: event.ToolCallID, name: event.ToolName}
	encoder.toolCalls = append(encoder.toolCalls, state)
	encoder.toolByContent[event.ContentIndex] = state
	return []SSEEvent{encoder.chunk([]chatChoice{{
		Delta: chatDelta{ToolCalls: []chatToolCall{{
			Index:    state.index,
			ID:       state.id,
			Type:     "function",
			Function: chatToolCallFunction{Name: state.name},
		}}},
	}}, nil)}
}

// toolCallDelta 下发一段工具参数增量。
func (encoder *StreamEncoder) toolCallDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.findTool(event.ToolCallID, event.ContentIndex)
	if state == nil {
		return nil, fmt.Errorf("tool call delta at content index %d (call %q) without toolcall_start", event.ContentIndex, event.ToolCallID)
	}
	return []SSEEvent{encoder.chunk([]chatChoice{{
		Delta: chatDelta{ToolCalls: []chatToolCall{{
			Index:    state.index,
			Function: chatToolCallFunction{Arguments: event.Delta},
		}}},
	}}, nil)}, nil
}

// endToolCall 校验块已开；OpenAI Chat Completions 流式工具调用不输出
// 单独的结束 chunk，finish_reason 会标记结束。
func (encoder *StreamEncoder) endToolCall(event llm.ResponseEvent) ([]SSEEvent, error) {
	if encoder.toolByContent[event.ContentIndex] == nil {
		return nil, fmt.Errorf("tool call end at content index %d without toolcall_start", event.ContentIndex)
	}
	return nil, nil
}

// finish 发 finish_reason chunk、可选 usage chunk 和 [DONE] 终止帧。
func (encoder *StreamEncoder) finish(event llm.ResponseEvent) []SSEEvent {
	encoder.finished = true
	encoder.finalUsage = event.Message.Usage
	reason := finishReason(event.Reason)
	events := []SSEEvent{encoder.chunk([]chatChoice{{FinishReason: reason}}, nil)}
	if encoder.includeUsage {
		events = append(events, encoder.chunk([]chatChoice{}, chatUsage(encoder.finalUsage)))
	}
	events = append(events, SSEEvent{Name: common.SSEDone, Data: []byte(common.SSEDone)})
	return events
}

// failed 发一个带 error 字段的终止 chunk 并关闭流。
func (encoder *StreamEncoder) failed(event llm.ResponseEvent) []SSEEvent {
	encoder.finished = true
	// OpenAI Chat Completions 流式错误没有官方统一格式。
	// 这里生成一个带 error 字段的 chat.completion.chunk，
	// 让 openai-python 等客户端看到 data.error 后抛出异常。
	// 顶层 status 供下游网关按真实 HTTP 语义分类错误。
	errorPayload, status := common.StreamErrorOpenAI(event, "chat completion stream failed")
	data, _ := json.Marshal(map[string]any{
		"id":      encoder.responseID,
		"object":  "chat.completion.chunk",
		"created": encoder.createdAt,
		"model":   encoder.model,
		"choices": []any{},
		"usage":   nil,
		"status":  status,
		"error":   errorPayload,
	})
	// 尾随 [DONE]：缺终止帧时部分客户端把流尾判成传输截断，
	// 而非干净的终态错误。
	return []SSEEvent{{Name: "", Data: data}, {Name: common.SSEDone, Data: []byte(common.SSEDone)}}
}

// findTool 先按供应商调用 id 匹配；id 缺失（上游可不产 id，decoder 另有
// 空 id 兜底路径）时退回 ContentIndex 映射。
func (encoder *StreamEncoder) findTool(id string, contentIndex int) *toolCallState {
	for _, state := range encoder.toolCalls {
		if state.id != "" && state.id == id {
			return state
		}
	}
	return encoder.toolByContent[contentIndex]
}

// chatChunk 是流式 chunk 的固定 envelope：五键整流不变，只有 choices/usage
// 随事件变。struct 编码替代每帧 map marshal（实测 ~2.7x 快、~5.7x 少分配）。
type chatChunk struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   any          `json:"usage"`
}

// 以下类型按 map marshal 的键名字母序声明字段，与旧的 map[string]any
// 编码保持逐字节一致的输出。

// chatChoice 是 choices 数组的单元素形态：finish_reason 恒输出（可为 null），
// index 恒为 0。
type chatChoice struct {
	Delta        chatDelta `json:"delta"`
	FinishReason any       `json:"finish_reason"`
	Index        int       `json:"index"`
}

// chatDelta 是全部 delta 键的并集：每种事件只填其中一键。
type chatDelta struct {
	Content          string         `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	Role             string         `json:"role,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
}

// chatToolCall 是 delta.tool_calls 的元素：start 事件带 id/type/name，
// 参数增量帧只有 function.arguments 与 index。
type chatToolCall struct {
	Function chatToolCallFunction `json:"function"`
	ID       string               `json:"id,omitempty"`
	Index    int                  `json:"index"`
	Type     string               `json:"type,omitempty"`
}

type chatToolCallFunction struct {
	Arguments string `json:"arguments"`
	Name      string `json:"name,omitempty"`
}

// chunk 把 choices/usage 装进固定 envelope marshal 成一帧 SSE。
func (encoder *StreamEncoder) chunk(choices []chatChoice, usage any) SSEEvent {
	data, _ := json.Marshal(chatChunk{
		ID:      encoder.responseID,
		Object:  "chat.completion.chunk",
		Created: encoder.createdAt,
		Model:   encoder.model,
		Choices: choices,
		Usage:   usage,
	})
	return SSEEvent{Name: "", Data: data}
}

// messageToChat 把最终消息投影成 chat message 对象。
func messageToChat(message *llm.AssistantMessage) map[string]any {
	var textParts []string
	var reasoningParts []string
	var toolCalls []any
	for _, block := range message.Content {
		switch content := block.(type) {
		case llm.TextContent:
			textParts = append(textParts, content.Text)
		case llm.ThinkingContent:
			// 非流式模式下把思考单独放到 reasoning_content，正文只放 text。
			reasoningParts = append(reasoningParts, content.Thinking)
		case llm.ToolCall:
			toolCalls = append(toolCalls, map[string]any{
				"id":       content.ID,
				"type":     "function",
				"function": map[string]any{"name": content.Name, "arguments": string(content.Arguments)},
			})
		}
	}
	messageObj := map[string]any{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	if len(reasoningParts) > 0 {
		messageObj["reasoning_content"] = strings.Join(reasoningParts, "")
	}
	if len(toolCalls) > 0 {
		messageObj["tool_calls"] = toolCalls
		// content 与 tool_calls 允许共存：有正文就保留，只有纯调用轮才置 nil。
		if len(textParts) == 0 {
			messageObj["content"] = nil
		}
	}
	return messageObj
}

// chatUsage 投影 Chat Completions usage 形态，含 cache 与 reasoning 明细。
func chatUsage(usage llm.Usage) map[string]any {
	inputTokens, total := common.UsageTotals(usage)
	result := map[string]any{
		"prompt_tokens":     inputTokens,
		"completion_tokens": usage.Output,
		"total_tokens":      total,
		"prompt_tokens_details": map[string]any{
			"cached_tokens":      usage.CacheRead,
			"cache_write_tokens": usage.CacheWrite,
		},
	}
	// Reasoning 为 nil 表示上游未报告推理子集；恒输出 0 会把「未知」
	// 伪造成「无推理」，下游网关据此统计推理占比时会算错。
	if usage.Reasoning != nil {
		result["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": *usage.Reasoning,
		}
	}
	return result
}

// finishReason 映射 Chat Completions finish_reason 枚举。
func finishReason(reason llm.StopReason) any {
	switch reason {
	case llm.StopReasonToolUse:
		return "tool_calls"
	case llm.StopReasonLength:
		return "length"
	case llm.StopReasonStop, llm.StopReasonStopSequence:
		return "stop"
	case llm.StopReasonContentFilter, llm.StopReasonError, llm.StopReasonAborted:
		return "content_filter"
	default:
		return nil
	}
}
