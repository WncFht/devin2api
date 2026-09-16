// 本文件验证 /v1/responses WebSocket transport 的多轮语义：
// previous_response_id 续链合并、错误事件分类、预热帧与中断替换回放。
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/llm"
)

// wsScriptAdapter 按调用顺序回放每轮的预定事件流，并记录每次收到的
// RequestMessages，供断言「合并后的 transcript 形态」。
type wsScriptAdapter struct {
	mu        sync.Mutex
	scripts   [][]llm.ResponseEvent
	requests  []llm.RequestMessages
	callCount int
}

func (fake *wsScriptAdapter) Stream(_ context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	index := fake.callCount
	fake.callCount++
	fake.requests = append(fake.requests, request)
	if index >= len(fake.scripts) {
		return nil, fmt.Errorf("wsScriptAdapter: unexpected call %d", index)
	}
	return &fakeStream{events: fake.scripts[index]}, nil
}

func (fake *wsScriptAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "gpt-test", Created: 1, OwnedBy: "test"}}, nil
}

func (fake *wsScriptAdapter) recordedRequests() []llm.RequestMessages {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]llm.RequestMessages(nil), fake.requests...)
}

// wsTextTurnScript 生成一轮纯文本回复的事件脚本。
func wsTextTurnScript(text string) []llm.ResponseEvent {
	partial := &llm.AssistantMessage{
		Content:    []llm.Content{llm.TextContent{Text: text}},
		StopReason: llm.StopReasonPending,
	}
	return []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: text, Partial: partial},
		{Type: llm.ResponseEventTextEnd, ContentIndex: 0, Content: text, Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: &llm.AssistantMessage{
			ResponseID:    "resp-turn",
			ResponseModel: "gpt-test",
			Content:       []llm.Content{llm.TextContent{Text: text}},
			StopReason:    llm.StopReasonStop,
		}},
	}
}

// wsToolCallTurnScript 生成一轮以 function_call 收尾的事件脚本。
func wsToolCallTurnScript(callID, name, arguments string) []llm.ResponseEvent {
	call := &llm.ToolCall{ID: callID, Name: name, Arguments: json.RawMessage(arguments)}
	partial := &llm.AssistantMessage{
		Content:    []llm.Content{*call},
		StopReason: llm.StopReasonPending,
	}
	return []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
		{Type: llm.ResponseEventToolCallStart, ContentIndex: 0, ToolCallID: callID, ToolName: name, Partial: partial},
		{Type: llm.ResponseEventToolCallDelta, ContentIndex: 0, Delta: arguments, ToolCallID: callID, Partial: partial},
		{Type: llm.ResponseEventToolCallEnd, ContentIndex: 0, ToolCall: call, Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonToolUse, Message: &llm.AssistantMessage{
			ResponseID:    "resp-tool",
			ResponseModel: "gpt-test",
			Content:       []llm.Content{*call},
			StopReason:    llm.StopReasonToolUse,
		}},
	}
}

// dialWS 连上测试服务的 WS 端点并注册清理。
func dialWS(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(url, http.Header{})
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		t.Fatalf("dial websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// wsWriteJSON 发送一帧 JSON 文本消息。
func wsWriteJSON(t *testing.T, conn *websocket.Conn, payload any) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal ws payload: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write ws message: %v", err)
	}
}

// wsReadEvent 读取下一条文本帧并按 JSON 解析；带读超时防挂死。
func wsReadEvent(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	messageType, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read ws message: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("ws message type = %d, want text", messageType)
	}
	var event map[string]any
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatalf("decode ws event: %v (%s)", err, data)
	}
	return event
}

// wsReadUntil 读到指定事件类型为止，返回该事件。
func wsReadUntil(t *testing.T, conn *websocket.Conn, eventTypes ...string) map[string]any {
	t.Helper()
	want := make(map[string]bool, len(eventTypes))
	for _, eventType := range eventTypes {
		want[eventType] = true
	}
	for i := 0; i < 50; i++ {
		event := wsReadEvent(t, conn)
		if eventType, _ := event["type"].(string); want[eventType] {
			return event
		}
	}
	t.Fatalf("did not receive any of %v", eventTypes)
	return nil
}

