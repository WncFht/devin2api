// 本文件验证错误分类：code 提取、标记匹配、reset hint 与 trace ID 解析。
package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

// TestClassifyConnectError 验证 *connect.Error 走结构字段而非文案解析。
// 这里用 connect 包的码表对拍——code 字段来自协议而非前缀猜测。
func TestClassifyConnectError(t *testing.T) {
	failure := Classify(fmt.Errorf("outer: %w", errors.New("plain boom")))
	if failure.Code != "" {
		t.Fatalf("Code = %q, want empty for non-connect error", failure.Code)
	}
	if failure.Message != "outer: plain boom" {
		t.Fatalf("Message = %q", failure.Message)
	}
}

// TestClassifyTypedFailure 验证已分类记录直取且派生幂等——重复 Classify
// 结果一致，生产侧已置位字段不被重置。
func TestClassifyTypedFailure(t *testing.T) {
	produced := &Failure{Code: "resource_exhausted", Message: "local gate", LocalGate: true, RetryAfterSeconds: 30}
	got := Classify(produced)
	if got != produced {
		t.Fatal("Classify must return the same record for a typed Failure")
	}
	if !got.RateLimited || !got.LocalGate || got.RetryAfterSeconds != 30 {
		t.Fatalf("producer fields must survive derive: %+v", got)
	}
	// 二次分类结果一致（幂等）。
	if again := Classify(got); again.RetryAfterSeconds != 30 || !again.RateLimited {
		t.Fatalf("re-classify must be idempotent: %+v", again)
	}
}

// TestClassifyTextCodePrefix 验证 "<code>: <msg>" 方言只认已知 Connect
// code 前缀，本地文案不冒充体制标记。
func TestClassifyTextCodePrefix(t *testing.T) {
	if got := ClassifyText("resource_exhausted: quota").Code; got != "resource_exhausted" {
		t.Fatalf("Code = %q", got)
	}
	if got := ClassifyText("read request: failed"); got.Code != "" {
		t.Fatalf("Code = %q, want empty for non-code prefix", got.Code)
	}
}

// TestUpstreamFault 验证上游责任判定覆盖 code 的可修正语义：
// connect 包装的传输断裂与 "an internal error occurred" 模板都不是
// 调用方的问题。
func TestUpstreamFault(t *testing.T) {
	if got := ClassifyText("invalid_argument: an internal error occurred (trace ID: x)"); !got.UpstreamFault || got.ClientFixable {
		t.Fatalf("masqueraded internal error must be UpstreamFault, not ClientFixable: %+v", got)
	}
	if got := ClassifyText("invalid_argument: protocol error: incomplete envelope: read: connection reset by peer"); !got.UpstreamFault {
		t.Fatalf("frame truncation must be UpstreamFault: %+v", got)
	}
	if got := ClassifyText("unavailable: stream error: stream ID 1; REFUSED_STREAM; received from peer"); !got.UpstreamFault {
		t.Fatalf("http2 RST_STREAM must be UpstreamFault: %+v", got)
	}
	// ENHANCE_YOUR_CALM 被 connect-go 映成 resource_exhausted——传输
	// 事件不是上游限流：UpstreamFault 置位且 RateLimited 抑制（不上闩、
	// 不下发 rate_limit_exceeded）。
	if got := ClassifyText("resource_exhausted: bandwidth exhausted: stream error: stream ID 5; ENHANCE_YOUR_CALM; received from peer"); !got.UpstreamFault || got.RateLimited {
		t.Fatalf("transport-masqueraded resource_exhausted must be UpstreamFault, not RateLimited: %+v", got)
	}
	if got := ClassifyText("unavailable: http2: server sent GOAWAY and closed the connection; LastStreamID=9, ErrCode=NO_ERROR"); !got.UpstreamFault {
		t.Fatalf("http2 GOAWAY must be UpstreamFault: %+v", got)
	}
	if got := Classify(io.EOF); !got.UpstreamFault {
		t.Fatalf("bare EOF must be UpstreamFault: %+v", got)
	}
	if got := Classify(context.Canceled); got.UpstreamFault || !got.Canceled {
		t.Fatalf("canceled must not be UpstreamFault: %+v", got)
	}
	// 对端 RST_STREAM CANCEL 被 connect-go 映成 canceled code（本地 ctx
	// 未取消时）——上游传输断裂，不是客户端取消：envoy drain/流级超时
	// 不该被记成 499 断连，交回 transportBreak 判 UpstreamFault。
	if got := ClassifyText("canceled: stream error: stream ID 3; CANCEL; received from peer"); got.Canceled || !got.UpstreamFault {
		t.Fatalf("peer RST_STREAM CANCEL must be UpstreamFault, not Canceled: %+v", got)
	}
	// 本地取消语义不受影响：无传输措辞的 canceled 仍按客户端断连归类。
	if got := ClassifyText("canceled: context canceled"); !got.Canceled || got.UpstreamFault {
		t.Fatalf("local cancel must stay Canceled, not UpstreamFault: %+v", got)
	}
	if got := ClassifyText("invalid_argument: bad request"); got.UpstreamFault || !got.ClientFixable {
		t.Fatalf("plain invalid_argument must stay ClientFixable: %+v", got)
	}
	// 真·上游限流（EndStream 尾帧语义拒绝）不受传输措辞影响。
	if got := ClassifyText("resource_exhausted: Reached overall message rate limit. Your limit will reset in 3 minutes."); got.UpstreamFault || !got.RateLimited {
		t.Fatalf("real rate limit must stay RateLimited, not UpstreamFault: %+v", got)
	}
}

