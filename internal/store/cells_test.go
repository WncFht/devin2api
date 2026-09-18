// 本文件验证 log_cells rollup 的三层不变量：
//  1. Go 镜像 logCellVals 与 cellMetrics 的 SQL 行级表达式逐行等价
//     （登记表即契约——两侧漂移会让双写与回填产出不同格子）。
//  2. 双写路径（InsertLog/WriteDebugBatch）在同事务把贡献记进格子
//     并推进水位；rejected 行被有意剔除但水位照样覆盖。
//  3. 读侧「格子 UNION 原始行补尾」与全原始行聚合逐值相等——
//     含 importIndex 式绕过双写的行（水位外由补尾段兜住）。
package store

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cellSeedRows 覆盖 cellMetrics 全部谓词分支：完成流式（可 decode）、
// fbt≥duration、fbt 缺席、499 两态、429、rate_limited 非 429、上游/
// 客户端责任归因、rejected 剔除、非流式、零时长、decode 速率上限、
// slack 正贡献/钳零/fc=0 剔除。
func cellSeedRows(base time.Time) []*LogRow {
	fbt := int64(1200)
	small := int64(10)
	fc := int64(2000)
	fcGone := int64(100)
	fcLate := int64(1500)
	fcZero := int64(0)
	return []*LogRow{
		{StartedAt: base, DurationMS: 5000, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			Stream: true, FirstUpstreamMS: &fbt, FirstClientMS: &fc, API: "anthropic", Model: "m-a", RequestedModel: "m-a", KeyHash: "kh1",
			InputTokens: 10, OutputTokens: 100, CacheReadTokens: 3, CacheWriteTokens: 2, ReasoningTokens: 4, TotalTokens: 119, CreditCost: 7},
		{StartedAt: base.Add(time.Minute), DurationMS: 1000, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			Stream: true, FirstUpstreamMS: &fbt, API: "anthropic", Model: "m-a", KeyHash: "kh1", OutputTokens: 5},
		{StartedAt: base.Add(2 * time.Minute), DurationMS: 2000, Method: "POST", Path: "/v1/chat", StatusCode: 200, Result: "completed",
			Stream: true, API: "openai-chat", Model: "m-b", KeyHash: "kh2", OutputTokens: 50},
		{StartedAt: base.Add(3 * time.Minute), DurationMS: 300, Method: "POST", Path: "/v1/messages", StatusCode: 499, Result: "disconnected",
			FirstClientMS: &fcGone, API: "anthropic", Model: "m-a", KeyHash: "kh1", OutputTokens: 3},
		{StartedAt: base.Add(4 * time.Minute), DurationMS: 400, Method: "POST", Path: "/v1/messages", StatusCode: 499, Result: "aborted",
			API: "anthropic", Model: "m-a", KeyHash: "kh1"},
		{StartedAt: base.Add(5 * time.Minute), DurationMS: 20, Method: "POST", Path: "/v1/messages", StatusCode: 429, Result: "failed",
			API: "anthropic", Model: "m-a", KeyHash: "kh1", ErrorStage: "devin_connect", ErrorMessage: "rate limited"},
		{StartedAt: base.Add(6 * time.Minute), DurationMS: 800, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "failed",
			RateLimited: true, API: "anthropic", Model: "m-a", KeyHash: "kh1", ErrorStage: "provider_stream", ErrorMessage: "limited mid-stream"},
		{StartedAt: base.Add(7 * time.Minute), DurationMS: 100, Method: "POST", Path: "/v1/messages", StatusCode: 500, Result: "failed",
			API: "anthropic", Model: "m-b", KeyHash: "kh2", ErrorStage: "devin_transport", ErrorMessage: "eof"},
		{StartedAt: base.Add(8 * time.Minute), DurationMS: 50, Method: "POST", Path: "/v1/messages", StatusCode: 400, Result: "failed",
			API: "anthropic", Model: "m-a", KeyHash: "kh1", ErrorStage: "http_decode", ErrorMessage: "bad body"},
		{StartedAt: base.Add(9 * time.Minute), DurationMS: 60, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "failed",
			API: "anthropic", Model: "m-a", KeyHash: "kh1", ErrorStage: "provider_stream", ErrorMessage: "upstream err"},
		{StartedAt: base.Add(10 * time.Minute), DurationMS: 0, Method: "POST", Path: "/v1/messages", StatusCode: 429, Result: "rejected",
			LogSource: "rejected", API: "anthropic", Model: "m-a", KeyHash: "kh1", ErrorStage: "pre_pipeline"},
		{StartedAt: base.Add(11 * time.Minute), DurationMS: 900, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			FirstClientMS: &fcLate, API: "anthropic", Model: "m-b", KeyHash: "kh2", OutputTokens: 20, TotalTokens: 20},
		{StartedAt: base.Add(12 * time.Minute), DurationMS: 0, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			FirstClientMS: &fcZero, API: "anthropic", KeyHash: "kh1"},
		{StartedAt: base.Add(13 * time.Minute), DurationMS: 1100, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			Stream: true, FirstUpstreamMS: &small, API: "anthropic", Model: "m-b", KeyHash: "kh2", OutputTokens: 1000},
		// 与首行同维度：验证同格累加而非按行并集。
		{StartedAt: base.Add(11 * time.Minute), DurationMS: 100, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			Stream: true, FirstUpstreamMS: &small, API: "anthropic", Model: "m-a", KeyHash: "kh1", OutputTokens: 8},
	}
}

