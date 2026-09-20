// 本文件是 logs 预聚合 rollup（log_cells/log_err_cells 两表）的单一
// 事实源：列注册表、DDL、回填/补漏 INSERT SELECT、写路径行贡献
// （Go 镜像）与读侧 SUM 清单全部由 cellMetrics 一份登记表派生。
//
// 设计要点：
//   - 格子键 (slot, day, api, emodel, key_hash)：slot=time/600000 是
//     600 秒桶序号；day 是逐行物化的本地日期（strftime localtime 同
//     源）——600s 槽会横跨本地午夜（时区偏移非 600s 整数倍，如 +5:45
//     尼泊尔），把 day 作维度而非派生谓词，让「按自然日聚合」对全部
//     时区都精确；slot 查询对跨界 cell 直接 SUM 两行，同样精确。
//   - 全部指标列是可加量（SUM/COUNT 形态），跨粒度任意再聚合；
//     min_time 记最早行时刻（entries 的 window_start），last_key 是
//     printf('%020d',id)|started_at 打包串——字符串 MAX 等价于
//     「最大 id 行的 started_at」，替代裸列绑定（同查询里还有
//     MIN(time)，多个 min/max 聚合下裸列归属不确定）。
//   - 覆盖水位线 runtime_state.log_cells_covered_id：所有 id ≤ 水位
//     的 logs 行都已完成 rollup 记账（非 rejected 已入格、rejected
//     被有意剔除）。双写路径在同一事务推进水位；任何绕过双写的
//     写入（无 cells 码的旧二进制、外部工具、importIndex）由
//     Open 收尾的 ReconcileCells 补记。
//   - rejected 行（管线前拒绝）不进 rollup——全部聚合口径本来就
//     剔除它；log_err_cells 只记 error_stage != ” 的行（错误稀疏，
//     单列迷你表比给主表加错误维度便宜得多）。
package store

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// cellMetrics 依序登记 log_cells 的 40 个可加指标列：DDL、回填
// SELECT 的 SUM(expr)、upsert 的增量 SET 与 Go 侧贡献字段全按此
// 顺序对齐——登记表即契约，cellsConsistencyTest 钉死 Go 镜像与
// SQL 表达式的逐行等价。
var cellMetrics = []struct {
	name string // 列名
	expr string // 单行贡献的 SQL 表达式（logs 行作用域）
}{
	{"req", `1`},
	{"disc", `CASE WHEN result IN ('disconnected', 'aborted') THEN 1 ELSE 0 END`},
	{"err", `CASE WHEN result NOT IN ('disconnected', 'aborted') AND (status_code >= 400 OR result = 'failed') THEN 1 ELSE 0 END`},
	{"rltd", `CASE WHEN status_code = 429 OR rate_limited != 0 THEN 1 ELSE 0 END`},
	{"cfault", `CASE WHEN ` + logOwnerCase + ` = 'client' THEN 1 ELSE 0 END`},
	{"ufault", `CASE WHEN ` + logOwnerCase + ` = 'upstream' THEN 1 ELSE 0 END`},
	{"ok", `CASE WHEN status_code >= 200 AND status_code < 300 THEN 1 ELSE 0 END`},
	{"gone", `CASE WHEN status_code = 499 THEN 1 ELSE 0 END`},
	{"lim", `CASE WHEN status_code = 429 THEN 1 ELSE 0 END`},
	{"n_dur", `CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END`},
	{"n_dur_ok", `CASE WHEN status_code >= 200 AND status_code < 300 AND duration_ms > 0 THEN 1 ELSE 0 END`},
	{"in_tok", `input_tokens`},
	{"out_tok", `output_tokens`},
	{"cr_tok", `cache_read_tokens`},
	{"cw_tok", `cache_write_tokens`},
	{"in_ng", `CASE WHEN status_code != 499 THEN input_tokens ELSE 0 END`},
	{"out_ng", `CASE WHEN status_code != 499 THEN output_tokens ELSE 0 END`},
	{"cr_ng", `CASE WHEN status_code != 499 THEN cache_read_tokens ELSE 0 END`},
	{"cw_ng", `CASE WHEN status_code != 499 THEN cache_write_tokens ELSE 0 END`},
	{"reas", `reasoning_tokens`},
	{"tot_tok", `total_tokens`},
	{"credit", `credit_cost`},
	{"sum_dur", `CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END`},
	{"sum_dur_ok", `CASE WHEN status_code >= 200 AND status_code < 300 THEN duration_ms ELSE 0 END`},
	{"n_first_ok", `CASE WHEN stream != 0 AND status_code >= 200 AND status_code < 300 AND first_upstream_ms > 0 THEN 1 ELSE 0 END`},
	{"sum_first_ok", `CASE WHEN stream != 0 AND status_code >= 200 AND status_code < 300 AND first_upstream_ms > 0 THEN first_upstream_ms ELSE 0 END`},
	{"n_first_stream", `CASE WHEN stream != 0 AND first_upstream_ms IS NOT NULL THEN 1 ELSE 0 END`},
	{"sum_first_stream", `CASE WHEN stream != 0 AND first_upstream_ms IS NOT NULL THEN first_upstream_ms ELSE 0 END`},
	{"n_nonstream", `CASE WHEN stream = 0 THEN 1 ELSE 0 END`},
	{"sum_dur_ns", `CASE WHEN stream = 0 THEN duration_ms ELSE 0 END`},
	{"n_stream_ng", `CASE WHEN status_code != 499 AND stream != 0 THEN 1 ELSE 0 END`},
	{"n_nonstream_ng", `CASE WHEN status_code != 499 AND stream = 0 THEN 1 ELSE 0 END`},
	{"sum_gen", `CASE WHEN duration_ms > 0 THEN
		CASE WHEN first_upstream_ms > 0 AND first_upstream_ms < duration_ms
			THEN duration_ms - first_upstream_ms ELSE duration_ms END
		ELSE 0 END`},
	{"n_ttfb", `CASE WHEN first_upstream_ms IS NOT NULL THEN 1 ELSE 0 END`},
	{"sum_ttfb", `COALESCE(first_upstream_ms, 0)`},
	{"gen_ms", `CASE WHEN ` + logDecodeCond + ` THEN duration_ms - first_upstream_ms ELSE 0 END`},
	{"gen_out", `CASE WHEN ` + logDecodeCond + ` THEN output_tokens ELSE 0 END`},
	{"sum_dur_all", `duration_ms`},
	// slack = 首字节下发后请求的剩余时长（duration_ms − first_client_ms），
	// 即死读者写阻塞的暴露窗口；只在 first_client_ms > 0 的行上有定义，
	// 缺席/零值行贡献 0 且不计 n_slack（沿用 n_ttfb/sum_ttfb 的 n_+sum_ 对）。
	{"n_slack", `CASE WHEN first_client_ms > 0 THEN 1 ELSE 0 END`},
	{"sum_slack_ms", `CASE WHEN first_client_ms > 0 THEN MAX(0, duration_ms - first_client_ms) ELSE 0 END`},
}

