// 本文件是流式响应子系统：泵协程驱动上游事件流、SSE 写出方、保活帧，
// 以及非流式请求的事件收集。编排入口 createCompletion 见 app.go。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// keepaliveInterval 是上游静默窗口内的 SSE 注释保活间隔。
// 上游长思考时首批帧可延迟数十秒（实测 45s+），而 Codex 客户端约 30s
// 无数据即弃连，下游网关也有自己的空闲/首字节超时。
// var 而非 const：测试会临时缩短它来验证保活路径。
var keepaliveInterval = 10 * time.Second

// sseKeepalive 是 SSE 注释行：协议合法、SSE 客户端解析器忽略，
// 仅用于刷新链路上各段的空闲计时器。
var sseKeepalive = []byte(": keepalive\n\n")

// streamWriter 是流式响应的唯一写出方；committed 标记首字节是否已把
// HTTP 状态提交为 200——提交后错误只能以 SSE error 事件下发。
type streamWriter struct {
	writer    http.ResponseWriter
	flusher   http.Flusher
	recorder  *debuglog.Recorder
	committed bool
	// upstreamOpen 标记上游流已建立：保活只在此后武装——建连前的静默期
	// 写任何字节都会提前提交 200，限流闩/上游 connect 失败便无法再以
	// 真实状态码（429 等）下发。
	upstreamOpen bool
	// heartbeat 是等待上游期间周期性写出的保活载荷：SSE 用注释行，
	// 非流式 JSON 用 "\n"（合法前导空白）；空表示不心跳。
	heartbeat []byte
	// bytes 累计写出字节数，供 metrics 统计响应流量。
	bytes int
}

// write 写一段响应体并立即 flush。
func (out *streamWriter) write(p []byte) error {
	out.committed = true
	if _, err := out.writer.Write(p); err != nil {
		return err
	}
	out.bytes += len(p)
	out.recorder.AddClientBytes(int64(len(p)))
	out.flusher.Flush()
	return nil
}

// writeContent 写一段协议内容帧（区别于 SSE 保活注释），
// 并标记「首个客户端可见字节」时间点。
func (out *streamWriter) writeContent(p []byte) error {
	out.recorder.NoteClientLatency()
	return out.write(p)
}

// awaitEvent 等待上游下一个事件；等待期间按 ticker 节奏写保活帧。
// 保活仅在上游流建立后武装：connected 标记由泵协程在 Stream() 返回后
// 推入，先于任何事件到达。items 关闭视为流结束；ctx 取消或写失败时
// 返回对应错误。
func (out *streamWriter) awaitEvent(ctx context.Context, items <-chan pumpItem, ticker *time.Ticker) (llm.ResponseEvent, error) {
	for {
		select {
		case item, ok := <-items:
			if !ok {
				return llm.ResponseEvent{}, io.EOF
			}
			if item.connected {
				out.upstreamOpen = true
				continue
			}
			return item.event, item.err
		case <-ticker.C:
			if len(out.heartbeat) == 0 || !out.upstreamOpen {
				continue
			}
			if err := out.write(out.heartbeat); err != nil {
				return llm.ResponseEvent{}, err
			}
		case <-ctx.Done():
			// Cause 携带取消原因：面板 abort 给的是「aborted via panel」
			// 而不是裸 context.Canceled，客户端/日志能区分主动中断。
			return llm.ResponseEvent{}, context.Cause(ctx)
		}
	}
}

// pumpItem 是 Stream()/Recv() 的一次产出。connected 为 true 时不是事件
// 而是「上游流已建立」信号——泵协程在 Stream() 成功后、首个 Recv 结果前
// 推入，写出方据此武装保活。它必然先于所有事件被消费（首个 awaitEvent
// 循环会把它吃掉），writeProtocolStream 的批量预取分支见不到它。
type pumpItem struct {
	event     llm.ResponseEvent
	err       error
	connected bool
}

