package devin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
)

// warmTestAdapter 是最小 Adapter：簿记只用到 gate/token/assignments/
// config；streamClient 不会被触达（测试注入假 sendPing）。
func warmTestAdapter() *Adapter {
	return &Adapter{
		token:       "test-token",
		gate:        newRateGate(GateConfig{}, ""),
		assignments: make(map[string]resolvedAssignment),
	}
}

// newTestWarmer 起一只钉死时钟与抖动的保温器；调度协程照跑（30s 真
// 钟节拍在测试内不触发），用例直接调 w.sweep() 驱动。
func newTestWarmer(t *testing.T, params WarmConfig) (*cacheWarmer, *fakeClock) {
	t.Helper()
	params.Enabled = true
	w := newCacheWarmer(warmTestAdapter(), params)
	clock := &fakeClock{t: time.Now()}
	w.now = clock.now
	w.jitter = func() float64 { return 0 }
	t.Cleanup(w.Close)
	return w, clock
}

// warmTestRequest 构造最小可投影请求：session+system+首条 user 文本。
func warmTestRequest(session, system, first string) llm.RequestMessages {
	return llm.RequestMessages{
		SessionKey:   session,
		SystemPrompt: system,
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: first}}},
		},
	}
}

// appendTurn 在请求尾追加一个 assistant+user 回合（客户端续写的标准
// 形态）；复制底层切片，不改原请求。
func appendTurn(request llm.RequestMessages, text string) llm.RequestMessages {
	messages := append([]llm.Message{}, request.Messages...)
	messages = append(messages,
		llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "reply"}}},
		llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: text}}},
	)
	request.Messages = messages
	return request
}

// seedPromoted 登记并晋升一条 lineage：首发 + 真追加第二发。
// MinPrefixTokens 需调到估计体量可达（测试请求 ~百字节 → /4 几十）。
func seedPromoted(w *cacheWarmer, request llm.RequestMessages, uid string) warmLineageKey {
	key := w.keyOf(request, uid)
	w.retain(key, request, uid, "")
	w.retain(key, appendTurn(request, "next"), uid, "")
	return key
}

// 键的稳定性与逐维敏感性：同请求同键，五个维度各改一处即换键；
// 关闭时返回零键让簿记全 no-op。
func TestWarmKeyOf(t *testing.T) {
	w, _ := newTestWarmer(t, WarmConfig{})
	base := warmTestRequest("sess", "system head", "hello")
	base.Tools = []llm.ToolDefinition{{Name: "Bash", Description: "run", InputSchema: []byte(`{"type":"object"}`)}}
	key := w.keyOf(base, "uid-a")
	if key == (warmLineageKey{}) {
		t.Fatal("enabled keyOf returned zero key")
	}
	if again := w.keyOf(base, "uid-a"); again != key {
		t.Fatal("identical request produced different key")
	}
	cases := map[string]warmLineageKey{
		"session": w.keyOf(warmTestRequest("sess-2", "system head", "hello"), "uid-a"),
		"system":  w.keyOf(warmTestRequest("sess", "system head v2", "hello"), "uid-a"),
		"message": w.keyOf(warmTestRequest("sess", "system head", "bye"), "uid-a"),
		"model":   w.keyOf(base, "uid-b"),
	}
	toolsChanged := warmTestRequest("sess", "system head", "hello")
	toolsChanged.Tools = append(append([]llm.ToolDefinition{}, base.Tools...),
		llm.ToolDefinition{Name: "Edit", Description: "edit", InputSchema: []byte(`{"type":"object"}`)})
	cases["tools"] = w.keyOf(toolsChanged, "uid-a")
	for name, other := range cases {
		if other == key {
			t.Fatalf("%s change did not alter lineage key", name)
		}
	}
	// 4K 截断内的 system 漂移才换键：尾部差异不进键（同口径种子）。
	tail := warmTestRequest("sess", strings.Repeat("s", 4096)+"TAIL-DRIFT", "hello")
	if k := w.keyOf(tail, "uid-a"); k.SysHash != w.keyOf(warmTestRequest("sess", strings.Repeat("s", 4096), "hello"), "uid-a").SysHash {
		t.Fatal("SysHash should only cover the 4K head")
	}
	var nilWarmer *cacheWarmer
	if nilWarmer.keyOf(base, "uid") != (warmLineageKey{}) {
		t.Fatal("nil warmer must return zero key")
	}
	off := newCacheWarmer(warmTestAdapter(), WarmConfig{Enabled: false})
	defer off.Close()
	if off.keyOf(base, "uid") != (warmLineageKey{}) {
		t.Fatal("disabled warmer must return zero key")
	}
}

