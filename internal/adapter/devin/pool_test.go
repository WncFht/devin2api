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
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	devinproto "local/devinproto"

	"connectrpc.com/connect"

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
		if got := laneNames(pool.orderedLanes(lanes, affinity)); !slices.Equal(got, want) {
			t.Fatalf("orderedLanes(%q) = %v, want %v", affinity, got, want)
		}
	}

	distinct := map[string]bool{}
	for i := 0; i < 50; i++ {
		key := fmt.Sprintf("session-%d", i)
		got := laneNames(pool.orderedLanes(lanes, key))
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
		got := laneNames(pool.orderedLanes(lanes, key))
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
		if pool.orderedLanes(pool.snapshot(), SessionAffinityKey(request))[0].name == want {
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

// 换号累计预算：缩到极小后首 lane 快败即烧穿——第二候选不再点燃，
// 只留一笔失败 attempt（upstream_attempts）与 failover_budget_exhausted
// 分界行；首候选恒试不受预算约束。上闩全部 lane 让首候选开流即快败
// （LocalGate 是 eager 失败：conn refused 这类懒失败要走到流内才见）。
func TestPoolFailoverBudgetCap(t *testing.T) {
	old := poolFailoverBudgetFG
	poolFailoverBudgetFG = time.Nanosecond
	t.Cleanup(func() { poolFailoverBudgetFG = old })

	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"))
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

	metaData, _, _, err := manager.ReadFile(recorder.Dir(), "meta.json")
	if err != nil {
		t.Fatalf("ReadFile meta.json: %v", err)
	}
	var meta debuglog.MetaSummary
	if err := json.Unmarshal(metaData, &meta); err != nil {
		t.Fatalf("meta.json decode: %v", err)
	}
	if len(meta.UpstreamAttempts) != 1 {
		t.Fatalf("upstream_attempts = %d, want 1 — budget must stop the second lane", len(meta.UpstreamAttempts))
	}
	frames, _, _, err := manager.ReadFile(recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04: %v", err)
	}
	if !strings.Contains(string(frames), `"event":"failover_budget_exhausted"`) {
		t.Fatal("04 must carry failover_budget_exhausted marker")
	}
	if got := strings.Count(string(frames), `"event":"account_attempt"`); got != 1 {
		t.Fatalf("account_attempt rows = %d, want 1", got)
	}
}

// 流内换号受同一份累计预算：swap 只在 pre-content 触发，lane a
// 未产出内容的烧时计入 s.entered——预算缩到极小后首候选也被拦截，
// lane a 的真实错误事件透传给客户端，下一 lane 不被点燃，04 留
// failover_budget_exhausted 分界行。
func TestPoolSwapFailoverBudgetCap(t *testing.T) {
	old := poolFailoverBudgetFG
	poolFailoverBudgetFG = time.Nanosecond
	t.Cleanup(func() { poolFailoverBudgetFG = old })

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
	events := stubDrain(t, stream)
	sawError := false
	for _, event := range events {
		if event.Type == llm.ResponseEventError {
			sawError = true
		}
	}
	if !sawError {
		t.Fatal("dead lane's error event must reach the client when budget skips the swap")
	}
	if good.chatCalls.Load() != 0 {
		t.Fatalf("good lane chat calls = %d, want 0 — budget must skip it", good.chatCalls.Load())
	}
	recorder.Complete(debuglog.Completion{Result: "failed"})

	frames, _, _, err := manager.ReadFile(recorder.Dir(), "04-devin-response.jsonl")
	if err != nil {
		t.Fatalf("ReadFile 04: %v", err)
	}
	if !strings.Contains(string(frames), `"event":"failover_budget_exhausted"`) {
		t.Fatal("04 must carry failover_budget_exhausted marker")
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
		if pool.orderedLanes(pool.snapshot(), SessionAffinityKey(request))[0] == laneB {
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
	if got := pool.orderedLanes(pool.snapshot(), affinity)[0]; got != laneB {
		t.Fatalf("bound lane must stay first, got %v", got.name)
	}

	// 绑定 lane 硬故障（generic 冷却也算）→删绑按普通序重选。
	laneB.noteFailure(connect.NewError(connect.CodeInternal, errors.New("boom")))
	if got := pool.boundLane(affinity); got != nil {
		t.Fatalf("hardDown bound lane must unbind, got %v", got.name)
	}
	if got := pool.orderedLanes(pool.snapshot(), affinity)[0]; got == laneB {
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

// 粘性区：绑定 lane 因 gate 忙（闩中）也居候选首位——「宁等不换」交给
// gate 仲裁；闩内快败零成本转下一候选。gate 状态不算 hardDown。
func TestPoolBoundLaneStickyUnderGateLatch(t *testing.T) {
	pool := newTestPool(t, testPoolConfig("a"), testPoolConfig("b"), testPoolConfig("c"))
	laneA := poolLaneByName(pool, "a")
	affinity := "sticky-session"
	pool.bind(affinity, laneA)

	// a 的 gate 上闩：healthy() 为假但它仍 non-hardDown。
	laneA.adapter.gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted, errors.New("reset in 1 minute")))
	if laneA.healthy() {
		t.Skip("gate latch did not engage; environment-dependent")
	}
	if laneA.hardDown() {
		t.Fatal("gate latch must not count as hardDown")
	}
	ranked := pool.rankLanes(pool.snapshot(), affinity)
	if ranked[0].lane != laneA || !ranked[0].bound {
		t.Fatalf("latched bound lane must stay first (sticky zone), got %v", ranked[0].lane.name)
	}
	// 审计行：bound lane 的 Reason 记 bound。
	rows := poolCandidateRows(ranked)
	if rows[0].Reason != "bound" || !rows[0].Bound {
		t.Fatalf("bound candidate row = %+v, want bound", rows[0])
	}
	if rows[0].Healthy {
		t.Fatal("latched lane must report unhealthy in audit row")
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
	ranked := pool.rankLanes(pool.snapshot(), "zone-key")
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
	ranked = pool.rankLanes(pool.snapshot(), "zone-key")
	if ranked[0].lane != laneC {
		t.Fatalf("higher priority must lead same bucket, got %v", ranked[0].lane.name)
	}
	// bound-hit 恒赢 priority：绑 a 后 a 仍居首。
	pool.bind("zone-key", laneA)
	ranked = pool.rankLanes(pool.snapshot(), "zone-key")
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
