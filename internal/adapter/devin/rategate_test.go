package devin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

func rateLimitErr(text string) error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New(text))
}

// fakeClock 是测试用静态假钟：wait/stats/闩事件全走 gate.now，
// 用例把钟钉在指定分钟秒位，再按需要手动推进。
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

// pinGateClock 把 gate 的时钟源换成静态假钟，初始钉在当前分钟第 sec 秒。
// 默认参数下可发区间是 :02~:58，:58~:02 是死区。
func pinGateClock(gate *rateGate, sec float64) *fakeClock {
	c := &fakeClock{t: time.Now().Truncate(time.Minute).Add(time.Duration(sec * float64(time.Second)))}
	gate.now = c.now
	return c
}

// offsetGateClock 把 gate.now 平移到当前分钟第 sec 秒并随真实时间前进：
// 用于让 wait 真实地睡一小段（睡醒复检依赖时钟随睡眠流逝）。
func offsetGateClock(gate *rateGate, sec float64) {
	real := time.Now()
	target := real.Truncate(time.Minute).Add(time.Duration(sec * float64(time.Second)))
	if !target.After(real) {
		target = target.Add(time.Minute)
	}
	delta := target.Sub(real)
	gate.now = func() time.Time { return time.Now().Add(delta) }
}

// 限流闩未到声明时刻：wait 本地拒绝且 retryAfter 等于闩剩余时长
// （分钟 hint 已向上对齐到 :59 桶界，实际可达 N*60+59s）。
func TestRateGateLatchRejectsUntilReset(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Please try again later. Your limit will reset in 8 minutes. (trace ID: x)"))
	err := gate.wait(context.Background())
	var failure *llm.Failure
	if !errors.As(err, &failure) || !failure.LocalGate {
		t.Fatalf("wait error = %v, want local-gate *llm.Failure", err)
	}
	if failure.RetryAfterSeconds < 480 || failure.RetryAfterSeconds > 545 {
		t.Fatalf("RetryAfterSeconds = %d, want 480~545 (bucket-aligned)", failure.RetryAfterSeconds)
	}
	// 分类记录必须能被公共错误管道译出 429。
	if status := common.HTTPStatus(failure); status != 429 {
		t.Fatalf("HTTPStatus = %d, want 429", status)
	}
}

// 闩内不排队：无论闩剩余长短都立即快败，Retry-After 报闩剩余，
// 由客户端睡到恢复时刻再来，而不是占着并发槽空等。
func TestRateGateLatchFastFails(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 1 seconds."))
	start := time.Now()
	err := gate.wait(context.Background())
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait held %v during latch, want instant fast-fail", elapsed)
	}
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) || gateErr.RetryAfterSeconds <= 0 || gateErr.RetryAfterSeconds > 1 {
		t.Fatalf("wait error = %v, want local-gate *llm.Failure with RetryAfterSeconds ~1s", err)
	}
}

// 闩内按滴灌间隔放行探针：槽空闲 → 放行；槽被占 → 快败。
// 探针是限流期间唯一到达上游的请求，负责探出解闩又不给上游续债。
func TestRateGateDripReleasesProbes(t *testing.T) {
	gate := newRateGate(GateConfig{DripInterval: 50 * time.Millisecond}, nil, "")
	clock := pinGateClock(gate, 10) // 可发区间内
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 30 seconds."))
	// 第一个槽在上闩后 dripInterval 才开放，先到请求快败。
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("first wait error = %v, want local-gate *llm.Failure (slot not open yet)", err)
	}
	clock.t = clock.t.Add(60 * time.Millisecond)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("drip-slot wait error = %v, want probe release", err)
	}
	// 槽已被取走，紧随其后的请求回到快败。
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("post-probe wait error = %v, want *llm.Failure", err)
	}
	clock.t = clock.t.Add(60 * time.Millisecond)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("next drip-slot wait error = %v, want probe release", err)
	}
}

// 死区内不放探针：滴灌槽空着但落在桶界死区时照样快败——桶界附近的
// 发送可能落进相邻真实上游桶白送计数。
func TestRateGateDripRespectsDeadZone(t *testing.T) {
	gate := newRateGate(GateConfig{DripInterval: time.Millisecond}, nil, "")
	clock := pinGateClock(gate, 10)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 60 seconds."))
	clock.t = clock.t.Add(48 * time.Second) // :58，闩内且进死区，槽已开
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("dead-zone wait error = %v, want *llm.Failure (no drip in dead zone)", err)
	}
	clock.t = clock.t.Add(5 * time.Second) // :03 下一分钟，回可发区间
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("sendable wait error = %v, want probe release", err)
	}
}

// 任一上游成功帧立即解闩：边际态下拒绝是概率执行，
// 成功帧是窗口已过的证据，不该再闩到声明时刻。
func TestRateGateUnlatchesOnUpstreamSuccess(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 10)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 30 seconds."))
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("latched wait error = %v, want *llm.Failure", err)
	}
	gate.noteUpstreamSuccess()
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("post-success wait error = %v, want released", err)
	}
}

// 非限流错误不上闩；新闩只延长不提前。
func TestRateGateLatchSelective(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	gate.noteUpstreamError(connect.NewError(connect.CodeInvalidArgument, errors.New("bad request")))
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want nil (no latch)", err)
	}
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 10 minutes."))
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 1 seconds."))
	err := gate.wait(context.Background())
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) || gateErr.RetryAfterSeconds < 590 {
		t.Fatalf("wait error = %v, want latch ~600s (max wins)", err)
	}
}

// "reset in 0 seconds" 是桶界到达的声明（新桶已爆、无追加封禁）：
// 闩态下不延长截止也不重排滴灌钟；无闩时闩到 now 即刻过期——
// 两种形态都不再落 60s 兜底闩（旧实现把它当无 hint，实测桶界上
// 每次 0-hint 都把闩续 60s 并重置滴灌，限流被自我续长）。
func TestRateGateZeroSecondHint(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	clock := pinGateClock(gate, 10)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 30 seconds."))
	latchedUntil := gate.limitedUntil
	nextDrip := gate.nextDrip
	clock.t = clock.t.Add(5 * time.Second)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 0 seconds."))
	if !gate.limitedUntil.Equal(latchedUntil) || !gate.nextDrip.Equal(nextDrip) {
		t.Fatalf("0-hint while latched must not extend latch or re-arm drip: until=%v drip=%v", gate.limitedUntil, gate.nextDrip)
	}
	// 无闩：0-hint 闩到 now 即刻过期，后续请求正常放行。
	fresh := newRateGate(GateConfig{}, nil, "")
	pinGateClock(fresh, 10)
	fresh.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 0 seconds."))
	if err := fresh.wait(context.Background()); err != nil {
		t.Fatalf("wait after unlatched 0-hint = %v, want pass (latch expired at arrival)", err)
	}
	if fresh.stats().Latched {
		t.Fatal("0-hint latch must expire cleanly")
	}
}

