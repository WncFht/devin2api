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
	"runtime/debug"
	"syscall"
	"time"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/dashboard"
	"github.com/WncFht/devin2api/internal/debuglog"
)

// version 由构建期 -ldflags "-X main.version=$(git describe --tags --always --dirty)"
// 注入（见 scripts/deploy.sh）；缺省 dev 表示未注入构建，此时 resolvedVersion
// 回退到 Go 内嵌的 VCS build info，让手动 go build 的二进制也能自报 commit。
var version = "dev"

// resolvedVersion 返回对外展示的运行版本：注入值优先，其次 build info 的
// 短 commit（dirty 标记工作区未提交），都没有时才退回 "dev"。
func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision == "" {
		return version
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		revision += "-dirty"
	}
	return "dev-" + revision
}

func main() {
	configPath := flag.String("config", "config.yaml", "YAML 配置文件路径")
	showVersion := flag.Bool("version", false, "打印构建版本后退出")
	flag.Parse()
	resolved := resolvedVersion()
	if *showVersion {
		fmt.Println(resolved)
		return
	}

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
			BaseURL:       serviceConfig.Devin.BaseURL,
			Token:         serviceConfig.Devin.Token,
			Model:         serviceConfig.Devin.Model,
			Proxy:         serviceConfig.Devin.Proxy,
			ForceHTTP1:    serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1,
			Aliases:       serviceConfig.Devin.Aliases,
			ClientName:    serviceConfig.Devin.ClientName,
			ClientVersion: serviceConfig.Devin.ClientVersion,
			ClientOS:      serviceConfig.Devin.ClientOS,
			// Devin CLI 会续期改写 credentials.toml；unauthenticated 时
			// 重载同一来源链（配置值 → 环境变量 → 凭证文件）拿新凭据。
			TokenSource: func() string {
				reloaded, err := config.Load(absoluteConfigPath)
				if err != nil {
					return ""
				}
				return reloaded.Devin.Token
			},
		})
		if createErr != nil {
			slog.Error("create devin adapter failed", "error", createErr)
			os.Exit(1)
		}
		providerAdapter = configured
	}
	// 管理器总是创建：enabled 只控制新请求是否写目录，历史查询、
	// 用量回放、清理与配额采样不随开关停掉，面板也可运行时热切换。
	logRoot := filepath.Join(filepath.Dir(absoluteConfigPath), "logs")
	debugManager := debuglog.NewManager(logRoot, debuglog.RetentionPolicy{
		Days:          *serviceConfig.Debug.RetentionDays,
		MaxTotalMB:    *serviceConfig.Debug.MaxTotalMB,
		PayloadHours:  *serviceConfig.Debug.PayloadHours,
		KeepErrorDirs: *serviceConfig.Debug.KeepErrorDirs,
	})
	debugManager.SetEnabled(serviceConfig.Debug.Enabled)
	defer debugManager.Close()
	application := app.New(providerAdapter, serviceConfig.Server, debugManager)
	application.SetAPIKey(serviceConfig.Auth.APIKey)
	application.SetVersion(resolved)
	if serviceConfig.Devin.Token != "" {
		panel := dashboard.New(serviceConfig.Dashboard.Password, serviceConfig.Devin.BaseURL, serviceConfig.Devin.Token, serviceConfig.Devin.Proxy, serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1, application.Metrics(), debugManager)
		panel.SetVersion(resolved)
		panel.StartQuotaSampler(time.Duration(*serviceConfig.Debug.QuotaIntervalMinutes) * time.Minute)
		application.SetDashboard(panel)
	}
	server := application.HTTPServer()
	slog.Info("HTTP server listening", "addr", listenURL(server.Addr), "version", resolved)

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
