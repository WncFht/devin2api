package store

import (
	"context"
	"database/sql"
	"time"
)

// SetSetting 写入一个设置键；updatedAt 传 0 时取当前毫秒。
// 面板设置的「覆盖 config.yaml 恒赢」语义在上层，本表只是 KV。
func (s *Store) SetSetting(ctx context.Context, key, value string, updatedAt int64) error {
	if updatedAt == 0 {
		updatedAt = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO settings("key", value, updated_at) VALUES(?,?,?)`,
		key, value, updatedAt)
	return err
}

// GetSetting 读一个设置键；不存在返回 ok=false。
func (s *Store) GetSetting(ctx context.Context, key string) (value string, updatedAt int64, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value, updated_at FROM settings WHERE "key"=?`, key).Scan(&value, &updatedAt)
	if err == sql.ErrNoRows {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, err
	}
	return value, updatedAt, true, nil
}

// DeleteSetting 删除一个设置键；不存在时为空操作。
func (s *Store) DeleteSetting(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM settings WHERE "key"=?`, key)
	return err
}

// ListSettings 返回全部设置键值与各自更新时间。
func (s *Store) ListSettings(ctx context.Context) (values map[string]string, updated map[string]int64, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT "key", value, updated_at FROM settings`)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	values = map[string]string{}
	updated = map[string]int64{}
	for rows.Next() {
		var k, v string
		var ts int64
		if err := rows.Scan(&k, &v, &ts); err != nil {
			return nil, nil, err
		}
		values[k] = v
		updated[k] = ts
	}
	return values, updated, rows.Err()
}
