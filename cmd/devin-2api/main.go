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
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
	"github.com/WncFht/devin2api/internal/store"
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
	configPath := flag.String("config", "", "YAML 配置文件路径；缺省按 $DEVIN2API_CONFIG → ./config.yaml → 平台默认目录解析")
	stateDir := flag.String("state-dir", "", "日志与状态文件根目录；缺省按 $DEVIN2API_STATE_DIR → 平台默认目录解析")
	showVersion := flag.Bool("version", false, "打印构建版本后退出")
	exportLegacyDir := flag.String("export-legacy", "", "把 <dir>/devin-2api.db 逐表导出为文件时代状态文件（logs/index.jsonl、auth_tokens.json、models.json、panel-settings.json、quota.jsonl、gate-state*.json）后退出；回滚文件版二进制或 DB 取证时用，不起服务")
	cellsAuditSpec := flag.String("cells-audit", "", "对账 log_cells/log_err_cells 的 rollup 覆盖缺口：'all' 全表扫或 'lo:hi' slot 窗口；只读，打完报告退出不起服务")
	cellsBackfillSpec := flag.String("cells-backfill", "", "重算修复 log_cells/log_err_cells 的 slot 窗口 'lo:hi'（如 2982899:2982912）；默认干跑只出报告，配 -cells-backfill-apply 才落库。完成后退出不起服务")
	cellsBackfillApply := flag.Bool("cells-backfill-apply", false, "让 -cells-backfill 真正写库；缺省只读审计")
	flag.Parse()
	resolved := resolvedVersion()
	if *showVersion {
		fmt.Println(resolved)
		return
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	// DEVIN2API_HANDOFF 由 deploy 的交接进程携带（scripts/lib-deploy.sh
	// spawn_handoff）：它是 reuseport 队列里接住端口的短命占位——旧实例
	// 排空、托管实例拉起之间，新连接真实落在它身上，所以请求服务路径
	// 照常装配；但后台维护与一次性播种全部归托管实例——两进程并发做
	// 同一份维护只会重复打上游、重复写库或在 UNIQUE 约束上互相打断。
	handoff := os.Getenv("DEVIN2API_HANDOFF") != ""

	if *exportLegacyDir != "" {
		if err := runExportLegacy(*exportLegacyDir); err != nil {
			slog.Error("export legacy state failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if *cellsAuditSpec != "" || *cellsBackfillSpec != "" {
		if err := runCellsMaintenance(*cellsAuditSpec, *cellsBackfillSpec, *cellsBackfillApply, *stateDir); err != nil {
			slog.Error("cells maintenance failed", "error", err)
			os.Exit(1)
		}
		return
	}

	// reuseport 并组是静默的：开了 DEVIN2API_REUSEPORT 的裸进程不撞
	// EADDRINUSE，而是直接并进监听组成为影子实例（9-15 事故：野进程
	// 并组 ~3 天吃掉 ~99% 流量、stderr 无处可查）。开 reuseport 必须
	// 持有托管出处，三者皆无即拒绝并组——不带 REUSEPORT env 的普通
	// 裸跑不受影响，仍是熟悉的 EADDRINUSE。检查在子命令 early-return
	// 之后：-export-legacy/-cells-audit 不监听端口，不受准入约束。
	if reusePortEnabled() && !reusePortProvenance() {
		slog.Error("DEVIN2API_REUSEPORT set without managed-service provenance; refusing silent reuseport join",
			"accepted", "INVOCATION_ID (systemd), DEVIN2API_HANDOFF (deploy takeover), DEVIN2API_MANAGED (service managers)",
			"fix", "run under a service manager or unset DEVIN2API_REUSEPORT")
		os.Exit(1)
	}

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
	// logRoot 是 logs/ 运行期产物（stdout/stderr.log、bind-failure.json
	// 等）的统一归属，独立于配置文件位置——配置是用户输入，状态目录是
	// 程序输出，按平台规范分家。请求摘要行、调试 payload、配额样本与
	// 闸门闩态已入库（devin-2api.db 落状态根，稍后打开）。
	logRoot := filepath.Join(absoluteStateDir, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		slog.Error("create state dir failed", "dir", logRoot, "error", err)
		os.Exit(1)
	}
	serviceConfig, lastGood, err := loadBootConfig(absoluteConfigPath, absoluteStateDir, logRoot)
	if err != nil {
		slog.Error("load config failed", "error", err)
		os.Exit(1)
	}
	slog.Info("paths resolved", "config", absoluteConfigPath, "state_dir", absoluteStateDir)

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

	// SQLite 持久层在 adapter 之前打开：号池 lane 的闸门状态与配额
	// 采样要读它（D6 消费方），导入器也得在文件被新写路径触碰前
	// 跑完。listen 已先行，导入期间的连接由内核 backlog 兜住。
	dbStore, err := store.Open(filepath.Join(absoluteStateDir, "devin-2api.db"))
	if err != nil {
		slog.Error("open store failed", "error", err)
		os.Exit(1)
	}
	defer func() { _ = dbStore.Close() }()
	// 文件时代状态的一次性导入是迁移维护而非服务依赖（能走 reuseport
	// 交接的旧实例必然已是 DB 时代、导入早完成），交接进程跳过。
	if !handoff {
		if err := dbStore.ImportLegacy(context.Background(), absoluteStateDir, logRoot); err != nil {
			slog.Error("import legacy state failed", "error", err)
			os.Exit(1)
		}
	}

	// token 允许为空启动：凭据是运行时字段——/admin/config/reload
	// 热应用与 unauthenticated 自愈链的 TokenSource 重读都能补进。
	// 空 token 起不来的话「先起服务后配凭据」没有任何热补入口。
	// devinPool 保留具体类型引用：配置热重载（ApplyConfigs）、闸门状态
	// （GateStats）、别名校验（Aliases）与逐账号凭据源（TokenFuncs）
	// 都挂在它上面；单号部署是 N=1 的退化形态，不走分支。
	// 池空集起步、立即走 Apply 首推：boot 与 reload/CRUD 共用同一条
	// 「DB 行 ∪ config 声明 merge → 整表校验 → ApplyConfigs」路径——
	// 面板建的号、disabled 与墓碑标记在启动时就生效，不存在「boot
	// 忘了 merge overlay」的旁路。rt 是账号域唯一持有点：配置快照、
	// reload 报告与写路径锁都归它，boot 首推即进它的串行化。
	devinPool, err := devin.NewPool(nil)
	if err != nil {
		slog.Error("create devin adapter failed", "error", err)
		os.Exit(1)
	}
	defer devinPool.Close()
	rt := accounts.New(absoluteConfigPath, absoluteStateDir, dbStore, devinPool)
	// 兜底服役的配置快照走带标记提交：/admin/config 的 stale 自省
	// 要能区分「文件值服役」与「缓存兜底服役」。
	if lastGood != nil {
		rt.CommitCachedConfig(serviceConfig, lastGood.CachedAt)
	} else {
		rt.CommitConfig(serviceConfig)
	}
	rt.Lock()
	_, _, err = rt.Apply(context.Background(), serviceConfig, nil)
	rt.Unlock()
	if err != nil {
		slog.Error("apply account configs failed", "error", err)
		os.Exit(1)
	}
	// 面板与 adapter 共享同一份凭据来源：adapter 的 unauthenticated
	// 自愈更新 token 后，面板的上游调用自动跟随新值。号池下面板 MVP
	// 固定绑首号；逐号凭据源另经 SetPoolTokenFuncs 喂给脱敏与配额采样。
	tokenFunc := devinPool.TokenFunc()
	// 管理器总是创建：enabled 只控制新请求是否写目录，历史查询、
	// 用量回放、清理与配额采样不随开关停掉，面板也可运行时热切换。
	debugManager := debuglog.NewManager(logRoot, debuglog.RetentionPolicy{
		Days:          *serviceConfig.Debug.RetentionDays,
		MaxTotalMB:    *serviceConfig.Debug.MaxTotalMB,
		PayloadHours:  *serviceConfig.Debug.PayloadHours,
		KeepErrorDirs: *serviceConfig.Debug.KeepErrorDirs,
	}, dbStore)
	debugManager.SetEnabled(serviceConfig.Debug.Enabled)
	debugManager.SetErrorsOnly(serviceConfig.Debug.ErrorsOnly)
	defer debugManager.Close()
	// 交接进程不跑目录清理：cleanOnce 是共享库上的多语句重事务，
	// 与托管实例并发清理只会争抢同一写连接互相打断（与下方导入
	// 跳过同一判据）。
	if handoff {
		debugManager.StopCleaner()
	}
	// 遗留磁盘请求目录的后台导入：逐目录事务搬进 debug 两表后删目录，
	// 断点记在 runtime_state，崩溃重启续传。异步跑——大目录导入不该
	// 拖住就绪；导入途中同秒新目录的 claim 由 DB 占位与 takenNames 兜底。
	// 交接进程不跑：它与托管实例并发导入会在同一目录的 UNIQUE 上互相打断。
	if !handoff {
		go func() {
			if err := dbStore.ImportDebugDirs(context.Background(), logRoot, "import_debug_progress"); err != nil {
				slog.Warn("import legacy debug dirs failed", "error", err)
			}
		}()
	}
	application := app.New(devinPool, serviceConfig.Server, debugManager)
	// 兜底服役标记进 healthz：外部探活能区分健康与「带陈化配置服役」。
	if lastGood != nil {
		application.SetServingLastGoodConfig(true)
	}
	// 用 logs 表回放预热 60 分钟趋势桶：重启后实时流量/健康时间线不从零
	// 开始，RPM 峰值口径同样恢复。完成时刻按 time+duration_ms 归桶，
	// 与 Finish 实时路径一致；管线前 Reject 不进表，这部分计数不回放。
	// 50000 是上限；LogTrendSeeds 本身只取最近 60 分钟完成的行。
	// 交接进程跳过：它的进程内指标随退出丢弃，扫表是白做的启动耗时。
	if !handoff {
		if seeds, err := dbStore.LogTrendSeeds(context.Background(), 50000); err == nil {
			for _, seed := range seeds {
				application.Metrics().SeedTrend(time.UnixMilli(seed.FinishedMS), seed.IsError)
			}
		} else {
			slog.Warn("seed trend buckets failed", "error", err)
		}
	}
	application.SetVersion(resolved)
	// 面板与 token 解耦：空 token 时 stats/rejects/日志查询仍是排障入口，
	// 上游相关调用靠 tokenFunc 现取，凭据补进后自动恢复。
	// /web、/admin、/dashboard、/public、/login、/logout 挂在根路径。
	ccPanel, err := ccpanel.New(serviceConfig.Dashboard.Password, serviceConfig.Devin.BaseURL, tokenFunc, serviceConfig.Devin.Proxy, *serviceConfig.Devin.ForceHTTP1, application.Metrics(), debugManager)
	if err != nil {
		slog.Error("create panel failed", "error", err)
		os.Exit(1)
	}
	ccPanel.SetVersion(resolved)
	ccPanel.SetGateStats(devinPool.GateStats)
	ccPanel.SetWarmStats(devinPool.WarmStats)
	ccPanel.SetDetachedStats(devinPool.DetachedStats)
	ccPanel.SetAccountGateStats(devinPool.AccountGateStats)
	ccPanel.SetAccountWarmStats(devinPool.AccountWarmStats)
	ccPanel.SetAccountLaneStates(devinPool.AccountLaneStates)
	ccPanel.SetAccountDetachedStats(devinPool.AccountDetachedStats)
	ccPanel.SetDetachEvictor(devinPool.EvictDetachedByOriginDir)
	// 排空收尾：面板 BeginDrain 经此把各 lane 闸门窗口行重放缓冲做
	// 最后一轮同步落库（best-effort，短 ctx 不拖关停）。
	ccPanel.SetGateFlusher(devinPool.FlushPendingWindows)
	// 配额探测回灌：面板采样与 test 端点把日/周剩余百分比喂给池侧
	// 降权簿记（quota_low 阈值判定在 adapter 内）。
	ccPanel.SetAccountQuotaSignal(devinPool.NoteQuotaSample)
	ccPanel.SetPoolTokenFuncs(devinPool.TokenFuncs)
	ccPanel.SetAliasesFunc(devinPool.Aliases)
	ccPanel.SetMaxConcurrencyFunc(application.MaxConcurrency)
	// 持久层须在 SetQuotaInterval 前注入：采样协程起跑时快照读它。
	ccPanel.SetStore(dbStore)
	// maskToken 常驻脱敏集合播种：config 声明的凭据与 upstream_accounts
	// 仓的存量行都登记——重启后 recentTokens 环是空的，旧调试目录里的
	// 凭据字面值照样罩得住。行内 token 含脱敏哈希形态也无妨（明文位
	// 不命中就不替换）。
	{
		var tokenSeeds []string
		for _, acc := range serviceConfig.Devin.Accounts {
			tokenSeeds = append(tokenSeeds, acc.Token)
		}
		if rows, err := dbStore.ListAccounts(context.Background()); err == nil {
			for _, row := range rows {
				tokenSeeds = append(tokenSeeds, row.Token)
			}
		}
		ccPanel.NoteUpstreamTokens(tokenSeeds...)
	}
	// 交接进程不起配额采样协程：起跑即对每个账号打一次上游并写
	// quota_samples，与托管实例的采样重复且互相计数。
	if !handoff {
		ccPanel.SetQuotaInterval(time.Duration(*serviceConfig.Debug.QuotaIntervalMinutes) * time.Minute)
	}
	// 运行时设置键仓：覆盖项落 settings 表；覆盖项对被登记键
	// 恒赢 config.yaml。构造须在 app/panel 装配与 SetQuotaInterval 之后——
	// 键的 apply/live 依赖这些持有者，boot 采样默认值反映文件生效态；
	// 先建仓再重放，让面板改的值在启动时就生效。
	settingsStore, err := ccpanel.NewPanelSettings(dbStore, ccpanel.SettingsDeps{
		Debug: debugManager,
		// 空池时 CurrentConfig 回零值 Config，devin_model 等键的 def
		// 与 reset 回落会跟着空转——回落到文件投影的 base 模板。
		DevinConfig: func() devin.Config { return rt.Snapshot() },
		// 面板写入经 UpdateConfig 在 configMu 内克隆+提交（与 reload 共用
		// 提交点）；端点三件套变化时面板自身的上游调用束跟随换绑。
		UpdateDevin: func(mutate func(*devin.Config) error) error {
			applied, err := devinPool.UpdateConfig(mutate)
			if err != nil {
				return err
			}
			if slices.Contains(applied, "devin.base_url") || slices.Contains(applied, "devin.proxy") ||
				slices.Contains(applied, "devin.force_http1") {
				cur := devinPool.CurrentConfig()
				return ccPanel.SetUpstream(cur.Endpoint.BaseURL, cur.Endpoint.Proxy, cur.Endpoint.ForceHTTP1)
			}
			return nil
		},
		MaxConcurrency:    application.MaxConcurrency,
		SetMaxConcurrency: application.SetMaxConcurrency,
		QuotaInterval:     ccPanel.QuotaInterval,
		SetQuotaInterval:  ccPanel.SetQuotaInterval,
		PprofListen:       currentPprofListen,
		SetPprofListen:    rebindPprof,
	})
	if err != nil {
		slog.Error("load panel settings failed", "error", err)
		os.Exit(1)
	}
	// 交接进程不重放面板覆盖：窗口内按文件配置服务即可，覆盖重放会
	// 顺带把配额采样等后台组件也点起来。
	if !handoff {
		if err := settingsStore.ApplyAll(); err != nil {
			slog.Warn("panel settings replay failed", "error", err)
		}
	}
	// 下游令牌仓：auth_tokens 表在刚打开并导入完的 dbStore 里。
	// /v1 准入与移植面板的令牌管理共用同一仓；costFn 用目录价把一次
	// 请求的 token 用量折成美元供费用限额窗口记账（cache_write 按
	// input 价，与 ccpanel cellCost 同口径）。
	tokenStore, err := authtoken.New(dbStore)
	if err != nil {
		slog.Error("load auth tokens failed", "error", err)
		os.Exit(1)
	}
	// 先于 dbStore.Close 排空统计写队列（defer 逆序执行，本句晚
	// 注册先跑）：排空期完成的请求回写经队列落库，不留尾巴。
	defer tokenStore.Close()
	application.SetAuthTokens(tokenStore, func(model string, input, output, cacheRead, cacheWrite int64) float64 {
		p, ok := ccPanel.CatalogPrices(context.Background())[model]
		if !ok {
			return 0
		}
		return (float64(input+cacheWrite)*p.Input + float64(cacheRead)*p.Cached + float64(output)*p.Output) / 1e6
	})
	ccPanel.SetTokenStore(tokenStore)
	ccPanel.SetConfigOps(ccpanel.ConfigOps{
		Reload: func() (*ccpanel.ConfigReloadReport, error) {
			return reloadRuntimeConfig(rt, application, ccPanel, debugManager, settingsStore)
		},
		Current: func() map[string]any {
			return rt.View()
		},
	})
	// /admin/accounts 操作面：行写入+重推+回滚的编排在 ops 闭包内
	// 完成，面板 handler 只做请求解码与 sentinel→状态码映射。
	ccPanel.SetAccountOps(rt.Ops(settingsStore))
	// 模型注册表：覆盖项落 model_registry 表；/v1 准入（停用/重定向）与
	// 移植面板的注册表页共用同一仓。
	modelStore, err := modelreg.New(dbStore)
	if err != nil {
		slog.Error("load model registry failed", "error", err)
		os.Exit(1)
	}
	application.SetModelRegistry(modelStore)
	ccPanel.SetModelRegistry(modelStore)
	ccPanel.SetSettingsStore(settingsStore)
	application.SetCCPanel(ccPanel)
	server := application.HTTPServer()
	// 探活走进程内根路由：与外部请求共用鉴权/准入/重定向/上游管线。
	ccPanel.SetProbeHandler(server.Handler)
	slog.Info("HTTP server listening", "addr", listenURL(server.Addr), "version", resolved, "reuseport", reusePortEnabled())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// ctx 取消后立刻恢复信号默认动作：否则排空期第二发 SIGINT/SIGTERM
	// 被 NotifyContext 静默吸收，运维失去「再发一次强杀」的逃生口。
	go func() {
		<-ctx.Done()
		stop()
	}()
	defer stop()
	// 库级周期养护（logs 行按龄删除、quota_samples 行数界、freelist
	// 回收）：养护对象是库不是调试目录，由这里驱动而非 debuglog
	// cleaner——后者随 debug.enabled/root 关停，保洁不该跟着停。
	// 节奏沿用原 cleaner 的 5 分钟；LogRowDays 每拍现读 Policy()，
	// 面板热改即时生效。交接进程不跑：与托管实例并发做同一份养护
	// 只会重复写库、在共享库上互相打断。
	if !handoff {
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// SIGTERM 与 tick 同就绪时 select 随机选——已取消
					// 就不再发起新一轮（Maintain 是共享库上的多语句
					// 重活，排空窗内只会白抢写连接）。
					if ctx.Err() != nil {
						return
					}
					if err := dbStore.Maintain(context.Background(), debugManager.Policy().LogRowDays); err != nil {
						slog.Warn("store maintain failed", "error", err)
					}
				}
			}
		}()
	}
	// SIGHUP（终端断开）不参与排空语义：前台裸跑时断连不应强杀在途流。
	signal.Ignore(syscall.SIGHUP)
	if err := run(ctx, application, server, listener); err != nil {
		slog.Error("serve HTTP failed", "error", err)
		os.Exit(1)
	}
}

