// 本文件把 06-http-response.jsonl 里的 SSE 帧合并成可读的最终响应。
// 兼容三种协议形态：OpenAI chat.chunk（choices[].delta.content）、
// OpenAI responses（response.output_text.delta）、
// Anthropic（content_block_delta 的 text_delta/thinking_delta/input_json_delta）。
// 原始文件仍可通过 /file/ 端点查看——这里只做阅读视图。
package dashboard

import (
	"encoding/json"
	"strings"
)

// mergedStream 是合并视图的返回结构。
type mergedStream struct {
	// Text 是按帧序拼接的正文增量。
	Text string `json:"text"`
	// Reasoning 是思考/推理增量（若有）。
	Reasoning string `json:"reasoning,omitempty"`
	// ToolInput 是工具调用参数的 JSON 增量拼接（若有）。
	ToolInput string `json:"tool_input,omitempty"`
	// Usage 是最后一个携带 usage 的帧内容（若有）。
	Usage json.RawMessage `json:"usage,omitempty"`
	// FinishReason 是最后出现的结束原因（若有）。
	FinishReason string `json:"finish_reason,omitempty"`
	// Events 是参与合并的帧数。
	Events int `json:"events"`
	// EventNames 是各 SSE 事件名出现次数，便于快速了解流构成。
	EventNames map[string]int `json:"event_names"`
}

// mergeStreamEvents 解析 06-http-response.jsonl 每行的
// {"event":<sse名>,"data":<payload>} 记录并拼接增量文本。
func mergeStreamEvents(data []byte) mergedStream {
	var text, reasoning, toolInput strings.Builder
	out := mergedStream{EventNames: map[string]int{}}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var record struct {
			Event string          `json:"event"`
			Data  json.RawMessage `json:"data"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		out.Events++
		out.EventNames[record.Event]++
		var payload map[string]any
		if json.Unmarshal(record.Data, &payload) != nil {
			continue // [DONE] 等纯文本帧
		}
		mergePayload(payload, &text, &reasoning, &toolInput, &out)
	}
	out.Text = text.String()
	out.Reasoning = reasoning.String()
	out.ToolInput = toolInput.String()
	return out
}

// mergePayload 从单帧 JSON 中提取增量片段与收尾信息。
func mergePayload(payload map[string]any, text, reasoning, toolInput *strings.Builder, out *mergedStream) {
	// OpenAI chat completions：choices[].delta.content / reasoning_content，
	// choices[].finish_reason；usage 在末帧或独立帧。
	if choices, ok := payload["choices"].([]any); ok {
		for _, c := range choices {
			choice, ok := c.(map[string]any)
			if !ok {
				continue
			}
			delta, _ := choice["delta"].(map[string]any)
			if s, _ := delta["content"].(string); s != "" {
				text.WriteString(s)
			}
			if s, _ := delta["reasoning_content"].(string); s != "" {
				reasoning.WriteString(s)
			}
			if s, _ := choice["finish_reason"].(string); s != "" {
				out.FinishReason = s
			}
		}
	}
	if usage, ok := payload["usage"]; ok {
		if raw, err := json.Marshal(usage); err == nil {
			out.Usage = raw
		}
	}

	// OpenAI responses API：type 字段区分增量与终态。
	switch typ, _ := payload["type"].(string); typ {
	case "response.output_text.delta":
		if s, _ := payload["delta"].(string); s != "" {
			text.WriteString(s)
		}
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if s, _ := payload["delta"].(string); s != "" {
			reasoning.WriteString(s)
		}
	case "response.completed", "response.done", "response.incomplete":
		if resp, ok := payload["response"].(map[string]any); ok {
			if usage, ok := resp["usage"]; ok {
				if raw, err := json.Marshal(usage); err == nil {
					out.Usage = raw
				}
			}
		}
		// Anthropic 消息流：content_block_delta 携带 text/thinking/tool 增量。
	case "content_block_delta":
		if delta, ok := payload["delta"].(map[string]any); ok {
			switch dt, _ := delta["type"].(string); dt {
			case "text_delta":
				if s, _ := delta["text"].(string); s != "" {
					text.WriteString(s)
				}
			case "thinking_delta", "signature_delta":
				if s, _ := delta["thinking"].(string); s != "" {
					reasoning.WriteString(s)
				}
			case "input_json_delta":
				if s, _ := delta["partial_json"].(string); s != "" {
					toolInput.WriteString(s)
				}
			}
		}
	case "message_delta":
		if delta, ok := payload["delta"].(map[string]any); ok {
			if s, _ := delta["stop_reason"].(string); s != "" {
				out.FinishReason = s
			}
		}
		if usage, ok := payload["usage"]; ok {
			if raw, err := json.Marshal(usage); err == nil {
				out.Usage = raw
			}
		}
	}
}