// cellExprListJoined 把 cellMetrics 的行级表达式按模板（"SUM(%s)"
// 或 "%s"）编译成逗号清单——一致性探测与真值查询的 SELECT 投影。
func cellExprListJoined(tmpl string) string {
	exprs := make([]string, len(cellMetrics))
	for i, m := range cellMetrics {
		exprs[i] = fmt.Sprintf(tmpl, m.expr)
	}
	return strings.Join(exprs, ", ")
}

// cellKey 是真值/存储两侧通用的格子键形（map key 需可比较）。
type cellKey struct {
	slot    int64
	day     string
	api     string
	emodel  string
	keyHash string
}

// cellGroupTruth 是一格的聚合值：40 个指标列 + min_time + last_key。
type cellGroupTruth struct {
	vals    []int64
	minTime int64
	lastKey string
}

// queryCellTruth 按 cellsInsertSQL 同一 GROUP BY 口径全量重算
// logs 的格子真值；where 是追加的行范围片段（"" 或 "AND id > ?"）。
func queryCellTruth(t *testing.T, s *Store, ctx context.Context, where string, args ...any) map[cellKey]cellGroupTruth {
	t.Helper()
	rows, err := s.db.QueryContext(ctx,
		`SELECT time/600000, `+logDayExpr+`, api, `+logEModelExpr+`, key_hash, `+
			cellExprListJoined("SUM(%s)")+`, MIN(time), MAX(printf('%020d', id)||'|'||started_at)
		FROM logs WHERE log_source != 'rejected' `+where+` GROUP BY 1,2,3,4,5`, args...)
	if err != nil {
		t.Fatalf("truth query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[cellKey]cellGroupTruth{}
	for rows.Next() {
		var k cellKey
		g := cellGroupTruth{vals: make([]int64, len(cellMetrics))}
		dests := []any{&k.slot, &k.day, &k.api, &k.emodel, &k.keyHash}
		for j := range g.vals {
			dests = append(dests, &g.vals[j])
		}
		dests = append(dests, &g.minTime, &g.lastKey)
		if err := rows.Scan(dests...); err != nil {
			t.Fatalf("truth scan: %v", err)
		}
		out[k] = g
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("truth rows: %v", err)
	}
	return out
}

// queryStoredCells 读回 log_cells 全表（与 queryCellTruth 同键形）。
func queryStoredCells(t *testing.T, s *Store, ctx context.Context) map[cellKey]cellGroupTruth {
	t.Helper()
	rows, err := s.db.QueryContext(ctx,
		`SELECT slot, day, api, emodel, key_hash, `+cellMetricNames+`, min_time, last_key FROM log_cells`)
	if err != nil {
		t.Fatalf("cells query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[cellKey]cellGroupTruth{}
	for rows.Next() {
		var k cellKey
		g := cellGroupTruth{vals: make([]int64, len(cellMetrics))}
		dests := []any{&k.slot, &k.day, &k.api, &k.emodel, &k.keyHash}
		for j := range g.vals {
			dests = append(dests, &g.vals[j])
		}
		dests = append(dests, &g.minTime, &g.lastKey)
		if err := rows.Scan(dests...); err != nil {
			t.Fatalf("cells scan: %v", err)
		}
		out[k] = g
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("cells rows: %v", err)
	}
	return out
}

// cellsWatermarkOf 读当前覆盖水位（缺席按 0）。
func cellsWatermarkOf(t *testing.T, s *Store, ctx context.Context) int64 {
	t.Helper()
	var wm int64
	err := s.db.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM runtime_state WHERE "key"=?`, cellsWatermarkKey).Scan(&wm)
	if err != nil {
		return 0
	}
	return wm
}

// unionCount 是读侧 UNION 口径的请求数（slot>=0 全窗），minute_bucket
// 下界传 0 即「全部行」——与 COUNT(logs 非 rejected) 应恒等。
func unionCount(t *testing.T, s *Store, ctx context.Context) int64 {
	t.Helper()
	tail, tailArgs := cellTailPred(0, int64(1)<<62)
	var n int64
	if err := s.ro.QueryRowContext(ctx, `SELECT COALESCE(SUM(c),0) FROM (
		SELECT req AS c FROM log_cells WHERE slot >= 0
		UNION ALL SELECT 1 FROM logs WHERE minute_bucket >= ?`+tail+` AND log_source != 'rejected')`,
		append([]any{int64(0)}, tailArgs...)...).Scan(&n); err != nil {
		t.Fatalf("union count: %v", err)
	}
	return n
}

func TestCellsConsistency(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	rows := cellSeedRows(base)

	// 双写入库 + 逐行 Go↔SQL 表达式等价。
	exprList := cellExprListJoined("%s")
	for i, r := range rows {
		r.Dir = fmt.Sprintf("d-%02d", i)
		id, err := s.InsertLog(ctx, r)
		if err != nil {
			t.Fatalf("InsertLog %d: %v", i, err)
		}
		got := make([]int64, len(cellMetrics))
		dests := make([]any, len(got))
		for j := range got {
			dests[j] = &got[j]
		}
		if err := s.db.QueryRowContext(ctx, `SELECT `+exprList+` FROM logs WHERE id=?`, id).Scan(dests...); err != nil {
			t.Fatalf("row %d expr scan: %v", i, err)
		}
		want := logCellVals(r)
		wantArgs := want.args()
		for j, m := range cellMetrics {
			if got[j] != wantArgs[j].(int64) {
				t.Fatalf("row %d metric %s: sql=%d go=%v", i, m.name, got[j], wantArgs[j])
			}
		}
	}

	// 双写产物与「logs 全量重算」逐格相等；rejected 行有 logs 行但
	// 被格子键集剔除。
	truth := queryCellTruth(t, s, ctx, "")
	stored := queryStoredCells(t, s, ctx)
	if !reflect.DeepEqual(truth, stored) {
		t.Fatalf("cells diverged:\n truth=%+v\n store=%+v", truth, stored)
	}
	var rejErr int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(req),0) FROM log_err_cells WHERE stage='pre_pipeline'`).Scan(&rejErr); err != nil {
		t.Fatal(err)
	}
	if rejErr != 0 {
		t.Fatalf("rejected row leaked into log_err_cells: %d", rejErr)
	}

	// 水位覆盖到 MAX(id)（rejected 行同样被水位覆盖——有意剔除 ≠
	// 未记账，它不会再被任何路径重算）。
	var maxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM logs`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if wm := cellsWatermarkOf(t, s, ctx); wm != maxID {
		t.Fatalf("watermark %d != MAX(id) %d", wm, maxID)
	}

	// err 迷你表与真值逐格比对。
	errTruth := map[errCellDim]int64{}
	errRows, err := s.db.QueryContext(ctx,
		`SELECT time/600000, error_stage, COUNT(*) FROM logs
		WHERE log_source != 'rejected' AND error_stage != '' GROUP BY 1,2`)
	if err != nil {
		t.Fatal(err)
	}
	for errRows.Next() {
		var d errCellDim
		var n int64
		if err := errRows.Scan(&d.slot, &d.stage, &n); err != nil {
			t.Fatal(err)
		}
		errTruth[d] = n
	}
	if err := errRows.Close(); err != nil {
		t.Fatal(err)
	}
	errStored := map[errCellDim]int64{}
	storedRows, err := s.db.QueryContext(ctx, `SELECT slot, stage, req FROM log_err_cells`)
	if err != nil {
		t.Fatal(err)
	}
	for storedRows.Next() {
		var d errCellDim
		var n int64
		if err := storedRows.Scan(&d.slot, &d.stage, &n); err != nil {
			t.Fatal(err)
		}
		errStored[d] = n
	}
	if err := storedRows.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(errTruth, errStored) {
		t.Fatalf("err cells diverged:\n truth=%+v\n store=%+v", errTruth, errStored)
	}

	// 读侧 UNION 与原始行总数一致。
	var rawReq int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE log_source != 'rejected'`).Scan(&rawReq); err != nil {
		t.Fatal(err)
	}
	if n := unionCount(t, s, ctx); n != rawReq {
		t.Fatalf("union req %d != raw %d", n, rawReq)
	}
}

// TestReconcileCellsCoversBypass 模拟 importIndex 的绕过双写：直接
// SQL 插行后水位不动，读侧 UNION 靠补尾段仍给真值；ReconcileCells
// 之后水位推进、格子补齐，UNION 结果不变（无重无漏）。
func TestReconcileCellsCoversBypass(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	row := cellSeedRows(base)[0]
	row.Dir = "bypass-1"
	if _, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(row)...); err != nil {
		t.Fatalf("bypass insert: %v", err)
	}
	if n := unionCount(t, s, ctx); n != 1 {
		t.Fatalf("pre-reconcile union req = %d, want 1 (tail-covered)", n)
	}
	if err := s.ReconcileCells(ctx); err != nil {
		t.Fatalf("ReconcileCells: %v", err)
	}
	if n := unionCount(t, s, ctx); n != 1 {
		t.Fatalf("post-reconcile union req = %d, want 1 (no double count)", n)
	}
	if wm := cellsWatermarkOf(t, s, ctx); wm != 1 {
		t.Fatalf("watermark = %d, want 1", wm)
	}
	stored := queryStoredCells(t, s, ctx)
	if len(stored) != 1 {
		t.Fatalf("stored cells = %d, want 1", len(stored))
	}
	for _, g := range stored {
		if g.vals[0] != 1 {
			t.Fatalf("cell req = %d, want 1", g.vals[0])
		}
	}
}

// TestOpenReconcilesBypassRows 钉死启动自愈：上个进程留下的未记账
// 行（旧二进制/外部工具等绕过双写的写入）在下一次 Open 被补记——
// 否则缝隙会被后续双写推进的水位碾过，对 UNION 读永久隐形。
func TestOpenReconcilesBypassRows(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	row := cellSeedRows(base)[0]
	row.Dir = "pre-open-bypass"
	if _, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(row)...); err != nil {
		t.Fatalf("bypass insert: %v", err)
	}
	path := s.path
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()

	stored := queryStoredCells(t, s2, ctx)
	if len(stored) != 1 {
		t.Fatalf("stored cells = %d, want 1", len(stored))
	}
	for _, g := range stored {
		if g.vals[0] != 1 {
			t.Fatalf("cell req = %d, want 1", g.vals[0])
		}
	}
	if wm := cellsWatermarkOf(t, s2, ctx); wm != 1 {
		t.Fatalf("watermark = %d, want 1", wm)
	}
	if n := unionCount(t, s2, ctx); n != 1 {
		t.Fatalf("union req = %d, want 1", n)
	}
}

