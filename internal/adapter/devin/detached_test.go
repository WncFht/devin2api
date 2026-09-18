// 本文件验证脱钩流与完成缓存：客户端断开后上游泵续命、同键重试
// 重放缓冲/追帧、键的语义等价性与条目终态分类。
package devin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
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

// waitRegistryStat 轮询缓存快照直到谓词命中：泵终局记账发生在
// finish 之后的独立调用，entry 定态观察不到它。
func waitRegistryStat(t *testing.T, registry *detachedRegistry, match func(DetachedStats) bool) DetachedStats {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := registry.stats()
		if match(stats) {
			return stats
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stats did not reach target: %+v", registry.stats())
	return DetachedStats{}
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

// TestFinishedStreamNotDetachable 钉住死后不登记：handler 返回同样取消
// 请求 ctx，哨兵在请求终结时必然醒来一次——此时已 finished 的流没有
// 可续命的泵，「产过内容但未见 stopReason」的失败收尾（midcontent 式
// 传输截断是其生产形态）若放行会把死流登记进缓存，白占容量与孤儿簿记。
func TestFinishedStreamNotDetachable(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 99, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
	}}
	stream := detachedTestStream(registry, "k5", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	// 榨干到 io.EOF：上游 EOF 无 stopReason → decoder.finish 产终态事件，
	// stream.finished 置位。
	for {
		_, err := stream.Recv(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
	}
	cancel()
	// 哨兵判定块与 Adapter.Stream 内同源：finished 流不得脱钩。
	stream.mu.Lock()
	if !stream.detached {
		if stream.detachable() {
			stream.detach(ctx)
		} else {
			stream.cancel()
		}
	}
	stream.mu.Unlock()
	if entry := registry.lookup("k5"); entry != nil {
		t.Fatal("finished stream must not be admitted to the detached cache")
	}
}

// TestAbortedStreamNotDetachable 钉住主动中断不登记：面板 abort/排空
// 强掐与客户端断连走同一 ctx.Done 分支，但脱钩缓存只救断连——被掐死
// 的生成若准入会继续烧上游至 running TTL，同键重试还会重放尸体。
// Abort 先置 aborted 位再取消，ctx.Done 可观察时 WasAborted 必真。
func TestAbortedStreamNotDetachable(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	defer close(receiver.release)
	stream := detachedTestStream(registry, "k7", receiver)
	manager := debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{}, nil)
	t.Cleanup(manager.Close)
	stream.recorder = manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/messages"})
	ctx, cancel := context.WithCancelCause(context.Background())
	stream.recorder.SetAbort(cancel)
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	if !stream.recorder.Abort(errors.New("aborted via panel request abort")) {
		t.Fatal("Abort should succeed on a recorder with an attached cancel")
	}
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after abort should return the cancel cause")
	}
	if stream.detached {
		t.Fatal("aborted stream must not detach")
	}
	if got := registry.lookup("k7"); got != nil {
		t.Fatal("aborted stream must not be admitted to the cache")
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
	registry.evictLocked("k4", entry, detachEvictCapacity)
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

// TestDetachedEventMirroredToMeta 钉住 detach() 调用点的 meta 镜像：
// 消费方 Recv 内脱钩时除写 04 标记行外还须经 NoteDetachedEvent 把同一
// detail 落进 meta.detached_events——04 标记行在队列压力下可丢，meta
// 随完结块出账不可丢。
func TestDetachedEventMirroredToMeta(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, db)
	t.Cleanup(manager.Close)

	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "mk", receiver)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/messages"})
	stream.recorder = recorder

	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry := registry.lookup("mk")
	if entry == nil {
		t.Fatal("detached stream was not registered")
	}
	close(receiver.release)
	waitEntryState(t, entry, detachedCompleted)

	recorder.Complete(debuglog.Completion{StatusCode: 499, Result: "disconnected"})
	<-manager.Drained(recorder.Dir())

	metaData, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "meta.json")
	if err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
	var meta debuglog.MetaSummary
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta.json decode: %v", err)
	}
	if len(meta.DetachedEvents) != 1 {
		t.Fatalf("detached_events = %+v, want exactly the detach marker", meta.DetachedEvents)
	}
	event := meta.DetachedEvents[0]
	if event["kind"] != "detached" || event["key"] != "mk" {
		t.Fatalf("detached_events[0] = %v, want kind=detached key=mk", event)
	}
	if _, ok := event["buffered_events"]; !ok {
		t.Fatalf("detached_events[0] missing buffered_events: %v", event)
	}
}

