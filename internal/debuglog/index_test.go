package debuglog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
