// 本文件定义 OpenAI Responses 最终 JSON 和带完整 item 生命周期的 typed SSE 编码。
package responses

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/randid"
)

// SSEEvent 别名共用的事件类型，保留包内引用的可读性。
type SSEEvent = common.SSEEvent

// StreamEncoder 保存一次 HTTP Responses 流的协议状态和完整 output items。
type StreamEncoder struct {
	// model 是对外 Responses 请求使用的模型标识。
	model string
	// responseID 是本次 HTTP Response 的稳定 resp_ 标识。
	responseID string
	// createdAt 是 Response 创建时的 Unix 秒时间戳。
	createdAt int64
	// sequenceNumber 是下一个 SSE 事件的连续序号。
	sequenceNumber int64
	// items 按中间内容块下标保存正在生成或已经结束的 output item。
	items map[int]*streamItem
	// output 按 output_index 保存已经结束、可供下一轮重放的 output item。
	output []any
	// started 表示 response.created 和 response.in_progress 已经发出。
	started bool
	// completed 表示终止事件已经发出。
	completed bool
	// outputIDClaimed 标记上游 outputId 已被首个 message item 领走：
	// item id 在一次响应内必须唯一，后续 message 块用合成的 msg_。
	outputIDClaimed bool
}

// streamItem 保存一个 reasoning、function_call 或 message output item 的编码状态。
type streamItem struct {
	// kind 是 Responses output item 的类型。
	kind string
	// id 是 rs_、fc_ 或 msg_ 开头的 item 标识。
	id string
	// outputIndex 是 item 在 Response output 数组中的下标。
	outputIndex int
	// callID 是 function_call 与 function_call_output 关联的业务标识。
	callID string
	// name 是 function_call 的工具名称。
	name string
	// contentIndex 是 message 内 output_text part 的下标。
	contentIndex int
	// value 累计文字、思考摘要或工具参数。
	value strings.Builder
	// encryptedContent 是可重放的思考签名；空值表示供应商未提供。
	encryptedContent string
	// closed 表示 item 已产生 output_item.done。
	closed bool
	// pendingDone 延迟 reasoning 的收尾事件，等待正文之后才到达的签名帧。
	pendingDone bool
	// pendingText 是 reasoning 收尾时要回放的完整摘要文本。
	pendingText string
}

// NewStreamEncoder 为一次 HTTP Responses 请求创建独立的 SSE 编码状态。
func NewStreamEncoder(model string) *StreamEncoder {
	return &StreamEncoder{
		model:      model,
		responseID: randid.Prefixed("resp_"),
		createdAt:  time.Now().Unix(),
		items:      make(map[int]*streamItem),
	}
}

// EncodeResponse 将最终助手消息编码为非流式 Responses JSON 响应。
func EncodeResponse(message *llm.AssistantMessage) ([]byte, error) {
	if message == nil {
		return nil, fmt.Errorf("response message is nil")
	}
	output, err := outputFromMessage(message)
	if err != nil {
		return nil, err
	}
	model := message.ResponseModel
	if model == "" {
		model = message.Model
	}
	responseID := message.ResponseID
	if !strings.HasPrefix(responseID, "resp_") {
		responseID = randid.Prefixed("resp_")
	}
	createdAt := time.UnixMilli(message.TimestampMS).Unix()
	if message.TimestampMS <= 0 {
		createdAt = time.Now().Unix()
	}
	status := responseStatus(message.StopReason)
	response := baseResponse(responseID, model, createdAt, status)
	if status == "completed" {
		response["completed_at"] = time.Now().Unix()
	}
	response["output"] = output
	response["usage"] = responseUsage(message.Usage)
	return json.Marshal(response)
}