// http2 ENHANCE_YOUR_CALM 被 connect-go 映成 resource_exhausted——
// 那是传输层事件不是上游限流，不能拿来上闩。
func TestRateGateIgnoresTransportMasquerade(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 10)
	gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted,
		errors.New("bandwidth exhausted: stream error: stream ID 5; ENHANCE_YOUR_CALM; received from peer")))
	if gate.stats().Latched || gate.stats().LatchCount != 0 {
		t.Fatal("transport-masqueraded resource_exhausted must not latch")
	}
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait after masqueraded error = %v, want pass", err)
	}
}

// 分钟窗口配额：本桶放行数打满后，请求睡到下一窗口；预计等待超过
// maxHold 时本地拒绝，而不是放行去上游续债。
func TestRateGateWindowQuotaReject(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 2}, nil, "")
	pinGateClock(gate, 10)
	for i := 0; i < 2; i++ {
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("wait %d error = %v, want immediate pass", i, err)
		}
	}
	err := gate.wait(context.Background())
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) {
		t.Fatalf("excess wait error = %v, want *llm.Failure", err)
	}
	// :10 配额度尽 → 下一窗口 :02+60 开放，retryAfter ≈ 52s。
	if gateErr.RetryAfterSeconds < 50 || gateErr.RetryAfterSeconds > 53 {
		t.Fatalf("RetryAfterSeconds = %d, want ~52s (next window)", gateErr.RetryAfterSeconds)
	}
}

// 窗口翻转重新计数：上一桶的用量不结转。
func TestRateGateWindowRollover(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	clock := pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("first wait error = %v, want pass", err)
	}
	if err := gate.wait(context.Background()); err == nil {
		t.Fatal("second wait should be rejected (quota exhausted)")
	}
	clock.t = clock.t.Add(time.Minute) // 下一桶同秒位
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("new-bucket wait error = %v, want pass after rollover", err)
	}
}

// 窗口翻页把刚关闭窗口的明细账落进 gate_windows：用量/配额/拒绝成因
// 分列可直查，不再靠 logs.time+upstream_sent_ms 回推窗口消耗。
func TestRateGatePersistsClosedWindow(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 1}, db, store.GateStateKey("default"))
	clock := pinGateClock(gate, 10)
	windowStart := clock.t.Truncate(time.Minute).Add(2 * time.Second) // 默认可发区间 :02
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v", err)
	}
	// 桶满快败计入关闭窗口的 reject_quota 账。
	if err := gate.wait(context.Background()); err == nil {
		t.Fatal("second wait should be rejected (quota exhausted)")
	}
	clock.t = clock.t.Add(time.Minute) // 下一桶同秒位
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("new-bucket wait error = %v", err)
	}
	// 落库走一次性协程脱离 mu：轮询到行出现。
	var rows []*store.GateWindow
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if rows, err = db.ListGateWindows(context.Background(), "default", 0, 0); err != nil {
			t.Fatalf("ListGateWindows: %v", err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 closed window", len(rows))
	}
	w := rows[0]
	if w.Lane != "default" || w.WindowStart != windowStart.Unix() || w.Quota != 1 ||
		w.UsedFg != 1 || w.UsedBg != 0 || w.RejectQuota != 1 || w.RejectLatch != 0 {
		t.Fatalf("row = %+v", w)
	}
	if last := gate.stats().LastWindow; last == nil || last.WindowStart != windowStart.Unix() {
		t.Fatalf("stats().LastWindow = %+v", last)
	}
	// 新窗口未关闭前不再出第二行。
	if rows, _ = db.ListGateWindows(context.Background(), "", 0, 0); len(rows) != 1 {
		t.Fatalf("rows after reopen = %d, want still 1", len(rows))
	}
}

// 窗口行写失败挂进重放缓冲随后续翻页重放：关闭库让每次 INSERT 必败，
// 三次翻页各产一笔失败账（persist_failures=3），缓冲深度
// gatePersistRetryCap=2 溢出后丢一笔最老行（persist_dropped=1）——
// 终局计数与协程落锁时序无关（守恒：推入 = 取走 + 丢弃 + 在缓）。
func TestRateGateWindowPersistRetry(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	clock := pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v", err)
	}
	_ = db.Close() // 持久化协程写必败
	for i := 0; i < 3; i++ {
		clock.t = clock.t.Add(time.Minute)
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("flip %d wait error = %v", i, err)
		}
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if stats := gate.stats(); stats.PersistFailures == 3 && stats.PersistDropped == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats := gate.stats()
	t.Fatalf("persist counters = failures:%d dropped:%d, want 3/1", stats.PersistFailures, stats.PersistDropped)
}

// 续试重发的放行单列进 retry_admits 窗口账：挂 WithGateRetry 的放行
// 计入 retry_admits，首发不挂不计——两者都照常占 used 配额（used
// 与 retry_admits 是总数与子集的关系，不是分列口径）。
func TestRateGateWindowRetryAdmits(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	clock := pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("first wait error = %v", err)
	}
	if err := gate.wait(withGateRetry(context.Background())); err != nil {
		t.Fatalf("retry wait error = %v", err)
	}
	clock.t = clock.t.Add(time.Minute) // 翻页触发关窗落库
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("new-bucket wait error = %v", err)
	}
	var rows []*store.GateWindow
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if rows, err = db.ListGateWindows(context.Background(), "default", 0, 0); err != nil {
			t.Fatalf("ListGateWindows: %v", err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 closed window", len(rows))
	}
	if w := rows[0]; w.UsedFg != 2 || w.RetryAdmits != 1 {
		t.Fatalf("row = %+v, want used_fg=2 retry_admits=1", w)
	}
}
func TestRateGateDeadZoneSleepsToNextWindow(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	offsetGateClock(gate, 1.9) // 死区尾，距 :02 开放 ~100ms
	start := time.Now()
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want pass after short sleep", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond || d > 2*time.Second {
		t.Fatalf("wait took %v, want ~100ms sleep until window opens", d)
	}
}

// 死区等待超 maxHold 直接快败，Retry-After 报到下一窗口的剩余。
func TestRateGateDeadZoneFastFails(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1, MaxHold: time.Second}, nil, "")
	pinGateClock(gate, 58.5) // 死区头，下一窗口 ~3.5s > maxHold
	err := gate.wait(context.Background())
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) {
		t.Fatalf("wait error = %v, want *llm.Failure", err)
	}
	if gateErr.RetryAfterSeconds < 3 || gateErr.RetryAfterSeconds > 4 {
		t.Fatalf("RetryAfterSeconds = %d, want ~3.5s (next window)", gateErr.RetryAfterSeconds)
	}
}

