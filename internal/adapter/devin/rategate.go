// 本文件实现发往上游 GetChatMessage 的本地速率闸门。
package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/store"
)

const (
	// gateDefaultMaxHold 是闸门内允许的最长排队等待：预计睡到下一窗口
	// 超过它时请求在本地快速失败并带 Retry-After——客户端/下游网关
	// 按声明时刻退避，比占着并发槽空等更符合冷却语义。半个窗口的长度
	// 让 fg 突发洪峰宁可闸内排队跨过死区也不把 429 甩给客户端重试。
	gateDefaultMaxHold = 30 * time.Second
	// gateDefaultBgMaxHold 是 bg 类请求的排队预算：无人值守负载等得
	// 起，给到两个窗口的长度让批跑宁可排队也不快败空转。
	gateDefaultBgMaxHold = 120 * time.Second
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
	// gateDefaultBgMargin 是 bg 预留公式中的固定安全边际：吸收 fg
	// 速率 EMA 的滞后与小并发突发。
	gateDefaultBgMargin = 4
	// gateBgRecheck 是 bg 被预留/爬坡挡住时的睡醒重查间隔：预留随可发
	// 区间剩余时间衰减、爬坡额度随经过时间线性释放，短间隔重查让
	// bg 吃到中段让出的槽而不必睡到下一窗口。
	gateBgRecheck = 4 * time.Second
	// gateEarlyRelease 是让位阈值：预计单次排队超过它且号池里有兄弟
	// lane 此刻能更快放行（expectedWait 落进阈值）时，闸门提前快败
	// 把请求交给 failover——在注定排长队的 lane 上把预算骑满再换号，
	// 只是把同一结局推迟一个排队预算（实测 bg 被吸收 p50≈119s，换号
	// 后 ~0.4s 建流）。取值压在 bg 短重查（4s）之上、fg 预算（30s）
	// 之下：只截「睡到下一窗口」类长阻塞（死区/桶满），不碰预留让路
	// 的短节奏；兄弟侧阈值同用本值——能比它更快放行才算有余量。
	gateEarlyRelease = 8 * time.Second
	// fgRateAlpha 是每窗口 fg 准入数 EMA 的更新系数：0.2 对应
	// ~3 窗口半衰期，足够跟上交互负载的起落又不被单窗口抖动带走。
	fgRateAlpha = 0.2
)

// 闸门拒绝的 X-Gate-Reason 取值：latch 是冷却闩快败（Retry-After
// 报闩剩余）；quota 是配额类拒绝（fg 桶满 / bg 预留不足，Retry-After
// 报下一窗口）；hold 是桶未满但等待将超预算（死区等待是唯一来源）；
// yield 是让位快败——预计长排队且兄弟 lane 有余量时提前放行给 failover
// （Retry-After 按底层阻塞成因同口径报出）。
const (
	gateReasonLatch = "latch"
	gateReasonQuota = "quota"
	gateReasonHold  = "hold"
	gateReasonYield = "yield"
)

// tryAdmit 拒绝的专有成因词（保温 ping 事件环的 skip.reason；latch/
// quota 与上方 X-Gate-Reason 同词同义直接复用）：deadzone 是落在
// 窗口尾段死区（lane verdict 的 gate_window_deadzone 同词），pace
// 是 bg 爬坡额度未释放（pace_allowance 的同源读数）。
const (
	tryAdmitSkipDeadzone = "deadzone"
	tryAdmitSkipPace     = "pace"
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
	bucketUsedFg int           // 本桶 fg 放行数（bucketUsed 的类别分列）
	bucketUsedBg int           // 本桶 bg 放行数（含保温 ping——ping 视同最低优先级背景流量）
	limitedUntil time.Time     // 冷却闩截止时刻；零值表示未上闩
	nextDrip     time.Time     // 闩内下一个探针放行时刻
	maxHold      time.Duration
	bgMaxHold    time.Duration // bg 请求的排队预算（fg 用 maxHold）
	bgMargin     int           // bg 预留公式的固定安全边际
	dripInterval time.Duration
	defaultLatch time.Duration
	// fgWindow/fgRateEMA 是 fg 需求估计：每窗口 fg 准入数的指数滑动
	// 平均（条/窗），在桶翻页时折叠。bg 预留量用它外推本桶剩余时间
	// 内 fg 还会来多少——估高让 bg 少吃，估低退化成 margin 静态预留。
	fgWindow  int
	fgRateEMA float64
	// lane 是闸门所属 lane 名，从 stateKey（gate:<lane>）前缀解析，
	// 供 X-Gate-Lane 遥测；无状态键的部署/测试为空。
	lane string
	// states/stateKey 非空时冷却闩截止时刻持久化到 runtime_state：
	// 重启后仍在闩内的实例不会裸发上游把限流续长——上游限流器把被拒
	// 尝试也计入窗口。键名对闸门不透明，由构造方给（gate:<lane>）。
	states   *store.Store
	stateKey string
	// now 是时钟源，测试可替换为可控假钟；窗口位置依赖墙钟，注入后
	// 配额/死区/闩的用例才能确定落在指定分钟秒位。
	now func() time.Time
	// 计数器供面板 stats 透出闸门状态；全部在 mu 下读写。
	latchCount      int
	dripCount       int
	rejectLatched   int // 闩内被快败的请求数
	rejectHold      int // 闩外排队预计超 maxHold 被快败的请求数
	rejectBgReserve int // bg 因预留/爬坡让路被快败的请求数（礼让强度指标；bg 桶满快败归 rejectHold）
	rejectYield     int // 兄弟 lane 有余量时提前快败让给 failover 的请求数（让位强度指标）
	waitersFg       int // 当前睡到下一窗口的 fg 请求数
	waitersBg       int // 当前睡着的 bg 请求数（预留阻塞重查也进此列）
	// win* 是本窗口明细账：rollBucket 翻页时快照成 gate_windows 行后
	// 清零，把窗口用量/拒绝成因从「事后回推 logs」变成直查。上面的
	// 累计计数器口径不动（面板兼容），win* 按 client 看到的 reason
	// 细分——桶满快败归 winRejectQuota 而不是笼统的 rejectHold。
	winRejectQuota     int // 桶满快败（fg/bg 同列）
	winRejectHold      int // 死区等待超预算快败
	winRejectBgReserve int // bg 让路快败（预留/爬坡）
	winRejectLatch     int // 闩内快败
	winRejectYield     int // 让位快败（兄弟有余量提前放给 failover）
	winDrip            int // 闩内滴灌探针放行数
	winRetryAdmits     int // 续试重发放行数（used_* 的子集——同请求 reopen/续轮/瞬时重试的再发送）
	winUsedBgPing      int // 保温 ping 放行数（used_bg 的子集——used_bg 减本列即真实 bg 需求）
	winReservePeak     int // 本窗 bg 预留量峰值（reserve 每次评估取样）
	winWaitersPeak     int // 本窗排队数峰值（waitersFg+waitersBg）
	lastWindow         *store.GateWindow
	// pendingWindows 是窗口行写失败的重放缓冲：持久化协程写失败后把
	// 未落库的行挂账回来，下一次窗口翻页持久化时随新行一并重放。容量
	// 封顶 gatePersistRetryCap，溢出丢最老行——缓冲是写争用期的安全带
	// 而非持久队列，永久丢失由 persistDropped 显式计数；
	// persistFailures 记写尝试失败次数（与 stderr 告警一一对应）。
	pendingWindows  []*store.GateWindow
	persistFailures int
	persistDropped  int
	// persistInFlight/persistDone 是在途窗口持久化协程的计数与落定信号：
	// persistWindow 交出协程前 +1（>0 时 persistDone 非 nil），协程收尾
	// （含失败挂回之后）-1，归零时 close 并置 nil。FlushPendingWindows
	// 凭它在排空时等写协程落定——失败行挂回缓冲后才能被冲刷看见。
	// 不用 sync.WaitGroup：排空期在途流量仍可能触发 persistWindow 的
	// Add，与 Wait 并发在空计数上属 misuse（会 panic）。均在 mu 下读写。
	persistInFlight int
	persistDone     chan struct{}
	// 闩迁移事件环：计数器只说发生过几次上闩，事件环回答「什么时候闩的、
	// 闩了多久、怎么解的」——概览趋势图的闩时段底色与系统页事件表同源。
	events    [gateEventCap]GateEvent
	eventHead int
	eventSize int
	// waits/waitHead/waitSize 是 wait 结局样本环（与 events 同构）：
	// 每次评估从进闸到结局各记一条——放行/拒绝/取消全录，无幸存者
	// 口径偏差；waitEvals 是启动以来累计评估数（环满后 Samples 饱和、
	// Evals 继续走）。stats 聚合出分类等待分位与 err 分位
	// （est−wait），是 expectedWait 估计器的实测校准面。
	waits     [gateWaitCap]gateWaitSample
	waitHead  int
	waitSize  int
	waitEvals int
	// waitTotal* 是进程期累计的分类等待账（与样本环并行：环是近窗，
	// 账是全期单调计数）——快照差分即任意区间的分类等待率
	// （waits/evals）与平均等待（wait_total_ms/waits），不受环容量
	// 覆盖期限制。
	waitTotalFg GateWaitTotal
	waitTotalBg GateWaitTotal
}

