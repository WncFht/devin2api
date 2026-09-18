// 本文件验证 SSE 写路径的逐写有界 deadline：死读客户端（conn 存活但
// 永不消费字节）造成的阻塞写必须在 sseWriteDeadline 内报 i/o timeout，
// 并按断连归因（disconnected/client_disconnected），而不是挂死泵协程
// 与上游流，或误记 failed/http_stream。
package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// dialDeadReader 发出请求后永不读取：内核接收缓冲填满后服务端写即阻塞，
// 模拟真实场景里「客户端进程卡死/崩溃但 conn 没关」的下游——它是
// 42min+ 挂起流的成因形态（读端死，ctx 不取消，写端干等）。
func dialDeadReader(t *testing.T, addr string, request []byte) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// 收窄接收缓冲让服务端尽早进入阻塞写，不必等慢启动窗口涨满。
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetReadBuffer(4096)
	}
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	return conn
}

// TestStreamWriteDeadlineCutsBlockedWrite 验证逐写 deadline 切断真实阻塞
// 写：写失败必须 ≈预算时刻到达、错误是包进 context.DeadlineExceeded 的
// i/o timeout，且 conn 被武装 linger 后的 RST 立即拆掉（客户端读到
// ECONNRESET，而非 FIN 排在发送队列后的「连接假活」）。
// 256KB 单次写超过两级 bufio（response 2KB + conn.bufw 4KB），阻塞的
// socket syscall 落在 Write 内——chunkWriter.Write 的同步 rwc.Close
// 在武装窗口内发 RST。
func TestStreamWriteDeadlineCutsBlockedWrite(t *testing.T) {
	assertDeadReaderRST(t, bytes.Repeat([]byte("x"), 256<<10))
}

// TestStreamWriteDeadlineFlushPathCutsBlockedWrite 覆盖 flush 路径：
// 1KB 单次写全程留在两级 bufio 里，真正的 socket syscall 只发生在
// Flush 的 conn.bufw.Flush 内——那里失败 net/http 只记 werr+cancelCtx
// 不同步关 fd，写路径必须自己在 linger(0) 武装位上 close 才发得出
// RST。真实 SSE delta 通常 <2KB，这条才是主形态。
func TestStreamWriteDeadlineFlushPathCutsBlockedWrite(t *testing.T) {
	assertDeadReaderRST(t, bytes.Repeat([]byte("x"), 1024))
}

// assertDeadReaderRST 驱动死读客户端断言共用收束：阻塞写 ≈预算时刻报
// DeadlineExceeded 包裹的 i/o timeout，且 conn 被 RST 强拆（客户端
// ECONNRESET，而非 FIN 排队后的「连接假活」）。
func assertDeadReaderRST(t *testing.T, chunk []byte) {
	t.Helper()
	old := sseWriteDeadline
	sseWriteDeadline = 200 * time.Millisecond
	t.Cleanup(func() { sseWriteDeadline = old })

	writeErr := make(chan error, 1)
	// 手工建 server 而非 httptest：RST 武装依赖 ConnContext 注入的 conn。
	srv := &http.Server{ConnContext: connContext, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 与 /v1 真实链路同构地包一层 gateHeaderWriter——顺带验证 Unwrap
		// 让 ResponseController 穿透包装层落到 conn 级 SetWriteDeadline。
		_, gate := adapter.WithGateContext(r.Context(), "fg")
		wrapped := &gateHeaderWriter{ResponseWriter: w, gate: gate}
		out := &streamWriter{writer: wrapped, conn: requestConn(r.Context())}
		var err error
		for err == nil {
			err = out.write(chunk)
		}
		writeErr <- err
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	started := time.Now()
	conn := dialDeadReader(t, ln.Addr().String(), []byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n"))
	var writeResult error
	select {
	case writeResult = <-writeErr:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked write did not unblock within deadline budget")
	}
	elapsed := time.Since(started)
	if !errors.Is(writeResult, context.DeadlineExceeded) || !errors.Is(writeResult, os.ErrDeadlineExceeded) {
		t.Fatalf("write err = %v, want i/o timeout wrapped as DeadlineExceeded", writeResult)
	}
	// 预算语义是「单次阻塞写最多挂起这么久」：错误类型已锁定 i/o
	// timeout，耗时上限确认续约生效、下限确认确实阻塞过而非早夭。
	if elapsed < sseWriteDeadline || elapsed > 10*sseWriteDeadline {
		t.Fatalf("blocked write elapsed = %v, want ≈%v", elapsed, sseWriteDeadline)
	}
	// 写超时路径靠武装 linger(0) 让 close 发 RST 强拆：客户端读到
	// ECONNRESET。若 linger 未生效或未同步 close，graceful close 把
	// FIN 排在未发队列后——死读客户端的读只会先撞上自己的读
	// deadline，拿到的 i/o timeout 同样过不了 ECONNRESET 断言。
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.Copy(io.Discard, conn); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("client read err = %v, want ECONNRESET (RST teardown)", err)
	}
}

// endlessDeltaAdapter 产出无限 text_delta 事件流：泵持续供给让写出方在
// 死读客户端上必然走入阻塞写。Recv 尊重 ctx 取消（真实 adapter 契约）。
type endlessDeltaAdapter struct{ chunk string }

func (a *endlessDeltaAdapter) Stream(context.Context, llm.RequestMessages) (llm.ResponseStream, error) {
	return &endlessDeltaStream{chunk: a.chunk}, nil
}

func (a *endlessDeltaAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "gpt-test", Created: 1, OwnedBy: "test"}}, nil
}

