// 编排层 in-process 上游测试：用 httptest + 生成的 Connect handler 把真
// Adapter.Stream() 打到本地桩上，覆盖现有 receiver-fake 测试够不到的
// New/getChatMessageWithRetry/ensureCatalog/resolveModelRouting 链路与
// 真实 HTTP 帧往返。与 cmd/upstreamstub 的区别：那里是外部进程打真实
// 部署，这里是 per-test 进程内桩，断言能落到「第 N 次调用的 wire 请求」。
package devin

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// stubUpstream 实现 ApiServerServiceHandler：GetChatMessage 交给可替换的
// 脚本函数（按调用序号分场景），GetCliModelConfigs 回固定目录，AssignModel
// 回固定解析结果；其余 RPC 由嵌入的 Unimplemented 兜底。
type stubUpstream struct {
	devinprotoconnect.UnimplementedApiServerServiceHandler
	// chat 是每次 GetChatMessage 的处理脚本；nil 时回 unimplemented。
	chat func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error
	// catalog 是 GetCliModelConfigs 返回的模型列表。
	catalog []*devinproto.ExaCodeiumCommonPb_ClientModelConfig
	// assign 是 AssignModel 的处理函数；nil 时回 unimplemented。
	assign func(req *devinproto.AssignModelRequest) (*devinproto.AssignModelResponse, error)

	chatCalls atomic.Int32
	mu        sync.Mutex
	// requests/auths 按调用次序记录 wire 请求与 Authorization 头，
	// 供断言自愈换 token、continue 追加、router jwt 绑定等编排行为。
	requests []*devinproto.GetChatMessageRequest
	auths    []string
}

// GetChatMessage 记录请求后交给脚本；脚本返回的 error 直接成为 RPC 错误。
func (stub *stubUpstream) GetChatMessage(ctx context.Context, req *connect.Request[devinproto.GetChatMessageRequest], stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
	call := int(stub.chatCalls.Add(1))
	stub.mu.Lock()
	stub.requests = append(stub.requests, req.Msg)
	stub.auths = append(stub.auths, req.Header().Get("Authorization"))
	stub.mu.Unlock()
	if stub.chat == nil {
		return connect.NewError(connect.CodeUnimplemented, errors.New("chat not scripted"))
	}
	return stub.chat(call, req.Msg, stream)
}

// GetCliModelConfigs 返回固定模型目录。
func (stub *stubUpstream) GetCliModelConfigs(context.Context, *connect.Request[devinproto.GetCliModelConfigsRequest]) (*connect.Response[devinproto.GetCliModelConfigsResponse], error) {
	return connect.NewResponse(&devinproto.GetCliModelConfigsResponse{ClientModelConfigs: stub.catalog}), nil
}

// AssignModel 把 router uid 解析为真实模型；无脚本时按 unimplemented 失败。
func (stub *stubUpstream) AssignModel(ctx context.Context, req *connect.Request[devinproto.AssignModelRequest]) (*connect.Response[devinproto.AssignModelResponse], error) {
	if stub.assign == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("assign not scripted"))
	}
	resp, err := stub.assign(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// stubModelEntry 生成一条目录条目；router 为真时标 is_model_router。
func stubModelEntry(uid string, router bool) *devinproto.ExaCodeiumCommonPb_ClientModelConfig {
	entry := &devinproto.ExaCodeiumCommonPb_ClientModelConfig{
		ModelUid:       proto.String(uid),
		SupportsImages: proto.Bool(true),
	}
	if router {
		entry.ModelInfo = &devinproto.ExaCodeiumCommonPb_ModelInfo{IsModelRouter: proto.Bool(true)}
	}
	return entry
}

// stubFrames 是脚本侧便捷构造器：元数据帧 / 文本增量 / stop 终止帧。
func stubMeta() *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{
		MessageId: proto.String("bot-stub"),
		RequestId: proto.String("stub-req"),
		Timestamp: &devinproto.GoogleProtobuf_Timestamp{Seconds: proto.Int64(time.Now().Unix())},
		Usage:     &devinproto.ExaCodeiumCommonPb_ModelUsageStats{ModelUid: proto.String("stub-model")},
	}
}

func stubDelta(text string) *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{DeltaText: proto.String(text)}
}

func stubStop() *devinproto.GetChatMessageResponse {
	return &devinproto.GetChatMessageResponse{
		StopReason: devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN.Enum(),
	}
}

