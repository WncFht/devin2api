// 本文件负责加载配置、组装服务依赖并启动 HTTP 服务器。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/leookun/devin-2api/internal/adapter"
	"github.com/leookun/devin-2api/internal/adapter/devin"
	"github.com/leookun/devin-2api/internal/app"
	"github.com/leookun/devin-2api/internal/config"
	"github.com/leookun/devin-2api/internal/dashboard"
	"github.com/leookun/devin-2api/internal/debuglog"
)

func main() {
	configPath := flag.String("config", "config.yaml", "YAML 配置文件路径")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	absoluteConfigPath, err := filepath.Abs(*configPath)
	if err != nil {
		slog.Error("resolve config path failed", "error", err)
		os.Exit(1)
	}
	serviceConfig, err := config.Load(absoluteConfigPath)
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}

	providerAdapter := adapter.Adapter(adapter.Unavailable{Reason: "provider adapter is not configured"})
	if serviceConfig.Devin.Token != "" {
		configured, createErr := devin.New(devin.Config{
			BaseURL:    serviceConfig.Devin.BaseURL,
			Token:      serviceConfig.Devin.Token,
			Model:      serviceConfig.Devin.Model,
			Proxy:      serviceConfig.Devin.Proxy,
			ForceHTTP1: serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1,
			Aliases:    serviceConfig.Devin.Aliases,
		})
		if createErr != nil {
			slog.Error("create devin adapter failed", "error", createErr)
			os.Exit(1)
		}
		providerAdapter = configured
	}
	var debugManager *debuglog.Manager
	if serviceConfig.Debug.Enabled {
		debugManager = debuglog.NewManager(
			filepath.Join(filepath.Dir(absoluteConfigPath), "logs"),
			*serviceConfig.Debug.RetentionDays,
			*serviceConfig.Debug.MaxTotalMB,
		)
		defer debugManager.Close()
	}
	application := app.New(providerAdapter, serviceConfig.Server, debugManager)
	application.SetAPIKey(serviceConfig.Auth.APIKey)
	if serviceConfig.Devin.Token != "" {
		application.SetDashboard(dashboard.New(serviceConfig.Dashboard.Password, serviceConfig.Devin.BaseURL, serviceConfig.Devin.Token, serviceConfig.Devin.Proxy, serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1, application.Metrics(), debugManager))
	}
	server := application.HTTPServer()
	slog.Info("HTTP server listening", "addr", listenURL(server.Addr))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, server); err != nil {
		slog.Error("serve HTTP failed", "error", err)
		os.Exit(1)
	}
}

// listenURL 生成启动日志中的监听描述：配置为通配地址时同时给出 localhost 可访问地址。
func listenURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		return listen + " (http://localhost:" + port + ")"
	}
	return "http://" + host + ":" + port
}

func run(ctx context.Context, server interface {
	ListenAndServe() error
	Shutdown(context.Context) error
	Close() error
}) error {
	result := make(chan error, 1)
	go func() {
		result <- server.ListenAndServe()
	}()

	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		// 优雅关闭超时（可能有活跃 SSE 流），强制关闭不再报错。
		server.Close()
	}
	return nil
}
