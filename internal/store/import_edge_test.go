package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tableCount 数单表行数；table 只接测试内字面量。
func tableCount(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// writeTextFile 落盘一个夹具文件。
func writeTextFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeJSONLines 把行切片拼成 JSONL 文件（补尾部换行）。
func writeJSONLines(t *testing.T, path string, lines []string) {
	t.Helper()
	writeTextFile(t, path, strings.Join(lines, "\n")+"\n")
}

// indexLine 造一行最小合法 index.jsonl 条目。
func indexLine(dir string) string {
	return fmt.Sprintf(`{"dir":%q,"started_at":"2026-09-17T10:00:00Z","method":"POST","path":"/v1/messages","status_code":200,"result":"completed"}`, dir)
}

// hasState 报告 runtime_state 里 key 是否存在。
func hasState(t *testing.T, s *Store, key string) bool {
	t.Helper()
	_, ok, err := s.GetState(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestImportIndexBadLines 覆盖 index.jsonl 的逐行容忍：截断行、空行、
// 纯空白行、非对象 JSON、字段类型错误、坏/缺 started_at 全部跳过；
// 结构合法但缺 dir 的行仍会入库占 dir 空串槽位（与文件时代 ScanIndex
// 放行语义一致——文件层不校验 dir 非空）。
func TestImportIndexBadLines(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl"), []string{
		indexLine("g1"),
		`{"dir":"torn","started_at":`, // 截断行
		"",                            // 空行
		"   ",                         // 纯空白行
		`42`,                          // 非对象 JSON
		`[1,2]`,                       // 数组而非对象
		`{"dir":"typedir","started_at":"2026-09-17T10:00:00Z","duration_ms":"abc"}`, // 字段类型错
		`{"dir":"badts","started_at":"yesterday"}`,                                  // 时间戳无法解析
		`{}`, // 缺 started_at
		indexLine("g2"),
		`{"dir":"","started_at":"2026-09-17T10:00:00Z"}`, // dir 缺席：合法行，落 dir=''
		indexLine("g3"),
	})

	s := openTemp(t)
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n := tableCount(t, s, "logs"); n != 4 {
		t.Fatalf("logs = %d, want 4 (g1/g2/g3 + dir='')", n)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE dir IN ('g1','g2','g3')`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("good dirs = %d, want 3", n)
	}
	migrated(t, filepath.Join(logRoot, "index.jsonl"))
}

// TestImportIndexOversizedLine 固定「单行超 4MB」的现行行为：ReadBytes
// 无行长上限，超大行与其他行一样入库，后续源照常导入——不再出现
// ErrTooLong 整体中止导致的重启 crash loop。
func TestImportIndexOversizedLine(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	giant := `{"dir":"huge","started_at":"2026-09-17T10:00:00Z","pad":"` + strings.Repeat("x", 4<<20) + `"}`
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl"), []string{
		indexLine("before"), giant, indexLine("after"),
	})
	writeJSONLines(t, filepath.Join(logRoot, "quota.jsonl"),
		[]string{`{"at":1700000000,"account":"default"}`})

	s := openTemp(t)
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n := tableCount(t, s, "logs"); n != 3 {
		t.Fatalf("logs = %d, want 3 (oversized line included)", n)
	}
	migrated(t, filepath.Join(logRoot, "index.jsonl"))
	migrated(t, filepath.Join(logRoot, "quota.jsonl"))
	if n := tableCount(t, s, "quota_samples"); n != 1 {
		t.Fatalf("quota_samples = %d, want 1", n)
	}
}

// TestImportPartialSources 只摆 index.jsonl 一个源：导入照常成功，
// 缺席源不写 imported:<src> 标记、不产生 .migrated。
func TestImportPartialSources(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl"),
		[]string{indexLine("only-1"), indexLine("only-2")})

	s := openTemp(t)
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n := tableCount(t, s, "logs"); n != 2 {
		t.Fatalf("logs = %d, want 2", n)
	}
	for _, table := range []string{"auth_tokens", "model_registry", "settings", "quota_samples"} {
		if n := tableCount(t, s, table); n != 0 {
			t.Fatalf("%s = %d, want 0", table, n)
		}
	}
	if !hasState(t, s, "imported:index") {
		t.Fatal("imported:index missing")
	}
	for _, key := range []string{"imported:auth_tokens", "imported:models", "imported:panel_settings", "imported:quota"} {
		if hasState(t, s, key) {
			t.Fatalf("%s should not be marked for absent source", key)
		}
	}
	if !hasState(t, s, "import_base_done") {
		t.Fatal("import_base_done missing")
	}
	// 缺席源不产生任何 .migrated。
	matches, err := filepath.Glob(filepath.Join(stateDir, "*.migrated"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("stateDir .migrated = %v, want none", matches)
	}
	matches, err = filepath.Glob(filepath.Join(logRoot, "*.migrated"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("logRoot .migrated = %v, want only index.jsonl.migrated", matches)
	}
}

// TestImportIdempotentRerun 覆盖两种「再跑一次」：
// A. db 还在、源文件重现（commit 后 rename 失败/旧仓重建同名文件）
// → 各源按唯一键去重，行数不变；
// B. db 整个删掉重建、源文件仍在 → 导入恢复同一全集。
func TestImportIdempotentRerun(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	writeLegacyFixtures(t, stateDir, logRoot)
	dbPath := filepath.Join(base, "test.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("first import: %v", err)
	}
	counts := func() (logs, tokens, quota, models, settings int) {
		return tableCount(t, s, "logs"), tableCount(t, s, "auth_tokens"),
			tableCount(t, s, "quota_samples"), tableCount(t, s, "model_registry"),
			tableCount(t, s, "settings")
	}
	l1, t1, q1, m1, st1 := counts()

	// 给 h3 改个字段，验证「按 token 合并覆盖」而不是追加新行。
	if _, err := s.db.ExecContext(ctx,
		`UPDATE auth_tokens SET success_count=777 WHERE token='h3'`); err != nil {
		t.Fatal(err)
	}

	// A：源文件重现（同内容），重跑应全部去重。
	writeLegacyFixtures(t, stateDir, logRoot)
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("rerun import: %v", err)
	}
	if l, tk, q, m, st := counts(); l != l1 || tk != t1 || q != q1 || m != m1 || st != st1 {
		t.Fatalf("counts drifted: (%d,%d,%d,%d,%d) -> (%d,%d,%d,%d,%d)",
			l1, t1, q1, m1, st1, l, tk, q, m, st)
	}
	var dup int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)-COUNT(DISTINCT token) FROM auth_tokens`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("duplicate tokens = %d", dup)
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*)-COUNT(DISTINCT account||'@'||at) FROM quota_samples`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("duplicate (account,at) = %d", dup)
	}
	var sc int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT success_count FROM auth_tokens WHERE token='h3'`).Scan(&sc); err != nil {
		t.Fatal(err)
	}
	if sc != 0 {
		t.Fatalf("h3 success_count = %d, want 0 (file value overwrote db value)", sc)
	}
	migrated(t, filepath.Join(logRoot, "index.jsonl"))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// B：db 删掉重建，源文件仍在 → 恢复同一全集。
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
	writeLegacyFixtures(t, stateDir, logRoot)
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("rebuild import: %v", err)
	}
	if l, tk, q, m, st := tableCount(t, s2, "logs"), tableCount(t, s2, "auth_tokens"),
		tableCount(t, s2, "quota_samples"), tableCount(t, s2, "model_registry"),
		tableCount(t, s2, "settings"); l != l1 || tk != t1 || q != q1 || m != m1 || st != st1 {
		t.Fatalf("rebuilt counts (%d,%d,%d,%d,%d) != first run (%d,%d,%d,%d,%d)",
			l, tk, q, m, st, l1, t1, q1, m1, st1)
	}
}