// 晋升规则：首发登记 sends=1；逐字重发不推进（探活形态）；真追加
// 推进到 2 且前缀体量达标才晋升；非追加改写把 sends 归 1 重计。
func TestWarmPromotion(t *testing.T) {
	w, _ := newTestWarmer(t, WarmConfig{MinPrefixTokens: 1})
	request := warmTestRequest("sess", "sys", "m1")
	key := w.keyOf(request, "uid")
	w.retain(key, request, "uid", "")
	w.retain(key, request, "uid", "") // 逐字重发
	if got := w.entries[key].sends; got != 1 {
		t.Fatalf("identical resend sends = %d, want 1", got)
	}
	grown := appendTurn(request, "m2")
	w.retain(key, grown, "uid", "")
	if got := w.entries[key].sends; got != 2 {
		t.Fatalf("append sends = %d, want 2", got)
	}
	if got := w.stats().Promoted; got != 1 {
		t.Fatalf("Promoted = %d, want 1", got)
	}
	// 同键同消息数的原地改写（digest 变、体量净增 <64B）→ sends 归 1。
	mutated := grown
	mutated.Messages = append([]llm.Message{}, grown.Messages...)
	mutated.Messages[1] = llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "REPLY"}}}
	w.retain(key, mutated, "uid", "")
	if got := w.entries[key].sends; got != 1 {
		t.Fatalf("non-append rewrite sends = %d, want reset to 1", got)
	}
}

// 前缀体量门槛：usage 未观测时按 retained/4 估，不达标不晋升；
// 第一发完成响应的 input+cache_read 是实测值，替换估计值。
func TestWarmPromotionMinPrefix(t *testing.T) {
	w, _ := newTestWarmer(t, WarmConfig{MinPrefixTokens: 8192})
	request := warmTestRequest("sess", "sys", "m1") // 体量远小于 8K tok
	key := seedPromoted(w, request, "uid")
	if w.stats().Promoted != 0 {
		t.Fatal("small prefix must not promote on byte estimate")
	}
	w.noteCompleted(key, &llm.AssistantMessage{Usage: llm.Usage{Input: 2000, CacheRead: 7000}})
	if w.stats().Promoted != 1 {
		t.Fatal("observed input+cache_read >= min must promote")
	}
	// 观测值反过来压过估计：观测 <min 时即使体量估计够也不晋升
	//（1KB 请求估计 ~256 tok > min=100，观测 10 < 100）。
	w2, _ := newTestWarmer(t, WarmConfig{MinPrefixTokens: 100})
	big := warmTestRequest("sess", "sys", strings.Repeat("x", 1024))
	key2 := seedPromoted(w2, big, "uid")
	w2.noteCompleted(key2, &llm.AssistantMessage{Usage: llm.Usage{Input: 10}})
	if w2.entries[key2].sends < 2 || w2.stats().Promoted != 0 {
		t.Fatal("observed usage below min must demote the byte estimate")
	}
}

