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
	recorder := NewManager(root).Start(RequestMeta{Method: "POST", Path: "/v1/responses"})
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
	manager := NewManager(filepath.Join(t.TempDir(), "logs"))
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
	recorder := NewManager(filepath.Join(t.TempDir(), "logs")).Start(RequestMeta{})
	recorder.WriteError("devin_connect", os.ErrPermission)
	recorder.WriteError("provider_stream", os.ErrNotExist)
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
