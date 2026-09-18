package ccpanel

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

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
		Name               string  `json:"name"`
		Token              string  `json:"token"`
		CredentialsFile    string  `json:"credentials_file"`
		CredentialsContent string  `json:"credentials_content"`
		APIKey             string  `json:"api_key"`
		Disabled           bool    `json:"disabled"`
		Verify             bool    `json:"verify"`
		Priority           *int64  `json:"priority"`
		MaxRPM             *int64  `json:"max_rpm"`
		Notes              *string `json:"notes"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if !accountNamePattern.MatchString(req.Name) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid account name %q", req.Name))
		return
	}
	if req.CredentialsFile != "" && req.CredentialsContent != "" {
		respondError(w, http.StatusBadRequest, "credentials_file and credentials_content are mutually exclusive")
		return
	}
	if req.Priority != nil && *req.Priority < 0 {
		respondError(w, http.StatusBadRequest, "priority must be >= 0")
		return
	}
	if req.MaxRPM != nil && *req.MaxRPM < 0 {
		respondError(w, http.StatusBadRequest, "max_rpm must be >= 0")
		return
	}
	in := AccountWrite{
		Name:               req.Name,
		Token:              req.Token,
		CredentialsFile:    req.CredentialsFile,
		CredentialsContent: req.CredentialsContent,
		APIKey:             req.APIKey,
		Disabled:           req.Disabled,
		Verify:             req.Verify,
		Priority:           req.Priority,
		MaxRPM:             req.MaxRPM,
		Notes:              req.Notes,
	}
	if req.Verify {
		// verify 是建行前的凭据探测：解析与持久化同一口径（ops 侧），
		// 探测走面板上游链——失败 400 不建行。
		if h.accountOps.CredentialOf == nil {
			respondError(w, http.StatusBadRequest, "credential verification unavailable")
			return
		}
		token, err := h.accountOps.CredentialOf(in)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if _, _, _, err := h.fetchUserStatusAs(r.Context(), token); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("credential verification failed: %v", err))
			return
		}
	}
	resolved, err := h.accountOps.Create(r.Context(), in)
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
		Token              *string `json:"token"`
		CredentialsFile    *string `json:"credentials_file"`
		CredentialsContent *string `json:"credentials_content"`
		APIKey             *string `json:"api_key"`
		Disabled           *bool   `json:"disabled"`
		Priority           *int64  `json:"priority"`
		MaxRPM             *int64  `json:"max_rpm"`
		Notes              *string `json:"notes"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CredentialsFile != nil && req.CredentialsContent != nil {
		respondError(w, http.StatusBadRequest, "credentials_file and credentials_content are mutually exclusive")
		return
	}
	if req.Priority != nil && *req.Priority < 0 {
		respondError(w, http.StatusBadRequest, "priority must be >= 0")
		return
	}
	if req.MaxRPM != nil && *req.MaxRPM < 0 {
		respondError(w, http.StatusBadRequest, "max_rpm must be >= 0")
		return
	}
	if req.Token == nil && req.CredentialsFile == nil && req.CredentialsContent == nil &&
		req.APIKey == nil && req.Disabled == nil && req.Priority == nil && req.MaxRPM == nil && req.Notes == nil {
		respondError(w, http.StatusBadRequest, "nothing to update")
		return
	}
	resolved, err := h.accountOps.Update(r.Context(), name, AccountPatch{
		Token:              req.Token,
		CredentialsFile:    req.CredentialsFile,
		CredentialsContent: req.CredentialsContent,
		APIKey:             req.APIKey,
		Disabled:           req.Disabled,
		Priority:           req.Priority,
		MaxRPM:             req.MaxRPM,
		Notes:              req.Notes,
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

// adminTestAccount 实现 POST /admin/accounts/{name}/test：以生效凭据
// 探测上游 GetUserStatus 并计时。探测失败是结果不是 HTTP 错误——恒
// 200：失败 {ok:false,latency_ms,error}，成功 {ok:true,latency_ms,
// user,plan} 并顺带把配额信号回灌池侧、清该号冷却。名不在生效集/
// tombstoned/凭据不可解 404（与 quota/refresh 同口径）。
func (h *Handler) adminTestAccount(w http.ResponseWriter, r *http.Request) {
	if h.accountOpsUnavailable(w) {
		return
	}
	name, ok := accountNameParam(w, r)
	if !ok {
		return
	}
	token, err := h.accountOps.TokenOf(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrAccountNotFound) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("account %q not found", name))
			return
		}
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	start := time.Now()
	user, plan, _, err := h.fetchUserStatusAs(r.Context(), token)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		respondOK(w, map[string]any{
			"ok":         false,
			"latency_ms": latency,
			"error":      err.Error(),
		})
		return
	}
	h.noteAccountQuotaSignal(name, plan)
	h.accountOps.ClearCooldown(name)
	resp := map[string]any{"ok": true, "latency_ms": latency, "user": user}
	if plan != nil {
		resp["plan"] = plan
	}
	respondOK(w, resp)
}
