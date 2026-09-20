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

	"github.com/WncFht/devin2api/internal/accounts"
	"github.com/WncFht/devin2api/internal/store"
)

// accountEnvelope 是 ccLoad 信封的最小解码形状。
type accountEnvelope struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Error   string          `json:"error"`
}

// newWriteOpsHandler 装配写端点的最小 Handler：只挂操作面，不拉
// store/debug（写路径不读它们）。
func newWriteOpsHandler(ops accounts.AccountOps) *Handler {
	return &Handler{accountOps: &ops}
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
	var got accounts.AccountWrite
	handler := newWriteOpsHandler(accounts.AccountOps{
		Create: func(_ context.Context, in accounts.AccountWrite) (*store.ResolvedAccount, error) {
			got = in
			return &store.ResolvedAccount{Name: in.Name, Source: store.AccountSourcePanel}, nil
		},
	})
	rec := httptest.NewRecorder()
	req := accountRequest(http.MethodPost, "/admin/accounts", "",
		`{"name":"bravo","token":"sess-1","disabled":true}`)
	handler.adminCreateAccount(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.Name != "bravo" || got.Token != "sess-1" || !got.Disabled || got.CredentialsFile != "" {
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
	if data["name"] != "bravo" {
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
			opsErr:   fmt.Errorf("account %q: %w", "bravo", store.ErrAccountExists),
			wantCode: http.StatusConflict,
			wantErr:  `account "bravo" already exists`,
		},
		{
			name:     "validation passthrough",
			opsErr:   errors.New(`devin.accounts[2]: duplicate token with account "alpha"`),
			wantCode: http.StatusBadRequest,
			wantErr:  `devin.accounts[2]: duplicate token with account "alpha"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newWriteOpsHandler(accounts.AccountOps{
				Create: func(context.Context, accounts.AccountWrite) (*store.ResolvedAccount, error) {
					return nil, tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "",
				`{"name":"bravo","token":"sess-1"}`))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if env := decodeEnvelope(t, rec); env.Error != tc.wantErr {
				t.Fatalf("error = %q, want %q", env.Error, tc.wantErr)
			}
		})
	}
}

// TestCreateAccountNewFields 验证建号请求的新字段原样进 ops：
// credentials_content/priority/max_rpm/notes 直通；verify 缺席时
// CredentialOf 不被动。
func TestCreateAccountNewFields(t *testing.T) {
	var got accounts.AccountWrite
	handler := newWriteOpsHandler(accounts.AccountOps{
		Create: func(_ context.Context, in accounts.AccountWrite) (*store.ResolvedAccount, error) {
			got = in
			return &store.ResolvedAccount{Name: in.Name, Source: store.AccountSourcePanel}, nil
		},
		CredentialOf: func(accounts.AccountWrite) (string, error) {
			t.Fatal("CredentialOf must not run without verify")
			return "", nil
		},
	})
	rec := httptest.NewRecorder()
	handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "",
		`{"name":"a","credentials_content":"windsurf_api_key = \"k\"","priority":5,"max_rpm":30,"notes":"n1"}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.CredentialsContent == "" || got.Priority == nil || *got.Priority != 5 ||
		got.MaxRPM == nil || *got.MaxRPM != 30 || got.Notes == nil || *got.Notes != "n1" {
		t.Fatalf("ops input = %+v", got)
	}
}

// TestCreateAccountConflicts 验证管线前校验：credentials_file 与
// credentials_content 互斥、priority/max_rpm 负值，均 400 不进 ops。
func TestCreateAccountConflicts(t *testing.T) {
	called := false
	handler := newWriteOpsHandler(accounts.AccountOps{
		Create: func(context.Context, accounts.AccountWrite) (*store.ResolvedAccount, error) {
			called = true
			return nil, nil
		},
	})
	for _, body := range []string{
		`{"name":"a","credentials_file":"/x.toml","credentials_content":"k=1"}`,
		`{"name":"a","token":"t","priority":-1}`,
		`{"name":"a","token":"t","max_rpm":-5}`,
	} {
		rec := httptest.NewRecorder()
		handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
	if called {
		t.Fatal("ops.Create must not run on invalid input")
	}
}

// TestCreateAccountVerify 验证 verify 探测的管线前段：CredentialOf
// 失败原文透传 400 且不进 Create；CredentialOf 未接线同 400。探测
// 成功段要真实上游，由端到端覆盖。
func TestCreateAccountVerify(t *testing.T) {
	createCalled := false
	handler := newWriteOpsHandler(accounts.AccountOps{
		Create: func(context.Context, accounts.AccountWrite) (*store.ResolvedAccount, error) {
			createCalled = true
			return nil, nil
		},
		CredentialOf: func(accounts.AccountWrite) (string, error) {
			return "", errors.New("credentials_file unreadable")
		},
	})
	rec := httptest.NewRecorder()
	handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "",
		`{"name":"a","token":"t","verify":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error != "credentials_file unreadable" {
		t.Fatalf("error = %q", env.Error)
	}
	if createCalled {
		t.Fatal("Create must not run after failed verify")
	}

	handler = newWriteOpsHandler(accounts.AccountOps{
		Create: func(context.Context, accounts.AccountWrite) (*store.ResolvedAccount, error) {
			createCalled = true
			return nil, nil
		},
	})
	rec = httptest.NewRecorder()
	handler.adminCreateAccount(rec, accountRequest(http.MethodPost, "/admin/accounts", "",
		`{"name":"a","token":"t","verify":true}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("nil CredentialOf: status = %d, want 400", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error != "credential verification unavailable" {
		t.Fatalf("error = %q", env.Error)
	}
}

// TestCreateAccountBadName 验证非法名在管线前被 400 拦下，不进 ops。
func TestCreateAccountBadName(t *testing.T) {
	called := false
	handler := newWriteOpsHandler(accounts.AccountOps{
		Create: func(context.Context, accounts.AccountWrite) (*store.ResolvedAccount, error) {
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
	var got accounts.AccountPatch
	handler := newWriteOpsHandler(accounts.AccountOps{
		Update: func(_ context.Context, name string, patch accounts.AccountPatch) (*store.ResolvedAccount, error) {
			got = patch
			return &store.ResolvedAccount{Name: name, Source: store.AccountSourceConfig, ConfigDeclared: true}, nil
		},
	})
	rec := httptest.NewRecorder()
	handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/bravo", "bravo",
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
	handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/bravo", "bravo", `{}`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch status = %d, want 400", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error != "nothing to update" {
		t.Fatalf("error = %q", env.Error)
	}
}

// TestUpdateAccountNewFields 验证新指针字段的缺席/显式值语义：
// credentials_content 显式 "" 是清覆盖、priority=0 是真实覆盖、
// notes 显式 "" 清注解；file+content 同现与负值在管线前 400。
func TestUpdateAccountNewFields(t *testing.T) {
	var got accounts.AccountPatch
	handler := newWriteOpsHandler(accounts.AccountOps{
		Update: func(_ context.Context, _ string, patch accounts.AccountPatch) (*store.ResolvedAccount, error) {
			got = patch
			return &store.ResolvedAccount{Name: "a", Source: store.AccountSourcePanel}, nil
		},
	})
	rec := httptest.NewRecorder()
	handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/a", "a",
		`{"credentials_content":"","priority":0,"max_rpm":60,"notes":""}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if got.CredentialsContent == nil || *got.CredentialsContent != "" {
		t.Fatalf("patch.CredentialsContent = %v, want pointer to empty string", got.CredentialsContent)
	}
	if got.Priority == nil || *got.Priority != 0 || got.MaxRPM == nil || *got.MaxRPM != 60 ||
		got.Notes == nil || *got.Notes != "" {
		t.Fatalf("patch = %+v", got)
	}

	called := false
	handler = newWriteOpsHandler(accounts.AccountOps{
		Update: func(context.Context, string, accounts.AccountPatch) (*store.ResolvedAccount, error) {
			called = true
			return nil, nil
		},
	})
	for _, body := range []string{
		`{"credentials_file":"/x.toml","credentials_content":"k=1"}`,
		`{"priority":-1}`,
		`{"max_rpm":-5}`,
	} {
		rec = httptest.NewRecorder()
		handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/a", "a", body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
	if called {
		t.Fatal("ops.Update must not run on invalid input")
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
		{"not found", fmt.Errorf("account %q: %w", "bravo", store.ErrAccountNotFound), http.StatusNotFound, `account "bravo" not found`},
		{"tombstoned", fmt.Errorf("account %q: %w", "bravo", store.ErrAccountTombstoned), http.StatusConflict, `account "bravo" is tombstoned; restore first`},
		{"validation", errors.New(`devin.accounts[0]: one of token/credentials_file is required`), http.StatusBadRequest, `devin.accounts[0]: one of token/credentials_file is required`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newWriteOpsHandler(accounts.AccountOps{
				Update: func(context.Context, string, accounts.AccountPatch) (*store.ResolvedAccount, error) {
					return nil, tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminUpdateAccount(rec, accountRequest(http.MethodPut, "/admin/accounts/bravo", "bravo",
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
		{"config tombstoned", &store.ResolvedAccount{Name: "bravo", ConfigDeclared: true, Source: store.AccountSourceTombstoned}, nil, http.StatusOK, "tombstoned"},
		{"panel deleted", &store.ResolvedAccount{Name: "bravo", Source: store.AccountSourcePanel}, nil, http.StatusOK, "deleted"},
		{"not found", nil, fmt.Errorf("account %q: %w", "bravo", store.ErrAccountNotFound), http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newWriteOpsHandler(accounts.AccountOps{
				Delete: func(context.Context, string) (*store.ResolvedAccount, error) {
					return tc.resolved, tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminDeleteAccount(rec, accountRequest(http.MethodDelete, "/admin/accounts/bravo", "bravo", ""))
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
			if data["name"] != "bravo" || data[tc.wantField] != true {
				t.Fatalf("data = %v, want {name:bravo, %s:true}", data, tc.wantField)
			}
		})
	}
}

// TestRestoreAccount 验证 restore 的映射：活号无墓可还 409、名不存在 404、
// 成功回单号视图。
func TestRestoreAccount(t *testing.T) {
	handler := newWriteOpsHandler(accounts.AccountOps{
		Restore: func(_ context.Context, name string) (*store.ResolvedAccount, error) {
			return &store.ResolvedAccount{Name: name, ConfigDeclared: true, Source: store.AccountSourceConfig}, nil
		},
	})
	rec := httptest.NewRecorder()
	handler.adminRestoreAccount(rec, accountRequest(http.MethodPost, "/admin/accounts/bravo/restore", "bravo", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	cases := []struct {
		opsErr   error
		wantCode int
		wantErr  string
	}{
		{fmt.Errorf("account %q: %w", "bravo", store.ErrAccountNotTombstoned), http.StatusConflict, `account "bravo" is not tombstoned`},
		{fmt.Errorf("account %q: %w", "bravo", store.ErrAccountNotFound), http.StatusNotFound, `account "bravo" not found`},
	}
	for _, tc := range cases {
		handler := newWriteOpsHandler(accounts.AccountOps{
			Restore: func(context.Context, string) (*store.ResolvedAccount, error) {
				return nil, tc.opsErr
			},
		})
		rec := httptest.NewRecorder()
		handler.adminRestoreAccount(rec, accountRequest(http.MethodPost, "/admin/accounts/bravo/restore", "bravo", ""))
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
	handler := newWriteOpsHandler(accounts.AccountOps{
		ClearCooldown: func(string) bool { return true },
	})
	rec := httptest.NewRecorder()
	handler.adminClearAccountCooldown(rec, accountRequest(http.MethodPost, "/admin/accounts/bravo/clear-cooldown", "bravo", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec)
	var data map[string]any
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["name"] != "bravo" || data["cleared"] != true {
		t.Fatalf("data = %v", data)
	}

	handler = newWriteOpsHandler(accounts.AccountOps{
		ClearCooldown: func(string) bool { return false },
	})
	rec = httptest.NewRecorder()
	handler.adminClearAccountCooldown(rec, accountRequest(http.MethodPost, "/admin/accounts/bravo/clear-cooldown", "bravo", ""))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if env := decodeEnvelope(t, rec); env.Error != `account "bravo" has no live lane` {
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
		{"not found", fmt.Errorf("account %q: %w", "bravo", store.ErrAccountNotFound), http.StatusNotFound, `account "bravo" not found`},
		{"unresolvable", errors.New("credentials_file unreadable"), http.StatusNotFound, "credentials_file unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newWriteOpsHandler(accounts.AccountOps{
				TokenOf: func(context.Context, string) (string, error) {
					return "", tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminRefreshAccountQuota(rec, accountRequest(http.MethodPost, "/admin/accounts/bravo/quota/refresh", "bravo", ""))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if env := decodeEnvelope(t, rec); env.Error != tc.wantErr {
				t.Fatalf("error = %q, want %q", env.Error, tc.wantErr)
			}
		})
	}
}

// TestTestAccountTokenErrors 验证连通性探测的凭据解析失败路径：
// TokenOf 任何失败（名不在生效集/tombstoned/凭据不可解）都归 404。
// 探测成功段要真实上游，由端到端覆盖。
func TestTestAccountTokenErrors(t *testing.T) {
	cases := []struct {
		name     string
		opsErr   error
		wantCode int
		wantErr  string
	}{
		{"not found", fmt.Errorf("account %q: %w", "bravo", store.ErrAccountNotFound), http.StatusNotFound, `account "bravo" not found`},
		{"unresolvable", errors.New("credentials_file unreadable"), http.StatusNotFound, "credentials_file unreadable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := newWriteOpsHandler(accounts.AccountOps{
				TokenOf: func(context.Context, string) (string, error) {
					return "", tc.opsErr
				},
			})
			rec := httptest.NewRecorder()
			handler.adminTestAccount(rec, accountRequest(http.MethodPost, "/admin/accounts/bravo/test", "bravo", ""))
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
	handler := newWriteOpsHandler(accounts.AccountOps{
		Update: func(context.Context, string, accounts.AccountPatch) (*store.ResolvedAccount, error) {
			guard()
			return nil, nil
		},
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
		{http.MethodPost, handler.adminTestAccount},
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
	handler := &Handler{}
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
		{http.MethodPost, handler.adminTestAccount, ""},
	}
	for _, ep := range endpoints {
		rec := httptest.NewRecorder()
		ep.handler(rec, accountRequest(ep.method, "/admin/accounts/a", "a", ep.body))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s: status = %d, want 503", ep.method, rec.Code)
		}
	}
}

// ---- 三段式探测（mint/chat/seat）的 stub 上游与端到端覆盖 ----

// probeStub 按路径分流三段探测端点；各段可断言 wire 认证形态
// （chat=Basic tok-tok、mint=X-Api-Key、seat=Bearer）。
type probeStub struct {
	chat func(w http.ResponseWriter, r *http.Request)
	mint func(w http.ResponseWriter, r *http.Request)
	seat func(w http.ResponseWriter, r *http.Request)
}

func (s probeStub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var h func(http.ResponseWriter, *http.Request)
		switch r.URL.Path {
		case chatCapacityPath:
			h = s.chat
		case seatMintPath:
			h = s.mint
		case seatUserStatusPath:
			h = s.seat
		}
		if h == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

const probeSeatOK = `{"userStatus":{"name":"Yan","email":"y@x","teamsTier":"ExaCodeiumCommonPb_TeamsTier_TEAMS_TIER_DEVIN_TEAMS_V2","planStatus":{"planInfo":{"planName":"Teams"}}}}`
const probeSeatGated = `{"error":{"code":"permission_denied","message":"Your account is on an individual plan. Please upgrade to team plan for multi-user access."}}`

// newProbeHandler 装配指到 stub 上游的写端点 Handler：三段探测只走
// baseTransport，apiClient 可由 newPanelUpstream 空挂。
func newProbeHandler(t *testing.T, srv *httptest.Server, ops accounts.AccountOps) *Handler {
	t.Helper()
	h := newWriteOpsHandler(ops)
	up, err := newPanelUpstream(srv.URL, "", false, func() string { return "unused" })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.baseTransport.CloseIdleConnections() })
	h.upstreamPtr.Store(up)
	return h
}

// TestCreateAccountVerifyUpstream 验证 verify 三段式探测的真实 wire 行为：
// chat 面是硬门（拒=400 不建行），seat 面只做回填（individual plan 403
// 记 seat_gated 不拦），api_key-only 先 mint 再以铸出 token 过 chat。
func TestCreateAccountVerifyUpstream(t *testing.T) {
	create := func() accounts.AccountOps {
		return accounts.AccountOps{
			Create: func(_ context.Context, in accounts.AccountWrite) (*store.ResolvedAccount, error) {
				return &store.ResolvedAccount{Name: in.Name, Source: store.AccountSourcePanel}, nil
			},
			CredentialOf: func(in accounts.AccountWrite) (string, error) {
				return in.Token, nil
			},
		}
	}
	createReq := func(body string) (*httptest.ResponseRecorder, *http.Request) {
		return httptest.NewRecorder(), accountRequest(http.MethodPost, "/admin/accounts", "", body)
	}
	decodeData := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		env := decodeEnvelope(t, rec)
		if !env.Success {
			t.Fatalf("success = false (%s)", env.Error)
		}
		var data map[string]any
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data
	}

	t.Run("happy", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			chat: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Basic tok-good-tok-good" {
					t.Errorf("chat auth = %q, want Basic tok-good-tok-good", r.Header.Get("Authorization"))
				}
				writeJSON(w, 200, `{"hasCapacity":true}`)
			},
			seat: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer tok-good" {
					t.Errorf("seat auth = %q, want Bearer tok-good", r.Header.Get("Authorization"))
				}
				writeJSON(w, 200, probeSeatOK)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, create())
		rec, req := createReq(`{"name":"a","token":"tok-good","verify":true}`)
		h.adminCreateAccount(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		ver, _ := decodeData(t, rec)["verification"].(map[string]any)
		if ver["chat"] != true || ver["seat"] != true || ver["has_capacity"] != true {
			t.Fatalf("verification = %v", ver)
		}
		if ver["plan"] != "Teams" || ver["teams_tier"] != "DEVIN_TEAMS_V2" {
			t.Fatalf("verification plan/tier = %v/%v", ver["plan"], ver["teams_tier"])
		}
	})

	t.Run("seat gated still creates", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			chat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, `{"hasCapacity":true}`)
			},
			seat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 403, probeSeatGated)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, create())
		rec, req := createReq(`{"name":"a","token":"tok-indie","verify":true}`)
		h.adminCreateAccount(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		ver, _ := decodeData(t, rec)["verification"].(map[string]any)
		if ver["seat"] != false || ver["seat_gated"] != true || ver["chat"] != true {
			t.Fatalf("verification = %v", ver)
		}
	})

	t.Run("chat failure rejects", func(t *testing.T) {
		createCalled := false
		ops := create()
		ops.Create = func(context.Context, accounts.AccountWrite) (*store.ResolvedAccount, error) {
			createCalled = true
			return nil, nil
		}
		srv := httptest.NewServer(probeStub{
			chat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 403, `{"error":{"code":"unauthenticated","message":"Invalid service key"}}`)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, ops)
		rec, req := createReq(`{"name":"a","token":"tok-dead","verify":true}`)
		h.adminCreateAccount(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		env := decodeEnvelope(t, rec)
		if !strings.Contains(env.Error, "CheckChatCapacity HTTP 403") {
			t.Fatalf("error = %q", env.Error)
		}
		if createCalled {
			t.Fatal("Create must not run after chat-gate failure")
		}
	})

	t.Run("api_key mints then serves", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			mint: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "cog_key1" {
					t.Errorf("mint X-Api-Key = %q", r.Header.Get("X-Api-Key"))
				}
				writeJSON(w, 200, `{"sessionToken":"sess-minted"}`)
			},
			chat: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Basic sess-minted-sess-minted" {
					t.Errorf("chat auth = %q, want minted token", r.Header.Get("Authorization"))
				}
				writeJSON(w, 200, `{"hasCapacity":true}`)
			},
			seat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, probeSeatOK)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, create())
		rec, req := createReq(`{"name":"a","api_key":"cog_key1","verify":true}`)
		h.adminCreateAccount(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		ver, _ := decodeData(t, rec)["verification"].(map[string]any)
		if ver["mint"] != true || ver["chat"] != true || ver["seat"] != true {
			t.Fatalf("verification = %v", ver)
		}
	})

	t.Run("api_key mint failure rejects", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			mint: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 401, `{"error":{"code":"unauthenticated","message":"bad key cog_key1"}}`)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, create())
		rec, req := createReq(`{"name":"a","api_key":"cog_key1","verify":true}`)
		h.adminCreateAccount(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		env := decodeEnvelope(t, rec)
		if !strings.Contains(env.Error, "api_key mint:") {
			t.Fatalf("error = %q", env.Error)
		}
		// durable key 不得随错误体回显（adapter 同款防回显）。
		if strings.Contains(env.Error, "cog_key1") {
			t.Fatalf("error echoes api key: %q", env.Error)
		}
	})

	t.Run("token plus api_key soft mint flag", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			mint: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 401, `{"error":{"code":"unauthenticated","message":"bad key"}}`)
			},
			chat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, `{"hasCapacity":false}`)
			},
			seat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, probeSeatOK)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, create())
		rec, req := createReq(`{"name":"a","token":"tok-good","api_key":"cog_key1","verify":true}`)
		h.adminCreateAccount(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
		ver, _ := decodeData(t, rec)["verification"].(map[string]any)
		if ver["mint"] != false || ver["mint_error"] == nil || ver["chat"] != true || ver["has_capacity"] != false {
			t.Fatalf("verification = %v", ver)
		}
	})
}

// TestTestAccountProbes 验证 /test 的两段式口径：ok 判据是 chat 面；
// api_key-only 号先 mint 再打 chat（mint 失败即 ok:false）；seat 面
// 只做回填——403 individual plan 记 seat_gated 不翻 ok。
func TestTestAccountProbes(t *testing.T) {
	opsWith := func(resolved *store.ResolvedAccount) accounts.AccountOps {
		return accounts.AccountOps{
			TokenOf: func(context.Context, string) (string, error) {
				if resolved.APIKey != "" {
					return resolved.APIKey, nil
				}
				return resolved.Token, nil
			},
			Effective: func(context.Context) ([]store.ResolvedAccount, error) {
				return []store.ResolvedAccount{*resolved}, nil
			},
			ClearCooldown: func(string) bool { return true },
		}
	}
	testReq := func() (*httptest.ResponseRecorder, *http.Request) {
		return httptest.NewRecorder(), accountRequest(http.MethodPost, "/admin/accounts/a/test", "a", "")
	}
	decodeData := func(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		env := decodeEnvelope(t, rec)
		var data map[string]any
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatalf("decode data: %v (%s)", err, rec.Body.String())
		}
		return data
	}

	t.Run("token ok", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			chat: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Basic tok-live-tok-live" {
					t.Errorf("chat auth = %q", r.Header.Get("Authorization"))
				}
				writeJSON(w, 200, `{"hasCapacity":true}`)
			},
			seat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, probeSeatOK)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, opsWith(&store.ResolvedAccount{Name: "a", Token: "tok-live"}))
		rec, req := testReq()
		h.adminTestAccount(rec, req)
		data := decodeData(t, rec)
		if data["ok"] != true || data["chat"] != true || data["seat"] != true || data["has_capacity"] != true {
			t.Fatalf("data = %v", data)
		}
	})

	t.Run("api_key-only mints first", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			mint: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Api-Key") != "cog_key1" {
					t.Errorf("mint X-Api-Key = %q", r.Header.Get("X-Api-Key"))
				}
				writeJSON(w, 200, `{"sessionToken":"sess-minted"}`)
			},
			chat: func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Basic sess-minted-sess-minted" {
					t.Errorf("chat auth = %q, want minted token", r.Header.Get("Authorization"))
				}
				writeJSON(w, 200, `{"hasCapacity":true}`)
			},
			seat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, probeSeatOK)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, opsWith(&store.ResolvedAccount{Name: "a", APIKey: "cog_key1"}))
		rec, req := testReq()
		h.adminTestAccount(rec, req)
		data := decodeData(t, rec)
		if data["ok"] != true || data["mint"] != true || data["chat"] != true {
			t.Fatalf("data = %v", data)
		}
	})

	t.Run("api_key-only mint failure", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			mint: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 401, `{"error":{"code":"unauthenticated"}}`)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, opsWith(&store.ResolvedAccount{Name: "a", APIKey: "cog_key1"}))
		rec, req := testReq()
		h.adminTestAccount(rec, req)
		data := decodeData(t, rec)
		if data["ok"] != false || data["mint"] != false {
			t.Fatalf("data = %v", data)
		}
		if msg, _ := data["error"].(string); !strings.Contains(msg, "api_key mint:") {
			t.Fatalf("error = %v", data["error"])
		}
	})

	t.Run("seat gated keeps ok", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			chat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 200, `{"hasCapacity":true}`)
			},
			seat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 403, probeSeatGated)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, opsWith(&store.ResolvedAccount{Name: "a", Token: "tok-indie"}))
		rec, req := testReq()
		h.adminTestAccount(rec, req)
		data := decodeData(t, rec)
		if data["ok"] != true || data["seat"] != false || data["seat_gated"] != true {
			t.Fatalf("data = %v", data)
		}
	})

	t.Run("chat failure flips ok", func(t *testing.T) {
		srv := httptest.NewServer(probeStub{
			chat: func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, 403, `{"error":{"code":"unauthenticated","message":"Invalid service key"}}`)
			},
		}.handler())
		defer srv.Close()
		h := newProbeHandler(t, srv, opsWith(&store.ResolvedAccount{Name: "a", Token: "tok-dead"}))
		rec, req := testReq()
		h.adminTestAccount(rec, req)
		data := decodeData(t, rec)
		if data["ok"] != false {
			t.Fatalf("data = %v", data)
		}
		if msg, _ := data["error"].(string); !strings.Contains(msg, "CheckChatCapacity HTTP 403") {
			t.Fatalf("error = %v", data["error"])
		}
	})
}
