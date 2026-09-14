// 本文件验证 index.jsonl 的用量聚合：多维累计、延迟分位数、启动回放。
package debuglog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// TestUsageAggregatorCounts 验证一次请求完成后各维度都被计入。
func TestUsageAggregatorCounts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	reasoning := int64(7)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", KeyHash: "k1"})
	recorder.Complete(Completion{
		StatusCode: 200, Result: "completed", Model: "swe-2-max", RequestedModel: "swe-2",
		Usage: usageFixture(100, 50, 40, 30, &reasoning, 187),
	})
	failed := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", KeyHash: "k1"})
	failed.WriteError("provider_stream", os.ErrNotExist)
	failed.Complete(Completion{StatusCode: 500, Result: "failed", Model: "swe-2-max", Usage: usageFixture(10, 0, 0, 0, nil, 10)})

	snap := manager.UsageStats()
	if snap.Today.Requests != 2 || snap.Today.Errors != 1 {
		t.Fatalf("today = %+v", snap.Today)
	}
	if snap.Today.InputTokens != 110 || snap.Today.OutputTokens != 50 ||
		snap.Today.CacheRead != 40 || snap.Today.CacheWrite != 30 ||
		snap.Today.Reasoning != 7 || snap.Today.TotalTokens != 197 {
		t.Fatalf("today tokens = %+v", snap.Today)
	}
	if len(snap.Models) != 1 || snap.Models[0].Name != "swe-2-max" || snap.Models[0].Requests != 2 || snap.Models[0].Errors != 1 {
		t.Fatalf("models = %+v", snap.Models)
	}
	// model_days 供面板按自然日范围过滤模型表：两次请求都落在今天。
	todayKey := time.Now().Local().Format("2006-01-02")
	md := snap.ModelDays["swe-2-max"][todayKey]
	if md.Requests != 2 || md.InputTokens != 110 {
		t.Fatalf("model_days = %+v", snap.ModelDays)
	}
	if len(snap.Keys) != 1 || snap.Keys[0].Requests != 2 {
		t.Fatalf("keys = %+v", snap.Keys)
	}
	if snap.ErrorStages["provider_stream"] != 1 {
		t.Fatalf("error_stages = %+v", snap.ErrorStages)
	}
	if len(snap.Points) != usageMinBuckets {
		t.Fatalf("points len = %d", len(snap.Points))
	}
	if snap.Duration.Samples != 2 {
		t.Fatalf("duration stats = %+v", snap.Duration)
	}
}

// TestUsageReplayOnRestart 验证进程重启（重建 Manager）后历史统计不丢。
func TestUsageReplayOnRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	first := NewManager(root, RetentionPolicy{})
	recorder := first.Start(RequestMeta{Method: "POST", Path: "/v1/chat/completions", API: "openai-chat"})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "glm-5-2", Usage: usageFixture(1000, 200, 0, 0, nil, 1200)})
	first.Close()

	second := NewManager(root, RetentionPolicy{})
	defer second.Close()
	snap := second.UsageStats()
	if snap.Window.Requests != 1 || snap.Window.InputTokens != 1000 || snap.Window.TotalTokens != 1200 {
		t.Fatalf("replayed window = %+v", snap.Window)
	}
	if len(snap.Models) != 1 || snap.Models[0].Name != "glm-5-2" {
		t.Fatalf("replayed models = %+v", snap.Models)
	}
	// 回放后新请求继续累加，不重复计数。
	recorder2 := second.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
	recorder2.Complete(Completion{StatusCode: 200, Result: "completed", Model: "glm-5-2", Usage: usageFixture(5, 5, 0, 0, nil, 10)})
	snap = second.UsageStats()
	if snap.Window.Requests != 2 || snap.Window.TotalTokens != 1210 {
		t.Fatalf("post-replay window = %+v", snap.Window)
	}
}

