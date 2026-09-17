// 本文件定义跨请求日志行的写路径：每完成一个请求向 store 的 logs
// 表插一行摘要（取代旧 index.jsonl 追加）。行形状是 store.LogRow——
// 它同时是读路径的 wire 投影（json tag 即对外字段名），不再另设 DTO。
//
// 有了索引行后，定位请求从「遍历目录逐个翻 meta.json」变成一次
// SQL 查询；面板的列表/聚合全部直接读表。
package debuglog

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// 日志行 log_source 的定版值：写入时由 insertLog 按客户端关联 ID 归类——
// 面板探活记 manual_test，与 ccLoad 同语义，不计入默认 proxy 视图。
const (
	LogSourceProxy      = "proxy"
	LogSourceManualTest = "manual_test"
)

// errorMessageCap 是日志行 error_message 的截断字节数：保留首个失败
// 的可归因文本，又不让超大错误文案把行撑变形。
const errorMessageCap = 300

// insertLog 在请求完成后把摘要行插入 logs 表。
// store 为 nil（测试或 DB 未接线）时静默跳过：日志行是观测副本，
// 不该反过来决定请求能否完结——失败只记 ioErrors。
func (manager *Manager) insertLog(recorder *Recorder, completion *Completion) {
	dir := recorder.dir
	if manager.store == nil || dir == "" {
		return
	}
	account, accountAttempts := recorder.upstreamAttribution()
	row := store.LogRow{
		Dir:               dir,
		StartedAt:         recorder.startedAt,
		DurationMS:        time.Since(recorder.startedAt).Milliseconds(),
		RequestReadyMS:    optionalLatency(recorder.requestReadyMS.Load()),
		UpstreamSentMS:    optionalLatency(recorder.upstreamSentMS.Load()),
		UpstreamOpenMS:    optionalLatency(recorder.upstreamOpenMS.Load()),
		FirstUpstreamMS:   optionalLatency(recorder.firstUpstreamMS.Load()),
		FirstClientMS:     optionalLatency(recorder.firstClientMS.Load()),
		API:               recorder.requestMeta.API,
		Method:            recorder.requestMeta.Method,
		Path:              recorder.requestMeta.Path,
		StatusCode:        completion.StatusCode,
		Result:            completion.Result,
		RequestedModel:    completion.RequestedModel,
		Model:             completion.Model,
		ResponseModel:     completion.ResponseModel,
		ModelMismatch:     completion.ModelMismatch,
		Stream:            completion.Stream,
		InputTokens:       completion.Usage.Input,
		OutputTokens:      completion.Usage.Output,
		CacheReadTokens:   completion.Usage.CacheRead,
		CacheWriteTokens:  completion.Usage.CacheWrite,
		ReasoningTokens:   reasoningTokens(completion.Usage),
		TotalTokens:       completion.Usage.TotalTokens,
		CreditCost:        creditCost(completion.Usage),
		UpstreamRequestID: completion.UpstreamRequestID,
		ClientIP:          recorder.requestMeta.ClientIP,
		KeyHash:           recorder.effectiveKeyHash(),
		ClientRequestID:   recorder.requestMeta.ClientRequestID,
		// 来源分类在这里定版（面板探活归 manual_test）：业务口径归
		// debuglog 写方，store.InsertLog 原样落字段。
		LogSource:         LogSourceProxy,
		DroppedEvents:     recorder.dropped.Load(),
		RetryAfterSeconds: recorder.retryAfterSeconds.Load(),
		RateLimited:       recorder.rateLimited.Load(),
		Retries:           len(recorder.retryAttempts()),
		Account:           account,
		AccountSwitches:   len(accountAttempts),
		PrematureEndTurn:  completion.PrematureEndTurn,
	}
	if row.ClientRequestID == ProbeClientRequestID {
		row.LogSource = LogSourceManualTest
	}
	if completion.Result != "completed" {
		// error 字段只对终结性失败出账：被重试救回的中间错误留在目录
		// error.json 与 meta.retry_attempts，不污染按失败点检索的口径。
		if stage, message := recorder.FirstError(); stage != "" {
			row.ErrorStage = stage
			row.ErrorMessage = truncateRunes(message, errorMessageCap)
		}
	}
	if conn := recorder.upstreamConn.Load(); conn != nil {
		row.ConnReused = &conn.reused
		row.ConnIdleMS = &conn.idleMS
	}
	if repairs := recorder.repairs.Load(); repairs != nil {
		row.Repairs = repairs.Total()
	}
	if _, err := manager.store.InsertLog(context.Background(), &row); err != nil {
		manager.ioErrors.Add(1)
	}
}

// isRateLimited 判定日志行是否被限流语义终结：HTTP 429（上游真拒或本地
// 闸门快败），或 200+流内错误事件下发的限流——后者靠 rate_limited
// 标记认出（recorder 在记录错误时按文案语义置位）。
// 判定只用行字段（result/status/error_stage/rate_limited）。
func isRateLimited(e *store.LogRow) bool {
	return e.StatusCode == 429 || e.RateLimited
}

// ErrorOwner 把一条日志记录按失败责任归因（对齐 sub2api 的 error_owner +
// is_business_limited 双标记，压缩成单维三值）。面板经 matrix 条目的
// owner 字段直接消费，JS 不再复刻这份判定。
//   - "client"：客户端断连/面板中断，或请求体读取与解码阶段的失败——
//     还没碰到上游，责任在调用方；
//   - "business_limited"：429（本地闩快败或上游限流）——配额动作不是
//     服务质量故障，SLA 分母剔除；
//   - "upstream"：其余失败（上游 5xx/语义错误/transport 断裂/代理自身
//     编码失败）——SLA 口径里唯一算失分的类别；
//   - ""：非失败请求。
func ErrorOwner(e *store.LogRow) string {
	if isRateLimited(e) {
		return "business_limited"
	}
	if e.Result == "disconnected" || e.Result == "aborted" {
		return "client"
	}
	if e.StatusCode < 400 && e.Result != "failed" {
		return ""
	}
	if e.ErrorStage == ErrStageHTTPRead || e.ErrorStage == ErrStageHTTPDecode {
		return "client"
	}
	return "upstream"
}

// reasoningTokens 展开 Usage.Reasoning 指针为整数值。
func reasoningTokens(usage llm.Usage) int64 {
	if usage.Reasoning == nil {
		return 0
	}
	return *usage.Reasoning
}

// creditCost 展开 Usage.Costs 的单请求计费读数；上游未上报时为 0。
func creditCost(usage llm.Usage) int64 {
	if usage.Costs == nil {
		return 0
	}
	return usage.Costs.CreditCost
}

// optionalLatency 把 -1 哨兵转成 nil，其余原样透传（含合法的 0ms）。
func optionalLatency(ms int64) *int64 {
	if ms < 0 {
		return nil
	}
	return &ms
}

// truncateRunes 按字节截断到 cap，但不在多字节 rune 中间切断。
func truncateRunes(s string, cap int) string {
	if len(s) <= cap {
		return s
	}
	cut := cap
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// releaseDir 把目录移出活跃集合，允许清理器回收它。
func (manager *Manager) releaseDir(dir string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	delete(manager.activeDirs, dir)
}
