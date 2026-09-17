// 本文件曾是 index.jsonl 的内存增量聚合立方体；现为 logs 表 SQL 聚合
// 的薄适配层——eachCell/recentWindow/recentRPM/lastByModel 每次查询
// 直接打 GROUP BY/窗口函数，统计覆盖从内存窗口升级为全表。
//
// 查询失败按空集处理：仪表盘是轮询面，「暂无数据」比 500 更贴近旧
// 内存路径无失败概念的语义；写侧失败面由 Stats().io_errors 承担。
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

// logScope 把 statScope 翻译成 store 的范围谓词（字段一一对应，
// model/modelLike 作用于生效模型）。
func (scope statScope) logScope() store.LogScope {
	return store.LogScope{
		KeyHash:   scope.kh,
		API:       scope.api,
		Model:     scope.model,
		ModelLike: scope.modelLike,
		Account:   scope.account,
	}
}

// eachCell 遍历 [since,until) 与 scope 相交的格子；cb 不得持有 cell 指针。
func (h *Handler) eachCell(ctx context.Context, since, until time.Time, scope statScope, cb func(store.LogCellKey, store.LogCellTotals)) {
	if h.store == nil {
		return
	}
	if err := h.store.LogCells(ctx, since.Unix(), until.Unix(), scope.logScope(), cb); err != nil {
		slog.Warn("ccpanel: log cells query failed", "error", err)
	}
}

// recentWindow 聚合最近 seconds 秒内完成的条目（scope 同 queryScope）。
func (h *Handler) recentWindow(ctx context.Context, seconds int64, scope statScope) store.LogRecentAgg {
	if h.store == nil {
		return store.LogRecentAgg{}
	}
	a, err := h.store.LogRecentWindow(ctx, seconds, scope.logScope())
	if err != nil {
		slog.Warn("ccpanel: recent window query failed", "error", err)
	}
	return a
}

// recentRPM 返回最近 60 秒内完成的非 499 请求数；model/kh 非空时
// 分别按生效模型、key_hash 过滤。
func (h *Handler) recentRPM(ctx context.Context, model, kh string) float64 {
	if h.store == nil {
		return 0
	}
	v, err := h.store.LogRecentRPM(ctx, store.LogScope{Model: model, KeyHash: kh})
	if err != nil {
		slog.Warn("ccpanel: recent rpm query failed", "error", err)
	}
	return v
}

// lastByModel 返回各生效模型的最近快照；kh 非空时只看该令牌的行。
func (h *Handler) lastByModel(ctx context.Context, kh string) map[string]store.LogModelLast {
	if h.store == nil {
		return nil
	}
	m, err := h.store.LogLastByModel(ctx, kh)
	if err != nil {
		slog.Warn("ccpanel: last-by-model query failed", "error", err)
		return nil
	}
	return m
}

// modelSet 返回出现过的生效模型名集合（排序）；kh 非空时只数该
// key_hash 产生过流量的模型（api_token 身份的数据范围收敛）。
func (h *Handler) modelSet(ctx context.Context, kh string) []string {
	if h.store == nil {
		return []string{}
	}
	models, err := h.store.LogModels(ctx, kh)
	if err != nil {
		slog.Warn("ccpanel: log models query failed", "error", err)
		return []string{}
	}
	return models
}

// statusCodeSet 返回出现过的状态码集合（排序）。
func (h *Handler) statusCodeSet(ctx context.Context) []int {
	if h.store == nil {
		return []int{}
	}
	codes, err := h.store.LogStatusCodes(ctx)
	if err != nil {
		slog.Warn("ccpanel: status codes query failed", "error", err)
		return []int{}
	}
	return codes
}

// addCells 返回 a+b 的逐字段和。
func addCells(a, b store.LogCellTotals) store.LogCellTotals {
	a.Requests += b.Requests
	a.OK += b.OK
	a.Gone += b.Gone
	a.Limited += b.Limited
	a.NDur += b.NDur
	a.NDurOK += b.NDurOK
	a.InTok += b.InTok
	a.OutTok += b.OutTok
	a.CacheRead += b.CacheRead
	a.CacheWrite += b.CacheWrite
	a.InTokNG += b.InTokNG
	a.OutTokNG += b.OutTokNG
	a.CacheReadNG += b.CacheReadNG
	a.CacheWriteNG += b.CacheWriteNG
	a.SumDurMS += b.SumDurMS
	a.SumDurOKMS += b.SumDurOKMS
	a.SumFirstOKMS += b.SumFirstOKMS
	a.NFirstOK += b.NFirstOK
	a.SumFirstStreamMS += b.SumFirstStreamMS
	a.NFirstStream += b.NFirstStream
	a.SumDurNonStreamMS += b.SumDurNonStreamMS
	a.NNonStream += b.NNonStream
	a.NStreamNG += b.NStreamNG
	a.NNonStreamNG += b.NNonStreamNG
	a.SumGenMS += b.SumGenMS
	return a
}