// cellMetricNames 是 cellMetrics 的列名清单（INSERT 列序）。
var cellMetricNames = strings.Join(func() []string {
	out := make([]string, len(cellMetrics))
	for i, m := range cellMetrics {
		out[i] = m.name
	}
	return out
}(), ", ")

// cellSumList 把逗号分隔列清单编译成 COALESCE(SUM(),0) 聚合清单——
// 读侧 UNION 外层的 SELECT；无 GROUP BY 的标量查询在空集上也得 0。
func cellSumList(cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = "COALESCE(SUM(" + strings.TrimSpace(p) + "),0)"
	}
	return strings.Join(parts, ", ")
}

// cellUsageCols 是 UsageTotals 15 字段的 rollup 列序——与
// usageTotalsDests 的扫描顺序一一对应。
const cellUsageCols = `req, disc, err, rltd, cfault, ufault, in_tok, out_tok, cr_tok, cw_tok, reas, tot_tok, credit, gen_ms, gen_out`

// cellTotalsCols 是 LogCellTotals 25 字段的 rollup 列序——与
// LogCells 的扫描顺序一一对应。
const cellTotalsCols = `req, ok, gone, lim, n_dur, n_dur_ok, in_tok, out_tok, cr_tok, cw_tok, in_ng, out_ng, cr_ng, cw_ng, sum_dur, sum_dur_ok, sum_first_ok, n_first_ok, sum_first_stream, n_first_stream, sum_dur_ns, n_nonstream, n_stream_ng, n_nonstream_ng, sum_gen`

// logCellsDDL/logErrCellsDDL 由 schemaStatements 引用；指标列从
// cellMetrics 派生，加列只改登记表一处。
var logCellsDDL = `CREATE TABLE IF NOT EXISTS log_cells (
	slot INTEGER NOT NULL,
	day TEXT NOT NULL,
	api TEXT NOT NULL,
	emodel TEXT NOT NULL,
	key_hash TEXT NOT NULL,
	` + cellMetricNames + `,
	min_time INTEGER NOT NULL,
	last_key TEXT NOT NULL,
	PRIMARY KEY (slot, day, api, emodel, key_hash)
)`

const logErrCellsDDL = `CREATE TABLE IF NOT EXISTS log_err_cells (
	slot INTEGER NOT NULL,
	stage TEXT NOT NULL,
	req INTEGER NOT NULL,
	PRIMARY KEY (slot, stage)
)`

// cellsConflict 是主表 upsert 的冲突子句：可加列累加，min_time 取小，
// last_key 取字符串大者（= 更大 id 的行）。
var cellsConflict = ` ON CONFLICT(slot, day, api, emodel, key_hash) DO UPDATE SET ` + strings.Join(func() []string {
	sets := make([]string, 0, len(cellMetrics)+2)
	for _, m := range cellMetrics {
		sets = append(sets, m.name+" = log_cells."+m.name+" + excluded."+m.name)
	}
	return append(sets,
		"min_time = MIN(log_cells.min_time, excluded.min_time)",
		"last_key = MAX(log_cells.last_key, excluded.last_key)")
}(), ", ")

// errCellsConflict 是错误迷你表的 upsert 冲突子句（单计数列累加）。
const errCellsConflict = ` ON CONFLICT(slot, stage) DO UPDATE SET req = log_err_cells.req + excluded.req`

// cellsSelectSQL 生成「logs 行 → 五维格子键」的聚合 SELECT：40 个
// 指标列的 SUM 表达式、min_time/last_key 两个非可加列全按 cellMetrics
// 一份登记表派生。where 是追加在 rejected 谓词后的行范围片段。
// 迁移回填、水位补漏与窗口重算共用同一投影——口径只有一份。
func cellsSelectSQL(where string) string {
	exprs := make([]string, 0, len(cellMetrics))
	for _, m := range cellMetrics {
		exprs = append(exprs, "SUM("+m.expr+")")
	}
	return `SELECT time/600000, ` + logDayExpr + `, api, ` + logEModelExpr + `, key_hash, ` +
		strings.Join(exprs, ", ") + `, MIN(time), MAX(printf('%020d', id)||'|'||started_at)
		FROM logs WHERE log_source != 'rejected' ` + where +
		` GROUP BY 1, 2, 3, 4, 5`
}

// cellsInsertSQL 生成增量 upsert 形态的回填语句：冲突时累加——适用
// 场景是「该行的贡献尚未入账」。ReconcileCells 补漏按 id 区间
// （"AND id > ? AND id <= ?"）分片调用。
func cellsInsertSQL(where string) string {
	return `INSERT INTO log_cells(slot, day, api, emodel, key_hash, ` + cellMetricNames + `, min_time, last_key)
		` + cellsSelectSQL(where) + cellsConflict
}

