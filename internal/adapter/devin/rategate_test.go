package devin

import (
	"context"
	"encoding/json"
	"errors"
	"math"
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

// throttle_ema 遥测：上游限流结局记 1、其余到达上游的结局记 0、
// 本地闸门拒绝（LocalGate，未触达上游）不入样；读数与样本数经
// GateStats 透出，不参与任何放行判定。
func TestRateGateThrottleEMA(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	approx := func(got, want float64) bool { return math.Abs(got-want) < 1e-9 }

	// 连续两个上游限流结局：EMA 递推 0→0.1→0.19。
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 0 seconds."))
	if ema := gate.stats().ThrottleEMA; !approx(ema, 0.1) {
		t.Fatalf("ThrottleEMA after first 429 = %v, want 0.1", ema)
	}
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 0 seconds."))
	if ema := gate.stats().ThrottleEMA; !approx(ema, 0.19) {
		t.Fatalf("ThrottleEMA after second 429 = %v, want 0.19", ema)
	}
	if n := gate.stats().ThrottleSamples; n != 2 {
		t.Fatalf("ThrottleSamples = %d, want 2", n)
	}

	// 非限流上游失败记 0：EMA ×0.9 衰减；传输伪装（ENHANCE_YOUR_CALM
	// 映成的 resource_exhausted）同样记 0 而非 1——与上闩判定同源。
	gate.noteUpstreamError(connect.NewError(connect.CodeInvalidArgument, errors.New("bad request")))
	gate.noteUpstreamError(connect.NewError(connect.CodeResourceExhausted,
		errors.New("bandwidth exhausted: stream error: stream ID 5; ENHANCE_YOUR_CALM; received from peer")))
	if ema := gate.stats().ThrottleEMA; !approx(ema, 0.19*0.81) {
		t.Fatalf("ThrottleEMA after two non-429 failures = %v, want %v", ema, 0.19*0.81)
	}

	// 上游确认帧记 0。
	gate.noteUpstreamSuccess()
	if ema := gate.stats().ThrottleEMA; !approx(ema, 0.19*0.81*0.9) {
		t.Fatalf("ThrottleEMA after upstream success = %v, want %v", ema, 0.19*0.81*0.9)
	}
	if n := gate.stats().ThrottleSamples; n != 5 {
		t.Fatalf("ThrottleSamples = %d, want 5", n)
	}

	// 本地闸门快败不入样：EMA 与样本数原样。
	before := gate.stats()
	gate.noteUpstreamError(gateRejection(time.Second, gateReasonLatch))
	after := gate.stats()
	if after.ThrottleEMA != before.ThrottleEMA || after.ThrottleSamples != before.ThrottleSamples {
		t.Fatalf("local-gate rejection must not sample: before=%v/%d after=%v/%d",
			before.ThrottleEMA, before.ThrottleSamples, after.ThrottleEMA, after.ThrottleSamples)
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
// 连翻 5 页（prod 实测连败峰值）全部留账零丢弃——旧深度 2 从第 3 行
// 起就会挤掉最老行。换入活库等价于写路径恢复，下一次翻页把 5 笔挂账
// 与刚关闭的窗口行一并落库（(lane,window_start) 唯一索引 + OR IGNORE
// 使重放幂等）。
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
	for i := 0; i < 5; i++ {
		clock.t = clock.t.Add(time.Minute)
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("flip %d wait error = %v", i, err)
		}
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if gate.stats().PersistFailures == 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gate.mu.Lock()
	failures, pending, dropped := gate.persistFailures, len(gate.pendingWindows), gate.persistDropped
	gate.mu.Unlock()
	if failures != 5 || pending != 5 || dropped != 0 {
		t.Fatalf("5-streak retention = failures:%d pending:%d dropped:%d, want 5/5/0", failures, pending, dropped)
	}

	// 写路径恢复等价于换入活库：gate.states 只被持久化协程读，本测试在
	// 下一次翻页前换入——go 语句的创建顺序保证新协程读到 db2。
	db2, err := store.Open(filepath.Join(t.TempDir(), "gate2.db"))
	if err != nil {
		t.Fatalf("store.Open db2: %v", err)
	}
	defer func() { _ = db2.Close() }()
	gate.mu.Lock()
	gate.states = db2
	gate.mu.Unlock()
	clock.t = clock.t.Add(time.Minute)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("recovery wait error = %v", err)
	}
	var rows []*store.GateWindow
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if rows, err = db2.ListGateWindows(context.Background(), "default", 0, 0); err != nil {
			t.Fatalf("ListGateWindows: %v", err)
		}
		if len(rows) == 6 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("replayed rows = %d, want 6 (5 stashed + newly closed)", len(rows))
}

// 缓冲溢出丢最老行并计 persist_dropped：死库连翻 cap+1 页，守恒式
// 推入(cap+1) = 在缓(cap) + 丢弃(1)；persist_failures 与失败批数
// 一一对应（每次翻页恰好一批协程写失败）。
func TestRateGateWindowPersistDrop(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	clock := pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v", err)
	}
	_ = db.Close()
	for i := 0; i < gatePersistRetryCap+1; i++ {
		clock.t = clock.t.Add(time.Minute)
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("flip %d wait error = %v", i, err)
		}
	}
	want := gatePersistRetryCap + 1
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if gate.stats().PersistFailures == want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	gate.mu.Lock()
	failures, pending, dropped := gate.persistFailures, len(gate.pendingWindows), gate.persistDropped
	gate.mu.Unlock()
	if failures != want || pending != gatePersistRetryCap || dropped != 1 {
		t.Fatalf("overflow = failures:%d pending:%d dropped:%d, want %d/%d/1",
			failures, pending, dropped, want, gatePersistRetryCap)
	}
}

