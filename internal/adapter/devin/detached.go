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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
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
	// sawCrossLaneRetry 记同键消费者曾到兄弟 lane 敲门（兄弟 lane
	// lookup 落 nil 后的 peek 探测置位）：移除时把孤儿拆成「没人来」
	// 与「来错门」两档——后者是选号让位/删绑的归因证据。
	sawCrossLaneRetry bool
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
		return len(content.Text) + len(content.Signature)
	case llm.ThinkingContent:
		return len(content.Thinking) + len(content.Signature)
	case llm.ImageContent:
		return len(content.Data) + len(content.Signature)
	case llm.ToolCall:
		return len(content.ID) + len(content.Name) + len(content.Arguments) + len(content.Signature)
	case llm.ServerToolResult:
		return detachedServerResultBytes(&content)
	}
	return 0
}

// detachedServerResultBytes 估计托管工具结果的驻留字节。
func detachedServerResultBytes(result *llm.ServerToolResult) int {
	size := len(result.ToolCallID) + len(result.ToolName) + len(result.ErrorCode)
	for _, block := range result.Content {
		size += detachedContentBytes(block)
	}
	for _, hit := range result.SearchResults {
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

// marker 取挂接标记（detached_attach）要用的字段快照：originDir/state
// 在脱钩登记与 finish 定态时都可能被并发写，收进同一把锁读。
func (entry *detachedEntry) marker() (originDir string, state detachedState, buffered int) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.originDir, entry.state, len(entry.events)
}

// detachedRegistry 是进程内完成缓存：键是语义请求哈希，值是脱钩条目。
// 命中即重放/挂接，未命中走正常上游请求。容量触顶的让位序是过期项 →
// 死尸体（截断/不可重放 failed）→ 最老 running → 最老终态兜底：尸体
// 缓冲已无重放价值却占槽，活泵不该给它让位；完成态条目兑现「重试秒回」
// 的价值更高，排在 running 之后只作兜底——缺它则全终态未过期时 map
// 会随 admit 速率×TTL 无界长大（软帽）。
type detachedRegistry struct {
	mu      sync.Mutex
	entries map[string]*detachedEntry
	// draining 由 Adapter.BeginDrain 置位（单调，不经 mu）：排空中的进程
	// 即将退出，此时登记的脱钩条目永远等不到挂接方——泵白烧上游算力到
	// 进程死。detachable() 读它拒收准入；置位后不再清零（排空不可逆）。
	draining atomic.Bool
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
	expired           int64 // 死条目惰性移除：TTL 到期 + 截断尸体经 lookup 逐出（截断发生数单列 truncated）
	evicted           int64 // 容量淘汰（尸体让位/最老 running/最老终态兜底）
	replaced          int64 // 同键新条目替换旧残骸
	aborted           int64 // 面板 abort 按来源目录清场（abort-after-detach 残留窗）
	closed            int64 // 登记表关停整体清场（进程退出路径——非零即关停发生）
	truncated         int64 // 缓冲越字节预算被冻结次数——append 截断点记账，与移除路径解耦
	orphans           int64 // 移除时从未挂接（全部态）——「脱钩但无消费者」
	orphanCompleted   int64 // 其中 completed：上游算完无人接，最纯的浪费
	orphanBuffered    int64 // 孤儿条目脱钩后新产出的事件量合计（浪费量级代理）
	orphansCrossLane  int64 // 孤儿中消费者来过但去了别门的（sawCrossLaneRetry）
	crossLaneMisses   int64 // 同键请求到本 lane 但条目在兄弟 lane 的探测命中
	// ledger/lane 是 detached_events 台账的写出口与归属维：事件环只有
	// 64 条且随进程死蒸发，而脱钩事件的两侧请求目录都可能缺席（claim
	// 失败的重试零 payload、origin 标记行在 sqlite 争用中被丢）——台账
	// 是这类「零足迹」现场的兜底取证层。ledger 为 nil 时只走内存环。
	ledger *store.Store
	lane   string
	// ledgerDrops 记台账写丢失数（队满拒收/写失败/关停后到队）——台账
	// 存在的目的就是兜住争用期的丢痕迹，它自己丢了多少必须有数。
	ledgerDrops int64
	// seeded 记开机播种灌回的 completed 条目数；blobDrops 记种子层的
	// 自身丢失（编码失败/写库失败/坏行解码失败）——持久层的兑现率与
	// 折损率都从这俩数读，与台账 ledgerDrops 分开记账。
	seeded    int64
	blobDrops int64
	events    eventRing[DetachedEvent]
	// closing 由 close() 置位（mu 下）：登记表进入关停态，admit 拒收新
	// 条目、pushEvent 不再入队台账（直接记 drop）。draining 只拒新准入
	// 不掐在册泵；closing 是真清场。
	closing bool
	// ledgerQueue 三件套是 detached_events 的异步写管道：pushEvent 在
	// mu 内入队（FIFO 序=环序——原来 mu 内直写 sqlite 就是买这个序，
	// 代价是全部生命周期路径背着写连接的争用延迟），runLedgerPump 单
	// 写者消费，close() 关停后排队存量排空退出。管道永不关闭——关停
	// 信号走独立的 stop 通道，迟到入队只占缓冲不 panic。
	ledgerQueue chan store.DetachedEvent
	ledgerStop  chan struct{}
	ledgerDone  chan struct{}
}

// detachedEventCap 是缓存事件环容量：脱钩/挂接/终局/移除低频，
// 64 条足够回看一整天的生命周期轨迹（与 gateEventCap 同档位）。
const detachedEventCap = 64

// detachedLedgerQueueCap 是台账写管道深度：每条目一生数条事件、
// 上限 8 条目，256 队深远超真实突发；真溢出说明写面已整体卡死，
// 丢行（记 ledgerDrops）比反压生命周期路径正确。
// detachedLedgerDrainBudget 是 close() 排空台账存量的总预算——常态
// 近空，预算只兜「库已卡死」场景，写注定失败时关库不被整队重放拖住。
const (
	detachedLedgerQueueCap    = 256
	detachedLedgerDrainBudget = 5 * time.Second
)

// 生命周期事件种类：admit（登记）、attach（挂接命中）、miss（同键
// 到场但条目不可用）、cross_miss（同键请求到场但条目在兄弟 lane，
// detail 记 <owner>:<state>）、evict（移除，detail 记原因）、
// finish（泵终局，detail 记四档终态）、truncate（缓冲越预算冻结——
// 04 标记行是尽力而为，事件环给截断留权威痕迹）、seed（开机从
// detached_blobs 灌回的条目——与 admit 分列：它没有活泵，只是前
// 进程完成态的借尸还魂）。
const (
	detachedEventAdmit     = "admit"
	detachedEventAttach    = "attach"
	detachedEventMiss      = "miss"
	detachedEventCrossMiss = "cross_miss"
	detachedEventEvict     = "evict"
	detachedEventFinish    = "finish"
	detachedEventTruncate  = "truncate"
	detachedEventSeed      = "seed"
)

// 移除原因（evict 事件的 detail）：expired 是 TTL 到点（lookup 惰性
// 逐出与 admit 扫描同口径），capacity 是容量淘汰（尸体让位/最老
// running/最老终态兜底），replaced 是同键新条目逐出旧残骸，
// truncated 是截断尸体被惰性逐出——只作事件归因，截断发生数在
// 缓冲冻结时已由 noteTruncated 记过，移除不再重复入账；aborted 是
// 面板 abort 按来源目录清场（abort-after-detach 残留窗收口），
// closed 是登记表关停时的整体清场（进程退出路径）。
const (
	detachEvictExpired   = "expired"
	detachEvictCapacity  = "capacity"
	detachEvictReplaced  = "replaced"
	detachEvictTruncated = "truncated"
	detachEvictAborted   = "aborted"
	detachEvictClosed    = "closed"
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
	// Lane 只在聚合视图（顶层 detached 段归并环）由 mergeDetachedStats
	// 回填；per-lane 透出（accounts.<name>.detached）的身份由路径携带，
	// 本字段留空。
	Lane string `json:"lane,omitempty"`
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
	case detachedEventCrossMiss:
		return "跨号未命中"
	case detachedEventEvict:
		switch detail {
		case detachEvictCapacity:
			return "容量淘汰"
		case detachEvictReplaced:
			return "同键替换"
		case detachEvictTruncated:
			return "截断移除"
		case detachEvictAborted:
			return "中断移除"
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
	case detachedEventTruncate:
		return "缓冲截断"
	case detachedEventSeed:
		return "播种"
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
	// CrossLaneMisses 是「同键请求到本 lane、条目却在兄弟 lane」的探测
	// 命中计数——与 attach_misses（条目在场不可用）对称的缺失补全：
	// 跨 lane 重试此前在两侧都不可见（本 lane miss 不计、owner 只能等
	// 移除时记不区分原因的孤儿）。
	CrossLaneMisses int64 `json:"cross_lane_misses"`

	FinishedCompleted int64 `json:"finished_completed"`
	FinishedFailed    int64 `json:"finished_failed"`
	FinishedKilled    int64 `json:"finished_killed"`
	FinishedExpired   int64 `json:"finished_expired"`

	Expired  int64 `json:"expired"`
	Evicted  int64 `json:"evicted"`
	Replaced int64 `json:"replaced"`
	// Aborted 是面板 abort 按来源目录清场的移除计数——abort-after-detach
	// 残留窗的兑现观测面（设计前提是稀有事件，非零即说明窗口真实命中）。
	Aborted int64 `json:"aborted"`
	// Closed 是登记表关停整体清场的移除计数——进程退出路径的兑现观测面，
	// 非零即关停清场发生过（与 aborted 并列的稀有事件口径）。
	Closed int64 `json:"closed"`
	// Truncated 是缓冲被字节预算冻结的次数（append 截断点记账）——
	// flood/异常上游 drain 进缓存被预算拦下的信号；与移除路径解耦，
	// 截断尸体无论经哪条路径淘汰都已入账，不会漏记也不会重复计。
	Truncated int64 `json:"truncated"`

	Orphans         int64 `json:"orphans"`
	OrphanCompleted int64 `json:"orphan_completed"`
	// OrphansCrossLane 是孤儿中「消费者确实来过、只是去了别门」的
	// 子集（sawCrossLaneRetry 置位）——orphans-orphans_cross_lane
	// 近似「客户端压根没重试」的上界。
	OrphansCrossLane int64 `json:"orphans_cross_lane"`
	// OrphanBufferedEvents 是孤儿条目脱钩后新产出的事件量合计——token
	// 级浪费拿不到（真实 token 只在内存事件载荷里），事件量是最接近
	// 的量级代理：区分「登记即死的孤儿」与「跑了 20 分钟无人认领」。
	OrphanBufferedEvents int64 `json:"orphan_buffered_events"`
	// LedgerDrops 是 detached_events 台账写失败计数：台账存在的意义
	// 就是兜住争用期的丢痕迹，它自身的丢失量必须可观测（sqlite 争用
	// 风暴期超时写失败时这里涨）。
	LedgerDrops int64 `json:"ledger_drops"`
	// Seeded 是开机从 detached_blobs 灌回的 completed 条目数——交接后
	// 持久层兑现了几条的直接口径。BlobDrops 是种子层自身丢失计数
	//（编码/写库/坏行解码失败），与 LedgerDrops 分列：台账丢的是
	// 取证痕迹，这里丢的是真重放价值。
	Seeded    int64 `json:"seeded"`
	BlobDrops int64 `json:"blob_drops"`

	Events []DetachedEvent `json:"events,omitempty"` // 新在前
}

// mergeDetachedStats 把各 lane 的快照聚合为顶层 detached 段：全部计数
// 与现值字段逐 lane 求和（本结构所有字段口径一致可直接加），事件环
// 按时刻归并（新在前）截断到 detachedEventCap，并在归并时回填事件
// 的 Lane——聚合视图不再能靠透出路径携带 lane 身份。
func mergeDetachedStats(per map[string]DetachedStats) DetachedStats {
	merged := DetachedStats{}
	for name, s := range per {
		merged.Entries += s.Entries
		merged.Running += s.Running
		merged.Completed += s.Completed
		merged.Failed += s.Failed
		merged.Detaches += s.Detaches
		merged.Attaches += s.Attaches
		merged.AttachMisses += s.AttachMisses
		merged.CrossLaneMisses += s.CrossLaneMisses
		merged.FinishedCompleted += s.FinishedCompleted
		merged.FinishedFailed += s.FinishedFailed
		merged.FinishedKilled += s.FinishedKilled
		merged.FinishedExpired += s.FinishedExpired
		merged.Expired += s.Expired
		merged.Evicted += s.Evicted
		merged.Replaced += s.Replaced
		merged.Aborted += s.Aborted
		merged.Closed += s.Closed
		merged.Truncated += s.Truncated
		merged.Orphans += s.Orphans
		merged.OrphanCompleted += s.OrphanCompleted
		merged.OrphansCrossLane += s.OrphansCrossLane
		merged.OrphanBufferedEvents += s.OrphanBufferedEvents
		merged.LedgerDrops += s.LedgerDrops
		merged.Seeded += s.Seeded
		merged.BlobDrops += s.BlobDrops
		for _, ev := range s.Events {
			ev.Lane = name
			merged.Events = append(merged.Events, ev)
		}
	}
	// 新在前；同刻事件按 lane/key 定序，map 遍历序不泄漏进透出结果。
	slices.SortFunc(merged.Events, func(a, b DetachedEvent) int {
		if c := b.At.Compare(a.At); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Lane, b.Lane); c != 0 {
			return c
		}
		return cmp.Compare(a.Key, b.Key)
	})
	if len(merged.Events) > detachedEventCap {
		merged.Events = merged.Events[:detachedEventCap]
	}
	return merged
}

// newDetachedRegistry 创建空缓存。ledger 非空时生命周期事件经异步写
// 管道落 detached_events 台账（每事件一行）；lane 名作为台账的归属维
// 写入。台账泵随登记表创建——不随 ledger 判空惰性起：close() 的关停
// 约定依赖泵恒在场（nil 三件套自然空转）。
func newDetachedRegistry(ledger *store.Store, lane string) *detachedRegistry {
	registry := &detachedRegistry{
		entries:    make(map[string]*detachedEntry),
		ledger:     ledger,
		lane:       lane,
		events:     newEventRing[DetachedEvent](detachedEventCap),
		ledgerStop: make(chan struct{}),
		ledgerDone: make(chan struct{}),
	}
	if ledger != nil {
		registry.ledgerQueue = make(chan store.DetachedEvent, detachedLedgerQueueCap)
		go registry.runLedgerPump()
	} else {
		// 无台账时 done 立即闭合：close() 的等待语义不变（空转返回）。
		close(registry.ledgerDone)
	}
	return registry
}

// detachedPeersKey 是兄弟 lane 完成缓存登记表在请求 ctx 里的挂接键
// （与 gateYield 同型的 ctx 注值传递——不碰 Adapter 构造签名）。
type detachedPeersKey struct{}

// withDetachedPeers 把全池 {lane 名→完成缓存} 登记表挂进 ctx：号池下
// adapter.Stream 本地 lookup 落 nil 后据此探测兄弟 lane 是否持有同键
// 条目（跨 lane 挂接 miss 的观测面）。登记表含本 lane——探测方按
// reg != adapter.detached 跳过自身。
func withDetachedPeers(ctx context.Context, peers map[string]*detachedRegistry) context.Context {
	return context.WithValue(ctx, detachedPeersKey{}, peers)
}

// detachedPeersFrom 取回 ctx 上的兄弟缓存登记表；未挂接返回 nil——
// 裸 New() 与单 lane 快捷路径无 peers，探测循环自然零成本。
func detachedPeersFrom(ctx context.Context) map[string]*detachedRegistry {
	peers, _ := ctx.Value(detachedPeersKey{}).(map[string]*detachedRegistry)
	return peers
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
	originDir := entry.originDir
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
		registry.pushEvent(detachedEventMiss, key, originDir, cause)
		registry.evictLocked(key, entry, cause)
		return nil
	}
	if !replayable {
		registry.attachMisses++
		registry.pushEvent(detachedEventMiss, key, originDir, "unreplayable")
		return nil
	}
	registry.attaches++
	registry.pushEvent(detachedEventAttach, key, originDir, state.String())
	return entry
}

