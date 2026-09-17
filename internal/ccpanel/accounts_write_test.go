// 本文件验证 /admin/accounts 写端点的请求解码、sentinel→状态码映射与
// 响应信封形状；accountView 内部投影由列表侧切片交付，这里只断言透传。
package ccpanel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/store"
)

// accountEnvelope 是 ccLoad 信封的最小解码形状。
type accountEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

// newAccountsHandler 起一个开放面板 Handler 并挂上给定操作面。
func newAccountsHandler(t *testing.T, ops AccountOps) *Handler {
	t.Helper()
	handler, err := New("", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	handler.SetAccountOps(ops)
	return handler
}

// accountRequest 构造带 {name} 路由参数的请求（name 为空串时不挂
// RouteContext，模拟路由外直调）。
func accountRequest(method, target, name, body string) *http.Request {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if name != "" {
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("name", name)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	}
	return req
}

// decodeEnvelope 校验响应写出的就是 ccLoad 信封。
func decodeEnvelope(t *testing.T, rec *httptest.ResponseRecorder) accountEnvelope {
	t.Helper()
	var env accountEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("response not an envelope: %v (%q)", err, rec.Body.String())
	}
	return env
}

// TestCreateAccount 验证建号成功路径：请求字段原样进 ops，200 回单号视图。
func TestCreateAccount(t *testing.T) {
	var got AccountWrite
	handler := newAccountsHandler(t, AccountOps{
		Create: func(_ context.Context, in AccountWrite) (*store.ResolvedAccount, error) {
			got = in
			return &store.ResolvedAccount{Name: in.Name, Source: store.AccountSourcePanel}, nil
		},
	})
	rec := httptest.NewRecorder()
	req := accountRequest(http.MethodPost, "/admin/accounts", "",
		`{"name":"randall","token":"sess-1","disabled":true}`)
	handler.adminCreateAccount(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.Name != "randall" || got.Token != "sess-1" || !got.Disabled || got.CredentialsFile != "" {
		t.Fatalf("ops input = %+v", got)
	}
	env := decodeEnvelope(t, rec)
	if !env.Success {
		t.Fatalf("success = false (%s)", env.Error)
	}
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["name"] != "randall" {
		t.Fatalf("data.name = %v", data["name"])
	}
}

// TestCreateAccountErrors 验证建号的错误映射：重名 409（文案含名）、
// 整表校验类错误 400 原文透传。
func TestCreateAccountErrors(t *testing.T) {
	cases := []struct {
		name     string
		opsErr   error
		wantCode int
		wantErr  string
	}{
		{
			name:     "duplicate",
			opsErr:   fmt.Errorf("account %q: %w", "randall", store.ErrAccountExists),
			wantCode: http.StatusConflict,
			wantErr:  `account "randall" already exists`,
		},
		{
			name:     "validation passthrough",
			opsErr:   errors.New(`devin.accounts[2]: duplicate token with account "yanjian"`),
			wantCode: http.StatusBadRequest,
			wantErr:  `devin.accounts[2]: duplicate token with account "yanjian"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newAccountsHandler(t, AccountOps{
				Create: func(context.Context, AccountWrite) (*store.ResolvedAccount, error) {
					return nil, tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "",
				`{"name":"randall","token":"sess-1"}`))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if env := decodeEnvelope(t, rec); env.Error != tc.wantErr {
				t.Fatalf("error = %q, want %q", env.Error, tc.wantErr)
			}
		})
	}
}

// TestCreateAccountBadName 验证非法名在管线前被 400 拦下，不进 ops。
func TestCreateAccountBadName(t *testing.T) {
	called := false
	handler := newAccountsHandler(t, AccountOps{
		Create: func(context.Context, AccountWrite) (*store.ResolvedAccount, error) {
			called = true
			return nil, nil
		},
	})
	for _, body := range []string{
		`{"name":"bad name","token":"t"}`,
		`{"name":"","token":"t"}`,
		`{"token":"t"}`,
	} {
		rec := httptest.NewRecorder()
		handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
	if called {
		t.Fatal("ops.Create must not run on invalid name")
	}
}

// TestUpdateAccountPatchSemantics 验证指针字段把「缺席」与「显式空」分开
// 传给 ops，并验证全缺席时 400。
func TestUpdateAccountPatchSemantics(t *testing.T) {
	var got AccountPatch
	handler := newAccountsHandler(t, AccountOps{
		Update: func(_ context.Context, name string, patch AccountPatch) (*store.ResolvedAccount, error) {
			got = patch
			return &store.ResolvedAccount{Name: name, Source: store.AccountSourceConfig, ConfigDeclared: true}, nil
		},
	})
	rec := httptest.NewRecorder()
	handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/randall", "randall",
		`{"token":"","disabled":true}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.Token == nil || *got.Token != "" {
		t.Fatalf("patch.Token = %v, want pointer to empty string", got.Token)
	}
	if got.Disabled == nil || !*got.Disabled {
		t.Fatalf("patch.Disabled = %v, want pointer to true", got.Disabled)
	}
	if got.CredentialsFile != nil {
		t.Fatalf("patch.CredentialsFile = %v, want nil (absent)", got.CredentialsFile)
	}

	rec = httptest.NewRecorder()
	handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/randall", "randall", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch status = %d, want 400", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error != "nothing to update" {
		t.Fatalf("error = %q", env.Error)
	}
}

// TestUpdateAccountErrors 验证改号的 sentinel 映射：不存在 404、
// 墓碑 409（指 restore）、其它 400。
func TestUpdateAccountErrors(t *testing.T) {
	cases := []struct {
		name     string
		opsErr   error
		wantCode int
		wantErr  string
	}{
		{"not found", fmt.Errorf("account %q: %w", "randall", store.ErrAccountNotFound), http.StatusNotFound, `account "randall" not found`},
		{"tombstoned", fmt.Errorf("account %q: %w", "randall", store.ErrAccountTombstoned), http.StatusConflict, `account "randall" is tombstoned; restore first`},
		{"validation", errors.New(`devin.accounts[0]: one of token/credentials_file is required`), http.StatusBadRequest, `devin.accounts[0]: one of token/credentials_file is required`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newAccountsHandler(t, AccountOps{
				Update: func(context.Context, string, AccountPatch) (*store.ResolvedAccount, error) {
					return nil, tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/randall", "randall",
				`{"disabled":true}`))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if env := decodeEnvelope(t, rec); env.Error != tc.wantErr {
				t.Fatalf("error = %q, want %q", env.Error, tc.wantErr)
			}
		})
	}
}

// TestDeleteAccount 验证删除结果分流：config 声明名回 tombstoned 标记，
// 纯面板名回 deleted 标记；名不在生效集 404。
func TestDeleteAccount(t *testing.T) {
	cases := []struct {
		name      string
		resolved  *store.ResolvedAccount
		opsErr    error
		wantCode  int
		wantField string
	}{
		{"config tombstoned", &store.ResolvedAccount{Name: "randall", ConfigDeclared: true, Source: store.AccountSourceTombstoned}, nil, http.StatusOK, "tombstoned"},
		{"panel deleted", &store.ResolvedAccount{Name: "randall", Source: store.AccountSourcePanel}, nil, http.StatusOK, "deleted"},
		{"not found", nil, fmt.Errorf("account %q: %w", "randall", store.ErrAccountNotFound), http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newAccountsHandler(t, AccountOps{
				Delete: func(context.Context, string) (*store.ResolvedAccount, error) {
					return tc.resolved, tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminDeleteAccount(rec, accountRequest(http.MethodDelete, "/admin/accounts/randall", "randall", ""))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			env := decodeEnvelope(t, rec)
			if tc.wantField == "" {
				if env.Success {
					t.Fatal("expected error envelope")
				}
				return
			}
			var data map[string]any
			if err := json.Unmarshal(env.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data["name"] != "randall" || data[tc.wantField] != true {
				t.Fatalf("data = %v, want {name:randall, %s:true}", data, tc.wantField)
			}
		})
	}
}

// TestRestoreAccount 验证 restore 的映射：活号无墓可还 409、名不存在 404、
// 成功回单号视图。
func TestRestoreAccount(t *testing.T) {
	handler := newAccountsHandler(t, AccountOps{
		Restore: func(_ context.Context, name string) (*store.ResolvedAccount, error) {
			return &store.ResolvedAccount{Name: name, ConfigDeclared: true, Source: store.AccountSourceConfig}, nil
		},
	})
	rec := httptest.NewRecorder()
	handler.adminRestoreAccount(rec, accountRequest(http.MethodPost, "/admin/accounts/randall/restore", "randall", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	cases := []struct {
		opsErr   error
		wantCode int
		wantErr  string
	}{
		{fmt.Errorf("account %q: %w", "randall", store.ErrAccountNotTombstoned), http.StatusConflict, `account "randall" is not tombstoned`},
		{fmt.Errorf("account %q: %w", "randall", store.ErrAccountNotFound), http.StatusNotFound, `account "randall" not found`},
	}
	for _, tc := range cases {
		handler := newAccountsHandler(t, AccountOps{
			Restore: func(context.Context, string) (*store.ResolvedAccount, error) {
				return nil, tc.opsErr
			},
		})
		rec := httptest.NewRecorder()
		handler.adminRestoreAccount(rec, accountRequest(http.MethodPost, "/admin/accounts/randall/restore", "randall", ""))
		if rec.Code != tc.wantCode {
			t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
		}
		if env := decodeEnvelope(t, rec); env.Error != tc.wantErr {
			t.Fatalf("error = %q, want %q", env.Error, tc.wantErr)
		}
	}
}

// TestClearAccountCooldown 验证清冷却：有活 lane 200 cleared、无活 lane 404。
func TestClearAccountCooldown(t *testing.T) {
	handler := newAccountsHandler(t, AccountOps{
		ClearCooldown: func(string) bool { return true },
	})
	rec := httptest.NewRecorder()
	handler.adminClearAccountCooldown(rec, accountRequest(http.MethodPost, "/admin/accounts/randall/clear-cooldown", "randall", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec)
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["name"] != "randall" || data["cleared"] != true {
		t.Fatalf("data = %v", data)
	}

	handler = newAccountsHandler(t, AccountOps{
		ClearCooldown: func(string) bool { return false },
	})
	rec = httptest.NewRecorder()
	handler.adminClearAccountCooldown(rec, accountRequest(http.MethodPost, "/admin/accounts/randall/clear-cooldown", "randall", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error != `account "randall" has no live lane` {
		t.Fatalf("error = %q", env.Error)
	}
}

// TestRefreshAccountQuotaTokenErrors 验证配额即采的凭据解析失败路径：
// TokenOf 的任何失败（名不在生效集/tombstoned/凭据不可解）都归 404。
func TestRefreshAccountQuotaTokenErrors(t *testing.T) {
	cases := []struct {
		name     string
		opsErr   error
		wantCode int
		wantErr  string
	}{
		{"not found", fmt.Errorf("account %q: %w", "randall", store.ErrAccountNotFound), http.StatusNotFound, `account "randall" not found`},
		{"unresolvable", errors.New("credentials_file unreadable"), http.StatusNotFound, "credentials_file unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newAccountsHandler(t, AccountOps{
				TokenOf: func(context.Context, string) (string, error) {
					return "", tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminRefreshAccountQuota(rec, accountRequest(http.MethodPost, "/admin/accounts/randall/quota/refresh", "randall", ""))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if env := decodeEnvelope(t, rec); env.Error != tc.wantErr {
				t.Fatalf("error = %q, want %q", env.Error, tc.wantErr)
			}
		})
	}
}

// TestAccountNameParamRejected 验证 {name} path 参数非法时各端点 400，
// 不进 ops。
func TestAccountNameParamRejected(t *testing.T) {
	called := false
	guard := func() { called = true }
	handler := newAccountsHandler(t, AccountOps{
		Update:  func(context.Context, string, AccountPatch) (*store.ResolvedAccount, error) { guard(); return nil, nil },
		Delete:  func(context.Context, string) (*store.ResolvedAccount, error) { guard(); return nil, nil },
		Restore: func(context.Context, string) (*store.ResolvedAccount, error) { guard(); return nil, nil },
		ClearCooldown: func(string) bool {
			guard()
			return true
		},
		TokenOf: func(context.Context, string) (string, error) { guard(); return "", nil },
	})
	endpoints := []struct {
		method  string
		handler http.HandlerFunc
	}{
		{http.MethodPut, handler.adminUpdateAccount},
		{http.MethodDelete, handler.adminDeleteAccount},
		{http.MethodPost, handler.adminRestoreAccount},
		{http.MethodPost, handler.adminClearAccountCooldown},
		{http.MethodPost, handler.adminRefreshAccountQuota},
	}
	for _, ep := range endpoints {
		rec := httptest.NewRecorder()
		body := ""
		if ep.method == http.MethodPut {
			body = `{"disabled":true}`
		}
		ep.handler(rec, accountRequest(ep.method, "/admin/accounts/bad!name", "bad!name", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", ep.method, rec.Code)
		}
	}
	if called {
		t.Fatal("ops must not run on invalid name")
	}
}

// TestAccountOpsUnavailable 验证操作面未接线时六个写端点全部 503。
func TestAccountOpsUnavailable(t *testing.T) {
	handler, err := New("", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	endpoints := []struct {
		method  string
		handler http.HandlerFunc
		body    string
	}{
		{http.MethodPost, handler.adminCreateAccount, `{"name":"a","token":"t"}`},
		{http.MethodPut, handler.adminUpdateAccount, `{"disabled":true}`},
		{http.MethodDelete, handler.adminDeleteAccount, ""},
		{http.MethodPost, handler.adminRestoreAccount, ""},
		{http.MethodPost, handler.adminClearAccountCooldown, ""},
		{http.MethodPost, handler.adminRefreshAccountQuota, ""},
	}
	for _, ep := range endpoints {
		rec := httptest.NewRecorder()
		ep.handler(rec, accountRequest(ep.method, "/admin/accounts/a", "a", ep.body))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", ep.method, rec.Code)
		}
	}
}
