// 本文件验证配额历史的读取与燃烧速率预测。
package ccpanel

import (
	"context"
	"database/sql"
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

// TestQuotaReportStaleSeries 验证冻结序列不再喂 forecast：尾点龄期
// 超过 max(3×采样周期, 1h) 的序列（移出号池的号、历史空串→default
// 遗留桶）打 stale 标记、daily/weekly 落空但曲线保留；同报告内的
// 活跃序列不受影响，顶层镜像跟随最新鲜的活跃序列。
func TestQuotaReportStaleSeries(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h := &Handler{store: st}
	h.quotaInterval = 5 * time.Minute // bound = max(15min, 1h) = 1h
	ctx := context.Background()
	now := time.Now().Unix()
	// 僵尸桶：尾点停在两天前（无 account 的历史行在报告侧折叠进
	// default——与线上 1,483 行 ''+'default' 遗留同形态）。
	for i, rem := range []float64{95, 93, 91} {
		point := &store.QuotaSample{At: now - 2*86400 + int64(i*300), DailyRemaining: f64(rem), DailyResetAt: now - 86400}
		if err := st.InsertQuotaSample(ctx, point); err != nil {
			t.Fatal(err)
		}
	}
	// 活跃 lane：尾点就是现在。
	for i, rem := range []float64{80, 70, 60} {
		point := &store.QuotaSample{At: now - int64(2-i)*3600, Account: "randall", DailyRemaining: f64(rem), DailyResetAt: now + 100000}
		if err := st.InsertQuotaSample(ctx, point); err != nil {
			t.Fatal(err)
		}
	}
	report := h.QuotaReport(ctx)
	accounts, ok := report["accounts"].(map[string]any)
	if !ok {
		t.Fatalf("accounts = %T", report["accounts"])
	}
	zombie, ok := accounts["default"].(map[string]any)
	if !ok {
		t.Fatalf("default report = %T", accounts["default"])
	}
	if zombie["stale"] != true {
		t.Fatalf("stale = %v, want true", zombie["stale"])
	}
	if zombie["daily"] != nil || zombie["weekly"] != nil {
		t.Fatalf("stale series forecast = %v/%v, want nil", zombie["daily"], zombie["weekly"])
	}
	if pts, ok := zombie["points"].([]*store.QuotaSample); !ok || len(pts) != 3 {
		t.Fatalf("points = %v, want 3 samples kept", zombie["points"])
	}
	live, ok := accounts["randall"].(map[string]any)
	if !ok {
		t.Fatalf("randall report = %T", accounts["randall"])
	}
	if live["stale"] != false {
		t.Fatalf("stale = %v, want false", live["stale"])
	}
	if live["daily"] == nil {
		t.Fatal("live series lost its daily forecast")
	}
	// 顶层镜像跟随最新鲜序列（randall），冻结桶不污染镜像。
	if report["daily"] == nil {
		t.Fatal("mirror daily = nil, want live series forecast")
	}
}

// TestQuotaSeriesStaleBound 验证冻结判定的 bound 形态：采样周期未设
// （<=0，停采态）时按一小时下限；周期拉大后 bound 随 3×周期放大。
func TestQuotaSeriesStaleBound(t *testing.T) {
	h := &Handler{}
	now := time.Now().Unix()
	aged := func(ageSeconds int64) []*store.QuotaSample {
		return []*store.QuotaSample{
			{At: now - ageSeconds - 300},
			{At: now - ageSeconds},
		}
	}
	if got := h.quotaSeriesReport("x", aged(50*60)); got["stale"] != false {
		t.Fatalf("50min-old tail stale = %v, want false", got["stale"])
	}
	if got := h.quotaSeriesReport("x", aged(70*60)); got["stale"] != true {
		t.Fatalf("70min-old tail stale = %v, want true", got["stale"])
	}
	h.quotaInterval = 2 * time.Hour // bound = max(6h, 1h) = 6h
	if got := h.quotaSeriesReport("x", aged(5*3600)); got["stale"] != false {
		t.Fatalf("5h-old tail stale = %v, want false (scaled bound)", got["stale"])
	}
	if got := h.quotaSeriesReport("x", aged(7*3600)); got["stale"] != true {
		t.Fatalf("7h-old tail stale = %v, want true (scaled bound)", got["stale"])
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

// TestQuotaSamplePersistRetry 验证配额点写失败挂进重放缓冲随下次落库
// 重放：关闭库让每次 INSERT 必败，六次落点各产一笔挂账，缓冲深度
// quotaPersistRetryCap=4 溢出后丢最老两点——守恒：推入 = 在缓 + 丢弃。
// 库重开后一次落库把缓冲四点与新点共五行写回，曲线无断档。落库健康账
// 同步校验：六次写尝试失败、两个溢出丢弃点、四个挂账点经末轮重放救回。
func TestQuotaSamplePersistRetry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}
	_ = st.Close() // 落库必败
	for i := 0; i < 6; i++ {
		_ = h.persistQuotaSample(
			&store.QuotaSample{At: 1700000000 + int64(i*300), Account: "randall", DailyRemaining: f64(float64(90 - i))})
	}
	h.quotaPendingMu.Lock()
	pending := len(h.pendingQuotaSamples)
	oldest := h.pendingQuotaSamples[0].At
	h.quotaPendingMu.Unlock()
	if pending != quotaPersistRetryCap || oldest != 1700000000+2*300 {
		t.Fatalf("pending = %d oldest at = %d, want %d rows from at=%d",
			pending, oldest, quotaPersistRetryCap, 1700000000+2*300)
	}
	if stats := h.quotaPersistStats(); stats["persist_failures"] != 6 ||
		stats["persist_dropped"] != 2 || stats["persist_replayed"] != 0 ||
		stats["pending_samples"] != quotaPersistRetryCap {
		t.Fatalf("persist stats after failures = %+v", stats)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	h.store = st2
	_ = h.persistQuotaSample(
		&store.QuotaSample{At: 1700000000 + 6*300, Account: "randall", DailyRemaining: f64(84)})
	rows, err := st2.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[0].At != 1700000000+2*300 || rows[4].At != 1700000000+6*300 {
		t.Fatalf("replayed samples = %+v, want 5 rows from at=%d", rows, 1700000000+2*300)
	}
	if stats := h.quotaPersistStats(); stats["persist_replayed"] != 4 || stats["pending_samples"] != 0 {
		t.Fatalf("persist stats after replay = %+v", stats)
	}
}

// TestQuotaPersistOwnsBudget 复刻 2026-09-19 02:02 randall 丢点形态：
// 拉取吃掉调用方预算大半后，写库又撞上外部连接 BEGIN IMMEDIATE 持锁
// ——调用方 ctx 在锁释放前到期。共享预算时代这次写死于
// context deadline exceeded 只能挂账等下轮；persist 自带
// quotaPersistBudget 独立死线后，等锁释放照常落库、缓冲不留痕。
func TestQuotaPersistOwnsBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// 外部连接持写锁 1.1s——复刻批量事务的争用窗。
	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("sql.Open holder: %v", err)
	}
	defer func() { _ = holder.Close() }()
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		time.Sleep(1100 * time.Millisecond)
		_, _ = conn.ExecContext(context.Background(), "COMMIT")
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{"name":"Randall","planStatus":{"dailyQuotaRemainingPercent":90}}}`))
	}))
	defer srv.Close()
	h := &Handler{store: st}
	up, err := newPanelUpstream(srv.URL, "", false, func() string { return "unused" })
	if err != nil {
		t.Fatal(err)
	}
	h.upstreamPtr.Store(up)

	// 调用方预算 800ms：fetch 毫秒级完成，但写锁 1.1s 后才释放——
	// persist 若仍共享调用方预算必死于 deadline。
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	if _, _, _, err := h.captureAccountQuota(ctx, "randall", "tok-r"); err != nil {
		t.Fatalf("captureAccountQuota: %v", err)
	}
	<-committed
	rows, err := st.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("samples = %+v, want the point landed after lock release", rows)
	}
	h.quotaPendingMu.Lock()
	pending := len(h.pendingQuotaSamples)
	h.quotaPendingMu.Unlock()
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 — write succeeded on first attempt", pending)
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

// TestSetQuotaIntervalAfterDrain 验证排空闩：BeginDrain 置位后
// SetQuotaInterval 只更新 quotaInterval 簿记、不再重起采样协程——
// 排空窗口内的 config reload 与设置写入都经 SetQuotaInterval，闩缺席
// 时它们会把已收束的上游生产者重新武装。
func TestSetQuotaIntervalAfterDrain(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	// 空池接线：协程起跑即取账号清单，空清单让它空转待机——不触上游、
	// 不落样本，quotaCancel 是否非 nil 即「协程是否活着」的判定面。
	h := &Handler{store: st, poolTokenFuncs: func() map[string]func() string { return nil }}

	h.SetQuotaInterval(time.Minute)
	h.quotaMu.Lock()
	armed := h.quotaCancel != nil
	h.quotaMu.Unlock()
	if !armed {
		t.Fatal("sampler not armed after SetQuotaInterval")
	}

	h.BeginDrain()
	h.SetQuotaInterval(2 * time.Minute)
	h.quotaMu.Lock()
	rearmed := h.quotaCancel != nil
	h.quotaMu.Unlock()
	if rearmed {
		t.Fatal("sampler re-armed after drain")
	}
	if got := h.QuotaInterval(); got != 2*time.Minute {
		t.Fatalf("QuotaInterval = %v, want 2m (bookkeeping still records)", got)
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

// TestQuotaSamplerHeartbeat 验证定时采样轮的逐 lane 心跳账：两号池下
// 一号成功、一号 fetch 失败，计数落位与代码路径一一对应——
// rounds_started 记起跑、fetch_ok/persist_ok 分阶段推进、
// failed_fetch 记失败并留 last_error；协程级 rounds_started 与
// last_round_*_at 同步推进。手动刷新走同一内核但不记 lane 账，
// 保住「调度器活没活」的判读纯度。
func TestQuotaSamplerHeartbeat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") == "Bearer tok-y" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"userStatus":{"name":"R","planStatus":{"dailyQuotaRemainingPercent":90}}}`))
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)
	h.poolTokenFuncs = func() map[string]func() string {
		return map[string]func() string{
			"randall": func() string { return "tok-r" },
			"yanjian": func() string { return "tok-y" },
		}
	}
	before := time.Now().Unix()
	h.sampleQuota(context.Background())

	stats := h.quotaPersistStats()
	if stats["rounds_started"] != int64(1) || stats["rounds_aborted"] != int64(0) {
		t.Fatalf("round counters = %+v", stats)
	}
	if stats["last_round_started_at"].(int64) < before ||
		stats["last_round_finished_at"].(int64) < stats["last_round_started_at"].(int64) {
		t.Fatalf("round timestamps = %+v", stats)
	}
	lanes, ok := stats["lanes"].(map[string]any)
	if !ok || len(lanes) != 2 {
		t.Fatalf("lanes = %+v", stats["lanes"])
	}
	r := lanes["randall"].(map[string]any)
	if r["rounds_started"] != int64(1) || r["rounds_fetch_ok"] != int64(1) ||
		r["rounds_persist_ok"] != int64(1) || r["rounds_failed"] != int64(0) {
		t.Fatalf("randall lane = %+v", r)
	}
	if _, ok := r["last_error"]; ok {
		t.Fatalf("randall last_error present on success: %+v", r)
	}
	y := lanes["yanjian"].(map[string]any)
	if y["rounds_started"] != int64(1) || y["rounds_fetch_ok"] != int64(0) ||
		y["rounds_persist_ok"] != int64(0) || y["rounds_failed"] != int64(1) {
		t.Fatalf("yanjian lane = %+v", y)
	}
	if y["last_error"] == nil || y["last_error"] == "" {
		t.Fatalf("yanjian last_error = %v, want fetch error text", y["last_error"])
	}
	failures := y["failures"].(map[string]any)
	if failures["fetch"] != int64(1) || failures["no_plan"] != int64(0) || failures["persist"] != int64(0) {
		t.Fatalf("yanjian failures = %+v", failures)
	}
	if y["last_started_at"].(int64) < before ||
		y["last_finished_at"].(int64) < y["last_started_at"].(int64) {
		t.Fatalf("yanjian timestamps = %+v", y)
	}

	// 手动刷新共用 capture 内核但不记 lane 账——混入会把「调度器死了
	// 但刷新还在写」误读成采样轮在推进。
	if _, err := h.refreshAccountQuota(context.Background(), "randall", "tok-r"); err != nil {
		t.Fatal(err)
	}
	r2 := h.quotaPersistStats()["lanes"].(map[string]any)["randall"].(map[string]any)
	if r2["rounds_started"] != int64(1) || r2["rounds_persist_ok"] != int64(1) {
		t.Fatalf("manual refresh polluted lane stats: %+v", r2)
	}
}

