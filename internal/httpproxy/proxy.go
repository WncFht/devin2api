// 本文件提供代理 Transport 构建工具，支持 HTTP/HTTPS/SOCKS5 代理。
package httpproxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

// NewTransport 根据 proxyURL 构建 RoundTripper。
// proxyURL 为空时返回针对高并发优化的 http.DefaultTransport Clone；
// 走系统环境变量代理时由 DefaultTransport 自行解析。
// forceHTTP1 为 true 时强制 HTTP/1.1，每请求独立 TCP 连接，
// 避免 HTTP/2 单连接多 stream 复用导致的上游并发瓶颈。
// 支持 http://、https://、socks5://、socks5h:// 协议。
func NewTransport(proxyURL string, forceHTTP1 bool) (*http.Transport, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	base := defaultTransport(forceHTTP1)
	if proxyURL == "" {
		return base, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
		// HTTP/HTTPS 代理可直接用 http.Transport.Proxy。
		base.Proxy = http.ProxyURL(parsed)
		return base, nil
	case "socks5", "socks5h":
		// SOCKS5 代理需要通过 golang.org/x/net/proxy 创建 dialer。
		dialer, err := proxy.FromURL(parsed, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}
		// 优先使用 DialContext，保证 HTTP/2、连接复用与 context 取消可传播。
		if cd, ok := dialer.(proxy.ContextDialer); ok {
			base.DialContext = cd.DialContext
		} else {
			base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialContext(ctx, dialer, network, addr)
			}
		}
		return base, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5, or socks5h)", scheme)
	}
}

// dialContext 将不支持 ContextDialer 的 Dialer 包装为可取消版本。
func dialContext(ctx context.Context, dialer interface {
	Dial(network, addr string) (net.Conn, error)
}, network, addr string) (net.Conn, error) {
	var (
		conn net.Conn
		err  error
	)
	done := make(chan struct{}, 1)
	go func() {
		conn, err = dialer.Dial(network, addr)
		close(done)
		if conn != nil && ctx.Err() != nil {
			conn.Close()
		}
	}()
	select {
	case <-ctx.Done():
		err = ctx.Err()
	case <-done:
	}
	return conn, err
}

func defaultTransport(forceHTTP1 bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 提高连接池上限，减少“太多人同时使用”时的连接创建/回收压力。
	transport.MaxIdleConns = 2000
	transport.MaxIdleConnsPerHost = 200
	transport.MaxConnsPerHost = 0
	transport.IdleConnTimeout = 120 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	// 仅限制等待响应头的时间，SSE 流本身不会被此超时打断；
	// 支持上游长时思考/排队，设置为 600 秒。
	transport.ResponseHeaderTimeout = 600 * time.Second
	transport.ExpectContinueTimeout = 1 * time.Second
	// 启用压缩，减少上行带宽占用。
	transport.DisableCompression = false
	if forceHTTP1 {
		// 强制 HTTP/1.1：每请求独立 TCP 连接（连接池复用空闲连接），
		// 避免 HTTP/2 单连接多 stream 复用被上游串行处理导致并发卡住。
		// 对齐 Devin 客户端多窗口各自独立连接的行为。
		transport.ForceAttemptHTTP2 = false
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{}
		}
		// ALPN 仅协商 http/1.1，确保不走 HTTP/2。
		transport.TLSClientConfig.NextProtos = []string{"http/1.1"}
	}
	return transport
}
