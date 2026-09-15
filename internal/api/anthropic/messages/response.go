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
	signature strings.Builder
	// pendingSig 表示思考块正文已结束但尚未发出 content_block_stop，
	// 等待可能尾随到达的签名帧，避免签名落成独立的畸形思考块。
	pendingSig bool
	// redacted 表示该思考块正文被上游隐藏（ThinkingRedacted），
	// 收尾时应发 redacted_thinking 块而非 thinking 块。
	redacted bool
	// startDeferred 表示开块时就已知 redacted：spec 的
	// redacted_thinking 是 content_block_start 一次性带 data 的完整块，
	// 而密封签名只在收尾才齐，故 start 推迟到收尾随 data 一起发。
	startDeferred bool
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
	return []SSEEvent{encoder.emitBlockDelta(event.ContentIndex, blockDelta{
		Type: "text_delta", Text: event.Delta,
	})}, nil
}

// endText 发 content_block_stop；正文经 text_delta 全部下发完毕，
// spec 的 stop 帧只带 type/index——不再回读 event.Content 补回声。
func (encoder *StreamEncoder) endText(event llm.ResponseEvent) ([]SSEEvent, error) {
	if encoder.block(event.ContentIndex, "text") == nil {
		return nil, fmt.Errorf("text end at content index %d without text_start", event.ContentIndex)
	}
	return []SSEEvent{encoder.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": event.ContentIndex,
	})}, nil
}

// startThinking 登记思考块状态并发 thinking 类型的 content_block_start。
func (encoder *StreamEncoder) startThinking(event llm.ResponseEvent) []SSEEvent {
	state := &contentBlockState{index: event.ContentIndex, kind: "thinking"}
	if thinking, ok := common.ContentAt[llm.ThinkingContent](event.Partial, event.ContentIndex); ok && thinking.Redacted {
		state.redacted = true
	}
	encoder.blocks = append(encoder.blocks, state)
	if state.redacted {
		// spec 的 redacted_thinking 是 start 一次性带 data 的完整块；
		// 此刻就发 {thinking,""} 会让规范客户端看到无载荷的空 thinking
		// 块、且 data 永远没有合法通道下发。推迟 start 到收尾。
		state.startDeferred = true
		return nil
	}
	return []SSEEvent{encoder.event("content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         event.ContentIndex,
		"content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""},
	})}
}

// thinkingDelta 发 thinking_delta 增量；redacted 块正文不外发。
func (encoder *StreamEncoder) thinkingDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "thinking")
	if state == nil {
		return nil, fmt.Errorf("thinking delta at content index %d without thinking_start", event.ContentIndex)
	}
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
	if t, ok := common.ContentAt[llm.ThinkingContent](event.Partial, event.ContentIndex); ok {
		state.signature.WriteString(t.ThinkingSignature)
		state.redacted = state.redacted || t.Redacted
	}
	// 上游把签名作为正文之后的尾随帧发送：尚无签名时推迟
	// content_block_stop，待 signature 事件或下一事件再收尾。
	if state.signature.Len() == 0 {
		state.pendingSig = true
		return nil, nil
	}
	if state.redacted {
		return encoder.stopThinking(state), nil
	}
	// 签名随 thinking_end 一次到齐（含 decodeLateSignature 合成块的
	// Start+End 路径——openai 体制签名是唯一思考产物）：规范客户端只
	// 从 signature_delta 累积签名，直接 stop 等于把签名丢给空气。
	return append([]SSEEvent{
		encoder.emitBlockDelta(state.index, blockDelta{
			Type: "signature_delta", Signature: state.signature.String(),
		}),
	}, encoder.stopThinking(state)...), nil
}

