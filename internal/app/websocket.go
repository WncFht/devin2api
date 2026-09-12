// 本文件为 Codex 等使用 OpenAI Responses WebSocket transport 的客户端提供 WebSocket 接入。
//
// OpenAI Responses WebSocket 模式把 HTTP/SSE 的事件流映射为 WebSocket 文本帧：
// 客户端在同一连接上反复发送 {"type":"response.create"|"response.append", ...} JSON；
// 服务端把每轮 SSE 事件的 data JSON 作为一条 WebSocket 文本消息发回，直到
// response.completed/response.failed/error。
//
// 上游没有 previous_response_id 语义，多轮通过 wsSession 把增量 input 展开成
// 完整 transcript 再走常规 /v1/responses 流水线（见 websocket_session.go）。
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/WncFht/devin2api/internal/obs"
	"github.com/WncFht/devin2api/internal/randid"
	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	// 允许跨源（Codex 本地调用可能使用不同 origin）。
	CheckOrigin: func(r *http.Request) bool { return true },
	// Codex/OpenAI Responses WebSocket 协商此子协议。
	Subprotocols: []string{"responses_websockets=2026-02-06"},
	// 给足读写缓冲，避免高频事件时频繁系统调用。
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
}

const (
	// wsWriteDeadline 是单次 WebSocket 写操作的预算；每条消息写出前续约，
	// 不会像一次性绝对 deadline 那样在长轮次中途截断流。
	wsWriteDeadline = 60 * time.Second
	// wsFirstMessageTimeout 是升级后等待第一条 response.create 的窗口，
	// 防慢握手占连接。
	wsFirstMessageTimeout = 30 * time.Second
	// wsIdleTimeout 是轮次之间/消息之间允许的空闲上限；PongHandler 与每条
	// 客户端消息都会续约。对应 ccLoad 的 5min 空闲回收。
	wsIdleTimeout = 5 * time.Minute
	// wsPingInterval 是服务端主动 ping 的周期；客户端 pong 在 PongHandler 里
	// 续约 read deadline，双向确认存活。WriteControl 不经写锁，不会被大
	// 数据帧饿死（CPA#5734 的教训）。
	wsPingInterval = 2 * time.Minute
	// wsMaxConnections 是进程级下游 WS 连接上限。连接占用的是 fd+goroutine，
	// 与上游并发槽分开计量——空闲连接不该烧并发额度。
	wsMaxConnections = 256
)

// wsInboundMessage 是 reader goroutine 交给主循环的一帧。
type wsInboundMessage struct {
	messageType int
	payload     []byte
}

// errWSConnectionClosed 表示 close 帧已发出、写管线应停止。它从 writeFrame
// 冒泡到 runWSTurn，主循环见到任何 turnErr 都会退出——这个哨兵只是阻止
// 关闭后继续向客户端写数据帧。
var errWSConnectionClosed = errors.New("websocket close frame sent")

// wsResponseWriter 把 HTTP SSE 响应解析为单个 WebSocket 文本帧，同时收集
// 本轮 output item 供会话状态回放。
// writeProtocolStream 写入的每段 SSE 帧被暂存在 buf 中，遇到 "\n\n" 分隔时
// 拆成完整事件，只把 data 行作为 JSON 文本帧发回客户端（与 OpenAI Responses
// WebSocket 协议一致）。
type wsResponseWriter struct {
	conn       *websocket.Conn
	header     http.Header
	statusCode int
	buf        []byte
	err        error
	// debugRefSent 标记 debug_ref 是否已随 response.created 下发。
	debugRefSent bool
	// 以下为轮级状态收集：completed/failed 标记终结形态，surfaced 表示
	// 客户端已收到可终结本轮的信号，output items 供 wsSession 回放。
	completed           bool
	failed              bool
	surfaced            bool
	completedResponseID string
	// lastTerminal 是最后收到的终结形态事件 payload（completed/failed 的
	// data JSON），commit 时从它提取 response.output。
	lastTerminal    json.RawMessage
	outputItems     map[int64]json.RawMessage
	outputUnindexed []json.RawMessage
	outputBytes     int64
}

