// 本文件定义服务启动配置及其 YAML 加载和校验逻辑。
//
// Package config 负责加载和校验服务启动配置。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 是一次配置加载的快照。运行期变更不走本结构换值——各热应用
// 路径（adapter.ApplyConfig、app.SetAPIKey、panel.SetPassword 等）直接
// 改各自持有的字段，快照保留给配置自省与 reload 的变更比对。
type Config struct {
	// Server 保存 HTTP 服务配置。
	Server ServerConfig `yaml:"server"`
	// Devin 保存 Devin Connect 上游配置。
	Devin DevinConfig `yaml:"devin"`
	// Debug 保存仅用于本地诊断的日志配置。
	Debug DebugConfig `yaml:"debug"`
	// Dashboard 保存管理面板配置。
	Dashboard DashboardConfig `yaml:"dashboard"`
	// Auth 保存对外 OpenAI 兼容接口的访问控制配置。
	Auth AuthConfig `yaml:"auth"`
}

// ServerConfig 保存 HTTP 服务监听配置。
type ServerConfig struct {
	// Listen 是 HTTP 服务监听地址。
	Listen string `yaml:"listen"`
	// MaxConcurrency 是同时处理的 /v1/* 请求数上限；0 表示使用默认值。
	MaxConcurrency int `yaml:"max_concurrency"`
}

// DevinAccountConfig 是上游账号池的一个号：name 是它在日志、闸门状态
// 文件与面板里的身份；token 给字面量凭据，credentials_file 指向
// Devin CLI credentials.toml（解析 windsurf_api_key），两者至少给一个；
// 都给时 token 作初始值、credentials_file 作 unauthenticated 自愈来源。
type DevinAccountConfig struct {
	Name            string `yaml:"name"`
	Token           string `yaml:"token"`
	CredentialsFile string `yaml:"credentials_file"`
	// Priority 是池级排序元数据：值越大越优先被新会话选中，0 为默认档。
	Priority int `yaml:"priority"`
	// MaxRPM 覆盖该号自己的分钟窗口配额；0 表示继承 devin.max_rpm 全局值。
	MaxRPM int `yaml:"max_rpm"`
}

