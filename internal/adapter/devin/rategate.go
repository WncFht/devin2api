// 本文件实现发往上游 GetChatMessage 的本地速率闸门。
package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/WncFht/devin2api/internal/api/common"
)

const (
	// gateDefaultMaxHold 是闸门内允许的最长排队等待：令牌排队预计
	// 超过它时请求在本地快速失败并带 Retry-After——客户端/下游网关
	// 按声明时刻退避，比占着并发槽空等更符合冷却语义。
	gateDefaultMaxHold = 15 * time.Second
	// gateDefaultDripInterval 是冷却闩内放行探针的间隔：上游限流按
	// 分钟桶计数，闩内到达速率压到秒级一条即可探出解闩又不续债。
	gateDefaultDripInterval = 8 * time.Second
	// gateDefaultLatch 是上游 resource_exhausted 未携带 reset hint 时的
	// 兜底闩时长（对齐同类网关 60s 冷却默认值）。
	gateDefaultLatch = 60 * time.Second
)

// rateGate 整形发往上游的消息流，两层机制各自独立：
//  1. 令牌桶把持续速率压到配置 rpm，桶容量取一整分钟额度以吸收
//     claude-cli 并行子代理的瞬时突发；预计排队超过 maxHold 的
//     请求直接本地拒绝，不打到上游。
//  2. 冷却闩：上游 resource_exhausted 声明「reset in N」时上闩到
//     该时刻（分钟 hint 向上对齐到 :59 桶界）。上游限流器实测按
//     分钟桶计数且把被拒尝试也计入，闩内若整队睡到恢复时刻再齐射，
//     边际态下必然重触并把 1 分钟小限流续成十几分钟自封（实测每次
//     到期齐射 ~20 条、5/5 次重触）。因此闩内不排队：按滴灌间隔
//     放探针，其余请求立即 429 + Retry-After=闩剩余快败，客户端
//     睡到恢复时刻再来；任一上游成功帧即提前解闩（边际态下拒绝
//     是概率执行，成功帧是窗口已过的证据）。上闩同时冻结令牌桶：
//     闩内不累计、存量额度清零，解闩后队列按 refill 节奏逐条放行。
//     上游规则推导见 docs/upstream-rate-limit.md。
type rateGate struct {
	mu           sync.Mutex
	tokens       float64 // 当前令牌数；可为负，负值是已预售给排队者的额度
	capacity     float64 // 桶容量：攒满即一分钟的请求额度
	refillPerSec float64 // 令牌补充速率；0 表示不限速（闩仍然生效）
	lastAccrual  time.Time
	limitedUntil time.Time // 冷却闩截止时刻；零值表示未上闩
	nextDrip     time.Time // 闩内下一个探针放行时刻
	maxHold      time.Duration
	dripInterval time.Duration
	defaultLatch time.Duration
	// statePath 非空时冷却闩截止时刻落盘（tmp+rename）：重启后仍在闩内
	// 的实例不会裸发上游把限流续长——上游限流器把被拒尝试也计入窗口。
	statePath string
	// 计数器供面板 stats 透出闸门状态；全部在 mu 下读写。
	latchCount    int
	dripCount     int
	rejectLatched int // 闩内被快败的请求数
	rejectHold    int // 闩外排队预计超 maxHold 被快败的请求数
	waiters       int // 当前睡在令牌桶上的请求数（闩内快败不进此列）
	// 闩迁移事件环：计数器只说发生过几次上闩，事件环回答「什么时候闩的、
	// 闩了多久、怎么解的」——概览趋势图的闩时段底色与系统页事件表同源。
	events    [gateEventCap]gateEvent
	eventHead int
	eventSize int
}

// gateEventCap 是闩事件环容量；闩迁移低频，64 条足够回看一整天。
const gateEventCap = 64

// gateEvent 是一次闩状态迁移的采样。kind：latched（上游限流上闩/延闩）、
// released（成功帧提前解闩）、expired（闩到期自然失效）、restored（重启
// 从 statePath 恢复未过期闩）。until 是该事件涉及的闩截止时刻。
type gateEvent struct {
	At     time.Time  `json:"at"`
	Kind   string     `json:"kind"`
	Until  *time.Time `json:"until,omitempty"`
	Detail string     `json:"detail,omitempty"` // latched 时 "extended" 表示闩中延闩
}

