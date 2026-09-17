// 本文件是 logs 表的读路径：列表检索（SearchLogs 的状态表达式/q/时间窗
// 等全量筛选都下推 SQL）、格子聚合（LogCells 替代旧的内存 rollup）、
// 短窗速率（recent 口径）、last_* 快照与趋势种子。
//
// 语义对齐被替换的 index.jsonl 读取面（debuglog.reader 的 match 与
// ccpanel.rollup 的格子口径），差异只有两处有意的升级：计数精确化
// （不再受 4MB 尾部窗口约束）与时间边界改成毫秒闭区间（修掉旧实现
// started_at 纳秒严格比较在边界丢掉最后 1ms 内行的问题）。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// logEModelExpr 是「生效模型」的 SQL 投影：Model 退化 RequestedModel，
// 与旧聚合（cellKey.model、perModel、lastByModel）同口径。
const logEModelExpr = `CASE WHEN model != '' THEN model ELSE requested_model END`

// logColumns 是行扫描的 SELECT 列清单（顺序即 scanLogRow 的 Scan 顺序）。
const logColumns = `id, dir, started_at, duration_ms,
	request_ready_ms, upstream_sent_ms, upstream_open_ms, first_upstream_ms, first_client_ms,
	api, method, path, status_code, result,
	requested_model, model, response_model, model_mismatch, stream,
	input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens,
	credit_cost, upstream_request_id, client_ip, key_hash, client_request_id,
	error_stage, error_message, dropped_events, retry_after_seconds, rate_limited,
	retries, account, account_switches, premature_end_turn, repairs,
	conn_reused, conn_idle_ms, log_source, upstream_protocol`

// scanLogRow 按 logColumns 顺序扫一行；extra 接收调用方追加的尾列
// （如 SearchLogs 的 COUNT(*) OVER()）。布尔与可空列走 NullInt64
// 中转——database/sql 不支持 int64→bool/**T 的直接反射转换。
func scanLogRow(rows *sql.Rows, extra ...any) (*LogRow, error) {
	var r LogRow
	var started string
	var ready, sent, open, firstUp, firstCli sql.NullInt64
	var modelMismatch, stream, rateLimited, premature int64
	var connReused, connIdle sql.NullInt64
	dests := append([]any{
		&r.ID, &r.Dir, &started, &r.DurationMS,
		&ready, &sent, &open, &firstUp, &firstCli,
		&r.API, &r.Method, &r.Path, &r.StatusCode, &r.Result,
		&r.RequestedModel, &r.Model, &r.ResponseModel, &modelMismatch, &stream,
		&r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.ReasoningTokens, &r.TotalTokens,
		&r.CreditCost, &r.UpstreamRequestID, &r.ClientIP, &r.KeyHash, &r.ClientRequestID,
		&r.ErrorStage, &r.ErrorMessage, &r.DroppedEvents, &r.RetryAfterSeconds, &rateLimited,
		&r.Retries, &r.Account, &r.AccountSwitches, &premature, &r.Repairs,
		&connReused, &connIdle, &r.LogSource, &r.UpstreamProtocol,
	}, extra...)
	if err := rows.Scan(dests...); err != nil {
		return nil, err
	}
	var err error
	r.StartedAt, err = time.Parse(time.RFC3339Nano, started)
	if err != nil {
		return nil, fmt.Errorf("logs.started_at %q: %w", started, err)
	}
	r.ModelMismatch = modelMismatch != 0
	r.Stream = stream != 0
	r.RateLimited = rateLimited != 0
	r.PrematureEndTurn = premature != 0
	r.RequestReadyMS = nullInt64Ptr(ready)
	r.UpstreamSentMS = nullInt64Ptr(sent)
	r.UpstreamOpenMS = nullInt64Ptr(open)
	r.FirstUpstreamMS = nullInt64Ptr(firstUp)
	r.FirstClientMS = nullInt64Ptr(firstCli)
	if connReused.Valid {
		b := connReused.Int64 != 0
		r.ConnReused = &b
	}
	r.ConnIdleMS = nullInt64Ptr(connIdle)
	return &r, nil
}

