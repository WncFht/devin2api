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

// TestClassifyTypedFailure 验证已分类记录在副本上派生——生产侧原对象
// 不被回写（并发 Classify 共享它是安全的），已置位字段不被重置，重复
// Classify 幂等；Cause 保留外层完整链，Join 在 Failure 之外的兄弟仍可判定。
func TestClassifyTypedFailure(t *testing.T) {
	produced := &Failure{Code: "resource_exhausted", Message: "local gate", LocalGate: true, RetryAfterSeconds: 30}
	got := Classify(produced)
	if got == produced {
		t.Fatal("Classify must derive on a copy, not write back into the shared record")
	}
	if produced.RateLimited {
		t.Fatalf("derived fields must not leak into the producer's record: %+v", produced)
	}
	if !got.RateLimited || !got.LocalGate || got.RetryAfterSeconds != 30 {
		t.Fatalf("producer fields must survive derive: %+v", got)
	}
	if wrapped := Classify(errors.Join(produced, context.Canceled)); !wrapped.Canceled || !errors.Is(wrapped.Cause, context.Canceled) {
		t.Fatalf("outer chain siblings must stay visible through Cause: %+v", wrapped)
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
	// 正则带 (?i)，单位大小写不定——"Minutes" 必须归一后判分钟粒度，
	// 落秒分支会把等待缩 60 倍。
	if failure := ClassifyText("resource_exhausted: rate limited. Your limit will reset in 2 Minutes."); failure.RetryAfterSeconds != 120 || !failure.RetryAfterMinute {
		t.Fatalf("case-insensitive minute hint = %ds,minute=%v, want 120s,true", failure.RetryAfterSeconds, failure.RetryAfterMinute)
	}
	// 冒号变体与词边界："reset in: 30 seconds" 认声明，"preset in"
	// 的复合词不算。
	if seconds := ClassifyText("reset in: 30 seconds").RetryAfterSeconds; seconds != 30 {
		t.Fatalf("colon variant RetryAfterSeconds = %d, want 30", seconds)
	}
	if failure := ClassifyText("preset in 5 seconds"); failure.ResetHint {
		t.Fatal("mid-word 'preset in' must not count as reset declaration")
	}
}

// TestRetryAfterGoDuration 验证同族上游的 Go duration 自述格式
// "Resets in: 3h0m0s"（Windsurf/Codeium 系侧录）：按 ParseDuration
// 折成精确秒数，不置分钟桶界标记。
func TestRetryAfterGoDuration(t *testing.T) {
	if failure := ClassifyText("resource_exhausted: rate limited. Resets in: 3h0m0s"); failure.RetryAfterSeconds != 10800 || failure.RetryAfterMinute || !failure.ResetHint {
		t.Fatalf("duration hint = %ds,minute=%v,hint=%v, want 10800,false,true", failure.RetryAfterSeconds, failure.RetryAfterMinute, failure.ResetHint)
	}
	if seconds := ClassifyText("resets in 1h30m").RetryAfterSeconds; seconds != 5400 {
		t.Fatalf("no-colon duration RetryAfterSeconds = %d, want 5400", seconds)
	}
	// 单位大小写经归一后解析（"3H0M0S" 的 ParseDuration 原文不收）。
	if seconds := ClassifyText("Resets In: 3H0M0S").RetryAfterSeconds; seconds != 10800 {
		t.Fatalf("uppercase duration RetryAfterSeconds = %d, want 10800", seconds)
	}
	// 亚秒与显式 0 duration 都落到「重置即现在」语义（秒数截断为 0、
	// hint 在场），与 "reset in 0 seconds" 同态。
	if failure := ClassifyText("resets in: 500ms"); failure.RetryAfterSeconds != 0 || !failure.ResetHint {
		t.Fatal("sub-second duration must collapse to explicit-zero semantics")
	}
	if failure := ClassifyText("resets in: 0s"); failure.RetryAfterSeconds != 0 || !failure.ResetHint {
		t.Fatal("zero duration must report explicit zero with hint")
	}
	// 非 duration 形状不误认：单位缺席或超 ParseDuration 值域
	// （>292 年溢出）都按无声明处理。
	if failure := ClassifyText("resets in: soon"); failure.ResetHint {
		t.Fatal("non-duration token must not count as hint")
	}
	if failure := ClassifyText("resets in: 99999999999h"); failure.ResetHint {
		t.Fatal("overflowing duration must fall back to no-hint")
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

// TestRateLimitResetMaxWait 验证采纳时长的 6h 封顶：字段保留上游字面
// 值（排障可见原始声明），采纳时刻收敛到 rateLimitResetMaxWait——
// 超窗声明到点重探即拿新 hint，畸值不再让闩封禁以月计；封顶同时
// 挡住 Duration 乘法的 int64 溢出。
func TestRateLimitResetMaxWait(t *testing.T) {
	now := time.Now()
	want := now.Add(6 * time.Hour)
	failure := ClassifyText("resets in: 10h")
	if failure.RetryAfterSeconds != 36000 {
		t.Fatalf("literal parse must keep upstream claim: %d, want 36000", failure.RetryAfterSeconds)
	}
	if reset, ok := failure.RateLimitReset(now); !ok || !reset.Equal(want) {
		t.Fatalf("10h duration reset = %v,%v, want %v,true", reset, ok, want)
	}
	// 分钟路径同样封顶（字段 30000s 字面，采纳 6h）。
	failure = ClassifyText("reset in 500 minutes")
	if !failure.RetryAfterMinute {
		t.Fatal("minute hint must keep bucket flag")
	}
	if reset, ok := failure.RateLimitReset(now); !ok || !reset.Equal(want) {
		t.Fatalf("500-minute reset = %v,%v, want %v,true", reset, ok, want)
	}
	// Duration 溢出的字面秒（>292 年）按上限采纳而非绕回过去。
	failure = ClassifyText("reset in 99999999999 seconds")
	if reset, ok := failure.RateLimitReset(now); !ok || !reset.Equal(want) {
		t.Fatalf("overflowing seconds reset = %v,%v, want %v,true", reset, ok, want)
	}
	// 上限内的声明不受影响。
	if reset, ok := ClassifyText("reset in 30 seconds").RateLimitReset(now); !ok || !reset.Equal(now.Add(30*time.Second)) {
		t.Fatalf("in-range reset = %v,%v", reset, ok)
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
