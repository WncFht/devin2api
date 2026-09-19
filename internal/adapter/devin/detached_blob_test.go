package devin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// TestDetachedBlobCodecRoundTrip 钉住编解码的往返恒等：三个金帧回放
// 产出的真实事件序列（text/thinking 含迟到签名/tool-call 全覆盖）
// 编码→解码→再编码必须逐字节相同——编码确定性（块表下标分配、
// omitempty 省略、map 序）使字节等价比逐字段断言更严格；另抽要点
// 字段核对解码产物语义无损。
func TestDetachedBlobCodecRoundTrip(t *testing.T) {
	for _, fixture := range []string{"text-answer.jsonl", "thinking-late-signature.jsonl", "tool-call.jsonl"} {
		events := replayFixture(t, fixture, nil, nil)
		payload, err := encodeDetachedEvents(events)
		if err != nil {
			t.Fatalf("%s: encode: %v", fixture, err)
		}
		decoded, err := decodeDetachedEvents(payload)
		if err != nil {
			t.Fatalf("%s: decode: %v", fixture, err)
		}
		if len(decoded) != len(events) {
			t.Fatalf("%s: decoded %d events, want %d", fixture, len(decoded), len(events))
		}
		for i := range events {
			if decoded[i].Type != events[i].Type || decoded[i].ContentIndex != events[i].ContentIndex ||
				decoded[i].Delta != events[i].Delta || decoded[i].Content != events[i].Content {
				t.Fatalf("%s: event %d diverged: %+v vs %+v", fixture, i, decoded[i], events[i])
			}
		}
		// 编码确定性断言：二次编码逐字节相同——块表去重与逐位置续用
		// 不改变产物，只改变成本。
		reencoded, err := encodeDetachedEvents(decoded)
		if err != nil {
			t.Fatalf("%s: re-encode: %v", fixture, err)
		}
		if !bytes.Equal(reencoded, payload) {
			t.Fatalf("%s: re-encoded payload differs (%d vs %d bytes)", fixture, len(reencoded), len(payload))
		}
		// Partial 快照的内容块语义无损：金帧的 thinking 签名与 tool call
		// 参数经块表往返后必须原值在场。
		for i, event := range events {
			if event.Partial == nil {
				continue
			}
			got := decoded[i].Partial
			if got == nil || len(got.Content) != len(event.Partial.Content) {
				t.Fatalf("%s: event %d partial content len diverged", fixture, i)
			}
			for j, block := range event.Partial.Content {
				if got.Content[j].ContentType() != block.ContentType() {
					t.Fatalf("%s: event %d block %d type = %v, want %v",
						fixture, i, j, got.Content[j].ContentType(), block.ContentType())
				}
			}
		}
	}
}

