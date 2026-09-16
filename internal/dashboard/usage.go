// 本文件是 /panel/api/usage 端点：index.jsonl 聚合快照 + 按模型目录价的估算成本。
package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
)

// apiUsage 返回 index.jsonl 聚合快照，并按模型目录价附估算成本。
// 价格是 catalog 标价（$/1M tokens），est_cost 为参考值而非上游账单。
func (h *Handler) apiUsage(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	if h.debugManager == nil {
		writeJSON(w, http.StatusOK, json.RawMessage(`{"disabled":true}`))
		return
	}
	snap := h.debugManager.UsageStats()
	catalog := h.modelCatalogMap(r.Context())
	var totalCost float64
	models := make([]map[string]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		// 行字段与 dimensionAgg 的 json tag 一一对应：marshal 往返代替
		// 手抄清单，聚合侧新增维度（如 last_status）自动透出。
		raw, _ := json.Marshal(m)
		var row map[string]any
		_ = json.Unmarshal(raw, &row)
		if c, ok := catalog[m.Name]; ok {
			// cache_write 实测按 input 价计费：配额翻转拟合的隐含单价 ≈ input 价，
			// 并非 Anthropic 惯例的 1.25×；catalog 无独立 cache_write 价格维。
			cost := (float64(m.InputTokens+m.CacheWrite)*c.input + float64(m.CacheRead)*c.cached + float64(m.OutputTokens)*c.output) / 1e6
			row["est_cost"] = cost
			totalCost += cost
			if c.contextTokens > 0 && m.Requests > 0 {
				row["context_tokens"] = c.contextTokens
				// 上下文填充率：平均单请求占用 token（输入+两向缓存）占
				// 窗口上限的比例——衡量「窗口挤不挤」，不是累计量。
				row["avg_context_tokens"] = float64(m.InputTokens+m.CacheRead+m.CacheWrite) / float64(m.Requests)
				row["context_fill_pct"] = row["avg_context_tokens"].(float64) / float64(c.contextTokens) * 100
			}
		}
		models = append(models, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot":      snap,
		"models":        models,
		"est_cost":      totalCost,
		"cost_basis":    "catalog price per 1M tokens (estimate, not invoice)",
		"price_missing": len(catalog) == 0,
	})
}

// modelCatalogEntry 是模型目录里用量页关心的部分：三类 token 单价
// （$/1M）与上下文窗口上限。
type modelCatalogEntry struct {
	input         float64
	cached        float64
	output        float64
	contextTokens int64
}

// CatalogPrice 是模型目录中单模型的 token 单价（$/1M tokens）。
// 移植面板（ccpanel）按它把 token 用量折算成估算成本。
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

// ModelUIDs 返回上游目录中全部模型 uid（排序后），供移植面板合成
// 渠道模型清单。
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
// 供移植面板把目录信息并入模型注册表页。缓存不可用时返回 nil。
func (h *Handler) CatalogModels(ctx context.Context) []map[string]any {
	models, err := h.cachedModels(ctx)
	if err != nil {
		return nil
	}
	return models
}
