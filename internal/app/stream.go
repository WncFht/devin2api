// 本文件是流式响应子系统：泵协程驱动上游事件流、SSE 写出方、保活帧，
// 以及非流式请求的事件收集。编排入口 createCompletion 见 app.go。
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
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

// sseWriteDeadline 是单次 SSE 写出（一次 Write+Flush）的预算：写出前把
// 底层 conn 的写 deadline 续到该点，死读客户端造成的阻塞写最多挂起这么久。
// 区别于响应级 writeTimeout（app.go 恒 0）——它是逐写续约而非绝对截止，
// 整条流的寿命不设上限；与 WS 侧 wsWriteDeadline 同语义。
// var 而非 const：测试会缩短它来验证超时断连路径。
var sseWriteDeadline = 60 * time.Second

// sseKeepalive 是 SSE 注释行：协议合法、SSE 客户端解析器忽略，
// 仅用于刷新链路上各段的空闲计时器。
var sseKeepalive = []byte(": keepalive\n\n")

// streamWriter 是流式响应的唯一写出方。两个提交位回答不同问题：
// committed 标记是否已尝试过写出——一次写尝试后无论成败连接多半已死，
// HTTP 状态行不再可改，错误只能走带内事件/错误体；delivered 标记是否有
// 字节真正写出——断连按它分 499/200：什么都没送达时记 499 才是线上实况。
type streamWriter struct {
	writer   http.ResponseWriter
	recorder *debuglog.Recorder
	// conn 是写出所经的网络连接：写前武装 SO_LINGER(0)，让传输级写失败时
	// net/http 在 chunkWriter.Write 里的同步 close 与本层主动 close 都改发
	// RST；nil（无 ConnContext 注入的 server、非 TCP conn）时跳过武装，
	// 退回 graceful close。
	conn      lingerConn
	committed bool
	delivered bool
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

// connContextKey 把本条网络连接挂进请求 ctx：写路径要把 SO_LINGER 武装成
// RST 强拆，这是 handler 侧不 hijack 就能触到 conn 的唯一通道（由
// HTTPServer 的 ConnContext 注入）。
type connContextKey struct{}

// connContext 是 http.Server.ConnContext：把接受下来的 conn 放进请求 ctx。
func connContext(ctx context.Context, conn net.Conn) context.Context {
	return context.WithValue(ctx, connContextKey{}, conn)
}

// lingerConn 是写路径对连接的最低要求——*net.TCPConn 满足；TLS 包装与
// 测试假连接不满足时跳过 RST 武装，退回 graceful close 语义。
type lingerConn interface {
	SetLinger(int) error
	Close() error
}

// requestConn 从请求 ctx 取回连接；取不到（非 TCP 实现）返回 nil。
func requestConn(ctx context.Context) lingerConn {
	conn, _ := ctx.Value(connContextKey{}).(lingerConn)
	return conn
}

// write 写一段响应体并立即 flush。committed 在写尝试前置位——一次写
// 尝试无论成败，响应行都不再可改；delivered 只在 Write 成功后置位，
// 断连归因按它区分「什么都没上链路」（499）与「响应行已提交」（200）。
func (out *streamWriter) write(p []byte) error {
	out.committed = true
	// 单次写出有界：写前把 conn 级写 deadline 续到预算点。续约失败不阻断
	// 写出——ErrNotSupported 说明写出方自己管写期限（WS 经 gorilla 的
	// SetWriteDeadline，见 websocket.go）；真实 conn 错误会随后由 Write
	// 原样报出，不在这里另开错误面。若中途 writer 被换成自定义实现，
	// 该调用静默退化为无 deadline，不影响正确性。
	rc := http.NewResponseController(out.writer)
	_ = rc.SetWriteDeadline(time.Now().Add(sseWriteDeadline))
	// 写前武装 SO_LINGER(0)：写失败时 net/http 在 chunkWriter.Write 内同步
	// 做 fd 级 graceful close，FIN 会排在数 MB 未发队列后——socket 成为
	// FIN_WAIT_1 孤儿（内核重传到放弃约 15min，期间发送队列占着内核内存，
	// 客户端也看不到流已死），且 fd 已关闭后无法补设 linger。提前武装让
	// 那次 close 改发 RST：RST 不受对端窗口约束即刻送达，两端 socket 与
	// 发送队列立即回收。写成功立即撤除——conn 回 keep-alive 池不能带
	// 武装，否则之后的正常关闭会把尾包截断成 RST；写失败路径上撤除打在
	// 死 fd 上无害落空（实测返回 use of closed network connection）。
	if out.conn != nil {
		_ = out.conn.SetLinger(0)
	}
	_, err := out.writer.Write(p)
	if err == nil {
		out.delivered = true
		out.bytes += len(p)
		out.recorder.AddClientBytes(int64(len(p)))
		// Flush 必须取错误返回版：单次写载荷 <conn.bufw（4KB）时真正的
		// socket syscall 只发生在 Flush 内，而 http.Flusher.Flush 无返回值，
		// bufio 层失败会被静默吞掉。RC 沿 Unwrap/FlushError 链取回
		// *http.response.FlushError 的真实传输错误。
		err = rc.Flush()
	}
	if out.conn != nil {
		if err != nil {
			// Flush 路径失败时 net/http 只记 conn.werr + cancelCtx，不同步
			// 关 fd——handler 返回后的 graceful close 仍把 FIN 排在未发
			// 队列后，孤儿形态照旧。linger(0) 尚在武装位，这里主动 close
			// 让 Flush 路径同样发 RST；Write 路径 fd 已被 net/http 同步关过，
			// 二次 close 无害落空。
			_ = out.conn.Close()
		}
		_ = out.conn.SetLinger(-1)
	}
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			// 写超时 = 死读客户端：conn 仍活着但不再消费字节，等价于断连。
			// 包进 context.DeadlineExceeded 让外层三处取消收口统一按
			// disconnected/499 归因（ctx 此刻并未取消，裸 i/o timeout
			// 会被错记成 failed/http_stream）；原错误留在 Unwrap 链里取证。
			return fmt.Errorf("%w: %w", context.DeadlineExceeded, err)
		}
		return err
	}
	return nil
}

