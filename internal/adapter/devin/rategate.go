// 本文件实现发往上游 GetChatMessage 的本地速率闸门。
package devin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/api/common"
)

const (
	// gateMaxHold 是闸门内允许的最长排队等待。上游恢复时刻或令牌排队
	// 超过它时请求在本地快速失败并带 Retry-After：客户端/下游网关
	//按声明时刻退避，比占着并发槽空等更符合冷却语义。
	gateMaxHold = 15 * time.Second
	// gateDefaultLatch 是上游 resource_exhausted 未携带 reset hint 时的
	// 兜底闩时长（对齐同类网关 60s 冷却默认值）。
	gateDefaultLatch = 60 * time.Second
)

// rateGate 整形发往上游的消息流，两层机制各自独立：
//  1. 令牌桶把持续速率压到配置 rpm，桶容量取一整分钟额度以吸收
//     claude-cli 并行子代理的瞬时突发；预计排队超过 gateMaxHold 的
//     请求直接本地拒绝，不打到上游。
//  2. 冷却闩：上游 resource_exhausted 声明「reset in N」时上闩到
//     该时刻。上游限流器实测把被拒尝试也计入、每次再推后恢复
//     ~2.4s（封顶 10min）——闩内本地拦停是防止「客户端重试把 1
//     分钟小限流续成十几分钟自封」的关键。上闩同时冻结令牌桶：
//     闩内不累计、存量额度清零——否则闩末桶已攒满，整队等待者
//     对齐释放形成齐射，在边际态上游必然重触（实测每次到期齐射
//     ~20 条、5/5 次重触）；冻结后队列按 refill 节奏逐条放行，
//     第一条即探针，被拒在 ~RTT 内重闩，损失 1~2 条而非整队。
//     上游规则推导见 notes/upstream-rate-limit.md。
type rateGate struct {
	mu           sync.Mutex
	tokens       float64 // 当前令牌数；可为负，负值是已预售给排队者的额度
	capacity     float64 // 桶容量：攒满即一分钟的请求额度
	refillPerSec float64 // 令牌补充速率；0 表示不限速（闩仍然生效）
	lastAccrual  time.Time
	limitedUntil time.Time // 冷却闩截止时刻；零值表示未上闩
}

// newRateGate 创建速率闸门；rpm<=0 时只有冷却闩生效，不做主动限速。
func newRateGate(rpm int) *rateGate {
	gate := &rateGate{lastAccrual: time.Now()}
	if rpm > 0 {
		gate.refillPerSec = float64(rpm) / 60
		gate.capacity = float64(rpm)
		gate.tokens = gate.capacity
	}
	return gate
}

// wait 阻塞到本次上游发送拿到许可，或判定不值得等：
// 预计等待超过 gateMaxHold 返回 *rateGateError（沿现有错误管道译成
// 429 + Retry-After）；等待中 ctx 取消则退还令牌并返回取消原因。
func (gate *rateGate) wait(ctx context.Context) error {
	if gate == nil {
		return nil
	}
	now := time.Now()
	gate.mu.Lock()
	// 令牌按经过时间补充；预订式扣减（允许为负）让等待者的发车间隔
	// 自动错开，闩解除或突发结束时不会向上游齐射。lastAccrual 可被
	// 闩推到将来（冻结期），只在正向流逝时结算，否则闩内每次调用
	// 都会把桶扣得更深。
	if elapsed := now.Sub(gate.lastAccrual); elapsed > 0 {
		gate.tokens = math.Min(gate.tokens+elapsed.Seconds()*gate.refillPerSec, gate.capacity)
		gate.lastAccrual = now
	}
	wait := gate.limitedUntil.Sub(now)
	if gate.refillPerSec > 0 && gate.tokens < 1 {
		if deficit := time.Duration((1 - gate.tokens) / gate.refillPerSec * float64(time.Second)); deficit > wait {
			wait = deficit
		}
	}
	if wait > gateMaxHold {
		gate.mu.Unlock()
		return &rateGateError{retryAfter: wait}
	}
	if gate.refillPerSec > 0 {
		gate.tokens--
	}
	gate.mu.Unlock()
	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		gate.mu.Lock()
		gate.tokens = math.Min(gate.tokens+1, gate.capacity)
		gate.mu.Unlock()
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

// noteUpstreamError 用上游失败刷新冷却闩；只有 resource_exhausted 与
// 限流有关，其余错误原样忽略。
func (gate *rateGate) noteUpstreamError(err error) {
	if gate == nil {
		return
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeResourceExhausted {
		return
	}
	latch := gateDefaultLatch
	if seconds, ok := common.RetryAfterSeconds(err.Error()); ok {
		latch = time.Duration(seconds) * time.Second
	}
	until := time.Now().Add(latch)
	gate.mu.Lock()
	extended := until.After(gate.limitedUntil)
	if extended {
		gate.limitedUntil = until
		// 冻结令牌桶到闩末：清空存量额度且闩内不累计，解除后队列
		// 按 refill 节奏逐条放行而非满桶齐射。
		gate.tokens = math.Min(gate.tokens, 0)
		gate.lastAccrual = until
	}
	gate.mu.Unlock()
	if extended {
		slog.Warn("upstream message rate limited; holding new requests", "until", until.Format(time.RFC3339), "latch", latch)
	}
}

// rateGateError 是本地闸门的拒绝。文案沿用上游限流格式
// （resource_exhausted + "reset in N seconds"），现有错误管道自然译出
// 429 + Retry-After + retry_after 细节字段，下游无需特判本地/上游。
type rateGateError struct {
	retryAfter time.Duration
}

func (e *rateGateError) Error() string {
	return fmt.Sprintf("resource_exhausted: upstream message rate limited by local gate; your limit will reset in %d seconds.", int(math.Ceil(e.retryAfter.Seconds())))
}
