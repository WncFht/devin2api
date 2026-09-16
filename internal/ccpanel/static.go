package ccpanel

import (
	"bytes"
	"compress/gzip"
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
		if len(body) >= 1024 {
			entry.gz = gzipBody(body)
		}
		h.staticEntries.Store(name, entry)
	}
	w.Header().Set("Content-Type", ccContentType(name))
	w.Header().Set("Cache-Control", "public, no-cache")
	w.Header().Set("ETag", entry.etag)
	// 响应体随客户端 Accept-Encoding 变体——无论 200 还是 304 都要声明。
	w.Header().Set("Vary", "Accept-Encoding")
	if r.Header.Get("If-None-Match") == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if entry.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(entry.gz)
		return
	}
	_, _ = w.Write(body)
}

// staticEntry 缓存资源名 → {etag, gzip 预压缩体}：内容随二进制固定，
// 按名惰性算一次。gzip 只服务 ≥1KB 的资源：echarts 这类大体积依赖
// 经 tailnet 远程访问面板时差距明显；小于阈值时压缩头开销比省的字节还多。
type staticEntry struct {
	etag string
	gz   []byte // nil 表示不值得压缩
}

// gzipBody 预压缩静态体；失败返回 nil（调用方按不压缩处理）。
func gzipBody(body []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(body); err != nil {
		return nil
	}
	if err := gw.Close(); err != nil {
		return nil
	}
	return buf.Bytes()
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
