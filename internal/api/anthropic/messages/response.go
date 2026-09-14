// 本文件定义 Anthropic Messages 最终 JSON 和 SSE 事件编码。
package messages

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

// StreamEncoder 保存一次 Anthropic Messages 流的协议状态。
type StreamEncoder struct {
	model     string
	messageID string
	finished  bool
	blocks    []*contentBlockState
	usage     llm.Usage
}

type contentBlockState struct {
	index     int
	kind      string
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	toolID    string
	toolName  string
	// pendingSig 表示思考块正文已结束但尚未发出 content_block_stop，
	// 等待可能尾随到达的签名帧，避免签名落成独立的畸形思考块。
	pendingSig bool
	// redacted 表示该思考块正文被上游隐藏（ThinkingRedacted），
	// 收尾时应发 redacted_thinking 块而非 thinking 块。
	redacted bool
}

// NewStreamEncoder 为一次 Anthropic Messages 流创建编码状态。
func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{
		model:     model,
		messageID: randid.Prefixed("msg_"),
	}
}

// EncodeResponse 把最终助手消息编码为非流式 Anthropic Messages JSON。
func EncodeResponse(message *llm.AssistantMessage) ([]byte, error) {
	if message == nil {
		return nil, errors.New("response message is nil")
	}
	model := message.ResponseModel
	if model == "" {
		model = message.Model
	}
	if model == "" {
		model = "claude"
	}
	response := map[string]any{
		"id":          randid.Prefixed("msg_"),
		"type":        "message",
		"role":        "assistant",
		"content":     messageToAnthropic(message),
		"model":       model,
		"stop_reason": anthropicStopReason(message.StopReason),
		"usage":       anthropicUsage(message.Usage),
	}
	if message.StopSequence != "" {
		response["stop_sequence"] = message.StopSequence
	}
	return json.Marshal(response)
}

// Encode 把一个中间响应事件展开为有序 Anthropic SSE 事件。
func (encoder *StreamEncoder) Encode(event llm.ResponseEvent) ([]SSEEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("validate response event: %w", err)
	}
	if encoder.finished {
		return nil, errors.New("anthropic message stream is already done")
	}
	switch event.Type {
	case llm.ResponseEventStart:
		return encoder.start(event), nil
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
		return encoder.thinkingSignature(event)
	case llm.ResponseEventToolCallStart:
		return encoder.startToolUse(event), nil
	case llm.ResponseEventToolCallDelta:
		return encoder.toolUseDelta(event)
	case llm.ResponseEventToolCallEnd:
		return encoder.endToolUse(event)
	case llm.ResponseEventDone:
		return encoder.finish(event), nil
	case llm.ResponseEventError:
		return encoder.failed(event), nil
	default:
		return nil, fmt.Errorf("unsupported response event type %q", event.Type)
	}
}

// start 发 message_start 帧，携带首个 partial 的 usage 快照。
func (encoder *StreamEncoder) start(event llm.ResponseEvent) []SSEEvent {
	// start 事件的契约字段是 Partial（requirePartial）；Message 只属于
	// done——读错字段会让 message_start 的 usage 恒为零。
	encoder.usage = event.Partial.Usage
	return []SSEEvent{encoder.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":          encoder.messageID,
			"type":        "message",
			"role":        "assistant",
			"content":     []any{},
			"model":       encoder.model,
			"stop_reason": nil,
			"usage":       anthropicUsage(encoder.usage),
		},
	})}
}

// startText 登记文字块状态并发 content_block_start。
func (encoder *StreamEncoder) startText(event llm.ResponseEvent) []SSEEvent {
	state := &contentBlockState{index: event.ContentIndex, kind: "text"}
	encoder.blocks = append(encoder.blocks, state)
	return []SSEEvent{encoder.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         event.ContentIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})}
}

// textDelta 在缺 text_start 前置时显式报错：解码器契约保证 start 先于
// delta，缺失即解码器 bug——静默丢弃会把错位序列伪装成正常流。
func (encoder *StreamEncoder) textDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "text")
	if state == nil {
		return nil, fmt.Errorf("text delta at content index %d without text_start", event.ContentIndex)
	}
	state.text.WriteString(event.Delta)
	return []SSEEvent{encoder.emitBlockDelta(event.ContentIndex, blockDelta{
		Type: "text_delta", Text: event.Delta,
	})}, nil
}

