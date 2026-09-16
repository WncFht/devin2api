package debuglog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestIndexFileCapTruncates 验证 index.jsonl 超限后保尾部重写：
// 文件收缩到上限一半以内、剩余行均为完整可解析的索引条目。
func TestIndexFileCapTruncates(t *testing.T) {
	root := t.TempDir()
	old := indexFileCap
	indexFileCap = 4 << 10 // 4KB：几十条摘要即可触发截断
	defer func() { indexFileCap = old }()

	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()
	for i := 0; i < 60; i++ {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
		if recorder == nil {
			t.Fatalf("request %d: Start returned nil", i)
		}
		recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	}
	info, err := os.Stat(filepath.Join(root, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > indexFileCap {
		t.Fatalf("index.jsonl size %d exceeds cap %d", info.Size(), indexFileCap)
	}
	data, err := os.ReadFile(filepath.Join(root, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || len(lines) >= 60 {
		t.Fatalf("expected a truncated tail of entries, got %d lines", len(lines))
	}
	for i, line := range lines {
		var entry IndexEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("line %d is a torn record: %v", i, err)
		}
	}
}

// TestIndexErrorFields 验证 error_stage/error_message 只在终结性失败时
// 落索引：被重试救回的中间错误留在目录 error.json，不污染按失败点
// 检索的口径；失败请求的 error_message 与 error.json 同源且被截断。
func TestIndexErrorFields(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	failed := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	failed.WriteError(ErrStageRateGate, errors.New(strings.Repeat("rate limited ", 40)))
	failed.Complete(Completion{StatusCode: 429, Result: "failed"})

	recovered := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	recovered.WriteError(ErrStageDevinTransport, errors.New("mid-flight EOF"))
	recovered.Complete(Completion{StatusCode: 200, Result: "completed"})

	data, err := os.ReadFile(filepath.Join(root, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 index lines, got %d", len(lines))
	}
	var failEntry, okEntry IndexEntry
	if err := json.Unmarshal([]byte(lines[0]), &failEntry); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &okEntry); err != nil {
		t.Fatal(err)
	}
	if failEntry.ErrorStage != ErrStageRateGate {
		t.Fatalf("failEntry.ErrorStage = %q, want %q", failEntry.ErrorStage, ErrStageRateGate)
	}
	if !strings.HasPrefix(failEntry.ErrorMessage, "rate limited") || len(failEntry.ErrorMessage) > errorMessageCap {
		t.Fatalf("failEntry.ErrorMessage = %q (len %d), want truncated prefix of original", failEntry.ErrorMessage, len(failEntry.ErrorMessage))
	}
	if okEntry.ErrorStage != "" || okEntry.ErrorMessage != "" {
		t.Fatalf("recovered request carried error fields: stage=%q message=%q", okEntry.ErrorStage, okEntry.ErrorMessage)
	}
	// error.json 仍应存在于两个目录：恢复证据不随索引口径删减。
	for _, dir := range []string{failEntry.Dir, okEntry.Dir} {
		if _, err := os.Stat(filepath.Join(root, dir, ErrorFile)); err != nil {
			t.Fatalf("error.json missing in %s: %v", dir, err)
		}
	}
}

// TestIndexConnReuseFields 验证成功建流的连接画像落进索引行。
func TestIndexConnReuseFields(t *testing.T) {
	root := t.TempDir()
	manager := NewManager(root, RetentionPolicy{})
	defer manager.Close()

	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages"})
	recorder.NoteUpstreamConn(true, 42*time.Millisecond)
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})

	data, err := os.ReadFile(filepath.Join(root, "index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var entry IndexEntry
	if err := json.Unmarshal(bytes.TrimSpace(data), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.ConnReused == nil || !*entry.ConnReused {
		t.Fatalf("ConnReused = %v, want true", entry.ConnReused)
	}
	if entry.ConnIdleMS == nil || *entry.ConnIdleMS != 42 {
		t.Fatalf("ConnIdleMS = %v, want 42", entry.ConnIdleMS)
	}
}
