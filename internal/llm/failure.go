package llm

// Failure 是请求失败的分类记录，实现 error 接口随错误链随车携带——
// 压平成字符串之前的类型信息不再丢失，消费侧经 common.Classify 一次取回，
// 不再各自从文案反推。
//
// 字段分两层：Code/Message/Cause/LocalGate/RetryAfterSeconds/RetryAfterMinute
// 由生产侧（adapter、rate gate）填结构已知的事实；其余派生字段由
// common.Classify 统一补齐——直接读未经 Classify 的记录时派生字段为零值。
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
	// RetryAfterSeconds 是生产侧结构已知的限流等待秒数（本地闸门）；
	// 上游只在文案里给 hint，由 Classify 解析补齐。
	RetryAfterSeconds int
	// RetryAfterMinute 为真表示 hint 以分钟粒度给出：重置时刻须按上游
	// 分钟桶界向上对齐（floor 取整的剩余时长），不能当精确秒数用。
	RetryAfterMinute bool

	// 以下由 common.Classify 派生填充。

	// ContextLength 表示请求超出上游上下文窗口——客户端可修正。
	ContextLength bool
	// RateLimited 表示限流类失败（resource_exhausted 或本地闸门）。
	RateLimited bool
	// Canceled 表示客户端主动断连/取消。
	Canceled bool
	// Timeout 表示等待超时（context.DeadlineExceeded 或上游等价物）。
	Timeout bool
	// ClientFixable 表示客户端可修正的请求错误（4xx 家族 code、上下文
	// 超长、本地校验标记）——错误类型与状态码据此压回 invalid_request/4xx。
	ClientFixable bool
	// TraceID 是上游错误尾缀 "(trace ID: …)" 提取出的排障锚点。
	TraceID string
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