// 超任：同 session 恰好一维相异的旧 lineage 标 suspect；真实上行
// （noteSend/retain）撤标记；宽限期（2×Interval）内无上行退役；
// 两维相异是异型兄弟不标。
func TestWarmSupersession(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute})
	reqA := warmTestRequest("sess", "sys", "msg-a")
	keyA := w.keyOf(reqA, "uid")
	w.retain(keyA, reqA, "uid", "")
	// 恰好一维相异（msgHash）：A 被标 suspect。
	reqB := warmTestRequest("sess", "sys", "msg-b")
	w.retain(w.keyOf(reqB, "uid"), reqB, "uid", "")
	if w.entries[keyA].suspectAt.IsZero() {
		t.Fatal("one-dim-different sibling must mark A suspect")
	}
	if got := w.stats().Suspects; got != 1 {
		t.Fatalf("Suspects = %d, want 1", got)
	}
	// 真实上行撤标记。
	w.noteSend(keyA)
	if !w.entries[keyA].suspectAt.IsZero() {
		t.Fatal("real upstream send must clear suspect")
	}
	// 再标 + 宽限期满无上行 → 退役。
	reqC := warmTestRequest("sess", "sys", "msg-c")
	w.retain(w.keyOf(reqC, "uid"), reqC, "uid", "")
	if w.entries[keyA].suspectAt.IsZero() {
		t.Fatal("re-mark failed")
	}
	clock.t = clock.t.Add(2*time.Minute + time.Second)
	w.sweep()
	if _, ok := w.entries[keyA]; ok {
		t.Fatal("suspect past grace with no real send must retire")
	}
	// C 的到达同样把 B 标成 suspect（同 session 一维相异），宽限期满
	// 一并退役——Retired 累计 A+B 两条。
	if got := w.stats().Retired; got != 2 {
		t.Fatalf("Retired = %d, want 2", got)
	}
	// 两维相异（sys+msg 同变）不标：异型兄弟/新话题。
	w2, _ := newTestWarmer(t, WarmConfig{Interval: time.Minute})
	reqE := warmTestRequest("sess", "sys", "msg-e")
	keyE := w2.keyOf(reqE, "uid")
	w2.retain(keyE, reqE, "uid", "")
	reqF := warmTestRequest("sess", "other-sys", "msg-f")
	w2.retain(w2.keyOf(reqF, "uid"), reqF, "uid", "")
	if !w2.entries[keyE].suspectAt.IsZero() {
		t.Fatal("two-dim-different sibling must not mark suspect")
	}
}