// pushEvent 追加一条闩迁移事件；调用方须持 mu（启动恢复路径在并发前
// 调用，视同持锁）。
func (gate *rateGate) pushEvent(kind string, until time.Time, detail string) {
	ev := gateEvent{At: time.Now(), Kind: kind, Detail: detail}
	if !until.IsZero() {
		u := until
		ev.Until = &u
	}
	gate.events[gate.eventHead] = ev
	gate.eventHead = (gate.eventHead + 1) % gateEventCap
	if gate.eventSize < gateEventCap {
		gate.eventSize++
	}
}

// expireIfDue 把到期的闩自然失效化：补 expired 事件并清闩——闩到期
// 不是解闩（没有成功帧证据），但截止已过，内存态与状态文件都该闭环。
// 调用方须持 mu。wait 路径每个请求检查一次，stats 轮询兜底——无流量
// 时闩到期也能在事件环与快照里及时反映。
func (gate *rateGate) expireIfDue(now time.Time) {
	if gate.limitedUntil.IsZero() || now.Before(gate.limitedUntil) {
		return
	}
	gate.pushEvent("expired", gate.limitedUntil, "")
	gate.limitedUntil = time.Time{}
	gate.nextDrip = time.Time{}
	gate.clearState()
}

// gateStateFile 是冷却闩的落盘形态；只持久化截止时刻——滴灌时钟与
// 令牌桶刻意不存（重启满桶是想要的，闩内节奏按 dripInterval 重排即可）。
type gateStateFile struct {
	LimitedUntil time.Time `json:"limited_until"`
}

// GateStats 是闸门状态快照，面板 /panel/api/stats 的 gate 段透出。
type GateStats struct {
	Latched       bool        `json:"latched"`
	LimitedUntil  *time.Time  `json:"limited_until,omitempty"`
	LatchCount    int         `json:"latch_count"`
	DripCount     int         `json:"drip_count"`
	RejectLatched int         `json:"reject_latched_count"`
	RejectHold    int         `json:"reject_hold_count"`
	RefillPerSec  float64     `json:"refill_per_sec"`
	Tokens        float64     `json:"tokens"`
	Capacity      float64     `json:"capacity"`
	Waiters       int         `json:"waiters"`
	Events        []gateEvent `json:"events,omitempty"` // 新在前
}

// newRateGate 创建速率闸门；rpm<=0 时只有冷却闩生效，不做主动限速。
// 时长参数 <=0 时用默认值。statePath 非空时恢复未过期的冷却闩。
func newRateGate(rpm int, maxHold, dripInterval, defaultLatch time.Duration, statePath string) *rateGate {
	gate := &rateGate{
		lastAccrual:  time.Now(),
		maxHold:      maxHold,
		dripInterval: dripInterval,
		defaultLatch: defaultLatch,
		statePath:    statePath,
	}
	gate.setParams(rpm, maxHold, dripInterval, defaultLatch)
	if rpm > 0 {
		gate.tokens = gate.capacity
	}
	gate.restoreState()
	return gate
}

// setParams 原位更新闸门参数（reload 热路径）：闩态与令牌桶保留，
// rpm 变化只改补充速率与容量，存量令牌按新容量截断。
func (gate *rateGate) setParams(rpm int, maxHold, dripInterval, defaultLatch time.Duration) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.maxHold = gateDurationOrDefault(maxHold, gateDefaultMaxHold)
	gate.dripInterval = gateDurationOrDefault(dripInterval, gateDefaultDripInterval)
	gate.defaultLatch = gateDurationOrDefault(defaultLatch, gateDefaultLatch)
	if rpm > 0 {
		gate.refillPerSec = float64(rpm) / 60
		gate.capacity = float64(rpm)
	} else {
		gate.refillPerSec = 0
		gate.capacity = 0
	}
	gate.tokens = math.Min(gate.tokens, gate.capacity)
}