// writeContent 写一段协议内容帧（区别于 SSE 保活注释），
// 并标记「首个客户端可见字节」时间点。
func (out *streamWriter) writeContent(p []byte) error {
	out.recorder.NoteClientLatency()
	return out.write(p)
}

// finishDisconnected 收口客户端断连的统一归因：结果记 disconnected；
// 状态码按线上实况——已有字节送达记 200（响应行已发出，断连不伪装成
// 5xx），什么都没送达记 499（nginx 约定的客户端关闭）。err 是调用方
// 手上的断连证据：ctx 取消原因、与取消竞速到达的上游错误、或写出失败。
// 取消原因是 drain 强掐（errDrainKill）时归 drain_timeout——进程部署
// 掐断不污染客户端断连口径。
func (out *streamWriter) finishDisconnected(completion *debuglog.Completion, err error) {
	completion.Result = "disconnected"
	if out.delivered {
		completion.StatusCode = http.StatusOK
	} else {
		completion.StatusCode = 499
	}
	stage := debuglog.ErrStageClientDisconnected
	if errors.Is(err, errDrainKill) {
		stage = debuglog.ErrStageDrainTimeout
	}
	out.recorder.WriteError(stage, err)
}

// sseEventSink 是写出方的事件级下沉口：实现者（WS 写出方）按编码后的
// (name, data) 直接收事件，不再经 SSE 文本渲染与回解往返。写出方不
// 实现它时事件仍走 AppendSSE 文本帧。
type sseEventSink interface {
	WriteSSEEvent(name string, data []byte) error
}

