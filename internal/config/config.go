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
	"strings"

	"gopkg.in/yaml.v3"
)

// Config 保存服务启动所需的全部配置；服务运行期间不会热更新。
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
	defer file.Close()

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
	// devin.token 为空时按优先级自动发现：环境变量 → Devin CLI 凭证文件。
	if strings.TrimSpace(config.Devin.Token) == "" {
		config.Devin.Token = resolveDevinToken()
	}
	return nil
}

// devinCredentialsTokenPattern 匹配 credentials.toml 中的 windsurf_api_key。
var devinCredentialsTokenPattern = regexp.MustCompile(`(?m)^\s*windsurf_api_key\s*=\s*"([^"]+)"`)

// resolveDevinToken 从本地 Devin 客户端状态中发现 session token。
// 依次尝试 DEVIN_TOKEN / WINDSURF_API_KEY 环境变量与
// ~/.local/share/devin/credentials.toml（Devin CLI 登录产物）。
// 找不到返回空串，由调用方决定是否报错。
func resolveDevinToken() string {
	for _, name := range []string{"DEVIN_TOKEN", "WINDSURF_API_KEY"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(home, ".local", "share", "devin", "credentials.toml"))
	if err != nil {
		return ""
	}
	match := devinCredentialsTokenPattern.FindSubmatch(data)
	if len(match) != 2 {
		return ""
	}
	return strings.TrimSpace(string(match[1]))
}
