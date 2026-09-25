package devin

import (
	"time"

	"github.com/WncFht/devin2api/internal/llm"
)

// upstreamStallTimeout 是上游首帧确认前允许的最长静默（pre-frame0 档）：
// 上游收下请求到发出首个确认帧之间没有心跳帧覆盖，排队深度无实测上界，
// 窗口取保守值；超时即判定传输层已死（半开连接、上游挂死），按传输错误
// 收尾而不是无限等待。
// var 而非 const：测试临时缩短它来覆盖超时路径。
var upstreamStallTimeout = 120 * time.Second

// upstreamConfirmedStallTimeout 是首个上游帧到达后相邻帧之间允许的最长
// 静默（post-frame0 档）：prod 04 帧时标重建显示上游在 ~60.0s 帧静默
// 边界发 latency 心跳帧，26536 个健康帧间隔硬顶 60000.84ms——90s 是
// 心跳周期 + 到达余量，可证零误杀；60s 档与心跳同周期禁用。继续沿用
// pre-frame0 档只是让 stalled 重试每次多等 ~30s 检测延迟。
var upstreamConfirmedStallTimeout = 90 * time.Second

// upstreamNoProgressTimeout 是「无内容进度」期限：任意帧（含上游
// latency 活性帧）喂 stall 看门狗，但只有产出事件的帧喂它。上游实测
// 合法内容帧间隔上限 ~60s，而退化上游可能周期性发零事件帧无限续命
// （latency 心跳/元数据帧）——10min 是观察值 10 倍余量的兜底。
// 它只管产出首个事件之前；产出过之后走 deadlines.progress 的 post 档。
// var 而非 const：测试临时缩短它来覆盖超时路径。覆盖旋钮是
// devin.pre_event_no_progress_timeout_seconds。
var upstreamNoProgressTimeout = 10 * time.Minute

// upstreamPreEventSilenceCap 是首个可解码事件产出前允许的累计静默
// 上限：从首条上游流建立起算、跨 pre-content 重开累计，重开的新流只
// 继承剩余额度而不是重开一扇窗。prod 实测退化形态是上游收单后只发
// ack/心跳包络续命、永不产出事件——逐次重开的 10min 档会把死等拖到
// 远超客户端 ~300s 耐心（~150 例/30h 全部以 client_disconnected 收场
// 且烧满座位）。180s 给合法慢首字留足余量（实测 pre-event 静默上界远
// 低于它，60s 心跳帧都到不了两拍），到期按传输错误收尾释放 lane。
// var 供测试缩短。
var upstreamPreEventSilenceCap = 180 * time.Second

// defaultPostProgressTimeout 是 post-content 无进度档的默认值：上游在
// 工具调用参数阶段可静默计算 15-25min 只发心跳帧（实测本机案例
// 17min+ 静默后一次性下 args），pre-content 的 10min 档必误杀。45min
// 覆盖该形态并留一倍余量；覆盖旋钮是 devin.no_progress_timeout_seconds。
var defaultPostProgressTimeout = 45 * time.Minute

// upstreamTailGrace 是消费到 stopReason 之后等待流终止帧的宽限。
// 实测健康流的尾帧（usage/dim/endstream）在 stopReason 后 <1ms 到达；
// connect-go 读到 endstream envelope 还会排空 body 等传输 EOF，上游
// 不关 body 时会卡到看门狗——语义内容已齐时按正常收尾，不再等。
var upstreamTailGrace = 15 * time.Second

// maxStreamResumes 是单条响应允许的截断续传次数：每次续传都把整段
// 上下文重发再计费一遍，封顶防止对挂死上游反复烧配额。var 供测试
// 缩短。
var maxStreamResumes = 2

// streamDeadlines 收拢流看门狗的全部期限算术：构造期定型的档值与
// 静默上限锚点，方法全是「档值 + 运行期单调位」的纯函数——零 I/O
// 零锁，可无流构造直接测试。responseStream 只剩计时器持有与「何时
// 问哪个期限」的接线。
type streamDeadlines struct {
	// firstSentAt 是首条上游流建立的时刻锚点：pre-event 累计静默上限
	// （upstreamPreEventSilenceCap）从它起算且跨换流累计——tryReopen
	// 重开的新流只继承剩余额度，逐次重开不再各得一扇 10min 死等窗。
	// 零值（测试构造）不启用上限。
	firstSentAt time.Time
	// preProgress 是「产出首个事件之前」每段等待的无进度档：<=0 回落
	// upstreamNoProgressTimeout。
	preProgress time.Duration
	// postProgress 是「产出过内容之后」的无进度档：上游在工具调用参数
	// 阶段可静默计算 15-25min 只发心跳，pre 档必误杀这类合法静默。
	// <=0 同法回落。
	postProgress time.Duration
}

// stall 按流的语义状态给静默看门狗分档：上游首帧确认前没有心跳帧覆盖
// （排队深度无界），取 upstreamStallTimeout 保守窗；首个非错误帧到达后
// 帧间隔被上游 ~60s 心跳硬顶，收紧到 upstreamConfirmedStallTimeout；
// 消费到 stopReason 后只剩传输尾帧，再缩到 upstreamTailGrace。换流
// （swap/tryResume）复位 upstreamConfirmed，新流重新从无覆盖档计起。
func (d streamDeadlines) stall(hasStopReason, upstreamConfirmed bool) time.Duration {
	if hasStopReason {
		return upstreamTailGrace
	}
	if upstreamConfirmed {
		return upstreamConfirmedStallTimeout
	}
	return upstreamStallTimeout
}

