package ccpanel

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/adapter/devin"
)

// TestMetricsHistoryRingOrder 验证环按写入序返回最老在前。
func TestMetricsHistoryRingOrder(t *testing.T) {
	var r metricsHistory
	for i := int64(0); i < 10; i++ {
		r.add(metricsSample{At: 1000 + i})
	}
	got := r.since(0)
	if len(got) != 10 {
		t.Fatalf("since(0) returned %d samples, want 10", len(got))
	}
	for i, s := range got {
		if s.At != 1000+int64(i) {
			t.Fatalf("sample %d at=%d, want %d", i, s.At, 1000+int64(i))
		}
	}
}

// TestMetricsHistoryRingWraparound 验证写满后覆盖最老样本：写 cap+50 点，
// 最老 50 点被逐出，序仍保持单调。
func TestMetricsHistoryRingWraparound(t *testing.T) {
	var r metricsHistory
	for i := int64(0); i < metricsHistoryCap+50; i++ {
		r.add(metricsSample{At: i})
	}
	got := r.since(0)
	if len(got) != metricsHistoryCap {
		t.Fatalf("since(0) returned %d samples, want %d", len(got), metricsHistoryCap)
	}
	if got[0].At != 50 {
		t.Fatalf("oldest surviving sample at=%d, want 50", got[0].At)
	}
	if last := got[len(got)-1]; last.At != metricsHistoryCap+49 {
		t.Fatalf("newest sample at=%d, want %d", last.At, metricsHistoryCap+49)
	}
}

// TestMetricsHistorySince 验证 cutoff 过滤：只有 at >= cutoff 的样本返回；
// 空环返回空切片（JSON 落成 [] 而非 null）。
func TestMetricsHistorySince(t *testing.T) {
	var r metricsHistory
	for i := int64(0); i < 10; i++ {
		r.add(metricsSample{At: i})
	}
	got := r.since(7)
	if len(got) != 3 || got[0].At != 7 {
		t.Fatalf("since(7) = %v, want samples at 7,8,9", got)
	}
	var empty metricsHistory
	if out := empty.since(0); out == nil || len(out) != 0 {
		t.Fatalf("empty ring since(0) = %v, want non-nil empty slice", out)
	}
}

// TestAdminRuntimeMetricsHistory 验证端点 ?minutes 过滤与响应形状：
// 窗口外样本被裁、interval_sec 透出、at 升序、minutes 超上限按 240 截断。
func TestAdminRuntimeMetricsHistory(t *testing.T) {
	h, err := New(Deps{Password: "pw", BaseURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	h.history.add(metricsSample{At: now - 3*3600, ActiveRequests: 1}) // 3h 前：出 60min 默认窗
	h.history.add(metricsSample{At: now - 30*60, ActiveRequests: 3})  // 30min 前
	h.history.add(metricsSample{At: now - 5*60, ActiveRequests: 7})   // 5min 前
	h.history.add(metricsSample{At: now, ActiveRequests: 9})          // 现在：所有窗内

	type payload struct {
		Data struct {
			IntervalSec int `json:"interval_sec"`
			Samples     []struct {
				At             int64 `json:"at"`
				ActiveRequests int64 `json:"active_requests"`
			} `json:"samples"`
		} `json:"data"`
	}
	get := func(query string) payload {
		r := httptest.NewRequest("GET", "/admin/runtime-metrics/history"+query, nil)
		rec := httptest.NewRecorder()
		h.adminRuntimeMetricsHistory(rec, r)
		var resp payload
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("response is not valid JSON: %v (%s)", err, rec.Body.String())
		}
		return resp
	}

	resp := get("")
	if resp.Data.IntervalSec != 30 {
		t.Fatalf("interval_sec = %d, want 30", resp.Data.IntervalSec)
	}
	if len(resp.Data.Samples) != 3 {
		t.Fatalf("default window returned %d samples, want 3", len(resp.Data.Samples))
	}
	for i, want := range []int64{3, 7, 9} {
		if resp.Data.Samples[i].ActiveRequests != want {
			t.Fatalf("sample %d active_requests = %d, want %d", i, resp.Data.Samples[i].ActiveRequests, want)
		}
	}
	for i := 1; i < len(resp.Data.Samples); i++ {
		if resp.Data.Samples[i].At < resp.Data.Samples[i-1].At {
			t.Fatalf("samples not in ascending at order")
		}
	}
	if resp := get("?minutes=240"); len(resp.Data.Samples) != 4 {
		t.Fatalf("minutes=240 returned %d samples, want 4", len(resp.Data.Samples))
	}
	if resp := get("?minutes=1"); len(resp.Data.Samples) != 1 {
		t.Fatalf("minutes=1 returned %d samples, want 1", len(resp.Data.Samples))
	}
	// 超过上限的 minutes 按 240 截断——与全窗同结果。
	if resp := get("?minutes=9999"); len(resp.Data.Samples) != 4 {
		t.Fatalf("minutes=9999 returned %d samples, want 4 (capped to 240)", len(resp.Data.Samples))
	}
}

// TestCaptureMetricsSampleSources 验证采样从注入源取数：gate 闩态按
// lane 聚合计数、闩次数求全 lane 和。
func TestCaptureMetricsSampleSources(t *testing.T) {
	h, err := New(Deps{Password: "pw", BaseURL: "https://example.com"})
	if err != nil {
		t.Fatal(err)
	}
	h.pool = &PoolDeps{Snapshot: func() devin.PoolSnapshot {
		return devin.PoolSnapshot{Accounts: map[string]devin.LaneSnapshot{
			"a": {Gate: devin.GateStats{Latched: true, LatchCount: 2}},
			"b": {Gate: devin.GateStats{Latched: false, LatchCount: 3}},
			"c": {Gate: devin.GateStats{Latched: true, LatchCount: 1}},
		}}
	}}
	h.captureMetricsSample()
	got := h.history.since(0)
	if len(got) != 1 {
		t.Fatalf("ring has %d samples, want 1", len(got))
	}
	if got[0].GateLatchedLanes != 2 {
		t.Fatalf("gate_latched_lanes = %d, want 2", got[0].GateLatchedLanes)
	}
	if got[0].GateLatchTotal != 6 {
		t.Fatalf("gate_latch_total = %d, want 6", got[0].GateLatchTotal)
	}
}
