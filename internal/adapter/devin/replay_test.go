// 本文件用「金帧」回放验证 responseDecoder：testdata/frames/*.jsonl 是
// 从真实抓取的 04-devin-response.jsonl 蒸馏的上游帧序列（内容截短、
// 信封结构与帧序保持原样），锁定「上游帧 → 中间事件」的语义不因
// 解码器改动而漂移。
package devin

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/WncFht/devin2api/internal/llm"
)

// replayFixture 把一个金帧文件逐行回放给解码器，返回含 start 与 finish
// 产物的完整事件序列。
func replayFixture(t *testing.T, name string, stopPatterns []string, customTools map[string]bool) []llm.ResponseEvent {
	t.Helper()
	file, err := os.Open(filepath.Join("testdata", "frames", name))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = file.Close() }()
	decoder := newResponseDecoder("swe-2-max", stopPatterns, customTools, nil)
	events := decoder.start()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	line := 0
	for scanner.Scan() {
		line++
		if len(strings.TrimSpace(scanner.Text())) == 0 {
			continue
		}
		frame := &devinproto.GetChatMessageResponse{}
		if err := protojson.Unmarshal(scanner.Bytes(), frame); err != nil {
			t.Fatalf("%s:%d protojson: %v", name, line, err)
		}
		events = append(events, decoder.decode(frame)...)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return append(events, decoder.finish(nil)...)
}

func eventTypes(events []llm.ResponseEvent) []llm.ResponseEventType {
	types := make([]llm.ResponseEventType, len(events))
	for index, event := range events {
		types[index] = event.Type
	}
	return types
}

func assertEventSequence(t *testing.T, events []llm.ResponseEvent, want ...llm.ResponseEventType) {
	t.Helper()
	got := eventTypes(events)
	if len(got) != len(want) {
		t.Fatalf("event count = %d %v, want %d %v", len(got), got, len(want), want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("event %d = %s, want %s (full sequence %v)", index, got[index], want[index], got)
		}
	}
}

func finalMessage(t *testing.T, events []llm.ResponseEvent) *llm.AssistantMessage {
	t.Helper()
	last := events[len(events)-1]
	if last.Type != llm.ResponseEventDone || last.Message == nil {
		t.Fatalf("last event = %s, want done with message", last.Type)
	}
	return last.Message
}

// 真实形态：前两帧是仅 usage 的元数据帧，工具调用 id+name 先行，
// argumentsJson 分片随后，stopReason=FUNCTION_CALL，尾巴是带 token 的
// usage 帧与 responseDimensionGroups 统计帧（源自 logs/20260913-080710）。
func TestGoldenFramesToolCallTurn(t *testing.T) {
	events := replayFixture(t, "tool-call.jsonl", nil, nil)
	assertEventSequence(t, events,
		llm.ResponseEventStart,
		llm.ResponseEventToolCallStart,
		llm.ResponseEventToolCallDelta,
		llm.ResponseEventToolCallDelta,
		llm.ResponseEventToolCallDelta,
		llm.ResponseEventToolCallEnd,
		llm.ResponseEventDone,
	)
	message := finalMessage(t, events)
	if message.StopReason != llm.StopReasonToolUse {
		t.Fatalf("stop = %s, want toolUse", message.StopReason)
	}
	if len(message.Content) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(message.Content))
	}
	call, ok := message.Content[0].(llm.ToolCall)
	if !ok {
		t.Fatalf("content[0] type = %T, want ToolCall", message.Content[0])
	}
	if call.ID != "Bash_75" || call.Name != "Bash" {
		t.Fatalf("tool call = %s/%s, want Bash_75/Bash", call.ID, call.Name)
	}
	var arguments map[string]string
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments["command"] != "ls -la" {
		t.Fatalf("arguments = %s, want {\"command\":\"ls -la\"}", call.Arguments)
	}
	if message.Usage.Input != 1618 || message.Usage.Output != 88 || message.Usage.CacheRead != 182211 {
		t.Fatalf("usage = %+v, want 1618/88/182211", message.Usage)
	}
	if message.ResponseModel != "swe-2-max" || message.UpstreamRequestID != "req-golden-t1" {
		t.Fatalf("identity = %s/%s", message.ResponseModel, message.UpstreamRequestID)
	}
}

// 真实形态：纯 deltaText 序列，STOP_PATTERN 收尾（上游自然 EOS），
// 尾部 usage 帧单独到达（源自 logs/20260913-080520）。
func TestGoldenFramesTextAnswerTurn(t *testing.T) {
	events := replayFixture(t, "text-answer.jsonl", nil, nil)
	assertEventSequence(t, events,
		llm.ResponseEventStart,
		llm.ResponseEventTextStart,
		llm.ResponseEventTextDelta,
		llm.ResponseEventTextDelta,
		llm.ResponseEventTextDelta,
		llm.ResponseEventTextEnd,
		llm.ResponseEventDone,
	)
	message := finalMessage(t, events)
	if message.StopReason != llm.StopReasonStop {
		t.Fatalf("stop = %s, want stop", message.StopReason)
	}
	text, ok := message.Content[0].(llm.TextContent)
	if !ok || text.Text != "All three checks passed — nothing left to do." {
		t.Fatalf("text = %#v", message.Content[0])
	}
	if message.Usage.Input != 1073 || message.Usage.CacheRead != 60118 {
		t.Fatalf("usage = %+v", message.Usage)
	}
}

// 真实形态：deltaThinking 在前、deltaText 随后，签名作为全部正文之后
// 的尾随帧到达（sealed 体制）——必须合并回已关闭的思考块而不是新开块
// （源自 logs/20260913-080520 的签名段）。
func TestGoldenFramesThinkingLateSignature(t *testing.T) {
	events := replayFixture(t, "thinking-late-signature.jsonl", nil, nil)
	assertEventSequence(t, events,
		llm.ResponseEventStart,
		llm.ResponseEventThinkingStart,
		llm.ResponseEventThinkingDelta,
		llm.ResponseEventThinkingDelta,
		llm.ResponseEventThinkingEnd,
		llm.ResponseEventTextStart,
		llm.ResponseEventTextDelta,
		llm.ResponseEventThinkingSignature,
		llm.ResponseEventTextEnd,
		llm.ResponseEventDone,
	)
	message := finalMessage(t, events)
	if len(message.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(message.Content))
	}
	thinking, ok := message.Content[0].(llm.ThinkingContent)
	if !ok {
		t.Fatalf("content[0] type = %T, want ThinkingContent", message.Content[0])
	}
	if thinking.Thinking != "Comparing the two code paths, the merged form wins." {
		t.Fatalf("thinking = %q", thinking.Thinking)
	}
	if thinking.ThinkingSignature != "sealed.v1.goldenfixturesignature" || thinking.SignatureType != "sealed" {
		t.Fatalf("signature = %q/%s", thinking.ThinkingSignature, thinking.SignatureType)
	}
	if text, ok := message.Content[1].(llm.TextContent); !ok || text.Text != "Verified: the merged history form fixes it." {
		t.Fatalf("text = %#v", message.Content[1])
	}
}
