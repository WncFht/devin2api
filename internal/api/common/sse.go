// 本文件定义三种协议（OpenAI Chat/Responses、Anthropic Messages）共用的
// SSE 事件表示，使 app 层流泵无需逐协议适配。
package common

// SSEDone 是 OpenAI 风格 data-only 流的终止标记：Chat Completions 在
// 最后一个 chunk 之后单独发一帧 data: [DONE]。
const SSEDone = "[DONE]"

// SSEEvent 是单个待写出的 SSE 事件。
type SSEEvent struct {
	// Name 是 SSE event 字段值；Chat Completions 的 data-only 风格下取 "[DONE]" 标记流终止。
	Name string
	// Data 是 JSON 编码的 SSE data 内容。
	Data []byte
}