func nullInt64Ptr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

// LogQuery 是 logs 表的结构化筛选；零值即全量。
// 对应旧 debuglog.RequestFilter + 行级筛选的合并下推：since/until 按
// time 列（毫秒）闭区间比较；status 是状态表达式（499/4xx/>=400/!200，
// 逗号 OR）；model 精确命中三段模型名其一；model_like 子串；q 是对行内
// 文本列拼接后的小写子串匹配（LOWER() 只折叠 ASCII，比旧 Go ToLower
// 的 Unicode 折叠窄——非 ASCII 搜索词行为基本不变）。
type LogQuery struct {
	SinceMS int64
	UntilMS int64
	// KeyHash 收敛数据范围（api_token 身份/auth_token_id 参数解析结果）。
	KeyHash string
	// Account 是上游账号 lane 名精确过滤。
	Account string
	// LogSource/API/UpstreamProtocol 为空或 "all" 不过滤，否则精确匹配。
	LogSource        string
	API              string
	UpstreamProtocol string
	Model            string
	ModelLike        string
	Query            string
	StatusExpr       string
	StatusClass      string
	Result           string
	ErrorStage       string
	// Limit<=0 表示不限（LIMIT -1）；Offset<0 按 0。
	Limit  int
	Offset int
}

// where 把 LogQuery 编译成 WHERE 片段（含前导 " WHERE "）与参数。
func (q LogQuery) where() (string, []any) {
	var conds []string
	var args []any
	add := func(c string, a ...any) {
		conds = append(conds, c)
		args = append(args, a...)
	}
	if q.SinceMS > 0 {
		add("time >= ?", q.SinceMS)
	}
	if q.UntilMS > 0 {
		add("time <= ?", q.UntilMS)
	}
	if q.KeyHash != "" {
		add("key_hash = ?", q.KeyHash)
	}
	if q.Account != "" {
		add("account = ?", q.Account)
	}
	if q.LogSource != "" && q.LogSource != "all" {
		add("log_source = ?", q.LogSource)
	}
	if q.API != "" && q.API != "all" {
		add("api = ?", q.API)
	}
	if q.UpstreamProtocol != "" && q.UpstreamProtocol != "all" {
		add("upstream_protocol = ?", q.UpstreamProtocol)
	}
	if q.Model != "" {
		add("(model = ? OR requested_model = ? OR response_model = ?)", q.Model, q.Model, q.Model)
	}
	if q.ModelLike != "" {
		add("(INSTR(model, ?) > 0 OR INSTR(requested_model, ?) > 0 OR INSTR(response_model, ?) > 0)",
			q.ModelLike, q.ModelLike, q.ModelLike)
	}
	if q.Result != "" {
		add("result = ?", q.Result)
	}
	if q.ErrorStage != "" {
		add("error_stage = ?", q.ErrorStage)
	}
	if q.StatusClass != "" {
		if len(q.StatusClass) == 3 && q.StatusClass[1:] == "xx" {
			add("status_code/100 = ?", int(q.StatusClass[0]-'0'))
		} else {
			// 非法段位写法（含非数字首字符）同旧 match：恒不匹配。
			conds = append(conds, "1=0")
		}
	}
	if q.StatusExpr != "" {
		frag, a := statusExprSQL(q.StatusExpr)
		conds = append(conds, frag)
		args = append(args, a...)
	}
	if q.Query != "" {
		add(`INSTR(LOWER(dir || ' ' || method || ' ' || path || ' ' || model || ' ' ||
			requested_model || ' ' || response_model || ' ' || key_hash || ' ' ||
			client_request_id || ' ' || error_stage || ' ' || error_message || ' ' || result), ?) > 0`,
			strings.ToLower(q.Query))
	}
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// statusExprSQL 把状态表达式编译成一段 WHERE 条件（term 间 OR）。
// 词汇与旧 parseStatusExpr 一致：可选 ! 前缀 + Nxx 段位 / >= <= > < 比较 /
// 裸三位码；任一单项非法时返回 "1=0"——宁可显式空结果也不静默无过滤。
func statusExprSQL(expr string) (string, []any) {
	var terms []string
	var args []any
	for _, part := range strings.Split(expr, ",") {
		frag, val, ok := statusTermSQL(strings.TrimSpace(part))
		if !ok {
			return "1=0", nil
		}
		terms = append(terms, frag)
		args = append(args, val)
	}
	return "(" + strings.Join(terms, " OR ") + ")", args
}

func statusTermSQL(t string) (string, int, bool) {
	neg := false
	if strings.HasPrefix(t, "!") {
		neg = true
		t = t[1:]
	}
	op := byte('=')
	for _, p := range []struct {
		pre string
		op  byte
	}{{">=", 'g'}, {"<=", 'l'}, {">", '>'}, {"<", '<'}} {
		if strings.HasPrefix(t, p.pre) {
			op = p.op
			t = t[len(p.pre):]
			break
		}
	}
	var frag string
	var val int
	// 段位写法 "4xx" 只在精确语义下成立（">=4xx" 无意义）。
	if len(t) == 3 && t[1:] == "xx" && t[0] >= '0' && t[0] <= '9' {
		if op != '=' {
			return "", 0, false
		}
		frag = "status_code/100 = ?"
		val = int(t[0] - '0')
	} else {
		n, err := strconv.Atoi(t)
		if err != nil || n < 100 || n > 999 {
			return "", 0, false
		}
		val = n
		switch op {
		case 'g':
			frag = "status_code >= ?"
		case 'l':
			frag = "status_code <= ?"
		case '>':
			frag = "status_code > ?"
		case '<':
			frag = "status_code < ?"
		default:
			frag = "status_code = ?"
		}
	}
	if neg {
		frag = "NOT (" + frag + ")"
	}
	return frag, val, true
}

// SearchLogs 返回命中行（新在前，id 倒序=旧 index 追加序倒排的忠实
// 移植——行只在完成时落库，id 序即完成序）与命中总数 total
// （分页前的完整计数——COUNT(*) OVER() 与旧「窗口内扫到的命中数」不同，
// 旧口径只是读取窗口内的命中，新口径是 SQL 精确值）。
func (s *Store) SearchLogs(ctx context.Context, q LogQuery) (rows []*LogRow, total int64, err error) {
	where, args := q.where()
	limit := q.Limit
	if limit <= 0 {
		limit = -1 // SQLite LIMIT -1 = 不限，占位符语义统一
	}
	offset := max(q.Offset, 0)
	sqlRows, err := s.db.QueryContext(ctx,
		`SELECT `+logColumns+`, COUNT(*) OVER() FROM logs`+where+
			` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = sqlRows.Close() }()
	for sqlRows.Next() {
		r, err := scanLogRow(sqlRows, &total)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, 0, err
	}
	// offset 越过命中尾部时窗口函数没有行可挂，total 留在零值——
	// 补一次标量计数把真实命中数还给分页器（空结果同样走这里）。
	if len(rows) == 0 {
		if err := s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM logs`+where, args...).Scan(&total); err != nil {
			return nil, 0, err
		}
	}
	return rows, total, nil
}

