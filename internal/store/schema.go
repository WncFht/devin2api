package store

import (
	"database/sql"
)

// schemaStatements 是全量建表/建索引 DDL，逐条幂等执行（CREATE
// IF NOT EXISTS）。存量库的列演进（ALTER/回填）走 migrations.go 的
// 版本化迁移；冗余索引等历史对象由 applySchema 末尾的 DROP IF
// EXISTS 幂等清残。
var schemaStatements = []string{
	// logs：每完成请求一行，列镜像 LogRow 全集（logColumnList 是
	// 代码层单一事实源），外加 minute_bucket（time/60000，聚合索引
	// 支点）、log_source（proxy/manual_test，写入时定版）与
	// upstream_protocol（恒 devin，保留过滤维度的统一形状）。
	`CREATE TABLE IF NOT EXISTS logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		dir TEXT NOT NULL,
		time INTEGER NOT NULL,
		minute_bucket INTEGER NOT NULL,
		started_at TEXT NOT NULL,
		duration_ms INTEGER NOT NULL DEFAULT 0,
		request_ready_ms INTEGER,
		upstream_sent_ms INTEGER,
		upstream_open_ms INTEGER,
		first_upstream_ms INTEGER,
		first_client_ms INTEGER,
		api TEXT NOT NULL DEFAULT '',
		method TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		result TEXT NOT NULL DEFAULT '',
		requested_model TEXT NOT NULL DEFAULT '',
		model TEXT NOT NULL DEFAULT '',
		response_model TEXT NOT NULL DEFAULT '',
		model_mismatch INTEGER NOT NULL DEFAULT 0,
		stream INTEGER NOT NULL DEFAULT 0,
		input_tokens INTEGER NOT NULL DEFAULT 0,
		output_tokens INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens INTEGER NOT NULL DEFAULT 0,
		cache_write_tokens INTEGER NOT NULL DEFAULT 0,
		reasoning_tokens INTEGER NOT NULL DEFAULT 0,
		total_tokens INTEGER NOT NULL DEFAULT 0,
		credit_cost INTEGER NOT NULL DEFAULT 0,
		upstream_request_id TEXT NOT NULL DEFAULT '',
		client_ip TEXT NOT NULL DEFAULT '',
		key_hash TEXT NOT NULL DEFAULT '',
		client_request_id TEXT NOT NULL DEFAULT '',
		error_stage TEXT NOT NULL DEFAULT '',
		error_message TEXT NOT NULL DEFAULT '',
		dropped_events INTEGER NOT NULL DEFAULT 0,
		retry_after_seconds INTEGER NOT NULL DEFAULT 0,
		rate_limited INTEGER NOT NULL DEFAULT 0,
		retries INTEGER NOT NULL DEFAULT 0,
		account TEXT NOT NULL DEFAULT '',
		account_switches INTEGER NOT NULL DEFAULT 0,
		premature_end_turn INTEGER NOT NULL DEFAULT 0,
		repairs INTEGER NOT NULL DEFAULT 0,
		conn_reused INTEGER,
		conn_idle_ms INTEGER,
		log_source TEXT NOT NULL DEFAULT 'proxy',
		upstream_protocol TEXT NOT NULL DEFAULT 'devin'
	)`,
	// 部分唯一索引：dir='' 的 rejected 留存行没有目录身份，不入
	// 唯一约束——全列 UNIQUE 会让第二条拒绝行永久撞约束失败。
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_logs_dir ON logs(dir) WHERE dir != ''`,
	`CREATE INDEX IF NOT EXISTS idx_logs_time_status ON logs(time, status_code)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_time_model ON logs(time, model)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_minute_model ON logs(minute_bucket, model)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_minute_api ON logs(minute_bucket, api)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_time_keyhash ON logs(time, key_hash)`,
	`CREATE INDEX IF NOT EXISTS idx_logs_minute_keyhash_status ON logs(minute_bucket, key_hash, status_code)`,
	// 生效模型（model 退化 requested_model）的表达式索引：面板的
	// GROUP BY emodel / DISTINCT / 「每模型最近 N 条」相关 LIMIT
	// 全走它——emodel 是 CASE 表达式，不可索引化时这些查询全是
	// 全表扫+排序。
	`CREATE INDEX IF NOT EXISTS idx_logs_emodel_id ON logs((CASE WHEN model != '' THEN model ELSE requested_model END), id)`,
	// 429/限流行的部分索引：rateLimitEvents 的「最近 N 条」ORDER BY id
	// DESC 直接由它供序，扫描体积=命中行数而非全表；不匹配的行不进
	// 索引，InsertLog 为常态行付的写代价≈0。
	`CREATE INDEX IF NOT EXISTS idx_logs_limited_id ON logs(id) WHERE status_code = 429 OR rate_limited != 0`,

	// log_cells：logs 的 600 秒预聚合 rollup（cells.go 登记表派生
	// DDL）——重聚合端点按格子 SUM 替代全窗行扫描；rejected 行不
	// 进表（口径内建剔除）。log_err_cells 是错误阶段的稀疏迷你表
	//（只记 error_stage != '' 的行），serve error_stages 聚合。
	// 除主键外不加索引：格子表本身体积小，范围扫已足够。
	logCellsDDL,
	logErrCellsDDL,

	// debug payload：键是目录名（dir 仍作 X-Request-Id/debug_ref
	// 身份），不是 logs.id——飞行中请求的 payload 先于 Complete 才
	// 落库的 logs 行存在，进程被杀的请求也可能只剩调试行。
	// debug_files 承载一次性小文件（meta.json、
	// error.json、attachments/*），error.json 的 first-write-wins
	// 靠 INSERT OR IGNORE 表达；debug_chunks 承载流式 JSONL
	// （04/05/06），每次 flush 批一行，读时 ORDER BY seq 拼接。
	`CREATE TABLE IF NOT EXISTS debug_files (
		dir TEXT NOT NULL,
		name TEXT NOT NULL,
		content BLOB NOT NULL,
		updated_at INTEGER NOT NULL,
		usize INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (dir, name)
	)`,
	`CREATE TABLE IF NOT EXISTS debug_chunks (
		dir TEXT NOT NULL,
		name TEXT NOT NULL,
		seq INTEGER NOT NULL,
		data BLOB NOT NULL,
		usize INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (dir, name, seq)
	)`,
	// name 单列索引：清理器的 error.json 目录枚举（DebugDirsContaining
	// 与 DebugErrorSignatures 的 name=? 探针）走它直接定位；PK 最左列
	// 是 dir，name 谓词借不上，无索引时每轮清理全扫两张 blob 大表。
	`CREATE INDEX IF NOT EXISTS idx_debug_files_name ON debug_files(name)`,
	`CREATE INDEX IF NOT EXISTS idx_debug_chunks_name ON debug_chunks(name)`,

	// auth_tokens：列镜像 authtoken.Token 持久字段；inflight/
	// rpmBucket/rpmCount 是瞬态字段不进库。token 存 sha256 全 hex，
	// 明文不落库。
	`CREATE TABLE IF NOT EXISTS auth_tokens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		token TEXT NOT NULL UNIQUE,
		description TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL DEFAULT 0,
		expires_at INTEGER,
		last_used_at INTEGER,
		is_active INTEGER NOT NULL DEFAULT 1,
		success_count INTEGER NOT NULL DEFAULT 0,
		failure_count INTEGER NOT NULL DEFAULT 0,
		stream_avg_ttfb REAL NOT NULL DEFAULT 0,
		non_stream_avg_rt REAL NOT NULL DEFAULT 0,
		stream_count INTEGER NOT NULL DEFAULT 0,
		non_stream_count INTEGER NOT NULL DEFAULT 0,
		prompt_tokens_total INTEGER NOT NULL DEFAULT 0,
		completion_tokens_total INTEGER NOT NULL DEFAULT 0,
		cache_read_tokens_total INTEGER NOT NULL DEFAULT 0,
		cache_creation_tokens_total INTEGER NOT NULL DEFAULT 0,
		total_cost_usd REAL NOT NULL DEFAULT 0,
		effective_cost_usd REAL NOT NULL DEFAULT 0,
		cost_used_microusd INTEGER NOT NULL DEFAULT 0,
		cost_limit_microusd INTEGER NOT NULL DEFAULT 0,
		cost_daily_used_microusd INTEGER NOT NULL DEFAULT 0,
		cost_daily_limit_microusd INTEGER NOT NULL DEFAULT 0,
		cost_daily_period_start INTEGER NOT NULL DEFAULT 0,
		cost_monthly_used_microusd INTEGER NOT NULL DEFAULT 0,
		cost_monthly_limit_microusd INTEGER NOT NULL DEFAULT 0,
		cost_monthly_period_start INTEGER NOT NULL DEFAULT 0,
		cost_5h_used_microusd INTEGER NOT NULL DEFAULT 0,
		cost_5h_limit_microusd INTEGER NOT NULL DEFAULT 0,
		cost_5h_anchor INTEGER NOT NULL DEFAULT 0,
		cost_weekly_used_microusd INTEGER NOT NULL DEFAULT 0,
		cost_weekly_limit_microusd INTEGER NOT NULL DEFAULT 0,
		cost_weekly_period_start INTEGER NOT NULL DEFAULT 0,
		allowed_models TEXT NOT NULL DEFAULT '[]',
		max_concurrency INTEGER NOT NULL DEFAULT 0,
		max_rpm INTEGER NOT NULL DEFAULT 0,
		class TEXT NOT NULL DEFAULT 'fg'
	)`,

	`CREATE TABLE IF NOT EXISTS model_registry (
		model TEXT PRIMARY KEY,
		redirect_model TEXT NOT NULL DEFAULT '',
		disabled INTEGER NOT NULL DEFAULT 0,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,

	`CREATE TABLE IF NOT EXISTS settings (
		"key" TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,

	// quota_samples：daily/weekly_remaining 可空 REAL 保留
	// 「上游没报」与「真到 0」的区分（QuotaSample 的 *float64 语义）。
	`CREATE TABLE IF NOT EXISTS quota_samples (
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
	)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_at ON quota_samples(at)`,
	// (account, at) 唯一：采样间隔以分钟计天然不撞，约束只为
	// 导入重跑（commit 后 rename 失败等断点续传场景）去重兜底。
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_acct_at ON quota_samples(account, at)`,

	// gate_windows：速率闸门按对齐分钟窗口聚合的明细账，每 lane 每个
	// 被观察关闭的窗口一行（闸门在该窗口内被流量/面板/保温触碰过才有
	// 行，整窗未触碰的空窗期是缺口而非零行）。列含义见 GateWindow。
	`CREATE TABLE IF NOT EXISTS gate_windows (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		lane TEXT NOT NULL DEFAULT '',
		window_start INTEGER NOT NULL,
		quota INTEGER NOT NULL DEFAULT 0,
		used_fg INTEGER NOT NULL DEFAULT 0,
		used_bg INTEGER NOT NULL DEFAULT 0,
		drip INTEGER NOT NULL DEFAULT 0,
		reserve_peak INTEGER NOT NULL DEFAULT 0,
		waiters_peak INTEGER NOT NULL DEFAULT 0,
		reject_quota INTEGER NOT NULL DEFAULT 0,
		reject_hold INTEGER NOT NULL DEFAULT 0,
		reject_bg_reserve INTEGER NOT NULL DEFAULT 0,
		reject_latch INTEGER NOT NULL DEFAULT 0,
		fg_rate REAL NOT NULL DEFAULT 0
	)`,
	// (lane, window_start) 唯一：单 lane 每窗口至多一行；reuseport
	// 交接期新旧两进程并发观察同一窗口时后写者被 OR IGNORE 丢弃。
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_gate_windows_lane_ws ON gate_windows(lane, window_start)`,
	// 保留期清理（PruneGateWindows 按 window_start 范围删）的支点；
	// lane 复合索引的第二列借不上纯 window_start 谓词。
	`CREATE INDEX IF NOT EXISTS idx_gate_windows_ws ON gate_windows(window_start)`,

	// runtime_state：键值小状态。gate:<lane> 存冷却闩 JSON；
	// import_base_done / debug_dirs_imported 是导入进度标记。
	`CREATE TABLE IF NOT EXISTS runtime_state (
		"key" TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at INTEGER NOT NULL DEFAULT 0
	)`,

	// upstream_accounts：号池 lane 的持久化账号行。deleted 软删标记
	// 保留历史 lane 归因；priority/max_rpm/notes 是全可空列——NULL
	// 语义是「无行覆盖」，读侧回落 config 值或零值。存量库的补齐
	// 走 ensureAccountColumns 的幂等 ALTER（列加在尾部，两条路径
	// 的物理列序一致）。
	`CREATE TABLE IF NOT EXISTS upstream_accounts (
		name TEXT PRIMARY KEY,
		token TEXT,
		credentials_file TEXT,
		disabled INTEGER NOT NULL DEFAULT 0,
		deleted INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL,
		updated_at INTEGER NOT NULL,
		priority INTEGER,
		max_rpm INTEGER,
		notes TEXT
	)`,

	// schema_migrations：版本化迁移登记表，migrations.go 的 runner
	// 读写——version 作幂等键，存量库的列演进经它逐版本推进。
	`CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`,
}

// applySchema 顺序执行全部 DDL；幂等，可重复调用。末尾清一次历史
// 残留对象：idx_logs_time 被 idx_logs_time_status 最左前缀完全覆盖
// （time 范围/排序走后者等价），旧库删它省掉每行白付的一份索引写。
// 存量库的列演进不在此做——统一走 migrations.go 的版本化迁移。
func applySchema(db *sql.DB) error {
	for _, stmt := range schemaStatements {
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	_, err := db.Exec(`DROP INDEX IF EXISTS idx_logs_time`)
	return err
}
