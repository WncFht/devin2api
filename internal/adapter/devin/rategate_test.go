package devin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
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
	gate := newRateGate(GateConfig{}, "")
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
	gate := newRateGate(GateConfig{}, "")
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
	gate := newRateGate(GateConfig{DripInterval: 50 * time.Millisecond}, "")
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
	gate := newRateGate(GateConfig{DripInterval: time.Millisecond}, "")
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
	gate := newRateGate(GateConfig{}, "")
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
	gate := newRateGate(GateConfig{}, "")
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

// 分钟窗口配额：本桶放行数打满后，请求睡到下一窗口；预计等待超过
// maxHold 时本地拒绝，而不是放行去上游续债。
func TestRateGateWindowQuotaReject(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 2}, "")
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
	gate := newRateGate(GateConfig{MaxRPM: 1}, "")
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

// 死区内请求睡到下一窗口开放再放行，而不是立即快败——
// 等待在 maxHold 内就值得睡。
func TestRateGateDeadZoneSleepsToNextWindow(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, "")
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
	gate := newRateGate(GateConfig{MaxRPM: 1, MaxHold: time.Second}, "")
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
	gate := newRateGate(GateConfig{}, "")
	pinGateClock(gate, 59) // 死区
	for i := 0; i < 3; i++ {
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("wait %d error = %v, want pass (no window limit)", i, err)
		}
	}
}

// 等待中 ctx 取消：返回取消原因，waiters 名额归还。
func TestRateGateWaitCancelRefunds(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 1}, "")
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
	if _, ok = llm.ClassifyText("Your limit will reset in 0 minutes.").RateLimitReset(now); ok {
		t.Fatal("zero-minute hint should not parse")
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
	gate := newRateGate(GateConfig{}, "")
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

// 冷却闩落盘与恢复：上闩写状态文件，新实例（模拟重启）恢复未过期的闩，
// 防止重启后裸发把上游限流续长；解闩清文件，过期文件被忽略并清除。
func TestRateGateLatchPersistRestore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	gate := newRateGate(GateConfig{MaxRPM: 60}, path)
	gate.noteUpstreamError(rateLimitErr("rate limited. Your limit will reset in 8 minutes."))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not written: %v", err)
	}

	restarted := newRateGate(GateConfig{MaxRPM: 60}, path)
	stats := restarted.stats()
	if !stats.Latched || stats.LimitedUntil == nil {
		t.Fatalf("restarted gate should restore latch, stats = %+v", stats)
	}
	if err := restarted.wait(context.Background()); err == nil {
		t.Fatal("restored latch should keep rejecting")
	}

	restarted.noteUpstreamSuccess()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("release should remove state file, err = %v", err)
	}
	if restarted.stats().Latched {
		t.Fatal("release should unlatch")
	}
}

func TestRateGateStateExpiredIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gate-state.json")
	past := gateStateFile{LimitedUntil: time.Now().Add(-time.Minute)}
	data, err := json.Marshal(past)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	gate := newRateGate(GateConfig{}, path)
	if gate.stats().Latched {
		t.Fatal("expired state must not latch")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("expired state file should be removed")
	}
}

func TestRateGateSetParamsPreservesLatch(t *testing.T) {
	gate := newRateGate(GateConfig{MaxRPM: 60}, "")
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
