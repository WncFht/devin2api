// 本文件验证请求日志行的字段口径：error_* 只在终结性失败时落行、
// 连接画像随成功建流出账。写路径是 Complete→logRowFor→批量事务→logs 表。
package debuglog

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// TestIndexErrorFields 验证 error_stage/error_message 只在终结性失败时
// 落日志行：被重试救回的中间错误留在目录 error.json，不污染按失败点
// 检索的口径；失败请求的 error_message 与 error.json 同源且被截断。
func TestIndexErrorFields(t *testing.T) {
	root := t.TempDir()
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	failed := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	failed.WriteError(ErrStageRateGate, errors.New(strings.Repeat("rate limited ", 40)))
	failed.Complete(Completion{StatusCode: 429, Result: "failed"})
	waitDrained(failed)

	recovered := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	recovered.WriteError(ErrStageDevinTransport, errors.New("mid-flight EOF"))
	recovered.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recovered)

	// 行序新在前：rows[0] 是被救回的请求，rows[1] 是终结性失败。
	rows, total, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if total != 2 || len(rows) != 2 {
		t.Fatalf("expected 2 log rows, got %d (total %d)", len(rows), total)
	}
	okEntry, failEntry := rows[0], rows[1]
	if failEntry.ErrorStage != ErrStageRateGate {
		t.Fatalf("failEntry.ErrorStage = %q, want %q", failEntry.ErrorStage, ErrStageRateGate)
	}
	if !strings.HasPrefix(failEntry.ErrorMessage, "rate limited") || len(failEntry.ErrorMessage) > errorMessageCap {
		t.Fatalf("failEntry.ErrorMessage = %q (len %d), want truncated prefix of original", failEntry.ErrorMessage, len(failEntry.ErrorMessage))
	}
	if okEntry.ErrorStage != "" || okEntry.ErrorMessage != "" {
		t.Fatalf("recovered request carried error fields: stage=%q message=%q", okEntry.ErrorStage, okEntry.ErrorMessage)
	}
	// error.json 仍应存在于两个目录：恢复证据不随日志行口径删减。
	for _, dir := range []string{failEntry.Dir, okEntry.Dir} {
		if _, _, _, err := manager.ReadFile(context.Background(), dir, ErrorFile); err != nil {
			t.Fatalf("error.json missing in %s: %v", dir, err)
		}
	}
}

// TestIndexConnReuseFields 验证成功建流的连接画像落进日志行；第二次
// NoteUpstreamConn（续轮重开/搜索扇出的后续建流）不得覆盖首个建流的
// 画像——sent→open 段延迟归因的是首个建流。
func TestIndexConnReuseFields(t *testing.T) {
	root := t.TempDir()
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	recorder.NoteUpstreamConn(true, 42*time.Millisecond)
	recorder.NoteUpstreamConn(false, 999*time.Millisecond)
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)

	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(rows))
	}
	entry := rows[0]
	if entry.ConnReused == nil || !*entry.ConnReused {
		t.Fatalf("ConnReused = %v, want true", entry.ConnReused)
	}
	if entry.ConnIdleMS == nil || *entry.ConnIdleMS != 42 {
		t.Fatalf("ConnIdleMS = %v, want 42", entry.ConnIdleMS)
	}
}

