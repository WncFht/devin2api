// 令牌仓行为测试：锚定滚动窗口（5h/weekly）记账与过期、Ensure 播种幂等、
// RPM 分钟桶、匿名通道行（sha256("")）的解析矩阵。
package authtoken

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// openDB 开一个临时 sqlite 库。令牌仓的「重启」用同一个 *store.Store
// 再跑 New 即可——瞬态字段（rpm 计数/inflight）不持久化，重新水合
// 与重开文件等价。
func openDB(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(openDB(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAnchoredWindowsChargeAndRollover 验证 5h/weekly 锚定滚动窗口：
// 首次记账落锚，窗口内累加，锚点过期后用量按 0 计、下一笔记账重锚。
func TestAnchoredWindowsChargeAndRollover(t *testing.T) {
	store := newStore(t)
	tok, created, err := store.Ensure("k", &Token{
		Description: "t", IsActive: true, MaxConcurrency: 5,
		Cost5hLimitMicroUSD:     10_000_000,
		CostWeeklyLimitMicroUSD: 10_000_000,
	})
	if err != nil || !created {
		t.Fatalf("Ensure = (%v, %v), want created", err, created)
	}

	// 快照语义：AddResult 写仓内对象，断言前用 Get 重新取快照。
	snap := func() *Token {
		got, ok := store.Get(tok.ID)
		if !ok {
			t.Fatal("token missing from store")
		}
		return got
	}

	charge := Result{StatusCode: 200, CostUSD: 1.5}
	store.AddResult(tok.ID, charge)
	tok = snap()
	if tok.Cost5hAnchor <= 0 || tok.CostWeeklyPeriodStart <= 0 {
		t.Fatalf("anchors not set: 5h=%d weekly=%d", tok.Cost5hAnchor, tok.CostWeeklyPeriodStart)
	}
	if tok.Cost5hUsedMicroUSD != 1_500_000 || tok.CostWeeklyUsedMicroUSD != 1_500_000 {
		t.Fatalf("used = 5h:%d weekly:%d, want 1500000 each", tok.Cost5hUsedMicroUSD, tok.CostWeeklyUsedMicroUSD)
	}
	anchor5h, anchorWeekly := tok.Cost5hAnchor, tok.CostWeeklyPeriodStart

	store.AddResult(tok.ID, charge)
	tok = snap()
	if tok.Cost5hUsedMicroUSD != 3_000_000 || tok.CostWeeklyUsedMicroUSD != 3_000_000 {
		t.Fatalf("used after 2nd charge = 5h:%d weekly:%d, want 3000000 each", tok.Cost5hUsedMicroUSD, tok.CostWeeklyUsedMicroUSD)
	}
	if tok.Cost5hAnchor != anchor5h || tok.CostWeeklyPeriodStart != anchorWeekly {
		t.Fatal("in-window charge must not move the anchor")
	}

	// 锚点过期：用量读取归 0，不计超额。改快照字段经 Update 落进仓。
	expired5h := time.Now().Add(-6 * time.Hour).UnixMilli()
	expiredWeekly := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	tok.Cost5hAnchor = expired5h
	tok.CostWeeklyPeriodStart = expiredWeekly
	if err := store.Update(tok); err != nil {
		t.Fatal(err)
	}
	if _, _, window, exceeded := store.CostLimitState(tok.ID); exceeded {
		t.Fatalf("expired windows reported exceeded (window %q)", window)
	}
	if view := tok.API(); view.Cost5hUsedUSD != 0 || view.CostWeeklyUsedUSD != 0 {
		t.Fatalf("API used = 5h:%f weekly:%f, want 0 for expired windows", view.Cost5hUsedUSD, view.CostWeeklyUsedUSD)
	}

	// 过期后下一笔记账重锚，窗口用量只剩新账。
	store.AddResult(tok.ID, charge)
	tok = snap()
	if tok.Cost5hUsedMicroUSD != 1_500_000 || tok.CostWeeklyUsedMicroUSD != 1_500_000 {
		t.Fatalf("post-rollover used = 5h:%d weekly:%d, want 1500000 each", tok.Cost5hUsedMicroUSD, tok.CostWeeklyUsedMicroUSD)
	}
	if tok.Cost5hAnchor <= expired5h || tok.CostWeeklyPeriodStart <= expiredWeekly {
		t.Fatal("post-expiry charge must re-anchor")
	}
}

// TestCostLimitStateNames5hAndWeekly 验证 5h/weekly 超额时窗口名随状态返回。
func TestCostLimitStateNames5hAndWeekly(t *testing.T) {
	for _, tc := range []struct {
		name   string
		apply  func(tok *Token)
		window string
	}{
		{"5h", func(tok *Token) {
			tok.Cost5hLimitMicroUSD = 1_000_000
			tok.Cost5hAnchor = time.Now().UnixMilli()
			tok.Cost5hUsedMicroUSD = 1_000_000
		}, "5h"},
		{"weekly", func(tok *Token) {
			tok.CostWeeklyLimitMicroUSD = 1_000_000
			tok.CostWeeklyPeriodStart = time.Now().UnixMilli()
			tok.CostWeeklyUsedMicroUSD = 1_000_000
		}, "weekly"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			tok, _, err := store.Ensure("k", &Token{Description: "t", IsActive: true, MaxConcurrency: 5})
			if err != nil {
				t.Fatal(err)
			}
			tc.apply(tok)
			if err := store.Update(tok); err != nil {
				t.Fatal(err)
			}
			used, limit, window, exceeded := store.CostLimitState(tok.ID)
			if !exceeded || window != tc.window || used != 1_000_000 || limit != 1_000_000 {
				t.Fatalf("CostLimitState = used %d limit %d window %q exceeded %v", used, limit, window, exceeded)
			}
		})
	}
}

// TestEnsureIdempotentAcrossRestart 验证播种幂等：重复 Ensure 不产生重复行
// （重启后亦同——按哈希命中原行），行被删后重启再 Ensure 重新播种。
// 已存在但被停用的行原样返回，不被复活。
func TestEnsureIdempotentAcrossRestart(t *testing.T) {
	st := openDB(t)
	store, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	seed := &Token{Description: "seeded token", IsActive: true}

	first, created, err := store.Ensure("seed-key", seed)
	if err != nil || !created {
		t.Fatalf("first Ensure = (%v, %v)", err, created)
	}
	firstID := first.ID
	again, created, err := store.Ensure("seed-key", &Token{Description: "other", IsActive: true})
	if err != nil || created || again.ID != firstID {
		t.Fatalf("second Ensure = (id %d, %v, %v), want same row", again.ID, created, err)
	}
	if n := len(store.List()); n != 1 {
		t.Fatalf("rows = %d, want 1", n)
	}

	// 重启（从表重新水合）后 Ensure 仍命中原行。
	reopened, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	third, created, err := reopened.Ensure("seed-key", &Token{Description: "seeded token", IsActive: true})
	if err != nil || created || third.ID != firstID {
		t.Fatalf("post-restart Ensure = (id %d, %v, %v), want same row", third.ID, created, err)
	}

	// 停用行不被播种复活。
	third.IsActive = false
	if err := reopened.Update(third); err != nil {
		t.Fatal(err)
	}
	inactive, created, err := reopened.Ensure("seed-key", seed)
	if err != nil || created || inactive.IsActive {
		t.Fatalf("Ensure on inactive row = (%v, %v), want original inactive row", err, created)
	}

	// 删行 + 重启 → 重新播种出新 ID。
	if err := reopened.Delete(firstID); err != nil {
		t.Fatal(err)
	}
	reseeded, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	fourth, created, err := reseeded.Ensure("seed-key", &Token{Description: "seeded token", IsActive: true})
	if err != nil || !created {
		t.Fatalf("re-seed Ensure = (%v, %v), want created", err, created)
	}
	if fourth.ID == firstID || fourth.Hash != HashToken("seed-key") {
		t.Fatalf("re-seeded row = id %d hash %q", fourth.ID, fourth.Hash)
	}
}

// TestAllowRPMLimitsAndRollsBucket 验证 MaxRPM 固定分钟桶：桶内计数到顶
// 拒绝；桶翻页（含重启后内存计数归零）恢复放行。
func TestAllowRPMLimitsAndRollsBucket(t *testing.T) {
	st := openDB(t)
	store, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := store.Ensure("k", &Token{Description: "t", IsActive: true, MaxRPM: 2})
	if err != nil {
		t.Fatal(err)
	}
	if used, limit, ok := store.AllowRPM(tok.ID); !ok || used != 1 || limit != 2 {
		t.Fatalf("1st AllowRPM = (%d, %d, %v)", used, limit, ok)
	}
	if used, limit, ok := store.AllowRPM(tok.ID); !ok || used != 2 || limit != 2 {
		t.Fatalf("2nd AllowRPM = (%d, %d, %v)", used, limit, ok)
	}
	if used, limit, ok := store.AllowRPM(tok.ID); ok || used != 2 || limit != 2 {
		t.Fatalf("3rd AllowRPM = (%d, %d, %v), want rejected", used, limit, ok)
	}

	// 分钟桶翻页：模拟桶过期后计数清零。rpmBucket 是瞬态字段、不经
	// Update 覆盖写（Update 从仓内旧对象继承瞬态），同包直拨仓内对象。
	store.byID[tok.ID].rpmBucket--
	if used, _, ok := store.AllowRPM(tok.ID); !ok || used != 1 {
		t.Fatalf("post-rollover AllowRPM = (%d, _, %v), want fresh count", used, ok)
	}

	// 重启归零：rpm 计数不持久化，重新水合的仓从 0 计。
	reopened, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	if used, _, ok := reopened.AllowRPM(tok.ID); !ok || used != 1 {
		t.Fatalf("post-restart AllowRPM = (%d, _, %v), want reset count", used, ok)
	}

	// MaxRPM<=0 不限。
	unlimited, _, err := reopened.Ensure("u", &Token{Description: "u", IsActive: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, _, ok := reopened.AllowRPM(unlimited.ID); !ok {
			t.Fatal("unlimited token rejected")
		}
	}
}

// TestResolveAnonymousChannel 验证空明文的解析矩阵：空仓 miss；种匿名行后
// Resolve("") 命中且 IsAnonymous；匿名行停用后 miss；坏凭据恒 miss。
func TestResolveAnonymousChannel(t *testing.T) {
	store := newStore(t)
	if _, ok := store.Resolve(""); ok {
		t.Fatal("empty store resolved empty credential")
	}

	anon, created, err := store.Ensure("", &Token{Description: "anonymous", IsActive: true})
	if err != nil || !created {
		t.Fatalf("Ensure(\"\") = (%v, %v)", err, created)
	}
	if !anon.IsAnonymous() || anon.Hash != AnonymousHash {
		t.Fatalf("seeded row not anonymous: hash %q", anon.Hash)
	}
	got, ok := store.Resolve("")
	if !ok || got.ID != anon.ID {
		t.Fatal("Resolve(\"\") missed the anonymous row")
	}
	if _, ok := store.Resolve("bad-credential"); ok {
		t.Fatal("bad credential resolved")
	}

	anon.IsActive = false
	if err := store.Update(anon); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Resolve(""); ok {
		t.Fatal("inactive anonymous row resolved")
	}
}

// TestSyncWriteEnqueueBound 验证库病态（worker 楔死 + 队列积满）时管理面
// 写不再无限期持 s.mu 等空位：submitSync 超时放弃入队并向调用方报错。
func TestSyncWriteEnqueueBound(t *testing.T) {
	s := newStore(t)
	defer func(d time.Duration) { syncEnqueueTimeout = d }(syncEnqueueTimeout)
	syncEnqueueTimeout = 50 * time.Millisecond

	tok, _, err := s.Ensure("k", &Token{Description: "t", IsActive: true})
	if err != nil {
		t.Fatal(err)
	}

	// 楔死 worker：一条 run 无视 ctx 永久阻塞（模拟不响应取消的库调用），
	// 再把 writes 填满——此后同步写只能等空位。
	release := make(chan struct{})
	s.writes <- writeTask{run: func(context.Context) error { <-release; return nil }}
	for len(s.writes) < cap(s.writes) {
		s.submitStats(func(context.Context) error { return nil })
	}
	defer func() { close(release); s.Close() }()

	start := time.Now()
	if err := s.Update(tok); err == nil {
		t.Fatal("Update on saturated queue returned nil error")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Update blocked %v, want bounded by syncEnqueueTimeout", elapsed)
	}
}

// TestWriteWorkerTaskBound 验证单条落库写的 ctx 上限：run 卡死时 worker
// 在 writeTaskTimeout 后取消它并继续消费后续写，不永久挂起。
func TestWriteWorkerTaskBound(t *testing.T) {
	s := newStore(t)
	defer func(d time.Duration) { writeTaskTimeout = d }(writeTaskTimeout)
	writeTaskTimeout = 50 * time.Millisecond
	defer s.Close()

	s.mu.Lock()
	done := s.submitSync(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	s.mu.Unlock()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wedged write err = %v, want DeadlineExceeded", err)
	}

	// worker 存活：后续写仍被消费。
	s.mu.Lock()
	done = s.submitSync(func(context.Context) error { return nil })
	s.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("follow-up write err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not resume after wedged write")
	}
}

// TestCloseStopsWriteWorker 钉住关停契约：Close 排空队列后返回，
// writeDone 闭合——写协程不退出会让 Close 挂死，回归在这里是具名
// 断言失败而非整包超时。
func TestCloseStopsWriteWorker(t *testing.T) {
	s := newStore(t)
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("Close did not return — write worker wedged")
	}
	select {
	case <-s.writeDone:
	default:
		t.Fatal("writeDone still open after Close")
	}
}