func newWSResponseWriter(conn *websocket.Conn) *wsResponseWriter {
	return &wsResponseWriter{
		conn:        conn,
		header:      make(http.Header),
		outputItems: make(map[int64]json.RawMessage),
	}
}

func (w *wsResponseWriter) Header() http.Header        { return w.header }
func (w *wsResponseWriter) WriteHeader(statusCode int) { w.statusCode = statusCode }

func (w *wsResponseWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	w.buf = append(w.buf, p...)
	for {
		idx := bytes.Index(w.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := w.buf[:idx]
		w.buf = w.buf[idx+2:]
		if err := w.writeFrame(frame); err != nil {
			w.err = err
			return 0, err
		}
	}
	return len(p), nil
}

func (w *wsResponseWriter) Flush() {
	// writeProtocolStream 在每次写入 SSE 后都会 Flush；
	// 但实际消息在 Write 遇到 "\n\n" 时已经发送，这里不需要额外动作。
}

// flushTail 把缓冲区里不构成完整 SSE 帧的残余内容（如非流式 JSON 错误体）
// 作为一条文本帧发出；没有它，非流式错误在 WS 路径上会被静默吞掉。
// 常规 {"error":{...}} 体会被包成 {"type":"error","status":N,"error":{...}}
// 事件形状——WS 客户端靠 type 字段分发，裸错误 JSON 无法终结回合。
func (w *wsResponseWriter) flushTail() {
	if w.err != nil || len(w.buf) == 0 {
		return
	}
	payload := bytes.TrimSpace(w.buf)
	w.buf = nil
	if len(payload) == 0 {
		return
	}
	payload = w.wrapErrorPayload(payload)
	w.surfaced = true
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	w.err = w.conn.WriteMessage(websocket.TextMessage, payload)
}

// wrapErrorPayload 把非流式 {"error":{...}} 响应体转成 WS error 事件；
// 已是事件形状（带 type 字段）或其他 JSON 原样透传。
func (w *wsResponseWriter) wrapErrorPayload(payload []byte) []byte {
	if wsJSONString(payload, "type") != "" {
		return payload
	}
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(payload, &envelope) != nil || len(envelope.Error) == 0 {
		return payload
	}
	status := w.statusCode
	if status == 0 {
		status = http.StatusInternalServerError
	}
	event, err := json.Marshal(map[string]any{
		"type":   "error",
		"status": status,
		"error":  envelope.Error,
	})
	if err != nil {
		return payload
	}
	return event
}

// writeFrame 解析单条 SSE 帧，把 data 行作为 JSON 文本消息发出。
// 帧级职责：收集 output item、识别终结事件、镜像 message_too_big 为 close 1009。
// SSE 注释行（": ..."）转换为 WebSocket Ping，承担同等的保活作用。
func (w *wsResponseWriter) writeFrame(frame []byte) error {
	if bytes.HasPrefix(frame, []byte(":")) {
		return w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline))
	}
	var data []byte
	lines := bytes.Split(frame, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data: ")) {
			data = append(data, line[len("data: "):]...)
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, line[len("data:"):]...)
		}
	}
	if len(data) == 0 {
		return nil
	}
	if !json.Valid(data) {
		// 非 JSON 的 data 帧原样透传（协议外内容不应静默丢弃）；不算终结信号。
		_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		return w.conn.WriteMessage(websocket.TextMessage, data)
	}
	eventType := wsJSONString(data, "type")
	if err := w.collectOutputItem(eventType, data); err != nil {
		return err
	}
	switch eventType {
	case "response.completed", "response.done", "response.incomplete":
		w.completed = true
		w.surfaced = true
		w.lastTerminal = bytes.Clone(data)
		w.completedResponseID = wsNestedJSONString(data, "response", "id")
	case "response.failed":
		w.failed = true
		w.surfaced = true
		w.lastTerminal = bytes.Clone(data)
		w.completedResponseID = wsNestedJSONString(data, "response", "id")
	}
	if wsIsMessageTooBigPayload(data) {
		_ = w.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "upstream websocket message too big"),
			time.Now().Add(wsWriteDeadline),
		)
		return errWSConnectionClosed
	}
	// surfaced 语义 = 客户端已收到可终结本轮的信号（终结事件/error 事件/
	// flushTail 兜底）。普通增量帧不算——只发过 delta 就断流仍属于
	// 中途断，需要补 upstream_stream_interrupted 让客户端收尾。
	if eventType == "error" {
		w.surfaced = true
	}
	data = w.withDebugRef(data)
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	return w.conn.WriteMessage(websocket.TextMessage, data)
}