// progressWindow 给无进度看门狗分档：首个事件产出前取 preProgress
// （还没产出就死等价值不大）；产出过之后取 postProgress——上游在工具
// 调用参数阶段可静默计算 15-25min 只发心跳，pre 档必误杀这类合法静默。
// 档值 <=0 回落 upstreamNoProgressTimeout，保持测试构造的既有语义。
func (d streamDeadlines) progressWindow(produced bool) time.Duration {
	if produced && d.postProgress > 0 {
		return d.postProgress
	}
	if !produced && d.preProgress > 0 {
		return d.preProgress
	}
	return upstreamNoProgressTimeout
}

// progress 是无进度看门狗本轮的武装时长：分档窗口之外，pre-event 档再被
// 累计静默上限截顶——额度从 firstSentAt 起算且跨换流累计，换流后新流
// 只继承剩余额度。post-event 与零锚点（firstSentAt 零值）不受上限影响。
func (d streamDeadlines) progress(produced bool, now time.Time) time.Duration {
	window := d.progressWindow(produced)
	if produced || d.firstSentAt.IsZero() {
		return window
	}
	if remain := d.firstSentAt.Add(upstreamPreEventSilenceCap).Sub(now); remain < window {
		return remain
	}
	return window
}

// progressBound 给 progressC 触发的错误归因取生效期限：触发点重算
// progress 会把已耗尽的累计静默上限残余误报成 ~0——上限已过时报上限
// 本身，其余回到分档窗口。
func (d streamDeadlines) progressBound(produced bool, now time.Time) time.Duration {
	if !produced && d.silenceCapExhausted(now) {
		return upstreamPreEventSilenceCap
	}
	return d.progressWindow(produced)
}

// silenceCapExhausted 判定 pre-event 累计静默上限是否已耗尽：耗尽后重开
// 只剩 ~0s 预算，新流活不过第一次看门狗评估，白烧一发上游发送。
func (d streamDeadlines) silenceCapExhausted(now time.Time) bool {
	return !d.firstSentAt.IsZero() && !now.Before(d.firstSentAt.Add(upstreamPreEventSilenceCap))
}

// capacityResendLimit 是单条流对模型容量拒绝的重开守卫上限：容量
// 吸收的语义终止界是调用方 ctx（pre-content 流不可脱钩，客户端断连
// 即杀泵），这个值只兜「上游无限拒绝 + ctx 不死」的失控循环——
// 512 发在 ~1s 拒绝回程下覆盖远超实测 episode 时长（~8min）的连打。
const capacityResendLimit = 512

// capacityAbsorbBudget 是同一流上容量吸收的墙钟预算：与次数守卫同
// 职责的另一轴——低 RT 环境下 512 发几分钟就烧完，慢链路上 8min
// episode 也可能吃不满次数，两轴取先到者收口。档值对齐
// resendLatchHold 量级，兜住实测 ~8min 事件窗后仍有余量。
const capacityAbsorbBudget = 15 * time.Minute

// retryPolicy 是两类流级重试（pre-content 整体重开 / 截断续传）的预算
// 计数与准入判定：计数随流存活，准入规则集中在这一个类型上——「什么
// 形态允许哪种重试」不再散进 tryReopen/tryResume 各自的布尔长句里。
type retryPolicy struct {
	// reopened 表示已经做过一次 pre-content 整体重试（上限 1 次）。
	reopened bool
	// resumes 是已执行的截断续传次数，封顶见 maxStreamResumes。
	resumes int
	// capacityReopens/capacitySince 是流内容量重开的失控守卫簿记：
	// 容量重开不吃一次性 reopened 额度也不吃静默上限，语义终止界
	// 是调用方 ctx；这对字段是「ctx 不死」场景的失控守卫，上限见
	// capacityResendLimit/capacityAbsorbBudget。
	capacityReopens int
	capacitySince   time.Time
}

// reopenable 判定 pre-content 整体重发的准入：额度未用、尚未产出内容
// （已产出只能续传）、有真实失败因或空轮续传诉求、累计静默上限未耗尽
// （耗尽时新流只剩 ~0s 预算，白烧一发上游发送——直接按原失败收尾）。
// 模型容量拒绝是独立准入档：它是一次性额度与静默上限都不适用的
// 故障类——拒绝本身是 ~1s 快回程的活跃应答而非死寂，episode（实测
// ~8min）也必然超 180s 上限；唯一保留的界是次数/墙钟守卫，兜住
// ctx 不死的失控场景。
func (r retryPolicy) reopenable(produced bool, cause error, continueEmpty, silenceExhausted bool, now time.Time) bool {
	if produced {
		return false
	}
	if cause != nil && llm.Classify(cause).ModelCapacity {
		if r.capacityReopens >= capacityResendLimit {
			return false
		}
		return r.capacitySince.IsZero() || now.Sub(r.capacitySince) < capacityAbsorbBudget
	}
	return !r.reopened && (cause != nil || continueEmpty) && !silenceExhausted
}

// resumable 判定截断续传的准入：已产出过内容（pre-content 走整体重发）、
// 语义未收口（hasStopReason 或本地停止序列命中，续传会在停止标记后再长
// 出一块内容）、无在飞工具调用（arguments 仍是截断 JSON，回传会被上游
// 参数校验拒掉，丢弃又让客户端已见的调用与上游历史分叉）、次数未封顶。
func (r retryPolicy) resumable(produced, sealed, toolInFlight bool) bool {
	return produced && !sealed && !toolInFlight && r.resumes < maxStreamResumes
}
