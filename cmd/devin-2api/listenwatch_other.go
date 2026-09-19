//go:build !linux

package main

import (
	"context"

	"github.com/WncFht/devin2api/internal/obs"
)

// watchListenOwnership 的 /proc 反查实现是 Linux 专属（见
// listenwatch_linux.go）；其它平台无对等低成本数据源，交接部署
// 也只在 systemd 侧存在，这里是编译兜底。
func watchListenOwnership(context.Context, int, *obs.Metrics) {}
