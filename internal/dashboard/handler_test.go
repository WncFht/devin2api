// 本文件验证面板请求浏览端点：Bearer 鉴权、索引列表、详情与文件读取。
package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/obs"
)

// newTestPanel 挂载面板路由到 chi，返回可直接 ServeHTTP 的 handler。
// token 留空时上游字段不会被这些端点触达。
func newTestPanel(t *testing.T, password string, manager *debuglog.Manager) http.Handler {
	t.Helper()
	handler := New(password, "https://example.com", func() string { return "" }, "", false, obs.NewMetrics(), manager)
	router := chi.NewRouter()
	handler.Register(router)
	return router
}

// TestPanelRequestsEndpoints 验证请求列表、详情与文件端点的完整链路。
func TestPanelRequestsEndpoints(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{})
	defer manager.Close()
	recorder := manager.Start(debuglog.RequestMeta{Method: "POST", Path: "/v1/messages", API: "anthropic"})
	recorder.WriteJSON("03-devin-request.json", map[string]any{"model": "swe-2-max"})
	dir := filepath.Base(recorder.DirectoryPath())
	recorder.Complete(debuglog.Completion{StatusCode: 200, Result: "completed", Model: "swe-2-max"})

	server := httptest.NewServer(newTestPanel(t, "pw", manager))
	defer server.Close()
	get := func(path string, auth bool) (int, map[string]any) {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if auth {
			request.Header.Set("Authorization", "Bearer pw")
		}
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		var body map[string]any
		_ = json.NewDecoder(response.Body).Decode(&body)
		return response.StatusCode, body
	}

	if code, _ := get("/panel/api/requests", false); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated = %d, want 401", code)
	}
	code, body := get("/panel/api/requests", true)
	if code != 200 {
		t.Fatalf("requests = %d: %v", code, body)
	}
	requests, _ := body["requests"].([]any)
	if len(requests) != 1 {
		t.Fatalf("requests = %v", body)
	}
	// offset 超出总数时返回空数组而非 null——前端按数组消费。
	code, body = get("/panel/api/requests?offset=9", true)
	if code != 200 || body["requests"] == nil || len(body["requests"].([]any)) != 0 {
		t.Fatalf("offset beyond total = %d %v, want empty array", code, body)
	}
	code, body = get("/panel/api/requests/"+dir, true)
	if code != 200 || body["dir"] != dir {
		t.Fatalf("detail = %d %v", code, body)
	}
	code, body = get("/panel/api/requests/"+dir+"/file/03-devin-request.json", true)
	if code != 200 || !strings.Contains(body["text"].(string), "swe-2-max") {
		t.Fatalf("file = %d %v", code, body)
	}
	// merged 响应必须带 truncated 字段：超过 4MB 读取上限的 06 只合并
	// 前 4MB，没有这个标记残缺流会被当成完整响应。
	streamPath := filepath.Join(recorder.DirectoryPath(), "06-http-response.jsonl")
	if err := os.WriteFile(streamPath, []byte(`{"event":"x","data":{}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = get("/panel/api/requests/"+dir+"/merged", true)
	if code != 200 || body["truncated"] != false {
		t.Fatalf("merged small = %d %v", code, body)
	}
	if err := os.WriteFile(streamPath, make([]byte, (4<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	code, body = get("/panel/api/requests/"+dir+"/merged", true)
	if code != 200 || body["truncated"] != true {
		t.Fatalf("merged big = %d %v, want truncated=true", code, body)
	}
	if code, _ := get("/panel/api/requests/"+dir+"/file/../../config.yaml", true); code != http.StatusNotFound {
		t.Fatalf("traversal should 404")
	}
	code, body = get("/panel/api/stats", true)
	if code != 200 || body["debuglog"] == nil {
		t.Fatalf("stats = %d %v", code, body)
	}
}

// TestPanelExportTruncatedHeader 验证导出打满扫描上限时回 X-Truncated 头。
func TestPanelExportTruncatedHeader(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	// 超过扫描上限的索引让 HasMore 为真——CSV/JSON 响应体本身没有
	// 元数据位，截断信号走响应头。
	var index strings.Builder
	for i := 0; i < requestsFetchCap+1; i++ {
		fmt.Fprintf(&index, `{"dir":"d%05d","started_at":"2026-09-14T00:00:%02dZ","status_code":200,"result":"completed"}`+"\n", i, i%60)
	}
	if err := os.WriteFile(filepath.Join(root, "index.jsonl"), []byte(index.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := debuglog.NewManager(root, debuglog.RetentionPolicy{})
	defer manager.Close()
	server := httptest.NewServer(newTestPanel(t, "pw", manager))
	defer server.Close()

	request, _ := http.NewRequest(http.MethodGet, server.URL+"/panel/api/requests/export?format=csv", nil)
	request.Header.Set("Authorization", "Bearer pw")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Header.Get("X-Truncated") != "true" {
		t.Fatalf("X-Truncated = %q, want true", response.Header.Get("X-Truncated"))
	}
}

// TestLoginFailureSweep 验证失败路径会清扫已失效的爆破条目——纯爆破
// 流量永远不走成功路径，loginFailures 不能无界增长。
func TestLoginFailureSweep(t *testing.T) {
	handler := New("pw", "https://example.com", nil, "", false, nil, nil)
	stale := time.Now().Add(-time.Hour)
	for i := 0; i < sessionSweepThreshold+10; i++ {
		handler.loginFailures[fmt.Sprintf("10.0.0.%d", i)] = &loginFail{fails: 1, lastSeen: stale}
	}
	request := httptest.NewRequest(http.MethodPost, "/panel/login", strings.NewReader("password=wrong"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.RemoteAddr = "1.2.3.4:5678"
	recorder := httptest.NewRecorder()
	handler.handleLogin(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", recorder.Code)
	}
	if len(handler.loginFailures) != 1 {
		t.Fatalf("loginFailures = %d entries, want stale entries swept", len(handler.loginFailures))
	}
}
