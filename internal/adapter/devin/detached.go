// 本文件实现「脱钩流 + 完成缓存」：已产出内容的上游流在客户端断开时
// 不取消，登记进进程内缓存由后台泵续命并缓冲全部已产出事件；语义等
// 价的重试（同键）直接重放缓冲——completed 秒回全量、running 重放
// 前缀后追帧、failed 按失败可重放性决定重放终态或当未命中走新上游。
//
// 动机：上游在工具调用参数阶段可静默计算 15-25min 只发心跳帧，客户端
// 300s 无字节即断连；旧实现断连即 cancel，服务端已算好的 args 全丢，
// 且半截工具调用无法续传（tryResume 拒收在飞 call），每次重试从零开始。
package devin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
)

// detachedMaxEntries 是完成缓存的容量上限：单条缓冲最坏 ~MB 级，
// 缓存定位是「断开重试接住」的兜底而非常驻存储，容量宁小勿大。
const detachedMaxEntries = 8

// 条目按态的存活窗口（var 供测试缩短）：running 覆盖上游长静默计算的
// 实测上限（15-25min args 静默 + 续传余量），也是后台泵的存活上界；
// completed 按客户端重试窗口收口——缓存无法区分「同键重试」与「同 body
// 新请求」，定时任务/探针的同 prompt 请求不得吃远超重试窗口的陈旧响应，
// 15min 是重试链（300s 断连 + 退避多次）的宽松上界；failed 只留短窗
// 回答「同键重试吃缓存终态还是走新上游」。
var (
	detachedRunningTTL   = 45 * time.Minute
	detachedCompletedTTL = 15 * time.Minute
	detachedFailedTTL    = 5 * time.Minute
	// detachedMaxBufferedBytes 是单条目的事件缓冲字节预算（var 供测试
	// 缩小）：生产 04 原始帧逐 dir 求和的 p99 ~100KiB、观测最大 ~400KiB，
	// 8MiB 对合法流留 ~20x 余量，同时把「flood 上游全速 drain 进缓存」
	// 的实测 ~133MB/条目钉死在预算内——最坏驻留 8 条目 x 8MiB x lane 数。
	detachedMaxBufferedBytes = 8 << 20
)

// detachedState 是条目生命周期：running 后台泵仍在喂；completed/failed
// 是终态，重放后不再变化。
type detachedState int

const (
	detachedRunning detachedState = iota
	detachedCompleted
	detachedFailed
)

// String 给 04 标记行一个可读的态名。
func (state detachedState) String() string {
	switch state {
	case detachedCompleted:
		return "completed"
	case detachedFailed:
		return "failed"
	default:
		return "running"
	}
}

// detachedEntry 是一条脱钩流的事件缓冲与终态：事件由 responseStream 的
// 返回点 tee 逐条追加（含 detach 前已下发给原客户端的前缀——重试需要
// 完整事件序列），挂接方经 attachStream 按下标顺序重放/追帧。
type detachedEntry struct {
	mu     sync.Mutex
	events []llm.ResponseEvent
	state  detachedState
	// notify 是「有新事件/已终态」的广播钟：每次 append/finish 关闭
	// 换新，全部等待中的挂接方被唤醒后重走 poll。
	notify chan struct{}
	// replayable 仅 failed 态有意义：终局错误是否值得原样重放——
	// 传输断裂/取消类瞬态失败的同键重试应走新上游而非吃缓存终态。
	replayable bool
	// originDir 是首请求的调试目录名：挂接请求的 04 标记行回指它，
	// 重放事件的原始帧取证都在首请求目录里。
	originDir string
	// admittedAt/expiresAt 由 registry.admit 与 finish 按态写定。
	admittedAt time.Time
	expiresAt  time.Time
	// drainCancel 是后台泵 ctx 的取消柄：running 条目被淘汰时掐它让
	// 泵自行退场（TTL 兜底之外的容量淘汰路径）。一次性写入的
	// CancelFunc 值调用永远安全——淘汰路径持 registry.mu 不能去拿
	// 流锁（锁序：stream.mu > registry.mu > entry.mu）。
	drainCancel context.CancelFunc
	// attached 记本条目是否兑现过一次挂接（lookup 命中时置位）：
	// 移除路径据此算孤儿——从未被挂接的条目是纯粹的上游浪费。
	attached bool
	// detachIndex 是登记时刻已缓冲的事件数（客户端断连前的前缀）：
	// len(events)-detachIndex = 脱钩后新产出的事件量，孤儿条目的
	// 这项和是最接近「token 级浪费」的可用代理（真实 token 数只在
	// 内存事件载荷里，计数层拿不到）。
	detachIndex int
	// bufferedBytes/truncated 是字节预算簿记：append 按 detachedEventBytes
	// 估值累计，越 detachedMaxBufferedBytes 置 truncated 并在尾部补一条
	// 截断错误事件后冻结缓冲。截断缓冲前缀残缺、永远产不出完整重放：
	// lookup 对它一律回未命中（同键重试走新上游），在飞挂接方重放到
	// 这条显式错误而非无声 EOF——截断响应不得以 completed 形态下发。
	bufferedBytes int
	truncated     bool
}

