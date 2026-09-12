//go:build !darwin && !linux

package obs

// rusageSample 在不支持 getrusage 的平台返回零值；其余进程指标不受影响。
func rusageSample() (cpuSeconds float64, maxRSSBytes int64) {
	return 0, 0
}
