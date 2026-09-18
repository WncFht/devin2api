// 本文件验证多账号池的纯逻辑面：rendezvous 钉选排序、健康分层、
// failoverable 词表、凭据失效冷却与 ApplyConfigs 热差集。lane 的
// BaseURL 指向 127.0.0.1:1——connWarmer 冷启动焐池只会撞
// connection refused，测试不触外网。
package devin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	devinproto "local/devinproto"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

// testPoolConfig 返回一条指向本机即拒端点的 lane 配置。
func testPoolConfig(name string) Config {
	return Config{
		Identity: LaneIdentity{Name: name, Token: "tok-" + name},
		Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"},
		Model:    "m",
	}
}

// newTestPool 建池并注册关闭：lane 后台协程（焐池/保温调度）随 Close 退出。
func newTestPool(t *testing.T, configs ...Config) *Pool {
	t.Helper()
	pool, err := NewPool(configs)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// poolLaneByName 在当前快照里按名取 lane；不存在返回 nil。
func poolLaneByName(pool *Pool, name string) *poolLane {
	for _, lane := range pool.snapshot() {
		if lane.name == name {
			return lane
		}
	}
	return nil
}

// laneNames 提取 lane 名序列供序比较。
func laneNames(lanes []*poolLane) []string {
	names := make([]string, len(lanes))
	for i, lane := range lanes {
		names[i] = lane.name
	}
	return names
}

// scoreOrder 按 rendezvous 分数（sha256(亲和键|lane 名) 升序）给名字
// 排序，是 orderedLanes 档内顺序的期望参照。
func scoreOrder(names []string, affinity string) []string {
	sorted := slices.Clone(names)
	slices.SortStableFunc(sorted, func(a, b string) int {
		sa := sha256.Sum256([]byte(affinity + "|" + a))
		sb := sha256.Sum256([]byte(affinity + "|" + b))
		return bytes.Compare(sa[:], sb[:])
	})
	return sorted
}

// unauthenticatedErr 构造与上游同形的凭据失效错误。
func unauthenticatedErr() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("session expired"))
}

// 同亲和键恒得同序（钉选）且等于 rendezvous 分数序；不同键各得全 lane
// 的一个排列，键足够多时序散开——钉选确实随键变化而非恒定输出。
func TestPoolOrderedLanesRendezvous(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	lanes := pool.snapshot()
	all := []string{"a", "b", "c"}

	affinity := "session-A"
	want := scoreOrder(all, affinity)
	for i := 0; i < 3; i++ {
		if got := laneNames(pool.orderedLanes(context.Background(), lanes, affinity)); !slices.Equal(got, want) {
			t.Fatalf("orderedLanes(%q) = %v, want %v", affinity, got, want)
		}
	}

	distinct := map[string]bool{}
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("session-%d", i)
		got := laneNames(pool.orderedLanes(context.Background(), lanes, key))
		distinct[strings.Join(got, ",")] = true
		if !slices.Equal(got, scoreOrder(all, key)) {
			t.Fatalf("orderedLanes(%q) = %v, want rendezvous order %v", key, got, scoreOrder(all, key))
		}
		sorted := slices.Clone(got)
		slices.Sort(sorted)
		if !slices.Equal(sorted, all) {
			t.Fatalf("orderedLanes(%q) = %v, not a permutation of %v", key, got, all)
		}
	}
	if len(distinct) < 2 {
		t.Fatal("50 affinity keys produced a single order; pinning does not vary with the key")
	}
}

// 不健康 lane 只排后不剔除：lane a 进凭据冷却后各亲和键下恒居末位，
// 健康档内部仍按分数排序。
func TestPoolOrderedLanesHealthTiers(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	lanes := pool.snapshot()
	laneA := poolLaneByName(pool, "a")

	laneA.authMu.Lock()
	laneA.badTokenHash = tokenHash("tok-a")
	laneA.badUntil = time.Now().Add(time.Hour)
	laneA.authMu.Unlock()

	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("session-%d", i)
		got := laneNames(pool.orderedLanes(context.Background(), lanes, key))
		if len(got) != 3 {
			t.Fatalf("orderedLanes(%q) = %v, unhealthy lane must be kept, not dropped", key, got)
		}
		if got[2] != "a" {
			t.Fatalf("orderedLanes(%q) = %v, unhealthy lane a must sort last", key, got)
		}
		if want := scoreOrder([]string{"b", "c"}, key); !slices.Equal(got[:2], want) {
			t.Fatalf("orderedLanes(%q) healthy tier = %v, want %v", key, got[:2], want)
		}
	}
}

// failoverable 词表围绕「换号能不能改变结果」：限流/传输断裂/凭据/权限
// 与有 code 的未知错误放行；客户端取消、请求形状错误与无 code 的本地
// 确定性失败（请求投影在触达上游前炸）不放行。
func TestPoolFailoverable(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"caller ctx canceled", canceledCtx, errors.New("boom"), false},
		{"context.Canceled error", context.Background(), context.Canceled, false},
		{"canceled failure", context.Background(), &llm.Failure{Code: "canceled", Message: "client went away"}, false},
		{"unauthenticated", context.Background(), unauthenticatedErr(), true},
		{"permission denied", context.Background(), connect.NewError(connect.CodePermissionDenied, errors.New("seat denied")), true},
		{"rate limited", context.Background(), connect.NewError(connect.CodeResourceExhausted, errors.New("slow down")), true},
		{"local gate", context.Background(), &llm.Failure{Code: "resource_exhausted", Message: "latched", LocalGate: true}, true},
		{"upstream fault", context.Background(), &llm.Failure{Code: "internal", Message: "boom", UpstreamFault: true}, true},
		{"client fixable", context.Background(), &llm.Failure{Code: "invalid_request_error", Message: "bad shape", ClientFixable: true}, false},
		{"invalid argument", context.Background(), connect.NewError(connect.CodeInvalidArgument, errors.New("bad field")), false},
		{"codeless local error", context.Background(), errors.New("mystery"), false},
		{"unknown coded error", context.Background(), connect.NewError(connect.CodeInternal, errors.New("mystery")), true},
	}
	for _, tc := range cases {
		if got := failoverable(tc.ctx, tc.err); got != tc.want {
			t.Errorf("failoverable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// pinnedRequest 返回亲和键钉选 want 为首的测试请求：逐号枚举文本
// 直到 rendezvous 分数序首项命中（两 lane 各 ~50%，收敛很快）。
func pinnedRequest(pool *Pool, want string) llm.RequestMessages {
	for i := 0; ; i++ {
		request := llm.RequestMessages{
			Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: fmt.Sprintf("pin-%d", i)}}}},
		}
		if pool.orderedLanes(context.Background(), pool.snapshot(), SessionAffinityKey(request))[0].name == want {
			return request
		}
	}
}

// poolStream 的流内换号：钉选 lane 的死 token 以流内首帧
// unauthenticated 败亡（Stream 已返回成功、lane 内自愈换不回同一份
// token），包装器拦下 error 事件换到下一 lane——客户端只见一份
// start 与救回内容，死 lane 进凭据冷却，之后同亲和键请求直连健康
// lane 不再先试死号。
func TestPoolInStreamFailover(t *testing.T) {
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	dead := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("dead token"))
		},
	}
	good := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("rescued"), stubStop())
		},
	}
	srvDead := stubServer(t, dead, nil)
	srvGood := stubServer(t, good, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "dead", Token: "tok-dead"}, Endpoint: Endpoint{BaseURL: srvDead.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "good", Token: "tok-good"}, Endpoint: Endpoint{BaseURL: srvGood.URL}, Model: "stub-model"},
	)

	manager := debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{}, nil)
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)

	request := pinnedRequest(pool, "dead")
	stream, err := pool.Stream(ctx, request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := stubDrain(t, stream)
	if got := stubDeltas(t, events); got != "rescued" {
		t.Fatalf("deltas = %q, want rescued", got)
	}
	starts := 0
	for _, event := range events {
		if event.Type == llm.ResponseEventStart {
			starts++
		}
		if event.Type == llm.ResponseEventError {
			t.Fatal("error event leaked to client despite successful failover")
		}
	}
	if starts != 1 {
		t.Fatalf("start events = %d, want exactly 1", starts)
	}
	if dead.chatCalls.Load() != 1 || good.chatCalls.Load() != 1 {
		t.Fatalf("chat calls dead=%d good=%d, want 1/1", dead.chatCalls.Load(), good.chatCalls.Load())
	}
	if !poolLaneByName(pool, "dead").authCooldown() {
		t.Fatal("dead lane should be in auth cooldown after unrecoverable unauthenticated")
	}

	// 死 lane 已在冷却：同亲和键第二次请求直接落 good，不再先试 dead。
	stream, err = pool.Stream(ctx, request)
	if err != nil {
		t.Fatalf("second Stream: %v", err)
	}
	stubDrain(t, stream)
	if dead.chatCalls.Load() != 1 || good.chatCalls.Load() != 2 {
		t.Fatalf("after cooldown: dead=%d good=%d, want 1/2", dead.chatCalls.Load(), good.chatCalls.Load())
	}
}