// TestWriteDebugBatchRollup 验证批量日志行的双写：同事务 upsert
// 格子 + 推进水位；OR IGNORE 跳过的重复行不重复记账。
func TestWriteDebugBatchRollup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	rows := cellSeedRows(base)[:4]
	for i, r := range rows {
		r.Dir = fmt.Sprintf("b-%02d", i)
	}
	if err := s.WriteDebugBatch(ctx, DebugBatch{LogRows: rows}); err != nil {
		t.Fatalf("WriteDebugBatch: %v", err)
	}
	// 重复行再写：OR IGNORE 跳过，格子不得翻倍。
	if err := s.WriteDebugBatch(ctx, DebugBatch{LogRows: []*LogRow{rows[0]}}); err != nil {
		t.Fatalf("dup batch: %v", err)
	}
	truth := queryCellTruth(t, s, ctx, "")
	stored := queryStoredCells(t, s, ctx)
	if !reflect.DeepEqual(truth, stored) {
		t.Fatalf("cells diverged:\n truth=%+v\n store=%+v", truth, stored)
	}
	var maxID int64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM logs`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if wm := cellsWatermarkOf(t, s, ctx); wm != maxID {
		t.Fatalf("watermark %d != MAX(id) %d", wm, maxID)
	}
}

// forceCellsWatermark 把覆盖水位人工推到给定 id——测试里用来复刻
// 「行已声称记账但格子缺失」的历史洞形态（prod 2026-09-19 实证：
// cells 化二进制迁移回填推满水位后，旧二进制继续写的新行没有双写）。
func forceCellsWatermark(t *testing.T, s *Store, ctx context.Context, id int64) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,0)`,
		cellsWatermarkKey, strconv.FormatInt(id, 10)); err != nil {
		t.Fatalf("force watermark: %v", err)
	}
}

