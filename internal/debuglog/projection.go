// 本文件定义中间 LLM 请求和响应事件的稳定调试日志投影。
package debuglog

import (
	"github.com/WncFht/devin2api/internal/llm"
)

// RequestMessagesProjection 将含接口字段的 RequestMessages 转成可读 JSON 结构。
func RequestMessagesProjection(request llm.RequestMessages) map[string]any {
	messages := make([]any, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, messageProjection(message))
	}
	tools := make([]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, map[string]any{
			"name": tool.Name, "description": tool.Description, "input_schema": tool.InputSchema,
		})
	}
	result := map[string]any{
		"model":          request.Model,
		"system_prompt":  request.SystemPrompt,
		"messages":       messages,
		"tools":          tools,
		"stop_sequences": request.StopSequences,
	}
	if request.ToolChoice != nil {
		result["tool_choice"] = map[string]any{"mode": request.ToolChoice.Mode, "tool_name": request.ToolChoice.ToolName}
	}
	if request.DisableParallelToolCalls {
		result["disable_parallel_tool_calls"] = true
	}
	if request.MaxTokens != nil {
		result["max_tokens"] = *request.MaxTokens
	}
	if request.Temperature != nil {
		result["temperature"] = *request.Temperature
	}
	if request.SessionKey != "" {
		result["session_key"] = request.SessionKey
	}
	if len(request.Dropped) > 0 {
		result["dropped_items"] = request.Dropped
	}
	return result
}

// RecordResponseEvent 把一条上游响应事件记入 05 阶段 JSONL。投影一律打包
// thunk 推迟到日志 worker——事件携带的 Partial 是解码器逐帧快照（各事件
// 独占拷贝），Message/Error/ToolCall 是终止指针无后续写入，延迟求值无
// 竞态，热路径只付一次入队。
func (recorder *Recorder) RecordResponseEvent(event llm.ResponseEvent) {
	if recorder == nil {
		return
	}
	recorder.AppendJSONL(StageResponseEvents, string(event.Type),
		func() any { return responseEventProjection(event) })
}

// responseEventProjection 将响应事件转成避免重复完整 Partial 的日志结构。
func responseEventProjection(event llm.ResponseEvent) map[string]any {
	result := map[string]any{"type": event.Type}
	switch event.Type {
	case llm.ResponseEventTextStart, llm.ResponseEventTextDelta, llm.ResponseEventTextEnd,
		llm.ResponseEventThinkingStart, llm.ResponseEventThinkingDelta, llm.ResponseEventThinkingEnd,
		llm.ResponseEventThinkingSignature,
		llm.ResponseEventToolCallStart, llm.ResponseEventToolCallDelta, llm.ResponseEventToolCallEnd:
		result["content_index"] = event.ContentIndex
	}
	if event.Delta != "" || event.Type == llm.ResponseEventToolCallDelta {
		result["delta"] = event.Delta
	}
	if event.Content != "" {
		result["content"] = event.Content
	}
	if event.ToolCallID != "" {
		result["tool_call_id"] = event.ToolCallID
	}
	if event.ToolName != "" {
		result["tool_name"] = event.ToolName
	}
	if event.ToolCall != nil {
		result["tool_call"] = contentProjection(*event.ToolCall)
	}
	if event.Reason != "" {
		result["reason"] = event.Reason
	}
	if event.Type == llm.ResponseEventStart && event.Partial != nil {
		result["message"] = assistantProjection(*event.Partial)
	}
	if event.Message != nil {
		result["message"] = assistantProjection(*event.Message)
	}
	if event.Error != nil {
		result["error"] = assistantProjection(*event.Error)
	}
	return result
}

// messageProjection 把单条中间模型消息投影为日志 JSON；助手消息复用
// assistantProjection 的全字段，其余类型取各自的可排障字段。
func messageProjection(message llm.Message) map[string]any {
	result := map[string]any{"role": message.Role()}
	switch message := message.(type) {
	case llm.UserMessage:
		result["content"] = contentListProjection(message.Content)
		result["timestamp_ms"] = message.TimestampMS
	case llm.AssistantMessage:
		for key, value := range assistantProjection(message) {
			result[key] = value
		}
	case llm.ToolResultMessage:
		result["tool_call_id"] = message.ToolCallID
		result["content"] = contentListProjection(message.Content)
		result["is_error"] = message.IsError
		result["timestamp_ms"] = message.TimestampMS
	}
	return result
}

// assistantProjection 把助手消息投影为日志 JSON；同一投影同时服务
// 02 的消息列表与 05 的事件内 message 字段，保证两处口径一致。
func assistantProjection(message llm.AssistantMessage) map[string]any {
	return map[string]any{
		"role":                llm.MessageRoleAssistant,
		"content":             contentListProjection(message.Content),
		"api":                 message.API,
		"provider":            message.Provider,
		"model":               message.Model,
		"response_model":      message.ResponseModel,
		"response_id":         message.ResponseID,
		"output_id":           message.OutputID,
		"upstream_request_id": message.UpstreamRequestID,
		"diagnostics":         message.Diagnostics,
		"usage":               message.Usage,
		"stop_reason":         message.StopReason,
		"stop_sequence":       message.StopSequence,
		"error_message":       message.ErrorMessage,
		"timestamp_ms":        message.TimestampMS,
	}
}

// contentListProjection 把内容块列表逐块投影为日志 JSON。
func contentListProjection(content []llm.Content) []any {
	result := make([]any, 0, len(content))
	for _, block := range content {
		result = append(result, contentProjection(block))
	}
	return result
}

// contentProjection 把单个内容块投影为日志 JSON，type 字段取块自报
// 的类型标识；未知块记 {"type":"unknown"} 而不是丢弃，日志如实反映形状。
func contentProjection(content llm.Content) map[string]any {
	switch content := content.(type) {
	case llm.TextContent:
		return map[string]any{"type": content.ContentType(), "text": content.Text}
	case llm.ThinkingContent:
		return map[string]any{"type": content.ContentType(), "thinking": content.Thinking, "thinking_signature": content.ThinkingSignature, "signature_type": content.SignatureType, "redacted": content.Redacted}
	case llm.ImageContent:
		return map[string]any{"type": content.ContentType(), "data": content.Data, "mime_type": content.MIMEType}
	case llm.ToolCall:
		// Custom 调用的 Arguments 是供应商原文而非 JSON，直接 marshal
		// RawMessage 会产生坏 JSON——按字符串落盘并标 custom。
		if content.Custom {
			return map[string]any{"type": content.ContentType(), "id": content.ID, "name": content.Name, "arguments": string(content.Arguments), "custom": true}
		}
		return map[string]any{"type": content.ContentType(), "id": content.ID, "name": content.Name, "arguments": content.Arguments}
	default:
		return map[string]any{"type": "unknown"}
	}
}