var (
	cellsGapSQL = cellsInsertSQL("AND id > ? AND id <= ?")
	// cellsUpsertSQL 是写路径单行/批量共用的 VALUES 形态 upsert。
	cellsUpsertSQL = `INSERT INTO log_cells(slot, day, api, emodel, key_hash, ` + cellMetricNames +
		`, min_time, last_key) VALUES(` + placeholders(5+len(cellMetrics)+2) + `)` + cellsConflict
)

var (
	// cellsRebuildSQL/errCellsRebuildSQL 是 BackfillCells 的窗口重算
	// 语句：同一聚合投影但冲突语义是整行 REPLACE——格子被重写成「当前
	// 源行应产生的值」，重跑收敛到同一结果（增量 upsert 重跑会双计）。
	// `id <= ?`（水位）把重算域限定在已记账 id 域内：水位外行仍归
	// 读侧补尾与 ReconcileCells 的增量补记，若一并 REPLACE 进格子
	// 会被两侧各算一次。
	cellsRebuildSQL = `INSERT OR REPLACE INTO log_cells(slot, day, api, emodel, key_hash, ` + cellMetricNames +
		`, min_time, last_key) ` + cellsSelectSQL(`AND id <= ? AND time/600000 BETWEEN ? AND ?`)
	errCellsRebuildSQL = `INSERT OR REPLACE INTO log_err_cells(slot, stage, req)
		SELECT time/600000, error_stage, COUNT(*) FROM logs
		WHERE log_source != 'rejected' AND error_stage != '' AND id <= ? AND time/600000 BETWEEN ? AND ?
		GROUP BY 1, 2`
)

var (
	errCellsGapSQL = `INSERT INTO log_err_cells(slot, stage, req)
		SELECT time/600000, error_stage, COUNT(*) FROM logs
		WHERE log_source != 'rejected' AND error_stage != '' AND id > ? AND id <= ?
		GROUP BY 1, 2` + errCellsConflict
	errCellsUpsertSQL = `INSERT INTO log_err_cells(slot, stage, req) VALUES(?,?,?)` + errCellsConflict
)

// cellsWatermarkKey 是 rollup 覆盖水位线在 runtime_state 的键。
const cellsWatermarkKey = "log_cells_covered_id"