// wsNestedJSONString 读取嵌套两层的字符串字段（如 response.id）。
func wsNestedJSONString(payload []byte, outer, inner string) string {
	object, has, _ := wsJSONField(payload, outer)
	if !has {
		return ""
	}
	return wsJSONString(object, inner)
}

// wsIsMessageTooBigPayload 识别上游/适配层的 message_too_big 错误事件——
// Codex 客户端靠 close code 1009 决策降级到 SSE，事件本身不转发。
func wsIsMessageTooBigPayload(payload []byte) bool {
	return wsNestedJSONString(payload, "error", "code") == "message_too_big"
}

// collectOutputItem 累积 response.output_item.done 的 item 快照。
// completed 事件的 response.output 才是回放基准；collected 只在它缺失/为空
// 时兜底（见 turnResult）。
func (w *wsResponseWriter) collectOutputItem(eventType string, payload []byte) error {
	if eventType != "response.output_item.done" {
		return nil
	}
	item, has, _ := wsJSONField(payload, "item")
	if !has {
		return nil
	}
	item = bytes.Clone(bytes.TrimSpace(item))
	indexRaw, hasIndex, _ := wsJSONField(payload, "output_index")
	var index int64 = -1
	if hasIndex {
		_ = json.Unmarshal(indexRaw, &index)
	}
	if index >= 0 {
		w.outputBytes += int64(len(item)) - int64(len(w.outputItems[index]))
		w.outputItems[index] = item
	} else {
		w.outputBytes += int64(len(item))
		w.outputUnindexed = append(w.outputUnindexed, item)
	}
	if w.outputBytes > wsMaxTranscriptBytes {
		// 收集的 output 超过 transcript 上限与 message_too_big 同义：
		// 回 close 1009 让客户端降级，同时停掉写管线。
		_ = w.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "response output exceeds websocket transcript limit"),
			time.Now().Add(wsWriteDeadline),
		)
		return errWSConnectionClosed
	}
	return nil
}

// turnResult 汇总本轮提交内容：优先用终结事件的 response.output；
// 没有（或为空）时退回 output_item.done 收集的项（过滤不完整 tool call）。
func (w *wsResponseWriter) turnResult(completedOutput json.RawMessage) wsTurnResult {
	output := completedOutput
	if len(bytes.TrimSpace(output)) <= 2 {
		output = w.collectedOutput()
	}
	return wsTurnResult{
		completedOutput:     output,
		completedResponseID: w.completedResponseID,
		pendingToolCallIDs:  wsPendingToolCallIDs(output),
	}
}

// collectedOutput 把 output_item.done 收集的项按 output_index 排序拼回数组；
// 不完整 tool call（缺 call_id/name/arguments）被过滤——回放半成品 call 会让
// 客户端的 output 变孤儿。
func (w *wsResponseWriter) collectedOutput() json.RawMessage {
	items := make([]json.RawMessage, 0, len(w.outputItems)+len(w.outputUnindexed))
	appendItem := func(raw json.RawMessage) {
		if wsIsToolCallItem(raw) && !wsIsCompleteToolCall(raw) {
			return
		}
		items = append(items, raw)
	}
	indices := make([]int, 0, len(w.outputItems))
	for index := range w.outputItems {
		indices = append(indices, int(index))
	}
	sort.Ints(indices)
	for _, index := range indices {
		appendItem(w.outputItems[int64(index)])
	}
	for _, raw := range w.outputUnindexed {
		appendItem(raw)
	}
	if len(items) == 0 {
		return json.RawMessage("[]")
	}
	encoded, err := json.Marshal(items)
	if err != nil {
		return json.RawMessage("[]")
	}
	return encoded
}

