// 本文件是 detached_events 表（脱钩流完成缓存的生命周期台账）的读写
// 路径。写方是 adapter/devin 的 detachedRegistry.pushEvent：内存事件环
// 每条生命周期事件同步落一行——环只有 64 条且随进程死蒸发，而脱钩事件
// 的两侧请求目录都可能缺席（claim 失败的重试零 payload、origin 标记行
// 在 sqlite 争用风暴中被丢弃），本表是「脱钩后谁来接过、怎么收场」的
// 兜底取证面。kind/detail 词表由写方定版：admit/attach/miss/cross_miss/
// evict/finish/truncate，detail 记移除原因或泵终局档。
package store

import (
	"context"
	"time"
)

// DetachedEvent 是 detached_events 的一行：一次脱钩生命周期事件。
// Key 是全量语义请求哈希（内存环为显示截前 12 位，台账留全键——
// 04 标记行里的 key 是全量，取证按全键对照）；OriginDir 是首请求的
// 调试目录名（重放事件的原始帧取证在那边），事件与条目脱钩时可为空
// （如 pre-detach 截断、跨 lane miss 由本 lane 记账但条目在 owner）。
type DetachedEvent struct {
	At        time.Time
	Lane      string
	Key       string
	OriginDir string
	Kind      string
	Detail    string
}

// InsertDetachedEvent 追加一条台账行；at 记 unix 毫秒（与 logs.time
// 同单位）。
func (s *Store) InsertDetachedEvent(ctx context.Context, e DetachedEvent) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO detached_events(at, lane, "key", origin_dir, kind, detail) VALUES(?,?,?,?,?,?)`,
		e.At.UnixMilli(), e.Lane, e.Key, e.OriginDir, e.Kind, e.Detail)
	return err
}

// PruneDetachedEvents 删除早于 before（unix 毫秒）的行，返回删除数。
// 与 logs 摘要行共用同一保留期（Maintain 同一 cutoff 调用）。
func (s *Store) PruneDetachedEvents(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM detached_events WHERE at < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
