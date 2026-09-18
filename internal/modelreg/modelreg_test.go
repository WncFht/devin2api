package modelreg

import (
	"path/filepath"
	"testing"

	"github.com/WncFht/devin2api/internal/store"
)

func openTemp(t *testing.T) (*Store, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, st
}

func TestSetLookupDeleteRoundTrip(t *testing.T) {
	s, _ := openTemp(t)

	if _, ok := s.Lookup("claude-x"); ok {
		t.Fatal("empty registry should miss")
	}
	if err := s.Set("claude-x", Entry{RedirectModel: "claude-y"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	e, ok := s.Lookup("claude-x")
	if !ok || e.RedirectModel != "claude-y" || e.Disabled {
		t.Fatalf("Lookup after Set: %+v ok=%v", e, ok)
	}
	if err := s.Delete("claude-x"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := s.Lookup("claude-x"); ok {
		t.Fatal("Lookup after Delete should miss")
	}
	// 幂等：删不存在的键按成功处理。
	if err := s.Delete("claude-x"); err != nil {
		t.Fatalf("Delete missing: %v", err)
	}
}

func TestSetDefaultEntryAutoDeletes(t *testing.T) {
	s, st := openTemp(t)

	if err := s.Set("claude-x", Entry{Disabled: true}); err != nil {
		t.Fatalf("Set disabled: %v", err)
	}
	// PUT 全默认值等价于重置：行与内存项都应消失。
	if err := s.Set("claude-x", Entry{}); err != nil {
		t.Fatalf("Set default: %v", err)
	}
	if _, ok := s.Lookup("claude-x"); ok {
		t.Fatal("default entry should have been removed from memory")
	}
	models, err := st.ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("default entry should have been removed from db: %+v", models)
	}
}

func TestCaseVariantKeysStaySingleRow(t *testing.T) {
	s, st := openTemp(t)

	if err := s.Set("Claude-X", Entry{Disabled: true}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	// 写路径经 foldKey 归键：大小写变体命中同一行、同一内存键。
	if err := s.Set("claude-x", Entry{RedirectModel: "claude-y"}); err != nil {
		t.Fatalf("Set variant: %v", err)
	}
	models, err := st.ListModels(t.Context())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].Model != "Claude-X" {
		t.Fatalf("case variant should reuse registered key: %+v", models)
	}
	if err := s.Delete("CLAUDE-X"); err != nil {
		t.Fatalf("Delete variant: %v", err)
	}
	if _, ok := s.Lookup("claude-x"); ok {
		t.Fatal("case-variant Delete should remove the registered key")
	}
}

func TestNewRehydratesFromDB(t *testing.T) {
	s, st := openTemp(t)
	if err := s.Set("claude-x", Entry{RedirectModel: "claude-y", Disabled: true}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s2, err := New(st)
	if err != nil {
		t.Fatalf("New rehydrate: %v", err)
	}
	e, ok := s2.Lookup("claude-x")
	if !ok || !e.Disabled || e.RedirectModel != "claude-y" {
		t.Fatalf("rehydrated entry: %+v ok=%v", e, ok)
	}
}
