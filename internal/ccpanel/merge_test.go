package ccpanel

import (
	"strings"
	"testing"
)

// TestMergeResponseBodyDevinFrameEnvelope 的测试动机是钉住 04 帧行两种
// 存储形态：信封行（{seq,event:"frame",data:{protojson}}）与旧格式裸
// protojson 行都要被 collectDevinFrame 认出，簿记行跳过。
func TestMergeResponseBodyDevinFrameEnvelope(t *testing.T) {
	bodies := map[string]string{
		"envelope": `{"seq":1,"time":"2026-09-18T12:00:00Z","elapsed_ms":3,"event":"frame","data":{"messageId":"m","deltaText":"hello "}}` + "\n" +
			`{"seq":2,"time":"2026-09-18T12:00:00Z","elapsed_ms":9,"event":"frame","data":{"deltaText":"world"}}` + "\n" +
			`{"seq":3,"time":"2026-09-18T12:00:01Z","elapsed_ms":900,"event":"retry_attempt","data":{"attempt":2,"elapsed_ms":900}}`,
		"bare": `{"messageId":"m","deltaText":"hello "}` + "\n" +
			`{"deltaText":"world"}` + "\n" +
			`{"seq":3,"time":"2026-09-18T12:00:01Z","elapsed_ms":900,"event":"retry_attempt","data":{"attempt":2}}`,
	}
	for name, body := range bodies {
		parts := mergeResponseBody(body)
		if parts.Content != "hello world" {
			t.Fatalf("%s: content = %q, want %q", name, parts.Content, "hello world")
		}
	}
}

// TestMergeResponseBodyDevinToolCallsInEnvelope 的测试动机是钉住信封内
// deltaToolCalls 的 per-index 重组：name 帧与 argumentsJson 分块跨行
// 拼接不因信封丢失。
func TestMergeResponseBodyDevinToolCallsInEnvelope(t *testing.T) {
	body := `{"seq":1,"time":"t","elapsed_ms":1,"event":"frame","data":{"deltaToolCalls":[{"id":"c0","name":"shell","argumentsJson":"{\"cmd\":"}]}}` + "\n" +
		`{"seq":2,"time":"t","elapsed_ms":2,"event":"frame","data":{"deltaToolCalls":[{"argumentsJson":"\"ls\"}"}]}}`
	parts := mergeResponseBody(body)
	if !strings.Contains(parts.Tools, "ls") || !strings.Contains(parts.Tools, "```bash") {
		t.Fatalf("tools = %q, want shell call with reassembled args", parts.Tools)
	}
}
