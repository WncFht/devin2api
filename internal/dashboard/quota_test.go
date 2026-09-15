// 本文件验证配额历史的读取与燃烧速率预测。
package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/debuglog"
)

func f64(v float64) *float64 { return &v }

// TestForecastBurnRate 验证线性差分得到的燃烧速率与耗尽时刻。
func TestForecastBurnRate(t *testing.T) {
	now := time.Now().Unix()
	points := []quotaPoint{
		{At: now - 7200, DailyRemaining: f64(80), DailyResetAt: now + 100000},
		{At: now - 3600, DailyRemaining: f64(70)},
		{At: now, DailyRemaining: f64(60), DailyResetAt: now + 100000},
	}
	got := forecast(points, 24*time.Hour,
		func(p quotaPoint) float64 { return floatOr0(p.DailyRemaining) },
		func(p quotaPoint) int64 { return p.DailyResetAt })
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
	points := []quotaPoint{
		{At: now - 3600, DailyRemaining: f64(91), DailyResetAt: now + 36000},
		{At: now, DailyRemaining: f64(90), DailyResetAt: now + 36000},
	}
	got := forecast(points, 24*time.Hour,
		func(p quotaPoint) float64 { return floatOr0(p.DailyRemaining) },
		func(p quotaPoint) int64 { return p.DailyResetAt })
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
	if got := forecast(nil, time.Hour, func(p quotaPoint) float64 { return 0 }, func(p quotaPoint) int64 { return 0 }); got != nil {
		t.Fatalf("forecast = %+v, want nil", got)
	}
	if got := forecast([]quotaPoint{{At: 1}}, time.Hour, func(p quotaPoint) float64 { return 0 }, func(p quotaPoint) int64 { return 0 }); got != nil {
		t.Fatalf("forecast = %+v, want nil", got)
	}
}

// TestQuotaFileRoundTrip 验证采样写盘与历史读取的往返。
func TestQuotaFileRoundTrip(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, quotaFileName)
	for i, remaining := range []float64{90, 85, 80} {
		point := quotaPoint{At: 1700000000 + int64(i*600), DailyRemaining: f64(remaining)}
		data, _ := json.Marshal(point)
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(append(data, '\n'))
		_ = f.Close()
	}

	h := &Handler{debugManager: debuglog.NewManager(root, debuglog.RetentionPolicy{})}
	defer h.debugManager.Close()
	points := h.readQuotaHistory()
	if len(points) != 3 || floatOr0(points[2].DailyRemaining) != 80 {
		t.Fatalf("points = %+v", points)
	}
	// 未上报的字段保持 nil 往返——指针字段就是为了区分「没报」与「报到 0%」。
	var bare quotaPoint
	if err := json.Unmarshal([]byte(`{"at":1}`), &bare); err != nil {
		t.Fatal(err)
	}
	if bare.DailyRemaining != nil {
		t.Fatalf("DailyRemaining = %v, want nil for unreported field", *bare.DailyRemaining)
	}
	got := forecast(points, 24*time.Hour,
		func(p quotaPoint) float64 { return floatOr0(p.DailyRemaining) },
		func(p quotaPoint) int64 { return p.DailyResetAt })
	if got == nil {
		t.Fatal("forecast = nil")
	}
	// 90→80 跨 1200s（1/3 小时）= 30 %/h。
	if rate := got["burn_per_hour"].(float64); fmt.Sprintf("%.2f", rate) != "30.00" {
		t.Fatalf("burn_per_hour = %v", rate)
	}
}

// TestQuotaPointGraceAndTopUpFields 验证宽限/加额字段的快照形态：
// grace_period_end 以 unix 秒落盘，缺省字段经 omitempty 不进 JSON。
func TestQuotaPointGraceAndTopUpFields(t *testing.T) {
	end := time.Date(2026, 9, 20, 16, 0, 0, 0, time.UTC)
	point := quotaPoint{
		At:                end.Unix() - 3600,
		GracePeriodStatus: "ACTIVE",
		GracePeriodEnd:    rfc3339Unix(end.Format(time.RFC3339)),
		OrphanedUsageCut:  true,
		TopUpEnabled:      true,
		TopUpStatus:       "SUCCEEDED",
	}
	data, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	var back quotaPoint
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.GracePeriodStatus != "ACTIVE" || back.GracePeriodEnd != end.Unix() ||
		!back.OrphanedUsageCut || !back.TopUpEnabled || back.TopUpStatus != "SUCCEEDED" {
		t.Fatalf("round trip = %+v", back)
	}
	// 空值不序列化：宽限字段在大多数快照里缺席，不能让 0/false 刷存在感。
	var bare quotaPoint
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