// thinkingSignature 处理尾随签名帧：增量按 signature_delta 下发，
// 块保持挂起、由 flushPendingThinking 统一收尾。上游可把签名拆成
// 多帧（decoder 每帧发一个事件），首个分片就关块会把后续分片丢在
// content_block_stop 之后——客户端只累积到前缀，下轮回放截断签名
// 被上游拒。
func (encoder *StreamEncoder) thinkingSignature(event llm.ResponseEvent) ([]SSEEvent, error) {
	state := encoder.block(event.ContentIndex, "thinking")
	if state == nil {
		return nil, fmt.Errorf("thinking signature at content index %d without thinking_start", event.ContentIndex)
	}
	state.signature.WriteString(event.Delta)
	if state.redacted {
		// redacted 块的 data 没有 signature_delta 增量形态，只能在收尾的
		// content_block_stop 整体下发——分片继续累积，flush 时随 data 走。
		return nil, nil
	}
	return []SSEEvent{
		encoder.emitBlockDelta(state.index, blockDelta{
			Type: "signature_delta", Signature: event.Delta,
		}),
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
			events = append(events, encoder.stopThinking(state)...)
		}
	}
	return events
}

// stopThinking 发思考块的收尾事件。spec 的 stop 帧只带 type/index；
// 唯一的例外是 redacted 思考块——上游密封签名（data）没有对应的
// delta 形态，只能在块边界整体下发：start 被推迟过（开块即知
// redacted）就按 spec 补 start{redacted_thinking,data}+stop；start
// 已按 thinking 发出（redacted 晚到）则 data 内嵌在收尾帧，是仅剩的通道。
func (encoder *StreamEncoder) stopThinking(state *contentBlockState) []SSEEvent {
	if state.redacted && state.signature.Len() > 0 {
		block := map[string]any{"type": "redacted_thinking", "data": state.signature.String()}
		if state.startDeferred {
			return []SSEEvent{
				encoder.event("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         state.index,
					"content_block": block,
				}),
				encoder.event("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": state.index,
				}),
			}
		}
		return []SSEEvent{encoder.event("content_block_stop", map[string]any{
			"type":          "content_block_stop",
			"index":         state.index,
			"content_block": block,
		})}
	}
	if state.startDeferred {
		// start 推迟后签名始终没到：该块在 wire 上从未开启、也没有可
		// 下发的 data——空 data 的 redacted_thinking 是畸形块，整块不发
		// 更合规（未使用的 index 空洞是合法的）。
		return nil
	}
	return []SSEEvent{encoder.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": state.index,
	})}
}

// startToolUse 登记工具块状态并发 tool_use 类型的 content_block_start。
func (encoder *StreamEncoder) startToolUse(event llm.ResponseEvent) []SSEEvent {
	state := &contentBlockState{index: event.ContentIndex, kind: "tool_use"}
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

// endToolUse 发工具块的 content_block_stop；完整 input 已由
// input_json_delta 增量送达，spec 的 stop 帧只带 type/index。
func (encoder *StreamEncoder) endToolUse(event llm.ResponseEvent) ([]SSEEvent, error) {
	if encoder.block(event.ContentIndex, "tool_use") == nil {
		return nil, fmt.Errorf("tool use end at content index %d without toolcall_start", event.ContentIndex)
	}
	return []SSEEvent{encoder.event("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": event.ContentIndex,
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
	encoder.usage = event.Message.Usage
	var stopSequence any
	if event.Message.StopSequence != "" {
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
	// 分类记录随车携带（decoder 产出时已挂）：type/status/retry 全读字段。
	failure := llm.FailureOf(event.Error)
	message := "anthropic message stream failed"
	if failure.Error() != "" {
		// 与 chat/responses 面一致：给限流消息补 "try again in Ns" 等待提示。
		message = common.RetryAfterHint(failure, time.Now())
	}
	// Anthropic 官方流式错误格式：
	// event: error
	// data: {"type":"error","error":{"type":"...","message":"..."}}
	// 顶层 status 供下游网关按真实 HTTP 语义分类错误，
	// error.code 让上下文超长被识别为请求级问题而非渠道故障。
	errorPayload := common.BuildErrorPayload(message, failure, common.AnthropicErrorType(failure), event.Error.DebugRef, false)
	events := encoder.flushPendingThinking()
	return append(events, encoder.event("error", map[string]any{
		"type":   "error",
		"status": common.HTTPStatus(failure),
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
		// "error" 不在 Anthropic stop_reason 枚举内——错误已由 error 事件
		// 承载，stop_reason 按「未正常收尾」回 null 而非编造的枚举值。
		return nil
	default:
		return nil
	}
}
