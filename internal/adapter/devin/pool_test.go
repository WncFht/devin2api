// 本文件验证多账号池的纯逻辑面：rendezvous 钉选排序、健康分层、
// failoverable 词表、凭据失效冷却与 ApplyConfigs 热差集。lane 的
// BaseURL 指向 127.0.0.1:1——connWarmer 冷启动焐池只会撞
// connection refused，测试不触外网。
package devin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	devinproto "local/devinproto"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// testPoolConfig 返回一条指向本机即拒端点的 lane 配置。
func testPoolConfig(name string) Config {
	return Config{
		Name:    name,
		BaseURL: "http://127.0.0.1:1",
		Model:   "m",
		Token:   "tok-" + name,
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
		Config{Name: "dead", BaseURL: srvDead.URL, Model: "stub-model", Token: "tok-dead"},
		Config{Name: "good", BaseURL: srvGood.URL, Model: "stub-model", Token: "tok-good"},
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
		Config{Name: "flaky", BaseURL: srvFlaky.URL, Model: "stub-model", Token: "tok-f"},
		Config{Name: "idle", BaseURL: srvIdle.URL, Model: "stub-model", Token: "tok-i"},
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

// 凭据失效冷却的生命周期：unauthenticated 标死当前 token；TokenSource
// 换出不同凭据（经 reloadToken 落进 token 槽）惰性解禁；badUntil 过期
// 同样解禁；后到标记只延长不缩短。
func TestPoolLaneAuthCooldown(t *testing.T) {
	lane, err := newPoolLane(Config{
		Name:        "x",
		BaseURL:     "http://127.0.0.1:1",
		Model:       "m",
		Token:       "tok-x",
		TokenSource: func() string { return "tok-y" },
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
	lane.authMu.Lock()
	lane.badUntil = time.Now().Add(2 * badTokenCooldown)
	far := lane.badUntil
	lane.authMu.Unlock()
	lane.noteFailure(unauthenticatedErr())
	lane.authMu.Lock()
	if !lane.badUntil.Equal(far) {
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
		{Name: "a", BaseURL: "http://127.0.0.1:1", Model: "m", Token: "tok-a2"},
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
	if _, err := NewPool([]Config{{Name: "bad", BaseURL: "http://127.0.0.1:1"}}); err == nil {
		t.Fatal("NewPool with missing model must fail")
	}
	if _, err := NewPool([]Config{testPoolConfig("ok"), {Name: "bad", BaseURL: "http://127.0.0.1:1"}}); err == nil {
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
	if name := pool.CurrentConfig().Name; name != "a" {
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
