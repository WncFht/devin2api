// 本文件是模型目录价目投影：CatalogPrices/ModelUIDs/CatalogModels 供
// 日志成本折算、模型注册表页与令牌费用窗口共用目录缓存。
package ccpanel

import (
	"context"
	"sort"
)

// modelCatalogEntry 是模型目录里用量/计价关心的部分：三类 token 单价
// （$/1M）与上下文窗口上限。
type modelCatalogEntry struct {
	input         float64
	cached        float64
	output        float64
	contextTokens int64
}

// CatalogPrice 是模型目录中单模型的 token 单价（$/1M tokens）。
type CatalogPrice struct {
	Input  float64
	Cached float64
	Output float64
}

// CatalogPrices 返回上游目录价目表（uid→单价）；目录不可用时返回空表。
func (h *Handler) CatalogPrices(ctx context.Context) map[string]CatalogPrice {
	catalog := h.modelCatalogMap(ctx)
	out := make(map[string]CatalogPrice, len(catalog))
	for uid, c := range catalog {
		out[uid] = CatalogPrice{Input: c.input, Cached: c.cached, Output: c.output}
	}
	return out
}

// ModelUIDs 返回上游目录中全部模型 uid（排序后），供合成渠道模型清单。
func (h *Handler) ModelUIDs(ctx context.Context) []string {
	models, err := h.cachedModels(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(models))
	for _, m := range models {
		if uid, _ := m["uid"].(string); uid != "" {
			out = append(out, uid)
		}
	}
	sort.Strings(out)
	return out
}

// modelCatalogMap 从模型目录缓存取 uid → 价格与上下文窗口。
func (h *Handler) modelCatalogMap(ctx context.Context) map[string]modelCatalogEntry {
	out := map[string]modelCatalogEntry{}
	models, err := h.cachedModels(ctx)
	if err != nil {
		return nil
	}
	for _, m := range models {
		uid, _ := m["uid"].(string)
		if uid == "" {
			continue
		}
		out[uid] = modelCatalogEntry{
			input:         floatAny(m["price_input"]),
			cached:        floatAny(m["price_cached"]),
			output:        floatAny(m["price_output"]),
			contextTokens: int64(floatAny(m["context_tokens"])),
		}
	}
	return out
}

// CatalogModels 返回上游模型目录缓存的原始行（uid/label/价格/能力标记等），
// 供把目录信息并入模型注册表页。缓存不可用时返回 nil。
func (h *Handler) CatalogModels(ctx context.Context) []map[string]any {
	models, err := h.cachedModels(ctx)
	if err != nil {
		return nil
	}
	return models
}