// TestBackfillCellsRepairsGap 复刻水位内缺口：首行走双写（格子在），
// 其余绕过双写直插后水位被人工推满——全部行 id ≤ 水位但多数格子
// 缺失。BackfillCells 干跑只出报告；apply 后格子与全量真值逐格相等，
// 水位不变（重算域本来就在水位内），重跑幂等。
func TestBackfillCellsRepairsGap(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	rows := cellSeedRows(base)

	rows[0].Dir = "covered-0"
	if _, err := s.InsertLog(ctx, rows[0]); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	var maxID int64
	for i, r := range rows[1:] {
		r.Dir = fmt.Sprintf("gap-%02d", i)
		res, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(r)...)
		if err != nil {
			t.Fatalf("bypass insert %d: %v", i, err)
		}
		if maxID, err = res.LastInsertId(); err != nil {
			t.Fatal(err)
		}
	}
	forceCellsWatermark(t, s, ctx, maxID)

	var lo, hi int64
	for _, r := range rows {
		slot := r.StartedAt.UnixMilli() / 600000
		if lo == 0 || slot < lo {
			lo = slot
		}
		if slot > hi {
			hi = slot
		}
	}

	// 干跑：报告缺口，不写库。
	dry, err := s.BackfillCells(ctx, lo, hi, false)
	if err != nil {
		t.Fatalf("dry backfill: %v", err)
	}
	if dry.Applied {
		t.Fatal("dry run reported applied")
	}
	if dry.Pre.Watermark != maxID {
		t.Fatalf("watermark = %d, want %d", dry.Pre.Watermark, maxID)
	}
	if dry.Pre.Rows != 14 || dry.Pre.Dims != 6 || dry.Pre.MismatchDims != 6 || dry.Pre.DeficitRows != 13 {
		t.Fatalf("pre audit: %+v", dry.Pre)
	}
	if dry.Pre.ErrDims != 4 || dry.Pre.ErrMismatchDims != 4 || dry.Pre.ErrDeficitRows != 5 {
		t.Fatalf("pre err audit: %+v", dry.Pre)
	}
	if dry.Pre.SurplusDims != 0 || dry.Pre.OrphanCells != 0 || dry.Pre.TailRows != 0 {
		t.Fatalf("unexpected surplus/orphan/tail: %+v", dry.Pre)
	}
	if stored := queryStoredCells(t, s, ctx); len(stored) != 1 {
		t.Fatalf("dry run wrote cells: %d", len(stored))
	}

	// apply：格子与「双写产物」逐格相等（全量真值对比）。
	rep, err := s.BackfillCells(ctx, lo, hi, true)
	if err != nil {
		t.Fatalf("apply backfill: %v", err)
	}
	if !rep.Applied {
		t.Fatal("apply run reported not applied")
	}
	if rep.Post.MismatchDims != 0 || rep.Post.DeficitRows != 0 || rep.Post.ErrMismatchDims != 0 || rep.Post.OrphanCells != 0 {
		t.Fatalf("post audit not clean: %+v", rep.Post)
	}
	truth := queryCellTruth(t, s, ctx, "")
	stored := queryStoredCells(t, s, ctx)
	if !reflect.DeepEqual(truth, stored) {
		t.Fatalf("cells diverged:\n truth=%+v\n store=%+v", truth, stored)
	}
	if wm := cellsWatermarkOf(t, s, ctx); wm != maxID {
		t.Fatalf("watermark moved: %d != %d", wm, maxID)
	}
	var rawReq int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE log_source != 'rejected'`).Scan(&rawReq); err != nil {
		t.Fatal(err)
	}
	if n := unionCount(t, s, ctx); n != rawReq {
		t.Fatalf("union req %d != raw %d", n, rawReq)
	}

	// 幂等：重跑同一窗口不再失配，格子不变。
	rep2, err := s.BackfillCells(ctx, lo, hi, true)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if rep2.Pre.MismatchDims != 0 || rep2.Pre.DeficitRows != 0 {
		t.Fatalf("second run still sees deficit: %+v", rep2.Pre)
	}
	if stored2 := queryStoredCells(t, s, ctx); !reflect.DeepEqual(stored, stored2) {
		t.Fatalf("second run changed cells")
	}
}

// TestBackfillCellsLeavesTailRows 钉死重算域边界：窗口内 id > 水位
// 的行属于 ReconcileCells 的职责域，BackfillCells 不得把它们写进
// 格子（否则补漏 upsert 时会双计）。
func TestBackfillCellsLeavesTailRows(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)
	rows := cellSeedRows(base)

	// 第一批绕过双写 + 人工推满水位 = 水位内缺口。
	var maxID int64
	for i, r := range rows[:4] {
		r.Dir = fmt.Sprintf("gap-%d", i)
		res, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(r)...)
		if err != nil {
			t.Fatal(err)
		}
		if maxID, err = res.LastInsertId(); err != nil {
			t.Fatal(err)
		}
	}
	forceCellsWatermark(t, s, ctx, maxID)

	// 第二批同样绕过双写，但 id 在水位之上（如导入器刚落下的行，
	// 尚等下一次 ReconcileCells）——与第一批同 slot 同维度。
	var tailMax int64
	for i, r := range rows[4:6] {
		r.Dir = fmt.Sprintf("tail-%d", i)
		res, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(r)...)
		if err != nil {
			t.Fatal(err)
		}
		if tailMax, err = res.LastInsertId(); err != nil {
			t.Fatal(err)
		}
	}

	lo := rows[0].StartedAt.UnixMilli() / 600000
	hi := rows[5].StartedAt.UnixMilli() / 600000
	rep, err := s.BackfillCells(ctx, lo, hi, true)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if rep.Pre.TailRows != 2 {
		t.Fatalf("tail rows = %d, want 2", rep.Pre.TailRows)
	}
	// 格子只含水位内 4 行的贡献；水位外 2 行仍由补尾段覆盖——
	// UNION 口径在修复前后都不重不漏。
	wmTruth := queryCellTruth(t, s, ctx, "AND id <= ?", maxID)
	stored := queryStoredCells(t, s, ctx)
	if !reflect.DeepEqual(wmTruth, stored) {
		t.Fatalf("cells include tail rows:\n wmTruth=%+v\n store=%+v", wmTruth, stored)
	}
	var rawReq int64
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE log_source != 'rejected'`).Scan(&rawReq); err != nil {
		t.Fatal(err)
	}
	if n := unionCount(t, s, ctx); n != rawReq {
		t.Fatalf("union req %d != raw %d", n, rawReq)
	}
	// 补漏收尾：尾行被增量记进格子，水位推进，UNION 仍一致。
	if err := s.ReconcileCells(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if wm := cellsWatermarkOf(t, s, ctx); wm != tailMax {
		t.Fatalf("watermark = %d, want %d", wm, tailMax)
	}
	fullTruth := queryCellTruth(t, s, ctx, "")
	if stored := queryStoredCells(t, s, ctx); !reflect.DeepEqual(fullTruth, stored) {
		t.Fatalf("cells diverged after reconcile:\n truth=%+v\n store=%+v", fullTruth, stored)
	}
	if n := unionCount(t, s, ctx); n != rawReq {
		t.Fatalf("union req %d != raw %d after reconcile", n, rawReq)
	}
}

