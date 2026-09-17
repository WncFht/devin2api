// 本文件验证 /admin/accounts 聚合视图的契约形状：source 三态、
// has_override、token_sha 口径、lane/gate/warm 的 null 语义、inflight
// 分桶、quota 子集裁剪，以及 cli-credentials 发现链探针。
package ccpanel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/store"
)

// accountsResponse 是 GET /admin/accounts 的测试信封：count 在 data 外。
type accountsResponse struct {
	Success bool            `json:"success"`
	Count   int             `json:"count"`
	Error   string          `json:"error"`
	Data    json.RawMessage `json:"data"`
}

// decodeAccounts 断言 HTTP 状态与信封 success，返回 accounts 数组。
func decodeAccounts(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int) []map[string]any {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, wantStatus, recorder.Body.String())
	}
	var env accountsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if wantStatus != http.StatusOK {
		return nil
	}
	if !env.Success {
		t.Fatalf("success=false: %s", recorder.Body.String())
	}
	var data struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data.Accounts
}

// newAccountsHandler 装配聚合视图的最小 Handler：fake ops 供身份集，
// 真实 debuglog.Manager + store 覆盖 inflight 分桶与 quota 子集。
func newAccountsHandler(t *testing.T, accounts []store.ResolvedAccount) (*Handler, func()) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	mgr := debuglog.NewManager(t.TempDir(), debuglog.RetentionPolicy{}, st)
	h := &Handler{
		store: st,
		debug: mgr,
		accountOps: &AccountOps{
			Effective: func(context.Context) ([]store.ResolvedAccount, error) {
				return accounts, nil
			},
		},
	}
	return h, func() {
		mgr.Close()
		_ = st.Close()
	}
}