// Encode 将一个中间响应事件展开为零个或多个有序 Responses SSE 事件。
func (encoder *StreamEncoder) Encode(event llm.ResponseEvent) ([]SSEEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, fmt.Errorf("validate response event: %w", err)
	}
	if encoder.completed {
		return nil, fmt.Errorf("response stream is already completed")
	}
	// 上游把思考签名作为正文之后的尾随帧发送，且可能隔着整个 toolcall
	// 块才到（实测 thinking_end → toolcall_* → thinking_signature）。中途
	// 不提前补发 reasoning 收尾，把等待窗口保留到流终止；签名事件到达时
	// 由 reasoningSignature 自行收尾。Done 之前兜底关闭，防挂起 item 拦下完成。
	var prefix []SSEEvent
	if event.Type == llm.ResponseEventDone {
		prefix = encoder.flushPendingReasoning()
	}
	var events []SSEEvent
	var err error
	switch event.Type {
	case llm.ResponseEventStart:
		events, err = encoder.start(), nil
	case llm.ResponseEventThinkingStart:
		events, err = encoder.startReasoning(event)
	case llm.ResponseEventThinkingDelta:
		events, err = encoder.reasoningDelta(event)
	case llm.ResponseEventThinkingEnd:
		events, err = encoder.endReasoning(event)
	case llm.ResponseEventThinkingSignature:
		events, err = encoder.reasoningSignature(event)
	case llm.ResponseEventTextStart:
		events, err = encoder.startText(event)
	case llm.ResponseEventTextDelta:
		events, err = encoder.textDelta(event)
	case llm.ResponseEventTextEnd:
		events, err = encoder.endText(event)
	case llm.ResponseEventToolCallStart:
		events, err = encoder.startToolCall(event)
	case llm.ResponseEventToolCallDelta:
		events, err = encoder.toolCallDelta(event)
	case llm.ResponseEventToolCallEnd:
		events, err = encoder.endToolCall(event)
	case llm.ResponseEventDone:
		events, err = encoder.done(event)
	case llm.ResponseEventError:
		events, err = encoder.failed(event), nil
	default:
		return nil, fmt.Errorf("unsupported response event type %q", event.Type)
	}
	if err != nil {
		return nil, err
	}
	return append(prefix, events...), nil
}

// start 发 response.created 与 response.in_progress 两帧开场事件。
func (encoder *StreamEncoder) start() []SSEEvent {
	if encoder.started {
		return nil
	}
	encoder.started = true
	created := baseResponse(encoder.responseID, encoder.model, encoder.createdAt, "in_progress")
	return []SSEEvent{
		encoder.emit("response.created", map[string]any{"response": created}),
		encoder.emit("response.in_progress", map[string]any{"response": created}),
	}
}

// startReasoning 开 reasoning item 并发 output_item.added 与
// reasoning_summary_part.added；openai 型签名用内层真实 rs_* id。
func (encoder *StreamEncoder) startReasoning(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.newItem(event.ContentIndex, "reasoning", "rs")
	if err != nil {
		return nil, err
	}
	if thinking, ok := common.ContentAt[llm.ThinkingContent](event.Partial, event.ContentIndex); ok {
		item.encryptedContent = thinking.ThinkingSignature
		if thinking.SignatureType == "openai" {
			// 签名原文 blob 整体进 encrypted_content（回放时按同一形态
			// 识别），但 item id 用内层真实 rs_*——与上游下发一致。
			if items := common.OpenAIReasoningItems(item.encryptedContent); items != nil && items[0].ID != "" {
				item.id = items[0].ID
			}
		}
	}
	addedItem := map[string]any{"id": item.id, "type": "reasoning", "summary": []any{}}
	if item.encryptedContent != "" {
		addedItem["encrypted_content"] = item.encryptedContent
	}
	return []SSEEvent{
		encoder.emit("response.output_item.added", map[string]any{"output_index": item.outputIndex, "item": addedItem}),
		encoder.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		}),
	}, nil
}

// reasoningDelta 把思考增量发为 reasoning_summary_text.delta。
func (encoder *StreamEncoder) reasoningDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.item(event.ContentIndex, "reasoning")
	if err != nil {
		return nil, err
	}
	item.value.WriteString(event.Delta)
	return []SSEEvent{encoder.emitDelta(deltaEvent{
		Type: "response.reasoning_summary_text.delta", ItemID: item.id, OutputIndex: item.outputIndex,
		SummaryIndex: new(int), Delta: event.Delta,
	})}, nil
}

