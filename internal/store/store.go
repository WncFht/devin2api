// Package store 是全部面板可见状态的 SQLite 持久层：请求日志、调试
// payload、下游令牌、模型注册表、面板设置、配额采样与运行时小状态。
// 它取代原先的 index.jsonl/auth_tokens.json/models.json/
// panel-settings.json/quota.jsonl/gate-state*.json 文件持久化。
//
// 读写分离双池：写只走 db（单连接串行化——ccLoad 实证 SQLite 多连接
// 高并发写触发 BUSY/DEADLOCK）；读只走 ro（WAL 下多读者与写者并行，
// query_only pragma 在连接级拒绝写语句）。面板聚合读不再与日志写、
// 配额采样、retention 清扫互相排队。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Store 包装读写两个 *sql.DB，对外只暴露领域方法。
type Store struct {
	// db 是写池：MaxOpenConns=1，串行化全部写（含 tx）与必要的
	// 写后读（import 流程）。ro 是读池：只跑 SELECT——靠约定维持，
	// query_only pragma 兜底把误写变成显式错误而非静默写竞争。
	db wconn
	ro *sql.DB

	path string
	// debugBytes 是 debug payload 四张表库存字节合计的内存镜像
	// （DebugDirSizes + DebugBlobBytes 总量同口径）：Open 时从持久化行
	// 播种（行缺席则 0 起步、异步协程重建后置换），各写/删方法在事务
	// 提交后按真实落库字节增减；cleaner 的容量闸读它免每轮全表聚合，
	// 周期对账兜底漏记账路径。
	debugBytes atomic.Int64
	// seedDone 在异步播种协程退出时关闭（持久化行命中的快路径下为
	// nil）；seedCancel 供 Close 中止仍在跑的聚合——sql.DB.Close 会
	// 等在飞查询结束，不先取消会把关库拖成聚合时长（GB 级库秒级以上）。
	seedDone   chan struct{}
	seedCancel context.CancelFunc
}

// slowWriteWarn 是写连接独占时长的告警线：远低于写调用方的
// storeOpTimeout(2min) 死线，高于常规批量写的正常量级。单连接写池
// 下一个 op 超时独占会让全部排队写者等待——recorder 侧的
// pending_bytes/late_writes 只能看到排队深度，看不到占用者是谁。
const slowWriteWarn = 2 * time.Second

// wconn 包装写池 *sql.DB：单发语句的计时起点取「拿到连接之后」——
// Conn(ctx) 的排队等待不计入，只剩连接上的真实执行时长，慢告警指名
// 的是真正的占用者而不是被堵住的排队者。
type wconn struct{ *sql.DB }

// ExecContext 语义同 *sql.DB.ExecContext，执行超 slowWriteWarn 记 WARN。
func (w wconn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	conn, err := w.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	start := time.Now()
	res, err := conn.ExecContext(ctx, query, args...)
	warnSlowWrite(sqlLabel(query), start)
	return res, err
}

// writeTx 开写事务并返回收尾回调（Rollback+慢占用告警），调用方 defer
// 它替代裸的 Rollback defer：Begin 成功到 Commit/Rollback 的全程独占
// 唯一写连接，含语句间的本地工作（payload 编码、目录遍历等）。
func (s *Store) writeTx(ctx context.Context, op string) (*sql.Tx, func(), error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	start := time.Now()
	return tx, func() {
		// 已提交的 Rollback 是免锁 no-op；失败路径的回滚耗时也计入占用窗。
		_ = tx.Rollback()
		warnSlowWrite(op, start)
	}, nil
}

// warnSlowWrite 在占用超阈值时按 op 名记一条 WARN。
func warnSlowWrite(op string, start time.Time) {
	if d := time.Since(start); d >= slowWriteWarn {
		slog.Warn("slow write-conn hold", "op", op, "duration_ms", d.Milliseconds())
	}
}

// sqlLabel 取 SQL 首行前缀作 op 名：单发 exec 没有具名入口，语句的
// 动词+表名前缀已足以指认占用者。
func sqlLabel(query string) string {
	if i := strings.IndexByte(query, '\n'); i >= 0 {
		query = query[:i]
	}
	query = strings.TrimSpace(query)
	if len(query) > 64 {
		query = query[:64]
	}
	return query
}