// startStreamPump 在后台协程里建立上游流并串行消费事件，把结果按序推入
// channel。这样唯一的写出方在等待上游的空窗期可以写保活帧，而 Recv
// 仍发生在同一协程。事件记录随泵进行，保持调试日志与上游顺序一致。
// ctx 取消时泵退出，channel 随之关闭。
func startStreamPump(ctx context.Context, provider adapter.Adapter, messages llm.RequestMessages, recorder *debuglog.Recorder) <-chan pumpItem {
	items := make(chan pumpItem, 8)
	go func() {
		defer close(items)
		recorder.NoteRequestReady()
		stream, err := provider.Stream(ctx, messages)
		if err != nil {
			items <- pumpItem{err: err}
			return
		}
		items <- pumpItem{connected: true}
		for {
			event, err := stream.Recv(ctx)
			if err == nil {
				// Start 事件是本地合成的信封且可能被 startHold 提前释放——
				// 不算上游产出；首帧时延要量的是上游真实事件的到达时刻。
				if event.Type != llm.ResponseEventStart {
					recorder.NoteUpstreamLatency()
				}
				if recorder != nil {
					// 多数事件的投影只读标量字段，建树推迟到日志 worker——
					// 泵 goroutine 只付一次 channel send。例外按
					// ResponseEventProjection 的解引用点逐一判定：Partial 只在
					// Start 读、ToolCall 只在 ToolCallEnd 读、Message/Error 存在
					// 即读；这些指针指向 decoder 跨帧续改的活对象，worker 里
					// 解引用会与泵协程并发读写竞争，必须就地求值。
					var projected any
					if event.Message != nil || event.Error != nil || event.ToolCall != nil ||
						(event.Type == llm.ResponseEventStart && event.Partial != nil) {
						projected = debuglog.ResponseEventProjection(event)
					} else {
						ev := event
						projected = func() any { return debuglog.ResponseEventProjection(ev) }
					}
					recorder.AppendJSONL("05-response-events.jsonl", string(event.Type), projected)
				}
			}
			select {
			case items <- pumpItem{event: event, err: err}:
			case <-ctx.Done():
				// 两路就绪时随机选：取消瞬间产出的尾帧会被丢掉。
				// 不在这里补投递——取消归因统一由消费端按 ctx 状态
				// 收口（writeProtocolStream/collectPumpedMessage）。
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return items
}

// streamCompletion 处理流式请求：泵协程驱动上游事件流，本函数是唯一写出方。
// 上游产生任何内容前的错误仍走非 200 状态码；保活一旦提交 200，
// 后续错误降级为 SSE error 事件（与流中途错误同形）。
func (application *App) streamCompletion(
	ctx context.Context,
	writer http.ResponseWriter,
	recorder *debuglog.Recorder,
	protocol protocolEncoder,
	messages llm.RequestMessages,
	options protocolOptions,
	completion *debuglog.Completion,
	responseBytes *int,
) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		completion.StatusCode = http.StatusInternalServerError
		writeLoggedError(writer, recorder, protocol, "http_stream", completion.StatusCode, errors.New("streaming response writer does not support flushing"))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	out := &streamWriter{writer: writer, flusher: flusher, recorder: recorder, heartbeat: sseKeepalive}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	items := startStreamPump(streamCtx, application.adapter, messages, recorder)
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	firstEvent, firstErr := out.awaitEvent(streamCtx, items, ticker)
	if firstErr != nil && !errors.Is(firstErr, io.EOF) {
		// 建流后首事件前的断连/中止先按取消归因——context.Canceled 会被
		// mapProviderErrorStatus 映成 499，走 writeLoggedError 就把断连
		// 记成了 failed（对照 app.go 非流式路径的 client_disconnected 分支）。
		if errors.Is(firstErr, context.Canceled) || errors.Is(firstErr, context.DeadlineExceeded) || streamCtx.Err() != nil {
			completion.Result = "disconnected"
			recorder.WriteError("client_disconnected", firstErr)
			return
		}
		status := mapProviderErrorStatus(firstErr)
		if !out.committed && (!protocol.StreamErrorEvents() || status != http.StatusTooManyRequests) {
			completion.StatusCode = status
			writeLoggedError(writer, recorder, protocol, "provider_stream", status, firstErr)
			return
		}
		// OpenAI 流式面上的限流是唯一转流内事件的 pre-stream 失败：
		// Codex 对 HTTP 429 一律终止（codex-rs retry_429 硬编码 false，
		// 5xx/transport 反而照常重试），只有流内错误事件进它的重试
		// 循环；确定性错误（4xx）重试无意义，保留真实状态码供下游
		// 网关分类。Anthropic 面不在此列——其客户端按 HTTP 状态码
		// 重试，提交 200 反而把失败降级为不可重试的畸形响应。
		firstEvent = llm.ResponseEvent{Type: llm.ResponseEventError, Reason: llm.StopReasonError,
			Error: &llm.AssistantMessage{ErrorMessage: firstErr.Error()}}
		firstErr = nil
	}
	prelude := []llm.ResponseEvent{firstEvent}
	if !out.committed && firstEvent.Type == llm.ResponseEventError {
		// 上游把断连物化成首事件错误时同样按取消归因——事件文本经
		// errors.New 重建后错误链已丢，errors.Is 接不到，直接看 ctx。
		if streamCtx.Err() != nil {
			completion.Result = "disconnected"
			recorder.WriteError("client_disconnected", context.Cause(streamCtx))
			return
		}
		message := "response stream returned an error event immediately"
		if firstEvent.Error != nil && firstEvent.Error.ErrorMessage != "" {
			message = firstEvent.Error.ErrorMessage
		}
		status := mapProviderErrorStatus(errors.New(message))
		if protocol.StreamErrorEvents() && (common.IsContextLengthError(message) || status == http.StatusTooManyRequests) {
			// Codex 只在 SSE response.failed 里按 error.code==
			// "context_length_exceeded" 识别窗口溢出并自动压缩——但网关
			// 会把无正常事件前置的 SSE 错误物化成 HTTP 错误响应，
			// 客户端永远收不到 response.failed。先补一个合成 start 让网关
			// 提交 200，error 事件随后以 SSE 送达；事件顶层 status 仍让
			// 网关按 413 归为客户端错误、不冷却渠道。
			// 限流错误同理走 200 + error 事件：OpenAI 流式客户端的
			// 可重试通道只有流内事件（见上方注释与 StreamErrorEvents）。
			// 两个子句都以 StreamErrorEvents 为前提——Anthropic 客户端
			// 按 HTTP 状态码重试（见上方 firstErr 分支），提前提交 200
			// 会把失败降级为不可重试的畸形响应。
			prelude = []llm.ResponseEvent{
				{Type: llm.ResponseEventStart, Reason: llm.StopReasonPending, Partial: firstEvent.Error},
				firstEvent,
			}
		} else {
			completion.StatusCode = status
			writeLoggedError(writer, recorder, protocol, "provider_stream", status, errors.New(message))
			return
		}
	}

	completion.StatusCode = http.StatusOK
	message, streamErr := writeProtocolStream(streamCtx, out, items, ticker, recorder, protocol, messages.Model, options, prelude, firstErr)
	updateCompletionIdentity(completion, messages, message)
	*responseBytes += out.bytes
	if streamErr != nil {
		noteRetryAfter(recorder, streamErr.Error())
		// 流内错误事件下发的限流 HTTP 状态仍是 200——按文案语义补标，
		// 责任归因与 429 采样才不会把这批限流漏成普通失败。
		if common.HTTPStatus(streamErr.Error()) == http.StatusTooManyRequests {
			recorder.SetRateLimited()
		}
		switch {
		case errors.Is(streamErr, context.Canceled), errors.Is(streamErr, context.DeadlineExceeded), streamCtx.Err() != nil:
			// ctx 取消收口：写出/编码错误与取消同时发生时同样归因断连。
			completion.Result = "disconnected"
			recorder.WriteError("client_disconnected", streamErr)
		case !out.committed:
			// 首字节前的失败（如编码器错误）：响应行还没提交成 200，
			// 按真实状态码下发，不能让客户端拿到「200 + 空流」。
			completion.StatusCode = mapProviderErrorStatus(streamErr)
			writeLoggedError(writer, recorder, protocol, "http_stream", completion.StatusCode, streamErr)
		default:
			recorder.WriteError("http_stream", streamErr)
		}
		return
	}
	completion.Result = "completed"
}

func writeProtocolStream(
	ctx context.Context,
	out *streamWriter,
	items <-chan pumpItem,
	ticker *time.Ticker,
	recorder *debuglog.Recorder,
	protocol protocolEncoder,
	model string,
	options protocolOptions,
	prelude []llm.ResponseEvent,
	preludeErr error,
) (*llm.AssistantMessage, error) {
	encoder := protocol.NewStreamEncoder(model, options.IncludeUsage)
	var latest *llm.AssistantMessage
	// batch 累计本批次的编码字节：泵 channel 持续供给时多个事件并入同一批，
	// 一次 Write+Flush；channel 空了立即落盘，空闲路径与逐事件写出等价。
	var batch []byte
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		data := batch
		// ResponseWriter.Write 不保留切片——写完后底层数组可复用，
		// 批次缓冲在整条流上只分配一次并随压力增长。
		batch = batch[:0]
		return out.writeContent(data)
	}
	for {
		var event llm.ResponseEvent
		var err error
		if len(prelude) > 0 {
			event = prelude[0]
			prelude = prelude[1:]
			if len(prelude) == 0 {
				err = preludeErr
			}
		} else if len(batch) > 0 {
			// 上批未落盘说明上游供给不断：非阻塞再取一帧并入同批；
			// channel 暂时空了才 flush，突发流量摊薄 syscall。
			select {
			case item, ok := <-items:
				if !ok {
					err = io.EOF
				} else {
					event, err = item.event, item.err
				}
			default:
				if wErr := flush(); wErr != nil {
					return latest, wErr
				}
				continue
			}
		} else {
			event, err = out.awaitEvent(ctx, items, ticker)
		}
		// 取消收口：ctx 取消后无论本轮拿到的是正常事件、物化的取消错误
		// 事件（errors.New 重建后错误链已丢）还是 channel 关闭的 EOF，
		// 统一按 context.Cause 归因——泵的投递 select 与 awaitEvent 的
		// 接收 select 在两路就绪时随机选，不查 ctx 会把断连随机记成
		// completed/failed/disconnected。
		if ctx.Err() != nil {
			return latest, context.Cause(ctx)
		}
		if errors.Is(err, io.EOF) {
			if wErr := flush(); wErr != nil {
				return latest, wErr
			}
			return latest, nil
		}
		if err != nil {
			if wErr := flush(); wErr != nil {
				return latest, wErr
			}
			return latest, err
		}
		latest = eventMessage(event, latest)
		if event.Type == llm.ResponseEventError && event.Error != nil {
			// 流内错误事件已没有 HTTP 头可用——把调试引用编进错误 JSON，
			// 让客户端（含 WS 帧）自身携带定位键。
			event.Error.DebugRef = debugRef(recorder)
		}
		encodedEvents, encodeErr := encoder.Encode(event)
		if encodeErr != nil {
			if wErr := flush(); wErr != nil {
				return latest, wErr
			}
			return latest, encodeErr
		}
		for _, encoded := range encodedEvents {
			batch = protocol.AppendSSE(batch, encoded.Name, encoded.Data)
			if encoded.Name == "[DONE]" {
				recorder.AppendJSONL("06-http-response.jsonl", encoded.Name, string(encoded.Data))
			} else {
				recorder.AppendJSONL("06-http-response.jsonl", encoded.Name, json.RawMessage(encoded.Data))
			}
		}
		if event.Type == llm.ResponseEventError {
			// 错误 SSE 已进批次，先落盘再返回错误供外层记录失败日志。
			if wErr := flush(); wErr != nil {
				return latest, wErr
			}
			if event.Error != nil && event.Error.ErrorMessage != "" {
				return latest, errors.New(event.Error.ErrorMessage)
			}
			return latest, errors.New("response stream returned an error event")
		}
	}
}

// collectPumpedMessage 从泵 channel 收集非流式最终消息。等待期间按
// ticker 节奏写 heartbeat 载荷：非流式请求在上游长思考窗口内完全无字节，
// Codex 约 30s 弃连、网关有自己的首字节超时——"\n" 是 JSON 响应体的
// 合法前导空白，心跳不污染最终文档。
func collectPumpedMessage(ctx context.Context, out *streamWriter, items <-chan pumpItem, ticker *time.Ticker) (*llm.AssistantMessage, error) {
	var final *llm.AssistantMessage
	for {
		event, err := out.awaitEvent(ctx, items, ticker)
		// 取消收口同 writeProtocolStream：物化取消事件与 channel 关闭
		// 的 EOF 一律归因 context.Cause，断连不会被记成普通失败。
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		if errors.Is(err, io.EOF) {
			if final == nil {
				return nil, errors.New("response stream ended without a final message")
			}
			return final, nil
		}
		if err != nil {
			return nil, err
		}
		switch event.Type {
		case llm.ResponseEventDone:
			if event.Message == nil {
				return nil, errors.New("done event has no final message")
			}
			final = event.Message
		case llm.ResponseEventError:
			if event.Error == nil {
				return nil, errors.New("error event has no error message")
			}
			return nil, errors.New(event.Error.ErrorMessage)
		}
	}
}

func eventMessage(event llm.ResponseEvent, fallback *llm.AssistantMessage) *llm.AssistantMessage {
	if event.Message != nil {
		return event.Message
	}
	if event.Error != nil {
		return event.Error
	}
	if event.Partial != nil {
		return event.Partial
	}
	return fallback
}
