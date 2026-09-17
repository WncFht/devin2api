// Package store 是全部面板可见状态的 SQLite 持久层：请求日志、调试
// payload、下游令牌、模型注册表、面板设置、配额采样与运行时小状态。
// 它取代原先的 index.jsonl/auth_tokens.json/models.json/
// panel-settings.json/quota.jsonl/gate-state*.json 文件持久化。
//
// 单连接串行化全部读写（ccLoad 实证：SQLite 多连接高并发写触发
// BUSY/DEADLOCK）；本服务写量级远低于 SQLite 上限，热读靠调用方
// 内存索引与 WAL 读放大容忍。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

// Store 包装 *sql.DB，对外只暴露领域方法。
type Store struct {
	db   *sql.DB
	path string
}

// Open 打开（或创建）path 处的库；created 报告文件是本次新建的——
// 调用方据此决定是否跑 ImportLegacy。schema 幂等，重复打开只做
// CREATE IF NOT EXISTS。
func Open(path string) (*Store, bool, error) {
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, os.ErrNotExist)

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode=WAL&_pragma=wal_autocheckpoint(500)&_loc=Local", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, false, fmt.Errorf("open sqlite: %w", err)
	}
	// 单连接是刻意的：写路径无并发诉求，串行化免除锁竞争调参。
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("ping sqlite: %w", err)
	}
	// auto_vacuum 只能在新库（无用户表）建表前开启：VACUUM 对空库
	// 只是把头部位写进文件，不重写业务数据；旧库不在启动路径做。
	if err := enableAutoVacuumOnEmpty(db); err != nil {
		_ = db.Close()
		return nil, false, err
	}
	if err := applySchema(db); err != nil {
		_ = db.Close()
		return nil, false, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, path: path}, created, nil
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

// Close 关闭连接池；WAL checkpoint 由驱动在关闭时收尾。
func (s *Store) Close() error {
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
