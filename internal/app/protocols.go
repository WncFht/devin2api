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
	// DecodeRequest 把该协议的请求体解码为中间请求和公共选项。
	// collectDropped 决定是否收集顶层未消费字段（Dropped 的唯一读者是
	// debuglog 请求投影）——debug 关时不值得为记账再做一遍全量扫描。
	DecodeRequest(data []byte, collectDropped bool) (llm.RequestMessages, protocolOptions, error)
	// NewStreamEncoder 创建与本次 HTTP 请求绑定的流式编码器。
	NewStreamEncoder(model string, options protocolOptions) streamEncoder
	// EncodeFinal 把最终助手消息编码为完整的非流式 JSON 响应体。
	// model 是回显给客户端的模型名——客户端请求原文（可能是别名）；
	// 为空时编码器回落到上游声明/解析后的 uid。流式路径在
	// NewStreamEncoder 拿同一名，两种模式回显一致。
	EncodeFinal(message *llm.AssistantMessage, model string, options protocolOptions) ([]byte, error)
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
	// NonStreamHeartbeat 返回非流式响应等待上游期间周期性写出的保活
	// 载荷；nil 表示保持静默、不心跳。
	NonStreamHeartbeat() []byte
}

// nonStreamHeartbeatNewline 是 OpenAI 系非流式响应的保活载荷："\n" 是
// JSON 响应体的合法前导空白，喂 Codex 约 30s 无字节弃连的窗口，
// 不污染最终文档。
var nonStreamHeartbeatNewline = []byte("\n")

// httpError 是 app 层归一化后的错误视图：协议层只负责把同一组字段
// 装进各自的信封。ClientFixable 为真时错误类型统一为
// invalid_request_error（两侧协议对该语义同名）。
type httpError struct {
	// Failure 是本次失败的分类记录：type/code/排障字段全部从记录派生，
	// 协议层不再按文本反推。
	Failure *llm.Failure
	// ClientFixable 标记客户端可修正的请求错误（图片不支持/
	// invalid_argument/超长），覆盖协议默认类型推导。
	ClientFixable bool
	// Stage 是失败发生的处理层（http_decode/provider_stream 等）。
	Stage string
	// DebugRef 是本地调试目录名；空串省略。
	DebugRef string
}

// marshalOpenAIError 编码 OpenAI 系（chat/responses 共享）的
// {"error":{...}} 信封；stage 非空即 HTTP 错误响应形态——附带 stage
// 字段与尾随换行，流内错误体不带。
func marshalOpenAIError(failure *llm.Failure, errorType, debugRef, stage string) []byte {
	payload := common.BuildErrorPayload(failure.Error(), failure, errorType, debugRef, true)
	if stage != "" {
		payload["stage"] = stage
	}
	body, _ := json.Marshal(map[string]any{"error": payload})
	if stage != "" {
		return append(body, '\n')
	}
	return body
}

// openAIHTTPError 编码 OpenAI 系（chat/responses 共享）的 HTTP 错误体。
func openAIHTTPError(e httpError) []byte {
	errorType := common.OpenAIErrorType(e.Failure)
	if e.ClientFixable {
		errorType = "invalid_request_error"
	}
	return marshalOpenAIError(e.Failure, errorType, e.DebugRef, e.Stage)
}

// openAIErrorBody 编码 OpenAI 系（chat/responses 共享）的错误 JSON 体。
func openAIErrorBody(err error, debugRef string) []byte {
	failure := llm.Classify(err)
	return marshalOpenAIError(failure, common.OpenAIErrorType(failure), debugRef, "")
}

// streamEncoder 抽象三种协议共有的中间事件编码。
type streamEncoder interface {
	Encode(event llm.ResponseEvent) ([]common.SSEEvent, error)
}

// protocolOptions 保存三个协议都需要的生成控制选项。
type protocolOptions struct {
	Stream       bool
	IncludeUsage bool
	// ToolNameMap 是 namespace 展平名到客户端面向名的还原表
	//（responses 侧 {ns}__{sub} → {ns}.{sub}）；其余协议恒为空。
	ToolNameMap map[string]string
}

// responsesProtocol 实现 OpenAI Responses API 协议。
type responsesProtocol struct{}

func (p responsesProtocol) DecodeRequest(data []byte, collectDropped bool) (llm.RequestMessages, protocolOptions, error) {
	adapted, err := responses.DecodeRequest(data, collectDropped)
	if err != nil {
		return llm.RequestMessages{}, protocolOptions{}, err
	}
	// HTTP 路径无响应存储（store=false 已如实声明），previous_response_id
	// 意味着客户端只发了增量 input——静默当全量会把上下文丢光，
	// 显式拒绝比带病执行便宜。WS 会话在规范化时已剥掉该字段做
	// 本地合并，不会走到这里。
	if adapted.Options.PreviousResponseID != "" {
		return llm.RequestMessages{}, protocolOptions{}, &llm.Failure{
			Code: "invalid_argument",
			Message: fmt.Sprintf(
				"previous_response_id %q requires a server-side response store; this proxy always reports store=false — resend the full conversation input without previous_response_id",
				adapted.Options.PreviousResponseID),
		}
	}
	return adapted.Context, protocolOptions{
		Stream:       adapted.Options.Stream,
		IncludeUsage: false,
		ToolNameMap:  adapted.Options.ToolNameMap,
	}, nil
}