// 档位判定：无 pending → sub 标记者 subDone、其余 userPaced；
// pending 全为提问类 → userPaced；含任何其他工具 → blocked；
// 无 SessionKey 恒 unknown。
func TestWarmTierClassify(t *testing.T) {
	w, _ := newTestWarmer(t, WarmConfig{})
	call := func(name string) llm.ToolCall {
		return llm.ToolCall{ID: "c-" + name, Name: name, Arguments: []byte(`{}`)}
	}
	cases := []struct {
		name    string
		session string
		system  string
		msg     *llm.AssistantMessage
		want    warmTier
	}{
		{"sub-done", "s", "x cc_is_subagent=true x", &llm.AssistantMessage{}, warmTierSubDone},
		{"turn-end", "s", "sys", &llm.AssistantMessage{}, warmTierUserPaced},
		{"ask-user", "s", "sys", &llm.AssistantMessage{Content: []llm.Content{call("AskUserQuestion")}}, warmTierUserPaced},
		{"blocked", "s", "sys", &llm.AssistantMessage{Content: []llm.Content{call("Bash")}}, warmTierBlocked},
		{"blocked-agent", "s", "sys", &llm.AssistantMessage{Content: []llm.Content{call("Agent")}}, warmTierBlocked},
		{"mixed-blocked", "s", "sys", &llm.AssistantMessage{Content: []llm.Content{call("AskUserQuestion"), call("Task")}}, warmTierBlocked},
		{"server-only", "s", "sys", &llm.AssistantMessage{Content: []llm.Content{llm.ToolCall{ID: "s1", Name: "web_search", Server: true}}}, warmTierUserPaced},
		{"no-session", "", "sys cc_is_subagent=true", &llm.AssistantMessage{Content: []llm.Content{call("Agent")}}, warmTierUnknown},
	}
	for _, tc := range cases {
		session := tc.session
		if session != "" {
			session = "s-" + tc.name // 各例独立流，互不触发超任标记
		}
		request := warmTestRequest(session, tc.system, "m")
		key := w.keyOf(request, "uid")
		w.retain(key, request, "uid", "")
		w.noteCompleted(key, tc.msg)
		if got := w.entries[key].tier; got != tc.want {
			t.Fatalf("%s: tier = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// 各档 maxIdle 退休：now-lastTouch 超档限即退役，ping 不续 lastTouch。
func TestWarmMaxIdleRetire(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{
		Interval:         time.Minute,
		BlockedMaxIdle:   time.Hour,
		UserPacedMaxIdle: 45 * time.Minute,
		SubDoneMaxIdle:   5 * time.Minute,
		UnknownMaxIdle:   10 * time.Minute,
	})
	mk := func(session, system string, msg *llm.AssistantMessage) warmLineageKey {
		request := warmTestRequest(session, system, "m-"+session)
		key := w.keyOf(request, "uid")
		w.retain(key, request, "uid", "")
		w.noteCompleted(key, msg)
		return key
	}
	call := llm.ToolCall{ID: "c", Name: "Bash", Arguments: []byte(`{}`)}
	blocked := mk("blocked", "sys", &llm.AssistantMessage{Content: []llm.Content{call}})
	userpaced := mk("userpaced", "sys", &llm.AssistantMessage{})
	subdone := mk("subdone", "sys cc_is_subagent=true", &llm.AssistantMessage{})
	unknown := w.keyOf(warmTestRequest("", "sys", "m-x"), "uid")
	w.retain(unknown, warmTestRequest("", "sys", "m-x"), "uid", "")

	clock.t = clock.t.Add(6 * time.Minute)
	w.sweep()
	if _, ok := w.entries[subdone]; ok {
		t.Fatal("subdone entry should retire past 5min idle")
	}
	for _, key := range []warmLineageKey{blocked, userpaced, unknown} {
		if _, ok := w.entries[key]; !ok {
			t.Fatalf("entry %v retired too early", key.SessionKey)
		}
	}
	clock.t = clock.t.Add(5 * time.Minute) // 11min
	w.sweep()
	if _, ok := w.entries[unknown]; ok {
		t.Fatal("unknown entry should retire past 10min idle")
	}
	if _, ok := w.entries[userpaced]; !ok {
		t.Fatal("userpaced should still be alive at 11min")
	}
	clock.t = clock.t.Add(35 * time.Minute) // 46min
	w.sweep()
	if _, ok := w.entries[userpaced]; ok {
		t.Fatal("userpaced should retire past 45min idle")
	}
	if _, ok := w.entries[blocked]; !ok {
		t.Fatal("blocked should still be alive at 46min")
	}
	clock.t = clock.t.Add(15 * time.Minute) // 61min
	w.sweep()
	if _, ok := w.entries[blocked]; ok {
		t.Fatal("blocked should retire past 1h idle")
	}
	if got := w.stats().Entries; got != 0 {
		t.Fatalf("Entries = %d, want 0", got)
	}
}

// 容量淘汰：suspect 优先、再按 lastTouch LRU；正在插入的条目永不挤。
func TestWarmCapacityEviction(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{MaxStreams: 3})
	mk := func(msg string) warmLineageKey {
		request := warmTestRequest("s-"+msg, "sys", msg)
		key := w.keyOf(request, "uid")
		w.retain(key, request, "uid", "")
		clock.t = clock.t.Add(time.Second)
		return key
	}
	old := mk("m-old")
	mid := mk("m-mid")
	keep := mk("m-keep")
	// 手动把 mid 标 suspect：挤占时它先于最旧的 old 出列。
	w.entries[mid].suspectAt = clock.t
	mk("m-new")
	if _, ok := w.entries[mid]; ok {
		t.Fatal("suspect should be evicted before older non-suspect")
	}
	if _, ok := w.entries[old]; !ok {
		t.Fatal("non-suspect must not be evicted while a suspect exists")
	}
	mk("m-newer") // mid 已走，此时挤最旧的 old
	if _, ok := w.entries[old]; ok {
		t.Fatal("LRU oldest should be evicted once suspects are gone")
	}
	if _, ok := w.entries[keep]; !ok {
		t.Fatal("younger entry must survive")
	}

	// 字节帽：1MB 上限下单条巨请求也受保护（正在插入），但再来一条
	// 就把它挤掉。
	wb, _ := newTestWarmer(t, WarmConfig{MaxRetainedMB: 1})
	giant := warmTestRequest("gs", "sys", strings.Repeat("x", 1500<<10))
	keyGiant := w.keyOf(giant, "uid")
	wb.retain(keyGiant, giant, "uid", "")
	if _, ok := wb.entries[keyGiant]; !ok {
		t.Fatal("the entry being inserted must never be evicted")
	}
	small := warmTestRequest("ss", "sys", "small")
	wb.retain(w.keyOf(small, "uid"), small, "uid", "")
	if _, ok := wb.entries[keyGiant]; ok {
		t.Fatal("over-cap bytes should evict the older entry")
	}
}

// 调度与 ping 账：晋升且到期才打；闸门闩内 tryAdmit 拒 → 跳过本轮
// 不消费配额；成功按 cache_read 分 hit/miss；cr=0 永不退役；发送即
// 推进 nextDue（节拍内最多一发）；真实上行把 nextDue 推后压 ping。
func TestWarmPingSchedule(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1})
	var calls int
	var lastReq *devinproto.GetChatMessageRequest
	w.sendPing = func(ctx context.Context, req *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		lastReq = req
		return 4416, nil
	}
	request := warmTestRequest("sess", "sys", "m1")
	key := seedPromoted(w, request, "uid")
	// 未到期不打。
	w.sweep()
	if calls != 0 {
		t.Fatal("ping fired before nextDue")
	}
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.sweep()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 due ping", calls)
	}
	if lastReq.GetConfiguration().GetMaxTokens() != 1 {
		t.Fatalf("ping MaxTokens = %d, want 1", lastReq.GetConfiguration().GetMaxTokens())
	}
	stats := w.stats()
	if stats.PingsSent != 1 || stats.PingHits != 1 || stats.PingMisses != 0 {
		t.Fatalf("stats = %+v, want 1 sent 1 hit", stats)
	}
	entry := w.entries[key]
	if entry.lastPingAt.IsZero() || !entry.nextDue.After(clock.t) {
		t.Fatal("successful ping must set lastPingAt and advance nextDue")
	}
	// 发送即消费本轮：紧接的 sweep 不再打。
	w.sweep()
	if calls != 1 {
		t.Fatal("next sweep must not re-ping inside the same interval")
	}
	// 真实上行把到期推后：noteSend 后下一拍不打。
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.noteSend(key)
	w.sweep()
	if calls != 1 {
		t.Fatal("noteSend must push nextDue past the sweep")
	}
	// cr=0 记 miss 但永不退役；随后静默超 maxIdle 才退役。
	w.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		return 0, nil
	}
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.sweep()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if w.stats().PingMisses != 1 {
		t.Fatalf("PingMisses = %d, want 1", w.stats().PingMisses)
	}
	if _, ok := w.entries[key]; !ok {
		t.Fatal("cr=0 ping must not retire the entry")
	}
	clock.t = clock.t.Add(31 * time.Minute) // >unknown 30min（无完成响应 → unknown 档）
	w.sweep()
	if _, ok := w.entries[key]; ok {
		t.Fatal("entry must retire once lastTouch exceeds maxIdle")
	}
	if w.stats().Retired == 0 {
		t.Fatal("idle retirement must count")
	}
}