// quota<=0 不做窗口限速：死区内也直接放行。
func TestRateGateZeroQuotaUnlimited(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 59) // 死区
	for i := 0; i < 3; i++ {
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("wait %d error = %v, want pass (no window limit)", i, err)
		}
	}
}

// 等待中 ctx 取消：返回取消原因，waiters 名额归还。
func TestRateGateWaitCancelRefunds(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	offsetGateClock(gate, 58.2) // 死区，睡到 :02 约 3.8s < maxHold
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := gate.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context.Canceled", err)
	}
	if waiters := gate.stats().Waiters; waiters != 0 {
		t.Fatalf("waiters = %d, want 0 after cancel", waiters)
	}
}

func TestRetryAfterParsesMinutes(t *testing.T) {
	for text, want := range map[string]int{
		"Your limit will reset in 32 seconds.": 32,
		"Your limit will reset in 1 minute.":   60,
		"Your limit will reset in 8 minutes.":  480,
	} {
		if got := llm.ClassifyText(text).RetryAfterSeconds; got != want {
			t.Errorf("ClassifyText(%q).RetryAfterSeconds = %d, want %d", text, got, want)
		}
	}
	if llm.ClassifyText("no hint here").RetryAfterSeconds != 0 {
		t.Error("expected no hint to parse")
	}
	if !strings.Contains(rateLimitErr("reset in 5 seconds.").Error(), "resource_exhausted") {
		t.Error("test helper should produce resource_exhausted errors")
	}
}

// 分钟 hint 向上取整到 :59 桶界：上游分钟桶的剩余时长被 floor，
// 真实截止 = now+Nmin 所在桶的 :59；秒级 hint 精确落地不改。
func TestRateLimitResetBucketAlignsMinutes(t *testing.T) {
	now := time.Date(2026, 9, 14, 4, 36, 12, 0, time.Local)
	reset, ok := llm.ClassifyText("Your limit will reset in 1 minute.").RateLimitReset(now)
	if !ok {
		t.Fatal("minute hint should parse")
	}
	// 04:37:12 所在分钟桶的 :59 → 04:37:59。
	if want := time.Date(2026, 9, 14, 4, 37, 59, 0, time.Local); !reset.Equal(want) {
		t.Fatalf("RateLimitReset = %v, want %v", reset, want)
	}
	// 目标时刻已过 :59 时进下一分钟桶界：04:36:59.5 + 1min = 04:37:59.5，
	// 本分钟 :59 已过 → 04:38:59。
	reset, ok = llm.ClassifyText("Your limit will reset in 1 minute.").RateLimitReset(
		time.Date(2026, 9, 14, 4, 36, 59, int(500*time.Millisecond), time.Local))
	if !ok {
		t.Fatal("minute hint should parse")
	}
	if want := time.Date(2026, 9, 14, 4, 38, 59, 0, time.Local); !reset.Equal(want) {
		t.Fatalf("RateLimitReset = %v, want %v", reset, want)
	}
	// "0 minutes" 是显式声明而非无 hint：分钟粒度 0 是剩余时长的 floor
	// 取整，按桶界模型对齐到本桶 :59。
	reset, ok = llm.ClassifyText("Your limit will reset in 0 minutes.").RateLimitReset(now)
	if !ok || !reset.Equal(time.Date(2026, 9, 14, 4, 36, 59, 0, time.Local)) {
		t.Fatalf("zero-minute RateLimitReset = %v,%v, want 04:36:59,true", reset, ok)
	}
	reset, ok = llm.ClassifyText("Your limit will reset in 1 minute.").RateLimitReset(time.Date(2026, 9, 14, 4, 36, 1, 0, time.Local))
	if !ok {
		t.Fatal("minute hint should parse")
	}
	if want := time.Date(2026, 9, 14, 4, 37, 59, 0, time.Local); !reset.Equal(want) {
		t.Fatalf("RateLimitReset = %v, want %v", reset, want)
	}
	// 秒级 hint 原样生效。
	reset, ok = llm.ClassifyText("Your limit will reset in 30 seconds.").RateLimitReset(now)
	if !ok || !reset.Equal(now.Add(30*time.Second)) {
		t.Fatalf("seconds RateLimitReset = %v,%v", reset, ok)
	}
}

// 闩时段还原：事件环按写入序重放，上闩开窗、延闩推右端、解闩/到期关窗；
// 闩中时段收到 now，开窗滚出环外的在闩时段给 nil Start。
func TestRateGateLatchRanges(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	clock := pinGateClock(gate, 10)
	// 闩 1：上闩 → 延闩 → 提前解闩，产出一段 [latchAt, releaseAt]，
	// 右端是延闩后的截止对不上的解闩时刻。
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 1 minutes."))
	latchAt := clock.t
	clock.t = clock.t.Add(5 * time.Second)
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 3 minutes."))
	clock.t = clock.t.Add(5 * time.Second)
	releaseAt := clock.t
	gate.noteUpstreamSuccess()
	ranges := gate.stats().LatchRanges
	if len(ranges) != 1 || ranges[0].Start == nil || !ranges[0].Start.Equal(latchAt) || !ranges[0].End.Equal(releaseAt) {
		t.Fatalf("latch→release ranges = %+v, want [%v, %v]", ranges, latchAt, releaseAt)
	}
	// 闩 2：上闩后到期自然失效——expireIfDue 在 stats 轮询里补 expired
	// 事件，关窗端点是闩截止时刻而非 now。
	clock.t = clock.t.Add(time.Minute)
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 2 minutes."))
	latch2At := clock.t
	clock.t = clock.t.Add(3 * time.Minute) // 过闩截止
	stats := gate.stats()
	if len(stats.LatchRanges) != 2 {
		t.Fatalf("ranges = %+v, want 2", stats.LatchRanges)
	}
	// 分钟 hint 向上对齐 :59 桶界：截止 = now+2min 所在分钟桶的 :59。
	wantEnd := latch2At.Truncate(time.Minute).Add(2*time.Minute + 59*time.Second)
	if got := stats.LatchRanges[1]; got.Start == nil || !got.Start.Equal(latch2At) || !got.End.Equal(wantEnd) {
		t.Fatalf("expired range = %+v, want [%v, %v]", got, latch2At, wantEnd)
	}
	// 闩 3：当前仍在闩中，开窗可见 → 末段 End=now、Start 是开窗时刻。
	clock.t = clock.t.Add(time.Minute)
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 5 minutes."))
	latch3At := clock.t
	clock.t = clock.t.Add(30 * time.Second)
	stats = gate.stats()
	last := stats.LatchRanges[len(stats.LatchRanges)-1]
	if last.Start == nil || !last.Start.Equal(latch3At) || !last.End.Equal(clock.t) {
		t.Fatalf("open range = %+v, want [%v, now]", last, latch3At)
	}
}

