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

// cellMetrics 依序登记 log_cells 的 38 个可加指标列：DDL、回填
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

// cellsInsertSQL 生成「logs → log_cells」的聚合回填语句，where 是追加
// 在 rejected 谓词后的行范围片段（ReconcileCells 补漏用 "AND id > ?"）。
// 同一个 GROUP BY 表达式服务迁移回填与运行期补漏——口径只有一份。
func cellsInsertSQL(where string) string {
	exprs := make([]string, 0, len(cellMetrics))
	for _, m := range cellMetrics {
		exprs = append(exprs, "SUM("+m.expr+")")
	}
	return `INSERT INTO log_cells(slot, day, api, emodel, key_hash, ` + cellMetricNames + `, min_time, last_key)
		SELECT time/600000, ` + logDayExpr + `, api, ` + logEModelExpr + `, key_hash, ` +
		strings.Join(exprs, ", ") + `, MIN(time), MAX(printf('%020d', id)||'|'||started_at)
		FROM logs WHERE log_source != 'rejected' ` + where +
		` GROUP BY 1, 2, 3, 4, 5` + cellsConflict
}

var (
	cellsGapSQL = cellsInsertSQL("AND id > ?")
	// cellsUpsertSQL 是写路径单行/批量共用的 VALUES 形态 upsert。
	cellsUpsertSQL = `INSERT INTO log_cells(slot, day, api, emodel, key_hash, ` + cellMetricNames +
		`, min_time, last_key) VALUES(` + placeholders(5+len(cellMetrics)+2) + `)` + cellsConflict
)

var (
	errCellsGapSQL = `INSERT INTO log_err_cells(slot, stage, req)
		SELECT time/600000, error_stage, COUNT(*) FROM logs
		WHERE log_source != 'rejected' AND error_stage != '' AND id > ?
		GROUP BY 1, 2` + errCellsConflict
	errCellsUpsertSQL = `INSERT INTO log_err_cells(slot, stage, req) VALUES(?,?,?)` + errCellsConflict
)

// cellsWatermarkKey 是 rollup 覆盖水位线在 runtime_state 的键。
const cellsWatermarkKey = "log_cells_covered_id"

// cellsWatermark 读当前覆盖水位（缺席按 0——全表未记账）。
func cellsWatermark(tx *sql.Tx) (int64, error) {
	var v string
	switch err := tx.QueryRow(`SELECT value FROM runtime_state WHERE "key" = ?`, cellsWatermarkKey).Scan(&v); {
	case err == sql.ErrNoRows:
		return 0, nil
	case err != nil:
		return 0, err
	}
	return strconv.ParseInt(v, 10, 64)
}

// setCellsWatermark 在事务内推进水位；单写连接串行化下 id 单调，
// 直接写新值即等价 MAX。
func setCellsWatermark(tx *sql.Tx, id int64) error {
	_, err := tx.Exec(`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
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

// cellVals 是单 cell 的批内累加器：38 个可加列与 cellMetrics 同序
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
	}
}

// logCellVals 计算一条 logs 行的 38 列贡献——逐谓词镜像 cellMetrics
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
	return v
}

// addCellContrib 把一条已入库日志行累加进批内聚合器；id 是该行实际
// 分配到的自增主键（INSERT OR IGNORE 跳过的重复行不进这里——首个
// 落库者已在它自己的事务里记过账）。source=='rejected' 的行被剔除。
func addCellContrib(cells map[cellDim]*cellVals, errs map[errCellDim]int64, e *LogRow, id int64) {
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
	dim := cellDim{slot: slot, day: e.StartedAt.Local().Format("2006-01-02"),
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
	if ms < acc.minTime {
		acc.minTime = ms
	}
	lastKey := fmt.Sprintf("%020d|%s", id, e.StartedAt.Format(time.RFC3339Nano))
	if lastKey > acc.lastKey {
		acc.lastKey = lastKey
	}
	if e.ErrorStage != "" {
		errs[errCellDim{slot: slot, stage: e.ErrorStage}]++
	}
}

// upsertCells 把批内聚合器逐组 upsert 进 rollup 表（同事务）。调用方
// 保证地图非 nil；空地图是廉价空转。
func upsertCells(ctx context.Context, tx *sql.Tx, cells map[cellDim]*cellVals, errs map[errCellDim]int64) error {
	for dim, acc := range cells {
		args := append([]any{dim.slot, dim.day, dim.api, dim.emodel, dim.keyHash}, acc.args()...)
		args = append(args, acc.minTime, acc.lastKey)
		if _, err := tx.ExecContext(ctx, cellsUpsertSQL, args...); err != nil {
			return err
		}
	}
	for dim, n := range errs {
		if _, err := tx.ExecContext(ctx, errCellsUpsertSQL, dim.slot, dim.stage, n); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileCells 把水位线之后落库的非 rejected 行补记进 rollup——
// 绕过双写的写入者有无 cells 码的旧二进制、外部工具与 importIndex：
// Open 收尾固定调它闭合这类缝隙（9-19 实证 7,634 行险些永隐），
// ImportLegacy 收尾再补一次导入期写入；常规调用是 id>水位 的空扫。
func (s *Store) ReconcileCells(ctx context.Context) error {
	// 无未记账行时纯读快退：水位随每行双写推进，缝隙只来自绕过
	// 双写的写入者，常规启动这里是空扫——但即便是空扫，写事务在
	// 共享库（交接期与在役实例并发）上也会被在役写流饿死到
	// SQLITE_BUSY，让本可无锁的路径死在启动期。
	var uncovered int
	if err := s.ro.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM logs WHERE id > `+cellsWatermarkSQL+`)`).Scan(&uncovered); err != nil {
		return err
	}
	if uncovered == 0 {
		return nil
	}
	tx, done, err := s.writeTx(ctx, "ReconcileCells")
	if err != nil {
		return err
	}
	defer done()
	wm, err := cellsWatermark(tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, cellsGapSQL, wm); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, errCellsGapSQL, wm); err != nil {
		return err
	}
	var maxID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM logs`).Scan(&maxID); err != nil {
		return err
	}
	if maxID < wm {
		maxID = wm
	}
	if err := setCellsWatermark(tx, maxID); err != nil {
		return err
	}
	return tx.Commit()
}

// cellWhere 把 LogScope 编译成 rollup 列谓词（含前导 " AND "）。
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
