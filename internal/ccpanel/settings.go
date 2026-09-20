package ccpanel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/store"
)

// 本文件实现 /admin/settings 的运行时键仓：值以字符串存取（ccLoad
// SystemSetting 契约），覆盖项持久化到 settings 表。
// 只登记存在真实热更新路径的键；启动与 config reload 后重放覆盖——
// 面板设置对被覆盖键恒赢 config.yaml（ccLoad system_settings 同款语义：
// DB 持久层压过启动默认）。

// settingDef 描述一个暴露的运行时键。live 读取子系统实况值（applied 键
// 必须有）；apply 把新值热应用进子系统，nil 表示纯面板侧键——值只进
// 覆盖表，由前端读取生效（如 auto_refresh_interval_seconds）。
type settingDef struct {
	key string
	// path 是该键在 config.yaml 里的点分路径（reload applied 名单同词）；
	// 空表示 settings 表专属键，无文件字段可对拍（auto_refresh 等）。
	path  string
	typ   string // value_type: bool|int|float|string|json
	desc  string // i18n settings.desc.<key> 缺失时的兜底描述
	def   func() string
	live  func() string
	apply func(string) error
}

// SettingsDeps 把各运行时持有者接进键仓（main 装配期注入）：devin.*
// 键克隆 adapter 配置快照改单字段后整体回灌 ApplyConfig——与 config
// reload 同路径，端点字段变化由注入方负责同步面板自身的上游调用束。
type SettingsDeps struct {
	// Debug 提供日志开关与保留策略的热更入口。
	Debug *debuglog.Manager
	// DevinConfig 返回 adapter 当前生效配置快照。
	DevinConfig func() devin.Config
	// UpdateDevin 在 adapter configMu 下克隆当前配置交给回调改字段后整体
	// 提交（devin.Adapter.UpdateConfig）——克隆与提交之间插不进 reload，
	// 单字段热改不会把并发的 reload 整份配置顶回去。
	UpdateDevin func(func(*devin.Config) error) error
	// MaxConcurrency/SetMaxConcurrency 是 /v1 并发闸的读写（CAS 计数器）。
	MaxConcurrency    func() int
	SetMaxConcurrency func(int)
	// QuotaInterval/SetQuotaInterval 是配额采样周期的读写（ticker 重起）。
	QuotaInterval    func() time.Duration
	SetQuotaInterval func(time.Duration)
	// PprofListen/SetPprofListen 是 pprof 监听地址的读写；SetPprofListen
	// 在 bind 失败时返回错误（面板写入要显式成败，区别于启动/reload
	// 的非致命语义）。
	PprofListen    func() string
	SetPprofListen func(string) error
}

// SettingDefaults 是各键「文件值」快照：构造时按生效态采样，config
// reload 后由 ResampleDefaults 用新文件值重灌（覆盖值不进来）。def
// 与 reset 回落目标都读它——不重灌的话 reload 后 reset 会应用启动时
// 的旧默认，default_value 展示也过期。
type SettingDefaults struct {
	Devin           devin.Config
	MaxConcurrency  int
	QuotaInterval   time.Duration
	PprofListen     string
	DebugEnabled    bool
	DebugErrorsOnly bool
	Policy          debuglog.RetentionPolicy
}

// PanelSettings 管理 settings 表与键注册表。
type PanelSettings struct {
	// writeMu 串行化 set/reset/ApplyAll 的 apply→persist→提交整条线：
	// apply 可能很慢（UpdateDevin 会重建上游连接束、SetPprofListen 要
	// bind），把它放出 s.mu 之外后读侧 List/Get 不再被慢应用堵住；
	// 写-写仍需互斥，否则同键的并发 set/reset 会让「最后落库的覆盖」
	// 与「最后进子系统的值」分家。
	writeMu sync.Mutex
	// mu 只守 byKey/values/updated 的短临界区簿记。
	mu       sync.Mutex
	st       *store.Store
	defs     []settingDef
	byKey    map[string]*settingDef
	values   map[string]string               // 覆盖值；不设则按 live/def 取生效值
	updated  map[string]int64                // 各键最近覆盖时刻（unix 秒）
	defaults atomic.Pointer[SettingDefaults] // def/reset 只读，atomic 免锁
}