// stubSend 依次写出脚本帧并正常收尾（connect-go 自动补 EndStream）。
func stubSend(stream *connect.ServerStream[devinproto.GetChatMessageResponse], frames ...*devinproto.GetChatMessageResponse) error {
	for _, f := range frames {
		if err := stream.Send(f); err != nil {
			return err
		}
	}
	return nil
}

// stubEnvelope 编码一条 Connect 流式 envelope（raw 场景用）。
func stubEnvelope(msg *devinproto.GetChatMessageResponse) []byte {
	payload, err := proto.Marshal(msg)
	if err != nil {
		panic(err)
	}
	out := make([]byte, 5, 5+len(payload))
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	return append(out, payload...)
}

// stubEndStream 编码流终止 envelope（0x02 标志 + JSON 尾帧体）。
func stubEndStream(payload string) []byte {
	out := make([]byte, 5, 5+len(payload))
	out[0] = 0x02
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	return append(out, payload...)
}

// stubServer 搭 in-process 上游：Connect handler 挂服务前缀，rawChat 非空时
// 在 GetChatMessage 精确路径上覆盖一层裸 http.HandlerFunc（connect 的
// ServerStream 表达不了「envelope 写一半断流」这类帧级故障）。
func stubServer(t *testing.T, stub *stubUpstream, rawChat http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := devinprotoconnect.NewApiServerServiceHandler(stub)
	mux.Handle(path, handler)
	if rawChat != nil {
		mux.HandleFunc(devinprotoconnect.ApiServerServiceGetChatMessageProcedure, rawChat)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stubAdapter 起指向 stub 的真 Adapter；Gate 零值 → quota<=0 直通不等待。
func stubAdapter(t *testing.T, srv *httptest.Server, cfg Config) *Adapter {
	t.Helper()
	cfg.BaseURL = srv.URL
	adapter, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(adapter.Close)
	return adapter
}

// stubRequest 是最小合法中间请求：一条 user 文本。
func stubRequest() llm.RequestMessages {
	return llm.RequestMessages{
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
	}
}

// stubDrain 消费事件流到 EOF，返回全部事件；中途出错即 fail。
func stubDrain(t *testing.T, stream llm.ResponseStream) []llm.ResponseEvent {
	t.Helper()
	var events []llm.ResponseEvent
	for {
		event, err := stream.Recv(context.Background())
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		events = append(events, event)
	}
}

// stubDeltas 汇合全部 delta 文本；最后一个事件应是 Done。
func stubDeltas(t *testing.T, events []llm.ResponseEvent) (text string) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	var b strings.Builder
	for _, event := range events {
		b.WriteString(event.Delta)
	}
	last := events[len(events)-1]
	if last.Type != llm.ResponseEventDone || last.Reason != llm.StopReasonStop {
		t.Fatalf("last event = %#v, want done/stop", last)
	}
	return b.String()
}

// TestOrchestrationHappyPath 验证完整链路：真实 Connect 建流 → 帧解码 →
// 事件序列 → Done 聚合。
func TestOrchestrationHappyPath(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("hello "), stubDelta("world"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Token: "tok"})

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "hello world" {
		t.Fatalf("deltas = %q", got)
	}
	if stub.chatCalls.Load() != 1 {
		t.Fatalf("chat calls = %d", stub.chatCalls.Load())
	}
	if got := stub.requests[0].GetChatModelUid(); got != "stub-model" {
		t.Fatalf("wire model = %q", got)
	}
}

