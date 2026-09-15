// 本文件是错误语义的唯一事实源：分类记录 Failure 随错误链随车携带，
// 压平成字符串之前的类型信息不再丢失；消费侧经 Classify 一次取回，
// 不再各自从文案反推。
//
// 字段分两层：Code/Message/Cause/LocalGate/UpstreamFault/
// RetryAfterSeconds/RetryAfterMinute 由生产侧（adapter、rate gate）填
// 结构已知的事实；其余派生字段由 Classify 统一补齐——直接读未经
// Classify 的记录时派生字段为零值。derive 不覆盖生产侧已置位的字段。
//
// 字符串解析（前缀 code、文案标记、hint 正则）只存在于 Classify 的
// 兜底分支——此前全库按 "<code>: <msg>" 方言各自重解析，每来一种
// 新的上游错误形状就要同步多处。
package llm

import (
	"context"
	"errors"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// Failure 是请求失败的分类记录，实现 error 接口。
type Failure struct {
	// Code 是 Connect 协议错误码（resource_exhausted 等）；非 Connect
	// 错误为空。文案前缀 "<code>: <msg>" 的方言由本字段替代。
	Code string
	// Message 是完整可读错误文案（不含 code 前缀）。
	Message string
	// Cause 是原始错误，errors.Is/As 链路（context.Canceled 等）不丢。
	Cause error
	// LocalGate 为真表示本地速率闸门拒绝——请求未触达上游，
	// 排障归因与上游真拒（devin_connect）区分。
	LocalGate bool
	// UpstreamFault 为真表示责任在上游侧，与 Code 声称的语义无关：
	// 传输断裂会被 connect-go 包成 invalid_argument/internal 文案，
	// 上游也把真实内部故障塞进可修正 code（"an internal error
	// occurred" 模板）——两者都不该让客户端按请求错误处理。
	UpstreamFault bool
	// RetryAfterSeconds 是生产侧结构已知的限流等待秒数（本地闸门）；
	// 上游只在文案里给 hint，由 Classify 解析补齐。
	RetryAfterSeconds int
	// RetryAfterMinute 为真表示 hint 以分钟粒度给出：重置时刻须按上游
	// 分钟桶界向上对齐（floor 取整的剩余时长），不能当精确秒数用。
	RetryAfterMinute bool

	// 以下由 Classify 派生填充。

	// ContextLength 表示请求超出上游上下文窗口——客户端可修正。
	ContextLength bool
	// RateLimited 表示限流类失败（resource_exhausted 或本地闸门）；
	// UpstreamFault 置位时恒假——传输断裂伪装的该 code（如 http2
	// ENHANCE_YOUR_CALM 被 connect-go 映成 resource_exhausted）不是
	// 上游限流信号。
	RateLimited bool
	// Canceled 表示客户端主动断连/取消。
	Canceled bool
	// Timeout 表示等待超时（context.DeadlineExceeded 或上游等价物）。
	Timeout bool
	// ClientFixable 表示客户端可修正的请求错误（4xx 家族 code、上下文
	// 超长）——错误类型与状态码据此压回 invalid_request/4xx。
	// UpstreamFault 置位时恒假。
	ClientFixable bool
	// TraceID 是上游错误尾缀 "(trace ID: …)" 提取出的排障锚点。
	TraceID string
	// ResetHint 表示上游文案携带了 reset 声明（含显式 0）——
	// RetryAfterSeconds==0 因此分两种语义：无声明，与「重置时刻即
	// 现在」。实测 "reset in 0 seconds" 全在桶界到达：新桶已爆、
	// 无追加封禁，RateLimitReset 对后者返回 now。
	ResetHint bool
}

// Error 保持既有线格式 "<code>: <msg>"——旧文本契约（日志、客户端
// 展示、尚存的文本兜底解析）不因记录化而改变。
func (e *Failure) Error() string {
	if e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

// Unwrap 暴露原始错误链，context.Canceled/DeadlineExceeded 等哨兵仍可判定。
func (e *Failure) Unwrap() error { return e.Cause }

// Classify 把任意错误归一为分类记录：*Failure 直取并补齐派生字段，
// *connect.Error 取结构字段，其余按文本兜底（前缀 code + 标记匹配）。
// 派生是幂等的纯计算，重复调用结果一致。
func Classify(err error) *Failure {
	if err == nil {
		return nil
	}
	failure := &Failure{Message: err.Error(), Cause: err}
	var typed *Failure
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
func FailureOf(message *AssistantMessage) *Failure {
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
func ClassifyText(message string) *Failure {
	failure := &Failure{Message: message, Cause: errors.New(message)}
	if code, rest, ok := splitCodePrefix(message); ok {
		failure.Code, failure.Message = code, rest
	}
	return derive(failure)
}

// derive 按原始字段补齐派生字段；生产侧已置位的字段保持不变——生产侧
// 能以文本标记以外的方式结构知道这些事实（本地闸门、上游细节字段）。
func derive(failure *Failure) *Failure {
	message := strings.ToLower(failure.Message)
	for _, marker := range contextLengthMarkers {
		if strings.Contains(message, marker) {
			failure.ContextLength = true
			break
		}
	}
	// connect-go 把对端 RST_STREAM CANCEL 也映成 canceled code（本地 ctx
	// 未取消时）——那是上游传输断裂而非客户端取消：命中 http2 传输措辞
	// 时 code 派生不成立，交回下方 transportBreak 判 UpstreamFault。
	codeCanceled := failure.Code == "canceled" && !IsHTTP2TransportError(failure.Message)
	failure.Canceled = failure.Canceled ||
		codeCanceled ||
		errors.Is(failure.Cause, context.Canceled) ||
		strings.Contains(message, "context canceled")
	failure.Timeout = failure.Timeout ||
		failure.Code == "deadline_exceeded" ||
		errors.Is(failure.Cause, context.DeadlineExceeded) ||
		strings.Contains(message, "context deadline exceeded")
	failure.UpstreamFault = failure.UpstreamFault ||
		strings.Contains(message, internalErrorMarker) ||
		transportBreak(failure)
	// UpstreamFault 置位时 code 声称的语义不可信：resource_exhausted
	// 也可能是传输断裂的伪装（http2 ENHANCE_YOUR_CALM），不能拿去
	// 上冷却闩或对客户端标 rate_limit_exceeded。
	failure.RateLimited = failure.RateLimited ||
		(!failure.UpstreamFault &&
			(failure.Code == "resource_exhausted" || failure.LocalGate))
	failure.ClientFixable = failure.ClientFixable ||
		(!failure.UpstreamFault &&
			(failure.ContextLength || requestCodeSet[failure.Code]))
	if match := traceIDPattern.FindStringSubmatch(failure.Message); len(match) == 2 {
		failure.TraceID = match[1]
	}
	if failure.RetryAfterSeconds == 0 {
		failure.RetryAfterSeconds, failure.RetryAfterMinute, failure.ResetHint = parseResetHint(failure.Message)
	}
	return failure
}

// internalErrorMarker 是上游内部故障的固定模板文案——Devin 把真实内部
// 错误塞进 invalid_argument/permission_denied 等可修正 code 下发，
// 文案是它唯一可靠的自报。
const internalErrorMarker = "an internal error occurred"

// transportBreak 判定传输层断裂：connect-go 把 RoundTrip/读写断包成
// CodeUnavailable、envelope 帧截断包成 CodeInvalidArgument "protocol
// error: ..."、流中段裸 EOF 包成 CodeUnknown、对端 RST_STREAM/GOAWAY
// 映成语义 code（REFUSED_STREAM→unavailable、ENHANCE_YOUR_CALM→
// resource_exhausted 等）——判据看 unwrap 链里的 io/net 错误与
// connect/http2 栈的固定措辞，而不是 code 本身。已归取消/超时的
// 不算传输故障。
func transportBreak(failure *Failure) bool {
	if failure.Canceled || failure.Timeout {
		return false
	}
	if errors.Is(failure.Cause, io.EOF) || errors.Is(failure.Cause, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	if errors.As(failure.Cause, &netErr) {
		return true
	}
	return IsHTTP2TransportError(failure.Message) ||
		IsIdleConnClosedError(failure.Message) ||
		((failure.Code == "invalid_argument" || failure.Code == "internal") &&
			strings.HasPrefix(failure.Message, "protocol error:"))
}

// http2TransportMarkers 是本地 http2 栈写进错误文案的固定措辞。
// connect-go 把对端 RST_STREAM 按尾缀 code 映射为语义 code（v1.20.0
// error.go wrapIfRSTError：REFUSED_STREAM→unavailable、
// PROTOCOL_ERROR/INTERNAL_ERROR 等→internal、ENHANCE_YOUR_CALM→
// resource_exhausted、INADEQUATE_SECURITY→permission_denied、
// CANCEL→canceled/deadline_exceeded）；vendored http2 类型不可导出，
// 只能认文案——形如 "stream error: stream ID N; CODE; received from
// peer"，ENHANCE_YOUR_CALM/INADEQUATE_SECURITY 外层再套
// "bandwidth exhausted: "/"transport protocol insecure: " 前缀，故只能
// Contains 不能 HasPrefix。GOAWAY 不走该映射，建连期以
// "unavailable: http2: server sent GOAWAY and closed the connection; ..."
// 透出。真正的上游语义错误经 EndStream 尾帧送达（connect 收到尾帧错误时
// 优先于传输错误返回，protocol_connect.go Receive 的 serverErr 分支），
// 不会携带这些本地措辞——命中即传输断裂，映射出的 code 与语义无关。
var http2TransportMarkers = []string{
	"stream error: stream ID ",
	"http2: server sent GOAWAY",
}

// IsHTTP2TransportError 判定错误文案是否携带本地 http2 栈的传输措辞
// （RST_STREAM/GOAWAY）。connect.Error 的 Message() 或整条 Error()
// 文本都可传入——标记串不会出现在 code 前缀里。
func IsHTTP2TransportError(message string) bool {
	for _, marker := range http2TransportMarkers {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// IsIdleConnClosedError 判定错误文案是否为 net/http 连接池的
// errServerClosedIdle 固定措辞（"http: server closed idle connection"）：
// 池复用到对端已关闭的空闲连接时报出，h1 池（force_http1）特有——
// 失败发生在任何字节写出之前，与 RST/GOAWAY 同属传输断裂，重试安全。
// h2 无此形态：GOAWAY 先于复用竞争到达。
func IsIdleConnClosedError(message string) bool {
	return strings.Contains(message, "server closed idle connection")
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

// traceIDPattern 匹配上游流内错误尾的 trace 标记 "(trace ID: …)"。
// 上游错误文案普遍是模糊 "internal error"，trace ID 是唯一排障锚点。
var traceIDPattern = regexp.MustCompile(`\(trace ID: ([^)\s]+)\)`)

// rateLimitResetPattern 匹配上游限流文案里的重试窗口。实测两种单位：
// 剩余不足一分钟时报 "reset in N seconds"，更长时报 "reset in N
// minute(s)"（floor 取整）。上游不给 Retry-After 头或 RetryInfo
// detail，这句文案是唯一可行动的 hint。
var rateLimitResetPattern = regexp.MustCompile(`(?i)reset in (\d+)\s*(seconds?|minutes?)`)

// parseResetHint 从文案解析限流重置声明，返回三态：无 hint（ok=false）、
// 显式 0（ok=true 且 seconds=0——上游在桶界到达时报 "reset in 0 seconds"，
// 语义是「新桶已爆、无追加封禁」，重置时刻即现在）、正数等待。
// 返回字面秒数（分钟按 60 折算）；要拿可行动的等待时长/绝对时刻用
// RateLimitReset——分钟 hint 是桶界剩余时长的 floor 取整，需向上对齐。
func parseResetHint(message string) (seconds int, minute bool, ok bool) {
	match := rateLimitResetPattern.FindStringSubmatch(message)
	if len(match) != 3 {
		return 0, false, false
	}
	n, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false, false
	}
	if strings.HasPrefix(match[2], "minute") {
		return n * 60, true, true
	}
	return n, false, true
}

// RateLimitReset 把限流重置时刻解析为绝对时刻：生产侧已知的精确秒数
// 直接 now+N；分钟级 hint 是上游对当前分钟桶剩余时长的 floor 取整
// （"reset in 1 minute" 实际指本桶结束，最晚 ~119s 后），按上游分钟桶
// 模型向上对齐到下一个 :59 秒桶界——上游时钟约快 1s，实测桶界落在本地
// :58.5~:59.5。显式 0 秒声明（"reset in 0 seconds"，ResetHint 置位）
// 返回 now——冷却闩按声明时刻即刻过期，而不是套兜底闩时长。
func (failure *Failure) RateLimitReset(now time.Time) (time.Time, bool) {
	if failure == nil {
		return time.Time{}, false
	}
	if failure.RetryAfterSeconds <= 0 && !failure.ResetHint {
		return time.Time{}, false
	}
	if failure.RetryAfterMinute {
		// now+Nmin 落入的分钟桶的 :59 边界；若该时刻本身已过 :59，
		// 取下一个分钟的 :59。N=0（"reset in 0 minutes"）对齐到本桶
		// :59——分钟粒度的 0 是 floor 取整，真实剩余最长 ~59s。
		target := now.Add(time.Duration(failure.RetryAfterSeconds) * time.Second)
		reset := target.Truncate(time.Minute).Add(59 * time.Second)
		if !reset.After(target) {
			reset = reset.Add(time.Minute)
		}
		return reset, true
	}
	if failure.RetryAfterSeconds <= 0 {
		// 显式 0 秒：声明的重置时刻即现在。
		return now, true
	}
	return now.Add(time.Duration(failure.RetryAfterSeconds) * time.Second), true
}
