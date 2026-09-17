package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeLegacyFixtures 在 stateDir/logRoot 摆一套文件时代产物，
// 覆盖全部六个源：index.jsonl（含探活行与截断尾行）、auth_tokens.json
// （非连续 id）、models.json、panel-settings.json、quota.jsonl、
// 两种命名的 gate-state。
func writeLegacyFixtures(t *testing.T, stateDir, logRoot string) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(stateDir, 0o755))
	must(os.MkdirAll(logRoot, 0o755))

	entry := func(dir, reqID string) map[string]any {
		return map[string]any{
			"dir": dir, "started_at": "2026-09-17T10:00:00.5Z", "duration_ms": 100,
			"method": "POST", "path": "/v1/messages", "status_code": 200,
			"result": "completed", "model": "devin", "client_request_id": reqID,
		}
	}
	var lines []byte
	for _, m := range []map[string]any{entry("d1", "r1"), entry("d2", "panel-probe")} {
		b, _ := json.Marshal(m)
		lines = append(lines, b...)
		lines = append(lines, '\n')
	}
	lines = append(lines, []byte(`{"dir":"torn","started_at":`)...) // 截断尾行
	must(os.WriteFile(filepath.Join(logRoot, "index.jsonl"), lines, 0o644))

	must(os.WriteFile(filepath.Join(stateDir, "auth_tokens.json"), []byte(`{
		"next_id": 10,
		"tokens": [
			{"id": 3, "token": "h3", "description": "three", "created_at": "2026-09-01T00:00:00Z",
			 "is_active": true, "allowed_models": ["m1"], "cost_daily_used_micro_usd": 500},
			{"id": 7, "token": "h7", "created_at": "2026-09-02T00:00:00Z", "is_active": false}
		]}`), 0o644))

	must(os.WriteFile(filepath.Join(stateDir, "models.json"), []byte(`{
		"models": {"a": {"redirect_model": "b"}, "c": {"disabled": true}}}`), 0o644))

	must(os.WriteFile(filepath.Join(stateDir, "panel-settings.json"), []byte(`{
		"values": {"debug_log_enabled": "false", "retention_days": "7"},
		"updated": {"debug_log_enabled": 111}}`), 0o644))

	var qlines []byte
	for _, q := range []map[string]any{
		{"at": 1700000000, "account": "default", "daily_remaining": 50.5},
		{"at": 1700000060, "account": "default"},
	} {
		b, _ := json.Marshal(q)
		qlines = append(qlines, b...)
		qlines = append(qlines, '\n')
	}
	must(os.WriteFile(filepath.Join(logRoot, "quota.jsonl"), qlines, 0o644))

	must(os.WriteFile(filepath.Join(logRoot, "gate-state.json"),
		[]byte(`{"limited_until":"2026-09-17T12:00:00Z"}`), 0o644))
	must(os.WriteFile(filepath.Join(logRoot, "gate-state-randall.json"),
		[]byte(`{"limited_until":"2026-09-17T13:00:00Z"}`), 0o644))
}

func migrated(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path + ".migrated"); err != nil {
		t.Fatalf("%s.migrated missing: %v", path, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s should be renamed away", path)
	}
}