func TestAdminAccountsAggregate(t *testing.T) {
	accounts := []store.ResolvedAccount{
		{
			Name: "yanjian", Source: store.AccountSourceConfig,
			ConfigDeclared: true, HasRow: true,
			Token: "sess-yanjian", CreatedAt: 1758000000000, UpdatedAt: 1758100000000,
		},
		{
			Name: "randall", Source: store.AccountSourceTombstoned,
			ConfigDeclared: true, HasRow: true,
			CredentialsFile: "/Users/x/Library/Application Support/windsurf/credentials.toml",
			CreatedAt:       1758100000000, UpdatedAt: 1758200000000,
		},
		{
			Name: "panel-acc", Source: store.AccountSourcePanel, HasRow: true,
			Token: "sess-panel", Disabled: true,
			CreatedAt: 1758000001000, UpdatedAt: 1758000002000,
		},
	}
	h, cleanup := newAccountsHandler(t, accounts)
	defer cleanup()

	// yanjian 有活 lane：三件套快照 + 一条在途请求 + 配额样本与身份。
	h.SetAccountLaneStates(func() map[string]devin.LaneState {
		return map[string]devin.LaneState{"yanjian": {Healthy: true}}
	})
	h.SetAccountGateStats(func() map[string]devin.GateStats {
		return map[string]devin.GateStats{"yanjian": {WindowQuota: 60, WindowUsed: 3, Sendable: true}}
	})
	h.SetAccountWarmStats(func() map[string]devin.WarmStats {
		return map[string]devin.WarmStats{"yanjian": {Enabled: true, Entries: 12, PingHits: 3, PingMisses: 1}}
	})
	if rec := h.debug.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/chat/completions"}); rec != nil {
		rec.SetUpstreamAccount("yanjian")
	}
	now := time.Now().Unix()
	for i, remaining := range []float64{90, 80, 70} {
		if err := h.store.InsertQuotaSample(context.Background(), &store.QuotaSample{
			At: now - int64(1200*(2-i)), Account: "yanjian",
			DailyRemaining: f64(remaining), DailyResetAt: now + 80000,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// randall 只有一个样本：forecast 落 null，但 accounts 条目存在，
	// user 身份键仍能进视图（契约示例形状）。
	if err := h.store.InsertQuotaSample(context.Background(), &store.QuotaSample{
		At: now, Account: "randall", DailyRemaining: f64(50),
	}); err != nil {
		t.Fatal(err)
	}
	h.quotaUsers = map[string]map[string]any{
		"yanjian": {"email": "a@b.c", "plan_name": "pro"},
		"randall": {"email": "x@y.z"},
	}

	recorder := httptest.NewRecorder()
	h.adminAccounts(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	got := decodeAccounts(t, recorder, http.StatusOK)
	if len(got) != 3 {
		t.Fatalf("len(accounts) = %d, want 3", len(got))
	}

	yj, rd, pa := got[0], got[1], got[2]
	// 排序 = Effective 入序（MergeAccounts 已保证），不重整。
	if yj["name"] != "yanjian" || rd["name"] != "randall" || pa["name"] != "panel-acc" {
		t.Fatalf("order = %v,%v,%v", yj["name"], rd["name"], pa["name"])
	}

	if yj["source"] != "config" || yj["config_declared"] != true || yj["has_override"] != true {
		t.Fatalf("yanjian identity = %v %v %v", yj["source"], yj["config_declared"], yj["has_override"])
	}
	if yj["credential"] != "literal" || yj["credentials_file"] != "" || yj["disabled"] != false {
		t.Fatalf("yanjian credential = %v file=%v disabled=%v", yj["credential"], yj["credentials_file"], yj["disabled"])
	}
	sum := sha256.Sum256([]byte("sess-yanjian"))
	if yj["token_sha"] != fmt.Sprintf("sha256:%x", sum[:6]) {
		t.Fatalf("token_sha = %v", yj["token_sha"])
	}
	lane, _ := yj["lane"].(map[string]any)
	if lane["healthy"] != true {
		t.Fatalf("yanjian lane = %v", yj["lane"])
	}
	gate, _ := yj["gate"].(map[string]any)
	if gate["window_quota"].(float64) != 60 || gate["sendable"] != true {
		t.Fatalf("yanjian gate = %v", yj["gate"])
	}
	warm, _ := yj["warm"].(map[string]any)
	if warm["entries"].(float64) != 12 || warm["ping_hit_rate"].(float64) != 75 {
		t.Fatalf("yanjian warm = %v", yj["warm"])
	}
	if yj["inflight"].(float64) != 1 {
		t.Fatalf("yanjian inflight = %v", yj["inflight"])
	}
	quota, _ := yj["quota"].(map[string]any)
	daily, _ := quota["daily"].(map[string]any)
	if daily["remaining"].(float64) != 70 || daily["burn_per_hour"].(float64) != 30 {
		t.Fatalf("yanjian quota.daily = %v", quota["daily"])
	}
	if _, ok := quota["points"]; ok {
		t.Fatal("quota must not carry points")
	}
	user, _ := quota["user"].(map[string]any)
	if user["email"] != "a@b.c" {
		t.Fatalf("yanjian quota.user = %v", quota["user"])
	}
	if v, ok := yj["usage"]; !ok || v != nil {
		t.Fatalf("usage = %v ok=%v, want explicit null", v, ok)
	}
	if yj["created_at"].(float64) != 1758000000000 || yj["updated_at"].(float64) != 1758100000000 {
		t.Fatalf("yanjian times = %v %v", yj["created_at"], yj["updated_at"])
	}

	// tombstoned：身份与凭据字段仍按生效值投影，lane/gate/warm 全 null，
	// quota 单样本 → daily/weekly null 但 user 在。
	if rd["source"] != "tombstoned" || rd["has_override"] != false {
		t.Fatalf("randall source = %v has_override=%v", rd["source"], rd["has_override"])
	}
	if rd["credential"] != "credentials_file" || rd["credentials_file"] == "" {
		t.Fatalf("randall credential = %v file=%v", rd["credential"], rd["credentials_file"])
	}
	if rd["token_sha"] != "" {
		t.Fatalf("randall token_sha = %v, want empty", rd["token_sha"])
	}
	for _, key := range []string{"lane", "gate", "warm"} {
		if v, ok := rd[key]; !ok || v != nil {
			t.Fatalf("randall %s = %v, want explicit null", key, v)
		}
	}
	if rd["inflight"].(float64) != 0 {
		t.Fatalf("randall inflight = %v", rd["inflight"])
	}
	rq, _ := rd["quota"].(map[string]any)
	if v, ok := rq["daily"]; !ok || v != nil {
		t.Fatalf("randall daily = %v, want explicit null", v)
	}
	if _, ok := rq["user"]; !ok {
		t.Fatal("randall quota.user missing")
	}

	// panel：config_declared=false、has_override=false；disabled 号无快照。
	if pa["source"] != "panel" || pa["config_declared"] != false || pa["has_override"] != false {
		t.Fatalf("panel identity = %v %v %v", pa["source"], pa["config_declared"], pa["has_override"])
	}
	if pa["disabled"] != true || pa["lane"] != nil || pa["gate"] != nil || pa["warm"] != nil {
		t.Fatalf("panel disabled/lane = %v %v %v %v", pa["disabled"], pa["lane"], pa["gate"], pa["warm"])
	}
	if len(pa["token_sha"].(string)) != len("sha256:")+12 {
		t.Fatalf("panel token_sha = %v", pa["token_sha"])
	}
}

func TestAdminAccountsEmpty(t *testing.T) {
	h, cleanup := newAccountsHandler(t, nil)
	defer cleanup()
	recorder := httptest.NewRecorder()
	h.adminAccounts(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	var env accountsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Count != 0 {
		t.Fatalf("count = %d, want 0", env.Count)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if string(data["accounts"]) != "[]" {
		t.Fatalf("accounts = %s, want []", data["accounts"])
	}
}

func TestAdminAccountsOpsError(t *testing.T) {
	h, cleanup := newAccountsHandler(t, nil)
	defer cleanup()
	h.accountOps.Effective = func(context.Context) ([]store.ResolvedAccount, error) {
		return nil, fmt.Errorf("db gone")
	}
	recorder := httptest.NewRecorder()
	h.adminAccounts(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", recorder.Code)
	}
	var env accountsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Success || env.Error != "db gone" {
		t.Fatalf("envelope = %+v", env)
	}
}

func TestAdminAccountsOpsUnavailable(t *testing.T) {
	h := &Handler{}
	recorder := httptest.NewRecorder()
	h.adminAccounts(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
}

// setDataHome 把 CLI 凭据发现要读的数据目录指到 dir：unix 认
// XDG_DATA_HOME，windows 认 APPDATA/LOCALAPPDATA（见 DevinCredentialsPaths）。
func setDataHome(t *testing.T, dir string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Setenv("APPDATA", dir)
		t.Setenv("LOCALAPPDATA", dir)
	} else {
		t.Setenv("XDG_DATA_HOME", dir)
	}
}

func TestAdminCLICredentials(t *testing.T) {
	t.Run("parsable", func(t *testing.T) {
		dir := t.TempDir()
		setDataHome(t, dir)
		path := filepath.Join(dir, "devin", "credentials.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("windsurf_api_key = \"sess-cli\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		(&Handler{}).adminCLICredentials(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/cli-credentials", nil))
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Data["available"] != true || env.Data["path"] != path ||
			env.Data["parsable"] != true || env.Data["suggested_name"] != "default-cli" {
			t.Fatalf("data = %v", env.Data)
		}
	})
	t.Run("unparsable", func(t *testing.T) {
		dir := t.TempDir()
		setDataHome(t, dir)
		path := filepath.Join(dir, "devin", "credentials.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("other = 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		recorder := httptest.NewRecorder()
		(&Handler{}).adminCLICredentials(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/cli-credentials", nil))
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Data["available"] != true || env.Data["parsable"] != false {
			t.Fatalf("data = %v", env.Data)
		}
	})
	t.Run("absent", func(t *testing.T) {
		setDataHome(t, t.TempDir())
		recorder := httptest.NewRecorder()
		(&Handler{}).adminCLICredentials(recorder, httptest.NewRequest(http.MethodGet, "/admin/accounts/cli-credentials", nil))
		var env struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Data["available"] != false {
			t.Fatalf("data = %v", env.Data)
		}
		if _, ok := env.Data["path"]; ok {
			t.Fatalf("path must be absent when unavailable: %v", env.Data)
		}
	})
}
