// 本文件负责加载配置、组装服务依赖并启动 HTTP 服务器。
package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/dashboard"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
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

// runtimeConfigState 是最近一次成功加载的配置快照：配置自省端点拿它
// 回答「文件在最后一次加载后是否被改过」（file_mtime vs 当前 mtime）。
type runtimeConfigState struct {
	cfg       config.Config
	loadedAt  time.Time
	fileMtime time.Time
}

var runtimeConfigPtr atomic.Pointer[runtimeConfigState]
var lastReloadPtr atomic.Pointer[dashboard.ConfigReloadReport]

// reloadMu 串行化热重载：ApplyConfig→SetAPIKey→…→runtimeConfigPtr.Store
// 是一串多步提交，并发 reload 交错会让配置快照与生效值分叉。
var reloadMu sync.Mutex

func main() {
	configPath := flag.String("config", "", "YAML 配置文件路径；缺省按 $DEVIN2API_CONFIG → ./config.yaml → 平台默认目录解析")
	stateDir := flag.String("state-dir", "", "日志与状态文件根目录；缺省按 $DEVIN2API_STATE_DIR → 平台默认目录解析")
	showVersion := flag.Bool("version", false, "打印构建版本后退出")
	flag.Parse()
	resolved := resolvedVersion()
	if *showVersion {
		fmt.Println(resolved)
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	resolvedConfigPath, err := config.ResolveConfigPath(*configPath)
	if err != nil {
		slog.Error("resolve config path failed", "error", err)
		os.Exit(1)
	}
	absoluteConfigPath, err := filepath.Abs(resolvedConfigPath)
	if err != nil {
		slog.Error("resolve config path failed", "error", err)
		os.Exit(1)
	}
	resolvedStateDir, err := config.ResolveStateDir(*stateDir)
	if err != nil {
		slog.Error("resolve state dir failed", "error", err)
		os.Exit(1)
	}
	absoluteStateDir, err := filepath.Abs(resolvedStateDir)
	if err != nil {
		slog.Error("resolve state dir failed", "error", err)
		os.Exit(1)
	}
	// logRoot 是所有运行期产物（请求 debug 目录、index.jsonl、quota.jsonl、
	// gate-state.json、stdout/stderr.log）的统一归属，独立于配置文件位置——
	// 配置是用户输入，状态目录是程序输出，按平台规范分家。
	logRoot := filepath.Join(absoluteStateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		slog.Error("create state dir failed", "dir", logRoot, "error", err)
		os.Exit(1)
	}
	serviceConfig, err := config.Load(absoluteConfigPath)
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}
	slog.Info("paths resolved", "config", absoluteConfigPath, "state_dir", absoluteStateDir)
	runtimeConfigPtr.Store(&runtimeConfigState{
		cfg: serviceConfig, loadedAt: time.Now(), fileMtime: configFileMtime(absoluteConfigPath),
	})

	// 尽早绑定监听端口：其后的适配器/日志管理器/指标回放都有 IO 耗时，
	// 先 listen 让内核把启动期连接排入 backlog（调用方 connect 成功但等待），
	// 否则 deploy 换进程期间整段是 connection refused。
	listener, err := listenConfigured(serviceConfig.Server.Listen)
	if err != nil {
		reportListenFailure(serviceConfig.Server.Listen, logRoot, err)
	}
	defer func() { _ = listener.Close() }()
	// 绑定成功才到这里：上一轮 EADDRINUSE 冲突循环（KeepAlive 反复拉起
	// vs 旧实例排空）若留过标记，补一条恢复告警把次数与占用者并进日志。
	warnIfBindContentionRecovered(logRoot)

	// pprof 侦听是可选的第二端口：空值不启用（默认）。监听失败不致命——
	// 剖析是诊断辅助，不该让主服务起不来；错误日志已说明原因。
	// applyPprofListen 与配置 reload 共用同一换绑路径，退出时置空关闭。
	applyPprofListen(serviceConfig.Debug.PprofListen)
	defer func() { applyPprofListen("") }()

	// token 允许为空启动：凭据是运行时字段——/panel/api/config/reload
	// 热应用与 unauthenticated 自愈链的 TokenSource 重读都能补进。
	// 空 token 起不来的话「先起服务后配凭据」没有任何热补入口。
	// devinAdapter 保留具体类型引用：配置热重载（ApplyConfig）、闸门状态
	// （GateStats）与别名校验（Aliases）都挂在它上面。
	devinAdapter, err := devin.New(devinConfigFrom(serviceConfig, absoluteConfigPath, logRoot))
	if err != nil {
		slog.Error("create devin adapter failed", "error", err)
		os.Exit(1)
	}
	defer devinAdapter.Close()
	// 面板与 adapter 共享同一份凭据来源：adapter 的 unauthenticated
	// 自愈更新 token 后，面板的上游调用自动跟随新值。
	tokenFunc := devinAdapter.TokenFunc()
	// 管理器总是创建：enabled 只控制新请求是否写目录，历史查询、
	// 用量回放、清理与配额采样不随开关停掉，面板也可运行时热切换。
	debugManager := debuglog.NewManager(logRoot, debuglog.RetentionPolicy{
		Days:          *serviceConfig.Debug.RetentionDays,
		MaxTotalMB:    *serviceConfig.Debug.MaxTotalMB,
		PayloadHours:  *serviceConfig.Debug.PayloadHours,
		KeepErrorDirs: *serviceConfig.Debug.KeepErrorDirs,
	})
	debugManager.SetEnabled(serviceConfig.Debug.Enabled)
	defer debugManager.Close()
	// 运行时设置键仓：panel-settings.json 落状态目录根；覆盖项对
	// debug 开关/保留策略恒赢 config.yaml（先建仓再重放，让面板改的
	// 值在启动时就生效；默认值在重放前采样，即 config 派生态）。
	settingsStore, err := ccpanel.NewPanelSettings(absoluteStateDir, debugManager)
	if err != nil {
		slog.Error("load panel settings failed", "error", err)
		os.Exit(1)
	}
	if err := settingsStore.ApplyAll(); err != nil {
		slog.Warn("panel settings replay failed", "error", err)
	}
	application := app.New(devinAdapter, serviceConfig.Server, debugManager)
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
	// 面板与 token 解耦：空 token 时 stats/rejects/日志查询仍是排障入口，
	// 上游相关调用靠 tokenFunc 现取，凭据补进后自动恢复。
	panel, err := dashboard.New(serviceConfig.Dashboard.Password, serviceConfig.Devin.BaseURL, tokenFunc, serviceConfig.Devin.Proxy, serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1, application.Metrics(), debugManager)
	if err != nil {
		slog.Error("create dashboard failed", "error", err)
		os.Exit(1)
	}
	panel.SetVersion(resolved)
	panel.SetGateStats(devinAdapter.GateStats)
	panel.SetAliasesFunc(devinAdapter.Aliases)
	panel.SetConfigOps(dashboard.ConfigOps{
		Reload: func() (*dashboard.ConfigReloadReport, error) {
			return reloadRuntimeConfig(absoluteConfigPath, logRoot, devinAdapter, application, panel, debugManager, settingsStore)
		},
		Current: func() map[string]any {
			return runtimeConfigView(absoluteConfigPath)
		},
	})
	panel.SetQuotaInterval(time.Duration(*serviceConfig.Debug.QuotaIntervalMinutes) * time.Minute)
	application.SetDashboard(panel)
	// 移植面板（ccLoad 契约）与旧面板并存：同一密码门槛，/web、/admin、
	// /dashboard、/public、/login、/logout 挂在根路径。
	ccPanel := ccpanel.New(panel, debugManager, application.Metrics())
	ccPanel.SetVersion(resolved)
	ccPanel.SetMaxConcurrencyFunc(application.MaxConcurrency)
	ccPanel.SetAliasesFunc(devinAdapter.Aliases)
	// 下游令牌仓：auth_tokens.json 落在状态目录根（与 logs/ 平级）。
	// /v1 准入与移植面板的令牌管理共用同一仓；costFn 用目录价把一次
	// 请求的 token 用量折成美元供费用限额窗口记账（cache_write 按
	// input 价，与 ccpanel cellCost 同口径）。
	tokenStore, err := authtoken.New(absoluteStateDir)
	if err != nil {
		slog.Error("load auth tokens failed", "error", err)
		os.Exit(1)
	}
	application.SetAuthTokens(tokenStore, func(model string, input, output, cacheRead, cacheWrite int64) float64 {
		p, ok := panel.CatalogPrices(context.Background())[model]
		if !ok {
			return 0
		}
		return (float64(input+cacheWrite)*p.Input + float64(cacheRead)*p.Cached + float64(output)*p.Output) / 1e6
	})
	ccPanel.SetTokenStore(tokenStore)
	// 模型注册表：models.json 落状态目录根；/v1 准入（停用/重定向）与
	// 移植面板的注册表页共用同一仓。
	modelStore, err := modelreg.New(absoluteStateDir)
	if err != nil {
		slog.Error("load model registry failed", "error", err)
		os.Exit(1)
	}
	application.SetModelRegistry(modelStore)
	ccPanel.SetModelRegistry(modelStore)
	ccPanel.SetSettingsStore(settingsStore)
	application.SetCCPanel(ccPanel)
	server := application.HTTPServer()
	// 探活走进程内根路由：与外部请求共用鉴权/准入/重定向/上游管线；
	// 主密钥读运行时值（热重载后跟随新 key）。
	ccPanel.SetProbeHandler(server.Handler)
	ccPanel.SetMasterKeyFunc(application.APIKey)
	slog.Info("HTTP server listening", "addr", listenURL(server.Addr), "version", resolved, "reuseport", reusePortEnabled())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// ctx 取消后立刻恢复信号默认动作：否则排空期第二发 SIGINT/SIGTERM
	// 被 NotifyContext 静默吸收，运维失去「再发一次强杀」的逃生口。
	go func() {
		<-ctx.Done()
		stop()
	}()
	defer stop()
	// SIGHUP（终端断开）不参与排空语义：前台裸跑时断连不应强杀在途流。
	signal.Ignore(syscall.SIGHUP)
	if err := run(ctx, application, server, listener); err != nil {
		slog.Error("serve HTTP failed", "error", err)
		os.Exit(1)
	}
}

