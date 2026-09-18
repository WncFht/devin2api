// 本文件验证请求完成→logs 表行的端到端写路径（聚合从 store 读回）、
// 失败责任归因、保留策略与运行时开关；纯 SQL 筛选/聚合口径的
// 单测在 internal/store/logs_usage_test.go。
package debuglog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// openTestStore 开一个临时 sqlite 库；断言日志行的测试把它接进
// NewManager 第三参，写完经 SearchLogs/UsageStats 读回验证。
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// TestUsageStatsFromLogRows 验证一次请求完成后各维度都被计入快照。
func TestUsageStatsFromLogRows(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	reasoning := int64(7)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", KeyHash: "k1"})
	recorder.Complete(Completion{
		StatusCode: 200, Result: "completed", Model: "swe-2-max", RequestedModel: "swe-2",
		Usage: usageFixture(100, 50, 40, 30, &reasoning, 187),
	})
	waitDrained(recorder)
	failed := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", KeyHash: "k1"})
	failed.WriteError("provider_stream", os.ErrNotExist)
	failed.Complete(Completion{StatusCode: 500, Result: "failed", Model: "swe-2-max", Usage: usageFixture(10, 0, 0, 0, nil, 10)})
	waitDrained(failed)

	snap, err := st.UsageStats(context.Background())
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
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
	if len(snap.Points) == 0 {
		t.Fatal("points empty")
	}
	if snap.Duration.Samples != 2 {
		t.Fatalf("duration stats = %+v", snap.Duration)
	}
}

// TestUsagePersistedAcrossManagers 验证重建 Manager 后历史统计不丢——
// 日志行在 logs 表里，与进程内存无关。
func TestUsagePersistedAcrossManagers(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	first := NewManager(root, RetentionPolicy{}, st)
	recorder := first.Start(RequestMeta{Method: "POST", Path: "/v1/chat/completions", API: "openai-chat"})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "glm-5-2", Usage: usageFixture(1000, 200, 0, 0, nil, 1200)})
	waitDrained(recorder)
	first.Close()

	second := NewManager(root, RetentionPolicy{}, st)
	defer second.Close()
	snap, err := st.UsageStats(context.Background())
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if snap.Window.Requests != 1 || snap.Window.InputTokens != 1000 || snap.Window.TotalTokens != 1200 {
		t.Fatalf("persisted window = %+v", snap.Window)
	}
	if len(snap.Models) != 1 || snap.Models[0].Name != "glm-5-2" {
		t.Fatalf("persisted models = %+v", snap.Models)
	}
	// 新请求继续累加，不重复计数。
	recorder2 := second.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
	recorder2.Complete(Completion{StatusCode: 200, Result: "completed", Model: "glm-5-2", Usage: usageFixture(5, 5, 0, 0, nil, 10)})
	waitDrained(recorder2)
	snap, err = st.UsageStats(context.Background())
	if err != nil {
		t.Fatalf("UsageStats: %v", err)
	}
	if snap.Window.Requests != 2 || snap.Window.TotalTokens != 1210 {
		t.Fatalf("post-restart window = %+v", snap.Window)
	}
}

// TestErrorOwner 验证失败责任归类：客户端责任（断连、请求体阶段失败）
// 与 429 限流不进 SLA 分母，只有服务端失分扣分。聚合口径的同名
// 断言在 store 包 TestUsageFaults（SQL 判定链与这里逐行对齐）。
func TestErrorOwner(t *testing.T) {
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
		if got := ErrorOwner(&store.LogRow{StatusCode: tc.status, Result: tc.result, ErrorStage: tc.stage}); got != tc.want {
			t.Fatalf("ErrorOwner(%d/%s/%s) = %q, want %q", tc.status, tc.result, tc.stage, got, tc.want)
		}
	}
}

// TestRetryAttemptsInIndex 验证重发计数随日志行与 meta 落盘：
// NoteRetryAttempt 与 04 的 retry_attempt 分界行同源。
func TestRetryAttemptsInIndex(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	dir := recorder.dir
	recorder.NoteRetryAttempt(2, "unauthenticated: token reloaded")
	recorder.NoteRetryAttempt(3, "transport: EOF")
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "m-x"})
	waitDrained(recorder)

	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 || rows[0].Retries != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	// meta.json 应带明细（attempt 号/原因/相对时刻），面板据此渲染链路。
	data, _, _, err := manager.ReadFile(dir, MetaFile)
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
	manager := NewManager(root, RetentionPolicy{}, nil)
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
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	dir := recorder.dir
	if manager.Abort(dir) {
		t.Fatal("Abort should fail before ctx is attached")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	recorder.SetAbort(cancel)

	active := manager.ActiveRequests()
	if len(active) != 1 || !active[0].Abortable || active[0].State != StateWaitingUpstream {
		t.Fatalf("active = %+v", active)
	}
	if !manager.Abort(dir) {
		t.Fatal("Abort returned false")
	}
	if ctx.Err() == nil {
		t.Fatal("ctx not cancelled by Abort")
	}
	recorder.Complete(Completion{StatusCode: 200, Result: "disconnected"})
	waitDrained(recorder)
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{Result: "aborted"})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("aborted rows = %+v", rows)
	}
	// 完结后再 abort 应失败。
	if manager.Abort(dir) {
		t.Fatal("Abort after Complete should fail")
	}
}

// TestLayeredRetention 验证负载剥离与失败目录豁免：行键名即年龄依据，
// 2020 年的目录名远超 1 小时负载保留界。
func TestLayeredRetention(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{PayloadHours: 1, KeepErrorDirs: 1}, st)
	defer manager.Close()

	// 重试分片 03-devin-request.attempt2.json 同属负载层，必须一并剥掉。
	ctx := context.Background()
	old := "20200101-000000"
	payloads := []string{
		"03-devin-request.json", "03-devin-request.attempt2.json",
		"04-devin-response.jsonl", "06-http-response.jsonl", "attachments/a.bin",
	}
	for _, name := range append(payloads, MetaFile, ErrorFile) {
		if err := st.PutDebugFile(ctx, old, name, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}

	manager.cleanOnce()
	names, err := st.DebugFileNames(ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	remaining := map[string]bool{}
	for _, name := range names {
		remaining[name] = true
	}
	for _, gone := range payloads {
		if remaining[gone] {
			t.Fatalf("payload %s should be stripped, remaining = %v", gone, names)
		}
	}
	for _, keep := range []string{MetaFile, ErrorFile} {
		if !remaining[keep] {
			t.Fatalf("evidence %s should remain, remaining = %v", keep, names)
		}
	}
}

// TestRetentionAgesByDirName 验证计龄以目录名内嵌时间戳为准：
// 行 updated_at 再新也不影响超龄目录的淘汰判定。
func TestRetentionAgesByDirName(t *testing.T) {
	st := openTestStore(t)
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{Days: 7, PayloadHours: 1}, st)
	defer manager.Close()

	ctx := context.Background()
	for _, name := range []string{"03-devin-request.json", MetaFile} {
		if err := st.PutDebugFile(ctx, "20200101-000000", name, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if removed := manager.cleanOnce(); removed != 1 {
		t.Fatalf("removed = %d, want 1（目录名说它是 2020 年，行新旧不算数）", removed)
	}
}

// usageFixture 构造带全部 token 字段的 Usage。
func usageFixture(input, output, cacheRead, cacheWrite int64, reasoning *int64, total int64) llm.Usage {
	return llm.Usage{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, Reasoning: reasoning, TotalTokens: total}
}