// gateEventCap 是闩事件环容量；闩迁移低频，64 条足够回看一整天。
const gateEventCap = 64

// gateWaitCap 是每 lane 等待样本环容量：80 RPM 配额下约覆盖最近三个
// 窗口的评估量，足够分位估计又不让面板轮询载荷膨胀。
const gateWaitCap = 256

// 等待结局词表：admit=放行、reject=闸门拒绝（reason 记 gateReason*）、
// cancel=ctx 取消（客户端断连/打断）。三态全录——logs 的 transform
// 段只反映放行幸存者，估计器校准需要全结局样本。
const (
	gateWaitAdmit  = "admit"
	gateWaitReject = "reject"
	gateWaitCancel = "cancel"
)

// gateWaitSample 是一次 wait 评估的实测记录：进闸时刻、墙钟等待时长
// （与 X-Gate-Wait-Ms 同源口径）、请求类与结局；est 是首次评估时
// expectedWaitLocked 的期望排队估计（负值 = 首拍评估前取消、无估计），
// 与 wait 组成 ew-vs-realized 校准对。
type gateWaitSample struct {
	at      time.Time
	wait    time.Duration
	est     time.Duration
	class   string // adapter.ClassFG / ClassBG
	outcome string // gateWait* 词表
	reason  string // outcome=reject 时的 gateReason*，其余空
}

// GateWait 是等待样本环的聚合视图：All 是全样本摘要，Fg/Bg 是它的
// 分类切片；Since 标出环覆盖期起点，Rejects 按 gateReason* 分账。
type GateWait struct {
	Samples int             `json:"samples"`           // 环内样本数（≤ gateWaitCap）
	Evals   int             `json:"evals"`             // 进程启动以来 wait 评估总数
	Since   *time.Time      `json:"since,omitempty"`   // 最老样本时刻（环覆盖期起点）
	Rejects map[string]int  `json:"rejects,omitempty"` // 环内拒绝按 gateReason* 分账
	All     GateWaitSummary `json:"all"`
	Fg      GateWaitSummary `json:"fg"`
	Bg      GateWaitSummary `json:"bg"`
	// Totals 是进程期单调累计的分类等待账（fg/bg 各一份）。
	Totals GateWaitTotals `json:"totals"`
}

// GateWaitTotals 是 fg/bg 两类的进程期累计等待账。
type GateWaitTotals struct {
	Fg GateWaitTotal `json:"fg"`
	Bg GateWaitTotal `json:"bg"`
}

// GateWaitTotal 是一类请求的进程期闸门等待累计账：Evals 评估总数、
// Waits 实际占过 waiters 名额的评估数（真排过队——闩内/配额/让位
// 快败与即时放行都不计）、WaitTotalMs 实测墙钟等待累计毫秒（与
// X-Gate-Wait-Ms 同口径）。与样本环并行：环覆盖最近 gateWaitCap
// 条评估，本账单调累计——两次快照差分即任意区间的分类等待率与
// 平均等待，不受环覆盖期限制。
type GateWaitTotal struct {
	Evals       int   `json:"evals"`
	Waits       int   `json:"waits"`
	WaitTotalMs int64 `json:"wait_total_ms"`
}

// GateWaitSummary 是一组等待样本的聚合读数：样本数、墙钟等待的
// 均值/分位/峰值（毫秒），以及拒绝/取消结局计数——后两者揭示
// 幸存者口径（仅放行）看不见的尾部。err* 是 ew 校准面：
// err = est（首次评估的 expectedWaitLocked）− realized（实测等待），
// 正值 = 估计偏高；只在有估计的放行样本上聚合——拒绝/取消的实测
// 等待被结局截断，不是估计器预测的「到放行时长」。ErrCount 为零时
// 各 err 字段缺省。
type GateWaitSummary struct {
	Count     int   `json:"count"`
	MeanMs    int64 `json:"mean_ms"`
	P50Ms     int64 `json:"p50_ms"`
	P90Ms     int64 `json:"p90_ms"`
	MaxMs     int64 `json:"max_ms"`
	Rejects   int   `json:"rejects"`
	Cancels   int   `json:"cancels"`
	ErrCount  int   `json:"err_count,omitempty"`
	ErrMeanMs int64 `json:"err_mean_ms,omitempty"`
	ErrP10Ms  int64 `json:"err_p10_ms,omitempty"`
	ErrP50Ms  int64 `json:"err_p50_ms,omitempty"`
	ErrP90Ms  int64 `json:"err_p90_ms,omitempty"`
}

// 闩迁移事件种类：latched（上游限流上闩/延闩）、released（成功帧提前
// 解闩）、expired（闩到期自然失效）、restored（重启从持久态恢复
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
// 不是解闩（没有成功帧证据），但截止已过，内存态与持久行都该闭环。
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

// gateState 是冷却闩的持久化形态（runtime_state 的值 JSON）；只存
// 截止时刻——滴灌时钟与窗口计数刻意不存（重启新窗口重新计数是想要的，
// 闩内节奏按 dripInterval 重排即可）。
type gateState struct {
	LimitedUntil time.Time `json:"limited_until"`
}

