package ccpanel

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/modelreg"
)

// modelRow 是 /admin/model-registry merged 视图的行形状：模型名、来源
// 标记、覆盖层字段（enabled/redirect_model）与最终解析名同列——管理端
// 一眼看到「这个名字实际会路由到哪」。
type modelRow struct {
	Model         string   `json:"model"`
	Enabled       bool     `json:"enabled"`
	RedirectModel string   `json:"redirect_model,omitempty"`
	Resolved      string   `json:"resolved"`
	Sources       []string `json:"sources"`
	HasOverride   bool     `json:"has_override"`
}

// modelNamesUnion 返回对外模型名的并集：目录 uid ∪ 别名键 ∪ 注册表名 ∪
// 流量中出现过的模型名，逐名标记来源。注册表页、渠道模型清单投影共用。
func (h *Handler) modelNamesUnion(r *http.Request) map[string][]string {
	src := map[string][]string{}
	add := func(name, s string) {
		if name != "" {
			src[name] = append(src[name], s)
		}
	}
	for _, uid := range h.panel.ModelUIDs(r.Context()) {
		add(uid, "catalog")
	}
	if h.aliasesFunc != nil {
		for name := range h.aliasesFunc() {
			add(name, "alias")
		}
	}
	if h.models != nil {
		for _, name := range h.models.Names() {
			add(name, "registry")
		}
	}
	for _, m := range h.ru.modelSet(h.debug, "") {
		add(m, "traffic")
	}
	return src
}

// adminModelRegistry 实现 GET /admin/model-registry：merged 模型视图。
// resolved = redirect_model ?? 模型名，再过一遍别名表（与 /v1 准入
// 后 adapter 的解析路径一致）。
func (h *Handler) adminModelRegistry(w http.ResponseWriter, r *http.Request) {
	aliases := map[string]string{}
	if h.aliasesFunc != nil {
		aliases = h.aliasesFunc()
	}
	overrides := map[string]modelreg.Entry{}
	if h.models != nil {
		overrides = h.models.Entries()
	}

	src := h.modelNamesUnion(r)
	rows := make([]modelRow, 0, len(src))
	for name, sources := range src {
		e, has := overrides[name]
		target := name
		if e.RedirectModel != "" {
			target = e.RedirectModel
		}
		rows = append(rows, modelRow{
			Model:         name,
			Enabled:       !e.Disabled,
			RedirectModel: e.RedirectModel,
			Resolved:      devin.ResolveModelAlias(aliases, target),
			Sources:       sources,
			HasOverride:   has,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	respondOK(w, map[string]any{"models": rows})
}

// modelRegistryPut 是 PUT /admin/model-registry 的请求体；enabled 用指针
// 区分「未传」（默认 true）与显式 false。
type modelRegistryPut struct {
	Model         string `json:"model"`
	Enabled       *bool  `json:"enabled"`
	RedirectModel string `json:"redirect_model"`
}

// adminPutModel 实现 PUT /admin/model-registry：upsert 一条覆盖。
// 提交默认值（enabled 且无重定向）等价于删除该覆盖——Store.Set 自动清理。
func (h *Handler) adminPutModel(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		respondError(w, http.StatusServiceUnavailable, "model registry unavailable")
		return
	}
	var req modelRegistryPut
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if err := h.models.Set(req.Model, modelreg.Entry{RedirectModel: req.RedirectModel, Disabled: !enabled}); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(w, map[string]any{"model": req.Model})
}

// adminDeleteModel 实现 DELETE /admin/model-registry?model=：移除覆盖。
// 名字走查询参数而非路径段——模型名允许含 "/"，路径段装不下。
func (h *Handler) adminDeleteModel(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		respondError(w, http.StatusServiceUnavailable, "model registry unavailable")
		return
	}
	if err := h.models.Delete(r.URL.Query().Get("model")); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(w, map[string]any{"deleted": true})
}

// modelTestTimeout 是单次探活的上限：探针走真实上游往返，拉长到分钟级
// 才有意义地区分「上游慢」与「上游挂」；同时不能无限占住面板请求。
const modelTestTimeout = 60 * time.Second

// adminModelTest 实现 POST /admin/model-test {model}：经注入的进程内根路由
// 发一次真实 /v1/messages 探针（max_tokens=1，非流式），过完整鉴权/并发
// 闸门/注册表准入/重定向/别名/上游管线——与 ccLoad 渠道测试同语义，返回的
// 是真实往返结果而非配置静态检查。探针在 index.jsonl 里以
// client_request_id=panel-probe 留痕，request_id 回传供定位调试目录。
func (h *Handler) adminModelTest(w http.ResponseWriter, r *http.Request) {
	if h.probeHandler == nil {
		respondError(w, http.StatusServiceUnavailable, "model test unavailable")
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		respondError(w, http.StatusBadRequest, "model is required")
		return
	}
	key := ""
	if h.masterKeyFunc != nil {
		key = strings.TrimSpace(h.masterKeyFunc())
	}
	// 主密钥为空但令牌仓非空时探针没有可用明文凭据（仓里只存哈希）；
	// 全空即开放模式，占位凭据也能过 authenticate。
	if key == "" && h.tokens != nil && !h.tokens.Empty() {
		respondError(w, http.StatusBadRequest, "auth.api_key is empty and auth tokens exist: configure a master key to run probes")
		return
	}
	body, err := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"stream":     false,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), modelTestTimeout)
	defer cancel()
	probeReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body)).WithContext(probeCtx)
	if key == "" {
		key = "panel-probe"
	}
	probeReq.Header.Set("Authorization", "Bearer "+key)
	probeReq.Header.Set("Content-Type", "application/json")
	probeReq.Header.Set("X-Client-Request-Id", "panel-probe")
	rec := httptest.NewRecorder()
	started := time.Now()
	h.probeHandler.ServeHTTP(rec, probeReq)
	res := rec.Result()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	out := map[string]any{
		"ok":          res.StatusCode >= 200 && res.StatusCode < 300,
		"status_code": res.StatusCode,
		"latency_ms":  time.Since(started).Milliseconds(),
		"request_id":  res.Header.Get("X-Request-Id"),
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		out["error"] = probeErrorSummary(respBody)
	}
	respondOK(w, out)
}

// probeErrorSummary 从探针错误响应里摘一句可读原因：优先 error.message
// （Anthropic 形态错误体），摘不到就截原文前 200 字节。
func probeErrorSummary(body []byte) string {
	var shaped struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(body, &shaped); err == nil && shaped.Error.Message != "" {
		if shaped.Stage != "" {
			return shaped.Stage + ": " + shaped.Error.Message
		}
		return shaped.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