// 排空冲刷把重放缓冲里的挂账行同步落库：进程退出不再丢等待下一窗口
// 重放的行；冲刷后缓冲清空。
func TestFlushPendingWindows(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	base := time.Now().Truncate(time.Minute).Unix()
	gate.mu.Lock()
	gate.pendingWindows = []*store.GateWindow{
		{Lane: "default", WindowStart: base - 120, Quota: 10, UsedFg: 3},
		{Lane: "default", WindowStart: base - 60, Quota: 10, UsedFg: 7},
	}
	gate.mu.Unlock()
	gate.FlushPendingWindows(context.Background())
	rows, err := db.ListGateWindows(context.Background(), "default", 0, 0)
	if err != nil {
		t.Fatalf("ListGateWindows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 flushed windows", len(rows))
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if len(gate.pendingWindows) != 0 {
		t.Fatalf("pendingWindows = %d, want empty after flush", len(gate.pendingWindows))
	}
}

// 冲刷先等在途持久化协程落定再取缓冲：在途写失败挂回的行必须赶上本轮
// 冲刷，而不是落定前被跳过、随进程退出丢失。
func TestFlushPendingWindowsWaitsInflight(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	base := time.Now().Truncate(time.Minute).Unix()
	// 模拟一笔在途持久化协程：计数 1 + 未关闭的落定信号。
	gate.mu.Lock()
	gate.persistInFlight = 1
	gate.persistDone = make(chan struct{})
	gate.pendingWindows = []*store.GateWindow{{Lane: "default", WindowStart: base - 60, Quota: 10}}
	gate.mu.Unlock()
	flushed := make(chan struct{})
	go func() {
		gate.FlushPendingWindows(context.Background())
		close(flushed)
	}()
	// 协程未落定前冲刷必须阻塞——抢先取缓冲会把挂回行丢给进程退出。
	select {
	case <-flushed:
		t.Fatal("flush returned before in-flight persist settled")
	case <-time.After(50 * time.Millisecond):
	}
	// 协程收尾：失败行挂回缓冲后计数归零、关落定信号（与 persistWindow
	// 协程尾声同序——挂回先于落定，冲刷才看得见）。
	gate.mu.Lock()
	gate.pendingWindows = append(gate.pendingWindows, &store.GateWindow{Lane: "default", WindowStart: base - 120, Quota: 10})
	gate.persistInFlight = 0
	close(gate.persistDone)
	gate.persistDone = nil
	gate.mu.Unlock()
	select {
	case <-flushed:
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not return after in-flight settled")
	}
	rows, err := db.ListGateWindows(context.Background(), "default", 0, 0)
	if err != nil {
		t.Fatalf("ListGateWindows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (seeded + re-stashed)", len(rows))
	}
}

// stats 透出重放缓冲当前深度：persist_failures/persist_dropped 是累计账，
// pending_windows 回答「此刻还欠几行」——缓冲挂账与冲刷清空两侧都要
// 反映在快照里。
func TestRateGateStatsPendingWindows(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	base := time.Now().Truncate(time.Minute).Unix()
	gate.mu.Lock()
	gate.pendingWindows = []*store.GateWindow{
		{Lane: "default", WindowStart: base - 120, Quota: 10},
		{Lane: "default", WindowStart: base - 60, Quota: 10},
	}
	gate.mu.Unlock()
	if got := gate.stats().PendingWindows; got != 2 {
		t.Fatalf("PendingWindows = %d, want 2", got)
	}
	gate.FlushPendingWindows(context.Background())
	if got := gate.stats().PendingWindows; got != 0 {
		t.Fatalf("PendingWindows after flush = %d, want 0", got)
	}
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

// tryAdmitOK 是 tryAdmit 的 bool 投影：闸门用例只断言放行与否；
// 拒绝成因（reason）由保温侧 TestWarmPingEventRing 经事件环覆盖。
func tryAdmitOK(gate *rateGate) bool {
	admitted, _ := gate.tryAdmit()
	return admitted
}

// 闩外可发区间且配额未满、无排队者：ping 放行并计入本桶配额。
func TestRateGateTryAdmitPass(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 10)
	if !tryAdmitOK(gate) {
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
	if admitted, reason := gate.tryAdmit(); admitted || reason != gateReasonLatch {
		t.Fatalf("tryAdmit = (%v, %q) while latched, want (false, latch)", admitted, reason)
	}
	if gate.bucketUsed != 0 {
		t.Fatalf("bucketUsed = %d, want 0 (rejected ping must not count)", gate.bucketUsed)
	}
}

// 死区内不放行：ping 与正式请求一样不得在桶界两侧冒险发送。
func TestRateGateTryAdmitDeadZoneRejects(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 59)
	if admitted, reason := gate.tryAdmit(); admitted || reason != tryAdmitSkipDeadzone {
		t.Fatalf("tryAdmit = (%v, %q) in dead zone, want (false, deadzone)", admitted, reason)
	}
}

// 本桶配额打满不放行：桶计数与 wait 共享同一本账。
func TestRateGateTryAdmitBucketFullRejects(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	pinGateClock(gate, 10)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want pass (fills bucket)", err)
	}
	if admitted, reason := gate.tryAdmit(); admitted || reason != gateReasonQuota {
		t.Fatalf("tryAdmit = (%v, %q) with full bucket, want (false, quota)", admitted, reason)
	}
}