// TestOrchestrationUnauthenticatedSelfHeal 验证凭据自愈：首调用
// unauthenticated → TokenSource 换新 token → 重发成功，且第二次 wire
// 请求的 Authorization 确实换了。
func TestOrchestrationUnauthenticatedSelfHeal(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			if call == 1 {
				return connect.NewError(connect.CodeUnauthenticated, errors.New("stale token"))
			}
			return stubSend(stream, stubMeta(), stubDelta("ok"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	var sourceCalls atomic.Int32
	adapter := stubAdapter(t, srv, Config{
		Model: "stub-model",
		Token: "old",
		TokenSource: func() string {
			sourceCalls.Add(1)
			return "new"
		},
	})

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "ok" {
		t.Fatalf("deltas = %q", got)
	}
	if stub.chatCalls.Load() != 2 {
		t.Fatalf("chat calls = %d, want 2", stub.chatCalls.Load())
	}
	if sourceCalls.Load() != 1 {
		t.Fatalf("token source calls = %d, want 1", sourceCalls.Load())
	}
	if stub.auths[0] != "Basic old-old" || stub.auths[1] != "Basic new-new" {
		t.Fatalf("auth headers = %q", stub.auths)
	}
}

// TestOrchestrationUnauthenticatedSelfHealAtConnect 用裸 401 + connect JSON
// 错误体验证非 200 响应的自愈。connect-go 的 CallServerStream 在 Send/
// CloseRequest 后即返回，不等响应头——非 2xx 也推迟到首个 Receive 才暴露，
// 所以本场景与 EndStream 尾帧错误一样走流内 reopen 分支；Stream() 层的
// isUnauthenticated 重试只对「请求体发送期就失败」的形态可达（真实部署中
// 如代理在转发前拒掉），spec 合规的 Connect 上游不会触发它。
func TestOrchestrationUnauthenticatedSelfHealAtConnect(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
	}
	good := append(append(append(stubEnvelope(stubMeta()), stubEnvelope(stubDelta("healed"))...), stubEnvelope(stubStop())...), stubEndStream("{}")...)
	var rawCalls atomic.Int32
	raw := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		stub.chatCalls.Add(1)
		stub.mu.Lock()
		stub.auths = append(stub.auths, r.Header.Get("Authorization"))
		stub.mu.Unlock()
		if rawCalls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"unauthenticated","message":"stale token"}`))
			return
		}
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(good)
	}
	srv := stubServer(t, stub, raw)
	adapter := stubAdapter(t, srv, Config{
		Model:       "stub-model",
		Token:       "old",
		TokenSource: func() string { return "new" },
	})

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "healed" {
		t.Fatalf("deltas = %q", got)
	}
	if stub.chatCalls.Load() != 2 {
		t.Fatalf("chat calls = %d, want 2", stub.chatCalls.Load())
	}
	if stub.auths[0] != "Basic old-old" || stub.auths[1] != "Basic new-new" {
		t.Fatalf("auth headers = %q", stub.auths)
	}
}

// TestOrchestrationTruncatedStreamReopens 验证 pre-content 传输截断续跑：
// 第一次响应 envelope 写一半收尾（与 cmd/upstreamstub 的 precontent 同构），
// 第二次返回完整流。走裸 handler——connect ServerStream 表达不了半帧。
func TestOrchestrationTruncatedStreamReopens(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
	}
	var rawCalls atomic.Int32
	good := append(append(append(stubEnvelope(stubMeta()), stubEnvelope(stubDelta("recovered"))...), stubEnvelope(stubStop())...), stubEndStream("{}")...)
	raw := func(w http.ResponseWriter, r *http.Request) {
		stub.chatCalls.Add(1)
		call := rawCalls.Add(1)
		w.Header().Set("Content-Type", "application/connect+proto")
		w.WriteHeader(http.StatusOK)
		if call == 1 {
			// 元数据帧 + 半截 envelope 前缀 → 客户端读帧器报 unexpected EOF。
			_, _ = w.Write(append(stubEnvelope(stubMeta()), 0x00, 0x00))
			return
		}
		_, _ = w.Write(good)
	}
	srv := stubServer(t, stub, raw)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Token: "tok"})

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "recovered" {
		t.Fatalf("deltas = %q", got)
	}
	if rawCalls.Load() != 2 {
		t.Fatalf("chat calls = %d, want 2 (reopen)", rawCalls.Load())
	}
}

// TestOrchestrationEmptyEndTurnContinues 验证空 end_turn 续跑：首流只有
// stopReason 零内容 → adapter 追加 "continue" 用户消息重发 → 拿真内容。
func TestOrchestrationEmptyEndTurnContinues(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			if call == 1 {
				return stubSend(stream, stubMeta(), stubStop())
			}
			return stubSend(stream, stubMeta(), stubDelta("continued"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Token: "tok"})

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "continued" {
		t.Fatalf("deltas = %q", got)
	}
	if stub.chatCalls.Load() != 2 {
		t.Fatalf("chat calls = %d, want 2", stub.chatCalls.Load())
	}
	// 第二次 wire 请求的 prompts 应多出 "continue" 用户消息。
	raw, _ := protojson.Marshal(stub.requests[1])
	if !strings.Contains(string(raw), "continue") {
		t.Fatalf("second request missing continue: %s", raw)
	}
}

// TestOrchestrationRateGateLocalReject 验证本地闸门归因：上游先回
// resource_exhausted 上闩 → 下一请求被 gate.wait 快败，stage 记 rate_gate
// 且 LocalGate 标记存在（请求未触达上游）。
func TestOrchestrationRateGateLocalReject(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("stub: rate limited"))
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Token: "tok"})

	// 第一次：上游限流 → 上闩。server-stream 的 handler 错误经 EndStream
	// 尾帧送达，从 Recv 侧暴露为 error 事件而不是 Stream() 的返回错误。
	first, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("first Stream: %v", err)
	}
	var sawError bool
	for {
		event, recvErr := first.Recv(context.Background())
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("first Recv: %v", recvErr)
		}
		if event.Type == llm.ResponseEventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("first stream should surface an error event")
	}
	if !adapter.GateStats().Latched {
		t.Fatal("gate should be latched after upstream resource_exhausted")
	}

	// 第二次：带真 Recorder 验证 stage 归因 rate_gate。
	manager := debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{})
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/messages"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)
	_, err = adapter.Stream(ctx, stubRequest())
	if err == nil {
		t.Fatal("second call should be gate-rejected")
	}
	failure := llm.Classify(err)
	if !failure.LocalGate {
		t.Fatalf("LocalGate = false: %+v", failure)
	}
	stage, _ := recorder.FirstError()
	if stage != debuglog.ErrStageRateGate {
		t.Fatalf("first error stage = %q, want %q", stage, debuglog.ErrStageRateGate)
	}
	recorder.Complete(debuglog.Completion{Result: "failed"})
	// 闩内请求不该触达上游：chat 只被第一次调用打过。
	if stub.chatCalls.Load() != 1 {
		t.Fatalf("chat calls = %d, want 1 (gate fast-fail)", stub.chatCalls.Load())
	}
}

// TestOrchestrationModelRouterAssign 验证 router uid 解析链：目录标
// is_model_router → AssignModel → wire 请求带解析后 model uid 与 jwt。
func TestOrchestrationModelRouterAssign(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("router-x", true)},
		assign: func(req *devinproto.AssignModelRequest) (*devinproto.AssignModelResponse, error) {
			return &devinproto.AssignModelResponse{
				Assignment: &devinproto.ModelAssignment{
					ModelUid:      proto.String("resolved-y"),
					AssignmentJwt: proto.String("jwt-123"),
				},
			}, nil
		},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("routed"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "router-x", Token: "tok"})

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "routed" {
		t.Fatalf("deltas = %q", got)
	}
	req := stub.requests[0]
	if got := req.GetChatModelUid(); got != "resolved-y" {
		t.Fatalf("wire model = %q, want resolved-y", got)
	}
	if got := req.GetModelAssignmentJwt(); got != "jwt-123" {
		t.Fatalf("assignment jwt = %q", got)
	}
}

// TestOrchestrationApplyConfig 验证热应用：运行时字段（model/token/闸门）
// 换值即生效——下一次 Stream 的 wire 模型与 Authorization 同步切换；
// 烤进 transport 的字段（base_url）列入 requiresRestart。
func TestOrchestrationApplyConfig(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("x"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Token: "tok", Aliases: map[string]string{"a": "stub-model"}})

	if adapter.TokenFunc()() != "tok" {
		t.Fatal("TokenFunc should read current token")
	}
	if adapter.Aliases()["a"] != "stub-model" {
		t.Fatal("Aliases should reflect config")
	}
	next := Config{
		BaseURL: srv.URL, Model: "other-model", Token: "tok2",
		Aliases: map[string]string{"b": "stub-model"},
		Gate:    GateConfig{MaxRPM: 60},
	}
	applied, restart := adapter.ApplyConfig(next)
	for _, want := range []string{"devin.model", "devin.token", "devin.aliases", "devin.max_rpm"} {
		if !slices.Contains(applied, want) {
			t.Fatalf("applied %v missing %q", applied, want)
		}
	}
	if len(restart) != 0 {
		t.Fatalf("requiresRestart = %v, want empty", restart)
	}

	stream, err := adapter.Stream(context.Background(), stubRequest())
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stubDrain(t, stream)
	if got := stub.requests[0].GetChatModelUid(); got != "other-model" {
		t.Fatalf("wire model after ApplyConfig = %q", got)
	}
	if stub.auths[0] != "Basic tok2-tok2" {
		t.Fatalf("auth after ApplyConfig = %q", stub.auths[0])
	}
}
