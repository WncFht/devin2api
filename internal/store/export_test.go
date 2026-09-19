package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// readJSONLines 逐行解析一个 JSONL 文件为通用对象序列。
func readJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("parse line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestExportLegacyRoundTrip 走「文件夹具 → ImportLegacy → ExportLegacy」
// 全环：六个源逐一核对行数与关键字段，确认导出文件能被旧二进制再消费
// （语义等价即可——next_id 重建、started_at 归一化等已知差异单列断言）。
func TestExportLegacyRoundTrip(t *testing.T) {
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

	outDir := filepath.Join(base, "export")
	outLogRoot := filepath.Join(outDir, "logs")
	if err := os.MkdirAll(outLogRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err := s.ExportLegacy(ctx, outDir, outLogRoot)
	if err != nil {
		t.Fatalf("ExportLegacy: %v", err)
	}
	if len(rep.Notices) != 0 {
		t.Fatalf("unexpected notices: %v", rep.Notices)
	}
	if rep.DebugRows != 0 {
		t.Fatalf("DebugRows = %d, want 0", rep.DebugRows)
	}
	// 5 固定文件 + 2 gate-state。
	if len(rep.Written) != 7 {
		t.Fatalf("Written = %v, want 7 files", rep.Written)
	}

	// index.jsonl：两行还原，字段与原行语义一致；截断行不入库也不出。
	lines := readJSONLines(t, filepath.Join(outLogRoot, "index.jsonl"))
	if len(lines) != 2 {
		t.Fatalf("index lines = %d, want 2", len(lines))
	}
	if lines[0]["dir"] != "d1" || lines[1]["dir"] != "d2" {
		t.Fatalf("dirs = %v,%v", lines[0]["dir"], lines[1]["dir"])
	}
	for i, want := range map[string]any{
		"started_at": "2026-09-17T10:00:00.5Z", "method": "POST",
		"path": "/v1/messages", "result": "completed",
	} {
		if lines[0][i] != want {
			t.Fatalf("line0[%s] = %v, want %v", i, lines[0][i], want)
		}
	}
	if lines[1]["client_request_id"] != "panel-probe" {
		t.Fatalf("probe line client_request_id = %v", lines[1]["client_request_id"])
	}

	// auth_tokens.json：令牌字段还原；next_id 重建为 max(id)+1=8
	//（原文件 10 是富余水位，不随库保存，属已知差异）。
	var tokFile legacyTokenFile
	data, err := os.ReadFile(filepath.Join(outDir, "auth_tokens.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &tokFile); err != nil {
		t.Fatal(err)
	}
	if tokFile.NextID != 8 || len(tokFile.Tokens) != 2 {
		t.Fatalf("next_id=%d tokens=%d", tokFile.NextID, len(tokFile.Tokens))
	}
	if tokFile.Tokens[0].ID != 3 || tokFile.Tokens[0].Hash != "h3" ||
		tokFile.Tokens[0].CreatedAt != "2026-09-01T00:00:00Z" ||
		tokFile.Tokens[0].DailyUsedMicroUSD != 500 {
		t.Fatalf("h3 = %+v", tokFile.Tokens[0])
	}
	if tokFile.Tokens[1].ID != 7 || tokFile.Tokens[1].IsActive {
		t.Fatalf("h7 = %+v", tokFile.Tokens[1])
	}

	// models.json：注册项语义还原（updated_at 不进文件）。
	var regFile legacyRegistryFile
	data, err = os.ReadFile(filepath.Join(outDir, "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &regFile); err != nil {
		t.Fatal(err)
	}
	if len(regFile.Models) != 2 || regFile.Models["a"].RedirectModel != "b" ||
		!regFile.Models["c"].Disabled {
		t.Fatalf("models = %+v", regFile.Models)
	}

	// panel-settings.json：values 与 updated 原样还原。
	var setFile legacySettingsFile
	data, err = os.ReadFile(filepath.Join(outDir, "panel-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &setFile); err != nil {
		t.Fatal(err)
	}
	if setFile.Values["debug_log_enabled"] != "false" || setFile.Values["retention_days"] != "7" ||
		setFile.Updated["debug_log_enabled"] != 111 {
		t.Fatalf("settings = %+v", setFile)
	}

	// quota.jsonl：两行还原，daily_remaining 指针语义保留。
	qlines := readJSONLines(t, filepath.Join(outLogRoot, "quota.jsonl"))
	if len(qlines) != 2 {
		t.Fatalf("quota lines = %d, want 2", len(qlines))
	}
	if qlines[0]["daily_remaining"] != 50.5 {
		t.Fatalf("quota[0] daily_remaining = %v", qlines[0]["daily_remaining"])
	}
	if _, ok := qlines[1]["daily_remaining"]; !ok {
		t.Fatal("quota[1] daily_remaining missing, want explicit null")
	}

	// gate-state：两 lane 拆回两份文件，内容逐字节还原。
	for name, want := range map[string]string{
		"gate-state.json":       `{"limited_until":"2026-09-17T12:00:00Z"}`,
		"gate-state-bravo.json": `{"limited_until":"2026-09-17T13:00:00Z"}`,
	} {
		data, err := os.ReadFile(filepath.Join(outLogRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s = %q, want %q", name, data, want)
		}
	}

	// 再导一次：导出文件本身是合法输入——重导入同一库行数不涨
	//（验证导出形状与导入解析互逆）。
	dup := openTemp(t)
	defer func() { _ = dup.Close() }()
	if err := dup.ImportLegacy(ctx, outDir, outLogRoot); err != nil {
		t.Fatalf("re-import exported files: %v", err)
	}
	if n := tableCount(t, dup, "logs"); n != 2 {
		t.Fatalf("re-imported logs = %d, want 2", n)
	}
	if n := tableCount(t, dup, "auth_tokens"); n != 2 {
		t.Fatalf("re-imported tokens = %d, want 2", n)
	}
	if n := tableCount(t, dup, "quota_samples"); n != 2 {
		t.Fatalf("re-imported quota = %d, want 2", n)
	}
	if v, ok, _ := dup.GetState(ctx, "gate:bravo"); !ok || v == "" {
		t.Fatal("re-imported gate:bravo missing")
	}
}

// TestExportLegacyDivertsOccupied 目标文件已存在时改道 <name>.exported：
// 现役文件原样保留，导出落在旁路并在 Notices 里说明；二次导出覆盖
// 自己的 .exported（幂等重跑）。
func TestExportLegacyDivertsOccupied(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := Open(filepath.Join(base, "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.InsertLog(ctx, &LogRow{Dir: "d9", Method: "POST"}); err != nil {
		t.Fatal(err)
	}

	// 占住 index.jsonl（模拟旧文件仍在/还原过 .migrated）。
	writeTextFile(t, filepath.Join(logRoot, "index.jsonl"), `{"dir":"live"}`+"\n")

	rep, err := s.ExportLegacy(ctx, stateDir, logRoot)
	if err != nil {
		t.Fatalf("ExportLegacy: %v", err)
	}
	exported := filepath.Join(logRoot, "index.jsonl.exported")
	if len(rep.Notices) != 1 || !strings.Contains(rep.Notices[0], ".exported") {
		t.Fatalf("notices = %v", rep.Notices)
	}
	lines := readJSONLines(t, exported)
	if len(lines) != 1 || lines[0]["dir"] != "d9" {
		t.Fatalf("exported lines = %v", lines)
	}
	// 现役文件原样未动。
	data, err := os.ReadFile(filepath.Join(logRoot, "index.jsonl"))
	if err != nil || !strings.Contains(string(data), `"live"`) {
		t.Fatalf("live file touched: %v %q", err, data)
	}

	// 重跑仍写 .exported 且不报错。
	if _, err := s.ExportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("re-export: %v", err)
	}
}

// TestExportLegacyPartialFailure 单源失败不阻断其余：把一个源的改道
// 落点也做成不可写（目标与 .exported 双目录占位 → rename 必失败），
// 其余源照常落盘，错误汇总返回。
func TestExportLegacyPartialFailure(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	// models.json 与其改道名双双被目录占住 → tmp 可写但 rename 必败。
	for _, p := range []string{
		filepath.Join(stateDir, "models.json"),
		filepath.Join(stateDir, "models.json.exported"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	s := openTemp(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if _, err := s.InsertToken(ctx, &TokenRow{Token: "hx"}); err != nil {
		t.Fatal(err)
	}

	rep, err := s.ExportLegacy(ctx, stateDir, logRoot)
	if err == nil || !strings.Contains(err.Error(), "export models") {
		t.Fatalf("err = %v, want 'export models' failure", err)
	}
	// 其余源照常落盘：auth_tokens/index/quota/settings 四个固定文件在场。
	for _, p := range []string{
		filepath.Join(stateDir, "auth_tokens.json"),
		filepath.Join(stateDir, "panel-settings.json"),
		filepath.Join(logRoot, "index.jsonl"),
		filepath.Join(logRoot, "quota.jsonl"),
	} {
		if _, statErr := os.Stat(p); statErr != nil {
			t.Fatalf("%s missing despite sibling failure: %v", p, statErr)
		}
	}
	if len(rep.Written) != 4 {
		t.Fatalf("Written = %v, want 4", rep.Written)
	}
}

// TestExportLegacyAccountsYAML upstream_accounts 活行导成可粘回
// config.yaml devin.accounts 的 yaml 片段：enabled 进 accounts: 列表，
// disabled 注释化列出（防盲粘回复活），墓碑不导；产出带「手工粘回、
// 不自动回灌」旁注，且重导入不吃该文件。
func TestExportLegacyAccountsYAML(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	s := openTemp(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	for _, r := range []*AccountRow{
		{Name: "alpha", Token: "sess-y", CreatedAt: 1},
		{Name: "bravo", CredentialsFile: "/p/c.toml", Disabled: true, CreatedAt: 2},
		{Name: "dead", Token: "x", Deleted: true, CreatedAt: 3},
	} {
		if err := s.UpsertAccount(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	rep, err := s.ExportLegacy(ctx, stateDir, logRoot)
	if err != nil {
		t.Fatalf("ExportLegacy: %v", err)
	}
	target := filepath.Join(stateDir, "upstream_accounts.yaml")
	found := false
	for _, p := range rep.Written {
		if p == target {
			found = true
		}
	}
	if !found {
		t.Fatalf("Written = %v, missing %s", rep.Written, target)
	}
	var sawPasteNotice bool
	for _, n := range rep.Notices {
		if strings.Contains(n, "paste") || strings.Contains(n, "never auto-imported") {
			sawPasteNotice = true
		}
	}
	if !sawPasteNotice {
		t.Fatalf("notices = %v, want manual-paste notice", rep.Notices)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "dead") {
		t.Fatalf("tombstone leaked into export:\n%s", content)
	}
	if !strings.Contains(content, "#   - name: bravo") ||
		!strings.Contains(content, "#     credentials_file: /p/c.toml") {
		t.Fatalf("disabled bravo should be a commented entry:\n%s", content)
	}
	var doc struct {
		Accounts []legacyAccountEntry `yaml:"accounts"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("export is not valid yaml: %v\n%s", err, content)
	}
	if len(doc.Accounts) != 1 || doc.Accounts[0].Name != "alpha" ||
		doc.Accounts[0].Token != "sess-y" {
		t.Fatalf("accounts = %+v, want [alpha]", doc.Accounts)
	}

	// 回灌豁免：ImportLegacy 不认这个文件——导出目录原样再导入，
	// 不报错且 upstream_accounts 保持空表。
	dup := openTemp(t)
	defer func() { _ = dup.Close() }()
	if err := dup.ImportLegacy(ctx, stateDir, logRoot); err != nil {
		t.Fatalf("re-import with yaml present: %v", err)
	}
	if n := tableCount(t, dup, "upstream_accounts"); n != 0 {
		t.Fatalf("upstream_accounts = %d, want 0 (yaml never auto-imported)", n)
	}
}

// TestExportAccountsSkipsEmpty 无活行（空表或全墓碑）不产文件、不进 Written。
func TestExportAccountsSkipsEmpty(t *testing.T) {
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	logRoot := filepath.Join(stateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	s := openTemp(t)
	defer func() { _ = s.Close() }()
	ctx := context.Background()
	if err := s.UpsertAccount(ctx, &AccountRow{Name: "dead", Deleted: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	rep, err := s.ExportLegacy(ctx, stateDir, logRoot)
	if err != nil {
		t.Fatalf("ExportLegacy: %v", err)
	}
	target := filepath.Join(stateDir, "upstream_accounts.yaml")
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("%s exists despite no live rows", target)
	}
	for _, p := range rep.Written {
		if p == target {
			t.Fatalf("Written contains skipped source %s", p)
		}
	}
}
