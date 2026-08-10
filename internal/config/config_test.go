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
