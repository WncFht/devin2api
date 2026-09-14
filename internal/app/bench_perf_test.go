// 性能基准：端到端请求路径与 SSE/WS 编码热路径。
// 这些基准用假 adapter 隔离上游网络，测量的是本进程 CPU/分配开销。
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// benchAdapter 产生 N 个 text delta + 正常收尾的流。
type benchAdapter struct {
	deltaCount int
	deltaSize  int
}

func (a *benchAdapter) Stream(_ context.Context, _ llm.RequestMessages) (llm.ResponseStream, error) {
	partial := &llm.AssistantMessage{Model: "fake", API: "connect", Provider: "fake"}
	events := make([]llm.ResponseEvent, 0, a.deltaCount+4)
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventStart, Partial: partial})
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial})
	delta := strings.Repeat("x", a.deltaSize)
	for i := 0; i < a.deltaCount; i++ {
		events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: delta, Partial: partial})
	}
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: "done", Partial: partial})
	partial.StopReason = llm.StopReasonStop
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: partial})
	return &fakeStream{events: events}, nil
}

// benchBody 构造一个带若干历史轮的 chat 请求体。
func benchBody(messages int) []byte {
	msgs := make([]map[string]any, 0, messages+1)
	msgs = append(msgs, map[string]any{"role": "system", "content": strings.Repeat("You are a helpful assistant. ", 40)})
	for i := 0; i < messages; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": fmt.Sprintf("question %d: %s", i, strings.Repeat("context ", 64))},
			map[string]any{"role": "assistant", "content": fmt.Sprintf("answer %d: %s", i, strings.Repeat("detail ", 64))},
		)
	}
	body, _ := json.Marshal(map[string]any{"model": "fake", "messages": msgs, "stream": true})
	return body
}

// BenchmarkStreamEndToEnd 打满「HTTP → 解码 → 泵 → SSE 编码 → 写客户端」链路，
// debuglog 关闭。模拟 200 个 32B delta 的流。
func BenchmarkStreamEndToEnd(b *testing.B) {
	benchEndToEnd(b, false, 200, 32)
}

func BenchmarkStreamEndToEndDebugLog(b *testing.B) {
	benchEndToEnd(b, true, 200, 32)
}

func benchEndToEnd(b *testing.B, debugEnabled bool, deltaCount, deltaSize int) {
	// 基准输出要能被 benchstat 解析：请求级的 slog 行会插进基准行里，
	// 抬高级别静默（只影响本测试进程的日志阈值）。
	slog.SetLogLoggerLevel(slog.LevelError)
	var manager *debuglog.Manager
	if debugEnabled {
		manager = debuglog.NewManager(b.TempDir(), debuglog.RetentionPolicy{})
		defer manager.Close()
	} else {
		manager = debuglog.NewManager("", debuglog.RetentionPolicy{})
	}
	application := New(&benchAdapter{deltaCount: deltaCount, deltaSize: deltaSize}, config.ServerConfig{Listen: ":0", MaxConcurrency: 1024}, manager)
	server := httptest.NewServer(application.Router())
	defer server.Close()
	body := benchBody(10)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := http.Post(server.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("status = %d", resp.StatusCode)
		}
	}
}

// BenchmarkWSWriteFrame 测量 WS 路径每帧的 SSE→JSON 解析开销：
// 一次 Unmarshal 成字段树后 type/error.code/item 直取。
func BenchmarkWSWriteFrame(b *testing.B) {
	writer := &wsResponseWriter{outputItems: make(map[int64]json.RawMessage)}
	// 典型 delta 帧：event: 行 + data: 行 + \n\n 已由 Write 拆分，writeFrame 只吃帧体。
	frame, _ := json.Marshal(map[string]any{
		"type": "response.output_text.delta", "sequence_number": 7,
		"item_id": "msg_1", "output_index": 0, "content_index": 0,
		"delta": strings.Repeat("x", 64), "logprobs": []any{},
	})
	frame = append([]byte("event: response.output_text.delta\ndata: "), frame...)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// 用 nil conn 不可行——writeFrame 最终会 WriteMessage。
		// 所以直接测解析段：复制 writeFrame 的前半逻辑。
		var data []byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data: ")) {
				data = append(data, line[len("data: "):]...)
			}
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			b.Fatal("bad frame")
		}
		_ = wsRawString(fields["type"])
		_ = wsJSONString(fields["error"], "code")
	}
	_ = writer
}

// BenchmarkWSNormalizeTurn 测量续轮规范化的端到端成本：
// 400 条 item 的历史 + 增量 input 的解析、合并、去重、配对校验与 marshal。
func BenchmarkWSNormalizeTurn(b *testing.B) {
	// 200 条 item 的历史：交替 message 与 function_call/output 对。
	items := make([]json.RawMessage, 0, 400)
	for i := 0; i < 100; i++ {
		items = append(items,
			json.RawMessage(fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}`, strings.Repeat("u", 200))),
			json.RawMessage(fmt.Sprintf(`{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"tool_%d","arguments":"{}"}`, i, i, i)),
			json.RawMessage(fmt.Sprintf(`{"type":"function_call_output","call_id":"call_%d","output":%q}`, i, strings.Repeat("o", 300))),
			json.RawMessage(fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}`, strings.Repeat("a", 300))),
		)
	}
	lastInput, _ := json.Marshal(items)
	// 首轮：历史作为完整 input 建立会话状态。
	first, _ := json.Marshal(map[string]any{"type": "response.create", "input": json.RawMessage(lastInput), "model": "fake"})
	next, _ := json.Marshal(map[string]any{
		"type": "response.create", "previous_response_id": "resp_1",
		"input": json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`),
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		session := newWSSession()
		normalized, err := session.normalizeRequest(first)
		if err != nil {
			b.Fatal(err)
		}
		_ = normalized
		session.commit(wsTurnResult{completedOutput: json.RawMessage(`[]`), completedResponseID: "resp_1"})
		if _, err := session.normalizeRequest(next); err != nil {
			b.Fatal(err)
		}
	}
}

func (a *benchAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "fake"}}, nil
}
