package devin

// eventRing 是定容事件环的共享骨架：gate 闩迁移事件、gate wait 结局样本、
// warm ping 结局、detached 生命周期事件四处簿记同构——定容数组 + 写头 +
// 在场计数，写满后覆盖最旧条目。骨架只管环算术；事件成形（打戳/标号）、
// 台账落库等逐站提交逻辑留在各调用方。环不自带同步：与宿主簿记同一把锁，
// 调用方须持自己的 mu。
type eventRing[T any] struct {
	events []T
	head   int
	size   int
}

// newEventRing 建定容环：capacity 即回看窗，写满后 push 覆盖最旧条目。
func newEventRing[T any](capacity int) eventRing[T] {
	return eventRing[T]{events: make([]T, capacity)}
}

// push 写一条并推进写头；写满后覆盖最旧条目。
func (r *eventRing[T]) push(e T) {
	r.events[r.head] = e
	r.head = (r.head + 1) % len(r.events)
	if r.size < len(r.events) {
		r.size++
	}
}

// recent 按新在前序拷贝出全部在场条目——stats 事件段的展示序。
func (r *eventRing[T]) recent() []T {
	out := make([]T, 0, r.size)
	for i := 1; i <= r.size; i++ {
		out = append(out, r.events[(r.head-i+len(r.events))%len(r.events)])
	}
	return out
}

// each 按写入序（旧到新）回放全部在场条目：时序还原（闩时段）与全量
// 聚合（wait 分位）要的是事件发生的顺序，不是展示序。
func (r *eventRing[T]) each(f func(T)) {
	for i := r.size; i >= 1; i-- {
		f(r.events[(r.head-i+len(r.events))%len(r.events)])
	}
}

// ordered 按写入序（旧到新）拷贝出全部在场条目：调用方持锁快照后放
// 到锁外做回放/聚合——与 each 同序，但产物是切片而非逐条回调。
func (r *eventRing[T]) ordered() []T {
	out := make([]T, 0, r.size)
	r.each(func(e T) { out = append(out, e) })
	return out
}