// wsUserItem 构造 input message item。
func wsUserItem(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

// wsEventResponseID 提取事件里的 response.id。
func wsEventResponseID(t *testing.T, event map[string]any) string {
	t.Helper()
	response, ok := event["response"].(map[string]any)
	if !ok {
		t.Fatalf("event has no response object: %v", event)
	}
	id, _ := response["id"].(string)
	return id
}

// wsAssertError 断言收到的是 error 事件并返回 error 对象。
func wsAssertError(t *testing.T, event map[string]any) map[string]any {
	t.Helper()
	if event["type"] != "error" {
		t.Fatalf("event type = %v, want error: %v", event["type"], event)
	}
	errorObj, ok := event["error"].(map[string]any)
	if !ok {
		t.Fatalf("error event missing error object: %v", event)
	}
	return errorObj
}

// wsMessagesText 提取 RequestMessages 里的消息形态摘要（类型 + 文本），
// 便于一行断言 transcript 结构。
func wsMessagesText(request llm.RequestMessages) []string {
	var summary []string
	for _, message := range request.Messages {
		switch typed := message.(type) {
		case llm.UserMessage:
			summary = append(summary, "user:"+wsContentText(typed.Content))
		case llm.AssistantMessage:
			summary = append(summary, "assistant:"+wsContentText(typed.Content))
		case llm.ToolResultMessage:
			summary = append(summary, "tool_result:"+typed.ToolCallID+":"+wsContentText(typed.Content))
		default:
			summary = append(summary, fmt.Sprintf("%T", message))
		}
	}
	return summary
}

func wsContentText(content []llm.Content) string {
	var parts []string
	for _, block := range content {
		switch typed := block.(type) {
		case llm.TextContent:
			parts = append(parts, typed.Text)
		case llm.ToolCall:
			parts = append(parts, "call:"+typed.ID+":"+typed.Name)
		}
	}
	return strings.Join(parts, "|")
}

// TestWebSocketMultiTurnMerge 验证续链：第二轮带 previous_response_id + 增量
// input 时，上游收到的是「首轮 input + 首轮 output + 增量」的完整回放。
func TestWebSocketMultiTurnMerge(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("first answer"),
		wsTextTurnScript("second answer"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("question one")},
	})
	completed := wsReadUntil(t, conn, "response.completed", "error")
	responseID := wsEventResponseID(t, completed)
	if !strings.HasPrefix(responseID, "resp_") {
		t.Fatalf("response id = %q, want resp_*", responseID)
	}

	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": responseID,
		"input":                []any{wsUserItem("question two")},
	})
	completed = wsReadUntil(t, conn, "response.completed", "error")
	if completed["type"] != "response.completed" {
		t.Fatalf("turn 2 event = %v", completed)
	}

	requests := fake.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("adapter calls = %d, want 2", len(requests))
	}
	got := wsMessagesText(requests[1])
	want := []string{"user:question one", "assistant:first answer", "user:question two"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("turn 2 transcript = %v, want %v", got, want)
	}
	// model 应继承首轮（续帧通常不重复携带）。
	if requests[1].Model != "gpt-test" {
		t.Fatalf("turn 2 model = %q", requests[1].Model)
	}
}

// TestWebSocketPreviousResponseMismatch 验证 previous_response_id 不匹配时
// 返回 400 previous_response_not_found（带 param），连接保持可用。
func TestWebSocketPreviousResponseMismatch(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("answer"),
		wsTextTurnScript("after mismatch"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("q")},
	})
	wsReadUntil(t, conn, "response.completed")

	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": "resp_does_not_exist",
		"input":                []any{wsUserItem("next")},
	})
	event := wsReadUntil(t, conn, "error")
	errorObj := wsAssertError(t, event)
	if errorObj["code"] != "previous_response_not_found" {
		t.Fatalf("error code = %v, want previous_response_not_found", errorObj)
	}
	if errorObj["param"] != "previous_response_id" {
		t.Fatalf("error param = %v, want previous_response_id", errorObj)
	}
	if status, _ := event["status"].(float64); int(status) != http.StatusBadRequest {
		t.Fatalf("error status = %v, want 400", event["status"])
	}

	// 连接仍可用：不带 prev_id 的完整回放应正常续跑。
	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("full replay")},
	})
	wsReadUntil(t, conn, "response.completed")
	if fake.callCount != 2 {
		t.Fatalf("adapter calls = %d, want 2", fake.callCount)
	}
}

