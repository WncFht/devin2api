package store

import (
	"context"
	"time"
)

// ModelEntry 是 model_registry 一行的领域形状，字段镜像
// modelreg.Entry（注册表覆盖项：重定向目标与停用位）。
type ModelEntry struct {
	Model         string
	RedirectModel string
	Disabled      bool
	UpdatedAt     int64 // unix 毫秒
}

// SetModel 写入或覆盖一条模型注册项。
func (s *Store) SetModel(ctx context.Context, e ModelEntry) error {
	if e.UpdatedAt == 0 {
		e.UpdatedAt = time.Now().UnixMilli()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT OR REPLACE INTO model_registry(model, redirect_model, disabled, updated_at) VALUES(?,?,?,?)`,
		e.Model, e.RedirectModel, e.Disabled, e.UpdatedAt)
	return err
}

// DeleteModel 删除一条模型注册项；不存在时为空操作。
func (s *Store) DeleteModel(ctx context.Context, model string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_registry WHERE model=?`, model)
	return err
}

// ListModels 返回全部注册项，按 model 排序。
func (s *Store) ListModels(ctx context.Context) ([]ModelEntry, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT model, redirect_model, disabled, updated_at FROM model_registry ORDER BY model`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ModelEntry
	for rows.Next() {
		var e ModelEntry
		if err := rows.Scan(&e.Model, &e.RedirectModel, &e.Disabled, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
