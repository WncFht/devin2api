// 本文件验证账号池装配层：applyAccounts 的生效集 merge / 校验拒绝 /
// 墓碑 GC，AccountOps 闭包的 sentinel 语义与行回滚，以及
// devinConfigsFrom 的 per-lane TokenSource。全部打真 sqlite + 真
// Pool——overlay 三态（config/panel/tombstoned）只有端到端才有意义。
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// writeTestConfig 落一份 config.yaml 并返回加载后的快照。
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

// TestApplyAccountsMergesOverlay 验证重推管线全貌：disabled 行压住
// config 号、panel 行加新号、死墓碑被 GC；进池的恰是「非墓碑且未
// 停用」子集，resolved 视图仍含 tombstoned/disabled 条目供面板展示。
func TestApplyAccountsMergesOverlay(t *testing.T) {
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
	resolved, _, err := applyAccounts(ctx, cfg, configPath, dbStore, pool, nil)
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

// TestApplyAccountsRejectsInvalidSet 钉住 reload 不变式：生效集整表
// 校验失败（panel 行撞 config 号的 token）时重推整体拒绝，旧 lane
// 集合原样服役——commit 前校验，不是推了再补救。
func TestApplyAccountsRejectsInvalidSet(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	pool := testPool(t)
	if _, _, err := applyAccounts(ctx, cfg, configPath, dbStore, pool, nil); err != nil {
		t.Fatal(err)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "gamma", Token: "tok-alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := applyAccounts(ctx, cfg, configPath, dbStore, pool, nil); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("applyAccounts() err = %v, want duplicate-token rejection", err)
	}
	if got := sortedKeys(pool.AccountLaneStates()); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("lanes = %v, want unchanged [alpha beta]", got)
	}
}

// TestApplyAccountsEmptySet 验证空池形态：config 零账号 + 无行 →
// 空集合法，全部 lane 摘出，不报错。
func TestApplyAccountsEmptySet(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, "server:\n  listen: '127.0.0.1:1'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n")
	dbStore := testAccountStore(t, dir)
	pool := testPool(t)
	resolved, applied, err := applyAccounts(context.Background(), cfg, configPath, dbStore, pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != 0 || len(applied) != 0 || len(pool.AccountLaneStates()) != 0 {
		t.Fatalf("empty pool expected: resolved=%v applied=%v lanes=%v", resolved, applied, pool.AccountLaneStates())
	}
}

// TestDevinConfigSnapshot 验证 settings 快照源的空池回落：有 lane 读
// 首 lane 活配置，空池回落 base 模板（文件值口径而非零值）。
func TestDevinConfigSnapshot(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	runtimeConfigPtr.Store(&runtimeConfigState{cfg: cfg, loadedAt: time.Now()})
	t.Cleanup(func() { runtimeConfigPtr.Store(nil) })

	empty := testPool(t)
	got := devinConfigSnapshot(empty)
	if got.Model != "m" || got.BaseURL != "https://example.com" || got.Name != "" {
		t.Fatalf("empty-pool snapshot = %+v, want base template", got)
	}
	pool := testPool(t)
	if _, _, err := applyAccounts(context.Background(), cfg, configPath, dbStore, pool, nil); err != nil {
		t.Fatal(err)
	}
	if got := devinConfigSnapshot(pool); got.Name != "alpha" {
		t.Fatalf("live snapshot name = %q, want alpha (first lane)", got.Name)
	}
}

// TestDevinConfigsFromTokenSource 验证两类凭据源：literal 型按名重解
// 生效集（config 值与行覆盖同权），credentials_file 型现读文件。
func TestDevinConfigsFromTokenSource(t *testing.T) {
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
	lanes := devinConfigsFrom(cfg, cfg.Devin.Accounts, configPath, dbStore)
	if len(lanes) != 2 {
		t.Fatalf("lanes = %d", len(lanes))
	}
	// credentials_file 型：文件改写后 TokenSource 跟随。
	filed := lanes[1]
	if got := filed.TokenSource(); got != "tok-file" {
		t.Fatalf("cf TokenSource = %q", got)
	}
	if err := os.WriteFile(credFile, []byte("windsurf_api_key = \"tok-file2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := filed.TokenSource(); got != "tok-file2" {
		t.Fatalf("cf TokenSource after rotate = %q", got)
	}
	// literal 型：行覆盖赢 config 值。
	alpha := lanes[0]
	if got := alpha.TokenSource(); got != "tok-alpha" {
		t.Fatalf("literal TokenSource = %q", got)
	}
	if err := dbStore.UpsertAccount(ctx, &store.AccountRow{Name: "alpha", Token: "tok-row"}); err != nil {
		t.Fatal(err)
	}
	if got := alpha.TokenSource(); got != "tok-row" {
		t.Fatalf("literal TokenSource with row override = %q, want tok-row", got)
	}
}

// TestAccountOpsLifecycle 走通 ops 全生命周期：建号（含重名与缺
// base_url/model 预检）、改号（含失败回滚）、删号（config 名墓碑化
// vs panel 名物理删）、restore、TokenOf 与 ClearCooldown。
func TestAccountOpsLifecycle(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir, testAccountsYAML)
	dbStore := testAccountStore(t, dir)
	ctx := context.Background()
	runtimeConfigPtr.Store(&runtimeConfigState{cfg: cfg, loadedAt: time.Now()})
	t.Cleanup(func() { runtimeConfigPtr.Store(nil) })
	pool := testPool(t)
	if _, _, err := applyAccounts(ctx, cfg, configPath, dbStore, pool, nil); err != nil {
		t.Fatal(err)
	}
	ops := newAccountOps(configPath, dbStore, pool, nil)

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

// TestAccountOpsCredentialsFile 验证建号时 credentials_file 锚定入库
// 与 TokenOf 现读文件——行里存的必须是绝对路径，merge 不做二次锚定。
func TestAccountOpsCredentialsFile(t *testing.T) {
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
	runtimeConfigPtr.Store(&runtimeConfigState{cfg: cfg, loadedAt: time.Now()})
	t.Cleanup(func() { runtimeConfigPtr.Store(nil) })
	pool := testPool(t)
	ops := newAccountOps(configPath, dbStore, pool, nil)

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

func mustEffective(t *testing.T, ops ccpanel.AccountOps, ctx context.Context) []store.ResolvedAccount {
	t.Helper()
	resolved, err := ops.Effective(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