// IsBusy 报告 err 是否为 SQLite 的「数据库文件被锁」失败（原始
// SQLITE_BUSY）。busy_timeout 只兜住连接级等待耐心：跨进程写者（reuseport
// 交接期持锁排空的前任进程、侧开 store 的工具）持锁超 30s 时驱动原样抛
// *sqlite.Error；tx.Commit 经 driver.Tx 接口拿不到 ctx（驱动内跑
// context.Background()），其唯一上界正是 busy_timeout，故请求级 5s ctx
// 之下 BUSY 仍可达——生产 9-19 实证 +5~36s 的「database is locked」簇。
// 驱动建连即开 extended result codes，Code() 低 8 位才是主码——
// BUSY_SNAPSHOT/RECOVERY/TIMEOUT 同属「此刻拿不到文件锁」，对整调用
// 重试（新事务）均可救，故按主码判。
func IsBusy(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.As(err, &sqliteErr) && sqliteErr.Code()&0xff == sqlite3.SQLITE_BUSY
}

// maintDeleteChunkRows 是保留期删除单片的行数上界：logs 行连带 ~10
// 个索引项维护，存量差触发点（面板调小 log_row_retention_days、批量
// 回填）下无界 DELETE 会秒级独占唯一写连接；分片后每片一条独立语句
// 自带隐式事务提交，片间把连接让回池，排队写者按请求序插队。
const maintDeleteChunkRows = 5000

// deleteRowsChunked 把「pred 命中的行全删」拆成 rowid 定批的逐片
// 删除。谓词即游标——已删行不再命中，中途失败或进程重启后下一轮
// 重跑幂等续删，无需持久化进度；内层 SELECT 走 pred 的既有索引支点，
// 只物化单片 rowid。供 Maintain 的按龄保留清理使用。
func (s *Store) deleteRowsChunked(ctx context.Context, table, pred string, args ...any) (int64, error) {
	query := `DELETE FROM ` + table + ` WHERE rowid IN (SELECT rowid FROM ` + table + ` WHERE ` + pred + ` LIMIT ?)`
	delArgs := append(args, maintDeleteChunkRows)
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, query, delArgs...)
		if err != nil {
			return total, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < maintDeleteChunkRows {
			return total, nil
		}
	}
}

// buildStamp 返回本进程二进制的构建标识：module version（go install
// module@version 装的）优先，其次 VCS 短 sha——dirty 尾缀标记未提交
// 工作区，是手搓/树外二进制与部署二进制的区分信号。无构建信息
// （-buildvcs=false、树外脚本构建）回 "unknown"。
func buildStamp() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if modified == "true" {
		rev += "-dirty"
	}
	return rev
}