// append 追加一条已产出事件并广播给挂接方；返回 true 表示本次追加越过
// 字节预算、缓冲就此截断（调用方记 04 标记行与停泵用）。截断后追加是
// 空操作——冻结保证截断错误事件恒为末帧。
func (entry *detachedEntry) append(event llm.ResponseEvent) bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if entry.truncated {
		return false
	}
	entry.events = append(entry.events, event)
	entry.bufferedBytes += detachedEventBytes(event)
	if entry.bufferedBytes <= detachedMaxBufferedBytes {
		close(entry.notify)
		entry.notify = make(chan struct{})
		return false
	}
	entry.truncated = true
	entry.events = append(entry.events, detachedTruncatedEvent(entry.bufferedBytes))
	close(entry.notify)
	entry.notify = make(chan struct{})
	return true
}

// isTruncated 报告缓冲是否已因字节预算截断。
func (entry *detachedEntry) isTruncated() bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.truncated
}

// detachedTruncatedEvent 合成缓冲截断的终止事件：UpstreamFault 让
// finish 把它收成不可重放的 failed（语义上「不要吃这条缓存终态」），
// 在飞挂接方把它当一次显式流失败下发，客户端重试自然走向新上游。
func detachedTruncatedEvent(bufferedBytes int) llm.ResponseEvent {
	return llm.ResponseEvent{
		Type:   llm.ResponseEventError,
		Reason: llm.StopReasonError,
		Error: &llm.AssistantMessage{
			ErrorMessage: fmt.Sprintf("detached buffer truncated: %d bytes exceeded %d byte budget", bufferedBytes, detachedMaxBufferedBytes),
			Failure:      &llm.Failure{Code: "internal", UpstreamFault: true},
		},
	}
}

// detachedEventBytes 估计单条事件在缓冲里的驻留字节：显性载荷取
// Delta/Content 与终态对象（ToolCall/ServerResult/Message/Error）的
// 字符串面值，另加每事件固定开销摊事件体与 Partial 快照的切片克隆。
// Partial 不深计——快照只克隆切片头，块字符串与既发事件共享底层字节，
// 深计会把同一份正文按事件数重复入账（真实驻留是线性而非平方）；
// 终态 Message 与既发块同样共享字节，面值计入属保守上偏，可接受。
func detachedEventBytes(event llm.ResponseEvent) int {
	size := 192 + len(event.Delta) + len(event.Content) + len(event.ToolCallID) + len(event.ToolName)
	if event.ToolCall != nil {
		size += len(event.ToolCall.ID) + len(event.ToolCall.Name) + len(event.ToolCall.Arguments)
	}
	if event.ServerResult != nil {
		size += detachedServerResultBytes(event.ServerResult)
	}
	for _, message := range []*llm.AssistantMessage{event.Message, event.Error} {
		if message == nil {
			continue
		}
		size += len(message.ErrorMessage) + len(message.DebugRef)
		for _, block := range message.Content {
			size += detachedContentBytes(block)
		}
	}
	return size
}

