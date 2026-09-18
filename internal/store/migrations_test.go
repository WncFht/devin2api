// 本文件验证版本化迁移的补列路径与幂等性（存量库 ALTER、新库直建、
// 重复 Open 不重复执行）。
package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// TestMigration0007LegacyDB 模拟停在 0006 的存量库：quota_samples 无
// overage_balance_micros 列、schema_migrations 已登记 0001-0006。
// Open 应补列、保留旧行、登记 0007；再次 Open 不重复 ALTER 不报错。
func TestMigration0007LegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw := openRaw(t, path)
	if _, err := raw.Exec(`CREATE TABLE quota_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER NOT NULL,
		account TEXT NOT NULL DEFAULT '',
		daily_remaining REAL,
		weekly_remaining REAL,
		daily_reset_at INTEGER NOT NULL DEFAULT 0,
		weekly_reset_at INTEGER NOT NULL DEFAULT 0,
		prompt_credits REAL NOT NULL DEFAULT 0,
		flow_credits REAL NOT NULL DEFAULT 0,
		flex_credits REAL NOT NULL DEFAULT 0,
		acu_consumed REAL NOT NULL DEFAULT 0,
		acu_limit REAL NOT NULL DEFAULT 0,
		used_prompt_credits REAL NOT NULL DEFAULT 0,
		used_flow_credits REAL NOT NULL DEFAULT 0,
		used_flex_credits REAL NOT NULL DEFAULT 0,
		grace_period_status TEXT NOT NULL DEFAULT '',
		grace_period_end INTEGER NOT NULL DEFAULT 0,
		was_reduced_by_orphaned_usage INTEGER NOT NULL DEFAULT 0,
		top_up_enabled INTEGER NOT NULL DEFAULT 0,
		top_up_transaction_status TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO quota_samples(at, account, daily_remaining) VALUES(1,'yanjian',86)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"0001_debug_blob_usize", "0002_auth_tokens_class", "0003_upstream_accounts_pool_columns",
		"0004_logs_dir_partial_unique", "0005_gate_windows", "0006_log_cells"} {
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, v, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	// 旧行保留，新列可读且为 0。
	var ov int64
	if err := s.ro.QueryRow(`SELECT overage_balance_micros FROM quota_samples WHERE account='yanjian'`).Scan(&ov); err != nil {
		t.Fatalf("read new column: %v", err)
	}
	if ov != 0 {
		t.Fatalf("overage_balance_micros = %d, want 0 for legacy row", ov)
	}
	var n int
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM quota_samples`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d err=%v", n, err)
	}
	var applied int
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version='0007_quota_samples_overage_micros'`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("0007 registered = %d err=%v", applied, err)
	}
	// 新列可写可读。
	daily := 62.5
	if err := s.InsertQuotaSample(context.Background(), &QuotaSample{
		At: 2, Account: "yanjian", DailyRemaining: &daily, OverageBalanceMicros: -1105665,
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListQuotaSamples(context.Background(), "yanjian", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[1].OverageBalanceMicros != -1105665 {
		t.Fatalf("rows = %+v", rows)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 第二次 Open：0007 已登记，幂等不重复 ALTER。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	defer func() { _ = s2.Close() }()
}

// TestMigration0015LegacyDB 模拟停在 0014 的存量库：log_cells 按
// 「登记表去掉 slack 双列」的形状建表（列序与真实 0014 形状一致），
// schema_migrations 已登记其余全部版本。Open 应补两列、旧格行新列
// 落 0、登记 0015；再次 Open 幂等不重复 ALTER。
func TestMigration0015LegacyDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	raw := openRaw(t, path)
	var names []string
	for _, m := range cellMetrics {
		if m.name == "n_slack" || m.name == "sum_slack_ms" {
			continue
		}
		names = append(names, m.name)
	}
	if _, err := raw.Exec(`CREATE TABLE log_cells (
		slot INTEGER NOT NULL, day TEXT NOT NULL, api TEXT NOT NULL,
		emodel TEXT NOT NULL, key_hash TEXT NOT NULL,
		` + strings.Join(names, ", ") + `,
		min_time INTEGER NOT NULL, last_key TEXT NOT NULL,
		PRIMARY KEY (slot, day, api, emodel, key_hash))`); err != nil {
		t.Fatal(err)
	}
	zeros := strings.TrimSuffix(strings.Repeat("0,", len(names)), ",")
	if _, err := raw.Exec(`INSERT INTO log_cells(slot,day,api,emodel,key_hash,` +
		strings.Join(names, ",") + `,min_time,last_key)
		VALUES(1,'2026-09-19','anthropic','m-a','kh1',` + zeros + `,1,'k')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE schema_migrations (version TEXT PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, m := range schemaMigrations {
		if m.version == "0015_log_cells_slack" {
			continue
		}
		if _, err := raw.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
			m.version, time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	var nSlack, sumSlack int64
	if err := s.ro.QueryRow(`SELECT n_slack, sum_slack_ms FROM log_cells`).Scan(&nSlack, &sumSlack); err != nil {
		t.Fatalf("read new columns: %v", err)
	}
	if nSlack != 0 || sumSlack != 0 {
		t.Fatalf("legacy cell slack = (%d,%d), want (0,0)", nSlack, sumSlack)
	}
	var applied int
	if err := s.ro.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version='0015_log_cells_slack'`).Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("0015 registered = %d err=%v", applied, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (idempotent): %v", err)
	}
	defer func() { _ = s2.Close() }()
}

// TestMigration0007FreshDB 新库 CREATE 已带列：迁移只登记不 ALTER，
// 重复 Open 同样幂等。
func TestMigration0007FreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		var cnt int
		if err := s.ro.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('quota_samples') WHERE name='overage_balance_micros'`).Scan(&cnt); err != nil || cnt != 1 {
			t.Fatalf("column count = %d err=%v", cnt, err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