// devinAccountNamePattern 约束账号名字符集：名字要进闸门状态键
// gate:<name> 与日志字段，限定字母数字连字符下划线。
var devinAccountNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// DevinConfig 保存 Devin Connect 上游调用配置。
type DevinConfig struct {
	// BaseURL 是 Devin Connect 服务的基础地址。
	BaseURL string `yaml:"base_url"`
	// Token 是已删除的单号简写字段的占位空壳：yaml 键保留只为让
	// Validate 对残留 devin.token 的旧配置产出迁移错误——删掉字段
	// 会让 KnownFields(true) 把它报成未知字段，迁移指引被 parse 错误
	// 吞掉。取值永不生效。
	Token string `yaml:"token"`
	// Accounts 声明上游账号池，是唯一的凭据来源；空集即合法空池。
	// 自动发现链（env/CLI credentials.toml）不进 load 路径。
	Accounts []DevinAccountConfig `yaml:"accounts"`
	// Model 是 Devin chat model UID。
	Model string `yaml:"model"`
	// Proxy 是可选的 HTTP/HTTPS/SOCKS5 代理地址；为空时直连或走系统环境变量。
	Proxy string `yaml:"proxy"`
	// ForceHTTP1 为 true 时强制使用 HTTP/1.1，每请求独立 TCP 连接，
	// 避免 HTTP/2 单连接多 stream 复用导致的上游并发瓶颈（首字延迟飙升/卡住）。
	// 行为对齐 Devin 客户端多窗口各自独立连接的模式。默认 true。
	ForceHTTP1 *bool `yaml:"force_http1"`
	// Aliases 是客户端模型名到上游真实 UID 的映射，
	// 例如 "swe-2": "swe-2-max"、"glm-5.2": "glm-5-2"。
	Aliases map[string]string `yaml:"aliases"`
	// ClientName 是发给上游 metadata.extension_name/ide_name 的客户端名；
	// 默认 "chisel"（与真实 Devin CLI 抓包一致）。
	ClientName string `yaml:"client_name"`
	// ClientVersion 是 metadata.extension_version/ide_version 的版本号；
	// 默认与当前抓包版本一致。上游若给新模型加版本门，改这里即可，
	// 不必发版。
	ClientVersion string `yaml:"client_version"`
	// ClientOS 是 metadata.os；默认 "mac"。
	ClientOS string `yaml:"client_os"`
	// MaxRPM 是每个对齐分钟窗口内发往上游 GetChatMessage 的配额
	// （条/分钟）：上游限流器按自然分钟桶计数，本地窗口与估计桶界
	// 对齐、两侧留死区，使单桶可见计数不超此值；<=0 不限速。
	// 上游限流冷却闩（resource_exhausted 后按声明 reset 时刻本地拦停）
	// 不受此项影响，始终生效。
	MaxRPM int `yaml:"max_rpm"`
	// GateMaxHoldSeconds 是闸门内允许的最长排队等待秒数：闩外睡到
	// 下一窗口预计超过它时请求本地快速失败 429 + Retry-After；<=0 默认 15。
	GateMaxHoldSeconds int `yaml:"gate_max_hold_seconds"`
	// GateDripIntervalSeconds 是冷却闩内放行探针的间隔秒数：闩期间
	// 按此节奏逐条放到上游探测解闩，其余请求快速失败；<=0 默认 8。
	GateDripIntervalSeconds int `yaml:"gate_drip_interval_seconds"`
	// GateDefaultLatchSeconds 是上游 resource_exhausted 未携带 reset
	// hint 时的兜底闩时长秒数；<=0 默认 60。
	GateDefaultLatchSeconds int `yaml:"gate_default_latch_seconds"`
	// GateWindowOffsetSeconds 是上游分钟桶界在本地分钟内的估计位置
	// （第几秒）：实测桶界在本地 :59~:00（上游时钟快 ~1s），默认 0。
	GateWindowOffsetSeconds int `yaml:"gate_window_offset_seconds"`
	// GateWindowGuardSeconds 是桶界两侧的停发死区秒数：覆盖桶界估计
	// 误差与多分片漂移，死区内请求睡到下一窗口；<=0 默认 2。
	GateWindowGuardSeconds int `yaml:"gate_window_guard_seconds"`
	// GateBgMaxHoldSeconds 是 bg 类请求的闸内排队预算秒数（fg 走
	// gate_max_hold_seconds）：无人值守负载等得起，给到两个窗口让
	// 批跑宁可排队也不快败空转；<=0 默认 120。
	GateBgMaxHoldSeconds int `yaml:"gate_bg_max_hold_seconds"`
	// GateBgReserveMargin 是 bg 准入预留公式的固定安全边际（条）：
	// 叠加在 fg 速率 EMA 外推与 fg 排队数之上，吸收估计滞后与小
	// 并发突发；<=0 默认 4。
	GateBgReserveMargin int `yaml:"gate_bg_reserve_margin"`
	// WarmPrefixEnabled 是前缀保温总开关：为 true 时对保留的会话谱系
	// 按节拍重放最近请求体，给上游 prompt cache 续期，压住 subagent
	// 等待结束后的冷 prefill。默认 false（灰度开关）；热重载生效，
	// 关闭立即停掉保温调度。
	WarmPrefixEnabled bool `yaml:"warm_prefix_enabled"`
	// WarmPrefixIntervalSeconds 是每条保留谱系的 ping 节拍秒数：必须
	// 明显低于上游滑动 TTL（标称 ~780s、实测有效 ~690s）让条目不死
	// ——ping 只能续命不能复活；<=0 默认 180。
	WarmPrefixIntervalSeconds int `yaml:"warm_prefix_interval_seconds"`
	// WarmPrefixJitterRatio 是节拍抖动幅度（±比例）：防止同批静默的
	// 谱系同刻齐射打满上游分钟桶；<=0 默认 0.15。
	WarmPrefixJitterRatio float64 `yaml:"warm_prefix_jitter_ratio"`
	// WarmPrefixMaxStreams 是同时保留保温的谱系数上限；<=0 默认 256。
	WarmPrefixMaxStreams int `yaml:"warm_prefix_max_streams"`
	// WarmPrefixMaxRetainedMB 是保留请求体的总字节上限（MB）：超限按
	// LRU 驱逐、可疑条目先挤；<=0 默认 96。
	WarmPrefixMaxRetainedMB int64 `yaml:"warm_prefix_max_retained_mb"`
	// WarmPrefixMinPrefixTokens 是谱系可保温的最低前缀 token 数（观测
	// 或估计值）：低于它冷 prefill 足够便宜（~48ms/1K tok），不值得
	// 烧 RPM；<=0 默认 8192。
	WarmPrefixMinPrefixTokens int `yaml:"warm_prefix_min_prefix_tokens"`
	// WarmPrefixBlockedMaxIdleSeconds 是存在阻塞型 pending 工具调用
	// （agent/task/wait 类）的谱系最长保温空闲秒数；<=0 默认
	// 14400（4h）。
	WarmPrefixBlockedMaxIdleSeconds int `yaml:"warm_prefix_blocked_max_idle_seconds"`
	// WarmPrefixUserPacedMaxIdleSeconds 是无 pending 或仅剩用户节奏
	// pending（AskUserQuestion/ExitPlanMode/request_user_input）的
	// 谱系最长保温空闲秒数；<=0 默认 2700（45min）。
	WarmPrefixUserPacedMaxIdleSeconds int `yaml:"warm_prefix_userpaced_max_idle_seconds"`
	// WarmPrefixSubDoneMaxIdleSeconds 是已完成 subagent（带 sub 标记
	// 且无 pending）的谱系最长保温空闲秒数，覆盖 SendMessage/agentId
	// 复活长尾；<=0 默认 600（10min）。
	WarmPrefixSubDoneMaxIdleSeconds int `yaml:"warm_prefix_subdone_max_idle_seconds"`
	// WarmPrefixUnknownMaxIdleSeconds 是无法分类的流（无会话标记、
	// 非 CC/codex 客户端）的最长保温空闲秒数兜底；<=0 默认 1800。
	WarmPrefixUnknownMaxIdleSeconds int `yaml:"warm_prefix_unknown_max_idle_seconds"`
	// WarmPrefixBlockedNames 是把 pending 工具调用判为阻塞型的名字集；
	// 空列表回退内置默认 {Agent, Task, Workflow, wait_agent}。
	WarmPrefixBlockedNames []string `yaml:"warm_prefix_blocked_names"`
	// WarmPrefixUserPacedNames 是把 pending 判为用户节奏的名字集；
	// 空列表回退内置默认
	// {AskUserQuestion, ExitPlanMode, request_user_input}。
	WarmPrefixUserPacedNames []string `yaml:"warm_prefix_userpaced_names"`
	// SessionAffinityTTLSeconds 是会话绑定的滑动 TTL 秒数：命中即续期；
	// <=0 回落默认 3600。全局字段，各 lane 一致。
	SessionAffinityTTLSeconds int `yaml:"session_affinity_ttl_seconds"`
	// QuotaLowThresholdPercent 是配额降权阈值：weekly 剩余百分比低于它
	// 时该 lane 对新会话降档（已绑定会话不受影响）；<=0 回落默认 15，
	// 负值关闭降权。
	QuotaLowThresholdPercent int `yaml:"quota_low_threshold_percent"`
	// NoProgressTimeoutSeconds 是「产出过内容之后」的上游无进度期限
	// 秒数：上游在工具调用参数阶段可静默计算 15-25min 只发心跳帧，
	// 该档必须盖住它（产出前沿用内置 10min 档）；<=0 回落默认 2700。
	// 全局字段，各 lane 一致。
	NoProgressTimeoutSeconds int `yaml:"no_progress_timeout_seconds"`
}

