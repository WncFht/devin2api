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
	"strings"
	"testing"
	"time"
)

// cellSeedRows 覆盖 cellMetrics 全部谓词分支：完成流式（可 decode）、
// fbt≥duration、fbt 缺席、499 两态、429、rate_limited 非 429、上游/
// 客户端责任归因、rejected 剔除、非流式、零时长、decode 速率上限。
func cellSeedRows(base time.Time) []*LogRow {
	fbt := int64(1200)
	small := int64(10)
	return []*LogRow{
		{StartedAt: base, DurationMS: 5000, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			Stream: true, FirstUpstreamMS: &fbt, API: "anthropic", Model: "m-a", RequestedModel: "m-a", KeyHash: "kh1",
			InputTokens: 10, OutputTokens: 100, CacheReadTokens: 3, CacheWriteTokens: 2, ReasoningTokens: 4, TotalTokens: 119, CreditCost: 7},
		{StartedAt: base.Add(time.Minute), DurationMS: 1000, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			Stream: true, FirstUpstreamMS: &fbt, API: "anthropic", Model: "m-a", KeyHash: "kh1", OutputTokens: 5},
		{StartedAt: base.Add(2 * time.Minute), DurationMS: 2000, Method: "POST", Path: "/v1/chat", StatusCode: 200, Result: "completed",
			Stream: true, API: "openai-chat", Model: "m-b", KeyHash: "kh2", OutputTokens: 50},
		{StartedAt: base.Add(3 * time.Minute), DurationMS: 300, Method: "POST", Path: "/v1/messages", StatusCode: 499, Result: "disconnected",
			API: "anthropic", Model: "m-a", KeyHash: "kh1", OutputTokens: 3},
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
			API: "anthropic", Model: "m-b", KeyHash: "kh2", OutputTokens: 20, TotalTokens: 20},
		{StartedAt: base.Add(12 * time.Minute), DurationMS: 0, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
			API: "anthropic", KeyHash: "kh1"},
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

// cellGroupTruth 是一格的聚合值：38 个指标列 + min_time + last_key。
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