// runExportLegacy 实现 -export-legacy：打开 <dir>/devin-2api.db，把各表
// 写回文件时代布局后返回，不起服务。DB 缺席直接报错——store.Open 会顺带
// 建空库，静默导出一套空文件比报错更危险（看着像「状态本来就空」）。
// 导出过程对每个源独立结算：部分失败时已写出的文件仍然有效，错误汇总
// 由调用方反映到退出码。
func runExportLegacy(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(abs, "devin-2api.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("state db %s: %w", dbPath, err)
	}
	dbStore, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = dbStore.Close() }()
	logRoot := filepath.Join(abs, "logs")
	if err := os.MkdirAll(logRoot, 0o755); err != nil {
		return err
	}
	rep, err := dbStore.ExportLegacy(context.Background(), abs, logRoot)
	for _, p := range rep.Written {
		slog.Info("exported", "file", p)
	}
	for _, note := range rep.Notices {
		slog.Warn("export diverted", "detail", note)
	}
	if rep.DebugRows > 0 {
		slog.Warn("debug payload left in db", "rows", rep.DebugRows,
			"detail", "debug_files/debug_chunks are not exported; request dirs will not be restored")
	}
	return err
}

// parseCellSlotRange 解析 'lo:hi' / 'lo' /（allowAll 时）'all' 的 slot
// 窗口参数，返回闭区间。
func parseCellSlotRange(spec string, allowAll bool) (int64, int64, error) {
	if spec == "all" && allowAll {
		return 0, math.MaxInt64, nil
	}
	lo, hi, ok := strings.Cut(spec, ":")
	if !ok {
		hi = lo
	}
	var loV, hiV int64
	var err error
	if loV, err = strconv.ParseInt(strings.TrimSpace(lo), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("bad slot range %q: %w", spec, err)
	}
	if hiV, err = strconv.ParseInt(strings.TrimSpace(hi), 10, 64); err != nil {
		return 0, 0, fmt.Errorf("bad slot range %q: %w", spec, err)
	}
	if loV < 0 || loV > hiV {
		return 0, 0, fmt.Errorf("bad slot range %q: need lo <= hi", spec)
	}
	return loV, hiV, nil
}

