// Package accounts 是上游账号域的运行时持有点与写路径收口：最近一次
// 成功加载的配置快照（config 声明集来源）、reload 报告与「DB overlay
// 行 ∪ config 声明 merge → 整表校验 → ApplyConfigs」的唯一重推管线
// 都在这。boot/reload/CRUD 三路共用同一把串行化锁——行写入、重推与
// 失败回滚是一条多步提交，和热更必须互斥才不交错。
package accounts

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/ccpanel"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/store"
)

// Runtime 是上游账号域的运行时持有点：configPath（声明集重读源与
// credentials_file 相对路径锚）、db（overlay 行与闸门状态仓）、
// pool（lane 热应用目标）三个依赖，加最近配置快照、reload 报告与
// 写路径串行化锁。装配层（main）构造后经参数透传——刻意不留包级
// 单例，测试可平行起多套互不影响。
type Runtime struct {
	mu         sync.Mutex
	configPath string
	// stateDir 是 credentials_content 粘贴上传的落盘根
	//（account-credentials/ 子目录），见 Ops 的 Create/Update。
	stateDir   string
	db         *store.Store
	pool       *devin.Pool
	state      atomic.Pointer[configState]
	lastReload atomic.Pointer[ccpanel.ConfigReloadReport]
}

// configState 是最近一次成功加载的配置快照：配置自省端点拿它回答
// 「文件在最后一次加载后是否被改过」（file_mtime vs 当前 mtime）。
type configState struct {
	cfg       config.Config
	loadedAt  time.Time
	fileMtime time.Time
}

// New 组装账号域持有点。db 允许为 nil（纯 config 视图的离线用法），
// 但一切经 EffectiveAccounts 的读路径随之不可用——调用方自行保证。
func New(configPath, stateDir string, db *store.Store, pool *devin.Pool) *Runtime {
	return &Runtime{configPath: configPath, stateDir: stateDir, db: db, pool: pool}
}

// Lock/Unlock 串行化写路径：merge→校验→ApplyConfigs→CommitConfig 是
// 一串多步提交，并发 reload/CRUD 交错会让配置快照与生效值分叉。
// Apply 与 Ops 闭包都要求调用方已持锁（Ops 内部自取）。
func (rt *Runtime) Lock()   { rt.mu.Lock() }
func (rt *Runtime) Unlock() { rt.mu.Unlock() }

// ConfigPath 返回声明集所在文件路径——reload 重读共用。
func (rt *Runtime) ConfigPath() string { return rt.configPath }

// Pool 返回 lane 热应用目标（reload 名集比对等只读用法）。
func (rt *Runtime) Pool() *devin.Pool { return rt.pool }

// Config 返回最近一次成功加载的配置；未加载过回零值。
func (rt *Runtime) Config() config.Config {
	if cur := rt.state.Load(); cur != nil {
		return cur.cfg
	}
	return config.Config{}
}

// CommitConfig 提交一次成功加载的配置快照（boot 首推与 reload 提交
// 共用）：fileMtime 现取，stale 判定据此成立。
func (rt *Runtime) CommitConfig(cfg config.Config) {
	rt.state.Store(&configState{cfg: cfg, loadedAt: time.Now(), fileMtime: fileMtime(rt.configPath)})
}

// StoreReport 记录最近一次 reload 报告，View 透出。
func (rt *Runtime) StoreReport(report *ccpanel.ConfigReloadReport) {
	rt.lastReload.Store(report)
}

// Apply 是账号集合的唯一重推入口（boot/reload/CRUD 三路共用，调用方
// 须持 rt 锁）：DB 行与 config 声明 merge 出生效集 → 剔除墓碑与停用
// → 整表干跑校验（校验失败即拒载、旧配置继续服役）→ 逐 lane 映射 →
// ApplyConfigs 名键差集热换（同名 lane 走 ApplyConfig 保 warm 谱系/
// assignments/在途流，新增建 lane，摘下异步 Close）→ settings 非空
// 时重放面板覆盖（必须在 ApplyConfigs 之后，新建 lane 才吃得到
// devin_model 等覆盖）→ 收死墓碑。空生效集合法：空集即全部 lane 被
// 摘出。返回生效视图与 ApplyConfigs 的字段差集。
func (rt *Runtime) Apply(ctx context.Context, cfg config.Config, settings *ccpanel.PanelSettings) ([]store.ResolvedAccount, []string, error) {
	resolved, err := rt.db.EffectiveAccounts(ctx, cfg.Devin.Accounts)
	if err != nil {
		return nil, nil, err
	}
	synthesized, err := config.ResolveAccounts(laneConfigs(resolved), filepath.Dir(rt.configPath))
	if err != nil {
		return nil, nil, err
	}
	applied, err := rt.pool.ApplyConfigs(rt.devinConfigs(cfg, synthesized))
	if err != nil {
		return nil, nil, err
	}
	if settings != nil {
		if err := settings.ApplyAll(); err != nil {
			slog.Warn("panel settings replay failed", "error", err)
		}
	}
	if n, err := rt.db.GCTombstonedAccounts(ctx, declaredNames(cfg)); err != nil {
		slog.Warn("tombstoned accounts gc failed", "error", err)
	} else if n > 0 {
		slog.Info("collected dead tombstone accounts", "count", n)
	}
	return resolved, applied, nil
}

