// 本文件验证配额历史的读取与燃烧速率预测。
package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// TestForecastBurnRate 验证线性差分得到的燃烧速率与耗尽时刻。
func TestForecastBurnRate(t *testing.T) {
	now := time.Now().Unix()
	points := []quotaPoint{
		{At: now - 7200, DailyRemaining: 80, DailyResetAt: now + 10000},
		{At: now - 3600, DailyRemaining: 70},
		{At: now, DailyRemaining: 60, DailyResetAt: now + 10000},
	}
	got := forecast(points, 24*time.Hour,
		func(p quotaPoint) float64 { return p.DailyRemaining },
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
	if got["reset_at"].(int64) != now+10000 {
		t.Fatalf("reset_at = %v", got["reset_at"])
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
		point := quotaPoint{At: 1700000000 + int64(i*600), DailyRemaining: remaining}
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
	if len(points) != 3 || points[2].DailyRemaining != 80 {
		t.Fatalf("points = %+v", points)
	}
	got := forecast(points, 24*time.Hour,
		func(p quotaPoint) float64 { return p.DailyRemaining },
		func(p quotaPoint) int64 { return p.DailyResetAt })
	if got == nil {
		t.Fatal("forecast = nil")
	}
	// 90→80 跨 1200s（1/3 小时）= 30 %/h。
	if rate := got["burn_per_hour"].(float64); fmt.Sprintf("%.2f", rate) != "30.00" {
		t.Fatalf("burn_per_hour = %v", rate)
	}
}