// detachedContentBytes 估计单个内容块的驻留字节。
func detachedContentBytes(block llm.Content) int {
	switch content := block.(type) {
	case llm.TextContent:
		return len(content.Text)
	case llm.ThinkingContent:
		return len(content.Thinking) + len(content.ThinkingSignature)
	case llm.ImageContent:
		return len(content.Data)
	case llm.ToolCall:
		return len(content.ID) + len(content.Name) + len(content.Arguments)
	case llm.ServerToolResult:
		return detachedServerResultBytes(&content)
	}
	return 0
}

// detachedServerResultBytes 估计托管工具结果的驻留字节。
func detachedServerResultBytes(result *llm.ServerToolResult) int {
	size := len(result.ToolCallID) + len(result.ToolName) + len(result.Text) + len(result.ErrorCode)
	for _, hit := range result.Results {
		size += len(hit.Title) + len(hit.URL) + len(hit.Summary)
	}
	return size
}

// finish 在后台泵读到流终态时定态：末帧是 error 记 failed（可重放性按
// 失败分类——上游责任/取消类瞬态失败的同键重试应走新上游而非吃缓存
// 终态），否则 completed；expiresAt 按态重置。返回定态结果供调用方
// 归因记账（泵的终局计数按它与 ctx 错误联合分类）。
func (entry *detachedEntry) finish() detachedState {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if n := len(entry.events); n > 0 && entry.events[n-1].Type == llm.ResponseEventError {
		entry.state = detachedFailed
		failure := llm.FailureOf(entry.events[n-1].Error)
		entry.replayable = failure == nil || (!failure.UpstreamFault && !failure.Canceled)
		entry.expiresAt = time.Now().Add(detachedFailedTTL)
	} else {
		entry.state = detachedCompleted
		entry.replayable = true
		entry.expiresAt = time.Now().Add(detachedCompletedTTL)
	}
	close(entry.notify)
	entry.notify = make(chan struct{})
	return entry.state
}

// poll 供挂接方按下标取下一事件：有则回事件（ok）；缓冲读空且已终态
// 回 done；running 空转时回 notify 供调用方 select 等待。
func (entry *detachedEntry) poll(cursor int) (event llm.ResponseEvent, ok, done bool, notify chan struct{}) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	if cursor < len(entry.events) {
		return entry.events[cursor], true, false, nil
	}
	if entry.state != detachedRunning {
		return llm.ResponseEvent{}, false, true, nil
	}
	return llm.ResponseEvent{}, false, false, entry.notify
}

// len 返回当前已缓冲事件数（04 标记行与测试用）。
func (entry *detachedEntry) len() int {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return len(entry.events)
}

// detachedRegistry 是进程内完成缓存：键是语义请求哈希，值是脱钩条目。
// 命中即重放/挂接，未命中走正常上游请求。容量触顶先逐过期再逐最老
// running——完成态条目兑现「重试秒回」的价值更高，且运行态的客户端
// 多半等不起会先断。
type detachedRegistry struct {
	mu      sync.Mutex
	entries map[string]*detachedEntry
	// 计数与事件环全在 mu 下读写：04 标记行只记 detach/attach 两个
	// 登记时刻，泵终局、移除原因与孤儿浪费没有其它观测面——计数器
	// 回答「缓存兑现了几次救援、烧了多少无人认领的上游算力」。
	detaches          int64 // 累计登记（后台泵启动数）
	attaches          int64 // 累计挂接命中
	attachMisses      int64 // 同键请求到场但条目不可用（过期/不可重放）
	finishedCompleted int64 // 泵终局 completed（上游 EOF 干净收尾）
	finishedFailed    int64 // 泵终局 failed（上游错误尾帧/泵内异常）
	finishedKilled    int64 // 泵被 registry 淘汰掐死（drainCancel）
	finishedExpired   int64 // 泵撞 running TTL（drainCtx 到期）
	expired           int64 // TTL 到期移除（lookup 惰性逐出 + admit 扫描）
	evicted           int64 // 容量淘汰（最老 running 让位）
	replaced          int64 // 同键新条目替换旧残骸
	truncated         int64 // 字节预算截断逐出（lookup 惰性逐出）
	orphans           int64 // 移除时从未挂接（全部态）——「脱钩但无消费者」
	orphanCompleted   int64 // 其中 completed：上游算完无人接，最纯的浪费
	orphanBuffered    int64 // 孤儿条目脱钩后新产出的事件量合计（浪费量级代理）
	events            [detachedEventCap]DetachedEvent
	eventHead         int
	eventSize         int
}