// TestUnclaimedCompletionRow 验证 claim 失败兜底行：dir 留空、log_source
// 归 proxy（请求真实执行过，不是管线前拒绝）、字段与 logRowFor 同口径；
// error_* 仍只对终结性失败出账；root 空/enabled 关（本就不会有目录的
// 请求）不落行。
func TestUnclaimedCompletionRow(t *testing.T) {
	root := t.TempDir()
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	startedAt := time.Now().Add(-1500 * time.Millisecond)
	meta := RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic",
		ClientIP: "10.0.0.1", KeyHash: "kh1", ClientRequestID: "crid-1"}
	manager.NoteUnclaimedCompletion(meta, Completion{
		StatusCode: 200, Result: "completed", Model: "m-a", RequestedModel: "m-a",
		Stream: true, Usage: llm.Usage{Input: 10, Output: 20, TotalTokens: 30},
	}, startedAt)
	manager.NoteUnclaimedCompletion(meta, Completion{
		StatusCode: 502, Result: "failed", ErrorStage: ErrStageDevinTransport,
		ErrorMessage: "upstream EOF", RateLimited: true,
	}, startedAt)
	// completed 行即便带上 ErrorStage 也不出账（与 logRowFor 同口径）。
	manager.NoteUnclaimedCompletion(meta, Completion{
		StatusCode: 200, Result: "completed", ErrorStage: ErrStageHTTPDecode,
		ErrorMessage: "rescued mid-flight decode",
	}, startedAt)
	// root 空/enabled 关是 Start 前置条件的镜像：本就不会有目录的请求
	// 不补行（protocensus 的 root="" manager、面板关掉日志开关期间）。
	disabled := NewManager(root, RetentionPolicy{}, st)
	disabled.SetEnabled(false)
	defer disabled.Close()
	disabled.NoteUnclaimedCompletion(meta, Completion{StatusCode: 200, Result: "completed"}, startedAt)
	rootless := NewManager("", RetentionPolicy{}, st)
	defer rootless.Close()
	rootless.NoteUnclaimedCompletion(meta, Completion{StatusCode: 200, Result: "completed"}, startedAt)

	rows, total, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if total != 3 || len(rows) != 3 {
		t.Fatalf("expected 3 log rows, got %d (total %d)", len(rows), total)
	}
	// 行序新在前：rows[0] 是带 ErrorStage 的 completed 行（应被压制），
	// rows[1] 是失败行，rows[2] 是首个 completed 行。
	for i, entry := range rows {
		if entry.Dir != "" {
			t.Fatalf("rows[%d].Dir = %q, want ''", i, entry.Dir)
		}
		if entry.LogSource != LogSourceProxy {
			t.Fatalf("rows[%d].LogSource = %q, want %q", i, entry.LogSource, LogSourceProxy)
		}
	}
	suppressed, failed, ok := rows[0], rows[1], rows[2]
	if failed.ErrorStage != ErrStageDevinTransport || failed.ErrorMessage != "upstream EOF" {
		t.Fatalf("failed row error fields = %q/%q", failed.ErrorStage, failed.ErrorMessage)
	}
	if !failed.RateLimited {
		t.Fatalf("failed row RateLimited = false, want true")
	}
	if suppressed.ErrorStage != "" || suppressed.ErrorMessage != "" {
		t.Fatalf("completed row carried error fields: %q/%q", suppressed.ErrorStage, suppressed.ErrorMessage)
	}
	if ok.InputTokens != 10 || ok.OutputTokens != 20 || ok.TotalTokens != 30 {
		t.Fatalf("completed row tokens = %d/%d/%d", ok.InputTokens, ok.OutputTokens, ok.TotalTokens)
	}
	if ok.DurationMS < 1000 {
		t.Fatalf("completed row DurationMS = %d, want >= 1000", ok.DurationMS)
	}
	// 面板探活走 manual_test 分域——claim 失败不改变来源归类。
	manager.NoteUnclaimedCompletion(RequestMeta{Method: "POST", Path: "/v1/messages",
		ClientRequestID: ProbeClientRequestID}, Completion{StatusCode: 200, Result: "completed"}, startedAt)
	probeRows, _, err := st.SearchLogs(context.Background(), store.LogQuery{LogSource: LogSourceManualTest})
	if err != nil {
		t.Fatalf("SearchLogs manual_test: %v", err)
	}
	if len(probeRows) != 1 || probeRows[0].Dir != "" {
		t.Fatalf("manual_test rows = %+v", probeRows)
	}
}

// TestIndexSwitchCauses 验证被放弃 lane 尝试经 NoteAccountAttempt→
// logRowFor→批量事务落进 lane_attempt_causes：local_gate[:reason] 是
// 本地闸门幻影换号（零上游发送——Code 同样 resource_exhausted 时靠
// LocalGate 分），connect code 是真实 failover 发送，无 code 的
// 传输断裂归 nocode。
func TestIndexSwitchCauses(t *testing.T) {
	root := t.TempDir()
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	recorder.NoteAccountAttempt("yanjian", &llm.Failure{
		Code: "resource_exhausted", LocalGate: true, GateReason: "latch"})
	recorder.NoteAccountAttempt("yanjian", &llm.Failure{
		Code: "resource_exhausted", Message: "upstream 429"})
	recorder.NoteAccountAttempt("randall", errors.New("connection reset by peer"))
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	waitDrained(recorder)

	day := time.Now().Local().Format("2006-01-02")
	got, err := st.LaneAttemptCauses(context.Background(), day)
	if err != nil {
		t.Fatalf("LaneAttemptCauses: %v", err)
	}
	want := []store.LaneAttemptCause{
		{Date: day, Lane: "randall", Cause: "nocode", N: 1},
		{Date: day, Lane: "yanjian", Cause: "local_gate:latch", N: 1},
		{Date: day, Lane: "yanjian", Cause: "resource_exhausted", N: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("causes = %+v, want %+v", got, want)
	}
}