// endText 发 content_block_stop，正文优先取 end 事件携带的完整内容。
func (encoder *StreamEncoder) endText(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "text")
	if state == nil {
		return nil, fmt.Errorf("text end at content index %d without text_start", event.ContentIndex)
	}
	text := event.Content
	if text == "" {
		text = state.text.String()
	}
	return []SSEEvent{encoder.event("content_block_stop", map[string]any{
		"type":          "content_block_stop",
		"index":         event.ContentIndex,
		"content_block": map[string]any{"type": "text", "text": text},
	})}, nil
}

// startThinking 登记思考块状态并发 thinking 类型的 content_block_start。
func (encoder *StreamEncoder) startThinking(event llm.ResponseEvent) []SSEEvent {
	state := &contentBlockState{index: event.ContentIndex, kind: "thinking"}
	if thinking, ok := thinkingAt(event.Partial, event.ContentIndex); ok && thinking.Redacted {
		state.redacted = true
	}
	encoder.blocks = append(encoder.blocks, state)
	return []SSEEvent{encoder.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         event.ContentIndex,
		"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
	})}
}

// thinkingDelta 发 thinking_delta 增量；redacted 块只累计正文不外发。
func (encoder *StreamEncoder) thinkingDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "thinking")
	if state == nil {
		return nil, fmt.Errorf("thinking delta at content index %d without thinking_start", event.ContentIndex)
	}
	state.thinking.WriteString(event.Delta)
	if state.redacted {
		// 隐藏思考不应把增量正文发出去（上游也不会给正文，但 belt-and-suspenders）。
		return nil, nil
	}
	return []SSEEvent{encoder.emitBlockDelta(event.ContentIndex, blockDelta{
		Type: "thinking_delta", Thinking: event.Delta,
	})}, nil
}

// endThinking 收尾思考块：有签名即发 content_block_stop，否则挂起等签名。
func (encoder *StreamEncoder) endThinking(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "thinking")
	if state == nil {
		return nil, fmt.Errorf("thinking end at content index %d without thinking_start", event.ContentIndex)
	}
	thinking := event.Content
	if thinking == "" {
		thinking = state.thinking.String()
	}
	state.thinking.Reset()
	state.thinking.WriteString(thinking)
	if t, ok := thinkingAt(event.Partial, event.ContentIndex); ok {
		state.signature.WriteString(t.ThinkingSignature)
		state.redacted = state.redacted || t.Redacted
	}
	// 上游把签名作为正文之后的尾随帧发送：尚无签名时推迟
	// content_block_stop，待 signature 事件或下一事件再收尾。
	if state.signature.Len() == 0 {
		state.pendingSig = true
		return nil, nil
	}
	return []SSEEvent{encoder.stopThinking(state)}, nil
}

// thinkingSignature 处理尾随签名帧：挂起中的块补 signature_delta 并收尾。
func (encoder *StreamEncoder) thinkingSignature(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "thinking")
	if state == nil {
		return nil, fmt.Errorf("thinking signature at content index %d without thinking_start", event.ContentIndex)
	}
	state.signature.WriteString(event.Delta)
	if !state.pendingSig {
		// 签名帧晚于 content_block_stop 到达属可容忍失序（块已收尾，
		// 增量无处可发）——与块不存在（解码器 bug）区分开。
		return nil, nil
	}
	state.pendingSig = false
	if state.redacted {
		// redacted 块的 data 只在收尾的 content_block_stop 里整体下发，
		// 没有 signature_delta 这种增量形态。
		return []SSEEvent{encoder.stopThinking(state)}, nil
	}
	return []SSEEvent{
		encoder.emitBlockDelta(state.index, blockDelta{
			Type: "signature_delta", Signature: event.Delta,
		}),
		encoder.stopThinking(state),
	}, nil
}

// flushPendingThinking 在流终止（finish/failed）前补发挂起的思考块收尾，
// 上游没有尾随签名时保证块仍按序正常关闭。签名帧可能隔着后续内容块
// 才到（实测 thinking_end → toolcall_* → signature），中途不调用以免
// 提前关块导致迟到签名被静默丢弃。
func (encoder *StreamEncoder) flushPendingThinking() []SSEEvent {
	var events []SSEEvent
	for _, state := range encoder.blocks {
		if state.pendingSig {
			state.pendingSig = false
			events = append(events, encoder.stopThinking(state))
		}
	}
	return events
}

