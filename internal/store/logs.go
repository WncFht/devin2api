package store

import (
	"context"
	"time"
)

// probeClientRequestID 镜像 debuglog.ProbeClientRequestID——
// store 是被 debuglog 导入的下层包，不能反向引用常量；该值是
// 客户端可见的 wire 契约（面板探活标记），两边必须同步改。
const probeClientRequestID = "panel-probe"

// LogRow 是 logs 表一行的领域形状，字段与 debuglog.IndexEntry
// 一一对应（time/minute_bucket/started_at/log_source 由
// InsertLog 从 StartedAt/ClientRequestID 派生，不在字段里）。
type LogRow struct {
	// ID 是自增日志行号——面板 log_id 与 last_*_id 的身份；读侧回填。
	ID int64
	// LogSource 是写入时定版的来源（proxy/manual_test）；读侧回填。
	LogSource string
	// UpstreamProtocol 保留过滤维度的统一形状（当前恒 devin）；读侧回填。
	UpstreamProtocol string

	Dir               string
	StartedAt         time.Time
	DurationMS        int64
	RequestReadyMS    *int64
	UpstreamSentMS    *int64
	UpstreamOpenMS    *int64
	FirstUpstreamMS   *int64
	FirstClientMS     *int64
	API               string
	Method            string
	Path              string
	StatusCode        int
	Result            string
	RequestedModel    string
	Model             string
	ResponseModel     string
	ModelMismatch     bool
	Stream            bool
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheWriteTokens  int64
	ReasoningTokens   int64
	TotalTokens       int64
	CreditCost        int64
	UpstreamRequestID string
	ClientIP          string
	KeyHash           string
	ClientRequestID   string
	ErrorStage        string
	ErrorMessage      string
	DroppedEvents     uint64
	RetryAfterSeconds int64
	RateLimited       bool
	Retries           int
	Account           string
	AccountSwitches   int
	PrematureEndTurn  bool
	Repairs           int
	ConnReused        *bool
	ConnIdleMS        *int64
}

// logsInsertSQL 44 列——占位符由 placeholders 按列名数派生，
// 避免手数字面量与列清单漂移。
var logsInsertSQL = `INSERT INTO logs(
	dir, time, minute_bucket, started_at, duration_ms,
	request_ready_ms, upstream_sent_ms, upstream_open_ms, first_upstream_ms, first_client_ms,
	api, method, path, status_code, result,
	requested_model, model, response_model, model_mismatch, stream,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens,
	credit_cost, upstream_request_id, client_ip, key_hash, client_request_id,
	error_stage, error_message, dropped_events, retry_after_seconds, rate_limited,
	retries, account, account_switches, premature_end_turn, repairs,
	conn_reused, conn_idle_ms, log_source
) VALUES(` + placeholders(44) + `)`

// InsertLog 写入一条请求日志行，返回自增 id。time/minute_bucket
// 由 StartedAt 派生；log_source 由 ClientRequestID 定版——面板
// 探活记 manual_test，与 ccLoad 同语义，不计入默认 proxy 视图。
func (s *Store) InsertLog(ctx context.Context, e *LogRow) (int64, error) {
	ms := e.StartedAt.UnixMilli()
	source := "proxy"
	if e.ClientRequestID == probeClientRequestID {
		source = "manual_test"
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