// peek 是只读的在场探测（跨 lane 挂接 miss 观测用）：条目在场回
// (state, usable, true, originDir)——usable 与 lookup 同判据（未过期 &&
// 未截断 && (非 failed || 可重放)），originDir 供探测方的跨 lane miss
// 台账行回指帧取证目录。不置 attached（不污染 owner 的孤儿口径）、
// 不惰性逐出、不计数；唯一写入是 sawCrossLaneRetry 置位——owner 侧移除
// 时据此把孤儿拆出「来错门」一档。缺席回 (_, _, false, "")（普通首发）。
func (registry *detachedRegistry) peek(key string) (state detachedState, usable, ok bool, originDir string) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry := registry.entries[key]
	if entry == nil {
		return 0, false, false, ""
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	usable = !time.Now().After(entry.expiresAt) && !entry.truncated &&
		(entry.state != detachedFailed || entry.replayable)
	entry.sawCrossLaneRetry = true
	return entry.state, usable, true, entry.originDir
}

// noteCrossLaneMiss 记一次跨 lane 挂接 miss：同键请求落到本 lane 但
// 条目在 owner lane——选号让位/删绑把本该挂接的消费者送错了门。计数与
// 事件环记在本 lane（attach_misses 的对称补全），owner 侧痕迹走
// sawCrossLaneRetry→orphans_cross_lane 口径。
func (registry *detachedRegistry) noteCrossLaneMiss(key, owner, originDir string, state detachedState) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.crossLaneMisses++
	registry.pushEvent(detachedEventCrossMiss, key, originDir, owner+":"+state.String())
}

