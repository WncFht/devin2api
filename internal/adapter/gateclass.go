// 本文件定义下游请求类（class）在 adapter 边界上的 ctx 传播与闸门
// 准入回执。class 由 authtoken.Token 声明（fg 前台/bg 后台），app 层
// 在鉴权后挂进请求 ctx，闸门 wait 路径读取并据此类别化准入；
// GateVerdict 是闸门放行时回填的遥测快照，app 层在首字节写出前
// 把它 stamp 成 X-Gate-* 响应头。
package adapter

import (
	"context"
	"sync/atomic"
)

// 请求类取值：fg 是默认（含未注入 ctx 的请求——存量与匿名流量全部
// 按前台处理）；bg 在闸门准入上叠加动态预留约束。词表与
// authtoken.Class* 是同一套 wire 常量。
const (
	ClassFG = "fg"
	ClassBG = "bg"
)

// gateCtxKey 是 GateContext 在请求 ctx 里的挂接键（空结构体类型做键，
// 不与字符串键空间碰撞）。
type gateCtxKey struct{}

// gateYieldKey 是闸门让位谓词在请求 ctx 里的挂接键。
type gateYieldKey struct{}

// gateRetryKey 是「本次发送是同一请求的续试重发」标记在 ctx 里的挂接键。
type gateRetryKey struct{}

// GateVerdict 是一次闸门放行的遥测快照：app 层据此写 X-Gate-* 头。
type GateVerdict struct {
	// Lane 是实际服务的 lane 名（单 lane/未命名部署为空）。
	Lane string
	// Class 是本次准入使用的请求类。
	Class string
	// WindowUsed/WindowQuota 是放行后该 lane 的桶用量与配额。
	WindowUsed  int
	WindowQuota int
	// WindowResetSec 是到下一窗口开放的秒数。
	WindowResetSec int
	// WaitMS 是本请求在闸内排队的累计毫秒（重试多次过闸时累加）。
	WaitMS int64
}

// GateContext 是请求级闸门上下文：携带请求类进入闸门，放行回执经
// 原子指针回传给 HTTP 层——泵协程写、HTTP goroutine 在首字节写出时读。
type GateContext struct {
	Class   string
	verdict atomic.Pointer[GateVerdict]
}

// WithGateContext 把请求类挂进 ctx 并返回回执句柄；class 为空或未知
// 值一律归一成 fg——下游任何注入路径都不会改变 fg 语义。
func WithGateContext(ctx context.Context, class string) (context.Context, *GateContext) {
	gc := &GateContext{Class: class}
	if gc.Class != ClassBG {
		gc.Class = ClassFG
	}
	return context.WithValue(ctx, gateCtxKey{}, gc), gc
}

// GateContextFrom 取回 ctx 上的闸门上下文；未挂接返回 nil——闸门与
// 遥测消费方都按 nil 容忍（等价于 fg 无回执）。
func GateContextFrom(ctx context.Context) *GateContext {
	gc, _ := ctx.Value(gateCtxKey{}).(*GateContext)
	return gc
}

// WithGateYield 把「兄弟 lane 此刻能否更快放行」的活探针挂进 ctx：
// 号池在每次 lane 尝试前按剩余候选装填，闸门预计排队将超让位阈值时
// 问一次，答真即提前快败把请求交给 failover。谓词在闸锁外求值——
// 实现不得依赖调用方持有任何锁（它会去拿兄弟 lane 自己的闸锁）。
func WithGateYield(ctx context.Context, yield func() bool) context.Context {
	return context.WithValue(ctx, gateYieldKey{}, yield)
}

// GateYieldFrom 取回 ctx 上的让位谓词；未挂接返回 nil——单 lane 与
// 无池部署下闸门按 nil 跳过让位判定。
func GateYieldFrom(ctx context.Context) func() bool {
	yield, _ := ctx.Value(gateYieldKey{}).(func() bool)
	return yield
}

// WithGateRetry 标记本次发送是同一请求在同 lane 上的续试重发
// （reopen/续轮/凭据自愈/内层瞬时重试的再发送）：闸门把放行计入窗口
// retry_admits 账，与首发区分。号池 failover 后新 lane 的首发不挂——
// 对那条 lane 它不是续试。
func WithGateRetry(ctx context.Context) context.Context {
	return context.WithValue(ctx, gateRetryKey{}, true)
}

// GateRetryFrom 取回 ctx 上的续试标记；未挂接返回 false。
func GateRetryFrom(ctx context.Context) bool {
	retry, _ := ctx.Value(gateRetryKey{}).(bool)
	return retry
}

// RequestClass 返回本请求在闸门语义里的类别；未挂接按 fg 处理。
func RequestClass(ctx context.Context) string {
	if gc := GateContextFrom(ctx); gc != nil {
		return gc.Class
	}
	return ClassFG
}

// NoteAdmission 记录一次闸门放行：快照按最后状态整体替换，排队耗时
// 在重试多次过闸的调用间累计。
func (gc *GateContext) NoteAdmission(v GateVerdict) {
	if prev := gc.verdict.Load(); prev != nil {
		v.WaitMS += prev.WaitMS
	}
	gc.verdict.Store(&v)
}

// Verdict 读最近一次放行快照；未放行过返回 nil。
func (gc *GateContext) Verdict() *GateVerdict {
	return gc.verdict.Load()
}
