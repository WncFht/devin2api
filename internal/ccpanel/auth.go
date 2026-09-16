package ccpanel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"
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

// loginFail 记录单个来源 IP 的连续登录失败状态。
type loginFail struct {
	// fails 是上次锁定以来的连续失败次数。
	fails int
	// lockedUntil 是锁定截止时间；到期前失败的请求直接 429。
	lockedUntil time.Time
	// lastSeen 是最近一次失败时刻：闲置超过一个锁定周期的条目计数
	// 清零并可被机会清扫——爆破流量不走成功路径也能被回收。
	lastSeen time.Time
}

const (
	// loginMaxFails 是触发锁定的连续失败次数。
	loginMaxFails = 5
	// loginLockout 是达到失败上限后的锁定时长。
	loginLockout = 10 * time.Minute
	// loginSweepThreshold 是触发机会清扫的失败条目水位。
	loginSweepThreshold = 64
)

// noteLoginFailure 把一次凭据校验失败计入 IP 账本（登录表单、admin/api_token
// Bearer 认证共用），返回该 IP 当前是否处于锁定期；顺带按水位机会清扫过期
// 条目——纯爆破流量不走成功路径，失败条目只增不扫会无界增长。
func (h *Handler) noteLoginFailure(ip string) bool {
	h.loginMu.Lock()
	defer h.loginMu.Unlock()
	now := time.Now()
	state := h.loginFailures[ip]
	if state == nil {
		state = &loginFail{}
		h.loginFailures[ip] = state
	}
	if now.Before(state.lockedUntil) {
		state.lastSeen = now
		return true
	}
	// 距上次失败超过一个锁定周期视为新一波尝试：陈旧计数跨时间
	// 累积会把低频手滑误算成爆破。
	if now.Sub(state.lastSeen) > loginLockout {
		state.fails = 0
	}
	state.lastSeen = now
	state.fails++
	if state.fails >= loginMaxFails {
		state.fails = 0
		state.lockedUntil = now.Add(loginLockout)
	}
	if len(h.loginFailures) > loginSweepThreshold {
		h.sweepLoginFailures(now)
	}
	return false
}

// sweepLoginFailures 清掉锁定已过期且闲置超过一个锁定周期的失败条目。
// 仍在锁定中或近期仍有失败活动的条目保留。调用方须持有 loginMu。
func (h *Handler) sweepLoginFailures(now time.Time) {
	for key, state := range h.loginFailures {
		if !now.Before(state.lockedUntil) && now.Sub(state.lastSeen) > loginLockout {
			delete(h.loginFailures, key)
		}
	}
}

// clearLoginFailure 清掉该 IP 的失败账本——正确凭据在锁定期内也放行
// 并清零：锁定只为抬高爆破代价，持对凭据的真用户不被挡在门外。
func (h *Handler) clearLoginFailure(ip string) {
	h.loginMu.RLock()
	_, hasEntry := h.loginFailures[ip]
	h.loginMu.RUnlock()
	if hasEntry {
		h.loginMu.Lock()
		delete(h.loginFailures, ip)
		h.loginMu.Unlock()
	}
}

// checkPasswordCredential 校验单份密码凭据（Bearer 头或登录表单的明文），
// 成功清该 IP 的失败账本，失败计入账本并报告是否已进入锁定期。
func (h *Handler) checkPasswordCredential(provided string, passwordHash [32]byte, ip string) (ok, locked bool) {
	sum := sha256.Sum256([]byte(provided))
	if subtle.ConstantTimeCompare(sum[:], passwordHash[:]) == 1 {
		h.clearLoginFailure(ip)
		return true, false
	}
	return false, h.noteLoginFailure(ip)
}

// CheckPanelBearer 校验 Authorization: Bearer 头中的密码凭据：
// 移植前端把密码本身当 Bearer token 用，不发 cookie、不查会话表。
// locked 报告来源 IP 是否处于登录锁定期——Bearer 失败与表单登录共用
// 同一 IP 账本，只守 login 端点等于把全速穷举通道留给 Bearer；
// authed 为真时 locked 无意义。
func (h *Handler) CheckPanelBearer(r *http.Request) (authed, locked bool) {
	password, passwordHash := h.passwordSnapshot()
	if password == "" {
		return true, false
	}
	auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false, false
	}
	return h.checkPasswordCredential(auth, passwordHash, remoteIP(r))
}

// CheckPanelPassword 校验登录表单提交的明文密码（/login admin 模式用）。
func (h *Handler) CheckPanelPassword(pw string, r *http.Request) (ok, locked bool) {
	password, passwordHash := h.passwordSnapshot()
	if password == "" {
		return true, false
	}
	return h.checkPasswordCredential(pw, passwordHash, remoteIP(r))
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
		ok, locked := h.CheckPanelPassword(req.Password, r)
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
				h.clearLoginFailure(remoteIP(r))
				respondOK(w, map[string]any{
					"token":     req.Token,
					"expiresIn": 86400,
					"role":      "api_token",
				})
				return
			}
		}
		// 令牌爆破面与密码等价（都直开面板只读面），共用同一按 IP
		// 失败账本：失败计数与 admin 分支同语义进 ledger。
		if h.noteLoginFailure(remoteIP(r)) {
			respondError(w, http.StatusTooManyRequests, "Too many failed login attempts")
			return
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
		authed, locked := h.CheckPanelBearer(r)
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

// withWebAuth 是 /dashboard 组的门槛：api_token Bearer 先解析（有效令牌
// 直接进 api_token 身份并清失败账本）；解析不中再走密码校验——有效令牌
// 若在密码校验上计失败，持钥人每请求 +1，5 次后被误判锁定。校验失败的
// Bearer 由 CheckPanelBearer 计入共享 IP 账本（一次失败只计一次，两种
// 凭据不重复记）。面板密码为空（开放面板）时无 Bearer 也按 admin 放行——
// CheckPanelBearer 的开放语义已覆盖这条。
func (h *Handler) withWebAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.tokens != nil {
			if t, ok := h.tokens.Resolve(bearerToken(r)); ok {
				h.clearLoginFailure(remoteIP(r))
				next(w, r.WithContext(context.WithValue(r.Context(), identityContextKey{}, webIdentity{
					Role:    "api_token",
					TokenID: t.ID,
					KeyHash: t.KeyHash(),
				})))
				return
			}
		}
		authed, locked := h.CheckPanelBearer(r)
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