// TestQuotaSamplerHeartbeatNoPlan 验证「拉到但缺 planStatus」的分桶：
// fetch_ok 照涨（拉取确实成功）、failed_no_plan 记无点可写并留
// last_error——匿名 fallback 单号的 account="" 折叠成 default 桶，
// 与 QuotaReport 的 ”/default 口径一致。
func TestQuotaSamplerHeartbeatNoPlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{"name":"Solo","email":"s@x.com"}}`))
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)
	h.tokenFunc = func() string { return "tok-solo" }

	h.sampleQuota(context.Background())

	lanes := h.quotaPersistStats()["lanes"].(map[string]any)
	d, ok := lanes["default"].(map[string]any)
	if !ok {
		t.Fatalf("default lane = %+v, want '' folded to default", lanes)
	}
	if d["rounds_started"] != int64(1) || d["rounds_fetch_ok"] != int64(1) ||
		d["rounds_persist_ok"] != int64(0) || d["rounds_failed"] != int64(1) {
		t.Fatalf("default lane = %+v", d)
	}
	failures := d["failures"].(map[string]any)
	if failures["no_plan"] != int64(1) {
		t.Fatalf("failures = %+v", failures)
	}
	if d["last_error"] != "userStatus carried no planStatus" {
		t.Fatalf("last_error = %v", d["last_error"])
	}
}

// TestQuotaSamplerHeartbeatPersistFail 验证落库失败的归因：fetch_ok
// 涨了但 persist_ok 不动、failed_persist 记批停错误文本——正是
// 「started 推进而 persist_ok 不动」三类形态里可归因的那一支
// （有 WARN 的写失败），点挂进重放缓冲待下轮救回。
func TestQuotaSamplerHeartbeatPersistFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{"name":"R","planStatus":{"dailyQuotaRemainingPercent":90}}}`))
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)
	_ = h.store.Close() // 落库必败

	h.sampleAccountQuota(context.Background(), "randall", "tok-r")

	stats := h.quotaPersistStats()
	if stats["pending_samples"] != 1 {
		t.Fatalf("pending_samples = %v, want point stashed for replay", stats["pending_samples"])
	}
	r := stats["lanes"].(map[string]any)["randall"].(map[string]any)
	if r["rounds_started"] != int64(1) || r["rounds_fetch_ok"] != int64(1) ||
		r["rounds_persist_ok"] != int64(0) || r["rounds_failed"] != int64(1) {
		t.Fatalf("randall lane = %+v", r)
	}
	failures := r["failures"].(map[string]any)
	if failures["persist"] != int64(1) || failures["fetch"] != int64(0) {
		t.Fatalf("failures = %+v", failures)
	}
	if r["last_error"] == nil || r["last_error"] == "" {
		t.Fatalf("last_error = %v, want persist error text", r["last_error"])
	}
}

