// 本文件钉住「泵退场交班 → 消费方排干 → unwind」的脱钩判定定序：
// 客户端断连后 metaJSON 定稿必须 happens-after 泵侧末次 Recv
// （stream.mu 下完成脱钩/杀泵判定）。旧实现里泵在投递点命中
// ctx.Done 直接退场，判定甩给客户端哨兵；哨兵调度迟到越过
// unwind→Complete→metaJSON 时 meta.detached_events 静默丢失
// （recon-detached-flake 的 ~2.5% flake 成因）。
package app

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// handoffStream 把「末次 Recv 见到已取消 ctx」暴露给测试：ctx 存活
// 期间按 start→text_start→text_delta 序无限产事件（灌满泵投递缓冲后
// 泵停在投递 select）；首次以取消态进入的 Recv 即交班——handoffGate
// 非空时挂起它来模拟判定耗时，返回前关 handedOff。
type handoffStream struct {
	handoffGate chan struct{}
	handedOff   chan struct{}
	once        sync.Once
	seq         int
}

func (s *handoffStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	if ctx.Err() != nil {
		if s.handoffGate != nil {
			<-s.handoffGate
		}
		s.once.Do(func() { close(s.handedOff) })
		return llm.ResponseEvent{}, ctx.Err()
	}
	s.seq++
	partial := &llm.AssistantMessage{
		ResponseID: "resp-1", StopReason: llm.StopReasonPending,
		Content: []llm.Content{llm.TextContent{Text: "x"}},
	}
	switch s.seq {
	case 1:
		return llm.ResponseEvent{Type: llm.ResponseEventStart, Partial: partial}, nil
	case 2:
		return llm.ResponseEvent{Type: llm.ResponseEventTextStart, ContentIndex: 0, Partial: partial}, nil
	}
	return llm.ResponseEvent{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "x", Partial: partial}, nil
}

type handoffAdapter struct{ stream *handoffStream }

func (a *handoffAdapter) Stream(context.Context, llm.RequestMessages) (llm.ResponseStream, error) {
	return a.stream, nil
}

func (a *handoffAdapter) ListModels(context.Context) ([]adapter.ModelInfo, error) {
	return []adapter.ModelInfo{{ID: "gpt-test", Created: 1, OwnedBy: "test"}}, nil
}

// gatedWriter 在 writeGate 关闭前阻塞所有写出：消费方停在
// out.write 内不再读 items，泵灌满缓冲后停在投递 select——此时
// 取消客户端 ctx 让 ctx.Done 成为泵的唯一就绪分支。
type gatedWriter struct {
	header       http.Header
	writeGate    chan struct{}
	writeEntered chan struct{}
	once         sync.Once
}

func (w *gatedWriter) Header() http.Header { return w.header }
func (w *gatedWriter) WriteHeader(int)     {}
func (w *gatedWriter) Flush()              {}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.writeEntered) })
	<-w.writeGate
	return len(p), nil
}

// TestStreamPumpHandsOffDecisionOnCancel 验证泵的投递点 ctx.Done 出口
// 先跑交班 Recv 再退场：不读 items 让缓冲灌满、泵停在投递 select，
// 取消后 handedOff 必须先于 close(items) 发生。旧实现直接 return，
// handedOff 永不关闭，按超时败露。
func TestStreamPumpHandsOffDecisionOnCancel(t *testing.T) {
	st := openTokenDB(t)
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, st)
	t.Cleanup(manager.Close)

	stream := &handoffStream{handedOff: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	items := startStreamPump(ctx, &handoffAdapter{stream: stream}, llm.RequestMessages{},
		manager.Start(debuglog.RequestMeta{API: "test"}))
	// 无消费方：缓冲（cap 8）灌满后泵停在投递 select。留出灌满+停稳
	// 的时间再取消，保证 ctx.Done 是唯一就绪分支。
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-stream.handedOff:
	case <-time.After(5 * time.Second):
		t.Fatal("pump exited via ctx.Done without handoff Recv")
	}
	for range items {
	}
}

// TestStreamCompletionUnwindAfterPumpHandoff 验证消费侧定序：交班
// Recv 被闸住期间 streamCompletion 不得返回——drain 等 close(items)、
// close 等泵退、泵退等交班，三者串成 unwind happens-after 判定。
// 旧实现的 defer cancel() 不等泵退场，闸住窗口内 returned 立即触发。
func TestStreamCompletionUnwindAfterPumpHandoff(t *testing.T) {
	st := openTokenDB(t)
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, st)
	t.Cleanup(manager.Close)

	stream := &handoffStream{handoffGate: make(chan struct{}), handedOff: make(chan struct{})}
	application := New(&handoffAdapter{stream: stream}, config.ServerConfig{Listen: ":0"}, manager)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/responses", API: "openai-responses"})

	ctx, cancel := context.WithCancel(context.Background())
	writer := &gatedWriter{header: http.Header{}, writeGate: make(chan struct{}), writeEntered: make(chan struct{})}
	completion := &debuglog.Completion{}
	responseBytes := 0
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		application.streamCompletion(ctx, writer, recorder, chatProtocol{},
			llm.RequestMessages{Model: "gpt-test"}, protocolOptions{Stream: true}, completion, &responseBytes)
	}()

	<-writer.writeEntered // 消费方停在首个 flush 写出
	// 给泵留灌满 items 并停在投递点的时间——此后取消时 ctx.Done 是
	// 投递 select 唯一就绪的分支，交班 Recv 必在其内执行。
	time.Sleep(200 * time.Millisecond)
	cancel()

	close(writer.writeGate) // 消费方写通 → ctx.Err 收口 → 开始 unwind
	select {
	case <-returned:
		t.Fatal("streamCompletion returned while pump handoff was still gated")
	case <-time.After(500 * time.Millisecond):
	}
	close(stream.handoffGate)
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("streamCompletion did not return after handoff release")
	}
	select {
	case <-stream.handedOff:
	default:
		t.Fatal("pump exited without handoff Recv")
	}
}