// devinConfigFrom 把启动配置映射为 Devin adapter 配置；启动与配置
// 热重载共用同一映射，保证 ApplyConfig 看到的字段口径与 New 一致。
// logRoot 决定 gate-state.json 的落点（沿用 logs/ 内的既有位置）。
func devinConfigFrom(serviceConfig config.Config, configPath, logRoot string) devin.Config {
	return devin.Config{
		BaseURL:       serviceConfig.Devin.BaseURL,
		Token:         serviceConfig.Devin.Token,
		Model:         serviceConfig.Devin.Model,
		Proxy:         serviceConfig.Devin.Proxy,
		ForceHTTP1:    serviceConfig.Devin.ForceHTTP1 != nil && *serviceConfig.Devin.ForceHTTP1,
		Aliases:       serviceConfig.Devin.Aliases,
		ClientName:    serviceConfig.Devin.ClientName,
		ClientVersion: serviceConfig.Devin.ClientVersion,
		ClientOS:      serviceConfig.Devin.ClientOS,
		Gate: devin.GateConfig{
			MaxRPM:       serviceConfig.Devin.MaxRPM,
			MaxHold:      time.Duration(serviceConfig.Devin.GateMaxHoldSeconds) * time.Second,
			DripInterval: time.Duration(serviceConfig.Devin.GateDripIntervalSeconds) * time.Second,
			DefaultLatch: time.Duration(serviceConfig.Devin.GateDefaultLatchSeconds) * time.Second,
			WindowOffset: time.Duration(serviceConfig.Devin.GateWindowOffsetSeconds) * time.Second,
			WindowGuard:  time.Duration(serviceConfig.Devin.GateWindowGuardSeconds) * time.Second,
		},
		GateStatePath: filepath.Join(logRoot, "gate-state.json"),
		// Devin CLI 会续期改写 credentials.toml；unauthenticated 时
		// 重载同一来源链（配置值 → 环境变量 → 凭证文件）拿新凭据。
		TokenSource: func() string {
			reloaded, err := config.Load(configPath)
			if err != nil {
				return ""
			}
			return reloaded.Devin.Token
		},
	}
}