// detachedEventCap 是缓存事件环容量：脱钩/挂接/终局/移除低频，
// 64 条足够回看一整天的生命周期轨迹（与 gateEventCap 同档位）。
const detachedEventCap = 64

// 生命周期事件种类：admit（登记）、attach（挂接命中）、miss（同键
// 到场但条目不可用）、evict（移除，detail 记原因）、finish（泵终局，
// detail 记四档终态）。
const (
	detachedEventAdmit  = "admit"
	detachedEventAttach = "attach"
	detachedEventMiss   = "miss"
	detachedEventEvict  = "evict"
	detachedEventFinish = "finish"
)

// 移除原因（evict 事件的 detail）：expired 是 TTL 到点（lookup 惰性
// 逐出与 admit 扫描同口径），capacity 是容量淘汰最老 running，
// replaced 是同键新条目逐出旧残骸，truncated 是字节预算截断
// （lookup 惰性逐出——截断缓冲无重放价值即无存活依据）。
const (
	detachEvictExpired   = "expired"
	detachEvictCapacity  = "capacity"
	detachEvictReplaced  = "replaced"
	detachEvictTruncated = "truncated"
)

// 泵终局原因（finish 事件的 detail 与 finished_* 计数桶）：completed/
// failed 是 finish 的尾帧定态，killed 是 registry 淘汰掐泵
// （drainCancel），ttl_expired 是 drainCtx 的 running TTL 到期。
const (
	detachFinishCompleted = "completed"
	detachFinishFailed    = "failed"
	detachFinishKilled    = "killed"
	detachFinishExpired   = "ttl_expired"
)

// DetachedEvent 是一次缓存生命周期采样。key 只留语义键前 12 位（与 04
// 标记行的全量键前缀对照可认，全键只是噪音）；detail 携带各事件自己的
// 归因——evict 的移除原因、finish 的终态、attach/miss 的条目态。
// label 是产出时算好的面板显示名，与 GateEvent 同构。
type DetachedEvent struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Key    string    `json:"key,omitempty"`
	Detail string    `json:"detail,omitempty"`
	Label  string    `json:"label"`
}

// detachedEventLabel 是生命周期事件的面板显示名。
func detachedEventLabel(kind, detail string) string {
	switch kind {
	case detachedEventAdmit:
		return "登记"
	case detachedEventAttach:
		return "挂接"
	case detachedEventMiss:
		if detail == detachEvictExpired {
			return "过期未命中"
		}
		if detail == detachEvictTruncated {
			return "截断未命中"
		}
		return "不可重放"
	case detachedEventEvict:
		switch detail {
		case detachEvictCapacity:
			return "容量淘汰"
		case detachEvictReplaced:
			return "同键替换"
		case detachEvictTruncated:
			return "截断移除"
		}
		return "过期移除"
	case detachedEventFinish:
		switch detail {
		case detachFinishCompleted:
			return "泵完成"
		case detachFinishKilled:
			return "淘汰掐泵"
		case detachFinishExpired:
			return "泵超时"
		}
		return "泵失败"
	}
	return kind
}

