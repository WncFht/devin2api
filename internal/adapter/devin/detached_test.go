// 本文件验证脱钩流与完成缓存：客户端断开后上游泵续命、同键重试
// 重放缓冲/追帧、键的语义等价性与条目终态分类。
package devin

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
)

// pauseReceiver 先按序发 frames、到达 pauseAt 下标时阻塞到 release、
// 放行后续发余下帧再 EOF——覆盖「脱钩时上游仍在静默、释放后正常收尾」
// 的完整生命周期（hangAfterReceiver 发完就永远阻塞，产不出 stopReason）。
type pauseReceiver struct {
	frames   []*devinproto.GetChatMessageResponse
	pauseAt  int
	release  chan struct{}
	released bool
	index    int
	current  *devinproto.GetChatMessageResponse
}

// Receive 实现 devinResponseReceiver。
func (receiver *pauseReceiver) Receive() bool {
	if receiver.index == receiver.pauseAt && !receiver.released {
		<-receiver.release
		receiver.released = true
	}
	if receiver.index >= len(receiver.frames) {
		return false
	}
	receiver.current = receiver.frames[receiver.index]
	receiver.index++
	return true
}

// Msg 返回最近一次成功读取的帧。
func (receiver *pauseReceiver) Msg() *devinproto.GetChatMessageResponse { return receiver.current }

// Err 模拟正常 EOF。
func (receiver *pauseReceiver) Err() error { return nil }

// detachedTestStream 构造一条带完成缓存挂接面的测试流：与 Adapter.Stream
// 注入的字段同形，差别只在泵与 cancel 是测试桩。
func detachedTestStream(registry *detachedRegistry, key string, receiver devinResponseReceiver) *responseStream {
	return &responseStream{
		frames:    pumpUpstream(context.Background(), receiver),
		cancel:    func() {},
		decoder:   newResponseDecoder("model", nil, nil, nil),
		gate:      newRateGate(GateConfig{}, nil, ""),
		detachKey: key,
		registry:  registry,
		entry:     &detachedEntry{notify: make(chan struct{})},
	}
}

// drainUntil 消费事件直到谓词命中或出错；返回读到的完整事件序列。
func drainUntil(t *testing.T, stream *responseStream, ctx context.Context, match func(llm.ResponseEvent) bool) []llm.ResponseEvent {
	t.Helper()
	var events []llm.ResponseEvent
	for {
		event, err := stream.Recv(ctx)
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		events = append(events, event)
		if match(event) {
			return events
		}
	}
}

// waitEntryState 轮询条目到达目标态（后台泵定态是异步的）。
func waitEntryState(t *testing.T, entry *detachedEntry, want detachedState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entry.mu.Lock()
		state := entry.state
		entry.mu.Unlock()
		if state == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	t.Fatalf("entry state = %v, want %v", entry.state, want)
}

// collectAttached 读干一条挂接流到 io.EOF。
func collectAttached(stream llm.ResponseStream) ([]llm.ResponseEvent, error) {
	var events []llm.ResponseEvent
	for {
		event, err := stream.Recv(context.Background())
		if errors.Is(err, io.EOF) {
			return events, nil
		}
		if err != nil {
			return events, err
		}
		events = append(events, event)
	}
}

// TestDetachedStreamKeepsPumping 钉住核心语义：客户端在内容产出后断开，
// 上游流不取消而是脱钩续命——后台泵把剩余事件喂进缓冲并定态 completed，
// 挂接方重放出含前缀的完整序列。
func TestDetachedStreamKeepsPumping(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		{Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{ModelUid: proto.String("m")}},
	}}
	stream := detachedTestStream(registry, "k1", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	consumed := drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	// 客户端断开后的下一次 Recv 决定去向：脱钩而不是取消。
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry := registry.lookup("k1")
	if entry == nil {
		t.Fatal("detached stream was not registered")
	}
	entry.mu.Lock()
	if entry.state != detachedRunning {
		entry.mu.Unlock()
		t.Fatalf("entry state = %v, want running", entry.state)
	}
	entry.mu.Unlock()
	close(receiver.release)
	waitEntryState(t, entry, detachedCompleted)
	replayed, err := collectAttached(&attachStream{entry: entry})
	if err != nil {
		t.Fatalf("attach Recv: %v", err)
	}
	if len(replayed) <= len(consumed) {
		t.Fatalf("replayed %d events, want more than the %d consumed before detach", len(replayed), len(consumed))
	}
	for i, event := range consumed {
		if replayed[i].Type != event.Type {
			t.Fatalf("replayed[%d].Type = %v, want %v", i, replayed[i].Type, event.Type)
		}
	}
	if replayed[len(replayed)-1].Type != llm.ResponseEventDone {
		t.Fatalf("last replayed event = %v, want done", replayed[len(replayed)-1].Type)
	}
}

