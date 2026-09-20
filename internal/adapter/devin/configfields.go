// 本文件是可热应用配置字段的登记表：finishConfigApply 的 applied 差集、
// 面板 settings 的 devin.* 键（取值/解析/回灌）与端点重建判定共用同一份
// 词表——新增可调字段只在此登记一行，三处消费者自动跟上。
package devin

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/config"
)

// ConfigField 登记一个可热应用的 devin.* 配置字段。Key 一词三用：
// applied 名单词、config.yaml 点分路径、面板 path（三者本来就同词）。
// Get/Set 是面板侧入口：Get 读生效值的面板展示串（gate/warm 经
// Normalize* 归一化后取），Set 把面板字符串解析校验后写进克隆配置；
// 两者为空的字段不进面板（凭据键刻意不登记）。
type ConfigField struct {
	Key string
	Get func(Config) string
	Set func(*Config, string) error
	// AffectsLink 标记端点三件套：变化触发上游调用束整体重建。
	AffectsLink bool
	// changed 比 prev→next 原始存储值（非归一化展示值），不同进 applied。
	changed func(prev, next Config) bool
	// keyFor 渲染逐 lane 动态键（devin.accounts.<name>.*）；nil 用 Key。
	keyFor func(Config) string
	// onChange 是换值后的 adapter 侧回写（凭据服役槽/minted 作废）；
	// 大多数字段无——读侧每次经 CurrentConfig 现读新值。
	onChange func(a *Adapter, next Config)
}

// appliedKey 返回字段在某份配置下的 applied 名单词。
func (f *ConfigField) appliedKey(c Config) string {
	if f.keyFor != nil {
		return f.keyFor(c)
	}
	return f.Key
}

