//go:build windows

package selfupdate

import (
	"os"
	"syscall"
)

// Windows 没有托管形态（spawnOrchestrator 对非 linux/darwin 一律
// ErrUnsupported，自更新端点整族 501），这三个同名列只是编译占位——
// 真实调用链不可达，实现按最合理语义给，不追求与 Unix 等价的信号语义。

func terminateProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer func() { _ = p.Release() }()
	return p.Kill()
}

func detachSysProcAttr() *syscall.SysProcAttr {
	return nil
}

func pidAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