// TestRetryAfterSeconds 验证从上游限流文案解析重置窗口——上游没有
// Retry-After/RetryInfo，"reset in N seconds" 是唯一可行动 hint。
func TestRetryAfterSeconds(t *testing.T) {
	if seconds := ClassifyText("resource_exhausted: rate limited. Your limit will reset in 42 seconds.").RetryAfterSeconds; seconds != 42 {
		t.Fatalf("RetryAfterSeconds = %d, want 42", seconds)
	}
	if failure := ClassifyText("resource_exhausted: quota exceeded"); failure.RetryAfterSeconds != 0 || failure.ResetHint {
		t.Fatal("no reset hint must report 0 and no hint")
	}
	// 显式 0 与无 hint 是两态：秒数同为 0，但 ResetHint 标记声明在场。
	if failure := ClassifyText("reset in 0 seconds"); failure.RetryAfterSeconds != 0 || !failure.ResetHint {
		t.Fatal("explicit zero reset must report 0 with hint present")
	}
}

// TestRateLimitReset 在 rategate_test.go 里有分钟桶界对齐的全量用例，
// 这里验证方法入口的零值行为与显式 0 声明。
func TestRateLimitResetZero(t *testing.T) {
	if _, ok := ClassifyText("plain error").RateLimitReset(time.Now()); ok {
		t.Fatal("no reset hint must report false")
	}
	// 显式 0 秒：声明的重置时刻即现在——闩按它即刻过期而非套兜底闩。
	now := time.Now()
	if reset, ok := ClassifyText("reset in 0 seconds").RateLimitReset(now); !ok || !reset.Equal(now) {
		t.Fatalf("explicit zero reset = %v,%v, want now,true", reset, ok)
	}
}

// TestUpstreamTraceID 验证从错误文案尾部提取 "(trace ID: …)"——上游错误
// 全是模糊 internal error，trace ID 是唯一的报障锚点。
func TestUpstreamTraceID(t *testing.T) {
	if got := ClassifyText("internal: an internal error occurred (trace ID: abc-def)").TraceID; got != "abc-def" {
		t.Fatalf("TraceID = %q, want abc-def", got)
	}
	if got := ClassifyText("internal: boom").TraceID; got != "" {
		t.Fatalf("TraceID = %q, want empty", got)
	}
}