// DebugConfig 保存请求级调试日志配置。
type DebugConfig struct {
	// Enabled 表示是否写入请求调试记录（debug_files/debug_chunks 表）。
	Enabled bool `yaml:"enabled"`
	// RetentionDays 是按 dir 归组的调试记录保留天数；<=0 不按时间清理。默认 14。
	RetentionDays *int `yaml:"retention_days"`
	// MaxTotalMB 是调试 payload 总量上限（MB），超限从最旧 dir 开始删；
	// <=0 不按大小清理。默认 1024。
	MaxTotalMB *int64 `yaml:"max_total_mb"`
	// PayloadHours 是大体积阶段记录（03/04/06 与 attachments/）的保留小时数，
	// 超时后剥离负载、保留证据记录；<=0 不剥离。默认 24。
	PayloadHours *int `yaml:"payload_hours"`
	// KeepErrorDirs 是容量淘汰时受保护的最新失败 dir 数；<=0 不保护。默认 32。
	KeepErrorDirs *int `yaml:"keep_error_dirs"`
	// ErrorsOnly 只保留失败请求的调试 payload：干净完成（completed 且
	// 无 premature_end_turn 标记）的请求在完结时整删目录行，logs 摘要行
	// 照常落库——大流量部署下调试库体积收敛到故障面。默认 false。
	ErrorsOnly bool `yaml:"errors_only"`
	// QuotaIntervalMinutes 是配额快照采样间隔（分钟），写入 quota_samples
	// 表；<=0 不采样。默认 5。
	QuotaIntervalMinutes *int `yaml:"quota_interval_minutes"`
	// PprofListen 是 pprof/fgprof 剖析端点的独立监听地址（如
	// "127.0.0.1:6060"）；空值不启用。端点无鉴权——应只绑回环地址，
	// 跨机访问经 ssh 端口转发；开启时同时启用 block/mutex 剖析采样。
	PprofListen string `yaml:"pprof_listen"`
}