// endReasoning 收尾 reasoning item：有签名即关项发 done 三帧，
// 否则挂起等待尾随签名帧。
func (encoder *StreamEncoder) endReasoning(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.item(event.ContentIndex, "reasoning")
	if err != nil {
		return nil, err
	}
	text := event.Content
	if text == "" {
		text = item.value.String()
	}
	if thinking, ok := common.ContentAt[llm.ThinkingContent](event.Partial, event.ContentIndex); ok && thinking.ThinkingSignature != "" {
		item.encryptedContent = thinking.ThinkingSignature
	}
	// 思考文本无论是否推迟收尾都要先落进 pendingText：签名与正文同帧
	// 到达时不走 pending 分支，若只在该分支赋值，reasoningDone 发出的
	// summary/done 会带空文本。
	item.pendingText = text
	// 上游把签名作为正文之后的尾随帧发送：尚无签名时推迟收尾事件。
	if item.encryptedContent == "" {
		item.pendingDone = true
		return nil, nil
	}
	return encoder.reasoningDone(item), nil
}

// reasoningDone 发出 reasoning item 的三个收尾事件。
func (encoder *StreamEncoder) reasoningDone(item *streamItem) []SSEEvent {
	item.pendingDone = false
	completedItem := map[string]any{
		"id": item.id, "type": "reasoning",
		"summary": []any{map[string]any{"type": "summary_text", "text": item.pendingText}},
	}
	if item.encryptedContent != "" {
		completedItem["encrypted_content"] = item.encryptedContent
	}
	encoder.closeItem(item, completedItem)
	return []SSEEvent{
		encoder.emit("response.reasoning_summary_text.done", map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0, "text": item.pendingText,
		}),
		encoder.emit("response.reasoning_summary_part.done", map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": item.pendingText},
		}),
		encoder.emit("response.output_item.done", map[string]any{"output_index": item.outputIndex, "item": completedItem}),
	}
}

// reasoningSignature 把尾随签名并入 reasoning item：挂起时补发收尾；
// item 已关闭时（签名随 thinking_end 同帧到达、或兜底 flush 后仍有迟到帧）
// 只补写 completed output 里的 encrypted_content，不再重发事件；
// 下标没有 reasoning item 属解码器 bug（start 先于块事件的契约被破坏），
// 显式报错而非静默丢弃。
func (encoder *StreamEncoder) reasoningSignature(event llm.ResponseEvent) ([]SSEEvent, error) {
	item := encoder.items[event.ContentIndex]
	if item == nil || item.kind != "reasoning" {
		return nil, fmt.Errorf("thinking signature at content index %d without thinking_start", event.ContentIndex)
	}
	item.encryptedContent += event.Delta
	if item.closed {
		if completed, ok := encoder.output[item.outputIndex].(map[string]any); ok {
			completed["encrypted_content"] = item.encryptedContent
		}
		return nil, nil
	}
	if !item.pendingDone {
		return nil, nil
	}
	return encoder.reasoningDone(item), nil
}

// flushPendingReasoning 在流终止（Done）前补发挂起的 reasoning 收尾，
// 上游始终没有尾随签名时保证 item 仍正常关闭。签名帧可能隔着后续
// 内容块才到，中途不调用以免提前关项导致迟到签名无处可落。
func (encoder *StreamEncoder) flushPendingReasoning() []SSEEvent {
	var events []SSEEvent
	for _, index := range slices.Sorted(maps.Keys(encoder.items)) {
		if item := encoder.items[index]; item.pendingDone {
			events = append(events, encoder.reasoningDone(item)...)
		}
	}
	return events
}

