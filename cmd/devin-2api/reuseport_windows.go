//go:build windows

package main

import "syscall"

// Windows 无 SO_REUSEPORT 语义（x/sys/unix 不支持该平台），交接部署也只在
// launchd/systemd 侧存在——reusePortEnabled 恒 false，裸 exe 行为不变。
const reusePortSupported = false

// setReusePort 在 Windows 上不可达（reusePortSupported=false 拦住调用方），
// 仅为跨平台编译存在。
func setReusePort(syscall.RawConn) error { return nil }
