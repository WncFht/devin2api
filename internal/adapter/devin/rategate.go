// 本文件实现发往上游 GetChatMessage 的本地速率闸门。
package devin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

const (
	// gateDefaultMaxHold 是闸门内允许的最长排队等待：预计睡到下一窗口
	// 超过它时请求在本地快速失败并带 Retry-After——客户端/下游网关
	// 按声明时刻退避，比占着并发槽空等更符合冷却语义。
	gateDefaultMaxHold = 15 * time.Second
	// gateDefaultDripInterval 是冷却闩内放行探针的间隔：上游限流按
	// 分钟桶计数，闩内到达速率压到秒级一条即可探出解闩又不续债。
	gateDefaultDripInterval = 8 * time.Second
	// gateDefaultLatch 是上游 resource_exhausted 未携带 reset hint 时的
	// 兜底闩时长（对齐同类网关 60s 冷却默认值）。
	gateDefaultLatch = 60 * time.Second
	// windowPeriod 是上游限流器的计数周期：实测按自然分钟桶计数。
	windowPeriod = time.Minute
	// gateDefaultWindowGuard 是桶界两侧的停发余量：覆盖桶界估计误差与
	// 多分片漂移，死区内不发送，使每个发送区间严格落在单一上游桶内。
	gateDefaultWindowGuard = 2 * time.Second
)

// rateGate 整形发往上游的消息流，两层机制各自独立：
//  1. 对齐分钟窗口：上游限流器按自然分钟桶计数（桶界实测在本地
//     :59~:00，多分片有漂移），本地把发送对齐到同一套桶——每个窗口
//     配额 max_rpm，窗口两端各留 guard 秒死区，使发送区间严格落在
//     单一上游桶内，单桶可见计数永不超配额。桶内不做秒级整形：上游
//     只按分钟计数，桶内瞬发与均摊在它的计数器里等价，叠加平滑层
//     只增加本地延迟。窗口配额耗尽或落在死区内的请求睡到下一窗口
//     开放；累计等待将超 maxHold 的直接本地 429 + Retry-After 快败。
//  2. 冷却闩：上游 resource_exhausted 声明「reset in N」时上闩到
//     该时刻（分钟 hint 向上对齐到 :59 桶界）。上游限流器实测按
//     分钟桶计数且把被拒尝试也计入，闩内若整队睡到恢复时刻再齐射，
//     边际态下必然重触并把 1 分钟小限流续成十几分钟自封（实测每次
//     到期齐射 ~20 条、5/5 次重触）。因此闩内不排队：按滴灌间隔
//     放探针，其余请求立即 429 + Retry-After=闩剩余快败，客户端
//     睡到恢复时刻再来；任一上游成功帧即提前解闩（边际态下拒绝
//     是概率执行，成功帧是窗口已过的证据）。探针同样只在可发区间
//     内放行并计入本桶配额——探针也是真实上游发送。
//     上游规则推导见 docs/upstream-rate-limit.md。
type rateGate struct {
	mu           sync.Mutex
	quota        int           // 每桶配额（= max_rpm）；<=0 不做窗口限速
	windowOpen   time.Duration // 可发窗口在分钟内的起点（= offset+guard，mod 60s）
	usable       time.Duration // 可发区间长度（= 60s - 2*guard）
	bucketStart  time.Time     // 当前计数桶的窗口起点
	bucketUsed   int           // 本桶已放行数（含滴灌探针，与上游「被拒也计数」口径一致）
	limitedUntil time.Time     // 冷却闩截止时刻；零值表示未上闩
	nextDrip     time.Time     // 闩内下一个探针放行时刻
	maxHold      time.Duration
	dripInterval time.Duration
	defaultLatch time.Duration
	// statePath 非空时冷却闩截止时刻落盘（tmp+rename）：重启后仍在闩内
	// 的实例不会裸发上游把限流续长——上游限流器把被拒尝试也计入窗口。
	statePath string
	// now 是时钟源，测试可替换为可控假钟；窗口位置依赖墙钟，注入后
	// 配额/死区/闩的用例才能确定落在指定分钟秒位。
	now func() time.Time
	// 计数器供面板 stats 透出闸门状态；全部在 mu 下读写。
	latchCount    int
	dripCount     int
	rejectLatched int // 闩内被快败的请求数
	rejectHold    int // 闩外排队预计超 maxHold 被快败的请求数
	waiters       int // 当前睡到下一窗口的请求数（闩内快败不进此列）
	// 闩迁移事件环：计数器只说发生过几次上闩，事件环回答「什么时候闩的、
	// 闩了多久、怎么解的」——概览趋势图的闩时段底色与系统页事件表同源。
	events    [gateEventCap]GateEvent
	eventHead int
	eventSize int
}