// DashboardConfig 保存管理面板配置。
type DashboardConfig struct {
	// Password 是面板访问密码；为空则不要求登录，直接进入面板。
	Password string `yaml:"password"`
}

// AuthConfig 保存对外 OpenAI 兼容接口的访问控制配置。
type AuthConfig struct {
	// APIKey 是下游令牌的播种源而非准入旁路：启动与 reload 时若仓内
	// 没有对应哈希行，它被写成一条普通 auth token（描述
	// "config: auth.api_key"）；留空则不再补种。
	APIKey string `yaml:"api_key"`
}

// Load 从 YAML 文件读取并校验配置。
func Load(path string) (Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open config %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	var config Config
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	if err := config.Validate(filepath.Dir(path)); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}
	return config, nil
}

// Validate 检查配置中的必填项，并设置默认值。configDir 是配置文件所在
// 目录：accounts 的相对 credentials_file 以它为锚（launchd 下 CWD=/，
// 相对路径不能以进程 CWD 解析）。
func (config *Config) Validate(configDir string) error {
	if config.Server.Listen == "" {
		return errors.New("server.listen is required")
	}
	if config.Server.MaxConcurrency <= 0 {
		config.Server.MaxConcurrency = 1024
	}
	// ForceHTTP1 默认开启：HTTP/2 单连接多 stream 复用是并发首字延迟飙升的根因。
	if config.Devin.ForceHTTP1 == nil {
		force := true
		config.Devin.ForceHTTP1 = &force
	}
	// 调试日志默认保留 14 天、总量 1GB，防止磁盘被静默打满。
	if config.Debug.RetentionDays == nil {
		days := 14
		config.Debug.RetentionDays = &days
	}
	if config.Debug.MaxTotalMB == nil {
		mb := int64(1024)
		config.Debug.MaxTotalMB = &mb
	}
	if config.Debug.PayloadHours == nil {
		hours := 24
		config.Debug.PayloadHours = &hours
	}
	if config.Debug.KeepErrorDirs == nil {
		keep := 32
		config.Debug.KeepErrorDirs = &keep
	}
	if config.Debug.QuotaIntervalMinutes == nil {
		minutes := 5
		config.Debug.QuotaIntervalMinutes = &minutes
	}
	aliases, err := NormalizeAliases(config.Devin.Aliases)
	if err != nil {
		return err
	}
	config.Devin.Aliases = aliases
	if err := config.Devin.resolveAccounts(configDir); err != nil {
		return err
	}
	return nil
}