// TestDetachedBlobCodecSynthesized 覆盖金帧产不出的形状：五类内容块
// 同消息共存、Message/Error 的 Failure 全标志位（Cause 按设计丢弃）、
// ServerResult 明细、Usage 全字段与标量面（ResponseID/OutputID/
// UpstreamRequestID/StopSequence/DebugRef/TimestampMS）。
func TestDetachedBlobCodecSynthesized(t *testing.T) {
	partial := &llm.AssistantMessage{
		Content: []llm.Content{
			llm.TextContent{Text: "answer"},
			llm.ThinkingContent{Thinking: "chain", Signature: "sig.v1", SignatureType: "sealed", Redacted: true},
			llm.ImageContent{MIMEType: "image/png", Data: "AAAA"},
			llm.ToolCall{ID: "call_1", Name: "exec", Arguments: json.RawMessage(`{"cmd":"ls"}`), Custom: true},
			llm.ServerToolResult{ToolCallID: "srv_1", ToolName: "web_search", Content: []llm.Content{llm.TextContent{Text: "hits"}}, SearchResults: []llm.WebSearchResult{{Title: "t", URL: "u", Summary: "s"}}},
		},
		API: "devin", Provider: "anthropic", Model: "m", ResponseModel: "rm",
		ResponseID: "rid", OutputID: "oid", UpstreamRequestID: "urid",
		Usage:        llm.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4},
		StopReason:   llm.StopReasonStopSequence,
		StopSequence: "SEQ",
		TimestampMS:  42,
	}
	done := &llm.AssistantMessage{
		Content:           partial.Content,
		StopReason:        llm.StopReasonStop,
		ErrorMessage:      "em",
		DebugRef:          "dbg",
		UpstreamRequestID: "urid",
	}
	failure := &llm.Failure{
		Code: "rate_limit", Message: "slow down", LocalGate: true, UpstreamFault: true,
		RetryAfterSeconds: 7, RetryAfterMinute: true, GateReason: "quota", GateProbeMS: 9,
		GateSiblingEwMS: 10, ContextLength: true, RateLimited: true, Canceled: true,
		Timeout: true, ClientFixable: true, TraceID: "tid", ResetHint: true,
	}
	errMessage := &llm.AssistantMessage{ErrorMessage: "boom", Failure: failure}
	events := []llm.ResponseEvent{
		{Type: llm.ResponseEventStart, Partial: partial},
		{Type: llm.ResponseEventTextDelta, ContentIndex: 0, Delta: "a", Partial: partial},
		{Type: llm.ResponseEventToolCallStart, ContentIndex: 3, ToolCallID: "call_1", ToolName: "exec", Partial: partial},
		{Type: llm.ResponseEventServerToolResult, ContentIndex: 4, ServerResult: &llm.ServerToolResult{ToolCallID: "srv_1", ToolName: "web_search"}, Partial: partial},
		{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: done},
		{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: errMessage},
	}
	payload, err := encodeDetachedEvents(events)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := decodeDetachedEvents(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(events) {
		t.Fatalf("decoded %d events, want %d", len(decoded), len(events))
	}
	got := decoded[0].Partial
	if got == nil || len(got.Content) != 5 {
		t.Fatalf("partial content = %+v", got)
	}
	if thinking, ok := got.Content[1].(llm.ThinkingContent); !ok ||
		thinking.Signature != "sig.v1" || thinking.SignatureType != "sealed" || !thinking.Redacted {
		t.Fatalf("thinking block = %+v", got.Content[1])
	}
	if image, ok := got.Content[2].(llm.ImageContent); !ok || image.Data != "AAAA" {
		t.Fatalf("image block = %+v", got.Content[2])
	}
	if call, ok := got.Content[3].(llm.ToolCall); !ok || !call.Custom || string(call.Arguments) != `{"cmd":"ls"}` {
		t.Fatalf("tool call block = %+v", got.Content[3])
	}
	if result, ok := got.Content[4].(llm.ServerToolResult); !ok || len(result.SearchResults) != 1 || result.SearchResults[0].Title != "t" {
		t.Fatalf("server result block = %+v", got.Content[4])
	}
	if got.Usage.Input != 1 || got.Usage.CacheWrite != 4 || got.StopSequence != "SEQ" || got.TimestampMS != 42 {
		t.Fatalf("partial scalars = %+v", got)
	}
	if decoded[4].Message == nil || decoded[4].Message.DebugRef != "dbg" || len(decoded[4].Message.Content) != 5 {
		t.Fatalf("done message = %+v", decoded[4].Message)
	}
	gotFailure := decoded[5].Error.Failure
	if gotFailure == nil || !gotFailure.UpstreamFault || !gotFailure.Canceled || !gotFailure.ContextLength ||
		!gotFailure.ClientFixable || gotFailure.RetryAfterSeconds != 7 || gotFailure.GateProbeMS != 9 || gotFailure.TraceID != "tid" {
		t.Fatalf("failure = %+v", gotFailure)
	}
	if gotFailure.Cause != nil {
		t.Fatal("Cause should be dropped by codec")
	}
	reencoded, err := encodeDetachedEvents(decoded)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if !bytes.Equal(reencoded, payload) {
		t.Fatal("re-encoded payload differs")
	}
}