// Open 打开（或创建）path 处的库。schema 幂等，重复打开只做
// CREATE IF NOT EXISTS；是否跑 ImportLegacy 由导入器按源文件
// 存在性自判，Open 不报告 created。
func Open(path string) (*Store, error) {
	openStart := time.Now()
	stageStart := openStart
	// 开库留痕打在干活之前：stderr 落点由调用方的 shell 决定——systemd
	// 实例与 deploy 交接进程汇聚进 stderr.log，probe/census/手搓二进制
	// 落各自终端——凡经 Open 碰库的进程都留一条「谁（argv 含二进制名
	// 与 flag 级 subcommand）、开哪个库（绝对路径）、什么构建」；打开
	// 中途挂死也有这条线在，与结尾的 "store opened" 成对夹住全程。
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	slog.Info("store opening", "path", absPath, "argv", os.Args, "build", buildStamp())
	// synchronous=NORMAL：WAL 下 commit 不再逐次 fsync（帧留在 OS 页缓存，
	// 进程崩溃不丢，仅断电/内核崩可能丢尾部事务，不产生损坏）。本库
	// 装的是可重建的观测与面板状态，用这丁点断电尾部风险换 commit 风暴
	// 期间的 fsync 开销。busy_timeout 放到 30s：reuseport 交接期新旧
	// 双写者并存，5s 曾在分钟级重叠窗内打出成片 SQLITE_BUSY 失败写，
	// 30s 吸收这类部署期锁等待风暴（单写者设计不变，只改等待耐心）。
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_pragma=journal_mode=WAL&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(500)&_loc=Local", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// 单连接是刻意的：写路径无并发诉求，串行化免除锁竞争调参。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	pingMS := time.Since(stageStart).Milliseconds()
	stageStart = time.Now()
	// auto_vacuum 只能在新库（无用户表）建表前开启：VACUUM 对空库
	// 只是把头部位写进文件，不重写业务数据；旧库不在启动路径做。
	if err := enableAutoVacuumOnEmpty(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := applySchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	schemaMS := time.Since(stageStart).Milliseconds()
	stageStart = time.Now()
	// 列演进走版本化迁移：幂等建表只管新库全量 DDL，存量库的
	// ALTER/回填由 runner 按 schema_migrations 登记跳过。
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	migrationsMS := time.Since(stageStart).Milliseconds()
	stageStart = time.Now()
	// payload 计数器从 runtime_state 持久化行 O(1) 播种：全部写/删
	// 路径在各自事务内对该行做净增量，四项加数口径与
	// DebugDirSizes + DebugBlobBytes 的合计逐项对应。行缺席（升级
	// 首启、全新库、行被手删）不在这里跑聚合——四表全扫在 5.6GB 库
	// 实测 ~26s，会挡住就绪；改由 Open 返回后异步重建（见
	// seedPayloadBytesAsync），窗内增量经 pending 行收编不丢账。
	debugBytes, persisted, err := seedDebugPayloadBytes(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed debug payload bytes: %w", err)
	}
	seedMS := time.Since(stageStart).Milliseconds()
	stageStart = time.Now()

	// 读池在写库建好 schema 之后打开：WAL 下读者拿连接级快照，
	// 与写者互不阻塞。并发数取面板页一次加载的端点扇出量级。
	ro, err := sql.Open("sqlite", dsn+"&_pragma=query_only(1)")
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open read pool: %w", err)
	}
	ro.SetMaxOpenConns(4)
	ro.SetMaxIdleConns(4)
	if err := ro.Ping(); err != nil {
		// 读池起不来时退化回写池串行读——功能不变，只是失去并行。
		_ = ro.Close()
		ro = db
	}
	readpoolMS := time.Since(stageStart).Milliseconds()
	stageStart = time.Now()
	st := &Store{db: wconn{db}, ro: ro, path: path}
	st.debugBytes.Store(debugBytes)
	// 水位自愈：任何绕过双写的写入者（无 cells 码的旧二进制、外部
	// 工具、importIndex）留下的未记账行在每次启动时补记——不做这步，
	// 缝隙会被后续双写推进的水位碾过，对 UNION 读永久隐形（9-19
	// prod 实证 7,634 行）。无缺口时退化为 id>水位 的空扫。
	if err := st.ReconcileCells(context.Background()); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("reconcile log cells: %w", err)
	}
	reconcileMS := time.Since(stageStart).Milliseconds()
	if !persisted {
		// 计数器行缺席：协程在 ro 快照上跑权威聚合，pending 行收编
		// 窗内增量——Open 就绪不被全扫阻塞，写者经 addPayloadBytes 的
		// IN 双键 UPDATE 无感切换记账目标。
		seedCtx, cancel := context.WithCancel(context.Background())
		st.seedDone = make(chan struct{})
		st.seedCancel = cancel
		go st.seedPayloadBytesAsync(seedCtx)
	}
	slog.Info("store opened",
		"path", path,
		"ping_ms", pingMS,
		"schema_ms", schemaMS,
		"migrations_ms", migrationsMS,
		"seed_ms", seedMS,
		"seed_persisted", persisted,
		"readpool_ms", readpoolMS,
		"reconcile_ms", reconcileMS,
		"total_ms", time.Since(openStart).Milliseconds(),
		"debug_payload_bytes", debugBytes)
	return st, nil
}

