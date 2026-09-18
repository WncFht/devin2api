//go:build windows

package obs

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// vmCountersEx 对应 winternl.h 的 VM_COUNTERS_EX（x/sys 未导出该结构）；
// 只关心 PeakWorkingSetSize，其余字段保留以维持正确的内存布局。
type vmCountersEx struct {
	peakVirtualSize            uintptr
	virtualSize                uintptr
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
	privateUsage               uintptr
}

// filetime100ns 把 FILETIME 还原为原始 100ns 计数。GetProcessTimes 返回的
// kernel/user 是时长而非时刻，不能用 Filetime.Nanoseconds()——它会把
// 1601→1970 的纪元偏移也减进去（小数值直接溢出成负数）。
func filetime100ns(ft windows.Filetime) int64 {
	return int64(ft.HighDateTime)<<32 | int64(ft.LowDateTime)
}

// queryVMCounters 取回本进程的 VM_COUNTERS_EX；峰值与瞬时工作集共用。
func queryVMCounters() (vmCountersEx, error) {
	var counters vmCountersEx
	var retLen uint32
	err := windows.NtQueryInformationProcess(windows.CurrentProcess(), windows.ProcessVmCounters, unsafe.Pointer(&counters), uint32(unsafe.Sizeof(counters)), &retLen)
	return counters, err
}

// rusageSample 返回进程累计 CPU 秒数（kernel+user）与峰值工作集（字节）。
// Windows 没有 getrusage：CPU 走 GetProcessTimes，峰值 RSS 走
// NtQueryInformationProcess(ProcessVmCounters)。
func rusageSample() (cpuSeconds float64, maxRSSBytes int64) {
	handle := windows.CurrentProcess()
	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return 0, 0
	}
	cpuSeconds = float64(filetime100ns(kernel)+filetime100ns(user)) / 1e7
	counters, err := queryVMCounters()
	if err != nil {
		return cpuSeconds, 0
	}
	return cpuSeconds, int64(counters.peakWorkingSetSize)
}

// currentRSSBytes 返回瞬时工作集（字节），与峰值同出一源
// （VM_COUNTERS_EX 的 WorkingSetSize），随真实占用起伏。
func currentRSSBytes() int64 {
	counters, err := queryVMCounters()
	if err != nil {
		return 0
	}
	return int64(counters.workingSetSize)
}