// cellsWatermark 读当前覆盖水位（缺席按 0——全表未记账）。
func cellsWatermark(ctx context.Context, q dbtx) (int64, error) {
	var v string
	switch err := q.QueryRowContext(ctx, `SELECT value FROM runtime_state WHERE "key" = ?`, cellsWatermarkKey).Scan(&v); {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// setCellsWatermark 在事务内推进水位。id 单调下写新值即等价 MAX，
// 但 reuseport 交接期新旧两写者并发，本方读水位到提交之间对侧可能
// 已推进更高值——冲突取 MAX 防迟到提交把水位回写变小（水位回退
// 会让已记账行被下次补漏重新聚合，双计贡献）。
func setCellsWatermark(ctx context.Context, q dbtx, id int64) error {
	_, err := q.ExecContext(ctx, `INSERT INTO runtime_state("key", value, updated_at) VALUES(?,?,?)
		ON CONFLICT("key") DO UPDATE SET
			value = CAST(MAX(CAST(value AS INTEGER), CAST(excluded.value AS INTEGER)) AS TEXT),
			updated_at = excluded.updated_at`,
		cellsWatermarkKey, strconv.FormatInt(id, 10), time.Now().UnixMilli())
	return err
}

// cellDim 是 log_cells 主键的 Go 形态。
type cellDim struct {
	slot    int64
	day     string
	api     string
	emodel  string
	keyHash string
}

// errCellDim 是 log_err_cells 主键的 Go 形态。
type errCellDim struct {
	slot  int64
	stage string
}

// cellVals 是单 cell 的批内累加器：40 个可加列与 cellMetrics 同序
// 对应；minTime/lastKey 跟踪两个非可加列。
type cellVals struct {
	req, disc, err, rltd, cfault, ufault int64
	ok, gone, lim, nDur, nDurOK          int64
	inTok, outTok, crTok, cwTok          int64
	inNG, outNG, crNG, cwNG              int64
	reas, totTok, credit                 int64
	sumDur, sumDurOK                     int64
	nFirstOK, sumFirstOK                 int64
	nFirstStream, sumFirstStream         int64
	nNonstream, sumDurNS                 int64
	nStreamNG, nNonstreamNG              int64
	sumGen                               int64
	nTTFB, sumTTFB                       int64
	genMS, genOut                        int64
	sumDurAll                            int64
	nSlack, sumSlackMS                   int64
	minTime                              int64
	lastKey                              string
}

// args 按 cellMetrics 顺序展开加值列实参（随后接 min_time, last_key）。
func (v *cellVals) args() []any {
	return []any{
		v.req, v.disc, v.err, v.rltd, v.cfault, v.ufault,
		v.ok, v.gone, v.lim, v.nDur, v.nDurOK,
		v.inTok, v.outTok, v.crTok, v.cwTok,
		v.inNG, v.outNG, v.crNG, v.cwNG,
		v.reas, v.totTok, v.credit,
		v.sumDur, v.sumDurOK,
		v.nFirstOK, v.sumFirstOK,
		v.nFirstStream, v.sumFirstStream,
		v.nNonstream, v.sumDurNS,
		v.nStreamNG, v.nNonstreamNG,
		v.sumGen,
		v.nTTFB, v.sumTTFB,
		v.genMS, v.genOut,
		v.sumDurAll,
		v.nSlack, v.sumSlackMS,
	}
}

// logCellVals 计算一条 logs 行的 40 列贡献——逐谓词镜像 cellMetrics
// 的 SQL 表达式（cellsConsistencyTest 验证两边逐行等价）。
func logCellVals(e *LogRow) cellVals {
	var v cellVals
	v.req = 1
	disc := e.Result == "disconnected" || e.Result == "aborted"
	if disc {
		v.disc = 1
	}
	if !disc && (e.StatusCode >= 400 || e.Result == "failed") {
		v.err = 1
	}
	if e.StatusCode == 429 || e.RateLimited {
		v.rltd = 1
	}
	switch {
	case e.Result == "rejected":
		// 'none'
	case e.StatusCode == 429 || e.RateLimited:
		// business_limited——两个归因列都不计。
	case disc:
		v.cfault = 1
	case e.StatusCode < 400 && e.Result != "failed":
		// 'none'
	case e.ErrorStage == "http_read" || e.ErrorStage == "http_decode":
		v.cfault = 1
	default:
		v.ufault = 1
	}
	ok2xx := e.StatusCode >= 200 && e.StatusCode < 300
	if ok2xx {
		v.ok = 1
	}
	if e.StatusCode == 499 {
		v.gone = 1
	}
	if e.StatusCode == 429 {
		v.lim = 1
	}
	if e.DurationMS > 0 {
		v.nDur = 1
		v.sumDur = e.DurationMS
	}
	if ok2xx && e.DurationMS > 0 {
		v.nDurOK = 1
	}
	v.inTok, v.outTok, v.crTok, v.cwTok = e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens
	if e.StatusCode != 499 {
		v.inNG, v.outNG, v.crNG, v.cwNG = e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens
		if e.Stream {
			v.nStreamNG = 1
		} else {
			v.nNonstreamNG = 1
		}
	}
	v.reas, v.totTok, v.credit = e.ReasoningTokens, e.TotalTokens, e.CreditCost
	if ok2xx {
		v.sumDurOK = e.DurationMS
	}
	var fbt int64
	hasFBT := e.FirstUpstreamMS != nil
	if hasFBT {
		fbt = *e.FirstUpstreamMS
	}
	if e.Stream && ok2xx && hasFBT && fbt > 0 {
		v.nFirstOK = 1
		v.sumFirstOK = fbt
	}
	if e.Stream && hasFBT {
		v.nFirstStream = 1
		v.sumFirstStream = fbt
	}
	if !e.Stream {
		v.nNonstream = 1
		v.sumDurNS = e.DurationMS
	}
	if e.DurationMS > 0 {
		if hasFBT && fbt > 0 && fbt < e.DurationMS {
			v.sumGen = e.DurationMS - fbt
		} else {
			v.sumGen = e.DurationMS
		}
	}
	if hasFBT {
		v.nTTFB = 1
		v.sumTTFB = fbt
	}
	if e.Result == "completed" && hasFBT && e.OutputTokens > 0 &&
		e.DurationMS-fbt > 0 && e.OutputTokens*1000 <= 400*(e.DurationMS-fbt) {
		v.genMS = e.DurationMS - fbt
		v.genOut = e.OutputTokens
	}
	v.sumDurAll = e.DurationMS
	if e.FirstClientMS != nil && *e.FirstClientMS > 0 {
		v.nSlack = 1
		if slack := e.DurationMS - *e.FirstClientMS; slack > 0 {
			v.sumSlackMS = slack
		}
	}
	return v
}

// dayCache 把「时刻→本地日串」的格式化摊到批级：批内行几乎总落在同一
// 本地日，缓存以 [lo,hi) unix 秒区间命中；跨界时按 time.Date 重算界点
// （AddDate 取次日界自动吸收 DST 的 23/25 小时日——不能 +86400）。
// 与 strftime('%Y-%m-%d', time/1000, 'unixepoch', 'localtime') 同源。
type dayCache struct {
	lo, hi int64
	day    string
}

// dayOf 返回 t 的本地日串（"2006-01-02"）。零值缓存首查必然装填。
func (c *dayCache) dayOf(t time.Time) string {
	if sec := t.Unix(); sec >= c.lo && sec < c.hi {
		return c.day
	}
	lt := t.Local()
	y, m, d := lt.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, lt.Location())
	c.lo, c.hi = start.Unix(), start.AddDate(0, 0, 1).Unix()
	c.day = lt.Format("2006-01-02")
	return c.day
}

// addCellContrib 把一条已入库日志行累加进批内聚合器；id 是该行实际
// 分配到的自增主键（INSERT OR IGNORE 跳过的重复行不进这里——首个
// 落库者已在它自己的事务里记过账）。source=='rejected' 的行被剔除。
func addCellContrib(cells map[cellDim]*cellVals, errs map[errCellDim]int64, e *LogRow, id int64, days *dayCache) {
	source := e.LogSource
	if source == "" {
		source = "proxy"
	}
	if source == "rejected" {
		return
	}
	ms := e.StartedAt.UnixMilli()
	emodel := e.Model
	if emodel == "" {
		emodel = e.RequestedModel
	}
	slot := ms / 600000
	dim := cellDim{slot: slot, day: days.dayOf(e.StartedAt),
		api: e.API, emodel: emodel, keyHash: e.KeyHash}
	acc := cells[dim]
	if acc == nil {
		acc = &cellVals{minTime: ms}
		cells[dim] = acc
	}
	contrib := logCellVals(e)
	acc.req += contrib.req
	acc.disc += contrib.disc
	acc.err += contrib.err
	acc.rltd += contrib.rltd
	acc.cfault += contrib.cfault
	acc.ufault += contrib.ufault
	acc.ok += contrib.ok
	acc.gone += contrib.gone
	acc.lim += contrib.lim
	acc.nDur += contrib.nDur
	acc.nDurOK += contrib.nDurOK
	acc.inTok += contrib.inTok
	acc.outTok += contrib.outTok
	acc.crTok += contrib.crTok
	acc.cwTok += contrib.cwTok
	acc.inNG += contrib.inNG
	acc.outNG += contrib.outNG
	acc.crNG += contrib.crNG
	acc.cwNG += contrib.cwNG
	acc.reas += contrib.reas
	acc.totTok += contrib.totTok
	acc.credit += contrib.credit
	acc.sumDur += contrib.sumDur
	acc.sumDurOK += contrib.sumDurOK
	acc.nFirstOK += contrib.nFirstOK
	acc.sumFirstOK += contrib.sumFirstOK
	acc.nFirstStream += contrib.nFirstStream
	acc.sumFirstStream += contrib.sumFirstStream
	acc.nNonstream += contrib.nNonstream
	acc.sumDurNS += contrib.sumDurNS
	acc.nStreamNG += contrib.nStreamNG
	acc.nNonstreamNG += contrib.nNonstreamNG
	acc.sumGen += contrib.sumGen
	acc.nTTFB += contrib.nTTFB
	acc.sumTTFB += contrib.sumTTFB
	acc.genMS += contrib.genMS
	acc.genOut += contrib.genOut
	acc.sumDurAll += contrib.sumDurAll
	acc.nSlack += contrib.nSlack
	acc.sumSlackMS += contrib.sumSlackMS
	if ms < acc.minTime {
		acc.minTime = ms
	}
	// last_key 的打包串镜像 SQL 的 printf('%020d',id)||'|'||started_at：
	// 零填充定宽是字典序等价数值序的前提，栈缓冲 Append 替代 Sprintf。
	var kb, lb [64]byte
	dig := strconv.AppendInt(kb[:0], id, 10)
	b := lb[:0]
	for i := len(dig); i < 20; i++ {
		b = append(b, '0')
	}
	b = append(b, dig...)
	b = append(b, '|')
	b = e.StartedAt.AppendFormat(b, time.RFC3339Nano)
	if lastKey := string(b); lastKey > acc.lastKey {
		acc.lastKey = lastKey
	}
	if e.ErrorStage != "" {
		errs[errCellDim{slot: slot, stage: e.ErrorStage}]++
	}
}

// upsertCells 把批内聚合器逐组 upsert 进 rollup 表（同事务）。调用方
// 保证地图非 nil；空地图是廉价空转。
func upsertCells(ctx context.Context, q dbtx, cells map[cellDim]*cellVals, errs map[errCellDim]int64) error {
	for dim, acc := range cells {
		args := append([]any{dim.slot, dim.day, dim.api, dim.emodel, dim.keyHash}, acc.args()...)
		args = append(args, acc.minTime, acc.lastKey)
		if _, err := q.ExecContext(ctx, cellsUpsertSQL, args...); err != nil {
			return err
		}
	}
	for dim, n := range errs {
		if _, err := q.ExecContext(ctx, errCellsUpsertSQL, dim.slot, dim.stage, n); err != nil {
			return err
		}
	}
	return nil
}

// reconcileGapRows 是 ReconcileCells 单片事务补记的行数上界：缝隙
// 补记是 logs 范围扫 + 聚合 upsert，无界单事务（prod 实证 7,634 行
// 缝隙）会在启动期数十秒独占唯一写连接——reuseport 交接期恰好还有
// 一个共享库的在役实例，它的写流会被饿死。片级提交让排队写者在片间
// 插队；水位随每片推进，崩溃留下正确的部分位置（id ≤ 水位 ⇒ 已记账
// 的恒真式不被破坏）。
const reconcileGapRows = 5000

// ReconcileCells 把水位线之后落库的非 rejected 行补记进 rollup——
// 绕过双写的写入者有无 cells 码的旧二进制、外部工具与 importIndex：
// Open 收尾固定调它闭合这类缝隙（9-19 实证 7,634 行险些永隐），
// ImportLegacy 收尾再补一次导入期写入；常规调用是 id>水位 的空扫。
func (s *Store) ReconcileCells(ctx context.Context) error {
	return s.reconcileCells(ctx, reconcileGapRows)
}

// reconcileCells 按 chunk 行分片补记（chunk 是测试缝：小片逼多轮
// 循环以验证分片终态与单遍等价）。
func (s *Store) reconcileCells(ctx context.Context, chunk int64) error {
	// 「最大未记账 id」一个 ro 读兼任两职：空缝快退判据与全程上界。
	// 无未记账行时纯读返回，不开写事务——水位随每行双写推进，缝隙只
	// 来自绕过双写的写入者，常规启动这里是空扫；但即便是空扫，写事务
	// 在共享库（交接期与在役实例并发）上也会被在役写流饿死到
	// SQLITE_BUSY，让本可无锁的路径死在启动期。
	//
	// 上界取扫描时刻的快照而非逐片重读：循环期间新落的行归其写者的
	// 双写记账（绕过双写者留下的由下次 Open 补记，期间有 UNION 补尾
	// 段兜底不重不漏）——追移动靶会让交接重叠期的补记变成无界占用。
	var bound int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id),0) FROM logs WHERE id > `+cellsWatermarkSQL).Scan(&bound); err != nil {
		return err
	}
	if bound == 0 {
		return nil
	}
	for {
		done, err := s.reconcileCellsChunk(ctx, bound, chunk)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// reconcileCellsChunk 补记一片：水位之上按 id 序取至多 chunk 行聚合
// upsert，水位在同一事务推进到该片最大 id——每片原子提交，中途失败
// 或进程重启后下一轮从已推进的水位续跑。返回 false 表示缝隙尚有余量。
// 片内先读水位再聚合写入——本函数在 Open 收尾（交接窗内）与 ImportLegacy
// 收尾点火，BEGIN IMMEDIATE 借 busy_timeout 排队等锁（同 applyMigrations）。
func (s *Store) reconcileCellsChunk(ctx context.Context, bound, chunk int64) (bool, error) {
	var drained bool
	err := writeTx(ctx, s.db.DB, "ReconcileCells", func(ctx context.Context, q dbtx) error {
		wm, err := cellsWatermark(ctx, q)
		if err != nil {
			return err
		}
		// 片上界 = 水位之上第 chunk 个存量 id（DeleteLogsBefore 同款内层
		// SELECT 定批）。水位在每片事务内重读：并发双写若在片间推进了它，
		// 本片从最新位置续起，不重扫已记账区间；对侧新行的 id 恒大于
		// bound（AUTOINCREMENT 不复用），永不进本方聚合域。
		var hi int64
		if err := q.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id),0) FROM (
				SELECT id FROM logs WHERE id > ? AND id <= ? ORDER BY id LIMIT ?)`,
			wm, bound, chunk).Scan(&hi); err != nil {
			return err
		}
		if hi == 0 {
			// (wm, bound] 已无存量行：缝隙闭合（行被并发删除，或水位
			// 已被并发写者推过 bound）。水位仍落后 bound 时补齐——与
			// 单遍版 wm=MAX(id) 终态一致，区间空洞不再重扫。
			if wm < bound {
				if err := setCellsWatermark(ctx, q, bound); err != nil {
					return err
				}
			}
			drained = true
			return nil
		}
		if _, err := q.ExecContext(ctx, cellsGapSQL, wm, hi); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, errCellsGapSQL, wm, hi); err != nil {
			return err
		}
		if err := setCellsWatermark(ctx, q, hi); err != nil {
			return err
		}
		drained = hi == bound
		return nil
	})
	if err != nil {
		return false, err
	}
	return drained, nil
}