// DetachedStats 是完成缓存快照，/admin/runtime-metrics 的 detached 组
// 透出。orphan_completed 是上游浪费的条目级口径（烧掉的 token 只在
// 内存缓冲里，盘上不可量——计数器是唯一观测面）。
type DetachedStats struct {
	Entries   int `json:"entries"`
	Running   int `json:"running"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`

	Detaches     int64 `json:"detaches"`
	Attaches     int64 `json:"attaches"`
	AttachMisses int64 `json:"attach_misses"`

	FinishedCompleted int64 `json:"finished_completed"`
	FinishedFailed    int64 `json:"finished_failed"`
	FinishedKilled    int64 `json:"finished_killed"`
	FinishedExpired   int64 `json:"finished_expired"`

	Expired  int64 `json:"expired"`
	Evicted  int64 `json:"evicted"`
	Replaced int64 `json:"replaced"`
	// Truncated 是字节预算截断的移除计数——flood/异常上游 drain 进
	// 缓存被预算拦下的信号，与 expired 桶分开才有观测意义。
	Truncated int64 `json:"truncated"`

	Orphans         int64 `json:"orphans"`
	OrphanCompleted int64 `json:"orphan_completed"`
	// OrphanBufferedEvents 是孤儿条目脱钩后新产出的事件量合计——token
	// 级浪费拿不到（真实 token 只在内存事件载荷里），事件量是最接近
	// 的量级代理：区分「登记即死的孤儿」与「跑了 20 分钟无人认领」。
	OrphanBufferedEvents int64 `json:"orphan_buffered_events"`

	Events []DetachedEvent `json:"events,omitempty"` // 新在前
}

// newDetachedRegistry 创建空缓存。
func newDetachedRegistry() *detachedRegistry {
	return &detachedRegistry{entries: make(map[string]*detachedEntry)}
}

// lookup 查可挂接条目：running/completed 恒可挂；failed 只在可重放时
// 挂（瞬态失败返回 nil 让调用方走新上游）；过期与截断即逐——截断缓冲
// 产不出完整重放，同键重试直接走新上游，同时把槽位与缓冲提前释放。
// 条目缺席是普通首发不计数；条目在场却不可用记 attach_miss——同键
// 重试确实来过。
func (registry *detachedRegistry) lookup(key string) *detachedEntry {
	if registry == nil || key == "" {
		return nil
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry := registry.entries[key]
	if entry == nil {
		return nil
	}
	entry.mu.Lock()
	expired := time.Now().After(entry.expiresAt)
	replayable := entry.state != detachedFailed || entry.replayable
	truncated := entry.truncated
	state := entry.state
	if !expired && !truncated && replayable {
		entry.attached = true
	}
	entry.mu.Unlock()
	if expired || truncated {
		cause := detachEvictExpired
		if !expired {
			cause = detachEvictTruncated
		}
		registry.attachMisses++
		registry.pushEvent(detachedEventMiss, key, cause)
		registry.evictLocked(key, entry, cause)
		return nil
	}
	if !replayable {
		registry.attachMisses++
		registry.pushEvent(detachedEventMiss, key, "unreplayable")
		return nil
	}
	registry.attaches++
	registry.pushEvent(detachedEventAttach, key, state.String())
	return entry
}

// admit 把条目按 key 登记进缓存并接管其后台泵的生命周期。容量触顶先
// 扫过期项，再逐最老的 running 条目。同键旧条目（前一次同请求脱钩的
// 残骸）先逐出再登记——两条同键后台泵同跑是纯粹的配额浪费。
func (registry *detachedRegistry) admit(key string, entry *detachedEntry) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry.mu.Lock()
	entry.admittedAt = time.Now()
	entry.expiresAt = entry.admittedAt.Add(detachedRunningTTL)
	entry.detachIndex = len(entry.events)
	entry.mu.Unlock()
	if old := registry.entries[key]; old != nil {
		registry.evictLocked(key, old, detachEvictReplaced)
	}
	if len(registry.entries) >= detachedMaxEntries {
		now := time.Now()
		for k, e := range registry.entries {
			e.mu.Lock()
			expired := now.After(e.expiresAt)
			e.mu.Unlock()
			if expired {
				registry.evictLocked(k, e, detachEvictExpired)
			}
		}
	}
	if len(registry.entries) >= detachedMaxEntries {
		var oldestKey string
		var oldestAt time.Time
		for k, e := range registry.entries {
			e.mu.Lock()
			running := e.state == detachedRunning
			admitted := e.admittedAt
			e.mu.Unlock()
			if running && (oldestKey == "" || admitted.Before(oldestAt)) {
				oldestKey, oldestAt = k, admitted
			}
		}
		if oldestKey != "" {
			registry.evictLocked(oldestKey, registry.entries[oldestKey], detachEvictCapacity)
		}
	}
	registry.entries[key] = entry
	registry.detaches++
	registry.pushEvent(detachedEventAdmit, key, "")
}

