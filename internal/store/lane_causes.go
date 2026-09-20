// 本文件是 lane_attempt_causes 表（号池被放弃 lane 尝试的持久聚合账）
// 的读写路径。写方是 logs 行的同事务展开：LogRow.SwitchCauses（由
// debuglog.logRowFor 从 meta.json 同源的 upstream_attempts 投影）在
// InsertLog/WriteDebugBatch 里按 (day,lane,cause) 主键累加——meta.json
// 的 upstream_attempts 明细受 payload 与目录保留期约束会淘汰，本表
// 是「为什么换号」的唯一持久口径。cause 词表的唯一事实源在 logvocab
// （写方 debuglog.switchCauseKey 经它取词）：local_gate[:reason] 是
// 本地闸门快败的幻影换号（零上游发送）、connect code 是真实 failover
// 发送、nocode 是无 code 的传输断裂。
package store

import (
	"context"
)

// SwitchCause 是一次被放弃 lane 尝试的归因键：Lane 是被试的 lane，
// Cause 是放弃成因（词表见 lane_attempt_causes DDL 注释）。
// LogRow.SwitchCauses 的键型——不是列，只是写路径的展开载体。
type SwitchCause struct {
	Lane  string
	Cause string
}

// laneCauseDim 是 lane_attempt_causes 主键的 Go 形态（写路径批内聚合键）。
type laneCauseDim struct {
	day   string
	lane  string
	cause string
}

var laneCauseUpsertSQL = `INSERT INTO lane_attempt_causes(day, lane, cause, n) VALUES(?,?,?,?)
	ON CONFLICT(day, lane, cause) DO UPDATE SET n = n + excluded.n`

// addCauseContrib 把一条日志行的 SwitchCauses 累加进批内聚合器；day 取
// 行 started_at 的本地日（days 与 cells 的 day 维度共享同一批级缓存）。
func addCauseContrib(causes map[laneCauseDim]int64, e *LogRow, days *dayCache) {
	if len(e.SwitchCauses) == 0 {
		return
	}
	day := days.dayOf(e.StartedAt)
	for k, n := range e.SwitchCauses {
		causes[laneCauseDim{day: day, lane: k.Lane, cause: k.Cause}] += int64(n)
	}
}

// upsertCauseCells 把批内聚合器逐键 upsert 进表（与 logs 行同事务）。
// 空地图是廉价空转。
func upsertCauseCells(ctx context.Context, q dbtx, causes map[laneCauseDim]int64) error {
	for dim, n := range causes {
		if _, err := q.ExecContext(ctx, laneCauseUpsertSQL, dim.day, dim.lane, dim.cause, n); err != nil {
			return err
		}
	}
	return nil
}

// LaneAttemptCause 是 lane_attempt_causes 的读侧行：某 lane 某日某成因
// 的被放弃尝试数。
type LaneAttemptCause struct {
	Date  string `json:"date"`
	Lane  string `json:"lane"`
	Cause string `json:"cause"`
	N     int64  `json:"n"`
}

// LaneAttemptCauses 返回 sinceDay（本地日 'YYYY-MM-DD'）起的全部聚合行，
// 按日/泳道/成因升序。表体积天然有界（lane×日×成因），不做额外聚合。
func (s *Store) LaneAttemptCauses(ctx context.Context, sinceDay string) ([]LaneAttemptCause, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT day, lane, cause, n FROM lane_attempt_causes WHERE day >= ? ORDER BY day, lane, cause`, sinceDay)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []LaneAttemptCause
	for rows.Next() {
		var c LaneAttemptCause
		if err := rows.Scan(&c.Date, &c.Lane, &c.Cause, &c.N); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PruneLaneAttemptCauses 删除早于 beforeDay 的行，返回删除数。与 logs
// 摘要行共用同一保留期（Maintain 同一 cutoff 调用）；分片与其他
// 保留删除同形。
func (s *Store) PruneLaneAttemptCauses(ctx context.Context, beforeDay string) (int64, error) {
	return s.deleteRowsChunked(ctx, "lane_attempt_causes", `day < ?`, beforeDay)
}
