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
	catalog := h.modelCatalogMap(r.Context())
	var totalCost float64
	models := make([]map[string]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		row := map[string]any{
			"name": m.Name, "requests": m.Requests, "errors": m.Errors, "disconnected": m.Disconnected,
			"rate_limited":  m.RateLimited,
			"client_faults": m.ClientFaults, "upstream_faults": m.UpstreamFaults,
			"sla_success_rate": m.SLASuccessRate,
			"input_tokens":     m.Input, "output_tokens": m.Output,
			"input_p50": m.InTokP50, "input_p95": m.InTokP95,
			"output_p50": m.OutTokP50, "output_p95": m.OutTokP95,
			"cache_read_tokens": m.CacheRead, "cache_write_tokens": m.CacheWrite,
			"reasoning_tokens": m.Reasoning, "total_tokens": m.TotalTokens,
			"gen_ms": m.GenMS, "gen_tokens": m.GenOut,
			"avg_duration_ms": m.AvgDuration, "avg_ttfb_ms": m.AvgTTFB,
			"success_rate": m.SuccessRate, "last_result": m.LastResult, "last_at": m.LastAt,
		}
		if c, ok := catalog[m.Name]; ok {
			// cache_write 实测按 input 价计费：配额翻转拟合的隐含单价 ≈ input 价，
			// 并非 Anthropic 惯例的 1.25×；catalog 无独立 cache_write 价格维。
			cost := (float64(m.Input+m.CacheWrite)*c.input + float64(m.CacheRead)*c.cached + float64(m.Output)*c.output) / 1e6
			row["est_cost"] = cost
			totalCost += cost
			if c.contextTokens > 0 && m.Requests > 0 {
				row["context_tokens"] = c.contextTokens
				// 上下文填充率：平均单请求占用 token（输入+两向缓存）占
				// 窗口上限的比例——衡量「窗口挤不挤」，不是累计量。
				row["avg_context_tokens"] = float64(m.Input+m.CacheRead+m.CacheWrite) / float64(m.Requests)
				row["context_fill_pct"] = row["avg_context_tokens"].(float64) / float64(c.contextTokens) * 100
			}
		}
		models = append(models, row)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
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