// TestImportResumePartial 模拟「commit 后 rename 前断掉」的续导：db 里已有
// 部分行（与源文件重叠），源文件仍在。重跑应去重不丢行——重叠行按
// first-wins 保留库内值，文件行只补缺口。
func TestImportResumePartial(t *testing.T) {
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

	// 预置断点残留：d1 与文件重叠、ghost 是文件外的存量行、h3 占 id=1、
	// quota (default,1700000000) 与文件行撞唯一键。
	started := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	if _, err := s.InsertLog(ctx, &LogRow{Dir: "d1", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertLog(ctx, &LogRow{Dir: "ghost", StartedAt: started}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertToken(ctx, &TokenRow{Token: "h3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertQuotaSample(ctx, &QuotaSample{At: 1700000000, Account: "default"}); err != nil {
		t.Fatal(err)
	}

	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}

	// logs：d1 去重（first-wins 留库内空 method）、d2 入库、ghost 不丢 → 3。
	if n := tableCount(t, s, "logs"); n != 3 {
		t.Fatalf("logs = %d, want 3", n)
	}
	var method string
	if err := s.db.QueryRowContext(ctx,
		`SELECT method FROM logs WHERE dir='d1'`).Scan(&method); err != nil {
		t.Fatal(err)
	}
	if method != "" {
		t.Fatalf("d1 method = %q, want '' (pre-existing row wins over file)", method)
	}

	// tokens：h3 按 token 列合并到既有 id=1（文件字段覆盖，但 id 不动）；
	// h7 文件 id=7 空闲 → 保留。
	tokens, err := s.ListTokens(ctx)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("tokens = %v %v", tokens, err)
	}
	byToken := map[string]*TokenRow{}
	for _, tk := range tokens {
		byToken[tk.Token] = tk
	}
	if byToken["h3"].ID != 1 || byToken["h3"].Description != "three" ||
		byToken["h3"].DailyUsedMicroUSD != 500 {
		t.Fatalf("h3 merge wrong: %+v", byToken["h3"])
	}
	if byToken["h7"].ID != 7 {
		t.Fatalf("h7 id = %d, want 7", byToken["h7"].ID)
	}

	// quota：撞键行 OR IGNORE，库内 NULL 行赢 → 仍 2 行且首行 remaining 为 nil。
	qs, err := s.ListQuotaSamples(ctx, "default", 0, 0)
	if err != nil || len(qs) != 2 {
		t.Fatalf("quota = %v %v", qs, err)
	}
	if qs[0].DailyRemaining != nil {
		t.Fatalf("dup quota row overwrote existing: %v", *qs[0].DailyRemaining)
	}
	migrated(t, filepath.Join(logRoot, "index.jsonl"))
	migrated(t, filepath.Join(stateDir, "auth_tokens.json"))
}

// TestImportLargeIndex 灌 ~5MB index.jsonl，耗时打日志做观感锚点。
func TestImportLargeIndex(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	var size int
	for i := 0; size < 5<<20; i++ {
		l := fmt.Sprintf(`{"dir":"bulk-%06d","started_at":"2026-09-17T10:%02d:%02d.123Z","duration_ms":%d,"api":"anthropic","method":"POST","path":"/v1/messages","status_code":200,"result":"completed","requested_model":"devin","model":"devin","response_model":"devin","stream":true,"input_tokens":%d,"output_tokens":%d,"cache_read_tokens":%d,"total_tokens":%d,"credit_cost":%d,"upstream_request_id":"up-%06d","client_ip":"10.0.0.1","key_hash":"kh-%06d","client_request_id":"cr-%06d","retries":1,"account":"default","repairs":15}`,
			i, i/60%60, i%60, i%5000, 1000+i, 200+i, 500+i, 1200+i, i%7, i, i, i)
		size += len(l) + 1
		lines = append(lines, l)
	}
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl"), lines)

	s := openTemp(t)
	ctx := context.Background()
	start := time.Now()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	dur := time.Since(start)
	t.Logf("imported %d index lines (%.1f MB) in %s", len(lines), float64(size)/(1<<20), dur)
	if dur > 30*time.Second {
		t.Fatalf("import too slow: %s", dur)
	}
	if n := tableCount(t, s, "logs"); n != len(lines) {
		t.Fatalf("logs = %d, want %d", n, len(lines))
	}
}

// TestImportEmptyFiles 空文件/空 JSON 一律按「零行导入」处理并照常改名。
func TestImportEmptyFiles(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTextFile(t, filepath.Join(logRoot, "index.jsonl"), "")
	writeTextFile(t, filepath.Join(stateDir, "auth_tokens.json"), `{}`)
	writeTextFile(t, filepath.Join(stateDir, "models.json"), `{}`)
	writeTextFile(t, filepath.Join(stateDir, "panel-settings.json"), `{"values":{}}`)
	writeTextFile(t, filepath.Join(logRoot, "quota.jsonl"), "")
	writeTextFile(t, filepath.Join(logRoot, "gate-state.json"), `{}`)

	s := openTemp(t)
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	for _, table := range []string{"logs", "auth_tokens", "model_registry", "settings", "quota_samples"} {
		if n := tableCount(t, s, table); n != 0 {
			t.Fatalf("%s = %d, want 0", table, n)
		}
	}
	if v, ok, _ := s.GetState(ctx, "gate:default"); !ok || v != `{}` {
		t.Fatalf("gate:default = %q,%v", v, ok)
	}
	for _, p := range []string{
		filepath.Join(logRoot, "index.jsonl"),
		filepath.Join(logRoot, "quota.jsonl"),
		filepath.Join(logRoot, "gate-state.json"),
		filepath.Join(stateDir, "auth_tokens.json"),
		filepath.Join(stateDir, "models.json"),
		filepath.Join(stateDir, "panel-settings.json"),
	} {
		migrated(t, p)
	}
}

// TestImportCorruptWholeFileAborts 整文件 JSON 源损坏时 fail-loud：报错中止，
// 已完成的源已改名落库，损坏源保留原名，排在后面的源不再尝试；修好文件
// 再跑能把剩余源补齐。这固定「坏文件不会被跳过改名」的现行语义。
func TestImportCorruptWholeFileAborts(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl"), []string{indexLine("ok-1")})
	writeTextFile(t, filepath.Join(stateDir, "auth_tokens.json"), `{not json`)
	writeTextFile(t, filepath.Join(stateDir, "models.json"), `{"models":{"a":{"disabled":true}}}`)
	writeJSONLines(t, filepath.Join(logRoot, "quota.jsonl"),
		[]string{`{"at":1700000000,"account":"default"}`})

	s := openTemp(t)
	ctx := context.Background()
	err := s.ImportLegacy(ctx, stateDir, logRoot)
	if err == nil || !strings.Contains(err.Error(), "import auth_tokens") {
		t.Fatalf("err = %v, want wrapped 'import auth_tokens' failure", err)
	}
	// index 已提交改名；auth_tokens 保留原名；models/quota 连尝试都没发生。
	migrated(t, filepath.Join(logRoot, "index.jsonl"))
	for _, p := range []string{
		filepath.Join(stateDir, "auth_tokens.json"),
		filepath.Join(stateDir, "models.json"),
		filepath.Join(logRoot, "quota.jsonl"),
	} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("%s should stay in place", p)
		}
	}
	if n := tableCount(t, s, "logs"); n != 1 {
		t.Fatalf("logs = %d, want 1", n)
	}
	if n := tableCount(t, s, "model_registry"); n != 0 {
		t.Fatalf("model_registry = %d, want 0 (aborted before reaching)", n)
	}
	if hasState(t, s, "imported:auth_tokens") || hasState(t, s, "import_base_done") {
		t.Fatal("failed source must not be marked done")
	}

	// 修好文件再跑：剩余源补齐；token 的坏 created_at 静默落成 0。
	writeTextFile(t, filepath.Join(stateDir, "auth_tokens.json"),
		`{"next_id":2,"tokens":[{"id":1,"token":"t1","created_at":"bogus"}]}`)
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("retry after fix: %v", err)
	}
	tokens, err := s.ListTokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].CreatedAt != 0 {
		t.Fatalf("tokens = %v %v, want one row with CreatedAt=0", tokens, err)
	}
	if n := tableCount(t, s, "model_registry"); n != 1 {
		t.Fatalf("model_registry = %d, want 1", n)
	}
	if n := tableCount(t, s, "quota_samples"); n != 1 {
		t.Fatalf("quota_samples = %d, want 1", n)
	}
	if !hasState(t, s, "import_base_done") {
		t.Fatal("import_base_done missing after successful retry")
	}
}

