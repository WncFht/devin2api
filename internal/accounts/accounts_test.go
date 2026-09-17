// 本文件验证账号域装配层：Apply 的生效集 merge / 校验拒绝 / 墓碑 GC，
// Ops 闭包的 sentinel 语义与行回滚，以及 devinConfigs 的 per-lane
// TokenSource。全部打真 sqlite + 真 Pool——overlay 三态
// （config/panel/tombstoned）只有端到端才有意义。
package accounts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/store"
)

const testAccountsYAML = `server:
  listen: '127.0.0.1:1'
devin:
  base_url: 'https://example.com'
  model: 'm'
  accounts:
    - {name: alpha, token: 'tok-alpha'}
    - {name: beta, token: 'tok-beta'}
`

// writeTestConfig 落一份 config.yaml 并返回路径与加载后的快照。
func writeTestConfig(t *testing.T, dir, body string) (string, config.Config) {
	t.Helper()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return configPath, cfg
}

func testAccountStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	dbStore, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbStore.Close() })
	return dbStore
}

func testPool(t *testing.T) *devin.Pool {
	t.Helper()
	pool, err := devin.NewPool(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sortedKeys 提取 lane 名集排序后供比对。
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestApplyMergesOverlay 验证重推管线全貌：disabled 行压住 config 号、
// panel 行加新号、死墓碑被 GC；进池的恰是「非墓碑且未停用」子集，
// resolved 视图仍含 tombstoned/disabled 条目供面板展示。
func TestApplyMergesOverlay(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "beta", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "gamma", Token: "tok-gamma"}); err != nil {
		t.Fatal(err)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "ghost", Token: "t", Deleted: true}); err != nil {
		t.Fatal(err)
	}
	pool := testPool(t)
	rt := New(configPath, dir, dbStore, pool)
	resolved, _, err := rt.Apply(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sortedKeys(pool.AccountLaneStates()); !slices.Equal(got, []string{"alpha", "gamma"}) {
		t.Fatalf("lanes = %v, want [alpha gamma]", got)
	}
	if findResolved(resolved, "beta") == nil || !findResolved(resolved, "beta").Disabled {
		t.Fatalf("beta should stay in resolved view as disabled: %+v", resolved)
	}
	if findResolved(resolved, "ghost") != nil {
		t.Fatal("dead tombstone must not appear in resolved view")
	}
	if _, ok, err := dbStore.GetAccount(ctx, "ghost"); err != nil || ok {
		t.Fatalf("dead tombstone should be GC'd, ok=%v err=%v", ok, err)
	}
}

// TestApplyRejectsInvalidSet 钉住 reload 不变式：生效集整表校验失败
// （panel 行撞 config 号的 token）时重推整体拒绝，旧 lane 集合原样
// 服役——commit 前校验，不是推了再补救。
func TestApplyRejectsInvalidSet(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	pool := testPool(t)
	rt := New(configPath, dir, dbStore, pool)
	if _, _, err := rt.Apply(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "gamma", Token: "tok-alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := rt.Apply(ctx, cfg, nil); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Apply() err = %v, want duplicate-token rejection", err)
	}
	if got := sortedKeys(pool.AccountLaneStates()); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("lanes = %v, want unchanged [alpha beta]", got)
	}
}

// TestApplyEmptySet 验证空池形态：config 零账号 + 无行 → 空集合法，
// 全部 lane 摘出，不报错。
func TestApplyEmptySet(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, "server:\n  listen: '127.0.0.1:1'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n")
	dbStore := testAccountStore(t, dir)
	pool := testPool(t)
	rt := New(configPath, dir, dbStore, pool)
	resolved, applied, err := rt.Apply(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 || len(applied) != 0 || len(pool.AccountLaneStates()) != 0 {
		t.Fatalf("empty pool expected: resolved=%v applied=%v lanes=%v", resolved, applied, pool.AccountLaneStates())
	}
}