// reloadRuntimeConfig 重读配置文件并把可安全换值的字段热应用；校验失败
// 直接返回错误、旧配置继续服役（validate-then-commit）。只报告值发生
// 变化的字段——unchanged 的字段不在 applied/requires_restart 里出现。
// 仅剩监听参数 server.listen 进 requires_restart（Serve 无法换绑端口）；
// transport 固化的端点三件套走调用束原子换指针热生效。
func reloadRuntimeConfig(configPath, logRoot string, devinAdapter *devin.Adapter, application *app.App, panel *dashboard.Handler, debugManager *debuglog.Manager, settings *ccpanel.PanelSettings) (*dashboard.ConfigReloadReport, error) {
	reloadMu.Lock()
	defer reloadMu.Unlock()
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	// 上游必填项收敛到 model/base_url：reload 提交空值会让全部请求
	// 失败——整单拒绝（422），旧配置继续服役。token 刻意不在必填集：
	// 补凭据的通道正是本端点与 unauthenticated 自愈链（读同一文件），
	// 空 token 是合法的待配状态而非配置事故。
	if strings.TrimSpace(cfg.Devin.Model) == "" || strings.TrimSpace(cfg.Devin.BaseURL) == "" {
		return nil, errors.New("devin.model and devin.base_url must be non-empty")
	}
	report := &dashboard.ConfigReloadReport{At: time.Now().Format(time.RFC3339), Applied: []string{}}
	applied, err := devinAdapter.ApplyConfig(devinConfigFrom(cfg, configPath, logRoot))
	if err != nil {
		return nil, err
	}
	report.Applied = append(report.Applied, applied...)
	// prev 必然非空：runtimeConfigPtr 在 panel 装配前已 Store，
	// 而本函数只能经 panel 端点触达。
	pcfg := runtimeConfigPtr.Load().cfg
	if pcfg.Devin.BaseURL != cfg.Devin.BaseURL || pcfg.Devin.Proxy != cfg.Devin.Proxy ||
		*pcfg.Devin.ForceHTTP1 != *cfg.Devin.ForceHTTP1 {
		// adapter 侧调用束已在 ApplyConfig 内换好（同参数构建成功是前提）；
		// 面板自身的上游调用束跟随同一端点——ccpanel 的展示地址经
		// panel.BaseURL 透出，无需单独同步。
		if err := panel.SetUpstream(cfg.Devin.BaseURL, cfg.Devin.Proxy, *cfg.Devin.ForceHTTP1); err != nil {
			return nil, err
		}
	}
	if pcfg.Auth.APIKey != cfg.Auth.APIKey {
		application.SetAPIKey(cfg.Auth.APIKey)
		report.Applied = append(report.Applied, "auth.api_key")
	}
	if pcfg.Dashboard.Password != cfg.Dashboard.Password {
		panel.SetPassword(cfg.Dashboard.Password)
		report.Applied = append(report.Applied, "dashboard.password")
	}
	if pcfg.Debug.Enabled != cfg.Debug.Enabled {
		debugManager.SetEnabled(cfg.Debug.Enabled)
		report.Applied = append(report.Applied, "debug.enabled")
	}
	newPolicy := debuglog.RetentionPolicy{
		Days:          *cfg.Debug.RetentionDays,
		MaxTotalMB:    *cfg.Debug.MaxTotalMB,
		PayloadHours:  *cfg.Debug.PayloadHours,
		KeepErrorDirs: *cfg.Debug.KeepErrorDirs,
	}
	if debugManager.Policy() != newPolicy {
		debugManager.SetPolicy(newPolicy)
		report.Applied = append(report.Applied, "debug.retention")
	}
	// 面板覆盖项恒赢 config.yaml：上面的 SetEnabled/SetPolicy 刚按
	// 文件值重置过，panel-settings.json 里登记的键要重放压回去。
	if err := settings.ApplyAll(); err != nil {
		slog.Warn("panel settings replay failed", "error", err)
	}
	if *pcfg.Debug.QuotaIntervalMinutes != *cfg.Debug.QuotaIntervalMinutes {
		panel.SetQuotaInterval(time.Duration(*cfg.Debug.QuotaIntervalMinutes) * time.Minute)
		report.Applied = append(report.Applied, "debug.quota_interval_minutes")
	}
	if pcfg.Debug.PprofListen != cfg.Debug.PprofListen {
		applyPprofListen(cfg.Debug.PprofListen)
		report.Applied = append(report.Applied, "debug.pprof_listen")
	}
	if pcfg.Server.Listen != cfg.Server.Listen {
		report.RequiresRestart = append(report.RequiresRestart, "server.listen")
	}
	if pcfg.Server.MaxConcurrency != cfg.Server.MaxConcurrency {
		application.SetMaxConcurrency(cfg.Server.MaxConcurrency)
		report.Applied = append(report.Applied, "server.max_concurrency")
	}
	runtimeConfigPtr.Store(&runtimeConfigState{cfg: cfg, loadedAt: time.Now(), fileMtime: configFileMtime(configPath)})
	lastReloadPtr.Store(report)
	slog.Info("config reloaded", "applied", report.Applied, "requires_restart", report.RequiresRestart)
	return report, nil
}

