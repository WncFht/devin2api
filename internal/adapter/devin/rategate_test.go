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

// 限流闩未到声明时刻：wait 本地拒绝且 retryAfter 等于闩剩余时长。
func TestRateGateLatchRejectsUntilReset(t *testing.T) {
	gate := newRateGate(0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Please try again later. Your limit will reset in 8 minutes. (trace ID: x)"))
	err := gate.wait(context.Background())
	var gateErr *rateGateError
	if !errors.As(err, &gateErr) {
		t.Fatalf("wait error = %v, want *rateGateError", err)
	}
	if gateErr.retryAfter < 470*time.Second || gateErr.retryAfter > 480*time.Second {
		t.Fatalf("retryAfter = %v, want ~480s", gateErr.retryAfter)
	}
	// 错误文案必须能被公共错误管道译出 429 + Retry-After。
	if status := common.HTTPStatus(err.Error()); status != 429 {
		t.Fatalf("HTTPStatus = %d, want 429", status)
	}
	if seconds, ok := common.RetryAfterSeconds(err.Error()); !ok || seconds < 470 || seconds > 480 {
		t.Fatalf("RetryAfterSeconds = %d,%v, want ~480", seconds, ok)
	}
}

// 闩剩余很短时 wait 原地等待、到点放行，不产生本地拒绝。
func TestRateGateShortLatchHolds(t *testing.T) {
	gate := newRateGate(0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 1 seconds."))
	start := time.Now()
	if err := gate.wait(context.Background()); err != nil {
		t.Fatalf("wait error = %v, want held then released", err)
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("wait returned after %v, want ~1s hold", elapsed)
	}
}

// 非限流错误不上闩；新闩只延长不提前。
func TestRateGateLatchSelective(t *testing.T) {
	gate := newRateGate(0)
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

// 令牌桶：容量打空后排队预估超过 gateMaxHold 时本地拒绝，
// 而不是放行去上游续债。
func TestRateGateBucketReject(t *testing.T) {
	gate := newRateGate(1) // 1 rpm：容量 1、补充 1/60s
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

// 等待中 ctx 取消：返回取消原因且退还令牌。
func TestRateGateWaitCancelRefunds(t *testing.T) {
	gate := newRateGate(0)
	gate.noteUpstreamError(rateLimitErr("Reached overall message rate limit. Your limit will reset in 10 seconds."))
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
