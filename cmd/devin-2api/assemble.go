// 本文件是服务装配图：main 只留进程边界（参数、listen、库打开、信号、
// 收尾 defer），组件构造序与互接线收口在 assemble——按 build→settle→
// verify 三段组织，段间注释即图的结构说明。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/app"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
	"github.com/WncFht/devin2api/internal/selfupdate"
	"github.com/WncFht/devin2api/internal/store"
)

// bootParams 是 assemble 的输入：装配前已就位的进程边界——生效配置与
// 兜底快照、路径三件套、构建版本、进程角色与持久层句柄。listen/store
// 的打开留在 main（各自失败有独立的取证落点），组件装配从池开始。
type bootParams struct {
	configPath string
	stateDir   string
	logRoot    string
	cfg        config.Config
	lastGood   *config.LastGood
	version    string
	role       procRole
	db         *store.Store
}

// assembly 是一次完整装配的产出：main 起停与收尾要的句柄按字段回传，
// 组件间构造序与互接线全留在 assemble 内部不外泄。
type assembly struct {
	pool   *devin.Pool
	debug  *debuglog.Manager
	app    *app.App
	tokens *authtoken.Store
	server *http.Server
}

// requiredWiring 是生产服役必须的互接线名单：settle 段每步执行后登记
// 同名键，assemble 收尾逐条比对——漏接或漏登记在启动时显式失败，而不
// 是带病服役后面板某组端点静默缺席。app 侧的注入完整性与面板路由时
// 序另有 App.WiringGaps 按字段自省，与本名单互补。
var requiredWiring = []string{
	"panel.update_ops",
	"app.auth_tokens",
	"panel.config_ops",
	"panel.account_ops",
	"app.model_registry",
	"panel.settings_store",
	"app.ccpanel",
	"panel.probe_handler",
}