func (p responsesProtocol) NewStreamEncoder(model string, options protocolOptions) streamEncoder {
	return responses.NewStreamEncoder(model, options.ToolNameMap)
}

func (p responsesProtocol) EncodeFinal(message *llm.AssistantMessage, model string, options protocolOptions) ([]byte, error) {
	return responses.EncodeResponse(message, model, options.ToolNameMap)
}

func (p responsesProtocol) EncodeError(err error, debugRef string) []byte {
	return openAIErrorBody(err, debugRef)
}

func (p responsesProtocol) EncodeHTTPError(e httpError) []byte {
	return openAIHTTPError(e)
}

func (p responsesProtocol) StreamErrorEvents() bool { return true }

func (p responsesProtocol) AppendSSE(dst []byte, name string, data []byte) []byte {
	return appendNamedSSE(dst, name, data)
}

func (p responsesProtocol) NonStreamHeartbeat() []byte { return nonStreamHeartbeatNewline }

// chatProtocol 实现 OpenAI Chat Completions 协议。
type chatProtocol struct{}

func (p chatProtocol) DecodeRequest(data []byte, collectDropped bool) (llm.RequestMessages, protocolOptions, error) {
	adapted, err := chat.DecodeRequest(data, collectDropped)
	if err != nil {
		return llm.RequestMessages{}, protocolOptions{}, err
	}
	return adapted.Context, protocolOptions{
		Stream:       adapted.Options.Stream,
		IncludeUsage: adapted.Options.IncludeUsage,
	}, nil
}

func (p chatProtocol) NewStreamEncoder(model string, options protocolOptions) streamEncoder {
	return chat.NewStreamEncoder(model, options.IncludeUsage)
}

func (p chatProtocol) EncodeFinal(message *llm.AssistantMessage, model string, _ protocolOptions) ([]byte, error) {
	return chat.EncodeResponse(message, model)
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
	if name == common.SSEDone {
		return append(dst, ("data: " + common.SSEDone + "\n\n")...)
	}
	dst = append(dst, "data: "...)
	dst = append(dst, data...)
	return append(dst, '\n', '\n')
}

func (p chatProtocol) NonStreamHeartbeat() []byte { return nonStreamHeartbeatNewline }

// anthropicProtocol 实现 Anthropic Messages 协议。
type anthropicProtocol struct{}

func (p anthropicProtocol) DecodeRequest(data []byte, collectDropped bool) (llm.RequestMessages, protocolOptions, error) {
	adapted, err := messages.DecodeRequest(data, collectDropped)
	if err != nil {
		return llm.RequestMessages{}, protocolOptions{}, err
	}
	return adapted.Context, protocolOptions{
		Stream:       adapted.Options.Stream,
		IncludeUsage: false,
	}, nil
}

func (p anthropicProtocol) NewStreamEncoder(model string, _ protocolOptions) streamEncoder {
	return messages.NewStreamEncoder(model)
}

func (p anthropicProtocol) EncodeFinal(message *llm.AssistantMessage, model string, _ protocolOptions) ([]byte, error) {
	return messages.EncodeResponse(message, model)
}

func (p anthropicProtocol) EncodeError(err error, debugRef string) []byte {
	failure := llm.Classify(err)
	payload := common.BuildErrorPayload(failure.Error(), failure, common.AnthropicErrorType(failure), debugRef, false)
	body, _ := json.Marshal(map[string]any{"type": "error", "error": payload})
	return body
}

func (p anthropicProtocol) StreamErrorEvents() bool { return false }

func (p anthropicProtocol) EncodeHTTPError(e httpError) []byte {
	errorType := common.AnthropicErrorType(e.Failure)
	if e.ClientFixable {
		errorType = "invalid_request_error"
	}
	payload := common.BuildErrorPayload(e.Failure.Error(), e.Failure, errorType, e.DebugRef, false)
	payload["stage"] = e.Stage
	body, _ := json.Marshal(map[string]any{"type": "error", "error": payload})
	return append(body, '\n')
}

func (p anthropicProtocol) AppendSSE(dst []byte, name string, data []byte) []byte {
	return appendNamedSSE(dst, name, data)
}

// Anthropic 非流式在上游思考窗口保持静默：其 SDK 系客户端容忍分钟级
// 首字等待，而任何提前写出的字节都把状态提交为 200，之后的失败只能以
// 「200 + 错误体」下发——Claude Code 把它判为 malformed response 并
// 终止整轮，不可重试。
func (p anthropicProtocol) NonStreamHeartbeat() []byte { return nil }

// appendNamedSSE 把单条带 event 字段的 SSE 帧追加编码到 dst——
// fmt.Appendf 每帧要过格式串解析与 reflect 装箱，直拼省掉这层开销。
func appendNamedSSE(dst []byte, name string, data []byte) []byte {
	dst = append(dst, "event: "...)
	dst = append(dst, name...)
	dst = append(dst, "\ndata: "...)
	dst = append(dst, data...)
	return append(dst, '\n', '\n')
}
