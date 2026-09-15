package ccpanel

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"

	"github.com/go-chi/chi/v5"
)

// serveStatic 下发 web/ 内嵌的移植面板资源。
// .html 替换 __VERSION__ 且不缓存（版本号引用必须新鲜）；
// 其余资源带 ?v= 版本戳，给 ETag + no-cache 让浏览器再验证即可——
// 内容随二进制固定，不追求 immutable 长缓存。
func (h *Handler) serveStatic(w http.ResponseWriter, r *http.Request) {
	name := path.Clean(strings.TrimPrefix(chi.URLParam(r, "*"), "/"))
	if name == "." || name == "" {
		name = "index.html"
	}
	if name == ".." || strings.HasPrefix(name, "../") || strings.HasPrefix(name, "/") {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	info, err := fs.Stat(webFS, "web/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if info.IsDir() {
		name = path.Join(name, "index.html")
		if _, err := fs.Stat(webFS, "web/"+name); err != nil {
			http.NotFound(w, r)
			return
		}
	}
	body, err := fs.ReadFile(webFS, "web/"+name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".html") {
		body = []byte(strings.ReplaceAll(string(body), "__VERSION__", h.Version()))
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		_, _ = w.Write(body)
		return
	}
	entryV, ok := h.staticEntries.Load(name)
	entry, _ := entryV.(*staticEntry)
	if !ok {
		sum := sha256.Sum256(body)
		entry = &staticEntry{etag: `"` + hex.EncodeToString(sum[:16]) + `"`}
		h.staticEntries.Store(name, entry)
	}
	w.Header().Set("Content-Type", ccContentType(name))
	w.Header().Set("Cache-Control", "public, no-cache")
	w.Header().Set("ETag", entry.etag)
	if r.Header.Get("If-None-Match") == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

type staticEntry struct {
	etag string
}

// ccContentType 按扩展名给移植资源定 MIME。
func ccContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".css":
		return "text/css; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}
