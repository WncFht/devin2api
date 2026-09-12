// 本文件验证面板请求浏览端点：Bearer 鉴权、索引列表、详情与文件读取。
package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	code, body = get("/panel/api/requests/"+dir, true)
	if code != 200 || body["dir"] != dir {
		t.Fatalf("detail = %d %v", code, body)
	}
	code, body = get("/panel/api/requests/"+dir+"/file/03-devin-request.json", true)
	if code != 200 || !strings.Contains(body["text"].(string), "swe-2-max") {
		t.Fatalf("file = %d %v", code, body)
	}
	if code, _ := get("/panel/api/requests/"+dir+"/file/../../config.yaml", true); code != http.StatusNotFound {
		t.Fatalf("traversal should 404")
	}
	code, body = get("/panel/api/stats", true)
	if code != 200 || body["debuglog"] == nil {
		t.Fatalf("stats = %d %v", code, body)
	}
}
