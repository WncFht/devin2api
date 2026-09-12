// 本文件验证 SSE 帧合并：三种协议形态的增量提取。
package dashboard

import (
	"fmt"
	"strings"
	"testing"
)

// TestMergeChatChunks 验证 OpenAI chat.chunk 帧合并出正文与 usage。
func TestMergeChatChunks(t *testing.T) {
	var sb strings.Builder
	write := func(event, data string) {
		fmt.Fprintf(&sb, `{"seq":1,"event":%q,"data":%s}`+"\n", event, data)
	}
	write("chat.completion.chunk", `{"choices":[{"delta":{"content":"Hel"}}]}`)
	write("chat.completion.chunk", `{"choices":[{"delta":{"content":"lo"},"finish_reason":null}]}`)
	write("chat.completion.chunk", `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"total_tokens":12}}`)
	write("[DONE]", `"[DONE]"`)

	got := mergeStreamEvents([]byte(sb.String()))
	if got.Text != "Hello" {
		t.Fatalf("text = %q", got.Text)
	}
	if got.FinishReason != "stop" || got.Events != 4 {
		t.Fatalf("merged = %+v", got)
	}
	if !strings.Contains(string(got.Usage), "total_tokens") {
		t.Fatalf("usage = %s", got.Usage)
	}
}

// TestMergeResponsesAndAnthropic 验证 responses 与 anthropic 增量帧的提取。
func TestMergeResponsesAndAnthropic(t *testing.T) {
	var sb strings.Builder
	write := func(event, data string) {
		fmt.Fprintf(&sb, `{"seq":1,"event":%q,"data":%s}`+"\n", event, data)
	}
	write("response.reasoning_text.delta", `{"type":"response.reasoning_text.delta","delta":"think-"}`)
	write("response.output_text.delta", `{"type":"response.output_text.delta","delta":"ans"}`)
	write("response.completed", `{"type":"response.completed","response":{"usage":{"output_tokens":5}}}`)
	write("content_block_delta", `{"type":"content_block_delta","delta":{"type":"text_delta","text":"xyz"}}`)
	write("content_block_delta", `{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`)
	write("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`)

	got := mergeStreamEvents([]byte(sb.String()))
	if got.Text != "ansxyz" {
		t.Fatalf("text = %q", got.Text)
	}
	if got.Reasoning != "think-" {
		t.Fatalf("reasoning = %q", got.Reasoning)
	}
	if got.ToolInput != `{"a":` {
		t.Fatalf("tool_input = %q", got.ToolInput)
	}
	if got.FinishReason != "end_turn" {
		t.Fatalf("finish = %q", got.FinishReason)
	}
}