// TestDetachedPumpStopsFrameWrites 钉住脱钩泵不再写 04 帧：脱钩后盘上
// 不再追写，后台泵 drain 的帧只进完成缓存缓冲——原 dir 此时多已
// Complete，续写只会被 closed 门口拒收计进 late_writes/dropped 噪声。
func TestDetachedPumpStopsFrameWrites(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, db)
	t.Cleanup(manager.Close)

	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{DeltaText: proto.String(" there")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "k4", receiver)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/messages"})
	stream.recorder = recorder

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
	close(receiver.release)
	waitEntryState(t, entry, detachedCompleted)

	recorder.Complete(debuglog.Completion{StatusCode: 499, Result: "disconnected"})
	<-manager.Drained(recorder.Dir())

	data, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04-devin-response.jsonl: %v", err)
	}
	var events []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var row struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("04 row decode: %v", err)
		}
		events = append(events, row.Event)
	}
	want := []string{"frame", "detached"}
	if len(events) != len(want) {
		t.Fatalf("04 events = %v, want %v — post-detach pump frames must not reach the log", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Fatalf("04 events = %v, want %v", events, want)
		}
	}
}

// TestDetachedRequestKeyDeterminism 钉住键的语义等价边界：会话标识
// 参与（生产 02 证据：同 body 重试的 session_key 恒定，纳回换跨会话
// 隔离），调用方身份参与，内容差异参与，模型键面用解析后 uid。
func TestDetachedRequestKeyDeterminism(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "sys",
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}},
		},
		SessionKey:    "session-a",
		CallerKeyHash: "keyhash-a",
	}
	base := detachedRequestKey(request, "resolved-uid")
	if base == "" {
		t.Fatal("empty key")
	}
	other := request
	other.SessionKey = "session-b"
	if detachedRequestKey(other, "resolved-uid") == base {
		t.Fatal("session key participates: two sessions must not share a replay")
	}
	other = request
	other.CallerKeyHash = "keyhash-b"
	if detachedRequestKey(other, "resolved-uid") == base {
		t.Fatal("caller key hash participates: cross-token requests must not share a replay")
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

// TestDetachedRequestKeyIgnoresDecodeTimestamps 钉住解码时刻时间戳不进
// 键的回归：三个解码面都给每条消息打 time.Now()，同 body 的两次解码
// （两次 HTTP 重试）TimestampMS 不同但键必须相同——否则功能恒 miss。
func TestDetachedRequestKeyIgnoresDecodeTimestamps(t *testing.T) {
	build := func(ts int64) llm.RequestMessages {
		return llm.RequestMessages{
			SystemPrompt: "sys",
			Messages: []llm.Message{
				llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}, TimestampMS: ts},
				llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}, TimestampMS: ts + 1},
				llm.ToolResultMessage{ToolCallID: "c1", Content: []llm.Content{llm.TextContent{Text: "out"}}, TimestampMS: ts + 2},
			},
		}
	}
	if detachedRequestKey(build(1700000000000), "m") != detachedRequestKey(build(1700000099999), "m") {
		t.Fatal("decode-time timestamps must not participate in the key")
	}
}