// startText 开 message item 并发 output_item.added 与 content_part.added。
func (encoder *StreamEncoder) startText(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.newItem(event.ContentIndex, "message", "msg")
	if err != nil {
		return nil, err
	}
	// 上游 output_id 是 OpenAI 侧 message item 的真实标识（msg_*），
	// 下发同一个 id 让客户端回放的 item 与上游记录对齐。一个上游
	// outputId 只认领一次：同响应内多个 message 块共用会让 output item
	// id 冲突，后续块落回合成的 msg_。
	if event.Partial.OutputID != "" && !encoder.outputIDClaimed {
		item.id = event.Partial.OutputID
		encoder.outputIDClaimed = true
	}
	return []SSEEvent{
		encoder.emit("response.output_item.added", map[string]any{
			"output_index": item.outputIndex,
			"item":         map[string]any{"id": item.id, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		}),
		encoder.emit("response.content_part.added", map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIndex,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}},
		}),
	}, nil
}

// textDelta 把正文增量发为 output_text.delta。
func (encoder *StreamEncoder) textDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.item(event.ContentIndex, "message")
	if err != nil {
		return nil, err
	}
	item.value.WriteString(event.Delta)
	return []SSEEvent{encoder.emitDelta(deltaEvent{
		Type: "response.output_text.delta", ItemID: item.id, OutputIndex: item.outputIndex,
		ContentIndex: &item.contentIndex, Delta: event.Delta, Logprobs: []any{},
	})}, nil
}

// endText 关 message item 并发 output_text.done、content_part.done、
// output_item.done 三帧收尾。
func (encoder *StreamEncoder) endText(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.item(event.ContentIndex, "message")
	if err != nil {
		return nil, err
	}
	text := event.Content
	if text == "" {
		text = item.value.String()
	}
	part := map[string]any{"type": "output_text", "text": text, "annotations": []any{}, "logprobs": []any{}}
	completedItem := map[string]any{
		"id": item.id, "type": "message", "status": "completed", "role": "assistant", "content": []any{part},
	}
	encoder.closeItem(item, completedItem)
	return []SSEEvent{
		encoder.emit("response.output_text.done", map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIndex,
			"text": text, "logprobs": []any{},
		}),
		encoder.emit("response.content_part.done", map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, "content_index": item.contentIndex, "part": part,
		}),
		encoder.emit("response.output_item.done", map[string]any{"output_index": item.outputIndex, "item": completedItem}),
	}, nil
}

// startToolCall 开 function_call/custom_tool_call item 并发 output_item.added。
func (encoder *StreamEncoder) startToolCall(event llm.ResponseEvent) ([]SSEEvent, error) {
	// custom/freeform 调用的参数体不是 JSON（上游 is_custom_tool_call），
	// 按 Responses custom_tool_call item 下发——input 字段而非 arguments。
	kind := "function_call"
	if call, ok := common.ContentAt[llm.ToolCall](event.Partial, event.ContentIndex); ok && call.Custom {
		kind = "custom_tool_call"
	}
	item, err := encoder.newItem(event.ContentIndex, kind, "fc")
	if err != nil {
		return nil, err
	}
	item.callID = event.ToolCallID
	item.name = event.ToolName
	addedItem := map[string]any{
		"id": item.id, "type": kind, "status": "in_progress",
		"call_id": item.callID, "name": item.name,
	}
	if kind == "custom_tool_call" {
		addedItem["input"] = ""
	} else {
		addedItem["arguments"] = ""
	}
	return []SSEEvent{encoder.emit("response.output_item.added", map[string]any{
		"output_index": item.outputIndex,
		"item":         addedItem,
	})}, nil
}

// toolCallDelta 按 item 类型发 arguments.delta 或 custom_tool_call_input.delta。
func (encoder *StreamEncoder) toolCallDelta(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.itemAnyKind(event.ContentIndex, "function_call", "custom_tool_call")
	if err != nil {
		return nil, err
	}
	eventName := "response.function_call_arguments.delta"
	if item.kind == "custom_tool_call" {
		eventName = "response.custom_tool_call_input.delta"
	}
	return []SSEEvent{encoder.emitDelta(deltaEvent{
		Type: eventName, ItemID: item.id, OutputIndex: item.outputIndex, Delta: event.Delta,
	})}, nil
}