// TestImportMigratedSemantics .migrated 是归档而非屏障：新同名文件照常
// 导入并覆盖旧归档；既有 .migrated 自身不作为数据源被拾取。
func TestImportMigratedSemantics(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// 旧归档里放着绝不能再进库的历史行。
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl.migrated"),
		[]string{indexLine("old-1"), indexLine("old-2"), indexLine("old-3")})
	writeTextFile(t, filepath.Join(stateDir, "auth_tokens.json.migrated"),
		`{"tokens":[{"id":1,"token":"archived"}]}`)
	writeTextFile(t, filepath.Join(logRoot, "gate-state-ghost.json.migrated"), `{}`)
	writeJSONLines(t, filepath.Join(logRoot, "index.jsonl"),
		[]string{indexLine("new-1"), indexLine("new-2")})

	s := openTemp(t)
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n := tableCount(t, s, "logs"); n != 2 {
		t.Fatalf("logs = %d, want 2 (new file only)", n)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs WHERE dir LIKE 'old-%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("migrated archive rows leaked into db")
	}
	if n := tableCount(t, s, "auth_tokens"); n != 0 {
		t.Fatalf("auth_tokens = %d, archived file should not be imported", n)
	}
	if hasState(t, s, "gate:ghost") {
		t.Fatal("gate-state-ghost.json.migrated must not be picked up")
	}
	// 新文件改名覆盖旧归档。
	data, err := os.ReadFile(filepath.Join(logRoot, "index.jsonl.migrated"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new-1") {
		t.Fatal("index.jsonl.migrated not overwritten by fresh file")
	}
}