// TestDetachedWatcherPathCoversAbandonedConsumer 钉住哨兵语义：消费方
// 断开后不再进 Recv（app 泵投递点两路就绪随机选中 ctx.Done）时，
// Adapter.Stream 里持 mu 的哨兵判定块同样能把流送进缓存——否则那条
// 路径上既无人脱钩也无人杀泵，孤儿泵随 streamBase 永久泄漏。
func TestDetachedWatcherPathCoversAbandonedConsumer(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "k3", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	// 消费方读到首个事件就弃流，模拟断开落在投递窗口而非 Recv 里。
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	// 哨兵判定块与 Adapter.Stream 内同源：持 mu 就地脱钩或杀泵。
	stream.mu.Lock()
	if !stream.detached {
		if stream.detachable() {
			stream.detach(ctx)
		} else {
			stream.cancel()
		}
	}
	stream.mu.Unlock()
	entry := registry.lookup("k3")
	if entry == nil {
		t.Fatal("sentinel path did not register the detached stream")
	}
	close(receiver.release)
	waitEntryState(t, entry, detachedCompleted)
	replayed, err := collectAttached(&attachStream{entry: entry})
	if err != nil {
		t.Fatalf("attach Recv: %v", err)
	}
	if replayed[len(replayed)-1].Type != llm.ResponseEventDone {
		t.Fatalf("last replayed event = %v, want done", replayed[len(replayed)-1].Type)
	}
}

// TestDetachedEvictStopsPump 钉住容量淘汰的杀泵路径：evict 掐的是
// drainCancel（一次性 CancelFunc），后台泵走 ctx.Done 退场并把条目
// 收成 failed——截断前缀不得误标 completed 重放给同键重试。
func TestDetachedEvictStopsPump(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "k4", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry := registry.lookup("k4")
	if entry == nil {
		t.Fatal("detached stream was not registered")
	}
	// 容量淘汰：掐 drainCtx 让泵退场，release 永不放行（泵被掐死）。
	registry.evictLocked("k4", entry)
	waitEntryState(t, entry, detachedFailed)
	entry.mu.Lock()
	last := entry.events[len(entry.events)-1]
	replayable := entry.replayable
	entry.mu.Unlock()
	if last.Type != llm.ResponseEventError {
		t.Fatalf("terminal event = %v, want synthetic error", last.Type)
	}
	if replayable {
		t.Fatal("upstream-fault failed entry must not be replayable")
	}
}

// TestDetachedAttachFollowsLive 钉住 running 挂接：挂接方先重放已缓冲
// 前缀，然后按下标追新事件直到后台泵读到终态。
func TestDetachedAttachFollowsLive(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "k2", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry := registry.lookup("k2")
	if entry == nil {
		t.Fatal("detached stream was not registered")
	}
	// running 态挂接：缓冲读空后阻塞在 notify 上等后台泵喂新事件。
	attach := &attachStream{entry: entry}
	type collectResult struct {
		events []llm.ResponseEvent
		err    error
	}
	done := make(chan collectResult, 1)
	go func() {
		events, err := collectAttached(attach)
		done <- collectResult{events, err}
	}()
	time.Sleep(20 * time.Millisecond)
	close(receiver.release)
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("attach Recv: %v", result.err)
		}
		if result.events[len(result.events)-1].Type != llm.ResponseEventDone {
			t.Fatalf("last event = %v, want done", result.events[len(result.events)-1].Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("attached stream did not finish after upstream EOF")
	}
}

