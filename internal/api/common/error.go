// 本文件提供把上游 Connect/gRPC 错误码映射到 OpenAI / Anthropic 错误类型的工具。
package common

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
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

// extractErrorCode 从 "<code>: <message>" 形式的消息中提取 code。
// 如果不是 Connect 错误格式，返回空字符串。
func extractErrorCode(message string) string {
	if i := strings.Index(message, ":"); i >= 0 {
		return strings.TrimSpace(message[:i])
	}
	return ""
}

// OpenAIErrorType 把上游错误消息中的 Connect code 映射为 OpenAI error.type。
// 无法识别时返回 "server_error"。
func OpenAIErrorType(message string) string {
	if t, ok := openAIErrorTypes[extractErrorCode(message)]; ok {
		return t
	}
	return "server_error"
}

// AnthropicErrorType 把上游错误消息中的 Connect code 映射为 Anthropic error.type。
// 无法识别时返回 "api_error"。
func AnthropicErrorType(message string) string {
	if t, ok := anthropicErrorTypes[extractErrorCode(message)]; ok {
		return t
	}
	return "api_error"
}

// contextLengthMarkers 是上游表示"输入超出上下文窗口"的错误文案特征。
var contextLengthMarkers = []string{
	"prompt is too long", "context length", "context window",
	"maximum context", "too many tokens",
}

// IsContextLengthError 判断错误消息是否表示请求超出上下文长度。
// 这类错误换渠道/换 Key 重试结果相同，属于客户端可修正的请求问题。
func IsContextLengthError(message string) bool {
	lower := strings.ToLower(message)
	for _, marker := range contextLengthMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// HTTPStatus 把上游错误消息映射为建议的 HTTP 状态码。
// 同一份映射同时用于响应行状态与流式错误事件里的 status 字段：
// 下游网关（如 ccload）按状态码区分"请求级错误"与"渠道故障"——
// 4xx 不冷却整个渠道；上下文超长给 413 并配合 error.code 让网关
// 直接归类为客户端问题，不做任何冷却。
// 无法识别的错误返回 502，表示上游服务故障。
func HTTPStatus(message string) int {
	switch {
	case strings.Contains(message, "invalid_argument"),
		strings.Contains(message, "failed_precondition"):
		// failed_precondition 实测是请求形状/前置状态问题（如非 CASCADE
		// request_type 缺真实会话），与 invalid_argument 同属调用方可修正。
		if IsContextLengthError(message) {
			return http.StatusRequestEntityTooLarge
		}
		return http.StatusBadRequest
	case strings.Contains(message, "unauthenticated"):
		return http.StatusUnauthorized
	case strings.Contains(message, "permission_denied"):
		// Devin 上游把内容策略拦截、模型 UID 无效、模型未授权都归并到
		// permission_denied。这三类都是调用方可修正的请求错误，
		// 归一成 400 而不是 403：下游网关（如 ccload）对 4xx 只按
		// 模型作用域冷却，不会把整个渠道标记为失效。
		return http.StatusBadRequest
	case strings.Contains(message, "not_found"):
		return http.StatusNotFound
	case strings.Contains(message, "resource_exhausted"):
		return http.StatusTooManyRequests
	default:
		return http.StatusBadGateway
	}
}

// ErrorCode 返回错误对象的 code 字段值；上下文超长统一为
// "context_length_exceeded"——与 OpenAI/Anthropic 惯例一致，也让下游
// 网关能把 SSE 错误事件识别为请求级问题而非渠道故障。其他错误返回 nil。
func ErrorCode(message string) any {
	if IsContextLengthError(message) {
		return "context_length_exceeded"
	}
	return nil
}

// rateLimitResetPattern 匹配上游限流文案里的重试窗口
// （"Your limit will reset in N seconds."）。上游不给 Retry-After 头
// 或 RetryInfo detail，这句文案是唯一可行动的 hint。
var rateLimitResetPattern = regexp.MustCompile(`(?i)reset in (\d+) seconds`)

// RetryAfterSeconds 从上游错误文案解析限流重置秒数；无 hint 返回 false。
func RetryAfterSeconds(message string) (int, bool) {
	match := rateLimitResetPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return 0, false
	}
	seconds, err := strconv.Atoi(match[1])
	if err != nil || seconds <= 0 {
		return 0, false
	}
	return seconds, true
}

// traceIDPattern 匹配上游流内错误尾的 trace 标记 "(trace ID: …)"。
// 上游错误文案普遍是模糊 "internal error"，trace ID 是唯一排障锚点。
var traceIDPattern = regexp.MustCompile(`\(trace ID: ([^)\s]+)\)`)

// UpstreamTraceID 从错误文案提取上游 trace ID；没有返回空串。
func UpstreamTraceID(message string) string {
	match := traceIDPattern.FindStringSubmatch(message)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

// UpstreamErrorDetails 返回应附进错误对象的上游排障字段：
// upstream_trace_id（报障锚点）与 retry_after（限流重置秒数 hint）。
func UpstreamErrorDetails(message string) map[string]any {
	details := map[string]any{}
	if traceID := UpstreamTraceID(message); traceID != "" {
		details["upstream_trace_id"] = traceID
	}
	if seconds, ok := RetryAfterSeconds(message); ok {
		details["retry_after"] = seconds
	}
	return details
}
