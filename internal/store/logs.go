package store

import (
	"context"
	"strings"
	"time"
)

// LogRow 是 logs 表一行的领域形状，也是日志行的 wire 投影
// （/admin/logs 导出与 matrix 的元素，json tag 即对外字段名）。
// time/minute_bucket 由 InsertLog 从 StartedAt 派生、不进字段；
// upstream_protocol 由 DDL 默认值供值——两者都只在读侧回填出现。
type LogRow struct {
	// ID 是自增日志行号——面板 log_id 与 last_*_id 的身份；读侧回填。
	ID int64 `json:"-"`
	// LogSource 是写入时定版的来源（proxy/manual_test）；读侧回填。
	LogSource string `json:"-"`
	// UpstreamProtocol 保留过滤维度的统一形状（当前恒 devin）；读侧回填。
	UpstreamProtocol string `json:"-"`

	Dir        string    `json:"dir"`
	StartedAt  time.Time `json:"started_at"`
	DurationMS int64     `json:"duration_ms"`
	// 延迟分解字段用指针区分「未发生」（nil，省略）与「即时发生」（0ms）；
	// int 零值会掩盖这两种语义。五段口径见 debuglog.Recorder 同名字段注释：
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
	// 截断后随索引落盘）。目录被保留策略淘汰后，日志行仍能回答
	// 「为什么败」——此前只剩阶段名，归因必须靠目录在场。
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
	// Account 是最终服务请求的上游账号名（号池 lane 名）；迁移前
	// 历史行另有 ''/'default' 残留，读侧按 'default' 折叠归桶。
	Account string `json:"account,omitempty"`
	// AccountSwitches 是号池 failover 换号次数（成功前的失败尝试数），
	// 明细在同目录 meta.json 的 upstream_attempts。0 表示首号即成。
	AccountSwitches int `json:"account_switches,omitempty"`
	// PrematureEndTurn 标记「工具结果之后模型纯文本 end_turn」的可疑收尾，
	// 供检索统计该模型行为的真实频率。
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

// logColumnList 是 logs 行的单一列清单：SELECT 列序即 scanLogRow 的
// Scan 顺序，INSERT 列序由它派生（logInsertColumnList）。schema.go 的
// CREATE TABLE 是该清单的 DDL 落点——加列只改清单、字段、scan、写侧
// 赋值，SQL 文本不再多处重复。
var logColumnList = []string{
	"id", "dir", "started_at", "duration_ms",
	"request_ready_ms", "upstream_sent_ms", "upstream_open_ms", "first_upstream_ms", "first_client_ms",
	"api", "method", "path", "status_code", "result",
	"requested_model", "model", "response_model", "model_mismatch", "stream",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "total_tokens",
	"credit_cost", "upstream_request_id", "client_ip", "key_hash", "client_request_id",
	"error_stage", "error_message", "dropped_events", "retry_after_seconds", "rate_limited",
	"retries", "account", "account_switches", "premature_end_turn", "repairs",
	"conn_reused", "conn_idle_ms", "log_source", "upstream_protocol",
}

// logColumns 是行扫描的 SELECT 列清单。
var logColumns = strings.Join(logColumnList, ", ")

// logInsertColumns 派生 INSERT 列序：去掉 id（自增）与
// upstream_protocol（DDL DEFAULT 'devin' 供值），dir 之后补
// time/minute_bucket 两个 InsertLog 派生列。
var logInsertColumns = func() []string {
	cols := make([]string, 0, len(logColumnList))
	for _, c := range logColumnList {
		switch c {
		case "id", "upstream_protocol":
			continue
		case "dir":
			cols = append(cols, "dir", "time", "minute_bucket")
			continue
		}
		cols = append(cols, c)
	}
	return cols
}()

// logsInsertSQL 的占位符由 placeholders 按列数派生，避免手数字面量
// 与列清单漂移。import.go 的 ON CONFLICT(dir) 变体共用此语句。
var logsInsertSQL = `INSERT INTO logs(` + strings.Join(logInsertColumns, ", ") +
	`) VALUES(` + placeholders(len(logInsertColumns)) + `)`

// InsertLog 写入一条请求日志行，返回自增 id。time/minute_bucket 由
// StartedAt 派生；log_source 原样落字段（业务分类归写方
// debuglog.insertLog），空值回落 'proxy' 与 DDL 默认值同口径。
func (s *Store) InsertLog(ctx context.Context, e *LogRow) (int64, error) {
	ms := e.StartedAt.UnixMilli()
	source := e.LogSource
	if source == "" {
		source = "proxy"
	}
	res, err := s.db.ExecContext(ctx, logsInsertSQL,
		e.Dir, ms, ms/60000, e.StartedAt.Format(time.RFC3339Nano), e.DurationMS,
		e.RequestReadyMS, e.UpstreamSentMS, e.UpstreamOpenMS, e.FirstUpstreamMS, e.FirstClientMS,
		e.API, e.Method, e.Path, e.StatusCode, e.Result,
		e.RequestedModel, e.Model, e.ResponseModel, e.ModelMismatch, e.Stream,
		e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
		e.CreditCost, e.UpstreamRequestID, e.ClientIP, e.KeyHash, e.ClientRequestID,
		e.ErrorStage, e.ErrorMessage, e.DroppedEvents, e.RetryAfterSeconds, e.RateLimited,
		e.Retries, e.Account, e.AccountSwitches, e.PrematureEndTurn, e.Repairs,
		e.ConnReused, e.ConnIdleMS, source)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
