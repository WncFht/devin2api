//go:build darwin

package obs

import "syscall"

// rusageSample 返回进程累计 CPU 秒数（user+system）与峰值 RSS（字节）。
// darwin 的 ru_maxrss 单位是字节。
func rusageSample() (cpuSeconds float64, maxRSSBytes int64) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, 0
	}
	return timevalSeconds(usage.Utime) + timevalSeconds(usage.Stime), usage.Maxrss
}

// timevalSeconds 把 syscall.Timeval 转成秒（darwin 上是秒+微秒）。
func timevalSeconds(t syscall.Timeval) float64 {
	return float64(t.Sec) + float64(t.Usec)/1e6
}

// currentRSSBytes 返回瞬时 RSS（字节）。darwin 的瞬时口径要走 mach
// task_info(MACH_TASK_BASIC_INFO)，当前未接——返回 0 表示无数据，
// 上游字段以 0/unavailable 呈现而非伪造峰值。
func currentRSSBytes() int64 {
	return 0
}