// gateDurationOrDefault 把 <=0 的时长参数回落到默认值。
func gateDurationOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// restoreState 在启动时恢复未过期的冷却闩：冻结令牌桶到闩末、滴灌
// 时钟按间隔重排——与 noteUpstreamError 的延闩路径保持同一组不变量。
// 文件缺失/损坏/已过期都按无闩处理并顺手清掉过期文件。
func (gate *rateGate) restoreState() {
	if gate.statePath == "" {
		return
	}
	data, err := os.ReadFile(gate.statePath)
	if err != nil {
		return
	}
	var state gateStateFile
	if err := json.Unmarshal(data, &state); err != nil || !state.LimitedUntil.After(time.Now()) {
		_ = os.Remove(gate.statePath)
		return
	}
	gate.limitedUntil = state.LimitedUntil
	gate.nextDrip = time.Now().Add(gate.dripInterval)
	gate.tokens = math.Min(gate.tokens, 0)
	gate.lastAccrual = state.LimitedUntil
	gate.pushEvent("restored", state.LimitedUntil, "")
	slog.Warn("rate gate latch restored from state file", "until", state.LimitedUntil.Format(time.RFC3339))
}

// persistState 把冷却闩截止时刻原子落盘（tmp+rename）；写失败只记
// 日志——落盘是防重启续限的保险，不挡请求路径。
func (gate *rateGate) persistState(until time.Time) {
	if gate.statePath == "" {
		return
	}
	data, err := json.Marshal(gateStateFile{LimitedUntil: until})
	if err != nil {
		return
	}
	tmp := gate.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		slog.Warn("rate gate state write failed", "error", err)
		return
	}
	if err := os.Rename(tmp, gate.statePath); err != nil {
		slog.Warn("rate gate state rename failed", "error", err)
	}
}

// clearState 在解闩后移除状态文件；文件不存在不算错误。
func (gate *rateGate) clearState() {
	if gate.statePath == "" {
		return
	}
	if err := os.Remove(gate.statePath); err != nil && !os.IsNotExist(err) {
		slog.Warn("rate gate state remove failed", "error", err)
	}
}

// stats 返回闸门状态快照。顺带惰性结算到期闩：wait 只在有流量时
// 触发，无流量时段的闩到期由这里的轮询补记，面板时间线才闭环。
func (gate *rateGate) stats() GateStats {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.expireIfDue(time.Now())
	stats := GateStats{
		Latched:       !gate.limitedUntil.IsZero() && time.Now().Before(gate.limitedUntil),
		LatchCount:    gate.latchCount,
		DripCount:     gate.dripCount,
		RejectLatched: gate.rejectLatched,
		RejectHold:    gate.rejectHold,
		RefillPerSec:  gate.refillPerSec,
		Tokens:        gate.tokens,
		Capacity:      gate.capacity,
		Waiters:       gate.waiters,
	}
	for i := 1; i <= gate.eventSize; i++ {
		stats.Events = append(stats.Events, gate.events[(gate.eventHead-i+gateEventCap)%gateEventCap])
	}
	if !gate.limitedUntil.IsZero() {
		until := gate.limitedUntil
		stats.LimitedUntil = &until
	}
	return stats
}