// configFields 是全部热应用字段的登记表，顺序即 applied 名单的发出顺序。
var configFields = []ConfigField{
	{
		Key:     "devin.model",
		changed: func(p, n Config) bool { return p.Model != n.Model },
		Get:     func(c Config) string { return c.Model },
		Set:     setString(func(c *Config) *string { return &c.Model }, requireNonEmpty("devin_model")),
	},
	{
		Key:     "devin.aliases",
		changed: func(p, n Config) bool { return !maps.Equal(p.Aliases, n.Aliases) },
		Get:     func(c Config) string { return jsonString(c.Aliases, "{}") },
		Set: func(c *Config, v string) error {
			var aliases map[string]string
			if strings.TrimSpace(v) != "" {
				if err := json.Unmarshal([]byte(v), &aliases); err != nil {
					return fmt.Errorf("aliases must be a JSON object of string→string: %w", err)
				}
			}
			normalized, err := config.NormalizeAliases(aliases)
			if err != nil {
				return err
			}
			if len(normalized) == 0 {
				normalized = nil
			}
			c.Aliases = normalized
			return nil
		},
	},
	{
		Key:     "devin.client_name",
		changed: func(p, n Config) bool { return p.ClientName != n.ClientName },
		Get:     func(c Config) string { n, _, _ := c.ClientIdentity(); return n },
		Set:     setString(func(c *Config) *string { return &c.ClientName }, nil),
	},
	{
		Key:     "devin.client_version",
		changed: func(p, n Config) bool { return p.ClientVersion != n.ClientVersion },
		Get:     func(c Config) string { _, v, _ := c.ClientIdentity(); return v },
		Set:     setString(func(c *Config) *string { return &c.ClientVersion }, nil),
	},
	{
		Key:     "devin.client_os",
		changed: func(p, n Config) bool { return p.ClientOS != n.ClientOS },
		Get:     func(c Config) string { _, _, o := c.ClientIdentity(); return o },
		Set:     setString(func(c *Config) *string { return &c.ClientOS }, nil),
	},
	{
		// token/api_key 是逐 lane 动态键（devin.accounts.<name>.*）：
		// 凭据键刻意不进面板登记表（Get/Set 留空即不被
		// ConfigFieldByKey 命中）。
		changed: func(p, n Config) bool { return p.Identity.Token != n.Identity.Token },
		keyFor:  func(c Config) string { return "devin.accounts." + c.Identity.Name + ".token" },
		onChange: func(a *Adapter, n Config) {
			a.tokenMu.Lock()
			a.token = n.Identity.Token
			// 声明凭据换值即夺回服役位：minted 是按旧声明铸出的，
			// 留下会让新 token 永不服役。
			a.minted = ""
			a.tokenMu.Unlock()
		},
	},
	{
		changed: func(p, n Config) bool { return p.Identity.APIKey != n.Identity.APIKey },
		keyFor:  func(c Config) string { return "devin.accounts." + c.Identity.Name + ".api_key" },
		onChange: func(a *Adapter, _ Config) {
			// mint key 换值意味着铸币身份可能换号——旧 key 铸出的
			// minted 一并作废，下一次需要时按新 key 重铸。
			a.tokenMu.Lock()
			a.minted = ""
			a.tokenMu.Unlock()
		},
	},
	{
		Key:     "devin.max_rpm",
		changed: func(p, n Config) bool { return p.Gate.MaxRPM != n.Gate.MaxRPM },
		Get:     func(c Config) string { return strconv.Itoa(NormalizeGateConfig(c.Gate).MaxRPM) },
		Set:     setInt(func(c *Config) *int { return &c.Gate.MaxRPM }),
	},
	{
		Key:     "devin.gate_max_hold_seconds",
		changed: func(p, n Config) bool { return p.Gate.MaxHold != n.Gate.MaxHold },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeGateConfig(c.Gate).MaxHold }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Gate.MaxHold }),
	},
	{
		Key:     "devin.gate_drip_interval_seconds",
		changed: func(p, n Config) bool { return p.Gate.DripInterval != n.Gate.DripInterval },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeGateConfig(c.Gate).DripInterval }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Gate.DripInterval }),
	},
	{
		Key:     "devin.gate_default_latch_seconds",
		changed: func(p, n Config) bool { return p.Gate.DefaultLatch != n.Gate.DefaultLatch },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeGateConfig(c.Gate).DefaultLatch }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Gate.DefaultLatch }),
	},
	{
		Key:     "devin.gate_window_offset_seconds",
		changed: func(p, n Config) bool { return p.Gate.WindowOffset != n.Gate.WindowOffset },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeGateConfig(c.Gate).WindowOffset }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Gate.WindowOffset }),
	},
	{
		Key:     "devin.gate_window_guard_seconds",
		changed: func(p, n Config) bool { return p.Gate.WindowGuard != n.Gate.WindowGuard },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeGateConfig(c.Gate).WindowGuard }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Gate.WindowGuard }),
	},
	{
		Key:     "devin.gate_bg_max_hold_seconds",
		changed: func(p, n Config) bool { return p.Gate.BgMaxHold != n.Gate.BgMaxHold },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeGateConfig(c.Gate).BgMaxHold }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Gate.BgMaxHold }),
	},
	{
		Key:     "devin.gate_bg_reserve_margin",
		changed: func(p, n Config) bool { return p.Gate.BgReserveMargin != n.Gate.BgReserveMargin },
		Get:     func(c Config) string { return strconv.Itoa(NormalizeGateConfig(c.Gate).BgReserveMargin) },
		Set:     setInt(func(c *Config) *int { return &c.Gate.BgReserveMargin }),
	},
	{
		Key:     "devin.warm_prefix_enabled",
		changed: func(p, n Config) bool { return p.Warm.Enabled != n.Warm.Enabled },
		Get:     func(c Config) string { return strconv.FormatBool(NormalizeWarmConfig(c.Warm).Enabled) },
		Set:     setBool(func(c *Config) *bool { return &c.Warm.Enabled }),
	},
	{
		Key:     "devin.warm_prefix_interval_seconds",
		changed: func(p, n Config) bool { return p.Warm.Interval != n.Warm.Interval },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeWarmConfig(c.Warm).Interval }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Warm.Interval }),
	},
	{
		Key:     "devin.warm_prefix_jitter_ratio",
		changed: func(p, n Config) bool { return p.Warm.JitterRatio != n.Warm.JitterRatio },
		Get: func(c Config) string {
			return strconv.FormatFloat(NormalizeWarmConfig(c.Warm).JitterRatio, 'g', -1, 64)
		},
		Set: func(c *Config, v string) error {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return fmt.Errorf("value must be a number: %w", err)
			}
			// !(f<1) 一并拦 NaN/+Inf：NaN 存进去下游 normalize 的
			// <=0||>=1 比较全 false 会漏过，time.Duration(NaN) 是垃圾值。
			if !(f < 1) {
				return errors.New("warm_prefix_jitter_ratio must be < 1 (<=0 resets to default 0.15)")
			}
			c.Warm.JitterRatio = f
			return nil
		},
	},
	{
		Key:     "devin.warm_prefix_max_streams",
		changed: func(p, n Config) bool { return p.Warm.MaxStreams != n.Warm.MaxStreams },
		Get:     func(c Config) string { return strconv.Itoa(NormalizeWarmConfig(c.Warm).MaxStreams) },
		Set:     setInt(func(c *Config) *int { return &c.Warm.MaxStreams }),
	},
	{
		Key:     "devin.warm_prefix_max_retained_mb",
		changed: func(p, n Config) bool { return p.Warm.MaxRetainedMB != n.Warm.MaxRetainedMB },
		Get:     func(c Config) string { return strconv.FormatInt(NormalizeWarmConfig(c.Warm).MaxRetainedMB, 10) },
		Set: func(c *Config, v string) error {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return fmt.Errorf("value must be an integer: %w", err)
			}
			c.Warm.MaxRetainedMB = n
			return nil
		},
	},
	{
		Key:     "devin.warm_prefix_min_prefix_tokens",
		changed: func(p, n Config) bool { return p.Warm.MinPrefixTokens != n.Warm.MinPrefixTokens },
		Get:     func(c Config) string { return strconv.Itoa(NormalizeWarmConfig(c.Warm).MinPrefixTokens) },
		Set:     setInt(func(c *Config) *int { return &c.Warm.MinPrefixTokens }),
	},
	{
		Key:     "devin.warm_prefix_blocked_max_idle_seconds",
		changed: func(p, n Config) bool { return p.Warm.BlockedMaxIdle != n.Warm.BlockedMaxIdle },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeWarmConfig(c.Warm).BlockedMaxIdle }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Warm.BlockedMaxIdle }),
	},
	{
		Key:     "devin.warm_prefix_userpaced_max_idle_seconds",
		changed: func(p, n Config) bool { return p.Warm.UserPacedMaxIdle != n.Warm.UserPacedMaxIdle },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeWarmConfig(c.Warm).UserPacedMaxIdle }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Warm.UserPacedMaxIdle }),
	},
	{
		Key:     "devin.warm_prefix_subdone_max_idle_seconds",
		changed: func(p, n Config) bool { return p.Warm.SubDoneMaxIdle != n.Warm.SubDoneMaxIdle },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeWarmConfig(c.Warm).SubDoneMaxIdle }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Warm.SubDoneMaxIdle }),
	},
	{
		Key:     "devin.warm_prefix_unknown_max_idle_seconds",
		changed: func(p, n Config) bool { return p.Warm.UnknownMaxIdle != n.Warm.UnknownMaxIdle },
		Get:     secondsOf(func(c Config) time.Duration { return NormalizeWarmConfig(c.Warm).UnknownMaxIdle }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.Warm.UnknownMaxIdle }),
	},
	{
		Key:     "devin.warm_prefix_blocked_names",
		changed: func(p, n Config) bool { return !slices.Equal(p.Warm.BlockedNames, n.Warm.BlockedNames) },
		Get:     func(c Config) string { return jsonString(NormalizeWarmConfig(c.Warm).BlockedNames, "[]") },
		Set:     setNames(func(c *Config) *[]string { return &c.Warm.BlockedNames }),
	},
	{
		Key:     "devin.warm_prefix_userpaced_names",
		changed: func(p, n Config) bool { return !slices.Equal(p.Warm.UserPacedNames, n.Warm.UserPacedNames) },
		Get:     func(c Config) string { return jsonString(NormalizeWarmConfig(c.Warm).UserPacedNames, "[]") },
		Set:     setNames(func(c *Config) *[]string { return &c.Warm.UserPacedNames }),
	},
	{
		Key:         "devin.base_url",
		changed:     func(p, n Config) bool { return p.Endpoint.BaseURL != n.Endpoint.BaseURL },
		Get:         func(c Config) string { return c.Endpoint.BaseURL },
		Set:         setString(func(c *Config) *string { return &c.Endpoint.BaseURL }, requireAbsoluteURL("devin_base_url")),
		AffectsLink: true,
	},
	{
		Key:         "devin.proxy",
		changed:     func(p, n Config) bool { return p.Endpoint.Proxy != n.Endpoint.Proxy },
		Get:         func(c Config) string { return c.Endpoint.Proxy },
		Set:         setString(func(c *Config) *string { return &c.Endpoint.Proxy }, nil),
		AffectsLink: true,
	},
	{
		Key:         "devin.force_http1",
		changed:     func(p, n Config) bool { return p.Endpoint.ForceHTTP1 != n.Endpoint.ForceHTTP1 },
		Get:         func(c Config) string { return strconv.FormatBool(c.Endpoint.ForceHTTP1) },
		Set:         setBool(func(c *Config) *bool { return &c.Endpoint.ForceHTTP1 }),
		AffectsLink: true,
	},
	{
		Key:     "devin.session_affinity_ttl_seconds",
		changed: func(p, n Config) bool { return p.SessionAffinityTTLSeconds != n.SessionAffinityTTLSeconds },
		Get:     func(c Config) string { return strconv.Itoa(c.SessionAffinityTTLSeconds) },
		Set:     setInt(func(c *Config) *int { return &c.SessionAffinityTTLSeconds }),
	},
	{
		Key:     "devin.quota_low_threshold_percent",
		changed: func(p, n Config) bool { return p.QuotaLowThresholdPercent != n.QuotaLowThresholdPercent },
		Get:     func(c Config) string { return strconv.Itoa(c.QuotaLowThresholdPercent) },
		Set:     setInt(func(c *Config) *int { return &c.QuotaLowThresholdPercent }),
	},
	{
		Key:     "devin.no_progress_timeout_seconds",
		changed: func(p, n Config) bool { return p.NoProgressTimeout != n.NoProgressTimeout },
		Get:     secondsOf(func(c Config) time.Duration { return c.NoProgressTimeout }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.NoProgressTimeout }),
	},
	{
		Key:     "devin.pre_event_no_progress_timeout_seconds",
		changed: func(p, n Config) bool { return p.PreEventNoProgressTimeout != n.PreEventNoProgressTimeout },
		Get:     secondsOf(func(c Config) time.Duration { return c.PreEventNoProgressTimeout }),
		Set:     setSeconds(func(c *Config) *time.Duration { return &c.PreEventNoProgressTimeout }),
	},
}