// gateEventCap 是闩事件环容量；闩迁移低频，64 条足够回看一整天。
const gateEventCap = 64

// 闩迁移事件种类：latched（上游限流上闩/延闩）、released（成功帧提前
// 解闩）、expired（闩到期自然失效）、restored（重启从 statePath 恢复
// 未过期闩）。
const (
	gateEventLatched  = "latched"
	gateEventReleased = "released"
	gateEventExpired  = "expired"
	gateEventRestored = "restored"
)

// gateEventLabel 是闩事件的面板显示名——词汇（kind）与展示文案同文件
// 产出，前端事件表不再持有镜像标签表；延闩（latched+extended）在产出
// 处就合并成单独显示名。
func gateEventLabel(kind, detail string) string {
	switch kind {
	case gateEventLatched:
		if detail == "extended" {
			return "延闩"
		}
		return "上闩"
	case gateEventReleased:
		return "解闩"
	case gateEventExpired:
		return "到期失效"
	case gateEventRestored:
		return "重启恢复"
	}
	return kind
}

// GateEvent 是一次闩状态迁移的采样。until 是该事件涉及的闩截止时刻；
// label 是产出时算好的面板显示名（kind+detail 的合并文案）。
type GateEvent struct {
	At     time.Time  `json:"at"`
	Kind   string     `json:"kind"`
	Until  *time.Time `json:"until,omitempty"`
	Detail string     `json:"detail,omitempty"` // latched 时 "extended" 表示闩中延闩
	Label  string     `json:"label"`
}

// pushEvent 追加一条闩迁移事件；调用方须持 mu（启动恢复路径在并发前
// 调用，视同持锁）。
func (gate *rateGate) pushEvent(kind string, until time.Time, detail string) {
	ev := GateEvent{At: gate.now(), Kind: kind, Detail: detail, Label: gateEventLabel(kind, detail)}
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
	gate.pushEvent(gateEventExpired, gate.limitedUntil, "")
	gate.limitedUntil = time.Time{}
	gate.nextDrip = time.Time{}
	gate.clearState()
}

// gateStateFile 是冷却闩的落盘形态；只持久化截止时刻——滴灌时钟与
// 窗口计数刻意不存（重启新窗口重新计数是想要的，闩内节奏按
// dripInterval 重排即可）。
type gateStateFile struct {
	LimitedUntil time.Time `json:"limited_until"`
}

// GateStats 是闸门状态快照，面板 /admin/runtime-metrics 的 gate 段透出。
type GateStats struct {
	Latched       bool        `json:"latched"`
	LimitedUntil  *time.Time  `json:"limited_until,omitempty"`
	LatchCount    int         `json:"latch_count"`
	DripCount     int         `json:"drip_count"`
	RejectLatched int         `json:"reject_latched_count"`
	RejectHold    int         `json:"reject_hold_count"`
	WindowQuota   int         `json:"window_quota"`          // 每桶配额（= max_rpm）；0 表示不限速
	WindowUsed    int         `json:"window_used"`           // 当前桶已放行数
	WindowOpen    *time.Time  `json:"window_open,omitempty"` // 当前桶的可发窗口起点
	WindowNext    *time.Time  `json:"window_next,omitempty"` // 下一桶可发窗口开放时刻
	Sendable      bool        `json:"sendable"`              // 当前是否处于可发区间（非死区）
	Waiters       int         `json:"waiters"`
	Events        []GateEvent `json:"events,omitempty"` // 新在前
	// LatchRanges 是从闩事件环还原的闩时段（[start,end] 对），由
	// stats() 与事件环同锁算出——前端趋势图直接铺 markArea，不再在
	// JS 里重放状态机。
	LatchRanges []GateLatchRange `json:"latch_ranges,omitempty"`
}