func TestImportLegacy(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	writeLegacyFixtures(t, stateDir, logRoot)

	s, err := Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()

	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}

	// index.jsonl：2 行入库（截断尾行跳过），探活行是 manual_test。
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("logs = %d, want 2", n)
	}
	var src string
	if err := s.db.QueryRowContext(ctx,
		`SELECT log_source FROM logs WHERE dir='d2'`).Scan(&src); err != nil {
		t.Fatal(err)
	}
	if src != "manual_test" {
		t.Fatalf("probe log_source = %q", src)
	}
	var ms int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT time FROM logs WHERE dir='d1'`).Scan(&ms); err != nil {
		t.Fatal(err)
	}
	wantMS := time.Date(2026, 9, 17, 10, 0, 0, 500000000, time.UTC).UnixMilli()
	if ms != wantMS {
		t.Fatalf("time = %d, want %d", ms, wantMS)
	}

	// auth_tokens：id 保留（3、7），新插入自增到 >7。
	tokens, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 2 || tokens[0].ID != 3 || tokens[1].ID != 7 {
		t.Fatalf("tokens = %+v", tokens)
	}
	if tokens[0].DailyUsedMicroUSD != 500 || len(tokens[0].AllowedModels) != 1 {
		t.Fatalf("token[0] = %+v", tokens[0])
	}
	newID, err := s.InsertToken(ctx, &TokenRow{Token: "hx"})
	if err != nil {
		t.Fatal(err)
	}
	if newID != 8 {
		t.Fatalf("new token id = %d, want 8 (max imported +1)", newID)
	}

	// models/settings/quota/gate。
	models, err := s.ListModels(ctx)
	if err != nil || len(models) != 2 {
		t.Fatalf("models = %v %v", models, err)
	}
	v, ts, ok, err := s.GetSetting(ctx, "debug_log_enabled")
	if err != nil || !ok || v != "false" || ts != 111 {
		t.Fatalf("setting = %q,%d,%v,%v", v, ts, ok, err)
	}
	qs, err := s.ListQuotaSamples(ctx, "default", 0, 0)
	if err != nil || len(qs) != 2 {
		t.Fatalf("quota = %v %v", qs, err)
	}
	if qs[0].DailyRemaining == nil || qs[1].DailyRemaining != nil {
		t.Fatalf("quota remaining nil/val = %v/%v", qs[0].DailyRemaining, qs[1].DailyRemaining)
	}
	gv, ok, err := s.GetState(ctx, "gate:default")
	if err != nil || !ok || gv != `{"limited_until":"2026-09-17T12:00:00Z"}` {
		t.Fatalf("gate:default = %q,%v,%v", gv, ok, err)
	}
	if _, ok, _ := s.GetState(ctx, "gate:randall"); !ok {
		t.Fatal("gate:randall missing")
	}

	// 原文件全部改名。
	for _, p := range []string{
		filepath.Join(logRoot, "index.jsonl"),
		filepath.Join(logRoot, "quota.jsonl"),
		filepath.Join(logRoot, "gate-state.json"),
		filepath.Join(logRoot, "gate-state-randall.json"),
		filepath.Join(stateDir, "auth_tokens.json"),
		filepath.Join(stateDir, "models.json"),
		filepath.Join(stateDir, "panel-settings.json"),
	} {
		migrated(t, p)
	}
	if _, ok, _ := s.GetState(ctx, "import_base_done"); !ok {
		t.Fatal("import_base_done not set")
	}

	// 重跑且源文件缺席：空操作，行数不变。
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("after rerun logs = %d", n)
	}
}

// TestImportLegacyRecreatedFiles 模拟分阶段落地：D1 已导入后旧写
// 路径仍在服役，重建的同名文件在下次启动时必须被合并而不是跳过——
// 这是文件存在性（而非一次性标记）驱动导入的原因。
func TestImportLegacyRecreatedFiles(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	writeLegacyFixtures(t, stateDir, logRoot)

	s, err := Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("first import: %v", err)
	}

	// 旧仓重建文件：新 index 行；token 文件带三种碰撞形态——
	// 同 token 不同 id（按 token 合并）、新 token 撞已占用 id
	// （autoincrement）、新 token 空闲 id（保留）。
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(logRoot, "index.jsonl"),
		[]byte(`{"dir":"d3","started_at":"2026-09-17T11:00:00Z","method":"POST","path":"/v1/messages","status_code":200}`+"\n"), 0o644))
	must(os.WriteFile(filepath.Join(stateDir, "auth_tokens.json"), []byte(`{
		"next_id": 4,
		"tokens": [
			{"id": 5, "token": "h3", "success_count": 999},
			{"id": 3, "token": "h-clash"},
			{"id": 1, "token": "h-seed"}
		]}`), 0o644))

	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("second import: %v", err)
	}

	var n int
	must(s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`).Scan(&n))
	if n != 3 {
		t.Fatalf("logs = %d, want 3", n)
	}
	tokens, err := s.ListTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 4 {
		t.Fatalf("tokens = %d, want 4", len(tokens))
	}
	byToken := map[string]*TokenRow{}
	for _, tk := range tokens {
		byToken[tk.Token] = tk
	}
	if byToken["h3"].ID != 3 || byToken["h3"].SuccessCount != 999 {
		t.Fatalf("h3 merged wrong: %+v", byToken["h3"])
	}
	if byToken["h-seed"].ID != 1 {
		t.Fatalf("h-seed id = %d, want 1", byToken["h-seed"].ID)
	}
	// h-clash 的文件 id 3 被 h3 占用 → autoincrement（>7）。
	if byToken["h-clash"].ID <= 7 {
		t.Fatalf("h-clash id = %d, want autoincrement >7", byToken["h-clash"].ID)
	}
	migrated(t, filepath.Join(stateDir, "auth_tokens.json"))
	migrated(t, filepath.Join(logRoot, "index.jsonl"))
}

func TestImportLegacyMissingFiles(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy on empty dirs: %v", err)
	}
	if _, ok, _ := s.GetState(ctx, "import_base_done"); !ok {
		t.Fatal("import_base_done not set for empty import")
	}
}