// TestDetachedRequestKeyMarkersAndToolPassthrough 钉住 wire 语义面进键：
// seed marker（beta/cache_control 声明）漂移换键、非 marker 的 dropped
// 项不换键、工具透传位（custom/server/strict/只读位/server_name/归因
// 名单）漂移换键——同名同 schema 不同透传位走不同 wire 语义。
func TestDetachedRequestKeyMarkersAndToolPassthrough(t *testing.T) {
	request := llm.RequestMessages{
		SystemPrompt: "sys",
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hello"}}},
		},
		Tools: []llm.ToolDefinition{{Name: "exec", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	}
	base := detachedRequestKey(request, "m")

	other := request
	other.Dropped = []string{"unmatched_tool_call_id:c9", "empty_message:"}
	if detachedRequestKey(other, "m") != base {
		t.Fatal("non-marker dropped items must not participate in the key")
	}
	other = request
	other.Dropped = []string{llm.MarkerAnthropicBeta + "interleaved-thinking-2025-05-14"}
	if detachedRequestKey(other, "m") == base {
		t.Fatal("seed markers participate in the key")
	}
	other = request
	other.Dropped = []string{llm.MarkerCacheControl + "ephemeral"}
	if detachedRequestKey(other, "m") == base {
		t.Fatal("cache_control markers participate in the key")
	}

	variants := map[string]func(*llm.RequestMessages){
		"custom":    func(r *llm.RequestMessages) { r.Tools[0].Custom = true },
		"server":    func(r *llm.RequestMessages) { r.Tools[0].Server = true },
		"strict":    func(r *llm.RequestMessages) { r.Tools[0].Strict = true },
		"readonly":  func(r *llm.RequestMessages) { r.Tools[0].ReadOnlyHint = true },
		"svrname":   func(r *llm.RequestMessages) { r.Tools[0].ServerName = "mcp" },
		"attribute": func(r *llm.RequestMessages) { r.Tools[0].AttributionFieldNames = []string{"f"} },
	}
	for name, mutate := range variants {
		other := request
		other.Tools = append([]llm.ToolDefinition(nil), request.Tools...)
		mutate(&other)
		if detachedRequestKey(other, "m") == base {
			t.Fatalf("tool passthrough field %s participates in the key", name)
		}
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

// TestDetachedEntryTruncatesAtByteBudget 钉住字节预算语义：越界追加把
// 缓冲冻结成「前缀 + 截断错误」，之后的追加是空操作——截断错误恒为
// 末帧，finish 据此收成不可重放的 failed。
func TestDetachedEntryTruncatesAtByteBudget(t *testing.T) {
	defer func(budget int) { detachedMaxBufferedBytes = budget }(detachedMaxBufferedBytes)
	detachedMaxBufferedBytes = 1024
	entry := &detachedEntry{notify: make(chan struct{})}
	if entry.append(llm.ResponseEvent{Type: llm.ResponseEventTextDelta, Delta: strings.Repeat("a", 600)}) {
		t.Fatal("append under budget must not report truncation")
	}
	if !entry.append(llm.ResponseEvent{Type: llm.ResponseEventTextDelta, Delta: strings.Repeat("b", 600)}) {
		t.Fatal("append crossing the budget should report truncation")
	}
	if entry.append(llm.ResponseEvent{Type: llm.ResponseEventTextDelta, Delta: "late"}) {
		t.Fatal("frozen buffer must not report truncation again")
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if len(entry.events) != 3 {
		t.Fatalf("events = %d, want 2 prefix + 1 truncation marker", len(entry.events))
	}
	last := entry.events[len(entry.events)-1]
	if last.Type != llm.ResponseEventError {
		t.Fatalf("terminal event = %v, want truncation error", last.Type)
	}
	if failure := llm.FailureOf(last.Error); failure == nil || !failure.UpstreamFault {
		t.Fatal("truncation marker must classify as upstream fault so finish marks it non-replayable")
	}
}

// TestDetachedTruncatedLookupMisses 钉住截断条目的挂接语义：lookup
// 一律回未命中并就地逐出——截断缓冲产不出完整重放，同键重试走新上游，
// 槽位与缓冲随淘汰提前释放。
func TestDetachedTruncatedLookupMisses(t *testing.T) {
	defer func(budget int) { detachedMaxBufferedBytes = budget }(detachedMaxBufferedBytes)
	detachedMaxBufferedBytes = 64
	registry := newDetachedRegistry()
	entry := &detachedEntry{notify: make(chan struct{})}
	entry.append(llm.ResponseEvent{Type: llm.ResponseEventTextDelta, Delta: strings.Repeat("a", 128)})
	registry.admit("t1", entry)
	if got := registry.lookup("t1"); got != nil {
		t.Fatal("truncated entry must miss lookup")
	}
	if len(registry.entries) != 0 {
		t.Fatal("truncated entry should be evicted on lookup")
	}
}

// TestDetachedTruncationStopsPump 钉住 flood 截断的全链路：脱钩后上游
// 灌入超预算增量，append 冻结缓冲，后台泵下轮自检截断立即停泵——不再
// 为死缓冲白耗上游配额；条目收成不可重放的 failed，在飞挂接方重放到
// 显式错误而非无声 EOF。
func TestDetachedTruncationStopsPump(t *testing.T) {
	defer func(budget int) { detachedMaxBufferedBytes = budget }(detachedMaxBufferedBytes)
	detachedMaxBufferedBytes = 4096
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{DeltaText: proto.String(strings.Repeat("x", 8192))},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "k9", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry := registry.lookup("k9")
	if entry == nil {
		t.Fatal("detached stream was not registered")
	}
	close(receiver.release)
	waitEntryState(t, entry, detachedFailed)
	entry.mu.Lock()
	replayable := entry.replayable
	last := entry.events[len(entry.events)-1]
	entry.mu.Unlock()
	if last.Type != llm.ResponseEventError {
		t.Fatalf("terminal event = %v, want truncation error", last.Type)
	}
	if replayable {
		t.Fatal("truncated entry must finish non-replayable")
	}
	if got := registry.lookup("k9"); got != nil {
		t.Fatal("truncated entry must miss lookup")
	}
	replayed, err := collectAttached(&attachStream{entry: entry})
	if err != nil {
		t.Fatalf("attach Recv: %v", err)
	}
	if replayed[len(replayed)-1].Type != llm.ResponseEventError {
		t.Fatalf("last replayed event = %v, want truncation error", replayed[len(replayed)-1].Type)
	}
}

// TestDetachedPreTruncatedStreamNotDetachable 钉住客户端在场期截断的
// 流：缓冲在客户端断开前就越预算（下游慢消费 + 上游 flood）时，
// 断开按不可脱钩杀流——死缓冲登记进缓存也只是占位垃圾。
func TestDetachedPreTruncatedStreamNotDetachable(t *testing.T) {
	defer func(budget int) { detachedMaxBufferedBytes = budget }(detachedMaxBufferedBytes)
	detachedMaxBufferedBytes = 4096
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 2, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{DeltaText: proto.String(strings.Repeat("x", 8192))},
	}}
	defer close(receiver.release)
	stream := detachedTestStream(registry, "k8", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	var deltas int
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		if e.Type == llm.ResponseEventTextDelta {
			deltas++
		}
		return deltas == 2
	})
	if !stream.entry.isTruncated() {
		t.Fatal("buffer should be truncated after the oversized delta")
	}
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	if got := registry.lookup("k8"); got != nil {
		t.Fatal("pre-truncated stream must not be admitted to the cache")
	}
	if len(registry.entries) != 0 {
		t.Fatal("registry must stay empty for a killed pre-truncated stream")
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

// TestDetachedEvictByOriginDir 钉住面板 abort 的残留窗收口：条目按来源
// 调试目录逐出并掐后台泵——detach 落册与请求出 activeDirs 之间的窗口内
// abort 到达时，被掐死的生成不得留在缓存里供同键重试重放；其它目录的
// 条目不受影响。
func TestDetachedEvictByOriginDir(t *testing.T) {
	registry := newDetachedRegistry()
	drainKilled := make(chan struct{})
	victim := &detachedEntry{
		notify:      make(chan struct{}),
		state:       detachedRunning,
		originDir:   "dir-victim",
		drainCancel: func() { close(drainKilled) },
	}
	registry.admit("k1", victim)
	keeper := &detachedEntry{notify: make(chan struct{}), state: detachedCompleted, originDir: "dir-other"}
	registry.admit("k2", keeper)

	registry.evictByOriginDir("dir-victim")

	if got := registry.lookup("k1"); got != nil {
		t.Fatal("entry from aborted dir must be evicted")
	}
	if got := registry.lookup("k2"); got != keeper {
		t.Fatal("unrelated entry must survive origin-dir eviction")
	}
	select {
	case <-drainKilled:
	default:
		t.Fatal("evicting a running entry must cancel its drain pump")
	}
	if stats := registry.stats(); stats.Aborted != 1 {
		t.Fatalf("aborted evictions = %d, want 1", stats.Aborted)
	}
	// 空目录不得误伤 recorder 缺失的条目（originDir 同为空串）。
	registry.admit("k3", &detachedEntry{notify: make(chan struct{}), state: detachedRunning})
	registry.evictByOriginDir("")
	if got := registry.lookup("k3"); got == nil {
		t.Fatal("empty dir must not evict entries with empty originDir")
	}
}

// TestDetachedAdmitCorpseYieldsBeforeRunning 钉住容量淘汰的让位序：
// 截断/不可重放 failed 尸体先于活泵让位——缓冲已死的条目留场只为给
// 同键到场记 miss，杀一条还在喂事件的泵给尸体留槽是本末倒置。
func TestDetachedAdmitCorpseYieldsBeforeRunning(t *testing.T) {
	defer func(budget int) { detachedMaxBufferedBytes = budget }(detachedMaxBufferedBytes)
	detachedMaxBufferedBytes = 64
	registry := newDetachedRegistry()
	// 截断尸体：缓冲冻结、态仍 running（泵未及收尾），靠 truncated 位认尸。
	truncated := &detachedEntry{notify: make(chan struct{})}
	truncated.append(llm.ResponseEvent{Type: llm.ResponseEventTextDelta, Delta: strings.Repeat("a", 128)})
	registry.admit("corpse-trunc", truncated)
	// 不可重放 failed 尸体：传输断裂类终态，lookup 只记 miss 不重放。
	broken := &detachedEntry{notify: make(chan struct{})}
	broken.append(llm.ResponseEvent{Type: llm.ResponseEventError, Error: &llm.AssistantMessage{
		ErrorMessage: "http2: stream closed", Failure: &llm.Failure{Code: "internal", UpstreamFault: true},
	}})
	broken.finish()
	registry.admit("corpse-fail", broken)
	for i := 0; i < detachedMaxEntries-2; i++ {
		registry.admit(strings.Repeat("r", 4)+string(rune('a'+i)), &detachedEntry{notify: make(chan struct{})})
	}
	registry.admit("new", &detachedEntry{notify: make(chan struct{})})
	// 两具尸体让位：6 running + new = 7 条，活泵一个不动。
	if len(registry.entries) != detachedMaxEntries-1 {
		t.Fatalf("entries = %d, want %d", len(registry.entries), detachedMaxEntries-1)
	}
	if registry.entries["corpse-trunc"] != nil || registry.entries["corpse-fail"] != nil {
		t.Fatal("corpses must yield their slots before any running pump dies")
	}
	for i := 0; i < detachedMaxEntries-2; i++ {
		if registry.entries[strings.Repeat("r", 4)+string(rune('a'+i))] == nil {
			t.Fatalf("running entry %d must survive corpse eviction", i)
		}
	}
	if stats := registry.stats(); stats.Evicted != 2 {
		t.Fatalf("evicted = %d, want 2 corpse yields", stats.Evicted)
	}
}

// TestDetachedAdmitTerminalFallbackClosesSoftCap 钉住全终态兜底：8 槽
// 全是未过期终态时 admit 逐出最老终态——此前终态永不参与容量淘汰，
// map 会随 admit 速率×TTL 无界长大（软帽）；兜底把容量关回硬上限。
func TestDetachedAdmitTerminalFallbackClosesSoftCap(t *testing.T) {
	registry := newDetachedRegistry()
	for i := 0; i < detachedMaxEntries; i++ {
		done := &detachedEntry{notify: make(chan struct{})}
		done.append(llm.ResponseEvent{Type: llm.ResponseEventDone})
		done.finish()
		registry.admit(strings.Repeat("d", 4)+string(rune('a'+i)), done)
		time.Sleep(time.Millisecond) // admittedAt 是最老终态的排序依据
	}
	registry.admit("new", &detachedEntry{notify: make(chan struct{})})
	if len(registry.entries) != detachedMaxEntries {
		t.Fatalf("entries = %d, want %d (soft cap must close)", len(registry.entries), detachedMaxEntries)
	}
	if registry.entries["dddda"] != nil {
		t.Fatal("oldest terminal entry should have been evicted by the fallback")
	}
	if registry.entries["new"] == nil {
		t.Fatal("new entry must be admitted")
	}
	if stats := registry.stats(); stats.Evicted != 1 {
		t.Fatalf("evicted = %d, want 1 terminal fallback", stats.Evicted)
	}
}

// TestDetachedTruncatedCountsAtFreeze 钉住截断记账点：缓冲越预算冻结
// 时计数，不等移除路径——截断尸体若经 admit 扫描/到期逐出而非 lookup
// 惰性逐出，旧口径会把这次截断漏记成 expired；lookup 逐出也不再复计。
func TestDetachedTruncatedCountsAtFreeze(t *testing.T) {
	defer func(budget int) { detachedMaxBufferedBytes = budget }(detachedMaxBufferedBytes)
	detachedMaxBufferedBytes = 4096
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{DeltaText: proto.String(strings.Repeat("x", 8192))},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "tz", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	close(receiver.release)
	// 泵 drain 到越界帧即冻结：截断发生点入账，不等任何移除路径。
	waitRegistryStat(t, registry, func(s DetachedStats) bool {
		return s.Truncated == 1
	})
	if got := registry.lookup("tz"); got != nil {
		t.Fatal("truncated entry must miss lookup")
	}
	stats := registry.stats()
	if stats.Truncated != 1 {
		t.Fatalf("truncated = %d, want 1 (freeze counted once at truncation point)", stats.Truncated)
	}
	if stats.Expired != 1 {
		t.Fatalf("expired = %d, want 1 (corpse removal counts as lazy collection)", stats.Expired)
	}
}

// TestDetachedStatsCounters 钉住缓存簿记：登记/挂接/未命中/移除/孤儿/
// 泵终局计数在各生命周期动作上恰各记一次——runtime-metrics 的
// detached 组是泵终局与孤儿浪费的唯一观测面，计数错了无处可对。
func TestDetachedStatsCounters(t *testing.T) {
	registry := newDetachedRegistry()

	// running 条目挂接命中：attaches+1 且 attached 置位（之后移除不算孤儿）。
	hit := &detachedEntry{notify: make(chan struct{})}
	registry.admit("k1", hit)
	if registry.lookup("k1") != hit {
		t.Fatal("running entry should attach")
	}
	// 同键再登记：旧条目算 replaced 移除，attached 过不算孤儿。
	registry.admit("k1", &detachedEntry{notify: make(chan struct{})})

	// 过期条目在场时同键请求到来：attach_miss + expired 移除 + 孤儿。
	stale := &detachedEntry{notify: make(chan struct{})}
	registry.admit("k2", stale)
	stale.mu.Lock()
	stale.expiresAt = time.Now().Add(-time.Second)
	stale.mu.Unlock()
	if registry.lookup("k2") != nil {
		t.Fatal("expired entry must not attach")
	}

	// completed 无人挂接过期移除：孤儿且 orphan_completed（纯浪费口径）。
	done := &detachedEntry{notify: make(chan struct{})}
	registry.admit("k3", done)
	done.append(llm.ResponseEvent{Type: llm.ResponseEventDone})
	done.finish()
	done.mu.Lock()
	done.expiresAt = time.Now().Add(-time.Second)
	done.mu.Unlock()
	registry.lookup("k3")

	// 不可重放 failed：attach_miss 但不移除（5min TTL 留在场等下一个同键）。
	broken := &detachedEntry{notify: make(chan struct{})}
	broken.append(llm.ResponseEvent{Type: llm.ResponseEventError, Error: &llm.AssistantMessage{
		ErrorMessage: "http2: stream closed", Failure: &llm.Failure{Code: "internal", UpstreamFault: true},
	}})
	broken.finish()
	registry.admit("k4", broken)
	if registry.lookup("k4") != nil {
		t.Fatal("unreplayable failed entry must not attach")
	}

	// 泵终局记账由后台泵调用方负责（detach 的泵 goroutine）——这里
	// 直记两条覆盖 completed 与 killed 两桶。
	registry.noteFinish("k1", detachFinishCompleted)
	registry.noteFinish("k2", detachFinishKilled)

	stats := registry.stats()
	// 在场：k1 新条目（running）与 k4（failed）；k2/k3 已逐、k1 旧条目已替换。
	if stats.Entries != 2 || stats.Running != 1 || stats.Failed != 1 || stats.Completed != 0 {
		t.Fatalf("entry states = %+v", stats)
	}
	if stats.Detaches != 5 || stats.Attaches != 1 || stats.AttachMisses != 3 {
		t.Fatalf("flow counters = %+v", stats)
	}
	if stats.Replaced != 1 || stats.Expired != 2 || stats.Evicted != 0 {
		t.Fatalf("removal counters = %+v", stats)
	}
	if stats.Orphans != 2 || stats.OrphanCompleted != 1 {
		t.Fatalf("orphan counters = %+v", stats)
	}
	// k3 孤儿脱钩后新产出 1 个事件（Done）；k2 零产出。浪费量级代理=1。
	if stats.OrphanBufferedEvents != 1 {
		t.Fatalf("orphan buffered events = %d, want 1", stats.OrphanBufferedEvents)
	}
	if stats.FinishedCompleted != 1 || stats.FinishedKilled != 1 || stats.FinishedFailed != 0 {
		t.Fatalf("finish counters = %+v", stats)
	}
	// 事件环新在前：最后一记是 k2 的 finish/ttl 类终局。
	if len(stats.Events) == 0 {
		t.Fatal("events ring empty")
	}
	head := stats.Events[0]
	if head.Kind != detachedEventFinish || head.Detail != detachFinishKilled || head.Key != "k2" {
		t.Fatalf("events head = %+v", head)
	}
}

// TestDetachedPumpFinishAccounting 钉住泵终局的归因记账：后台泵 EOF
// 收口记 finished_completed，被 registry 淘汰掐死记 finished_killed
// ——两档走真实 detach→泵→finish 路径，与 orphan（有没有人接）正交。
func TestDetachedPumpFinishAccounting(t *testing.T) {
	registry := newDetachedRegistry()
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream := detachedTestStream(registry, "p1", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	close(receiver.release)
	stats := waitRegistryStat(t, registry, func(s DetachedStats) bool {
		return s.FinishedCompleted == 1
	})
	if stats.FinishedFailed != 0 || stats.FinishedKilled != 0 || stats.FinishedExpired != 0 {
		t.Fatalf("clean EOF pump misaccounted: %+v", stats)
	}

	// 淘汰掐死：第二条泵被 evict 掐 drainCancel → finished_killed。
	receiver2 := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
	}}
	stream2 := detachedTestStream(registry, "p2", receiver2)
	ctx2, cancel2 := context.WithCancel(context.Background())
	drainUntil(t, stream2, ctx2, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel2()
	if _, err := stream2.Recv(ctx2); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry2 := registry.lookup("p2")
	if entry2 == nil {
		t.Fatal("second detached stream was not registered")
	}
	registry.evictLocked("p2", entry2, detachEvictCapacity)
	stats = waitRegistryStat(t, registry, func(s DetachedStats) bool {
		return s.FinishedKilled == 1
	})
	if stats.Evicted != 1 || stats.FinishedCompleted != 1 {
		t.Fatalf("evicted pump misaccounted: %+v", stats)
	}
}

// TestDetachedPeek 钉住跨 lane 探测的三态与只读语义：在场可用/在场不可用/
// 缺席。usable 与 lookup 同判据，但 peek 不置 attached、不惰性逐出、不计
// attach_misses——它只给 owner 条目盖 sawCrossLaneRetry 章供孤儿拆分。
func TestDetachedPeek(t *testing.T) {
	registry := newDetachedRegistry()

	// 在场可用：running 条目 → (running, usable, ok)；attached 不得置位。
	live := &detachedEntry{notify: make(chan struct{})}
	registry.admit("p1", live)
	state, usable, ok := registry.peek("p1")
	if !ok || state != detachedRunning || !usable {
		t.Fatalf("peek running = (%v,%v,%v), want (running,true,true)", state, usable, ok)
	}
	live.mu.Lock()
	if live.attached {
		t.Fatal("peek must not mark attached — that would hide the orphan from owner-side accounting")
	}
	if !live.sawCrossLaneRetry {
		t.Fatal("peek must set sawCrossLaneRetry for owner-side orphan split")
	}
	live.mu.Unlock()

	// 在场不可用：不可重放的 failed → (failed, !usable, ok)；不逐出、
	// 不计 attach_misses（lookup 的口径只认真实挂接尝试）。
	broken := &detachedEntry{notify: make(chan struct{})}
	broken.append(llm.ResponseEvent{Type: llm.ResponseEventError, Error: &llm.AssistantMessage{
		ErrorMessage: "http2: stream closed", Failure: &llm.Failure{Code: "internal", UpstreamFault: true},
	}})
	broken.finish()
	registry.admit("p2", broken)
	state, usable, ok = registry.peek("p2")
	if !ok || state != detachedFailed || usable {
		t.Fatalf("peek unreplayable = (%v,%v,%v), want (failed,false,true)", state, usable, ok)
	}
	if len(registry.entries) != 2 {
		t.Fatal("peek must not lazily evict — entry removal stays owner-side business")
	}

	// 过期条目同样在场但不可用，且不被 peek 清掉。
	stale := &detachedEntry{notify: make(chan struct{})}
	registry.admit("p3", stale)
	stale.mu.Lock()
	stale.expiresAt = time.Now().Add(-time.Second)
	stale.mu.Unlock()
	if _, usable, ok = registry.peek("p3"); !ok || usable {
		t.Fatalf("peek expired = (?, %v, %v), want (false,true)", usable, ok)
	}

	// 缺席：普通首发。
	if _, _, ok := registry.peek("p9"); ok {
		t.Fatal("peek on absent key must miss")
	}
	stats := registry.stats()
	if stats.AttachMisses != 0 || stats.Expired != 0 {
		t.Fatalf("peek must be counter-free: %+v", stats)
	}
}

// TestDetachedCrossLaneMissBookkeeping 钉住探测侧的记账：本 lane 的
// crossLaneMisses 递增、事件环收 cross_miss（detail 带 owner:state）。
func TestDetachedCrossLaneMissBookkeeping(t *testing.T) {
	registry := newDetachedRegistry()
	registry.noteCrossLaneMiss("abcdef1234567890", "owner-a", detachedRunning)
	registry.noteCrossLaneMiss("abcdef1234567890", "owner-a", detachedCompleted)
	stats := registry.stats()
	if stats.CrossLaneMisses != 2 {
		t.Fatalf("cross_lane_misses = %d, want 2", stats.CrossLaneMisses)
	}
	head := stats.Events[0]
	if head.Kind != detachedEventCrossMiss || head.Detail != "owner-a:completed" || head.Key != "abcdef123456" {
		t.Fatalf("events head = %+v", head)
	}
	if head.Label != "跨号未命中" {
		t.Fatalf("cross_miss label = %q", head.Label)
	}
}

// TestDetachedOrphansCrossLane 钉住孤儿拆分：sawCrossLaneRetry 置位的孤儿
// 移除时另记 orphans_cross_lane——「来错门」与「没人来」分开归因。
func TestDetachedOrphansCrossLane(t *testing.T) {
	registry := newDetachedRegistry()

	// 来错门：peek 盖过章的孤儿。
	wrongDoor := &detachedEntry{notify: make(chan struct{})}
	registry.admit("x1", wrongDoor)
	registry.peek("x1")
	registry.evictLocked("x1", wrongDoor, detachEvictExpired)

	// 没人来：普通孤儿。
	nobody := &detachedEntry{notify: make(chan struct{})}
	registry.admit("x2", nobody)
	registry.evictLocked("x2", nobody, detachEvictExpired)

	// 挂接过的不算孤儿。
	attached := &detachedEntry{notify: make(chan struct{})}
	registry.admit("x3", attached)
	registry.lookup("x3")
	registry.evictLocked("x3", attached, detachEvictExpired)

	stats := registry.stats()
	if stats.Orphans != 2 || stats.OrphansCrossLane != 1 {
		t.Fatalf("orphans = %d cross = %d, want 2/1", stats.Orphans, stats.OrphansCrossLane)
	}
}