// 冷却闩持久化与恢复：上闩写 runtime_state 行，新实例（模拟重启）恢复
// 未过期的闩，防止重启后裸发把上游限流续长；解闩删行，过期行被忽略
// 并清除。
func TestRateGateLatchPersistRestore(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 60}, st, "gate:test")
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 8 minutes."))
	if _, ok, err := st.GetState(context.Background(), "gate:test"); err != nil || !ok {
		t.Fatalf("state not persisted: ok=%v err=%v", ok, err)
	}

	restarted := newRateGate(GateConfig{MaxRPM: 60}, st, "gate:test")
	stats := restarted.stats()
	if !stats.Latched || stats.LimitedUntil == nil {
		t.Fatalf("restarted gate should restore latch, stats = %+v", stats)
	}
	if err := restarted.wait(context.Background()); err == nil {
		t.Fatal("restored latch should keep rejecting")
	}

	restarted.noteUpstreamSuccess()
	if _, ok, err := st.GetState(context.Background(), "gate:test"); err != nil || ok {
		t.Fatalf("release should remove state row, ok=%v err=%v", ok, err)
	}
	if restarted.stats().Latched {
		t.Fatal("release should unlatch")
	}
}

func TestRateGateStateExpiredIgnored(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	past := gateState{LimitedUntil: time.Now().Add(-time.Minute)}
	data, err := json.Marshal(past)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetState(context.Background(), "gate:test", string(data)); err != nil {
		t.Fatal(err)
	}
	gate := newRateGate(GateConfig{}, st, "gate:test")
	if gate.stats().Latched {
		t.Fatal("expired state must not latch")
	}
	if _, ok, err := st.GetState(context.Background(), "gate:test"); err != nil || ok {
		t.Fatalf("expired state row should be removed, ok=%v err=%v", ok, err)
	}
}

func TestRateGateSetParamsPreservesLatch(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 60}, nil, "")
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 8 minutes."))
	gate.setParams(GateConfig{MaxRPM: 30})
	stats := gate.stats()
	if !stats.Latched {
		t.Fatal("setParams must preserve latch")
	}
	if stats.WindowQuota != 30 {
		t.Fatalf("WindowQuota = %v, want 30", stats.WindowQuota)
	}
	gate.setParams(GateConfig{MaxRPM: 0})
	if gate.stats().WindowQuota != 0 {
		t.Fatal("quota=0 should disable window limit")
	}
}

// 闩外可发区间且配额未满、无排队者：ping 放行并计入本桶配额。
func TestRateGateTryAdmitPass(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 10)
	if !gate.tryAdmit() {
		t.Fatal("tryAdmit = false, want admit in sendable window with free quota")
	}
	if gate.bucketUsed != 1 {
		t.Fatalf("bucketUsed = %d, want 1 (admitted ping counts into bucket)", gate.bucketUsed)
	}
}

// 闩内一律拒绝：配额再空也不放行——冷却期恰是最不该打上游的时刻，
// ping 不占滴灌探针槽，被拒也不计桶。
func TestRateGateTryAdmitLatchedRejects(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 10)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 30 seconds."))
	if gate.tryAdmit() {
		t.Fatal("tryAdmit = true while latched, want false (no drip-slot stealing)")
	}
	if gate.bucketUsed != 0 {
		t.Fatalf("bucketUsed = %d, want 0 (rejected ping must not count)", gate.bucketUsed)
	}
}

// 死区内不放行：ping 与正式请求一样不得在桶界两侧冒险发送。
func TestRateGateTryAdmitDeadZoneRejects(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 59)
	if gate.tryAdmit() {
		t.Fatal("tryAdmit = true in dead zone, want false")
	}
}

// 本桶配额打满不放行：桶计数与 wait 共享同一本账。
func TestRateGateTryAdmitBucketFullRejects(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want pass (fills bucket)", err)
	}
	if gate.tryAdmit() {
		t.Fatal("tryAdmit = true with full bucket, want false")
	}
}

// quota<=0 不做窗口限速：死区内也放行（与 wait 的零配额口径一致）。
func TestRateGateTryAdmitZeroQuota(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 59) // 死区
	if !gate.tryAdmit() {
		t.Fatal("tryAdmit = false with quota<=0, want true (no window limit)")
	}
}

// nil 闸门放行：与 wait 的 nil 接收者语义一致。
func TestRateGateTryAdmitNilGate(t *testing.T) {
	var gate *rateGate
	if !gate.tryAdmit() {
		t.Fatal("tryAdmit on nil gate = false, want true")
	}
}

// ping 与 bg 共用 quota-reserve 上界同一本账：tryAdmit 放行占用的
// 桶位让随后的 bg 请求更早触顶（reason=quota），fg 不受预留约束
// 照常进预留槽放行。
func TestRateGateTryAdmitConsumesSharedQuota(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 3, BgReserveMargin: 1, BgMaxHold: 2 * time.Second}, nil, "")
	pinGateClock(gate, 50) // 爬坡额度 ceil(2*48/56)=2 = quota-reserve：纯预留约束
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	if !gate.tryAdmit() {
		t.Fatal("tryAdmit = false, want admit")
	}
	// quota-reserve=2：ping 已占 1 槽，bg 再进 1 条即触顶。
	if err := gate.wait(bgCtx); err != nil {
		t.Fatalf("bg wait error = %v, want pass (one slot left)", err)
	}
	var gateErr *llm.Failure
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("bg wait error = %v, want *llm.Failure reason=quota (ping+bg exhausted quota-reserve)", err)
	}
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("fg wait error = %v, want pass (reserve slots are for fg)", err)
	}
}

// bg 准入让出预留槽：fg 吃掉配额后 bucketUsed+1 越过 quota-reserve 的
// bg 快败（reason=quota、Retry-After 报下一窗口、记 rejectBgReserve），
// fg 不受预留约束照常放行。quota 取 8 让 :30 的爬坡额度（4）盖过本例
// 要的 2 个 bg 槽，隔离出纯预留阻塞。
func TestRateGateBgReserveBlocks(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 8, BgReserveMargin: 1, BgMaxHold: 2 * time.Second}, nil, "")
	pinGateClock(gate, 30)
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	// fg 先占 5 槽：预留 1 → bg 总额度 quota-reserve=7，只剩 2 槽。
	for i := 0; i < 5; i++ {
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("fg wait %d error = %v, want pass", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if err := gate.wait(bgCtx); err != nil {
			t.Fatalf("bg wait %d error = %v, want pass (within quota-reserve)", i, err)
		}
	}
	err := gate.wait(bgCtx)
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) {
		t.Fatalf("bg wait error = %v, want *llm.Failure (reserve blocked)", err)
	}
	if gateErr.GateReason != gateReasonQuota {
		t.Fatalf("GateReason = %q, want quota", gateErr.GateReason)
	}
	// :30 预留阻塞 → 下一窗口 :02+60 开放，retryAfter ≈ 32s。
	if gateErr.RetryAfterSeconds < 30 || gateErr.RetryAfterSeconds > 34 {
		t.Fatalf("RetryAfterSeconds = %d, want ~32s (next window)", gateErr.RetryAfterSeconds)
	}
	// fg 仍放行到满桶：预留只对 bg 生效。
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("fg wait error = %v, want pass (reserve only constrains bg)", err)
	}
	stats := gate.stats()
	if stats.RejectBgReserve != 1 {
		t.Fatalf("RejectBgReserve = %d, want 1", stats.RejectBgReserve)
	}
	if stats.WindowUsed != 8 || stats.WindowUsedFg != 6 || stats.WindowUsedBg != 2 {
		t.Fatalf("window used = %d (fg %d, bg %d), want 8 (6, 2)", stats.WindowUsed, stats.WindowUsedFg, stats.WindowUsedBg)
	}
	if stats.Reserve != 1 {
		t.Fatalf("Reserve = %d, want 1 (margin only, no EMA/waiters)", stats.Reserve)
	}
}