// runCellsMaintenance 实现 -cells-audit / -cells-backfill：打开生效状态
// 目录里的 devin-2api.db 跑一次对账/重算后退出，不起服务。DB 缺席直接
// 报错（store.Open 会顺手建空库，对一台没跑过实例的机器「修复」空库
// 比报错更误导）。store.Open 自身带 ReconcileCells——水位外尾巴先在
// 这里收拢，之后报告的水位即最新口径。
func runCellsMaintenance(auditSpec, backfillSpec string, apply bool, stateDir string) error {
	dir, err := config.ResolveStateDir(stateDir)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dbPath := filepath.Join(abs, "devin-2api.db")
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("state db %s: %w", dbPath, err)
	}
	dbStore, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = dbStore.Close() }()
	ctx := context.Background()
	if auditSpec != "" {
		lo, hi, err := parseCellSlotRange(auditSpec, true)
		if err != nil {
			return err
		}
		rep, err := dbStore.CellsAudit(ctx, lo, hi)
		if err != nil {
			return err
		}
		logCellsAudit("cells audit", rep)
	}
	if backfillSpec != "" {
		lo, hi, err := parseCellSlotRange(backfillSpec, false)
		if err != nil {
			return err
		}
		rep, err := dbStore.BackfillCells(ctx, lo, hi, apply)
		if err != nil {
			return err
		}
		logCellsAudit("cells backfill pre", &rep.Pre)
		if rep.Applied {
			logCellsAudit("cells backfill post", &rep.Post)
			slog.Info("cells backfill applied", "slots", fmt.Sprintf("%d:%d", rep.Pre.SlotLo, rep.Pre.SlotHi))
		} else {
			slog.Info("cells backfill dry-run (pass -cells-backfill-apply to write)")
		}
	}
	return nil
}