// TestWebSocketPrewarm 验证 generate:false 预热帧本地合成响应、不打上游，
// 且其 resp_prewarm_ id 可供下一轮续链（首轮 input 计入 transcript）。
func TestWebSocketPrewarm(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("real answer"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":     "response.create",
		"generate": false,
		"model":    "gpt-test",
		"input":    []any{wsUserItem("prewarm context")},
	})
	completed := wsReadUntil(t, conn, "response.completed", "error")
	prewarmID := wsEventResponseID(t, completed)
	if !strings.HasPrefix(prewarmID, "resp_prewarm_") {
		t.Fatalf("prewarm id = %q, want resp_prewarm_*", prewarmID)
	}
	if fake.callCount != 0 {
		t.Fatalf("prewarm hit upstream %d times, want 0", fake.callCount)
	}

	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": prewarmID,
		"input":                []any{wsUserItem("real question")},
	})
	wsReadUntil(t, conn, "response.completed")

	requests := fake.recordedRequests()
	if len(requests) != 1 {
		t.Fatalf("adapter calls = %d, want 1", len(requests))
	}
	got := wsMessagesText(requests[0])
	want := []string{"user:prewarm context", "user:real question"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("post-prewarm transcript = %v, want %v", got, want)
	}
}

// TestWebSocketPendingToolCall 验证上轮遗留 function_call 时，
// 续轮必须带对应 output；缺失按 invalid_request 报错，补齐后正常合并。
func TestWebSocketPendingToolCall(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsToolCallTurnScript("call_abc", "shell", `{"cmd":"ls"}`),
		wsTextTurnScript("tool result consumed"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("run ls")},
	})
	completed := wsReadUntil(t, conn, "response.completed", "error")
	responseID := wsEventResponseID(t, completed)

	// 缺 output 的增量 → error，连接不断。
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": responseID,
		"input":                []any{wsUserItem("no tool output")},
	})
	errorObj := wsAssertError(t, wsReadUntil(t, conn, "error"))
	if errorObj["code"] != "invalid_request" {
		t.Fatalf("error code = %v, want invalid_request", errorObj)
	}

	// 补齐 output 后正常合并：input = 首轮 + function_call + output + 新消息。
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": responseID,
		"input": []any{
			map[string]any{"type": "function_call_output", "call_id": "call_abc", "output": "file.txt"},
			wsUserItem("what did it list"),
		},
	})
	wsReadUntil(t, conn, "response.completed")

	requests := fake.recordedRequests()
	if len(requests) != 2 {
		t.Fatalf("adapter calls = %d, want 2", len(requests))
	}
	got := wsMessagesText(requests[1])
	want := []string{
		"user:run ls",
		"assistant:call:call_abc:shell",
		"tool_result:call_abc:file.txt",
		"user:what did it list",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("turn 2 transcript = %v, want %v", got, want)
	}
}

// TestWebSocketInterruptedTurnReplaysReplacement 验证上游流中途静默断开：
// 客户端收到 upstream_stream_interrupted 事件，连接保持，下一轮不带
// previous_response_id 的 create 按全量替换处理而非与历史合并。
func TestWebSocketInterruptedTurnReplaysReplacement(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("ok"),
		// 第二轮：只有增量，无终结事件，流直接 EOF。
		{
			{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
			{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "partial", Partial: &llm.AssistantMessage{
				Content:    []llm.Content{llm.TextContent{Text: "partial"}},
				StopReason: llm.StopReasonPending,
			}},
		},
		wsTextTurnScript("replaced"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("first")},
	})
	completed := wsReadUntil(t, conn, "response.completed")
	responseID := wsEventResponseID(t, completed)

	// 第二轮上游中断：delta 之后应收到 interrupted 错误事件，连接不断。
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": responseID,
		"input":                []any{wsUserItem("second")},
	})
	event := wsReadUntil(t, conn, "error")
	errorObj := wsAssertError(t, event)
	if errorObj["code"] != "upstream_stream_interrupted" {
		t.Fatalf("error code = %v, want upstream_stream_interrupted", errorObj)
	}

	// 中断后不带 prev_id 的 create = 全量替换：上游只应看到本轮 input，
	// 不再与 first/second 的历史合并。
	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("fresh restart")},
	})
	wsReadUntil(t, conn, "response.completed")

	requests := fake.recordedRequests()
	if len(requests) != 3 {
		t.Fatalf("adapter calls = %d, want 3", len(requests))
	}
	got := wsMessagesText(requests[2])
	want := []string{"user:fresh restart"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("replacement transcript = %v, want %v", got, want)
	}
}