// TestQuotaSamplerRoundAbort 验证轮次被 ctx 中途截断的记账：
// rounds_started 照计（协程确实起跑），rounds_aborted 记截断，
// lane 账一行都没有——与「ticker 没触发」（last_round_started_at
// 冻结）区分开。
func TestQuotaSamplerRoundAbort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"userStatus":{"name":"R","planStatus":{}}}`))
	}))
	defer srv.Close()
	h := newQuotaTestHandler(t, srv)
	h.poolTokenFuncs = func() map[string]func() string {
		return map[string]func() string{
			"randall": func() string { return "tok-r" },
			"yanjian": func() string { return "tok-y" },
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.sampleQuota(ctx)

	stats := h.quotaPersistStats()
	if stats["rounds_started"] != int64(1) || stats["rounds_aborted"] != int64(1) {
		t.Fatalf("round counters = %+v", stats)
	}
	if lanes := stats["lanes"].(map[string]any); len(lanes) != 0 {
		t.Fatalf("lanes = %+v, want none — round aborted before first lane", lanes)
	}
}

// TestQuotaDrainFlushPending 验证排空冲刷：写失败挂进重放缓冲的点在
// BeginDrain 时经 FlushPendingQuotaSamples 拿到最后一轮同步落库——
// 缓冲先靠关库喂出两笔挂账，重开后 BeginDrain 把它们写回，曲线不断档，
// 救回点数照常计 persist_replayed。
func TestQuotaDrainFlushPending(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{store: st}
	_ = st.Close() // 落库必败，喂两笔挂账
	for i := 0; i < 2; i++ {
		h.persistQuotaSample(
			&store.QuotaSample{At: 1700000000 + int64(i*300), Account: "randall", DailyRemaining: f64(90 - float64(i))})
	}
	if stats := h.quotaPersistStats(); stats["pending_samples"] != 2 {
		t.Fatalf("pending = %+v, want 2 stashed", stats)
	}

	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	h.store = st2

	h.BeginDrain()
	rows, err := st2.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].At != 1700000000 || rows[1].At != 1700000000+300 {
		t.Fatalf("flushed samples = %+v, want 2 rows", rows)
	}
	if stats := h.quotaPersistStats(); stats["persist_replayed"] != 2 || stats["pending_samples"] != 0 {
		t.Fatalf("persist stats after drain flush = %+v", stats)
	}
}

// TestQuotaDrainFlushWaitsInflight 验证冲刷的在途落定等待：一笔落库
// 被外部写锁卡住时 BeginDrain 不得抢先取走空缓冲收工——等锁释放、
// 在途写落定后冲刷才返回，样本落库不留挂账。
func TestQuotaDrainFlushWaitsInflight(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	h := &Handler{store: st}

	holder, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)")
	if err != nil {
		t.Fatalf("sql.Open holder: %v", err)
	}
	defer func() { _ = holder.Close() }()
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatalf("holder conn: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE: %v", err)
	}
	committed := make(chan struct{})
	go func() {
		defer close(committed)
		time.Sleep(400 * time.Millisecond)
		_, _ = conn.ExecContext(context.Background(), "COMMIT")
	}()

	go h.persistQuotaSample(&store.QuotaSample{At: 1700000000, Account: "randall", DailyRemaining: f64(90)})
	// 等在途计数起来再排空：否则冲刷读到的 persistDone 还是 nil，
	// 等待路径根本没被踩到。
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.quotaPendingMu.Lock()
		n := h.quotaPersistInFlight
		h.quotaPendingMu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("persist goroutine never went in-flight")
		}
		time.Sleep(5 * time.Millisecond)
	}

	h.BeginDrain()
	select {
	case <-committed:
	default:
		t.Fatal("BeginDrain returned before in-flight persist settled")
	}
	rows, err := st.ListQuotaSamples(context.Background(), "randall", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].At != 1700000000 {
		t.Fatalf("samples = %+v, want the in-flight point landed", rows)
	}
	if stats := h.quotaPersistStats(); stats["pending_samples"] != 0 || stats["persist_failures"] != 0 {
		t.Fatalf("persist stats = %+v, want clean settle", stats)
	}
}
