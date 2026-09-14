// 本文件定义三种 API 协议在 app 层的统一适配边界。
package app

import (
	"encoding/json"
	"fmt"

	"github.com/WncFht/devin2api/internal/api/anthropic/messages"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/api/openai/chat"
	"github.com/WncFht/devin2api/internal/api/openai/responses"
	"github.com/WncFht/devin2api/internal/llm"
)

// protocolEncoder 抽象流式与非流式协议编码。
type protocolEncoder interface {
	// NewStreamEncoder 创建与本次 HTTP 请求绑定的流式编码器。
	NewStreamEncoder(model string, includeUsage bool) streamEncoder
	// EncodeFinal 把最终助手消息编码为完整的非流式 JSON 响应体。
	EncodeFinal(message *llm.AssistantMessage) ([]byte, error)
	// EncodeError 把错误编码为该协议形状的错误 JSON 体——非流式心跳
	// 已提交 200 后，错误只能以错误体下发，形状按客户端协议决定。
	EncodeError(err error, debugRef string) []byte
	// EncodeHTTPError 把错误编码为该协议形状的完整错误响应体，供
	// 未提交响应头的 HTTP 错误路径使用：/v1/messages 的失败必须是
	// Anthropic 的 {"type":"error","error":{...}} 信封，回 OpenAI 形状
	// 时 Claude Code 等客户端解析不出 error 字段。
	EncodeHTTPError(e httpError) []byte
	// StreamErrorEvents 为 true 表示该协议的流式客户端把流内错误
	// 事件当可重试信号：限流（429）是 pre-stream 失败中唯一转
	// 「200 + 错误事件」下发的类别——Codex 对 HTTP 429 一律终止
	//（codex-rs retry_429 硬编码 false，5xx/transport 却照常重试），
	// 只有流内错误事件进它的重试循环；确定性 4xx 重试无意义，
	// 保留真实状态码让下游网关按请求级错误分类。false（Anthropic）
	// 表示客户端按 HTTP 状态码重试，提前提交 200 会把失败降级为
	// 不可重试的畸形响应。
	StreamErrorEvents() bool
	// AppendSSE 把单个 SSE 事件追加编码到 dst；写方持有 dst 的所有权，
	// 避免每帧先分配临时切片再整体拷贝进批次缓冲。
	AppendSSE(dst []byte, name string, data []byte) []byte
}

// httpError 是 app 层归一化后的错误视图：协议层只负责把同一组字段
// 装进各自的信封。ClientFixable 为真时错误类型统一为
// invalid_request_error（两侧协议对该语义同名）。
type httpError struct {
	// Message 是透传给客户端的错误原文（不改写上游文案）。
	Message string
	// ClientFixable 标记客户端可修正的请求错误（图片不支持/
	// invalid_argument/超长），覆盖协议默认类型推导。
	ClientFixable bool
	// Stage 是失败发生的处理层（http_decode/provider_stream 等）。
	Stage string
	// DebugRef 是本地调试目录名；空串省略。
	DebugRef string
}

// openAIHTTPError 编码 OpenAI 系（chat/responses 共享）的 HTTP 错误体。
func openAIHTTPError(e httpError) []byte {
	errorType := common.OpenAIErrorType(e.Message)
	if e.ClientFixable {
		errorType = "invalid_request_error"
	}
	payload := map[string]any{
		"message": e.Message, "type": errorType,
		"code": common.ErrorCode(e.Message), "param": nil, "stage": e.Stage,
	}
	for key, value := range common.UpstreamErrorDetails(e.Message) {
		payload[key] = value
	}
	if e.DebugRef != "" {
		payload["debug_ref"] = e.DebugRef
	}
	body, _ := json.Marshal(map[string]any{"error": payload})
	return append(body, '\n')
}

// openAIErrorBody 编码 OpenAI 系（chat/responses 共享）的错误 JSON 体。
func openAIErrorBody(err error, debugRef string) []byte {
	payload := map[string]any{
		"message": err.Error(), "type": common.OpenAIErrorType(err.Error()),
		"code": common.ErrorCode(err.Error()), "param": nil,
	}
	for key, value := range common.UpstreamErrorDetails(err.Error()) {
		payload[key] = value
	}
	if debugRef != "" {
		payload["debug_ref"] = debugRef
	}
	body, _ := json.Marshal(map[string]any{"error": payload})
	return body
}

// streamEncoder 抽象三种协议共有的中间事件编码。
type streamEncoder interface {
	Encode(event llm.ResponseEvent) ([]common.SSEEvent, error)
}

// protocolOptions 保存三个协议都需要的生成控制选项。
type protocolOptions struct {
	Stream       bool
	IncludeUsage bool
}

// responsesProtocol 实现 OpenAI Responses API 协议。
type responsesProtocol struct{}

func (p responsesProtocol) NewStreamEncoder(model string, _ bool) streamEncoder {
	return responses.NewStreamEncoder(model)
}

func (p responsesProtocol) EncodeFinal(message *llm.AssistantMessage) ([]byte, error) {
	return responses.EncodeResponse(message)
}

func (p responsesProtocol) EncodeError(err error, debugRef string) []byte {
	return openAIErrorBody(err, debugRef)
}