// stopThinking 发思考块的 content_block_stop，块体按 redacted 形态生成。
func (encoder *StreamEncoder) stopThinking(state *contentBlockState) SSEEvent {
	return encoder.event("content_block_stop", map[string]any{
		"type":          "content_block_stop",
		"index":         state.index,
		"content_block": thinkingBlock(state),
	})
}

// thinkingBlock 生成思考块的最终形态：上游标记隐藏的思考按 Anthropic
// redacted_thinking 块发出（data 即上游密封签名），正文不落盘不外发。
func thinkingBlock(state *contentBlockState) map[string]any {
	if state.redacted {
		return map[string]any{"type": "redacted_thinking", "data": state.signature.String()}
	}
	block := map[string]any{"type": "thinking", "thinking": state.thinking.String()}
	if sig := state.signature.String(); sig != "" {
		block["signature"] = sig
	}
	return block
}

// startToolUse 登记工具块状态并发 tool_use 类型的 content_block_start。
func (encoder *StreamEncoder) startToolUse(event llm.ResponseEvent) []SSEEvent {
	state := &contentBlockState{index: event.ContentIndex, kind: "tool_use", toolID: event.ToolCallID, toolName: event.ToolName}
	encoder.blocks = append(encoder.blocks, state)
	return []SSEEvent{encoder.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         event.ContentIndex,
		"content_block": map[string]any{"type": "tool_use", "id": event.ToolCallID, "name": event.ToolName, "input": map[string]any{}},
	})}
}

// toolUseDelta 把参数增量发为 input_json_delta。
func (encoder *StreamEncoder) toolUseDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "tool_use")
	if state == nil {
		return nil, fmt.Errorf("tool use delta at content index %d without toolcall_start", event.ContentIndex)
	}
	return []SSEEvent{encoder.emitBlockDelta(event.ContentIndex, blockDelta{
		Type: "input_json_delta", PartialJSON: event.Delta,
	})}, nil
}

// endToolUse 发工具块的 content_block_stop，input 取 end 事件携带的完整调用。
func (encoder *StreamEncoder) endToolUse(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "tool_use")
	if state == nil {
		return nil, fmt.Errorf("tool use end at content index %d without toolcall_start", event.ContentIndex)
	}
	// event.Validate 已保证 ToolCall 在场且非 Custom 时 Arguments 是 JSON 对象。
	state.toolID = event.ToolCall.ID
	state.toolName = event.ToolCall.Name
	return []SSEEvent{encoder.event("content_block_stop", map[string]any{
		"type":          "content_block_stop",
		"index":         event.ContentIndex,
		"content_block": map[string]any{"type": "tool_use", "id": state.toolID, "name": state.toolName, "input": anthropicToolInput(*event.ToolCall)},
	})}, nil
}

// anthropicToolInput 把工具调用参数转成 Anthropic input 对象。Custom 调用的
// 参数体不是 JSON（freeform 补丁原文/回放的畸形参数），而 Anthropic input
// 必须是对象——按上游 custom 工具的 wire 包装形态 {"input":"<原文>"} 下发：
// 客户端回放该 input 后恰好还原成上游期待的单参数包装，原文不丢。
func anthropicToolInput(call llm.ToolCall) any {
	if call.Custom {
		return map[string]any{"input": string(call.Arguments)}
	}
	var parsed any
	if err := json.Unmarshal(call.Arguments, &parsed); err != nil || parsed == nil {
		return map[string]any{}
	}
	return parsed
}

// finish 收尾全部挂起思考块后发 message_delta 与 message_stop。
func (encoder *StreamEncoder) finish(event llm.ResponseEvent) []SSEEvent {
	encoder.finished = true
	if event.Message != nil {
		encoder.usage = event.Message.Usage
	}
	var stopSequence any
	if event.Message != nil && event.Message.StopSequence != "" {
		stopSequence = event.Message.StopSequence
	}
	delta := map[string]any{"stop_reason": anthropicStopReason(event.Reason), "stop_sequence": stopSequence}
	events := encoder.flushPendingThinking()
	events = append(events,
		encoder.event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": delta,
			"usage": anthropicUsage(encoder.usage),
		}),
		encoder.event("message_stop", map[string]any{"type": "message_stop"}),
	)
	return events
}

