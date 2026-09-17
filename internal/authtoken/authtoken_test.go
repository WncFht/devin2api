// 令牌仓行为测试：锚定滚动窗口（5h/weekly）记账与过期、Ensure 播种幂等、
// RPM 分钟桶、匿名通道行（sha256("")）的解析矩阵。
package authtoken

import (
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	store, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
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

	charge := Result{StatusCode: 200, CostUSD: 1.5}
	store.AddResult(tok.ID, charge)
	if tok.Cost5hAnchor <= 0 || tok.CostWeeklyPeriodStart <= 0 {
		t.Fatalf("anchors not set: 5h=%d weekly=%d", tok.Cost5hAnchor, tok.CostWeeklyPeriodStart)
	}
	if tok.Cost5hUsedMicroUSD != 1_500_000 || tok.CostWeeklyUsedMicroUSD != 1_500_000 {
		t.Fatalf("used = 5h:%d weekly:%d, want 1500000 each", tok.Cost5hUsedMicroUSD, tok.CostWeeklyUsedMicroUSD)
	}
	anchor5h, anchorWeekly := tok.Cost5hAnchor, tok.CostWeeklyPeriodStart

	store.AddResult(tok.ID, charge)
	if tok.Cost5hUsedMicroUSD != 3_000_000 || tok.CostWeeklyUsedMicroUSD != 3_000_000 {
		t.Fatalf("used after 2nd charge = 5h:%d weekly:%d, want 3000000 each", tok.Cost5hUsedMicroUSD, tok.CostWeeklyUsedMicroUSD)
	}
	if tok.Cost5hAnchor != anchor5h || tok.CostWeeklyPeriodStart != anchorWeekly {
		t.Fatal("in-window charge must not move the anchor")
	}

	// 锚点过期：用量读取归 0，不计超额。
	expired5h := time.Now().Add(-6 * time.Hour).UnixMilli()
	expiredWeekly := time.Now().Add(-8 * 24 * time.Hour).UnixMilli()
	tok.Cost5hAnchor = expired5h
	tok.CostWeeklyPeriodStart = expiredWeekly
	if _, _, window, exceeded := store.CostLimitState(tok.ID); exceeded {
		t.Fatalf("expired windows reported exceeded (window %q)", window)
	}
	if view := tok.API(); view.Cost5hUsedUSD != 0 || view.CostWeeklyUsedUSD != 0 {
		t.Fatalf("API used = 5h:%f weekly:%f, want 0 for expired windows", view.Cost5hUsedUSD, view.CostWeeklyUsedUSD)
	}

	// 过期后下一笔记账重锚，窗口用量只剩新账。
	store.AddResult(tok.ID, charge)
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
	dir := t.TempDir()
	store, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	seed := &Token{Description: "config: auth.api_key", IsActive: true}

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

	// 重启（重新加载文件）后 Ensure 仍命中原行。
	reopened, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	third, created, err := reopened.Ensure("seed-key", &Token{Description: "config: auth.api_key", IsActive: true})
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
	reseeded, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	fourth, created, err := reseeded.Ensure("seed-key", &Token{Description: "config: auth.api_key", IsActive: true})
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
	dir := t.TempDir()
	store, err := New(dir)
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

	// 分钟桶翻页：模拟桶过期后计数清零。
	tok.rpmBucket--
	if used, _, ok := store.AllowRPM(tok.ID); !ok || used != 1 {
		t.Fatalf("post-rollover AllowRPM = (%d, _, %v), want fresh count", used, ok)
	}

	// 重启归零：rpm 计数不持久化，重载的仓从 0 计。
	reopened, err := New(dir)
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