// TestWebSocketErrorTaxonomy 覆盖杂项错误帧：非法 JSON、不支持的 type、
// 非 resp_ 前缀 prev_id、二进制帧——全部回 error 事件且连接保持。
func TestWebSocketErrorTaxonomy(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("still alive"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	// 非法 JSON。
	if err := conn.WriteMessage(websocket.TextMessage, []byte("{not json")); err != nil {
		t.Fatalf("write: %v", err)
	}
	wsAssertError(t, wsReadUntil(t, conn, "error"))

	// 不支持的请求类型 → unsupported_event。
	wsWriteJSON(t, conn, map[string]any{"type": "response.cancel"})
	if errObj := wsAssertError(t, wsReadUntil(t, conn, "error")); errObj["code"] != "unsupported_event" {
		t.Fatalf("code = %v, want unsupported_event", errObj)
	}

	// 非 resp_ 前缀 previous_response_id → invalid_request。
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"model":                "gpt-test",
		"previous_response_id": "msg_foreign",
		"input":                []any{wsUserItem("x")},
	})
	if errObj := wsAssertError(t, wsReadUntil(t, conn, "error")); errObj["code"] != "invalid_request" {
		t.Fatalf("code = %v, want invalid_request", errObj)
	}

	// 空会话上 append → invalid_request（先 create 才能 append）。
	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.append",
		"input": []any{wsUserItem("x")},
	})
	wsAssertError(t, wsReadUntil(t, conn, "error"))

	// 二进制帧 → unsupported_frame。
	if err := conn.WriteMessage(websocket.BinaryMessage, []byte("binary")); err != nil {
		t.Fatalf("write binary: %v", err)
	}
	if errObj := wsAssertError(t, wsReadUntil(t, conn, "error")); errObj["code"] != "unsupported_frame" {
		t.Fatalf("code = %v, want unsupported_frame", errObj)
	}

	// 连接在所有错误后仍可用。
	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("still here")},
	})
	wsReadUntil(t, conn, "response.completed")
}

// TestWebSocketFailedResponseKeepsConnection 验证上游 response.failed 原样
// 转发且连接保持；失败轮不推进会话，续链其 id 会 404。
func TestWebSocketFailedResponseKeepsConnection(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("ok"),
		{
			{Type: llm.ResponseEventStart, Partial: &llm.AssistantMessage{StopReason: llm.StopReasonPending}},
			{Type: llm.ResponseEventError, Reason: llm.StopReasonError,
				Error: &llm.AssistantMessage{ErrorMessage: "upstream exploded"}},
		},
		wsTextTurnScript("recovered"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("one")},
	})
	completed := wsReadUntil(t, conn, "response.completed")
	firstID := wsEventResponseID(t, completed)

	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": firstID,
		"input":                []any{wsUserItem("two")},
	})
	failed := wsReadUntil(t, conn, "response.failed", "error")
	if failed["type"] != "response.failed" {
		t.Fatalf("event = %v, want response.failed", failed)
	}

	// 失败轮的 id 不可续链。
	failedID := wsEventResponseID(t, failed)
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": failedID,
		"input":                []any{wsUserItem("three")},
	})
	if errObj := wsAssertError(t, wsReadUntil(t, conn, "error")); errObj["code"] != "previous_response_not_found" {
		t.Fatalf("code = %v, want previous_response_not_found", errObj)
	}

	// 回退到首轮 id 仍可续链（失败轮被跳过，transcript 回到首轮末）。
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": firstID,
		"input":                []any{wsUserItem("three")},
	})
	wsReadUntil(t, conn, "response.completed")

	requests := fake.recordedRequests()
	if len(requests) != 3 {
		t.Fatalf("adapter calls = %d, want 3", len(requests))
	}
	got := wsMessagesText(requests[2])
	want := []string{"user:one", "assistant:ok", "user:three"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("post-failure transcript = %v, want %v", got, want)
	}
}