// TestDetachedBlobCarrySkipsMarshal 钉住逐位置续用的成本界：Delta 洪流
// 里成形块逐事件命中续用下标、只在块值真变时付 marshal——100 事件
// 带恒同 thinking + 逐事件增长的 text，marshal 总数 = 成形块 1 +
// 在产块 100 个真变值，不是事件数×块体的平方开销。
func TestDetachedBlobCarrySkipsMarshal(t *testing.T) {
	enc := &blobEncoder{blockIDs: make(map[string]int)}
	thinking := llm.ThinkingContent{Thinking: "chain", Signature: "sig"}
	var text string
	for i := 0; i < 100; i++ {
		text += "x"
		out, err := enc.event(llm.ResponseEvent{
			Type: llm.ResponseEventTextDelta, ContentIndex: 1, Delta: "x",
			Partial: &llm.AssistantMessage{Content: []llm.Content{thinking, llm.TextContent{Text: text}}},
		})
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if len(out.Partial.Content) != 2 || out.Partial.Content[0] != 0 {
			t.Fatalf("event %d: content idx = %v, want formed block carried at 0", i, out.Partial.Content)
		}
	}
	if enc.marshals != 101 {
		t.Fatalf("marshals = %d, want 101 (1 formed block + 100 in-flight deltas)", enc.marshals)
	}
}

