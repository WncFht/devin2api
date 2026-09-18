// 本文件实现 X-Gate-* 响应头的统一写出：闸门放行回执由泵协程在
// wait() 内经 GateContext 原子指针回填，本包装层在响应首字节提交前
// 读取快照 stamp 成头——流式 SSE、非流式与错误路径共用同一时机，
// 不需要各写出点各自补头。
package app

import (
	"net/http"
	"strconv"

	"github.com/WncFht/devin2api/internal/adapter"
)

// gateHeaderWriter 包装下游响应写出器：WriteHeader 时把请求类与闸门
// 放行快照（若已产生）写进响应头。X-Gate-Class 恒在——未过闸门的
// 准入拒绝（令牌并发/费用/模型）也回显分类；Window-*/Wait-Ms 只在
// 真实过闸后出现（闸门快败无快照，归因走 X-Gate-Reason，由
// writeLoggedError 从 llm.Failure.GateReason 写入）。
// 首字节时机晚于一切放行路径：保活帧在上游建流后才武装（见
// stream.go 的 upstreamOpen 门控），闸内排队期间没有任何字节写出，
// 快照 stamp 不会赶在放行之前提交。
type gateHeaderWriter struct {
	http.ResponseWriter
	gate *adapter.GateContext
	// wrote 记录头是否已提交：net/http 对重复 WriteHeader 逐条打
	// superfluous 日志——流式路径每帧 Flush 都会触发一次，stderr 写锁
	// 会把全部在飞请求串行（实测单这条日志吃掉大半吞吐）。stamp 只在
	// 首个字节提交前做一次，之后 Write/Flush 不再碰 WriteHeader。
	wrote bool
}

func (w *gateHeaderWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	h := w.Header()
	h.Set("X-Gate-Class", w.gate.Class)
	if v := w.gate.Verdict(); v != nil {
		if v.Lane != "" {
			h.Set("X-Gate-Lane", v.Lane)
		}
		h.Set("X-Gate-Window-Used", strconv.Itoa(v.WindowUsed))
		h.Set("X-Gate-Window-Quota", strconv.Itoa(v.WindowQuota))
		h.Set("X-Gate-Window-Reset", strconv.Itoa(v.WindowResetSec))
		h.Set("X-Gate-Wait-Ms", strconv.FormatInt(v.WaitMS, 10))
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *gateHeaderWriter) Write(b []byte) (int, error) {
	// net/http 对裸 Write 隐式提交 200——显式走 WriteHeader 保证
	// stamp 逻辑不被绕过；wrote 闸保证重复 Write/Flush 幂等。
	w.WriteHeader(http.StatusOK)
	return w.ResponseWriter.Write(b)
}

// Flush 透出 http.Flusher：streamCompletion 与非流式心跳的类型断言
// 经包装层后仍成立。
func (w *gateHeaderWriter) Flush() {
	_ = w.FlushError()
}

// FlushError 透出错误返回版 Flush：ResponseController.Flush 在包装层命中
// Flusher 便不再走 Unwrap 链，缺它则内层 *http.response.FlushError 的
// 传输错误（conn.werr）到不了写路径调用方，<4KB 载荷的 socket 失败被吞。
func (w *gateHeaderWriter) FlushError() error {
	w.WriteHeader(http.StatusOK)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap 透出内层 writer：http.ResponseController 沿包装链找 conn 级能力
// （SetWriteDeadline/FlushError/Hijack），缺它则链到 *http.response 断掉，
// SSE 逐写 deadline 落不下去。
func (w *gateHeaderWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
