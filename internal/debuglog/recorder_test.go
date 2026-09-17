// 本文件验证调试日志的目录隔离、JSONL 顺序、脱敏和附件落盘策略。
package debuglog

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// TestRecorderWritesRedactedStagesAndAttachments 的测试动机是防止诊断日志泄露凭据或重复嵌入大图片。
func TestRecorderWritesRedactedStagesAndAttachments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	recorder := NewManager(root, RetentionPolicy{}, nil).Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
	if recorder == nil {
		t.Fatal("Start() = nil")
	}
	image := base64.StdEncoding.EncodeToString([]byte("png-data"))
	recorder.WriteJSON("01-http-request.json", map[string]any{
		"authorization": "secret",
		// "f" 是客户端负载里的普通短键名——只有上游 metadata.f 指纹该脱敏。
		"f": "client-field",
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
	if !strings.Contains(httpLog, `"f": "client-field"`) {
		t.Fatalf("non-metadata \"f\" key was over-redacted: %s", httpLog)
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
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil)
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
	recorder := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil).Start(RequestMeta{})
	recorder.WriteError("devin_connect", os.ErrPermission)
	recorder.WriteError("provider_stream", os.ErrNotExist)
	// 写任务经队列异步执行，Complete 排空后才能读到文件。
	recorder.Complete(Completion{StatusCode: 500, Result: "failed"})
	log := readTestFile(t, filepath.Join(recorder.directory, "error.json"))
	if !strings.Contains(log, "devin_connect") || strings.Contains(log, "provider_stream") {
		t.Fatalf("error log = %s", log)
	}
}

// TestSameSecondSuffixBeyondPattern 验证同秒第 100+ 个请求的目录名仍被
// 读取面接受：%02d 后缀位数不设上限，三位数后缀不得被 requestDirPattern 拒绝。
func TestSameSecondSuffixBeyondPattern(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil)
	manager.now = func() time.Time { return time.Date(2027, time.January, 1, 23, 54, 54, 0, time.Local) }
	var recorders []*Recorder
	for i := 0; i < 105; i++ {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/x"})
		if recorder == nil {
			t.Fatalf("request %d: Start returned nil", i)
		}
		recorders = append(recorders, recorder)
	}
	dir := filepath.Base(recorders[104].directory)
	if !strings.HasSuffix(dir, "-105") {
		t.Fatalf("dir = %q, want -105 suffix", dir)
	}
	for _, recorder := range recorders {
		recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	}
	if _, err := manager.Detail(dir); err != nil {
		t.Fatalf("Detail(%q): %v", dir, err)
	}
	if _, _, _, err := manager.ReadFile(dir, "meta.json"); err != nil {
		t.Fatalf("ReadFile(%q): %v", dir, err)
	}
}

// TestSanitizeEscapedAndHyphenatedKeys 验证预筛无法按字节判别的敏感键名
// ——JSON 转义拼写的键名与连字符变体——仍被完整脱敏，不以原文落盘。
func TestSanitizeEscapedAndHyphenatedKeys(t *testing.T) {
	recorder := NewManager(filepath.Join(t.TempDir(), "logs"), RetentionPolicy{}, nil).Start(RequestMeta{})
	// 键名用 JSON \u 转义拼写：字节预筛看到的不是解码后的 "apikey"，
	// 必须靠「键名含转义→慢路径」兜底，否则原文落盘。
	escapedKey := `{"api` + "\\u006b" + `ey":"secret","plain":1}`
	recorder.WriteJSON("01-http-request.json", json.RawMessage(escapedKey))
	recorder.WriteJSON("03-devin-request.json", json.RawMessage(`{"api-key":"secret","set-cookie":"secret","keep":"ok"}`))
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	for _, name := range []string{"01-http-request.json", "03-devin-request.json"} {
		log := readTestFile(t, filepath.Join(recorder.directory, name))
		if strings.Contains(log, "secret") {
			t.Fatalf("%s contains unredacted credentials: %s", name, log)
		}
	}
}

