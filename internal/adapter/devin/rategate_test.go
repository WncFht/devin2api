package devin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/api/common"
)

func rateLimitErr(text string) error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New(text))
}

// 限流闩未到声明时刻：wait 本地拒绝且 retryAfter 等于闩剩余时长
// （分钟 hint 已向上对齐到 :59 桶界，实际可达 N*60+59s）。
func TestRateGateLatchRejectsUntilReset(t *testing.T) {
	gate := newRateGate(0, 0, 0, 0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Please try again later. Your limit will reset in 8 minutes. (trace ID: x)"))
	err := gate.wait(context.Background())
	var gateErr *rateGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("wait error = %v, want *rateGateError", err)
	}
	if gateErr.retryAfter < 480*time.Second || gateErr.retryAfter > 545*time.Second {
		t.Fatalf("retryAfter = %v, want 480~545s (bucket-aligned)", gateErr.retryAfter)
	}
	// 错误文案必须能被公共错误管道译出 429 + Retry-After。
	if status := common.HTTPStatus(err.Error()); status != 429 {
		t.Fatalf("HTTPStatus = %d, want 429", status)
	}
	if seconds, ok := common.RetryAfterSeconds(err.Error()); !ok || seconds < 480 || seconds > 545 {
		t.Fatalf("RetryAfterSeconds = %d,%v, want 480~545", seconds, ok)
	}
}

// 闩内不排队：无论闩剩余长短都立即快败，Retry-After 报闩剩余，
// 由客户端睡到恢复时刻再来，而不是占着并发槽空等。
func TestRateGateLatchFastFails(t *testing.T) {
	gate := newRateGate(0, 0, 0, 0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 1 seconds."))
	start := time.Now()
	err := gate.wait(context.Background())
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("wait held %v during latch, want instant fast-fail", elapsed)
	}
	var gateErr *rateGateError
	if !errors.As(err, &gateErr) || gateErr.retryAfter <= 0 || gateErr.retryAfter > time.Second {
		t.Fatalf("wait error = %v, want rateGateError with retryAfter ~1s", err)
	}
}

// 闩内按滴灌间隔放行探针：槽空闲 → 放行；槽被占 → 快败。
// 探针是限流期间唯一到达上游的请求，负责探出解闩又不给上游续债。
func TestRateGateDripReleasesProbes(t *testing.T) {
	gate := newRateGate(0, 0, 50*time.Millisecond, 0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 30 seconds."))
	// 第一个槽在上闩后 dripInterval 才开放，先到请求快败。
	var gateErr *rateGateError
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("first wait error = %v, want *rateGateError (slot not open yet)", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("drip-slot wait error = %v, want probe release", err)
	}
	// 槽已被取走，紧随其后的请求回到快败。
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("post-probe wait error = %v, want *rateGateError", err)
	}
	time.Sleep(60 * time.Millisecond)
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("next drip-slot wait error = %v, want probe release", err)
	}
}

// 任一上游成功帧立即解闩：边际态下拒绝是概率执行，
// 成功帧是窗口已过的证据，不该再闩到声明时刻。
func TestRateGateUnlatchesOnUpstreamSuccess(t *testing.T) {
	gate := newRateGate(0, 0, 0, 0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 30 seconds."))
	var gateErr *rateGateError
	if err := gate.wait(context.Background()); !errors.As(err, &gateErr) {
		t.Fatalf("latched wait error = %v, want *rateGateError", err)
	}
	gate.noteUpstreamSuccess()
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("post-success wait error = %v, want released", err)
	}
}

// 非限流错误不上闩；新闩只延长不提前。
func TestRateGateLatchSelective(t *testing.T) {
	gate := newRateGate(0, 0, 0, 0)
	gate.noteUpstreamError(connect.NewError(connect.CodeInvalidArgument, errors.New("bad request")))
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want nil (no latch)", err)
	}
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 10 minutes."))
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 1 seconds."))
	err := gate.wait(context.Background())
	var gateErr *rateGateError
	if !errors.As(err, &gateErr) || gateErr.retryAfter < 590*time.Second {
		t.Fatalf("wait error = %v, want latch ~600s (max wins)", err)
	}
}