// GateStats 是闸门状态快照，面板 /admin/runtime-metrics 的 gate 段透出。
type GateStats struct {
	Latched          bool       `json:"latched"`
	LimitedUntil     *time.Time `json:"limited_until,omitempty"`
	LatchCount       int        `json:"latch_count"`
	DripCount        int        `json:"drip_count"`
	RejectLatched    int        `json:"reject_latched_count"`
	RejectHold       int        `json:"reject_hold_count"`
	WindowQuota      int        `json:"window_quota"`          // 每桶配额（= max_rpm）；0 表示不限速
	WindowUsed       int        `json:"window_used"`           // 当前桶已放行数（= used_fg + used_bg + 未分类）
	WindowUsedFg     int        `json:"window_used_fg"`        // 本桶 fg 放行数
	WindowUsedBg     int        `json:"window_used_bg"`        // 本桶 bg 放行数（含保温 ping）
	WindowUsedBgPing int        `json:"window_used_bg_ping"`   // 本桶放行中经 tryAdmit 的保温 ping 数（window_used_bg 的子集）
	WindowOpen       *time.Time `json:"window_open,omitempty"` // 当前桶的可发窗口起点
	WindowNext       *time.Time `json:"window_next,omitempty"` // 下一桶可发窗口开放时刻
	Sendable         bool       `json:"sendable"`              // 当前是否处于可发区间（非死区）
	Waiters          int        `json:"waiters"`               // fg+bg 排队总数
	WaitersFg        int        `json:"waiters_fg"`
	WaitersBg        int        `json:"waiters_bg"`
	RejectBgReserve  int        `json:"reject_bg_reserve_count"` // bg 因预留/爬坡让路被快败数（礼让强度指标）
	RejectYield      int        `json:"reject_yield_count"`      // 兄弟 lane 有余量时提前快败让给 failover 数（让位强度指标）
	// Reserve/FgRate 是预留机制的实时读数：当前预留槽数与 fg 准入
	// 速率 EMA（条/窗）——bg 被拒/放行的可解释性来源。
	Reserve int     `json:"reserve"`
	FgRate  float64 `json:"fg_rate"`
	// PaceAllowance 是爬坡机制的实时读数：此刻 bg 放行额度上限
	// （quota-reserve 按经过时间线性释放）；死区或零配额时为 0。
	PaceAllowance int         `json:"pace_allowance"`
	Events        []GateEvent `json:"events,omitempty"` // 新在前
	// LatchRanges 是从闩事件环还原的闩时段（[start,end] 对），由
	// stats() 与事件环同锁算出——前端趋势图直接铺 markArea，不再在
	// JS 里重放状态机。
	LatchRanges []GateLatchRange `json:"latch_ranges,omitempty"`
	// LastWindow 是最近一个被关闭窗口的聚合行（与落库 gate_windows
	// 同一份快照）；进程内零窗口翻转或持久层未接线时为 nil。
	LastWindow *store.GateWindow `json:"last_window,omitempty"`
	// PersistFailures/PersistDropped 是窗口行落库的健康账：写协程
	// INSERT 失败的次数（失败行挂回重放缓冲随下一窗口重放）与缓冲
	// 溢出被永久丢弃的行数。窗口行写走异步协程，没有这两个读数
	// 写争用期丢行完全不可见。
	PersistFailures int `json:"persist_failures"`
	PersistDropped  int `json:"persist_dropped"`
	// PendingWindows 是重放缓冲当前深度——persist_failures/persist_dropped
	// 是累计账，本字段回答「此刻还有几行欠着没落库」（quota 组的
	// pending_samples 同口径）。
	PendingWindows int `json:"pending_windows"`
	// Wait 是最近 gateWaitCap 次 wait 评估的分类聚合：实测等待分位
	// 与 est−realized 误差分位是 expectedWait 估计器的校准面；环
	// 覆盖全结局（含拒绝与取消），补上 transform 段看不见的尾部。
	// 进程内尚无评估时为 nil。
	Wait *GateWait `json:"wait,omitempty"`
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
	// BgMaxHold 是 bg 请求的排队预算上限（fg 用 MaxHold）：无人值守
	// 负载等得起，宁可在闸内排队等预留衰减/窗口翻页也不快败空转。
	// <=0 回落 gateDefaultBgMaxHold。
	BgMaxHold time.Duration
	// BgReserveMargin 是 bg 预留公式的固定安全边际（条）：叠加在
	// fg 速率 EMA 与 fg 排队数之上，吸收估计滞后与小并发突发。
	// <=0 回落 gateDefaultBgMargin。
	BgReserveMargin int
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
// states+stateKey 非空时从 runtime_state 恢复未过期的冷却闩；stateKey
// 的 gate:<lane> 前缀同时解析出 lane 名供 X-Gate-Lane 遥测。
func newRateGate(params GateConfig, states *store.Store, stateKey string) *rateGate {
	gate := &rateGate{
		states:   states,
		stateKey: stateKey,
		now:      time.Now,
	}
	if rest, ok := strings.CutPrefix(stateKey, "gate:"); ok {
		gate.lane = rest
	}
	gate.setParams(params)
	gate.restoreState()
	return gate
}

// NormalizeGateConfig 把无效闸门参数回落到默认：时长 <=0、桶界估计
// 折进 [0,60)、死区 <=0 或吞掉整窗。MaxRPM 原样保留——<=0 是「不限速」
// 的合法语义，不是缺省。运行时与面板展示共用此函数，两处口径一致。
func NormalizeGateConfig(params GateConfig) GateConfig {
	params.MaxHold = gateDurationOrDefault(params.MaxHold, gateDefaultMaxHold)
	params.BgMaxHold = gateDurationOrDefault(params.BgMaxHold, gateDefaultBgMaxHold)
	if params.BgReserveMargin <= 0 {
		params.BgReserveMargin = gateDefaultBgMargin
	}
	params.DripInterval = gateDurationOrDefault(params.DripInterval, gateDefaultDripInterval)
	params.DefaultLatch = gateDurationOrDefault(params.DefaultLatch, gateDefaultLatch)
	params.WindowOffset %= windowPeriod
	if params.WindowOffset < 0 {
		params.WindowOffset += windowPeriod
	}
	if params.WindowGuard <= 0 || 2*params.WindowGuard >= windowPeriod {
		params.WindowGuard = gateDefaultWindowGuard
	}
	return params
}

// setParams 原位更新闸门参数（reload 热路径）：闩态保留，窗口参数变化
// 后下一次 wait/stats 按新边界重算当前桶，桶起点不同即开新桶重新计数。
func (gate *rateGate) setParams(params GateConfig) {
	params = NormalizeGateConfig(params)
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.maxHold = params.MaxHold
	gate.bgMaxHold = params.BgMaxHold
	gate.bgMargin = params.BgReserveMargin
	gate.dripInterval = params.DripInterval
	gate.defaultLatch = params.DefaultLatch
	gate.quota = params.MaxRPM
	gate.windowOpen = (params.WindowOffset + params.WindowGuard) % windowPeriod
	gate.usable = windowPeriod - 2*params.WindowGuard
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

// rollBucket 把计数桶对齐到 ws 所属窗口：跨边界时把刚结束窗口的 fg
// 准入数折叠进 fgRateEMA（α=fgRateAlpha）、明细账快照成 gate_windows
// 行，并清零用量账——过期桶的用量不结转。每多跳过一个空窗 EMA 再衰减
// 一档：无流量时段估计值按半衰期回落而非冻结在最后一次观测。
// 调用方须持 mu。
func (gate *rateGate) rollBucket(ws time.Time) {
	if ws.Equal(gate.bucketStart) {
		return
	}
	// skipped 是本窗口之前完整流逝的空窗数；ws 与 bucketStart 都对齐
	// 在 windowOpen 边界上，差值是 60s 的倍数（桶界参数被 reload 改
	// 动时退化成正负漂移，按 0 处理——折叠一次刚结束的窗口即可）。
	skipped := int(ws.Sub(gate.bucketStart)/windowPeriod) - 1
	if gate.bucketStart.IsZero() || skipped < 0 {
		skipped = 0
	}
	gate.fgRateEMA += fgRateAlpha * (float64(gate.fgWindow) - gate.fgRateEMA)
	for i := 0; i < skipped; i++ {
		gate.fgRateEMA *= 1 - fgRateAlpha
	}
	if !gate.bucketStart.IsZero() {
		gate.persistWindow(gate.bucketStart)
	}
	gate.bucketStart = ws
	gate.fgWindow = 0
	gate.bucketUsed = 0
	gate.bucketUsedFg = 0
	gate.bucketUsedBg = 0
	gate.winRejectQuota = 0
	gate.winRejectHold = 0
	gate.winRejectBgReserve = 0
	gate.winRejectLatch = 0
	gate.winRejectYield = 0
	gate.winDrip = 0
	gate.winRetryAdmits = 0
	gate.winUsedBgPing = 0
	gate.winReservePeak = 0
	gate.winWaitersPeak = 0
}

// gateWindowStoreTimeout 是窗口行持久化的写上限：观测写不能拿无界
// ctx 进 SQLite——库卡死时协程泄漏比丢行更糟。整批（挂账重放行+新行）
// 共享一个上限，争用期里重放行不追加新的占用时长。
const gateWindowStoreTimeout = 30 * time.Second

// lockedStateStoreTimeout 是锁内 runtime_state 写的上限：闩/冷却的
// persist 与 clear 必须留在 mu/authMu 内与对侧操作同锁序化（解锁后
// 写删交错会把已清除的状态复活成幽灵行），不能像窗口行那样甩进协程；
// 但 SQLite 单写连接被批量事务占压时无界等待会冻结全部 lane 准入。
// 5s 远高于正常写耗时，超时按既有写失败路径只记日志丢持久化——内存
// 态已生效，重启至多丢一份簿记，下次限流/失败自然重建。pool.go 的
// persistCooldownLocked/deleteCooldownState 共用本上限。
const lockedStateStoreTimeout = 5 * time.Second

// gatePersistRetryCap 是窗口行写失败后的重放缓冲深度（每 lane）。
// 翻页率天然每分钟至多一次，深度 N = 失败行最多挂账 N 分钟——60 覆盖
// 小时级写停摆（prod 实测批量日志事务占死单写连接的争用波间歇绵延
// 逾一小时、连败峰值 5 行）；60 行 × ~160B 内存可忽略。选挂账缓冲而
// 非协程内退避重试：重放随下次翻页的单个协程走天然串行，争用期不会
// 养一批存活数分钟的协程轮流向已占死的写连接试写。缓冲是安全带而非
// 持久队列，溢出丢最老行并计 persistDropped。
const gatePersistRetryCap = 60

// persistWindow 把刚关闭窗口的明细账快照成行交给持久层，并把上轮写
// 失败挂账的行一并重放（取走即清，行集独占移交协程；(lane,
// window_start) 唯一索引 + INSERT OR IGNORE 使重放幂等）。行语义是
// 「闸门实际观察到关闭的窗口」：翻页只在流量/面板/保温触碰闸门时
// 发生，整窗未被触碰的空窗期不产生行（缺口=无观测而非零用量）。
// 写走一次性协程脱离 mu：SQLite 单写连接在批量日志事务/真空回收下
// 可被占压到秒级，持锁同步写会把全部准入堵在 busy_timeout 上；翻页
// 率天然有界（每 lane 每窗口至多一次），协程不会堆积。调用方须持 mu。
func (gate *rateGate) persistWindow(ws time.Time) {
	if gate.states == nil {
		return
	}
	row := &store.GateWindow{
		Lane:            gate.lane,
		WindowStart:     ws.Unix(),
		Quota:           gate.quota,
		UsedFg:          gate.bucketUsedFg,
		UsedBg:          gate.bucketUsedBg,
		UsedBgPing:      gate.winUsedBgPing,
		Drip:            gate.winDrip,
		ReservePeak:     gate.winReservePeak,
		WaitersPeak:     gate.winWaitersPeak,
		RejectQuota:     gate.winRejectQuota,
		RejectHold:      gate.winRejectHold,
		RejectBgReserve: gate.winRejectBgReserve,
		RejectLatch:     gate.winRejectLatch,
		RejectYield:     gate.winRejectYield,
		RetryAdmits:     gate.winRetryAdmits,
		FgRate:          gate.fgRateEMA,
	}
	gate.lastWindow = row
	pending := gate.pendingWindows
	gate.pendingWindows = nil
	rows := append(pending, row)
	gate.persistInFlight++
	if gate.persistDone == nil {
		gate.persistDone = make(chan struct{})
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), gateWindowStoreTimeout)
		defer cancel()
		for i, r := range rows {
			if err := gate.states.InsertGateWindow(ctx, r); err != nil {
				slog.Warn("gate window persist failed", "lane", gate.lane, "error", err)
				gate.stashWindows(rows[i:])
				break
			}
		}
		// 计数落定放在失败挂回之后：FlushPendingWindows 等到落定才取
		// 缓冲，挂回的行必须赶在它取走前入帐。
		gate.mu.Lock()
		gate.persistInFlight--
		if gate.persistInFlight == 0 {
			close(gate.persistDone)
			gate.persistDone = nil
		}
		gate.mu.Unlock()
	}()
}