// 闸门闩内 tryAdmit 一律拒：ping 不发出、记 PingSkips、条目不消费
// 本轮（下拍再试），且不占滴灌探针槽。
func TestWarmPingGateSkip(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1})
	pinGateClock(w.adapter.gate, 10)
	w.adapter.gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 8 minutes."))
	var calls int
	w.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		return 1, nil
	}
	seedPromoted(w, warmTestRequest("sess", "sys", "m1"), "uid")
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.sweep()
	if calls != 0 {
		t.Fatal("latched gate must block pings entirely")
	}
	if got := w.stats().PingSkips; got != 1 {
		t.Fatalf("PingSkips = %d, want 1", got)
	}
	// 解闩后下一拍正常发出。
	w.adapter.gate.noteUpstreamSuccess()
	w.sweep()
	if calls != 1 {
		t.Fatal("released gate should admit the due ping")
	}
}

// ping 错误分账：传输/未分类只跳本轮（PingErrors++，条目在）；
// 语义错误（ClientFixable）退役；cr=0 已在调度用例覆盖。
func TestWarmPingErrors(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1})
	var calls int
	w.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		return 0, connect.NewError(connect.CodeUnavailable, errors.New("connection reset by peer"))
	}
	request := warmTestRequest("sess", "sys", "m1")
	key := seedPromoted(w, request, "uid")
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.sweep()
	if got := w.stats().PingErrors; got != 1 {
		t.Fatalf("PingErrors = %d, want 1", got)
	}
	entry := w.entries[key]
	if entry == nil {
		t.Fatal("transport error must not retire")
	}
	if !entry.lastPingAt.IsZero() {
		t.Fatal("failed ping must not set lastPingAt")
	}
	if !entry.nextDue.After(clock.t) {
		t.Fatal("sent-but-failed round must still consume the round (nextDue advanced)")
	}
}

