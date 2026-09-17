// 本文件验证配额历史的读取与燃烧速率预测。
package ccpanel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

func f64(v float64) *float64 { return &v }

// TestForecastBurnRate 验证线性差分得到的燃烧速率与耗尽时刻。
func TestForecastBurnRate(t *testing.T) {
	now := time.Now().Unix()
	points := []*store.QuotaSample{
		{At: now - 7200, DailyRemaining: f64(80), DailyResetAt: now + 100000},
		{At: now - 3600, DailyRemaining: f64(70)},
		{At: now, DailyRemaining: f64(60), DailyResetAt: now + 100000},
	}
	got := forecast(points, 24*time.Hour,
		func(p *store.QuotaSample) float64 { return floatOr0(p.DailyRemaining) },
		func(p *store.QuotaSample) int64 { return p.DailyResetAt })
	if got == nil {
		t.Fatal("forecast = nil")
	}
	if rate := got["burn_per_hour"].(float64); rate != 10 {
		t.Fatalf("burn_per_hour = %v, want 10", rate)
	}
	// 60% / 10%/h = 6h 后耗尽。
	if left := got["hours_left"].(float64); left != 6 {
		t.Fatalf("hours_left = %v, want 6", left)
	}
	if got["exhausted_at"].(int64) != now+21600 {
		t.Fatalf("exhausted_at = %v", got["exhausted_at"])
	}
	if got["reset_at"].(int64) != now+100000 {
		t.Fatalf("reset_at = %v", got["reset_at"])
	}
}

// TestForecastSurvivesUntilReset 验证外推耗尽越过重置点时报 survives_until_reset。
func TestForecastSurvivesUntilReset(t *testing.T) {
	now := time.Now().Unix()
	points := []*store.QuotaSample{
		{At: now - 3600, DailyRemaining: f64(91), DailyResetAt: now + 36000},
		{At: now, DailyRemaining: f64(90), DailyResetAt: now + 36000},
	}
	got := forecast(points, 24*time.Hour,
		func(p *store.QuotaSample) float64 { return floatOr0(p.DailyRemaining) },
		func(p *store.QuotaSample) int64 { return p.DailyResetAt })
	if got == nil {
		t.Fatal("forecast = nil")
	}
	// 90% 按 1%/h 要 90h 才烧完，但 10h 后就重置——不可能发生的耗尽时刻
	// 不应出现在响应里。
	if _, ok := got["exhausted_at"]; ok {
		t.Fatalf("exhausted_at = %v, want absent (quota resets first)", got["exhausted_at"])
	}
	if got["survives_until_reset"] != true {
		t.Fatalf("survives_until_reset = %v, want true", got["survives_until_reset"])
	}
	if left := got["hours_left"].(float64); left != 90 {
		t.Fatalf("hours_left = %v, want 90", left)
	}
}

// TestForecastTooFewPoints 验证样本不足时不产生预测。
func TestForecastTooFewPoints(t *testing.T) {
	if got := forecast(nil, time.Hour, func(p *store.QuotaSample) float64 { return 0 }, func(p *store.QuotaSample) int64 { return 0 }); got != nil {
		t.Fatalf("forecast = %+v, want nil", got)
	}
	if got := forecast([]*store.QuotaSample{{At: 1}}, time.Hour, func(p *store.QuotaSample) float64 { return 0 }, func(p *store.QuotaSample) int64 { return 0 }); got != nil {
		t.Fatalf("forecast = %+v, want nil", got)
	}
}