// GateLatchRange 是一段闩时段；Start 为 nil 表示开窗事件已滚出事件环
// （时段左端不可考，展示层按视窗左缘裁剪）。
type GateLatchRange struct {
	Start *time.Time `json:"start,omitempty"`
	End   time.Time  `json:"end"`
}

// GateConfig 是速率闸门的可调参数集；时长参数 <=0 时取默认值。
// Config.Gate 与闸门入参同型：启动构建与 ApplyConfig 热更新整块下发，
// 不再逐字段翻译。
type GateConfig struct {
	// MaxRPM 是每个对齐分钟窗口内发往上游 GetChatMessage 的配额
	// （条/分钟）；<=0 不做主动限速。上游限流冷却闩不受此项影响，始终生效。
	MaxRPM int
	// MaxHold/DripInterval/DefaultLatch 是冷却闩参数：
	// 闩外排队允许的最长等待、闩内滴灌探针的放行间隔、上游未带
	// reset hint 时的兜底闩时长。
	MaxHold      time.Duration
	DripInterval time.Duration
	DefaultLatch time.Duration
	// WindowOffset 是上游分钟桶界在本地分钟内的估计位置——拒绝 hint
	// 隐含 deadline 实测落在 :58.6~:01（上游时钟快 ~1s），默认 0 即以
	// 本地 :00 为估计中心，负值按 mod 60 折算（-1 = :59）；WindowGuard
	// 是桶界两侧的停发死区——可发区间 = [offset+guard, offset+60-guard)，
	// 只要真实桶界落在估计值 ±guard 内，每个可发区间都是某个真实上游
	// 桶的严格子集，单桶可见发送计数永不超 MaxRPM。
	WindowOffset time.Duration
	WindowGuard  time.Duration
}

// newRateGate 创建速率闸门；MaxRPM<=0 时只有冷却闩生效，不做窗口限速。
// statePath 非空时恢复未过期的冷却闩。
func newRateGate(params GateConfig, statePath string) *rateGate {
	gate := &rateGate{
		statePath: statePath,
		now:       time.Now,
	}
	gate.setParams(params)
	gate.restoreState()
	return gate
}

// setParams 原位更新闸门参数（reload 热路径）：闩态保留，窗口参数变化
// 后下一次 wait/stats 按新边界重算当前桶，桶起点不同即开新桶重新计数。
func (gate *rateGate) setParams(params GateConfig) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.maxHold = gateDurationOrDefault(params.MaxHold, gateDefaultMaxHold)
	gate.dripInterval = gateDurationOrDefault(params.DripInterval, gateDefaultDripInterval)
	gate.defaultLatch = gateDurationOrDefault(params.DefaultLatch, gateDefaultLatch)
	offset := params.WindowOffset % windowPeriod
	if offset < 0 {
		offset += windowPeriod
	}
	guard := params.WindowGuard
	if guard <= 0 || 2*guard >= windowPeriod {
		guard = gateDefaultWindowGuard
	}
	gate.quota = params.MaxRPM
	gate.windowOpen = (offset + guard) % windowPeriod
	gate.usable = windowPeriod - 2*guard
}

// gateDurationOrDefault 把 <=0 的时长参数回落到默认值。
func gateDurationOrDefault(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// windowStart 返回 t 所属计数桶的窗口起点：t 所在分钟内最近的
// windowOpen 边界；t 落在边界前则归上一分钟的边界。
func (gate *rateGate) windowStart(t time.Time) time.Time {
	start := t.Truncate(windowPeriod).Add(gate.windowOpen)
	if t.Before(start) {
		start = start.Add(-windowPeriod)
	}
	return start
}

// restoreState 在启动时恢复未过期的冷却闩：滴灌时钟按间隔重排。
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
	if err := json.Unmarshal(data, &state); err != nil || !state.LimitedUntil.After(gate.now()) {
		_ = os.Remove(gate.statePath)
		return
	}
	gate.limitedUntil = state.LimitedUntil
	gate.nextDrip = gate.now().Add(gate.dripInterval)
	gate.pushEvent(gateEventRestored, state.LimitedUntil, "")
	slog.Warn("rate gate latch restored from state file", "until", state.LimitedUntil.Format(time.RFC3339))
}

