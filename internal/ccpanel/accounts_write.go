package ccpanel

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/store"
)

// accountNamePattern 与 config.devinAccountNamePattern 同字面量（config 侧
// 未导出）：面板对 path/body 里的账号名做同一约束的管线前快检，非法名
// 不必走到 ops 层的整表校验。
var accountNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`)

// accountNameParam 取 {name} path 参数并快检；非法时写 400 并返回 false。
func accountNameParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := chi.URLParam(r, "name")
	if !accountNamePattern.MatchString(name) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid account name %q", name))
		return "", false
	}
	return name, true
}

// adminCreateAccount 实现 POST /admin/accounts（建号）。
func (h *Handler) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	var req struct {
		Name            string `json:"name"`
		Token           string `json:"token"`
		CredentialsFile string `json:"credentials_file"`
		Disabled        bool   `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !accountNamePattern.MatchString(req.Name) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid account name %q", req.Name))
		return
	}
	resolved, err := h.accountOps.Create(r.Context(), AccountWrite{
		Name:            req.Name,
		Token:           req.Token,
		CredentialsFile: req.CredentialsFile,
		Disabled:        req.Disabled,
	})
	if err != nil {
		// 重名（含 config 声明名与墓碑名）是唯一的 409；其余错误——
		// ResolveAccounts 整表校验文案、缺 base_url/model——原样 400 透传。
		if errors.Is(err, store.ErrAccountExists) {
			respondError(w, http.StatusConflict, fmt.Sprintf("account %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(w, h.accountView(r.Context(), resolved))
}

// adminUpdateAccount 实现 PUT /admin/accounts/{name}（改凭据/停启用）。
func (h *Handler) adminUpdateAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name, ok := accountNameParam(w, r)
	if !ok {
		return
	}
	// 指针字段区分缺席与显式空：显式 "" 是「清行覆盖」（config 名回落
	// config 值），不是「不变」。
	var req struct {
		Token           *string `json:"token"`
		CredentialsFile *string `json:"credentials_file"`
		Disabled        *bool   `json:"disabled"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Token == nil && req.CredentialsFile == nil && req.Disabled == nil {
		respondError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	resolved, err := h.accountOps.Update(r.Context(), name, AccountPatch{
		Token:           req.Token,
		CredentialsFile: req.CredentialsFile,
		Disabled:        req.Disabled,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAccountNotFound):
			respondError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", name))
		case errors.Is(err, store.ErrAccountTombstoned):
			respondError(w, http.StatusConflict, fmt.Sprintf("account %q is tombstoned; restore first", name))
		default:
			respondError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	respondOK(w, h.accountView(r.Context(), resolved))
}

// adminDeleteAccount 实现 DELETE /admin/accounts/{name}：config 名置
// 墓碑（可 restore），panel 名物理删。
func (h *Handler) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name, ok := accountNameParam(w, r)
	if !ok {
		return
	}
	resolved, err := h.accountOps.Delete(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrAccountNotFound) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", name))
			return
		}
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if resolved.ConfigDeclared {
		respondOK(w, map[string]any{"name": name, "tombstoned": true})
		return
	}
	respondOK(w, map[string]any{"name": name, "deleted": true})
}

// adminRestoreAccount 实现 POST /admin/accounts/{name}/restore。
func (h *Handler) adminRestoreAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name, ok := accountNameParam(w, r)
	if !ok {
		return
	}
	resolved, err := h.accountOps.Restore(r.Context(), name)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrAccountNotTombstoned):
			respondError(w, http.StatusConflict, fmt.Sprintf("account %q is not tombstoned", name))
		case errors.Is(err, store.ErrAccountNotFound):
			respondError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", name))
		default:
			respondError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	respondOK(w, h.accountView(r.Context(), resolved))
}

// adminClearAccountCooldown 实现 POST /admin/accounts/{name}/clear-cooldown：
// 清池侧两档冷却（auth + unhealthy），保留 last_failure_* 证据，不动
// gate 闩；无活 lane（disabled/tombstoned/未接线）404。
func (h *Handler) adminClearAccountCooldown(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name, ok := accountNameParam(w, r)
	if !ok {
		return
	}
	if !h.accountOps.ClearCooldown(name) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("account %q has no live lane", name))
		return
	}
	respondOK(w, map[string]any{"name": name, "cleared": true})
}

// adminRefreshAccountQuota 实现 POST /admin/accounts/{name}/quota/refresh：
// 即采一次该号配额（不经 lane；disabled 可刷，tombstoned 404）。
func (h *Handler) adminRefreshAccountQuota(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name, ok := accountNameParam(w, r)
	if !ok {
		return
	}
	token, err := h.accountOps.TokenOf(r.Context(), name)
	if err != nil {
		// 契约只定义 404/502 两档失败：名不在生效集、tombstoned 与
		// 凭据不可解同归 404；上游拉取失败在 refreshAccountQuota 侧 502。
		if errors.Is(err, store.ErrAccountNotFound) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", name))
			return
		}
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
