// 本文件验证模型拥塞簿记：未拥塞直通零开销、拥塞期在飞槽按上限放行
// （FIFO 补位）、失败滑动窗到期恢复自由流、等待可被 ctx 取消。
package devin

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCongestionAcquireFreeWhenIdle(t *testing.T) {
	subject := newModelCongestion()
	release, err := subject.acquire(context.Background(), "m")
	if err != nil {
		t.Fatalf("idle acquire: %v", err)
	}
	release()
}

// TestCongestionCap 验证拥塞期在飞并发被 cap 闸住且按到达序补位：
// 占满全部槽后再来的等待者阻塞，放一槽只放进一个。
func TestCongestionCap(t *testing.T) {
	subject := newModelCongestion()
	subject.noteFailure("m")

	// 占满全部槽位。
	releases := make([]func(), 0, congestionInflightCap)
	for i := 0; i < congestionInflightCap; i++ {
		release, err := subject.acquire(context.Background(), "m")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, release)
	}

	// 第 cap+1 个等待者必须阻塞；给它一个短时限证明它确实在等。
	waiting := make(chan func(), 1)
	go func() {
		release, err := subject.acquire(context.Background(), "m")
		if err == nil {
			waiting <- release
		}
	}()
	select {
	case <-waiting:
		t.Fatal("waiter acquired a slot while all were held")
	case <-time.After(50 * time.Millisecond):
	}

	// 放一槽 → 等待者补位成功；再放完剩余。
	releases[0]()
	select {
	case release := <-waiting:
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("released slot was not refilled by the waiter")
	}
	for _, release := range releases[1:] {
		release()
	}
}

// TestCongestionInflightBound 并发压测在飞上限：N 个等待者抢槽，
// 持有计数峰值不得超过 congestionInflightCap。
func TestCongestionInflightBound(t *testing.T) {
	subject := newModelCongestion()
	subject.noteFailure("m")
	var held atomic.Int32
	var peak atomic.Int32
	var wg sync.WaitGroup
	const waiters = 16
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := subject.acquire(context.Background(), "m")
			if err != nil {
				return
			}
			cur := held.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			held.Add(-1)
			release()
		}()
	}
	wg.Wait()
	if got := peak.Load(); got > int32(congestionInflightCap) {
		t.Fatalf("in-flight peak = %d, want <= %d", got, congestionInflightCap)
	}
}

// TestCongestionDecay 验证失败滑动窗到期即恢复自由流——不需要显式
// 解闩，窗过期后 acquire 回到直通档。
func TestCongestionDecay(t *testing.T) {
	subject := newModelCongestion()
	subject.noteFailure("m")
	subject.mu.Lock()
	subject.states["m"].congestedUntil = time.Now().Add(-time.Second)
	subject.mu.Unlock()
	release, err := subject.acquire(context.Background(), "m")
	if err != nil {
		t.Fatalf("expired window should pass through: %v", err)
	}
	release()
}

// TestCongestionRearm 验证窗内新失败把拥塞窗向后推——episode 持续
// 期不提前松绑。
func TestCongestionRearm(t *testing.T) {
	subject := newModelCongestion()
	subject.noteFailure("m")
	first := subject.states["m"].congestedUntil
	subject.noteFailure("m")
	if !subject.states["m"].congestedUntil.After(first.Add(-time.Second)) {
		t.Fatal("re-armed window should extend, not shrink")
	}
	if subject.states["m"].episodes != 1 {
		t.Fatalf("failures inside one window must stay one episode, got %d", subject.states["m"].episodes)
	}
}

// TestCongestionAcquireCtxCancel 验证槽位占满时的等待随 ctx 取消
// 折返——客户端断连不悬挂。
func TestCongestionAcquireCtxCancel(t *testing.T) {
	subject := newModelCongestion()
	subject.noteFailure("m")
	releases := make([]func(), 0, congestionInflightCap)
	for i := 0; i < congestionInflightCap; i++ {
		release, err := subject.acquire(context.Background(), "m")
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := subject.acquire(ctx, "m"); err == nil {
		t.Fatal("blocked acquire must fail on ctx cancel")
	}
}

// TestCongestionModelIsolation 验证拥塞按模型键隔离：m 拥塞不影响
// other 的直通。
func TestCongestionModelIsolation(t *testing.T) {
	subject := newModelCongestion()
	subject.noteFailure("m")
	release, err := subject.acquire(context.Background(), "other")
	if err != nil {
		t.Fatalf("unrelated model should pass through: %v", err)
	}
	release()
}
