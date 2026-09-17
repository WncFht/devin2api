// 本文件定义跨请求日志行的领域形状与写路径：每完成一个请求向
// store 的 logs 表插一行摘要（取代旧 index.jsonl 追加）。
//
// 有了索引行后，定位请求从「遍历目录逐个翻 meta.json」变成一次
// SQL 查询；面板的列表/聚合全部直接读表。
package debuglog

import (
	"context"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// IndexEntry 是一条请求日志行的面板投影形状——与 logs 表列一一对应，
// 也是 /admin/logs/matrix 与 export 的 wire 元素。
// 字段选择面向「grep 定位 + 面板列表」两个用途。
type IndexEntry struct {
	// ID 是 logs 表自增行号（面板 log_id 与 last_*_id 的身份）；
	// 只在读路径回填，不随 JSON 出账——wire 上的 id 字段由投影层命名。
	ID int64 `json:"-"`
	// LogSource 是写入时定版的来源（proxy/manual_test）；读路径回填。
	LogSource string `json:"-"`
	// UpstreamProtocol 保留过滤维度的统一形状（当前恒 devin）；读路径回填。
	UpstreamProtocol string `json:"-"`

	Dir        string `json:"dir"`
	StartedAt  string `json:"started_at"`
	DurationMS int64  `json:"duration_ms"`
	// 延迟分解字段用指针区分「未发生」（nil，省略）与「即时发生」（0ms）；
	// int 零值会掩盖这两种语义。五段口径见 recorder.go 同名字段注释：
	// ready→sent 本地投影、sent→open 建流往返、open→first_upstream 上游
	// 思考 TTFT、first_upstream→first_client 代理编码下发。
	RequestReadyMS   *int64 `json:"request_ready_ms,omitempty"`
	UpstreamSentMS   *int64 `json:"upstream_sent_ms,omitempty"`
	UpstreamOpenMS   *int64 `json:"upstream_open_ms,omitempty"`
	FirstUpstreamMS  *int64 `json:"first_upstream_ms,omitempty"`
	FirstClientMS    *int64 `json:"first_client_ms,omitempty"`
	API              string `json:"api,omitempty"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	StatusCode       int    `json:"status_code"`
	Result           string `json:"result"`
	RequestedModel   string `json:"requested_model,omitempty"`
	Model            string `json:"model,omitempty"`
	ResponseModel    string `json:"response_model,omitempty"`
	ModelMismatch    bool   `json:"model_mismatch,omitempty"`
	Stream           bool   `json:"stream"`
	InputTokens      int64  `json:"input_tokens,omitempty"`
	OutputTokens     int64  `json:"output_tokens,omitempty"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
	// CreditCost 是上游帧上报的权威计费读数（单请求口径，可加总）；
	// Committed* 系账户快照读数不进索引——逐请求保留见 meta.json。
	CreditCost        int64  `json:"credit_cost,omitempty"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
	ClientIP          string `json:"client_ip,omitempty"`
	KeyHash           string `json:"key_hash,omitempty"`
	// ClientRequestID 是客户端自带的关联 ID（X-Request-Id 等），
	// 让调用方能用自己的 ID 反查本次请求。
	ClientRequestID string `json:"client_request_id,omitempty"`
	// ErrorStage 是首个失败阶段（http_decode/provider_stream/http_stream 等），
	// 让检索直接定位失败发生在哪一层。只在终结性失败（result!=completed）
	// 时落盘：中途被重试救回的错误仍留在目录 error.json 与 retry_attempts
	// 里，进索引会把「发生过失败」与「请求失败」混成一桶。
	ErrorStage string `json:"error_stage,omitempty"`
	// ErrorMessage 是首个失败的错误文案（与 error.json 的 message 同源，
	// 截断至 errorMessageCap 字节）。目录被保留策略淘汰后，日志行仍能
	// 回答「为什么败」——此前只剩阶段名，归因必须靠目录在场。
	ErrorMessage  string `json:"error_message,omitempty"`
	DroppedEvents uint64 `json:"dropped_events,omitempty"`
	// RetryAfterSeconds 是上游限流给出的 reset 秒数 hint，
	// 供聚合区分「有退避提示的限流」与「裸限流」；非限流请求为 0。
	RetryAfterSeconds int64 `json:"retry_after_seconds,omitempty"`
	// RateLimited 标记本请求被限流语义终结（上游 429 / 本地闸门 / 流内
	// 限流错误事件）。HTTP 状态码认不全限流——流内下发的限流仍是 200，
	// 责任归因与限流采样用本字段而不是 status_code。
	RateLimited bool `json:"rate_limited,omitempty"`
	// Retries 是上游重发次数（attempt2+，token 自愈/空响应/transport
	// 重开）；明细在同目录 meta.json 的 retry_attempts 与 04 的
	// retry_attempt 分界行。0 表示一次发送完成。
	Retries int `json:"retries,omitempty"`
	// Account 是最终服务请求的上游账号名（号池 lane 身份，单号部署
	// 恒为 "default"）——「哪号在扛」的聚合不必区分部署形态。
	Account string `json:"account,omitempty"`
	// AccountSwitches 是号池 failover 换号次数（成功前的失败尝试数），
	// 明细在同目录 meta.json 的 upstream_attempts。0 表示首号即成。
	AccountSwitches int `json:"account_switches,omitempty"`
	// PrematureEndTurn 标记「工具结果之后模型纯文本 end_turn」的可疑收尾，
	// 供检索统计该模型行为的真实频率（见 Completion 同名字段）。
	PrematureEndTurn bool `json:"premature_end_turn,omitempty"`
	// Repairs 是请求投影为上游 wire 格式时的静默修复动作总数
	//（重排/降级/剥离/指纹改写），明细在同名 meta.json 字段。
	Repairs int `json:"repairs,omitempty"`
	// ConnReused 标记成功建流那次发送是否复用了 idle 连接；指针是为了
	// 区分「未记录」（nil，省略）与「复用失败新建」（false）——connect
	// 段偏高时靠它区分「握手成本」与「上游响应头延迟」。
	ConnReused *bool  `json:"conn_reused,omitempty"`
	ConnIdleMS *int64 `json:"conn_idle_ms,omitempty"`
}

// errorMessageCap 是日志行 error_message 的截断字节数：保留首个失败
// 的可归因文本，又不让超大错误文案把行撑变形。
const errorMessageCap = 300

// insertLog 在请求完成后把摘要行插入 logs 表。
// store 为 nil（测试或 DB 未接线）时静默跳过：日志行是观测副本，
// 不该反过来决定请求能否完结——失败只记 ioErrors。
func (manager *Manager) insertLog(recorder *Recorder, completion *Completion) {
	dir := filepath.Base(recorder.directory)
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
		DroppedEvents:     recorder.dropped.Load(),
		RetryAfterSeconds: recorder.retryAfterSeconds.Load(),
		RateLimited:       recorder.rateLimited.Load(),
		Retries:           len(recorder.retryAttempts()),
		Account:           account,
		AccountSwitches:   len(accountAttempts),
		PrematureEndTurn:  completion.PrematureEndTurn,
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

// IndexEntryFromRow 把 logs 表读回的行投影成 IndexEntry——matrix、
// export 与测试断言共用的读路径 DTO 转换。
func IndexEntryFromRow(row *store.LogRow) IndexEntry {
	return IndexEntry{
		ID:                row.ID,
		LogSource:         row.LogSource,
		UpstreamProtocol:  row.UpstreamProtocol,
		Dir:               row.Dir,
		StartedAt:         row.StartedAt.Format(time.RFC3339Nano),
		DurationMS:        row.DurationMS,
		RequestReadyMS:    row.RequestReadyMS,
		UpstreamSentMS:    row.UpstreamSentMS,
		UpstreamOpenMS:    row.UpstreamOpenMS,
		FirstUpstreamMS:   row.FirstUpstreamMS,
		FirstClientMS:     row.FirstClientMS,
		API:               row.API,
		Method:            row.Method,
		Path:              row.Path,
		StatusCode:        row.StatusCode,
		Result:            row.Result,
		RequestedModel:    row.RequestedModel,
		Model:             row.Model,
		ResponseModel:     row.ResponseModel,
		ModelMismatch:     row.ModelMismatch,
		Stream:            row.Stream,
		InputTokens:       row.InputTokens,
		OutputTokens:      row.OutputTokens,
		CacheReadTokens:   row.CacheReadTokens,
		CacheWriteTokens:  row.CacheWriteTokens,
		ReasoningTokens:   row.ReasoningTokens,
		TotalTokens:       row.TotalTokens,
		CreditCost:        row.CreditCost,
		UpstreamRequestID: row.UpstreamRequestID,
		ClientIP:          row.ClientIP,
		KeyHash:           row.KeyHash,
		ClientRequestID:   row.ClientRequestID,
		ErrorStage:        row.ErrorStage,
		ErrorMessage:      row.ErrorMessage,
		DroppedEvents:     row.DroppedEvents,
		RetryAfterSeconds: row.RetryAfterSeconds,
		RateLimited:       row.RateLimited,
		Retries:           row.Retries,
		Account:           row.Account,
		AccountSwitches:   row.AccountSwitches,
		PrematureEndTurn:  row.PrematureEndTurn,
		Repairs:           row.Repairs,
		ConnReused:        row.ConnReused,
		ConnIdleMS:        row.ConnIdleMS,
	}
}

// isRateLimited 判定日志行是否被限流语义终结：HTTP 429（上游真拒或本地
// 闸门快败），或 200+流内错误事件下发的限流——后者靠 rate_limited
// 标记认出（recorder 在记录错误时按文案语义置位）。
// 判定只用行字段（result/status/error_stage/rate_limited）。
func isRateLimited(e IndexEntry) bool {
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
func ErrorOwner(e IndexEntry) string {
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
func (manager *Manager) releaseDir(directory string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	delete(manager.activeDirs, filepath.Base(directory))
}
