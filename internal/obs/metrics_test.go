package obs

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestMetricsLifecycle(t *testing.T) {
	m := NewMetrics()
	r := m.Begin()
	r.Observe(true, 100)
	r.Finish(http.StatusOK, 500, "completed")
	r2 := m.Begin()
	r2.Finish(http.StatusBadRequest, 0, "failed")
	m.Reject(RejectDraining, RejectEvent{Status: http.StatusServiceUnavailable, Path: "/v1/messages"})

	snap := m.Snapshot()
	if snap.CompletedRequests != 2 || snap.OKResponses != 1 || snap.ClientErrorResponses != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.ActiveRequests != 0 || snap.RejectedRequests != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Rejects.ByReason[string(RejectDraining)] != 1 {
		t.Fatalf("rejects = %+v", snap.Rejects)
	}
	recent := snap.Rejects.Recent
	if len(recent) != 1 || recent[0].Path != "/v1/messages" || recent[0].Reason != string(RejectDraining) {
		t.Fatalf("recent = %+v", recent)
	}
	if snap.StreamingRequests != 1 || snap.ResponseBodyBytes != 500 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestForeignListenHolders(t *testing.T) {
	m := NewMetrics()
	snap := m.Snapshot()
	if snap.ForeignListenHolders != 0 || snap.ForeignListenLastSeen != 0 {
		t.Fatalf("fresh snapshot = %+v", snap)
	}
	m.NoteForeignListenHolders(2)
	m.NoteForeignListenHolders(0)
	snap = m.Snapshot()
	// gauge 随当轮归零，last_seen 保留「曾经见过」的口径。
	if snap.ForeignListenHolders != 0 || snap.ForeignListenLastSeen == 0 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestDiagnosticBoundsAndRedacts(t *testing.T) {
	err := errors.New(`upstream failed: authorization="Bearer sk-live-secret-token" status=503`)
	got := Diagnostic(err)
	if strings.Contains(got, "sk-live-secret-token") {
		t.Fatalf("diagnostic leaked credential: %s", got)
	}
	if !strings.Contains(got, "status_503") {
		t.Fatalf("missing status signal: %s", got)
	}
	if got := Diagnostic(io.EOF); !strings.Contains(got, "EOF") {
		t.Fatalf("missing EOF signal: %s", got)
	}
	connErr := connect.NewError(connect.CodeUnavailable, errors.New("upstream provider is experiencing issues"))
	if got := Diagnostic(connErr); !strings.Contains(got, "connect_unavailable") {
		t.Fatalf("missing connect code: %s", got)
	}
	long := errors.New(strings.Repeat("x", 1000))
	if len(Diagnostic(long)) > 400 {
		t.Fatalf("diagnostic not bounded: %d", len(Diagnostic(long)))
	}
}

// TestTrendBuckets 验证趋势桶把请求与错误归入当前 10 秒窗口并出现在快照里。
func TestTrendBuckets(t *testing.T) {
	m := NewMetrics()
	m.Begin().Finish(200, 0, "completed")
	m.Begin().Finish(500, 0, "failed")
	// SSE 已提交 200 后客户端断连：HTTP 状态是 2xx，但趋势应计为错误。
	m.Begin().Finish(200, 0, "disconnected")
	m.Reject(RejectConcurrencyLimit, RejectEvent{Status: http.StatusTooManyRequests})
	trend := m.Snapshot().TrendMinutes
	if len(trend) != trendBuckets {
		t.Fatalf("trend len = %d, want %d", len(trend), trendBuckets)
	}
	last := trend[len(trend)-1]
	if last.Requests != 4 || last.Errors != 3 {
		t.Fatalf("last bucket = %+v, want requests=4 errors=3", last)
	}
}

// TestRatesDerived 验证趋势桶按自然分钟合并后派生的 RPM/QPS 指标。
func TestRatesDerived(t *testing.T) {
	m := NewMetrics()
	m.Begin().Finish(200, 0, "completed")
	m.Begin().Finish(200, 0, "completed")
	m.Begin().Finish(500, 0, "failed")
	rates := m.Snapshot().Rates
	if rates.RPMCurrent != 3 || rates.RPMPeak != 3 {
		t.Fatalf("rates = %+v", rates)
	}
	if rates.QPSCurrent <= 0 {
		t.Fatalf("qps_current = %v", rates.QPSCurrent)
	}
	if rates.RPMAvg != 3 {
		t.Fatalf("rpm_avg = %v, want 3 (first minute)", rates.RPMAvg)
	}
}

// TestProcessMetrics 验证进程级指标存在且值域合理。
func TestProcessMetrics(t *testing.T) {
	m := NewMetrics()
	proc := m.Snapshot().Process
	if proc.Goroutines <= 0 {
		t.Fatalf("goroutines = %v", proc.Goroutines)
	}
	if proc.HeapAllocBytes == 0 {
		t.Fatalf("heap_alloc_bytes = %v", proc.HeapAllocBytes)
	}
	// 第二次快照应有非负 CPU 百分比（相邻 rusage 差分）。
	m.Snapshot()
	proc = m.Snapshot().Process
	if proc.CPUPercent < 0 {
		t.Fatalf("cpu_percent = %v", proc.CPUPercent)
	}
}