// linkFieldKeys 是 AffectsLink 字段的静态键集（端点三件套）。
var linkFieldKeys = func() map[string]bool {
	m := make(map[string]bool)
	for i := range configFields {
		if configFields[i].AffectsLink {
			m[configFields[i].Key] = true
		}
	}
	return m
}()

// AppliedAffectsLink 报告 applied 名单是否含端点三件套词：这些字段变化
// 时 adapter 已重建上游调用束，reload 路径据此决定面板等共享同一上游
// 端点的组件是否也要换绑。
func AppliedAffectsLink(applied []string) bool {
	for _, key := range applied {
		if linkFieldKeys[key] {
			return true
		}
	}
	return false
}

// ConfigFieldByKey 按 devin.* 词查登记字段；凭据等刻意不暴露给面板的
// 键（Set 为空）返回 nil。面板用它派生 devin.* 键的 live/apply 闭包。
func ConfigFieldByKey(key string) *ConfigField {
	for i := range configFields {
		f := &configFields[i]
		if f.Key == key && f.Set != nil {
			return f
		}
	}
	return nil
}

// setString 生成字符串字段的 Set：trim 后可选校验再写入。
func setString(field func(*Config) *string, validate func(string) error) func(*Config, string) error {
	return func(c *Config, v string) error {
		v = strings.TrimSpace(v)
		if validate != nil {
			if err := validate(v); err != nil {
				return err
			}
		}
		*field(c) = v
		return nil
	}
}

