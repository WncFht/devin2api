// 本文件验证流级重试准入里容量类故障的独立档位：无视一次性 reopened
// 额度与 pre-event 累计静默上限（拒绝是活跃快回程而非死寂），但被与
// 发送面同口径的次数/墙钟守卫兜底——detached 孤儿流的 ctx 不死，这组
// 守卫是它在超长 episode 里的唯一终止界。
package devin

import (
	"errors"
	"testing"
	"time"
)

func TestReopenableCapacityBound(t *testing.T) {
	cause := errors.New("unimplemented: We are currently experiencing capacity issues with this serving model. Please switch to a different model or try again later.")
	now := time.Now()

	// 容量类无视一次性额度与静默耗尽：episode 实测 ~8min 必然超 180s
	// 上限，首个事件产出前的连续拒绝必须能一直重开。
	policy := retryPolicy{reopened: true}
	if !policy.reopenable(false, cause, false, true, now) {
		t.Fatal("capacity cause must bypass reopened budget and silence cap")
	}

	// 已产出内容的流只能续传——容量类不破这条边界。
	if policy.reopenable(true, cause, false, false, now) {
		t.Fatal("produced stream must not reopen on capacity cause")
	}

	// 次数守卫：簿记到顶的流拒绝再开——孤儿流不能无限打上游。
	policy = retryPolicy{capacityReopens: capacityResendLimit}
	if policy.reopenable(false, cause, false, false, now) {
		t.Fatal("capacity reopens at the resend limit must refuse")
	}

	// 墙钟守卫：首个容量重开距今超过吸收预算即收口。
	policy = retryPolicy{capacitySince: now.Add(-capacityAbsorbBudget)}
	if policy.reopenable(false, cause, false, false, now) {
		t.Fatal("capacity window past the absorb budget must refuse")
	}

	// 非容量类维持原判据：reopened 已用即拒，未用在静默耗尽前放行。
	policy = retryPolicy{}
	transport := errors.New("stream disconnected")
	if !policy.reopenable(false, transport, false, false, now) {
		t.Fatal("fresh transport failure should be reopenable")
	}
	policy.reopened = true
	if policy.reopenable(false, transport, false, false, now) {
		t.Fatal("transport failure after one reopen must refuse")
	}
}