func (p responsesProtocol) EncodeHTTPError(e httpError) []byte {
	return openAIHTTPError(e)
}

func (p responsesProtocol) StreamErrorEvents() bool { return true }

func (p responsesProtocol) AppendSSE(dst []byte, name string, data []byte) []byte {
	return fmt.Appendf(dst, "event: %s\ndata: %s\n\n", name, data)
}

// chatProtocol 实现 OpenAI Chat Completions 协议。
type chatProtocol struct{}

func (p chatProtocol) NewStreamEncoder(model string, includeUsage bool) streamEncoder {
	return chat.NewStreamEncoder(model, includeUsage)
}

func (p chatProtocol) EncodeFinal(message *llm.AssistantMessage) ([]byte, error) {
	return chat.EncodeResponse(message)
}

func (p chatProtocol) EncodeError(err error, debugRef string) []byte {
	return openAIErrorBody(err, debugRef)
}

func (p chatProtocol) EncodeHTTPError(e httpError) []byte {
	return openAIHTTPError(e)
}

func (p chatProtocol) StreamErrorEvents() bool { return true }

func (p chatProtocol) AppendSSE(dst []byte, name string, data []byte) []byte {
	// OpenAI Chat Completions 使用 data-only SSE；[DONE] 作为流终止标记。
	if name == "[DONE]" {
		return append(dst, "data: [DONE]\n\n"...)
	}
	return fmt.Appendf(dst, "data: %s\n\n", data)
}

// anthropicProtocol 实现 Anthropic Messages 协议。
type anthropicProtocol struct{}

func (p anthropicProtocol) NewStreamEncoder(model string, _ bool) streamEncoder {
	return messages.NewStreamEncoder(model)
}

func (p anthropicProtocol) EncodeFinal(message *llm.AssistantMessage) ([]byte, error) {
	return messages.EncodeResponse(message)
}

func (p anthropicProtocol) EncodeError(err error, debugRef string) []byte {
	payload := map[string]any{
		"type": common.AnthropicErrorType(err.Error()), "message": err.Error(),
		"code": common.ErrorCode(err.Error()),
	}
	for key, value := range common.UpstreamErrorDetails(err.Error()) {
		payload[key] = value
	}
	if debugRef != "" {
		payload["debug_ref"] = debugRef
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": payload})
	return body
}

func (p anthropicProtocol) StreamErrorEvents() bool { return false }

func (p anthropicProtocol) EncodeHTTPError(e httpError) []byte {
	errorType := common.AnthropicErrorType(e.Message)
	if e.ClientFixable {
		errorType = "invalid_request_error"
	}
	payload := map[string]any{
		"type": errorType, "message": e.Message,
		"code": common.ErrorCode(e.Message), "stage": e.Stage,
	}
	for key, value := range common.UpstreamErrorDetails(e.Message) {
		payload[key] = value
	}
	if e.DebugRef != "" {
		payload["debug_ref"] = e.DebugRef
	}
	body, _ := json.Marshal(map[string]any{"type": "error", "error": payload})
	return append(body, '\n')
}

func (p anthropicProtocol) AppendSSE(dst []byte, name string, data []byte) []byte {
	return fmt.Appendf(dst, "event: %s\ndata: %s\n\n", name, data)
}

// decodeRequest 把具体协议的解码结果统一为中间请求和公共选项。
type decodeRequestFunc func([]byte) (llm.RequestMessages, protocolOptions, error)

func decodeResponsesRequest(data []byte) (llm.RequestMessages, protocolOptions, error) {
	adapted, err := responses.DecodeRequest(data)
	if err != nil {
		return llm.RequestMessages{}, protocolOptions{}, err
	}
	// HTTP 路径无响应存储（store=false 已如实声明），previous_response_id
	// 意味着客户端只发了增量 input——静默当全量会把上下文丢光，
	// 显式拒绝比带病执行便宜。WS 会话在规范化时已剥掉该字段做
	// 本地合并，不会走到这里。
	if adapted.Options.PreviousResponseID != "" {
		return llm.RequestMessages{}, protocolOptions{}, fmt.Errorf(
			"invalid_argument: previous_response_id %q requires a server-side response store; this proxy always reports store=false — resend the full conversation input without previous_response_id",
			adapted.Options.PreviousResponseID)
	}
	return adapted.Context, protocolOptions{
		Stream:       adapted.Options.Stream,
		IncludeUsage: false,
	}, nil
}

func decodeChatRequest(data []byte) (llm.RequestMessages, protocolOptions, error) {
	adapted, err := chat.DecodeRequest(data)
	if err != nil {
		return llm.RequestMessages{}, protocolOptions{}, err
	}
	return adapted.Context, protocolOptions{
		Stream:       adapted.Options.Stream,
		IncludeUsage: adapted.Options.IncludeUsage,
	}, nil
}

func decodeAnthropicRequest(data []byte) (llm.RequestMessages, protocolOptions, error) {
	adapted, err := messages.DecodeRequest(data)
	if err != nil {
		return llm.RequestMessages{}, protocolOptions{}, err
	}
	return adapted.Context, protocolOptions{
		Stream:       adapted.Options.Stream,
		IncludeUsage: false,
	}, nil
}