// setBool 生成布尔字段的 Set。
func setBool(field func(*Config) *bool) func(*Config, string) error {
	return func(c *Config, v string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("value must be a boolean: %w", err)
		}
		*field(c) = b
		return nil
	}
}

// setInt 生成 int 字段的 Set：十进制整数直接写入。
func setInt(field func(*Config) *int) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("value must be an integer: %w", err)
		}
		*field(c) = n
		return nil
	}
}

// setSeconds 生成「秒→time.Duration」字段的 Set（gate/warm 时长参数
// 共用，<=0 语义由子系统归一化为默认值）。
func setSeconds(field func(*Config) *time.Duration) func(*Config, string) error {
	return func(c *Config, v string) error {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("value must be an integer (seconds): %w", err)
		}
		if int64(n) > math.MaxInt64/int64(time.Second) || int64(n) < math.MinInt64/int64(time.Second) {
			return fmt.Errorf("value overflows duration: %d", n)
		}
		*field(c) = time.Duration(n) * time.Second
		return nil
	}
}

// setNames 生成 []string 名表字段的 Set：JSON 数组，元素 trim 后必须
// 非空；空值/空数组归一成 nil（子系统回落内置默认表）。
func setNames(field func(*Config) *[]string) func(*Config, string) error {
	return func(c *Config, v string) error {
		var names []string
		if strings.TrimSpace(v) != "" {
			if err := json.Unmarshal([]byte(v), &names); err != nil {
				return fmt.Errorf("value must be a JSON array of strings: %w", err)
			}
		}
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				return errors.New("name list elements must be non-empty")
			}
		}
		if len(names) == 0 {
			names = nil
		}
		*field(c) = names
		return nil
	}
}

// requireNonEmpty 拦截空白输入：model/base_url 是上游必填项，空值会让
// 全部请求失败（reload 端点有同款校验）。
func requireNonEmpty(key string) func(string) error {
	return func(v string) error {
		if v == "" {
			return fmt.Errorf("%s must be non-empty", key)
		}
		return nil
	}
}

// requireAbsoluteURL 校验可解析的绝对 URL（scheme+host）。
func requireAbsoluteURL(key string) func(string) error {
	return func(v string) error {
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("%s must be an absolute URL", key)
		}
		return nil
	}
}

// secondsOf 把时间字段读成秒字符串。
func secondsOf(get func(Config) time.Duration) func(Config) string {
	return func(c Config) string { return strconv.FormatInt(int64(get(c)/time.Second), 10) }
}

// jsonString 把名表/别名类字段序列化成 JSON 字符串；nil/空落成 [] 或 {}
// 的稳定展示形（不序列化成 null）。map 序列化键序确定（Go 排序）。
func jsonString(v any, empty string) string {
	data, err := json.Marshal(v)
	if err != nil || string(data) == "null" {
		return empty
	}
	return string(data)
}