// stashWindows 把未落库的窗口行挂回重放缓冲：写协程在 mu 外失败后
// 回调入队，自拿 mu（缓冲只在本方法与 persistWindow 的取走两处触及，
// 均在 mu 下）。首个失败即停手——争用期里同批后续行大概率同病，剩余
// 尾部整段挂回等下一窗口重试。超出深度的最老行丢弃并计 persistDropped。
func (gate *rateGate) stashWindows(rows []*store.GateWindow) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.persistFailures++
	gate.pendingWindows = append(gate.pendingWindows, rows...)
	for len(gate.pendingWindows) > gatePersistRetryCap {
		gate.pendingWindows = gate.pendingWindows[1:]
		gate.persistDropped++
	}
}

// FlushPendingWindows 在排空起点对未落库的窗口行做最后一轮同步冲刷：
// 先等在途持久化协程落定（失败行会挂回 pendingWindows，跳过等待直接
// 取缓冲会漏掉它们），再取走缓冲整批写。全程共用调用方的短 ctx——
// 排空有时限，写连接卡死不能拖住关停；预算内写不完的行与进程直接
// 退出一样丢弃（best-effort，不是持久化保证），失败行照常挂回缓冲
// 并计 persistFailures。
func (gate *rateGate) FlushPendingWindows(ctx context.Context) {
	if gate == nil || gate.states == nil {
		return
	}
	gate.mu.Lock()
	done := gate.persistDone
	gate.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return
		}
	}
	gate.mu.Lock()
	rows := gate.pendingWindows
	gate.pendingWindows = nil
	gate.mu.Unlock()
	for i, r := range rows {
		if err := gate.states.InsertGateWindow(ctx, r); err != nil {
			slog.Warn("gate window drain flush failed", "lane", gate.lane, "error", err)
			gate.stashWindows(rows[i:])
			return
		}
	}
}

// reserve 返回当前 bg 准入必须为预期 fg 需求让出的槽数：fg 速率 EMA
// 按可发区间剩余比例外推 + 已在排队的 fg + 固定安全边际。预留随可发
// 区间消耗线性衰减——窗口尾段退化成 waitersFg+margin，fg 没来的预测
// 需求自动归零，bg 吃到 work-conserving 的尾填。配额上限截断使预留
// 不会把 bg 永久封零以外的形式挤出。调用方须持 mu。
func (gate *rateGate) reserve(now time.Time, ws time.Time) int {
	if gate.quota <= 0 {
		return 0
	}
	usableLeft := max(ws.Add(gate.usable).Sub(now), 0)
	reserve := gate.reserveAt(usableLeft)
	// 峰值取样放评估点：准入与 stats 的每次评估都算数——「本窗预留
	// 到过多少」比「关窗那刻是多少」更能反映预留压力。
	gate.winReservePeak = max(gate.winReservePeak, reserve)
	return reserve
}

// reserveAt 按给定的可发区间剩余时长算预留槽数：fg 速率 EMA 外推 +
// 已在排队的 fg + 固定安全边际，封顶配额。usableLeft 取满段 usable
// 即投影下一窗口开放时刻的预留——expectedWaitLocked 用它判定 fg
// 饱和 lane 的下窗 bg 饥饿。调用方须持 mu。
func (gate *rateGate) reserveAt(usableLeft time.Duration) int {
	reserve := int(math.Ceil(gate.fgRateEMA*usableLeft.Seconds()/windowPeriod.Seconds())) + gate.waitersFg + gate.bgMargin
	return min(reserve, gate.quota)
}

