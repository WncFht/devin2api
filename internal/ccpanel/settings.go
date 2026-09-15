package ccpanel

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// 本文件实现 /admin/settings 的运行时键仓：值以字符串存取（ccLoad
// SystemSetting 契约），覆盖项持久化到 state/panel-settings.json。
// 只登记存在真实热更新路径的键；启动与 config reload 后重放覆盖——
// 面板设置对被覆盖键恒赢 config.yaml（ccLoad system_settings 同款语义：
// DB 持久层压过启动默认）。

// settingDef 描述一个暴露的运行时键。live 读取子系统实况值（applied 键
// 必须有）；apply 把新值热应用进子系统，nil 表示纯面板侧键——值只进
// 覆盖表，由前端读取生效（如 auto_refresh_interval_seconds）。
type settingDef struct {
	key   string
	typ   string // value_type: bool|int
	desc  string // i18n settings.desc.<key> 缺失时的兜底描述
	def   func() string
	live  func() string
	apply func(string) error
}

// PanelSettings 管理 panel-settings.json 与键注册表。
type PanelSettings struct {
	mu      sync.Mutex
	path    string
	defs    []settingDef
	byKey   map[string]*settingDef
	values  map[string]string // 覆盖值；不设则按 live/def 取生效值
	updated map[string]int64  // 各键最近覆盖时刻（unix 秒）
}

// settingsFile 是 panel-settings.json 的持久化形状。
type settingsFile struct {
	Values  map[string]string `json:"values"`
	Updated map[string]int64  `json:"updated,omitempty"`
}

// NewPanelSettings 创建键仓并加载 stateDir/panel-settings.json；debug
// 提供日志开关与保留策略的热更入口。构造时采样 config 派生状态作默认值。
func NewPanelSettings(stateDir string, debug *debuglog.Manager) (*PanelSettings, error) {
	s := &PanelSettings{
		path:    filepath.Join(stateDir, "panel-settings.json"),
		byKey:   map[string]*settingDef{},
		values:  map[string]string{},
		updated: map[string]int64{},
	}
	s.defs = buildSettingDefs(debug)
	for i := range s.defs {
		s.byKey[s.defs[i].key] = &s.defs[i]
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f settingsFile
	if err := json.Unmarshal(data, &f); err != nil {
		_ = os.Rename(s.path, s.path+".corrupt")
		return s, nil
	}
	for k, v := range f.Values {
		if _, ok := s.byKey[k]; ok {
			s.values[k] = v
		}
	}
	for k, ts := range f.Updated {
		if _, ok := s.byKey[k]; ok {
			s.updated[k] = ts
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

// buildSettingDefs 注册全部可暴露键：日志开关与四段保留策略映射
// debuglog.Manager 热更入口；auto_refresh_interval_seconds 为前端消费的
// 轮询间隔（apply 为空，只入覆盖表）。
func buildSettingDefs(debug *debuglog.Manager) []settingDef {
	boot := debug.Policy()
	bootEnabled := debug.Enabled()
	return []settingDef{
		{
			key:  "debug_log_enabled",
			typ:  "bool",
			desc: "启用请求日志（记录上游请求/响应原始数据）",
			def:  func() string { return strconv.FormatBool(bootEnabled) },
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
			key:   "log_retention_days",
			typ:   "int",
			desc:  "日志保留天数(-1永久保留,1-365天)",
			def:   func() string { return strconv.Itoa(boot.Days) },
			live:  func() string { return strconv.Itoa(debug.Policy().Days) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.Days = n }),
		},
		{
			key:   "log_max_total_mb",
			typ:   "int",
			desc:  "日志总容量上限(MB,<=0不限制,超限从最旧目录开始清理)",
			def:   func() string { return strconv.FormatInt(boot.MaxTotalMB, 10) },
			live:  func() string { return strconv.FormatInt(debug.Policy().MaxTotalMB, 10) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.MaxTotalMB = int64(n) }),
		},
		{
			key:   "log_payload_hours",
			typ:   "int",
			desc:  "大体积阶段文件保留小时数(超时剥离03/04/06与附件,保留meta/error等证据,<=0不剥离)",
			def:   func() string { return strconv.Itoa(boot.PayloadHours) },
			live:  func() string { return strconv.Itoa(debug.Policy().PayloadHours) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.PayloadHours = n }),
		},
		{
			key:   "log_keep_error_dirs",
			typ:   "int",
			desc:  "容量淘汰时受保护的最新失败目录数(<=0不保护)",
			def:   func() string { return strconv.Itoa(boot.KeepErrorDirs) },
			live:  func() string { return strconv.Itoa(debug.Policy().KeepErrorDirs) },
			apply: setPolicyField(debug, func(p *debuglog.RetentionPolicy, n int) { p.KeepErrorDirs = n }),
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
}

// ApplyAll 重放全部覆盖项（启动加载后与 config reload 后调用——
// reload 会按 config.yaml 重置 debug 开关/策略，覆盖键须压回去）。
// 单项失败不中断后续重放，聚合返回错误。
func (s *PanelSettings) ApplyAll() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, d := range s.defs {
		v, ok := s.values[d.key]
		if !ok || d.apply == nil {
			continue
		}
		if err := d.apply(v); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", d.key, err))
		}
	}
	return errors.Join(errs...)
}

// row 把键投影成 ccLoad SystemSetting 的 wire 形状。
func (s *PanelSettings) row(d *settingDef) map[string]any {
	value, overridden := s.values[d.key]
	if !overridden {
		if d.live != nil {
			value = d.live()
		} else {
			value = d.def()
		}
	}
	return map[string]any{
		"key":           d.key,
		"value":         value,
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

// set 校验并应用单键（apply 含类型校验），成功后入覆盖表并落盘。
// 调用方不得持锁。
func (s *PanelSettings) set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byKey[key]
	if !ok {
		return errSettingNotFound
	}
	if err := d.apply(value); err != nil {
		return err
	}
	s.values[key] = value
	s.updated[key] = time.Now().Unix()
	return s.saveLocked()
}

// reset 删除覆盖并回放到默认值。
func (s *PanelSettings) reset(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.byKey[key]
	if !ok {
		return errSettingNotFound
	}
	delete(s.values, key)
	delete(s.updated, key)
	if d.apply != nil {
		if err := d.apply(d.def()); err != nil {
			return err
		}
	}
	return s.saveLocked()
}

// saveLocked 原子落盘；调用方必须持锁。
func (s *PanelSettings) saveLocked() error {
	data, err := json.MarshalIndent(settingsFile{Values: s.values, Updated: s.updated}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json body")
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
// {key: value} 平铺表。先全量校验（未知键/非法值整单拒绝），再逐项应用。
func (h *Handler) adminBatchUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if h.settings == nil {
		respondError(w, http.StatusServiceUnavailable, "settings unavailable")
		return
	}
	var req map[string]string
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	keys := make([]string, 0, len(req))
	for k := range req {
		if _, ok := h.settings.byKey[k]; !ok {
			respondError(w, http.StatusNotFound, fmt.Sprintf("setting not found: %s", k))
			return
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := h.settings.set(k, req[k]); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("%s: %s", k, err.Error()))
			return
		}
	}
	respondOK(w, map[string]any{"message": fmt.Sprintf("%d settings updated", len(keys))})
}

// adminUpdateCheck 实现 POST /admin/update/check：本服务无内建更新器
// （部署走 scripts/deploy.sh），与 ccLoad updateManager 缺省路径同语义。
func (h *Handler) adminUpdateCheck(w http.ResponseWriter, _ *http.Request) {
	respondError(w, http.StatusServiceUnavailable, "update manager is not available")
}
