// 本文件验证服务入口能向调用方返回服务器启动失败。
package main

import (
	"context"
	"errors"
	"testing"
)

type fakeServer struct {
	shutdown bool
}

func (server *fakeServer) ListenAndServe() error {
	return errors.New("listen stopped")
}

func (server *fakeServer) Shutdown(context.Context) error {
	server.shutdown = true
	return nil
}

func (server *fakeServer) Close() error { return nil }

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
	server := &fakeServer{}
	if err := run(context.Background(), server); err == nil {
		t.Fatal("run() error = nil, want serve error")
	}
	if server.shutdown {
		t.Fatal("server was shut down after an unexpected serve failure")
	}
}
