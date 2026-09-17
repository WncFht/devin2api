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
	insertFoldFixture(t, s, now, map[string]int{"": 1, "default": 2, "yanjian": 3})

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
	if got := count(LogQuery{Account: "yanjian"}); got != 3 {
		t.Fatalf("LogQuery account=yanjian = %d, want 3", got)
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
	if got := recent("yanjian"); got != 3 {
		t.Fatalf("recent(yanjian) = %d, want 3", got)
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
	err = s.LogCells(ctx, now.Unix()-1, now.Unix()+1, LogScope{Account: "yanjian"},
		func(_ LogCellKey, c LogCellTotals) { cellReqs += c.Requests })
	if err != nil || cellReqs != 3 {
		t.Fatalf("LogCells(yanjian) = %d err=%v, want 3", cellReqs, err)
	}
	cellReqs = 0
	err = s.LogCells(ctx, now.Unix()-1, now.Unix()+1, LogScope{Account: "default"},
		func(_ LogCellKey, c LogCellTotals) { cellReqs += c.Requests })
	if err != nil || cellReqs != 3 {
		t.Fatalf("LogCells(default) = %d err=%v, want 3", cellReqs, err)
	}
}
