// 本文件提供把上游 Connect/gRPC 错误码映射到 OpenAI / Anthropic 错误类型的工具。
//
// 分类记录 llm.Failure 是错误语义的唯一载体：生产侧（adapter、rate gate）
// 在错误仍有类型时填结构事实，Classify 统一补齐派生字段；消费函数一律
// 接收 *llm.Failure 读字段。字符串解析（前缀 code、文案标记、hint 正则）
// 只存在于 classify 的兜底分支——此前全库按 "<code>: <msg>" 方言各自
// 重解析，每来一种新的上游错误形状就要同步多处。
package common

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

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

// Classify 把任意错误归一为分类记录：*llm.Failure 直取并补齐派生字段，
// *connect.Error 取结构字段，其余按文本兜底（前缀 code + 标记匹配）。
// 派生是幂等的纯计算，重复调用结果一致。
func Classify(err error) *llm.Failure {
	if err == nil {
		return nil
	}
	failure := &llm.Failure{Message: err.Error(), Cause: err}
	var typed *llm.Failure
	var connectErr *connect.Error
	switch {
	case errors.As(err, &typed):
		failure = typed
	case errors.As(err, &connectErr):
		// Message 不含 code 前缀，Failure.Error() 重组即原线文本；
		// 空 message 时不再回填 connectErr.Error()——那会引入前缀重复。
		failure.Message = strings.TrimSpace(connectErr.Message())
		failure.Code = connectErr.Code().String()
	default:
		if code, rest, ok := splitCodePrefix(failure.Message); ok {
			failure.Code, failure.Message = code, rest
		}
	}
	return derive(failure)
}

// FailureOf 返回错误助手消息的分类记录：优先用生产侧携带的 typed
// Failure，缺失时按 ErrorMessage 文本兜底分类。
func FailureOf(message *llm.AssistantMessage) *llm.Failure {
	if message != nil && message.Failure != nil {
		return derive(message.Failure)
	}
	text := ""
	if message != nil {
		text = message.ErrorMessage
	}
	return ClassifyText(text)
}

// ClassifyText 对无 error 载体的纯文案分类（事件层压平后的 ErrorMessage、
// 测试构造等）。
func ClassifyText(message string) *llm.Failure {
	failure := &llm.Failure{Message: message, Cause: errors.New(message)}
	if code, rest, ok := splitCodePrefix(message); ok {
		failure.Code, failure.Message = code, rest
	}
	return derive(failure)
}

// derive 按原始字段补齐派生字段；生产侧已填的字段（RetryAfterSeconds 等）
// 保持不变。
func derive(failure *llm.Failure) *llm.Failure {
	message := strings.ToLower(failure.Message)
	failure.ContextLength = false
	for _, marker := range contextLengthMarkers {
		if strings.Contains(message, marker) {
			failure.ContextLength = true
			break
		}
	}
	failure.RateLimited = failure.Code == "resource_exhausted" || failure.LocalGate
	failure.Canceled = failure.Code == "canceled" ||
		errors.Is(failure.Cause, context.Canceled) ||
		strings.Contains(message, "context canceled")
	failure.Timeout = failure.Code == "deadline_exceeded" ||
		errors.Is(failure.Cause, context.DeadlineExceeded) ||
		strings.Contains(message, "context deadline exceeded")
	failure.ClientFixable = failure.ContextLength || requestCodeSet[failure.Code]
	for _, marker := range clientFixableMarkers {
		if strings.Contains(message, marker) {
			failure.ClientFixable = true
			break
		}
	}
	if match := traceIDPattern.FindStringSubmatch(failure.Message); len(match) == 2 {
		failure.TraceID = match[1]
	}
	if failure.RetryAfterSeconds == 0 {
		failure.RetryAfterSeconds, failure.RetryAfterMinute = parseResetHint(failure.Message)
	}
	return failure
}

// connectCodes 是 Connect 协议全部错误码——文本兜底时只有前缀命中该集合
// 才认作 code，避免把 "read request: …" 之类本地文案误当体制标记。
var connectCodes = map[string]bool{
	"canceled": true, "unknown": true, "invalid_argument": true,
	"deadline_exceeded": true, "not_found": true, "already_exists": true,
	"permission_denied": true, "resource_exhausted": true,
	"failed_precondition": true, "aborted": true, "out_of_range": true,
	"unimplemented": true, "internal": true, "unavailable": true,
	"data_loss": true, "unauthenticated": true,
}

// requestCodeSet 是调用方可修正的请求错误 code 家族——与 HTTPStatus 的
// 4xx 分支同源。failed_precondition 实测是请求形状/前置状态问题（如非
// CASCADE request_type 缺真实会话），与 invalid_argument 同属可修正；
// Devin 上游把内容策略拦截、模型 UID 无效、模型未授权都归并到
// permission_denied，同样可归一。
var requestCodeSet = map[string]bool{
	"invalid_argument": true, "failed_precondition": true,
	"permission_denied": true,
}