// ── 水位内缺口的窗口重算 ──────────────────────────────────────────
//
// ReconcileCells 只补水位之后的行；「水位已覆盖但格子缺失/残缺」的
// 历史洞（cells 流水线接管全部写路径之前由旧二进制写下的行，其 id
// 永远 ≤ 水位）需要另一件工具：按 slot 窗口把格子整行重算重写。
// REPLACE 语义让重复执行收敛到同一份真值，不产生增量 upsert 的重跑
// 双计；重算域限定 id ≤ 水位，水位外行留给读侧补尾与补漏记账。

// CellsAuditReport 汇总一个 slot 窗口（闭区间）内「源行 vs 格子」的
// 对账结果；对账域是 id ≤ Watermark 的非 rejected 行（水位外行归
// ReconcileCells，见 TailRows）。
type CellsAuditReport struct {
	SlotLo, SlotHi  int64 // 审计窗口
	Watermark       int64 // 对账时刻的覆盖水位
	Rows            int64 // 窗口内 id≤水位 的非 rejected 源行数
	Dims            int64 // 源行聚合出的维度数
	MismatchDims    int64 // src.req != cell.req 的维度数
	DeficitRows     int64 // 失配维度上 Σ(src-cell)，带符号
	SurplusDims     int64 // cell.req > src.req 的维度数（REPLACE 会按源真值重写）
	OrphanCells     int64 // 窗口内有格无源的维度数（源行已被删净的残格）
	ErrDims         int64 // err 侧 (slot,stage) 源组数
	ErrMismatchDims int64
	ErrDeficitRows  int64
	ErrOrphans      int64
	TailRows        int64 // 窗口内 id>水位 的行数（不属本对账域）
}

