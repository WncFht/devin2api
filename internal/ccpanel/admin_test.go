// 本文件验证 /admin/runtime-metrics 的 store 组投影：开库台账的
// 形状、argv 渲染与表缺席降级（观测面不拖死端点）。
package ccpanel

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/WncFht/devin2api/internal/store"
)

// TestRuntimeMetricsStoreSection 钉住 store 组形状：opens_total 全
// 行数、opens_recent 新在前且 argv 渲染成单行、opens_distinct_pids_24h
// 只数窗内 pid——第二个 pid 附着即在这里可见。
func TestRuntimeMetricsStoreSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	// 外来者用另一条连接附着（WAL 多连接即台账要逮的场景）：先一
	// 行 24h 窗外的老 pid，再一行窗内 rogue——id 序即附着序。
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := raw.Exec(`INSERT INTO store_opens(at, pid, argv, build, path) VALUES(?,?,?,?,?)`,
		now-25*3600*1000, 9999, `["old-probe"]`, "v0-old", path); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO store_opens(at, pid, argv, build, path) VALUES(?,?,?,?,?)`,
		now-1000, 4242, `["rogue","--attach"]`, "v9-rogue", path); err != nil {
		t.Fatalf("seed rogue: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	h, err := New("pw", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.SetStore(st)
	rec := httptest.NewRecorder()
	h.adminRuntimeMetrics(rec, httptest.NewRequest("GET", "/admin/runtime-metrics", nil))

	var env struct {
		Success bool `json:"success"`
		Data    struct {
			Store *struct {
				Total           int64 `json:"opens_total"`
				DistinctPIDs24h int64 `json:"opens_distinct_pids_24h"`
				Recent          []struct {
					At    int64  `json:"at"`
					PID   int64  `json:"pid"`
					Build string `json:"build"`
					Argv  string `json:"argv"`
				} `json:"opens_recent"`
			} `json:"store"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if !env.Success {
		t.Fatalf("success=false body=%s", rec.Body.String())
	}
	if env.Data.Store == nil {
		t.Fatalf("store section missing: %s", rec.Body.String())
	}
	got := env.Data.Store
	if got.Total != 3 {
		t.Fatalf("opens_total = %d, want 3", got.Total)
	}
	if got.DistinctPIDs24h != 2 {
		t.Fatalf("opens_distinct_pids_24h = %d, want 2", got.DistinctPIDs24h)
	}
	if len(got.Recent) != 3 {
		t.Fatalf("opens_recent len = %d, want 3", len(got.Recent))
	}
	top := got.Recent[0]
	if top.PID != 4242 || top.Build != "v9-rogue" || top.Argv != "rogue --attach" {
		t.Fatalf("opens_recent[0] = %+v, want rogue row with joined argv", top)
	}
}

// TestRuntimeMetricsStoreSectionMissingTable 钉住降级姿态：台账表
// 缺席（老库被旧二进制打开后交接）时端点仍 200，只省略 store 组。
func TestRuntimeMetricsStoreSectionMissingTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() { _ = st.Close() }()

	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := raw.Exec(`DROP TABLE store_opens`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("raw close: %v", err)
	}

	h, err := New("pw", "https://example.com", nil, "", false, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.SetStore(st)
	rec := httptest.NewRecorder()
	h.adminRuntimeMetrics(rec, httptest.NewRequest("GET", "/admin/runtime-metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var env map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if env["success"] != true {
		t.Fatalf("success=%v body=%s", env["success"], rec.Body.String())
	}
	if _, ok := env["data"].(map[string]any)["store"]; ok {
		t.Fatalf("store section should be absent on missing table: %s", rec.Body.String())
	}
}