// bg 爬坡：放行额度按可发区间经过时间线性释放——窗口前段 bg 只能吃
// 到斜坡放出的几条槽（fg 看到的桶是半空的），额度随经过时间增长，
// 窗口末尾恰好收敛到 quota-reserve，总吞吐不变。
func TestRateGateBgRampPaces(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 8, BgReserveMargin: 1, BgMaxHold: 2 * time.Second}, nil, "")
	clock := pinGateClock(gate, 10)
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	// :10 经过 8s：额度 ceil(7*8/56)=1——第二个 bg 被爬坡挡住快败。
	if err := gate.wait(bgCtx); err != nil {
		t.Fatalf("bg wait error = %v, want pass (first ramp slot)", err)
	}
	var gateErr *llm.Failure
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("bg wait at :10 error = %v, want *llm.Failure reason=quota (ramp blocked)", err)
	}
	// :40 经过 38s：额度 ceil(7*38/56)=5——再补 4 条到 usedBg=5。
	clock.t = clock.t.Add(30 * time.Second)
	for i := 0; i < 4; i++ {
		if err := gate.wait(bgCtx); err != nil {
			t.Fatalf("bg wait %d at :40 error = %v, want pass", i, err)
		}
	}
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("bg wait at :40 error = %v, want *llm.Failure reason=quota (ramp blocked)", err)
	}
	// :57 经过 55s：额度 ceil(7*55/56)=7=quota-reserve——爬坡收敛，
	// bg 吃满预留让出的全部槽。
	clock.t = clock.t.Add(17 * time.Second)
	for i := 0; i < 2; i++ {
		if err := gate.wait(bgCtx); err != nil {
			t.Fatalf("bg wait %d at :57 error = %v, want pass (ramp converged)", i, err)
		}
	}
	stats := gate.stats()
	if stats.WindowUsedBg != 7 || stats.WindowUsed != 7 {
		t.Fatalf("window used = %d (bg %d), want 7 (7)", stats.WindowUsed, stats.WindowUsedBg)
	}
	if stats.PaceAllowance != 7 {
		t.Fatalf("PaceAllowance = %d, want 7 (quota-reserve at window tail)", stats.PaceAllowance)
	}
}

// 预留随可发区间衰减：窗口前段 EMA 外推的预留把 bg 封零（快败），
// 尾段衰减到 margin 附近后 bg 吃到 fg 没用的尾槽——工作保守性。
func TestRateGateBgReserveDecaysToTailFill(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 3, BgReserveMargin: 1, BgMaxHold: 2 * time.Second}, nil, "")
	clock := pinGateClock(gate, 10)
	gate.mu.Lock()
	gate.fgRateEMA = 2 // 条/窗：:10 时可发区间剩 48s → 外推 ceil(2*48/60)=2
	gate.mu.Unlock()
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	// :10 预留 = 2+1=3=quota：bg 被封零快败。
	var gateErr *llm.Failure
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("bg wait at window head error = %v, want *llm.Failure reason=quota", err)
	}
	// :57 可发区间剩 ~1s：外推项 ceil(2*1/60)=1，预留降到 2，
	// bg 拿到 quota-reserve=1 个尾槽。
	clock.t = clock.t.Add(47 * time.Second)
	if err := gate.wait(bgCtx); err != nil {
		t.Fatalf("bg wait at window tail error = %v, want pass (reserve decayed)", err)
	}
	stats := gate.stats()
	if stats.Reserve != 2 {
		t.Fatalf("Reserve = %d, want 2 (extrapolated 1 + margin 1)", stats.Reserve)
	}
	if stats.WindowUsedBg != 1 {
		t.Fatalf("WindowUsedBg = %d, want 1 (tail fill)", stats.WindowUsedBg)
	}
}

// waiters_fg 显式计入预留：上一桶没挤上的 fg 在新窗口是既得需求，
// 空桶也要为它让位——bg 在桶空时仍被拒，fg 照常放行。
func TestRateGateBgReserveCountsFgWaiters(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 3, BgReserveMargin: 1, BgMaxHold: 2 * time.Second}, nil, "")
	pinGateClock(gate, 10)
	gate.mu.Lock()
	gate.waitersFg = 2
	gate.mu.Unlock()
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	// reserve = 0(EMA) + 2(waiters) + 1(margin) = 3 = quota：bg 封零。
	var gateErr *llm.Failure
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("bg wait error = %v, want *llm.Failure reason=quota", err)
	}
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("fg wait error = %v, want pass", err)
	}
}

// fg 准入速率 EMA 在桶翻页时折叠：本窗 fg 放行数按 α=0.2 并入，
// 空窗再衰减一档——bg 预留的需求外推项以它为输入。
func TestRateGateFgRateEMAFoldsAtRoll(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5, BgReserveMargin: 1}, nil, "")
	clock := pinGateClock(gate, 10)
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	for i := 0; i < 2; i++ {
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("fg wait %d error = %v, want pass", i, err)
		}
	}
	if err := gate.wait(bgCtx); err != nil {
		t.Fatalf("bg wait error = %v, want pass", err)
	}
	// 翻页：fgWindow=2 折叠进 EMA（bg 不计入 fg 需求样本）。
	clock.t = clock.t.Add(time.Minute)
	if rate := gate.stats().FgRate; rate < 0.39 || rate > 0.41 {
		t.Fatalf("FgRate = %v, want ~0.4 (2 fg admissions folded)", rate)
	}
	// 空窗再翻页：EMA *= 0.8。
	clock.t = clock.t.Add(time.Minute)
	if rate := gate.stats().FgRate; rate < 0.31 || rate > 0.33 {
		t.Fatalf("FgRate = %v, want ~0.32 (decayed on empty window)", rate)
	}
}

