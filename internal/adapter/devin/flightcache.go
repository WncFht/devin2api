package devin

import (
	"context"
	"sync"
)

// flightCall 是一次在飞调用的共享句柄：done 关闭前 result/err 已写定
// （close 建立 happens-before），同键等待者直接取共享结果——失败也随
// 结果广播，不产生逐个重试的串行风暴。
type flightCall[V any] struct {
	done   chan struct{}
	result V
	err    error
}

// flightCache 是「同键并发收敛为单次上游调用」的共享骨架：assignModel
// 的会话级解析与 ListModels 的目录拉取同构——等待者收 done 后直接读
// flight 上的共享结果，不各发一次 RPC。骨架不自带内锁：flights 登记表与
// 宿主的键级缓存（assignments/models）同属一个锁域，hit/commit 回调读写
// 的就是宿主状态——另立内锁会让同一份状态在两个锁下互相撕裂。mu 由宿主
// 在构造点（mutex 声明旁）一次性赋值配对，run/reset 不再逐次传参；
// mu 未就位前不可 run。
type flightCache[K comparable, V any] struct {
	mu      sync.Locker
	flights map[K]*flightCall[V]
}

// run 执行一次键级收敛调用。hit 在锁内快查宿主缓存，命中即返回；同键已有
// 在飞调用时等 done（吃调用方 ctx 可中途退出，拉取方不受影响）；否则注册
// 自己为拉取方，fetch 在锁外进行且 detach 自调用方 ctx——结果是键级
// 共享状态，一个客户端断连不该掐死全体等待者共享的调用。提交只在本
// flight 仍是注册项时生效：宿主 reset（换端点/换凭据）后在飞结果落进
// 缓存就是陈旧数据借尸还魂。
func (fc *flightCache[K, V]) run(ctx context.Context, key K,
	hit func() (V, error, bool),
	fetch func(context.Context) (V, error),
	commit func(context.Context, V, error) (V, error),
) (V, error) {
	fc.mu.Lock()
	if v, err, ok := hit(); ok {
		fc.mu.Unlock()
		return v, err
	}
	if f := fc.flights[key]; f != nil {
		fc.mu.Unlock()
		select {
		case <-f.done:
			return f.result, f.err
		case <-ctx.Done():
			var zero V
			return zero, ctx.Err()
		}
	}
	if fc.flights == nil {
		fc.flights = make(map[K]*flightCall[V])
	}
	f := &flightCall[V]{done: make(chan struct{})}
	fc.flights[key] = f
	fc.mu.Unlock()

	v, err := fetch(context.WithoutCancel(ctx))

	fc.mu.Lock()
	if fc.flights[key] == f {
		delete(fc.flights, key)
		v, err = commit(ctx, v, err)
	}
	// 结果广播不随注册项存废：被 reset 的在飞调用不缓存结果，但等待者
	// 与调用方仍拿当次 fetch 的真实结局（未缓存≠未发生）。
	f.result, f.err = v, err
	fc.mu.Unlock()
	close(f.done)
	return v, err
}

// reset 清空在飞登记表：在飞调用的提交以「flight 仍是注册项」为前提，
// 清表即让旧端点/旧凭据在飞的结果不落缓存。调用方须持构造时绑定的
// 那把锁。
func (fc *flightCache[K, V]) reset() {
	clear(fc.flights)
}
