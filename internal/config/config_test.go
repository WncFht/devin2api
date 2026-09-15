// 本文件验证配置未知字段拒绝和时间字段解析行为。
package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadRejectsUnknownFields 验证未知配置字段会被严格拒绝。
func TestLoadRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: ':8080'\n  typo: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("Load() error = nil, want unknown field error")
	}
}

// TestLoadParsesListenAddress 验证服务监听地址来自 YAML 配置。
func TestLoadParsesListenAddress(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: ':9090'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Server.Listen != ":9090" {
		t.Fatalf("Listen = %q, want :9090", config.Server.Listen)
	}
}

// TestLoadDisablesDebugLoggingByDefault 的测试动机是保证生产配置未显式开启时不会写入请求内容。
func TestLoadDisablesDebugLoggingByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: ':9090'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Debug.Enabled {
		t.Fatal("Debug.Enabled = true, want disabled by default")
	}
}

// TestLoadParsesAuthAPIKey 验证可选的 API Key 可从配置中读取。
func TestLoadParsesAuthAPIKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: ':9090'\nauth:\n  api_key: 'my-secret-key'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if config.Auth.APIKey != "my-secret-key" {
		t.Fatalf("Auth.APIKey = %q, want my-secret-key", config.Auth.APIKey)
	}
}

// TestLoadEnablesDebugLoggingExplicitly 的测试动机是保留排查协议问题时主动开启日志的能力。
func TestLoadEnablesDebugLoggingExplicitly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("server:\n  listen: ':9090'\ndebug:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Debug.Enabled {
		t.Fatal("Debug.Enabled = false, want explicitly enabled")
	}
}

// TestNormalizeAliases 钉住别名归一化契约：trim、链式展开、合法 "*" 兜底键；
// 空键/空目标/"*" 目标/大小写重复/环都在加载期报错而不是运行时静默漂移。
func TestNormalizeAliases(t *testing.T) {
	valid := map[string]string{
		" swe-2 ": "swe-2-max",
		"a":       "b",
		"b":       "real-uid",
		"*":       "glm-5-2",
	}
	got, err := normalizeAliases(valid)
	if err != nil {
		t.Fatalf("normalizeAliases() error = %v", err)
	}
	want := map[string]string{"swe-2": "swe-2-max", "a": "real-uid", "b": "real-uid", "*": "glm-5-2"}
	if len(got) != len(want) {
		t.Fatalf("normalized = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("normalized[%q] = %q, want %q (full map %v)", k, got[k], v, got)
		}
	}

	invalid := []map[string]string{
		{"  ": "x"},                    // 空键
		{"a": "  "},                    // 空目标
		{"a": "*"},                     // "*" 不能作目标
		{"A": "x", "a": "y"},           // 大小写重复
		{"a": "b", "b": "a"},           // 环
		{"a": "a"},                     // 自环
		{"a": "b", "b": "c", "c": "b"}, // 中间环
	}
	for i, m := range invalid {
		if _, err := normalizeAliases(m); err == nil {
			t.Fatalf("case %d: normalizeAliases(%v) error = nil, want rejection", i, m)
		}
	}
}

// TestResolveConfigPath 验证配置路径解析优先级：flag > env > ./config.yaml
// > 平台默认。env/CWD 用 t.Setenv+t.Chdir 隔离，平台默认路径只做非空与
// 后缀断言——具体值随 GOOS 变。
func TestResolveConfigPath(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "explicit.yaml")
	if got, err := ResolveConfigPath(explicit); err != nil || got != explicit {
		t.Fatalf("flag 优先: got %q err %v", got, err)
	}

	t.Setenv("DEVIN2API_CONFIG", "/env/cfg.yaml")
	if got, _ := ResolveConfigPath(""); got != "/env/cfg.yaml" {
		t.Fatalf("env 优先: got %q", got)
	}
	if got, _ := ResolveConfigPath(explicit); got != explicit {
		t.Fatalf("flag 应压过 env: got %q", got)
	}
	t.Setenv("DEVIN2API_CONFIG", "   ")
	t.Chdir(t.TempDir())
	if got, _ := ResolveConfigPath(""); filepath.Base(got) != "config.yaml" || filepath.Dir(got) == "." {
		t.Fatalf("无 ./config.yaml 时应回落平台默认: got %q", got)
	}
	if err := os.WriteFile("config.yaml", []byte("server:\n  listen: ':1'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := ResolveConfigPath(""); got != "config.yaml" {
		t.Fatalf("CWD 有 config.yaml 应选它: got %q", got)
	}
}

// TestResolveStateDir 验证状态目录解析：flag > env > 平台默认（非空）。
func TestResolveStateDir(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "st")
	if got, err := ResolveStateDir(explicit); err != nil || got != explicit {
		t.Fatalf("flag 优先: got %q err %v", got, err)
	}
	t.Setenv("DEVIN2API_STATE_DIR", "/env/state")
	if got, _ := ResolveStateDir(""); got != "/env/state" {
		t.Fatalf("env 优先: got %q", got)
	}
	t.Setenv("DEVIN2API_STATE_DIR", "")
	if got, err := ResolveStateDir(""); err != nil || filepath.Base(got) != "devin-2api" {
		t.Fatalf("平台默认: got %q err %v", got, err)
	}
}