// completedOutputFromEvent 提取终结事件的 response.output 数组。
func completedOutputFromEvent(payload json.RawMessage) json.RawMessage {
	response, has, _ := wsJSONField(payload, "response")
	if !has {
		return nil
	}
	output, has, isArray := wsJSONField(response, "output")
	if !has || !isArray {
		return nil
	}
	return output
}

// withDebugRef 把调试目录名编进 response.created 的 response 对象。
// WS 握手响应在 createCompletion 分配请求 ID 之前就已发出，X-Request-Id
// 无处可放——客户端只能靠 payload 携带的引用回查日志目录（错误事件
// 自带 debug_ref，这里只补成功路径的首帧）。
func (w *wsResponseWriter) withDebugRef(data []byte) []byte {
	if w.debugRefSent || !bytes.Contains(data, []byte(`"response.created"`)) {
		return data
	}
	ref := w.header.Get("X-Request-Id")
	var event map[string]any
	if ref == "" || json.Unmarshal(data, &event) != nil || event["type"] != "response.created" {
		return data
	}
	response, ok := event["response"].(map[string]any)
	if !ok {
		return data
	}
	response["debug_ref"] = ref
	patched, err := json.Marshal(event)
	if err != nil {
		return data
	}
	w.debugRefSent = true
	return patched
}

