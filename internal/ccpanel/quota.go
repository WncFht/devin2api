package ccpanel

import (
	"context"
	"net/http"
	"time"
)

// adminQuota 实现 GET /admin/quota：日/周配额历史曲线与燃烧速率预测，
// 数据来自旧面板采样协程落的 logs/quota.jsonl。账户计费数据只对 admin 开放。
func (h *Handler) adminQuota(w http.ResponseWriter, r *http.Request) {
	respondOK(w, h.panel.QuotaReport())
}

// adminStatus 实现 GET /admin/status：上游账户/plan/容量/IDE/模型状态/
// 供应商的六路聚合（与旧面板 /panel/api/status 同一数据源）。
func (h *Handler) adminStatus(w http.ResponseWriter, r *http.Request) {
	// 与旧面板同口径：多路上游调用聚合，超时对齐 ResponseHeaderTimeout。
	ctx, cancel := context.WithTimeout(r.Context(), 610*time.Second)
	defer cancel()
	respondOK(w, h.panel.StatusReport(ctx))
}
