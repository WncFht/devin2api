// 本文件是 /panel/api/usage 端点：index.jsonl 聚合快照 + 按模型目录价的估算成本。
package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
)

// apiUsage 返回 index.jsonl 聚合快照，并按模型目录价附估算成本。
// 价格是 catalog 标价（$/1M tokens），est_cost 为参考值而非上游账单。
func (h *Handler) apiUsage(w http.ResponseWriter, r *http.Request) {
	if !h.requireAuth(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if h.debugManager == nil {
		_, _ = w.Write([]byte(`{"disabled":true}`))
		return
	}
	snap := h.debugManager.UsageStats()
	prices := h.modelPriceMap(r.Context())
	var totalCost float64
	models := make([]map[string]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		row := map[string]any{
			"name": m.Name, "requests": m.Requests, "errors": m.Errors, "disconnected": m.Disconnected,
			"rate_limited": m.RateLimited,
			"input_tokens": m.Input, "output_tokens": m.Output,
			"cache_read_tokens": m.CacheRead, "cache_write_tokens": m.CacheWrite,
			"reasoning_tokens": m.Reasoning, "total_tokens": m.TotalTokens,
			"avg_duration_ms": m.AvgDuration, "avg_ttfb_ms": m.AvgTTFB,
			"success_rate": m.SuccessRate, "last_result": m.LastResult, "last_at": m.LastAt,
		}
		if p, ok := prices[m.Name]; ok {
			// cache_write 实测按 input 价计费：配额翻转拟合的隐含单价 ≈ input 价，
			// 并非 Anthropic 惯例的 1.25×；catalog 无独立 cache_write 价格维。
			cost := (float64(m.Input+m.CacheWrite)*p.input + float64(m.CacheRead)*p.cached + float64(m.Output)*p.output) / 1e6
			row["est_cost"] = cost
			totalCost += cost
		}
		models = append(models, row)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"snapshot":      snap,
		"models":        models,
		"est_cost":      totalCost,
		"cost_basis":    "catalog price per 1M tokens (estimate, not invoice)",
		"price_missing": len(prices) == 0,
	})
}

// modelPrice 是目录价：三类 token 的单价（$/1M tokens）。
type modelPrice struct {
	input  float64
	cached float64
	output float64
}

// modelPriceMap 从模型目录缓存取 uid → 三类 token 单价（$/1M）。
func (h *Handler) modelPriceMap(ctx context.Context) map[string]modelPrice {
	out := map[string]modelPrice{}
	models, err := h.cachedModels(ctx)
	if err != nil {
		return nil
	}
	for _, m := range models {
		uid, _ := m["uid"].(string)
		if uid == "" {
			continue
		}
		out[uid] = modelPrice{
			input:  floatAny(m["price_input"]),
			cached: floatAny(m["price_cached"]),
			output: floatAny(m["price_output"]),
		}
	}
	return out
}
