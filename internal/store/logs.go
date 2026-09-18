package store

import (
	"context"
	"slices"
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
	// LogSource 是写入时定版的来源：写侧置值即生效（rejected 行靠它
	// 与服役流量分域），留空时 InsertLog 回落 'proxy'——业务分类归
	// 写方（debuglog.logRowFor / 导入器各自定版）；读路径回填库内原值。
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
	// AffinityHash 是号池选号的会话谱系亲和键（SessionAffinityKey 的
	// SHA-256，不可逆），与 meta.json 的 affinity_hash 同源——谱系
	// 分析（绑定谱系/warm 救援/failover 同族）的 GROUP BY 维。非号池
	// 路径与管线前拒绝留空串；读侧回填库内原值。
	AffinityHash string `json:"affinity_hash,omitempty"`

	// SwitchCauses 是被放弃 lane 尝试的归因聚合（{lane,cause}→次数），
	// 由写方 debuglog.logRowFor 从 meta.json 同源的 upstream_attempts
	// 投影而来——它不是列，只作 InsertLog/WriteDebugBatch 展开进
	// lane_attempt_causes 表的瞬时载体，读侧回填恒为 nil。
	SwitchCauses map[SwitchCause]int `json:"-"`
}

// logColumnList 是 logs 表全部列，顺序与 schema.go 的 CREATE TABLE
// 一致（加列两边同步改）。读/写列清单由它剔除派生：
//   - INSERT 剔除 id（自增）与 upstream_protocol（恒默认值 'devin'）；
//   - SELECT 剔除 time/minute_bucket（读侧以 started_at 呈现，毫秒
//     谓词仍走原始列）。
//
// scanLogRow 按 SELECT 派生序逐列绑定，加列时须补它的 case。
var logColumnList = []string{
	"id", "dir", "time", "minute_bucket", "started_at", "duration_ms",
	"request_ready_ms", "upstream_sent_ms", "upstream_open_ms", "first_upstream_ms", "first_client_ms",
	"api", "method", "path", "status_code", "result",
	"requested_model", "model", "response_model", "model_mismatch", "stream",
	"input_tokens", "output_tokens", "cache_read_tokens", "cache_write_tokens", "reasoning_tokens", "total_tokens",
	"credit_cost", "upstream_request_id", "client_ip", "key_hash", "client_request_id",
	"error_stage", "error_message", "dropped_events", "retry_after_seconds", "rate_limited",
	"retries", "account", "account_switches", "premature_end_turn", "repairs",
	"conn_reused", "conn_idle_ms", "affinity_hash", "log_source", "upstream_protocol",
}

var (
	// logSelectCols 是行扫描的列集（顺序即 scanLogRow 的 Scan 顺序）。
	logSelectCols = logColumnsExcept("time", "minute_bucket")
	logWriteCols  = logColumnsExcept("id", "upstream_protocol")
	// logColumns 是行扫描的 SELECT 列清单字面量（SearchLogs/exportIndex 用）。
	logColumns = strings.Join(logSelectCols, ", ")
	// logsInsertSQL 的列清单与占位符都由 logWriteCols 派生，不手数字面量。
	// import.go 的 ON CONFLICT(dir) 变体共用此语句。
	logsInsertSQL = `INSERT INTO logs(` + strings.Join(logWriteCols, ", ") +
		`) VALUES(` + placeholders(len(logWriteCols)) + `)`
	// logsBatchInsertSQL 是写 worker 批量收尾用的幂等变体：idx_logs_dir
	// 唯一索引下重复行静默跳过——单个坏行不能把共享事务拖成永久重试。
	logsBatchInsertSQL = strings.Replace(logsInsertSQL, `INSERT INTO`, `INSERT OR IGNORE INTO`, 1)
)

// logColumnsExcept 从 logColumnList 剔除指定列（保持原序）。
func logColumnsExcept(drop ...string) []string {
	out := make([]string, 0, len(logColumnList)-len(drop))
	for _, c := range logColumnList {
		if !slices.Contains(drop, c) {
			out = append(out, c)
		}
	}
	return out
}

// logInsertArgs 按 logWriteCols 序展开一行的 INSERT 实参；time/
// minute_bucket 由 StartedAt 派生，log_source 空值回落 'proxy' 与
// DDL 默认值同口径。InsertLog 与 WriteDebugBatch 的批量日志行共用
// 同一投影，列序只在此维护一份。
func logInsertArgs(e *LogRow) []any {
	ms := e.StartedAt.UnixMilli()
	source := e.LogSource
	if source == "" {
		source = "proxy"
	}
	return []any{
		e.Dir, ms, ms / 60000, e.StartedAt.Format(time.RFC3339Nano), e.DurationMS,
		e.RequestReadyMS, e.UpstreamSentMS, e.UpstreamOpenMS, e.FirstUpstreamMS, e.FirstClientMS,
		e.API, e.Method, e.Path, e.StatusCode, e.Result,
		e.RequestedModel, e.Model, e.ResponseModel, e.ModelMismatch, e.Stream,
		e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
		e.CreditCost, e.UpstreamRequestID, e.ClientIP, e.KeyHash, e.ClientRequestID,
		e.ErrorStage, e.ErrorMessage, e.DroppedEvents, e.RetryAfterSeconds, e.RateLimited,
		e.Retries, e.Account, e.AccountSwitches, e.PrematureEndTurn, e.Repairs,
		e.ConnReused, e.ConnIdleMS, e.AffinityHash, source,
	}
}

// InsertLog 写入一条请求日志行，返回自增 id。log_source 原样落字段
// （业务分类归写方 debuglog.logRowFor 与导入器）。行插入与 rollup
// 记账（log_cells 贡献 + 水位推进）同一事务提交。
func (s *Store) InsertLog(ctx context.Context, e *LogRow) (int64, error) {
	tx, done, err := s.writeTx(ctx, "InsertLog")
	if err != nil {
		return 0, err
	}
	defer done()
	res, err := tx.ExecContext(ctx, logsInsertSQL, logInsertArgs(e)...)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	cells := map[cellDim]*cellVals{}
	errCells := map[errCellDim]int64{}
	addCellContrib(cells, errCells, e, id)
	if err := upsertCells(ctx, tx, cells, errCells); err != nil {
		return 0, err
	}
	causes := map[laneCauseDim]int64{}
	addCauseContrib(causes, e)
	if err := upsertCauseCells(ctx, tx, causes); err != nil {
		return 0, err
	}
	if err := setCellsWatermark(tx, id); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}
