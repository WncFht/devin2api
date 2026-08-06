// 本文件提供代理 Transport 构建工具，支持 HTTP/HTTPS/SOCKS5 代理。
package httpproxy

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/proxy"
)

// NewTransport 根据 proxyURL 构建 RoundTripper。
// proxyURL 为空时返回 http.DefaultTransport（走系统环境变量）。
// 支持 http://、https://、socks5://、socks5h:// 协议。
func NewTransport(proxyURL string) (http.RoundTripper, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return http.DefaultTransport, nil
	}
	parsed, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "http", "https":
		// HTTP/HTTPS 代理可直接用 http.Transport.Proxy。
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = http.ProxyURL(parsed)
		return transport, nil
	case "socks5", "socks5h":
		// SOCKS5 代理需要通过 golang.org/x/net/proxy 创建 dialer。
		dialer, err := proxy.FromURL(parsed, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("create SOCKS5 dialer: %w", err)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Dial = dialer.Dial
		return transport, nil
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q (use http, https, socks5, or socks5h)", scheme)
	}
}