// quota<=0 不做窗口限速：死区内也放行（与 wait 的零配额口径一致）。
func TestRateGateTryAdmitZeroQuota(t *testing.T) {
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 59) // 死区
	if !tryAdmitOK(gate) {
		t.Fatal("tryAdmit = false with quota<=0, want true (no window limit)")
	}
}

// nil 闸门放行：与 wait 的 nil 接收者语义一致。
func TestRateGateTryAdmitNilGate(t *testing.T) {
	var gate *rateGate
	if !tryAdmitOK(gate) {
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
	if !tryAdmitOK(gate) {
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

// bg 快败语义：死区/桶满与 fg 同形按窗口节奏拒绝（统一报 quota）；
// 预留阻塞单独记 rejectBgReserve 不复用 rejectBudget，让「礼让强度」
// 与「排队预算耗尽」可区分。
func TestRateGateRejectionReasons(t *testing.T) {
	// 闩内：reason=latch。
	gate := newRateGate(GateConfig{}, nil, "")
	pinGateClock(gate, 10)
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 30 seconds."))
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonLatch {
		t.Fatalf("latched wait error = %v, want *llm.Failure reason=latch", err)
	}
	// fg 死区超预算：reason=quota（死区等待与桶满同归预算类拒绝）。
	fresh := newRateGate(GateConfig{MaxRPM: 1, MaxHold: time.Second}, nil, "")
	pinGateClock(fresh, 58.5)
	if err := fresh.wait(context.Background()); !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonQuota {
		t.Fatalf("dead-zone wait error = %v, want *llm.Failure reason=quota", err)
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
	// err 校准集只收带估计的放行样本：fg 只有死区睡醒那一条
	// （est≈toNext~100ms、realized≈100ms → err≈0）；桶满拒绝的
	// est≈58s 不进校准集，首拍前取消的无估计。
	if fg.ErrCount != 1 || fg.ErrP50Ms < -2000 || fg.ErrP50Ms > 500 {
		t.Fatalf("fg err summary = count %d p50 %dms, want count=1 err≈0", fg.ErrCount, fg.ErrP50Ms)
	}
	var admits, rejects, cancels int
	for i := 0; i < gate.waitSize; i++ {
		s := gate.waits[(gate.waitHead-1-i+gateWaitCap)%gateWaitCap]
		switch s.outcome {
		case gateWaitAdmit:
			admits++
			if s.est < 50*time.Millisecond || s.est > 150*time.Millisecond {
				t.Fatalf("admit sample est = %v, want ~100ms deadzone estimate", s.est)
			}
		case gateWaitReject:
			rejects++
			if s.est < 50*time.Second {
				t.Fatalf("reject sample est = %v, want ~58s bucket-full estimate", s.est)
			}
		case gateWaitCancel:
			cancels++
			if s.est >= 0 {
				t.Fatalf("cancel sample est = %v, want unset (<0)", s.est)
			}
		}
	}
	if admits != 1 || rejects != 2 || cancels != 1 {
		t.Fatalf("ring outcomes = %d admit/%d reject/%d cancel, want 1/2/1", admits, rejects, cancels)
	}
	bg := view.Bg
	if bg.Count != 1 || bg.Rejects != 1 {
		t.Fatalf("bg summary = %+v, want count=1 rejects=1", bg)
	}
	if view.All.Count != 4 || view.All.Rejects != 2 || view.All.Cancels != 1 {
		t.Fatalf("all summary = %+v, want count=4 rejects=2 cancels=1", view.All)
	}
	if view.All.ErrCount != 1 || view.Bg.ErrCount != 0 {
		t.Fatalf("err counts = all:%d bg:%d, want 1/0 (admits only)", view.All.ErrCount, view.Bg.ErrCount)
	}
	if view.Rejects[gateReasonQuota] != 2 {
		t.Fatalf("reject reasons = %v, want quota:2", view.Rejects)
	}
}

// err 校准聚合：err = est−realized 只在带估计的放行样本上计；
// 拒绝/取消结局的实测等待被截断，与估计器预测的「到放行时长」
// 不同口径，不进校准集。
func TestSummarizeWaitsErrQuantiles(t *testing.T) {
	samples := []gateWaitSample{
		{wait: 60 * time.Second, est: 50 * time.Second, outcome: gateWaitAdmit}, // err −10s（低估侧）
		{wait: 10 * time.Second, est: 20 * time.Second, outcome: gateWaitAdmit}, // err +10s
		{wait: 12 * time.Second, est: 32 * time.Second, outcome: gateWaitAdmit}, // err +20s
		{wait: 11 * time.Second, est: 71 * time.Second, outcome: gateWaitAdmit}, // err +60s
		{est: 90 * time.Second, outcome: gateWaitReject, reason: gateReasonQuota},
		{est: -1, outcome: gateWaitCancel},
	}
	s := summarizeWaits(samples)
	// errs = {−10,+10,+20,+60}s → mean +20s；nearest-rank 下 p10=idx0、
	// p50=idx1、p90=idx2。
	if s.ErrCount != 4 || s.ErrMeanMs != 20000 ||
		s.ErrP10Ms != -10000 || s.ErrP50Ms != 10000 || s.ErrP90Ms != 20000 {
		t.Fatalf("err summary = %+v, want count=4 mean=20s p10=-10s p50=+10s p90=+20s", s)
	}
	if s.Count != 6 || s.Rejects != 1 || s.Cancels != 1 {
		t.Fatalf("summary = %+v, want count=6 rejects=1 cancels=1", s)
	}
}

// tryAdmit 计入 bg 账：保温 ping 视同最低优先级背景流量，
// window_used_bg 的观测口径含它。
func TestRateGateTryAdmitCountsBg(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 5}, nil, "")
	pinGateClock(gate, 10)
	if !tryAdmitOK(gate) {
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
		if !tryAdmitOK(gate) {
			t.Fatalf("tryAdmit %d = false, want admit (under quota-reserve)", i)
		}
	}
	// bucketUsed=6=quota-reserve < quota=8：桶未满，预留槽不许 ping 占。
	if tryAdmitOK(gate) {
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
	if !tryAdmitOK(gate) {
		t.Fatal("tryAdmit = false, want admit (first ramp slot)")
	}
	if admitted, reason := gate.tryAdmit(); admitted || reason != tryAdmitSkipPace {
		t.Fatalf("tryAdmit = (%v, %q) with ramp exhausted, want (false, pace)", admitted, reason)
	}
	// :40 经过 38s：额度 ceil(6*38/56)=5——已用 1，再放 4 条到界。
	clock.t = clock.t.Add(30 * time.Second)
	for i := 0; i < 4; i++ {
		if !tryAdmitOK(gate) {
			t.Fatalf("tryAdmit %d at :40 = false, want admit (ramp released)", i)
		}
	}
	if tryAdmitOK(gate) {
		t.Fatal("tryAdmit = true at :40 ramp bound, want false")
	}
}

// fg 串行折算只挂超出可并行放行余量的前队：开窗有槽即并行放行，
// waiters≤room 的队开窗瞬间齐进（实测 ~0.2s，旧公式高估 ~waiters/
// quota×60s）。可发分支：余量内期望 0；超出余量的前队等到翻窗按
// 整窗配额折算，封顶到下窗+一窗。
func TestRateGateExpectedWaitFgExcess(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 80}, nil, "")
	clock := pinGateClock(gate, 10) // toNext = 52s，封顶 52+60=112s
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.waitersFg = 20
	gate.mu.Unlock()
	// waiters=20 < room=80：全队即刻并行放行，期望 ~0（旧 15s 幻影
	// 串行项——waiters=30/room=76 实测放行 0.2s 的同型）。
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 0 {
		t.Fatalf("fg expectedWait = %v, want 0 (queue fits in remaining room)", got)
	}
	// waiters=100 > room=80：20 条超额睡到翻窗折算 → 52+20/80*60=67s。
	gate.mu.Lock()
	gate.waitersFg = 100
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 67*time.Second {
		t.Fatalf("fg expectedWait = %v, want 67s (toNext + excess/quota*window)", got)
	}
	// 余量收窄放大超额：used=79 → room=1、excess=99 → 52+74.25s，
	// 仍吃封顶 112s（超额超一整窗的部分被截——深队分辨不需要）。
	gate.mu.Lock()
	gate.bucketUsed = 79
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 112*time.Second {
		t.Fatalf("fg expectedWait = %v, want 112s (capped at toNext+window)", got)
	}
	// 桶满分支同口径：翻窗配额整窗重置，room=quota——waiters=100
	// 超额 20 → 52+15=67s。
	gate.mu.Lock()
	gate.bucketUsed = 80
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 67*time.Second {
		t.Fatalf("fg expectedWait = %v, want 67s (bucket-full toNext + excess)", got)
	}
	// 桶满且前队≤整窗配额：开窗瞬间全进，期望只剩 toNext。
	gate.mu.Lock()
	gate.waitersFg = 30
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 52*time.Second {
		t.Fatalf("fg expectedWait = %v, want 52s (toNext only, queue fits next window)", got)
	}
	// 死区同分支：sendable=false 桶未满，waiters≤quota → 只剩 toNext。
	clock.t = clock.t.Add(49 * time.Second) // :59 死区，toNext=3s
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 0
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 3*time.Second {
		t.Fatalf("fg dead-zone expectedWait = %v, want 3s (toNext only)", got)
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
	if got := gate.admissionVerdict(adapter.ClassBG).ExpectedWait; got != 92*time.Second {
		t.Fatalf("bg expectedWait = %v, want 92s (toNext + starved window)", got)
	}
	// fg 同态：前队 5 < 整窗配额 80，开窗瞬间全进——期望只剩 toNext，
	// 不吃饥饿项。
	if got := gate.admissionVerdict(adapter.ClassFG).ExpectedWait; got != 32*time.Second {
		t.Fatalf("fg expectedWait = %v, want 32s (queue fits next window, no starvation term)", got)
	}
	// 死区同分支：桶未满但 sendable=false 时投影同样生效。
	// :59 toNext=3s → 3 + 60 = 63s。
	clock.t = clock.t.Add(29 * time.Second)
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 0
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassBG).ExpectedWait; got != 63*time.Second {
		t.Fatalf("bg dead-zone expectedWait = %v, want 63s", got)
	}
	// 投影未满预留不追加：fgRateEMA=10 → 下窗预留 ceil(10*56/60)+5+4=19。
	clock.t = clock.t.Add(-29 * time.Second)
	gate.mu.Lock()
	gate.bucketStart = gate.windowStart(clock.t)
	gate.bucketUsed = 80
	gate.fgRateEMA = 10
	gate.mu.Unlock()
	if got := gate.admissionVerdict(adapter.ClassBG).ExpectedWait; got != 32*time.Second {
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
	ctx := adapter.WithGateYield(bgCtx, func() (time.Duration, bool) { probed++; return 2500 * time.Millisecond, true })
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
	// 让位探针量随拒绝出账：本侧探针是桶满睡眠折算（~52s>阈值），
	// 兄弟侧带回谓词报告的期望排队。
	if gateErr.GateProbeMS < 50000 || gateErr.GateProbeMS > 54000 {
		t.Fatalf("GateProbeMS = %d, want ~52s probe wait", gateErr.GateProbeMS)
	}
	if gateErr.GateSiblingEwMS != 2500 {
		t.Fatalf("GateSiblingEwMS = %d, want 2500 (predicate report)", gateErr.GateSiblingEwMS)
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
	ctx := adapter.WithGateYield(context.Background(), func() (time.Duration, bool) { probed++; return 0, false })
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
	ctx = adapter.WithGateYield(context.Background(), func() (time.Duration, bool) { probed++; return 0, true })
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
	ctx := adapter.WithGateYield(bgCtx, func() (time.Duration, bool) { probed++; return 0, true })
	err := gate.wait(ctx)
	var gateErr *llm.Failure
	if !errors.As(err, &gateErr) || gateErr.GateReason != gateReasonYield {
		t.Fatalf("reserve-blocked yield error = %v, want *llm.Failure reason=yield", err)
	}
	if probed == 0 {
		t.Fatal("yield predicate was not consulted on reserveBlocked path")
	}
	// reserveBlocked 的探针是 expectedWait 口径（~37s）而非 ≤4s 的重查
	// 睡眠——探针量落账须带同一口径才审得出让位对错。
	if gateErr.GateProbeMS <= int64(gateEarlyRelease/time.Millisecond) {
		t.Fatalf("GateProbeMS = %d, want expectedWait-scale probe > %d", gateErr.GateProbeMS, gateEarlyRelease/time.Millisecond)
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
	ctx = adapter.WithGateYield(bgCtx, func() (time.Duration, bool) { probed++; return 0, false })
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
	ctx = adapter.WithGateYield(bgCtx, func() (time.Duration, bool) { probed++; return 0, true })
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
	ctx := adapter.WithGateYield(bgCtx, func() (time.Duration, bool) { return 0, true })
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
	if w.RejectYield != 1 || w.RejectQuota != 0 {
		t.Fatalf("row rejects = yield:%d quota:%d, want 1/0", w.RejectYield, w.RejectQuota)
	}
}

// 保温 ping 的放行单列进 used_bg_ping 窗口账：tryAdmit 是 ping 的唯一
// 入口，其放行既是 used_bg 的子集也是 ping 需求账——used_bg 减去
// used_bg_ping 即真实 bg 需求；实时读数 window_used_bg_ping 同口径。
func TestRateGateWindowUsedBgPing(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "gate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	gate := newRateGate(GateConfig{MaxRPM: 10}, db, store.GateStateKey("default"))
	clock := pinGateClock(gate, 57) // 窗口尾：爬坡已收敛到 quota-reserve，纯预留约束下 ping/bg 都有槽
	for i := 0; i < 2; i++ {
		if ok, _ := gate.tryAdmit(); !ok {
			t.Fatalf("tryAdmit %d = false, want admit", i)
		}
	}
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	if err := gate.wait(bgCtx); err != nil {
		t.Fatalf("bg wait error = %v, want pass", err)
	}
	if stats := gate.stats(); stats.WindowUsedBg != 3 || stats.WindowUsedBgPing != 2 {
		t.Fatalf("live split = used_bg:%d ping:%d, want 3/2", stats.WindowUsedBg, stats.WindowUsedBgPing)
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
	if w := rows[0]; w.UsedBg != 3 || w.UsedBgPing != 2 {
		t.Fatalf("row = used_bg:%d ping:%d, want 3/2 (real bg demand = 1)", w.UsedBg, w.UsedBgPing)
	}
}

// 分类累计账（wait.totals）：进程期 evals/waits/wait_total_ms 单调
// 累计——waits 只记真排过队的评估（占过 waiters 名额），睡中被取消
// 同样算排过；即时放行与快败不占名额不计。快照差分即任意区间的
// 分类等待率与平均等待，不受样本环覆盖期限制。
func TestRateGateWaitTotals(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1, BgMaxHold: 100 * time.Millisecond}, nil, "")
	offsetGateClock(gate, 1.9) // 死区尾：睡到 :02 开放 ~100ms
	// fg 死区短睡后放行：占过 waiters 名额 → waits 记账。
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("dead-zone wait error = %v, want pass after short sleep", err)
	}
	// fg 桶满快败（预计 ~58s > maxHold 30s）：不排队 → waits 不记。
	var gateErr *llm.Failure
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("bucket-full wait = %v, want *llm.Failure", err)
	}
	// bg 桶满快败（预计 ~58s > bgMaxHold 100ms）：同口径不记 waits。
	bgCtx, _ := adapter.WithGateContext(context.Background(), adapter.ClassBG)
	if err := gate.wait(bgCtx); !errors.As(err, &gateErr) {
		t.Fatalf("bg wait = %v, want *llm.Failure", err)
	}
	totals := gate.stats().Wait.Totals
	if totals.Fg.Evals != 2 || totals.Fg.Waits != 1 {
		t.Fatalf("fg totals = %+v, want evals=2 waits=1", totals.Fg)
	}
	if totals.Fg.WaitTotalMs < 40 || totals.Fg.WaitTotalMs > 2000 {
		t.Fatalf("fg wait_total_ms = %d, want ~100ms (one real sleep)", totals.Fg.WaitTotalMs)
	}
	if totals.Bg.Evals != 1 || totals.Bg.Waits != 0 {
		t.Fatalf("bg totals = %+v, want evals=1 waits=0 (fast reject never queued)", totals.Bg)
	}
	// 睡到一半被取消同样算「排过」：bg 在默认预算下死区可睡，
	// 50ms 取消 → waits 记账、结局 cancel。
	fresh := newRateGate(GateConfig{MaxRPM: 1}, nil, "")
	offsetGateClock(fresh, 58.5) // 死区头：睡到下一窗口 ~3.5s < bgMaxHold
	ctx, cancel := context.WithCancel(bgCtx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := fresh.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v, want context.Canceled", err)
	}
	if got := fresh.stats().Wait.Totals.Bg; got.Evals != 1 || got.Waits != 1 {
		t.Fatalf("fresh bg totals = %+v, want evals=1 waits=1 (cancel-during-sleep counted)", got)
	}
}
