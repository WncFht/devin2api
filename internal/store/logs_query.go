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

// logAccountExpr 是上游账号 lane 名的读侧折叠投影：号池前时代的 ”
// 行并进 'default' 桶（对齐 QuotaReport 的 ”→default 合流口径）。
// 所有按号过滤/分组的读路径统一用它——过滤参数 'default' 命中
// ”+'default' 两群，真名参数只命中真名行；写侧不做折叠，新行恒写真名。
const logAccountExpr = `COALESCE(NULLIF(account,''),'default')`

// scanLogRow 按 logSelectCols 顺序扫一行——dests 由列清单驱动生成，
// 列序错位的唯一表现是加列漏 case 时 Scan 报 nil 目的地。布尔与可空
// 列走 NullInt64 中转——database/sql 不支持 int64→bool/**T 的直接
// 反射转换。
func scanLogRow(rows *sql.Rows) (*LogRow, error) {
	var r LogRow
	var started string
	var ready, sent, open, firstUp, firstCli, upDone sql.NullInt64
	var modelMismatch, stream, rateLimited, premature int64
	var connReused, connIdle sql.NullInt64
	dests := make([]any, 0, len(logSelectCols))
	for _, col := range logSelectCols {
		var d any
		switch col {
		case "id":
			d = &r.ID
		case "dir":
			d = &r.Dir
		case "started_at":
			d = &started
		case "duration_ms":
			d = &r.DurationMS
		case "request_ready_ms":
			d = &ready
		case "upstream_sent_ms":
			d = &sent
		case "upstream_open_ms":
			d = &open
		case "first_upstream_ms":
			d = &firstUp
		case "first_client_ms":
			d = &firstCli
		case "api":
			d = &r.API
		case "method":
			d = &r.Method
		case "path":
			d = &r.Path
		case "status_code":
			d = &r.StatusCode
		case "result":
			d = &r.Result
		case "requested_model":
			d = &r.RequestedModel
		case "model":
			d = &r.Model
		case "response_model":
			d = &r.ResponseModel
		case "model_mismatch":
			d = &modelMismatch
		case "stream":
			d = &stream
		case "input_tokens":
			d = &r.InputTokens
		case "output_tokens":
			d = &r.OutputTokens
		case "cache_read_tokens":
			d = &r.CacheReadTokens
		case "cache_write_tokens":
			d = &r.CacheWriteTokens
		case "reasoning_tokens":
			d = &r.ReasoningTokens
		case "total_tokens":
			d = &r.TotalTokens
		case "credit_cost":
			d = &r.CreditCost
		case "upstream_request_id":
			d = &r.UpstreamRequestID
		case "client_ip":
			d = &r.ClientIP
		case "key_hash":
			d = &r.KeyHash
		case "client_request_id":
			d = &r.ClientRequestID
		case "error_stage":
			d = &r.ErrorStage
		case "error_message":
			d = &r.ErrorMessage
		case "dropped_events":
			d = &r.DroppedEvents
		case "retry_after_seconds":
			d = &r.RetryAfterSeconds
		case "rate_limited":
			d = &rateLimited
		case "retries":
			d = &r.Retries
		case "account":
			d = &r.Account
		case "account_switches":
			d = &r.AccountSwitches
		case "premature_end_turn":
			d = &premature
		case "repairs":
			d = &r.Repairs
		case "conn_reused":
			d = &connReused
		case "conn_idle_ms":
			d = &connIdle
		case "affinity_hash":
			d = &r.AffinityHash
		case "log_source":
			d = &r.LogSource
		case "upstream_protocol":
			d = &r.UpstreamProtocol
		case "upstream_done_ms":
			d = &upDone
		}
		dests = append(dests, d)
	}
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
	r.UpstreamDoneMS = nullInt64Ptr(upDone)
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
	// Account 是上游账号 lane 名过滤，走读侧折叠口径
	//（logAccountExpr：'default' 命中 ''+'default' 两群）。
	Account string
	// LogSource 为空时默认剔除 rejected（管线前拒绝行有自己的计数
	// 与事件环，落表只为留存检索，不与服役流量混排）；"all" 不过滤，
	// 其余值精确匹配。API/UpstreamProtocol 为空或 "all" 不过滤。
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
	// BeforeID 是 keyset 游标：只取 id 小于它的行。翻页传「上一页
	// 最旧行的 id」即可走主键范围扫，替代随页深线性退化的 OFFSET。
	BeforeID int64
	// Limit<=0 表示不限（LIMIT -1）；Offset<0 按 0。
	Limit  int
	Offset int
	// SkipCount 置位时 SearchLogs 省略 COUNT(*) 返回 total=-1：深页
	// 翻页不需要精确命中数（列表页有缺省降级路径），生产全窗计数
	// ~0.7-1s/页是纯税。调用方需自行用 limit+1 探测判 has_more。
	SkipCount bool
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
		add(logAccountExpr+" = ?", q.Account)
	}
	switch q.LogSource {
	case "":
		add("log_source != ?", "rejected")
	case "all":
	default:
		add("log_source = ?", q.LogSource)
	}
	if q.BeforeID > 0 {
		add("id < ?", q.BeforeID)
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
// （分页前的完整计数——与旧「窗口内扫到的命中数」不同，旧口径只是
// 读取窗口内的命中，新口径是 SQL 精确值）。页查询与计数分两条：
// COUNT(*) OVER() 会把全命中集物化进 temp B-tree（实测 9k 行无过滤
// 首页 ~45ms），独立标量计数只走索引扫描，页查询靠主键逆序 LIMIT
// 只读本页行。
func (s *Store) SearchLogs(ctx context.Context, q LogQuery) (rows []*LogRow, total int64, err error) {
	where, args := q.where()
	limit := q.Limit
	if limit <= 0 {
		limit = -1 // SQLite LIMIT -1 = 不限，占位符语义统一
	}
	offset := max(q.Offset, 0)
	sqlRows, err := s.ro.QueryContext(ctx,
		`SELECT `+logColumns+` FROM logs`+where+
			` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = sqlRows.Close() }()
	for sqlRows.Next() {
		r, err := scanLogRow(sqlRows)
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, 0, err
	}
	if q.SkipCount {
		return rows, -1, nil
	}
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

// MatrixCell 是按 (slot, account) 聚合的健康矩阵桶：账号页 48×30min
// 健康条的服务端口径——entries 被 requestsFetchCap 截断时格子仍覆盖
// 全窗。分类对齐 debuglog.ErrorOwner 的失败责任归因：rl 是限流语义
// 终结（429 或 rate_limited 标记），err 是上游责任失败（失败且非客户
// 端断连/中断/读写阶段错），ok 是其余（成功 + 客户端责任——lane 已尽
// 责送达，断连不归 lane 失分）；sw 是 account_switches 合计。
type MatrixCell struct {
	Slot    int64  `json:"slot"`    // 桶起点 unix 秒
	Account string `json:"account"` // 读侧折叠名（logAccountExpr）
	OK      int64  `json:"ok"`
	Err     int64  `json:"err"`
	RL      int64  `json:"rl"`
	SW      int64  `json:"sw"`
}

// LogMatrixCells 把 q 筛选窗口内的日志行聚合成 (slotSec 秒槽, 折叠
// account) 桶，供健康条逐格分色；与 SearchLogs 共享 where 编译，
// rejected 行同口径剔除。
func (s *Store) LogMatrixCells(ctx context.Context, q LogQuery, slotSec int64) ([]MatrixCell, error) {
	if slotSec < 1 {
		return nil, nil
	}
	where, args := q.where()
	sqlRows, err := s.ro.QueryContext(ctx,
		fmt.Sprintf(`SELECT slot, acct, tot-err-rl AS ok, err, rl, sw FROM (
			SELECT time/%d*%d AS slot, %s AS acct, COUNT(*) AS tot,
				SUM(rate_limited = 0 AND status_code != 429
					AND (status_code >= 400 OR result = 'failed')
					AND result NOT IN ('disconnected','aborted')
					AND error_stage NOT IN ('http_read','http_decode')) AS err,
				SUM(status_code = 429 OR rate_limited != 0) AS rl,
				SUM(account_switches) AS sw
				FROM logs%s GROUP BY slot, acct)`,
			slotSec*1000, slotSec, logAccountExpr, where),
		args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = sqlRows.Close() }()
	var cells []MatrixCell
	for sqlRows.Next() {
		var c MatrixCell
		if err := sqlRows.Scan(&c.Slot, &c.Account, &c.OK, &c.Err, &c.RL, &c.SW); err != nil {
			return nil, err
		}
		cells = append(cells, c)
	}
	return cells, sqlRows.Err()
}

// ExistsLogBefore 报告在 q 的筛选口径下是否仍存在 time 早于
// q.SinceMS 的行（has_more 的「窗口下界之外仍有更早历史」投影）。
// 沿用同一 where 编译——不带筛选的无条件探测会让过滤翻页窗外无命中
// 时也报 has_more。
func (s *Store) ExistsLogBefore(ctx context.Context, q LogQuery) (bool, error) {
	bound := q.SinceMS
	// 时间窗与分页维度不参与「更早历史」判定：窗口下界换成 time<bound，
	// 上界天然蕴含；Limit/Offset/BeforeID 是页内游标不是筛选条件。
	q.SinceMS, q.UntilMS, q.Limit, q.Offset, q.BeforeID = 0, 0, 0, 0, 0
	where, args := q.where()
	if where == "" {
		where = " WHERE "
	} else {
		where += " AND "
	}
	var n int64
	err := s.ro.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM logs`+where+`time < ?)`,
		append(args, bound)...).Scan(&n)
	return n != 0, err
}

// deleteLogsBatch 是 DeleteLogsBefore 单片删除的行数上界：logs 是
// 无界增长表，每行删除连带约十个索引项维护，无界单事务在保留期调小
// 或批量回填触发存量清理时会独占唯一写连接数秒。片级提交让请求路径
// 写者在片间插队。
const deleteLogsBatch = 5000

// DeleteLogsBefore 删除 time 早于 ms 的行（logs 表的时间保留清理），
// 返回删除行数。按 deleteLogsBatch 分片循环：每片一条 IN 子查询圈定
// 最旧 id 段的小事务，中断后下一轮按原谓词续删。删除域钳在 rollup
// 水位内（id ≤ log_cells_covered_id）：水位内行已记账，删除后其贡献
// 留在 cells 账本；水位外行只属于绕过双写的写入者留下的缝隙，留给
// 覆盖（后续任意插入推进水位、或 Open 的 ReconcileCells）后下轮再删，
// 避免其聚合贡献无声消失。
func (s *Store) DeleteLogsBefore(ctx context.Context, ms int64) (int64, error) {
	var total int64
	for {
		var n int64
		err := writeTx(ctx, s.db.DB, "DeleteLogsBefore", func(ctx context.Context, q dbtx) error {
			res, err := q.ExecContext(ctx,
				`DELETE FROM logs WHERE id IN (
					SELECT id FROM logs WHERE time < ? AND id <= `+cellsWatermarkSQL+`
					ORDER BY id LIMIT ?)`, ms, deleteLogsBatch)
			if err != nil {
				return err
			}
			n, err = res.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < deleteLogsBatch {
			return total, nil
		}
	}
}

// LogDirByID 按自增 id 反查调试目录名与请求时刻（毫秒）。
func (s *Store) LogDirByID(ctx context.Context, id int64) (dir string, timeMS int64, ok bool, err error) {
	err = s.ro.QueryRowContext(ctx, `SELECT dir, time FROM logs WHERE id = ?`, id).Scan(&dir, &timeMS)
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
	err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&n)
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
	sqlRows, err := s.ro.QueryContext(ctx, query+` ORDER BY 1`, args...)
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

// LogStatusCodes 返回出现过的状态码集合（升序，不含 0 占位）；
// keyHash 非空时只看该令牌的行——api_token 身份的过滤面板与别处
// 的 key_hash 收敛同口径。
func (s *Store) LogStatusCodes(ctx context.Context, keyHash string) ([]int, error) {
	var args []any
	where := ` WHERE status_code != 0`
	if keyHash != "" {
		where += ` AND key_hash = ?`
		args = append(args, keyHash)
	}
	sqlRows, err := s.ro.QueryContext(ctx,
		`SELECT DISTINCT status_code FROM logs`+where+` ORDER BY status_code`, args...)
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
// model 精确命中生效模型，modelLike 是它的子串匹配，account 按读侧
// 折叠口径过滤上游账号 lane（'default' 命中 ”+'default' 两群）。
type LogScope struct {
	KeyHash   string
	API       string
	Model     string
	ModelLike string
	Account   string
}

// where 返回追加在 WHERE/AND 链上的条件片段（含前导 " AND "）与参数。
// 聚合口径恒剔除 rejected 行：管线前拒绝没有 token/时长，计入只会
// 污染请求数、成功率与延迟分布——它们的观测面是 rejects 计数与环。
func (sc LogScope) where() (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(` AND log_source != 'rejected'`)
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
	if sc.Account != "" {
		b.WriteString(` AND ` + logAccountExpr + ` = ?`)
		args = append(args, sc.Account)
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

// alignUp/alignDown 把 unix 秒对齐到 w 秒槽边界。
func alignUp(v, w int64) int64   { return v + ((w - v%w) % w) }
func alignDown(v, w int64) int64 { return v - ((v%w + w) % w) }

// LogCells 按 (slotSec 秒槽,api,生效模型,key_hash) 聚合时间窗内的日志行，
// 逐格回调。格子计入条件是它与 [since,until)（unix 秒）有任意重叠——
// 即 slot+slotSec>since 且 slot<until，翻译成行的 time 范围是
// [alignUp(since-(slotSec-1)), alignDown(until-1)+slotSec) 毫秒。
// 槽宽是 600s 整数倍且 scope 无 account 维时走 log_cells UNION 补尾：
// 窗口两界已按槽宽对齐，整格覆盖无需边带，只有水位外行落原始侧。
func (s *Store) LogCells(ctx context.Context, slotSec, sinceSec, untilSec int64, sc LogScope, cb func(LogCellKey, LogCellTotals)) error {
	if untilSec < 1 || slotSec < 1 {
		return nil
	}
	lo := alignUp(sinceSec-(slotSec-1), slotSec) * 1000
	hi := (alignDown(untilSec-1, slotSec) + slotSec) * 1000
	if hi <= lo {
		return nil
	}
	scopeWhere, scopeArgs := sc.where()
	var query string
	var args []any
	if sc.Account == "" && slotSec%600 == 0 {
		factor := slotSec / 600
		cellScope, cellArgs := sc.cellWhere()
		query = fmt.Sprintf(`SELECT slot/%d*%d AS qslot, api, emodel, key_hash,`, factor, slotSec) +
			cellSumList(cellTotalsCols) + ` FROM (
				SELECT slot, api, emodel, key_hash, ` + cellTotalsCols + `
				FROM log_cells WHERE slot >= ? AND slot < ?` + cellScope + `
				UNION ALL
				SELECT time/600000, api, ` + logEModelExpr + `, key_hash, ` + cellRowList(cellTotalsCols) + `
				FROM logs WHERE id > ` + cellsWatermarkSQL + ` AND time >= ? AND time < ?` + scopeWhere + `
			) GROUP BY slot/` + fmt.Sprint(factor) + `, api, emodel, key_hash`
		args = append(append(append([]any{lo / 600000, hi / 600000}, cellArgs...), lo, hi), scopeArgs...)
	} else {
		query = fmt.Sprintf(`SELECT time/%d*%d AS slot, api, `, slotSec*1000, slotSec) +
			logEModelExpr + ` AS emodel, key_hash,` +
			logCellCols + ` FROM logs WHERE time >= ? AND time < ?` + scopeWhere +
			` GROUP BY slot, api, emodel, key_hash`
		args = append([]any{lo, hi}, scopeArgs...)
	}
	sqlRows, err := s.ro.QueryContext(ctx, query, args...)
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
// idx_logs_time_status 范围——duration 受排空上限约束（≪3600s），不漏行。
func (s *Store) LogRecentWindow(ctx context.Context, seconds int64, sc LogScope) (LogRecentAgg, error) {
	var a LogRecentAgg
	cut := time.Now().Unix() - seconds
	scopeWhere, scopeArgs := sc.where()
	err := s.ro.QueryRowContext(ctx, `SELECT
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

// LogRecentRPM 返回最近 60 秒内完成的非 499 请求数；sc 为零值时全量
// （model 生效模型、kh、account 逐维下推，per-account 变体即
// LogScope{Account: lane}）。time 预筛同 LogRecentWindow。
func (s *Store) LogRecentRPM(ctx context.Context, sc LogScope) (float64, error) {
	cut := time.Now().Unix() - 60
	scopeWhere, scopeArgs := sc.where()
	var n int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE status_code != 499 AND time > ? AND `+logRecentEndExpr+` > ?`+scopeWhere,
		append([]any{(cut - 3600) * 1000, cut}, scopeArgs...)...).Scan(&n); err != nil {
		return 0, err
	}
	return float64(n), nil
}

// LogRecentRPMByModel 按生效模型分组返回最近 60 秒内完成的非 499
// 请求数；kh 非空时只看该令牌的行。替代逐模型 LogRecentRPM 循环
// 的一次 GROUP BY 扫描。
func (s *Store) LogRecentRPMByModel(ctx context.Context, kh string) (map[string]float64, error) {
	return s.logRecentRPMGroup(ctx, logEModelExpr, LogScope{KeyHash: kh})
}

// LogRecentRPMByKeyHash 按 key_hash 分组返回最近 60 秒内完成的非
// 499 请求数——替代逐令牌 LogRecentRPM 循环的一次 GROUP BY 扫描。
func (s *Store) LogRecentRPMByKeyHash(ctx context.Context) (map[string]float64, error) {
	return s.logRecentRPMGroup(ctx, `key_hash`, LogScope{})
}

// logRecentRPMGroup 是 LogRecentRPM 的分组形态：同一过滤口径，
// GROUP BY dimExpr 一次扫出全图。
func (s *Store) logRecentRPMGroup(ctx context.Context, dimExpr string, sc LogScope) (map[string]float64, error) {
	cut := time.Now().Unix() - 60
	scopeWhere, scopeArgs := sc.where()
	rows, err := s.ro.QueryContext(ctx,
		`SELECT `+dimExpr+`, COUNT(*) FROM logs WHERE status_code != 499 AND time > ? AND `+logRecentEndExpr+` > ?`+scopeWhere+` GROUP BY `+dimExpr,
		append([]any{(cut - 3600) * 1000, cut}, scopeArgs...)...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]float64)
	for rows.Next() {
		var dim string
		var n int64
		if err := rows.Scan(&dim, &n); err != nil {
			return nil, err
		}
		out[dim] = float64(n)
	}
	return out, rows.Err()
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

// logEModels 返回 logs 里出现过的全部生效模型名（含 ” 组——
// 双空行在内部聚合口径里也算一组，与对外 LogModels 的排除不同）。
// DISTINCT 走 idx_logs_emodel_id 的覆盖扫描，不触表行。
func (s *Store) logEModels(ctx context.Context) ([]string, error) {
	rows, err := s.ro.QueryContext(ctx, `SELECT DISTINCT `+logEModelExpr+` FROM logs`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// LogLastByModel 返回各生效模型的最近快照；kh 非空时只看该令牌的行。
// 逐模型 emodel=? ORDER BY id DESC LIMIT 1 点查走 idx_logs_emodel_id
// （模型数远小于行数），替代两遍无时间界 GROUP BY 全表扫。
func (s *Store) LogLastByModel(ctx context.Context, kh string) (map[string]LogModelLast, error) {
	emodels, err := s.logEModels(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]LogModelLast{}
	khCond := ""
	var khArgs []any
	if kh != "" {
		khCond = ` AND key_hash = ?`
		khArgs = []any{kh}
	}
	latest := func(cond string, apply func(m *LogModelLast, at, id int64, status int, result string)) error {
		query := `SELECT time, id, status_code, result FROM logs
			WHERE ` + logEModelExpr + ` = ? AND ` + cond + ` AND log_source != 'rejected'` + khCond + `
			ORDER BY id DESC LIMIT 1`
		for _, m := range emodels {
			var at, id int64
			var status int
			var result string
			err := s.ro.QueryRowContext(ctx, query,
				append([]any{m}, khArgs...)...).Scan(&at, &id, &status, &result)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return err
			}
			cur := out[m]
			apply(&cur, at, id, status, result)
			out[m] = cur
		}
		return nil
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
	rows, err := s.ro.QueryContext(ctx, `
		SELECT time + duration_ms,
			CASE WHEN status_code >= 400 OR (result != '' AND result != 'completed') THEN 1 ELSE 0 END
		FROM logs WHERE time + duration_ms >= ? AND log_source != 'rejected'
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