// bg 快败语义：死区/桶满与 fg 同形按窗口节奏拒绝；预留阻塞单独记
// rejectBgReserve 不复用 rejectHold，让「礼让强度」与「排队预算
// 耗尽」可区分。
func TestRateGateRejectionReasons(t *testing.T) {
	// 闩内：reason=latch。
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 10)
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 30 seconds."))
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonLatch {
		t.Fatalf("latched wait error = %v, want *llm.Failure reason=latch", err)
	}
	// fg 死区：reason=hold。
	fresh := newRateGate(GateConfig{MaxRPM: 1, MaxHold: time.Second}, nil, "")
	pinGateClock(fresh, 58.5)
	if err := fresh.wait(context.Background()); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonHold {
		t.Fatalf("dead-zone wait error = %v, want *llm.Failure reason=hold", err)
	}
	// fg 桶满：reason=quota。
	full := newRateGate(GateConfig{MaxRPM: 1, MaxHold: time.Second}, nil, "")
	pinGateClock(full, 10)
	if err := full.wait(context.Background()); err != nil {
		t.Fatalf("fg wait error = %v, want pass", err)
	}
	if err := full.wait(context.Background()); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("bucket-full wait error = %v, want *llm.Failure reason=quota", err)
	}
}

// 放行回执经 GateContext 回填：class/lane/窗口账/排队耗时随
// X-Gate-* 头数据源透出；未挂接 ctx 的请求不产生回执。
func TestRateGateVerdictReceipt(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 6, BgReserveMargin: 1}, nil, "gate:yanjian")
	pinGateClock(gate, 10)
	ctx, gc := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	if err := gate.wait(ctx); err != nil {
		t.Fatalf("bg wait error = %v, want pass", err)
	}
	v := gc.Verdict()
	if v == nil {
		t.Fatal("Verdict = nil, want admission receipt")
	}
	if v.Class != adapter.ClassBG || v.Lane != "yanjian" {
		t.Fatalf("verdict = %+v, want class=bg lane=yanjian", v)
	}
	if v.WindowUsed != 1 || v.WindowQuota != 6 {
		t.Fatalf("verdict window = %d/%d, want 1/6", v.WindowUsed, v.WindowQuota)
	}
	// :10 → 下一窗口 :02+60，reset ≈ 52s。
	if v.WindowResetSec < 50 || v.WindowResetSec > 53 {
		t.Fatalf("WindowResetSec = %d, want ~52", v.WindowResetSec)
	}
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("bare-ctx wait error = %v, want pass", err)
	}
}

// 等待样本环：wait 各结局（放行/拒绝/取消）各记一条实测墙钟等待，
// stats 聚合出分类分位——expectedWait 估计器的实测校准面。
func TestRateGateWaitSamples(t *testing.T) {
	// BgMaxHold 压到 100ms：bg 桶满的 ~60s 预计等待超预算即快败，
	// 不会真睡（fg maxHold=30s 同理——59.9s 预计等待直接拒）。
	gate := newRateGate(GateConfig{MaxRPM: 1, BgMaxHold: 100 * time.Millisecond}, nil, "")
	if gate.stats().Wait != nil {
		t.Fatal("Wait view should be nil before any evaluation")
	}
	offsetGateClock(gate, 1.9) // 死区尾：睡到 :02 开放 ~100ms 真等
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want pass after short sleep", err)
	}
	// quota=1 已被睡醒者占掉：第二个请求桶满快败 → reject 样本。
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("bucket-full wait = %v, want *llm.Failure", err)
	}
	// bg 同样桶满快败 → bg 分类样本。
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) {
		t.Fatalf("bg wait = %v, want *llm.Failure", err)
	}
	// 已取消 ctx → cancel 样本（loop 首检查即返回）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v, want context.Canceled", err)
	}
	view := gate.stats().Wait
	if view == nil || view.Samples != 4 || view.Evals != 4 {
		t.Fatalf("wait view = %+v, want 4 samples/4 evals", view)
	}
	if view.Since == nil {
		t.Fatal("Since = nil, want oldest sample timestamp")
	}
	fg := view.Fg
	if fg.Count != 3 || fg.Rejects != 1 || fg.Cancels != 1 {
		t.Fatalf("fg summary = %+v, want count=3 rejects=1 cancels=1", fg)
	}
	// 睡过 ~100ms 的放行样本撑起峰值与均值；中位数落在两个即时样本
	// 上是 0，属预期形状（快败/取消的墙钟等待本就接近零）。
	if fg.MaxMs < 50 || fg.MeanMs <= 0 || fg.P50Ms != 0 {
		t.Fatalf("fg wait quantiles should reflect the ~100ms sleep: %+v", fg)
	}
	bg := view.Bg
	if bg.Count != 1 || bg.Rejects != 1 {
		t.Fatalf("bg summary = %+v, want count=1 rejects=1", bg)
	}
	if view.All.Count != 4 || view.All.Rejects != 2 || view.All.Cancels != 1 {
		t.Fatalf("all summary = %+v, want count=4 rejects=2 cancels=1", view.All)
	}
	if view.Rejects[gateReasonQuota] != 2 {
		t.Fatalf("reject reasons = %v, want quota:2", view.Rejects)
	}
}

// tryAdmit 计入 bg 账：保温 ping 视同最低优先级背景流量，
// window_used_bg 的观测口径含它。
func TestRateGateTryAdmitCountsBg(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 10)
	if !gate.tryAdmit() {
		t.Fatal("tryAdmit = false, want admit")
	}
	stats := gate.stats()
	if stats.WindowUsed != 1 || stats.WindowUsedBg != 1 || stats.WindowUsedFg != 0 {
		t.Fatalf("window used = %d (fg %d, bg %d), want 1 (0, 1)", stats.WindowUsed, stats.WindowUsedFg, stats.WindowUsedBg)
	}
}

// ping 不得占用 fg 预留槽：预留把 ping 的桶位上界压到 quota-reserve，
// 桶未满但已用数触界时 tryAdmit 拒绝——与 wait 的 bg 准入同一口径；
// 界内照常放行，fg 不受约束进预留槽。
func TestRateGateTryAdmitRespectsFgReserve(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 8, BgReserveMargin: 2}, nil, "")
	pinGateClock(gate, 57) // 窗口尾：爬坡已收敛到 quota-reserve，隔离纯预留约束
	// reserve = 2（仅 margin，无 EMA/waiters）→ ping 上界 quota-reserve = 6。
	for i := 0; i < 6; i++ {
		if !gate.tryAdmit() {
			t.Fatalf("tryAdmit %d = false, want admit (under quota-reserve)", i)
		}
	}
	// bucketUsed=6=quota-reserve < quota=8：桶未满，预留槽不许 ping 占。
	if gate.tryAdmit() {
		t.Fatal("tryAdmit = true at quota-reserve, want false (reserve slots are for fg)")
	}
	if gate.bucketUsed != 6 {
		t.Fatalf("bucketUsed = %d, want 6 (rejected ping must not count)", gate.bucketUsed)
	}
	// fg 照常进预留槽：预留只对 bg/ping 生效。
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("fg wait error = %v, want pass (reserve only constrains bg/ping)", err)
	}
}