// TestUsagePercentiles 验证蓄水池 p50/p95/p99 与环覆盖。
func TestUsagePercentiles(t *testing.T) {
	agg := newUsageAggregator()
	for i := 1; i <= 100; i++ {
		agg.add(IndexEntry{StartedAt: time.Now().Format(time.RFC3339Nano), DurationMS: int64(i), Result: "completed"})
	}
	stats := agg.durationSamples.stats()
	if stats.P50 != 50 || stats.P95 != 95 || stats.P99 != 99 || stats.Max != 100 {
		t.Fatalf("percentiles = %+v", stats)
	}
	// 蓄水池超容量后只保留最近样本。
	for i := 0; i < usageSampleCapacity; i++ {
		agg.add(IndexEntry{StartedAt: time.Now().Format(time.RFC3339Nano), DurationMS: 1, Result: "completed"})
	}
	stats = agg.durationSamples.stats()
	if stats.Samples != usageSampleCapacity {
		t.Fatalf("samples = %d, want %d", stats.Samples, usageSampleCapacity)
	}
}

// TestUsageRateLimitSampling 验证上游 429 单独计数，并按前 60s 窗口采样发出速率；
// 同时验证事件经 index.jsonl 回放在重启后重建。
func TestUsageRateLimitSampling(t *testing.T) {
	base := time.Now()
	// entry 以完成序构造：off 为相对 base 的启动时刻。
	entry := func(off time.Duration, status int, durMS int64) IndexEntry {
		return IndexEntry{
			StartedAt:  base.Add(off).Format(time.RFC3339Nano),
			DurationMS: durMS, StatusCode: status, Result: "completed", Model: "m-a",
		}
	}
	entries := []IndexEntry{
		entry(-120*time.Second, 200, 100), // 在 429 的 60s 窗口之外
		entry(-30*time.Second, 200, 100),
		entry(-20*time.Second, 200, 100),
		entry(-10*time.Second, 200, 100),
		// end = -5s+2s = -3s；窗口 (-63s,-3s] 内含 -30/-20/-10/-5 共 4 个 start。
		entry(-5*time.Second, 429, 2000),
	}

	agg := newUsageAggregator()
	for _, e := range entries {
		agg.add(e)
	}
	snap := agg.snapshot()
	if snap.Window.RateLimited != 1 {
		t.Fatalf("window rate_limited = %d", snap.Window.RateLimited)
	}
	if len(snap.RateLimitEvents) != 1 {
		t.Fatalf("rate_limit_events = %+v", snap.RateLimitEvents)
	}
	ev := snap.RateLimitEvents[0]
	if ev.Model != "m-a" || ev.RPM != 4 || ev.At != base.Unix()-3 {
		t.Fatalf("event = %+v", ev)
	}
	if snap.Models[0].RateLimited != 1 {
		t.Fatalf("model agg = %+v", snap.Models[0])
	}

	// 同一份 index 内容写文件回放，重启后聚合与采样应一致重建。
	path := filepath.Join(t.TempDir(), "index.jsonl")
	var buf strings.Builder
	for _, e := range entries {
		data, _ := json.Marshal(e)
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	replayed := newUsageAggregator()
	replayed.replayIndex(path)
	rsnap := replayed.snapshot()
	if rsnap.Window.RateLimited != 1 || len(rsnap.RateLimitEvents) != 1 || rsnap.RateLimitEvents[0].RPM != 4 {
		t.Fatalf("replayed = rate_limited %d events %+v", rsnap.Window.RateLimited, rsnap.RateLimitEvents)
	}
}

// TestRequestFilters 验证结构化筛选与 has_more 信号。
func TestRequestFilters(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	cases := []struct {
		model  string
		status int
		result string
		stage  string
	}{
		{"m-a", 200, "completed", ""},
		{"m-b", 400, "failed", "http_decode"},
		{"m-a", 500, "failed", "provider_stream"},
	}
	for _, c := range cases {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
		if c.stage != "" {
			recorder.WriteError(c.stage, os.ErrNotExist)
		}
		recorder.Complete(Completion{StatusCode: c.status, Result: c.result, Model: c.model})
	}

	if got := manager.ListRequests(10, RequestFilter{StatusClass: "4xx"}); len(got.Entries) != 1 || got.Entries[0].Model != "m-b" {
		t.Fatalf("status_class=4xx = %+v", got.Entries)
	}
	// status 表达式：精确/取反/比较/段位/逗号 OR；非法表达式不匹配任何条目。
	for _, tc := range []struct {
		expr string
		want int
	}{
		{"400", 1}, {"!200", 2}, {">=400", 2}, {"<300", 1}, {"4xx", 1},
		{"400,500", 2}, {"!2xx", 2}, {"garbage", 0}, {">=4xx", 0},
	} {
		if got := manager.ListRequests(10, RequestFilter{Status: tc.expr}); len(got.Entries) != tc.want {
			t.Fatalf("status=%q = %d 条, want %d", tc.expr, len(got.Entries), tc.want)
		}
	}
	if got := manager.ListRequests(10, RequestFilter{Model: "m-a"}); len(got.Entries) != 2 {
		t.Fatalf("model=m-a = %+v", got.Entries)
	}
	if got := manager.ListRequests(10, RequestFilter{ErrorStage: "provider_stream"}); len(got.Entries) != 1 {
		t.Fatalf("error_stage = %+v", got.Entries)
	}
	if got := manager.ListRequests(10, RequestFilter{Result: "completed"}); len(got.Entries) != 1 {
		t.Fatalf("result=completed = %+v", got.Entries)
	}
	if got := manager.ListRequests(10, RequestFilter{Since: time.Now().Add(time.Hour)}); len(got.Entries) != 0 {
		t.Fatalf("since future = %+v", got.Entries)
	}
	// until 把列表钉在历史窗口内：过去的上界一条不留，未来的上界全保留。
	if got := manager.ListRequests(10, RequestFilter{Until: time.Now().Add(-time.Hour)}); len(got.Entries) != 0 {
		t.Fatalf("until past = %+v", got.Entries)
	}
	if got := manager.ListRequests(10, RequestFilter{Until: time.Now().Add(time.Hour)}); len(got.Entries) != 3 {
		t.Fatalf("until future = %+v", got.Entries)
	}
	// limit 用尽时应提示窗口内仍有历史。
	if got := manager.ListRequests(1, RequestFilter{}); len(got.Entries) != 1 || !got.HasMore {
		t.Fatalf("limit=1 = %+v has_more=%v", got.Entries, got.HasMore)
	}
	if got := manager.ListRequests(10, RequestFilter{}); got.HasMore {
		t.Fatal("full scan should not report has_more")
	}
}

// TestErrorOwnerAndSLA 验证失败责任归类与 SLA 口径：客户端责任（断连、
// 请求体阶段失败）与 429 限流不进 SLA 分母，只有服务端失分扣分。
func TestErrorOwnerAndSLA(t *testing.T) {
	for _, tc := range []struct {
		status int
		result string
		stage  string
		want   string
	}{
		{200, "completed", "", ""},
		{429, "failed", "devin_connect", "business_limited"},
		{429, "failed", "rate_gate", "business_limited"},
		{200, "disconnected", "client_disconnected", "client"},
		{500, "aborted", "", "client"},
		{400, "failed", "http_decode", "client"},
		{413, "failed", "http_read", "client"},
		{500, "failed", "provider_stream", "upstream"},
		{502, "failed", "devin_transport", "upstream"},
		// 200+流内错误事件下发的失败：状态 200 但 result=failed，归服务端。
		{200, "failed", "response_event", "upstream"},
		// 上游返回的 4xx（非请求体阶段）同样记服务端失分。
		{404, "failed", "devin_connect", "upstream"},
	} {
		if got := errorOwner(IndexEntry{StatusCode: tc.status, Result: tc.result, ErrorStage: tc.stage}); got != tc.want {
			t.Fatalf("errorOwner(%d/%s/%s) = %q, want %q", tc.status, tc.result, tc.stage, got, tc.want)
		}
	}

	agg := newUsageAggregator()
	add := func(status int, result, stage string) {
		agg.add(IndexEntry{
			StartedAt: time.Now().Format(time.RFC3339Nano), DurationMS: 10,
			StatusCode: status, Result: result, ErrorStage: stage, Model: "m-x",
		})
	}
	add(200, "completed", "")
	add(200, "completed", "")
	add(429, "failed", "devin_connect")   // 限流：剔除分母
	add(200, "disconnected", "")          // 客户端：剔除分母
	add(400, "failed", "http_decode")     // 客户端 4xx：剔除分母
	add(500, "failed", "provider_stream") // 服务端失分：SLA 唯一扣分项
	snap := agg.snapshot()
	m := snap.Models[0]
	if m.ClientFaults != 2 || m.UpstreamFaults != 1 || m.RateLimited != 1 {
		t.Fatalf("faults = %+v", m)
	}
	// slable = 6 - 2 - 1 = 3；SLA = (3-1)/3 ≈ 0.667。
	if want := 2.0 / 3.0; m.SLASuccessRate < want-1e-9 || m.SLASuccessRate > want+1e-9 {
		t.Fatalf("sla_success_rate = %v, want %v", m.SLASuccessRate, want)
	}
	// 聚合层级同步：窗口 totals 与 10 分钟桶也要带归因计数。
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

// TestRetryAttemptsInIndex 验证重发计数随索引与 meta 落盘：
// NoteRetryAttempt 与 04 的 retry_attempt 分界行同源。
func TestRetryAttemptsInIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	dir := filepath.Base(recorder.DirectoryPath())
	recorder.NoteRetryAttempt(2, "unauthenticated: token reloaded")
	recorder.NoteRetryAttempt(3, "transport: EOF")
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "m-x"})

	entries := manager.ListRequests(10, RequestFilter{}).Entries
	if len(entries) != 1 || entries[0].Retries != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	// meta.json 应带明细（attempt 号/原因/相对时刻），面板据此渲染链路。
	data, err := os.ReadFile(filepath.Join(root, dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	attempts, _ := meta["retry_attempts"].([]any)
	if len(attempts) != 2 {
		t.Fatalf("retry_attempts = %v", meta["retry_attempts"])
	}
	second, _ := attempts[1].(map[string]any)
	if second["attempt"] != float64(3) || second["cause"] != "transport: EOF" {
		t.Fatalf("attempts[1] = %v", second)
	}
}

// TestSetEnabledHotToggle 验证日志开关运行时切换后 Start 立即生效。
func TestSetEnabledHotToggle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	manager.SetEnabled(false)
	if r := manager.Start(RequestMeta{}); r != nil {
		t.Fatal("Start should return nil while disabled")
	}
	manager.SetEnabled(true)
	if r := manager.Start(RequestMeta{}); r == nil {
		t.Fatal("Start should work after re-enable")
	} else {
		r.Complete(Completion{StatusCode: 200, Result: "completed"})
	}
}