// 语义拒绝退役：invalid_argument（ClientFixable）→ 条目退役计数。
func TestWarmPingClientFixableRetires(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1})
	w.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("bad request shape"))
	}
	key := seedPromoted(w, warmTestRequest("sess", "sys", "m1"), "uid")
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.sweep()
	if _, ok := w.entries[key]; ok {
		t.Fatal("persistent ClientFixable must retire the entry")
	}
	stats := w.stats()
	if stats.Retired != 1 || stats.PingErrors != 1 {
		t.Fatalf("stats = %+v, want Retired=1 PingErrors=1", stats)
	}
}

// 凭证自愈：unauthenticated → reloadToken 换 token + 重发一次；重发
// 成功按成功结账；重发仍是 ClientFixable（permission_denied 同族）
// → 退役。自愈只一遍，不成环。
func TestWarmPingSelfHeal(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1})
	w.adapter.config.TokenSource = func() string { return "fresh-token" }
	var calls int
	w.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		if calls == 1 {
			return 0, connect.NewError(connect.CodeUnauthenticated, errors.New("bad token"))
		}
		return 4096, nil
	}
	key := seedPromoted(w, warmTestRequest("sess", "sys", "m1"), "uid")
	clock.t = clock.t.Add(time.Minute + time.Second)
	w.sweep()
	if calls != 2 {
		t.Fatalf("calls = %d, want heal resend once", calls)
	}
	if got := w.adapter.currentToken(); got != "fresh-token" {
		t.Fatalf("token = %q, want fresh-token", got)
	}
	stats := w.stats()
	if stats.PingsSent != 1 || stats.PingHits != 1 || stats.PingErrors != 0 {
		t.Fatalf("stats = %+v, want healed ping counted as sent hit", stats)
	}
	if _, ok := w.entries[key]; !ok {
		t.Fatal("healed entry must stay alive")
	}

	// 重发仍是凭证/语义拒绝 → 退役。用一只新保温器隔离 due 状态。
	w2, clock2 := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1})
	w2.adapter.config.TokenSource = func() string { return "fresh-token" }
	calls = 0
	w2.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		return 0, connect.NewError(connect.CodePermissionDenied, errors.New("model not authorized"))
	}
	key2 := seedPromoted(w2, warmTestRequest("sess2", "sys", "x1"), "uid")
	clock2.t = clock2.t.Add(time.Minute + time.Second)
	w2.sweep()
	if calls != 2 {
		t.Fatalf("calls = %d, want exactly one heal resend", calls)
	}
	if _, ok := w2.entries[key2]; ok {
		t.Fatal("permission_denied after heal must retire")
	}
}