// assemble 完成全部组件装配：build 段单向依赖构造（依赖只向前指），
// settle 段收口循环互接线，verify 段核对必需接线已执行。返回装配产物
// 句柄集；任一步失败即返回错误，调用方按进程级失败处理（os.Exit 前
// 的组件泄漏与旧内联序同语义——进程退出即回收）。
func assemble(p bootParams) (*assembly, error) {
	// ———— build：单向依赖构造 ————
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
		return nil, fmt.Errorf("create devin adapter failed: %w", err)
	}
	rt := accounts.New(p.configPath, p.stateDir, p.db, devinPool)
	// 兜底服役的配置快照走带标记提交：/admin/config 的 stale 自省
	// 要能区分「文件值服役」与「缓存兜底服役」。
	if p.lastGood != nil {
		rt.CommitCachedConfig(p.cfg, p.lastGood.CachedAt)
	} else {
		rt.CommitConfig(p.cfg)
	}
	rt.Lock()
	_, err = rt.Apply(context.Background(), p.cfg, nil)
	rt.Unlock()
	if err != nil {
		return nil, fmt.Errorf("apply account configs failed: %w", err)
	}
	// 面板与 adapter 共享同一份凭据来源：adapter 的 unauthenticated
	// 自愈更新 token 后，面板的上游调用自动跟随新值。号池下面板 MVP
	// 固定绑首号；逐号凭据源走 PoolDeps.Snapshot 的 TokenFuncs 喂给
	// 脱敏与配额采样。
	tokenFunc := devinPool.TokenFunc()
	// 管理器总是创建：enabled 只控制新请求是否写目录，历史查询、
	// 用量回放、清理与配额采样不随开关停掉，面板也可运行时热切换。
	debugManager := debuglog.NewManager(p.logRoot, debuglog.RetentionPolicy{
		Days:          *p.cfg.Debug.RetentionDays,
		MaxTotalMB:    *p.cfg.Debug.MaxTotalMB,
		PayloadHours:  *p.cfg.Debug.PayloadHours,
		KeepErrorDirs: *p.cfg.Debug.KeepErrorDirs,
	}, p.db)
	debugManager.SetEnabled(p.cfg.Debug.Enabled)
	debugManager.SetErrorsOnly(p.cfg.Debug.ErrorsOnly)
	// 目录清理器随 NewManager 自起跑，交接侧反向关停：cleanOnce 是
	// 共享库上的多语句重事务，与托管实例并发清理只会争抢同一写连接
	// 互相打断（与下方导入跳过同一判据）。
	if p.role.handoff {
		debugManager.StopCleaner()
	}
	// 遗留磁盘请求目录的后台导入：逐目录事务搬进 debug 两表后删目录，
	// 断点记在 runtime_state，崩溃重启续传。异步跑——大目录导入不该
	// 拖住就绪；导入途中同秒新目录的 claim 由 DB 占位与 takenNames 兜底。
	// 跳过交接侧：与托管实例并发导入会在同一目录的 UNIQUE 上互相打断。
	p.role.spawn("import legacy debug dirs", func() {
		if err := p.db.ImportDebugDirs(context.Background(), p.logRoot, "import_debug_progress"); err != nil {
			slog.Warn("import legacy debug dirs failed", "error", err)
		}
	})
	application := app.New(devinPool, p.cfg.Server, debugManager)
	// 兜底服役标记进 healthz：外部探活能区分健康与「带陈化配置服役」。
	if p.lastGood != nil {
		application.SetServingLastGoodConfig(true)
	}
	// 用 logs 表回放预热 60 分钟趋势桶：重启后实时流量/健康时间线不从零
	// 开始，RPM 峰值口径同样恢复。完成时刻按 time+duration_ms 归桶，
	// 与 Finish 实时路径一致；管线前 Reject 不进表，这部分计数不回放。
	// 50000 是上限；LogTrendSeeds 本身只取最近 60 分钟完成的行。
	// 交接侧跳过的理由不同：它的进程内指标随退出丢弃，扫表是白做的
	// 启动耗时（不是共享库竞争）。
	p.role.duty("seed trend buckets", func() {
		seeds, err := p.db.LogTrendSeeds(context.Background(), 50000)
		if err != nil {
			slog.Warn("seed trend buckets failed", "error", err)
			return
		}
		for _, seed := range seeds {
			application.Metrics().SeedTrend(time.UnixMilli(seed.FinishedMS), seed.IsError)
		}
	})
	application.SetVersion(p.version)
	// 下游令牌仓：auth_tokens 表在刚打开并导入完的 db 里。
	// /v1 准入与移植面板的令牌管理共用同一仓；costFn 用目录价把一次
	// 请求的 token 用量折成美元供费用限额窗口记账（公式即
	// ccpanel.TokenCost，与面板聚合同一份实现）。
	tokenStore, err := authtoken.New(p.db)
	if err != nil {
		return nil, fmt.Errorf("load auth tokens failed: %w", err)
	}
	// 模型注册表：覆盖项落 model_registry 表；/v1 准入（停用/重定向）与
	// 移植面板的注册表页共用同一仓。
	modelStore, err := modelreg.New(p.db)
	if err != nil {
		return nil, fmt.Errorf("load model registry failed: %w", err)
	}
	// 面板与 token 解耦：空 token 时 stats/rejects/日志查询仍是排障入口，
	// 上游相关调用靠 tokenFunc 现取，凭据补进后自动恢复。
	// /web、/admin、/dashboard、/public、/login、/logout 挂在根路径。
	// 依赖一次接齐：仓类（store/tokens/models）与号池接口都在构造期
	// 注入——遥测走 Snapshot（每请求一次求值，同一切面收齐
	// gate/warm/detached/逐号状态/别名/凭据源），动作口是脱钩逐出、
	// 排空闸门冲刷与配额探测回灌三个。
	ccPanel, err := ccpanel.New(ccpanel.Deps{
		Password:           p.cfg.Dashboard.Password,
		BaseURL:            p.cfg.Devin.BaseURL,
		Proxy:              p.cfg.Devin.Proxy,
		ForceHTTP1:         *p.cfg.Devin.ForceHTTP1,
		TokenFunc:          tokenFunc,
		Metrics:            application.Metrics(),
		Debug:              debugManager,
		Store:              p.db,
		Tokens:             tokenStore,
		Models:             modelStore,
		MaxConcurrencyFunc: application.MaxConcurrency,
		Pool: &ccpanel.PoolDeps{
			Snapshot:      devinPool.Snapshot,
			EvictDetached: devinPool.EvictDetachedByOriginDir,
			FlushGates:    devinPool.FlushPendingWindows,
			NoteQuota:     devinPool.NoteQuotaSample,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create panel failed: %w", err)
	}
	ccPanel.SetVersion(p.version)
	// 自更新服务端点（/admin/update*）：Service 只持有路径/版本/库句柄，
	// 编排进程拉起、换名与重启推进全在 internal/selfupdate 内闭环。
	updateSvc, err := selfupdate.New(p.db, p.version, p.configPath, p.stateDir)
	if err != nil {
		return nil, fmt.Errorf("init self-update service failed: %w", err)
	}
	// 进程指标历史环：30s 一拍采进内存环，/admin/runtime-metrics/history
	// 的数据源。纯进程内读取——不打上游、不写库，与配额采样不同，
	// 交接进程同样起跑（重叠窗内它自己的历史也是有效观测）。
	ccPanel.StartMetricsHistory()
	// maskToken 常驻脱敏集合播种归面板自理：重启后 recentTokens 环是
	// 空的，旧调试目录里的凭据字面值照样罩得住。
	ccPanel.SeedCredentialMasks(context.Background(), &p.cfg, p.db)
	// 配额采样协程是托管职责：起跑即对每个账号打一次上游并写
	// quota_samples，交接侧与托管实例的采样会重复且互相计数。
	p.role.duty("quota sampler", func() {
		ccPanel.SetQuotaInterval(time.Duration(*p.cfg.Debug.QuotaIntervalMinutes) * time.Minute)
	})
	// 运行时设置键仓：覆盖项落 settings 表；覆盖项对被登记键
	// 恒赢 config.yaml。构造须在 app/panel 装配与 SetQuotaInterval 之后——
	// 键的 apply/live 依赖这些持有者，boot 采样默认值反映文件生效态；
	// 先建仓再重放，让面板改的值在启动时就生效。
	settingsStore, err := ccpanel.NewPanelSettings(p.db, ccpanel.SettingsDeps{
		Debug: debugManager,
		// 空池时 CurrentConfig 回零值 Config，devin_model 等键的 def
		// 与 reset 回落会跟着空转——回落到文件投影的 base 模板。
		DevinConfig: func() devin.Config { return rt.Snapshot() },
		// 面板写入经 UpdateConfig 在 configMu 内克隆+提交（与 reload 共用
		// 提交点）；端点三件套按生效值变化时面板自身的上游调用束跟随换绑。
		UpdateDevin: func(mutate func(*devin.Config) error) error {
			changed, err := rt.UpdateDevin(mutate)
			if err != nil {
				return err
			}
			if changed {
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
		return nil, fmt.Errorf("load panel settings failed: %w", err)
	}
	// 覆盖重放是托管职责：交接窗口内按文件配置服务即可，重放会顺带把
	// 配额采样等后台组件也点起来。
	p.role.duty("replay panel settings", func() {
		if err := settingsStore.ApplyAll(); err != nil {
			slog.Warn("panel settings replay failed", "error", err)
		}
	})

	// ———— settle：互接线收口 ————
	// app↔panel 互指只能两阶段：面板构造期要吃 app 的 Metrics/
	// MaxConcurrencyFunc/Handler，app 的 Router 又要在构建期挂面板
	// 路由——任一方先构造都拿不到对方全集，只能后注入收口。顺序
	// 敏感点只有一个：SetCCPanel 必须先于 HTTPServer()（Router 构建
	// 期才挂面板路由，之后注入会静默丢整组 /admin、/dashboard 路由）。
	// 每步执行后登记 wired，verify 段按 requiredWiring 逐条比对。
	wired := map[string]bool{}
	ccPanel.SetUpdateOps(updateSvc)
	wired["panel.update_ops"] = true
	application.SetAuthTokens(tokenStore, func(model string, input, output, cacheRead, cacheWrite int64) float64 {
		return ccpanel.TokenCost(input, output, cacheRead, cacheWrite, ccPanel.CatalogPrices(context.Background())[model])
	})
	wired["app.auth_tokens"] = true
	ccPanel.SetConfigOps(ccpanel.ConfigOps{
		Reload: func() (*accounts.ReloadReport, error) {
			return reloadRuntimeConfig(rt, application, ccPanel, debugManager, settingsStore)
		},
		Current: func() map[string]any {
			return rt.View()
		},
		// provenance 把三层合并摊平给面板读：settings 覆盖行（键→yaml
		// 路径→文件值→生效值）、账号 overlay（名→来源→被顶掉的字段）、
		// 面板密码来源（db 覆盖 / file / open）。账号段缺席语义=读库失败，
		// 其余两段照常回——provenance 是观测面，不因一路缺腿整体 500。
		Provenance: func(ctx context.Context) map[string]any {
			view := map[string]any{
				"settings":           settingsStore.OverrideProvenance(),
				"dashboard_password": ccPanel.PasswordSource(),
			}
			resolved, err := p.db.EffectiveAccounts(ctx, rt.Config().Devin.Accounts)
			if err != nil {
				slog.Warn("provenance: resolve accounts failed", "error", err)
				return view
			}
			rows := make([]map[string]any, 0, len(resolved))
			for _, a := range resolved {
				rows = append(rows, map[string]any{
					"name":       a.Name,
					"source":     a.Source,
					"overridden": a.Overridden,
					"disabled":   a.Disabled,
				})
			}
			view["accounts"] = rows
			return view
		},
	})
	wired["panel.config_ops"] = true
	// /admin/accounts 操作面：行写入+重推+回滚的编排在 ops 闭包内
	// 完成，面板 handler 只做请求解码与 sentinel→状态码映射。
	ccPanel.SetAccountOps(rt.Ops(settingsStore.ApplyAll))
	wired["panel.account_ops"] = true
	application.SetModelRegistry(modelStore)
	wired["app.model_registry"] = true
	ccPanel.SetSettingsStore(settingsStore)
	wired["panel.settings_store"] = true
	application.SetCCPanel(ccPanel)
	wired["app.ccpanel"] = true
	server := application.HTTPServer()
	// 探活走进程内根路由：与外部请求共用鉴权/准入/重定向/上游管线。
	ccPanel.SetProbeHandler(server.Handler)
	wired["panel.probe_handler"] = true

	// ———— verify：必需接线自检 ————
	// 两层互补：wired 名单覆盖不可自省的面板 setter（ccpanel 不外露
	// 读口），WiringGaps 覆盖 app 侧的注入完整性与面板路由时序。
	// 任一缺口即拒绝服役——缺的线不会半路再长出来。
	for _, name := range requiredWiring {
		if !wired[name] {
			return nil, fmt.Errorf("required wiring %q did not run", name)
		}
	}
	if gaps := application.WiringGaps(); len(gaps) > 0 {
		return nil, fmt.Errorf("app wiring incomplete: %s", strings.Join(gaps, ", "))
	}
	return &assembly{
		pool:   devinPool,
		debug:  debugManager,
		app:    application,
		tokens: tokenStore,
		server: server,
	}, nil
}
