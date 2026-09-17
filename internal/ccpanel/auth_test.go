// 本文件验证面板爆破防护账本：失败计数、锁定与机会清扫。
package ccpanel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/store"
)

// TestLoginFailureSweep 验证失败路径会清扫已失效的爆破条目——纯爆破
// 流量永远不走成功路径，loginFailures 不能无界增长。
func TestLoginFailureSweep(t *testing.T) {
	handler, err := New("pw", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Hour)
	for i := 0; i < loginSweepThreshold+10; i++ {
		handler.loginFailures[fmt.Sprintf("10.0.0.%d", i)] = &loginFail{fails: 1, lastSeen: stale}
	}
	request := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(`{"mode":"admin","password":"wrong"}`))
	request.Header.Set("Content-Type", "application/json")
	request.RemoteAddr = "1.2.3.4:5678"
	recorder := httptest.NewRecorder()
	handler.handleLogin(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(handler.loginFailures) != 1 {
		t.Fatalf("loginFailures = %d entries, want stale entries swept", len(handler.loginFailures))
	}
}

// TestBearerFailureSharesLoginLedger 验证 Bearer 认证失败与表单登录共用
// 同一 IP 账本——只守 login 端点等于把全速穷举通道留给 Bearer。
func TestBearerFailureSharesLoginLedger(t *testing.T) {
	handler, err := New("pw", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	bearer := func(password string) *http.Request {
		request := httptest.NewRequest(http.MethodGet, "/admin/stats", nil)
		request.RemoteAddr = "1.2.3.4:5678"
		request.Header.Set("Authorization", "Bearer "+password)
		return request
	}
	for i := 0; i < loginMaxFails; i++ {
		if authed, _ := handler.CheckPanelBearer(bearer("wrong")); authed {
			t.Fatalf("attempt %d: wrong Bearer must not authenticate", i)
		}
	}
	state := handler.loginFailures["1.2.3.4"]
	if state == nil || !time.Now().Before(state.lockedUntil) {
		t.Fatalf("after %d Bearer failures IP must be locked: %+v", loginMaxFails, state)
	}
	// 锁定期内继续失败的 Bearer 拿 429 而非 401——账本不只记录，
	// 要真的把穷举通道节死掉。
	recorder := httptest.NewRecorder()
	handler.withAuth(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })(recorder, bearer("wrong"))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("locked Bearer status = %d, want 429", recorder.Code)
	}
	// 锁定只抬高爆破代价：正确密码在锁定期内仍放行并清账本（与表单登录一致）。
	if authed, _ := handler.CheckPanelBearer(bearer("pw")); !authed {
		t.Fatal("correct Bearer must authenticate even under lockout")
	}
	if _, ok := handler.loginFailures["1.2.3.4"]; ok {
		t.Fatal("correct Bearer must clear the failure ledger")
	}
}

// TestWebAuthPasswordBeatsSeededToken 覆盖播种回归：auth.api_key 被种进
// 令牌仓后，拿它当 Bearer 的存量面板会话会被 Resolve 命中——若直接判
// api_token，管理员会被降级到受限导航。密码命中（含开放面板）时必须
// 仍给 admin；只有「非密码」的真实令牌才进 api_token。
func TestWebAuthPasswordBeatsSeededToken(t *testing.T) {
	newStore := func(t *testing.T) *authtoken.Store {
		t.Helper()
		db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		s, err := authtoken.New(db)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	probe := func(h *Handler, bearer string) (int, string) {
		var role string
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/dashboard/session", nil)
		request.RemoteAddr = "1.2.3.4:5678"
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		h.withWebAuth(func(w http.ResponseWriter, r *http.Request) {
			role = identityFrom(r).Role
			w.WriteHeader(http.StatusOK)
		})(recorder, request)
		return recorder.Code, role
	}

	// 开放面板（password==""）：播种的 api_key 命中 Resolve，但仍应 admin。
	open, err := New("", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t)
	if _, _, err := store.Ensure("the-api-key", &authtoken.Token{Description: "config: auth.api_key", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	open.SetTokenStore(store)
	if code, role := probe(open, "the-api-key"); code != http.StatusOK || role != "admin" {
		t.Fatalf("open panel + seeded key = (%d, %q), want (200, admin)", code, role)
	}

	// 密码面板 + Bearer 与密码同值（用户拿 api_key 当管理密码用）→ admin。
	guarded, err := New("the-api-key", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	store2 := newStore(t)
	if _, _, err := store2.Ensure("the-api-key", &authtoken.Token{Description: "config: auth.api_key", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store2.Ensure("real-token", &authtoken.Token{Description: "t", IsActive: true}); err != nil {
		t.Fatal(err)
	}
	guarded.SetTokenStore(store2)
	if code, role := probe(guarded, "the-api-key"); code != http.StatusOK || role != "admin" {
		t.Fatalf("bearer == panel password = (%d, %q), want (200, admin)", code, role)
	}
	// 与密码不同值的真实令牌 → 仍是 api_token 受限身份。
	if code, role := probe(guarded, "real-token"); code != http.StatusOK || role != "api_token" {
		t.Fatalf("distinct token = (%d, %q), want (200, api_token)", code, role)
	}
}
