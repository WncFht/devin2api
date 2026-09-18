//go:build linux

package obs

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

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

// currentRSSBytes 返回瞬时 RSS（字节），读 /proc/self/statm——ru_maxrss 是
// 只涨不降的峰值水印，画成曲线像泄漏；本字段随真实占用起伏。读取失败返回
// 0（/proc 受限时无数据可报，不伪造）。
func currentRSSBytes() int64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	return statmResidentBytes(string(data))
}

// statmResidentBytes 解析 statm 行：第 2 列是常驻页数（与 VmRSS 同口径），
// 乘页大小换成字节；畸形输入返回 0。
func statmResidentBytes(line string) int64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * int64(os.Getpagesize())
}
