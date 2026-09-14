// 本文件验证服务入口能向调用方返回服务器启动失败。
package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/dashboard"
	"github.com/WncFht/devin2api/internal/debuglog"
)

// TestListenURL verifies listen address descriptions used in the startup log.
func TestListenURL(t *testing.T) {
	cases := map[string]string{
		":8080":          ":8080 (http://localhost:8080)",
		"0.0.0.0:8080":   "0.0.0.0:8080 (http://localhost:8080)",
		"127.0.0.1:9090": "http://127.0.0.1:9090",
		"[::]:8080":      "[::]:8080 (http://localhost:8080)",
		"invalid":        "invalid",
	}
	for listen, want := range cases {
		if got := listenURL(listen); got != want {
			t.Errorf("listenURL(%q) = %q, want %q", listen, got, want)
		}
	}
}

// TestRunReturnsServeError verifies unexpected server failures are returned to main.
func TestRunReturnsServeError(t *testing.T) {
	// 已关闭的 listener 让 Serve 立即返回错误——无需伪造 server。
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	application := app.New(adapter.Unavailable{Reason: "test"}, config.ServerConfig{},
		debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{}))
	if err := run(context.Background(), application, &http.Server{}, listener); err == nil {
		t.Fatal("run() error = nil, want serve error")
	}
}

// TestRedactConfigSecretsProxyUserinfo verifies proxy URL userinfo is stripped
// from the config introspection view while the host stays identifiable.
func TestRedactConfigSecretsProxyUserinfo(t *testing.T) {
	fields := map[string]any{
		"devin": map[string]any{
			"token": "topsecret",
			"proxy": "http://alice:hunter2@proxy.local:8080",
		},
	}
	redactConfigSecrets(fields)
	devinSection := fields["devin"].(map[string]any)
	proxy := devinSection["proxy"].(string)
	if strings.Contains(proxy, "alice") || strings.Contains(proxy, "hunter2") {
		t.Fatalf("proxy userinfo leaked: %q", proxy)
	}
	if !strings.Contains(proxy, "proxy.local:8080") {
		t.Fatalf("proxy host should be preserved: %q", proxy)
	}
	if token := devinSection["token"].(string); !strings.HasPrefix(token, "sha256:") {
		t.Fatalf("token not redacted: %q", token)
	}
}

// TestReloadRuntimeConfigRejectsEmptyUpstream verifies a live adapter refuses a
// reload that drops devin.token/model/base_url — committing empty values would
// fail every request and starve the token self-heal chain.
func TestReloadRuntimeConfigRejectsEmptyUpstream(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	valid := "server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  token: 't'\n  model: 'm'\n"
	if err := os.WriteFile(configPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	prev, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeConfigPtr.Store(&runtimeConfigState{cfg: prev, loadedAt: time.Now()})

	manager := debuglog.NewManager(dir, debuglog.RetentionPolicy{})
	defer manager.Close()
	devinAdapter, err := devin.New(devin.Config{BaseURL: "https://example.com", Token: "t", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	application := app.New(devinAdapter, config.ServerConfig{}, manager)
	panel := dashboard.New("pw", "https://example.com", func() string { return "t" }, "", false, nil, manager)

	if err := os.WriteFile(configPath, []byte("server:\n  listen: ':1'\ndevin:\n  base_url: 'https://example.com'\n  token: 't'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadRuntimeConfig(configPath, devinAdapter, application, panel, manager); err == nil {
		t.Fatal("reloadRuntimeConfig() error = nil, want non-empty validation error")
	}

	if err := os.WriteFile(configPath, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reloadRuntimeConfig(configPath, devinAdapter, application, panel, manager); err != nil {
		t.Fatalf("reloadRuntimeConfig() error = %v, want nil", err)
	}
}
