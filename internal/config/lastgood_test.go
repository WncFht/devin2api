// 本文件验证 last-good 配置缓存的写入/回读契约：自包含投影
// （credentials_file 摘除、token 已物化）、损坏/缺席/不可服役形态
// 各自按错误返回，兜底调用方据错误判「没有可兜底配置」。
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestConfig 落一份带 credentials_file 账号的配置并返回 Load 结果——
// 复刻事故形态：凭据经引用文件解出，缓存时 token 已物化。
func writeTestConfig(t *testing.T, dir string) (string, Config) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "creds.toml"),
		[]byte("windsurf_api_key = \"tok-from-file\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	yaml := "server:\n  listen: ':3033'\ndevin:\n  base_url: 'https://example.com'\n  model: 'm'\n  accounts:\n    - {name: a, credentials_file: 'creds.toml'}\n    - {name: b, token: 'tok-literal', api_key: 'cog_key'}\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, cfg
}

// TestWriteReadLastGoodRoundTrip 钉住缓存形态：投影里 CredentialsFile
// 摘除（兜底服役的 Apply 会再跑 resolveAccounts，留着引用文件等于把
// 「文件缺失」带进第二次加载）、token 已物化、调用方原 cfg 不被改写。
func TestWriteReadLastGoodRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPath, cfg := writeTestConfig(t, dir)

	if err := WriteLastGood(dir, configPath, cfg); err != nil {
		t.Fatalf("WriteLastGood: %v", err)
	}
	// 调用方快照必须保持 credentials_file——投影是克隆上摘除，不是原地改。
	if cfg.Devin.Accounts[0].CredentialsFile == "" {
		t.Fatal("WriteLastGood mutated caller's cfg: CredentialsFile stripped in place")
	}

	cached, err := ReadLastGood(dir)
	if err != nil {
		t.Fatalf("ReadLastGood: %v", err)
	}
	if cached.SourcePath != configPath {
		t.Fatalf("SourcePath = %q, want %q", cached.SourcePath, configPath)
	}
	if cached.CachedAt.IsZero() {
		t.Fatal("CachedAt is zero")
	}
	got := cached.Config.Devin.Accounts
	if len(got) != 2 {
		t.Fatalf("accounts = %v, want 2", got)
	}
	if got[0].CredentialsFile != "" || got[0].Token != "tok-from-file" {
		t.Fatalf("account a = {file:%q token:%q}, want {file:\"\" token:tok-from-file}",
			got[0].CredentialsFile, got[0].Token)
	}
	if got[1].Token != "tok-literal" || got[1].APIKey != "cog_key" {
		t.Fatalf("account b = %+v, want {token:tok-literal api_key:cog_key}", got[1])
	}
	if cached.Config.Devin.ForceHTTP1 == nil || !*cached.Config.Devin.ForceHTTP1 {
		t.Fatal("ForceHTTP1 default not materialized in cache")
	}
	// 兜底投影必须过 Apply 的整表校验：自包含形态不触任何外部文件。
	if _, err := ResolveAccounts(got, dir); err != nil {
		t.Fatalf("cached accounts fail re-validation: %v", err)
	}
}

// TestReadLastGoodMissing 缺席缓存返回错误——boot 兜底判无即照旧退出。
func TestReadLastGoodMissing(t *testing.T) {
	if _, err := ReadLastGood(t.TempDir()); err == nil {
		t.Fatal("ReadLastGood() error = nil, want missing-cache error")
	}
}

// TestReadLastGoodCorrupt 损坏缓存返回错误而不是零值配置带病服役。
func TestReadLastGoodCorrupt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lastGoodFile), []byte("{{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLastGood(dir); err == nil {
		t.Fatal("ReadLastGood() error = nil, want corrupt-cache error")
	}
}

// TestReadLastGoodNoListen 形态不可服役的缓存（无 listen）按错误返回。
func TestReadLastGoodNoListen(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, lastGoodFile),
		[]byte("cached_at: 2026-01-01T00:00:00Z\nsource_path: /x\nconfig:\n  devin:\n    model: m\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLastGood(dir); err == nil {
		t.Fatal("ReadLastGood() error = nil, want no-listen error")
	}
}