// claim 是脱钩登记的 registry 侧握手：先把条目未登记期缺失的字段补齐
// ——originDir（台账行归因）与 drainCancel（running TTL 到期与淘汰掐泵
// 的句柄）——再经 admit 落册。顺序即不变量：admit 之后条目对淘汰、
// 挂接与 census 立即可见，drainCancel 先于落册就位，「已登记但不可杀」
// 的窗口不存在。detached CAS（唯一认领位）由调用方在流上兑入，claim
// 只管「字段 → 落册」这一段。parent 是客户端请求 ctx——泵的 drain
// 生命周期脱离客户端取消，WithTimeout 单独挂 running TTL。登记表已关停
// 时 admitted=false：drain ctx 就地回收，调用方放弃起泵，条目随 GC
// 蒸发——否则泵会脱离登记表管控空烧。
func (registry *detachedRegistry) claim(key string, entry *detachedEntry, originDir string, parent context.Context) (drainCtx context.Context, drainCancel context.CancelFunc, admitted bool) {
	drainCtx, drainCancel = context.WithTimeout(context.WithoutCancel(parent), detachedRunningTTL)
	entry.mu.Lock()
	entry.originDir = originDir
	entry.drainCancel = drainCancel
	entry.mu.Unlock()
	if !registry.admit(key, entry) {
		drainCancel()
		return nil, nil, false
	}
	return drainCtx, drainCancel, true
}