// runtimeConfigView 返回配置自省视图：最近成功加载的配置（脱敏）、
// 文件 mtime、以及文件在加载后是否被改动（stale）。配置经 yaml 往返
// 成 map，键名与 config.yaml 一致。
func runtimeConfigView(configPath string) map[string]any {
	view := map[string]any{"path": configPath}
	cur := runtimeConfigPtr.Load()
	if cur == nil {
		view["error"] = "config not loaded"
		return view
	}
	fields := map[string]any{}
	if raw, err := yaml.Marshal(cur.cfg); err == nil {
		_ = yaml.Unmarshal(raw, &fields)
	}
	redactConfigSecrets(fields)
	view["config"] = fields
	view["loaded_at"] = cur.loadedAt.Format(time.RFC3339)
	view["file_mtime"] = cur.fileMtime.Format(time.RFC3339)
	view["stale"] = configFileMtime(configPath).After(cur.fileMtime)
	if report := lastReloadPtr.Load(); report != nil {
		view["last_reload"] = report
	}
	return view
}

// redactConfigSecrets 把配置视图里的凭据值替换为 sha256 前缀——
// 既能和日志里的 key_hash 对照确认「是不是我以为的那把 key」，又不回明文。
// devin.proxy 允许 http://user:pass@host 形式，userinfo 同样是凭据：
// 清掉整段 User 保留 host，排障仍能辨认代理指向。
func redactConfigSecrets(fields map[string]any) {
	for _, path := range [][2]string{{"devin", "token"}, {"auth", "api_key"}, {"dashboard", "password"}} {
		section, ok := fields[path[0]].(map[string]any)
		if !ok {
			continue
		}
		raw, ok := section[path[1]].(string)
		if !ok || raw == "" {
			continue
		}
		sum := sha256.Sum256([]byte(raw))
		section[path[1]] = fmt.Sprintf("sha256:%x", sum[:6])
	}
	if devin, ok := fields["devin"].(map[string]any); ok {
		if raw, ok := devin["proxy"].(string); ok && raw != "" {
			if parsed, err := url.Parse(raw); err == nil && parsed.User != nil {
				parsed.User = nil
				devin["proxy"] = parsed.String()
			}
		}
	}
}

