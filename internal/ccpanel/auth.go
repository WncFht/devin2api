package ccpanel

import (
	"encoding/json"
	"net/http"
)

// handleLogin 实现 ccLoad 契约的 POST /login：body {mode,password|token}。
// admin 模式校验 dashboard.password，返回 token=密码本身——下游 Bearer
// 校验本来就接受密码，因此无会话表、无过期状态、重启不掉线。
// api_token 模式预留给 auth_tokens（S4 落地前一律 401）。
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
		respondError(w, http.StatusUnauthorized, "Invalid credentials")
	default:
		respondError(w, http.StatusBadRequest, "Invalid request format")
	}
}

// handleLogout 无服务端会话可清，回个成功让前端清本地 token 即可。
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	respondOK(w, map[string]any{"message": "已登出"})
}

// dashboardSession 返回合成 admin 会话；api_token 会话待 S4 补 description 等字段。
func (h *Handler) dashboardSession(w http.ResponseWriter, _ *http.Request) {
	respondOK(w, map[string]any{"role": "admin"})
}

// withAuth 是 /dashboard、/admin 组的 Bearer 门槛：凭据即面板密码。
// 401 触发前端 fetchWithAuth 跳回 /web/login.html；429 复用爆破锁定语义。
func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authed, locked := h.panel.CheckPanelBearer(r)
		if authed {
			next(w, r)
			return
		}
		if locked {
			respondError(w, http.StatusTooManyRequests, "登录尝试过多，请稍后再试")
			return
		}
		respondError(w, http.StatusUnauthorized, "未授权访问，请先登录")
	}
}