// TestImportQuotaMixedAccounts 单号时代无 account 行与双号行混排：
// 缺席 account 落空串；(account,at) 撞键 first-wins；坏行跳过。
func TestImportQuotaMixedAccounts(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeJSONLines(t, filepath.Join(logRoot, "quota.jsonl"), []string{
		`{"at":100}`,                     // 单号时代：无 account
		`{"at":200,"account":"default"}`, //
		`{"at":300,"account":"randall","daily_remaining":7.5}`,  //
		`{"at":200,"account":"default","daily_remaining":99.9}`, // 撞 (account,at)，IGNORE
		`{"at":100,"account":"default"}`,                        // 与首行同 at 但不同 account
		`{"at":"nope"}`,                                         // 坏行
	})

	s := openTemp(t)
	ctx := context.Background()
	if err := s.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("ImportLegacy: %v", err)
	}
	if n := tableCount(t, s, "quota_samples"); n != 4 {
		t.Fatalf("quota_samples = %d, want 4", n)
	}
	var anon int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM quota_samples WHERE account=''`).Scan(&anon); err != nil {
		t.Fatal(err)
	}
	if anon != 1 {
		t.Fatalf("account='' rows = %d, want 1", anon)
	}
	qs, err := s.ListQuotaSamples(ctx, "default", 0, 0)
	if err != nil || len(qs) != 2 {
		t.Fatalf("default samples = %v %v", qs, err)
	}
	if qs[0].At != 100 || qs[1].At != 200 {
		t.Fatalf("default ats = %d,%d", qs[0].At, qs[1].At)
	}
	if qs[1].DailyRemaining != nil {
		t.Fatalf("(default,200) daily_remaining = %v, want nil (first insert wins)",
			*qs[1].DailyRemaining)
	}
	if qs, err = s.ListQuotaSamples(ctx, "randall", 0, 0); err != nil || len(qs) != 1 {
		t.Fatalf("randall samples = %v %v", qs, err)
	}
	migrated(t, filepath.Join(logRoot, "quota.jsonl"))
}
