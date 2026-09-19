//go:build !windows

package selfupdate

import (
	"errors"
	"syscall"
)

// terminateProcess 向 pid 发优雅终止信号——SIGTERM 触发目标进程的
// 排空路径（托管实例与编排进程都实现了 SIGTERM 优雅退出）。
func terminateProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

// detachSysProcAttr 让编排子进程脱离调用方的进程组/控制终端——macOS
// 需要 setsid 逃出 launchd job 组，防止托管实例 restart 时把编排
// 进程一并带走；Linux 侧编排经 systemd-run 起在瞬态 unit 里不走这里。
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// pidAlive 报告 pid 是否对应活进程（EPERM 视为存在——别人的进程
// 也不该回收，按活着处理最保守）。
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