// resolveAccounts 校验并落实账号池声明：每个账号必须带合法且唯一的
// name、至少一种凭据来源；credentials_file 在加载期就必须能解出 key
// （路径笔误不该静默产出一个死 lane）。同一有效 token 或同一
// credentials_file 被两个条目引用等于同一账号进池两次——限流簿记会
// 各自按满额计数、合并超发，按配置错误拒绝。
// devin.token 已删除：残留非空值报迁移错误。空 accounts 即空生效集
// （合法空池）；自动发现链不进 load 路径，凭据来源只剩各条目自己的
// token/credentials_file。
func (devin *DevinConfig) resolveAccounts(configDir string) error {
	if strings.TrimSpace(devin.Token) != "" {
		return errors.New(`devin.token removed; declare devin.accounts: [{name, token|credentials_file}] (e.g. [{name: main, token: "<session-token>"}])`)
	}
	seenNames := make(map[string]bool, len(devin.Accounts))
	seenTokens := make(map[string]string, len(devin.Accounts))
	seenFiles := make(map[string]string, len(devin.Accounts))
	for index := range devin.Accounts {
		account := &devin.Accounts[index]
		account.Name = strings.TrimSpace(account.Name)
		if !devinAccountNamePattern.MatchString(account.Name) {
			return fmt.Errorf("devin.accounts[%d]: name must match %s", index, devinAccountNamePattern)
		}
		if seenNames[account.Name] {
			return fmt.Errorf("devin.accounts[%d]: duplicate name %q", index, account.Name)
		}
		seenNames[account.Name] = true
		if account.Priority < 0 {
			return fmt.Errorf("devin.accounts[%d]: priority must be >= 0", index)
		}
		if account.MaxRPM < 0 {
			return fmt.Errorf("devin.accounts[%d]: max_rpm must be >= 0", index)
		}
		account.Token = strings.TrimSpace(account.Token)
		account.CredentialsFile = strings.TrimSpace(account.CredentialsFile)
		if account.CredentialsFile != "" {
			// 锚定到配置文件目录：launchd 下 CWD=/，相对路径按进程
			// CWD 解析必死；~/ 展开顺手做掉（用户自然写法）。
			path := expandHomeDir(account.CredentialsFile)
			if !filepath.IsAbs(path) {
				path = filepath.Join(configDir, path)
			}
			account.CredentialsFile = filepath.Clean(path)
			if prior, dup := seenFiles[account.CredentialsFile]; dup {
				return fmt.Errorf("devin.accounts[%d]: credentials_file already used by account %q", index, prior)
			}
			seenFiles[account.CredentialsFile] = account.Name
			resolved, err := readCredentialsFile(account.CredentialsFile)
			if err != nil {
				return fmt.Errorf("devin.accounts[%d]: credentials_file %q: %w", index, account.CredentialsFile, err)
			}
			if account.Token == "" {
				account.Token = resolved
			}
		}
		if account.Token == "" {
			return fmt.Errorf("devin.accounts[%d]: one of token/credentials_file is required", index)
		}
		if prior, dup := seenTokens[account.Token]; dup {
			return fmt.Errorf("devin.accounts[%d]: token duplicates account %q", index, prior)
		}
		seenTokens[account.Token] = account.Name
	}
	return nil
}

// ResolveAccounts 对一整份账号集跑整表校验（名正则/重名/凭据至少
// 其一/重 token/重 credentials_file/文件可解 key），返回锚定好
// credentials_file、文件型条目 token 已补成文件内容的副本；入参不动。
// 空集合法（合法空池），"default" 是普通账号名。API 干跑与 reload
// 整表校验共用同一出口，不留平行校验。
func ResolveAccounts(accounts []DevinAccountConfig, configDir string) ([]DevinAccountConfig, error) {
	synthetic := &DevinConfig{Accounts: append([]DevinAccountConfig(nil), accounts...)}
	if err := synthetic.resolveAccounts(configDir); err != nil {
		return nil, err
	}
	return synthetic.Accounts, nil
}