// configFileMtime 返回配置文件的最后修改时刻；stat 失败回零值。
func configFileMtime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
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

// drainTimeout 是优雅退出排空在途请求的最长等待：plist ExitTimeOut=660、
// systemd TimeoutStopSec=660，留 ~60s 给 Close 与进程退出。重叠交接部署下
// 排空不再阻塞新请求，上限按在途时长分布取（实测 p99≈163s，但 CC 长会话
// 尾部分布远超该值——300s 窗口内仍有真实请求被硬切）。注意这里不用
// http.Server.Shutdown——它先关 listener 再排空，排空期所有新连接都被内核
// refused；非交接场景改为 listener 保持开启、/v1/* 由应用层快速 503。
const drainTimeout = 600 * time.Second

// reusePortEnabled 报告是否启用 SO_REUSEPORT 重叠交接：开启后多个进程可
// 绑定同一监听地址，deploy 先起桥接进程入队再排空旧实例，做到零停机重启。
// 经环境变量（非 config）控制：只有托管实例（plist/unit 注入）与 deploy
// 拉起的交接进程拿到它——裸跑 ./devin-2api 不带 env 仍会 EADDRINUSE，
// 单实例约定的端口冲突保护不变。平台不支持（Windows）时恒 false。
func reusePortEnabled() bool {
	v := os.Getenv("DEVIN2API_REUSEPORT")
	return reusePortSupported && (v == "1" || strings.EqualFold(v, "true"))
}

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
	// 关闭 keep-alive：此后的响应都带 Connection: close，客户端收完
	// 当前响应后新开连接。reuseport 交接时新连接落到接替实例——不开
	// 的话，已 accept 的 keep-alive 连接会继续打回正在排空的旧实例，
	// 每个复用请求都吃一次 draining 503。在途连接不受影响照常跑完。
	server.SetKeepAlivesEnabled(false)
	// 交接语义的关键：reuseport 组内 macOS 按绑定先后派发新连接、Linux 按
	// 哈希分流——无论哪种，旧实例都必须立刻关闭 listener，新连接才会全部
	// 落到接替者（deploy 预置的交接进程）身上；开着只会白收连接再发 503。
	// 关闭只切断新 accept，已 accept 的在途连接继续排空。未开 reuseport
	// 时维持旧行为：listener 保持开启，新请求拿 503+Retry-After 而非拒绝。
	if reusePortEnabled() {
		_ = listener.Close()
		slog.Info("shutdown: listener released for handoff")
	}
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := application.WaitDrain(drainCtx); err != nil {
		slog.Warn("shutdown: drain timed out, closing remaining connections", "error", err)
	}
	// reuseport 路径上面已直接关过底层 listener：Serve 的 defer 解除
	// listener 追踪与这里的 Close 存在竞态，落后时对同一 fd 做真实
	// 二次 close → ErrClosed 冒泡成 serve HTTP failed + exit(1)，
	// 干净的重启概率性留假错误日志。吞掉这一种，其余照常上报。
	if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	return nil
}

