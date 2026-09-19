// 本文件是面板侧统一的 TTL 快照缓存：此前 models/providers/
// modelStatuses/usage/status 五处各写了一份变体，其中 providers 与
// modelStatuses 把写锁横在 ≤610s 的上游 RPC 上——一个慢接口会堵死
// 同缓存全部读，且锁内等待不吃 ctx。这里收敛成一份实现：mutex 只守
// snap/at/inflight 簿记，真实拉取永远发生在锁外。
package ccpanel

import (
	"context"
	"sync"
	"time"
)

const (
	// catalogCacheTTL 是模型目录/供应商/模型状态三个上游面的缓存寿命：
	// 它们不经常变化，长 TTL 显著降低上游压力。
	catalogCacheTTL = 5 * time.Minute
	// usageCacheTTL 是 /admin/usage 聚合快照的新鲜窗口：面板按轮询
	// 消费，过期不阻塞——有旧快照直接发旧值并后台重算。
	usageCacheTTL = 5 * time.Second
	// statusCacheTTL 是 /admin/status 聚合快照的缓存寿命：面板按页面
	// 加载与轮询消费，秒级陈旧无感。
	statusCacheTTL = 30 * time.Second
	// swrRefreshTimeout 是 swr 后台刷新的超时上界：刷新脱离调用方
	// ctx，不给上限的 fetch 一旦卡死会让 inflight 永不清除——后续
	// Get 永远回旧快照且不再触发重拉。上界把永漏降级为「本次刷新
	// 失败、下一趟 Get 重试」；取值压过 sqlite busy_timeout（30s），
	// 正常慢查询仍由 fetch 自己的超时先报。
	swrRefreshTimeout = 60 * time.Second
	// fetchFailCooldown 是 fetch 失败后的冷却窗：窗内 Get 有旧快照回
	// 旧快照、无快照回缓存的错误——上游/库持续故障期，面板轮询的重试
	// 频率不该等于请求频率。取值对齐最短 TTL（usage 5s）：恢复延迟
	// 最多一个轮询周期，故障期重试被折到冷却粒度。
	fetchFailCooldown = 5 * time.Second
)

// ttlCache 是「TTL 快照 + singleflight」缓存。swr=false 时过期调用方
// 同步等一趟拉取，并发等待方挂 done channel 收敛成单次 fetch；
// swr=true（stale-while-revalidate）时过期但有旧快照直接回旧值、
// 由首个过期调用方起后台刷新——页面扇出的并发请求只吃一趟计算。
type ttlCache[T any] struct {
	mu    sync.Mutex
	ttl   time.Duration
	swr   bool
	fetch func(ctx context.Context) (T, error)
	// snap/at 是最近一趟成功 fetch 的产物；at 为零值表示还没有任何
	// 快照（swr 的旧值语义也以 at 判定，空结果同样占住 TTL）。
	snap T
	at   time.Time
	// failUntil/failErr 是最近一趟失败 fetch 的冷却窗：窗内 Get 不再
	// 触发新拉取——有旧快照回旧快照，无快照回缓存的错误。
	failUntil time.Time
	failErr   error
	// inflight 非空表示有 fetch 在途（singleflight 的 done channel），
	// 关闭即完成信号。
	inflight chan struct{}
}

func newTTLCache[T any](ttl time.Duration, swr bool, fetch func(context.Context) (T, error)) ttlCache[T] {
	return ttlCache[T]{ttl: ttl, swr: swr, fetch: fetch}
}

// Get 返回快照：TTL 内直接命中；过期按 swr 语义回旧值或同步等拉取。
// 等待方吃 ctx 取消；真正跑 fetch 的那趟脱离调用方取消——快照是
// handler 级共享状态，一个断连不该掐死共用的拉取。
func (c *ttlCache[T]) Get(ctx context.Context) (T, error) {
	for {
		c.mu.Lock()
		hasSnap := !c.at.IsZero()
		if hasSnap && time.Since(c.at) < c.ttl {
			snap := c.snap
			c.mu.Unlock()
			return snap, nil
		}
		if time.Now().Before(c.failUntil) {
			if hasSnap {
				snap := c.snap
				c.mu.Unlock()
				return snap, nil
			}
			err := c.failErr
			c.mu.Unlock()
			var zero T
			return zero, err
		}
		if c.inflight != nil {
			done := c.inflight
			snap := c.snap
			c.mu.Unlock()
			if c.swr && hasSnap {
				return snap, nil // 刷新由在途者收尾
			}
			select {
			case <-done:
				continue
			case <-ctx.Done():
				var zero T
				return zero, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.inflight = done
		snap := c.snap
		c.mu.Unlock()
		if c.swr && hasSnap {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), swrRefreshTimeout)
				defer cancel()
				_ = c.run(ctx, done)
			}()
			return snap, nil
		}
		// WithoutCancel：调用方断连不掐死共享拉取，fetch 自带的
		// client/超时上限仍兜底。
		if err := c.run(context.WithoutCancel(ctx), done); err != nil {
			var zero T
			return zero, err
		}
	}
}

// run 执行一趟 fetch、刷新快照并关闭 done 通知等待方。先写快照再
// close：被唤醒的等待方回到循环立刻读到新值；失败时保持旧快照并
// 开一扇短冷却窗——窗内醒来的等待方拿旧快照/缓存错误，不再顺位
// 成为新拉取者，上游故障期的重试被折到冷却粒度而非请求粒度。
func (c *ttlCache[T]) run(ctx context.Context, done chan struct{}) error {
	snap, err := c.fetch(ctx)
	c.mu.Lock()
	if err == nil {
		c.snap = snap
		c.at = time.Now()
		c.failUntil = time.Time{}
		c.failErr = nil
	} else {
		c.failUntil = time.Now().Add(fetchFailCooldown)
		c.failErr = err
	}
	close(done)
	c.inflight = nil
	c.mu.Unlock()
	return err
}