// NewPanelSettings 创建键仓并从 settings 表水合覆盖项；deps
// 提供全部热更入口（调用方须在 ccPanel/app 装配完成后构造，boot 采样
// 的默认值反映 config.yaml 生效态）。构造时采样 config 派生状态作默认值。
func NewPanelSettings(st *store.Store, deps SettingsDeps) (*PanelSettings, error) {
	s := &PanelSettings{
		st:      st,
		byKey:   map[string]*settingDef{},
		values:  map[string]string{},
		updated: map[string]int64{},
	}
	s.defaults.Store(&SettingDefaults{
		Devin:           deps.DevinConfig(),
		MaxConcurrency:  deps.MaxConcurrency(),
		QuotaInterval:   deps.QuotaInterval(),
		PprofListen:     deps.PprofListen(),
		DebugEnabled:    deps.Debug.Enabled(),
		DebugErrorsOnly: deps.Debug.ErrorsOnly(),
		Policy:          deps.Debug.Policy(),
	})
	s.defs = s.buildSettingDefs(deps)
	for i := range s.defs {
		s.byKey[s.defs[i].key] = &s.defs[i]
	}
	values, updated, err := st.ListSettings(context.Background())
	if err != nil {
		return nil, err
	}
	for k, v := range values {
		if _, ok := s.byKey[k]; ok {
			s.values[k] = v
			s.updated[k] = updated[k]
		}
	}
	return s, nil
}

// setPolicyField 生成「改 RetentionPolicy 一个字段」的 apply：取快照、
// 改字段、整体写回（SetPolicy 是原子换值）。
func setPolicyField(debug *debuglog.Manager, mutate func(*debuglog.RetentionPolicy, int)) func(string) error {
	return func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("value must be an integer: %w", err)
		}
		p := debug.Policy()
		mutate(&p, n)
		debug.SetPolicy(p)
		return nil
	}
}

// devinField 生成「adapter 内克隆当前配置→改单字段→整体提交」的 apply：
// 与 config reload 同路径，端点三件套变化时内部重建调用束；mutate 内的
// 解析错误在提交前拦截。
func devinField(deps SettingsDeps, mutate func(*devin.Config, string) error) func(string) error {
	return func(v string) error {
		return deps.UpdateDevin(func(c *devin.Config) error { return mutate(c, v) })
	}
}

// devinLive 生成读 devin.Config 快照单字段的 live 取值。
func devinLive(deps SettingsDeps, get func(devin.Config) string) func() string {
	return func() string { return get(deps.DevinConfig()) }
}

// devinSettingDef 由 devin 包字段登记表（devin.ConfigFieldByKey）派生一个
// devin.* 面板键：path/def/live/apply 全部来自表内登记项，面板侧只携带
// key/typ/desc 三元组。面板键词与 devin.* path 词的差仅是 devin_ 前缀；
// 表内查不到是两侧词表漂移的编程错误，直接 panic。
func devinSettingDef(deps SettingsDeps, d0 func() SettingDefaults, key, typ, desc string) settingDef {
	f := devin.ConfigFieldByKey("devin." + strings.TrimPrefix(key, "devin_"))
	if f == nil {
		panic("devin config field not registered for panel key " + key)
	}
	return settingDef{
		key:   key,
		path:  f.Key,
		typ:   typ,
		desc:  desc,
		def:   func() string { return f.Get(d0().Devin) },
		live:  devinLive(deps, f.Get),
		apply: devinField(deps, f.Set),
	}
}