// Snapshot 是 settings 的 devin 配置快照源：有 lane 时读首 lane 活
// 配置（面板覆盖与热应用后的口径）；空池回落到最近加载配置投影出的
// base 模板，def 展示与 reset 回落仍按文件值给默认。
func (rt *Runtime) Snapshot() devin.Config {
	if cfg := rt.pool.CurrentConfig(); cfg.Identity.Name != "" {
		return cfg
	}
	if cur := rt.state.Load(); cur != nil {
		return BaseConfig(cur.cfg)
	}
	return devin.Config{}
}

// View 返回配置自省视图（/admin/config/current 的载荷）：最近成功
// 加载的配置（脱敏）、文件 mtime、加载后是否被改动（stale）与最近
// 一次 reload 报告。配置经 yaml 往返成 map，键名与 config.yaml 一致。
func (rt *Runtime) View() map[string]any {
	view := map[string]any{"path": rt.configPath}
	cur := rt.state.Load()
	if cur == nil {
		view["error"] = "config not loaded"
		return view
	}
	fields := map[string]any{}
	if raw, err := yaml.Marshal(cur.cfg); err == nil {
		_ = yaml.Unmarshal(raw, &fields)
	}
	redactSecrets(fields)
	view["config"] = fields
	view["loaded_at"] = cur.loadedAt.Format(time.RFC3339)
	view["file_mtime"] = cur.fileMtime.Format(time.RFC3339)
	view["stale"] = fileMtime(rt.configPath).After(cur.fileMtime)
	if report := rt.lastReload.Load(); report != nil {
		view["last_reload"] = report
	}
	return view
}

// ResolveToken 解析生效视图里某账号的凭据（ops.TokenOf 与 cmd/probe
// 共用同一口径）：name 非空按名取（无名或墓碑即 ErrAccountNotFound），
// 空名取首个非墓碑账号（disabled 仍可取——它的凭据本身有效，停用只
// 摘 lane 不废凭据）。解析逐字段生效：credentials_file 现读文件
// （CLI 续期直接改写文件），读不出回落已合并的 token（行覆盖或
// config 值——「都给」形态下它是初始值兜底）。
func ResolveToken(ctx context.Context, db *store.Store, declared []config.DevinAccountConfig, name string) (string, error) {
	resolved, err := db.EffectiveAccounts(ctx, declared)
	if err != nil {
		return "", err
	}
	var acc *store.ResolvedAccount
	if name != "" {
		acc = findResolved(resolved, name)
	} else {
		for i := range resolved {
			if resolved[i].Source != store.AccountSourceTombstoned {
				acc = &resolved[i]
				break
			}
		}
	}
	if acc == nil || acc.Source == store.AccountSourceTombstoned {
		return "", fmt.Errorf("account %q: %w", name, store.ErrAccountNotFound)
	}
	return credential(acc)
}

// credential 从一条生效账号解析凭据（ResolveToken 的收尾段）：文件现读
// → 字面量 → api_key。durable api_key 在 seat 系端点（GetUserStatus/
// 配额采样）上本身就是合法凭据——api_key-only 账号的探测路径照样成立，
// lane 侧它另作 mint 来源。
func credential(acc *store.ResolvedAccount) (string, error) {
	if acc.CredentialsFile != "" {
		if token := config.TokenFromCredentialsFile(acc.CredentialsFile); token != "" {
			return token, nil
		}
	}
	if acc.Token != "" {
		return acc.Token, nil
	}
	if acc.APIKey != "" {
		return acc.APIKey, nil
	}
	return "", fmt.Errorf("account %q has no resolvable credential", acc.Name)
}

