// 本文件验证 logs 表读路径的 account 读侧折叠口径（logAccountExpr）：
// ” 与 'default' 行合流进 default 桶，真名账号只命中真名行——
// LogQuery、LogScope 及各聚合查询共用同一谓词。
package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// insertFoldFixture 造 ”/'default'/真名三群 account 行（各 n 条，
// status=200，时刻 now，duration=0——完成时刻即现在，落进 60s 完成窗）。
func insertFoldFixture(t *testing.T, s *Store, now time.Time, groups map[string]int) {
	t.Helper()
	i := 0
	for account, n := range groups {
		for j := 0; j < n; j++ {
			if _, err := s.InsertLog(context.Background(), &LogRow{
				Dir: fmt.Sprintf("fold-%d", i), StartedAt: now,
				StatusCode: 200, Result: "completed", Account: account,
			}); err != nil {
				t.Fatalf("InsertLog %q: %v", account, err)
			}
			i++
		}
	}
}

// TestAccountFoldFilter 验证单号过滤的折叠语义：参数 'default' 命中
// ”+'default' 两群，真名参数只命中真名行，空参数不过滤。三个消费
// 面——LogQuery（SearchLogs）、LogScope（LogRecentWindow/LogCells/
// LogRecentRPM）——口径一致。
func TestAccountFoldFilter(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	insertFoldFixture(t, s, now, map[string]int{"": 1, "default": 2, "alpha": 3})

	count := func(q LogQuery) int64 {
		_, total, err := s.SearchLogs(ctx, q)
		if err != nil {
			t.Fatalf("SearchLogs %+v: %v", q, err)
		}
		return total
	}
	if got := count(LogQuery{Account: "default"}); got != 3 {
		t.Fatalf("LogQuery account=default = %d, want 3 (''+'default')", got)
	}
	if got := count(LogQuery{Account: "alpha"}); got != 3 {
		t.Fatalf("LogQuery account=alpha = %d, want 3", got)
	}
	if got := count(LogQuery{}); got != 6 {
		t.Fatalf("LogQuery 无过滤 = %d, want 6", got)
	}

	recent := func(account string) int64 {
		a, err := s.LogRecentWindow(ctx, 60, LogScope{Account: account})
		if err != nil {
			t.Fatalf("LogRecentWindow %q: %v", account, err)
		}
		return a.Req
	}
	if got := recent("default"); got != 3 {
		t.Fatalf("recent(default) = %d, want 3", got)
	}
	if got := recent("alpha"); got != 3 {
		t.Fatalf("recent(alpha) = %d, want 3", got)
	}
	if got := recent(""); got != 6 {
		t.Fatalf("recent(全量) = %d, want 6", got)
	}

	rpm, err := s.LogRecentRPM(ctx, LogScope{Account: "default"})
	if err != nil || rpm != 3 {
		t.Fatalf("LogRecentRPM(default) = %v err=%v, want 3", rpm, err)
	}

	// LogCells 逐格回调，scope 过滤后格子计数只含命中群的行。
	var cellReqs int64
	err = s.LogCells(ctx, 600, now.Unix()-1, now.Unix()+1, LogScope{Account: "alpha"},
		func(_ LogCellKey, c LogCellTotals) { cellReqs += c.Requests })
	if err != nil || cellReqs != 3 {
		t.Fatalf("LogCells(alpha) = %d err=%v, want 3", cellReqs, err)
	}
	cellReqs = 0
	err = s.LogCells(ctx, 600, now.Unix()-1, now.Unix()+1, LogScope{Account: "default"},
		func(_ LogCellKey, c LogCellTotals) { cellReqs += c.Requests })
	if err != nil || cellReqs != 3 {
		t.Fatalf("LogCells(default) = %d err=%v, want 3", cellReqs, err)
	}
}

// TestRejectedRowsPartitioned 验证 rejected 行（管线前拒绝留存）的分域
// 口径：写侧 LogSource 置值即生效；默认列表剔除、log_source=rejected
// 与 all 可见；LogScope 系聚合与 UsageStats 一律不计。
func TestRejectedRowsPartitioned(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	for i, dir := range []string{"ok-1", "ok-2"} {
		if _, err := s.InsertLog(ctx, &LogRow{
			Dir: dir, StartedAt: now.Add(time.Duration(i) * time.Second),
			StatusCode: 200, Result: "completed", Model: "m",
		}); err != nil {
			t.Fatalf("InsertLog %q: %v", dir, err)
		}
	}
	if _, err := s.InsertLog(ctx, &LogRow{
		StartedAt: now, StatusCode: 429, Result: "rejected",
		LogSource: "rejected", ErrorStage: "pre_pipeline", ErrorMessage: "concurrency_limit",
	}); err != nil {
		t.Fatalf("InsertLog rejected: %v", err)
	}

	total := func(q LogQuery) int64 {
		_, n, err := s.SearchLogs(ctx, q)
		if err != nil {
			t.Fatalf("SearchLogs %+v: %v", q, err)
		}
		return n
	}
	if got := total(LogQuery{}); got != 2 {
		t.Fatalf("默认视图 = %d, want 2（rejected 剔除）", got)
	}
	if got := total(LogQuery{LogSource: "rejected"}); got != 1 {
		t.Fatalf("log_source=rejected = %d, want 1", got)
	}
	if got := total(LogQuery{LogSource: "all"}); got != 3 {
		t.Fatalf("log_source=all = %d, want 3", got)
	}
	// result=rejected 筛选在默认口径下仍空——来源过滤先于结果列。
	if got := total(LogQuery{Result: "rejected"}); got != 0 {
		t.Fatalf("result=rejected 默认口径 = %d, want 0", got)
	}

	snap, err := s.UsageStats(ctx)
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if snap.Window.Requests != 2 || snap.Entries != 2 {
		t.Fatalf("UsageStats window=%d entries=%d, want 2/2（rejected 不入聚合）",
			snap.Window.Requests, snap.Entries)
	}
	recent, err := s.LogRecentWindow(ctx, 60, LogScope{})
	if err != nil {
		t.Fatalf("LogRecentWindow: %v", err)
	}
	if recent.Req != 2 {
		t.Fatalf("LogRecentWindow = %d, want 2", recent.Req)
	}
}