// vacuumMinPages 以下不值得动： freelist 太小，搬页成本换不回磁盘。
// vacuumChunkPages 给单次 incremental_vacuum 调用封顶（实测 8.3GB 库上
// 512 页约 8ms），写连接占压钳在毫秒级；无界调用在 GB 级 freelist 上会
// 长时间独占写连接，曾把 debug 写队列压到丢事件。vacuumBudget 给一轮
// Maintain 的回收总量封顶——固定页数上限在高摄入下永远追不上 freelist
// 增长（512 页/5min 是旧病：.db 停在历史高水位不再回缩），按墙钟预算
// 循环小块回收则摄入越快单轮收得越多，跨轮收敛。
const (
	vacuumMinPages   = 64
	vacuumChunkPages = 512
	vacuumBudget     = 2 * time.Second
)

// walRestartBytes 是 Maintain 主动 wal_checkpoint(RESTART) 的触发阈值。
// 正常流量下 wal_autocheckpoint(500) 把 WAL 压在 ~2MB；autocheckpoint 是
// PASSIVE 语义，读者持快照就把它饿死，WAL 可积到数百 MB。阈值取 64MB：
// 单次 RESTART 的回拷量以此封顶，配合 walCheckpointBudget 把写连接占用
// 钳在 claim 预算（5s）之下。
const walRestartBytes = 64 << 20

// walCheckpointBudget 给 Maintain 单次 checkpoint 尝试的写连接占用封顶。
// RESTART 要等全部读者越过 WAL 末尾才复位，等待上限本是 busy_timeout
// 30s——读者饥饿时每次触发都打出数十秒独占。改用短 ctx 把「等不到」变成
// 「下轮再来」：sqlite3_interrupt 中止 busy 等待与回拷，已拷页幂等无害，
// WAL 留原状待下轮收。占用界随 WAL 体积缩放（128MB/s 回拷估计）保证
// 任意大 WAL 一轮内可收——被中断的 checkpoint 不累计进度，固定 2s 会
// 让 GB 级 WAL 永远收不掉；上限对齐 busy_timeout，超出时 RESTART 自己
// 也等不住。
const (
	walCheckpointBudget    = 2 * time.Second
	walCheckpointBudgetMax = 30 * time.Second
)

// walCheckpointTimeout 按 WAL 体积给出本轮 checkpoint 的 ctx 预算：
// 吞吐估计 128MB/s 覆盖回拷 I/O，下界 2s 兜住短读者排空。
func walCheckpointTimeout(walBytes int64) time.Duration {
	scaled := time.Duration(walBytes/(128<<20)) * time.Second
	return min(max(scaled, walCheckpointBudget), walCheckpointBudgetMax)
}

