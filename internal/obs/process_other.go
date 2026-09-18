//go:build !darwin && !linux && !windows

package obs

// rusageSample 在不支持 getrusage 且没有专用实现（见 process_windows.go）
// 的平台返回零值；其余进程指标不受影响。
func rusageSample() (cpuSeconds float64, maxRSSBytes int64) {
	return 0, 0
}

// currentRSSBytes 在没有 /proc 或等效接口的平台返回 0（无瞬时数据源）。
func currentRSSBytes() int64 {
	return 0
}
