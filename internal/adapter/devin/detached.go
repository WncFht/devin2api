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
// completed 覆盖客户端退避重试的全部窗口；failed 只留短窗回答「同键
// 重试吃缓存终态还是走新上游」。
var (
	detachedRunningTTL   = 45 * time.Minute
	detachedCompletedTTL = 60 * time.Minute
	detachedFailedTTL    = 5 * time.Minute
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
	// stream 是后台泵持有的上游流句柄：running 态被淘汰时经它 cancel
	// 杀掉泵（TTL 兜底之外的容量淘汰路径）。
	stream *responseStream
}

// append 追加一条已产出事件并广播给挂接方。
func (entry *detachedEntry) append(event llm.ResponseEvent) {
	entry.mu.Lock()
	entry.events = append(entry.events, event)
	close(entry.notify)
	entry.notify = make(chan struct{})
	entry.mu.Unlock()
}

// finish 在后台泵读到流终态时定态：末帧是 error 记 failed（可重放性按
// 失败分类——上游责任/取消类瞬态失败的同键重试应走新上游而非吃缓存
// 终态），否则 completed；expiresAt 按态重置。
func (entry *detachedEntry) finish() {
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
}

// newDetachedRegistry 创建空缓存。
func newDetachedRegistry() *detachedRegistry {
	return &detachedRegistry{entries: make(map[string]*detachedEntry)}
}

// lookup 查可挂接条目：running/completed 恒可挂；failed 只在可重放时
// 挂（瞬态失败返回 nil 让调用方走新上游）；过期即逐。
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
	entry.mu.Unlock()
	if expired {
		registry.evictLocked(key, entry)
		return nil
	}
	if !replayable {
		return nil
	}
	return entry
}

// admit 把条目按 key 登记进缓存并接管其后台泵的生命周期。容量触顶先
// 扫过期项，再逐最老的 running 条目。同键旧条目（前一次同请求脱钩的
// 残骸）先逐出再登记——两条同键后台泵同跑是纯粹的配额浪费。
func (registry *detachedRegistry) admit(key string, entry *detachedEntry, stream *responseStream) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	entry.mu.Lock()
	entry.admittedAt = time.Now()
	entry.expiresAt = entry.admittedAt.Add(detachedRunningTTL)
	entry.stream = stream
	entry.mu.Unlock()
	if old := registry.entries[key]; old != nil {
		registry.evictLocked(key, old)
	}
	if len(registry.entries) >= detachedMaxEntries {
		now := time.Now()
		for k, e := range registry.entries {
			e.mu.Lock()
			expired := now.After(e.expiresAt)
			e.mu.Unlock()
			if expired {
				registry.evictLocked(k, e)
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
			registry.evictLocked(oldestKey, registry.entries[oldestKey])
		}
	}
	registry.entries[key] = entry
}

// evictLocked 摘出条目：running 条目同时 cancel 后台泵——泵的下一次
// Recv 会看到帧通道关闭，按 EOF 收尾并 finish 条目（挂接方拿到一个
// 截断但干净的终态）。
func (registry *detachedRegistry) evictLocked(key string, entry *detachedEntry) {
	delete(registry.entries, key)
	entry.mu.Lock()
	stream := entry.stream
	running := entry.state == detachedRunning
	entry.mu.Unlock()
	if running && stream != nil {
		stream.cancel()
	}
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
// 调试日志同一份规范化中间表示），剔除会话标识与审计位（重试计数/
// dropped 清单不参与语义等价），model 覆盖为别名/路由解析后的真实
// uid——同一段提示词换个客户端模型名殊途同归。
// json.Marshal 对 map 键排序，投影值全是已规范化结构，哈希确定。
func detachedRequestKey(request llm.RequestMessages, model string) string {
	projection := debuglog.RequestMessagesProjection(request)
	delete(projection, "session_key")
	delete(projection, "dropped_items")
	projection["model"] = model
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
