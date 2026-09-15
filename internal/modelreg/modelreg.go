// Package modelreg 实现全局模型注册表：models.json 持久化 + /v1 准入用的
// 启用开关与重定向覆盖。等价于 ccLoad 单渠道 ModelEntry{redirect_model,
// disabled} 的集合——本服务只有一条上游，注册表是全局覆盖层而非渠道属性。
//
// 生效序：注册表 redirect_model → config.yaml 的 devin.aliases → 原名
// （别名解析在 adapter 内完成，注册表只改写请求的模型名）。停用命中在
// 准入层返回 404——对客户端的语义是「本网关不提供该模型」，与令牌
// 白名单的 403 分层。
package modelreg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Entry 是一条注册表覆盖；字段名对齐 ccLoad model.ModelEntry。
type Entry struct {
	RedirectModel string `json:"redirect_model,omitempty"`
	Disabled      bool   `json:"disabled,omitempty"`
}

// registryFile 是 models.json 的持久化形状：按模型名键控的覆盖表。
type registryFile struct {
	Models map[string]Entry `json:"models"`
}

// Store 管理 models.json 与内存覆盖表；变更写穿透落盘（文件 KB 级，
// 管理操作低频）。加载失败按坏档处理：原文件改名留档后空仓起步。
type Store struct {
	mu      sync.RWMutex
	path    string
	entries map[string]Entry
}

// New 加载 stateDir/models.json；文件缺失以空仓起步。
func New(stateDir string) (*Store, error) {
	s := &Store{
		path:    filepath.Join(stateDir, "models.json"),
		entries: map[string]Entry{},
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f registryFile
	if err := json.Unmarshal(data, &f); err != nil {
		_ = os.Rename(s.path, s.path+".corrupt")
		return s, nil
	}
	for name, e := range f.Models {
		if name == "" {
			continue
		}
		s.entries[name] = e
	}
	return s, nil
}

// normalize 校验并规整模型名与重定向目标（ccLoad ModelEntry.Validate
// 同款：去首尾空白、拒 \x00\r\n、target==name 时清空 target）。
func normalize(name string, e Entry) (string, Entry, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", e, errors.New("model cannot be empty")
	}
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", e, errors.New("model contains illegal characters")
	}
	e.RedirectModel = strings.TrimSpace(e.RedirectModel)
	if strings.ContainsAny(e.RedirectModel, "\x00\r\n") {
		return "", e, errors.New("redirect_model contains illegal characters")
	}
	if e.RedirectModel == name {
		e.RedirectModel = ""
	}
	return name, e, nil
}

// Lookup 查模型的覆盖项：精确命中 → 大小写折叠（与 ResolveModelAlias
// 的匹配顺序一致）。未命中返回 ok=false，调用方按默认启用、无重定向处理。
func (s *Store) Lookup(name string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if e, ok := s.entries[name]; ok {
		return e, true
	}
	for n, e := range s.entries {
		if strings.EqualFold(n, name) {
			return e, true
		}
	}
	return Entry{}, false
}

// Set 覆盖写一条注册项并落盘；项退化为全默认（启用且无重定向）时自动
// 删除——文件只承载非默认覆盖，PUT 传默认值即等价于重置。
func (s *Store) Set(name string, e Entry) error {
	name, e, err := normalize(name, e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !e.Disabled && e.RedirectModel == "" {
		delete(s.entries, name)
	} else {
		s.entries[name] = e
	}
	return s.saveLocked()
}

// Delete 移除覆盖；不存在时按成功处理（幂等删除）。
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, name)
	return s.saveLocked()
}

// Entries 返回全部覆盖项的副本（键为注册时的原始大小写）。
func (s *Store) Entries() map[string]Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]Entry, len(s.entries))
	for n, e := range s.entries {
		out[n] = e
	}
	return out
}

// saveLocked 原子落盘（tmp+rename）；调用方必须持锁。
func (s *Store) saveLocked() error {
	f := registryFile{Models: make(map[string]Entry, len(s.entries))}
	for n, e := range s.entries {
		f.Models[n] = e
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Names 返回覆盖项的名字表（排序后），供渠道模型清单投影并入注册表名。
func (s *Store) Names() []string {
	entries := s.Entries()
	out := make([]string, 0, len(entries))
	for n := range entries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