// admit 把条目按 key 登记进缓存并接管其后台泵的生命周期。容量触顶的
// 让位序：过期项 → 死尸体（截断/不可重放 failed——缓冲已无重放价值，
// 留场只为给同键到场记 miss，活泵不该给它让位）→ 最老 running →
// 最老终态兜底（四类穷尽分类保证至少逐出一条，容量是硬上限而非软帽）。
// 同键旧条目（前一次同请求脱钩的残骸）先逐出再登记——两条同键后台泵
// 同跑是纯粹的配额浪费。返回 false 表示登记表已关停（close 置位），
// 调用方须放弃起泵并回收 drain ctx——否则泵会脱离登记表管控空烧。
func (registry *detachedRegistry) admit(key string, entry *detachedEntry) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.closing {
		return false
	}
	entry.mu.Lock()
	entry.admittedAt = time.Now()
	entry.expiresAt = entry.admittedAt.Add(detachedRunningTTL)
	entry.detachIndex = len(entry.events)
	originDir := entry.originDir
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
		for k, e := range registry.entries {
			e.mu.Lock()
			corpse := e.truncated || (e.state == detachedFailed && !e.replayable)
			e.mu.Unlock()
			if corpse {
				registry.evictLocked(k, e, detachEvictCapacity)
			}
		}
	}
	if len(registry.entries) >= detachedMaxEntries {
		var runKey, termKey string
		var runAt, termAt time.Time
		for k, e := range registry.entries {
			e.mu.Lock()
			state, admitted := e.state, e.admittedAt
			e.mu.Unlock()
			if state == detachedRunning {
				if runKey == "" || admitted.Before(runAt) {
					runKey, runAt = k, admitted
				}
			} else if termKey == "" || admitted.Before(termAt) {
				termKey, termAt = k, admitted
			}
		}
		victim := runKey
		if victim == "" {
			victim = termKey
		}
		if victim != "" {
			registry.evictLocked(victim, registry.entries[victim], detachEvictCapacity)
		}
	}
	registry.entries[key] = entry
	registry.detaches++
	registry.pushEvent(detachedEventAdmit, key, originDir, "")
	return true
}

