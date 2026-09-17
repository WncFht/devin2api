package ccpanel

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/authtoken"
)

// optionalInt64JSON 区分「字段缺席」与「显式 null」——PUT 语义里
// 缺席=不动，null=清除（ccLoad 的 expires_at 更新语义）。
type optionalInt64JSON struct {
	set   bool
	value *int64
}

// UnmarshalJSON 记录字段是否出现，并把 null 归一成 nil。
func (v *optionalInt64JSON) UnmarshalJSON(data []byte) error {
	v.set = true
	if string(data) == "null" {
		v.value = nil
		return nil
	}
	var n int64
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	v.value = &n
	return nil
}

// tokenLimitFields 是创建/更新共用的限额字段（美元浮点入、micro 出）。
type tokenLimitFields struct {
	CostLimitUSD        *float64 `json:"cost_limit_usd"`
	Cost5hLimitUSD      *float64 `json:"cost_5h_limit_usd"`
	CostDailyLimitUSD   *float64 `json:"cost_daily_limit_usd"`
	CostWeeklyLimitUSD  *float64 `json:"cost_weekly_limit_usd"`
	CostMonthlyLimitUSD *float64 `json:"cost_monthly_limit_usd"`
	MaxConcurrency      *int     `json:"max_concurrency"`
	MaxRPM              *int     `json:"max_rpm"`
}

// applyTo 把限额写进令牌内部 micro 字段；负值由调用方预检拦掉。
func (f tokenLimitFields) applyTo(t *authtoken.Token) {
	if f.CostLimitUSD != nil {
		t.CostLimitMicroUSD = int64(*f.CostLimitUSD * 1e6)
	}
	if f.Cost5hLimitUSD != nil {
		t.Cost5hLimitMicroUSD = int64(*f.Cost5hLimitUSD * 1e6)
	}
	if f.CostDailyLimitUSD != nil {
		t.DailyLimitMicroUSD = int64(*f.CostDailyLimitUSD * 1e6)
	}
	if f.CostWeeklyLimitUSD != nil {
		t.CostWeeklyLimitMicroUSD = int64(*f.CostWeeklyLimitUSD * 1e6)
	}
	if f.CostMonthlyLimitUSD != nil {
		t.MonthlyLimitMicroUSD = int64(*f.CostMonthlyLimitUSD * 1e6)
	}
	if f.MaxConcurrency != nil {
		t.MaxConcurrency = *f.MaxConcurrency
	}
	if f.MaxRPM != nil {
		t.MaxRPM = *f.MaxRPM
	}
}

// validateLimits 逐项拒掉负限额，文案对齐 ccLoad 的 400 信息。
func (f tokenLimitFields) validateLimits(w http.ResponseWriter) bool {
	for _, c := range []struct {
		name string
		v    *float64
	}{
		{"cost_limit_usd", f.CostLimitUSD},
		{"cost_5h_limit_usd", f.Cost5hLimitUSD},
		{"cost_daily_limit_usd", f.CostDailyLimitUSD},
		{"cost_weekly_limit_usd", f.CostWeeklyLimitUSD},
		{"cost_monthly_limit_usd", f.CostMonthlyLimitUSD},
	} {
		if c.v != nil && *c.v < 0 {
			respondError(w, http.StatusBadRequest, c.name+" must be >= 0")
			return false
		}
	}
	if f.MaxConcurrency != nil && *f.MaxConcurrency < 0 {
		respondError(w, http.StatusBadRequest, "max_concurrency must be >= 0")
		return false
	}
	if f.MaxRPM != nil && *f.MaxRPM < 0 {
		respondError(w, http.StatusBadRequest, "max_rpm must be >= 0")
		return false
	}
	return true
}

// tokensUnavailable 在令牌仓未接线时回 503（开发期中间态）。
func (h *Handler) tokensUnavailable(w http.ResponseWriter) bool {
	if h.tokens == nil {
		respondError(w, http.StatusServiceUnavailable, "auth token store unavailable")
		return true
	}
	return false
}