// writeEvent 把一条编码后事件直交 sink，与 write 同等地记提交位与
// 字节数——对 WS 而言「送达」就是已有事件帧写到连接上。
func (out *streamWriter) writeEvent(sink sseEventSink, name string, data []byte) error {
	out.recorder.NoteClientLatency()
	out.committed = true
	if err := sink.WriteSSEEvent(name, data); err != nil {
		return err
	}
	out.delivered = true
	out.bytes += len(data)
	out.recorder.AddClientBytes(int64(len(data)))
	return nil
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
				if event.Type == llm.ResponseEventError && event.Error != nil {
					// 流内错误事件已没有 HTTP 头可用——把调试引用编进错误
					// JSON 让客户端自身携带定位键。必须在入队前打标：
					// 入队后终止指针无后续写入，worker 投影读到最终值。
					event.Error.DebugRef = debugRef(recorder)
				}
				// 事件投影推迟到日志 worker 求值——RecordResponseEvent 打
				// thunk；Partial 是逐帧快照、终止指针无后续写入，无竞态。
				recorder.RecordResponseEvent(event)
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
	if _, ok := writer.(http.Flusher); !ok {
		completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPStream, http.StatusInternalServerError, errors.New("streaming response writer does not support flushing"))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	out := &streamWriter{writer: writer, recorder: recorder, heartbeat: sseKeepalive, conn: requestConn(ctx)}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	items := startStreamPump(streamCtx, application.adapter, messages, recorder)
	ticker := time.NewTicker(keepaliveInterval)
	defer ticker.Stop()

	firstEvent, firstErr := out.awaitEvent(streamCtx, items, ticker)
	if errors.Is(firstErr, io.EOF) && streamCtx.Err() == nil {
		// 上游在首事件前裸 EOF（adapter Recv 契约允许）：无内容可下发，
		// 包上显式语义走下方统一错误出口——下 200 空流会把上游故障记成
		// completed。取消竞态导致的 channel 关闭不改写，进块内由 ctx
		// 检查按断连归因。
		firstErr = fmt.Errorf("response stream ended before the first event: %w", firstErr)
	}
	if firstErr != nil {
		// 建流后首事件前的断连/中止先按取消归因——context.Canceled 会被
		// 分类记录映成 499，走 writeLoggedError 就把断连
		// 记成了 failed（对照 app.go 非流式路径的 client_disconnected 分支）。
		if errors.Is(firstErr, context.Canceled) || errors.Is(firstErr, context.DeadlineExceeded) || streamCtx.Err() != nil {
			// ctx 已取消时 Cause 是权威归因（abort 原因/取消语义），
			// firstErr 可能只是 channel 关闭物化出的裸 EOF。
			cause := firstErr
			if streamCtx.Err() != nil {
				cause = context.Cause(streamCtx)
			}
			out.finishDisconnected(completion, cause)
			return
		}
		firstFailure := llm.Classify(firstErr)
		status := common.HTTPStatus(firstFailure)
		if !out.committed && (!protocol.StreamErrorEvents() || status != http.StatusTooManyRequests) {
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageProviderStream, status, firstErr)
			return
		}
		// OpenAI 流式面上的限流是唯一转流内事件的 pre-stream 失败：
		// Codex 对 HTTP 429 一律终止（codex-rs retry_429 硬编码 false，
		// 5xx/transport 反而照常重试），只有流内错误事件进它的重试
		// 循环；确定性错误（4xx）重试无意义，保留真实状态码供下游
		// 网关分类。Anthropic 面不在此列——其客户端按 HTTP 状态码
		// 重试，提交 200 反而把失败降级为不可重试的畸形响应。
		firstEvent = llm.ResponseEvent{Type: llm.ResponseEventError, Reason: llm.StopReasonError,
			Error: &llm.AssistantMessage{ErrorMessage: firstErr.Error(), Failure: firstFailure, DebugRef: debugRef(recorder)}}
		firstErr = nil
	}
	prelude := []llm.ResponseEvent{firstEvent}
	if !out.committed && firstEvent.Type == llm.ResponseEventError {
		// 上游把断连物化成首事件错误时同样按取消归因——泵投递与 ctx
		// 完成存在竞态（取消可能先落成错误事件再被看见），ctx 是兜底。
		if streamCtx.Err() != nil {
			out.finishDisconnected(completion, context.Cause(streamCtx))
			return
		}
		failure := llm.FailureOf(firstEvent.Error)
		if failure.Error() == "" {
			failure.Message = "response stream returned an error event immediately"
		}
		status := common.HTTPStatus(failure)
		if protocol.StreamErrorEvents() && (failure.ContextLength || status == http.StatusTooManyRequests) {
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
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageProviderStream, status, failure)
			return
		}
	}

	completion.StatusCode = http.StatusOK
	// 回显客户端原始请求名而非 redirect 后的内部名（RequestedModel 在
	// 注册表改写前采样）——客户端不能看到自己没请求的模型名。
	message, streamErr := writeProtocolStream(streamCtx, out, items, ticker, recorder, protocol, strings.TrimSpace(completion.RequestedModel), options, prelude, firstErr)
	updateCompletionIdentity(completion, messages, message)
	*responseBytes += out.bytes
	if streamErr != nil {
		streamFailure := llm.Classify(streamErr)
		noteRetryAfter(recorder, streamFailure)
		// 流内错误事件下发的限流 HTTP 状态仍是 200——按记录语义补标，
		// 责任归因与 429 采样才不会把这批限流漏成普通失败。
		if streamFailure.RateLimited {
			recorder.SetRateLimited()
		}
		switch {
		case errors.Is(streamErr, context.Canceled), errors.Is(streamErr, context.DeadlineExceeded), streamCtx.Err() != nil:
			// ctx 取消收口：与取消竞速到达的写出/编码错误让位给取消原因——
			// Cause 是权威归因（abort/drain 语义钉在原因链上，写死连接
			// 物化的 broken pipe 不盖住它）。
			cause := streamErr
			if streamCtx.Err() != nil {
				cause = context.Cause(streamCtx)
			}
			out.finishDisconnected(completion, cause)
		case !out.committed:
			// 首字节前的失败（如编码器错误）：响应行还没提交成 200，
			// 按真实状态码下发，不能让客户端拿到「200 + 空流」。
			completion.StatusCode = writeLoggedError(writer, recorder, protocol, debuglog.ErrStageHTTPStream, common.HTTPStatus(streamFailure), streamErr)
		default:
			recorder.WriteError(debuglog.ErrStageHTTPStream, streamErr)
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
	encoder := protocol.NewStreamEncoder(model, options)
	// WS 写出方按事件下沉：SSEEvent 直交 sink，省掉文本渲染与回解往返；
	// HTTP 写出方不实现该接口，事件照常并入 SSE 批次。
	sink, _ := out.writer.(sseEventSink)
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
		encodedEvents, encodeErr := encoder.Encode(event)
		if encodeErr != nil {
			if wErr := flush(); wErr != nil {
				return latest, wErr
			}
			return latest, encodeErr
		}
		for _, encoded := range encodedEvents {
			if sink != nil {
				if err := out.writeEvent(sink, encoded.Name, encoded.Data); err != nil {
					return latest, err
				}
			} else {
				batch = protocol.AppendSSE(batch, encoded.Name, encoded.Data)
			}
			if encoded.Name == common.SSEDone {
				recorder.AppendJSONL(debuglog.StageHTTPResponse, encoded.Name, string(encoded.Data))
			} else {
				recorder.AppendJSONL(debuglog.StageHTTPResponse, encoded.Name, json.RawMessage(encoded.Data))
			}
		}
		if event.Type == llm.ResponseEventError {
			// 错误 SSE 已进批次，先落盘再返回错误供外层记录失败日志。
			if wErr := flush(); wErr != nil {
				return latest, wErr
			}
			// 分类记录随车返回——Cause 链（context.Canceled 等）与
			// 生产侧结构字段不再经文本重推。
			if failure := llm.FailureOf(event.Error); failure.Error() != "" {
				return latest, failure
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
			return nil, llm.FailureOf(event.Error)
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