// evictLocked 摘出条目：running 条目同时掐后台泵的 drain ctx——泵的
// Recv 走 ctx.Done 退场并把条目收成 failed（挂接方拿到一个截断但干净
// 的终态）。掐的是一次性写入的 CancelFunc 而非直接 cancel 流：本函数
// 持 registry.mu 不能去拿流锁。全部移除走这一个漏斗：按 cause 记移除
// 计数，从未挂接的条目同时记孤儿（orphan_completed 是上游浪费口径）。
func (registry *detachedRegistry) evictLocked(key string, entry *detachedEntry, cause string) {
	delete(registry.entries, key)
	switch cause {
	case detachEvictCapacity:
		registry.evicted++
	case detachEvictReplaced:
		registry.replaced++
	case detachEvictTruncated:
		registry.truncated++
	default:
		registry.expired++
	}
	entry.mu.Lock()
	drainCancel := entry.drainCancel
	running := entry.state == detachedRunning
	orphan := !entry.attached
	completed := entry.state == detachedCompleted
	bufferedAfterDetach := len(entry.events) - entry.detachIndex
	entry.mu.Unlock()
	if orphan {
		registry.orphans++
		registry.orphanBuffered += int64(bufferedAfterDetach)
		if completed {
			registry.orphanCompleted++
		}
	}
	registry.pushEvent(detachedEventEvict, key, cause)
	if running && drainCancel != nil {
		drainCancel()
	}
}

// noteFinish 记一次后台泵终局：被掐死的泵在条目移出后仍会走到这里，
// 计数按原因四档（detachFinish*）——orphan 口径量「有没有人接」，
// finish 口径量「泵怎么死的」，两维独立。
func (registry *detachedRegistry) noteFinish(key, reason string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	switch reason {
	case detachFinishCompleted:
		registry.finishedCompleted++
	case detachFinishKilled:
		registry.finishedKilled++
	case detachFinishExpired:
		registry.finishedExpired++
	default:
		registry.finishedFailed++
	}
	registry.pushEvent(detachedEventFinish, key, reason)
}

// pushEvent 追加一条生命周期事件；调用方须持 mu。key 截前 12 位——
// 与 04 标记行的全量键前缀对照可认，全键写进快照只是噪音。
func (registry *detachedRegistry) pushEvent(kind, key, detail string) {
	if len(key) > 12 {
		key = key[:12]
	}
	registry.events[registry.eventHead] = DetachedEvent{
		At:     time.Now(),
		Kind:   kind,
		Key:    key,
		Detail: detail,
		Label:  detachedEventLabel(kind, detail),
	}
	registry.eventHead = (registry.eventHead + 1) % detachedEventCap
	if registry.eventSize < detachedEventCap {
		registry.eventSize++
	}
}