// expandHomeDir 展开路径开头的 ~/（Go 不做 shell 式 ~ 展开，配置里
// 写 ~/.local/share/... 是 Devin CLI 凭证文件的自然写法）。
func expandHomeDir(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// NormalizeAliases 归一化 devin.aliases：键与目标去空白，拒绝空键、
// 空目标、把 "*" 当目标用（"*" 只作兜底键）、trim 后重复键与仅大小写
// 不同的键（折叠匹配要求无歧义）；随后把链式映射展开成最终目标并检出
// 环（a→b、b→c 归一成 a→c、b→c；a→a 按环报错）。展开发生在加载期，
// 运行时按 精确 → 折叠 → "*" 顺序单跳查找即可。导出供面板设置页
// （/admin/settings 的 devin_aliases 键）复用同一套校验。
func NormalizeAliases(aliases map[string]string) (map[string]string, error) {
	if len(aliases) == 0 {
		return aliases, nil
	}
	normalized := make(map[string]string, len(aliases))
	folded := make(map[string]string, len(aliases))
	for key, target := range aliases {
		key = strings.TrimSpace(key)
		target = strings.TrimSpace(target)
		if key == "" {
			return nil, errors.New(`devin.aliases contains an empty key`)
		}
		if target == "" {
			return nil, fmt.Errorf("devin.aliases[%q] has an empty target", key)
		}
		if target == "*" {
			return nil, fmt.Errorf("devin.aliases[%q]: \"*\" is only valid as a catch-all key, not a target", key)
		}
		if prev, ok := normalized[key]; ok && prev != target {
			return nil, fmt.Errorf("devin.aliases: key %q maps to both %q and %q", key, prev, target)
		}
		if prev, ok := folded[strings.ToLower(key)]; ok && prev != key {
			return nil, fmt.Errorf("devin.aliases: keys %q and %q differ only by case", prev, key)
		}
		normalized[key] = target
		folded[strings.ToLower(key)] = key
	}
	for key := range normalized {
		seen := map[string]bool{key: true}
		target := normalized[key]
		for {
			next, ok := normalized[target]
			if !ok {
				break
			}
			if seen[target] {
				return nil, fmt.Errorf("devin.aliases: cycle detected through %q", key)
			}
			seen[target] = true
			target = next
		}
		normalized[key] = target
	}
	return normalized, nil
}

// ResolveConfigPath 按优先级解析配置文件路径：显式 -config flag →
// DEVIN2API_CONFIG 环境变量 → ./config.yaml（存在才选，仓库内开发便利，
// Windows zip 解压即跑同理）→ DefaultConfigPath 平台规范位置。
// 返回值可能指向不存在的文件——Load 的报错会带上该路径，指向规范安装位置。
func ResolveConfigPath(flagPath string) (string, error) {
	if flagPath != "" {
		return flagPath, nil
	}
	if env := strings.TrimSpace(os.Getenv("DEVIN2API_CONFIG")); env != "" {
		return env, nil
	}
	if _, err := os.Stat("config.yaml"); err == nil {
		return "config.yaml", nil
	}
	return DefaultConfigPath()
}

// ResolveStateDir 按优先级解析状态根目录（devin-2api.db、logs/ 等运行期
// 产物的归属）：-state-dir flag → DEVIN2API_STATE_DIR → DefaultStateDir。
func ResolveStateDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	if env := strings.TrimSpace(os.Getenv("DEVIN2API_STATE_DIR")); env != "" {
		return env, nil
	}
	return DefaultStateDir()
}

// DefaultConfigPath 返回平台规范的默认配置文件位置：Linux/Unix 为
// $XDG_CONFIG_HOME/devin-2api/config.yaml（os.UserConfigDir 已含 XDG 兜底），
// macOS 为 ~/Library/Application Support/devin-2api/config.yaml，
// Windows 为 %APPDATA%\devin-2api\config.yaml。
func DefaultConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config dir: %w", err)
	}
	return filepath.Join(dir, "devin-2api", "config.yaml"), nil
}

