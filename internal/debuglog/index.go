// 本文件定义跨请求日志行的写路径：每完成一个请求向 store 的 logs
// 表插一行摘要（取代旧 index.jsonl 追加）。行形状是 store.LogRow——
// 它同时是读路径的 wire 投影（json tag 即对外字段名），不再另设 DTO。
//
// 有了索引行后，定位请求从「遍历目录逐个翻 meta.json」变成一次
// SQL 查询；面板的列表/聚合全部直接读表。
package debuglog

import (
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// 日志行 log_source 的定版值：写入时由 logRowFor 按客户端关联 ID 归类——
// 面板探活记 manual_test，与 ccLoad 同语义，不计入默认 proxy 视图。
const (
	LogSourceProxy      = "proxy"
	LogSourceManualTest = "manual_test"
)

// errorMessageCap 是日志行 error_message 的截断字节数：保留首个失败
// 的可归因文本，又不让超大错误文案把行撑变形。
const errorMessageCap = 300

// logRowFor 构建完成请求的 logs 摘要行；store 为 nil（测试或 DB 未
// 接线）或 dir 为空时返回 nil——日志行是观测副本，不反向决定请求
// 能否完结。在收尾 op 入列时刻调用：duration 等时点字段取完成瞬间
// 口径，不随批量冲刷的等待漂移。落库由写 worker 合并进批量事务
// （runWriter → flushAll），失败重试随批次走。
func (manager *Manager) logRowFor(recorder *Recorder, completion *Completion) *store.LogRow {
	if manager.store == nil || recorder.dir == "" {
		return nil
	}
	account, accountAttempts := recorder.upstreamAttribution()
	row := &store.LogRow{
		Dir:               recorder.dir,
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
		// debuglog 写方，store 侧原样落字段。
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
	// 被放弃尝试同时投影成 lane_attempt_causes 的写方载体：meta.json
	// 的 upstream_attempts 随目录淘汰，持久「为什么换号」口径只剩
	// store 展开的聚合账（与 AccountSwitches 同源同计数）。
	for _, a := range accountAttempts {
		if row.SwitchCauses == nil {
			row.SwitchCauses = map[store.SwitchCause]int{}
		}
		row.SwitchCauses[store.SwitchCause{Lane: a.Account, Cause: switchCauseKey(a)}]++
	}
	return row
}

// switchCauseKey 把一次被放弃 lane 尝试压成 lane_attempt_causes 的
// cause 词（词表归本写方定版）：local_gate[:reason] 是本地闸门快败
// 的幻影换号——零上游发送，Code 同样是 resource_exhausted，真假
// 限流靠 LocalGate 分；connect code 是真实 failover 发送；nocode
// 是无 code 的传输断裂类（failoverable 放行 UpstreamFault）。
func switchCauseKey(a AccountAttempt) string {
	if a.LocalGate {
		if a.GateReason != "" {
			return "local_gate:" + a.GateReason
		}
		return "local_gate"
	}
	if a.Code != "" {
		return a.Code
	}
	return "nocode"
}

// NoteReject 把一次管线前拒绝（鉴权 401/并发 429/排空 503/WS 准入/
// 读体中断）落为 logs 表一行：dir 留空（没有调试目录），result 记
// rejected，log_source=rejected 把它与服役流量分域——默认列表与全部
// 聚合口径剔除，只为留存检索（此前拒绝的跨重启痕迹只剩 stderr.log，
// 无 key/来源维度可查）。同步写而非走全局队列：拒绝发生在 recorder
// 创建之前，无目录可排；拒绝低频，写库失败只记 ioErrors。
func (manager *Manager) NoteReject(meta RequestMeta, status int, reason string) {
	if manager == nil || manager.store == nil {
		return
	}
	row := store.LogRow{
		LogSource:       "rejected",
		StartedAt:       manager.now(),
		API:             meta.API,
		Method:          meta.Method,
		Path:            meta.Path,
		StatusCode:      status,
		Result:          "rejected",
		ClientIP:        meta.ClientIP,
		KeyHash:         meta.KeyHash,
		ClientRequestID: meta.ClientRequestID,
		ErrorStage:      ErrStagePrePipeline,
		ErrorMessage:    truncateRunes(reason, errorMessageCap),
	}
	ctx, cancel := storeCtx()
	defer cancel()
	if _, err := manager.store.InsertLog(ctx, &row); err != nil {
		manager.ioErrors.Add(1)
		slog.Warn("debuglog: insert rejected log failed", "path", meta.Path, "error", err)
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
	// rejected 行是管线前拒绝的留存记录：既非客户端断连也非上游
	// 失分，观测面在 rejects 计数与事件环，责任归因恒为空。
	if e.Result == "rejected" {
		return ""
	}
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

// releaseDir 把目录移出活跃集合，允许清理器回收它；同时退回本目录
// 钉住的 delta 基座预算。只在覆盖收尾的批量事务提交（或无内容可提交）
// 后由 flushAll 调用——提前解除会让未落库的暂存目录失去活跃保护，被
// 容量淘汰删掉造成丢数据窗口。此刻本目录全部编码任务已随排空哨兵
// 收尾，deltaBase 不会再有读者。
func (manager *Manager) releaseDir(recorder *Recorder) {
	if recorder.deltaBase != nil {
		manager.deltaBaseBytes.Add(-int64(len(recorder.deltaBase)))
		recorder.deltaBase = nil
	}
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	delete(manager.activeDirs, recorder.dir)
}

// drainedClosed 是已关闭通道的单例：Drained 对未知或已释放的目录
// 直接返回它，调用方统一 <- 等待而不必判空。
var drainedClosed = func() chan struct{} {
	c := make(chan struct{})
	close(c)
	return c
}()

// Drained 返回在 dir 的完成收尾首次落库事务 resolve（提交或失败放行）
// 后关闭的通道；dir 不在活跃集（从未存在或已释放）时返回已关闭通道。
// Complete 不再阻塞排空——需要「payload 已可对外读」语义的调用方
// （取证导出、测试断言）在请求完结后自行 <- 等待；对仍在进行中的
// 请求调用会阻塞到它完结落库。
func (manager *Manager) Drained(dir string) <-chan struct{} {
	if manager == nil {
		return drainedClosed
	}
	manager.mutex.Lock()
	recorder := manager.activeDirs[dir]
	manager.mutex.Unlock()
	if recorder == nil {
		return drainedClosed
	}
	return recorder.drained
}
