// 本文件给上游拨号路径挂「最后成功解析」缓存。系统解析器偶发整窗失败
// （实测 "lookup server.codeium.com: no such host" 成簇出现于同一
// 时段，重试期间持续不可用）——有缓存时直接拨已解析 IP 兜底。
// 兜底只替换 TCP 对端地址：TLS 的 SNI 与证书校验收 URL host，dial 到
// 哪个 IP 不影响身份验证语义。
package httpproxy

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// dialFunc 是 net.Dialer.DialContext 的函数形态。
type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

const (
	// dnsCacheRefresh 是解析缓存的最小刷新间隔：正常拨号成功且缓存
	// 陈旧时异步重解析一次，故障期的兜底集始终保持近新鲜。
	dnsCacheRefresh = 5 * time.Minute
	// dnsFallbackMaxIPs 是兜底路径最多尝试的缓存地址数。
	dnsFallbackMaxIPs = 3
	// dnsRedialTimeout 是解析失败时 inline 重查一次解析器的时限：
	// 整窗失败时该调用大概率同样超时，给一个短预算避免故障期
	// 每次拨号都背上完整的 resolver 超时。
	dnsRedialTimeout = 3 * time.Second
)

// dnsFallbackDialer 包装一个拨号函数：正常路径原样透传，DNS 解析
// 失败时先 inline 重查一次解析器，再失败则拨本 host 最近一次成功
// 解析出的 IP 集。缓存由成功拨号后的异步重解析填充。
type dnsFallbackDialer struct {
	inner dialFunc
	// lookupIP 做 host 解析（默认 net.DefaultResolver）；dialIP 做兜底
	// 直拨——两者是字段而非直接调包级函数，只为测试可注入桩。
	lookupIP func(ctx context.Context, host string) ([]net.IP, error)
	dialIP   dialFunc
	mu       sync.Mutex
	entries  map[string]dnsCacheEntry
}

// dnsCacheEntry 是某 host 最近一次成功解析的结果与刷新状态；
// refreshing 期间保留旧 ips——刷新窗内的 DNS 故障仍能拿旧集兜底。
type dnsCacheEntry struct {
	ips        []net.IP
	fetchedAt  time.Time
	refreshing bool
}

// newDNSFallbackDialer 返回包装后的拨号函数；inner 为 nil 时以
// net.Dialer 默认值兜底。
func newDNSFallbackDialer(inner dialFunc) dialFunc {
	if inner == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		inner = dialer.DialContext
	}
	fallback := &dnsFallbackDialer{
		inner:    inner,
		lookupIP: defaultLookupIP,
		dialIP:   defaultDialIP,
		entries:  map[string]dnsCacheEntry{},
	}
	return fallback.dial
}

// defaultLookupIP 是生产路径的解析器入口。
func defaultLookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// defaultDialIP 是生产路径的兜底直拨：超时对齐 net.Dialer 默认值。
// SNI/证书校验由 transport 按 URL host 完成，与拨的对端地址无关。
func defaultDialIP(ctx context.Context, network, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return dialer.DialContext(ctx, network, address)
}

// dial 先走原拨号路径；成功则按需异步刷新解析缓存。失败且错误属于
// DNS 解析族时进入兜底：inline 短时限重查 → 缓存 IP 直拨。
func (dialer *dnsFallbackDialer) dial(ctx context.Context, network, address string) (net.Conn, error) {
	conn, err := dialer.inner(ctx, network, address)
	if err == nil {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr == nil {
			dialer.refreshIfStale(host)
		}
		return conn, nil
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		return nil, err
	}
	host, port, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		return nil, err
	}
	// 解析失败先短时限重查：故障窗可能已结束，新鲜答案优于缓存。
	if ips := dialer.lookupFresh(host); len(ips) > 0 {
		if conn, dialErr := dialer.dialFirstIP(ctx, network, port, ips); dialErr == nil {
			return conn, nil
		}
	}
	if ips := dialer.cached(host); len(ips) > 0 {
		if conn, dialErr := dialer.dialFirstIP(ctx, network, port, ips); dialErr == nil {
			return conn, nil
		}
	}
	return nil, err
}

// refreshIfStale 在缓存缺席或陈旧时发起一次异步重解析；refreshing
// 标记做并发去重，失败则退避到下一个刷新周期再试。
func (dialer *dnsFallbackDialer) refreshIfStale(host string) {
	dialer.mu.Lock()
	entry := dialer.entries[host]
	if entry.refreshing || time.Since(entry.fetchedAt) < dnsCacheRefresh {
		dialer.mu.Unlock()
		return
	}
	entry.refreshing = true
	dialer.entries[host] = entry
	dialer.mu.Unlock()
	go func() {
		lookupCtx, cancel := context.WithTimeout(context.Background(), dnsRedialTimeout)
		defer cancel()
		ips, err := dialer.lookupIP(lookupCtx, host)
		dialer.mu.Lock()
		defer dialer.mu.Unlock()
		entry := dialer.entries[host]
		entry.refreshing = false
		if err == nil && len(ips) > 0 {
			entry.ips = ips
		}
		// 失败也推进 fetchedAt——否则故障期每个成功拨号都叠发一次
		// 注定超时的重查。
		entry.fetchedAt = time.Now()
		dialer.entries[host] = entry
	}()
}

// lookupFresh inline 重查一次解析器；成功即回填缓存。
func (dialer *dnsFallbackDialer) lookupFresh(host string) []net.IP {
	lookupCtx, cancel := context.WithTimeout(context.Background(), dnsRedialTimeout)
	defer cancel()
	ips, err := dialer.lookupIP(lookupCtx, host)
	if err != nil || len(ips) == 0 {
		return nil
	}
	dialer.mu.Lock()
	dialer.entries[host] = dnsCacheEntry{ips: ips, fetchedAt: time.Now()}
	dialer.mu.Unlock()
	return ips
}

// cached 返回该 host 的缓存 IP 集；不设过期——故障期陈旧地址仍比
// 无地址强，拨不通时错误原样返回。
func (dialer *dnsFallbackDialer) cached(host string) []net.IP {
	dialer.mu.Lock()
	defer dialer.mu.Unlock()
	return dialer.entries[host].ips
}

// dialFirstIP 依次拨缓存 IP（上限 dnsFallbackMaxIPs），返回首个成功连接。
func (dialer *dnsFallbackDialer) dialFirstIP(ctx context.Context, network, port string, ips []net.IP) (net.Conn, error) {
	var lastErr error
	for _, ip := range ips[:min(len(ips), dnsFallbackMaxIPs)] {
		conn, err := dialer.dialIP(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}