// wait 阻塞到本次上游发送拿到许可，或判定不值得等：
//   - 闩内：滴灌槽空闲立即放行（该请求即探针），槽被占直接返回
//     *rateGateError，retryAfter 报闩剩余——客户端睡到恢复时刻
//     重试比按槽位节奏轮询更省重试预算，长闩也能完整存活；
//   - 闩外：令牌排队预计超过 maxHold 返回 *rateGateError；
//   - 等待中 ctx 取消退还令牌并返回原因；睡醒后不直接放行，
//     回到循环首重新评估——睡眠期间闩态可能已变。
func (gate *rateGate) wait(ctx context.Context) error {
	if gate == nil {
		return nil
	}
	// presold 是本请求已预售的令牌：睡醒复检发现已上闩时先退还，
	// 再按闩内规则决定去留——预售额在冻结期不能带走。
	presold := false
	// sleeping 标记本请求占着一个 waiters 名额：睡醒回到循环首的
	// 同一把锁里归还，与状态重估保持同一临界区。
	sleeping := false
	for {
		now := time.Now()
		gate.mu.Lock()
		if sleeping {
			gate.waiters--
			sleeping = false
		}
		// 令牌按经过时间补充；预订式扣减（允许为负）让等待者的发车间隔
		// 自动错开，闩解除或突发结束时不会向上游齐射。lastAccrual 可被
		// 闩推到将来（冻结期），只在正向流逝时结算，否则闩内每次调用
		// 都会把桶扣得更深。
		if elapsed := now.Sub(gate.lastAccrual); elapsed > 0 {
			gate.tokens = math.Min(gate.tokens+elapsed.Seconds()*gate.refillPerSec, gate.capacity)
			gate.lastAccrual = now
		}
		// 闩到期是自然失效而非解闩（没有成功帧证据）。
		gate.expireIfDue(now)
		if now.Before(gate.limitedUntil) {
			if presold {
				gate.tokens = math.Min(gate.tokens+1, gate.capacity)
				presold = false
			}
			if !now.Before(gate.nextDrip) {
				// 探针槽空闲：放行并推进下一个槽。滴灌放行不耗令牌——
				// 闩内节奏由槽位控制，桶仍冻结到闩末。
				gate.nextDrip = now.Add(gate.dripInterval)
				gate.dripCount++
				gate.mu.Unlock()
				return nil
			}
			retryAfter := gate.limitedUntil.Sub(now)
			gate.rejectLatched++
			gate.mu.Unlock()
			return &rateGateError{retryAfter: retryAfter}
		}
		if presold {
			// 睡醒且未上闩：预售令牌生效，放行。
			gate.mu.Unlock()
			return nil
		}
		var wait time.Duration
		if gate.refillPerSec > 0 && gate.tokens < 1 {
			wait = time.Duration((1 - gate.tokens) / gate.refillPerSec * float64(time.Second))
		}
		if wait > gate.maxHold {
			gate.rejectHold++
			gate.mu.Unlock()
			return &rateGateError{retryAfter: wait}
		}
		if gate.refillPerSec > 0 {
			gate.tokens--
			presold = true
		}
		if wait > 0 {
			gate.waiters++
			sleeping = true
		}
		gate.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			gate.mu.Lock()
			gate.waiters--
			gate.tokens = math.Min(gate.tokens+1, gate.capacity)
			presold = false
			gate.mu.Unlock()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// noteUpstreamError 用上游失败刷新冷却闩；只有 resource_exhausted 与
// 限流有关，其余错误原样忽略。闩只延长不提前；新拒绝同时重置滴灌
// 时钟——上一枚探针刚被打回来，下一槽从头计起。
func (gate *rateGate) noteUpstreamError(err error) {
	if gate == nil {
		return
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeResourceExhausted {
		return
	}
	now := time.Now()
	until := now.Add(gate.defaultLatch)
	if resetAt, ok := common.RateLimitReset(err.Error(), now); ok {
		until = resetAt
	}
	gate.mu.Lock()
	extended := until.After(gate.limitedUntil)
	remaining := gate.limitedUntil.Sub(now)
	if extended {
		gate.limitedUntil = until
		gate.nextDrip = now.Add(gate.dripInterval)
		gate.latchCount++
		// 闩中再触记 extended——原闩未到期就被刷新截止，与新闩区分开。
		detail := ""
		if remaining > 0 {
			detail = "extended"
		}
		gate.pushEvent("latched", until, detail)
		// 冻结令牌桶到闩末：清空存量额度且闩内不累计，解除后队列
		// 按 refill 节奏逐条放行而非满桶齐射。
		gate.tokens = math.Min(gate.tokens, 0)
		gate.lastAccrual = until
	}
	gate.mu.Unlock()
	if extended {
		gate.persistState(until)
		slog.Warn("upstream message rate limited; drip-latching new requests", "until", until.Format(time.RFC3339), "latch", until.Sub(now))
	} else {
		slog.Info("upstream message rate limited while latched", "remaining", remaining)
	}
}

// noteUpstreamSuccess 用任一上游正常帧解除冷却闩：边际态下拒绝是
// 概率执行，成功帧说明窗口已过，继续闩到声明时刻只会浪费滴灌窗口。
// 冻结期 lastAccrual 保持原值——解闩后队列仍按 refill 节奏放行，
// 提早解闩不等于立刻满速。
func (gate *rateGate) noteUpstreamSuccess() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	latched := !gate.limitedUntil.IsZero()
	if latched {
		gate.pushEvent("released", gate.limitedUntil, "")
		gate.limitedUntil = time.Time{}
		gate.nextDrip = time.Time{}
	}
	gate.mu.Unlock()
	if latched {
		gate.clearState()
		slog.Info("rate gate released: upstream accepted a message")
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