// ExistsLogBefore 报告是否存在 time 早于 ms 的行（has_more 的
// 「索引尾部窗外仍有更早历史」投影）。
func (s *Store) ExistsLogBefore(ctx context.Context, ms int64) (bool, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM logs WHERE time < ?)`, ms).Scan(&n)
	return n != 0, err
}

// DeleteLogsBefore 删除 time 早于 ms 的行（logs 表的时间保留清理），
// 返回删除行数。
func (s *Store) DeleteLogsBefore(ctx context.Context, ms int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE time < ?`, ms)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LogDirByID 按自增 id 反查调试目录名与请求时刻（毫秒）。
func (s *Store) LogDirByID(ctx context.Context, id int64) (dir string, timeMS int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT dir, time FROM logs WHERE id = ?`, id).Scan(&dir, &timeMS)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return dir, timeMS, true, nil
}

// LogCount 返回 logs 表行数（Stats 的 log_rows 口径）。
func (s *Store) LogCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&n)
	return n, err
}

// LogModels 返回出现过的生效模型名集合（排序）；kh 非空时只数该
// key_hash 产生过流量的模型（api_token 身份的数据范围收敛）。
func (s *Store) LogModels(ctx context.Context, kh string) ([]string, error) {
	query := `SELECT DISTINCT ` + logEModelExpr + ` FROM logs WHERE (model != '' OR requested_model != '')`
	var args []any
	if kh != "" {
		query += ` AND key_hash = ?`
		args = append(args, kh)
	}
	sqlRows, err := s.db.QueryContext(ctx, query+` ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlRows.Close() }()
	out := []string{}
	for sqlRows.Next() {
		var m string
		if err := sqlRows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, sqlRows.Err()
}

