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
	UsedBgPing  int    `json:"used_bg_ping"` // 本窗经 tryAdmit 的保温 ping 放行数（used_bg 的子集——used_bg-本列=真实 bg 需求）
	Drip        int    `json:"drip"`         // 本窗闩内滴灌探针放行数（used_* 的子集，单列供配额归因）
	RetryAdmits int    `json:"retry_admits"` // 本窗同 lane 续试重发的放行数（used_* 的子集——reopen/续轮/凭据自愈/瞬时重试的再发送，不含号池 failover 后新 lane 首发）
	ReservePeak int    `json:"reserve_peak"` // 本窗 bg 预留量的峰值
	WaitersPeak int    `json:"waiters_peak"` // 本窗闸内排队数峰值（fg+bg）
	// 按拒绝成因分列的快败数：quota 桶满、hold 等待超预算（死区等待）、
	// bgReserve bg 让路（预留/爬坡）、latch 闩内快败、yield 兄弟有余量
	// 提前让给 failover。
	RejectQuota     int     `json:"reject_quota"`
	RejectHold      int     `json:"reject_hold"`
	RejectBgReserve int     `json:"reject_bg_reserve"`
	RejectLatch     int     `json:"reject_latch"`
	RejectYield     int     `json:"reject_yield"`
	FgRate          float64 `json:"fg_rate"` // 关窗折叠后的 fg 准入速率 EMA（条/窗，bg 预留的输入）
}

// InsertGateWindow 追加一条闸门窗口聚合行。(lane,window_start) 有唯一
// 索引——reuseport 交接期新旧进程会并发观察同一窗口，撞车丢后写者不报错，
// 与 quota_samples 的 OR IGNORE 口径一致。
func (s *Store) InsertGateWindow(ctx context.Context, w *GateWindow) error {
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO gate_windows(
		lane, window_start, quota, used_fg, used_bg, used_bg_ping, drip, retry_admits,
		reserve_peak, waiters_peak,
		reject_quota, reject_hold, reject_bg_reserve, reject_latch, reject_yield, fg_rate
	) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.Lane, w.WindowStart, w.Quota, w.UsedFg, w.UsedBg, w.UsedBgPing, w.Drip, w.RetryAdmits,
		w.ReservePeak, w.WaitersPeak,
		w.RejectQuota, w.RejectHold, w.RejectBgReserve, w.RejectLatch, w.RejectYield, w.FgRate)
	return err
}

// ListGateWindows 按窗口起点升序返回某 lane since（unix 秒）之后的行，
// 至多 limit 条（取最新者——SQL 倒序截断后 Go 侧反转回升序）；limit<=0
// 不限。lane 为空串时返回全部 lane。
func (s *Store) ListGateWindows(ctx context.Context, lane string, since int64, limit int) ([]*GateWindow, error) {
	if limit <= 0 {
		limit = -1
	}
	query := `SELECT lane, window_start, quota, used_fg, used_bg, used_bg_ping, drip, retry_admits,
		reserve_peak, waiters_peak,
		reject_quota, reject_hold, reject_bg_reserve, reject_latch, reject_yield, fg_rate
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
		if err := rows.Scan(&w.Lane, &w.WindowStart, &w.Quota, &w.UsedFg, &w.UsedBg, &w.UsedBgPing, &w.Drip, &w.RetryAdmits,
			&w.ReservePeak, &w.WaitersPeak,
			&w.RejectQuota, &w.RejectHold, &w.RejectBgReserve, &w.RejectLatch, &w.RejectYield, &w.FgRate); err != nil {
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
// 与 logs 摘要行共用同一时间保留口径（Maintain 按 logRowDays 喂入）；
// 分片逐批提交，保留期调小的存量差不独占写连接。
func (s *Store) PruneGateWindows(ctx context.Context, before int64) (int64, error) {
	return s.deleteRowsChunked(ctx, "gate_windows", `window_start < ?`, before)
}

// GateDaySends 是单日闸门放行数的分解：Sends 是当日放行的真实请求数
// （used_fg+used_bg−used_bg_ping——每次放行对应一次真实上游发送，含
// 内层重试与闩内滴灌探针；保温 ping 走 adapter 内部路径不产生 logs
// 行，留在分子里会垫高 sends/row 的「每请求一发」基线），RetryAdmits
// 是其中同 lane 续试重发的放行数，Slots 是当日落库的 (lane,window_start)
// 行数——(lane,window_start) 有唯一索引，COUNT(*) 即去重 lane-slot 数，
// 是覆盖率的实有侧。
type GateDaySends struct {
	Sends       int64
	RetryAdmits int64
	Slots       int64
}

// GateSendsByDay 把闸门放行数按本地日聚合，返回 'YYYY-MM-DD'→分解读数
// 与窗口内出现过的 lane 数。sends/row 的覆盖率期望 = lanes × 当日已流逝
// 分钟数——lane 数取整个查询窗的全集而非逐日：某 lane 整日零行时逐日
// 口径会把它从期望值里抹掉，而整日静默恰恰是要被覆盖率暴露的缺口。
// sinceUnix（unix 秒）按 window_start 下界过滤。跨 lane 合计——sends/row
// 指标的分母（logs 行数）同样是跨 lane 口径。quota<=0 的闸门照常记窗行：
// 不限速放行仍是真实发送，与限流放行走同一本 used_* 账。
func (s *Store) GateSendsByDay(ctx context.Context, sinceUnix int64) (map[string]GateDaySends, int, error) {
	rows, err := s.ro.QueryContext(ctx,
		`SELECT strftime('%Y-%m-%d', window_start, 'unixepoch', 'localtime') AS day,
			COALESCE(SUM(used_fg + used_bg - used_bg_ping), 0), COALESCE(SUM(retry_admits), 0), COUNT(*)
		FROM gate_windows WHERE window_start >= ? GROUP BY day`, sinceUnix)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]GateDaySends{}
	for rows.Next() {
		var day string
		var sends GateDaySends
		if err := rows.Scan(&day, &sends.Sends, &sends.RetryAdmits, &sends.Slots); err != nil {
			return nil, 0, err
		}
		out[day] = sends
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var lanes int
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT lane) FROM gate_windows WHERE window_start >= ?`,
		sinceUnix).Scan(&lanes); err != nil {
		return nil, 0, err
	}
	return out, lanes, nil
}