// CellsBackfillReport 是 BackfillCells 的结算：Pre 为动手前审计，
// Post 为写入后复测（干跑时与 Pre 相同——Remaining 即 Pre 自身）。
type CellsBackfillReport struct {
	Pre     CellsAuditReport
	Post    CellsAuditReport
	Applied bool // false=干跑（未写库）
}

// cellSrcPred 是对账/重算共用的源行域谓词与实参序：水位内 + 窗口内
// 的非 rejected 行。实参恒为 (wm, lo, hi)。
const cellSrcPred = `log_source != 'rejected' AND id <= ? AND time/600000 BETWEEN ? AND ?`

// CellsAudit 只读对账一个 slot 窗口的 rollup 覆盖——返回的失配集即
// BackfillCells 会修复的对象。lo=0/hi=math.MaxInt64 即全表扫。
// 水位与计数放在同一读事务里取：谓词以水位分域，两个读快照不拼在
// 同一事务会出「行已记账但水位读旧」的假缺口。
func (s *Store) CellsAudit(ctx context.Context, slotLo, slotHi int64) (*CellsAuditReport, error) {
	conn, err := s.ro.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `BEGIN`); err != nil {
		return nil, err
	}
	defer func() { _, _ = conn.ExecContext(context.Background(), `ROLLBACK`) }()
	wm, err := cellsWatermark(ctx, conn)
	if err != nil {
		return nil, err
	}
	return s.cellsAuditTx(ctx, conn, wm, slotLo, slotHi)
}