// TestAbortActiveRequest 验证 Abort 取消挂接的 ctx 并把结果记为 aborted。
func TestAbortActiveRequest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	dir := filepath.Base(recorder.DirectoryPath())
	if manager.Abort(dir) {
		t.Fatal("Abort should fail before ctx is attached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder.SetAbort(cancel)

	active := manager.ActiveRequests()
	if len(active) != 1 || !active[0].Abortable || active[0].State != "waiting_upstream" {
		t.Fatalf("active = %+v", active)
	}
	if !manager.Abort(dir) {
		t.Fatal("Abort returned false")
	}
	if ctx.Err() == nil {
		t.Fatal("ctx not cancelled by Abort")
	}
	recorder.Complete(Completion{StatusCode: 200, Result: "disconnected"})
	result := manager.ListRequests(10, RequestFilter{Result: "aborted"})
	if len(result.Entries) != 1 {
		t.Fatalf("aborted entries = %+v", result.Entries)
	}
	// 完结后再 abort 应失败。
	if manager.Abort(dir) {
		t.Fatal("Abort after Complete should fail")
	}
}

// TestLayeredRetention 验证负载剥离与失败目录豁免。
func TestLayeredRetention(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{PayloadHours: 1, KeepErrorDirs: 1})
	defer manager.Close()

	// 一个 2 小时前的目录：负载应被剥离，证据保留。
	old := filepath.Join(root, "20200101-000000")
	for _, name := range []string{"03-devin-request.json", "04-devin-response.jsonl", "06-http-response.jsonl", "meta.json", "error.json"} {
		if err := os.MkdirAll(old, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(old, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(old, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "attachments", "a.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	manager.cleanOnce()
	for _, gone := range payloadNames {
		if _, err := os.Stat(filepath.Join(old, gone)); !os.IsNotExist(err) {
			t.Fatalf("payload %s should be stripped", gone)
		}
	}
	for _, keep := range []string{"meta.json", "error.json"} {
		if _, err := os.Stat(filepath.Join(old, keep)); err != nil {
			t.Fatalf("evidence %s should remain: %v", keep, err)
		}
	}
}

// usageFixture 构造带全部 token 字段的 Usage。
func usageFixture(input, output, cacheRead, cacheWrite int64, reasoning *int64, total int64) llm.Usage {
	return llm.Usage{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning, TotalTokens: total}
}