// 关闭即全 no-op：簿记入口不进表、调度不打 ping、stats 报关态；
// nil 接收者与零键同样安全。
func TestWarmDisabledAndNilSafe(t *testing.T) {
	off := newCacheWarmer(warmTestAdapter(), WarmConfig{Enabled: false})
	defer off.Close()
	request := warmTestRequest("sess", "sys", "m1")
	key := warmLineageKey{SessionKey: "sess", SysHash: "s", MsgHash: "m", Model: "u"}
	off.retain(key, request, "uid", "")
	off.noteSend(key)
	off.noteCompleted(key, &llm.AssistantMessage{})
	off.sweep()
	if stats := off.stats(); stats.Enabled || stats.Entries != 0 {
		t.Fatalf("disabled stats = %+v, want zero", stats)
	}
	var nilWarmer *cacheWarmer
	nilWarmer.retain(key, request, "uid", "")
	nilWarmer.noteSend(key)
	nilWarmer.noteCompleted(key, &llm.AssistantMessage{})
	nilWarmer.setParams(WarmConfig{})
	nilWarmer.Close()
	if nilWarmer.stats() != (WarmStats{}) {
		t.Fatal("nil warmer stats must be zero")
	}
	// 开启态零键同样 no-op。
	w, _ := newTestWarmer(t, WarmConfig{})
	w.retain(warmLineageKey{}, request, "uid", "")
	w.noteSend(warmLineageKey{})
	w.noteCompleted(warmLineageKey{}, &llm.AssistantMessage{})
	if w.stats().Entries != 0 {
		t.Fatal("zero key must no-op")
	}
}

// noteCompleted 副账：response_model 进观测集、usage 刷新 prefixTokens；
// 中断/aborted 走不到这里由调用方保证。
func TestWarmNoteCompletedSideBooks(t *testing.T) {
	w, _ := newTestWarmer(t, WarmConfig{})
	request := warmTestRequest("sess", "sys", "m1")
	key := w.keyOf(request, "uid")
	w.retain(key, request, "uid", "")
	w.noteCompleted(key, &llm.AssistantMessage{
		ResponseModel: "swe-2-max",
		Usage:         llm.Usage{Input: 3000, CacheRead: 6000},
	})
	entry := w.entries[key]
	if _, ok := entry.observedModels["swe-2-max"]; !ok {
		t.Fatal("response_model must join observed set")
	}
	if entry.prefixTokens != 9000 {
		t.Fatalf("prefixTokens = %d, want 9000", entry.prefixTokens)
	}
	// 不存在的键与空 usage 都不炸。
	w.noteCompleted(w.keyOf(warmTestRequest("ghost", "sys", "g"), "uid"), &llm.AssistantMessage{})
	w.noteCompleted(key, &llm.AssistantMessage{})
	if entry.prefixTokens != 9000 {
		t.Fatal("zero usage must not overwrite observed prefixTokens")
	}
}

// 排空：BeginDrain 后到期条目不 ping、表留作观测；退役判定照常——
// 静默超档限的条目照样移除。
func TestWarmBeginDrain(t *testing.T) {
	w, clock := newTestWarmer(t, WarmConfig{Interval: time.Minute, MinPrefixTokens: 1, UnknownMaxIdle: 10 * time.Minute})
	calls := 0
	w.sendPing = func(context.Context, *devinproto.GetChatMessageRequest) (int64, error) {
		calls++
		return 100, nil
	}
	request := warmTestRequest("sess", "sys", "m1")
	key := seedPromoted(w, request, "uid")
	clock.t = clock.t.Add(2 * time.Minute)
	w.BeginDrain()
	w.sweep()
	if calls != 0 {
		t.Fatal("drained warmer must not ping")
	}
	if w.stats().Entries != 1 {
		t.Fatal("drain keeps the table for stats")
	}
	clock.t = clock.t.Add(9 * time.Minute)
	w.sweep()
	if _, ok := w.entries[key]; ok {
		t.Fatal("drained entry past maxIdle must still retire")
	}
	var nilWarmer *cacheWarmer
	nilWarmer.BeginDrain()
}