// TestQuotaStoreRoundTrip 验证采样入库与历史读取的往返，含
// 「上游没报」字段的 NULL↔nil 保持。
func TestQuotaStoreRoundTrip(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for i, remaining := range []float64{90, 85, 80} {
		point := &store.QuotaSample{At: 1700000000 + int64(i*600), DailyRemaining: f64(remaining)}
		if err := st.InsertQuotaSample(context.Background(), point); err != nil {
			t.Fatal(err)
		}
	}

	h := &Handler{store: st}
	points := h.readQuotaHistory(context.Background())
	if len(points) != 3 || floatOr0(points[2].DailyRemaining) != 80 {
		t.Fatalf("points = %+v", points)
	}
	// 未上报的字段保持 nil 往返——指针字段就是为了区分「没报」与「报到 0%」。
	if points[0].WeeklyRemaining != nil {
		t.Fatalf("WeeklyRemaining = %v, want nil for unreported field", *points[0].WeeklyRemaining)
	}
	got := forecast(points, 24*time.Hour,
		func(p *store.QuotaSample) float64 { return floatOr0(p.DailyRemaining) },
		func(p *store.QuotaSample) int64 { return p.DailyResetAt })
	if got == nil {
		t.Fatal("forecast = nil")
	}
	// 90→80 跨 1200s（1/3 小时）= 30 %/h。
	if rate := got["burn_per_hour"].(float64); fmt.Sprintf("%.2f", rate) != "30.00" {
		t.Fatalf("burn_per_hour = %v", rate)
	}
}

// TestQuotaSampleGraceAndTopUpFields 验证宽限/加额字段的快照形态：
// grace_period_end 以 unix 秒落库，缺省字段经 omitempty 不进 JSON。
func TestQuotaSampleGraceAndTopUpFields(t *testing.T) {
	end := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	point := &store.QuotaSample{
		At:                        end.Unix() - 3600,
		GracePeriodStatus:         "ACTIVE",
		GracePeriodEnd:            rfc3339Unix(end.Format(time.RFC3339)),
		WasReducedByOrphanedUsage: true,
		TopUpEnabled:              true,
		TopUpTransactionStatus:    "SUCCEEDED",
	}
	data, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	var back store.QuotaSample
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.GracePeriodStatus != "ACTIVE" || back.GracePeriodEnd != end.Unix() ||
		!back.WasReducedByOrphanedUsage || !back.TopUpEnabled || back.TopUpTransactionStatus != "SUCCEEDED" {
		t.Fatalf("round trip = %+v", back)
	}
	// 空值不序列化：宽限字段在大多数快照里缺席，不能让 0/false 刷存在感。
	var bare store.QuotaSample
	if err := json.Unmarshal([]byte(`{"at":1}`), &bare); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(bare)
	for _, key := range []string{"grace_period_status", "grace_period_end", "was_reduced_by_orphaned_usage", "top_up_enabled", "top_up_transaction_status"} {
		if strings.Contains(string(out), key) {
			t.Fatalf("empty %s leaked into %s", key, out)
		}
	}
	// 畸形 RFC3339 与缺席都记 0。
	if rfc3339Unix("not-a-time") != 0 || rfc3339Unix(nil) != 0 {
		t.Fatal("rfc3339Unix should map bad input to 0")
	}
}

// TestQuotaAccountsStates 验证采样清单的三态处置：未接线回退单号
// 匿名采样；已接线但空池整轮跳过（不触碰 tokenFunc——它下面连着
// firstLane，空池裸取下标会 panic，刻意不装它以放大误用）；有号
// 按名序输出。
func TestQuotaAccountsStates(t *testing.T) {
	// 未接线：回退 {token: tokenFunc()} 匿名行。
	h := &Handler{tokenFunc: func() string { return "tok-single" }}
	got := h.quotaAccounts()
	if len(got) != 1 || got[0].name != "" || got[0].token != "tok-single" {
		t.Fatalf("unwired fallback = %+v", got)
	}

	// 已接线但空池：空清单整轮跳过。
	h = &Handler{poolTokenFuncs: func() map[string]func() string { return map[string]func() string{} }}
	if got := h.quotaAccounts(); len(got) != 0 {
		t.Fatalf("empty pool = %+v, want empty", got)
	}

	// 有号：按名序输出，token 取各 lane 当前凭据。
	h = &Handler{poolTokenFuncs: func() map[string]func() string {
		return map[string]func() string{
			"randall": func() string { return "tok-r" },
			"yanjian": func() string { return "tok-y" },
		}
	}}
	got = h.quotaAccounts()
	if len(got) != 2 ||
		got[0].name != "randall" || got[0].token != "tok-r" ||
		got[1].name != "yanjian" || got[1].token != "tok-y" {
		t.Fatalf("pool accounts = %+v", got)
	}
}

