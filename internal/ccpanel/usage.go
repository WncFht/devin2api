// 本文件是模型目录价目投影：CatalogPrices/ModelUIDs/CatalogModels 供
// 日志成本折算、模型注册表页与令牌费用窗口共用目录缓存。
package ccpanel

import (
	"context"
	"sort"
)

// modelCatalogEntry 是模型目录里用量/计价关心的部分：三类 token 单价
// （$/1M，内嵌 CatalogPrice）与上下文窗口上限。
type modelCatalogEntry struct {
	CatalogPrice
	contextTokens int64
}

// CatalogPrice 是模型目录中单模型的 token 单价（$/1M tokens）。
type CatalogPrice struct {
	Input  float64
	Cached float64
	Output float64
}

// TokenCost 是目录价折算公式：prompt 侧 input+cache_write 按 input 价、
// cache_read 按 cached 价、output 按 output 价，目录单位是 USD/百万 token。
// 无目录价的模型传零值 CatalogPrice 即得 0。面板聚合与 /v1 费用窗口
// 记账共用同一份公式——口径只在这里定义一次。
func TokenCost(in, out, cacheRead, cacheWrite int64, p CatalogPrice) float64 {
	return (float64(in+cacheWrite)*p.Input + float64(cacheRead)*p.Cached + float64(out)*p.Output) / 1e6
}

// CatalogPrices 返回上游目录价目表（uid→单价）；目录不可用时返回空表。
func (h *Handler) CatalogPrices(ctx context.Context) map[string]CatalogPrice {
	catalog := h.modelCatalogMap(ctx)
	out := make(map[string]CatalogPrice, len(catalog))
	for uid, c := range catalog {
		out[uid] = c.CatalogPrice
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
			CatalogPrice: CatalogPrice{
				Input:  floatAny(m["price_input"]),
				Cached: floatAny(m["price_cached"]),
				Output: floatAny(m["price_output"]),
			},
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