// splitCodePrefix 把 "<code>: <msg>" 方言拆成结构字段：前缀命中已知
// Connect code 才成立，返回 code 与去掉前缀的正文——Message 存剥前缀的
// 正文，Failure.Error() 重组后与原线文本逐字节一致。
func splitCodePrefix(message string) (code, rest string, ok bool) {
	i := strings.Index(message, ":")
	if i < 0 {
		return "", "", false
	}
	code = strings.TrimSpace(message[:i])
	if !connectCodes[code] {
		return "", "", false
	}
	return code, strings.TrimSpace(message[i+1:]), true
}

// contextLengthMarkers 是上游表示"输入超出上下文窗口"的错误文案特征。
var contextLengthMarkers = []string{
	"prompt is too long", "context length", "context window",
	"maximum context", "too many tokens",
}

// clientFixableMarkers 是本地/适配器侧产生的调用方可修正错误文案特征
// （图片不支持、请求校验失败等）——上游未用 code 表达的部分靠标记兜底。
// "invalid_argument" 按子串而非前缀匹配：包裹文本（如 "AssignModel(uid):
// invalid_argument: …"）前缀不是 code 但语义不变。
var clientFixableMarkers = []string{
	"does not support image", "file_id images", "only data URL",
	"validate Devin request", "validate adapted request", "invalid_argument",
}

// OpenAIErrorType 把失败记录映射为 OpenAI error.type；无法识别时返回
// "server_error"。
func OpenAIErrorType(failure *llm.Failure) string {
	if failure != nil {
		if t, ok := openAIErrorTypes[failure.Code]; ok {
			return t
		}
	}
	return "server_error"
}

// AnthropicErrorType 把失败记录映射为 Anthropic error.type；无法识别时
// 返回 "api_error"。
func AnthropicErrorType(failure *llm.Failure) string {
	if failure != nil {
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
// 502 会污染指标并让网关误判渠道故障。
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

// rateLimitResetPattern 匹配上游限流文案里的重试窗口。实测两种单位：
// 剩余不足一分钟时报 "reset in N seconds"，更长时报 "reset in N
// minute(s)"（floor 取整）。上游不给 Retry-After 头或 RetryInfo
// detail，这句文案是唯一可行动的 hint。
var rateLimitResetPattern = regexp.MustCompile(`(?i)reset in (\d+)\s*(seconds?|minutes?)`)

// parseResetHint 从文案解析限流重置秒数与粒度；无 hint 返回 0。
// 返回的字面秒数（分钟按 60 折算）；要拿可行动的等待时长/绝对时刻用
// RateLimitReset——分钟 hint 是桶界的 floor 取整，需向上对齐。
func parseResetHint(message string) (seconds int, minute bool) {
	match := rateLimitResetPattern.FindStringSubmatch(message)
	if len(match) != 3 {
		return 0, false
	}
	n, err := strconv.Atoi(match[1])
	if err != nil || n <= 0 {
		return 0, false
	}
	if strings.HasPrefix(match[2], "minute") {
		return n * 60, true
	}
	return n, false
}

// RetryAfterHint 给限流错误追加 Codex 可解析的等待提示 " (try again
// in Ns)"：codex-rs 只在 error.code=="rate_limit_exceeded" 且 message
// 匹配 /try again in N(s|ms|seconds)/ 时才按服务端建议时刻睡眠重试
// （sse/responses.rs try_parse_retry_after），否则退回 ~200ms 起跳的
// 本地指数退避——分钟级限流 episode 会在闩期内烧光重试预算。等待
// 时长与 unified-reset 头同源（分钟 hint 向上对齐桶界）。无 hint
// 或 reset 已过期的错误原样返回。
func RetryAfterHint(failure *llm.Failure, now time.Time) string {
	message := failure.Error()
	resetAt, ok := RateLimitReset(failure, now)
	if !ok {
		return message
	}
	wait := int(math.Ceil(time.Until(resetAt).Seconds()))
	if wait <= 0 {
		return message
	}
	return fmt.Sprintf("%s (try again in %ds)", message, wait)
}

// RateLimitReset 把限流重置时刻解析为绝对时刻：生产侧已知的精确秒数
// 直接 now+N；分钟级 hint 是上游对当前分钟桶剩余时长的 floor 取整
// （"reset in 1 minute" 实际指本桶结束，最晚 ~119s 后），按上游分钟桶
// 模型向上对齐到下一个 :59 秒桶界——上游时钟约快 1s，实测桶界落在本地
// :58.5~:59.5。
func RateLimitReset(failure *llm.Failure, now time.Time) (time.Time, bool) {
	if failure == nil || failure.RetryAfterSeconds <= 0 {
		return time.Time{}, false
	}
	if !failure.RetryAfterMinute {
		return now.Add(time.Duration(failure.RetryAfterSeconds) * time.Second), true
	}
	// now+Nmin 落入的分钟桶的 :59 边界；若该时刻本身已过 :59，
	// 取下一个分钟的 :59。
	target := now.Add(time.Duration(failure.RetryAfterSeconds) * time.Second)
	reset := target.Truncate(time.Minute).Add(59 * time.Second)
	if !reset.After(target) {
		reset = reset.Add(time.Minute)
	}
	return reset, true
}

// traceIDPattern 匹配上游流内错误尾的 trace 标记 "(trace ID: …)"。
// 上游错误文案普遍是模糊 "internal error"，trace ID 是唯一排障锚点。
var traceIDPattern = regexp.MustCompile(`\(trace ID: ([^)\s]+)\)`)

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
