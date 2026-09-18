package store

import (
	"context"
)

// GateWindow 是 gate_windows 一行的领域形状：某 lane 一个被观察关闭的
// 对齐分钟窗口的聚合账。行只覆盖闸门实际触碰过的窗口——未被触碰的空窗
// 期没有行（缺口=无观测，与零用量区分）。写方是闸门翻页：窗口关闭时把
// 本窗口的用量/拒绝/峰值快照成行；字段语义与 rategate 的 win* 账同源。
type GateWindow struct {
	Lane        string `json:"lane"`
	WindowStart int64  `json:"window_start"` // unix 秒，窗口起点（对齐分钟桶界）
	Quota       int    `json:"quota"`        // 关窗时生效的窗口配额（<=0 表示不限速）
	UsedFg      int    `json:"used_fg"`      // 本窗 fg 放行数
	UsedBg      int    `json:"used_bg"`      // 本窗 bg 放行数（含保温 ping 与闩内探针）
	Drip        int    `json:"drip"`         // 本窗闩内滴灌探针放行数（used_* 的子集，单列供配额归因）
	ReservePeak int    `json:"reserve_peak"` // 本窗 bg 预留量的峰值
	WaitersPeak int    `json:"waiters_peak"` // 本窗闸内排队数峰值（fg+bg）
	// 按拒绝成因分列的快败数：quota 桶满、hold 等待超预算（死区等待）、
	// bgReserve bg 让路（预留/爬坡）、latch 闩内快败。
	RejectQuota     int     `json:"reject_quota"`
	RejectHold      int     `json:"reject_hold"`
	RejectBgReserve int     `json:"reject_bg_reserve"`
	RejectLatch     int     `json:"reject_latch"`
	FgRate          float64 `json:"fg_rate"` // 关窗折叠后的 fg 准入速率 EMA（条/窗，bg 预留的输入）
}

// InsertGateWindow 追加一条闸门窗口聚合行。(lane,window_start) 有唯一
// 索引——reuseport 交接期新旧进程会并发观察同一窗口，撞车丢后写者不报错，
// 与 quota_samples 的 OR IGNORE 口径一致。
func (s *Store) InsertGateWindow(ctx context.Context, w *GateWindow) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO gate_windows(
		lane, window_start, quota, used_fg, used_bg, drip,
		reserve_peak, waiters_peak,
		reject_quota, reject_hold, reject_bg_reserve, reject_latch, fg_rate
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.Lane, w.WindowStart, w.Quota, w.UsedFg, w.UsedBg, w.Drip,
		w.ReservePeak, w.WaitersPeak,
		w.RejectQuota, w.RejectHold, w.RejectBgReserve, w.RejectLatch, w.FgRate)
	return err
}

// ListGateWindows 按窗口起点升序返回某 lane since（unix 秒）之后的行，
// 至多 limit 条（取最新者——SQL 倒序截断后 Go 侧反转回升序）；limit<=0
// 不限。lane 为空串时返回全部 lane。
func (s *Store) ListGateWindows(ctx context.Context, lane string, since int64, limit int) ([]*GateWindow, error) {
	if limit <= 0 {
		limit = -1
	}
	query := `SELECT lane, window_start, quota, used_fg, used_bg, drip,
		reserve_peak, waiters_peak,
		reject_quota, reject_hold, reject_bg_reserve, reject_latch, fg_rate
		FROM gate_windows WHERE window_start>=?`
	args := []any{since}
	if lane != "" {
		query += ` AND lane=?`
		args = append(args, lane)
	}
	query += ` ORDER BY window_start DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.ro.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*GateWindow
	for rows.Next() {
		var w GateWindow
		if err := rows.Scan(&w.Lane, &w.WindowStart, &w.Quota, &w.UsedFg, &w.UsedBg, &w.Drip,
			&w.ReservePeak, &w.WaitersPeak,
			&w.RejectQuota, &w.RejectHold, &w.RejectBgReserve, &w.RejectLatch, &w.FgRate); err != nil {
			return nil, err
		}
		out = append(out, &w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// PruneGateWindows 删除窗口起点早于 before（unix 秒）的行，返回删除数。
// 与 logs 摘要行共用同一时间保留口径（Maintain 按 logRowDays 喂入）。
func (s *Store) PruneGateWindows(ctx context.Context, before int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM gate_windows WHERE window_start < ?`, before)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// GateSendsByDay 把 used_fg+used_bg（闸门放行数=真实上游发送数，含内层
// 重试与保温/drip 探针）按本地日聚合，返回 'YYYY-MM-DD'→发送数。
// sinceUnix（unix 秒）按 window_start 下界过滤。跨 lane 合计——sends/row
// 指标的分母（logs 行数）同样是跨 lane 口径。注意 quota<=0 的闸门不记
// 窗口行（admitLocked 不跑），该口径下分子随无窗期自然缺记。
func (s *Store) GateSendsByDay(ctx context.Context, sinceUnix int64) (map[string]int64, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', window_start, 'unixepoch', 'localtime') AS day,
			COALESCE(SUM(used_fg + used_bg), 0)
		FROM gate_windows WHERE window_start >= ? GROUP BY day`, sinceUnix)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var day string
		var sends int64
		if err := rows.Scan(&day, &sends); err != nil {
			return nil, err
		}
		out[day] = sends
	}
	return out, rows.Err()
}