// TestBackfillCellsAbortsOnBreachedSlot 前缀删除已越过窗口下缘时
// （占用 slot 格底低于全表最早存活行）必须整体中止——REPLACE 会把
// 被部分删除的格子写小，毁掉格子里尚存的历史。
func TestBackfillCellsAbortsOnBreachedSlot(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	// 同 slot 两行：删掉较早的一行使 MIN(time) 越过该 slot 格底——
	// 这正是 retention 前缀删除扫到窗口中间时的形态。
	var firstID, maxID int64
	for i, off := range []time.Duration{0, time.Minute} {
		r := &LogRow{StartedAt: base.Add(off), DurationMS: 100, Method: "POST", Path: "/v1/messages",
			StatusCode: 200, Result: "completed", API: "anthropic", Model: "m-a", KeyHash: "kh1",
			Dir: fmt.Sprintf("b-%d", i)}
		res, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(r)...)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if i == 0 {
			firstID = id
		}
		maxID = id
	}
	forceCellsWatermark(t, s, ctx, maxID)
	if _, err := s.db.ExecContext(ctx, `DELETE FROM logs WHERE id = ?`, firstID); err != nil {
		t.Fatal(err)
	}

	slot := base.UnixMilli() / 600000
	if _, err := s.BackfillCells(ctx, slot, slot, true); err == nil ||
		!strings.Contains(err.Error(), "not intact") {
		t.Fatalf("expected intactness abort, got %v", err)
	}
	// 干跑同样被拦（守卫先于审计；纯诊断走 CellsAudit）。
	if _, err := s.BackfillCells(ctx, slot, slot, false); err == nil {
		t.Fatal("expected dry-run abort too")
	}
	if _, err := s.CellsAudit(ctx, slot, slot); err != nil {
		t.Fatalf("CellsAudit should still report: %v", err)
	}
}