// TestWebSocketConcurrencySlotIsPerTurn 验证并发槽按轮次获取：
// 槽占满时下一轮收到 rate_limit error，释放后同连接可继续跑。
func TestWebSocketConcurrencySlotIsPerTurn(t *testing.T) {
	fake := &wsScriptAdapter{scripts: [][]llm.ResponseEvent{
		wsTextTurnScript("first"),
		wsTextTurnScript("second"),
	}}
	application := New(fake, config.ServerConfig{Listen: ":0", MaxConcurrency: 1}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("one")},
	})
	completed := wsReadUntil(t, conn, "response.completed")
	responseID := wsEventResponseID(t, completed)

	// 占满唯一并发槽，下一轮应收到 rate_limit error 而不是被挤断。
	application.concurrencyInUse.Add(1)
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": responseID,
		"input":                []any{wsUserItem("two")},
	})
	if errObj := wsAssertError(t, wsReadUntil(t, conn, "error")); errObj["type"] != "rate_limit_error" {
		t.Fatalf("error type = %v, want rate_limit_error", errObj)
	}
	application.concurrencyInUse.Add(-1)

	// 释放后同一条连接继续跑。
	wsWriteJSON(t, conn, map[string]any{
		"type":                 "response.create",
		"previous_response_id": responseID,
		"input":                []any{wsUserItem("two")},
	})
	wsReadUntil(t, conn, "response.completed")
	if fake.callCount != 2 {
		t.Fatalf("adapter calls = %d, want 2", fake.callCount)
	}
}

// TestWebSocketConnectionLimit 验证连接数上限独立于并发槽：
// 连接占满时新的 upgrade 直接 429。
func TestWebSocketConnectionLimit(t *testing.T) {
	fake := &wsScriptAdapter{}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)

	for i := 0; i < wsMaxConnections; i++ {
		application.wsConns <- struct{}{}
	}
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	_, response, err := websocket.DefaultDialer.Dial(url, http.Header{})
	if response != nil {
		defer func() { _ = response.Body.Close() }()
	}
	if err == nil {
		t.Fatal("dial should fail when connection limit is reached")
	}
	if response == nil || response.StatusCode != http.StatusTooManyRequests {
		status := 0
		if response != nil {
			status = response.StatusCode
		}
		t.Fatalf("status = %d, want 429 (err %v)", status, err)
	}
}

// TestWebSocketNoUpgradeRejected 验证非 upgrade 的 GET 走 405。
func TestWebSocketNoUpgradeRejected(t *testing.T) {
	fake := &wsScriptAdapter{}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	response := httptest.NewRecorder()
	application.Router().ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}

// TestWebSocketInboundQueueByteLimit 验证入队字节闸：turn 进行中积压的
// 客户端帧总量超过 wsMaxQueuedBytes（64MiB）时服务端回 close 1009 断连。
// 按帧数限额会让 16×32MiB≈512MiB/连接成为最坏值，字节预算才是内存闸。
func TestWebSocketInboundQueueByteLimit(t *testing.T) {
	fake := &blockedStreamAdapter{entered: make(chan struct{})}
	application := New(fake, config.ServerConfig{Listen: ":0"}, nil)
	server := httptest.NewServer(application.Router())
	t.Cleanup(server.Close)
	conn := dialWS(t, server)

	wsWriteJSON(t, conn, map[string]any{
		"type":  "response.create",
		"model": "gpt-test",
		"input": []any{wsUserItem("hold the turn")},
	})
	<-fake.entered

	// 单帧受 32MiB read limit 约束，分帧累积越过 64MiB 入队字节预算；
	// 服务端触发关闭后后续写可能失败，容忍之。
	payload := make([]byte, 16<<20)
	for i := 0; i < 5; i++ {
		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			break
		}
	}
	_, _, err := conn.ReadMessage()
	closeErr, ok := err.(*websocket.CloseError)
	if !ok || closeErr.Code != websocket.CloseMessageTooBig {
		t.Fatalf("read err = %v, want close 1009", err)
	}
}
