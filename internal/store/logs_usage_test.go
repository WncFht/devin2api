// 本文件验证 logs 表的读路径：SearchLogs 的结构化筛选与分页计数、
// UsageStats 的延迟分位数/限流采样/SLA 归因/10 分钟桶网格。
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// TestSearchLogsFilters 验证 LogQuery 各筛选维度的下推语义与
// total（独立标量计数，分页前的精确命中数）。
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
// 计入窗口 totals，但不落任何 10 分钟桶——当前桶数据不被覆盖。points
// 是稀疏序列：只有当前行落桶，len=1 而非完整网格。
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
	if len(snap.Points) != 1 {
		t.Fatalf("points len = %d, want 1（稀疏序列只有当前桶）", len(snap.Points))
	}
	current := snap.Points[len(snap.Points)-1]
	if current.At != now.Unix()/600*600 || current.Requests != 1 || current.InputTokens != 7 {
		t.Fatalf("current bucket = %+v, want at=%d requests=1 input=7", current, now.Unix()/600*600)
	}
}

// TestUsagePointsSparse 验证 points 的稀疏编码：零流量桶不进序列，
// 非零桶在 JSON 里只发非零字段（at 恒在）。消费侧按缺省 0 求和，
// 合计口径与稠密编码一致。
func TestUsagePointsSparse(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now().Truncate(10 * time.Minute)
	// 两个相隔 1 小时的非零桶，中间 5 个零桶不应出现。
	for i, off := range []time.Duration{0, -time.Hour} {
		if _, err := s.InsertLog(ctx, &LogRow{
			Dir: fmt.Sprintf("sp-%d", i), StartedAt: now.Add(off), Result: "completed", InputTokens: int64(10 + i),
		}); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}
	snap, err := s.UsageStats(ctx)
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if len(snap.Points) != 2 {
		t.Fatalf("sparse points len = %d, want 2", len(snap.Points))
	}
	// 旧到新排序保留；首尾 at 相隔恰好 1 小时。
	if snap.Points[1].At-snap.Points[0].At != 3600 {
		t.Fatalf("points at = %d/%d, want 相隔 3600s", snap.Points[0].At, snap.Points[1].At)
	}
	raw, err := json.Marshal(snap.Points[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]int64
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["requests"] != 1 || m["input_tokens"] != 10 {
		t.Fatalf("nonzero fields = %v", m)
	}
	for _, k := range []string{"errors", "disconnected", "rate_limited", "output_tokens", "gen_ms"} {
		if _, ok := m[k]; ok {
			t.Fatalf("zero field %q should be omitted: %v", k, m)
		}
	}
	if _, ok := m["at"]; !ok {
		t.Fatal("at must always be emitted")
	}
}

// TestAccountAggsFold 验证按号分组聚合的折叠口径：” 与 'default' 行
// 合流进 default 桶（对齐 QuotaReport ”→default），不出三群幽灵桶。
func TestAccountAggsFold(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	fup := func(v int64) *int64 { return &v }
	rows := []*LogRow{
		{Dir: "ag-e0", StartedAt: now, DurationMS: 100, StatusCode: 200, Result: "completed", Account: "", FirstUpstreamMS: fup(10)},
		{Dir: "ag-e1", StartedAt: now, DurationMS: 300, StatusCode: 200, Result: "completed", Account: "", FirstUpstreamMS: fup(30)},
		{Dir: "ag-d0", StartedAt: now, DurationMS: 200, StatusCode: 200, Result: "completed", Account: "default", FirstUpstreamMS: fup(20)},
		{Dir: "ag-yj", StartedAt: now, DurationMS: 500, StatusCode: 200, Result: "completed", Account: "yanjian", FirstUpstreamMS: fup(50)},
	}
	for _, r := range rows {
		if _, err := s.InsertLog(ctx, r); err != nil {
			t.Fatalf("InsertLog: %v", err)
		}
	}

	aggs, err := s.AccountAggs(ctx)
	if err != nil {
		t.Fatalf("AccountAggs: %v", err)
	}
	if len(aggs) != 2 {
		t.Fatalf("dims = %+v, want default+yanjian 两桶", aggs)
	}
	byName := map[string]DimensionAgg{}
	for _, d := range aggs {
		byName[d.Name] = d
	}
	def := byName["default"]
	if def.Requests != 3 || def.AvgDuration != 200 || def.AvgTTFB != 20 {
		t.Fatalf("default 桶 = %+v, want requests=3 avgDur=200 avgTTFB=20", def)
	}
	yj := byName["yanjian"]
	if yj.Requests != 1 || yj.AvgDuration != 500 || yj.AvgTTFB != 50 {
		t.Fatalf("yanjian 桶 = %+v", yj)
	}
}

// TestAccountUsage 验证 AccountUsage 的原始量：今日计数（requests/ok/
// non499/tokens）、60s 完成窗聚合（rpm_now/tps_now/cache_rate 原料）、
// 首字延迟样本分位与均值；并覆盖 'default' 折叠 ” 历史行。
func TestAccountUsage(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	yesterday := now.AddDate(0, 0, -1)
	fup := func(v int64) *int64 { return &v }
	rows := []*LogRow{
		// yanjian：今日 3 行（200/500/499）+ 昨日 1 行（边界守卫）。
		{Dir: "u-y1", StartedAt: now, DurationMS: 2000, StatusCode: 200, Result: "completed",
			Account: "yanjian", InputTokens: 30, OutputTokens: 50, CacheReadTokens: 10,
			TotalTokens: 100, FirstUpstreamMS: fup(100)},
		{Dir: "u-y2", StartedAt: now, DurationMS: 1000, StatusCode: 500, Result: "failed",
			Account: "yanjian", OutputTokens: 20, TotalTokens: 60},
		{Dir: "u-y3", StartedAt: now, DurationMS: 500, StatusCode: 499, Result: "disconnected",
			Account: "yanjian"},
		{Dir: "u-y4", StartedAt: yesterday, DurationMS: 100, StatusCode: 200, Result: "completed",
			Account: "yanjian", TotalTokens: 5},
		// 别号行——不得漏进 yanjian 的 usage。
		{Dir: "u-r1", StartedAt: now, DurationMS: 800, StatusCode: 200, Result: "completed",
			Account: "randall", TotalTokens: 7, FirstUpstreamMS: fup(50)},
		// '' 与 'default' 两群——折叠后同属 default。
		{Dir: "u-e1", StartedAt: now, DurationMS: 3000, StatusCode: 200, Result: "completed",
			Account: "", OutputTokens: 6, TotalTokens: 3, FirstUpstreamMS: fup(10)},
		{Dir: "u-d1", StartedAt: now, DurationMS: 1000, StatusCode: 200, Result: "completed",
			Account: "default", OutputTokens: 4, TotalTokens: 2, FirstUpstreamMS: fup(30)},
	}
	for _, r := range rows {
		if _, err := s.InsertLog(ctx, r); err != nil {
			t.Fatalf("InsertLog %s: %v", r.Dir, err)
		}
	}

	row, err := s.AccountUsage(ctx, "yanjian")
	if err != nil {
		t.Fatalf("AccountUsage: %v", err)
	}
	// 今日：3 完成行（含 499），1 个 2xx，2 个非 499，tokens=160。
	if row.Today.Requests != 3 || row.Today.OK != 1 || row.Today.Non499 != 2 || row.Today.Tokens != 160 {
		t.Fatalf("today = %+v", row.Today)
	}
	// 近窗：非 499 完成数 2（y1+y2）；GenMS=(2000-100)+1000+500=3400
	//（499 行也计入生成时长分母，与 LogRecentWindow 口径一致）。
	if row.Recent.Req != 2 || row.Recent.OutTok != 70 || row.Recent.InTok != 30 ||
		row.Recent.CrTok != 10 || row.Recent.GenMS != 3400 {
		t.Fatalf("recent = %+v", row.Recent)
	}
	// TTFB 样本：yanjian 只有 y1 的 100ms。
	if row.TTFB.Samples != 1 || row.TTFB.P50 != 100 || row.TTFB.P90 != 100 || row.TTFBAvgMS != 100 {
		t.Fatalf("ttfb = %+v avg=%v", row.TTFB, row.TTFBAvgMS)
	}

	// 'default' 折叠 ''+'default' 两群（e1+d1）。
	def, err := s.AccountUsage(ctx, "default")
	if err != nil {
		t.Fatalf("AccountUsage default: %v", err)
	}
	if def.Today.Requests != 2 || def.Today.OK != 2 || def.Today.Tokens != 5 {
		t.Fatalf("default today = %+v", def.Today)
	}
	if def.Recent.Req != 2 || def.Recent.OutTok != 10 || def.Recent.GenMS != 2990+970 {
		t.Fatalf("default recent = %+v", def.Recent)
	}
	// 样本 [10,30]：pick=int(q*(n-1))，p50=p90=sorted[0]=10，均值 20。
	if def.TTFB.Samples != 2 || def.TTFB.P50 != 10 || def.TTFB.P90 != 10 || def.TTFBAvgMS != 20 {
		t.Fatalf("default ttfb = %+v avg=%v", def.TTFB, def.TTFBAvgMS)
	}
}
