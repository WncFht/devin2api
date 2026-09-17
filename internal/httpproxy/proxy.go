// 本文件提供代理 Transport 构建工具，支持 HTTP/HTTPS/SOCKS5 代理。
package httpproxy

import (
	"crypto/tls"
	"fmt"
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
	dnsFallback := true
	if proxyURL != "" {
		var err error
		if dnsFallback, err = applyProxy(base, proxyURL); err != nil {
			return nil, err
		}
	}
	// 直连与 HTTP 代理路径的 DialContext 都由本进程解析并拨号（目标主机
	// 或代理主机），挂解析缓存兜底：解析器故障窗内用最近成功解析的 IP 直拨。
	// SOCKS5 除外——见 applyProxy。
	if dnsFallback {
		base.DialContext = newDNSFallbackDialer(base.DialContext)
	}
	return base, nil
}

// applyProxy 把 proxyURL 落到 transport 上；支持 http://、https://、
// socks5://、socks5h:// 协议。返回 DialContext 拨号路径可否挂 DNS 兜底：
// 只有本进程自己做「解析主机名 + 建 TCP」的路径才适用。
func applyProxy(base *http.Transport, proxyURL string) (bool, error) {
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return false, fmt.Errorf("parse proxy URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
		// HTTP/HTTPS 代理可直接用 http.Transport.Proxy。
		base.Proxy = http.ProxyURL(parsed)
		return true, nil
	case "socks5", "socks5h":
		// SOCKS5 代理需要通过 golang.org/x/net/proxy 创建 dialer。
		dialer, err := proxy.FromURL(parsed, proxy.Direct)
		if err != nil {
			return false, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}
		// FromURL 产出的 socks.Dialer 必实现 ContextDialer；断言失败说明
		// 依赖行为变化，显式报错而不是回落到会泄漏协程的包装器。
		cd, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return false, fmt.Errorf("SOCKS5 dialer %T does not implement ContextDialer", dialer)
		}
		base.DialContext = cd.DialContext
		// socks 拨号器收到的是目标主机地址：目标主机名整段发给代理端解析，
		// 本进程只解析代理主机。此路径上挂 DNS 兜底，代理主机解析失败时
		// 会拿目标主机名做本地解析并裸 net.Dialer 直连目标——配了代理的
		// 流量在代理 DNS 故障窗内静默绕过代理。
		return false, nil
	default:
		return false, fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5, or socks5h)", scheme)
	}
}

func defaultTransport(forceHTTP1 bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// 提高连接池上限，减少“太多人同时使用”时的连接创建/回收压力。
	transport.MaxIdleConns = 2000
	transport.MaxIdleConnsPerHost = 200
	transport.IdleConnTimeout = 120 * time.Second
	transport.TLSHandshakeTimeout = 10 * time.Second
	// 仅限制等待响应头的时间，SSE 流本身不会被此超时打断；
	// 支持上游长时思考/排队，设置为 600 秒。
	transport.ResponseHeaderTimeout = 600 * time.Second
	transport.ExpectContinueTimeout = 1 * time.Second
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