// TestSearchLogsBeforeID 验证 keyset 翻页：before_id 只取更早的行，
// 与 OFFSET 语义可独立组合；ExistsLogBefore 不把 before_id 当筛选条件。
func TestSearchLogsBeforeID(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	var ids []int64
	for i := 0; i < 5; i++ {
		id, err := s.InsertLog(ctx, &LogRow{
			Dir: fmt.Sprintf("k-%d", i), StartedAt: now.Add(time.Duration(i) * time.Second),
			StatusCode: 200, Result: "completed",
		})
		if err != nil {
			t.Fatalf("InsertLog %d: %v", i, err)
		}
		ids = append(ids, id)
	}

	rows, _, err := s.SearchLogs(ctx, LogQuery{BeforeID: ids[3], Limit: 2})
	if err != nil {
		t.Fatalf("SearchLogs before_id: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != ids[2] || rows[1].ID != ids[1] {
		t.Fatalf("before_id 页 = %+v, want [%d %d]", rows, ids[2], ids[1])
	}
	// 与 Result 过滤正交：筛不存在的 result 时 before_id 不造出行。
	if rows, _, err := s.SearchLogs(ctx, LogQuery{BeforeID: ids[3], Result: "failed"}); err != nil || len(rows) != 0 {
		t.Fatalf("before_id+result = %v err=%v, want 空", rows, err)
	}
}

// TestAffinityHashRoundTrip 钉住 logs.affinity_hash 的写读回路：号池
// 谱系亲和键随摘要行落库（迁移 0011），读侧原值回填——谱系分析的
// GROUP BY 维。空串行照读不误（非号池/拒绝路径的默认口径）。
func TestAffinityHashRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.InsertLog(ctx, &LogRow{
		Dir: "aff-1", StartedAt: time.Now(), StatusCode: 200,
		Result: "completed", AffinityHash: "abc123",
	}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	rows, _, err := s.SearchLogs(ctx, LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 || rows[0].AffinityHash != "abc123" {
		t.Fatalf("AffinityHash round-trip = %+v, want [abc123]", rows)
	}
}

// TestDeleteLogsBeforeBatched 验证 logs 按龄删除的分片循环与水位钳制：
// 超一片（deleteLogsBatch）的存量分批删净；绕过双写的水位外老行本轮
// 豁免——其聚合贡献未入 cells 账本，删了会无声消失，ReconcileCells
// 覆盖后下轮再删。
func TestDeleteLogsBeforeBatched(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	old := time.Now().Add(-100 * 24 * time.Hour)
	cutoff := time.Now().Add(-90 * 24 * time.Hour).UnixMilli()

	// 直接 SQL 批量插超一片的老行再 ReconcileCells 记账——与写入期
	// 双写推进水位同效，行数逼出多片循环。
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	for i := 0; i < deleteLogsBatch+7; i++ {
		if _, err := tx.ExecContext(ctx, logsInsertSQL, logInsertArgs(&LogRow{
			Dir: fmt.Sprintf("bulk-%d", i), StartedAt: old,
			StatusCode: 200, Result: "completed",
		})...); err != nil {
			t.Fatalf("bulk insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit bulk: %v", err)
	}
	if err := s.ReconcileCells(ctx); err != nil {
		t.Fatalf("ReconcileCells: %v", err)
	}
	// 保留期内的行不删。
	if _, err := s.InsertLog(ctx, &LogRow{
		Dir: "fresh", StartedAt: time.Now(), StatusCode: 200, Result: "completed",
	}); err != nil {
		t.Fatalf("InsertLog fresh: %v", err)
	}
	got, err := s.DeleteLogsBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteLogsBefore: %v", err)
	}
	if want := int64(deleteLogsBatch + 7); got != want {
		t.Fatalf("deleted = %d, want %d", got, want)
	}
	if n, err := s.LogCount(ctx); err != nil || n != 1 {
		t.Fatalf("remaining = %d err=%v, want 1（fresh 行）", n, err)
	}

	// 水位外老行豁免：bypass 插入的 id > 水位，本轮不删；
	// ReconcileCells 覆盖后可删。
	if _, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(&LogRow{
		Dir: "bypass-old", StartedAt: old, StatusCode: 200, Result: "completed",
	})...); err != nil {
		t.Fatalf("bypass insert: %v", err)
	}
	if got, err := s.DeleteLogsBefore(ctx, cutoff); err != nil || got != 0 {
		t.Fatalf("pre-reconcile deleted = %d err=%v, want 0（水位外豁免）", got, err)
	}
	if err := s.ReconcileCells(ctx); err != nil {
		t.Fatalf("ReconcileCells: %v", err)
	}
	if got, err := s.DeleteLogsBefore(ctx, cutoff); err != nil || got != 1 {
		t.Fatalf("post-reconcile deleted = %d err=%v, want 1", got, err)
	}
}