// adminCreateAuthToken 实现 POST /admin/auth-tokens：生成令牌并一次性
// 返回明文；响应字段与 ccLoad HandleCreateAuthToken 逐项对齐。
// anonymous=true 时种的是匿名通道行（空明文）：响应不含 token 字段——
// 没有可出示的凭据，未带凭据的 /v1 请求即按该行准入。仓内至多一行。
func (h *Handler) adminCreateAuthToken(w http.ResponseWriter, r *http.Request) {
	if h.tokensUnavailable(w) {
		return
	}
	var req struct {
		Description   string   `json:"description"`
		ExpiresAt     *int64   `json:"expires_at"`
		IsActive      *bool    `json:"is_active"`
		AllowedModels []string `json:"allowed_models"`
		Anonymous     bool     `json:"anonymous"`
		Class         string   `json:"class"`
		tokenLimitFields
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Description) == "" {
		if !req.Anonymous {
			respondError(w, http.StatusBadRequest, "Key: 'Description' Error:Field validation for 'Description' failed on the 'required' tag")
			return
		}
		req.Description = "anonymous"
	}
	if !authtoken.ValidateClass(req.Class) {
		respondError(w, http.StatusBadRequest, "class must be 'fg' or 'bg'")
		return
	}
	if !req.validateLimits(w) {
		return
	}
	t := &authtoken.Token{
		Description:   req.Description,
		ExpiresAt:     req.ExpiresAt,
		IsActive:      req.IsActive == nil || *req.IsActive,
		AllowedModels: req.AllowedModels,
		Class:         authtoken.NormalizeClass(req.Class),
	}
	req.applyTo(t)
	if req.Anonymous {
		row, created, err := h.tokens.Ensure("", t)
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if !created {
			// 匿名行至多一行：已有行时不再新建，按冲突返回现有行 id。
			respondError(w, http.StatusConflict, "anonymous token already exists: id "+strconv.FormatInt(row.ID, 10))
			return
		}
		respondOK(w, row.API())
		return
	}
	plain, err := h.tokens.Create(t)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(w, map[string]any{
		"id":              t.ID,
		"token":           plain,
		"description":     t.Description,
		"created_at":      t.CreatedAt,
		"expires_at":      t.ExpiresAt,
		"is_active":       t.IsActive,
		"allowed_models":  t.AllowedModels,
		"max_concurrency": t.MaxConcurrency,
		"max_rpm":         t.MaxRPM,
		"class":           t.Class,
	})
}

// adminUpdateAuthToken 实现 PUT /admin/auth-tokens/{id}：指针字段区分
// 缺席与置空；expires_at 走 optionalInt64JSON 支持显式 null 清除。
func (h *Handler) adminUpdateAuthToken(w http.ResponseWriter, r *http.Request) {
	if h.tokensUnavailable(w) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid token id")
		return
	}
	var req struct {
		Description   *string           `json:"description"`
		IsActive      *bool             `json:"is_active"`
		ExpiresAt     optionalInt64JSON `json:"expires_at"`
		AllowedModels *[]string         `json:"allowed_models"`
		Class         *string           `json:"class"`
		tokenLimitFields
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Class != nil && !authtoken.ValidateClass(*req.Class) {
		respondError(w, http.StatusBadRequest, "class must be 'fg' or 'bg'")
		return
	}
	if !req.validateLimits(w) {
		return
	}
	t, ok := h.tokens.Get(id)
	if !ok {
		respondError(w, http.StatusNotFound, "token not found")
		return
	}
	if req.Description != nil {
		t.Description = *req.Description
	}
	if req.IsActive != nil {
		t.IsActive = *req.IsActive
	}
	if req.ExpiresAt.set {
		t.ExpiresAt = req.ExpiresAt.value
	}
	if req.AllowedModels != nil {
		t.AllowedModels = *req.AllowedModels
	}
	if req.Class != nil {
		t.Class = authtoken.NormalizeClass(*req.Class)
	}
	req.applyTo(t)
	if err := h.tokens.Update(t); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(w, t.API())
}

// adminDeleteAuthToken 实现 DELETE /admin/auth-tokens/{id} → {id}。
func (h *Handler) adminDeleteAuthToken(w http.ResponseWriter, r *http.Request) {
	if h.tokensUnavailable(w) {
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid token id")
		return
	}
	if err := h.tokens.Delete(id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(w, map[string]any{"id": id})
}
