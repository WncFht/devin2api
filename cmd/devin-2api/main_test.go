// 本文件验证服务入口能向调用方返回服务器启动失败。
package main

import (
	"context"
	"net"
	"net/http"
	"testing"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/config"
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