// devinConfigs 把「合成+校验后的生效账号集」映射成每 lane 一份的
// adapter 配置：synthesized 入参必须是 Apply 里经 ResolveAccounts
// 校验、锚定、补齐过的生效集（非 cfg.Devin.Accounts 直取——overlay
// 行覆盖已并入生效集）。Name/Token/GateStateStore/TokenSource 按账号
// 落地，闸门状态各 lane 共用同一库（键按 lane 名派生 gate:<name>）。
// boot/reload/CRUD 都经 Apply 走这里，ApplyConfigs 与 NewPool 看到的
// 字段口径一致。
func (rt *Runtime) devinConfigs(cfg config.Config, synthesized []config.DevinAccountConfig) []devin.Config {
	lanes := make([]devin.Config, 0, len(synthesized))
	for _, account := range synthesized {
		lane := BaseConfig(cfg)
		lane.Identity.Name = account.Name
		lane.Identity.Token = account.Token
		lane.Identity.APIKey = account.APIKey
		lane.Priority = account.Priority
		// 号级 max_rpm 覆盖全局闸门配额；0 继承 devin.max_rpm。
		if account.MaxRPM > 0 {
			lane.Gate.MaxRPM = account.MaxRPM
		}
		lane.GateStateStore = rt.db
		if account.CredentialsFile != "" {
			// credentials_file 型账号：CLI 续期直接改写该文件，重读它
			// 即跟随续期——fht-mba 的 B 号正是这个形态。
			credentialsFile := account.CredentialsFile
			lane.Identity.TokenSource = func() string {
				return config.TokenFromCredentialsFile(credentialsFile)
			}
		} else {
			// 字面量 token 账号：自愈源是「当下生效集」——重读文件拿
			// 声明集再过 DB 行 merge，面板写过的行覆盖值与 config 编辑
			// 同权，单号改凭据两条路都救得回来。
			name := account.Name
			lane.Identity.TokenSource = func() string {
				return rt.tokenForLane(name)
			}
		}
		lanes = append(lanes, lane)
	}
	return lanes
}

// tokenForLane 是字面量账号的自愈凭据源：重读文件拿声明集再过 DB
// 行 merge——面板写过的行覆盖值与 config 编辑同权，单号改凭据两条
// 路都救得回来。
func (rt *Runtime) tokenForLane(name string) string {
	reloaded, err := config.Load(rt.configPath)
	if err != nil {
		return ""
	}
	effective, err := rt.db.EffectiveAccounts(context.Background(), reloaded.Devin.Accounts)
	if err != nil {
		return ""
	}
	if acc := findResolved(effective, name); acc != nil {
		return acc.Token
	}
	return ""
}

// BaseConfig 把 devin.* 全局字段投影成 lane 无关的 adapter 配置模板：
// 端点/指纹/闸门/保温各 lane 共享同一组值。devinConfigs 逐 lane 复制
// 它再叠加 Name/Token/TokenSource；空池时它还是 settings 默认值
// （devin_model 等键的 def 展示与 reset 回落目标）的兜底形态——
// 文件值口径，不是零值。reload 的端点换绑比对也用它。
func BaseConfig(cfg config.Config) devin.Config {
	return devin.Config{
		Endpoint: devin.Endpoint{
			BaseURL:    cfg.Devin.BaseURL,
			Proxy:      cfg.Devin.Proxy,
			ForceHTTP1: cfg.Devin.ForceHTTP1 != nil && *cfg.Devin.ForceHTTP1,
		},
		Model:         cfg.Devin.Model,
		Aliases:       cfg.Devin.Aliases,
		ClientName:    cfg.Devin.ClientName,
		ClientVersion: cfg.Devin.ClientVersion,
		ClientOS:      cfg.Devin.ClientOS,
		// 会话亲和 TTL、配额降权阈值与 post-content 无进度档是全局字段，
		// 经 base 模板铺进各 lane。
		SessionAffinityTTLSeconds: cfg.Devin.SessionAffinityTTLSeconds,
		QuotaLowThresholdPercent:  cfg.Devin.QuotaLowThresholdPercent,
		NoProgressTimeout:         time.Duration(cfg.Devin.NoProgressTimeoutSeconds) * time.Second,
		Gate: devin.GateConfig{
			MaxRPM:          cfg.Devin.MaxRPM,
			MaxHold:         time.Duration(cfg.Devin.GateMaxHoldSeconds) * time.Second,
			DripInterval:    time.Duration(cfg.Devin.GateDripIntervalSeconds) * time.Second,
			DefaultLatch:    time.Duration(cfg.Devin.GateDefaultLatchSeconds) * time.Second,
			WindowOffset:    time.Duration(cfg.Devin.GateWindowOffsetSeconds) * time.Second,
			WindowGuard:     time.Duration(cfg.Devin.GateWindowGuardSeconds) * time.Second,
			BgMaxHold:       time.Duration(cfg.Devin.GateBgMaxHoldSeconds) * time.Second,
			BgReserveMargin: cfg.Devin.GateBgReserveMargin,
		},
		Warm: devin.WarmConfig{
			Enabled:          cfg.Devin.WarmPrefixEnabled,
			Interval:         time.Duration(cfg.Devin.WarmPrefixIntervalSeconds) * time.Second,
			JitterRatio:      cfg.Devin.WarmPrefixJitterRatio,
			MaxStreams:       cfg.Devin.WarmPrefixMaxStreams,
			MaxRetainedMB:    cfg.Devin.WarmPrefixMaxRetainedMB,
			MinPrefixTokens:  cfg.Devin.WarmPrefixMinPrefixTokens,
			BlockedMaxIdle:   time.Duration(cfg.Devin.WarmPrefixBlockedMaxIdleSeconds) * time.Second,
			UserPacedMaxIdle: time.Duration(cfg.Devin.WarmPrefixUserPacedMaxIdleSeconds) * time.Second,
			SubDoneMaxIdle:   time.Duration(cfg.Devin.WarmPrefixSubDoneMaxIdleSeconds) * time.Second,
			UnknownMaxIdle:   time.Duration(cfg.Devin.WarmPrefixUnknownMaxIdleSeconds) * time.Second,
			BlockedNames:     cfg.Devin.WarmPrefixBlockedNames,
			UserPacedNames:   cfg.Devin.WarmPrefixUserPacedNames,
		},
	}
}

