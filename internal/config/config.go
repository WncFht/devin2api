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

// DevinConfig 保存 Devin Connect 上游调用配置。
type DevinConfig struct {
	// BaseURL 是 Devin Connect 服务的基础地址。
	BaseURL string `yaml:"base_url"`
	// Token 是 Devin session token；不会写入日志。
	Token string `yaml:"token"`
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
}

// DebugConfig 保存请求级调试日志配置。
type DebugConfig struct {
	// Enabled 表示是否在配置文件同目录的 logs 下写入请求调试日志。
	Enabled bool `yaml:"enabled"`
	// RetentionDays 是请求日志目录的保留天数；<=0 不按时间清理。默认 14。
	RetentionDays *int `yaml:"retention_days"`
	// MaxTotalMB 是 logs 目录总量上限（MB），超限从最旧目录开始删；
	// <=0 不按大小清理。默认 1024。
	MaxTotalMB *int64 `yaml:"max_total_mb"`
	// PayloadHours 是大体积阶段文件（03/04/06 与 attachments/）的保留小时数，
	// 超时后剥离负载、保留证据文件；<=0 不剥离。默认 24。
	PayloadHours *int `yaml:"payload_hours"`
	// KeepErrorDirs 是容量淘汰时受保护的最新失败目录数；<=0 不保护。默认 32。
	KeepErrorDirs *int `yaml:"keep_error_dirs"`
	// QuotaIntervalMinutes 是配额快照采样间隔（分钟），写入 logs/quota.jsonl；
	// <=0 不采样。默认 5。
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
	// APIKey 是客户端访问 /v1/* 接口所需的密钥；为空时不启用鉴权。
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
	if err := config.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate config %q: %w", path, err)
	}
	return config, nil
}

// Validate 检查配置中的必填项，并设置默认值。
func (config *Config) Validate() error {
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
	aliases, err := normalizeAliases(config.Devin.Aliases)
	if err != nil {
		return err
	}
	config.Devin.Aliases = aliases
	// devin.token 为空时按优先级自动发现：环境变量 → Devin CLI 凭证文件。
	if strings.TrimSpace(config.Devin.Token) == "" {
		config.Devin.Token = ResolveDevinToken()
	}
	return nil
}

// normalizeAliases 归一化 devin.aliases：键与目标去空白，拒绝空键、
// 空目标、把 "*" 当目标用（"*" 只作兜底键）、trim 后重复键与仅大小写
// 不同的键（折叠匹配要求无歧义）；随后把链式映射展开成最终目标并检出
// 环（a→b、b→c 归一成 a→c、b→c；a→a 按环报错）。展开发生在加载期，
// 运行时按 精确 → 折叠 → "*" 顺序单跳查找即可。
func normalizeAliases(aliases map[string]string) (map[string]string, error) {
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

// ResolveStateDir 按优先级解析状态根目录（logs/、gate-state.json 等运行期
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
// credentials.toml（路径见 devinCredentialsPaths，随平台变化）。
// 找不到返回空串，由调用方决定是否报错。cmd/probe 等工具在 config.yaml
// 缺失时也走这条链。
func ResolveDevinToken() string {
	for _, name := range []string{"DEVIN_TOKEN", "WINDSURF_API_KEY"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	for _, path := range devinCredentialsPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if match := devinCredentialsTokenPattern.FindSubmatch(data); len(match) == 2 {
			return strings.TrimSpace(string(match[1]))
		}
	}
	return ""
}

// devinCredentialsPaths 返回 Devin CLI credentials.toml 的候选位置。
// Linux/macOS 上 CLI 遵循 XDG：数据目录为 $XDG_DATA_HOME，缺省
// ~/.local/share。Windows 上 CLI 不单发，由 Windsurf 桌面端（即
// Devin app）内置携带：resources/app/extensions/windsurf/devin/bin/devin.exe，
// `devin.exe auth login` 写 %APPDATA%\devin\credentials.toml（已实测）；
// %LOCALAPPDATA% 一并探测作兜底。
func devinCredentialsPaths() []string {
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