// TestDetachedRequestKeyDeterminism 钉住键的语义等价边界：会话标识不
// 参与（CC 重试会换 user_id），内容差异参与，模型键面用解析后 uid。
func TestDetachedRequestKeyDeterminism(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "sys",
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}},
		},
		SessionKey: "session-a",
	}
	base := detachedRequestKey(request, "resolved-uid")
	if base == "" {
		t.Fatal("empty key")
	}
	other := request
	other.SessionKey = "session-b-with-retry-counter"
	if detachedRequestKey(other, "resolved-uid") != base {
		t.Fatal("session key must not participate in the semantic key")
	}
	other = request
	other.Messages = append(other.Messages, llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "more"}}})
	if detachedRequestKey(other, "resolved-uid") == base {
		t.Fatal("different messages must produce a different key")
	}
	if detachedRequestKey(request, "other-uid") == base {
		t.Fatal("resolved model uid participates in the key")
	}
	topP := 0.5
	other = request
	other.TopP = &topP
	if detachedRequestKey(other, "resolved-uid") == base {
		t.Fatal("sampling params participate in the key")
	}
}

// TestDetachedEntryFinishClassifies 钉住终态分类：末帧 error 记 failed
// 且按失败可重放性决定 lookup 是否放行；正常 Done 记 completed。
func TestDetachedEntryFinishClassifies(t *testing.T) {
	entry := &detachedEntry{notify: make(chan struct{})}
	entry.append(llm.ResponseEvent{Type: llm.ResponseEventError, Error: &llm.AssistantMessage{
		ErrorMessage: "boom", Failure: &llm.Failure{Code: "invalid_argument", ClientFixable: true},
	}})
	entry.finish()
	entry.mu.Lock()
	if entry.state != detachedFailed || !entry.replayable {
		t.Fatalf("state=%v replayable=%v, want failed+replayable", entry.state, entry.replayable)
	}
	entry.mu.Unlock()

	registry := newDetachedRegistry()
	registry.admit("f1", entry)
	if got := registry.lookup("f1"); got != entry {
		t.Fatal("client-fixable failed entry should replay")
	}

	// 传输断裂类失败不可重放：同键重试应走新上游而非吃缓存终态。
	broken := &detachedEntry{notify: make(chan struct{})}
	broken.append(llm.ResponseEvent{Type: llm.ResponseEventError, Error: &llm.AssistantMessage{
		ErrorMessage: "http2: stream closed", Failure: &llm.Failure{Code: "internal", UpstreamFault: true},
	}})
	broken.finish()
	registry.admit("f2", broken)
	if got := registry.lookup("f2"); got != nil {
		t.Fatal("upstream-fault failed entry must not replay")
	}
}

// TestProgressDeadlineTiers 钉住无进度期限的两档：产出前是 pre 档，
// 产出过内容后切到 postProgressTimeout（覆盖工具参数静默计算）。
func TestProgressDeadlineTiers(t *testing.T) {
	defer func(d time.Duration) { upstreamNoProgressTimeout = d }(upstreamNoProgressTimeout)
	upstreamNoProgressTimeout = 30 * time.Millisecond
	stream := &responseStream{postProgressTimeout: 90 * time.Millisecond}
	if got := stream.progressDeadline(); got != upstreamNoProgressTimeout {
		t.Fatalf("pre-content deadline = %v, want %v", got, upstreamNoProgressTimeout)
	}
	stream.producedEvents = true
	if got := stream.progressDeadline(); got != 90*time.Millisecond {
		t.Fatalf("post-content deadline = %v, want 90ms", got)
	}
	// 裸流（postProgressTimeout 零值）回落 pre 档——测试构造语义不变。
	bare := &responseStream{producedEvents: true}
	if got := bare.progressDeadline(); got != upstreamNoProgressTimeout {
		t.Fatalf("bare stream deadline = %v, want %v", got, upstreamNoProgressTimeout)
	}
}

// TestDetachedRegistryEvictsOldestRunning 钉住容量淘汰序：触顶先逐过期
// 再逐最老 running，completed 条目不被 running 挤掉。
func TestDetachedRegistryEvictsOldestRunning(t *testing.T) {
	registry := newDetachedRegistry()
	completed := &detachedEntry{notify: make(chan struct{}), state: detachedCompleted}
	registry.admit("done", completed)
	old := &detachedEntry{notify: make(chan struct{}), state: detachedRunning}
	registry.admit("old", old)
	time.Sleep(time.Millisecond)
	for i := 0; i < detachedMaxEntries-1; i++ {
		registry.admit(strings.Repeat("x", 4)+string(rune('a'+i)), &detachedEntry{notify: make(chan struct{}), state: detachedRunning})
	}
	if registry.lookup("old") != nil {
		t.Fatal("oldest running entry should have been evicted")
	}
	if registry.lookup("done") != completed {
		t.Fatal("completed entry must not be evicted by running pressure")
	}
}
