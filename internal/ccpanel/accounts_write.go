package ccpanel

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// adminCreateAccount 实现 POST /admin/accounts（建号）。
func (h *Handler) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}

// adminUpdateAccount 实现 PUT /admin/accounts/{name}（改凭据/停启用）。
func (h *Handler) adminUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}

// adminDeleteAccount 实现 DELETE /admin/accounts/{name}：config 名置
// 墓碑，panel 名物理删。
func (h *Handler) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}

// adminRestoreAccount 实现 POST /admin/accounts/{name}/restore。
func (h *Handler) adminRestoreAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}

// adminClearAccountCooldown 实现 POST /admin/accounts/{name}/clear-cooldown。
func (h *Handler) adminClearAccountCooldown(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	respondError(w, http.StatusNotImplemented, "not implemented")
}

// adminRefreshAccountQuota 实现 POST /admin/accounts/{name}/quota/refresh：
// 即采一次该号配额（不经 lane；disabled 可刷，tombstoned 404）。
func (h *Handler) adminRefreshAccountQuota(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name := chi.URLParam(r, "name")
	token, err := h.accountOps.TokenOf(r.Context(), name)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	data, err := h.refreshAccountQuota(r.Context(), name, token)
	if err != nil {
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	respondOK(w, data)
}