// TestDetachedBlobCodecCorrupt 钉住坏行的失败形态：乱码载荷与不支持的
// 版本都必须显式报错——播种侧据此计 blobDrops 跳过，绝不静默灌册。
func TestDetachedBlobCodecCorrupt(t *testing.T) {
	if _, err := decodeDetachedEvents([]byte("garbage")); err == nil {
		t.Fatal("garbage payload should fail decode")
	}
	badVersion, err := json.Marshal(detachedBlob{Version: detachedBlobVersion + 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := decodeDetachedEvents(badVersion); err == nil {
		t.Fatal("unsupported version should fail decode")
	}
}

// TestDetachedFinishPersistsBlob 钉住真实路径的写方：completed 泵终局
// 经 noteFinish 把缓冲事件编库——detached_blobs 行带 lane/全量 key/
// origin_dir，payload 解回与内存缓冲同长的事件序列。
func TestDetachedFinishPersistsBlob(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "detached.db")
	db, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	registry := newDetachedRegistry(db, "lane-a")
	receiver := &pauseReceiver{pauseAt: 1, release: make(chan struct{}), frames: []*devinproto.GetChatMessageResponse{
		{DeltaText: proto.String("hi")},
		{StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum()},
		{Usage: &devinproto.ExaCodeiumCommonPb_ModelUsageStats{ModelUid: proto.String("m")}},
	}}
	stream := detachedTestStream(registry, "blob-key", receiver)
	ctx, cancel := context.WithCancel(context.Background())
	drainUntil(t, stream, ctx, func(e llm.ResponseEvent) bool {
		return e.Type == llm.ResponseEventTextDelta
	})
	cancel()
	if _, err := stream.Recv(ctx); err == nil {
		t.Fatal("Recv after client cancel should return the cancel cause")
	}
	entry := registry.lookup("blob-key")
	if entry == nil {
		t.Fatal("detached stream was not registered")
	}
	close(receiver.release)
	waitEntryState(t, entry, detachedCompleted)
	// persistBlob 在 finish 记账之后的同一泵协程内跑——行出现是异步的，
	// 直接轮询库而不是猜时序。独立连接读回与台账测试同口径：顺带证明
	// 写已提交、对外部读者可见。
	sqlDB, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=query_only(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	deadline := time.Now().Add(2 * time.Second)
	var payload []byte
	var lane, originDir string
	var finishedAt int64
	for time.Now().Before(deadline) {
		err := sqlDB.QueryRow(`SELECT lane, origin_dir, finished_at, payload FROM detached_blobs WHERE "key" = ?`, "blob-key").
			Scan(&lane, &originDir, &finishedAt, &payload)
		if err == nil {
			break
		}
		if err != sql.ErrNoRows {
			t.Fatalf("query detached_blobs: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if payload == nil {
		t.Fatal("no detached_blobs row after completed finish")
	}
	if lane != "lane-a" {
		t.Fatalf("blob lane = %q, want lane-a", lane)
	}
	decoded, err := decodeDetachedEvents(payload)
	if err != nil {
		t.Fatalf("decode persisted payload: %v", err)
	}
	if len(decoded) != entry.len() {
		t.Fatalf("persisted %d events, entry buffered %d", len(decoded), entry.len())
	}
	if decoded[len(decoded)-1].Type != llm.ResponseEventDone {
		t.Fatalf("last persisted event = %v, want done", decoded[len(decoded)-1].Type)
	}
}

// TestDetachedSeedReplays 钉住读方全链：detached_blobs 里 TTL 内的行
// 经 seed 灌回注册表成 completed 条目——lookup 命中、attach 重放到
// done、Seeded 计数与 seed 事件环痕迹齐全；过期行与异 lane 行不灌。
func TestDetachedSeedReplays(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	source := replayFixture(t, "text-answer.jsonl", nil, nil)
	payload, err := encodeDetachedEvents(source)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ctx := context.Background()
	if err := db.InsertDetachedBlob(ctx, store.DetachedBlob{
		Key: "fresh-key", Lane: "lane-a", OriginDir: "20260919-010203",
		FinishedAt: time.Now(), Payload: payload,
	}); err != nil {
		t.Fatalf("insert fresh: %v", err)
	}
	if err := db.InsertDetachedBlob(ctx, store.DetachedBlob{
		Key: "stale-key", Lane: "lane-a",
		FinishedAt: time.Now().Add(-2 * detachedCompletedTTL), Payload: payload,
	}); err != nil {
		t.Fatalf("insert stale: %v", err)
	}
	if err := db.InsertDetachedBlob(ctx, store.DetachedBlob{
		Key: "other-lane-key", Lane: "lane-b",
		FinishedAt: time.Now(), Payload: payload,
	}); err != nil {
		t.Fatalf("insert other-lane: %v", err)
	}
	registry := newDetachedRegistry(db, "lane-a")
	registry.seed()
	entry := registry.lookup("fresh-key")
	if entry == nil {
		t.Fatal("seeded entry should attach")
	}
	replayed, err := collectAttached(&attachStream{entry: entry})
	if err != nil {
		t.Fatalf("attach Recv: %v", err)
	}
	if len(replayed) != len(source) {
		t.Fatalf("replayed %d events, want %d", len(replayed), len(source))
	}
	if replayed[len(replayed)-1].Type != llm.ResponseEventDone {
		t.Fatalf("last replayed event = %v, want done", replayed[len(replayed)-1].Type)
	}
	if registry.lookup("stale-key") != nil {
		t.Fatal("expired row must not seed")
	}
	if registry.lookup("other-lane-key") != nil {
		t.Fatal("other-lane row must not seed")
	}
	stats := registry.stats()
	if stats.Seeded != 1 {
		t.Fatalf("seeded = %d, want 1", stats.Seeded)
	}
	var sawSeed bool
	for _, event := range stats.Events {
		if event.Kind == detachedEventSeed {
			sawSeed = true
		}
	}
	if !sawSeed {
		t.Fatal("event ring missing seed event")
	}
}

// TestDetachedSeedSkipsCorrupt 钉住坏行的降级语义：解不开的载荷计
// blobDrops 并跳过该行——播种整体不失败，与缓存不存在同语义。
func TestDetachedSeedSkipsCorrupt(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if err := db.InsertDetachedBlob(context.Background(), store.DetachedBlob{
		Key: "corrupt-key", Lane: "lane-a", FinishedAt: time.Now(), Payload: []byte("garbage"),
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	registry := newDetachedRegistry(db, "lane-a")
	registry.seed()
	if registry.lookup("corrupt-key") != nil {
		t.Fatal("corrupt row must not seed")
	}
	if stats := registry.stats(); stats.BlobDrops != 1 || stats.Seeded != 0 {
		t.Fatalf("stats = seeded %d blob_drops %d, want 0/1", stats.Seeded, stats.BlobDrops)
	}
}

// TestRegistryCloseStopsLedgerPump 钉住台账泵的关停契约：close 返回后
// ledgerDone 闭合——泵不退出会让 close 挂死，回归在这里是具名断言
// 失败而非整包超时。
func TestRegistryCloseStopsLedgerPump(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "detached.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	registry := newDetachedRegistry(db, "lane-a")
	closed := make(chan struct{})
	go func() { registry.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("registry.close did not return — ledger pump wedged")
	}
	select {
	case <-registry.ledgerDone:
	default:
		t.Fatal("ledgerDone still open after close")
	}
}
