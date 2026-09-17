package store

import (
	"context"
	"database/sql"
	"time"
)

// GateStateKey 是 lane 冷却闩在 runtime_state 里的键名约定：gate:<lane>，
// 隐式单 lane 记 gate:default。devin 闸门的读写与导入器对
// gate-state*.json 的搬移共用这一约定。
func GateStateKey(lane string) string {
	return "gate:" + lane
}

// GetState 读 runtime_state 一键；不存在返回 ok=false。
func (s *Store) GetState(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT value FROM runtime_state WHERE "key"=?`, key).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// SetState 写 runtime_state 一键（upsert）。
func (s *Store) SetState(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO runtime_state("key", value, updated_at) VALUES(?,?,?)`,
		key, value, time.Now().UnixMilli())
	return err
}

// DeleteState 删 runtime_state 一键；不存在时为空操作。
func (s *Store) DeleteState(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM runtime_state WHERE "key"=?`, key)
	return err
}
