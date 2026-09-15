package ccpanel

import "net/http"

// publicVersion 实现 GET /public/version：页脚版本与更新提示数据源。
// 本服务没有内置更新检查器，has_update 恒 false。
func (h *Handler) publicVersion(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	respondOK(w, map[string]any{
		"version":        h.Version(),
		"has_update":     false,
		"latest_version": h.Version(),
		"release_url":    "",
	})
}

// publicProtocols 实现 GET /public/protocols：本服务入口协议固定三类。
func (h *Handler) publicProtocols(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	respondOK(w, []string{"anthropic", "codex", "openai"})
}
