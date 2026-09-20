// 本文件曾是 index.jsonl 的内存增量聚合立方体；现为 logs 表 SQL 聚合
// 的薄适配层——eachCell/recentWindow/recentRPM/lastByModel 每次查询
// 直接打 GROUP BY/窗口函数，统计覆盖从内存窗口升级为全表。
//
// 查询失败按空集处理：仪表盘是轮询面，「暂无数据」比 500 更贴近旧
// 内存路径无失败概念的语义；写侧失败面由 Stats().io_errors 承担。
// 「store 缺席→零值 / 查询失败→告警+零值」的政策收在 storeQuery 一处。
package ccpanel

import (
	"context"
	"log/slog"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// rollupSlotSeconds 是格子时间槽宽度；10 分钟对齐面板趋势的桶，
// 误差边界 ±10 分钟（天界对齐的预设 range 不受此误差影响）。
const rollupSlotSeconds = 600

// storeQuery 是面板聚合读路径错误政策的唯一落点：h.store 缺席或
// 查询报错时记告警并返回调用方给的零值。what 是告警日志里的查询名。
func storeQuery[T any](h *Handler, zero T, what string, fn func(*store.Store) (T, error)) T {
	if h.store == nil {
		return zero
	}
	v, err := fn(h.store)
	if err != nil {
		slog.Warn("ccpanel: "+what+" query failed", "error", err)
		return zero
	}
	return v
}

// eachCell 以标准 10 分钟槽遍历 [since,until) 与 scope 相交的格子；
// cb 不得持有 cell 指针。需要其它槽宽的调用方走 eachCellSec。
func (h *Handler) eachCell(ctx context.Context, since, until time.Time, scope statScope, cb func(store.LogCellKey, store.LogCellTotals)) {
	h.eachCellSec(ctx, rollupSlotSeconds, since, until, scope, cb)
}

// eachCellSec 按 slotSec 秒槽遍历格子，语义同 eachCell。回调形态
// 不进 storeQuery——错误政策在这里内联同一口径。
func (h *Handler) eachCellSec(ctx context.Context, slotSec int64, since, until time.Time, scope statScope, cb func(store.LogCellKey, store.LogCellTotals)) {
	if h.store == nil {
		return
	}
	if err := h.store.LogCells(ctx, slotSec, since.Unix(), until.Unix(), scope, cb); err != nil {
		slog.Warn("ccpanel: log cells query failed", "error", err)
	}
}

// recentWindow 聚合最近 seconds 秒内完成的条目（scope 同 queryScope）。
func (h *Handler) recentWindow(ctx context.Context, seconds int64, scope statScope) store.LogRecentAgg {
	return storeQuery(h, store.LogRecentAgg{}, "recent window", func(st *store.Store) (store.LogRecentAgg, error) {
		return st.LogRecentWindow(ctx, seconds, scope)
	})
}

// recentRPM 返回最近 60 秒内完成的非 499 请求数；model/kh 非空时
// 分别按生效模型、key_hash 过滤。
func (h *Handler) recentRPM(ctx context.Context, model, kh string) float64 {
	return storeQuery(h, 0.0, "recent rpm", func(st *store.Store) (float64, error) {
		return st.LogRecentRPM(ctx, store.LogScope{Model: model, KeyHash: kh})
	})
}

// recentRPMByModel 一次 GROUP BY 扫描取全部生效模型的最近 60s
// RPM；kh 非空时只看该令牌——替代逐模型 recentRPM 的 N+1。
func (h *Handler) recentRPMByModel(ctx context.Context, kh string) map[string]float64 {
	return storeQuery(h, nil, "recent rpm by model", func(st *store.Store) (map[string]float64, error) {
		return st.LogRecentRPMByModel(ctx, kh)
	})
}

// recentRPMByKeyHash 一次 GROUP BY 扫描取全部令牌的最近 60s
// RPM——替代逐令牌 recentRPM 的 N+1。
func (h *Handler) recentRPMByKeyHash(ctx context.Context) map[string]float64 {
	return storeQuery(h, nil, "recent rpm by key hash", func(st *store.Store) (map[string]float64, error) {
		return st.LogRecentRPMByKeyHash(ctx)
	})
}

// lastByModel 返回各生效模型的最近快照；kh 非空时只看该令牌的行。
func (h *Handler) lastByModel(ctx context.Context, kh string) map[string]store.LogModelLast {
	return storeQuery(h, nil, "last-by-model", func(st *store.Store) (map[string]store.LogModelLast, error) {
		return st.LogLastByModel(ctx, kh)
	})
}

// modelSet 返回出现过的生效模型名集合（排序）；kh 非空时只数该
// key_hash 产生过流量的模型（api_token 身份的数据范围收敛）。
// 零值必须是 []string{}：nil 在响应里序列化成 null 而非 []。
func (h *Handler) modelSet(ctx context.Context, kh string) []string {
	return storeQuery(h, []string{}, "log models", func(st *store.Store) ([]string, error) {
		return st.LogModels(ctx, kh)
	})
}

// statusCodeSet 返回出现过的状态码集合（排序）；kh 非空时只看该
// 令牌的行——api_token 身份的筛选面板与 models 维同口径。
// 零值必须是 []int{}：nil 在响应里序列化成 null 而非 []。
func (h *Handler) statusCodeSet(ctx context.Context, kh string) []int {
	return storeQuery(h, []int{}, "status codes", func(st *store.Store) ([]int, error) {
		return st.LogStatusCodes(ctx, kh)
	})
}