// BackfillCells 重算 [slotLo, slotHi] 窗口内全部格子的 rollup——
// 「水位内无格子」历史洞的修复路径（9-19 prod 实证：cells 化二进制
// 与旧二进制 reuseport 交接期交叠写库，留下 47 维 / 8,582 行的洞，
// 行 id 均 ≤ 水位，ReconcileCells 的 id>水位 补漏永远够不着）。
//
// apply=false 时只审计不写库（Pre 即缺口报告）。apply=true 时在
// 一个 BEGIN IMMEDIATE 事务里顺序做：完整性守卫 → 审计 → 两表
// INSERT OR REPLACE → 复测。守卫与写入同事务共享快照；事务对prod
// 写者串行（busy_timeout 兜底等待），窗口内新落的行要么先于事务
// 提交（被重算吃进）要么在其后（双写 upsert 叠在重算格上）——
// 两种交错都收敛，故不阻塞运行中实例执行。
//
// 两道守卫把「源行已被部分删除」的窗口拦在重写前（REPLACE 会把格子
// 写小、永久毁掉格子里尚存的历史）：
//   - 完整性：占用 slot 的格底 < 全表 MIN(time) ⇒ 前缀删除已越过该
//     slot 下缘（retention 的 DELETE FROM logs WHERE time < cutoff
//     只可能是部分删除的来源）。窗口内任一占用 slot 破缺即整体中止，
//     不跳过单格——「缺口+部分删除」在对账面上不可分。
//   - 盈余：某 slot 的 SUM(cell.req) > 存活源行数 ⇒ 出现了非前缀
//     删除或外部改写，REPLACE 修复前提不成立，中止。
func (s *Store) BackfillCells(ctx context.Context, slotLo, slotHi int64, apply bool) (*CellsBackfillReport, error) {
	if slotLo < 0 || slotLo > slotHi {
		return nil, fmt.Errorf("bad slot range %d:%d", slotLo, slotHi)
	}
	var rep *CellsBackfillReport
	err := writeTx(ctx, s.db.DB, "BackfillCells", func(ctx context.Context, q dbtx) error {
		wm, err := cellsWatermark(ctx, q)
		if err != nil {
			return err
		}
		breached, err := cellsBreachedSlots(ctx, q, wm, slotLo, slotHi)
		if err != nil {
			return err
		}
		if len(breached) > 0 {
			return fmt.Errorf("slots %v not intact (slot floor below MIN(logs.time)——源行已被部分删除，REPLACE 会把格子写小；先人工核对再决定放行)", breached)
		}
		surplus, err := cellsSurplusSlots(ctx, q, wm, slotLo, slotHi)
		if err != nil {
			return err
		}
		if len(surplus) > 0 {
			return fmt.Errorf("slots %v have surplus cells (SUM(cell req) > 存活源行——非前缀删除或外部改写痕迹，中止)", surplus)
		}
		pre, err := s.cellsAuditTx(ctx, q, wm, slotLo, slotHi)
		if err != nil {
			return err
		}
		rep = &CellsBackfillReport{Pre: *pre, Post: *pre}
		if !apply {
			return nil
		}
		if _, err := q.ExecContext(ctx, cellsRebuildSQL, wm, slotLo, slotHi); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, errCellsRebuildSQL, wm, slotLo, slotHi); err != nil {
			return err
		}
		post, err := s.cellsAuditTx(ctx, q, wm, slotLo, slotHi)
		if err != nil {
			return err
		}
		rep.Post = *post
		rep.Applied = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rep, nil
}

// cellsAuditTx 在连接当前事务快照上跑窗口对账（调用方负责事务）。
func (s *Store) cellsAuditTx(ctx context.Context, q dbtx, wm, lo, hi int64) (*CellsAuditReport, error) {
	r := &CellsAuditReport{SlotLo: lo, SlotHi: hi, Watermark: wm}
	args := func() []any { return []any{wm, lo, hi} }
	scan := func(query string, dests ...any) error {
		return q.QueryRowContext(ctx, query, args()...).Scan(dests...)
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE log_source != 'rejected' AND id > ? AND time/600000 BETWEEN ? AND ?`,
		wm, lo, hi).Scan(&r.TailRows); err != nil {
		return nil, err
	}
	if err := scan(`WITH dims AS (
			SELECT time/600000 AS slot, `+logDayExpr+` AS day, api, `+logEModelExpr+` AS emodel, key_hash, COUNT(*) AS total
			FROM logs WHERE `+cellSrcPred+` GROUP BY 1,2,3,4,5)
		SELECT COUNT(*), COALESCE(SUM(d.total),0),
			COALESCE(SUM(CASE WHEN d.total != COALESCE(c.req,0) THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN d.total != COALESCE(c.req,0) THEN d.total - COALESCE(c.req,0) END),0),
			COALESCE(SUM(CASE WHEN d.total < COALESCE(c.req,0) THEN 1 ELSE 0 END),0)
		FROM dims d LEFT JOIN log_cells c
			ON c.slot=d.slot AND c.day=d.day AND c.api=d.api AND c.emodel=d.emodel AND c.key_hash=d.key_hash`,
		&r.Dims, &r.Rows, &r.MismatchDims, &r.DeficitRows, &r.SurplusDims); err != nil {
		return nil, err
	}
	// 残格计数：格子还在但维度下已无任何源行（前缀删除越过该 dim
	// 但 slot 内尚有其他源行时 Layer-1 拦不住，这里如实报告）。
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM log_cells c WHERE c.slot BETWEEN ? AND ? AND NOT EXISTS (
			SELECT 1 FROM logs l WHERE l.log_source != 'rejected' AND l.id <= ?
			AND l.time/600000 = c.slot
			AND strftime('%Y-%m-%d', l.time/1000, 'unixepoch', 'localtime') = c.day
			AND l.api = c.api
			AND CASE WHEN l.model != '' THEN l.model ELSE l.requested_model END = c.emodel
			AND l.key_hash = c.key_hash)`, lo, hi, wm).Scan(&r.OrphanCells); err != nil {
		return nil, err
	}
	if err := scan(`WITH errs AS (
			SELECT time/600000 AS slot, error_stage AS stage, COUNT(*) AS total
			FROM logs WHERE `+cellSrcPred+` AND error_stage != '' GROUP BY 1,2)
		SELECT COUNT(*),
			COALESCE(SUM(CASE WHEN e.total != COALESCE(c.req,0) THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN e.total != COALESCE(c.req,0) THEN e.total - COALESCE(c.req,0) END),0)
		FROM errs e LEFT JOIN log_err_cells c ON c.slot=e.slot AND c.stage=e.stage`,
		&r.ErrDims, &r.ErrMismatchDims, &r.ErrDeficitRows); err != nil {
		return nil, err
	}
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM log_err_cells c WHERE c.slot BETWEEN ? AND ? AND NOT EXISTS (
			SELECT 1 FROM logs l WHERE l.log_source != 'rejected' AND l.error_stage != '' AND l.id <= ?
			AND l.time/600000 = c.slot AND l.error_stage = c.stage)`, lo, hi, wm).Scan(&r.ErrOrphans); err != nil {
		return nil, err
	}
	return r, nil
}

// cellsBreachedSlots 列出窗口内「不可证完整」的占用 slot（格底低于
// 全表最早存活行——前缀删除已吃掉该 slot 的一部分）。返回空集即
// 窗口内每个占用 slot 的源行都完好。
func cellsBreachedSlots(ctx context.Context, q dbtx, wm, lo, hi int64) ([]int64, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT time/600000 FROM logs WHERE `+cellSrcPred+`
		AND time/600000*600000 < (SELECT MIN(time) FROM logs)`, wm, lo, hi)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// cellsSurplusSlots 列出窗口内格子计数超过存活源行的 slot（两张
// rollup 表各查一遍）。盈余意味着格子声称的历史比源行多——REPLACE
// 会把它写成更小的数，属数据损毁方向，必须中止人工核对。
func cellsSurplusSlots(ctx context.Context, q dbtx, wm, lo, hi int64) ([]int64, error) {
	var out []int64
	for _, query := range []string{
		`SELECT slot FROM (
			SELECT c.slot AS slot, SUM(c.req) AS cellreq,
				(SELECT COUNT(*) FROM logs l WHERE l.time/600000 = c.slot
					AND l.log_source != 'rejected' AND l.id <= ?) AS srcreq
			FROM log_cells c WHERE c.slot BETWEEN ? AND ? GROUP BY c.slot)
		WHERE cellreq > srcreq`,
		`SELECT slot FROM (
			SELECT c.slot AS slot, SUM(c.req) AS cellreq,
				(SELECT COUNT(*) FROM logs l WHERE l.time/600000 = c.slot
					AND l.log_source != 'rejected' AND l.error_stage != '' AND l.id <= ?) AS srcreq
			FROM log_err_cells c WHERE c.slot BETWEEN ? AND ? GROUP BY c.slot)
		WHERE cellreq > srcreq`,
	} {
		rows, err := q.QueryContext(ctx, query, wm, lo, hi)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var s int64
			if err := rows.Scan(&s); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out = append(out, s)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// 调用方已保证 Account==""（rollup 无 account 维度，该 scope 组合
// 走原始行回退）；rejected 剔除是表内建语义，不需要谓词。
func (sc LogScope) cellWhere() (string, []any) {
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
		b.WriteString(` AND emodel = ?`)
		args = append(args, sc.Model)
	}
	if sc.ModelLike != "" {
		b.WriteString(` AND INSTR(emodel, ?) > 0`)
		args = append(args, sc.ModelLike)
	}
	return b.String(), args
}

// ── 读侧：格子 + 原始行补尾的 UNION 口径 ──────────────────────────
//
// 聚合查询的答案 = 「水位内行的格子」+「水位外行与边带行的原始
// SUM」。语句内水位子查询让每条 SELECT 自成一致快照：格子提交与
// 水位推进是同事务，快照要么都看见要么都看不见，不需要额外读事务
// 就能把 UNION 两侧拼成无重无漏的划分。

// cellsWatermarkSQL 是嵌进原始行谓词的语句内水位标量子查询。
const cellsWatermarkSQL = `COALESCE((SELECT CAST(value AS INTEGER) FROM runtime_state WHERE "key"='` + cellsWatermarkKey + `'),0)`

// cellRowExprs 把 rollup 列名映射回 logs 行级表达式——UNION 补尾
// 段的 SELECT 投影与 cellsInsertSQL 的 SELECT 同源。min_time/
// last_key 两个非可加列也登记在内。
var cellRowExprs = func() map[string]string {
	m := make(map[string]string, len(cellMetrics)+2)
	for _, c := range cellMetrics {
		m[c.name] = c.expr
	}
	m["min_time"] = `time`
	m["last_key"] = `printf('%020d', id)||'|'||started_at`
	return m
}()

// cellRowList 把逗号分隔的 rollup 列清单编译成行级表达式清单
// （UNION 原始行侧的 SELECT 投影，顺序与列清单一致）。
func cellRowList(cols string) string {
	parts := strings.Split(cols, ",")
	for i, p := range parts {
		parts[i] = cellRowExprs[strings.TrimSpace(p)]
	}
	return strings.Join(parts, ", ")
}

// cellTailPred 返回补尾段追加在窗口谓词后的片段（含前导 " AND "）：
// 「该走原始行」= 未记账行（id > 语句内水位）∪ 两端不满一格的边带
// （time < slotLo 格底 或 time >= slotHi 格顶）。slotLo==0 时无边带；
// slotHi==math.MaxInt64 表示无上界。调用方负责窗口谓词本身
// （time>=?/minute_bucket>=? 等）与 log_source/scope 条件。
func cellTailPred(slotLo, slotHi int64) (string, []any) {
	var b strings.Builder
	var args []any
	b.WriteString(` AND (id > ` + cellsWatermarkSQL)
	if slotLo > 0 {
		b.WriteString(` OR time < ?`)
		args = append(args, slotLo*600000)
	}
	// slotHi*600000 换算毫秒前先钳位：上界远超 int64 毫秒域时该
	// 项恒假，直接省略防乘法溢出成负数而恒真。
	if slotHi < math.MaxInt64/600000 {
		b.WriteString(` OR time >= ?`)
		args = append(args, slotHi*600000)
	}
	b.WriteString(`)`)
	return b.String(), args
}

// cellSlotLo 是毫秒窗下界 lo 对应的「整格在窗内」最小 slot：
// lo 恰在格界时该格整格入选，否则上取到下一格。
func cellSlotLo(loMS int64) int64 {
	if loMS <= 0 {
		return 0
	}
	return (loMS + 599999) / 600000
}