// buildSettingDefs 注册全部可暴露键。凡有真实热应用通道的 config.yaml
// 字段都在册：devin.* 经 ApplyConfig 克隆回灌，server.max_concurrency
// 走 CAS 计数器，debug.* 走 debuglog/配额 ticker/pprof 换绑；冷键
// （server.listen）与凭据键（devin.token/dashboard.password）
// 刻意不登记。
// auto_refresh_interval_seconds 为前端消费的轮询间隔（纯覆盖表键）。
func (s *PanelSettings) buildSettingDefs(deps SettingsDeps) []settingDef {
	debug := deps.Debug
	d0 := s.loadDefaults
	// devin.* 键的 path/取值/热应用全部由 devin 包字段登记表派生
	// （见 devinSettingDef）；此列表只携带面板侧三元组。
	d := func(key, typ, desc string) settingDef {
		return devinSettingDef(deps, d0, key, typ, desc)
	}

	defs := []settingDef{
		// ---- 上游端点与模型 ----
		d("devin_base_url", "string", "上游 Devin Connect 基础地址（devin.base_url）；换端点会重建上游连接束并清空 AssignModel 缓存"),
		d("devin_proxy", "string", "上游代理地址（devin.proxy，http/https/socks5，可带 userinfo）；空为直连或走系统环境变量"),
		d("devin_force_http1", "bool", "强制 HTTP/1.1 每请求独立连接（devin.force_http1）；关闭走 HTTP/2 单连接多路复用"),
		d("devin_model", "string", "上游 chat model UID（devin.model）；请求未指定模型时的兜底目标"),
		d("devin_aliases", "json", "模型别名映射（devin.aliases），JSON 对象 {\"客户端模型名\":\"上游UID\"}；空/{} 为无别名"),
		// ---- 客户端身份（上游 metadata） ----
		d("devin_client_name", "string", "发给上游的客户端名 metadata.extension_name/ide_name（devin.client_name）；空用内置默认"),
		d("devin_client_version", "string", "客户端版本号 metadata.extension_version/ide_version（devin.client_version）；空用内置默认"),
		d("devin_client_os", "string", "客户端系统 metadata.os（devin.client_os）；空用内置默认"),
		// ---- 速率闸门 ----
		d("devin_max_rpm", "int", "每个对齐分钟窗口发往上游的消息配额（devin.max_rpm，条/分钟）；<=0 不做窗口限速"),
		d("gate_max_hold_seconds", "int", "闩外排队最长等待秒数，超时快速失败 429+Retry-After（devin.gate_max_hold_seconds）；<=0 默认 30"),
		d("gate_drip_interval_seconds", "int", "冷却闩内放行探针的间隔秒数（devin.gate_drip_interval_seconds）；<=0 默认 8"),
		d("gate_default_latch_seconds", "int", "上游限流未带 reset hint 时的兜底闩秒数（devin.gate_default_latch_seconds）；<=0 默认 60"),
		d("gate_window_offset_seconds", "int", "上游分钟桶界在本地分钟内的估计位置（devin.gate_window_offset_seconds，第几秒）；负值按 mod 60 折算（-1=:59），默认 0"),
		d("gate_window_guard_seconds", "int", "桶界两侧停发死区秒数（devin.gate_window_guard_seconds）；<=0 或 >=30 默认 2"),
		d("gate_bg_max_hold_seconds", "int", "bg 类请求闸内排队预算秒数（devin.gate_bg_max_hold_seconds，fg 走 gate_max_hold_seconds）；<=0 默认 120"),
		d("gate_bg_reserve_margin", "int", "bg 准入预留公式的固定安全边际条数（devin.gate_bg_reserve_margin）；<=0 默认 4"),
		// ---- 前缀保温 ----
		d("warm_prefix_enabled", "bool", "前缀保温总开关（devin.warm_prefix_enabled）：静默会话按节拍重放请求体给上游 prompt cache 续期"),
		d("warm_prefix_interval_seconds", "int", "每条保温谱系的 ping 节拍秒数（devin.warm_prefix_interval_seconds，须明显低于上游 ~780s TTL）；<=0 默认 180"),
		d("warm_prefix_jitter_ratio", "float", "ping 节拍抖动幅度 ±比例（devin.warm_prefix_jitter_ratio，防同刻齐射打满分钟桶）；取值 (0,1)，<=0 默认 0.15"),
		d("warm_prefix_max_streams", "int", "同时保温的谱系数上限（devin.warm_prefix_max_streams）；<=0 默认 256"),
		d("warm_prefix_max_retained_mb", "int", "保留请求体内存总量上限 MB（devin.warm_prefix_max_retained_mb，超限按 LRU 驱逐）；<=0 默认 96"),
		d("warm_prefix_min_prefix_tokens", "int", "谱系可保温的最低前缀 token 数（devin.warm_prefix_min_prefix_tokens，低于此冷启动够便宜不烧 RPM）；<=0 默认 8192"),
		d("warm_prefix_blocked_max_idle_seconds", "int", "含阻塞型 pending 工具调用的谱系最长保温静默秒数（devin.warm_prefix_blocked_max_idle_seconds）；<=0 默认 14400"),
		d("warm_prefix_userpaced_max_idle_seconds", "int", "无 pending 或仅用户节奏 pending 的谱系最长静默秒数（devin.warm_prefix_userpaced_max_idle_seconds）；<=0 默认 2700"),
		d("warm_prefix_subdone_max_idle_seconds", "int", "已完成 subagent 谱系最长保温静默秒数（devin.warm_prefix_subdone_max_idle_seconds）；<=0 默认 600"),
		d("warm_prefix_unknown_max_idle_seconds", "int", "无法分类流的最长保温静默秒数兜底（devin.warm_prefix_unknown_max_idle_seconds）；<=0 默认 1800"),
		d("warm_prefix_blocked_names", "json", "判为阻塞型的 pending 工具名表（devin.warm_prefix_blocked_names），JSON 数组；空数组用内置默认"),
		d("warm_prefix_userpaced_names", "json", "判为用户节奏型的 pending 工具名表（devin.warm_prefix_userpaced_names），JSON 数组；空数组用内置默认"),
		// ---- 号池调度与流超时 ----
		d("devin_session_affinity_ttl_seconds", "int", "会话→账号绑定的滑动 TTL 秒数（devin.session_affinity_ttl_seconds）；命中即续期，<=0 默认 3600"),
		d("devin_quota_low_threshold_percent", "int", "weekly 剩余配额低于此百分比时新会话降档（devin.quota_low_threshold_percent）；<=0 默认 15，负值关闭降权"),
		d("devin_no_progress_timeout_seconds", "int", "产出过内容后的上游无进度期限秒数（devin.no_progress_timeout_seconds）；须盖住工具参数 15-25min 静默，<=0 默认 2700"),
		d("devin_pre_event_no_progress_timeout_seconds", "int", "产出首个事件前每段无进度期限秒数（devin.pre_event_no_progress_timeout_seconds）；<=0 默认 600，pre-event 累计另有硬顶不突破"),
		// ---- 服务与日志 ----
		{
			key:  "max_concurrency",
			path: "server.max_concurrency",
			typ:  "int",
			desc: "同时处理的 /v1/* 请求数上限（server.max_concurrency）；必须 >=1",
			def:  func() string { return strconv.Itoa(d0().MaxConcurrency) },
			live: func() string { return strconv.Itoa(deps.MaxConcurrency()) },
			apply: func(v string) error {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 1 {
					return errors.New("max_concurrency must be an integer >= 1")
				}
				deps.SetMaxConcurrency(n)
				return nil
			},
		},
		{
			key:  "debug_log_enabled",
			path: "debug.enabled",
			typ:  "bool",
			desc: "启用请求日志（记录上游请求/响应原始数据）",
			def:  func() string { return strconv.FormatBool(d0().DebugEnabled) },
			live: func() string { return strconv.FormatBool(debug.Enabled()) },
			apply: func(v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("value must be a boolean: %w", err)
				}
				debug.SetEnabled(b)
				return nil
			},
		},
		{
			key:  "debug_log_errors_only",
			path: "debug.errors_only",
			typ:  "bool",
			desc: "只保留失败请求的调试记录(干净完成的请求完结即删payload,logs摘要行仍保留)",
			def:  func() string { return strconv.FormatBool(d0().DebugErrorsOnly) },
			live: func() string { return strconv.FormatBool(debug.ErrorsOnly()) },
			apply: func(v string) error {
				b, err := strconv.ParseBool(v)
				if err != nil {
					return fmt.Errorf("value must be a boolean: %w", err)
				}
				debug.SetErrorsOnly(b)
				return nil
			},
		},
		{
			key:   "log_retention_days",
			path:  "debug.retention_days",
			typ:   "int",
			desc:  "日志保留天数(-1永久保留,1-365天)",
			def:   func() string { return strconv.Itoa(d0().Policy.Days) },
			live:  func() string { return strconv.Itoa(debug.Policy().Days) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.Days = n }),
		},
		{
			key:   "log_max_total_mb",
			path:  "debug.max_total_mb",
			typ:   "int",
			desc:  "日志总容量上限(MB,<=0不限制,超限从最旧请求记录开始清理)",
			def:   func() string { return strconv.FormatInt(d0().Policy.MaxTotalMB, 10) },
			live:  func() string { return strconv.FormatInt(debug.Policy().MaxTotalMB, 10) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.MaxTotalMB = int64(n) }),
		},
		{
			key:   "log_payload_hours",
			path:  "debug.payload_hours",
			typ:   "int",
			desc:  "大体积阶段记录保留小时数(超时剥离03/04/06与附件,保留meta/error等证据,<=0不剥离)",
			def:   func() string { return strconv.Itoa(d0().Policy.PayloadHours) },
			live:  func() string { return strconv.Itoa(debug.Policy().PayloadHours) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.PayloadHours = n }),
		},
		{
			key:   "log_keep_error_dirs",
			path:  "debug.keep_error_dirs",
			typ:   "int",
			desc:  "容量淘汰时受保护的最新失败请求记录数(<=0不保护)",
			def:   func() string { return strconv.Itoa(d0().Policy.KeepErrorDirs) },
			live:  func() string { return strconv.Itoa(debug.Policy().KeepErrorDirs) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.KeepErrorDirs = n }),
		},
		{
			key:   "log_row_retention_days",
			typ:   "int",
			desc:  "logs 表摘要行保留天数(独立于调试记录保留,<=0不清理)",
			def:   func() string { return strconv.FormatInt(debuglog.DefaultLogRowRetentionDays, 10) },
			live:  func() string { return strconv.FormatInt(debug.Policy().LogRowDays, 10) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.LogRowDays = int64(n) }),
		},
		{
			key:  "debug_quota_interval_minutes",
			path: "debug.quota_interval_minutes",
			typ:  "int",
			desc: "配额快照采样间隔分钟（debug.quota_interval_minutes，写 quota_samples 表）；<=0 不采样",
			def:  func() string { return strconv.Itoa(int(d0().QuotaInterval / time.Minute)) },
			live: func() string { return strconv.Itoa(int(deps.QuotaInterval() / time.Minute)) },
			apply: func(v string) error {
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil {
					return fmt.Errorf("value must be an integer (minutes): %w", err)
				}
				if int64(n) > math.MaxInt64/int64(time.Minute) || int64(n) < math.MinInt64/int64(time.Minute) {
					return fmt.Errorf("value overflows duration: %d", n)
				}
				deps.SetQuotaInterval(time.Duration(n) * time.Minute)
				return nil
			},
		},
		{
			key:  "debug_pprof_listen",
			path: "debug.pprof_listen",
			typ:  "string",
			desc: "pprof/fgprof 剖析端点独立监听地址（debug.pprof_listen，如 127.0.0.1:6060）；空不启用，端点无鉴权只应绑回环",
			def:  func() string { return d0().PprofListen },
			live: func() string { return deps.PprofListen() },
			apply: func(v string) error {
				return deps.SetPprofListen(strings.TrimSpace(v))
			},
		},
		{
			key:  "auto_refresh_interval_seconds",
			typ:  "int",
			desc: "页面自动刷新间隔(秒,0=禁用,建议>=30;有对话框打开时跳过本次刷新)",
			def:  func() string { return "0" },
			live: nil, // store-only：生效值=覆盖或默认
			apply: func(v string) error { // 无子系统落点，仅校验后入覆盖表
				if _, err := strconv.Atoi(v); err != nil {
					return fmt.Errorf("value must be an integer: %w", err)
				}
				return nil
			},
		},
	}
	return defs
}