type endlessDeltaStream struct {
	chunk string
	seq   int
}

func (s *endlessDeltaStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	select {
	case <-ctx.Done():
		return llm.ResponseEvent{}, ctx.Err()
	default:
	}
	s.seq++
	partial := &llm.AssistantMessage{
		ResponseID: "resp-1", StopReason: llm.StopReasonPending,
		Content: []llm.Content{llm.TextContent{Text: s.chunk}},
	}
	switch s.seq {
	case 1:
		return llm.ResponseEvent{Type: llm.ResponseEventStart, Partial: partial}, nil
	case 2:
		return llm.ResponseEvent{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial}, nil
	}
	return llm.ResponseEvent{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: s.chunk, Partial: partial}, nil
}

// TestStreamCompletionDeadReaderRecordsDisconnected 端到端验证归因：死读
// 客户端的写超时与中途断连同口径——logs 行 result=disconnected、
// error_stage=client_disconnected，不是 failed/http_stream。status 按
// delivered 分 499/200：哪次写出先阻塞取决于内核缓冲水位，两值都合法。
func TestStreamCompletionDeadReaderRecordsDisconnected(t *testing.T) {
	old := sseWriteDeadline
	sseWriteDeadline = 200 * time.Millisecond
	t.Cleanup(func() { sseWriteDeadline = old })

	st := openTokenDB(t)
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, st)
	t.Cleanup(func() { manager.Close() })
	application := New(&endlessDeltaAdapter{chunk: strings.Repeat("x", 64<<10)},
		config.ServerConfig{Listen: ":0"}, manager)

	// 走 application.HTTPServer()（带 ConnContext）+ 手工 listener，
	// 与生产 server 的构造完全一致。
	srv := application.HTTPServer()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	body := []byte(`{"model":"gpt-test","stream":true,"input":"hi"}`)
	request := []byte(fmt.Sprintf("POST /v1/responses HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	conn := dialDeadReader(t, ln.Addr().String(), request)

	// conn 被服务端 RST 强拆（写超时 → linger(0) 武装 → handler 返回后
	// net/http close 发 RST）即端到端证据：挂起流被逐写 deadline 拆掉，
	// 泵/槽位随之释放。客户端读到 ECONNRESET；若 linger 未生效，
	// graceful close 会先把积压数据 drain 完再给干净 EOF，同样通不过
	// 本断言（RST 与 FIN 的区分见上个用例）。
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.Copy(io.Discard, conn); !errors.Is(err, syscall.ECONNRESET) {
		t.Fatalf("client read err = %v, want ECONNRESET (RST teardown)", err)
	}

	expire := time.Now().Add(5 * time.Second)
	for {
		rows, _, err := st.SearchLogs(context.Background(), store.LogQuery{})
		if err != nil {
			t.Fatalf("SearchLogs: %v", err)
		}
		if len(rows) == 1 {
			row := rows[0]
			if row.Result != "disconnected" || row.ErrorStage != debuglog.ErrStageClientDisconnected {
				t.Fatalf("log row = %+v, want disconnected/client_disconnected", row)
			}
			if row.StatusCode != http.StatusOK && row.StatusCode != 499 {
				t.Fatalf("status = %d, want 200 or 499 (delivered follows kernel buffer fill)", row.StatusCode)
			}
			return
		}
		if time.Now().After(expire) {
			t.Fatalf("log row not written; rows = %v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
