// 本文件是 schema_migrations 表的运行器：schema.go 的 CREATE TABLE
// 始终声明当前形状（新列可直接写进建表语句），这里的版本化迁移为
// 存量库补同样的演进——补列一律走 addColumnIfAbsent 幂等执行，新库
// 上「列已在」只登记版本不重复 ALTER。每条迁移一个版本号、一个事务
// ——apply 与版本登记同提交，不存在 DDL 成功而登记失败的半迁移窗口；
// 任一迁移失败 Open 即报错退出。
//
// 事务一律 BEGIN IMMEDIATE（immediateTx）：deferred 事务先读
// （pragma_table_info 探列）后写（ALTER 升级写锁）时，读快照与升级
// 之间被并发写挤入会吃 SQLITE_BUSY_SNAPSHOT 快败——busy_timeout 救
// 不了过期快照（9-19 prod 实证：交接进程迁移 6ms 死于此，正值在役
// 实例提交请求行）。IMMEDIATE 在 BEGIN 即取写锁、借 busy_timeout
// 排队等锁，没有读快照就没有 SNAPSHOT 类。写锁被连续占压越过单次
// 排队耐心（在役实例的长写事务）时外层按 migrationLockBudget 重试
// 整轮事务——失败即回滚还原，重跑幂等。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// migration 是一条版本化演进：version 进 schema_migrations 作幂等键，
// apply 在同一事务内执行 DDL/回填。dbtx 形参即 immediateTx 的事务
// 连接（BEGIN IMMEDIATE 已由运行器开好）。
type migration struct {
	version string
	apply   func(ctx context.Context, q dbtx) error
}