// ResampleDefaults 用最近一次文件加载的派生值重灌默认值快照：config
// reload 后由 main 调用——覆盖值不进来，reset 回落目标与 default_value
// 展示因此始终反映当前文件而非启动态。
func (s *PanelSettings) ResampleDefaults(d SettingDefaults) {
	s.defaults.Store(&d)
}

// loadDefaults 读当前默认值快照（def 闭包共用）。
func (s *PanelSettings) loadDefaults() SettingDefaults {
	return *s.defaults.Load()
}

// ApplyAll 重放全部覆盖项（启动加载后与 config reload 后调用——
// reload 会按 config.yaml 重置 debug 开关/策略，覆盖键须压回去）。
// writeMu 持满整个重放：与并发 set/reset 同键交错会让「子系统最终
// 生效值」与「库里最终覆盖」分家。单项失败不中断后续重放，聚合返回。
func (s *PanelSettings) ApplyAll() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	type item struct {
		d *settingDef
		v string
	}
	items := make([]item, 0, len(s.values))
	for i := range s.defs {
		d := &s.defs[i]
		if v, ok := s.values[d.key]; ok && d.apply != nil {
			items = append(items, item{d, v})
		}
	}
	s.mu.Unlock()
	var errs []error
	for _, it := range items {
		if err := it.d.apply(it.v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", it.d.key, err))
		}
	}
	return errors.Join(errs...)
}