// IncrementalVacuum 回收 freelist 页——auto_vacuum=INCREMENTAL 只把
// 删除页挂进 freelist，不显式跑这步 .db 文件不回缩（db_bytes 会与
// 实际占用分叉）。按 vacuumChunkPages 小块循环：每次调用间释放写连接，
// 让批量写事务与清洁工删除插队，直到 freelist 清空或耗尽墙钟预算。
func (s *Store) IncrementalVacuum(ctx context.Context) error {
	deadline := time.Now().Add(vacuumBudget)
	for {
		var free int64
		if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&free); err != nil {
			return err
		}
		if free < vacuumMinPages || !time.Now().Before(deadline) {
			return nil
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA incremental_vacuum(%d)`, vacuumChunkPages)); err != nil {
			return err
		}
	}
}

// Maintain 执行一轮库级周期养护：logs 行按龄删除（logRowDays<=0 时
// 跳过）、quota_samples 恢复到行数界、gate_windows 与
// lane_attempt_causes、detached_events 按同一保留期按龄删除、回收 freelist 页。各项互相独立，单项失败不阻断后续——错误经
// errors.Join 汇总返回，调用方记日志即可。原 debuglog.cleanOnce 的
// 收尾职责上移到这里：养护对象是库不是目录，由 main.go 的 ticker 驱动。
// gate_windows 与 logs 摘要行共用 logRowDays：两者都是「时间序列
// 明细行」的检索面，保留期同口径不单设旋钮。
func (s *Store) Maintain(ctx context.Context, logRowDays int64) error {
	var errs []error
	if logRowDays > 0 {
		cutoff := time.Now().Add(-time.Duration(logRowDays) * 24 * time.Hour)
		if _, err := s.DeleteLogsBefore(ctx, cutoff.UnixMilli()); err != nil {
			errs = append(errs, err)
		}
		if _, err := s.PruneGateWindows(ctx, cutoff.Unix()); err != nil {
			errs = append(errs, err)
		}
		if _, err := s.PruneLaneAttemptCauses(ctx, cutoff.Local().Format("2006-01-02")); err != nil {
			errs = append(errs, err)
		}
		if _, err := s.PruneDetachedEvents(ctx, cutoff.UnixMilli()); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := s.PruneQuotaSamples(ctx); err != nil {
		errs = append(errs, err)
	}
	// CAS 的 mark-sweep 放养护而不放淘汰路径：对象是表不是目录，
	// 且必须先于 vacuum 跑，孤儿页才能进 freelist 被当轮回收。
	if err := s.ReapOrphanBlobs(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.IncrementalVacuum(ctx); err != nil {
		errs = append(errs, err)
	}
	// WAL 体积纪律放在养护末尾，每轮一次 checkpoint 尝试：超阈换
	// RESTART 强制复位（读者排空等待由 walCheckpointTimeout 封顶，
	// 超时中断不算没收干净——WAL 仍超阈下一轮重试）；未超阈跑
	// PASSIVE——不等待读者、能收多少收多少，读间隙顺带复位，兜
	// autocheckpoint 够不着的空闲尾部与低度饥饿。返回行本就无人
	// 消费，走 ExecContext 顺带进入慢占用计时。
	wal := s.WALBytes()
	ckptCtx, cancel := context.WithTimeout(ctx, walCheckpointTimeout(wal))
	var err error
	if wal > walRestartBytes {
		_, err = s.db.ExecContext(ckptCtx, `PRAGMA wal_checkpoint(RESTART)`)
	} else {
		_, err = s.db.ExecContext(ckptCtx, `PRAGMA wal_checkpoint(PASSIVE)`)
	}
	cancel()
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DBBytes 返回库文件与 WAL 的磁盘占用合计（Stats 的 db_bytes 口径）。
func (s *Store) DBBytes() int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(s.path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

// WALBytes 返回 WAL 文件的磁盘占用：db_bytes 与 debug payload 口径差的
// 主要解释项——checkpoint 饥饿时 WAL 可远超主库文件，单列它让「WAL
// 顶爆 DBBytes」在面板上可见而不是事后挖掘。
func (s *Store) WALBytes() int64 {
	if info, err := os.Stat(s.path + "-wal"); err == nil {
		return info.Size()
	}
	return 0
}

// Close 关闭连接池；WAL checkpoint 由驱动在关闭时收尾。读池退化
// 复用写池时 ro==db，避免重复 Close。异步播种协程先取消再等退出——
// 它的 ro 快照聚合在 GB 级库上是秒级在飞查询，直接 Close 会被池等待
// 拖住；取消后经 ctx 中止，等待退出口径把「协程写已关闭池」噪声消掉。
func (s *Store) Close() error {
	if s.seedCancel != nil {
		s.seedCancel()
		select {
		case <-s.seedDone:
		case <-time.After(5 * time.Second):
		}
	}
	if s.ro != s.db.DB {
		_ = s.ro.Close()
	}
	return s.db.Close()
}

// enableAutoVacuumOnEmpty 在空库上开启 INCREMENTAL 真空回收；已有
// 用户表的库直接跳过（切换需完整 VACUUM，属运维动作不自动做）。
func enableAutoVacuumOnEmpty(db *sql.DB) error {
	var tables int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return fmt.Errorf("count user tables: %w", err)
	}
	if tables > 0 {
		return nil
	}
	var mode int
	if err := db.QueryRow("PRAGMA auto_vacuum").Scan(&mode); err != nil {
		return fmt.Errorf("query auto_vacuum: %w", err)
	}
	if mode == 2 {
		return nil
	}
	if _, err := db.Exec("PRAGMA auto_vacuum = INCREMENTAL"); err != nil {
		return fmt.Errorf("set auto_vacuum: %w", err)
	}
	if _, err := db.Exec("VACUUM"); err != nil {
		return fmt.Errorf("vacuum to activate auto_vacuum: %w", err)
	}
	return nil
}