// ping 与 bg 共用爬坡额度：窗口前段额度尚未放出时 ping 同样被挡，
// 不给同拍到期的多条目齐射穿坡——爬坡限的是 bg 类计数，ping 计在其中。
func TestRateGateTryAdmitRespectsPaceRamp(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 8, BgReserveMargin: 2}, nil, "")
	clock := pinGateClock(gate, 10) // 经过 8s：额度 ceil(6*8/56)=1
	if !gate.tryAdmit() {
		t.Fatal("tryAdmit = false, want admit (first ramp slot)")
	}
	if gate.tryAdmit() {
		t.Fatal("tryAdmit = true with ramp exhausted, want false")
	}
	// :40 经过 38s：额度 ceil(6*38/56)=5——已用 1，再放 4 条到界。
	clock.t = clock.t.Add(30 * time.Second)
	for i := 0; i < 4; i++ {
		if !gate.tryAdmit() {
			t.Fatalf("tryAdmit %d at :40 = false, want admit (ramp released)", i)
		}
	}
	if gate.tryAdmit() {
		t.Fatal("tryAdmit = true at :40 ramp bound, want false")
	}
}

// fg 可发分支的拥堵代理与 bg 同形封顶：排空竞态里睡醒者逐个重评估、
// waiters 账还没减完时，waiters>0 可与 used→quota⁻ 共存，分母→1 的
// 不封顶队列项把期望等待吹到分钟级——封顶到下窗+一窗（bg 分支口径）。
func TestRateGateExpectedWaitFgCap(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 80}, nil, "")
	clock := pinGateClock(gate, 10) // toNext = 52s，封顶 52+60=112s
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 79
	gate.waitersFg = 20
	gate.mu.Unlock()
	// 不封顶值 20/max(80-79,1)*60s = 1200s → 截到 112s。
	if got := gate.admissionSnapshot(adapter.ClassFG).ExpectedWait; got != 112*time.Second {
		t.Fatalf("fg expectedWait = %v, want 112s (capped at toNext+window)", got)
	}
	// 分母正常时线性项原样：20/80*60s = 15s < 封顶。
	gate.mu.Lock()
	gate.bucketUsed = 0
	gate.mu.Unlock()
	if got := gate.admissionSnapshot(adapter.ClassFG).ExpectedWait; got != 15*time.Second {
		t.Fatalf("fg expectedWait = %v, want 15s (uncapped queue term)", got)
	}
}

// bg 满桶/死区分支补下窗饥饿项：投影下一窗开放的预留（fgRateEMA 满段
// 外推 + waitersFg + margin）≥quota 时 bg 跨窗无槽，期望等待追加一整
// 窗——fg 饱和 lane 上 bg 实测排队 ~120s 才被拒，旧估计 ~toNext 低估
// 约 4 倍；投影未满预留时不追加，fg 视图不受影响。
func TestRateGateExpectedWaitBgNextWindowStarved(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 80}, nil, "")
	clock := pinGateClock(gate, 30) // toNext = 32s
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 80
	gate.fgRateEMA = 80
	gate.waitersFg = 5
	gate.mu.Unlock()
	// 下窗预留 = ceil(80*56/60)+5+4 = 84 → 封顶 80 = quota：bg 饿死。
	if got := gate.admissionSnapshot(adapter.ClassBG).ExpectedWait; got != 92*time.Second {
		t.Fatalf("bg expectedWait = %v, want 92s (toNext + starved window)", got)
	}
	// fg 同态：队列项照算，不吃饥饿项——32 + 5/80*60 = 35.75s。
	if got := gate.admissionSnapshot(adapter.ClassFG).ExpectedWait; got != 35750*time.Millisecond {
		t.Fatalf("fg expectedWait = %v, want 35.75s (no starvation term)", got)
	}
	// 死区同分支：桶未满但 sendable=false 时投影同样生效。
	// :59 toNext=3s → 3 + 60 = 63s。
	clock.t = clock.t.Add(29 * time.Second)
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 0
	gate.mu.Unlock()
	if got := gate.admissionSnapshot(adapter.ClassBG).ExpectedWait; got != 63*time.Second {
		t.Fatalf("bg dead-zone expectedWait = %v, want 63s", got)
	}
	// 投影未满预留不追加：fgRateEMA=10 → 下窗预留 ceil(10*56/60)+5+4=19。
	clock.t = clock.t.Add(-29 * time.Second)
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 80
	gate.fgRateEMA = 10
	gate.mu.Unlock()
	if got := gate.admissionSnapshot(adapter.ClassBG).ExpectedWait; got != 32*time.Second {
		t.Fatalf("bg expectedWait = %v, want 32s (toNext only, next window not saturated)", got)
	}
}

// 让位快败：预计单次排队超 gateEarlyRelease 且让位谓词答「兄弟 lane
// 有余量」时按 reason=yield 立即快败交给 failover——不挂谓词的同形
// 阻塞（bg 桶满 ~52s < bgMaxHold）原语义是睡到下一窗口，正是被吸收
// 换号税的来源；快败把同一结局提前一个排队预算。
func TestRateGateYieldFastFail(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("seed wait error = %v, want pass", err)
	}
	probed := 0
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	ctx := adapter.WithGateYield(bgCtx, func() bool { probed++; return true })
	start := time.Now()
	err := gate.wait(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("yield wait took %v, want immediate fast-fail", d)
	}
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonYield {
		t.Fatalf("yield wait error = %v, want *llm.Failure reason=yield", err)
	}
	if probed == 0 {
		t.Fatal("yield predicate was not consulted")
	}
	// Retry-After 按底层成因同口径：桶满报下一窗口开放剩余 ~52s。
	if gateErr.RetryAfterSeconds < 50 || gateErr.RetryAfterSeconds > 53 {
		t.Fatalf("RetryAfterSeconds = %d, want ~52s (next window)", gateErr.RetryAfterSeconds)
	}
	if got := gate.stats().RejectYield; got != 1 {
		t.Fatalf("stats().RejectYield = %d, want 1", got)
	}
}

