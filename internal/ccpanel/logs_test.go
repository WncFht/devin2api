// 本文件验证 /admin/logs 列表端点的分页契约：count 仅首页返回
// （深页省全窗 COUNT(*)），has_more 在深页靠 limit+1 探测维持。
package ccpanel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/WncFht/devin2api/internal/store"
)

// TestDashboardLogsCountFirstPageOnly 验证 count 的分页语义：首页付
// 精确 COUNT(*) 并回 count；offset/before_id 深页省略 count（前端
// 既有缺省降级路径），has_more 由 limit+1 探测补位。
func TestDashboardLogsCountFirstPageOnly(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := st.InsertLog(ctx, &store.LogRow{
			Dir: fmt.Sprintf("p-%d", i), StartedAt: time.Now(), StatusCode: 200, Result: "completed",
		}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := New("pw", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.SetStore(st)
	call := func(query string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("GET", "/admin/logs?"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), identityContextKey{}, webIdentity{Role: "admin"}))
		rec := httptest.NewRecorder()
		h.dashboardLogs(rec, r)
		var env map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode %q: %v (%s)", query, err, rec.Body.String())
		}
		if env["success"] != true {
			t.Fatalf("query %q success=%v body=%s", query, env["success"], rec.Body.String())
		}
		return env
	}

	// 首页：count=5 精确值在场，has_more 说后面还有。
	p1 := call("limit=2")
	if n, ok := p1["count"]; !ok || n.(float64) != 5 {
		t.Fatalf("page1 count = %v, want 5", p1["count"])
	}
	if p1["has_more"] != true {
		t.Fatalf("page1 has_more = %v, want true", p1["has_more"])
	}
	if got := len(p1["data"].([]any)); got != 2 {
		t.Fatalf("page1 rows = %d, want 2", got)
	}

	// 深页：count 缺席；has_more 由多取的一行判出。
	p2 := call("limit=2&offset=2")
	if _, ok := p2["count"]; ok {
		t.Fatalf("page2 count should be absent, got %v", p2["count"])
	}
	if p2["has_more"] != true {
		t.Fatalf("page2 has_more = %v, want true", p2["has_more"])
	}
	if got := len(p2["data"].([]any)); got != 2 {
		t.Fatalf("page2 rows = %d, want 2", got)
	}

	// 末页：只剩 1 行，has_more=false。
	p3 := call("limit=2&offset=4")
	if _, ok := p3["count"]; ok {
		t.Fatalf("page3 count should be absent, got %v", p3["count"])
	}
	if got := len(p3["data"].([]any)); got != 1 {
		t.Fatalf("page3 rows = %d, want 1", got)
	}
	if p3["has_more"] == true {
		t.Fatal("page3 has_more = true, want false")
	}

	// keyset 深页同样豁免 count：before_id 取 p1 最旧行 id。
	rows, _, err := st.SearchLogs(ctx, store.LogQuery{Limit: 2})
	if err != nil || len(rows) != 2 {
		t.Fatalf("SearchLogs: %v", err)
	}
	pk := call(fmt.Sprintf("limit=2&before_id=%d", rows[1].ID))
	if _, ok := pk["count"]; ok {
		t.Fatalf("before_id page count should be absent, got %v", pk["count"])
	}
	if pk["has_more"] != true {
		t.Fatalf("before_id page has_more = %v, want true", pk["has_more"])
	}
}