// failover 的 wire 证据：lane a 的首发占 03 基座文件，lane b 接管后
// 的首发必须续占 attemptN 分片而不是覆写基座——否则 lane a 的 wire
// 体被同名 REPLACE 顶掉，逐 lane 对账不可行。两个分片的 executionId
// 逐次 buildRequest 重铸，不同即证明基座仍是 lane a 的原文。
func TestPoolFailoverKeepsLaneWireBodies(t *testing.T) {
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	dead := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return connect.NewError(connect.CodeUnauthenticated, errors.New("dead token"))
		},
	}
	good := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("rescued"), stubStop())
		},
	}
	srvDead := stubServer(t, dead, nil)
	srvGood := stubServer(t, good, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "dead", Token: "tok-dead"}, Endpoint: Endpoint{BaseURL: srvDead.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "good", Token: "tok-good"}, Endpoint: Endpoint{BaseURL: srvGood.URL}, Model: "stub-model"},
	)

	db, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, db)
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)

	stream, err := pool.Stream(ctx, pinnedRequest(pool, "dead"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "rescued" {
		t.Fatalf("deltas = %q, want rescued", got)
	}
	recorder.Complete(debuglog.Completion{Result: "completed"})
	<-manager.Drained(recorder.Dir())

	stages, err := manager.DevinRequestStages(context.Background(), recorder.Dir())
	if err != nil {
		t.Fatalf("DevinRequestStages: %v", err)
	}
	if !slices.Contains(stages, "03-devin-request.json") || !slices.Contains(stages, "03-devin-request.attempt2.json") {
		t.Fatalf("wire stages = %v, want base + attempt2 (one per lane)", stages)
	}
	execID := func(name string) string {
		data, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), name)
		if err != nil {
			t.Fatalf("ReadFile %s: %v", name, err)
		}
		var body struct {
			ExecutionID string `json:"executionId"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			t.Fatalf("%s decode: %v", name, err)
		}
		return body.ExecutionID
	}
	base, shard := execID("03-devin-request.json"), execID("03-devin-request.attempt2.json")
	if base == "" || shard == "" || base == shard {
		t.Fatalf("executionId base=%q shard=%q, want distinct non-empty", base, shard)
	}

	metaData, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "meta.json")
	if err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
	var meta debuglog.MetaSummary
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta.json decode: %v", err)
	}
	if meta.UpstreamAccount != "good" || len(meta.UpstreamAttempts) != 1 || meta.UpstreamAttempts[0].Account != "dead" {
		t.Fatalf("account attribution = %q/%+v, want good after one dead attempt", meta.UpstreamAccount, meta.UpstreamAttempts)
	}
	if len(meta.RetryAttempts) != 0 {
		t.Fatalf("retry_attempts = %+v, want empty — failover is not a same-lane resend", meta.RetryAttempts)
	}

	frames, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04: %v", err)
	}
	if got := strings.Count(string(frames), `"event":"account_attempt"`); got != 2 {
		t.Fatalf("account_attempt rows = %d, want 2 (one per lane)", got)
	}
}

// poolStream 的换号边界：lane 已产出内容后才来的终局错误不能换号
// （客户端已见部分内容），原样透传且不触碰其余候选 lane。
func TestPoolInStreamNoFailoverAfterContent(t *testing.T) {
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	flaky := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			if err := stubSend(stream, stubMeta(), stubDelta("partial")); err != nil {
				return err
			}
			return connect.NewError(connect.CodeInternal, errors.New("mid-stream boom"))
		},
	}
	idle := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("idle"), stubStop())
		},
	}
	srvFlaky := stubServer(t, flaky, nil)
	srvIdle := stubServer(t, idle, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "flaky", Token: "tok-f"}, Endpoint: Endpoint{BaseURL: srvFlaky.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "idle", Token: "tok-i"}, Endpoint: Endpoint{BaseURL: srvIdle.URL}, Model: "stub-model"},
	)

	stream, err := pool.Stream(context.Background(), pinnedRequest(pool, "flaky"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var sawDelta, sawError bool
	for {
		event, recvErr := stream.Recv(context.Background())
		if errors.Is(recvErr, io.EOF) {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if event.Type == llm.ResponseEventTextDelta {
			sawDelta = true
		}
		if event.Type == llm.ResponseEventError {
			sawError = true
		}
	}
	if !sawDelta || !sawError {
		t.Fatalf("delta=%v error=%v, want both surfaced", sawDelta, sawError)
	}
	if idle.chatCalls.Load() != 0 {
		t.Fatalf("idle lane called %d times; post-content failure must not fail over", idle.chatCalls.Load())
	}
}

// 换号累计预算：缩到极小后首 lane 快败即烧穿——首个换号候选保底放行
// （非硬故障时预算不拦第一跳），第三候选才被预算拦截——三 lane 全闩
// 的收口是两条失败 attempt（upstream_attempts）与
// failover_budget_exhausted 分界行；首候选恒试不受预算约束。上闩全部
// lane 让候选开流即快败（LocalGate 是 eager 失败：conn refused 这类
// 懒失败要走到流内才见）。
func TestPoolFailoverBudgetCap(t *testing.T) {
	old := poolFailoverBudgetFG
	poolFailoverBudgetFG = time.Nanosecond
	t.Cleanup(func() { poolFailoverBudgetFG = old })

	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	for _, lane := range pool.snapshot() {
		lane.adapter.gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted, errors.New("rate limited; reset in 60 seconds")))
	}

	db, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, db)
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)

	_, err = pool.Stream(ctx, llm.RequestMessages{
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
	})
	if err == nil {
		t.Fatal("Stream must fail: every lane latched")
	}
	// 预算拦截回的是最后真实失败（首 lane 的闸门拒绝），不是合成错误。
	if failure := llm.Classify(err); failure == nil || !failure.LocalGate {
		t.Fatalf("returned error = %v, want the first lane's LocalGate rejection", err)
	}
	recorder.Complete(debuglog.Completion{Result: "failed"})
	<-manager.Drained(recorder.Dir())

	metaData, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "meta.json")
	if err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
	var meta debuglog.MetaSummary
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta.json decode: %v", err)
	}
	if len(meta.UpstreamAttempts) != 2 {
		t.Fatalf("upstream_attempts = %d, want 2 — first swap is guaranteed, budget stops the third lane", len(meta.UpstreamAttempts))
	}
	attempted := map[string]bool{}
	for _, attempt := range meta.UpstreamAttempts {
		attempted[attempt.Account] = true
	}
	var skipped string
	for _, name := range []string{"a", "b", "c"} {
		if !attempted[name] {
			skipped = name
		}
	}
	frames, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04: %v", err)
	}
	if !strings.Contains(string(frames), `"event":"failover_budget_exhausted"`) {
		t.Fatal("04 must carry failover_budget_exhausted marker")
	}
	if !strings.Contains(string(frames), `"skipped":"`+skipped+`"`) {
		t.Fatalf("marker must skip the untried lane %q, 04 = %s", skipped, frames)
	}
	if got := strings.Count(string(frames), `"event":"account_attempt"`); got != 2 {
		t.Fatalf("account_attempt rows = %d, want 2", got)
	}
}

// 流内换号受同一份累计预算，且与开流级共享「每请求一次保底」额度：
// lane a 开流即败时已用掉保底（b 被无条件点燃），b 流内 pre-content
// 再败时 swap 的下一候选恢复查账——预算缩到极小后 c 被拦截，b 的真实
// 错误事件透传给客户端，04 留 failover_budget_exhausted 分界行。
func TestPoolSwapFailoverBudgetCap(t *testing.T) {
	old := poolFailoverBudgetFG
	poolFailoverBudgetFG = time.Nanosecond
	t.Cleanup(func() { poolFailoverBudgetFG = old })

	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	dead := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("dead lane quota"))
		},
	}
	// in-stream 失败脚本：先发 meta 让开流成功，再以 handler 错误把
	// pre-content error 事件送进泵——swap 路径（区别于开流级失败）。
	midFail := func() *stubUpstream {
		return &stubUpstream{
			catalog: catalog,
			chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
				if err := stubSend(stream, stubMeta()); err != nil {
					return err
				}
				return connect.NewError(connect.CodeInternal, errors.New("in-stream boom"))
			},
		}
	}
	flaky1, flaky2 := midFail(), midFail()
	srvDead := stubServer(t, dead, nil)
	srvFlaky1 := stubServer(t, flaky1, nil)
	srvFlaky2 := stubServer(t, flaky2, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "dead", Token: "tok-dead"}, Endpoint: Endpoint{BaseURL: srvDead.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "flaky1", Token: "tok-f1"}, Endpoint: Endpoint{BaseURL: srvFlaky1.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "flaky2", Token: "tok-f2"}, Endpoint: Endpoint{BaseURL: srvFlaky2.URL}, Model: "stub-model"},
	)

	db, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, db)
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)

	stream, err := pool.Stream(ctx, pinnedRequest(pool, "dead"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	events := stubDrain(t, stream)
	sawError := false
	for _, event := range events {
		if event.Type == llm.ResponseEventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("second lane's error event must reach the client when budget skips the next swap")
	}
	if dead.chatCalls.Load() != 1 {
		t.Fatalf("dead lane chat calls = %d, want 1", dead.chatCalls.Load())
	}
	// 两条 flaky lane 谁排第二由钉选序定——被保底点燃的是第二候选，
	// 恰有一条被调用，另一条被预算拦在 swap 外。
	if got := flaky1.chatCalls.Load() + flaky2.chatCalls.Load(); got != 1 {
		t.Fatalf("flaky lanes chat calls = %d, want exactly 1 — guaranteed swap consumed by the open-level failover", got)
	}
	recorder.Complete(debuglog.Completion{Result: "failed"})
	<-manager.Drained(recorder.Dir())

	metaData, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "meta.json")
	if err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
	var meta debuglog.MetaSummary
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta.json decode: %v", err)
	}
	if len(meta.UpstreamAttempts) != 2 {
		t.Fatalf("upstream_attempts = %d, want 2 (dead open + one flaky in-stream)", len(meta.UpstreamAttempts))
	}

	frames, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04: %v", err)
	}
	if !strings.Contains(string(frames), `"event":"failover_budget_exhausted"`) {
		t.Fatal("04 must carry failover_budget_exhausted marker")
	}
}

// 预算兜底换号：首 lane 烧穿预算后，首个换号候选在非硬故障时保底
// 放行——预算与上游开流 deadline 同量级，无保底则健康兄弟永远接不
// 到管（prod ~80/日 504 死锁）。dead 未发一帧即 error 收尾（懒失败
// 走流内 swap 查账点），good 在预算耗尽状态下仍被点燃救回请求。
func TestPoolFailoverGuaranteedSwap(t *testing.T) {
	old := poolFailoverBudgetFG
	poolFailoverBudgetFG = time.Nanosecond
	t.Cleanup(func() { poolFailoverBudgetFG = old })

	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	dead := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return connect.NewError(connect.CodeResourceExhausted, errors.New("dead lane quota"))
		},
	}
	good := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("rescued"), stubStop())
		},
	}
	srvDead := stubServer(t, dead, nil)
	srvGood := stubServer(t, good, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "dead", Token: "tok-dead"}, Endpoint: Endpoint{BaseURL: srvDead.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "good", Token: "tok-good"}, Endpoint: Endpoint{BaseURL: srvGood.URL}, Model: "stub-model"},
	)

	manager := debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{}, nil)
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)

	stream, err := pool.Stream(ctx, pinnedRequest(pool, "dead"))
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "rescued" {
		t.Fatalf("deltas = %q, want rescued — first swap must be guaranteed despite exhausted budget", got)
	}
	if dead.chatCalls.Load() != 1 || good.chatCalls.Load() != 1 {
		t.Fatalf("chat calls dead=%d good=%d, want 1/1", dead.chatCalls.Load(), good.chatCalls.Load())
	}
}

// 保底只给「还值得试」的候选：下一候选处于池侧硬冷却（凭据/通用
// 两档）时，预算检查照常拦截——点燃判死 lane 只会复烧一条注定失败
// 的自愈链。cooled 被 noteFailure 判进通用冷却、dead 上闩出 eager
// 快败后，首 lane 烧穿预算的请求不再换号，真实错误原样返回。
func TestPoolFailoverBudgetSkipsHardDown(t *testing.T) {
	old := poolFailoverBudgetFG
	poolFailoverBudgetFG = time.Nanosecond
	t.Cleanup(func() { poolFailoverBudgetFG = old })

	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	cooled := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("rescued"), stubStop())
		},
	}
	srvCooled := stubServer(t, cooled, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "dead", Token: "tok-dead"}, Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "cooled", Token: "tok-c"}, Endpoint: Endpoint{BaseURL: srvCooled.URL}, Model: "stub-model"},
	)
	// dead 上闩让其 gate.wait 即拒（eager 失败走 Stream 循环查账点，
	// conn refused 这类懒失败会绕到流内 swap 路径——不是本测试目标）。
	poolLaneByName(pool, "dead").adapter.gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted, errors.New("rate limited; reset in 60 seconds")))
	poolLaneByName(pool, "cooled").noteFailure(connect.NewError(connect.CodeInternal, errors.New("boom")))

	db, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	manager := debuglog.NewManager(filepath.Join(t.TempDir(), "logs"), debuglog.RetentionPolicy{}, db)
	t.Cleanup(manager.Close)
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat"})
	ctx := debuglog.WithRecorder(context.Background(), recorder)

	_, err = pool.Stream(ctx, pinnedRequest(pool, "dead"))
	if err == nil {
		t.Fatal("Stream must fail: only candidate is hardDown and budget is exhausted")
	}
	if cooled.chatCalls.Load() != 0 {
		t.Fatalf("cooled lane chat calls = %d, want 0 — hardDown candidate must not get the free pass", cooled.chatCalls.Load())
	}
	recorder.Complete(debuglog.Completion{Result: "failed"})
	<-manager.Drained(recorder.Dir())

	frames, _, _, err := manager.ReadFile(context.Background(), recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04: %v", err)
	}
	if !strings.Contains(string(frames), `"event":"failover_budget_exhausted"`) ||
		!strings.Contains(string(frames), `"skipped":"cooled"`) {
		t.Fatalf("04 must carry failover_budget_exhausted{skipped:cooled}, 04 = %s", frames)
	}
}

// 凭据失效冷却的生命周期：unauthenticated 标死当前 token；TokenSource
// 换出不同凭据（经 reloadToken 落进 token 槽）惰性解禁；badUntil 过期
// 同样解禁；后到标记只延长不缩短。
func TestPoolLaneAuthCooldown(t *testing.T) {
	lane, err := newPoolLane(Config{
		Identity: LaneIdentity{
			Name:        "x",
			Token:       "tok-x",
			TokenSource: func() string { return "tok-y" },
		},
		Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"},
		Model:    "m",
	})
	if err != nil {
		t.Fatalf("newPoolLane: %v", err)
	}
	t.Cleanup(lane.adapter.Close)

	if lane.authCooldown() {
		t.Fatal("fresh lane must not be in auth cooldown")
	}
	lane.noteFailure(errors.New("permission_denied: nope"))
	if lane.authCooldown() {
		t.Fatal("non-unauthenticated failure must not mark cooldown")
	}
	lane.noteFailure(unauthenticatedErr())
	if !lane.authCooldown() {
		t.Fatal("unauthenticated failure must put lane in auth cooldown")
	}

	// 后到失败只延长不缩短：把闩拨远再标一次，截止时间不回退。
	// 退避档下窗内复发按当前档从 now 重算延长——与旧截止同档时
	// 允许微幅后延，语义判据是不缩短。
	lane.authMu.Lock()
	lane.badUntil = time.Now().Add(2 * badTokenCooldown)
	far := lane.badUntil
	lane.authMu.Unlock()
	lane.noteFailure(unauthenticatedErr())
	lane.authMu.Lock()
	if lane.badUntil.Before(far) {
		lane.authMu.Unlock()
		t.Fatal("later failure shortened the cooldown")
	}
	lane.authMu.Unlock()

	// 凭据轮换即解禁（authCooldown 按当前 token 哈希判等）。
	if !lane.adapter.reloadToken() {
		t.Fatal("reloadToken must succeed with a rotated TokenSource")
	}
	if lane.authCooldown() {
		t.Fatal("rotated token must clear auth cooldown")
	}

	// 同 token + 过期 badUntil → 解禁。
	lane.noteFailure(unauthenticatedErr())
	lane.authMu.Lock()
	lane.badUntil = time.Now().Add(-time.Second)
	lane.authMu.Unlock()
	if lane.authCooldown() {
		t.Fatal("expired cooldown must clear")
	}
}

// ApplyConfigs 按 lane 名差集：同名复用旧 adapter（token 原地热换），
// 新增建 lane，被删退出快照；空集合法——等同全部 lane 被删。
func TestPoolApplyConfigs(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")

	applied, err := pool.ApplyConfigs([]Config{
		{Identity: LaneIdentity{Name: "a", Token: "tok-a2"}, Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"}, Model: "m"},
		testPoolConfig("c"),
	})
	if err != nil {
		t.Fatalf("ApplyConfigs: %v", err)
	}
	if !slices.Contains(applied, "devin.accounts.a.token") {
		t.Fatalf("applied = %v, want devin.accounts.a.token", applied)
	}
	if poolLaneByName(pool, "a") != laneA {
		t.Fatal("lane a must reuse the existing poolLane")
	}
	funcs := pool.TokenFuncs()
	if _, ok := funcs["a"]; !ok {
		t.Fatal("TokenFuncs missing lane a")
	}
	if _, ok := funcs["c"]; !ok {
		t.Fatal("TokenFuncs missing new lane c")
	}
	if _, ok := funcs["b"]; ok {
		t.Fatal("TokenFuncs still holds removed lane b")
	}
	if token := funcs["a"](); token != "tok-a2" {
		t.Fatalf("lane a token = %q, want tok-a2 applied in place", token)
	}
	if token := funcs["c"](); token != "tok-c" {
		t.Fatalf("lane c token = %q, want tok-c", token)
	}

	applied, err = pool.ApplyConfigs(nil)
	if err != nil {
		t.Fatalf("ApplyConfigs(nil): %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("ApplyConfigs(nil) applied = %v, want empty", applied)
	}
	if len(pool.snapshot()) != 0 {
		t.Fatal("ApplyConfigs(nil) must empty the lane set")
	}
}

// 建池校验：空集合合法（空池）；任一 lane 构建失败整体报错。
func TestNewPoolValidation(t *testing.T) {
	pool := newTestPool(t)
	if len(pool.snapshot()) != 0 {
		t.Fatal("NewPool(nil) must produce an empty lane set")
	}
	if _, err := NewPool([]Config{{Identity: LaneIdentity{Name: "bad"}, Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"}}}); err == nil {
		t.Fatal("NewPool with missing model must fail")
	}
	if _, err := NewPool([]Config{testPoolConfig("ok"), {Identity: LaneIdentity{Name: "bad"}, Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"}}}); err == nil {
		t.Fatal("NewPool must fail when any lane fails to build")
	}
}

// 按号索引的视图键集与 lane 名一一对应；首 lane 视图绑配置序首号。
func TestPoolKeyedViews(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	want := []string{"a", "b", "c"}

	check := func(name string, keys []string) {
		t.Helper()
		slices.Sort(keys)
		if !slices.Equal(keys, want) {
			t.Fatalf("%s keys = %v, want %v", name, keys, want)
		}
	}
	var tokenKeys []string
	for name := range pool.TokenFuncs() {
		tokenKeys = append(tokenKeys, name)
	}
	check("TokenFuncs", tokenKeys)
	var gateKeys []string
	for name := range pool.AccountGateStats() {
		gateKeys = append(gateKeys, name)
	}
	check("AccountGateStats", gateKeys)
	var warmKeys []string
	for name := range pool.AccountWarmStats() {
		warmKeys = append(warmKeys, name)
	}
	check("AccountWarmStats", warmKeys)

	if token := pool.TokenFunc()(); token != "tok-a" {
		t.Fatalf("TokenFunc() = %q, want first lane token tok-a", token)
	}
	if name := pool.CurrentConfig().Identity.Name; name != "a" {
		t.Fatalf("CurrentConfig().Name = %q, want a", name)
	}
}

// 空池是合法态（账号被面板删光）：首 lane 系视图全部给零值不 panic；
// Stream/ListModels 显式报非 ClientFixable 的 unavailable——/v1 拿
// 5xx，而不是 (nil,nil) 让泵协程 nil deref。
func TestPoolEmptyPool(t *testing.T) {
	pool := newTestPool(t)

	if token := pool.TokenFunc()(); token != "" {
		t.Fatalf("TokenFunc() = %q, want empty on empty pool", token)
	}
	if stats := pool.GateStats(); !reflect.DeepEqual(stats, GateStats{}) {
		t.Fatalf("GateStats = %+v, want zero value", stats)
	}
	if stats := pool.WarmStats(); stats != (WarmStats{}) {
		t.Fatalf("WarmStats = %+v, want zero value", stats)
	}
	if aliases := pool.Aliases(); aliases != nil {
		t.Fatalf("Aliases = %v, want nil", aliases)
	}
	if cfg := pool.CurrentConfig(); !reflect.DeepEqual(cfg, Config{}) {
		t.Fatalf("CurrentConfig = %+v, want zero value", cfg)
	}
	if len(pool.TokenFuncs()) != 0 || len(pool.AccountGateStats()) != 0 ||
		len(pool.AccountWarmStats()) != 0 || len(pool.AccountLaneStates()) != 0 {
		t.Fatal("keyed views must be empty on empty pool")
	}
	if pool.ClearCooldown("a") {
		t.Fatal("ClearCooldown on empty pool must return false")
	}

	request := llm.RequestMessages{
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
	}
	calls := map[string]func() error{
		"Stream":     func() error { _, err := pool.Stream(context.Background(), request); return err },
		"ListModels": func() error { _, err := pool.ListModels(context.Background()); return err },
	}
	for name, call := range calls {
		err := call()
		failure := llm.Classify(err)
		if failure == nil || failure.Code != "unavailable" || failure.ClientFixable {
			t.Fatalf("%s error = %v, want non-ClientFixable unavailable failure", name, err)
		}
	}
}

// ClearCooldown 把两档冷却窗与 badTokenHash 判死键一并清掉，lane 立即
// 回候选（healthy 转真）；lastFailure* 证据保留；无该名 lane 返 false。
func TestPoolClearCooldown(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")

	if pool.ClearCooldown("nope") {
		t.Fatal("ClearCooldown on unknown name must return false")
	}

	laneA.noteFailure(unauthenticatedErr())
	laneA.noteFailure(connect.NewError(connect.CodeInternal, errors.New("mid boom")))
	if laneA.healthy() {
		t.Fatal("lane must be unhealthy inside cooldown windows")
	}
	if !laneA.authCooldown() {
		t.Fatal("unauthenticated failure must engage auth cooldown")
	}

	if !pool.ClearCooldown("a") {
		t.Fatal("ClearCooldown must return true for a live lane")
	}
	if !laneA.healthy() || laneA.authCooldown() {
		t.Fatal("lane must rejoin candidates immediately after ClearCooldown")
	}
	state := laneA.state()
	if state.AuthCooldownUntil != nil || state.UnhealthyUntil != nil {
		t.Fatalf("cooldown windows must be cleared: %+v", state)
	}
	if state.LastFailureCode != "internal" || state.LastFailureAt == nil {
		t.Fatalf("lastFailure evidence must survive clearing: %+v", state)
	}

	// 隔壁 lane 的冷却不受影响。
	laneB := poolLaneByName(pool, "b")
	laneB.noteFailure(connect.NewError(connect.CodeInternal, errors.New("boom")))
	pool.ClearCooldown("a")
	if !laneB.genericCooldown() {
		t.Fatal("ClearCooldown(a) must not touch lane b's cooldown")
	}
}

// 会话绑定生命周期：开流成功即写绑定；同亲和键后续请求直连绑定 lane
// （命中即续期，rendezvous 分数不再主导）；绑定 lane 硬故障（池侧冷却）
// 删绑按普通序重选；lane 摘除清绑；TTL 过期自然失效。
func TestPoolSessionBinding(t *testing.T) {
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	upstream := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("ok"), stubStop())
		},
	}
	srv := stubServer(t, upstream, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "a", Token: "tok-a"}, Endpoint: Endpoint{BaseURL: srv.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "b", Token: "tok-b"}, Endpoint: Endpoint{BaseURL: srv.URL}, Model: "stub-model"},
	)
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")

	// 钉一个亲和键到 b：反复试直到 rendezvous 把 b 排首位。
	var request llm.RequestMessages
	for i := 0; ; i++ {
		request = llm.RequestMessages{
			SessionKey: fmt.Sprintf("sess-%d", i),
			Messages:   []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "hi"}}}},
		}
		if pool.orderedLanes(context.Background(), pool.snapshot(), SessionAffinityKey(request))[0] == laneB {
			break
		}
	}
	affinity := SessionAffinityKey(request)
	stream, err := pool.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stubDrain(t, stream)
	if got := pool.boundLane(affinity); got != laneB {
		t.Fatalf("boundLane = %v, want laneB after successful open", got)
	}

	// 绑定命中恒居首：换一个分数序偏好 a 的请求形状（同 SessionKey → 同
	// 亲和键），b 仍排第一。
	if got := pool.orderedLanes(context.Background(), pool.snapshot(), affinity)[0]; got != laneB {
		t.Fatalf("bound lane must stay first, got %v", got.name)
	}

	// 绑定 lane 硬故障（generic 冷却也算）→删绑按普通序重选。
	laneB.noteFailure(connect.NewError(connect.CodeInternal, errors.New("boom")))
	if got := pool.boundLane(affinity); got != nil {
		t.Fatalf("hardDown bound lane must unbind, got %v", got.name)
	}
	if got := pool.orderedLanes(context.Background(), pool.snapshot(), affinity)[0]; got == laneB {
		t.Fatal("hardDown lane must not lead candidates")
	}

	// 冷却结束且 b 重新产出内容 → 换号接管语义下重新绑定。
	laneB.noteSuccess()
	stream, err = pool.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream after cooldown: %v", err)
	}
	stubDrain(t, stream)
	if got := pool.boundLane(affinity); got == nil {
		t.Fatal("successful lane must take the binding")
	}

	// lane 摘除清绑：b 从生效集退出后绑定不再指向它。
	pool.bind("other-session", laneA)
	if _, err := pool.ApplyConfigs([]Config{{Identity: LaneIdentity{Name: "a", Token: "tok-a"}, Endpoint: Endpoint{BaseURL: srv.URL}, Model: "stub-model"}}); err != nil {
		t.Fatalf("ApplyConfigs: %v", err)
	}
	pool.bindingsMu.Lock()
	for key, binding := range pool.bindings {
		if binding.lane == laneB {
			pool.bindingsMu.Unlock()
			t.Fatalf("binding %q still points to removed lane b", key)
		}
	}
	pool.bindingsMu.Unlock()

	// TTL 过期自然失效：手工把到期时刻拨到过去。
	pool.bindingsMu.Lock()
	b := pool.bindings["other-session"]
	b.expiry = time.Now().Add(-time.Second)
	pool.bindings["other-session"] = b
	pool.bindingsMu.Unlock()
	if got := pool.boundLane("other-session"); got != nil {
		t.Fatal("expired binding must be released")
	}
}

// 在飞钉选：同亲和键有在飞请求时后继钉同一 lane——正式绑定落地前的
// 并发窗口不再各自按当时的健康快照散选。钉选 lane 硬故障（池侧冷却）
// 即失效按普通序重选；绑定命中恒赢于在飞钉选。
func TestPoolInflightPin(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	lanes := pool.snapshot()
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")
	affinity := "inflight-session"

	pin := pool.inflightAcquire(affinity)
	pin.setLane(laneB)
	ranked := pool.rankLanes(context.Background(), lanes, affinity)
	if ranked[0].lane != laneB || !ranked[0].pinned {
		t.Fatalf("inflight-pinned lane must lead, got %v pinned=%v", ranked[0].lane.name, ranked[0].pinned)
	}
	if ranked[0].bound {
		t.Fatal("inflight pin must not mark as bound")
	}
	rows := poolCandidateRows(ranked)
	if !rows[0].Pinned {
		t.Fatalf("audit row must carry pinned flag: %+v", rows[0])
	}

	// 在飞 lane 硬故障 → 钉选失效按普通序重选。
	laneB.noteFailure(connect.NewError(connect.CodeInternal, errors.New("boom")))
	ranked = pool.rankLanes(context.Background(), lanes, affinity)
	if ranked[0].lane == laneB || ranked[0].pinned {
		t.Fatalf("hardDown inflight lane must not be pinned, got %v", ranked[0].lane.name)
	}
	laneB.noteSuccess()

	// 绑定恒赢于在飞钉选：绑 a 后 a 居首且记 bound。
	pool.bind(affinity, laneA)
	ranked = pool.rankLanes(context.Background(), lanes, affinity)
	if ranked[0].lane != laneA || !ranked[0].bound || ranked[0].pinned {
		t.Fatalf("bound must beat inflight pin, got %v bound=%v pinned=%v", ranked[0].lane.name, ranked[0].bound, ranked[0].pinned)
	}

	// failover 改派：在飞条目跟随最新指派 lane。
	pin.setLane(poolLaneByName(pool, "c"))
	if got := pool.inflightLane(affinity); got != poolLaneByName(pool, "c") {
		t.Fatalf("inflightLane after move = %v, want c", got.name)
	}

	// 释放归零删条目：同键后继回到绑定/分数序语义。
	pin2 := pool.inflightAcquire(affinity)
	pin.release()
	if pool.inflightLane(affinity) == nil {
		t.Fatal("second pin must keep the entry alive")
	}
	pin2.release()
	pool.inflightMu.Lock()
	_, exists := pool.inflight[affinity]
	pool.inflightMu.Unlock()
	if exists {
		t.Fatal("inflight entry must be removed at zero refcount")
	}
}

// 在飞钉选的粘性区同语义：在飞 lane 因 gate 忙（闩中）也仍钉住——
// 「宁等不换」与绑定一致，交给 gate 仲裁。
func TestPoolInflightPinStickyUnderGateLatch(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")
	affinity := "inflight-sticky"
	pin := pool.inflightAcquire(affinity)
	pin.setLane(laneA)
	t.Cleanup(pin.release)

	laneA.adapter.gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted, errors.New("reset in 1 minute")))
	if laneA.healthy() {
		t.Skip("gate latch did not engage; environment-dependent")
	}
	ranked := pool.rankLanes(context.Background(), pool.snapshot(), affinity)
	if ranked[0].lane != laneA || !ranked[0].pinned {
		t.Fatalf("latched inflight lane must stay pinned, got %v", ranked[0].lane.name)
	}
}

// 在飞钉选端到端：pre-seed 在飞指派到 b，rendezvous 分数偏好 a 的
// 后继请求仍落 b——钉选压过分数序（这正是并发窗口竞态的修复点：
// 前驱还没开流，后继已经按在飞指派同 lane 走）。
func TestPoolInflightPinDirectsStream(t *testing.T) {
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	upA := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("a"), stubStop())
		},
	}
	upB := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("b"), stubStop())
		},
	}
	srvA := stubServer(t, upA, nil)
	srvB := stubServer(t, upB, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "a", Token: "tok-a"}, Endpoint: Endpoint{BaseURL: srvA.URL}, Model: "stub-model"},
		Config{Identity: LaneIdentity{Name: "b", Token: "tok-b"}, Endpoint: Endpoint{BaseURL: srvB.URL}, Model: "stub-model"},
	)
	laneB := poolLaneByName(pool, "b")

	request := pinnedRequest(pool, "a")
	pin := pool.inflightAcquire(SessionAffinityKey(request))
	pin.setLane(laneB)
	defer pin.release()

	stream, err := pool.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	stubDrain(t, stream)
	if upA.chatCalls.Load() != 0 || upB.chatCalls.Load() != 1 {
		t.Fatalf("inflight pin must direct to b: a=%d b=%d", upA.chatCalls.Load(), upB.chatCalls.Load())
	}
}

// 粘性区 + 泄压阀：绑定 lane 闩中（expectedWait=闩剩余 ~60s，兄弟
// lane 绿）时本轮让位——bound 标记仍记审计但居首特权摘除，最优兄弟
// 领先。gate 状态不算 hardDown，绑定本身不删。
func TestPoolBoundLaneYieldsUnderGateLatch(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	laneA := poolLaneByName(pool, "a")
	affinity := "yield-session"
	pool.bind(affinity, laneA)

	// a 的 gate 上闩：healthy() 为假但它仍 non-hardDown。
	laneA.adapter.gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted, errors.New("reset in 1 minute")))
	if laneA.healthy() {
		t.Skip("gate latch did not engage; environment-dependent")
	}
	if laneA.hardDown() {
		t.Fatal("gate latch must not count as hardDown")
	}
	ranked := pool.rankLanes(context.Background(), pool.snapshot(), affinity)
	if ranked[0].lane == laneA {
		t.Fatalf("latched bound lane must yield to a healthy sibling, got %v", ranked[0].lane.name)
	}
	boundIdx := slices.IndexFunc(ranked, func(c poolCandidate) bool { return c.bound })
	if boundIdx < 0 || !ranked[boundIdx].yielded {
		t.Fatalf("bound candidate must carry yielded mark, ranked=%+v", ranked)
	}
	// 审计行：Bound 仍为真，Reason 记降级归因 + bound_yield。
	rows := poolCandidateRows(ranked)
	row := rows[boundIdx]
	if !row.Bound || !strings.Contains(row.Reason, "bound_yield") || !strings.Contains(row.Reason, "gate_latched") {
		t.Fatalf("yielded bound row = %+v, want Bound + gate_latched/bound_yield reason", row)
	}
	if row.Healthy {
		t.Fatal("latched lane must report unhealthy in audit row")
	}
	// 领先候选必须是健康兄弟。
	if !ranked[0].verdict.healthy {
		t.Fatalf("leader %v must be a healthy sibling", ranked[0].lane.name)
	}
}

// 让位的 τ 边沿：全 lane 同病（闩剩余相近）时无「最优兄弟」，绑定
// 维持居首——粘性只在确有更优落点时放开。
func TestPoolBoundLaneNoYieldWithoutBetterSibling(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")
	affinity := "all-sick-session"
	pool.bind(affinity, laneA)

	err := connect.NewError(connect.CodeResourceExhausted, errors.New("reset in 1 minute"))
	laneA.adapter.gate.noteUpstreamError(err)
	laneB.adapter.gate.noteUpstreamError(err)
	if laneA.healthy() || laneB.healthy() {
		t.Skip("gate latch did not engage; environment-dependent")
	}
	ranked := pool.rankLanes(context.Background(), pool.snapshot(), affinity)
	if ranked[0].lane != laneA || ranked[0].yielded {
		t.Fatalf("bound lane must stay first when no sibling is meaningfully better, got %v yielded=%v", ranked[0].lane.name, ranked[0].yielded)
	}
}

// 让位第二触发面：bound lane 未闩但前队拥堵（绿档深队）显著慢于
// 空闲兄弟——非对称饱和下 bound 烧 maxHold 的主场景。
func TestPoolBoundLaneYieldsOnDeepQueue(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")
	affinity := "queue-yield-session"
	pool.bind(affinity, laneA)

	// 给 a 造 fg 深队：quota 80、waitersFg 100 → fg expectedWait ~75s。
	gate := laneA.adapter.gate
	gate.mu.Lock()
	gate.quota = 80
	gate.waitersFg = 100
	gate.mu.Unlock()
	ranked := pool.rankLanes(context.Background(), pool.snapshot(), affinity)
	if ranked[0].lane == laneA {
		t.Fatalf("deep-queue bound lane must yield to the idle sibling, got %v", ranked[0].lane.name)
	}
	if !ranked[0].verdict.healthy {
		t.Fatal("idle sibling must lead as healthy")
	}
}

// bg 准入轨让位：bound lane 的拥堵全在 bg 侧（fgRateEMA 顶起预留 +
// 爬坡额度被 bucketUsedBg 吃成赤字 + bg 前队），fg 视图完全空闲——
// bg bound 会话让位给空闲兄弟，同状态 fg bound 会话不让位（估计器
// 按本类准入轨分账）。bg 是原事故中 bound 烧 maxHold 的主受害类，
// reserve/爬坡估计器也是让位判据里最复杂的一条路径。
func TestPoolBoundLaneYieldsOnBgCongestion(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")
	affinity := "bg-yield-session"
	pool.bind(affinity, laneA)

	// 钉在 :10——可发区间前段（usableLeft=48s，已开放 8s）。
	// reserve=ceil(60*48/60)+0+4=52 → 释放速率 (80-52)/56=0.5/s；
	// 爬坡额度 ceil(28*8/56)=4 < bucketUsedBg=5 → room=-1 赤字 +2s；
	// waitersBg=30 → bg 期望 30/0.5+2=62s。
	gate := laneA.adapter.gate
	clock := pinGateClock(gate, 10)
	gate.mu.Lock()
	gate.quota = 80
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 10
	gate.bucketUsedBg = 5
	gate.fgRateEMA = 60
	gate.waitersBg = 30
	gate.mu.Unlock()

	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	ranked := pool.rankLanes(bgCtx, pool.snapshot(), affinity)
	if ranked[0].lane == laneA {
		t.Fatalf("bg-congested bound lane must yield to the idle sibling, got %v", ranked[0].lane.name)
	}
	boundIdx := slices.IndexFunc(ranked, func(c poolCandidate) bool { return c.bound })
	if boundIdx < 0 || !ranked[boundIdx].yielded {
		t.Fatalf("bound candidate must carry yielded mark, ranked=%+v", ranked)
	}
	// 无降级归因词时审计行 Reason 只剩 bound_yield 一词。
	row := poolCandidateRows(ranked)[boundIdx]
	if !row.Bound || row.Reason != "bound_yield" {
		t.Fatalf("yielded bound row = %+v, want Bound + bound_yield reason", row)
	}
	if !ranked[0].verdict.healthy {
		t.Fatal("idle sibling must lead as healthy")
	}

	// 同状态 fg 视图：bg 专轨压力不进 fg 期望排队（waitersFg=0 →
	// ew=0），fg bound 会话维持粘性居首。
	ranked = pool.rankLanes(context.Background(), pool.snapshot(), affinity)
	if ranked[0].lane != laneA || ranked[0].yielded {
		t.Fatalf("fg-bound session must not yield to bg-only congestion, got %v yielded=%v", ranked[0].lane.name, ranked[0].yielded)
	}

	// τ 边界：waitersBg=4 → bg 期望 4/0.5+2=10s 恰等 τ，判据是严格
	// <，不让位。
	gate.mu.Lock()
	gate.waitersBg = 4
	gate.mu.Unlock()
	ranked = pool.rankLanes(bgCtx, pool.snapshot(), affinity)
	if ranked[0].lane != laneA || ranked[0].yielded {
		t.Fatalf("bound lane must stay at the τ boundary (strict <), got %v yielded=%v", ranked[0].lane.name, ranked[0].yielded)
	}
}

// bg 跨窗饥饿让位：死区内 bound lane 桶未用满，但投影下窗开放即满
// 预留（fgRateEMA 满段外推 + waitersFg + margin ≥ quota）时 bg 期望
// 追加一整窗——fg 饱和 lane 上 bg 实测要排到 bgMaxHold 才被拒。同
// 状态 fg 期望只吃队列项（<τ）不让位：饥饿项是 bg 专有加项。
func TestPoolBoundLaneYieldsOnBgStarvedWindow(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")
	affinity := "bg-starved-session"
	pool.bind(affinity, laneA)

	// 钉在 :59——死区（usable :02~:58），toNext=3s。
	// 下窗预留 ceil(80*56/60)+5+4=84≥80：bg 跨窗无槽，期望
	// 3+0+60=63s；fg 同态只算队列项 3+5/80*60=6.75s<τ。
	gate := laneA.adapter.gate
	clock := pinGateClock(gate, 59)
	gate.mu.Lock()
	gate.quota = 80
	gate.bucketStart = gate.windowStart(clock.t)
	gate.fgRateEMA = 80
	gate.waitersFg = 5
	gate.mu.Unlock()

	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	ranked := pool.rankLanes(bgCtx, pool.snapshot(), affinity)
	if ranked[0].lane == laneA {
		t.Fatalf("starved-window bound lane must yield for bg, got %v", ranked[0].lane.name)
	}
	boundIdx := slices.IndexFunc(ranked, func(c poolCandidate) bool { return c.bound })
	if boundIdx < 0 || !ranked[boundIdx].yielded {
		t.Fatalf("bound candidate must carry yielded mark, ranked=%+v", ranked)
	}
	row := poolCandidateRows(ranked)[boundIdx]
	if !row.Bound || !strings.Contains(row.Reason, "bound_yield") || !strings.Contains(row.Reason, "gate_window_full") {
		t.Fatalf("yielded bound row = %+v, want Bound + gate_window_full/bound_yield reason", row)
	}

	// fg bound 同状态不让位：饥饿项不压 fg 期望。
	ranked = pool.rankLanes(context.Background(), pool.snapshot(), affinity)
	if ranked[0].lane != laneA || ranked[0].yielded {
		t.Fatalf("fg-bound must not yield on the bg starvation term, got %v yielded=%v", ranked[0].lane.name, ranked[0].yielded)
	}
}

// 三区排序：健康档 → 配额低档 → 病档；档内 priority desc 再 rendezvous
// 分数升序。
func TestPoolRankLanesThreeZones(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")
	laneC := poolLaneByName(pool, "c")

	// c 配额低、b 病：三区各一条。
	laneC.quotaLow.Store(true)
	laneB.noteFailure(connect.NewError(connect.CodeInternal, errors.New("boom")))
	ranked := pool.rankLanes(context.Background(), pool.snapshot(), "zone-key")
	if ranked[0].lane != laneA {
		t.Fatalf("green lane must lead, got %v", ranked[0].lane.name)
	}
	if ranked[1].lane != laneC || ranked[1].verdict.bucket != 1 {
		t.Fatalf("quota-low lane must take the middle zone, got %v bucket %d", ranked[1].lane.name, ranked[1].verdict.bucket)
	}
	if ranked[2].lane != laneB {
		t.Fatalf("sick lane must be last, got %v", ranked[2].lane.name)
	}

	// 同桶内 priority desc：a/c 健康（c 已清 quotaLow）时高 priority 先。
	laneC.quotaLow.Store(false)
	laneA.priority.Store(1)
	laneC.priority.Store(9)
	ranked = pool.rankLanes(context.Background(), pool.snapshot(), "zone-key")
	if ranked[0].lane != laneC {
		t.Fatalf("higher priority must lead same bucket, got %v", ranked[0].lane.name)
	}
	// bound-hit 恒赢 priority：绑 a 后 a 仍居首。
	pool.bind("zone-key", laneA)
	ranked = pool.rankLanes(context.Background(), pool.snapshot(), "zone-key")
	if ranked[0].lane != laneA || !ranked[0].bound {
		t.Fatalf("bound-hit must beat priority, got %v", ranked[0].lane.name)
	}
}

// 连败退避：窗内复发不升档只延长；冷却过期后的新失败升档；成功清账
// 归零连败与两档冷却。
func TestPoolFailureBackoff(t *testing.T) {
	lane, err := newPoolLane(testPoolConfig("x"))
	if err != nil {
		t.Fatalf("newPoolLane: %v", err)
	}
	t.Cleanup(lane.adapter.Close)
	internalErr := func() error { return connect.NewError(connect.CodeInternal, errors.New("boom")) }

	lane.noteFailure(internalErr())
	if lane.failStreak != 1 {
		t.Fatalf("failStreak = %d, want 1", lane.failStreak)
	}
	firstUntil := lane.unhealthyUntil
	// 窗内复发：streak 不升，截止按当前档从 now 重算（不缩短）。
	lane.noteFailure(internalErr())
	if lane.failStreak != 1 {
		t.Fatalf("in-window failure must not raise streak, got %d", lane.failStreak)
	}
	if lane.unhealthyUntil.Before(firstUntil) {
		t.Fatal("in-window failure must not shorten cooldown")
	}
	// 旧窗过期后的新失败：升档 90s→180s。
	lane.authMu.Lock()
	lane.unhealthyUntil = time.Now().Add(-time.Second)
	lane.authMu.Unlock()
	lane.noteFailure(internalErr())
	if lane.failStreak != 2 {
		t.Fatalf("failStreak = %d, want 2", lane.failStreak)
	}
	if got := time.Until(lane.unhealthyUntil); got < 150*time.Second || got > 190*time.Second {
		t.Fatalf("streak-2 cooldown = %v, want ~180s", got)
	}
	// 成功清账：归零连败、两档冷却与判死键。
	lane.noteFailure(unauthenticatedErr())
	lane.noteSuccess()
	lane.authMu.Lock()
	clean := lane.failStreak == 0 && lane.badTokenHash == "" && lane.badUntil.IsZero() && lane.unhealthyUntil.IsZero()
	lane.authMu.Unlock()
	if !clean {
		t.Fatal("noteSuccess must clear streak, cooldowns and bad-token mark")
	}

	// backoffDuration 阶梯与封顶。
	if got := backoffDuration(90*time.Second, 30*time.Minute, 3); got != 6*time.Minute {
		t.Fatalf("generic streak-3 backoff = %v, want 6m", got)
	}
	if got := backoffDuration(90*time.Second, 30*time.Minute, 20); got != 30*time.Minute {
		t.Fatalf("generic backoff must cap at 30m, got %v", got)
	}
	if got := backoffDuration(10*time.Minute, time.Hour, 4); got != time.Hour {
		t.Fatalf("auth streak-4 backoff must cap at 1h, got %v", got)
	}
}

// 上游限流自述 reset 时 generic 冷却对齐 resetAt：上游报文带的
// "reset in N seconds/minutes" 是最优恢复点估计（分钟 hint 已由
// RateLimitReset 对齐桶界），固定 90s 退避会多压 ~30-60s；无 hint
// 或 RateLimited 不成立的失败仍回落连败退避档。
func TestPoolCooldownAlignsResetHint(t *testing.T) {
	lane, err := newPoolLane(testPoolConfig("x"))
	if err != nil {
		t.Fatalf("newPoolLane: %v", err)
	}
	t.Cleanup(lane.adapter.Close)

	lane.noteFailure(connect.NewError(connect.CodeResourceExhausted, errors.New("upstream message rate limited by local gate; reset in 25 seconds")))
	if got := time.Until(lane.unhealthyUntil); got < 20*time.Second || got > 30*time.Second {
		t.Fatalf("reset-hint cooldown = %v, want ~25s", got)
	}
	if lane.failStreak != 1 {
		t.Fatalf("reset-aligned failure still counts streak, got %d", lane.failStreak)
	}

	// 无 hint 的限流回退连败退避档（streak-1 = 90s）。
	fresh, err := newPoolLane(testPoolConfig("y"))
	if err != nil {
		t.Fatalf("newPoolLane fresh: %v", err)
	}
	t.Cleanup(fresh.adapter.Close)
	fresh.noteFailure(connect.NewError(connect.CodeResourceExhausted, errors.New("upstream message rate limited by local gate")))
	if got := time.Until(fresh.unhealthyUntil); got < 80*time.Second || got > 95*time.Second {
		t.Fatalf("no-hint cooldown = %v, want ~90s", got)
	}

	// 显式 "reset in 0 seconds"：桶界已到，冷却截止即现在——不追加封禁。
	zero, err := newPoolLane(testPoolConfig("z"))
	if err != nil {
		t.Fatalf("newPoolLane zero: %v", err)
	}
	t.Cleanup(zero.adapter.Close)
	zero.noteFailure(connect.NewError(connect.CodeResourceExhausted, errors.New("rate limited; reset in 0 seconds")))
	if zero.genericCooldown() {
		t.Fatal("reset-in-0 must not leave the lane in cooldown")
	}
}

// LocalGate 与 Canceled 豁免：gate 快败只记 lastFailure 证据不进冷却
// （gate 自身已是惩罚）；取消连证据都不记——请求方行为不是 lane 信号。
func TestPoolNoteFailureExemptions(t *testing.T) {
	lane, err := newPoolLane(testPoolConfig("x"))
	if err != nil {
		t.Fatalf("newPoolLane: %v", err)
	}
	t.Cleanup(lane.adapter.Close)

	lane.noteFailure(&llm.Failure{Code: "resource_exhausted", Message: "latched", LocalGate: true})
	lane.authMu.Lock()
	evidenced := lane.lastFailureCode == "resource_exhausted" && !lane.lastFailureAt.IsZero()
	cooled := !lane.unhealthyUntil.IsZero() || !lane.badUntil.IsZero() || lane.failStreak > 0
	lane.authMu.Unlock()
	if !evidenced {
		t.Fatal("LocalGate failure must keep lastFailure evidence")
	}
	if cooled {
		t.Fatal("LocalGate failure must not engage pool cooldown")
	}

	lane.noteFailure(context.Canceled)
	lane.authMu.Lock()
	// 判据是不动既有证据而非清零：LocalGate 记下的 lastFailure* 必须原样
	// 保留（canceled 早退在证据写入之前）。
	if lane.lastFailureCode != "resource_exhausted" {
		lane.authMu.Unlock()
		t.Fatalf("canceled must not touch failure evidence, code = %q", lane.lastFailureCode)
	}
	lane.authMu.Unlock()

	// 首条记录也不能是 canceled：换一个干净 lane 验证。
	fresh, err := newPoolLane(testPoolConfig("y"))
	if err != nil {
		t.Fatalf("newPoolLane fresh: %v", err)
	}
	t.Cleanup(fresh.adapter.Close)
	fresh.noteFailure(context.Canceled)
	fresh.authMu.Lock()
	if !fresh.lastFailureAt.IsZero() {
		fresh.authMu.Unlock()
		t.Fatal("canceled must not record failure evidence on a clean lane")
	}
	fresh.authMu.Unlock()
}

// 池侧冷却持久化：noteFailure 写 poolcool:<name> 行；lane 重建（模拟
// 重启）装载恢复未过期冷却与连败；ClearCooldown 删行。
func TestPoolCooldownPersistence(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	config := testPoolConfig("x")
	config.GateStateStore = db

	lane, err := newPoolLane(config)
	if err != nil {
		t.Fatalf("newPoolLane: %v", err)
	}
	lane.noteFailure(unauthenticatedErr())
	lane.adapter.Close()

	// 重建同名 lane：未过期冷却、连败与 lastFailure 证据一并恢复。
	rebuilt, err := newPoolLane(config)
	if err != nil {
		t.Fatalf("newPoolLane rebuild: %v", err)
	}
	t.Cleanup(rebuilt.adapter.Close)
	if !rebuilt.authCooldown() {
		t.Fatal("persisted auth cooldown must survive lane rebuild")
	}
	if rebuilt.failStreak != 1 || rebuilt.lastFailureCode != "unauthenticated" {
		t.Fatalf("persisted state = streak %d code %q", rebuilt.failStreak, rebuilt.lastFailureCode)
	}

	// ClearCooldown 删行：重建 lane 不再恢复任何冷却。
	pool := &Pool{bindings: make(map[string]laneBinding)}
	lanes := []*poolLane{rebuilt}
	pool.lanes.Store(&lanes)
	if !pool.ClearCooldown("x") {
		t.Fatal("ClearCooldown must hit live lane")
	}
	reloaded, err := newPoolLane(config)
	if err != nil {
		t.Fatalf("newPoolLane reload: %v", err)
	}
	t.Cleanup(reloaded.adapter.Close)
	if reloaded.authCooldown() || reloaded.failStreak != 0 {
		t.Fatal("cleared cooldown must not persist")
	}
}

// 惰性解禁同步落盘：凭据换出（或冷却到期）时 authCooldown 内存清判死
// 键的同时必须重写 poolcool 行——否则重启把已解禁的冷却复活。重写保留
// failStreak/lastFailure 证据（它们仍属有效簿记，不随判死键清）。
func TestPoolCooldownUnbanPersists(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	config := testPoolConfig("x")
	config.GateStateStore = db

	lane, err := newPoolLane(config)
	if err != nil {
		t.Fatalf("newPoolLane: %v", err)
	}
	lane.noteFailure(unauthenticatedErr())
	// 模拟凭据源换上新 token：adapter 的 token 与判死哈希分叉即解禁。
	lane.adapter.tokenMu.Lock()
	lane.adapter.token = "tok-rotated"
	lane.adapter.tokenMu.Unlock()
	if lane.authCooldown() {
		t.Fatal("rotated token must lift auth cooldown")
	}
	lane.adapter.Close()

	// 重建读回：badUntil 已清但 failStreak/证据仍在——行被重写而非删除。
	rebuilt, err := newPoolLane(config)
	if err != nil {
		t.Fatalf("newPoolLane rebuild: %v", err)
	}
	t.Cleanup(rebuilt.adapter.Close)
	if rebuilt.authCooldown() {
		t.Fatal("lifted cooldown must not resurrect after rebuild")
	}
	if rebuilt.failStreak != 1 || rebuilt.lastFailureCode != "unauthenticated" {
		t.Fatalf("rewrite must keep streak/evidence, got streak %d code %q", rebuilt.failStreak, rebuilt.lastFailureCode)
	}
}

// 配额降权：weekly 剩余低于阈值置 quotaLow；负阈值关闭恒 false；
// 已绑定会话不受影响（绑定命中恒居首）。
func TestPoolNoteQuotaSample(t *testing.T) {
	pool := newTestPool(t,
		testPoolConfig("a"),
		testPoolConfig("b"),
	)
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")

	pool.NoteQuotaSample("b", 50, 5)
	if !laneB.quotaLow.Load() {
		t.Fatal("weekly below default threshold must mark quotaLow")
	}
	pool.NoteQuotaSample("b", 50, 90)
	if laneB.quotaLow.Load() {
		t.Fatal("weekly above threshold must clear quotaLow")
	}
	pool.NoteQuotaSample("nonexistent", 0, 0) // 无名 lane 静默忽略

	// 负阈值关闭降权。
	pool2 := newTestPool(t, func() Config {
		c := testPoolConfig("a")
		c.QuotaLowThresholdPercent = -1
		return c
	}())
	pool2.NoteQuotaSample("a", 0, 1)
	if poolLaneByName(pool2, "a").quotaLow.Load() {
		t.Fatal("negative threshold must disable demotion")
	}
	_ = laneA
}

// weightOf 取候选快照里指定 lane 的排序权重；缺席回 -1。
func weightOf(ranked []poolCandidate, lane *poolLane) float64 {
	for _, c := range ranked {
		if c.lane == lane {
			return c.weight
		}
	}
	return -1
}

// laneWeight 的双因子语义：τ=10s 的压力折减、minMedian/median 的 TTFB
// 相对比、n/50 线性置信度；无样本与最快 lane 都回中性 1。
func TestLaneWeight(t *testing.T) {
	cases := []struct {
		name         string
		expectedWait time.Duration
		median       time.Duration
		n            int
		minMedian    time.Duration
		want         float64
	}{
		{"neutral", 0, 0, 0, 0, 1},
		{"pressure one tau", 10 * time.Second, 0, 0, 0, 0.5},
		{"pressure six tau", 60 * time.Second, 0, 0, 0, 1.0 / 7},
		{"ttfb full confidence", 0, 800 * time.Millisecond, 60, 100 * time.Millisecond, 0.125},
		{"ttfb half confidence", 0, 800 * time.Millisecond, 25, 100 * time.Millisecond, 0.5625},
		{"ttfb unsampled stays neutral", 0, 0, 0, 100 * time.Millisecond, 1},
		{"ttfb fastest lane", 0, 100 * time.Millisecond, 60, 100 * time.Millisecond, 1},
		{"pressure and ttfb multiply", 10 * time.Second, 800 * time.Millisecond, 60, 100 * time.Millisecond, 0.0625},
	}
	for _, tc := range cases {
		if got := laneWeight(tc.expectedWait, tc.median, tc.n, tc.minMedian); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("laneWeight(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// 闸门压力权重：a 的 fg 前队压 60 人 → expectedWait=60s → w_a=1/7，
// 加权 HRW 下 b 居首概率 7/8≈87.5%（200 键断言 >75%，~5σ 余量）。
// 权重按请求类分轨：bg 前队只压 bg 类候选的权重，fg 视图不受影响。
func TestPoolRankLanesPressureWeight(t *testing.T) {
	config := func(name string) Config {
		c := testPoolConfig(name)
		c.Gate = GateConfig{MaxRPM: 60}
		return c
	}
	pool := newTestPool(t, config("a"), config("b"))
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")
	// 钉在 :10——可发区间中段，expectedWait 只由前队深度折算。
	pinGateClock(laneA.adapter.gate, 10)
	pinGateClock(laneB.adapter.gate, 10)

	laneA.adapter.gate.mu.Lock()
	laneA.adapter.gate.waitersFg = 60
	laneA.adapter.gate.mu.Unlock()

	ctx := context.Background()
	probe := pool.rankLanes(ctx, pool.snapshot(), "weight-probe")
	if got := weightOf(probe, laneA); math.Abs(got-1.0/7) > 1e-9 {
		t.Fatalf("congested lane weight = %v, want 1/7", got)
	}
	if got := weightOf(probe, laneB); got != 1 {
		t.Fatalf("idle lane weight = %v, want 1", got)
	}
	if rows := poolCandidateRows(probe); rows[0].Weight <= 0 || rows[0].Weight > 1 {
		t.Fatalf("audit row must carry weight in (0,1]: %+v", rows[0])
	}

	wins := 0
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("weight-key-%d", i)
		if pool.rankLanes(ctx, pool.snapshot(), key)[0].lane == laneB {
			wins++
		}
	}
	if wins < 150 {
		t.Fatalf("weighted HRW must favor idle lane: b won %d/200, want >150", wins)
	}

	// 类分轨：bg 前队只进 bg 视图的 expectedWait。
	laneA.adapter.gate.mu.Lock()
	laneA.adapter.gate.waitersFg = 0
	laneA.adapter.gate.waitersBg = 60
	laneA.adapter.gate.mu.Unlock()
	ctxBg, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	if got := weightOf(pool.rankLanes(ctxBg, pool.snapshot(), "weight-probe"), laneA); got >= 1 {
		t.Fatalf("bg congestion must lower bg weight, got %v", got)
	}
	if got := weightOf(pool.rankLanes(ctx, pool.snapshot(), "weight-probe"), laneA); got != 1 {
		t.Fatalf("bg congestion must not affect fg weight, got %v", got)
	}
}

// TTFB 相对权重：a 中位 800ms、b 中位 100ms → w_a=0.125（满信），
// b 居首概率 1/1.125≈88.9%。样本不足 50 时权重向中性回缩（半信
// 0.5625）——没观测够的慢不被全量惩罚。
func TestPoolRankLanesTTFBWeight(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA := poolLaneByName(pool, "a")
	laneB := poolLaneByName(pool, "b")
	for i := 0; i < 60; i++ {
		laneA.noteTTFB(800 * time.Millisecond)
		laneB.noteTTFB(100 * time.Millisecond)
	}
	ctx := context.Background()
	lanes := pool.snapshot()
	probe := pool.rankLanes(ctx, lanes, "ttfb-probe")
	if got := weightOf(probe, laneA); math.Abs(got-0.125) > 1e-9 {
		t.Fatalf("slow lane weight = %v, want 0.125", got)
	}
	if got := weightOf(probe, laneB); got != 1 {
		t.Fatalf("fast lane weight = %v, want 1", got)
	}

	wins := 0
	for i := 0; i < 200; i++ {
		key := fmt.Sprintf("ttfb-key-%d", i)
		if pool.rankLanes(ctx, lanes, key)[0].lane == laneB {
			wins++
		}
	}
	if wins < 150 {
		t.Fatalf("weighted HRW must favor fast lane: b won %d/200, want >150", wins)
	}

	// 置信度回缩：a 只有 25 个样本（<50）→ w_a=1+(0.125-1)*0.5=0.5625。
	pool2 := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
	laneA2 := poolLaneByName(pool2, "a")
	laneB2 := poolLaneByName(pool2, "b")
	for i := 0; i < 25; i++ {
		laneA2.noteTTFB(800 * time.Millisecond)
	}
	for i := 0; i < 60; i++ {
		laneB2.noteTTFB(100 * time.Millisecond)
	}
	if got := weightOf(pool2.rankLanes(ctx, pool2.snapshot(), "ttfb-probe"), laneA2); math.Abs(got-0.5625) > 1e-9 {
		t.Fatalf("half-confidence weight = %v, want 0.5625", got)
	}
}

// 让位端到端：在飞钉选的 lane 桶满时闸门按 yield 快败把请求交给兄弟
// lane——钉选 lane 恒居候选首位（排序期让位只摘 bound 特权，不碰
// pinned），正是被吸收换号税的现场；无让位时 bg 请求会在 a 上睡到
// 下一窗口（~52s < bgMaxHold）再被放行，把同一结局推迟一个排队预算。
// a 的 chat 计数为 0 证明换号发生在本地快败（幻影换号，零上游发送）。
func TestPoolGateYieldToSibling(t *testing.T) {
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("stub-model", false)}
	upA := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("a"), stubStop())
		},
	}
	upB := &stubUpstream{
		catalog: catalog,
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("rescued"), stubStop())
		},
	}
	srvA := stubServer(t, upA, nil)
	srvB := stubServer(t, upB, nil)
	cfgA := testPoolConfig("a")
	cfgA.Endpoint.BaseURL = srvA.URL
	cfgA.Model = "stub-model"
	cfgA.Gate = GateConfig{MaxRPM: 1}
	cfgB := testPoolConfig("b")
	cfgB.Endpoint.BaseURL = srvB.URL
	cfgB.Model = "stub-model"
	pool := newTestPool(t, cfgA, cfgB)

	laneA := poolLaneByName(pool, "a")
	pinGateClock(laneA.adapter.gate, 10)
	if err := laneA.adapter.gate.wait(context.Background()); err != nil {
		t.Fatalf("seed wait error = %v, want pass", err)
	}

	request := llm.RequestMessages{
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "yield-pinned"}}}},
	}
	pin := pool.inflightAcquire(SessionAffinityKey(request))
	pin.setLane(laneA)
	defer pin.release()

	// bg 类请求复刻吸收现场：~52s 预计等待在 bgMaxHold 内，无让位
	// 谓词即盲睡到底；钉选+谓词下应立即让位给 b。
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	start := time.Now()
	stream, err := pool.Stream(bgCtx, request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Stream took %v, want yield fast-fail not absorbed sleep", d)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "rescued" {
		t.Fatalf("deltas = %q, want rescued", got)
	}
	if upA.chatCalls.Load() != 0 || upB.chatCalls.Load() != 1 {
		t.Fatalf("chat calls a=%d b=%d, want 0/1 (phantom switch)", upA.chatCalls.Load(), upB.chatCalls.Load())
	}
	if got := laneA.adapter.gate.stats().RejectYield; got != 1 {
		t.Fatalf("lane a RejectYield = %d, want 1", got)
	}
}
// AssignModel 挂起的 lane 经 preGateTimeout 判死后 failover：dead lane 的
// 闸门前解析吃 deadline_exceeded（failoverable 词表内），兄弟 lane 重新
// 解析救回请求；死 lane 进 generic 冷却——同亲和键的后续请求不再先试它。
func TestPoolAssignModelTimeoutFailover(t *testing.T) {
	defer func(d time.Duration) { preGateTimeout = d }(preGateTimeout)
	preGateTimeout = 100 * time.Millisecond
	catalog := []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntry("router-x", true)}
	dead := &stubUpstream{catalog: catalog, assignBlock: make(chan struct{})}
	good := &stubUpstream{
		catalog: catalog,
		assign: func(req *devinproto.AssignModelRequest) (*devinproto.AssignModelResponse, error) {
			return &devinproto.AssignModelResponse{
				Assignment: &devinproto.ModelAssignment{
					ModelUid:      proto.String("resolved-y"),
					AssignmentJwt: proto.String("jwt-1"),
				},
			}, nil
		},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("rescued"), stubStop())
		},
	}
	srvDead := stubServer(t, dead, nil)
	srvGood := stubServer(t, good, nil)
	pool := newTestPool(t,
		Config{Identity: LaneIdentity{Name: "dead", Token: "tok-dead"}, Endpoint: Endpoint{BaseURL: srvDead.URL}, Model: "router-x"},
		Config{Identity: LaneIdentity{Name: "good", Token: "tok-good"}, Endpoint: Endpoint{BaseURL: srvGood.URL}, Model: "router-x"},
	)

	request := pinnedRequest(pool, "dead")
	started := time.Now()
	stream, err := pool.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "rescued" {
		t.Fatalf("deltas = %q, want rescued", got)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("failover took %v, want ≈preGateTimeout", elapsed)
	}
	if dead.assignCalls.Load() != 1 || good.assignCalls.Load() != 1 {
		t.Fatalf("assign calls dead=%d good=%d, want 1/1", dead.assignCalls.Load(), good.assignCalls.Load())
	}
	if !poolLaneByName(pool, "dead").genericCooldown() {
		t.Fatal("dead lane should be in generic cooldown after assign timeout")
	}
	if got := good.requests[0].GetChatModelUid(); got != "resolved-y" {
		t.Fatalf("wire model = %q, want resolved-y", got)
	}
	if got := good.requests[0].GetModelAssignmentJwt(); got != "jwt-1" {
		t.Fatalf("assignment jwt = %q, want jwt-1", got)
	}
}