// listenConfigured 绑定配置的监听地址。KeepAlive 3 分钟与
// http.Server.ListenAndServe 内部 tcpKeepAliveListener 的行为一致。
// reuseport 开启时经 Control 在 bind 前置 SO_REUSEPORT。
func listenConfigured(listen string) (net.Listener, error) {
	lc := &net.ListenConfig{KeepAlive: 3 * time.Minute}
	if reusePortEnabled() {
		lc.Control = func(_, _ string, c syscall.RawConn) error {
			return setReusePort(c)
		}
	}
	return lc.Listen(context.Background(), "tcp", listen)
}

// reportListenFailure 处理绑定失败并退出：EADDRINUSE 时探活占用者的
// /healthz，把「谁在占端口、跑哪版、是否正在排空」写进日志并落
// bind-failure.json 标记——launchd KeepAlive 每 5s 拉起一次的 bind
// 冲突循环里，这行日志是唯一能区分「旧实例在排空」「孤儿/手动实例
// 占坑」「非本服务占用」的信号，标记文件把同一件事留成机器可查的
// 持久痕迹（面板 stats 透出）。
func reportListenFailure(listen, logRoot string, err error) {
	if errors.Is(err, syscall.EADDRINUSE) {
		holder := probeExistingInstance(listen)
		slog.Error("port already in use", "addr", listen, "holder", holder)
		recordBindFailure(logRoot, listen, holder)
	} else {
		slog.Error("listen failed", "addr", listen, "error", err)
	}
	os.Exit(1)
}