// logCellsAudit 把一份对账报告打成一行 slog 摘要。
func logCellsAudit(op string, r *store.CellsAuditReport) {
	slog.Info(op,
		"slots", fmt.Sprintf("%d:%d", r.SlotLo, r.SlotHi),
		"watermark", r.Watermark,
		"src_rows", r.Rows,
		"src_dims", r.Dims,
		"mismatch_dims", r.MismatchDims,
		"deficit_rows", r.DeficitRows,
		"surplus_dims", r.SurplusDims,
		"orphan_cells", r.OrphanCells,
		"err_dims", r.ErrDims,
		"err_mismatch_dims", r.ErrMismatchDims,
		"err_deficit_rows", r.ErrDeficitRows,
		"err_orphans", r.ErrOrphans,
		"tail_rows", r.TailRows)
}

// poolLaneNames 返回池中现存 lane 名序（排序后）——reload 报告的
// 「重推前 lane 名集」取它而不是 config 声明序：overlay 行（面板建
// 的号、disabled、墓碑）让声明集与 lane 集分叉。
func poolLaneNames(pool *devin.Pool) []string {
	states := pool.AccountLaneStates()
	names := make([]string, 0, len(states))
	for name := range states {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// reloadRuntimeConfig 重读配置文件并把可安全换值的字段热应用；校验失败
// 直接返回错误、旧配置继续服役（validate-then-commit）。只报告值发生
// 变化的字段——unchanged 的字段不在 applied/requires_restart 里出现。
// 仅剩监听参数 server.listen 进 requires_restart（Serve 无法换绑端口）；
// transport 固化的端点三件套走调用束原子换指针热生效。
func reloadRuntimeConfig(rt *accounts.Runtime, application *app.App, panel *ccpanel.Handler, debugManager *debuglog.Manager, settings *ccpanel.PanelSettings) (*ccpanel.ConfigReloadReport, error) {
	rt.Lock()
	defer rt.Unlock()
	cfg, err := config.Load(rt.ConfigPath())
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
	report := &ccpanel.ConfigReloadReport{At: time.Now().Format(time.RFC3339), Applied: []string{}}
	// prev 必然非空：rt.CommitConfig 在 panel 装配前已提交，
	// 而本函数只能经 panel 端点触达。
	pcfg := rt.Config()
	preLaneNames := poolLaneNames(rt.Pool())
	// 账号集合走唯一重推入口：merge overlay → 整表干跑校验 →
	// ApplyConfigs。校验失败整单 422，lanes 与库行都没动——
	// 旧配置继续服役。面板覆盖重放不在此做：下方字段差集 →
	// ResampleDefaults → ApplyAll 的顺序必须保住（覆盖恒赢文件值）。
	resolved, applied, err := rt.Apply(context.Background(), cfg, nil)
	if err != nil {
		return nil, err
	}
	report.Applied = append(report.Applied, applied...)
	// lane 增删不进任何单 lane 的字段差集：按「重推前池内名集 vs
	// 新生效名集」比对单独上报——声明序不能当判据（overlay 行让
	// 声明集与 lane 集分叉）。
	if !slices.Equal(preLaneNames, accounts.LaneNames(resolved)) {
		report.Applied = append(report.Applied, "devin.accounts")
	}
	// 端点三件套变化时面板自身的上游调用束跟随换绑（adapter 侧已在
	// ApplyConfig 内换好，同参数构建成功是前提；展示地址经 BaseURL
	// 透出，无需单独同步）。这里必须比文件级生效值而不是消费 applied
	// 名单：applied 只汇总存活 lane 的 ApplyConfig 字段差集，lane 集
	// 整体换届或空池期间改端点时新值烤进新 lane 不产生字段差，
	// 走 applied 会漏掉面板换绑。
	if accounts.BaseConfig(pcfg).Endpoint != accounts.BaseConfig(cfg).Endpoint {
		if err := panel.SetUpstream(cfg.Devin.BaseURL, cfg.Devin.Proxy, *cfg.Devin.ForceHTTP1); err != nil {
			return nil, err
		}
	}
	if pcfg.Dashboard.Password != cfg.Dashboard.Password {
		panel.SetPassword(cfg.Dashboard.Password)
		report.Applied = append(report.Applied, "dashboard.password")
	}
	if pcfg.Debug.Enabled != cfg.Debug.Enabled {
		debugManager.SetEnabled(cfg.Debug.Enabled)
		report.Applied = append(report.Applied, "debug.enabled")
	}
	if pcfg.Debug.ErrorsOnly != cfg.Debug.ErrorsOnly {
		debugManager.SetErrorsOnly(cfg.Debug.ErrorsOnly)
		report.Applied = append(report.Applied, "debug.errors_only")
	}
	newPolicy := debuglog.RetentionPolicy{
		Days:          *cfg.Debug.RetentionDays,
		MaxTotalMB:    *cfg.Debug.MaxTotalMB,
		PayloadHours:  *cfg.Debug.PayloadHours,
		KeepErrorDirs: *cfg.Debug.KeepErrorDirs,
		// LogRowDays 没有 config 对应键（面板专属旋钮）：reload 不该
		// 重置它——沿用生效值参与比较与提交，否则每次 reload 清零、
		// logs 行按龄清理被静默关闭。
		LogRowDays: debugManager.Policy().LogRowDays,
	}
	if debugManager.Policy() != newPolicy {
		debugManager.SetPolicy(newPolicy)
		report.Applied = append(report.Applied, "debug.retention")
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
	// 面板覆盖项恒赢 config.yaml：先把各键的「文件值」默认快照重灌成
	// 本次加载的派生值（def 展示与 reset 回落目标都读它），再重放
	// settings 表里登记的覆盖键压回文件值。
	settings.ResampleDefaults(ccpanel.SettingDefaults{
		Devin:           accounts.BaseConfig(cfg),
		MaxConcurrency:  cfg.Server.MaxConcurrency,
		QuotaInterval:   time.Duration(*cfg.Debug.QuotaIntervalMinutes) * time.Minute,
		PprofListen:     cfg.Debug.PprofListen,
		DebugEnabled:    cfg.Debug.Enabled,
		DebugErrorsOnly: cfg.Debug.ErrorsOnly,
		Policy:          newPolicy,
	})
	if err := settings.ApplyAll(); err != nil {
		slog.Warn("panel settings replay failed", "error", err)
	}
	rt.CommitConfig(cfg)
	// 文件再次成功加载：刷新 last-good 缓存（兜底期若有缓存也推进到
	// 最新好配置）、清 healthz 兜底位并补恢复告警——与 boot 成功加载
	// 同一条记账线。
	application.SetServingLastGoodConfig(false)
	warnIfConfigFallbackRecovered(filepath.Join(rt.StateDir(), "logs"))
	if err := config.WriteLastGood(rt.StateDir(), rt.ConfigPath(), cfg); err != nil {
		slog.Warn("write last-good config cache failed", "error", err)
	}
	rt.StoreReport(report)
	slog.Info("config reloaded", "applied", report.Applied, "requires_restart", report.RequiresRestart)
	return report, nil
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
// var 而非 const：测试缩短它来走 drain 超时的强掐路径。
var drainTimeout = 600 * time.Second

// drainKillGrace 是 drain 超时强掐后、进程退出前等在途请求跑完收尾簿记
// 的窗口：server.Close 只关连接不等待 handler 协程，被掐请求的 Complete
// 哨兵/logs 行/error.json 全在各自 defer 里跑——不等这一拍，进程先行
// 退出会让掐断请求对 logs 聚合不可见。收尾只需微秒级入队（落库由
// debugManager.Close 排空保证），窗口只为挂死的 handler 兜底。
const drainKillGrace = 5 * time.Second

// reusePortEnabled 报告是否启用 SO_REUSEPORT 重叠交接：开启后多个进程可
// 绑定同一监听地址，deploy 先起桥接进程入队再排空旧实例，做到零停机重启。
// 经环境变量（非 config）控制：只有托管实例（plist/unit 注入）与 deploy
// 拉起的交接进程拿到它——裸跑 ./devin-2api 不带 env 仍会 EADDRINUSE，
// 单实例约定的端口冲突保护不变。平台不支持（Windows）时恒 false。
func reusePortEnabled() bool {
	v := os.Getenv("DEVIN2API_REUSEPORT")
	return reusePortSupported && (v == "1" || strings.EqualFold(v, "true"))
}

// reusePortProvenance 报告进程是否持有允许加入 reuseport 组的托管出处，
// 三选一即放行：INVOCATION_ID（systemd 每次拉起 unit 自动注入，手写
// unit 与 systemd-run 同样覆盖）、DEVIN2API_HANDOFF（lib-deploy.sh
// spawn_handoff 给交接进程的标记）、DEVIN2API_MANAGED（launchd 等无
// systemd 原生标记的托管器由服务定义显式注入——deploy.sh 的 plist 与
// deploy-linux.sh 的 unit 都写它，其它托管体系照此声明）。env 证明防的
// 是不知情并组：同机能设 env 的进程仍可伪造，这层是准入宣告不是认证。
func reusePortProvenance() bool {
	return os.Getenv("INVOCATION_ID") != "" ||
		os.Getenv("DEVIN2API_HANDOFF") != "" ||
		os.Getenv("DEVIN2API_MANAGED") != ""
}

// run 启动 HTTP 服务直到 ctx 取消（SIGINT/SIGTERM），随后优雅排空：
// 关 keep-alive 让复用连接流走、reuseport 下立即释放 listener 给接替
// 进程、等在途请求排空后关闭服务器。
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
		// 带因取消先于 server.Close：conn 关闭对请求 ctx 的取消是裸
		// Canceled 无归因，先在 ctx 上钉住 drain 原因再断连接，被掐
		// 请求的收尾簿记才标得出 drain_timeout。
		killed := application.KillInflight()
		slog.Warn("shutdown: drain timed out, killing in-flight requests", "error", err, "killed", killed)
	}
	// reuseport 路径上面已直接关过底层 listener：Serve 的 defer 解除
	// listener 追踪与这里的 Close 存在竞态，落后时对同一 fd 做真实
	// 二次 close → ErrClosed 冒泡成 serve HTTP failed + exit(1)，
	// 干净的重启概率性留假错误日志。吞掉这一种，其余照常上报。
	if err := server.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		return err
	}
	// server.Close 不等待 handler 协程：被掐请求的收尾簿记（logs 行/
	// error.json）在各自 defer 里跑，等有界窗口让哨兵入队——落库由
	// deferred debugManager.Close 排空保证。缺这一步进程先行退出，
	// 掐断请求对 logs 聚合不可见。
	killCtx, killCancel := context.WithTimeout(context.Background(), drainKillGrace)
	defer killCancel()
	if err := application.WaitDrain(killCtx); err != nil {
		slog.Warn("shutdown: exiting with in-flight bookkeeping unfinished", "error", err)
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

// loadBootConfig 加载启动配置：文件加载成功即刷新 last-good 缓存、给上一轮
// 兜底期补恢复告警；加载失败且状态目录有缓存时兜底服役——配置文件的瞬时
// 破损（半截写入、引用的 credentials_file 缺失、编辑中态）不该把服务打成
// 零，判据与 reload 校验失败保留旧配置同源：坏的新配置永不顶替最近一次
// 的好配置。Restart=always 下兜底把「重启循环断供」改写成「降级服役」。
// 返回的 lastGood 非空表示本次服役的是缓存投影。
func loadBootConfig(configPath, stateDir, logRoot string) (config.Config, *config.LastGood, error) {
	cfg, err := config.Load(configPath)
	if err == nil {
		warnIfConfigFallbackRecovered(logRoot)
		if err := config.WriteLastGood(stateDir, configPath, cfg); err != nil {
			slog.Warn("write last-good config cache failed", "error", err)
		}
		return cfg, nil, nil
	}
	cached, cacheErr := config.ReadLastGood(stateDir)
	if cacheErr != nil {
		return config.Config{}, nil, err
	}
	slog.Warn("load config failed; serving last-known-good config",
		"error", err, "cached_at", cached.CachedAt.Format(time.RFC3339), "cache_source", cached.SourcePath)
	recordConfigFallback(logRoot, err, cached)
	return cached.Config, &cached, nil
}

// configFallbackFile 是兜底服役事件落在 logs/ 的标记名。
const configFallbackFile = "config-fallback.json"

// configFallbackMarker 记录兜底服役的累计形态：count 跨兜底 boot 累加，
// reason 取最新一次加载失败原因，cached_at 是所服缓存的写入时刻，
// recovered_at 标记「恢复告警已发到哪」。
type configFallbackMarker struct {
	FirstAt     string `json:"first_at"`
	LastAt      string `json:"last_at"`
	Count       int    `json:"count"`
	Reason      string `json:"reason"`
	CachedAt    string `json:"cached_at"`
	RecoveredAt string `json:"recovered_at,omitempty"`
}

// recordConfigFallback 读改写 config-fallback.json：每兜底服役一次 count
// 加一。文件只增不删——恢复后的汇报与清理由成功加载侧负责。
func recordConfigFallback(logRoot string, loadErr error, cached config.LastGood) {
	path := filepath.Join(logRoot, configFallbackFile)
	var marker configFallbackMarker
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &marker)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if marker.Count == 0 {
		marker.FirstAt = now
	}
	marker.LastAt = now
	marker.Count++
	marker.Reason = loadErr.Error()
	marker.CachedAt = cached.CachedAt.UTC().Format(time.RFC3339)
	if raw, err := json.Marshal(marker); err == nil {
		_ = os.WriteFile(path, raw, 0o644)
	}
}

// warnIfConfigFallbackRecovered 在配置成功加载后读兜底标记：上一轮兜底
// 服役若发生过，补一条恢复告警把「兜底过几次、最新失败原因、服的缓存
// 时刻」并进 stderr.log——兜底期的事故复盘需要这条边界。recovered_at
// 记忆已汇报到的 last_at：只在新兜底晚于上次汇报时再警。
func warnIfConfigFallbackRecovered(logRoot string) {
	path := filepath.Join(logRoot, configFallbackFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var marker configFallbackMarker
	if err := json.Unmarshal(raw, &marker); err != nil || marker.Count == 0 {
		return
	}
	if marker.RecoveredAt != "" && marker.LastAt <= marker.RecoveredAt {
		return
	}
	slog.Warn("config fallback recovered",
		"count", marker.Count, "reason", marker.Reason,
		"first_at", marker.FirstAt, "last_at", marker.LastAt, "cached_at", marker.CachedAt)
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