// valueOf 返回键的生效值：覆盖在册取覆盖值，否则取 live（无 live
// 取 def）。row 投影与 set/reset 的回滚目标同源。
func (s *PanelSettings) valueOf(d *settingDef) string {
	if v, overridden := s.values[d.key]; overridden {
		return v
	}
	if d.live != nil {
		return d.live()
	}
	return d.def()
}

// row 把键投影成 ccLoad SystemSetting 的 wire 形状。
func (s *PanelSettings) row(d *settingDef) map[string]any {
	return map[string]any{
		"key":           d.key,
		"value":         s.valueOf(d),
		"value_type":    d.typ,
		"description":   d.desc,
		"default_value": d.def(),
		"updated_at":    s.updated[d.key],
		"editable":      true,
	}
}

// List 返回全部键的 wire 行（按注册表顺序输出，稳定）。
func (s *PanelSettings) List() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.defs))
	for i := range s.defs {
		out = append(out, s.row(&s.defs[i]))
	}
	return out
}

// OverrideProvenance 返回在册覆盖项的来源投影（/admin/config 的
// provenance.settings）：每行给 键→yaml 路径→文件值→生效值，让
// 「settings 表压过 config.yaml」这层合并可直接读出来。path 为空的
// settings 表专属键没有文件字段可对拍，不列入。proxy userinfo 与
// 凭据同口径脱敏——override 值本身可能带 user:pass@。
func (s *PanelSettings) OverrideProvenance() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	for i := range s.defs {
		d := &s.defs[i]
		v, ok := s.values[d.key]
		if !ok || d.path == "" {
			continue
		}
		file := d.def()
		if d.key == "devin_proxy" {
			file = redactURLUserinfo(file)
			v = redactURLUserinfo(v)
		}
		out = append(out, map[string]any{
			"key":        d.key,
			"path":       d.path,
			"file":       file,
			"effective":  v,
			"updated_at": s.updated[d.key],
		})
	}
	return out
}

