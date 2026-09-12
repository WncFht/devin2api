// 本文件验证调试日志的目录隔离、JSONL 顺序、脱敏和附件落盘策略。
package debuglog

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRecorderWritesRedactedStagesAndAttachments 的测试动机是防止诊断日志泄露凭据或重复嵌入大图片。
func TestRecorderWritesRedactedStagesAndAttachments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	recorder := NewManager(root, 0, 0).Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	if recorder == nil {
		t.Fatal("Start() = nil")
	}
	image := base64.StdEncoding.EncodeToString([]byte("png-data"))
	recorder.WriteJSON("01-http-request.json", map[string]any{
		"authorization": "secret",
		"body": map[string]any{
			"image": map[string]any{"mime_type": "image/png", "data": image},
		},
	})
	recorder.WriteJSON("03-devin-request.json", map[string]any{"metadata": map[string]any{"api_key": "secret", "f": "fingerprint"}})
	recorder.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"delta_text": "a"})
	recorder.AppendJSONL("04-devin-response.jsonl", "message", map[string]any{"delta_text": "b"})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "model", Provider: "devin", Stream: true})

	httpLog := readTestFile(t, filepath.Join(recorder.directory, "01-http-request.json"))
	if strings.Contains(httpLog, "secret") || strings.Contains(httpLog, image) {
		t.Fatalf("request log contains a secret or inline image: %s", httpLog)
	}
	if !strings.Contains(httpLog, `"file": "attachments/image-001.png"`) {
		t.Fatalf("request log has no attachment reference: %s", httpLog)
	}
	devinLog := readTestFile(t, filepath.Join(recorder.directory, "03-devin-request.json"))
	if strings.Contains(devinLog, "secret") || strings.Contains(devinLog, "fingerprint") {
		t.Fatalf("Devin request log contains credentials: %s", devinLog)
	}
	lines := strings.Split(strings.TrimSpace(readTestFile(t, filepath.Join(recorder.directory, "04-devin-response.jsonl"))), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"seq":1`) || !strings.Contains(lines[1], `"seq":2`) {
		t.Fatalf("JSONL sequence = %q", lines)
	}
	if got := string(readTestBytes(t, filepath.Join(recorder.directory, "attachments", "image-001.png"))); got != "png-data" {
		t.Fatalf("attachment = %q, want png-data", got)
	}
	meta := readTestFile(t, filepath.Join(recorder.directory, "meta.json"))
	if !strings.Contains(meta, `"provider": "devin"`) || !strings.Contains(meta, `"result": "completed"`) {
		t.Fatalf("meta = %s", meta)
	}
}

// TestManagerAllocatesCollisionSuffix 的测试动机是保证同秒并发请求不会共写同一个目录。
func TestManagerAllocatesCollisionSuffix(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), 0, 0)
	manager.now = func() time.Time { return time.Date(2027, time.January, 1, 23, 54, 54, 0, time.Local) }
	first := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	second := manager.Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	if first == nil || second == nil {
		t.Fatal("Start() returned nil")
	}
	if first.directory == second.directory {
		t.Fatalf("request directories are equal: %s", first.directory)
	}
	if !strings.HasSuffix(second.directory, "-02") {
		t.Fatalf("second directory = %q, want -02 suffix", second.directory)
	}
}

// TestWriteErrorKeepsFirstCause 的测试动机是让最接近故障源的阶段不被外层通用错误覆盖。
func TestWriteErrorKeepsFirstCause(t *testing.T) {
	recorder := NewManager(filepath.Join(t.TempDir(), "logs"), 0, 0).Start(RequestMeta{})
	recorder.WriteError("devin_connect", os.ErrPermission)
	recorder.WriteError("provider_stream", os.ErrNotExist)
	// 写任务经队列异步执行，Complete 排空后才能读到文件。
	recorder.Complete(Completion{StatusCode: 500, Result: "failed"})
	log := readTestFile(t, filepath.Join(recorder.directory, "error.json"))
	if !strings.Contains(log, "devin_connect") || strings.Contains(log, "provider_stream") {
		t.Fatalf("error log = %s", log)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	return string(readTestBytes(t, path))
}

func readTestBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestIndexWrittenOnComplete 验证全局索引每完成一个请求追加一行可定位摘要。
func TestIndexWrittenOnComplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, 0, 0)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", ClientIP: "127.0.0.1", KeyHash: "abcd1234"})
	recorder.NoteUpstreamLatency()
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "swe-2-max", RequestedModel: "swe-2", ResponseModel: "swe-2-max", Stream: true, UpstreamRequestID: "req-1"})
	index := readTestFile(t, filepath.Join(root, "index.jsonl"))
	for _, want := range []string{`"dir":`, `"api":"anthropic"`, `"requested_model":"swe-2"`, `"response_model":"swe-2-max"`, `"upstream_request_id":"req-1"`, `"key_hash":"abcd1234"`, `"first_upstream_ms"`} {
		if !strings.Contains(index, want) {
			t.Fatalf("index.jsonl missing %s: %s", want, index)
		}
	}
}

// TestCleanerRemovesExpiredDirs 验证清理器删除超龄目录、跳过活跃目录、不碰索引文件。
func TestCleanerRemovesExpiredDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, 7, 0)
	defer manager.Close()

	old := filepath.Join(root, "20200101-000000")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "meta.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	active := manager.Start(RequestMeta{Method: "POST", Path: "/x"})

	if removed := manager.cleanOnce(); removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("expired dir should be deleted")
	}
	if _, err := os.Stat(active.directory); err != nil {
		t.Fatal("active dir must be protected")
	}
	active.Complete(Completion{StatusCode: 200, Result: "completed"})
}

// TestDroppedCounterOnClosedQueue 验证 Complete 之后的写入被丢弃并计数。
func TestDroppedCounterOnClosedQueue(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, 0, 0)
	recorder := manager.Start(RequestMeta{})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	recorder.WriteJSON("late.json", map[string]any{"x": 1})
	if recorder.dropped.Load() != 1 {
		t.Fatalf("dropped = %d, want 1", recorder.dropped.Load())
	}
}

// TestReaderListDetailAndFiles 验证索引倒读、单请求详情与文件读取接口。
func TestReaderListDetailAndFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, 0, 0)
	defer manager.Close()
	for _, model := range []string{"m-a", "m-b"} {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
		recorder.WriteJSON("03-devin-request.json", map[string]any{"model": model})
		recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: model})
	}
	entries := manager.ListRequests(10)
	if len(entries) != 2 || entries[0].Model != "m-b" || entries[1].Model != "m-a" {
		t.Fatalf("ListRequests order = %+v", entries)
	}
	detail, err := manager.Detail(entries[0].Dir)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	if len(detail.Meta) == 0 || !strings.Contains(string(detail.Meta), `"m-b"`) {
		t.Fatalf("detail meta = %s", detail.Meta)
	}
	var names []string
	for _, f := range detail.Files {
		names = append(names, f.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "meta.json") || !strings.Contains(strings.Join(names, ","), "03-devin-request.json") {
		t.Fatalf("files = %v", names)
	}
	data, total, truncated, err := manager.ReadFile(entries[0].Dir, "03-devin-request.json")
	if err != nil || truncated || total == 0 || !strings.Contains(string(data), "m-b") {
		t.Fatalf("ReadFile = %q total=%d truncated=%v err=%v", data, total, truncated, err)
	}
}

// TestReaderRejectsTraversal 验证目录名与文件名的路径穿越防护。
func TestReaderRejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, 0, 0)
	defer manager.Close()
	recorder := manager.Start(RequestMeta{})
	dir := filepath.Base(recorder.directory)
	recorder.Complete(Completion{StatusCode: 200})
	if _, err := manager.Detail("../etc"); err == nil {
		t.Fatal("Detail should reject traversal")
	}
	for _, bad := range []string{"../meta.json", "meta.json/../x", "/abs", "sub/dir/x.json"} {
		if _, _, _, err := manager.ReadFile(dir, bad); err == nil {
			t.Fatalf("ReadFile should reject %q", bad)
		}
	}
	if _, _, _, err := manager.ReadFile(dir, "meta.json"); err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
}

// TestActiveRequestsSnapshot 验证进行中请求的活快照在 Complete 后消失。
func TestActiveRequestsSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, 0, 0)
	defer manager.Close()
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
	active := manager.ActiveRequests()
	if len(active) != 1 || active[0].Meta.API != "anthropic" {
		t.Fatalf("ActiveRequests = %+v", active)
	}
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	if got := manager.ActiveRequests(); len(got) != 0 {
		t.Fatalf("ActiveRequests after Complete = %+v", got)
	}
}