// evictLocked 摘出条目：running 条目同时掐后台泵的 drain ctx——泵的
// Recv 走 ctx.Done 退场并把条目收成 failed（挂接方拿到一个截断但干净
// 的终态）。掐的是一次性写入的 CancelFunc 而非直接 cancel 流：本函数
// 持 registry.mu 不能去拿流锁。全部移除走这一个漏斗：按 cause 记移除
// 计数，从未挂接的条目同时记孤儿（orphan_completed 是上游浪费口径；
// sawCrossLaneRetry 置位的孤儿另记 orphans_cross_lane「来错门」档）。
// 移除计数只分四桶——capacity 归 evicted、replaced 归 replaced、
// aborted 归 aborted（面板 abort 清场）、其余惰性收集（expired 到期与
// truncated 截断尸体）归 expired；truncated 计数在缓冲冻结点
// noteTruncated 已记，此处再记会双重入账。
func (registry *detachedRegistry) evictLocked(key string, entry *detachedEntry, cause string) {
	delete(registry.entries, key)
	switch cause {
	case detachEvictCapacity:
		registry.evicted++
	case detachEvictReplaced:
		registry.replaced++
	case detachEvictAborted:
		registry.aborted++
	case detachEvictClosed:
		registry.closed++
	default:
		registry.expired++
	}
	entry.mu.Lock()
	drainCancel := entry.drainCancel
	running := entry.state == detachedRunning
	orphan := !entry.attached
	crossLane := entry.sawCrossLaneRetry
	completed := entry.state == detachedCompleted
	bufferedAfterDetach := len(entry.events) - entry.detachIndex
	originDir := entry.originDir
	entry.mu.Unlock()
	if orphan {
		registry.orphans++
		registry.orphanBuffered += int64(bufferedAfterDetach)
		if completed {
			registry.orphanCompleted++
		}
		if crossLane {
			registry.orphansCrossLane++
		}
	}
	registry.pushEvent(detachedEventEvict, key, originDir, cause)
	if running && drainCancel != nil {
		drainCancel()
	}
}

