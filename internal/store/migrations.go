// 本文件是 schema_migrations 表的运行器：schema.go 的 CREATE TABLE
// 始终声明当前形状（新列可直接写进建表语句），这里的版本化迁移为
// 存量库补同样的演进——补列一律走 addColumnIfAbsent 幂等执行，新库
// 上「列已在」只登记版本不重复 ALTER。每条迁移一个版本号、一个事务
// ——apply 与版本登记同提交，不存在 DDL 成功而登记失败的半迁移窗口；
// 任一迁移失败 Open 即报错退出。
package store

import (
	"database/sql"
	"fmt"
	"time"
)

// migration 是一条版本化演进：version 进 schema_migrations 作幂等键，
// apply 在同一事务内执行 DDL/回填。
type migration struct {
	version string
	apply   func(tx *sql.Tx) error
}

// schemaMigrations 按应用顺序登记全部迁移；新迁移追加在末尾，
// 已发布版本永不改写（乱序应用会把「已应用」判定搅乱）。
var schemaMigrations = []migration{
	{
		// usize 记录 BLOB 解压前的字节数；0 表示未压缩——存量行
		// 经 DEFAULT 0 天然落入未压缩语义，无需回填。
		version: "0001_debug_blob_usize",
		apply: func(tx *sql.Tx) error {
			for _, alter := range []struct{ table, ddl string }{
				{"debug_files", `ALTER TABLE debug_files ADD COLUMN usize INTEGER NOT NULL DEFAULT 0`},
				{"debug_chunks", `ALTER TABLE debug_chunks ADD COLUMN usize INTEGER NOT NULL DEFAULT 0`},
			} {
				if err := addColumnIfAbsent(tx, alter.table, "usize", alter.ddl); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		// auth_tokens.class 标记请求类（fg 前台/bg 后台）：存量行
		// 经 DEFAULT 'fg' 落入前台语义，准入行为与加列前一致。
		version: "0002_auth_tokens_class",
		apply: func(tx *sql.Tx) error {
			return addColumnIfAbsent(tx, "auth_tokens", "class",
				`ALTER TABLE auth_tokens ADD COLUMN class TEXT NOT NULL DEFAULT 'fg'`)
		},
	},
	{
		// upstream_accounts 的号池可空列：新库 CREATE 已带，存量库
		// 在这里幂等补齐。
		version: "0003_upstream_accounts_pool_columns",
		apply: func(tx *sql.Tx) error {
			for _, col := range []struct{ name, ddl string }{
				{"priority", `ALTER TABLE upstream_accounts ADD COLUMN priority INTEGER`},
				{"max_rpm", `ALTER TABLE upstream_accounts ADD COLUMN max_rpm INTEGER`},
				{"notes", `ALTER TABLE upstream_accounts ADD COLUMN notes TEXT`},
			} {
				if err := addColumnIfAbsent(tx, "upstream_accounts", col.name, col.ddl); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		// idx_logs_dir 改部分唯一索引：dir='' 的 rejected 行没有
		// 目录身份，全列 UNIQUE 下第二条拒绝行永久撞约束失败。
		version: "0004_logs_dir_partial_unique",
		apply: func(tx *sql.Tx) error {
			if _, err := tx.Exec(`DROP INDEX IF EXISTS idx_logs_dir`); err != nil {
				return err
			}
			_, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_logs_dir ON logs(dir) WHERE dir != ''`)
			return err
		},
	},
}

// addColumnIfAbsent 在目标列缺席时执行 ALTER。新库的 CREATE 可能已
// 带入该列（列定义以 schema.go 为准时），而中途建出的库也可能带列
// 却无迁移登记——只在缺席时补列，两种来源都不撞 duplicate column。
func addColumnIfAbsent(tx *sql.Tx, table, column, ddl string) error {
	var n int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.Exec(ddl)
	return err
}

// applyMigrations 按序执行全部未应用的迁移，每条在自己的事务里
// 应用并登记版本。须在 applySchema 之后调用（schema_migrations
// 表本身由幂等建表保证存在）。
func applyMigrations(db *sql.DB) error {
	applied := map[string]bool{}
	rows, err := db.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("list applied migrations: %w", err)
	}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan applied migrations: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read applied migrations: %w", err)
	}
	_ = rows.Close()
	for _, m := range schemaMigrations {
		if applied[m.version] {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.version, err)
		}
		if err := m.apply(tx); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
			m.version, time.Now().UnixMilli()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.version, err)
		}
	}
	return nil
}