// TestSnapshot 验证 settings 快照源的空池回落：有 lane 读首 lane 活
// 配置，空池回落 base 模板（文件值口径而非零值）。
func TestSnapshot(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	pool := testPool(t)
	rt := New(configPath, dir, dbStore, pool)
	rt.CommitConfig(cfg)

	got := rt.Snapshot()
	if got.Model != "m" || got.Endpoint.BaseURL != "https://example.com" || got.Identity.Name != "" {
		t.Fatalf("empty-pool snapshot = %+v, want base template", got)
	}
	if _, _, err := rt.Apply(context.Background(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	if got := rt.Snapshot(); got.Identity.Name != "alpha" {
		t.Fatalf("live snapshot name = %q, want alpha (first lane)", got.Identity.Name)
	}
}

// TestDevinConfigsTokenSource 验证两类凭据源：literal 型按名重解生效
// 集（config 值与行覆盖同权），credentials_file 型现读文件。
func TestDevinConfigsTokenSource(t *testing.T) {
	dir := t.TempDir()
	credFile := filepath.Join(dir, "credentials.toml")
	if err := os.WriteFile(credFile, []byte("windsurf_api_key = \"tok-file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, cfg := writeTestConfig(t, dir, `server:
  listen: '127.0.0.1:1'
devin:
  base_url: 'https://example.com'
  model: 'm'
  accounts:
    - {name: alpha, token: 'tok-alpha'}
    - {name: filed, credentials_file: 'credentials.toml'}
`)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	rt := New(configPath, dir, dbStore, testPool(t))
	lanes := rt.devinConfigs(cfg, cfg.Devin.Accounts)
	if len(lanes) != 2 {
		t.Fatalf("lanes = %d", len(lanes))
	}
	// credentials_file 型：文件改写后 TokenSource 跟随。
	filed := lanes[1]
	if got := filed.Identity.TokenSource(); got != "tok-file" {
		t.Fatalf("cf TokenSource = %q", got)
	}
	if err := os.WriteFile(credFile, []byte("windsurf_api_key = \"tok-file2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := filed.Identity.TokenSource(); got != "tok-file2" {
		t.Fatalf("cf TokenSource after rotate = %q", got)
	}
	// literal 型：行覆盖赢 config 值。
	alpha := lanes[0]
	if got := alpha.Identity.TokenSource(); got != "tok-alpha" {
		t.Fatalf("literal TokenSource = %q", got)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "alpha", Token: "tok-row"}); err != nil {
		t.Fatal(err)
	}
	if got := alpha.Identity.TokenSource(); got != "tok-row" {
		t.Fatalf("literal TokenSource with row override = %q, want tok-row", got)
	}
}

// TestOpsLifecycle 走通 ops 全生命周期：建号（含重名与缺 base_url/
// model 预检）、改号（含失败回滚）、删号（config 名墓碑化 vs panel
// 名物理删）、restore、TokenOf 与 ClearCooldown。
func TestOpsLifecycle(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	pool := testPool(t)
	rt := New(configPath, dir, dbStore, pool)
	rt.CommitConfig(cfg)
	if _, _, err := rt.Apply(ctx, cfg, nil); err != nil {
		t.Fatal(err)
	}
	ops := rt.Ops(nil)

	// 建号：撞 config 名 → ErrAccountExists；零凭据 → 校验错。
	if _, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "alpha", Token: "x"}); !errors.Is(err, store.ErrAccountExists) {
		t.Fatalf("create config name err = %v, want ErrAccountExists", err)
	}
	if _, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "gamma"}); err == nil {
		t.Fatal("create without credential should fail validation")
	}
	created, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "gamma", Token: "tok-gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if created.Source != store.AccountSourcePanel || created.Token != "tok-gamma" {
		t.Fatalf("created = %+v", created)
	}
	if _, ok := pool.AccountLaneStates()["gamma"]; !ok {
		t.Fatal("gamma lane missing after create")
	}
	if _, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "gamma", Token: "y"}); !errors.Is(err, store.ErrAccountExists) {
		t.Fatalf("create live row err = %v, want ErrAccountExists", err)
	}

	// 改号：撞 token 的干跑失败要回滚行——行保持写前值，lane 不死。
	badToken := "tok-alpha"
	if _, err := ops.Update(ctx, "gamma", ccpanel.AccountPatch{Token: &badToken}); err == nil {
		t.Fatal("update to duplicate token should fail")
	}
	if row, ok, _ := dbStore.GetAccount(ctx, "gamma"); !ok || row.Token != "tok-gamma" {
		t.Fatalf("row after failed update = %+v ok=%v, want rolled back", row, ok)
	}
	newToken := "tok-gamma2"
	updated, err := ops.Update(ctx, "gamma", ccpanel.AccountPatch{Token: &newToken})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Token != "tok-gamma2" {
		t.Fatalf("updated.Token = %q", updated.Token)
	}
	// config 名的 Update 建行覆盖：disabled=true 摘 lane。
	off := true
	disabled, err := ops.Update(ctx, "beta", ccpanel.AccountPatch{Disabled: &off})
	if err != nil {
		t.Fatal(err)
	}
	if !disabled.Disabled || disabled.Source != store.AccountSourceConfig {
		t.Fatalf("disabled beta = %+v", disabled)
	}
	if _, ok := pool.AccountLaneStates()["beta"]; ok {
		t.Fatal("beta lane should be gone while disabled")
	}
	if _, err := ops.Update(ctx, "nosuch", ccpanel.AccountPatch{Disabled: &off}); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	// 删号：config 名 → 墓碑（可 restore）；panel 名 → 物理删。
	deleted, err := ops.Delete(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted.ConfigDeclared {
		t.Fatal("delete should return pre-delete view with ConfigDeclared=true")
	}
	if acc := findResolved(mustEffective(t, ops, ctx), "beta"); acc == nil || acc.Source != store.AccountSourceTombstoned {
		t.Fatalf("beta should be tombstoned, resolved = %+v", mustEffective(t, ops, ctx))
	}
	if _, err := ops.Delete(ctx, "beta"); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("re-delete tombstoned err = %v", err)
	}
	if _, err := ops.Update(ctx, "beta", ccpanel.AccountPatch{Disabled: &off}); !errors.Is(err, store.ErrAccountTombstoned) {
		t.Fatalf("update tombstoned err = %v", err)
	}
	restored, err := ops.Restore(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if restored.Source != store.AccountSourceConfig || !restored.Disabled {
		t.Fatalf("restored = %+v, want config source keeping disabled override", restored)
	}
	if _, err := ops.Restore(ctx, "beta"); !errors.Is(err, store.ErrAccountNotTombstoned) {
		t.Fatalf("restore live err = %v", err)
	}
	deletedPanel, err := ops.Delete(ctx, "gamma")
	if err != nil {
		t.Fatal(err)
	}
	if deletedPanel.ConfigDeclared {
		t.Fatal("panel delete should return ConfigDeclared=false")
	}
	if _, ok, _ := dbStore.GetAccount(ctx, "gamma"); ok {
		t.Fatal("panel row should be physically deleted")
	}

	// TokenOf：literal 取生效 token；墓碑 → ErrAccountNotFound。
	if token, err := ops.TokenOf(ctx, "alpha"); err != nil || token != "tok-alpha" {
		t.Fatalf("TokenOf(alpha) = %q, %v", token, err)
	}
	if _, err := ops.TokenOf(ctx, "gamma"); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("TokenOf(deleted) err = %v", err)
	}
	// ClearCooldown：活 lane true，无名 false。
	if !ops.ClearCooldown("alpha") || ops.ClearCooldown("nosuch") {
		t.Fatal("ClearCooldown semantics broken")
	}
}

// TestOpsCredentialsFile 验证建号时 credentials_file 锚定入库与
// TokenOf 现读文件——行里存的必须是绝对路径，merge 不做二次锚定。
func TestOpsCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	credDir := filepath.Join(dir, "conf")
	if err := os.MkdirAll(credDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, "creds.toml"), []byte("windsurf_api_key = \"tok-cf\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, cfg := writeTestConfig(t, credDir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	rt := New(configPath, dir, dbStore, testPool(t))
	rt.CommitConfig(cfg)
	ops := rt.Ops(nil)

	created, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "cf", CredentialsFile: "creds.toml"})
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(created.CredentialsFile) {
		t.Fatalf("stored credentials_file not anchored: %q", created.CredentialsFile)
	}
	if token, err := ops.TokenOf(ctx, "cf"); err != nil || token != "tok-cf" {
		t.Fatalf("TokenOf(cf) = %q, %v", token, err)
	}
}

// TestResolveToken 验证离线凭据解析口径：空名取首个非墓碑、点名
// 墓碑/无名即 not found、credentials_file 现读文件。
func TestResolveToken(t *testing.T) {
	dir := t.TempDir()
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	declared := []config.DevinAccountConfig{
		{Name: "alpha", Token: "tok-alpha"},
		{Name: "beta", Token: "tok-beta"},
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "alpha", Deleted: true}); err != nil {
		t.Fatal(err)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "gamma", Token: "tok-gamma"}); err != nil {
		t.Fatal(err)
	}
	// alpha 被墓碑压住 → 空名落到 beta。
	if token, err := ResolveToken(ctx, dbStore, declared, ""); err != nil || token != "tok-beta" {
		t.Fatalf("ResolveToken(unnamed) = %q, %v, want tok-beta", token, err)
	}
	if _, err := ResolveToken(ctx, dbStore, declared, "alpha"); !errors.Is(err, store.ErrAccountNotFound) {
		t.Fatalf("ResolveToken(tombstoned) err = %v, want ErrAccountNotFound", err)
	}
	if token, err := ResolveToken(ctx, dbStore, declared, "gamma"); err != nil || token != "tok-gamma" {
		t.Fatalf("ResolveToken(gamma) = %q, %v", token, err)
	}
}

// TestRedactSecretsProxyUserinfo 验证代理 URL userinfo 从配置自省视图
// 剥掉、host 仍可辨认，accounts[].token 同样脱敏。
func TestRedactSecretsProxyUserinfo(t *testing.T) {
	fields := map[string]any{
		"devin": map[string]any{
			"proxy":    "http://alice:hunter2@proxy.local:8080",
			"accounts": []any{map[string]any{"name": "a", "token": "topsecret"}},
		},
	}
	redactSecrets(fields)
	devinSection := fields["devin"].(map[string]any)
	proxy := devinSection["proxy"].(string)
	if strings.Contains(proxy, "alice") || strings.Contains(proxy, "hunter2") {
		t.Fatalf("proxy userinfo leaked: %q", proxy)
	}
	if !strings.Contains(proxy, "proxy.local:8080") {
		t.Fatalf("proxy host should be preserved: %q", proxy)
	}
	token := devinSection["accounts"].([]any)[0].(map[string]any)["token"].(string)
	if !strings.HasPrefix(token, "sha256:") {
		t.Fatalf("account token not redacted: %q", token)
	}
}

// TestAccountOpsCredentialsContent 验证 credentials_content 粘贴上传与 CredentialOf 三源解析。
func TestAccountOpsCredentialsContent(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	rt := New(configPath, dir, dbStore, testPool(t))
	rt.CommitConfig(cfg)
	ops := rt.Ops(nil)

	content := "windsurf_api_key = \"tok-paste\"\n"
	created, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "cc", CredentialsContent: content})
	if err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(dir, "account-credentials", "cc.toml")
	if created.CredentialsFile != managed {
		t.Fatalf("CredentialsFile = %q, want managed %q", created.CredentialsFile, managed)
	}
	data, err := os.ReadFile(managed)
	if err != nil || string(data) != content {
		t.Fatalf("managed file = %q, %v", data, err)
	}
	if info, _ := os.Stat(managed); info.Mode().Perm() != 0o600 {
		t.Fatalf("managed file mode = %v, want 0600", info.Mode())
	}
	if token, err := ops.TokenOf(ctx, "cc"); err != nil || token != "tok-paste" {
		t.Fatalf("TokenOf(cc) = %q, %v", token, err)
	}
	if _, err := ops.Create(ctx, ccpanel.AccountWrite{Name: "bad", CredentialsContent: "no key here"}); err == nil {
		t.Fatal("create with unresolvable credentials_content should fail")
	}
	// Update 路径：换新内容落同一管理位；显式空串清 credentials_file。
	newContent := "windsurf_api_key = \"tok-paste2\"\n"
	if _, err := ops.Update(ctx, "cc", ccpanel.AccountPatch{CredentialsContent: &newContent}); err != nil {
		t.Fatal(err)
	}
	if token, _ := ops.TokenOf(ctx, "cc"); token != "tok-paste2" {
		t.Fatalf("TokenOf(cc) after content update = %q", token)
	}
	empty, lit := "", "tok-lit"
	if _, err := ops.Update(ctx, "cc", ccpanel.AccountPatch{CredentialsContent: &empty, Token: &lit}); err != nil {
		t.Fatal(err)
	}
	if acc := findResolved(mustEffective(t, ops, ctx), "cc"); acc == nil || acc.CredentialsFile != "" {
		t.Fatalf("cc after empty content = %+v, want credentials_file cleared", acc)
	}

	// CredentialOf：content 直解、file 锚定、token 兜底、全缺报错。
	if token, err := ops.CredentialOf(ccpanel.AccountWrite{CredentialsContent: content}); err != nil || token != "tok-paste" {
		t.Fatalf("CredentialOf(content) = %q, %v", token, err)
	}
	if token, err := ops.CredentialOf(ccpanel.AccountWrite{Token: "tok-lit"}); err != nil || token != "tok-lit" {
		t.Fatalf("CredentialOf(token) = %q, %v", token, err)
	}
	if _, err := ops.CredentialOf(ccpanel.AccountWrite{}); err == nil {
		t.Fatal("CredentialOf(empty) should fail")
	}
}

func mustEffective(t *testing.T, ops ccpanel.AccountOps, ctx context.Context) []store.ResolvedAccount {
	t.Helper()
	resolved, err := ops.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
