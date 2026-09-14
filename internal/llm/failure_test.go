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
	if got := Classify(io.EOF); !got.UpstreamFault {
		t.Fatalf("bare EOF must be UpstreamFault: %+v", got)
	}
	if got := Classify(context.Canceled); got.UpstreamFault || !got.Canceled {
		t.Fatalf("canceled must not be UpstreamFault: %+v", got)
	}
	if got := ClassifyText("invalid_argument: bad request"); got.UpstreamFault || !got.ClientFixable {
		t.Fatalf("plain invalid_argument must stay ClientFixable: %+v", got)
	}
}

// TestRetryAfterSeconds 验证从上游限流文案解析重置窗口——上游没有
// Retry-After/RetryInfo，"reset in N seconds" 是唯一可行动 hint。
func TestRetryAfterSeconds(t *testing.T) {
	if seconds := ClassifyText("resource_exhausted: rate limited. Your limit will reset in 42 seconds.").RetryAfterSeconds; seconds != 42 {
		t.Fatalf("RetryAfterSeconds = %d, want 42", seconds)
	}
	if seconds := ClassifyText("resource_exhausted: quota exceeded").RetryAfterSeconds; seconds != 0 {
		t.Fatal("no reset hint must report 0")
	}
	if seconds := ClassifyText("reset in 0 seconds").RetryAfterSeconds; seconds != 0 {
		t.Fatal("zero reset must report 0")
	}
}

// TestRateLimitReset 在 rategate_test.go 里有分钟桶界对齐的全量用例，
// 这里只验证方法入口的零值行为。
func TestRateLimitResetZero(t *testing.T) {
	if _, ok := ClassifyText("plain error").RateLimitReset(time.Now()); ok {
		t.Fatal("no reset hint must report false")
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
