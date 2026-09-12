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
	r.Finish(http.StatusOK, 500)
	r2 := m.Begin()
	r2.Finish(http.StatusBadRequest, 0)
	m.Reject()

	snap := m.Snapshot()
	if snap["completed_requests"].(uint64) != 2 || snap["ok_responses"].(uint64) != 1 || snap["client_error_responses"].(uint64) != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
	if snap["active_requests"].(int64) != 0 || snap["rejected_requests"].(uint64) != 1 {
		t.Fatalf("snapshot = %v", snap)
	}
	if snap["streaming_requests"].(uint64) != 1 || snap["response_body_bytes"].(uint64) != 500 {
		t.Fatalf("snapshot = %v", snap)
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

// TestTrendBuckets 验证分钟桶把请求与错误归入当前分钟并出现在快照里。
func TestTrendBuckets(t *testing.T) {
	m := NewMetrics()
	m.Begin().Finish(200, 0)
	m.Begin().Finish(500, 0)
	m.Reject()
	trend, _ := m.Snapshot()["trend_minutes"].([]map[string]any)
	if len(trend) != 60 {
		t.Fatalf("trend len = %d, want 60", len(trend))
	}
	last := trend[len(trend)-1]
	if last["requests"] != uint64(3) || last["errors"] != uint64(2) {
		t.Fatalf("last bucket = %+v, want requests=3 errors=2", last)
	}
}
