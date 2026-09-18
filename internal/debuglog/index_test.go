// 本文件验证请求日志行的字段口径：error_* 只在终结性失败时落行、
// 连接画像随成功建流出账。写路径是 Complete→logRowFor→批量事务→logs 表。
package debuglog

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
		if _, _, _, err := manager.ReadFile(dir, ErrorFile); err != nil {
			t.Fatalf("error.json missing in %s: %v", dir, err)
		}
	}
}

// TestIndexConnReuseFields 验证成功建流的连接画像落进日志行。
func TestIndexConnReuseFields(t *testing.T) {
	root := t.TempDir()
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	recorder.NoteUpstreamConn(true, 42*time.Millisecond)
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