// TestBackfillCellsAbortsOnSurplus 格子计数超过存活源行（非前缀删除
// 或外部改写的痕迹）时中止，不把盈余改写成更小的数。
func TestBackfillCellsAbortsOnSurplus(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 17, 10, 0, 0, 0, time.UTC)

	r := &LogRow{StartedAt: base, DurationMS: 100, Method: "POST", Path: "/v1/messages",
		StatusCode: 200, Result: "completed", API: "anthropic", Model: "m-a", KeyHash: "kh1", Dir: "s-0"}
	res, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(r)...)
	if err != nil {
		t.Fatal(err)
	}
	maxID, _ := res.LastInsertId()
	forceCellsWatermark(t, s, ctx, maxID)

	// 手插一个 req=999 的格子制造盈余维度。
	vals := make([]any, len(cellMetrics))
	for i := range vals {
		vals[i] = int64(0)
	}
	vals[0] = int64(999)
	args := append([]any{base.UnixMilli() / 600000, "2026-09-17", "anthropic", "m-a", "kh1"}, vals...)
	args = append(args, base.UnixMilli(), "00000000000000000001|2026-09-17T10:00:00Z")
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO log_cells(slot, day, api, emodel, key_hash, `+cellMetricNames+`, min_time, last_key)
		VALUES(`+placeholders(5+len(cellMetrics)+2)+`)`, args...); err != nil {
		t.Fatalf("bogus cell: %v", err)
	}

	slot := base.UnixMilli() / 600000
	if _, err := s.BackfillCells(ctx, slot, slot, true); err == nil ||
		!strings.Contains(err.Error(), "surplus") {
		t.Fatalf("expected surplus abort, got %v", err)
	}
}
