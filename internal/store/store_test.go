package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenCreatesFileAndReopens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file should exist after Open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	// schema 幂等：重开后写读正常即可。
	if err := s2.SetState(context.Background(), "k", "v"); err != nil {
		t.Fatalf("SetState after reopen: %v", err)
	}
}

func TestInsertLogDerivations(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	started := time.Date(2026, 9, 17, 10, 30, 0, 123456789, time.UTC)
	id, err := s.InsertLog(ctx, &LogRow{
		Dir: "20260917-103000-abc", StartedAt: started, DurationMS: 1500,
		Method: "POST", Path: "/v1/messages", StatusCode: 200, Result: "completed",
		Model: "devin", Stream: true, ClientRequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("InsertLog: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	var ms, bucket int64
	var source, proto string
	err = s.db.QueryRowContext(ctx,
		`SELECT time, minute_bucket, log_source, upstream_protocol FROM logs WHERE id=?`, id).
		Scan(&ms, &bucket, &source, &proto)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if ms != started.UnixMilli() {
		t.Fatalf("time = %d, want %d", ms, started.UnixMilli())
	}
	if bucket != ms/60000 {
		t.Fatalf("minute_bucket = %d, want %d", bucket, ms/60000)
	}
	if source != "proxy" || proto != "devin" {
		t.Fatalf("log_source=%q upstream_protocol=%q, want proxy/devin", source, proto)
	}

	// 面板探活行归 manual_test。
	if _, err := s.InsertLog(ctx, &LogRow{
		Dir: "probe-1", StartedAt: started, Method: "POST", Path: "/v1/messages",
		StatusCode: 200, ClientRequestID: probeClientRequestID,
	}); err != nil {
		t.Fatalf("InsertLog probe: %v", err)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT log_source FROM logs WHERE dir='probe-1'`).Scan(&source); err != nil {
		t.Fatalf("query probe: %v", err)
	}
	if source != "manual_test" {
		t.Fatalf("log_source = %q, want manual_test", source)
	}

	// dir 唯一：重复插入报错。
	if _, err := s.InsertLog(ctx, &LogRow{Dir: "probe-1", StartedAt: started}); err == nil {
		t.Fatal("duplicate dir should fail")
	}
}

func TestTokenCRUD(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	id, err := s.InsertToken(ctx, &TokenRow{
		Token: "hash-a", Description: "first", CreatedAt: 1000,
		IsActive: true, AllowedModels: []string{"m1", "m2"}, MaxConcurrency: 4,
	})
	if err != nil {
		t.Fatalf("InsertToken: %v", err)
	}
	if id != 1 {
		t.Fatalf("id = %d, want 1", id)
	}
	id2, err := s.InsertToken(ctx, &TokenRow{Token: "hash-b", IsActive: true})
	if err != nil {
		t.Fatalf("InsertToken 2: %v", err)
	}
	if id2 != 2 {
		t.Fatalf("id2 = %d, want 2", id2)
	}

	tokens, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(tokens) != 2 || tokens[0].Token != "hash-a" || tokens[1].Token != "hash-b" {
		t.Fatalf("ListTokens = %+v", tokens)
	}
	if len(tokens[0].AllowedModels) != 2 || tokens[0].AllowedModels[1] != "m2" {
		t.Fatalf("AllowedModels = %v", tokens[0].AllowedModels)
	}

	// Upsert 按 id 覆盖。
	exp := int64(999999)
	tokens[0].ExpiresAt = &exp
	tokens[0].SuccessCount = 42
	if err := s.UpsertToken(ctx, tokens[0]); err != nil {
		t.Fatalf("UpsertToken: %v", err)
	}
	back, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatalf("ListTokens after upsert: %v", err)
	}
	if back[0].SuccessCount != 42 || back[0].ExpiresAt == nil || *back[0].ExpiresAt != exp {
		t.Fatalf("upserted row = %+v", back[0])
	}

	if err := s.DeleteToken(ctx, id); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	rest, err := s.ListTokens(ctx)
	if err != nil || len(rest) != 1 {
		t.Fatalf("after delete: %v %v", rest, err)
	}
}

func TestSettingsAndState(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.SetSetting(ctx, "debug_log_enabled", "true", 0); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	v, ts, ok, err := s.GetSetting(ctx, "debug_log_enabled")
	if err != nil || !ok || v != "true" || ts <= 0 {
		t.Fatalf("GetSetting = %q,%d,%v,%v", v, ts, ok, err)
	}
	if _, _, ok, _ := s.GetSetting(ctx, "absent"); ok {
		t.Fatal("absent key reported ok")
	}
	if err := s.SetSetting(ctx, "x", "y", 123); err != nil {
		t.Fatalf("SetSetting x: %v", err)
	}
	values, updated, err := s.ListSettings(ctx)
	if err != nil {
		t.Fatalf("ListSettings: %v", err)
	}
	if len(values) != 2 || values["x"] != "y" || updated["x"] != 123 {
		t.Fatalf("ListSettings = %v %v", values, updated)
	}
	if err := s.DeleteSetting(ctx, "x"); err != nil {
		t.Fatalf("DeleteSetting: %v", err)
	}

	if err := s.SetState(ctx, "gate:default", `{"limited_until":"2026-09-17T11:00:00Z"}`); err != nil {
		t.Fatalf("SetState: %v", err)
	}
	gv, ok, err := s.GetState(ctx, "gate:default")
	if err != nil || !ok || gv == "" {
		t.Fatalf("GetState = %q,%v,%v", gv, ok, err)
	}
	if err := s.DeleteState(ctx, "gate:default"); err != nil {
		t.Fatalf("DeleteState: %v", err)
	}
	if _, ok, _ := s.GetState(ctx, "gate:default"); ok {
		t.Fatal("deleted state reported ok")
	}
}

func TestQuotaSamples(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	rem := 88.5
	if err := s.InsertQuotaSample(ctx, &QuotaSample{
		At: 1700000000, Account: "default", DailyRemaining: &rem,
		PromptCredits: 100, ACUConsumed: 5,
	}); err != nil {
		t.Fatalf("InsertQuotaSample: %v", err)
	}
	// nil remaining → NULL（区别于 0）。
	if err := s.InsertQuotaSample(ctx, &QuotaSample{
		At: 1700000060, Account: "default",
	}); err != nil {
		t.Fatalf("InsertQuotaSample 2: %v", err)
	}
	if err := s.InsertQuotaSample(ctx, &QuotaSample{
		At: 1700000030, Account: "randall",
	}); err != nil {
		t.Fatalf("InsertQuotaSample 3: %v", err)
	}
	// 同 (account,at) 撞车静默丢新点（OR IGNORE），不报错。
	if err := s.InsertQuotaSample(ctx, &QuotaSample{
		At: 1700000060, Account: "default", DailyRemaining: &rem,
	}); err != nil {
		t.Fatalf("InsertQuotaSample dup: %v", err)
	}

	got, err := s.ListQuotaSamples(ctx, "default", 0, 0)
	if err != nil {
		t.Fatalf("ListQuotaSamples: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (dup dropped)", len(got))
	}
	if got[0].DailyRemaining == nil || *got[0].DailyRemaining != rem {
		t.Fatalf("DailyRemaining = %v", got[0].DailyRemaining)
	}
	if got[1].DailyRemaining != nil {
		t.Fatalf("DailyRemaining = %v, want nil", *got[1].DailyRemaining)
	}
	// since 过滤 + account 过滤。
	got, err = s.ListQuotaSamples(ctx, "default", 1700000060, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("since filter: %v %v", got, err)
	}
	got, err = s.ListQuotaSamples(ctx, "randall", 0, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("randall filter: %v %v", got, err)
	}
	// limit 截尾取最新 N 条，返回仍升序。
	got, err = s.ListQuotaSamples(ctx, "", 0, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("limit: %v %v", got, err)
	}
	if got[0].At != 1700000030 || got[1].At != 1700000060 {
		t.Fatalf("limit order: %v %v", got[0].At, got[1].At)
	}
}

func TestPruneQuotaSamples(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	for i := 0; i < quotaSampleKeep+5; i++ {
		if err := s.InsertQuotaSample(ctx, &QuotaSample{At: int64(1000 + i)}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	n, err := s.PruneQuotaSamples(ctx)
	if err != nil {
		t.Fatalf("PruneQuotaSamples: %v", err)
	}
	if n != 5 {
		t.Fatalf("pruned = %d, want 5", n)
	}
	var cnt, minAt int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(at) FROM quota_samples`).Scan(&cnt, &minAt); err != nil {
		t.Fatalf("count: %v", err)
	}
	if cnt != quotaSampleKeep || minAt != 1005 {
		t.Fatalf("after prune: count=%d min=%d, want %d/1005", cnt, minAt, quotaSampleKeep)
	}
	// 不足帽时调用安全（不删行）。
	if n, err = s.PruneQuotaSamples(ctx); err != nil || n != 0 {
		t.Fatalf("second prune = %d,%v want 0,nil", n, err)
	}
}

func TestModelRegistry(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.SetModel(ctx, ModelEntry{Model: "claude-x", RedirectModel: "claude-y"}); err != nil {
		t.Fatalf("SetModel: %v", err)
	}
	if err := s.SetModel(ctx, ModelEntry{Model: "claude-z", Disabled: true}); err != nil {
		t.Fatalf("SetModel 2: %v", err)
	}
	models, err := s.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("len = %d", len(models))
	}
	byName := map[string]ModelEntry{}
	for _, m := range models {
		byName[m.Model] = m
	}
	if byName["claude-x"].RedirectModel != "claude-y" || !byName["claude-z"].Disabled {
		t.Fatalf("models = %+v", byName)
	}
	if err := s.DeleteModel(ctx, "claude-x"); err != nil {
		t.Fatalf("DeleteModel: %v", err)
	}
	models, _ = s.ListModels(ctx)
	if len(models) != 1 {
		t.Fatalf("after delete len = %d", len(models))
	}
}