// redactURLUserinfo 剔除 URL 里的 user:pass@ 段（devin.proxy 覆盖值与
// redactSecrets 同口径——面板展示不暴露代理凭据）。
func redactURLUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// Get 返回单键的 wire 行；未知键返回 false。
func (s *PanelSettings) Get(key string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byKey[key]
	if !ok {
		return nil, false
	}
	return s.row(d), true
}

// set 校验并应用单键（apply 含类型校验），成功后入库并更新覆盖表。
// apply 在锁外跑（慢路径），持久化失败时回灌进入前的生效值补偿——
// 不让「runtime 已改、库里没有」的分裂态活到重启才发现。
func (s *PanelSettings) set(key, value string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	d, ok := s.byKey[key]
	if !ok {
		s.mu.Unlock()
		return errSettingNotFound
	}
	prev, hadOverride := s.values[key]
	s.mu.Unlock()

	if err := d.apply(value); err != nil {
		return err
	}
	// updated_at 沿用文件时代的 unix 秒口径（导入器原样保留），
	// 不同于 store.SetSetting 零值的毫秒默认。
	ts := time.Now().Unix()
	if err := s.st.SetSetting(context.Background(), key, value, ts); err != nil {
		if !hadOverride {
			prev = d.def()
		}
		if cerr := d.apply(prev); cerr != nil {
			slog.Warn("settings: compensate apply failed after persist error", "key", key, "error", cerr)
		}
		return err
	}
	s.mu.Lock()
	s.values[key] = value
	s.updated[key] = ts
	s.mu.Unlock()
	return nil
}

