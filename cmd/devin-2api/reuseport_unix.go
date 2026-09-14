//go:build !windows

package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// reusePortSupported 标记平台支持 SO_REUSEPORT——交接部署只在
// launchd/systemd（macOS/Linux）侧存在，见 lib-deploy.sh 的注释。
const reusePortSupported = true

// setReusePort 在 bind 前给监听 socket 置 SO_REUSEPORT，供
// net.ListenConfig.Control 调用。
func setReusePort(c syscall.RawConn) error {
	var setErr error
	if err := c.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return setErr
}
