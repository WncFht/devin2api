package ccpanel

import (
	"encoding/json"
	"net/http"
	"sort"

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
