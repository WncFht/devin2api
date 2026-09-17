// 本文件验证 logs 表的读路径：SearchLogs 的结构化筛选与分页计数、
// UsageStats 的延迟分位数/限流采样/SLA 归因/10 分钟桶网格。
package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestSearchLogsFilters 验证 LogQuery 各筛选维度的下推语义与
// total（COUNT(*) OVER()，分页前的精确命中数）。
func TestSearchLogsFilters(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	for i, r := range []*LogRow{
		{StartedAt: now, Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed", Model: "m-a"},
		{StartedAt: now, Method: "POST", Path: "/v1/messages", StatusCode: 400, Result: "failed", Model: "m-b", ErrorStage: "http_decode"},
		{StartedAt: now, Method: "POST", Path: "/v1/messages", StatusCode: 500, Result: "failed", Model: "m-a", ErrorStage: "provider_stream"},
	} {
		r.Dir = fmt.Sprintf("d-%d", i)
		if _, err := s.InsertLog(ctx, r); err != nil {
			t.Fatalf("InsertLog %d: %v", i, err)
		}
	}

	rows, total, err := s.SearchLogs(ctx, LogQuery{StatusClass: "4xx"})
	if err != nil || len(rows) != 1 || rows[0].Model != "m-b" {
		t.Fatalf("status_class=4xx = %v/%v err=%v", rows, total, err)
	}
	// status 表达式：精确/取反/比较/段位/逗号 OR；非法表达式不匹配任何条目。
	for _, tc := range []struct {
		expr string
		want int
	}{
		{"400", 1}, {"!200", 2}, {">=400", 2}, {"<300", 1}, {"4xx", 1},
		{"400,500", 2}, {"!2xx", 2}, {"garbage", 0}, {">=4xx", 0},
	} {
		rows, _, err := s.SearchLogs(ctx, LogQuery{StatusExpr: tc.expr})
		if err != nil || len(rows) != tc.want {
			t.Fatalf("status=%q = %d 条, want %d (err=%v)", tc.expr, len(rows), tc.want, err)
		}
	}
	if rows, _, _ := s.SearchLogs(ctx, LogQuery{Model: "m-a"}); len(rows) != 2 {
		t.Fatalf("model=m-a = %+v", rows)
	}
	if rows, _, _ := s.SearchLogs(ctx, LogQuery{ErrorStage: "provider_stream"}); len(rows) != 1 {
		t.Fatalf("error_stage = %+v", rows)
	}
	if rows, _, _ := s.SearchLogs(ctx, LogQuery{Result: "completed"}); len(rows) != 1 {
		t.Fatalf("result=completed = %+v", rows)
	}
	if rows, _, _ := s.SearchLogs(ctx, LogQuery{SinceMS: now.Add(time.Hour).UnixMilli()}); len(rows) != 0 {
		t.Fatalf("since future = %+v", rows)
	}
	if rows, _, _ := s.SearchLogs(ctx, LogQuery{UntilMS: now.Add(-time.Hour).UnixMilli()}); len(rows) != 0 {
		t.Fatalf("until past = %+v", rows)
	}
	if rows, _, _ := s.SearchLogs(ctx, LogQuery{UntilMS: now.Add(time.Hour).UnixMilli()}); len(rows) != 3 {
		t.Fatalf("until future = %+v", rows)
	}
	// 倒序同刻按 id 打破平局：最后插入的在前。
	rows, _, err = s.SearchLogs(ctx, LogQuery{})
	if err != nil || len(rows) != 3 || rows[0].Dir != "d-2" {
		t.Fatalf("order = %+v err=%v", rows, err)
	}
	// limit 用尽时 total 仍报命中总数——面板据此推 has_more。
	rows, total, err = s.SearchLogs(ctx, LogQuery{Limit: 1})
	if err != nil || len(rows) != 1 || total != 3 {
		t.Fatalf("limit=1 = %d rows total=%d err=%v, want 1/3", len(rows), total, err)
	}
}

// TestUsageLatencyPercentiles 验证分位数口径（pick=sorted[q*(n-1)]）与
// 样本上限：超过 usageSampleCapacity 后只留最近 N 条。
func TestUsageLatencyPercentiles(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	for i := 1; i <= 100; i++ {
		if _, err := s.InsertLog(ctx, &LogRow{
			Dir: fmt.Sprintf("p-%d", i), StartedAt: now, DurationMS: int64(i), Result: "completed",
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	lat, err := s.LogLatency(ctx)
	if err != nil {
		t.Fatalf("LogLatency: %v", err)
	}
	stats := lat["duration"]
	if stats.P50 != 50 || stats.P95 != 95 || stats.P99 != 99 || stats.Max != 100 {
		t.Fatalf("percentiles = %+v", stats)
	}
	for i := 0; i < usageSampleCapacity; i++ {
		if _, err := s.InsertLog(ctx, &LogRow{
			Dir: fmt.Sprintf("c-%d", i), StartedAt: now, DurationMS: 1, Result: "completed",
		}); err != nil {
			t.Fatalf("InsertLog cap: %v", err)
		}
	}
	lat, err = s.LogLatency(ctx)
	if err != nil {
		t.Fatalf("LogLatency: %v", err)
	}
	if stats = lat["duration"]; stats.Samples != usageSampleCapacity || stats.P50 != 1 {
		t.Fatalf("capped samples = %+v, want %d/p50=1", stats, usageSampleCapacity)
	}
}

// TestUsageRateLimitEvents 验证上游 429 的计数与「完成时刻前 60s 内启动
// 请求数」的 RPM 采样。
func TestUsageRateLimitEvents(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	base := time.Now()
	row := func(dir string, off time.Duration, status int, durMS int64) *LogRow {
		return &LogRow{Dir: dir, StartedAt: base.Add(off), DurationMS: durMS,
			StatusCode: status, Result: "completed", Model: "m-a"}
	}
	rows := []*LogRow{
		row("rl-120", -120*time.Second, 200, 100), // 在 429 的 60s 窗口之外
		row("rl-30", -30*time.Second, 200, 100),
		row("rl-20", -20*time.Second, 200, 100),
		row("rl-10", -10*time.Second, 200, 100),
		// end = -5s+2s = -3s；窗口 (-63s,-3s] 内含 -30/-20/-10/-5 共 4 个 start。
		row("rl-5", -5*time.Second, 429, 2000),
	}
	rows[len(rows)-1].ErrorStage = "rate_gate"
	for _, r := range rows {
		if _, err := s.InsertLog(ctx, r); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}

	snap, err := s.UsageStats(ctx)
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if snap.Window.RateLimited != 1 {
		t.Fatalf("window rate_limited = %d", snap.Window.RateLimited)
	}
	if len(snap.RateLimitEvents) != 1 {
		t.Fatalf("rate_limit_events = %+v", snap.RateLimitEvents)
	}
	ev := snap.RateLimitEvents[0]
	if ev.Model != "m-a" || ev.RPM != 4 || ev.At != base.Unix()-3 || ev.Stage != "rate_gate" {
		t.Fatalf("event = %+v", ev)
	}
	if len(snap.Models) != 1 || snap.Models[0].RateLimited != 1 {
		t.Fatalf("model agg = %+v", snap.Models)
	}
}

// TestUsageFaults 验证失败责任归因的聚合口径：客户端责任（断连、请求体
// 阶段失败）与 429 限流分列，只有服务端失分计 upstream_faults。
func TestUsageFaults(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	add := func(i, status int, result, stage string) {
		if _, err := s.InsertLog(ctx, &LogRow{
			Dir: fmt.Sprintf("f-%d", i), StartedAt: now, DurationMS: 10,
			StatusCode: status, Result: result, ErrorStage: stage, Model: "m-x",
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	add(0, 200, "completed", "")
	add(1, 200, "completed", "")
	add(2, 429, "failed", "devin_connect")
	add(3, 200, "disconnected", "")
	add(4, 400, "failed", "http_decode")
	add(5, 500, "failed", "provider_stream")

	snap, err := s.UsageStats(ctx)
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if len(snap.Models) != 1 {
		t.Fatalf("models = %+v", snap.Models)
	}
	m := snap.Models[0]
	if m.ClientFaults != 2 || m.UpstreamFaults != 1 || m.RateLimited != 1 {
		t.Fatalf("faults = %+v", m)
	}
	if snap.Window.ClientFaults != 2 || snap.Window.UpstreamFaults != 1 {
		t.Fatalf("window = %+v", snap.Window)
	}
	var sumClient, sumUpstream int64
	for _, p := range snap.Points {
		sumClient += p.ClientFaults
		sumUpstream += p.UpstreamFaults
	}
	if sumClient != 2 || sumUpstream != 1 {
		t.Fatalf("points faults = %d/%d", sumClient, sumUpstream)
	}
}

// TestUsageMinBucketWraparound 验证 8 天网格的边界：早于保留窗的条目仍
// 计入窗口 totals，但不落任何 10 分钟桶——当前桶数据不被覆盖。
func TestUsageMinBucketWraparound(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().Truncate(10 * time.Minute)
	if _, err := s.InsertLog(ctx, &LogRow{
		Dir: "cur", StartedAt: now, DurationMS: 5, Result: "completed", InputTokens: 7,
	}); err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	// 恰好 usageMinBuckets 个桶（8 天）之前的条目落在网格窗口之外。
	old := now.Add(-usageMinBuckets * 10 * time.Minute)
	if _, err := s.InsertLog(ctx, &LogRow{
		Dir: "old", StartedAt: old, DurationMS: 9, Result: "completed", InputTokens: 3,
	}); err != nil {
		t.Fatalf("InsertLog old: %v", err)
	}
	snap, err := s.UsageStats(ctx)
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if snap.Window.Requests != 2 || snap.Window.InputTokens != 10 {
		t.Fatalf("window = %+v", snap.Window)
	}
	if len(snap.Points) != usageMinBuckets {
		t.Fatalf("points len = %d, want %d", len(snap.Points), usageMinBuckets)
	}
	current := snap.Points[len(snap.Points)-1]
	if current.Requests != 1 || current.InputTokens != 7 {
		t.Fatalf("current bucket = %+v, want requests=1 input=7", current)
	}
}