// reset 应用文件默认值后删除覆盖。应用失败时覆盖原样保留；删行失败
// 时回灌旧覆盖值补偿——与 set 同一份「不让分裂态出门」口径。
func (s *PanelSettings) reset(key string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	d, ok := s.byKey[key]
	if !ok {
		s.mu.Unlock()
		return errSettingNotFound
	}
	prev, hadOverride := s.values[key]
	s.mu.Unlock()

	if d.apply != nil {
		prev = s.valueOf(d)
		if err := d.apply(d.def()); err != nil {
			return err
		}
	}
	if err := s.st.DeleteSetting(context.Background(), key); err != nil {
		if d.apply != nil && hadOverride {
			if cerr := d.apply(prev); cerr != nil {
				slog.Warn("settings: compensate apply failed after delete error", "key", key, "error", cerr)
			}
		}
		return err
	}
	s.mu.Lock()
	delete(s.values, key)
	delete(s.updated, key)
	s.mu.Unlock()
	return nil
}

var errSettingNotFound = errors.New("setting not found")

// adminListSettings 实现 GET /admin/settings：全部键的 wire 数组。
func (h *Handler) adminListSettings(w http.ResponseWriter, _ *http.Request) {
	if h.settings == nil {
		respondOK(w, []any{})
		return
	}
	respondOK(w, h.settings.List())
}

// adminGetSetting 实现 GET /admin/settings/{key}。
func (h *Handler) adminGetSetting(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		respondError(w, http.StatusNotFound, errSettingNotFound.Error())
		return
	}
	row, ok := h.settings.Get(chi.URLParam(r, "key"))
	if !ok {
		respondError(w, http.StatusNotFound, errSettingNotFound.Error())
		return
	}
	respondOK(w, row)
}

// adminUpdateSetting 实现 PUT /admin/settings/{key}，body {"value": "..."}。
func (h *Handler) adminUpdateSetting(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		respondError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := h.settings.set(chi.URLParam(r, "key"), req.Value); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, errSettingNotFound) {
			status = http.StatusNotFound
		}
		respondError(w, status, err.Error())
		return
	}
	respondOK(w, map[string]any{"key": chi.URLParam(r, "key"), "value": req.Value})
}

// adminResetSetting 实现 POST /admin/settings/{key}/reset：回默认。
func (h *Handler) adminResetSetting(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		respondError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	key := chi.URLParam(r, "key")
	if err := h.settings.reset(key); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errSettingNotFound) {
			status = http.StatusNotFound
		}
		respondError(w, status, err.Error())
		return
	}
	respondOK(w, map[string]any{"key": key})
}

// adminBatchUpdateSettings 实现 POST /admin/settings/batch：body 是
// {key: value} 平铺表。未知键先整单拒绝；逐项按注册顺序应用，值非法
// 或应用失败即停——已生效的前项不回滚（apply 含子系统副作用，无法
// 预检），但错误响应带 applied 回执，调用方拿得到部分生效的边界。
// 当前调用方只发单键，原子性由单键天然满足。
func (h *Handler) adminBatchUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		respondError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	var req map[string]string
	if !decodeJSON(w, r, &req) {
		return
	}
	keys := make([]string, 0, len(req))
	h.settings.mu.Lock()
	for k := range req {
		if _, ok := h.settings.byKey[k]; !ok {
			h.settings.mu.Unlock()
			respondError(w, http.StatusNotFound, fmt.Sprintf("setting not found: %s", k))
			return
		}
		keys = append(keys, k)
	}
	h.settings.mu.Unlock()
	sort.Strings(keys)
	applied := make([]string, 0, len(keys))
	for _, k := range keys {
		if err := h.settings.set(k, req[k]); err != nil {
			writeEnvelope(w, http.StatusBadRequest, apiResponse{Success: false,
				Error: fmt.Sprintf("%s: %s", k, err.Error()),
				Data:  map[string]any{"applied": applied}})
			return
		}
		applied = append(applied, k)
	}
	respondOK(w, map[string]any{"message": fmt.Sprintf("%d settings updated", len(keys))})
}