// stats 返回完成缓存快照：在场条目按态分解 + 累计计数 + 事件环
// （新在前）。每请求一次的轮询成本是 8 条上限的线性扫。
func (registry *detachedRegistry) stats() DetachedStats {
	stats := DetachedStats{}
	if registry == nil {
		return stats
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	stats.Entries = len(registry.entries)
	for _, entry := range registry.entries {
		entry.mu.Lock()
		switch entry.state {
		case detachedCompleted:
			stats.Completed++
		case detachedFailed:
			stats.Failed++
		default:
			stats.Running++
		}
		entry.mu.Unlock()
	}
	stats.Detaches = registry.detaches
	stats.Attaches = registry.attaches
	stats.AttachMisses = registry.attachMisses
	stats.FinishedCompleted = registry.finishedCompleted
	stats.FinishedFailed = registry.finishedFailed
	stats.FinishedKilled = registry.finishedKilled
	stats.FinishedExpired = registry.finishedExpired
	stats.Expired = registry.expired
	stats.Evicted = registry.evicted
	stats.Replaced = registry.replaced
	stats.Truncated = registry.truncated
	stats.Orphans = registry.orphans
	stats.OrphanCompleted = registry.orphanCompleted
	stats.OrphanBufferedEvents = registry.orphanBuffered
	for i := 1; i <= registry.eventSize; i++ {
		stats.Events = append(stats.Events, registry.events[(registry.eventHead-i+detachedEventCap)%detachedEventCap])
	}
	return stats
}

// attachStream 是挂接到脱钩条目的只读视图：先重放已缓冲事件，
// running 态下按下标追新事件，条目终态且读空后回 io.EOF。
// 条目不因挂接方断开而停泵——断开的挂接方与最初断开的客户端同语义。
type attachStream struct {
	entry  *detachedEntry
	cursor int
}

var _ llm.ResponseStream = (*attachStream)(nil)

// Recv 返回缓冲序列的下一事件；终态读空回 io.EOF；ctx 取消原样返回。
func (s *attachStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	for {
		event, ok, done, notify := s.entry.poll(s.cursor)
		if ok {
			s.cursor++
			return event, nil
		}
		if done {
			return llm.ResponseEvent{}, io.EOF
		}
		select {
		case <-notify:
		case <-ctx.Done():
			return llm.ResponseEvent{}, context.Cause(ctx)
		}
	}
}

// detachedRequestKey 返回请求的语义等价键：哈希输入是 02 投影（与
// 调试日志同一份规范化中间表示），做三类修正后落键——
//  1. 审计位剔除：timestamp_ms 是解码时刻的墙上时钟不进 wire，逐条
//     消息删除（不剔则同 body 的每次重试重新解码打新时间戳，键恒
//     miss——decode→key 链路上没有归一点）；dropped_items 是解码降级
//     痕迹，其中带 wire 语义的 marker 以规范序经 seed_markers 回键。
//  2. 身份与会话：key_hash 隔离租户——同 body 的跨令牌请求不得共享
//     重放（usage 归属与上游轨迹隔离都靠它）；session_key 保留——
//     生产 02 证据表明同 body 重试的 session_key 恒定，纳回零挂接
//     成本换同租户跨会话隔离。
//  3. model 覆盖为别名/路由解析后的真实 uid——同一段提示词换个客户
//     端模型名殊途同归。
//
// tools 键面与 prefixwarm.putTool 的 wire 身份清单同集——透传位不同
// 的同名同 schema 工具走不同 wire 语义（custom 改参数编码、server
// 触发托管跳），不得共享重放。
// json.Marshal 对 map 键排序，投影值全是已规范化结构，哈希确定。
// 注意：键复用了为可观测性设计的调试投影，投影新增的审计位会静默
// 进键——新增消息字段时须复核本函数口径（fingerprintRequest 同纪律：
// 不进 wire 的字段不参与等价）。
func detachedRequestKey(request llm.RequestMessages, model string) string {
	projection := debuglog.RequestMessagesProjection(request)
	delete(projection, "dropped_items")
	projection["model"] = model
	for _, message := range projection["messages"].([]any) {
		delete(message.(map[string]any), "timestamp_ms")
	}
	tools := make([]any, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, map[string]any{
			"name":                    tool.Name,
			"description":             tool.Description,
			"input_schema":            tool.InputSchema,
			"custom":                  tool.Custom,
			"server":                  tool.Server,
			"strict":                  tool.Strict,
			"read_only_hint":          tool.ReadOnlyHint,
			"server_name":             tool.ServerName,
			"attribution_field_names": tool.AttributionFieldNames,
		})
	}
	projection["tools"] = tools
	if request.CallerKeyHash != "" {
		projection["key_hash"] = request.CallerKeyHash
	}
	if markers := seedMarkers(request.Dropped); len(markers) > 0 {
		projection["seed_markers"] = markers
	}
	// 投影不含的采样参数补进键面：top_p/top_k/seed 不同的同名请求
	// 语义上不该共享同一份重放。
	if request.TopP != nil {
		projection["top_p"] = *request.TopP
	}
	if request.TopK != nil {
		projection["top_k"] = *request.TopK
	}
	if request.Seed != nil {
		projection["seed"] = *request.Seed
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