// persistState 把冷却闩截止时刻原子落盘（tmp+rename）；写失败只记
// 日志——落盘是防重启续限的保险，不挡请求路径。
func (gate *rateGate) persistState(until time.Time) {
	if gate.statePath == "" {
		return
	}
	data, _ := json.Marshal(gateStateFile{LimitedUntil: until})
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

// stats 返回闸门状态快照。顺带惰性结算到期闩与滚动桶：wait 只在有
// 流量时触发，无流量时段的闩到期与窗口翻转由这里的轮询补记，面板
// 时间线才闭环。
func (gate *rateGate) stats() GateStats {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	now := gate.now()
	gate.expireIfDue(now)
	ws := gate.windowStart(now)
	if !ws.Equal(gate.bucketStart) {
		gate.bucketStart = ws
		gate.bucketUsed = 0
	}
	stats := GateStats{
		Latched:       !gate.limitedUntil.IsZero() && now.Before(gate.limitedUntil),
		LatchCount:    gate.latchCount,
		DripCount:     gate.dripCount,
		RejectLatched: gate.rejectLatched,
		RejectHold:    gate.rejectHold,
		WindowQuota:   gate.quota,
		WindowUsed:    gate.bucketUsed,
		Sendable:      now.Sub(ws) < gate.usable,
		Waiters:       gate.waiters,
	}
	if gate.quota > 0 {
		open := ws
		next := ws.Add(windowPeriod)
		stats.WindowOpen = &open
		stats.WindowNext = &next
	}
	for i := 1; i <= gate.eventSize; i++ {
		stats.Events = append(stats.Events, gate.events[(gate.eventHead-i+gateEventCap)%gateEventCap])
	}
	if !gate.limitedUntil.IsZero() {
		until := gate.limitedUntil
		stats.LimitedUntil = &until
	}
	stats.LatchRanges = gate.latchRanges(now)
	return stats
}

// latchRanges 按事件时间序还原闩时段：latched/restored 开窗，released
// 提前关窗，expired 按截止关窗；延闩（latched 落在开窗内）只推进右端。
// 仍在闩中的时段收到 now；当前闩的开窗事件滚出环外时给 nil Start。
// 调用方须持 mu。
func (gate *rateGate) latchRanges(now time.Time) []GateLatchRange {
	if gate.eventSize == 0 {
		return nil
	}
	var ranges []GateLatchRange
	var open *GateLatchRange
	closeOpen := func(end time.Time) {
		if open != nil {
			open.End = end
			ranges = append(ranges, *open)
			open = nil
		}
	}
	// 事件环按写入序（旧到新）重放——stats.Events 的新在前序是展示序。
	for i := gate.eventSize; i >= 1; i-- {
		ev := gate.events[(gate.eventHead-i+gateEventCap)%gateEventCap]
		until := ev.At
		if ev.Until != nil {
			until = *ev.Until
		}
		switch ev.Kind {
		case gateEventLatched, gateEventRestored:
			// 开窗事件晚于当前窗右端：上一闩其实已自然失效（expired
			// 可能滚出环外），先闭旧窗再开新窗。
			if open != nil && ev.At.After(open.End) {
				closeOpen(open.End)
			}
			if open == nil {
				start := ev.At
				open = &GateLatchRange{Start: &start, End: until}
			} else if until.After(open.End) {
				open.End = until
			}
		case gateEventReleased:
			closeOpen(ev.At)
		case gateEventExpired:
			closeOpen(until)
		}
	}
	if open != nil {
		end := open.End
		if now.Before(end) {
			end = now
		}
		closeOpen(end)
	} else if stats_latched := !gate.limitedUntil.IsZero() && now.Before(gate.limitedUntil); stats_latched {
		// 当前闩的开窗事件已滚出环外：左端不可考，给 nil Start。
		ranges = append(ranges, GateLatchRange{End: now})
	}
	return ranges
}

// wait 阻塞到本次上游发送拿到许可，或判定不值得等：
//   - 闩内：滴灌槽空闲且在可发区间内立即放行（该请求即探针，计入
//     本桶配额）；否则直接返回闸门拒绝（*llm.Failure，LocalGate），
//     retryAfter 报闩剩余——客户端睡到恢复时刻重试比按槽位节奏轮询
//     更省重试预算；
//   - 闩外：可发区间内配额未满立即放行；配额耗尽或在死区内睡到
//     下一窗口开放，预计等待超出剩余预算（累计上限 maxHold）同样
//     返回闸门拒绝；
//   - 睡眠不做配额预约：窗口开放时睡醒者与新到者一起竞争，抢不到
//     的看到满桶按剩余预算决定再睡或快败——分钟粒度下排序公平性
//     不值得换复杂度。睡醒后不直接放行，回到循环首重新评估——
//     睡眠期间闩态可能已变。
func (gate *rateGate) wait(ctx context.Context) error {
	if gate == nil {
		return nil
	}
	sleeping := false // 标记本请求占着一个 waiters 名额
	// 等待预算约束「累计等待」而非「单次睡眠」：睡醒后要重新抢配额，
	// maxHold 超过一个窗口周期时逐睡校验会放行多轮睡眠，累计等待
	// 膨胀到 ~maxHold+60s——预算从进入起算，预计等待超出剩余额度
	// 即快败。maxHold 须在锁内读（setParams 热更新），deadline 因此
	// 惰性到首个持锁循环才落定。
	var deadline time.Time
	for {
		gate.mu.Lock()
		if sleeping {
			gate.waiters--
			sleeping = false
		}
		// 睡醒与 ctx 取消可能同时就绪（select 二选一随机）：回环首
		// 复查一次，避免取消请求在计时器侥幸先触发时仍被放行计数。
		if err := ctx.Err(); err != nil {
			gate.mu.Unlock()
			return context.Cause(ctx)
		}
		now := gate.now()
		if deadline.IsZero() {
			deadline = now.Add(gate.maxHold)
		}
		// 闩到期是自然失效而非解闩（没有成功帧证据）。
		gate.expireIfDue(now)
		// 计数桶随窗口边界滚动：过期桶的用量不结转。
		ws := gate.windowStart(now)
		if !ws.Equal(gate.bucketStart) {
			gate.bucketStart = ws
			gate.bucketUsed = 0
		}
		sendable := now.Sub(ws) < gate.usable
		if now.Before(gate.limitedUntil) {
			if sendable && !now.Before(gate.nextDrip) {
				// 探针槽空闲且在可发区间：放行并推进下一个槽。死区内
				// 不放探针——桶界附近的探针可能落进相邻真实桶白送计数。
				gate.nextDrip = now.Add(gate.dripInterval)
				gate.dripCount++
				gate.bucketUsed++
				gate.mu.Unlock()
				return nil
			}
			retryAfter := gate.limitedUntil.Sub(now)
			gate.rejectLatched++
			gate.mu.Unlock()
			return gateRejection(retryAfter)
		}
		if gate.quota <= 0 {
			gate.mu.Unlock()
			return nil
		}
		if sendable && gate.bucketUsed < gate.quota {
			gate.bucketUsed++
			gate.mu.Unlock()
			return nil
		}
		wait := ws.Add(windowPeriod).Sub(now)
		if now.Add(wait).After(deadline) {
			gate.rejectHold++
			gate.mu.Unlock()
			return gateRejection(wait)
		}
		gate.waiters++
		sleeping = true
		gate.mu.Unlock()
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			gate.mu.Lock()
			gate.waiters--
			gate.mu.Unlock()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// tryAdmit 给后台流量（前缀保温 ping）一条不排队、不偷槽的准入路径：
// 闩内一律拒绝（不占滴灌探针槽——冷却期恰是最不该打上游的时刻）；
// 闩外仅当当前处于可发区间、本桶配额未满且没有排队等待者时放行并计
// 入桶计数（排队者优先——ping 与睡醒者同权抢配额会让整形形同虚设）。
// 与 wait 的区别：不睡眠、不预约、不产事件；被拒调用方跳过本轮即可。
func (gate *rateGate) tryAdmit() bool {
	if gate == nil {
		return true
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	now := gate.now()
	gate.expireIfDue(now)
	// 计数桶随窗口边界滚动，与 wait 同一本账。
	ws := gate.windowStart(now)
	if !ws.Equal(gate.bucketStart) {
		gate.bucketStart = ws
		gate.bucketUsed = 0
	}
	if now.Before(gate.limitedUntil) {
		return false
	}
	if gate.quota <= 0 {
		return true
	}
	if now.Sub(ws) < gate.usable && gate.bucketUsed < gate.quota && gate.waiters == 0 {
		gate.bucketUsed++
		return true
	}
	return false
}

// noteUpstreamError 用上游失败刷新冷却闩；只有 resource_exhausted 与
// 限流有关，其余错误原样忽略。闩只延长不提前；只有闩被延长时才重置
// 滴灌时钟——截止未变的重复拒绝说明窗口未过，原探测节奏仍然成立，
// 重排滴灌只会无谓推迟下一枚探针。
func (gate *rateGate) noteUpstreamError(err error) {
	if gate == nil {
		return
	}
	failure := llm.Classify(err)
	// 本地闸门自己的拒绝（LocalGate）不带上游证据，不能拿来上闩。
	if failure == nil || failure.LocalGate || !failure.RateLimited {
		return
	}
	now := gate.now()
	// defaultLatch 由 setParams 热更新，须在锁内读。
	var until time.Time
	if resetAt, ok := failure.RateLimitReset(now); ok {
		until = resetAt
	}
	gate.mu.Lock()
	if until.IsZero() {
		until = now.Add(gate.defaultLatch)
	}
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
		gate.pushEvent(gateEventLatched, until, detail)
		// 落盘须在锁内：解锁后 persist 可能与并发 release 的 clearState
		// 交错——clear 先跑、persist 后写，已解闩的截止时刻会作为
		// 死文件残留，重启后复活成幽灵闩。
		gate.persistState(until)
	}
	gate.mu.Unlock()
	if extended {
		slog.Warn("upstream message rate limited; drip-latching new requests", "until", until.Format(time.RFC3339), "latch", until.Sub(now))
	} else {
		slog.Info("upstream message rate limited while latched", "remaining", remaining)
	}
}

// noteUpstreamSuccess 用任一上游数据帧解除冷却闩：收到数据帧说明该次
// 发送已越过上游准入（边际态下拒绝是概率执行），继续闩到声明时刻只会
// 浪费滴灌窗口。若该次发送随后以限流错误收尾，noteUpstreamError 会重新
// 上闩——两段判定间存在亚毫秒解闩窗，至多漏放一枚等待中的请求，代价
// 与一枚滴灌探针同价，可接受。解闩后放行仍受窗口配额约束——剩余配额
// 是窗口内齐射的天然上限。
func (gate *rateGate) noteUpstreamSuccess() {
	if gate == nil {
		return
	}
	gate.mu.Lock()
	latched := !gate.limitedUntil.IsZero()
	if latched {
		gate.pushEvent(gateEventReleased, gate.limitedUntil, "")
		gate.limitedUntil = time.Time{}
		gate.nextDrip = time.Time{}
		// clear 与上闩方的 persist 同锁序化：锁外执行会让「persist 晚于
		// clear」交错把已解闩的时刻写回状态文件。
		gate.clearState()
	}
	gate.mu.Unlock()
	if latched {
		slog.Info("rate gate released: upstream accepted a message")
	}
}

// gateRejection 是本地闸门拒绝的分类记录：Code 沿用上游限流方言
// resource_exhausted 让下游自然译出 429，LocalGate 标记未触达上游
// （排障归因 rate_gate），RetryAfterSeconds 直接带精确等待秒数——
// 不再靠伪造 "reset in N seconds" 文案让下游重解析。Message 保留
// 同一句式，客户端与日志看到的文案不变。
func gateRejection(retryAfter time.Duration) *llm.Failure {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	return &llm.Failure{
		Code:              "resource_exhausted",
		Message:           fmt.Sprintf("upstream message rate limited by local gate; your limit will reset in %d seconds.", seconds),
		LocalGate:         true,
		RetryAfterSeconds: seconds,
	}
}
