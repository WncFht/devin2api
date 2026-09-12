// 本文件为 Codex 等使用 OpenAI Responses WebSocket transport 的客户端提供 WebSocket 接入。
//
// OpenAI Responses WebSocket 模式把 HTTP/SSE 的事件流映射为 WebSocket 文本帧：
// 客户端发送 {"type":"response.create", ...} JSON，服务端把每个 SSE 事件的 data JSON
// 作为一条 WebSocket 文本消息发回，直到 response.completed 或 error。
package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/leookun/devin-2api/internal/llm"
	"github.com/leookun/devin-2api/internal/obs"
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

// wsResponseWriter 把 HTTP SSE 响应解析为单个 WebSocket 文本帧。
// writeProtocolStream 写入的每段 SSE 帧被暂存在 buf 中，遇到 "\n\n" 分隔时拆成完整事件，
// 只把 data 行作为 JSON 文本帧发回客户端（与 OpenAI Responses WebSocket 协议一致）。
type wsResponseWriter struct {
	conn       *websocket.Conn
	header     http.Header
	statusCode int
	buf        []byte
	err        error
}

func newWSResponseWriter(conn *websocket.Conn) *wsResponseWriter {
	return &wsResponseWriter{
		conn:   conn,
		header: make(http.Header),
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

// flushTail 把缓冲区里不构成完整 SSE 帧的残余内容（如错误 JSON 响应体）
// 作为一条文本帧发出；没有它，非流式错误在 WS 路径上会被静默吞掉。
func (w *wsResponseWriter) flushTail() {
	if w.err != nil || len(w.buf) == 0 {
		return
	}
	payload := bytes.TrimSpace(w.buf)
	w.buf = nil
	if len(payload) > 0 {
		_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
		w.err = w.conn.WriteMessage(websocket.TextMessage, payload)
	}
}

// wsWriteDeadline 是单次 WebSocket 写操作的预算；每条消息写出前续约，
// 不会像一次性绝对 deadline 那样在长轮次中途截断流。
const wsWriteDeadline = 60 * time.Second

// writeFrame 解析单条 SSE 帧，把 data 行作为 JSON 文本消息发出。
// 如果遇到 event: error 事件，同样只把 data 发回，让客户端按 OpenAI 协议处理。
// SSE 注释行（": ..."）转换为 WebSocket Ping，承担同等的保活作用。
func (w *wsResponseWriter) writeFrame(frame []byte) error {
	if bytes.HasPrefix(frame, []byte(":")) {
		return w.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteDeadline))
	}
	var data []byte
	lines := bytes.Split(frame, []byte("\n"))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data: ")) {
			data = line[len("data: "):]
		}
	}
	if len(data) == 0 {
		return nil
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteDeadline))
	return w.conn.WriteMessage(websocket.TextMessage, data)
}

// responsesWebSocket 处理 OpenAI Responses WebSocket 连接。
// 只处理单轮 response.create；多轮 response.append 可在此基础上扩展。
func (application *App) responsesWebSocket(writer http.ResponseWriter, request *http.Request) {
	conn, err := upgrader.Upgrade(writer, request, nil)
	if err != nil {
		slog.Warn("websocket upgrade failed", "error", obs.Diagnostic(err))
		return
	}
	defer conn.Close()

	// 读超时只覆盖第一条 response.create：之后的读取由后台协程负责，
	// 客户端整个轮次可能不再发消息，绝对 deadline 会在 10 分钟处误杀
	// 活跃流；连接断开本身通过 ReadMessage 错误或写失败被发现。
	_ = conn.SetReadDeadline(time.Now().Add(600 * time.Second))

	// 读取第一条 response.create 消息。
	messageType, body, err := conn.ReadMessage()
	if err != nil {
		slog.Warn("websocket read failed", "error", obs.Diagnostic(err))
		return
	}
	if messageType != websocket.TextMessage {
		slog.Warn("websocket received non-text message", "type", messageType)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	// OpenAI WebSocket 模式不写 stream 字段，等价于 stream=true。
	// 把 body 解析后强制加上 stream=true，确保走流式分支。
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		slog.Warn("websocket parse request failed", "error", obs.Diagnostic(err))
		return
	}
	payload["stream"] = true
	body, err = json.Marshal(payload)
	if err != nil {
		slog.Warn("websocket encode request failed", "error", obs.Diagnostic(err))
		return
	}

	// 构造一个内部 POST /v1/responses 请求，让现有 createResponses 处理。
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()

	// 监听连接关闭，及时取消上游流。response.create 之后的客户端帧
	// （response.append 等扩展类型）未实现——显式记录而不是静默吞掉。
	go func() {
		seen := make(map[string]bool)
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
			var probe struct {
				Type string `json:"type"`
			}
			frameType := "<unparseable>"
			if json.Unmarshal(frame, &probe) == nil && probe.Type != "" {
				frameType = probe.Type
			}
			if !seen[frameType] {
				seen[frameType] = true
				slog.Warn("websocket: ignoring unsupported client message type", "type", frameType)
			}
		}
	}()

	innerRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	if err != nil {
		slog.Warn("websocket create inner request failed", "error", obs.Diagnostic(err))
		return
	}
	innerRequest.Header.Set("Content-Type", "application/json")
	if auth := request.Header.Get("Authorization"); auth != "" {
		innerRequest.Header.Set("Authorization", auth)
	}
	innerRequest.RemoteAddr = request.RemoteAddr

	wsWriter := newWSResponseWriter(conn)
	application.createResponses(wsWriter, innerRequest)
	wsWriter.flushTail()

	// 如果 createResponses 没有发送 completed/error，尝试补一个干净的关闭。
	if wsWriter.err == nil {
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}
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

// isWebSocketRequest 判断请求是否为 OpenAI Responses WebSocket upgrade。
// 同时检查 Upgrade 头和 Sec-WebSocket-Protocol（OpenAI WebSocket 可能带此协议头）。
func isWebSocketRequest(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// ensureAssistantMessage 保证最终消息不为空；辅助处理 codex 的 SSE 转换。
func ensureAssistantMessage(message *llm.AssistantMessage, fallback *llm.AssistantMessage) *llm.AssistantMessage {
	if message != nil {
		return message
	}
	return fallback
}
