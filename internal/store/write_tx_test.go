// 本文件验证 CL-252 枚举的启动期 deferred 先读后写事务改 BEGIN
// IMMEDIATE 后的等锁行为：竞争写者持写锁期间操作经 busy_timeout 排队
// 而非快败 SQLITE_BUSY_SNAPSHOT，放锁后照常完成。失败类本身的确定性
// 复现见 migrations_test.go 的 TestDeferredReadThenWriteBusySnapshot。
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// holdImmediateLock 在独立连接池上持 BEGIN IMMEDIATE 写锁，模拟交接期
// 在役实例占住写者；返回的 release 提交后锁放出。未 release 时 Cleanup
// 回滚兜底。
func holdImmediateLock(t *testing.T, path string) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", walDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		}
		_ = conn.Close()
		_ = db.Close()
	})
	return func() {
		released = true
		if _, err := conn.ExecContext(context.Background(), `COMMIT`); err != nil {
			t.Fatal(err)
		}
	}
}

// assertQueuesThenDone 断言被测操作在持锁期间不返回、release 后完成——
// 「排队而非快败」是 BEGIN IMMEDIATE 相对 deferred 的行为分水岭。
func assertQueuesThenDone(t *testing.T, done chan error, release func()) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("op returned while write lock held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("op: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("op did not finish after lock release")
	}
}

func openPathStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestReconcileCellsImmediateUnderLock：事务体先读 cells 水位再聚合
// 写入（cellsGapSQL/errCellsGapSQL/setCellsWatermark）——deferred 下
// 读快照与写锁升级之间被并发写挤入即 BUSY_SNAPSHOT，Open 在交接窗内
// 点火它。持锁期间应排队，放锁后补漏落账。
func TestReconcileCellsImmediateUnderLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s := openPathStore(t, path)
	ctx := context.Background()
	// 绕过双写插一行未记账日志（importIndex 式缝隙——补漏对象）。
	if _, err := s.db.ExecContext(ctx, logsInsertSQL, logInsertArgs(&LogRow{
		StartedAt: time.Now(), DurationMS: 100, Method: "POST", Path: "/v1/messages",
		StatusCode: 200, Result: "completed", API: "anthropic", Model: "m-a", KeyHash: "kh1",
	})...); err != nil {
		t.Fatal(err)
	}
	release := holdImmediateLock(t, path)
	done := make(chan error, 1)
	go func() { done <- s.ReconcileCells(ctx) }()
	assertQueuesThenDone(t, done, release)
	var cells int
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM log_cells`).Scan(&cells); err != nil || cells != 1 {
		t.Fatalf("log_cells rows = %d err=%v", cells, err)
	}
	var wm, maxID int64
	if err := s.ro.QueryRow(
		`SELECT CAST(value AS INTEGER) FROM runtime_state WHERE "key"=?`,
		cellsWatermarkKey).Scan(&wm); err != nil {
		t.Fatalf("read watermark: %v", err)
	}
	if err := s.ro.QueryRow(`SELECT MAX(id) FROM logs`).Scan(&maxID); err != nil {
		t.Fatal(err)
	}
	if wm != maxID {
		t.Fatalf("watermark = %d, want MAX(id) = %d", wm, maxID)
	}
}

// TestPutDebugFileImmediateUnderLock：OR REPLACE 的计数增量要先读旧行
// 尺寸（SELECT LENGTH）再写——与 WriteDebugBatch 的非 IfAbsent 路径同形，
// deferred 下是 BUSY_SNAPSHOT 裸露面。持锁期间应排队，放锁后覆写落库。
func TestPutDebugFileImmediateUnderLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s := openPathStore(t, path)
	ctx := context.Background()
	// 先铺一行旧值，逼覆写路径走「读旧尺寸→写新行」。
	if err := s.PutDebugFile(ctx, "d1", "meta.json", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	release := holdImmediateLock(t, path)
	done := make(chan error, 1)
	go func() { done <- s.PutDebugFile(ctx, "d1", "meta.json", []byte(`{"v":2,"x":1}`)) }()
	assertQueuesThenDone(t, done, release)
	data, _, ok, err := s.DebugFile(ctx, "d1", "meta.json", 0)
	if err != nil || !ok || string(data) != `{"v":2,"x":1}` {
		t.Fatalf("DebugFile = %q,%v,%v", data, ok, err)
	}
}

// TestImportTokensImmediateUnderLock：逐 token 先 SELECT 探行再决定覆盖
// 或插入（withSourceTx 的事务体），deferred 下探行快照与写锁
// 升级之间可吃 BUSY_SNAPSHOT。持锁期间应排队，放锁后数据行与
// imported:auth_tokens 标记同事务落库。
func TestImportTokensImmediateUnderLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	s := openPathStore(t, path)
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "auth_tokens.json")
	body, err := json.Marshal(legacyTokenFile{NextID: 2, Tokens: []*legacyToken{
		{ID: 1, Hash: "tok-abc", Description: "d", CreatedAt: "2026-09-19T00:00:00Z", IsActive: true},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, body, 0o600); err != nil {
		t.Fatal(err)
	}
	release := holdImmediateLock(t, path)
	done := make(chan error, 1)
	go func() { done <- s.importTokens(ctx, src) }()
	assertQueuesThenDone(t, done, release)
	var n int
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM auth_tokens WHERE token='tok-abc'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("tokens = %d err=%v", n, err)
	}
	var marker int
	if err := s.ro.QueryRow(
		`SELECT COUNT(*) FROM runtime_state WHERE "key"='imported:auth_tokens'`).Scan(&marker); err != nil || marker != 1 {
		t.Fatalf("marker = %d err=%v", marker, err)
	}
}
