package store

import (
	"context"
	"testing"
	"time"
)

// TestDetachedEventsRoundTripAndPrune 钉住台账表的写读与保留期清理：
// 行按事件落库，PruneDetachedEvents 按 at 毫秒界删旧留新。
func TestDetachedEventsRoundTripAndPrune(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	for i, e := range []DetachedEvent{
		{At: now.Add(-48 * time.Hour), Lane: "a", Key: "k-old", Kind: "admit"},
		{At: now, Lane: "a", Key: "k1", OriginDir: "20260919-030405", Kind: "admit"},
		{At: now, Lane: "a", Key: "k1", OriginDir: "20260919-030405", Kind: "attach", Detail: "running"},
	} {
		if err := s.InsertDetachedEvent(ctx, e); err != nil {
			t.Fatalf("InsertDetachedEvent %d: %v", i, err)
		}
	}
	var n int
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM detached_events WHERE "key" = 'k1' AND origin_dir = '20260919-030405'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("rows for k1 = %d, want 2", n)
	}
	deleted, err := s.PruneDetachedEvents(ctx, now.Add(-24*time.Hour).UnixMilli())
	if err != nil {
		t.Fatalf("PruneDetachedEvents: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the 48h-old row)", deleted)
	}
	if err := s.ro.QueryRowContext(ctx, `SELECT COUNT(*) FROM detached_events`).Scan(&n); err != nil {
		t.Fatalf("count after prune: %v", err)
	}
	if n != 2 {
		t.Fatalf("remaining = %d, want 2", n)
	}
}
