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

// TestDetachedBlobsRoundTripConflictAndPrune 钉住种子表三件事：
// LoadDetachedBlobs 按 lane+finished_at 窗过滤并升序返回；同键冲突按
// finished_at 新者胜（旧写被 WHERE 挡下）；PruneDetachedBlobs 按
// finished_at 界删旧留新。
func TestDetachedBlobsRoundTripConflictAndPrune(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	now := time.Now()
	rows := []DetachedBlob{
		{Key: "k1", Lane: "a", OriginDir: "d1", FinishedAt: now.Add(-time.Minute), Payload: []byte("p1")},
		{Key: "k2", Lane: "a", FinishedAt: now, Payload: []byte("p2")},
		{Key: "k3", Lane: "b", FinishedAt: now, Payload: []byte("p3")},
		{Key: "k4", Lane: "a", FinishedAt: now.Add(-2 * time.Hour), Payload: []byte("p4")},
	}
	for i, b := range rows {
		if err := s.InsertDetachedBlob(ctx, b); err != nil {
			t.Fatalf("InsertDetachedBlob %d: %v", i, err)
		}
	}
	// 同键后到的旧写被仲裁挡下：finished_at 更老的 payload 不得覆盖。
	if err := s.InsertDetachedBlob(ctx, DetachedBlob{
		Key: "k2", Lane: "a", FinishedAt: now.Add(-time.Minute), Payload: []byte("stale"),
	}); err != nil {
		t.Fatalf("stale conflict insert: %v", err)
	}
	loaded, err := s.LoadDetachedBlobs(ctx, "a", now.Add(-detachedBlobRetention).UnixMilli())
	if err != nil {
		t.Fatalf("LoadDetachedBlobs: %v", err)
	}
	if len(loaded) != 2 || loaded[0].Key != "k1" || loaded[1].Key != "k2" {
		t.Fatalf("loaded = %+v, want k1,k2 ascending", loaded)
	}
	if string(loaded[1].Payload) != "p2" {
		t.Fatalf("k2 payload = %q, want p2 (newest wins)", loaded[1].Payload)
	}
	if loaded[1].OriginDir != "" || loaded[0].OriginDir != "d1" {
		t.Fatalf("origin_dir = %q/%q", loaded[0].OriginDir, loaded[1].OriginDir)
	}
	deleted, err := s.PruneDetachedBlobs(ctx, now.Add(-detachedBlobRetention).UnixMilli())
	if err != nil {
		t.Fatalf("PruneDetachedBlobs: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only k4)", deleted)
	}
}