// bgAllowance 是爬坡机制此刻为 bg 释放的放行额度：quota-reserve 按
// 可发区间经过时间线性放出——ceil 让首槽在窗口开放后立即可用、末尾
// 恰好收敛到 quota-reserve，既压住窗口开放瞬间的齐射又不损失吞吐。
// 只在 sendable 时被调用（死区内不评估）。调用方须持 mu。
func (gate *rateGate) bgAllowance(now, ws time.Time, reserve int) int {
	rampCap := gate.quota - reserve
	if rampCap <= 0 {
		return 0
	}
	return int(math.Ceil(float64(rampCap) * now.Sub(ws).Seconds() / gate.usable.Seconds()))
}

// restoreState 在启动时恢复未过期的冷却闩：滴灌时钟按间隔重排。
// 行缺失/损坏/已过期都按无闩处理并顺手清掉残留行。
func (gate *rateGate) restoreState() {
	if gate.states == nil || gate.stateKey == "" {
		return
	}
	value, ok, err := gate.states.GetState(context.Background(), gate.stateKey)
	if err != nil {
		slog.Warn("rate gate state read failed", "error", err)
		return
	}
	if !ok {
		return
	}
	var state gateState
	if err := json.Unmarshal([]byte(value), &state); err != nil || !state.LimitedUntil.After(gate.now()) {
		_ = gate.states.DeleteState(context.Background(), gate.stateKey)
		return
	}
	gate.limitedUntil = state.LimitedUntil
	gate.nextDrip = gate.now().Add(gate.dripInterval)
	gate.pushEvent(gateEventRestored, state.LimitedUntil, "")
	slog.Warn("rate gate latch restored from persisted state", "until", state.LimitedUntil.Format(time.RFC3339))
}

// persistState 把冷却闩截止时刻写入 runtime_state；写失败只记
// 日志——持久化是防重启续限的保险，不挡请求路径。
func (gate *rateGate) persistState(until time.Time) {
	if gate.states == nil || gate.stateKey == "" {
		return
	}
	data, _ := json.Marshal(gateState{LimitedUntil: until})
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	defer cancel()
	if err := gate.states.SetState(ctx, gate.stateKey, string(data)); err != nil {
		slog.Warn("rate gate state persist failed", "error", err)
	}
}

// clearState 在解闩后删除状态行；行不存在不算错误（DeleteState 空操作）。
func (gate *rateGate) clearState() {
	if gate.states == nil || gate.stateKey == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), lockedStateStoreTimeout)
	defer cancel()
	if err := gate.states.DeleteState(ctx, gate.stateKey); err != nil {
		slog.Warn("rate gate state delete failed", "error", err)
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
	gate.rollBucket(ws)
	stats := GateStats{
		Latched:          !gate.limitedUntil.IsZero() && now.Before(gate.limitedUntil),
		LatchCount:       gate.latchCount,
		DripCount:        gate.dripCount,
		RejectLatched:    gate.rejectLatched,
		RejectHold:       gate.rejectHold,
		RejectBgReserve:  gate.rejectBgReserve,
		RejectYield:      gate.rejectYield,
		WindowQuota:      gate.quota,
		WindowUsed:       gate.bucketUsed,
		WindowUsedFg:     gate.bucketUsedFg,
		WindowUsedBg:     gate.bucketUsedBg,
		WindowUsedBgPing: gate.winUsedBgPing,
		Sendable:         now.Sub(ws) < gate.usable,
		Waiters:          gate.waitersFg + gate.waitersBg,
		WaitersFg:        gate.waitersFg,
		WaitersBg:        gate.waitersBg,
		Reserve:          gate.reserve(now, ws),
		FgRate:           gate.fgRateEMA,
		LastWindow:       gate.lastWindow,
		PersistFailures:  gate.persistFailures,
		PersistDropped:   gate.persistDropped,
		PendingWindows:   len(gate.pendingWindows),
	}
	if gate.quota > 0 {
		open := ws
		next := ws.Add(windowPeriod)
		stats.WindowOpen = &open
		stats.WindowNext = &next
		if stats.Sendable {
			stats.PaceAllowance = gate.bgAllowance(now, ws, stats.Reserve)
		}
	}
	for i := 1; i <= gate.eventSize; i++ {
		stats.Events = append(stats.Events, gate.events[(gate.eventHead-i+gateEventCap)%gateEventCap])
	}
	if !gate.limitedUntil.IsZero() {
		until := gate.limitedUntil
		stats.LimitedUntil = &until
	}
	stats.LatchRanges = gate.latchRanges(now)
	stats.Wait = gate.waitView()
	return stats
}

// waitView 把样本环聚合成分类摘要：等待分位按墙钟毫秒计，结局计数
// 同步分出（拒绝再按 reason 细账）。调用方须持 mu。
func (gate *rateGate) waitView() *GateWait {
	if gate.waitSize == 0 {
		return nil
	}
	view := &GateWait{Samples: gate.waitSize, Evals: gate.waitEvals}
	all := make([]gateWaitSample, 0, gate.waitSize)
	var fg, bg []gateWaitSample
	for i := gate.waitSize; i >= 1; i-- {
		s := gate.waits[(gate.waitHead-i+gateWaitCap)%gateWaitCap]
		if s.outcome == gateWaitReject {
			if view.Rejects == nil {
				view.Rejects = map[string]int{}
			}
			view.Rejects[s.reason]++
		}
		all = append(all, s)
		if s.class == adapter.ClassBG {
			bg = append(bg, s)
		} else {
			fg = append(fg, s)
		}
	}
	since := all[0].at // 环按写入序遍历，首元素即最老样本
	view.Since = &since
	view.All = summarizeWaits(all)
	view.Fg = summarizeWaits(fg)
	view.Bg = summarizeWaits(bg)
	view.Totals.Fg = gate.waitTotalFg
	view.Totals.Bg = gate.waitTotalBg
	return view
}

// summarizeWaits 聚合一组等待样本：分位取最近秩（nearest-rank），
// 结局计数与样本同窗；err* 校准面只收带估计的放行样本（拒绝/取消的
// 实测等待被结局截断，与 est 预测的「到放行时长」不同口径）。
func summarizeWaits(samples []gateWaitSample) GateWaitSummary {
	n := len(samples)
	if n == 0 {
		return GateWaitSummary{}
	}
	waits := make([]time.Duration, n)
	errs := make([]int64, 0, n) // est−wait 毫秒
	var sum time.Duration
	var errSum int64
	out := GateWaitSummary{Count: n}
	for i, s := range samples {
		waits[i] = s.wait
		sum += s.wait
		switch s.outcome {
		case gateWaitReject:
			out.Rejects++
		case gateWaitCancel:
			out.Cancels++
		case gateWaitAdmit:
			if s.est >= 0 {
				e := (s.est - s.wait).Milliseconds()
				errs = append(errs, e)
				errSum += e
			}
		}
	}
	slices.Sort(waits)
	out.MeanMs = sum.Milliseconds() / int64(n)
	out.P50Ms = waits[int(float64(n-1)*0.5)].Milliseconds()
	out.P90Ms = waits[int(float64(n-1)*0.9)].Milliseconds()
	out.MaxMs = waits[n-1].Milliseconds()
	if m := len(errs); m > 0 {
		slices.Sort(errs)
		out.ErrCount = m
		out.ErrMeanMs = errSum / int64(m)
		out.ErrP10Ms = errs[int(float64(m-1)*0.1)]
		out.ErrP50Ms = errs[int(float64(m-1)*0.5)]
		out.ErrP90Ms = errs[int(float64(m-1)*0.9)]
	}
	return out
}

