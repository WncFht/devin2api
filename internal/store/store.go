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
	"os"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Store 包装读写两个 *sql.DB，对外只暴露领域方法。
type Store struct {
	// db 是写池：MaxOpenConns=1，串行化全部写（含 tx）与必要的
	// 写后读（import 流程）。ro 是读池：只跑 SELECT——靠约定维持，
	// query_only pragma 兜底把误写变成显式错误而非静默写竞争。
	db *sql.DB
	ro *sql.DB

	path string
	// debugBytes 是 debug_files.content 与 debug_chunks.data 库存字节
	// 合计的内存镜像（DebugDirSizes 总量同口径）：Open 时聚合播种，
	// 各写/删方法在事务提交后按真实落库字节增减；cleaner 的容量闸读它
	// 免每轮全表聚合，周期对账兜底漏记账路径。
	debugBytes atomic.Int64
}

// Open 打开（或创建）path 处的库。schema 幂等，重复打开只做
// CREATE IF NOT EXISTS；是否跑 ImportLegacy 由导入器按源文件
// 存在性自判，Open 不报告 created。
func Open(path string) (*Store, error) {
	// synchronous=NORMAL：WAL 下 commit 不再逐次 fsync（帧留在 OS 页缓存，
	// 进程崩溃不丢，仅断电/内核崩可能丢尾部事务，不产生损坏）。本库
	// 装的是可重建的观测与面板状态，用这丁点断电尾部风险换 commit 风暴
	// 期间的 fsync 开销。
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode=WAL&_pragma=synchronous(NORMAL)&_pragma=wal_autocheckpoint(500)&_loc=Local", path)
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
	// 列演进走版本化迁移：幂等建表只管新库全量 DDL，存量库的
	// ALTER/回填由 runner 按 schema_migrations 登记跳过。
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	// payload 计数器以权威聚合播种：此后全部写/删路径在各自事务提交
	// 时增减它，读侧 O(1)。一次性启动全扫替代周期全扫（原 DBBytes
	// 闸门被 WAL 撑真后每 5min 白跑一轮 GB 级聚合）。
	var debugBytes int64
	if err := db.QueryRow(`SELECT
		(SELECT COALESCE(SUM(LENGTH(content)),0) FROM debug_files) +
		(SELECT COALESCE(SUM(LENGTH(data)),0) FROM debug_chunks)`).Scan(&debugBytes); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seed debug payload bytes: %w", err)
	}

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
	st := &Store{db: db, ro: ro, path: path}
	st.debugBytes.Store(debugBytes)
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
// 跳过）、quota_samples 恢复到行数界、回收 freelist 页。三项互相
// 独立，单项失败不阻断后续——错误经 errors.Join 汇总返回，调用方
// 记日志即可。原 debuglog.cleanOnce 的收尾职责上移到这里：养护对象
// 是库不是目录，由 main.go 的 ticker 驱动。
func (s *Store) Maintain(ctx context.Context, logRowDays int64) error {
	var errs []error
	if logRowDays > 0 {
		cutoff := time.Now().Add(-time.Duration(logRowDays) * 24 * time.Hour).UnixMilli()
		if _, err := s.DeleteLogsBefore(ctx, cutoff); err != nil {
			errs = append(errs, err)
		}
	}
	if _, err := s.PruneQuotaSamples(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := s.IncrementalVacuum(ctx); err != nil {
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
// 复用写池时 ro==db，避免重复 Close。
func (s *Store) Close() error {
	if s.ro != s.db {
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
