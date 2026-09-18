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
	"runtime"
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
	srv := &http.Server{ConnContext: smallBufferConnContext, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	var opErr *net.OpError
	switch {
	case errors.Is(writeResult, context.DeadlineExceeded) && errors.Is(writeResult, os.ErrDeadlineExceeded):
		// 预算语义是「单次阻塞写最多挂起这么久」：错误类型已锁定 i/o
		// timeout，耗时上限确认续约生效、下限确认确实阻塞过而非早夭。
		if elapsed < sseWriteDeadline || elapsed > 10*sseWriteDeadline {
			t.Fatalf("blocked write elapsed = %v, want ≈%v", elapsed, sseWriteDeadline)
		}
	case runtime.GOOS == "windows" && errors.As(writeResult, &opErr):
		// Windows loopback 对死读 peer 的发送可能被内核直接快败
		// （WSAENOBUFS/WSAECONNABORTED 族），写根本进不了阻塞等待，
		// 逐写 deadline 无从触发——OS 自己切断了写。传输层错误同样
		// 证明「写不会挂死」，conn 拆除仍由下方客户端读断言钉住。
		t.Logf("windows: dead-peer write failed fast without reaching deadline: %v", writeResult)
	default:
		t.Fatalf("write err = %v, want i/o timeout wrapped as DeadlineExceeded", writeResult)
	}
	expectConnReset(t, conn)
}

// smallBufferConnContext 在 connContext 注入前收窄服务端发送缓冲：写阻塞
// 发生在在途字节填满（客户端窗口 + 服务端 SO_SNDBUF）之后，各平台
// SO_SNDBUF 默认差异大（Linux loopback 自动调到 MB 级），收窄后一两次
// 写出即进入阻塞，deadline 触发时机不依赖平台缓冲水位。
func smallBufferConnContext(ctx context.Context, conn net.Conn) context.Context {
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetWriteBuffer(4096)
	}
	return connContext(ctx, conn)
}

// expectConnReset 断言死读客户端观察到 conn 已被拆除：linger(0) 武装的
// close 发 RST，客户端读 ECONNRESET。若 linger 未生效或未同步 close，
// graceful close 把 FIN 排在未发队列后——读会撞上读 deadline 拿到
// i/o timeout（FIN-orphan 的客户端侧形态）。Windows 的栈对 RST 拆除
// 交付真实 WSA errno：WSAECONNRESET（10054）为主，WSAECONNABORTED
// （10053）是同族变体。
func expectConnReset(t *testing.T, conn net.Conn) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := io.Copy(io.Discard, conn)
	if errors.Is(err, syscall.ECONNRESET) {
		return
	}
	if runtime.GOOS == "windows" {
		// Windows 的 syscall.ECONNRESET/ECONNABORTED 是 ≥1<<29 的虚构
		// 值（zerrors_windows.go APPLICATION_ERROR 段），与 wsarecv
		// 实际交付的 WSA errno 永不相等——errors.Is 的 == 比对必假，
		// 须解包到底层 Errno 按数值比对（net/http http2 的
		// isClosedConnError 同法）。io.Copy 经 TCPConn.WriteTo 时链形
		// 是 writeto→read→wsarecv 三层包裹，Unwrap 照常落底。
		const (
			wsaECONNABORTED = 10053
			wsaECONNRESET   = 10054
		)
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == wsaECONNRESET || errno == wsaECONNABORTED) {
			return
		}
	}
	t.Fatalf("client read err = %v, want ECONNRESET (RST teardown)", err)
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
	// 与生产 server 的构造完全一致。ConnContext 在 Serve 前换成收窄
	// 发送缓冲版：写阻塞时机不依赖平台 SO_SNDBUF 水位。
	srv := application.HTTPServer()
	srv.ConnContext = smallBufferConnContext
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	body := []byte(`{"model":"gpt-test","stream":true,"input":"hi"}`)
	request := []byte(fmt.Sprintf("POST /v1/responses HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body))
	conn := dialDeadReader(t, ln.Addr().String(), request)

	// 在 logs 行落账前客户端一个字节都不读：一旦 recv 打开接收窗口，
	// 服务端写就不再阻塞、写 deadline 永不触发——旧的「先 io.Copy 再
	// 等行」顺序下，测试成败取决于泵产速与客户端 drain 速的竞速，
	// -race/慢 CI 上消费端追平生产端时写永不阻塞（实测 i/o timeout
	// 取代 ECONNRESET，请求挂到客户端读 deadline 才死）。
	expire := time.Now().Add(15 * time.Second)
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
			break
		}
		if time.Now().After(expire) {
			t.Fatalf("log row not written; rows = %v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// logs 行只在 handler 退出后落账：行已出现即证明阻塞写被逐写
	// deadline 掐死、conn 已在 linger(0) 武装位上 close——客户端读
	// ECONNRESET（RST 强拆），而非 FIN-orphan 的悬挂/超时形态。
	expectConnReset(t, conn)
}

// TestDisconnectCause 验证断连归因的复合包裹：ctx 已取消时 context.Cause
// 是权威原因（errors.Is 必须穿透到 errDrainKill 保住 drain_timeout 归因），
// 同时竞速到达的物化错误（i/o timeout 等传输细节）留在 message 与
// Unwrap 链里取证；同文错误自重复时只留 cause。
func TestDisconnectCause(t *testing.T) {
	writeErr := fmt.Errorf("%w: %w", context.DeadlineExceeded, &net.OpError{Op: "write", Err: os.ErrDeadlineExceeded})

	t.Run("ctx live keeps surfaced", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if got := disconnectCause(ctx, writeErr); got != writeErr {
			t.Fatalf("got %v, want surfaced unchanged", got)
		}
	})

	t.Run("cancelled composes cause and surfaced", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errDrainKill)
		got := disconnectCause(ctx, writeErr)
		if !errors.Is(got, errDrainKill) {
			t.Fatalf("errors.Is(errDrainKill) = false on %v — drain_timeout attribution lost", got)
		}
		if !errors.Is(got, os.ErrDeadlineExceeded) {
			t.Fatalf("errors.Is(os.ErrDeadlineExceeded) = false on %v — write detail lost", got)
		}
		for _, want := range []string{"drain timeout", "i/o timeout"} {
			if !strings.Contains(got.Error(), want) {
				t.Fatalf("message %q missing %q", got.Error(), want)
			}
		}
	})

	t.Run("same-text surfaced deduped", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if got := disconnectCause(ctx, context.Canceled); got != context.Cause(ctx) {
			t.Fatalf("got %q, want bare cause (no self-dup)", got.Error())
		}
	})

	t.Run("nil surfaced returns cause", func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(errDrainKill)
		if got := disconnectCause(ctx, nil); !errors.Is(got, errDrainKill) {
			t.Fatalf("got %v, want errDrainKill", got)
		}
	})
}