// TestIOErrorsCountedOncePerKind 验证 worker 内写盘失败计入 ioErrors，
// 且同目录同类别失败只记一笔。
func TestIOErrorsCountedOncePerKind(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{}, nil)
	defer manager.Close()
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/x"})
	// 目录消失后所有文件写与 JSONL 打开都会失败：
	// file 类（writeMeta/01/03/meta）一笔、jsonl 类（04 open）一笔。
	if err := os.RemoveAll(recorder.DirectoryPath()); err != nil {
		t.Fatal(err)
	}
	recorder.WriteJSON("01-http-request.json", map[string]any{"x": 1})
	recorder.WriteJSON("03-devin-request.json", map[string]any{"x": 2})
	recorder.AppendJSONL("04-devin-response.jsonl", "e", map[string]any{"x": 1})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	if got := manager.Stats()["io_errors"]; got != uint64(2) {
		t.Fatalf("io_errors = %v, want 2（file/jsonl 各一笔）", got)
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

// TestIndexWrittenOnComplete 验证每完成一个请求向 logs 表落一行可定位摘要。
func TestIndexWrittenOnComplete(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic", ClientIP: "127.0.0.1", KeyHash: "abcd1234"})
	recorder.NoteUpstreamLatency()
	recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: "swe-2-max", RequestedModel: "swe-2", ResponseModel: "swe-2-max", Stream: true, UpstreamRequestID: "req-1"})
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 log row, got %d", len(rows))
	}
	row := rows[0]
	if row.Dir == "" || row.API != "anthropic" || row.RequestedModel != "swe-2" ||
		row.ResponseModel != "swe-2-max" || row.UpstreamRequestID != "req-1" ||
		row.KeyHash != "abcd1234" || row.FirstUpstreamMS == nil {
		t.Fatalf("log row missing fields: %+v", row)
	}
}

// TestCleanerRemovesExpiredDirs 验证清理器删除超龄目录、跳过活跃目录、不碰索引文件。
func TestCleanerRemovesExpiredDirs(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{Days: 7}, nil)
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
	manager := NewManager(root, RetentionPolicy{}, nil)
	recorder := manager.Start(RequestMeta{})
	recorder.Complete(Completion{StatusCode: 200, Result: "completed"})
	recorder.WriteJSON("late.json", map[string]any{"x": 1})
	if recorder.dropped.Load() != 1 {
		t.Fatalf("dropped = %d, want 1", recorder.dropped.Load())
	}
}

// TestDroppedCounterOnFullQueue 验证队列打满时 enqueue 走 default 丢弃：
// 不起写 worker，让 tasks 永远排满——第 writeQueueSize+1 个任务必掉。
func TestDroppedCounterOnFullQueue(t *testing.T) {
	recorder := &Recorder{tasks: make(chan writeTask, writeQueueSize)}
	task := writeTask(func() {})
	for i := 0; i < writeQueueSize; i++ {
		recorder.enqueue(task)
	}
	if got := recorder.dropped.Load(); got != 0 {
		t.Fatalf("dropped = %d after filling queue, want 0", got)
	}
	recorder.enqueue(task)
	if got := recorder.dropped.Load(); got != 1 {
		t.Fatalf("dropped = %d after overflow, want 1", got)
	}
}

// TestReaderListDetailAndFiles 验证日志行倒读、单请求详情与文件读取接口。
func TestReaderListDetailAndFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	st := openTestStore(t)
	manager := NewManager(root, RetentionPolicy{}, st)
	defer manager.Close()
	for _, model := range []string{"m-a", "m-b"} {
		recorder := manager.Start(RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
		recorder.WriteJSON("03-devin-request.json", map[string]any{"model": model})
		recorder.Complete(Completion{StatusCode: 200, Result: "completed", Model: model})
	}
	rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
	if err != nil {
		t.Fatalf("SearchLogs: %v", err)
	}
	if len(rows) != 2 || rows[0].Model != "m-b" || rows[1].Model != "m-a" {
		t.Fatalf("SearchLogs order = %+v", rows)
	}
	detail, err := manager.Detail(rows[0].Dir)
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
	data, total, truncated, err := manager.ReadFile(rows[0].Dir, "03-devin-request.json")
	if err != nil || truncated || total == 0 || !strings.Contains(string(data), "m-b") {
		t.Fatalf("ReadFile = %q total=%d truncated=%v err=%v", data, total, truncated, err)
	}
}

// TestReaderRejectsTraversal 验证目录名与文件名的路径穿越防护。
func TestReaderRejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := NewManager(root, RetentionPolicy{}, nil)
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
	manager := NewManager(root, RetentionPolicy{}, nil)
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