// schemaMigrations 按应用顺序登记全部迁移；新迁移追加在末尾，
// 已发布版本永不改写（乱序应用会把「已应用」判定搅乱）。
var schemaMigrations = []migration{
	{
		// usize 记录 BLOB 解压前的字节数；0 表示未压缩——存量行
		// 经 DEFAULT 0 天然落入未压缩语义，无需回填。
		version: "0001_debug_blob_usize",
		apply: func(ctx context.Context, q dbtx) error {
			for _, alter := range []struct{ table, ddl string }{
				{"debug_files", `ALTER TABLE debug_files ADD COLUMN usize INTEGER NOT NULL DEFAULT 0`},
				{"debug_chunks", `ALTER TABLE debug_chunks ADD COLUMN usize INTEGER NOT NULL DEFAULT 0`},
			} {
				if err := addColumnIfAbsent(ctx, q, alter.table, "usize", alter.ddl); err != nil {
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
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "auth_tokens", "class",
				`ALTER TABLE auth_tokens ADD COLUMN class TEXT NOT NULL DEFAULT 'fg'`)
		},
	},
	{
		// upstream_accounts 的号池可空列：新库 CREATE 已带，存量库
		// 在这里幂等补齐。
		version: "0003_upstream_accounts_pool_columns",
		apply: func(ctx context.Context, q dbtx) error {
			for _, col := range []struct{ name, ddl string }{
				{"priority", `ALTER TABLE upstream_accounts ADD COLUMN priority INTEGER`},
				{"max_rpm", `ALTER TABLE upstream_accounts ADD COLUMN max_rpm INTEGER`},
				{"notes", `ALTER TABLE upstream_accounts ADD COLUMN notes TEXT`},
			} {
				if err := addColumnIfAbsent(ctx, q, "upstream_accounts", col.name, col.ddl); err != nil {
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
		apply: func(ctx context.Context, q dbtx) error {
			if _, err := q.ExecContext(ctx, `DROP INDEX IF EXISTS idx_logs_dir`); err != nil {
				return err
			}
			_, err := q.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_logs_dir ON logs(dir) WHERE dir != ''`)
			return err
		},
	},
	{
		// gate_windows 是全新表：幂等建表路径（schema.go）已覆盖新库
		// 与存量库，这里登记版本让 schema_migrations 如实反映演进史。
		version: "0005_gate_windows",
		apply: func(context.Context, dbtx) error {
			return nil
		},
	},
	{
		// log_cells/log_err_cells 预聚合表：表本身由 applySchema 幂等
		// 建好，这里一次性回填存量行并把覆盖水位线钉在当前 MAX(id)。
		// 之后新写入走双写（WriteDebugBatch/InsertLog 同事务），
		// importIndex 的缺口由 ReconcileCells 闭合——水位线语义是
		// 「id ≤ 它的非 rejected 行都已记进 rollup」。
		version: "0006_log_cells",
		apply: func(ctx context.Context, q dbtx) error {
			if _, err := q.ExecContext(ctx, cellsGapSQL, 0, int64(math.MaxInt64)); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, errCellsGapSQL, 0, int64(math.MaxInt64)); err != nil {
				return err
			}
			var maxID int64
			if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM logs`).Scan(&maxID); err != nil {
				return err
			}
			return setCellsWatermark(ctx, q, maxID)
		},
	},
	{
		// quota_samples.overage_balance_micros：上游 planStatus 的
		// 欠费余额账本（micros，负值=负债），粒度远细于整数百分比，
		// 是分辨「真零燃烧」与「付费燃烧走 overage 通道」的信号。
		// 存量行经 DEFAULT 0 落位——proto3 缺席键本就编码 0。
		version: "0007_quota_samples_overage_micros",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "quota_samples", "overage_balance_micros",
				`ALTER TABLE quota_samples ADD COLUMN overage_balance_micros INTEGER NOT NULL DEFAULT 0`)
		},
	},
	{
		// lane_attempt_causes 是全新表：幂等建表路径（schema.go）
		// 已覆盖新库与存量库，这里登记版本如实反映演进史；历史
		// upstream_attempts 只活在 meta.json 里不可回填，表从
		// 部署后新写入起累计。
		version: "0008_lane_attempt_causes",
		apply: func(context.Context, dbtx) error {
			return nil
		},
	},
	{
		// debug_blobs/debug_chunk_refs 是 CAS 新表：幂等建表路径
		// （schema.go）覆盖新库与存量库，登记版本让演进史如实反映
		//（同 0005_gate_windows 先例）。存量 01 行不回填——旧行保持
		// 原编码可读，随保留期自然淘汰，不回写历史。
		version: "0009_debug_cas_tables",
		apply: func(context.Context, dbtx) error {
			return nil
		},
	},
	{
		// upstream_accounts.api_key 是 durable mint key 列（cog_*），
		// 与 token/credentials_file 并列的第三种凭据来源；可空——NULL
		// 即「无行覆盖」，读侧回落 config 值（同 0003 列的语义）。
		version: "0010_upstream_accounts_api_key",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "upstream_accounts", "api_key",
				`ALTER TABLE upstream_accounts ADD COLUMN api_key TEXT`)
		},
	},
	{
		// logs.affinity_hash 把号池选号的会话谱系亲和键落到摘要行——
		// 与 meta.json 的 affinity_hash 同源（SessionAffinityKey 的
		// SHA-256，不可逆）。此前谱系分析（绑定谱系/warm 救援/failover
		// 同族）只能逐 dir 解码 meta.json，落列后 GROUP BY 一行可查。
		// 管线前拒绝与非号池路径留空串，与 key_hash 同口径。
		version: "0011_logs_affinity_hash",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "logs", "affinity_hash",
				`ALTER TABLE logs ADD COLUMN affinity_hash TEXT NOT NULL DEFAULT ''`)
		},
	},
	{
		// gate_windows.reject_yield 是让位快败的窗口账：兄弟 lane 有
		// 余量时闸门提前放给 failover 的拒绝数。存量行经 DEFAULT 0
		// 落位——部署前没有让位语义，0 即真实值。
		version: "0012_gate_windows_reject_yield",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "gate_windows", "reject_yield",
				`ALTER TABLE gate_windows ADD COLUMN reject_yield INTEGER NOT NULL DEFAULT 0`)
		},
	},
	{
		// gate_windows.retry_admits 是同 lane 续试重发的放行账
		//（reopen/续轮/凭据自愈/瞬时重试——used_* 的子集）：放行中
		// 的重试份额由此可测，此前只能整窗回推 logs 残差。存量行
		// 经 DEFAULT 0 落位——部署前没有续试标记，0 即真实值。
		version: "0013_gate_windows_retry_admits",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "gate_windows", "retry_admits",
				`ALTER TABLE gate_windows ADD COLUMN retry_admits INTEGER NOT NULL DEFAULT 0`)
		},
	},
	{
		// gate_windows.used_bg_ping 是保温 ping 的放行账（used_bg 的
		// 子集）：used_bg 把真实 bg 需求与保温 ping 混计，本分列后
		// used_bg-used_bg_ping 即真实需求。存量行经 DEFAULT 0 落位
		// ——历史行的 ping 份额只活在 used_bg 合计里，不可回补。
		version: "0014_gate_windows_used_bg_ping",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "gate_windows", "used_bg_ping",
				`ALTER TABLE gate_windows ADD COLUMN used_bg_ping INTEGER NOT NULL DEFAULT 0`)
		},
	},
	{
		// log_cells 的 slack 双列是「死读者写阻塞」尾部的可加账：
		// slack = MAX(0, duration_ms − first_client_ms)，只在首字节
		// 确已下发（first_client_ms > 0）的行上有定义——缺席/零值行
		// 贡献 0 且不计 n_slack。存量格经 DEFAULT 0 落位；历史 slack
		// 可由 logs 源列经 BackfillCells 窗口重算回补（投影同源），
		// 迁移自身不做全量重算。
		version: "0015_log_cells_slack",
		apply: func(ctx context.Context, q dbtx) error {
			for _, col := range []struct{ name, ddl string }{
				{"n_slack", `ALTER TABLE log_cells ADD COLUMN n_slack INTEGER NOT NULL DEFAULT 0`},
				{"sum_slack_ms", `ALTER TABLE log_cells ADD COLUMN sum_slack_ms INTEGER NOT NULL DEFAULT 0`},
			} {
				if err := addColumnIfAbsent(ctx, q, "log_cells", col.name, col.ddl); err != nil {
					return err
				}
			}
			return nil
		},
	},
	{
		// logs.upstream_done_ms 是泵收完上游事件流的时刻标记（同属
		// 延迟分解可空列）：非流式攒完整条流才一次性写出，egress 段
		// 须以流末为基线（first_client−upstream_done）——以
		// first_upstream 为基线量到的是剩余上游时长而非出口延迟。
		// 存量行经 NULL 落位=「未记录」，与可空语义一致无需回填。
		version: "0016_logs_upstream_done_ms",
		apply: func(ctx context.Context, q dbtx) error {
			return addColumnIfAbsent(ctx, q, "logs", "upstream_done_ms",
				`ALTER TABLE logs ADD COLUMN upstream_done_ms INTEGER`)
		},
	},
	{
		// detached_events 是全新表：幂等建表路径（schema.go）已覆盖新库
		// 与存量库，这里登记版本让 schema_migrations 如实反映演进史
		//（同 0005_gate_windows / 0008_lane_attempt_causes 先例）。
		version: "0017_detached_events",
		apply: func(context.Context, dbtx) error {
			return nil
		},
	},
	{
		// store_opens 是全新表：幂等建表路径（schema.go）已覆盖新库
		// 与存量库，这里登记版本如实反映演进史（同 0017 先例）。历史
		// 开库事件本就无迹可考，表从部署后新打开起累计。
		version: "0018_store_opens",
		apply: func(context.Context, dbtx) error {
			return nil
		},
	},
	{
		// detached_blobs 是全新表：幂等建表路径（schema.go）已覆盖新库
		// 与存量库，这里登记版本让 schema_migrations 如实反映演进史
		//（同 0017_detached_events 先例）。0018_store_opens 由并行分支
		// feat/store-open-ledger 占用，本迁移取下一空号。
		version: "0019_detached_blobs",
		apply: func(_ *sql.Tx) error {
			return nil
		},
	},
}