// DefaultStateDir 返回平台规范的状态/日志根目录：Linux/Unix 为
// $XDG_STATE_HOME/devin-2api（缺省 ~/.local/state/devin-2api，stdlib 无
// UserStateDir 故手写）；macOS 与配置同目录（Application Support 无独立
// state 惯例，维持 app 目录模型）；Windows 为 %LOCALAPPDATA%\devin-2api
// （os.UserCacheDir——日志/状态是 machine-local，不进漫游配置）。
func DefaultStateDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		dir, err := os.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolve user cache dir: %w", err)
		}
		return filepath.Join(dir, "devin-2api"), nil
	case "darwin":
		dir, err := os.UserConfigDir()
		if err != nil {
			return "", fmt.Errorf("resolve user config dir: %w", err)
		}
		return filepath.Join(dir, "devin-2api"), nil
	default:
		if dir := os.Getenv("XDG_STATE_HOME"); dir != "" {
			return filepath.Join(dir, "devin-2api"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home dir: %w", err)
		}
		return filepath.Join(home, ".local", "state", "devin-2api"), nil
	}
}

// devinCredentialsTokenPattern 匹配 credentials.toml 中的 windsurf_api_key。
var devinCredentialsTokenPattern = regexp.MustCompile(`(?m)^\s*windsurf_api_key\s*=\s*"([^"]+)"`)

// ResolveDevinToken 从本地 Devin 客户端状态中发现 session token。
// 依次尝试 DEVIN_TOKEN / WINDSURF_API_KEY 环境变量与 Devin CLI 登录产物
// credentials.toml（路径见 DevinCredentialsPaths，随平台变化）。
// 找不到返回空串，由调用方决定是否报错。
// 不进服务 load 路径（账号凭据只认 devin.accounts 声明）——消费方是
// /admin/accounts/cli-credentials 探针与 cmd/probe 等工具的兜底链。
func ResolveDevinToken() string {
	for _, name := range []string{"DEVIN_TOKEN", "WINDSURF_API_KEY"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	for _, path := range DevinCredentialsPaths() {
		if token := TokenFromCredentialsFile(path); token != "" {
			return token
		}
	}
	return ""
}

// TokenFromCredentialsFile 从一份 credentials.toml 解出 windsurf_api_key；
// 文件不可读或无该键返回空串。账号池 TokenSource 与自动发现链共用这一
// 解析（自愈重读场景下区分错误类型没有额外动作，故吞掉）。
func TokenFromCredentialsFile(path string) string {
	token, _ := readCredentialsFile(path)
	return token
}

// readCredentialsFile 同 TokenFromCredentialsFile 但保留错误区分：
// 加载期校验要把「文件不存在/不可读」与「文件在但没该键」分开报错。
func readCredentialsFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if token := TokenFromCredentialsContent(data); token != "" {
		return token, nil
	}
	return "", errors.New("no windsurf_api_key")
}

// TokenFromCredentialsContent 从 credentials.toml 的字节内容解出
// windsurf_api_key；无该键返回空串。与文件版共用同一解析——面板的
// credentials_content 粘贴上传在落盘前先过它校验。
func TokenFromCredentialsContent(data []byte) string {
	if match := devinCredentialsTokenPattern.FindSubmatch(data); len(match) == 2 {
		return strings.TrimSpace(string(match[1]))
	}
	return ""
}

// DevinCredentialsPaths 返回 Devin CLI credentials.toml 的候选位置。
// Linux/macOS 上 CLI 遵循 XDG：数据目录为 $XDG_DATA_HOME，缺省
// ~/.local/share。Windows 上 CLI 不单发，由 Windsurf 桌面端（即
// Devin app）内置携带：resources/app/extensions/windsurf/devin/bin/devin.exe，
// `devin.exe auth login` 写 %APPDATA%\devin\credentials.toml（已实测）；
// %LOCALAPPDATA% 一并探测作兜底。
func DevinCredentialsPaths() []string {
	var dirs []string
	if runtime.GOOS == "windows" {
		for _, env := range []string{"APPDATA", "LOCALAPPDATA"} {
			if dir := os.Getenv(env); dir != "" {
				dirs = append(dirs, dir)
			}
		}
	} else {
		dir := os.Getenv("XDG_DATA_HOME")
		if dir == "" {
			if home, err := os.UserHomeDir(); err == nil {
				dir = filepath.Join(home, ".local", "share")
			}
		}
		if dir != "" {
			dirs = append(dirs, dir)
		}
	}
	paths := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		paths = append(paths, filepath.Join(dir, "devin", "credentials.toml"))
	}
	return paths
}