// newQuotaTestHandler 搭一个指向 httptest 假上游、带真 store 的
// Handler，供刷新/采样链路测试。
func newQuotaTestHandler(t *testing.T, srv *httptest.Server) *Handler {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &Handler{store: st}
	up, err := newPanelUpstream(srv.URL, "", false, func() string { return "unused" })
	if err != nil {
		t.Fatal(err)
	}
	h.upstreamPtr.Store(up)
	return h
}

// TestRefreshAccountQuota 验证手动刷新与定时采样共用内核：返回
// {account,user,plan} 回显，同时更新 quotaUsers 投影并落一行
// quota_samples（字段名与采样写库同口径）。
func TestRefreshAccountQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != seatUserStatusPath {
			t.Errorf("path = %s, want %s", r.URL.Path, seatUserStatusPath)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer tok-r" {
			t.Errorf("Authorization = %q, want Bearer tok-r", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{
			"name":"Randall","email":"r@x.com","pro":true,
			"teamsTier":"ExaCodeiumCommonPb_TeamsTier_TEAMS_TIER_PRO",
			"planStatus":{
				"dailyQuotaRemainingPercent":62.5,
				"weeklyQuotaRemainingPercent":80,
				"availablePromptCredits":500,
				"planInfo":{"planName":"pro","billingStrategy":"ExaCodeiumCommonPb_BillingStrategy_BILLING_STRATEGY_MONTHLY"}
			}}}`))
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)

	data, err := h.refreshAccountQuota(context.Background(), "randall", "tok-r")
	if err != nil {
		t.Fatal(err)
	}
	if data["account"] != "randall" {
		t.Fatalf("account = %v", data["account"])
	}
	user, ok := data["user"].(map[string]any)
	if !ok || user["email"] != "r@x.com" || user["plan_name"] != "pro" {
		t.Fatalf("user = %+v", data["user"])
	}
	plan, ok := data["plan"].(map[string]any)
	if !ok || floatAny(plan["daily_quota_remaining"]) != 62.5 {
		t.Fatalf("plan = %+v", data["plan"])
	}

	// 身份投影按名更新（与定时采样同一路径）。
	h.quotaUserMu.Lock()
	proj := h.quotaUsers["randall"]
	h.quotaUserMu.Unlock()
	if proj["email"] != "r@x.com" || proj["teams_tier"] != "PRO" || proj["billing_strategy"] != "MONTHLY" {
		t.Fatalf("quotaUsers[randall] = %+v", proj)
	}

	// 样本行落库，account 记真名。
	rows, err := h.store.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Account != "randall" || floatOr0(rows[0].DailyRemaining) != 62.5 {
		t.Fatalf("samples = %+v", rows)
	}
}

// TestRefreshAccountQuotaNoPlan 验证上游 200 但缺 planStatus：身份
// 投影照常更新，plan 回 nil 不写点、不报错。
func TestRefreshAccountQuotaNoPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{"name":"Randall","email":"r@x.com"}}`))
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)

	data, err := h.refreshAccountQuota(context.Background(), "randall", "tok-r")
	if err != nil {
		t.Fatal(err)
	}
	// data["plan"] 是装着 nil map 的 interface，断言出来再判 nil。
	if p, _ := data["plan"].(map[string]any); p != nil {
		t.Fatalf("plan = %v, want nil", p)
	}
	if user := data["user"].(map[string]any); user["email"] != "r@x.com" {
		t.Fatalf("user = %+v", data["user"])
	}
	rows, err := h.store.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("samples = %+v, want none", rows)
	}
}

// TestRefreshAccountQuotaUpstreamError 验证上游失败透传 error
// （handler 映 502），不落样本行。
func TestRefreshAccountQuotaUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)

	if _, err := h.refreshAccountQuota(context.Background(), "randall", "tok-r"); err == nil {
		t.Fatal("want error for upstream 500")
	}
	rows, err := h.store.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("samples = %+v, want none", rows)
	}
}
