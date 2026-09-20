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
		affinity_hash TEXT NOT NULL DEFAULT '',
		log_source TEXT NOT NULL DEFAULT 'proxy',
		upstream_protocol TEXT NOT NULL DEFAULT 'devin',
		upstream_done_ms INTEGER
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

	// CAS 共享层（cas.go）：debug_blobs 装跨目录共享的内容切块
	// （hash=明文 sha256 截 16B，content 是 EncodePayload 编码的块
	// 明文，created_at 供 reaper 的插入宽限）；debug_chunk_refs 记
	// 「哪个文件行引用了哪些 blob」——引用即行，随删除漏斗与文件行
	// 同一 WHERE 同生死，是 mark-sweep GC 的事实源。ref 不记序号：
	// 块序由 manifest 位置表承载，refs 只回答「是否被引用」。
	`CREATE TABLE IF NOT EXISTS debug_blobs (
		hash BLOB NOT NULL PRIMARY KEY,
		content BLOB NOT NULL,
		usize INTEGER NOT NULL DEFAULT 0,
		created_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS debug_chunk_refs (
		dir TEXT NOT NULL,
		name TEXT NOT NULL,
		hash BLOB NOT NULL,
		PRIMARY KEY (dir, name, hash)
	)`,
	// hash 反查索引：reaper 的 NOT EXISTS 反连接与「哪些文件引用
	// 此 blob」的归因查询走它；PK 最左是 dir，hash 谓词借不上。
	`CREATE INDEX IF NOT EXISTS idx_debug_chunk_refs_hash ON debug_chunk_refs(hash)`,

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
	// overage_balance_micros 列尾追加（存量库由迁移 0007 幂等补齐，
	// 两条路径物理列序一致）。
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
		top_up_transaction_status TEXT NOT NULL DEFAULT '',
		overage_balance_micros INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS idx_quota_at ON quota_samples(at)`,
	// (account, at) 唯一：采样间隔以分钟计天然不撞，约束只为
	// 导入重跑（commit 后 rename 失败等断点续传场景）去重兜底。
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_quota_acct_at ON quota_samples(account, at)`,

	// gate_windows：速率闸门按对齐分钟窗口聚合的明细账，每 lane 每个
	// 被观察关闭的窗口一行（闸门在该窗口内被流量/面板/保温触碰过才有
	// 行，整窗未触碰的空窗期是缺口而非零行）。列含义见 GateWindow。
	// reject_hold 是退役列：hold 快败词已并入 quota 不再读写，列位
	// 保留——REUSEPORT 交接期旧进程仍按旧列表写入，删列会让其 INSERT
	// 全败。
	`CREATE TABLE IF NOT EXISTS gate_windows (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		lane TEXT NOT NULL DEFAULT '',
		window_start INTEGER NOT NULL,
		quota INTEGER NOT NULL DEFAULT 0,
		used_fg INTEGER NOT NULL DEFAULT 0,
		used_bg INTEGER NOT NULL DEFAULT 0,
		used_bg_ping INTEGER NOT NULL DEFAULT 0,
		drip INTEGER NOT NULL DEFAULT 0,
		retry_admits INTEGER NOT NULL DEFAULT 0,
		reserve_peak INTEGER NOT NULL DEFAULT 0,
		waiters_peak INTEGER NOT NULL DEFAULT 0,
		reject_quota INTEGER NOT NULL DEFAULT 0,
		reject_hold INTEGER NOT NULL DEFAULT 0,
		reject_bg_reserve INTEGER NOT NULL DEFAULT 0,
		reject_latch INTEGER NOT NULL DEFAULT 0,
		reject_yield INTEGER NOT NULL DEFAULT 0,
		fg_rate REAL NOT NULL DEFAULT 0
	)`,
	// (lane, window_start) 唯一：单 lane 每窗口至多一行；reuseport
	// 交接期新旧两进程并发观察同一窗口时后写者被 OR IGNORE 丢弃。
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_gate_windows_lane_ws ON gate_windows(lane, window_start)`,
	// 保留期清理（PruneGateWindows 按 window_start 范围删）的支点；
	// lane 复合索引的第二列借不上纯 window_start 谓词。
	`CREATE INDEX IF NOT EXISTS idx_gate_windows_ws ON gate_windows(window_start)`,

	// lane_attempt_causes：号池被放弃 lane 尝试的日粒度聚合账
	// （meta.json 的 upstream_attempts 明细随目录淘汰后，「为什么
	// 换号」只剩这里的口径）。cause 词表的唯一事实源在 logvocab：
	// local_gate[:reason] 本地闸门快败的幻影换号（零上游发送）、
	// connect code 真实 failover 发送、nocode 无 code 传输断裂。
	// PK 以 day 打头：读（day>=?）与 prune（day<?）同走前缀范围扫。
	`CREATE TABLE IF NOT EXISTS lane_attempt_causes (
		day TEXT NOT NULL,
		lane TEXT NOT NULL,
		cause TEXT NOT NULL,
		n INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (day, lane, cause)
	)`,

	// detached_events：脱钩流完成缓存的生命周期台账（写方与词表
	// 见 detached.go 文件头）。at 记 unix 毫秒与 logs.time 同单位；
	// key 存全量语义请求哈希（与 04 标记行的 key 对照），origin_dir
	// 是首请求调试目录名。两侧请求目录都可能缺席时（claim 失败、
	// 标记被争用丢弃）这是唯一持久取证面。
	`CREATE TABLE IF NOT EXISTS detached_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER NOT NULL,
		lane TEXT NOT NULL DEFAULT '',
		"key" TEXT NOT NULL DEFAULT '',
		origin_dir TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT ''
	)`,
	// 保留期清理（PruneDetachedEvents 按 at 范围删）的支点。
	`CREATE INDEX IF NOT EXISTS idx_detached_events_at ON detached_events(at)`,

	// store_opens：开库台账——每次 Open 落一行进程身份（at/pid/argv/
	// build/path），补 stderr 留痕的盲区：stderr 被丢弃的 opener 仍
	// 在库内可枚举（reuseport 交接进程曾静默持锁三天，stderr 无迹）。
	// 行数由 Open 内 keep-last-N 修剪自界，故不设保留期支点索引——
	// PK 顺序即修剪与枚举序。
	`CREATE TABLE IF NOT EXISTS store_opens (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at INTEGER NOT NULL,
		pid INTEGER NOT NULL,
		argv TEXT NOT NULL DEFAULT '',
		build TEXT NOT NULL DEFAULT '',
		path TEXT NOT NULL DEFAULT ''
	)`,

	// detached_blobs：脱钩流完成缓存的跨进程种子——completed 条目
	// finish 时把缓冲事件序列编成一条 blob 落库，REUSEPORT 交接后新
	// 进程开机按 lane+TTL 灌回注册表（写方与编解码见 adapter/devin
	// detached_blob.go）。key 是语义请求哈希全量；finished_at 记 unix
	// 毫秒，既是播种的 TTL 判据也是同键冲突的新旧仲裁（后完成者胜）。
	`CREATE TABLE IF NOT EXISTS detached_blobs (
		"key" TEXT PRIMARY KEY,
		lane TEXT NOT NULL DEFAULT '',
		origin_dir TEXT NOT NULL DEFAULT '',
		finished_at INTEGER NOT NULL,
		payload BLOB NOT NULL
	)`,
	// 保留期清理（PruneDetachedBlobs 按 finished_at 范围删）与开机
	// 播种扫描的支点。
	`CREATE INDEX IF NOT EXISTS idx_detached_blobs_finished ON detached_blobs(finished_at)`,

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
		notes TEXT,
		api_key TEXT
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
