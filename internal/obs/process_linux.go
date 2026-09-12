//go:build linux

package obs

import "syscall"

// rusageSample 返回进程累计 CPU 秒数（user+system）与峰值 RSS（字节）。
// linux 的 ru_maxrss 单位是 KiB。
func rusageSample() (cpuSeconds float64, maxRSSBytes int64) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0
	}
	return timevalSeconds(usage.Utime) + timevalSeconds(usage.Stime), usage.Maxrss * 1024
}

// timevalSeconds 把 syscall.Timeval 转成秒。
func timevalSeconds(t syscall.Timeval) float64 {
	return float64(t.Sec) + float64(t.Usec)/1e6
}
