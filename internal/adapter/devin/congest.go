// 本文件是模型级上游容量拥塞的本地吸收器：上游按 serving model 做
// 准入硬币翻转（"capacity issues with this serving model"，实测
// 2026-09-25 swe-2-medium ~75% 拒绝率持续 ~8min、双 lane 同墙、
// 无任何限流惩罚机制）。这类拒绝的特征决定了正确姿势不是躲开它——
// 换号打到同一堵墙、换模型改了客户端语义、快败把会话炸掉——而是
// 把发送排成细流：拥塞期间每个真实发送自己就是探针，在飞并发被
// 一个小上限闸住，墙松了自然打穿。
//
// 与 rate gate 的分工：gate 闩的是「账号维度限流惩罚」（有 reset
// hint、有冷却升级语义），这里的拥塞态闩的是「模型维度准入紧张」
// （无惩罚语义，纯节奏整形）——键、判据、目的都不相交。
package devin

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// congestionDecay 是拥塞态的滑动保鲜期：每次模型容量失败重武装，
// 这么久无新失败即认为墙已拆（恢复自由流，不再限并发）。用失败
// 衰减而非「首个成功即解除」——硬币翻转期成功与失败交错，单发
// 成功不代表容量回来。
const congestionDecay = 60 * time.Second

// congestionInflightCap 是拥塞期间该模型同时在飞的上游发送上限
// （本 adapter 维度）。容量拒绝是 ~1s 的快速 pre-content 拒绝，
// cap=2 意味着单 lane 对该模型的探测速率封顶 ~2/s——低于事故
// 当晚实测上游无惩罚承受的流量，同时让排队请求以可接受尾延迟
// 依次打穿。墙松后每发成功都是真实流量，不需要解闩仪式。
const congestionInflightCap = 2

// modelCongestion 按模型 uid 跟踪上游容量拥塞窗口，并在拥塞期给
// 发送侧提供有界并发槽。key 用路由定稿的 wire uid（binding.Model）：
// 别名收口到同一上游模型自然同键。所有发送（首发/自愈 resend/
// 流内 reopen）经 attemptRunner.send 单点过 acquire——拥塞只对
// 「还有下一发要打」的路径起作用，本身不产生任何额外上游调用。
type modelCongestion struct {
	mu     sync.Mutex
	states map[string]*congestionState
}

// congestionState 是单模型的拥塞簿记：congestedUntil 是滑动失效
// 的拥塞窗（只由容量失败延长），sem 是拥塞期生效的在飞上限
// （channel 计数信号量），episodes 是累计故障期数（首个失败开新
// 期）——仅用于日志归因，不进状态库：拥塞窗最长 decay 量级，
// 重启后由下一发真实失败重新发现，持久化没有收益。
type congestionState struct {
	congestedUntil time.Time
	sem            chan struct{}
	episodes       int
}

func newModelCongestion() *modelCongestion {
	return &modelCongestion{states: make(map[string]*congestionState)}
}

// acquire 在发送前做模型拥塞准入：未拥塞返回零开销的 no-op 释放
// 函数；拥塞中排队等一个在飞槽（槽数即探测并发上限，ctx 取消即
// 放弃）。槽只在真实发送瞬间持有——上游拒绝的 ~1s 快速回程即放，
// 等待者按到达序补位，天然 FIFO。返回的 release 必须调用且仅一次。
func (c *modelCongestion) acquire(ctx context.Context, model string) (func(), error) {
	c.mu.Lock()
	state := c.states[model]
	if state == nil || !time.Now().Before(state.congestedUntil) {
		c.mu.Unlock()
		return func() {}, nil
	}
	sem := state.sem
	c.mu.Unlock()
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, context.Cause(ctx)
	}
}

// noteFailure 记一次模型容量拒绝：该模型的拥塞窗按 decay 重武装。
// 新故障期（窗已失效后第一次失败）落一条告警——episode 起止在
// 日志里可辨，窗内的每次续武装不刷屏。
func (c *modelCongestion) noteFailure(model string) {
	if model == "" {
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	state := c.states[model]
	if state == nil {
		state = &congestionState{sem: make(chan struct{}, congestionInflightCap)}
		c.states[model] = state
	}
	until := now.Add(congestionDecay)
	if !now.Before(state.congestedUntil) {
		state.episodes++
		slog.Warn("model congested: upstream serving capacity rejection",
			"model", model, "episode", state.episodes,
			"congested_until", until.Format(time.RFC3339))
	}
	if until.After(state.congestedUntil) {
		state.congestedUntil = until
	}
}
