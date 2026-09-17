package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/config"
)

// TestAccountsCRUDRoundTrip 覆盖建/读/列/删主路径与 NULL 语义：
// token/credentials_file 写 "" 落 NULL、读 NULL 回 ""。
func TestAccountsCRUDRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if row, ok, err := s.GetAccount(ctx, "ghost"); err != nil || ok || row != nil {
		t.Fatalf("GetAccount missing = (%v,%v,%v), want (nil,false,nil)", row, ok, err)
	}

	if err := s.UpsertAccount(ctx, &AccountRow{Name: "yanjian", Token: "sess-y", CreatedAt: 1000}); err != nil {
		t.Fatalf("upsert yanjian: %v", err)
	}
	if err := s.UpsertAccount(ctx, &AccountRow{
		Name: "randall", CredentialsFile: "/p/c.toml", Disabled: true, CreatedAt: 900,
	}); err != nil {
		t.Fatalf("upsert randall: %v", err)
	}
	if err := s.UpsertAccount(ctx, &AccountRow{Name: "old", Deleted: true, CreatedAt: 1100}); err != nil {
		t.Fatalf("upsert old: %v", err)
	}

	rows, err := s.ListAccounts(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// ORDER BY created_at：randall(900) → yanjian(1000) → old(1100)。
	if rows[0].Name != "randall" || rows[1].Name != "yanjian" || rows[2].Name != "old" {
		t.Fatalf("order = %v,%v,%v", rows[0].Name, rows[1].Name, rows[2].Name)
	}
	if rows[0].Token != "" || rows[0].CredentialsFile != "/p/c.toml" || !rows[0].Disabled {
		t.Fatalf("randall = %+v", rows[0])
	}
	if rows[1].Token != "sess-y" || rows[1].CredentialsFile != "" || rows[1].Disabled {
		t.Fatalf("yanjian = %+v", rows[1])
	}
	if !rows[2].Deleted {
		t.Fatalf("old.Deleted = false, want tombstone")
	}

	row, ok, err := s.GetAccount(ctx, "randall")
	if err != nil || !ok {
		t.Fatalf("GetAccount randall = ok=%v err=%v", ok, err)
	}
	if row.CreatedAt != 900 || row.UpdatedAt == 0 {
		t.Fatalf("randall times = %+v", row)
	}

	if err := s.DeleteAccount(ctx, "old"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, ok, _ := s.GetAccount(ctx, "old"); ok {
		t.Fatal("old still present after delete")
	}
	if err := s.DeleteAccount(ctx, "ghost"); err != nil {
		t.Fatalf("delete missing should be a no-op: %v", err)
	}
}

// TestUpsertAccountConflictSemantics 二次 upsert：created_at 保留首插值，
// updated_at 刷新，字段整行覆盖（"" 落 NULL 即清行覆盖）。
func TestUpsertAccountConflictSemantics(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.UpsertAccount(ctx, &AccountRow{
		Name: "yanjian", Token: "sess-v1", CredentialsFile: "/p/c.toml", CreatedAt: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	first, _, _ := s.GetAccount(ctx, "yanjian")
	time.Sleep(5 * time.Millisecond) // 跨过毫秒边界，updated_at 可见刷新
	if err := s.UpsertAccount(ctx, &AccountRow{Name: "yanjian", Token: "sess-v2"}); err != nil {
		t.Fatal(err)
	}
	second, _, _ := s.GetAccount(ctx, "yanjian")
	if second.Token != "sess-v2" {
		t.Fatalf("token = %q, want sess-v2", second.Token)
	}
	if second.CredentialsFile != "" {
		t.Fatalf("credentials_file = %q, want cleared to NULL", second.CredentialsFile)
	}
	if second.CreatedAt != 1000 {
		t.Fatalf("created_at = %d, want first-insert 1000", second.CreatedAt)
	}
	if second.UpdatedAt <= first.UpdatedAt {
		t.Fatalf("updated_at not refreshed: %d <= %d", second.UpdatedAt, first.UpdatedAt)
	}
}

// TestGCTombstonedAccounts 只收死墓碑：declared 集内的墓碑保留（可
// restore），活行不碰；declared 为空收全部墓碑。
func TestGCTombstonedAccounts(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	for _, r := range []*AccountRow{
		{Name: "a", Deleted: true, CreatedAt: 1},             // config 声明的墓碑
		{Name: "b", Deleted: true, CreatedAt: 2},             // 死墓碑
		{Name: "c", Token: "t", CreatedAt: 3},                // 活行（panel）
		{Name: "d", Token: "t", Deleted: true, CreatedAt: 4}, // declared 墓碑 2 号
		{Name: "e", Token: "t", CreatedAt: 5},                // 活行且 declared
	} {
		if err := s.UpsertAccount(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.GCTombstonedAccounts(ctx, []string{"a", "d", "e"})
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n != 1 {
		t.Fatalf("GC removed %d, want 1 (only dead tombstone b)", n)
	}
	if _, ok, _ := s.GetAccount(ctx, "a"); !ok {
		t.Fatal("declared tombstone a wrongly collected")
	}
	if _, ok, _ := s.GetAccount(ctx, "b"); ok {
		t.Fatal("dead tombstone b still present")
	}
	if _, ok, _ := s.GetAccount(ctx, "c"); !ok {
		t.Fatal("live panel row c wrongly collected")
	}

	n, err = s.GCTombstonedAccounts(ctx, nil)
	if err != nil {
		t.Fatalf("GC nil declared: %v", err)
	}
	if n != 2 {
		t.Fatalf("GC(nil) removed %d, want 2 (a and d)", n)
	}
	rows, _ := s.ListAccounts(ctx)
	if len(rows) != 2 || rows[0].Name != "c" || rows[1].Name != "e" {
		t.Fatalf("survivors = %+v", rows)
	}
}

// TestMergeAccounts 覆盖 overlay merge 全部分支：declared 序优先、
// 行值逐字段覆盖（空行值不压 config）、disabled 恒取行值、墓碑压
// config 名、死墓碑不进视图、panel 名按 created_at 再按 name 排。
func TestMergeAccounts(t *testing.T) {
	declared := []config.DevinAccountConfig{
		{Name: "cfg1", Token: "cfg-tok"},
		{Name: "cfg2", Token: "cfg2-tok", CredentialsFile: "/cfg2.toml"},
		{Name: "gone", Token: "gone-tok"},
	}
	rows := []*AccountRow{
		{Name: "cfg1", Token: "row-tok", Disabled: true, CreatedAt: 5, UpdatedAt: 6},
		{Name: "cfg2", CreatedAt: 7, UpdatedAt: 8}, // 行值全空 → 不压 config
		{Name: "gone", Deleted: true, CreatedAt: 9, UpdatedAt: 10},
		{Name: "ghost", Deleted: true, CreatedAt: 11}, // 死墓碑
		{Name: "panel-b", Token: "pb", CreatedAt: 12},
		{Name: "panel-a", Token: "pa", CreatedAt: 12}, // 同 created_at，name 序靠前
	}

	got := MergeAccounts(declared, rows)
	if len(got) != 5 {
		t.Fatalf("merged = %d, want 5 (ghost excluded)", len(got))
	}
	order := []string{got[0].Name, got[1].Name, got[2].Name, got[3].Name, got[4].Name}
	want := []string{"cfg1", "cfg2", "gone", "panel-a", "panel-b"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}

	c1 := got[0]
	if c1.Source != AccountSourceConfig || !c1.ConfigDeclared || !c1.HasRow {
		t.Fatalf("cfg1 source flags = %+v", c1)
	}
	if c1.Token != "row-tok" || !c1.Disabled || c1.CreatedAt != 5 || c1.UpdatedAt != 6 {
		t.Fatalf("cfg1 = %+v", c1)
	}

	c2 := got[1]
	if c2.Token != "cfg2-tok" || c2.CredentialsFile != "/cfg2.toml" {
		t.Fatalf("cfg2 = %+v, empty row fields must not clobber config", c2)
	}
	if c2.Disabled || c2.Source != AccountSourceConfig {
		t.Fatalf("cfg2 = %+v", c2)
	}

	g := got[2]
	if g.Source != AccountSourceTombstoned || !g.ConfigDeclared || !g.HasRow {
		t.Fatalf("gone = %+v", g)
	}
	if g.Token != "gone-tok" {
		t.Fatalf("tombstoned token = %q, want config value", g.Token)
	}

	for _, p := range got[3:] {
		if p.Source != AccountSourcePanel || p.ConfigDeclared || !p.HasRow {
			t.Fatalf("panel entry = %+v", p)
		}
	}
}

// TestEffectiveAccounts 读路径封装 = ListAccounts + MergeAccounts。
func TestEffectiveAccounts(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.UpsertAccount(ctx, &AccountRow{Name: "extra", Token: "t", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(ctx, &AccountRow{Name: "gone", Deleted: true, CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}

	got, err := s.EffectiveAccounts(ctx, []config.DevinAccountConfig{
		{Name: "gone", Token: "cfg"},
		{Name: "live", Token: "cfg2"},
	})
	if err != nil {
		t.Fatalf("EffectiveAccounts: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("merged = %d, want 3", len(got))
	}
	if got[0].Source != AccountSourceTombstoned || got[1].Source != AccountSourceConfig ||
		got[2].Source != AccountSourcePanel || got[2].Name != "extra" {
		t.Fatalf("merged = %+v", got)
	}
}

// TestAccountMetaColumns 覆盖 priority/max_rpm/notes 的 NULL 语义：
// nil 落 NULL、读 NULL 回 nil/""；merge 时非空行值赢 config 值、
// NULL 行回落 config，墓碑行同受行覆盖（restore 复活的就是行值）。
func TestAccountMetaColumns(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	p7, m30 := int64(7), int64(30)
	if err := s.UpsertAccount(ctx, &AccountRow{
		Name: "a", Token: "t", Priority: &p7, MaxRPM: &m30, Notes: "hi", CreatedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertAccount(ctx, &AccountRow{Name: "b", Token: "t", CreatedAt: 2}); err != nil {
		t.Fatal(err)
	}
	row, _, _ := s.GetAccount(ctx, "a")
	if row.Priority == nil || *row.Priority != 7 || row.MaxRPM == nil || *row.MaxRPM != 30 || row.Notes != "hi" {
		t.Fatalf("a = %+v", row)
	}
	row, _, _ = s.GetAccount(ctx, "b")
	if row.Priority != nil || row.MaxRPM != nil || row.Notes != "" {
		t.Fatalf("b = %+v, want NULL meta", row)
	}

	merged := MergeAccounts([]config.DevinAccountConfig{
		{Name: "a", Token: "cfg", Priority: 3, MaxRPM: 10},
		{Name: "c", Token: "cfg", Priority: 3},
	}, []*AccountRow{
		{Name: "a", Priority: &p7, Notes: "n", CreatedAt: 1},
		{Name: "c", MaxRPM: &m30, Deleted: true, CreatedAt: 2},
	})
	if merged[0].Priority != 7 || merged[0].MaxRPM != 10 || merged[0].Notes != "n" {
		t.Fatalf("merged a = %+v", merged[0])
	}
	if merged[1].Source != AccountSourceTombstoned || merged[1].Priority != 3 || merged[1].MaxRPM != 30 {
		t.Fatalf("merged c = %+v", merged[1])
	}
}

// TestAccountColumnsMigration 验证存量表（首发形状、无新列）经
// 版本化迁移幂等补齐 priority/max_rpm/notes：Open 走 ALTER 路径后
// 新列可正常读写。
func TestAccountColumnsMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?_pragma=journal_mode=WAL", path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE upstream_accounts (
		name TEXT PRIMARY KEY, token TEXT, credentials_file TEXT,
		disabled INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on legacy schema: %v", err)
	}
	defer func() { _ = s.Close() }()

	p := int64(2)
	if err := s.UpsertAccount(context.Background(), &AccountRow{Name: "a", Priority: &p, Notes: "n", CreatedAt: 1}); err != nil {
		t.Fatalf("upsert on migrated schema: %v", err)
	}
	row, ok, _ := s.GetAccount(context.Background(), "a")
	if !ok || row.Priority == nil || *row.Priority != 2 || row.Notes != "n" {
		t.Fatalf("row = %+v ok=%v", row, ok)
	}
}