// responsesWebSocket 处理 OpenAI Responses WebSocket 连接：
// 一条连接上按 turn 串行处理 response.create/response.append。
// 读循环与 turn 处理分离——turn 进行中 reader goroutine 继续消费控制帧
// （ping/pong/close）与排队下一帧，客户端断连能及时取消上游。
func (application *App) responsesWebSocket(writer http.ResponseWriter, request *http.Request) {
	// 连接级准入：与上游并发槽分开计量，空闲长连接不占并发额度。
	select {
	case application.wsConns <- struct{}{}:
		defer func() { <-application.wsConns }()
	default:
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"error": map[string]any{
				"message": "websocket connection limit reached; close an existing connection or retry later",
				"type":    "rate_limit_error",
				"code":    "responses_websocket_connection_limit_exceeded",
			},
		})
		return
	}

	conn, err := upgrader.Upgrade(writer, request, wsUpgradeHeaders(request))
	if err != nil {
		slog.Warn("websocket upgrade failed", "error", obs.Diagnostic(err))
		return
	}
	defer conn.Close()
	conn.SetReadLimit(wsMaxTranscriptBytes)

	connCtx, cancelConn := context.WithCancel(context.Background())
	defer cancelConn()

	// 读协程常驻：turn 进行中也要继续消费帧——gorilla 只在 ReadMessage 里
	// 处理 ping/pong/close 控制帧，且这是发现客户端断连的唯一手段。
	// channel 带缓冲：turn 进行中读到的数据帧排队等主循环，控制帧照常应答；
	// 读端一旦出错立即 cancelConn，让在途上游随 ctx 取消而不是空跑到结束。
	messages := make(chan wsInboundMessage, 16)
	go func() {
		defer close(messages)
		first := true
		for {
			if first {
				_ = conn.SetReadDeadline(time.Now().Add(wsFirstMessageTimeout))
			} else {
				_ = conn.SetReadDeadline(time.Now().Add(wsIdleTimeout))
			}
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					slog.Debug("websocket client disconnected", "error", obs.Diagnostic(err))
				} else {
					slog.Debug("websocket read ended", "error", obs.Diagnostic(err))
				}
				cancelConn()
				return
			}
			first = false
			select {
			case messages <- wsInboundMessage{messageType: messageType, payload: payload}:
			case <-connCtx.Done():
				return
			}
		}
	}()
	// PongHandler 续约 read deadline：reader 的 deadline 由 ping/pong 心跳与
	// 客户端消息共同维持，纯空闲（客户端一言不发）的连接也靠这个活过 idle 窗。
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(wsIdleTimeout))
	})
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-connCtx.Done():
				return
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline)); err != nil {
					cancelConn()
					return
				}
			}
		}
	}()

	session := newWSSession()
	for {
		var message wsInboundMessage
		select {
		case <-connCtx.Done():
			return
		case inbound, ok := <-messages:
			if !ok {
				return
			}
			message = inbound
		}

		if message.messageType != websocket.TextMessage {
			if err := writeWSErrorEvent(conn, http.StatusBadRequest, "invalid_request_error", "unsupported_frame", "", "only text websocket messages are supported"); err != nil {
				return
			}
			continue
		}

		normalized, err := session.normalizeRequest(message.payload)
		if err != nil {
			code := "invalid_request"
			param := ""
			if errors.Is(err, errWSPreviousResponseNotFound) {
				code = "previous_response_not_found"
				param = "previous_response_id"
			} else if errors.Is(err, errWSUnsupportedRequestType) {
				code = "unsupported_event"
			}
			if err := writeWSErrorEvent(conn, http.StatusBadRequest, "invalid_request_error", code, param, err.Error()); err != nil {
				return
			}
			continue
		}

		// 预热帧：generate:false 的请求本地合成 created+completed，input 计入
		// transcript 但不打上游。判定放在 normalize 之后——normalize 会把
		// generate 字段剥掉，且本轮的规范化错误仍按常规路径报给客户端。
		if wsGenerateDisabled(message.payload) {
			result, err := writeWSPrewarm(conn, normalized)
			if err != nil {
				return
			}
			session.commit(normalized, result)
			continue
		}

		// 并发槽按轮次获取：连接的空闲期不烧额度，溢出回 429 事件不断连。
		select {
		case application.concurrency <- struct{}{}:
		default:
			application.metrics.Reject()
			if err := writeWSErrorEvent(conn, http.StatusTooManyRequests, "rate_limit_error", "rate_limit", "", "server is busy, please try again later"); err != nil {
				return
			}
			continue
		}
		turnWriter, turnErr := application.runWSTurn(connCtx, conn, request, normalized)
		<-application.concurrency

		if turnErr != nil {
			// 写出层失败（断连/close sent）——连接已不可用，直接退出。
			return
		}
		completedOutput := completedOutputFromEvent(turnWriter.lastTerminal)
		switch {
		case turnWriter.completed:
			session.commit(normalized, turnWriter.turnResult(completedOutput))
		case turnWriter.failed:
			// response.failed 已转发客户端；本轮不推进会话——续链 prev_id
			// 会 404 触发重放，符合预期。
			session.requireReplacementReplay()
		case !turnWriter.surfaced:
			// 流结束但客户端没收到任何可终结本轮的信号：补一个中断事件，
			// 并标记下次 create 为全量替换（客户端会重放完整 transcript）。
			session.requireReplacementReplay()
			if err := writeWSErrorEvent(conn, http.StatusBadGateway, "server_error", "upstream_stream_interrupted",
				"", "upstream response was interrupted; resend the full conversation input"); err != nil {
				return
			}
		default:
			// surfaced 但既非 completed 也非 failed（如 error 事件兜底）：
			// 不推进会话，下一轮照增量/替换规则处理。
			session.requireReplacementReplay()
		}
	}
}

