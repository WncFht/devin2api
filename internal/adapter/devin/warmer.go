package devin

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// 直连上游路径上每次新建 TCP+TLS 握手实测 0.6-1.4s，而上游/链路对 idle
// 连接的存活窗口约 30-60s。connWarmer 以 25s 为周期给 transport 的 idle
// 池补焐，并在每个真实发送入口 kick 一发补位——让建流（sent→open）
// 基本落在已焐好的连接上，省掉整段握手。焐法是对 baseURL 发一个会快速
// 404 的 GET：响应体读空后连接回 idle 池，真实请求拿到即复用。
const (
	warmInterval  = 25 * time.Second
	warmTarget    = 6 // 保底空闲连接数；突发由入口 kick 追加
	warmOpTimeout = 15 * time.Second
	warmKickCap   = 32 // kick 通道容量与单批补焐上限
)

// connWarmer 维护上游 idle 连接池。与客户端共享同一个 *http.Transport——
// idle 池长在 transport 上，warm 用的 RoundTripper 必须就是它。
type connWarmer struct {
	transport *http.Transport
	baseURL   string
	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// newConnWarmer 启动后台焐池协程；Close 后协程退出。
func newConnWarmer(transport *http.Transport, baseURL string) *connWarmer {
	w := &connWarmer{
		transport: transport,
		baseURL:   baseURL,
		kick:      make(chan struct{}, warmKickCap),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go w.run()
	return w
}

// Close 停掉焐池协程并等待其退出；可重入——link 退役（finishConfigApply）
// 与 adapter 收尾可能先后调到同一个 warmer。
func (w *connWarmer) Close() {
	w.closeOnce.Do(func() { close(w.stop) })
	<-w.done
}

// kick 由真实发送路径调用：每来一个上游调用补一条焐连接，
// 池深因此自动跟随并发需求；非阻塞，打满即丢。
func (w *connWarmer) kickRequest() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

func (w *connWarmer) run() {
	defer close(w.done)
	tick := time.NewTicker(warmInterval)
	defer tick.Stop()
	w.warmPool(warmTarget) // 冷启动先焐满保底数
	for {
		select {
		case <-w.stop:
			return
		case <-tick.C:
			w.warmPool(warmTarget)
		case <-w.kick:
			// 攒批：把积压的 kick 一次合并成一轮并发补焐
			n := 1
		drain:
			for n < warmKickCap {
				select {
				case <-w.kick:
					n++
				default:
					break drain
				}
			}
			w.warmPool(n)
		}
	}
}

// warmPool 并发发 n 个廉价 GET——并发是关键：串行会反复复用同一条连接，
// 只有并发占线才迫使 transport 拨出新连接留在池里。
func (w *connWarmer) warmPool(n int) {
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.warmOnce()
		}()
	}
	// Close 的语义是不再发起新一轮，在途 warmOnce 随自己 15s 的 ctx
	// 收尾——stop 到了就不必再等本轮 wg，否则关停平白多卡一拍。
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-w.stop:
	}
}

// warmOnce 打一发 GET 让连接落进 idle 池；读空 body 是复用的前提。
// 焐失败（对端/链路故障）静默丢弃——它只是预热，失败不改变任何行为。
func (w *connWarmer) warmOnce() {
	ctx, cancel := context.WithTimeout(context.Background(), warmOpTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.baseURL+"/", nil)
	if err != nil {
		return
	}
	resp, err := w.transport.RoundTrip(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