// LogStatusCodes 返回出现过的状态码集合（升序，不含 0 占位）。
func (s *Store) LogStatusCodes(ctx context.Context) ([]int, error) {
	sqlRows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT status_code FROM logs WHERE status_code != 0 ORDER BY status_code`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlRows.Close() }()
	out := []int{}
	for sqlRows.Next() {
		var c int
		if err := sqlRows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, sqlRows.Err()
}

// LogScope 是聚合查询的范围谓词（旧 cellKey 行级筛选的下推版）：
// model 精确命中生效模型，modelLike 是它的子串匹配。
type LogScope struct {
	KeyHash   string
	API       string
	Model     string
	ModelLike string
}

// where 返回追加在 WHERE/AND 链上的条件片段（含前导 " AND "）与参数。
func (sc LogScope) where() (string, []any) {
	var b strings.Builder
	var args []any
	if sc.KeyHash != "" {
		b.WriteString(` AND key_hash = ?`)
		args = append(args, sc.KeyHash)
	}
	if sc.API != "" {
		b.WriteString(` AND api = ?`)
		args = append(args, sc.API)
	}
	if sc.Model != "" {
		b.WriteString(` AND ` + logEModelExpr + ` = ?`)
		args = append(args, sc.Model)
	}
	if sc.ModelLike != "" {
		b.WriteString(` AND INSTR(` + logEModelExpr + `, ?) > 0`)
		args = append(args, sc.ModelLike)
	}
	return b.String(), args
}

// LogCellKey 是聚合格子的键：10 分钟时间槽 × 入口 api × 生效模型 ×
// key_hash——与旧 rollup cellKey 同形。
type LogCellKey struct {
	Slot    int64  // unix 秒，600 对齐
	API     string // 入口协议原值：anthropic/openai-chat/openai-responses/responses-ws
	Model   string // 生效模型（model 退化 requested_model）
	KeyHash string
}

// LogCellTotals 是单格子的累计计数；字段口径与旧 cellTotals 一一对应：
// ok=2xx，gone=499（客户端断连），limited=429；NG 后缀=非 499 行合计
// （metrics/token 统计口径），无后缀 token 字段含 499 行（summary/stats）。
type LogCellTotals struct {
	Requests     int64
	OK           int64
	Gone         int64
	Limited      int64
	NDur         int64 // duration>0 行数（stats avg_duration 分母，含 499）
	NDurOK       int64 // 2xx && duration>0（metrics duration_count）
	InTok        int64
	OutTok       int64
	CacheRead    int64
	CacheWrite   int64
	InTokNG      int64
	OutTokNG     int64
	CacheReadNG  int64
	CacheWriteNG int64
	SumDurMS     int64 // duration>0 行 duration 和（含 499）
	SumDurOKMS   int64 // 2xx 行 duration 和
	// streaming && 2xx && fbt>0：stats/health/metrics 的 TTFB 口径
	SumFirstOKMS int64
	NFirstOK     int64
	// streaming 行 fbt 和与样本数（不限状态）：token 统计 stream_avg_ttfb
	SumFirstStreamMS int64
	NFirstStream     int64
	// 非 streaming 行 duration 和与行数（不限状态）：token 统计 non_stream_avg_rt
	SumDurNonStreamMS int64
	NNonStream        int64
	// 非 499 行按 stream 拆分：token 统计 stream_count/non_stream_count
	NStreamNG    int64
	NNonStreamNG int64
	// SumGenMS 是生成时长和：stream 行扣首字（dur−fbt），非 stream/无
	// fbt 行取全时长——TPS（输出 token/生成秒）的分母，同速度列口径。
	SumGenMS int64
}

// logCellCols 是 LogCells 的聚合列清单（顺序即扫描顺序）。
const logCellCols = `
	COUNT(*),
	SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END),
	SUM(CASE WHEN status_code = 499 THEN 1 ELSE 0 END),
	SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END),
	SUM(CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END),
	SUM(CASE WHEN status_code >= 200 AND status_code < 300 AND duration_ms > 0 THEN 1 ELSE 0 END),
	COALESCE(SUM(input_tokens), 0),
	COALESCE(SUM(output_tokens), 0),
	COALESCE(SUM(cache_read_tokens), 0),
	COALESCE(SUM(cache_write_tokens), 0),
	COALESCE(SUM(CASE WHEN status_code != 499 THEN input_tokens ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code != 499 THEN output_tokens ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code != 499 THEN cache_read_tokens ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code != 499 THEN cache_write_tokens ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status_code >= 200 AND status_code < 300 THEN duration_ms ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN stream != 0 AND status_code >= 200 AND status_code < 300 AND first_upstream_ms > 0 THEN first_upstream_ms ELSE 0 END), 0),
	SUM(CASE WHEN stream != 0 AND status_code >= 200 AND status_code < 300 AND first_upstream_ms > 0 THEN 1 ELSE 0 END),
	COALESCE(SUM(CASE WHEN stream != 0 AND first_upstream_ms IS NOT NULL THEN first_upstream_ms ELSE 0 END), 0),
	SUM(CASE WHEN stream != 0 AND first_upstream_ms IS NOT NULL THEN 1 ELSE 0 END),
	COALESCE(SUM(CASE WHEN stream = 0 THEN duration_ms ELSE 0 END), 0),
	SUM(CASE WHEN stream = 0 THEN 1 ELSE 0 END),
	SUM(CASE WHEN status_code != 499 AND stream != 0 THEN 1 ELSE 0 END),
	SUM(CASE WHEN status_code != 499 AND stream = 0 THEN 1 ELSE 0 END),
	COALESCE(SUM(CASE WHEN duration_ms > 0 THEN
		CASE WHEN first_upstream_ms > 0 AND first_upstream_ms < duration_ms
			THEN duration_ms - first_upstream_ms ELSE duration_ms END
		ELSE 0 END), 0)`

// alignUp600/alignDown600 把 unix 秒对齐到 600s 槽边界。
func alignUp600(v int64) int64   { return v + ((600 - v%600) % 600) }
func alignDown600(v int64) int64 { return v - ((v%600 + 600) % 600) }

// LogCells 按 (槽,api,生效模型,key_hash) 聚合时间窗内的日志行，逐格回调。
// 格子口径与旧 eachCell 一致：格子计入条件是它与 [since,until)（unix 秒）
// 有任意重叠——即 slot+600>since 且 slot<until，翻译成行的 time 范围是
// [alignUp600(since-599), alignDown600(until-1)+600) 毫秒。
func (s *Store) LogCells(ctx context.Context, sinceSec, untilSec int64, sc LogScope, cb func(LogCellKey, LogCellTotals)) error {
	if untilSec < 1 {
		return nil
	}
	lo := alignUp600(sinceSec-599) * 1000
	hi := (alignDown600(untilSec-1) + 600) * 1000
	if hi <= lo {
		return nil
	}
	scopeWhere, scopeArgs := sc.where()
	query := `SELECT time/600000*600 AS slot, api, ` + logEModelExpr + ` AS emodel, key_hash,` +
		logCellCols + ` FROM logs WHERE time >= ? AND time < ?` + scopeWhere +
		` GROUP BY slot, api, emodel, key_hash`
	sqlRows, err := s.db.QueryContext(ctx, query, append([]any{lo, hi}, scopeArgs...)...)
	if err != nil {
		return err
	}
	defer func() { _ = sqlRows.Close() }()
	for sqlRows.Next() {
		var key LogCellKey
		var c LogCellTotals
		if err := sqlRows.Scan(&key.Slot, &key.API, &key.Model, &key.KeyHash,
			&c.Requests, &c.OK, &c.Gone, &c.Limited, &c.NDur, &c.NDurOK,
			&c.InTok, &c.OutTok, &c.CacheRead, &c.CacheWrite,
			&c.InTokNG, &c.OutTokNG, &c.CacheReadNG, &c.CacheWriteNG,
			&c.SumDurMS, &c.SumDurOKMS, &c.SumFirstOKMS, &c.NFirstOK,
			&c.SumFirstStreamMS, &c.NFirstStream, &c.SumDurNonStreamMS, &c.NNonStream,
			&c.NStreamNG, &c.NNonStreamNG, &c.SumGenMS); err != nil {
			return err
		}
		cb(key, c)
	}
	return sqlRows.Err()
}

// LogRecentAgg 是短窗聚合（旧 recentAgg）：req 只数非 499（RPM 口径），
// token 全量累计；genMS 是生成时长和，firstMS/nFirst 沿用格子 TTFB 口径。
type LogRecentAgg struct {
	Req     int64
	InTok   int64
	OutTok  int64
	CrTok   int64
	CwTok   int64
	DurMS   int64
	NDur    int64
	GenMS   int64
	FirstMS int64
	NFirst  int64
}

// logRecentEndExpr 是请求完成时刻（unix 秒）的 SQL 表达式：
// started.Unix() + duration_ms/1000——两端整数除法与旧 Go 语义一致。
const logRecentEndExpr = `time/1000 + duration_ms/1000`

// LogRecentWindow 聚合最近 seconds 秒内完成的条目（recent 环的 SQL 版）。
// 完成时刻表达式不可索引，用 time > (cut-3600s) 预筛把扫描圈进
// idx_logs_time 范围——duration 受排空上限约束（≪3600s），不漏行。
func (s *Store) LogRecentWindow(ctx context.Context, seconds int64, sc LogScope) (LogRecentAgg, error) {
	var a LogRecentAgg
	cut := time.Now().Unix() - seconds
	scopeWhere, scopeArgs := sc.where()
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN status_code != 499 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(cache_read_tokens), 0),
		COALESCE(SUM(cache_write_tokens), 0),
		COALESCE(SUM(CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_upstream_ms > 0 AND first_upstream_ms < duration_ms
			THEN duration_ms - first_upstream_ms ELSE duration_ms END), 0),
		COALESCE(SUM(CASE WHEN stream != 0 AND status_code >= 200 AND status_code < 300 AND first_upstream_ms > 0 THEN first_upstream_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN stream != 0 AND status_code >= 200 AND status_code < 300 AND first_upstream_ms > 0 THEN 1 ELSE 0 END), 0)
		FROM logs WHERE time > ? AND `+logRecentEndExpr+` > ?`+scopeWhere,
		append([]any{(cut - 3600) * 1000, cut}, scopeArgs...)...).Scan(
		&a.Req, &a.InTok, &a.OutTok, &a.CrTok, &a.CwTok, &a.DurMS, &a.NDur, &a.GenMS, &a.FirstMS, &a.NFirst)
	return a, err
}

