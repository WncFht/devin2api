// 本文件负责加载配置、组装服务依赖并启动 HTTP 服务器。
package main

import (
	"context"
	_ "embed"
	"encoding/json"
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
	"strings"
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
// 逐级回退（见下），让任何渠道构建的二进制都能自报版本。
var version = "dev"

// embeddedVersion 是最近一次 release 的 tag，由 release.sh 在打 tag 前写入
// VERSION 文件并提交；覆盖无 .git 的源码 tarball 构建场景。
//
//go:embed VERSION
var embeddedVersion string

// resolvedVersion 返回对外展示的运行版本，优先级：ldflags 注入 >
// `go install @vX.Y.Z` 的 module version > VCS 短 commit（dirty 标记工作区
// 未提交）> 内嵌 VERSION 文件 > "dev"。
func resolvedVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return strings.TrimSpace(embeddedVersion)
	}
	// go install module@version 构建：Main.Version 是模块版本（如 v0.6.0），
	// 源码树内构建则是 "(devel)"。
	if info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
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
	if revision != "" {
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if modified == "true" {
			revision += "-dirty"
		}
		return "dev-" + revision
	}
	if v := strings.TrimSpace(embeddedVersion); v != "" {
		return v
	}
	return version
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

	// 尽早绑定监听端口：其后的适配器/日志管理器/指标回放都有 IO 耗时，
	// 先 listen 让内核把启动期连接排入 backlog（调用方 connect 成功但等待），
	// 否则 deploy 换进程期间整段是 connection refused。
	listener, err := listenConfigured(serviceConfig.Server.Listen)
	if err != nil {
		reportListenFailure(serviceConfig.Server.Listen, err)
	}
	defer func() { _ = listener.Close() }()

	providerAdapter := adapter.Adapter(adapter.Unavailable{Reason: "provider adapter is not configured"})
	var tokenFunc func() string
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
			MaxRPM:        serviceConfig.Devin.MaxRPM,
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
		// 面板与 adapter 共享同一份凭据来源：adapter 的 unauthenticated
		// 自愈更新 token 后，面板的上游调用自动跟随新值。
		tokenFunc = configured.TokenFunc()
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
	// 用 index.jsonl 回放预热 60 分钟趋势桶：重启后实时流量/健康时间线不从零
	// 开始，RPM 峰值口径同样恢复。完成时刻按 started_at+duration_ms 归桶，
	// 与 Finish 实时路径一致；管线前 Reject 不进索引，这部分计数不回放。
	// 异步回放：回放数千条会拖慢 listen 之后的首次应答，SeedTrend 有锁。
	// 50000 只是「尽可能多」的软上限——实际深度受 ListRequests 的
	// indexTailBytes（4MB 尾部）约束，正常流量下也远超 60 分钟窗口所需。
	go func() {
		for _, e := range debugManager.ListRequests(50000, debuglog.RequestFilter{}).Entries {
			started, err := time.Parse(time.RFC3339Nano, e.StartedAt)
			if err != nil {
				continue
			}
			application.Metrics().SeedTrend(started.Add(time.Duration(e.DurationMS)*time.Millisecond),
				e.StatusCode >= 400 || (e.Result != "" && e.Result != "completed"))
		}
	}()
	application.SetAPIKey(serviceConfig.Auth.APIKey)
	application.SetVersion(resolved)
	if serviceConfig.Devin.Token != "" {
		panel := dashboard.New(serviceConfig.Dashboard.Password, serviceConfig.Devin.BaseURL, tokenFunc, serviceConfig.Devin.Proxy, serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1, application.Metrics(), debugManager)
		panel.SetVersion(resolved)
		panel.StartQuotaSampler(time.Duration(*serviceConfig.Debug.QuotaIntervalMinutes) * time.Minute)
		application.SetDashboard(panel)
	}
	server := application.HTTPServer()
	slog.Info("HTTP server listening", "addr", listenURL(server.Addr), "version", resolved)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, application, server, listener); err != nil {
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

// drainTimeout 是优雅退出排空在途请求的最长等待：plist ExitTimeOut=60，
// 留 ~10s 给 Close 与进程退出。注意这里不用 http.Server.Shutdown——
// 它先关 listener 再排空，排空期所有新连接都被内核 refused；改为
// listener 保持开启、/v1/* 由应用层快速 503，排空结束才关 listener。
const drainTimeout = 50 * time.Second

func run(ctx context.Context, application *app.App, server *http.Server, listener net.Listener) error {
	result := make(chan error, 1)
	go func() {
		result <- server.Serve(listener)
	}()

	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, context.Canceled) {
			return nil
		}
		return fmt.Errorf("serve HTTP: %w", err)
	case <-ctx.Done():
	}

	slog.Info("shutdown: draining in-flight requests", "timeout", drainTimeout)
	application.BeginDrain()
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := application.WaitDrain(drainCtx); err != nil {
		slog.Warn("shutdown: drain timed out, closing remaining connections", "error", err)
	}
	return server.Close()
}

// listenConfigured 绑定配置的监听地址。KeepAlive 3 分钟与
// http.Server.ListenAndServe 内部 tcpKeepAliveListener 的行为一致。
func listenConfigured(listen string) (net.Listener, error) {
	return (&net.ListenConfig{KeepAlive: 3 * time.Minute}).Listen(context.Background(), "tcp", listen)
}

// reportListenFailure 处理绑定失败并退出：EADDRINUSE 时探活占用者的
// /healthz，把「谁在占端口、跑哪版、是否正在排空」写进日志——
// launchd KeepAlive 每 5s 拉起一次的 bind 冲突循环里，这行日志是
// 唯一能区分「旧实例在排空」「孤儿/手动实例占坑」「非本服务占用」的信号。
func reportListenFailure(listen string, err error) {
	if errors.Is(err, syscall.EADDRINUSE) {
		slog.Error("port already in use", "addr", listen, "holder", probeExistingInstance(listen))
	} else {
		slog.Error("listen failed", "addr", listen, "error", err)
	}
	os.Exit(1)
}

// probeExistingInstance 查询占用监听端口的进程是否为本服务实例。
func probeExistingInstance(listen string) string {
	_, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "unknown"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return "unresponsive"
	}
	defer func() { _ = resp.Body.Close() }()
	var health struct {
		Version string `json:"version"`
		Uptime  int64  `json:"uptime_seconds"`
		Drain   bool   `json:"draining"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || health.Version == "" {
		return "not devin-2api"
	}
	return fmt.Sprintf("devin-2api version=%s uptime=%ds draining=%v", health.Version, health.Uptime, health.Drain)
}