// migrationLockBudget 是整轮迁移等写锁的墙钟预算：BEGIN IMMEDIATE 在
// BEGIN 即取写锁、busy_timeout(30s) 已在锁等待内部排队，仍失败说明锁
// 被连续占压越界（部署窗口在役实例的长写事务——busy_timeout 正是为
// 吸收这类交接期锁等待风暴从 5s 提到 30s，再越界时重试整轮事务比让
// Open 早夭便宜：事务回滚即还原，重跑幂等）。预算罩在 spawn_handoff
// 的 3min 就绪窗内。
const migrationLockBudget = 90 * time.Second

// migrationBusyBackoff 是两次 IMMEDIATE 重试间的停顿：busy_timeout 已
// 在锁内部等过一轮，外层只错开重入点，退避取秒级即可。
const migrationBusyBackoff = time.Second

// addColumnIfAbsent 在目标列缺席时执行 ALTER。新库的 CREATE 可能已
// 带入该列（列定义以 schema.go 为准时），而中途建出的库也可能带列
// 却无迁移登记——只在缺席时补列，两种来源都不撞 duplicate column。
func addColumnIfAbsent(ctx context.Context, q dbtx, table, column, ddl string) error {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
		table, column).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, ddl)
	return err
}

// isSQLiteBusy 判 busy 族错误：主码 &0xff == SQLITE_BUSY 把
// BUSY_SNAPSHOT/BUSY_TIMEOUT 等扩展码一并收入。
func isSQLiteBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// applyMigrations 按序执行全部未应用的迁移，每条在自己的 BEGIN
// IMMEDIATE 事务里应用并登记版本。须在 applySchema 之后调用
// （schema_migrations 表本身由幂等建表保证存在）。
func applyMigrations(ctx context.Context, db *sql.DB) error {
	applied := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
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
	lockDeadline := time.Now().Add(migrationLockBudget)
	for _, m := range schemaMigrations {
		if applied[m.version] {
			continue
		}
		for {
			err := immediateTx(ctx, db, "migration "+m.version, func(ctx context.Context, q dbtx) error {
				if err := m.apply(ctx, q); err != nil {
					return fmt.Errorf("apply: %w", err)
				}
				if _, err := q.ExecContext(ctx,
					`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
					m.version, time.Now().UnixMilli()); err != nil {
					return fmt.Errorf("record: %w", err)
				}
				return nil
			})
			if err == nil {
				break
			}
			if !isSQLiteBusy(err) || time.Now().After(lockDeadline) {
				return fmt.Errorf("migration %s: %w", m.version, err)
			}
			slog.Warn("migration lock wait exceeded busy_timeout, retrying",
				"version", m.version, "err", err)
			select {
			case <-ctx.Done():
				return fmt.Errorf("migration %s: %w", m.version, ctx.Err())
			case <-time.After(migrationBusyBackoff):
			}
		}
	}
	return nil
}
