package ccpanel

import (
	"context"
	"encoding/json"
	"net/http"
)

// webIdentity 是一次面板请求的身份：admin=密码持有者，api_token=下游
// 令牌持有者（只读、按 auth_token_id 限定数据范围）。
type webIdentity struct {
	Role string
	// TokenID 是 api_token 身份的令牌 id；admin 为 0。
	TokenID int64
	// KeyHash 是令牌对应 index.jsonl 的 key_hash（哈希前 16 hex），
	// 用于把 /dashboard 查询强制收敛到该令牌的数据。
	KeyHash string
}

type identityContextKey struct{}

// identityFrom 取请求的面板身份；未经 withWebAuth 的链路返回 admin
// （本地面板语义：到得了 handler 必有身份，缺省按 admin 不丢数据）。
func identityFrom(r *http.Request) webIdentity {
	if id, ok := r.Context().Value(identityContextKey{}).(webIdentity); ok {
		return id
	}
	return webIdentity{Role: "admin"}
}

// bearerToken 提取 Authorization: Bearer 的凭据部分。
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(auth) > len(prefix) && auth[:len(prefix)] == prefix {
		return auth[len(prefix):]
	}
	return ""
}

// handleLogin 实现 ccLoad 契约的 POST /login：body {mode,password|token}。
// admin 模式校验 dashboard.password，返回 token=密码本身——下游 Bearer
// 校验本来就接受密码，因此无会话表、无过期状态、重启不掉线。
// api_token 模式校验 auth_tokens 仓里的下游令牌，返回 token=明文令牌本身，
// 面板后续 Bearer<令牌> 经 withWebAuth 解析回 api_token 身份。
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode     string `json:"mode"`
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "Invalid request format")
		return
	}
	switch req.Mode {
	case "admin":
		ok, locked := h.panel.CheckPanelPassword(req.Password, r)
		if ok {
			respondOK(w, map[string]any{
				"token":     req.Password,
				"expiresIn": 86400,
				"role":      "admin",
			})
			return
		}
		if locked {
			respondError(w, http.StatusTooManyRequests, "Too many failed login attempts")
			return
		}
		respondError(w, http.StatusUnauthorized, "Invalid credentials")
	case "api_token":
		if h.tokens != nil {
			if _, ok := h.tokens.Resolve(req.Token); ok {
				respondOK(w, map[string]any{
					"token":     req.Token,
					"expiresIn": 86400,
					"role":      "api_token",
				})
				return
			}
		}
		respondError(w, http.StatusUnauthorized, "Invalid credentials")
	default:
		respondError(w, http.StatusBadRequest, "Invalid request format")
	}
}

// handleLogout 无服务端会话可清，回个成功让前端清本地 token 即可。
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	respondOK(w, map[string]any{"message": "已登出"})
}

// dashboardSession 实现 GET /dashboard/session：按身份回角色形状——
// admin 只有 role；api_token 附带 ccLoad 契约的令牌视图字段。
func (h *Handler) dashboardSession(w http.ResponseWriter, r *http.Request) {
	id := identityFrom(r)
	if id.Role == "api_token" && h.tokens != nil {
		if t, ok := h.tokens.Get(id.TokenID); ok && t.IsValid() {
			models := t.AllowedModels
			if models == nil {
				models = []string{}
			}
			api := t.API()
			respondOK(w, map[string]any{
				"role":            "api_token",
				"auth_token_id":   t.ID,
				"description":     t.Description,
				"allowed_models":  models,
				"cost_used_usd":   api.CostUsedUSD,
				"cost_limit_usd":  api.CostLimitUSD,
				"max_concurrency": t.MaxConcurrency,
			})
			return
		}
		respondError(w, http.StatusUnauthorized, "API Token 已失效")
		return
	}
	respondOK(w, map[string]any{"role": "admin"})
}

// withAuth 是 /admin 组的 Bearer 门槛：只认面板密码（admin）。
// 401 触发前端 fetchWithAuth 跳回 /web/login.html；429 复用爆破锁定语义。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authed, locked := h.panel.CheckPanelBearer(r)
		if authed {
			next(w, r.WithContext(context.WithValue(r.Context(), identityContextKey{}, webIdentity{Role: "admin"})))
			return
		}
		if locked {
			respondError(w, http.StatusTooManyRequests, "登录尝试过多，请稍后再试")
			return
		}
		respondError(w, http.StatusUnauthorized, "未授权访问，请先登录")
	}
}

// withWebAuth 是 /dashboard 组的门槛：admin Bearer 直通；否则尝试把
// Bearer 解析为下游令牌（api_token 身份进 ctx，handler 据 TokenID/KeyHash
// 收敛数据范围）。面板密码为空（开放面板）时无 Bearer 也按 admin 放行——
// CheckPanelBearer 的开放语义已覆盖这条。
func (h *Handler) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authed, locked := h.panel.CheckPanelBearer(r)
		if authed {
			next(w, r.WithContext(context.WithValue(r.Context(), identityContextKey{}, webIdentity{Role: "admin"})))
			return
		}
		if !locked && h.tokens != nil {
			if t, ok := h.tokens.Resolve(bearerToken(r)); ok {
				next(w, r.WithContext(context.WithValue(r.Context(), identityContextKey{}, webIdentity{
					Role:    "api_token",
					TokenID: t.ID,
					KeyHash: t.KeyHash(),
				})))
				return
			}
		}
		if locked {
			respondError(w, http.StatusTooManyRequests, "登录尝试过多，请稍后再试")
			return
		}
		respondError(w, http.StatusUnauthorized, "未授权访问，请先登录")
	}
}