// laneConfigs 投影该进池的账号（非墓碑且未停用）成声明形状——喂整表
// 校验与 devinConfigs；校验锚定/补齐后再进 lane 映射。
func laneConfigs(resolved []store.ResolvedAccount) []config.DevinAccountConfig {
	out := make([]config.DevinAccountConfig, 0, len(resolved))
	for _, acc := range resolved {
		if acc.Source == store.AccountSourceTombstoned || acc.Disabled {
			continue
		}
		out = append(out, config.DevinAccountConfig{
			Name: acc.Name, Token: acc.Token, CredentialsFile: acc.CredentialsFile,
			APIKey: acc.APIKey, Priority: acc.Priority, MaxRPM: acc.MaxRPM,
		})
	}
	return out
}

// LaneNames 返回生效集里该进池的名序（非墓碑且未停用，排序后）——
// 与 laneConfigs 同口径，reload 据此比对出账号集合变化。
func LaneNames(resolved []store.ResolvedAccount) []string {
	names := make([]string, 0, len(resolved))
	for _, acc := range resolved {
		if acc.Source == store.AccountSourceTombstoned || acc.Disabled {
			continue
		}
		names = append(names, acc.Name)
	}
	slices.Sort(names)
	return names
}

// declaredNames 返回 config 声明名集——GCTombstonedAccounts 的「仍受
// 声明保护」白名单：不在集里的墓碑已死透，可以物理收。
func declaredNames(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.Devin.Accounts))
	for _, acc := range cfg.Devin.Accounts {
		names = append(names, acc.Name)
	}
	return names
}

// findResolved 按名找生效视图条目；不在集里返回 nil。
func findResolved(resolved []store.ResolvedAccount, name string) *store.ResolvedAccount {
	for i := range resolved {
		if resolved[i].Name == name {
			return &resolved[i]
		}
	}
	return nil
}

// redactSecrets 把配置视图里的凭据值替换为 sha256 前缀——既能和日志
// 里的 key_hash 对照确认「是不是我以为的那把 key」，又不回明文。
// devin.proxy 允许 http://user:pass@host 形式，userinfo 同样是凭据：
// 清掉整段 User 保留 host，排障仍能辨认代理指向。
func redactSecrets(fields map[string]any) {
	for _, path := range [][2]string{{"auth", "api_key"}, {"dashboard", "password"}} {
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
		// 账号池逐条脱敏：accounts[].token/api_key 与 devin.token 同
		// 规则 sha256 前缀——漏遮任一号都是凭据泄露。
		if accounts, ok := devin["accounts"].([]any); ok {
			for _, entry := range accounts {
				account, ok := entry.(map[string]any)
				if !ok {
					continue
				}
				for _, key := range []string{"token", "api_key"} {
					raw, ok := account[key].(string)
					if !ok || raw == "" {
						continue
					}
					sum := sha256.Sum256([]byte(raw))
					account[key] = fmt.Sprintf("sha256:%x", sum[:6])
				}
			}
		}
	}
}

// fileMtime 返回配置文件的最后修改时刻；stat 失败回零值。
func fileMtime(path string) time.Time {
	if info, err := os.Stat(path); err == nil {
		return info.ModTime()
	}
	return time.Time{}
}
