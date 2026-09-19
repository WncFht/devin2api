// 本文件是面板 JSON 端点的 gzip 中间件：/admin/usage、/admin/quota、
// /admin/logs/matrix 这类聚合端点单次响应数百 KB，LAN 传输也值得一压
// （实测 442KB→~40KB）。只在 /admin|/dashboard 前缀与客户端声明
// Accept-Encoding: gzip 时启用；?raw=1 的原样字节端点（附件保真、
// 自带 CSP sandbox）与响应头里已是流式/已编码的响应原样透传。
// 挂在面板子树注册侧（app.Router 里 Register 的 mux），/v1 的 SSE/WS
// 不经过它。放 app 包而非 ccpanel：ccPanel 以 PanelRegistrar 接口注入，
// app 不 import ccpanel。
package app

import (
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"sync"
)

// gzipWriterPool 复用面板响应的 gzip.Writer：flate 窗口与哈希表是
// 压缩路径的分配大头（store.payloadGzipPool 同构）。归还前
// Reset(io.Discard) 断开对响应 writer 的引用。
var gzipWriterPool = sync.Pool{
	New: func() any { return gzip.NewWriter(io.Discard) },
}

// gzipPanelMiddleware 压缩面板端点响应；压缩与否逐请求按路径与
// 响应头判定。
func gzipPanelMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !gzipApplies(r) {
			next.ServeHTTP(w, r)
			return
		}
		gzw := &gzipResponseWriter{ResponseWriter: w}
		defer gzw.Close()
		w.Header().Add("Vary", "Accept-Encoding")
		next.ServeHTTP(gzw, r)
	})
}

// gzipApplies 判定请求侧条件：面板 API 前缀、客户端接受 gzip、非 raw 字节直出。
func gzipApplies(r *http.Request) bool {
	p := r.URL.Path
	if !strings.HasPrefix(p, "/admin/") && !strings.HasPrefix(p, "/dashboard/") {
		return false
	}
	if r.URL.Query().Get("raw") == "1" {
		return false
	}
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

// gzipResponseWriter 把压缩决策推迟到首个 WriteHeader：handler 先写
// Content-Type，SSE/已编码响应据此透传不进 gzip。
type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	wroteHeader bool
	skip        bool
}

func (w *gzipResponseWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.wroteHeader = true
		ct := w.Header().Get("Content-Type")
		// SSE/已编码响应透传；无响应体的状态码（1xx/204/304）与重定向
		// 同样不装 gzip——空载荷挂 Content-Encoding 是畸形响应，
		// 重定向自带的迷你 HTML 也不值得一压。
		w.skip = strings.HasPrefix(ct, "text/event-stream") ||
			w.Header().Get("Content-Encoding") != "" ||
			code < http.StatusOK || code == http.StatusNoContent ||
			code == http.StatusNotModified ||
			(code >= http.StatusMultipleChoices && code < http.StatusBadRequest)
		if !w.skip {
			w.gz = gzipWriterPool.Get().(*gzip.Writer)
			w.gz.Reset(w.ResponseWriter)
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Del("Content-Length")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.skip {
		return w.ResponseWriter.Write(b)
	}
	return w.gz.Write(b)
}

// Flush 保住透传路径与压缩路径两侧的流式语义：未来 SSE 端点进面板
// 子树时不因 middleware 丢失 http.Flusher 能力。
func (w *gzipResponseWriter) Flush() {
	_ = w.FlushError()
}

// FlushError 透出错误返回版 Flush：与 gateHeaderWriter 同理，
// ResponseController 在包装层命中 Flusher 便不再走 Unwrap 链，
// 缺它则内层传输错误到不了调用方。
func (w *gzipResponseWriter) FlushError() error {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if !w.skip {
		if err := w.gz.Flush(); err != nil {
			return err
		}
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap 透出内层 writer：与 gateHeaderWriter 同理，ResponseController
// 的链上能力探测（SetWriteDeadline/FlushError/Hijack）靠它穿透包装层。
func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Close 仅在真正压缩时收尾 gzip 流——透传路径不能向连接写 gzip 帧头。
// Close 先把残帧刷进响应再 Reset 脱钩归还：顺序反了残帧会写进
// io.Discard 丢尾。
func (w *gzipResponseWriter) Close() {
	if w.gz != nil {
		_ = w.gz.Close()
		w.gz.Reset(io.Discard)
		gzipWriterPool.Put(w.gz)
		w.gz = nil
	}
}
