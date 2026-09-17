// 本文件验证配额历史的读取与燃烧速率预测。
package ccpanel

import (
	"context"
	"encoding/json"
	"fmt"
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
	st, _, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
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