// gateAdmission 是闸门准入面的窄快照：闩态、可发区间与桶位四元供
// lane 健康判定用，ExpectedWait 是本类请求此刻进闸的期望排队时长
// 估计（号池选号的压力权重输入）。
type gateAdmission struct {
	Latched      bool
	Sendable     bool
	WindowQuota  int
	WindowUsed   int
	ExpectedWait time.Duration
}

// admissionSnapshot 读闸门准入面，读数与 stats 惰性结算后的口径等效但
// 不产生写：到期未清的陈闩按「now >= limitedUntil」自然读出非闩态，
// 翻过窗口的旧桶用量不结转——pool.healthy 是每请求热路径，不该为面板
// 视角付事件环复制与闩时段重放的成本。class 决定 ExpectedWait 按哪条
// 准入轨（fg 直放 / bg 预留+爬坡）估计。
func (gate *rateGate) admissionSnapshot(class string) gateAdmission {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	now := gate.now()
	ws := gate.windowStart(now)
	used := gate.bucketUsed
	if !ws.Equal(gate.bucketStart) {
		used = 0
	}
	sendable := now.Sub(ws) < gate.usable
	return gateAdmission{
		Latched:      !gate.limitedUntil.IsZero() && now.Before(gate.limitedUntil),
		Sendable:     sendable,
		WindowQuota:  gate.quota,
		WindowUsed:   used,
		ExpectedWait: gate.expectedWaitLocked(class, now, ws, used, sendable),
	}
}