// runWSTurn 把一条规范化请求交给常规 /v1/responses 流水线执行。
// 返回的 writer 供调用方读取轮级状态（completed/failed/output 收集）。
func (application *App) runWSTurn(connCtx context.Context, conn *websocket.Conn, upgradeRequest *http.Request, body json.RawMessage) (*wsResponseWriter, error) {
	innerRequest, err := http.NewRequestWithContext(connCtx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	innerRequest.Header.Set("Content-Type", "application/json")
	// innerRequest 不经过 HTTP middleware，鉴权与日志关联所需的头逐一手动透传：
	// createCompletion 的凭据哈希、clientRequestID、UserAgent 都从这里取。
	// X-Request-Id 不透传——那是升级请求级的关联 ID，每轮各记各的调试目录。
	for _, name := range []string{
		"Authorization", "X-Api-Key", "X-Session-Id", "User-Agent",
		"Session-Id", "Session_id", "Thread-Id", "X-Codex-Turn-Metadata",
	} {
		if value := upgradeRequest.Header.Get(name); value != "" {
			innerRequest.Header.Set(name, value)
		}
	}
	innerRequest.RemoteAddr = upgradeRequest.RemoteAddr

	turnWriter := newWSResponseWriter(conn)
	application.createResponses(turnWriter, innerRequest)
	turnWriter.flushTail()
	if turnWriter.err != nil {
		return turnWriter, turnWriter.err
	}
	return turnWriter, nil
}

// writeWSPrewarm 合成预热回合的 response.created + response.completed。
// Codex 发 generate:false 是为了让连接进入可续链状态而不消耗上游配额；
// 合成响应必须带 resp_ 前缀 id 供下一轮 previous_response_id 引用。
func writeWSPrewarm(conn *websocket.Conn, request json.RawMessage) (wsTurnResult, error) {
	responseID := randid.Prefixed("resp_prewarm_")
	createdAt := time.Now().Unix()
	model := wsJSONString(request, "model")
	response := map[string]any{
		"id": responseID, "object": "response", "created_at": createdAt,
		"status": "in_progress", "background": false, "error": nil,
		"model": model, "output": []any{},
	}
	created, err := json.Marshal(map[string]any{
		"type": "response.created", "sequence_number": 0, "response": response,
	})
	if err != nil {
		return wsTurnResult{}, err
	}
	if err := writeWSPayload(conn, created); err != nil {
		return wsTurnResult{}, err
	}
	response["status"] = "completed"
	response["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0}
	completed, err := json.Marshal(map[string]any{
		"type": "response.completed", "sequence_number": 1, "response": response,
	})
	if err != nil {
		return wsTurnResult{}, err
	}
	if err := writeWSPayload(conn, completed); err != nil {
		return wsTurnResult{}, err
	}
	return wsTurnResult{
		completedOutput:     json.RawMessage("[]"),
		completedResponseID: responseID,
	}, nil
}

// writeWSErrorEvent 发送 {"type":"error","status":N,"error":{...}} 事件。
// 非终结错误走这条路——连接保持开启，客户端可继续下发一帧。
func writeWSErrorEvent(conn *websocket.Conn, status int, errorType, code, param, message string) error {
	body := map[string]any{"message": message, "type": errorType}
	if code != "" {
		body["code"] = code
	}
	if param != "" {
		body["param"] = param
	}
	event, err := json.Marshal(map[string]any{
		"type":   "error",
		"status": status,
		"error":  body,
	})
	if err != nil {
		return err
	}
	return writeWSPayload(conn, event)
}

func writeWSPayload(conn *websocket.Conn, payload []byte) error {
	if conn == nil {
		return errors.New("websocket connection is nil")
	}
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// wsUpgradeHeaders 在 upgrade 101 响应里回带 x-codex-turn-state——Codex 靠它
// 在重连后保持 turn 状态粘性（参照 CLIProxyAPI websocketUpgradeHeaders）。
func wsUpgradeHeaders(request *http.Request) http.Header {
	headers := http.Header{}
	if request == nil {
		return headers
	}
	if turnState := request.Header.Get("x-codex-turn-state"); turnState != "" {
		headers.Set("x-codex-turn-state", turnState)
	}
	return headers
}

// createResponsesWebSocket 是 /v1/responses 的 WebSocket upgrade 入口。
// 它只在请求包含 WebSocket upgrade 头时升级连接；否则应由 chi POST 路由处理。
func (application *App) createResponsesWebSocket(writer http.ResponseWriter, request *http.Request) {
	if !websocket.IsWebSocketUpgrade(request) {
		// 非 WebSocket 请求应走正常 POST 路由；这里返回 405，提示需要 POST。
		writer.Header().Set("Allow", "POST")
		http.Error(writer, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	application.responsesWebSocket(writer, request)
}
