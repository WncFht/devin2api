package store

import (
	"context"
	"time"
)

// storeOpensRecent 是台账投影的最近行数：8 行足以让「第二个
// pid/build 附着」一眼可见，又不在轮询端点里膨胀载荷。
const storeOpensRecent = 8

// storeOpensDistinctWindow 是 distinct pid 信号的回看窗：24h 覆盖
// 一次部署交接周期——窗内 distinct pid>1 即有别处进程碰过这个库
// （reuseport 交接残留、探针、手搓工具）。
const storeOpensDistinctWindow = 24 * time.Hour

// StoreOpen 是 store_opens 台账一行的领域形状；Argv 保留落库原值
// （os.Args 的 JSON 数组文本，4KB 截断可能切出非法 JSON），渲染归
// 调用方。
type StoreOpen struct {
	At    int64
	PID   int64
	Argv  string
	Build string
	Path  string
}

// StoreOpensReport 是开库台账快照：总行数、最近若干行（新在前）、
// 回看窗内 distinct pid 数。台账是观测面——表缺席/被污染时读失败
// 原样上抛，由调用方降级省略该组而非视为致命。
type StoreOpensReport struct {
	Total           int64
	Recent          []*StoreOpen
	DistinctPIDs24h int64
}

// StoreOpens 读开库台账快照。三条 SELECT 都走读池；行数被
// keep-last-N 自界（≤storeOpensKeep），无索引全扫也是微秒级。
func (s *Store) StoreOpens(ctx context.Context) (*StoreOpensReport, error) {
	rep := &StoreOpensReport{}
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM store_opens`).Scan(&rep.Total); err != nil {
		return nil, err
	}
	rows, err := s.ro.QueryContext(ctx,
		`SELECT at, pid, argv, build, path FROM store_opens ORDER BY id DESC LIMIT ?`,
		storeOpensRecent)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		o := &StoreOpen{}
		if err := rows.Scan(&o.At, &o.PID, &o.Argv, &o.Build, &o.Path); err != nil {
			return nil, err
		}
		rep.Recent = append(rep.Recent, o)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	since := time.Now().Add(-storeOpensDistinctWindow).UnixMilli()
	if err := s.ro.QueryRowContext(ctx,
		`SELECT COUNT(DISTINCT pid) FROM store_opens WHERE at >= ?`, since).
		Scan(&rep.DistinctPIDs24h); err != nil {
		return nil, err
	}
	return rep, nil
}