// bindFailureFile 是 EADDRINUSE 退出前落在 logs/ 的冲突标记名。
const bindFailureFile = "bind-failure.json"

// bindFailureMarker 记录一轮端口冲突的累计形态：count 跨失败累加，
// holder 取最新一次探活结果，recovered_at 标记「恢复告警已发到哪」。
type bindFailureMarker struct {
	FirstAt     string `json:"first_at"`
	LastAt      string `json:"last_at"`
	Count       int    `json:"count"`
	Addr        string `json:"addr"`
	Holder      string `json:"holder"`
	RecoveredAt string `json:"recovered_at,omitempty"`
}

// recordBindFailure 读改写 bind-failure.json：每失败一次 count 加一。
// 文件只增不删——恢复后的汇报与清理由成功启动侧负责。
func recordBindFailure(logRoot, listen, holder string) {
	path := filepath.Join(logRoot, bindFailureFile)
	var marker bindFailureMarker
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &marker)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if marker.Count == 0 {
		marker.FirstAt = now
	}
	marker.LastAt = now
	marker.Count++
	marker.Addr = listen
	marker.Holder = holder
	if raw, err := json.Marshal(marker); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// warnIfBindContentionRecovered 在成功绑定后读冲突标记：上一轮
// EADDRINUSE 循环（KeepAlive 拉起 vs 旧实例排空）若发生过，补一条
// 恢复告警把「冲突已解除、共失败几次、谁占的坑」并进 stderr.log。
// recovered_at 记忆已汇报到的 last_at：只在新冲突晚于上次汇报时
// 再警，避免每次重启都复读旧冲突。
func warnIfBindContentionRecovered(logRoot string) {
	path := filepath.Join(logRoot, bindFailureFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var marker bindFailureMarker
	if err := json.Unmarshal(raw, &marker); err != nil || marker.Count == 0 {
		return
	}
	if marker.RecoveredAt != "" && marker.LastAt <= marker.RecoveredAt {
		return
	}
	slog.Warn("port contention recovered",
		"addr", marker.Addr, "holder", marker.Holder,
		"count", marker.Count, "first_at", marker.FirstAt, "last_at", marker.LastAt)
	marker.RecoveredAt = time.Now().UTC().Format(time.RFC3339)
	if raw, err := json.Marshal(marker); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// probeExistingInstance 查询占用监听端口的进程是否为本服务实例。
// 探测地址按 listen 派生：绑了具体地址（tailscale IP 等）就探它，
// 通配（":3003"/"0.0.0.0"/"[::]"）无法拨号，回落 loopback——
// 一律 127.0.0.1 会在非环回监听时把本服务实例误报成 unresponsive。
func probeExistingInstance(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return "unknown"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return "unresponsive"
	}
	defer func() { _ = resp.Body.Close() }()
	var health struct {
		Version string `json:"version"`
		Uptime  int64  `json:"uptime_seconds"`
		Drain   bool   `json:"draining"`
		PID     int    `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || health.Version == "" {
		return "not devin-2api"
	}
	return fmt.Sprintf("devin-2api pid=%d version=%s uptime=%ds draining=%v", health.PID, health.Version, health.Uptime, health.Drain)
}