// endToolCall 关工具 item 并发 *.done 与 output_item.done；完整参数
// 优先取 end 事件携带的 ToolCall。
func (encoder *StreamEncoder) endToolCall(event llm.ResponseEvent) ([]SSEEvent, error) {
	item, err := encoder.itemAnyKind(event.ContentIndex, "function_call", "custom_tool_call")
	if err != nil {
		return nil, err
	}
	// event.Validate 保证 ToolCallEnd 的 ToolCall 非空，完整参数直接取它，
	// 不再依赖 delta 累计值。
	arguments := string(event.ToolCall.Arguments)
	item.callID = event.ToolCall.ID
	item.name = event.ToolCall.Name
	completedItem := map[string]any{
		"id": item.id, "type": item.kind, "status": "completed",
		"call_id": item.callID, "name": item.name,
	}
	eventName := "response.function_call_arguments.done"
	field := "arguments"
	if item.kind == "custom_tool_call" {
		eventName = "response.custom_tool_call_input.done"
		field = "input"
	}
	completedItem[field] = arguments
	encoder.closeItem(item, completedItem)
	return []SSEEvent{
		encoder.emit(eventName, map[string]any{
			"item_id": item.id, "output_index": item.outputIndex, field: arguments,
		}),
		encoder.emit("response.output_item.done", map[string]any{"output_index": item.outputIndex, "item": completedItem}),
	}, nil
}

