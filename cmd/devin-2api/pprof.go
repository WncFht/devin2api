// 本文件提供按需开启的 pprof/fgprof 调试监听器。
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/felixge/fgprof"
)

// pprof 监听的可变状态：启动与配置 reload 共用 applyPprofListen 换绑。
// pprofAddr 记的是当前实际绑定的地址而非配置值——上次 bind 失败
// （addr 已写日志、server 为 nil）时同值 reload 会重试，瞬时
// EADDRINUSE 可自愈。
var (
	pprofMu     sync.Mutex
	pprofServer *http.Server
	pprofAddr   string
)

// applyPprofListen 把 pprof 监听状态对齐到目标地址；bind 失败不致命
// （与启动/reload 语义一致，错误已写日志）。面板设置页需要显式成败，
// 走 rebindPprof 拿错误返回。
func applyPprofListen(addr string) {
	_ = rebindPprof(addr)
}

// rebindPprof 换绑 pprof 监听：与已绑地址相同 no-op；addr 非空先 bind
// 新地址——bind 失败旧 listener 保住不掉线（面板 PUT 拿错误返回时
// 当前剖析端点仍可用）；bind 成功才 Close 旧 server 并归还进程级
// 采样开关，再挂上新 listener。
func rebindPprof(addr string) error {
	pprofMu.Lock()
	defer pprofMu.Unlock()
	if addr == pprofAddr {
		return nil
	}
	var listener net.Listener
	if addr != "" {
		// 跟随主监听同一 reuseport 开关：reuseport 交接部署时旧进程最长
		// 300s 排空期内仍占着 pprof 口，不叠加 REUSEPORT 会让新实例终身
		// 失去剖析端点（bind 失败后无人重试）。交接窗口内请求可能落到
		// 任一进程——排障时多看一眼 pid 即可。
		lc := &net.ListenConfig{KeepAlive: 3 * time.Minute}
		if reusePortEnabled() {
			lc.Control = func(_, _ string, c syscall.RawConn) error {
				return setReusePort(c)
			}
		}
		l, err := lc.Listen(context.Background(), "tcp", addr)
		if err != nil {
			slog.Error("pprof listen failed", "addr", addr, "error", err)
			return fmt.Errorf("pprof listen on %q: %w", addr, err)
		}
		listener = l
	}
	if pprofServer != nil {
		_ = pprofServer.Close()
		pprofServer = nil
		pprofAddr = ""
		// block/mutex 采样率是开 listener 时配的进程级开关——listener
		// 关了要把采样还回去，否则白留开销却没有出口。
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	}
	if listener == nil {
		return nil
	}
	pprofServer = servePprof(addr, listener)
	pprofAddr = addr
	return nil
}

// currentPprofListen 返回当前实际绑定的 pprof 监听地址（空=未启用），
// 供面板设置页回读生效值。
func currentPprofListen() string {
	pprofMu.Lock()
	defer pprofMu.Unlock()
	return pprofAddr
}

// servePprof 在已 bind 的 listener 上暴露 Go 运行时剖析端点并开服。
// 与主监听分离的原因：pprof 端点无鉴权，回环地址是唯一信任边界——
// 配置应绑 127.0.0.1，跨机访问走 ssh 端口转发而不是放开监听。
// block/mutex 采样率只在侦听器开启时设置：两者带持续开销，
// 默认关闭保持生产路径干净。
func servePprof(addr string, listener net.Listener) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	mux.Handle("/debug/pprof/allocs", pprof.Handler("allocs"))
	mux.Handle("/debug/pprof/block", pprof.Handler("block"))
	mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
	mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
	// fgprof 采 wall-clock：内置 CPU profile 只看 on-CPU，对 I/O 密集型
	// 代理会漏掉 socket/channel 等待——这里补上 off-CPU 那一半。
	mux.Handle("/debug/fgprof", fgprof.Handler())

	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// 阻塞剖析按「每累计 1ms 阻塞采样一次」取——代理的阻塞大头是
	// channel/锁等待，1ms 粒度足够覆盖锁竞争又不过度采样。放在成功
	// bind 之后：失败路径不该留着采样开销却没有出口。
	runtime.SetBlockProfileRate(int(time.Millisecond))
	runtime.SetMutexProfileFraction(10)
	slog.Info("pprof endpoints listening", "addr", "http://"+addr+"/debug/pprof/")
	go func() { _ = server.Serve(listener) }()
	return server
}
