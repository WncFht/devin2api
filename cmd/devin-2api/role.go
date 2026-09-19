// 本文件定义进程角色门禁：凡交接进程（handoff）不跑、托管实例才跑的
// 后台职责都经它登记，跳过判据与理由只写在这里一次。
package main

import (
	"log/slog"
	"os"
)

// procRole 区分进程身份：managed 是常驻托管实例；handoff 是 deploy 交接期
// 的短命占位（scripts/deploy/lib-deploy.sh spawn_handoff 注入 DEVIN2API_HANDOFF，
// 在 reuseport 队列里接住端口，旧实例排空、托管实例拉起之间新连接真实
// 落在它身上）。交接进程照常装配请求服务路径，但一切后台维护、一次性
// 播种与周期看门狗归托管实例——两进程并发做同一份维护只会重复打上游、
// 重复写库或在共享库的 UNIQUE 约束上互相打断。
type procRole struct{ handoff bool }

// roleFromEnv 按 spawn_handoff 注入的 env 判定本进程角色。
func roleFromEnv() procRole {
	return procRole{handoff: os.Getenv("DEVIN2API_HANDOFF") != ""}
}

// duty 执行一项托管职责；交接进程记一条跳过明细后返回。后台职责一律经
// 它登记——新增组件不存在「忘加门禁」的写法。错误的严重级别（fatal 退
// 出还是 warn 放过）留在 fn 内部：那是职责自己的语义，门禁只管身份。
func (r procRole) duty(name string, fn func()) {
	if r.handoff {
		slog.Debug("handoff process skips managed duty", "duty", name)
		return
	}
	fn()
}

// spawn 与 duty 同门禁，fn 放到新 goroutine 里跑。
func (r procRole) spawn(name string, fn func()) {
	r.duty(name, func() { go fn() })
}
