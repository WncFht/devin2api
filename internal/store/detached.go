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
	return s.deleteRowsChunked(ctx, "detached_events", `at < ?`, before)
}

// DetachedBlob 是 detached_blobs 的一行：一条 completed 脱钩条目的
// 事件载荷（编码格式见 adapter/devin detached_blob.go，本层不解读）。
// FinishedAt 记 unix 毫秒，作播种 TTL 判据与同键冲突仲裁。
type DetachedBlob struct {
	Key        string
	Lane       string
	OriginDir  string
	FinishedAt time.Time
	Payload    []byte
}

// InsertDetachedBlob 落一条 completed 条目载荷。同键冲突按 finished_at
// 仲裁：只接受不旧于在场的写入——同语义请求在两条 lane 先后脱钩完成、
// 或被替换的旧泵晚于新泵收尾时，库里的恒是较新那份重放。
func (s *Store) InsertDetachedBlob(ctx context.Context, b DetachedBlob) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO detached_blobs("key", lane, origin_dir, finished_at, payload) VALUES(?,?,?,?,?)
		 ON CONFLICT("key") DO UPDATE SET lane=excluded.lane, origin_dir=excluded.origin_dir,
			finished_at=excluded.finished_at, payload=excluded.payload
		 WHERE excluded.finished_at >= detached_blobs.finished_at`,
		b.Key, b.Lane, b.OriginDir, b.FinishedAt.UnixMilli(), b.Payload)
	return err
}

// LoadDetachedBlobs 取某 lane 在 since（unix 毫秒）之后完成的全部
// 载荷行——开机播种窗口即条目的 completed TTL。按 finished_at 升序
// 返回，播种侧依序灌册让较新的条目在容量让位序里排在后。
func (s *Store) LoadDetachedBlobs(ctx context.Context, lane string, since int64) ([]DetachedBlob, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT "key", lane, origin_dir, finished_at, payload FROM detached_blobs
		 WHERE lane = ? AND finished_at >= ? ORDER BY finished_at`, lane, since)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var blobs []DetachedBlob
	for rows.Next() {
		var b DetachedBlob
		var finishedAt int64
		if err := rows.Scan(&b.Key, &b.Lane, &b.OriginDir, &finishedAt, &b.Payload); err != nil {
			return nil, err
		}
		b.FinishedAt = time.UnixMilli(finishedAt)
		blobs = append(blobs, b)
	}
	return blobs, rows.Err()
}

// detachedBlobRetention 是载荷行的保留界：唯一消费者是开机播种，
// 窗口即 adapter 侧 completed TTL（15min）；取 1h 留出死后取证余量
// （交接事故后仍能 sqlite3 直查 payload），又钉住表体积——行均
// ~100-400KiB、按脱钩速率一天 ~34 条封顶在 MB 级。
const detachedBlobRetention = time.Hour

// PruneDetachedBlobs 删除 finished_at 早于 before（unix 毫秒）的行，
// 返回删除数。表的唯一消费者是开机播种（窗口=条目 completed TTL）；
// before 取略大于 TTL 的保留界即可——过期行只供死后取证，不留长史。
func (s *Store) PruneDetachedBlobs(ctx context.Context, before int64) (int64, error) {
	return s.deleteRowsChunked(ctx, "detached_blobs", `finished_at < ?`, before)
}