// done 校验无悬空 item 后发 response.completed/incomplete 终帧。
func (encoder *StreamEncoder) done(event llm.ResponseEvent) ([]SSEEvent, error) {
	for _, index := range slices.Sorted(maps.Keys(encoder.items)) {
		if item := encoder.items[index]; !item.closed {
			return nil, fmt.Errorf("cannot finish response with open %s item at output index %d", item.kind, item.outputIndex)
		}
	}
	encoder.completed = true
	response := baseResponse(encoder.responseID, encoder.model, encoder.createdAt, responseStatus(event.Reason))
	response["output"] = encoder.completedOutput()
	response["usage"] = responseUsage(event.Message.Usage)
	eventName := "response.completed"
	if event.Reason == llm.StopReasonLength || event.Reason == llm.StopReasonContentFilter {
		eventName = "response.incomplete"
		reason := "max_output_tokens"
		if event.Reason == llm.StopReasonContentFilter {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
	} else {
		response["completed_at"] = time.Now().Unix()
	}
	return []SSEEvent{encoder.emit(eventName, map[string]any{"response": response})}, nil
}

// failed 先补发挂起 reasoning 的收尾，再发 response.failed 并关闭流。
func (encoder *StreamEncoder) failed(event llm.ResponseEvent) []SSEEvent {
	encoder.completed = true
	// 分类记录随车携带（decoder 产出时已挂）：type/status/retry 全读字段。
	failure := llm.FailureOf(event.Error)
	message := "response stream failed"
	if failure.Error() != "" {
		// Codex 只在 message 含 "try again in Ns" 时按服务端时刻睡眠重试；
		// 追加该短语不影响其它客户端阅读，内部日志保留未改写原文。
		message = common.RetryAfterHint(failure, time.Now())
	}
	// OpenAI Responses API 中，流式失败应发送 response.failed 事件，
	// 包含 status="failed" 的 response 对象与 error 字段。
	// 顶层 status 供下游网关按真实 HTTP 语义分类错误，
	// error.code 让上下文超长被识别为请求级问题而非渠道故障。
	// 事件顶层 error 与 response.error 共用同一份 payload（spec 位置与
	// 排障位置同事实源），debug_ref 等排障字段两处一致。
	errorPayload := common.BuildErrorPayload(message, failure, common.OpenAIErrorType(failure), event.Error.DebugRef, true)
	response := baseResponse(encoder.responseID, encoder.model, encoder.createdAt, "failed")
	response["error"] = errorPayload
	// 挂起的 reasoning item 先补发收尾再下发失败事件，与 Done 路径一致——
	// 否则等待尾随签名的 item 会悬空在 output 之外。
	events := encoder.flushPendingReasoning()
	return append(events, encoder.emit("response.failed", map[string]any{
		"response": response,
		"status":   common.HTTPStatus(failure),
		"error":    errorPayload,
	}))
}

// newItem 按 llm ContentIndex 登记新 output item；下标重复即报解码器 bug。
func (encoder *StreamEncoder) newItem(contentIndex int, kind string, prefix string) (*streamItem, error) {
	if _, exists := encoder.items[contentIndex]; exists {
		return nil, fmt.Errorf("content index %d already has an output item", contentIndex)
	}
	item := &streamItem{
		kind: kind, id: randid.Prefixed(prefix + "_"), outputIndex: len(encoder.output), contentIndex: 0,
	}
	encoder.items[contentIndex] = item
	encoder.output = append(encoder.output, nil)
	return item, nil
}

// item 取指定下标、指定类型的进行中 item。
func (encoder *StreamEncoder) item(contentIndex int, kind string) (*streamItem, error) {
	return encoder.itemAnyKind(contentIndex, kind)
}

// itemAnyKind 取指定下标的进行中 item，kind 必须属于给定集合——
// 工具调用在事件途中才能区分 function_call / custom_tool_call。
func (encoder *StreamEncoder) itemAnyKind(contentIndex int, kinds ...string) (*streamItem, error) {
	item := encoder.items[contentIndex]
	if item == nil {
		return nil, fmt.Errorf("content index %d has no active output item", contentIndex)
	}
	for _, kind := range kinds {
		if item.kind == kind {
			if item.closed {
				return nil, fmt.Errorf("content index %d output item is already closed", contentIndex)
			}
			return item, nil
		}
	}
	return nil, fmt.Errorf("content index %d is %q, want one of %v", contentIndex, item.kind, kinds)
}

// closeItem 关闭 item 并把最终形态写进 output 槽位。
func (encoder *StreamEncoder) closeItem(item *streamItem, output any) {
	item.closed = true
	encoder.output[item.outputIndex] = output
}

// completedOutput 返回已关闭 item 的最终形态数组（跳过未关闭槽位）。
func (encoder *StreamEncoder) completedOutput() []any {
	output := make([]any, 0, len(encoder.output))
	for _, item := range encoder.output {
		if item != nil {
			output = append(output, item)
		}
	}
	return output
}

// emit 补 type/sequence_number 后 marshal 成一帧 SSE。
func (encoder *StreamEncoder) emit(name string, payload map[string]any) SSEEvent {
	payload["type"] = name
	payload["sequence_number"] = encoder.sequenceNumber
	encoder.sequenceNumber++
	data, _ := json.Marshal(payload)
	return SSEEvent{Name: name, Data: data}
}

// deltaEvent 是高频增量事件的固定编码形态：键集与 emit(map) 产出逐一
// 对应，但走 struct 编码——省掉每帧一次 map 反射 marshal。omitempty
// 指针字段保证缺省键不出现，与各事件原 map 键集一致。
type deltaEvent struct {
	Type           string `json:"type"`
	SequenceNumber int64  `json:"sequence_number"`
	ItemID         string `json:"item_id"`
	OutputIndex    int    `json:"output_index"`
	ContentIndex   *int   `json:"content_index,omitempty"`
	SummaryIndex   *int   `json:"summary_index,omitempty"`
	Delta          string `json:"delta"`
	Logprobs       []any  `json:"logprobs,omitempty"`
}

// emitDelta 补 sequence_number 后用 struct 编码一帧增量 SSE。
func (encoder *StreamEncoder) emitDelta(event deltaEvent) SSEEvent {
	event.SequenceNumber = encoder.sequenceNumber
	encoder.sequenceNumber++
	data, _ := json.Marshal(event)
	return SSEEvent{Name: event.Type, Data: data}
}

// baseResponse 生成 Response 对象的稳定字段骨架（store=false 等见函数体注释）。
func baseResponse(id string, model string, createdAt int64, status string) map[string]any {
	// 对齐 OpenAI Response 对象的稳定字段。store=false 是诚实声明：
	// 本代理无响应存储，报 true 会诱使 Codex 等客户端走
	// previous_response_id 续链而静默丢掉全部上下文；false 让客户端
	// 回退到每次携带完整历史。
	return map[string]any{
		"id": id, "object": "response", "created_at": createdAt, "status": status,
		"error": nil, "incomplete_details": nil, "instructions": nil, "model": model,
		"output": []any{}, "parallel_tool_calls": true, "previous_response_id": nil,
		"reasoning": map[string]any{"effort": nil, "summary": nil}, "store": false,
		"temperature": nil, "top_p": nil, "truncation": "disabled",
		"tool_choice": "auto", "tools": []any{}, "usage": nil, "metadata": map[string]any{},
		"max_output_tokens": nil, "text": map[string]any{"format": map[string]any{"type": "text"}},
	}
}

// responseUsage 投影 Responses usage 形态，含 cache 与 reasoning 明细。
func responseUsage(usage llm.Usage) map[string]any {
	inputTokens, total := common.UsageTotals(usage)
	result := map[string]any{
		"input_tokens": inputTokens,
		"input_tokens_details": map[string]any{
			"cached_tokens": usage.CacheRead, "cache_write_tokens": usage.CacheWrite,
		},
		"output_tokens": usage.Output,
		"total_tokens":  total,
	}
	// Reasoning 为 nil 表示上游未报告推理子集；恒输出 0 会把「未知」
	// 伪造成「无推理」，与 chat 面 completion_tokens_details 处理一致。
	if usage.Reasoning != nil {
		result["output_tokens_details"] = map[string]any{
			"reasoning_tokens": *usage.Reasoning,
		}
	}
	return result
}

// outputFromMessage 把最终消息内容块投影成 Responses output 数组。
func outputFromMessage(message *llm.AssistantMessage) ([]any, error) {
	// OpenAI 常见顺序：reasoning → function_call → message；稳定排序避免 IDE 只读 output[0] 当 message。
	var reasonings, toolCalls, messages []any
	messageIDClaimed := false
	for _, block := range message.Content {
		switch content := block.(type) {
		case llm.TextContent:
			// 上游 outputId 只归首个 message item——多个文字块共用同一 id
			// 会让 output item 标识冲突，其余用合成 msg_。
			messageID := message.OutputID
			if messageID == "" || messageIDClaimed {
				messageID = randid.Prefixed("msg_")
			}
			messageIDClaimed = true
			messages = append(messages, map[string]any{
				"id": messageID, "type": "message", "status": "completed", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": content.Text, "annotations": []any{}}},
			})
		case llm.ThinkingContent:
			itemID := randid.Prefixed("rs_")
			if content.SignatureType == "openai" {
				if items := common.OpenAIReasoningItems(content.ThinkingSignature); items != nil && items[0].ID != "" {
					itemID = items[0].ID
				}
			}
			item := map[string]any{
				"id": itemID, "type": "reasoning", "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": content.Thinking}},
			}
			if content.ThinkingSignature != "" {
				item["encrypted_content"] = content.ThinkingSignature
			}
			reasonings = append(reasonings, item)
		case llm.ToolCall:
			if content.Custom {
				toolCalls = append(toolCalls, map[string]any{
					"id": randid.Prefixed("fc_"), "type": "custom_tool_call", "status": "completed",
					"call_id": content.ID, "name": content.Name, "input": string(content.Arguments),
				})
			} else {
				toolCalls = append(toolCalls, map[string]any{
					"id": randid.Prefixed("fc_"), "type": "function_call", "status": "completed",
					"call_id": content.ID, "name": content.Name, "arguments": string(content.Arguments),
				})
			}
		default:
			return nil, fmt.Errorf("unsupported response content type %T", block)
		}
	}
	output := make([]any, 0, len(reasonings)+len(toolCalls)+len(messages))
	output = append(output, reasonings...)
	output = append(output, toolCalls...)
	output = append(output, messages...)
	return output, nil
}

// responseStatus 映射 Response 对象的 status 枚举。
func responseStatus(reason llm.StopReason) string {
	if reason == llm.StopReasonLength || reason == llm.StopReasonContentFilter {
		return "incomplete"
	}
	if reason == llm.StopReasonError || reason == llm.StopReasonAborted {
		return "failed"
	}
	return "completed"
}
