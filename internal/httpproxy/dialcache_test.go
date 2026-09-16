package httpproxy

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeConn 是拨号桩的最小 net.Conn 实现——测试只关心有没有拨通。
type fakeConn struct{ net.Conn }

func (fakeConn) Close() error { return nil }

// dnsError 构造 net.DNSError 形态的解析失败。
func dnsError() error {
	return &net.DNSError{Err: "no such host", IsNotFound: true}
}

// 正常路径透传：inner 成功即返回，且触发一次异步缓存填充。
func TestDNSFallbackSuccessRefreshesCache(t *testing.T) {
	lookuped := make(chan string, 1)
	dialer := &dnsFallbackDialer{
		inner: func(ctx context.Context, network, address string) (net.Conn, error) {
			return fakeConn{}, nil
		},
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			lookuped <- host
			return []net.IP{net.ParseIP("1.2.3.4")}, nil
		},
		entries: map[string]dnsCacheEntry{},
	}
	conn, err := dialer.dial(context.Background(), "tcp", "example.com:443")
	if err != nil || conn == nil {
		t.Fatalf("dial error = %v, conn = %v", err, conn)
	}
	select {
	case host := <-lookuped:
		if host != "example.com" {
			t.Fatalf("lookup host = %q", host)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cache refresh lookup did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ips := dialer.cached("example.com"); len(ips) == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("cache not populated after successful dial")
}

// 解析失败且缓存有货：inline 重查也失败时拨缓存 IP。
func TestDNSFallbackDialsCachedIP(t *testing.T) {
	var dialed []string
	dialer := &dnsFallbackDialer{
		inner: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, dnsError()
		},
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			return nil, dnsError() // 解析器仍在故障窗
		},
		dialIP: func(ctx context.Context, network, address string) (net.Conn, error) {
			dialed = append(dialed, address)
			return fakeConn{}, nil
		},
		entries: map[string]dnsCacheEntry{
			"example.com": {ips: []net.IP{net.ParseIP("1.2.3.4")}, fetchedAt: time.Now()},
		},
	}
	conn, err := dialer.dial(context.Background(), "tcp", "example.com:443")
	if err != nil || conn == nil {
		t.Fatalf("fallback dial error = %v", err)
	}
	if len(dialed) != 1 || dialed[0] != "1.2.3.4:443" {
		t.Fatalf("dialed = %v, want [1.2.3.4:443]", dialed)
	}
}

// 解析失败但 inline 重查成功：用新鲜答案并回填缓存。
func TestDNSFallbackPrefersFreshLookup(t *testing.T) {
	dialer := &dnsFallbackDialer{
		inner: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, dnsError()
		},
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("5.6.7.8")}, nil
		},
		dialIP: func(ctx context.Context, network, address string) (net.Conn, error) {
			if !strings.HasPrefix(address, "5.6.7.8:") {
				t.Fatalf("fallback dialed %q, want fresh IP", address)
			}
			return fakeConn{}, nil
		},
		entries: map[string]dnsCacheEntry{},
	}
	if _, err := dialer.dial(context.Background(), "tcp", "example.com:443"); err != nil {
		t.Fatalf("dial error = %v", err)
	}
	if ips := dialer.cached("example.com"); len(ips) != 1 || ips[0].String() != "5.6.7.8" {
		t.Fatalf("cache = %v, want fresh lookup result", ips)
	}
}

// 缓存空且重查失败：原错误原样返回，兜底不编造新失败形态。
func TestDNSFallbackReturnsOriginalError(t *testing.T) {
	dialer := &dnsFallbackDialer{
		inner: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, dnsError()
		},
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			return nil, dnsError()
		},
		entries: map[string]dnsCacheEntry{},
	}
	_, err := dialer.dial(context.Background(), "tcp", "example.com:443")
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) || !dnsErr.IsNotFound {
		t.Fatalf("dial error = %v, want original *net.DNSError", err)
	}
}

// 非 DNS 错误（如连接被拒）不进兜底路径。
func TestDNSFallbackIgnoresNonDNSError(t *testing.T) {
	dialer := &dnsFallbackDialer{
		inner: func(ctx context.Context, network, address string) (net.Conn, error) {
			return nil, errors.New("connection refused")
		},
		lookupIP: func(ctx context.Context, host string) ([]net.IP, error) {
			t.Fatal("lookupIP should not run for non-DNS errors")
			return nil, nil
		},
		entries: map[string]dnsCacheEntry{},
	}
	_, err := dialer.dial(context.Background(), "tcp", "example.com:443")
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("dial error = %v, want passthrough of non-DNS error", err)
	}
}