// failed 收尾挂起思考块后发 Anthropic 形态的 error 事件并关闭流。
func (encoder *StreamEncoder) failed(event llm.ResponseEvent) []SSEEvent {
	encoder.finished = true
	message := "anthropic message stream failed"
	if event.Error != nil && event.Error.ErrorMessage != "" {
		message = event.Error.ErrorMessage
	}
	// 与 chat/responses 面一致：给限流消息补 "try again in Ns" 等待提示。
	message = common.RetryAfterHint(message, time.Now())
	// Anthropic 官方流式错误格式：
	// event: error
	// data: {"type":"error","error":{"type":"...","message":"..."}}
	// 顶层 status 供下游网关按真实 HTTP 语义分类错误，
	// error.code 让上下文超长被识别为请求级问题而非渠道故障。
	errorPayload := common.BuildErrorPayload(message, common.AnthropicErrorType(message), event.Error, false)
	events := encoder.flushPendingThinking()
	return append(events, encoder.event("error", map[string]any{
		"type":   "error",
		"status": common.HTTPStatus(message),
		"error":  errorPayload,
	}))
}

// block 按下标和类型找已登记的内容块。
func (encoder *StreamEncoder) block(index int, kind string) *contentBlockState {
	for _, state := range encoder.blocks {
		if state.index == index && state.kind == kind {
			return state
		}
	}
	return nil
}

// event 把 payload marshal 成一帧 SSE。
func (encoder *StreamEncoder) event(name string, payload map[string]any) SSEEvent {
	data, _ := json.Marshal(payload)
	return SSEEvent{Name: name, Data: data}
}

// blockDelta 覆盖 content_block_delta 的四种增量形态；各形态键位互斥，
// omitempty 保证 wire 键集与原 map 逐字节一致。
type blockDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
}

// blockDeltaEvent 是 content_block_delta 的固定外壳。
type blockDeltaEvent struct {
	Type  string     `json:"type"`
	Index int        `json:"index"`
	Delta blockDelta `json:"delta"`
}

// emitBlockDelta 用 struct 编码最高频的 content_block_delta 帧，
// 省掉每帧 map 反射 marshal。
func (encoder *StreamEncoder) emitBlockDelta(index int, delta blockDelta) SSEEvent {
	data, _ := json.Marshal(blockDeltaEvent{Type: "content_block_delta", Index: index, Delta: delta})
	return SSEEvent{Name: "content_block_delta", Data: data}
}

// thinkingAt 取 partial 消息中指定下标的思考块。
func thinkingAt(message *llm.AssistantMessage, index int) (llm.ThinkingContent, bool) {
	if message == nil || index < 0 || index >= len(message.Content) {
		return llm.ThinkingContent{}, false
	}
	content, ok := message.Content[index].(llm.ThinkingContent)
	return content, ok
}

// messageToAnthropic 把最终消息的内容块转成 Anthropic content 数组。
func messageToAnthropic(message *llm.AssistantMessage) []any {
	var blocks []any
	for _, block := range message.Content {
		switch content := block.(type) {
		case llm.TextContent:
			blocks = append(blocks, map[string]any{"type": "text", "text": content.Text})
		case llm.ThinkingContent:
			if content.Redacted {
				blocks = append(blocks, map[string]any{"type": "redacted_thinking", "data": content.ThinkingSignature})
				continue
			}
			b := map[string]any{"type": "thinking", "thinking": content.Thinking}
			if content.ThinkingSignature != "" {
				b["signature"] = content.ThinkingSignature
			}
			blocks = append(blocks, b)
		case llm.ToolCall:
			blocks = append(blocks, map[string]any{"type": "tool_use", "id": content.ID, "name": content.Name, "input": anthropicToolInput(content)})
		}
	}
	return blocks
}

// anthropicUsage 投影 Anthropic usage 形态。
func anthropicUsage(usage llm.Usage) map[string]any {
	return map[string]any{
		"input_tokens":                usage.Input,
		"output_tokens":               usage.Output,
		"cache_creation_input_tokens": usage.CacheWrite,
		"cache_read_input_tokens":     usage.CacheRead,
	}
}

// anthropicStopReason 映射 Anthropic stop_reason 枚举。
func anthropicStopReason(reason llm.StopReason) any {
	switch reason {
	case llm.StopReasonToolUse:
		return "tool_use"
	case llm.StopReasonLength:
		return "max_tokens"
	case llm.StopReasonStop:
		return "end_turn"
	case llm.StopReasonStopSequence:
		return "stop_sequence"
	case llm.StopReasonContentFilter:
		return "refusal"
	case llm.StopReasonError, llm.StopReasonAborted:
		return "error"
	default:
		return nil
	}
}