// expectedWaitLocked 估算本类请求此刻进闸的期望排队时长，是 wait 准入
// 判定的静态估计版——同一本账（reserve/bgAllowance/waiters）但不含
// 睡眠与重查，只回答「现在到放行大概要多久」。串行折算只挂在超出可
// 并行放行余量的前队上：配额按整窗重置、开窗有槽即并行放行，实测
// waiters≤room 时开窗瞬间全队齐进（~0.2s），整队串行折算会高估
// ~waiters/quota×60s：
//   - 闩内 → 闩剩余：快败语义下不会真排，但选号视角它等价「这段时间
//     不可用」；
//   - 死区或桶满 → 到下一窗口开放；fg 前队只把超出整窗配额的部分按
//     窗速率折算追加，bg 开窗额度从 ~0 重新爬坡、没有并行齐射，前队
//     仍整队折算，另投影下一窗开放的预留，满预留（fg 需求持续饱和）
//     时追加一整窗——跨窗饥饿期睡醒重查也抢不到槽，实测排队到
//     bgMaxHold 被拒；
//   - fg 可发且桶有位 → 本窗余量即刻并行放行，只有超出余量的前队
//     等到翻窗后按整窗配额折算，封顶到下窗+一窗；
//   - bg 被预留/爬坡挡 → 负余量缺口与超出余量的前队都按爬坡释放速率
//     折算，封顶到下窗+一窗（usable 末额度定格，更深的队只能翻窗）。
//
// 调用方须持 mu。
func (gate *rateGate) expectedWaitLocked(class string, now, ws time.Time, used int, sendable bool) time.Duration {
	if now.Before(gate.limitedUntil) {
		return gate.limitedUntil.Sub(now)
	}
	if gate.quota <= 0 {
		return 0
	}
	waiters := gate.waitersFg
	if class == adapter.ClassBG {
		waiters = gate.waitersBg
	}
	toNext := ws.Add(windowPeriod).Sub(now)
	if !sendable || used >= gate.quota {
		// fg 翻窗即整窗配额并行放行——前队只有超出整窗配额的部分才
		// 按窗速率串行折算；bg 开窗爬坡额度从 ~0 重新释放、没有并行
		// 齐射，前队仍整队折算。
		excess := waiters
		if class != adapter.ClassBG {
			excess = max(waiters-gate.quota, 0)
		}
		wait := toNext + time.Duration(float64(excess)/float64(gate.quota)*float64(windowPeriod))
		// 下一窗开放即满预留时 bg 整窗无槽：睡醒者与重查都抢不到
		// 位，只能等到再下一窗竞争——fg 饱和 lane 上 bg 实测等待
		// ~120s，缺这项的估计（~toNext）低估约 4 倍。
		if class == adapter.ClassBG && gate.reserveAt(gate.usable) >= gate.quota {
			wait += windowPeriod
		}
		return wait
	}
	if class != adapter.ClassBG {
		if excess := waiters - (gate.quota - used); excess > 0 {
			return min(
				toNext+time.Duration(float64(excess)/float64(gate.quota)*float64(windowPeriod)),
				toNext+windowPeriod,
			)
		}
		return 0
	}
	reserve := gate.reserve(now, ws)
	room := min(gate.quota-reserve-used, gate.bgAllowance(now, ws, reserve)-gate.bucketUsedBg)
	rate := float64(max(gate.quota-reserve, 1)) / gate.usable.Seconds()
	// 余量内的前队即刻放行；room<0 时 waiters-room 自动并入缺口，
	// 与旧「waiters/rate + 缺口/rate」同式。
	return min(
		time.Duration(float64(max(waiters-room, 0))/rate*float64(time.Second)),
		toNext+windowPeriod,
	)
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
	} else if latched := !gate.limitedUntil.IsZero() && now.Before(gate.limitedUntil); latched {
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
//   - 闩外 fg：可发区间内配额未满立即放行；配额耗尽或在死区内睡到
//     下一窗口开放，预计等待超出剩余预算（累计上限 maxHold）同样
//     返回闸门拒绝；
//   - 闩外 bg：在 fg 规则上叠加动态预留与爬坡——bucketUsed+1 必须
//     不越过 quota-reserve（reserve 见同文件，随可发区间衰减），
//     且 bucketUsedBg+1 不越过爬坡额度（quota-reserve 按经过时间
//     线性释放），压住窗口开放瞬间的齐射。被预留或爬坡挡住时不睡
//     整窗，按 gateBgRecheck 短间隔重查吃中段让出/释放的槽；桶满/
//     死区与 fg 同形睡到下一窗口。排队预算用 bgMaxHold。
//   - 睡眠不做配额预约：窗口开放时睡醒者与新到者一起竞争，抢不到
//     的看到满桶按剩余预算决定再睡或快败——分钟粒度下排序公平性
//     不值得换复杂度。睡醒后不直接放行，回到循环首重新评估——
//     睡眠期间闩态可能已变。
func (gate *rateGate) wait(ctx context.Context) (err error) {
	if gate == nil {
		return nil
	}
	class := adapter.RequestClass(ctx)
	gc := adapter.GateContextFrom(ctx)
	retry := adapter.GateRetryFrom(ctx)
	bg := class == adapter.ClassBG
	sleeping := false // 标记本请求占着一个 waiters 名额
	// blocked 标记本请求是否曾占过 waiters 名额（真排过队）——wait
	// 累计账的 waits 口径；闩内/配额/让位快败与即时放行都不算「排过」。
	blocked := false
	// 等待预算约束「累计等待」而非「单次睡眠」：睡醒后要重新抢配额，
	// maxHold 超过一个窗口周期时逐睡校验会放行多轮睡眠，累计等待
	// 膨胀到 ~maxHold+60s——预算从进入起算，预计等待超出剩余额度
	// 即快败。预算参数须在锁内读（setParams 热更新），deadline 因此
	// 惰性到首个持锁循环才落定。
	var deadline time.Time
	// entered 用真实墙钟：gate.now 在测试里是假钟，而回执的 WaitMS
	// 是客户端可见的排队耗时。
	entered := time.Now()
	// est 记首次评估的期望排队估计，与实测等待组成 ew-vs-realized
	// 校准对；负值哨兵 = 首拍评估前取消（样本无估计）。后续循环重估
	// 不覆盖——校准语义是「进闸时刻的预测 vs 实际经历的排队」。
	est := time.Duration(-1)
	// 每次评估的结局记入等待样本环：defer 覆盖全部出口（放行/拒绝/
	// 取消），测量口径与回执 WaitMS 的 time.Since(entered) 一致。
	defer func() { gate.recordWait(class, err, entered, est, blocked) }()
	for {
		gate.mu.Lock()
		if sleeping {
			if bg {
				gate.waitersBg--
			} else {
				gate.waitersFg--
			}
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
			if bg {
				deadline = now.Add(gate.bgMaxHold)
			} else {
				deadline = now.Add(gate.maxHold)
			}
		}
		// 闩到期是自然失效而非解闩（没有成功帧证据）。
		gate.expireIfDue(now)
		ws := gate.windowStart(now)
		gate.rollBucket(ws)
		sendable := now.Sub(ws) < gate.usable
		if est < 0 {
			est = gate.expectedWaitLocked(class, now, ws, gate.bucketUsed, sendable)
		}
		if now.Before(gate.limitedUntil) {
			if sendable && !now.Before(gate.nextDrip) {
				// 探针槽空闲且在可发区间：放行并推进下一个槽。死区内
				// 不放探针——桶界附近的探针可能落进相邻真实桶白送计数。
				gate.nextDrip = now.Add(gate.dripInterval)
				gate.dripCount++
				gate.winDrip++
				gate.admitLocked(class, gc, now, ws, entered, retry)
				gate.mu.Unlock()
				return nil
			}
			retryAfter := gate.limitedUntil.Sub(now)
			gate.rejectLatched++
			gate.winRejectLatch++
			gate.mu.Unlock()
			rej := gateRejection(retryAfter, gateReasonLatch)
			// 闩内期望排队即闩剩余（expectedWaitLocked 闩分支同值）——
			// 本侧探针量随拒绝行落账，让位审计能复现当次评估现场。
			rej.GateProbeMS = retryAfter.Milliseconds()
			return rej
		}
		if gate.quota <= 0 {
			// 不限速放行仍是一次真实上游发送：照常记桶，窗口行的
			// used_*/retry_admits 在零配额口径下保持诚实。
			gate.admitLocked(class, gc, now, ws, entered, retry)
			gate.mu.Unlock()
			return nil
		}
		admit := sendable && gate.bucketUsed < gate.quota
		if bg && admit {
			// 预留检查只在桶未满时才有意义：桶满时 bg 与 fg 同走
			// 睡下一窗口的分支，不需要 reserve 读数。
			reserve := gate.reserve(now, ws)
			admit = gate.bucketUsed+1 <= gate.quota-reserve
			if admit {
				// 爬坡约束：bg 放行额度按可发区间经过时间线性释放，
				// 压住窗口开放瞬间的齐射——fg 在窗口前段到达看到的
				// 是半空的桶。被爬坡挡住与预留阻塞走同一条短间隔
				// 重查路径，桶尾吞吐不变。
				admit = gate.bucketUsedBg+1 <= gate.bgAllowance(now, ws, reserve)
			}
		}
		if admit {
			gate.admitLocked(class, gc, now, ws, entered, retry)
			gate.mu.Unlock()
			return nil
		}
		// 不可放行：按阻塞成因选睡眠时长与快败归因。
		//   - 桶满：睡到下一窗口；快败按 quota 类（Retry-After 报下窗）。
		//   - 死区：睡到下一窗口开放；快败按 hold 类。
		//   - bg 让路阻塞（sendable 且桶未满：预留不足或爬坡额度还没
		//     释放到它）：只睡 gateBgRecheck——预留随可发区间衰减、
		//     爬坡随经过时间释放，中段让出的槽即时可吃；快败仍按
		//     quota 类、Retry-After 报下一窗口（客户端按窗口节奏
		//     重试，不该按本地重查节奏轮询）。
		var wait time.Duration
		reason := gateReasonHold
		reserveBlocked := bg && sendable && gate.bucketUsed < gate.quota
		switch {
		case gate.bucketUsed >= gate.quota:
			wait = ws.Add(windowPeriod).Sub(now)
			reason = gateReasonQuota
		case !sendable:
			wait = ws.Add(windowPeriod).Sub(now)
		case reserveBlocked:
			wait = min(gateBgRecheck, ws.Add(gate.usable).Sub(now))
			reason = gateReasonQuota
		default:
			wait = ws.Add(windowPeriod).Sub(now)
		}
		// 让位快败：预计排队超 gateEarlyRelease 且号池里有兄弟 lane
		// 此刻能更快放行时，立即按 yield 快败交给 failover——在注定
		// 排长队的 lane 上把预算骑满再换号，只是把同一结局推迟一个
		// 排队预算（实测 bg 被吸收 p50≈119s，换号后 ~0.4s 建流）。
		// 谓词必须在锁外求值：兄弟的准入快照要拿它自己的闸锁，持本锁
		// 去取会与对侧同形让位构成 ABBA。答否后按先前算好的 wait 落回
		// 正常流程——解锁窗口内的状态变化与普通睡眠竞态同价，下一拍
		// 睡醒自会重估。
		// reserveBlocked 的 wait 只是 gateBgRecheck 重查节奏（≤4s）而
		// 非期望排队时长——拿它当触发会让探针永不被评估，桶未满的
		// 预留/爬坡饥饿能每 4s 重查烧满 bgMaxHold，兄弟 lane 空着也
		// 看不见。该分支触发改用 expectedWaitLocked 口径：room<0 的
		// 缺口按释放速率折算，与选号侧 expectedWait 同一本账。
		probeWait := wait
		if reserveBlocked {
			probeWait = gate.expectedWaitLocked(class, now, ws, gate.bucketUsed, sendable)
		}
		// siblingEW 是本次评估让位判定咨询到的兄弟最小期望排队；未咨询
		//（无谓词或探针未达阈值）保持零值，拒绝行按零值缺席。
		var siblingEW time.Duration
		if probeWait > gateEarlyRelease {
			if yield := adapter.GateYieldFrom(ctx); yield != nil {
				gate.mu.Unlock()
				var free bool
				siblingEW, free = yield()
				gate.mu.Lock()
				if free {
					gate.rejectYield++
					gate.winRejectYield++
					gate.mu.Unlock()
					retryAfter := wait
					if reason == gateReasonQuota {
						retryAfter = ws.Add(windowPeriod).Sub(now)
					}
					rej := gateRejection(retryAfter, gateReasonYield)
					rej.GateProbeMS = probeWait.Milliseconds()
					rej.GateSiblingEwMS = siblingEW.Milliseconds()
					return rej
				}
			}
		}
		if now.Add(wait).After(deadline) {
			if reserveBlocked {
				gate.rejectBgReserve++
				gate.winRejectBgReserve++
			} else {
				gate.rejectHold++
				// 窗口账按客户端看到的 reason 细分：桶满快败归
				// quota，死区等超预算归 hold——与 X-Gate-Reason 同源。
				if reason == gateReasonQuota {
					gate.winRejectQuota++
				} else {
					gate.winRejectHold++
				}
			}
			retryAfter := wait
			if reason == gateReasonQuota {
				retryAfter = ws.Add(windowPeriod).Sub(now)
			}
			gate.mu.Unlock()
			rej := gateRejection(retryAfter, reason)
			rej.GateProbeMS = probeWait.Milliseconds()
			rej.GateSiblingEwMS = siblingEW.Milliseconds()
			return rej
		}
		if bg {
			gate.waitersBg++
		} else {
			gate.waitersFg++
		}
		gate.winWaitersPeak = max(gate.winWaitersPeak, gate.waitersFg+gate.waitersBg)
		sleeping = true
		blocked = true
		gate.mu.Unlock()
		// 号池请求的长睡眠按让位阈值封顶：睡醒重估时会重问兄弟侧，
		// 兄弟 lane 排队中腾出余量也能在一拍内被接住——否则单次
		// 「睡到下一窗口」会把可换号的等待盲睡到底（fg 桶满盲睡
		// 可达 ~56s）。无谓词（单 lane/末位候选）保持原睡眠不加重查。
		timerWait := wait
		if adapter.GateYieldFrom(ctx) != nil && timerWait > gateEarlyRelease {
			timerWait = gateEarlyRelease
		}
		timer := time.NewTimer(timerWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			gate.mu.Lock()
			if bg {
				gate.waitersBg--
			} else {
				gate.waitersFg--
			}
			gate.mu.Unlock()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

// recordWait 把一次 wait 评估的实测结局写进样本环并推进进程期分类
// 累计账：结局按 err 归类——nil=放行、LocalGate Failure=拒绝
// （记 reason）、其余=ctx 取消。等待时长取墙钟（与 X-Gate-Wait-Ms
// 同口径）；假钟测试里睡的是真 timer，样本时长即真实经过。est 是
// 首次评估的期望排队估计（负值 = 未评估）；blocked 是本次评估是否
// 曾占 waiters 名额，决定累计账的 waits 列是否记账。调用方不得持
// mu——wait 各出口先解锁再返回，defer 才到这里取锁。
func (gate *rateGate) recordWait(class string, err error, entered time.Time, est time.Duration, blocked bool) {
	sample := gateWaitSample{at: entered, wait: time.Since(entered), est: est, class: class}
	var failure *llm.Failure
	switch {
	case err == nil:
		sample.outcome = gateWaitAdmit
	case errors.As(err, &failure) && failure.LocalGate:
		sample.outcome = gateWaitReject
		sample.reason = failure.GateReason
	default:
		sample.outcome = gateWaitCancel
	}
	gate.mu.Lock()
	gate.waits[gate.waitHead] = sample
	gate.waitHead = (gate.waitHead + 1) % gateWaitCap
	if gate.waitSize < gateWaitCap {
		gate.waitSize++
	}
	gate.waitEvals++
	total := &gate.waitTotalFg
	if class == adapter.ClassBG {
		total = &gate.waitTotalBg
	}
	total.Evals++
	if blocked {
		total.Waits++
	}
	total.WaitTotalMs += sample.wait.Milliseconds()
	gate.mu.Unlock()
}

// admitLocked 记账一次闸门放行并回填回执：桶总量与类别分列同增，
// fg 另计入本窗口 fg 需求样本（fgWindow——fgRateEMA 的输入）；retry
// 标记的续试重发同时计入 winRetryAdmits（used_* 的子集账）。调用方
// 须持 mu。
func (gate *rateGate) admitLocked(class string, gc *adapter.GateContext, now, ws, entered time.Time, retry bool) {
	gate.bucketUsed++
	if class == adapter.ClassBG {
		gate.bucketUsedBg++
	} else {
		gate.bucketUsedFg++
		gate.fgWindow++
	}
	if retry {
		gate.winRetryAdmits++
	}
	gate.noteVerdict(gc, class, now, ws, entered)
}

// noteVerdict 把放行快照经 GateContext 回传 HTTP 层——泵协程持锁写、
// 响应首字节写出前读，stamp 成 X-Gate-* 头。未挂接 GateContext 的
// 调用（测试、进程内直通）跳过。调用方须持 mu（读取桶账）。
func (gate *rateGate) noteVerdict(gc *adapter.GateContext, class string, now, ws, entered time.Time) {
	if gc == nil {
		return
	}
	gc.NoteAdmission(adapter.GateVerdict{
		Lane:           gate.lane,
		Class:          class,
		WindowUsed:     gate.bucketUsed,
		WindowQuota:    max(gate.quota, 0),
		WindowResetSec: int(math.Ceil(ws.Add(windowPeriod).Sub(now).Seconds())),
		WaitMS:         time.Since(entered).Milliseconds(),
	})
}

// tryAdmit 给后台流量（前缀保温 ping）一条不排队、不偷槽的准入路径：
// 闩内一律拒绝（不占滴灌探针槽——冷却期恰是最不该打上游的时刻）；
// 闩外可发区间内按 wait 的 bg 准入同一上界放行并计入 bg 桶计数——
// 桶位不越过 quota-reserve（fg 预留槽 ping 不占），bg 计数不越过
// 爬坡释放额度（同拍到期的多条目也不能齐射穿坡）。ping 不要求
// waiters 为空：bg 常驻排队不该饿死保温（缓存冷掉伤的是 fg），
// 被挡住时本轮跳过、下拍再试。与 wait 的区别：不睡眠、不预约、
// 不产事件。拒绝时 reason 按 tryAdmitSkip* / gateReason* 词表给出
// 阻塞成因（保温事件环的 skip.reason 直接取用），放行时为空串。
// 放行同时记 winUsedBgPing——本路径当前唯一调用方是保温
// sweep（pingEntry），放行即 ping；若接入非 ping 来源须先另立
// 口径拆分，used_bg_ping 列的语义依赖这条不变式。
func (gate *rateGate) tryAdmit() (bool, string) {
	if gate == nil {
		return true, ""
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	now := gate.now()
	gate.expireIfDue(now)
	// 计数桶随窗口边界滚动，与 wait 同一本账。
	ws := gate.windowStart(now)
	gate.rollBucket(ws)
	if now.Before(gate.limitedUntil) {
		return false, gateReasonLatch
	}
	if gate.quota <= 0 {
		// 与 wait 同口径：不限速的 ping 放行也是真实发送，计入 bg 桶账。
		gate.bucketUsed++
		gate.bucketUsedBg++
		gate.winUsedBgPing++
		return true, ""
	}
	if now.Sub(ws) >= gate.usable {
		return false, tryAdmitSkipDeadzone
	}
	reserve := gate.reserve(now, ws)
	if gate.bucketUsed+1 > gate.quota-reserve {
		return false, gateReasonQuota
	}
	if gate.bucketUsedBg+1 > gate.bgAllowance(now, ws, reserve) {
		return false, tryAdmitSkipPace
	}
	gate.bucketUsed++
	gate.bucketUsedBg++
	gate.winUsedBgPing++
	return true, ""
}

// noteUpstreamError 用上游失败刷新冷却闩；只有 resource_exhausted 与
// 限流有关，其余错误原样忽略。闩只延长不提前；只有闩被延长时才重置
// 滴灌时钟——截止未变的重复拒绝说明窗口未过，原探测节奏仍然成立，
// 重排滴灌只会无谓推迟下一枚探针。
func (gate *rateGate) noteUpstreamError(err error) {
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
		// 持久化须在锁内：解锁后 persist 可能与并发 release 的 clearState
		// 交错——clear 先跑、persist 后写，已解闩的截止时刻会作为
		// 死行残留，重启后复活成幽灵闩。
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
	gate.mu.Lock()
	latched := !gate.limitedUntil.IsZero()
	if latched {
		gate.pushEvent(gateEventReleased, gate.limitedUntil, "")
		gate.limitedUntil = time.Time{}
		gate.nextDrip = time.Time{}
		// clear 与上闩方的 persist 同锁序化：锁外执行会让「persist 晚于
		// clear」交错把已解闩的时刻写回状态行。
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
// 同一句式，客户端与日志看到的文案不变。reason 取 gateReason*
// 词表（latch/quota/hold），经响应头 X-Gate-Reason 透出。
func gateRejection(retryAfter time.Duration, reason string) *llm.Failure {
	seconds := int(math.Ceil(retryAfter.Seconds()))
	return &llm.Failure{
		Code:              "resource_exhausted",
		Message:           fmt.Sprintf("upstream message rate limited by local gate; your limit will reset in %d seconds.", seconds),
		LocalGate:         true,
		RetryAfterSeconds: seconds,
		GateReason:        reason,
	}
}
