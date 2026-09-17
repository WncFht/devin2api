// Package modelreg 实现全局模型注册表：model_registry 表持久化 + /v1 准入用的
// 启用开关与重定向覆盖。等价于 ccLoad 单渠道 ModelEntry{redirect_model,
// disabled} 的集合——本服务只有一条上游，注册表是全局覆盖层而非渠道属性。
//
// 生效序：注册表 redirect_model → config.yaml 的 devin.aliases → 原名
// （别名解析在 adapter 内完成，注册表只改写请求的模型名）。停用命中在
// 准入层返回 404——对客户端的语义是「本网关不提供该模型」，与令牌
// 白名单的 403 分层。
package modelreg

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/WncFht/devin2api/internal/store"
)

// Entry 是一条注册表覆盖；字段名对齐 ccLoad model.ModelEntry。
type Entry struct {
	RedirectModel string
	Disabled      bool
}

// Store 管理 model_registry 表与内存覆盖表；管理操作写穿透入库（低频），
// /v1 准入路径的 Lookup 纯走内存。
type Store struct {
	mu      sync.RWMutex
	st      *store.Store
	entries map[string]Entry
}

// New 从 model_registry 表水合内存覆盖表；空表以空仓起步。
func New(st *store.Store) (*Store, error) {
	s := &Store{st: st, entries: map[string]Entry{}}
	models, err := st.ListModels(context.Background())
	if err != nil {
		return nil, err
	}
	for _, m := range models {
		if m.Model == "" {
			continue
		}
		s.entries[m.Model] = Entry{RedirectModel: m.RedirectModel, Disabled: m.Disabled}
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

// Set 覆盖写一条注册项并入库；项退化为全默认（启用且无重定向）时自动
// 删除——表只承载非默认覆盖，PUT 传默认值即等价于重置。先写库后改
// 内存：写失败时两侧一致保持旧值。
func (s *Store) Set(name string, e Entry) error {
	name, e, err := normalize(name, e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !e.Disabled && e.RedirectModel == "" {
		if err := s.st.DeleteModel(context.Background(), name); err != nil {
			return err
		}
		delete(s.entries, name)
		return nil
	}
	if err := s.st.SetModel(context.Background(), store.ModelEntry{
		Model:         name,
		RedirectModel: e.RedirectModel,
		Disabled:      e.Disabled,
	}); err != nil {
		return err
	}
	s.entries[name] = e
	return nil
}

// Delete 移除覆盖；不存在时按成功处理（幂等删除）。
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.st.DeleteModel(context.Background(), name); err != nil {
		return err
	}
	delete(s.entries, name)
	return nil
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