// 让位谓词的门控面：缺席与答否都不落 yield——缺席谓词的 bg 同形
// 阻塞走原睡眠语义（取消收 context.Canceled）；答否的谓词被问过
// 但不快败、不计账。预计等待不超 gateEarlyRelease 的短阻塞连谓词
// 都不问（死区 ~3.5s 属闸内正常节奏，换号不会更快）。
func TestRateGateYieldPredicateGating(t *testing.T) {
	fullGate := func() *rateGate {
		gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
		pinGateClock(gate, 10)
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("seed wait error = %v, want pass", err)
		}
		return gate
	}
	bgWait := func(gate *rateGate, ctx context.Context) error {
		bgCtx, _ := adapter.WithGateContext(ctx, adapter.ClassBG)
		cancelCtx, cancel := context.WithTimeout(bgCtx, 100*time.Millisecond)
		defer cancel()
		return gate.wait(cancelCtx)
	}
	// 无谓词：睡到取消。
	if err := bgWait(fullGate(), context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no-predicate wait error = %v, want context.DeadlineExceeded", err)
	}
	// 谓词答否：问过但不快败。
	probed := 0
	gate := fullGate()
	ctx := adapter.WithGateYield(context.Background(), func() bool { probed++; return false })
	if err := bgWait(gate, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("false-predicate wait error = %v, want context.DeadlineExceeded", err)
	}
	if probed == 0 {
		t.Fatal("false predicate was not consulted")
	}
	if got := gate.stats().RejectYield; got != 0 {
		t.Fatalf("stats().RejectYield = %d, want 0 after false answer", got)
	}
	// 短阻塞不问谓词：死区头 :58.5 距下一窗口 ~3.5s < gateEarlyRelease。
	dead := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	pinGateClock(dead, 58.5)
	probed = 0
	ctx = adapter.WithGateYield(context.Background(), func() bool { probed++; return true })
	if err := bgWait(dead, ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("short-wait error = %v, want context.DeadlineExceeded", err)
	}
	if probed != 0 {
		t.Fatalf("predicate consulted %d times on sub-threshold wait, want 0", probed)
	}
}

// reserveBlocked 分支的让位探针：bg 被预留/爬坡挡住时单次睡眠只是
// gateBgRecheck 重查节奏（≤4s），wait>gateEarlyRelease 永假——不补
// 评估的话桶未满的饥饿能每 4s 重查烧满 bgMaxHold，兄弟 lane 空着也
// 看不见。该分支触发改用 expectedWaitLocked 口径：缺口按释放速率
// 折算成期望排队，谓词答是按 yield 快败，答否/期望排队不超阈值都
// 回落原短间隔重查语义。
func TestRateGateYieldReserveBlocked(t *testing.T) {
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	// 预留封死 bg：quota-reserve=3 已被 fg 桶位占穿（used=5 > 3），
	// 估计器把缺口 -2 按 3/56 释放速率折算成 ~37s 期望排队 > τ。
	newReserveBlocked := func() *rateGate {
		gate := newRateGate(GateConfig{MaxRPM: 8, BgReserveMargin: 1}, nil, "")
		pinGateClock(gate, 10)
		gate.mu.Lock()
		gate.fgRateEMA = 4 // :10 可发区间剩 48s → 预留 ceil(4*48/60)+1 = 5
		gate.mu.Unlock()
		for i := 0; i < 5; i++ {
			if err := gate.wait(context.Background()); err != nil {
				t.Fatalf("fg wait %d error = %v, want pass", i, err)
			}
		}
		return gate
	}
	// 谓词答是：探针被问过并按 reason=yield 快败，Retry-After 按
	// 底层成因同口径报下一窗口 ~52s。
	probed := 0
	gate := newReserveBlocked()
	ctx := adapter.WithGateYield(bgCtx, func() bool { probed++; return true })
	err := gate.wait(ctx)
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonYield {
		t.Fatalf("reserve-blocked yield error = %v, want *llm.Failure reason=yield", err)
	}
	if probed == 0 {
		t.Fatal("yield predicate was not consulted on reserveBlocked path")
	}
	if gateErr.RetryAfterSeconds < 50 || gateErr.RetryAfterSeconds > 54 {
		t.Fatalf("RetryAfterSeconds = %d, want ~52s (next window)", gateErr.RetryAfterSeconds)
	}
	if got := gate.stats().RejectYield; got != 1 {
		t.Fatalf("stats().RejectYield = %d, want 1", got)
	}
	// 谓词答否：探针照样被问过，但不快败——回落 gateBgRecheck 重查。
	probed = 0
	gate = newReserveBlocked()
	ctx = adapter.WithGateYield(bgCtx, func() bool { probed++; return false })
	cancelCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if err := gate.wait(cancelCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("false-predicate wait error = %v, want context.DeadlineExceeded", err)
	}
	if probed == 0 {
		t.Fatal("yield predicate was not consulted on reserveBlocked path")
	}
	if got := gate.stats().RejectYield; got != 0 {
		t.Fatalf("stats().RejectYield = %d, want 0 after false answer", got)
	}
	// 期望排队恰在阈值上不触发：爬坡额度退到 1 而 bg 已占 2 槽，
	// 缺口 -1 按 7/56 折算恰好 8s = τ——不超过 gateEarlyRelease
	// 连谓词都不问。
	boundary := newRateGate(GateConfig{MaxRPM: 8, BgReserveMargin: 1}, nil, "")
	pinGateClock(boundary, 10)
	boundary.mu.Lock()
	boundary.bucketStart = boundary.windowStart(boundary.now())
	boundary.bucketUsed = 2
	boundary.bucketUsedBg = 2
	boundary.mu.Unlock()
	probed = 0
	ctx = adapter.WithGateYield(bgCtx, func() bool { probed++; return true })
	boundCtx, boundCancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer boundCancel()
	if err := boundary.wait(boundCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("boundary wait error = %v, want context.DeadlineExceeded", err)
	}
	if probed != 0 {
		t.Fatalf("predicate consulted %d times at τ boundary, want 0", probed)
	}
}

// 让位快败入窗账：关闭窗口的 reject_yield 列如实记出，与 quota/hold
// 分列（yield 拒绝不再混进 quota 账——归因「为什么换号」靠它区分）。
func TestRateGateYieldPersistsInWindow(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 1}, db, store.GateStateKey("default"))
	clock := pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("seed wait error = %v, want pass", err)
	}
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	ctx := adapter.WithGateYield(bgCtx, func() bool { return true })
	if err := gate.wait(ctx); err == nil {
		t.Fatal("bucket-full yield wait should be rejected")
	}
	clock.t = clock.t.Add(time.Minute)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("new-bucket wait error = %v, want pass", err)
	}
	var rows []*store.GateWindow
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if rows, err = db.ListGateWindows(context.Background(), "default", 0, 0); err != nil {
			t.Fatalf("ListGateWindows: %v", err)
		}
		if len(rows) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 closed window", len(rows))
	}
	w := rows[0]
	if w.RejectYield != 1 || w.RejectQuota != 0 || w.RejectHold != 0 {
		t.Fatalf("row rejects = yield:%d quota:%d hold:%d, want 1/0/0", w.RejectYield, w.RejectQuota, w.RejectHold)
	}
}