// 令牌桶：容量打空后排队预估超过 maxHold 时本地拒绝，
// 而不是放行去上游续债。
func TestRateGateBucketReject(t *testing.T) {
	gate := newRateGate(1, 0, 0, 0) // 1 rpm：容量 1、补充 1/60s
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("first wait error = %v, want immediate pass", err)
	}
	err := gate.wait(context.Background())
	var gateErr *rateGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("second wait error = %v, want *rateGateError", err)
	}
	if gateErr.retryAfter < 59*time.Second {
		t.Fatalf("retryAfter = %v, want ~60s", gateErr.retryAfter)
	}
}

// 闩期间冻结令牌桶：存量清零、闩内不累计。解除后队列按 refill
// 节奏逐条放行（首条即探针），而不是满桶齐射——实测上游在闩末
// 仍在边际态，齐射必然重触并各加 ~2.4s 刑期。
func TestRateGateLatchFreezesBucket(t *testing.T) {
	gate := newRateGate(60, 0, 0, 0) // 每秒 1 令牌，闩前满桶 60
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 1 seconds."))
	time.Sleep(1100 * time.Millisecond) // 闩过期；若未冻结，桶已重新攒满、三条都瞬时放行
	for i := 0; i < 3; i++ {
		start := time.Now()
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("post-latch wait %d error = %v", i, err)
		}
		if d := time.Since(start); d < 600*time.Millisecond {
			t.Fatalf("post-latch wait %d released after %v, want ~1s serialized drip", i, d)
		}
	}
}

// 等待中 ctx 取消：返回取消原因且退还令牌。
func TestRateGateWaitCancelRefunds(t *testing.T) {
	gate := newRateGate(6, 0, 0, 0) // 0.1/s 补充：排空满桶后缺口 ~10s < maxHold，原地等待
	for i := 0; i < 6; i++ {
		if err := gate.wait(context.Background()); err != nil {
			t.Fatalf("drain wait %d error = %v, want immediate pass", i, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := gate.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context.Canceled", err)
	}
}

func TestRetryAfterParsesMinutes(t *testing.T) {
	for text, want := range map[string]int{
		"Your limit will reset in 32 seconds.": 32,
		"Your limit will reset in 1 minute.":   60,
		"Your limit will reset in 8 minutes.":  480,
	} {
		if got, ok := common.RetryAfterSeconds(text); !ok || got != want {
			t.Errorf("RetryAfterSeconds(%q) = %d,%v, want %d", text, got, ok, want)
		}
	}
	if _, ok := common.RetryAfterSeconds("no hint here"); ok {
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
	reset, ok := common.RateLimitReset("Your limit will reset in 1 minute.", now)
	if !ok {
		t.Fatal("minute hint should parse")
	}
	// 04:37:12 所在分钟桶的 :59 → 04:37:59。
	if want := time.Date(2026, 9, 14, 4, 37, 59, 0, time.Local); !reset.Equal(want) {
		t.Fatalf("RateLimitReset = %v, want %v", reset, want)
	}
	// 目标时刻已过 :59 时进下一分钟桶界：04:36:59.5 + 1min = 04:37:59.5，
	// 本分钟 :59 已过 → 04:38:59。
	reset, ok = common.RateLimitReset("Your limit will reset in 1 minute.",
		time.Date(2026, 9, 14, 4, 36, 59, int(500*time.Millisecond), time.Local))
	if !ok {
		t.Fatal("minute hint should parse")
	}
	if want := time.Date(2026, 9, 14, 4, 38, 59, 0, time.Local); !reset.Equal(want) {
		t.Fatalf("RateLimitReset = %v, want %v", reset, want)
	}
	if _, ok = common.RateLimitReset("Your limit will reset in 0 minutes.", now); ok {
		t.Fatal("zero-minute hint should not parse")
	}
	reset, ok = common.RateLimitReset("Your limit will reset in 1 minute.", time.Date(2026, 9, 14, 4, 36, 1, 0, time.Local))
	if !ok {
		t.Fatal("minute hint should parse")
	}
	if want := time.Date(2026, 9, 14, 4, 37, 59, 0, time.Local); !reset.Equal(want) {
		t.Fatalf("RateLimitReset = %v, want %v", reset, want)
	}
	// 秒级 hint 原样生效。
	reset, ok = common.RateLimitReset("Your limit will reset in 30 seconds.", now)
	if !ok || !reset.Equal(now.Add(30*time.Second)) {
		t.Fatalf("seconds RateLimitReset = %v,%v", reset, ok)
	}
}
