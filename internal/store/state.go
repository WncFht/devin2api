package store

import (
	"context"
	"database/sql"
	"log/slog"
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
	err = s.ro.QueryRowContext(ctx,
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

// stateWrite 是 runtime_state 的一步异步写：del 置位表示删除。
type stateWrite struct {
	key   string
	value string
	del   bool
}

// stateQueueCap 给异步写管道一个远超真实流量的缓冲：闩迁移与号池
// 冷却是每分钟个位数事件，256 的队深足以吸收任何合理突发；真溢出
// 说明写面已整体卡死，丢行（有计数）比反压锁内调用方正确。
const stateQueueCap = 256

// stateWriteTimeout 是单步队列写的 ctx 上限：沿用原锁内写
// lockedStateStoreTimeout（5s）的口径——争用期超时不阻塞队列，
// 超时即按写失败记账。
const stateWriteTimeout = 5 * time.Second

// stateQueueDrainBudget 是 Close 排空存量的总预算：队列常态近空，
// 预算只兜「库已卡死」场景——写已注定失败时不让关库被整队重放拖住。
const stateQueueDrainBudget = 5 * time.Second

// QueueState 把一步 runtime_state 写排进单写者协程：set/delete 按
// 入队序应用——调用方在自己的互斥区内入队，原「写/删须在锁内序化」
// 的约束由 FIFO 承接（clear 先入队、persist 后入队的交错在协程侧
// 不可能倒序落库），锁内自此不再带 SQLite I/O。非阻塞：队满丢弃
// 记数——内存态权威，落库只服务重启恢复，丢行最坏是复活一条到期
// 即清的旧冷却。
func (s *Store) QueueState(key, value string, del bool) {
	select {
	case <-s.stateQueueDone:
		// 消费协程已退：写入注定无人消费，记 drop 而不是静默落进死队列。
		s.stateQueueDrops.Add(1)
		return
	default:
	}
	select {
	case s.stateQueue <- stateWrite{key: key, value: value, del: del}:
	default:
		s.stateQueueDrops.Add(1)
		slog.Warn("runtime_state write dropped: queue full", "key", key)
	}
}

// StateQueueDrops 返回异步写丢弃数（队满拒收与写失败合计）——台账
// 存在的目的就是兜住争用期的丢痕迹，它自己丢了多少必须有数。
func (s *Store) StateQueueDrops() int64 {
	return s.stateQueueDrops.Load()
}

// runStateQueue 是 runtime_state 异步写的唯一消费者：FIFO 逐条落库
// 直到关停信号，随后排空存量退出——排空带总预算，写面卡死时关库不
// 被整队重放拖住。
func (s *Store) runStateQueue() {
	defer close(s.stateQueueDone)
	for {
		select {
		case w := <-s.stateQueue:
			s.applyStateWrite(w)
		case <-s.stateQueueStop:
			deadline := time.Now().Add(stateQueueDrainBudget)
			for {
				select {
				case w := <-s.stateQueue:
					s.applyStateWrite(w)
				default:
					return
				}
				if time.Now().After(deadline) {
					return
				}
			}
		}
	}
}

// applyStateWrite 执行单步队列写：失败按丢弃入账（与队满同口径）。
func (s *Store) applyStateWrite(w stateWrite) {
	ctx, cancel := context.WithTimeout(context.Background(), stateWriteTimeout)
	var err error
	if w.del {
		err = s.DeleteState(ctx, w.key)
	} else {
		err = s.SetState(ctx, w.key, w.value)
	}
	cancel()
	if err != nil {
		s.stateQueueDrops.Add(1)
		slog.Warn("runtime_state queued write failed", "key", w.key, "del", w.del, "error", err)
	}
}