// evictByOriginDir 按来源调试目录清场：摘出全部 originDir 匹配的条目。
// 收口 abort-after-detach 残留窗——detach 把条目登记落册到请求 Complete
// 出 activeDirs 之间有 µs 级窗口，窗口内 Abort 置位成功但 cancel(reqCtx)
// 已无效（客户端断连早已取消该 ctx），后台泵的 drainCtx 又是
// WithoutCancel 不受影响：不补这一刀，被掐死的生成继续烧上游配额且留在
// 缓存里供同键重试重放。dir 为空串时直接返回——recorder 缺失的流
// originDir 也是空串，空匹配会把它们一并清掉。running 条目经
// evictLocked 掐 drainCancel，泵走 ctx.Done 自行退场。
func (registry *detachedRegistry) evictByOriginDir(dir string) {
	if registry == nil || dir == "" {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	for key, entry := range registry.entries {
		entry.mu.Lock()
		match := entry.originDir == dir
		entry.mu.Unlock()
		if match {
			registry.evictLocked(key, entry, detachEvictAborted)
		}
	}
}

// noteFinish 记一次后台泵终局：被掐死的泵在条目移出后仍会走到这里，
// 计数按原因四档（detachFinish*）——orphan 口径量「有没有人接」，
// finish 口径量「泵怎么死的」，两维独立。entry 供台账行取 origin_dir：
// 被逐出后才到终局的泵在 map 里已查不到，只能由调用方递进来。
func (registry *detachedRegistry) noteFinish(key, reason string, entry *detachedEntry) {
	entry.mu.Lock()
	originDir := entry.originDir
	// completed 终局的缓冲事件引用一并取出：定态后泵是唯一写者且已
	// 退场，切片不会再被追加——锁外编码落库（persistBlob）不占双锁。
	var events []llm.ResponseEvent
	if reason == detachFinishCompleted {
		events = entry.events
	}
	entry.mu.Unlock()
	registry.mu.Lock()
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
	registry.pushEvent(detachedEventFinish, key, originDir, reason)
	registry.mu.Unlock()
	if events != nil {
		registry.persistBlob(key, events, originDir)
	}
}

// noteTruncated 记一次缓冲截断：append 越字节预算冻结缓冲时由 tee 点
// 调用——计数挂在截断发生点而非移除路径，截断尸体之后经 lookup 惰性
// 逐出还是 admit 容量扫描让位都不再重复入账。nil 接收容忍 entry 在场
// 而缓存未启用的流。entry 供台账行取 origin_dir（pre-detach 截断时
// 尚未赋值，落空串）。
func (registry *detachedRegistry) noteTruncated(key string, entry *detachedEntry) {
	if registry == nil {
		return
	}
	entry.mu.Lock()
	originDir := entry.originDir
	entry.mu.Unlock()
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.truncated++
	registry.pushEvent(detachedEventTruncate, key, originDir, "")
}

// noteBlobDrop 记一次种子层丢失：blob 编码失败、写库失败与播种时坏行
// 解码失败都计入 blobDrops——持久层兑现重放价值的折损必须有数，
// 事件环不为这类后台损耗产行（台账侧已有 ledgerDrops 同口径先例）。
func (registry *detachedRegistry) noteBlobDrop() {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.blobDrops++
}

// pushEvent 追加一条生命周期事件；调用方须持 mu。key 截前 12 位——
// 与 04 标记行的全量键前缀对照可认，全键写进快照只是噪音。
// ledger 非空时同一事件入队 detached_events 台账写管道（全量 key）：
// 入队在 mu 内做，FIFO 序天然与环序一致——原来 mu 内直写 sqlite 就是
// 买这个序，代价是 admit/evict/finish 等全生命周期路径都背着写连接的
// 争用延迟。队满按写失败记 ledgerDrops；台账是内存环之下的持久层，
// 不是替代。
func (registry *detachedRegistry) pushEvent(kind, key, originDir, detail string) {
	at := time.Now()
	registry.events.push(DetachedEvent{
		At:     at,
		Kind:   kind,
		Key:    detachedRingKey(key),
		Detail: detail,
		Label:  detachedEventLabel(kind, detail),
	})
	if registry.ledger == nil {
		return
	}
	if registry.closing {
		registry.ledgerDrops++
		return
	}
	select {
	case registry.ledgerQueue <- store.DetachedEvent{
		At: at, Lane: registry.lane, Key: key, OriginDir: originDir, Kind: kind, Detail: detail,
	}:
	default:
		registry.ledgerDrops++
		slog.Warn("detached event ledger write dropped: queue full", "lane", registry.lane, "kind", kind, "key", detachedRingKey(key))
	}
}

// runLedgerPump 是 detached_events 台账的单写者：FIFO 逐行落库直到
// 关停信号，随后排空存量退出——排空带总预算，库卡死时 close() 不被
// 整队重放拖住。
func (registry *detachedRegistry) runLedgerPump() {
	defer close(registry.ledgerDone)
	for {
		select {
		case e := <-registry.ledgerQueue:
			registry.writeLedgerRow(e)
		case <-registry.ledgerStop:
			deadline := time.Now().Add(detachedLedgerDrainBudget)
			for {
				select {
				case e := <-registry.ledgerQueue:
					registry.writeLedgerRow(e)
				default:
					return
				}
				if time.Now().After(deadline) {
					return
				}
			}
		}
	}
}

// writeLedgerRow 落单行台账；失败记 ledgerDrops（与队满拒收同口径）。
func (registry *detachedRegistry) writeLedgerRow(e store.DetachedEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	err := registry.ledger.InsertDetachedEvent(ctx, e)
	cancel()
	if err == nil {
		return
	}
	registry.mu.Lock()
	registry.ledgerDrops++
	registry.mu.Unlock()
	slog.Warn("detached event ledger write failed", "lane", e.Lane, "kind", e.Kind, "key", detachedRingKey(e.Key), "error", err)
}

// close 关停登记表：摘出全部在场条目（running 泵被 drainCancel 掐死，
// 泵走 ctx.Done 退场、终局经 noteFinish 照常记账）、置 closing 拒收
// 新登记与台账入队，随后停台账泵并排空存量写。Adapter 关闭路径调用——
// 与 BeginDrain 的差别：draining 只拒新准入不掐在册泵，close 是真清场。
// 台账管道永不关闭——closing 置位后 pushEvent 直接记 drop，迟到入队
// 不可能发生，管道自然无 send-on-closed 风险。
func (registry *detachedRegistry) close() {
	if registry == nil {
		return
	}
	registry.mu.Lock()
	if registry.closing {
		// 重入防护：ledgerStop 只能关一次（Adapter.Close 与号池删 lane
		// 的异步 Close 撞在同一登记表上时第二次到此直接退场）。
		registry.mu.Unlock()
		return
	}
	for key, entry := range registry.entries {
		registry.evictLocked(key, entry, detachEvictClosed)
	}
	registry.closing = true
	registry.mu.Unlock()
	close(registry.ledgerStop)
	<-registry.ledgerDone
}

// detachedRingKey 给事件环的 key 截前 12 位。
func detachedRingKey(key string) string {
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// stats 返回完成缓存快照：在场条目按态分解 + 累计计数 + 事件环
// （新在前）。每请求一次的轮询成本是 8 条上限的线性扫。遍历顺手
// 清收过期条目——与 lookup/admit 相同的 evictLocked 惰性口径：低流量
// 期两条惰性路径数小时不点火，metrics 轮询就此兼任被动清扫器，entries
// 现值与 expired/孤儿记账不再被尸体驻留推迟。running 条目过期时其
// drainCtx 期限已先点火（泵的存活上界先于 admit 写 expiresAt 创建），
// evictLocked 的 drainCancel 落在已断的 ctx 上，不抢泵的 ttl_expired
// 终局归因。
func (registry *detachedRegistry) stats() DetachedStats {
	stats := DetachedStats{}
	if registry == nil {
		return stats
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := time.Now()
	for key, entry := range registry.entries {
		entry.mu.Lock()
		expired := now.After(entry.expiresAt)
		state := entry.state
		entry.mu.Unlock()
		if expired {
			registry.evictLocked(key, entry, detachEvictExpired)
			continue
		}
		switch state {
		case detachedCompleted:
			stats.Completed++
		case detachedFailed:
			stats.Failed++
		default:
			stats.Running++
		}
	}
	stats.Entries = len(registry.entries)
	stats.Detaches = registry.detaches
	stats.Attaches = registry.attaches
	stats.AttachMisses = registry.attachMisses
	stats.CrossLaneMisses = registry.crossLaneMisses
	stats.FinishedCompleted = registry.finishedCompleted
	stats.FinishedFailed = registry.finishedFailed
	stats.FinishedKilled = registry.finishedKilled
	stats.FinishedExpired = registry.finishedExpired
	stats.Expired = registry.expired
	stats.Evicted = registry.evicted
	stats.Replaced = registry.replaced
	stats.Aborted = registry.aborted
	stats.Closed = registry.closed
	stats.Truncated = registry.truncated
	stats.Orphans = registry.orphans
	stats.OrphanCompleted = registry.orphanCompleted
	stats.OrphansCrossLane = registry.orphansCrossLane
	stats.OrphanBufferedEvents = registry.orphanBuffered
	stats.LedgerDrops = registry.ledgerDrops
	stats.Seeded = registry.seeded
	stats.BlobDrops = registry.blobDrops
	stats.Events = registry.events.recent()
	return stats
}

// watchClientCtx 是客户端断连哨兵：断连时先走锁外快路——admitIntent
// 单调位判定、CAS 认领后条目即刻落册（对同键 lookup、面板 abort 的
// 清场与 census 立即可见，台账行同步持久化），后台泵不持锁启动、首个
// Recv 随 mu 释放自然进场。资格不符或占位旁落才回落持 mu 的终局判定
// （可脱钩则正式脱钩，否则杀泵）。
func (stream *responseStream) watchClientCtx(ctx context.Context) {
	<-ctx.Done()
	if stream.admitIntent() {
		stream.admitDetached(ctx)
	}
	if stream.detached.Load() {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	if stream.detached.Load() {
		return
	}
	if stream.detachable() {
		stream.detach(ctx)
	} else {
		stream.cancel()
	}
}

// detachable 判定这条流客户端断开后是否值得脱钩续命：七个条件缺一
// 不可——缓存挂接面注入（registry/detachKey/entry 非空，Adapter.Stream
// 才有；测试裸流恒假）、未被主动中断（面板 abort/排空强掐与客户端
// 断连同走 ctx.Done，但缓存只救断连：被掐死的流准入会续烧上游至
// running TTL，同键重试还会重放尸体）、缓存未闩门（排空中的进程即将
// 退出，登记的条目随进程死蒸发，挂接方永远来不了——续烧的上游算力
// 纯浪费，断连流直接随客户端死掉）、缓冲未截断（客户端还在场时
// 缓冲就越预算的流续命也产不出完整重放，直接按不可脱钩杀）、已产出
// 过内容（pre-content 流没有重放价值，且重放键会污染缓存）、语义未
// 收口（已见 stopReason/停止序列的流只剩传输尾帧，续命等不到新
// 内容）、流未终结（finished 的流没有可续命的泵：handler 返回同样
// 取消请求 ctx，哨兵必然醒来一次——产过内容但未带 stopReason 的
// 失败收尾如 midcontent 式截断，少了这道闸会把死流登记进缓存白占
// 容量）。已脱钩的流不可再脱钩。
func (stream *responseStream) detachable() bool {
	return !stream.detached.Load() && !stream.finished.Load() &&
		!stream.recorder.WasAborted() &&
		stream.registry != nil && !stream.registry.draining.Load() &&
		stream.detachKey != "" && stream.entry != nil &&
		!stream.entry.isTruncated() &&
		stream.producedEvents.Load() && !stream.decoder.hasStopReason && !stream.decoder.stoppedByPattern
}

// admitIntent 是断连时刻的占位登记资格（admitDetached 锁外快路的准入
// 判定）：与 detachable() 的分工是「断开瞬间值不值得留」对「持锁终局
// 该不该留」——判据只收单调位与非可变字段（producedEvents/finished/
// detached、entry.truncated、recorder.aborted 全单向翻转，registry/key/
// entry 构造期定型），锁外读到的 true 恒为已成立事实。finished 仍在列：
// 正常完结的请求 handler 返回即取消 ctx、哨兵必醒，那时 finished 已真，
// 拒绝占位才不致把每条成功请求都存进缓存；断连落在锁内阻塞段的场景
// 段未收束 finished 必假，洞例照样占得上位。hasStopReason/
// stoppedByPattern 不卡：断连落在「语义已收口、只剩尾帧」的窗口时登记
// 仍兑现前缀价值，泵随后自然定态 completed。
func (stream *responseStream) admitIntent() bool {
	return stream.registry != nil && !stream.registry.draining.Load() &&
		stream.detachKey != "" && stream.entry != nil &&
		stream.producedEvents.Load() && !stream.finished.Load() &&
		!stream.entry.isTruncated() && !stream.recorder.WasAborted()
}

// detach 把流从客户端生命周期解耦（持 mu 路径）：停掉两个看门狗计时器
// 后经 admitDetached 登记——占位哨兵先 CAS 时本调用只剩停表与兑入已
// 认领的 detached 标记。调用方须持 stream.mu（消费方 Recv 与哨兵慢路
// 都满足）；由此串行保证同一时刻只有一个 Recv 在场，后台泵的第一轮
// Recv 会等持锁方退场。
func (stream *responseStream) detach(ctx context.Context) {
	if stream.stall != nil {
		stream.stall.Stop()
		stream.stall = nil
	}
	if stream.progress != nil {
		stream.progress.Stop()
		stream.progress = nil
	}
	stream.admitDetached(ctx)
}

// admitDetached 是登记动作的唯一实现：detached CAS 认领「登记一次」——
// 占位哨兵（锁外快路）与持 mu 的 detach() 共用，后到者空转。赢家把
// 「字段 → 落册」交给 registry.claim（时序不变量收进 registry 域），
// 写 04 detached 标记与 meta 镜像，再起后台泵。锁序：本方法不持
// stream.mu 也可运行，内部只依次取 entry.mu→registry.mu，不破坏
// stream.mu > registry.mu > entry.mu 的既有顺序。
func (stream *responseStream) admitDetached(ctx context.Context) {
	if !stream.detached.CompareAndSwap(false, true) {
		return
	}
	entry := stream.entry
	drainCtx, drainCancel, admitted := stream.registry.claim(stream.detachKey, entry, stream.recorder.Dir(), ctx)
	if !admitted {
		return
	}
	detail := map[string]any{
		"key":             stream.detachKey,
		"buffered_events": entry.len(),
	}
	stream.recorder.AppendJSONL(debuglog.StageDevinResponse, "detached", detail)
	stream.recorder.NoteDetachedEvent("detached", detail)
	go stream.pumpDetached(drainCtx, drainCancel, entry)
}

// teeDetached 把每个下发事件镜像进完成缓存条目：脱钩后重试方需要含前缀
// 的完整事件序列，tee 在返回点才能覆盖 start 扣留在内的全部对外事件。
// append 越预算时缓冲冻结成截断前缀+终止错误——截断发生数在冻结点记账
// （与条目之后的移除路径解耦），并记 04 detached_truncated 标记行给
// 「flood drain 进缓存」留取证（detached/detached_attach 之外的第三条
// 脱钩标记）。后台泵经同一 tee 点触发的截断落在 Complete 之后：04 标记
// 行走 closed 豁免照常落库；meta 镜像追加同时排一条终态 meta 重写任务，
// detached_events 覆盖脱钩泵的余生而非停在定稿点。
func (stream *responseStream) teeDetached(event llm.ResponseEvent) {
	if stream.entry == nil {
		return
	}
	if stream.entry.append(event) {
		stream.registry.noteTruncated(stream.detachKey, stream.entry)
		detail := map[string]any{
			"key":          stream.detachKey,
			"budget_bytes": detachedMaxBufferedBytes,
		}
		stream.recorder.AppendJSONL(debuglog.StageDevinResponse, "detached_truncated", detail)
		stream.recorder.NoteDetachedEvent("detached_truncated", detail)
	}
}

// pumpDetached 是脱钩后的后台泵：续消费直到终态——缓冲经 tee 持续追加，
// 挂接方按序追帧。泵的存活上界是 running TTL：到期/被容量淘汰掐
// drainCtx 退场时补一条终局错误——截断前缀若误标 completed 会把半成品
// 当完整响应重放给同键重试。退场必收尸：TTL/淘汰走的 ctx.Done 分支不
// 杀泵（detached 态直接退场），不补这一刀泵协程会随 streamBase 永久
// 阻塞在 Receive 上；kill 在 mu 下拿当前 cancel，换流后也不会杀错。
func (stream *responseStream) pumpDetached(drainCtx context.Context, drainCancel context.CancelFunc, entry *detachedEntry) {
	defer drainCancel()
	defer stream.kill()
	for {
		_, err := stream.Recv(drainCtx)
		// 缓冲越预算截断时同步停泵：重放价值归零，继续 drain 是
		// 纯配额浪费——末帧已是截断错误，finish 收成不可重放的
		// failed。isTruncated 走 entry.mu，与 append 同一临界区。
		if err == nil && !entry.isTruncated() {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			if entry.append(llm.ResponseEvent{
				Type: llm.ResponseEventError,
				Error: &llm.AssistantMessage{
					ErrorMessage: "detached pump stopped: " + err.Error(),
					Failure:      &llm.Failure{Code: "internal", UpstreamFault: true},
				},
			}) {
				// 终局错误的追加自身越预算：同一冻结点记账口径。
				stream.registry.noteTruncated(stream.detachKey, entry)
			}
		}
		// 泵终局按原因记四档：drainCtx 超时是 running TTL 到期，
		// drainCancel 是 registry 淘汰掐泵，EOF 的 completed/failed
		// 由 finish 尾帧定态，其余错误归 failed。
		reason := detachFinishFailed
		switch state := entry.finish(); {
		case errors.Is(err, context.DeadlineExceeded):
			reason = detachFinishExpired
		case errors.Is(err, context.Canceled):
			reason = detachFinishKilled
		case errors.Is(err, io.EOF) && state == detachedCompleted:
			reason = detachFinishCompleted
		}
		stream.registry.noteFinish(stream.detachKey, reason, entry)
		return
	}
}

// kill 中止上游泵：持 mu 拿当前 cancel 值再调——swap/tryResume 换流时
// 在 mu 下重赋值该字段，func 值两词，裸读写有撕裂风险。只供锁外
// 调用方（后台泵退场 defer）；流内持锁路径直接调 stream.cancel()。
func (stream *responseStream) kill() {
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.cancel()
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
	// messages 形状断言必须 comma-ok：投影是观测面而非类型契约，
	// 改形（类型化切片/换投影结构）时宁可让 timestamp_ms 残留进键
	// 退化挂接命中，也不能在每条请求的算键路径上 panic。
	if messages, ok := projection["messages"].([]any); ok {
		for _, message := range messages {
			if item, ok := message.(map[string]any); ok {
				delete(item, "timestamp_ms")
			}
		}
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
