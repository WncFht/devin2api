// 本文件把失败分类记录映射到 OpenAI / Anthropic 错误类型与 HTTP 状态。
//
// 错误语义的全部判定在 llm.Classify/Failure（internal/llm/failure.go）：
// 生产侧（adapter、rate gate）在错误仍有类型时填结构事实，消费函数一律
// 接收 *llm.Failure 读字段。这里只剩面向客户端协议的呈现层映射。
package common

import (
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// openAIErrorTypes 把 Connect code 映射为 OpenAI 兼容的错误对象 type。
// 参考：https://platform.openai.com/docs/guides/error-codes
var openAIErrorTypes = map[string]string{
	"invalid_argument":    "invalid_request_error",
	"failed_precondition": "invalid_request_error",
	"out_of_range":        "invalid_request_error",
	"unimplemented":       "invalid_request_error",
	"unauthenticated":     "authentication_error",
	// Devin 上游把内容策略拦截、无效模型 UID、未授权模型全部归并到
	// permission_denied；这些都是调用方可修正的请求错误，归为
	// invalid_request_error 以便下游网关不误判为账号/渠道失效。
	"permission_denied":  "invalid_request_error",
	"not_found":          "not_found_error",
	"resource_exhausted": "rate_limit_error",
	"deadline_exceeded":  "timeout_error",
	"unavailable":        "server_error",
	"internal":           "server_error",
	"unknown":            "server_error",
}

// anthropicErrorTypes 把 Connect code 映射为 Anthropic 兼容的错误对象 type。
// 参考：https://platform.claude.com/docs/en/api/errors
var anthropicErrorTypes = map[string]string{
	"invalid_argument":    "invalid_request_error",
	"failed_precondition": "invalid_request_error",
	"out_of_range":        "invalid_request_error",
	"unimplemented":       "invalid_request_error",
	"unauthenticated":     "authentication_error",
	// 同上：permission_denied 统一视为可修正的请求错误。
	"permission_denied":  "invalid_request_error",
	"not_found":          "not_found_error",
	"resource_exhausted": "rate_limit_error",
	"deadline_exceeded":  "timeout_error",
	"unavailable":        "api_error",
	"internal":           "api_error",
	"unknown":            "api_error",
}

// OpenAIErrorType 把失败记录映射为 OpenAI error.type；无法识别时返回
// "server_error"。上游责任的失败不论 code 都报 server_error——
// 上游把内部故障塞进可修正 code 时，按 code 映射会把锅甩给调用方。
func OpenAIErrorType(failure *llm.Failure) string {
	if failure != nil && !failure.UpstreamFault {
		if t, ok := openAIErrorTypes[failure.Code]; ok {
			return t
		}
	}
	return "server_error"
}

// AnthropicErrorType 把失败记录映射为 Anthropic error.type；无法识别时
// 返回 "api_error"。UpstreamFault 同上压过 code 映射。
func AnthropicErrorType(failure *llm.Failure) string {
	if failure != nil && !failure.UpstreamFault {
		if t, ok := anthropicErrorTypes[failure.Code]; ok {
			return t
		}
	}
	return "api_error"
}

// HTTPStatus 返回失败记录对应的 HTTP 状态码。
// 同一份映射同时用于响应行状态与流式错误事件里的 status 字段：
// 下游网关按状态码区分"请求级错误"与"渠道故障"——4xx 不冷却整个渠道；
// 上下文超长给 413 并配合 error.code 让网关直接归类为客户端问题，不做任何
// 冷却。客户端取消映射 499（nginx 约定）、超时 504：客户端主动断开计成
// 502 会污染指标并让网关误判渠道故障。failure.Canceled 除客户端断连外
// 还覆盖本端主动取消上游 ctx（面板 abort）——同样归 499：渠道无责，
// 不该计成渠道故障触发冷却。上游责任故障（传输断裂、伪装进
// 可修正 code 的内部错误）返回 502——它确实就是上游故障。
// 无法识别的错误返回 502，表示上游服务故障。
func HTTPStatus(failure *llm.Failure) int {
	if failure == nil {
		return http.StatusBadGateway
	}
	switch {
	case failure.Canceled:
		return 499
	case failure.Timeout:
		return http.StatusGatewayTimeout
	case failure.ContextLength:
		return http.StatusRequestEntityTooLarge
	case failure.UpstreamFault:
		return http.StatusBadGateway
	case failure.ClientFixable:
		// permission_denied 归一到 400 而非 403：下游网关对 4xx 只按
		// 模型作用域冷却，不会把整个渠道标记为失效。
		return http.StatusBadRequest
	}
	switch failure.Code {
	case "unauthenticated":
		return http.StatusUnauthorized
	case "not_found":
		return http.StatusNotFound
	case "resource_exhausted":
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

// ErrorCode 返回错误对象的 code 字段值；上下文超长统一为
// "context_length_exceeded"——与 OpenAI/Anthropic 惯例一致，也让下游
// 网关能把 SSE 错误事件识别为请求级问题而非渠道故障。限流给
// "rate_limit_exceeded"：Codex 只在 error.code 为该值时把流内错误
// 归入 RateLimitExceeded 重试档（codex-rs sse/responses.rs）。
// 其他错误返回 nil。
func ErrorCode(failure *llm.Failure) any {
	if failure == nil {
		return nil
	}
	if failure.ContextLength {
		return "context_length_exceeded"
	}
	if failure.RateLimited {
		return "rate_limit_exceeded"
	}
	return nil
}

// RetryAfterHint 给限流错误追加 Codex 可解析的等待提示 " (try again
// in Ns)"：codex-rs 只在 error.code=="rate_limit_exceeded" 且 message
// 匹配 /try again in N(s|ms|seconds)/ 时才按服务端建议时刻睡眠重试
// （sse/responses.rs try_parse_retry_after），否则退回 ~200ms 起跳的
// 本地指数退避——分钟级限流 episode 会在闩期内烧光重试预算。等待
// 时长与 unified-reset 头同源（分钟 hint 向上对齐桶界）。无 hint
// 或 reset 已过期的错误原样返回。failure 须非 nil——调用方先经
// failure.Error() 判空后才走到这里。
func RetryAfterHint(failure *llm.Failure, now time.Time) string {
	message := failure.Error()
	resetAt, ok := failure.RateLimitReset(now)
	if !ok {
		return message
	}
	wait := int(math.Ceil(time.Until(resetAt).Seconds()))
	if wait <= 0 {
		return message
	}
	return fmt.Sprintf("%s (try again in %ds)", message, wait)
}

// UpstreamErrorDetails 返回应附进错误对象的上游排障字段：
// upstream_trace_id（报障锚点）与 retry_after（限流重置秒数 hint）。
func UpstreamErrorDetails(failure *llm.Failure) map[string]any {
	details := map[string]any{}
	if failure == nil {
		return details
	}
	if failure.TraceID != "" {
		details["upstream_trace_id"] = failure.TraceID
	}
	if failure.RetryAfterSeconds > 0 {
		details["retry_after"] = failure.RetryAfterSeconds
	}
	return details
}

// StreamErrorOpenAI 产出 OpenAI 方言的流式失败前奏：error.type 用
// OpenAI 命名并附带 "param":null 字段。
func StreamErrorOpenAI(event llm.ResponseEvent, fallbackMessage string) (map[string]any, int) {
	return streamError(event, fallbackMessage, true)
}

// StreamErrorAnthropic 产出 Anthropic 方言的流式失败前奏：error.type 用
// Anthropic 命名，不带 "param" 字段。
func StreamErrorAnthropic(event llm.ResponseEvent, fallbackMessage string) (map[string]any, int) {
	return streamError(event, fallbackMessage, false)
}

// streamError 收敛三面流式失败帧的共用前奏：从终止事件取出分类记录，
// 限流消息统一补 "try again in Ns" 等待提示（fallback 是错误本身为空时
// 的兜底文案），产出 BuildErrorPayload 结果与对应 HTTP status。
// openAI 选定 OpenAI 方言（error.type 命名 + "param":null 字段），false
// 走 Anthropic 方言。各面 encoder 只负责把 payload 装进自己的 wire 帧。
func streamError(event llm.ResponseEvent, fallbackMessage string, openAI bool) (map[string]any, int) {
	failure := llm.FailureOf(event.Error)
	message := fallbackMessage
	if failure.Error() != "" {
		message = RetryAfterHint(failure, time.Now())
	}
	errorType, openAIParam := AnthropicErrorType(failure), false
	if openAI {
		errorType, openAIParam = OpenAIErrorType(failure), true
	}
	return BuildErrorPayload(message, failure, errorType, event.Error.DebugRef, openAIParam), HTTPStatus(failure)
}

// BuildErrorPayload 组装协议错误对象的 error 字段：message/type/code 三键、
// openAIParam 为 true 时附带 OpenAI 风格的 "param":null，再并入上游排障
// 字段（upstream_trace_id/retry_after）与调试目录引用 debug_ref。
// 三协议的错误共用同一份字段清单，避免各处抄写随演进漂移。
// message 是下发的完整文案（可能已由 RetryAfterHint 追加等待提示）。
func BuildErrorPayload(message string, failure *llm.Failure, errorType string, debugRef string, openAIParam bool) map[string]any {
	payload := map[string]any{
		"message": message,
		"type":    errorType,
		"code":    ErrorCode(failure),
	}
	if openAIParam {
		payload["param"] = nil
	}
	for key, value := range UpstreamErrorDetails(failure) {
		payload[key] = value
	}
	if debugRef != "" {
		payload["debug_ref"] = debugRef
	}
	return payload
}