// LogRecentRPM 返回最近 60 秒内完成的非 499 请求数；model/kh 非空时
// 分别按生效模型、key_hash 过滤。time 预筛同 LogRecentWindow。
func (s *Store) LogRecentRPM(ctx context.Context, model, kh string) (float64, error) {
	cut := time.Now().Unix() - 60
	query := `SELECT COUNT(*) FROM logs WHERE status_code != 499 AND time > ? AND ` + logRecentEndExpr + ` > ?`
	args := []any{(cut - 3600) * 1000, cut}
	if model != "" {
		query += ` AND ` + logEModelExpr + ` = ?`
		args = append(args, model)
	}
	if kh != "" {
		query += ` AND key_hash = ?`
		args = append(args, kh)
	}
	var n int64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return float64(n), nil
}

// LogModelLast 是单生效模型的最近时刻快照：Req* 是最近非 499 行
// （含真实自增 id），OK* 是最近 2xx 行——ccLoad last_request_*/
// last_success_at 的投影源。
type LogModelLast struct {
	ReqAt     int64 // unix 毫秒
	ReqID     int64
	ReqStatus int
	ReqResult string
	OKAt      int64 // unix 毫秒
	OKID      int64
}

// LogLastByModel 返回各生效模型的最近快照；kh 非空时只看该令牌的行。
// 两个窗口查询各自取每模型最新行（time DESC, id DESC）；emodel=” 组
// 同样保留（与旧口径一致）。
func (s *Store) LogLastByModel(ctx context.Context, kh string) (map[string]LogModelLast, error) {
	out := map[string]LogModelLast{}
	khCond := ""
	var args []any
	if kh != "" {
		khCond = ` AND key_hash = ?`
		args = append(args, kh)
	}
	latest := func(cond string, apply func(m *LogModelLast, at, id int64, status int, result string)) error {
		rows, err := s.db.QueryContext(ctx, `
			SELECT emodel, time, id, status_code, result FROM (
				SELECT `+logEModelExpr+` AS emodel, time, id, status_code, result,
					ROW_NUMBER() OVER (PARTITION BY `+logEModelExpr+` ORDER BY id DESC) AS rn
				FROM logs WHERE `+cond+khCond+`
			) WHERE rn = 1`, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var emodel string
			var at, id int64
			var status int
			var result string
			if err := rows.Scan(&emodel, &at, &id, &status, &result); err != nil {
				return err
			}
			cur := out[emodel]
			apply(&cur, at, id, status, result)
			out[emodel] = cur
		}
		return rows.Err()
	}
	if err := latest(`status_code != 499`, func(m *LogModelLast, at, id int64, status int, result string) {
		m.ReqAt, m.ReqID, m.ReqStatus, m.ReqResult = at, id, status, result
	}); err != nil {
		return nil, err
	}
	if err := latest(`status_code >= 200 AND status_code < 300`, func(m *LogModelLast, at, id int64, _ int, _ string) {
		m.OKAt, m.OKID = at, id
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// LogTrendSeed 是 obs 趋势桶的预热种子：完成时刻 + 是否错误。
type LogTrendSeed struct {
	FinishedMS int64
	IsError    bool
}

// LogTrendSeeds 返回最近 60 分钟内完成的条目（新在前，上限 limit），
// 供启动时预热 RPM/QPS 趋势桶——等价于旧「读索引尾部 50000 行再
// 按完成时刻过滤」的路径，窗口谓词直接下推。
func (s *Store) LogTrendSeeds(ctx context.Context, limit int) ([]LogTrendSeed, error) {
	cut := time.Now().Add(-time.Hour).UnixMilli()
	rows, err := s.db.QueryContext(ctx, `
		SELECT time + duration_ms,
			CASE WHEN status_code >= 400 OR (result != '' AND result != 'completed') THEN 1 ELSE 0 END
		FROM logs WHERE time + duration_ms >= ?
		ORDER BY id DESC LIMIT ?`, cut, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []LogTrendSeed{}
	for rows.Next() {
		var seed LogTrendSeed
		var isErr int64
		if err := rows.Scan(&seed.FinishedMS, &isErr); err != nil {
			return nil, err
		}
		seed.IsError = isErr != 0
		out = append(out, seed)
	}
	return out, rows.Err()
}
