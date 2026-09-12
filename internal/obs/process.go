// 本文件采集进程级运行指标：goroutine、堆内存、GC、CPU 占用、RSS。
// 用途是区分「代理本身成为瓶颈」与「上游/客户端慢」——ccLoad 的
// processRuntimeMetrics 同款思路，数据源为 runtime.ReadMemStats 与
// 平台相关的 getrusage（见 process_darwin.go / process_linux.go）。
package obs

import (
	"runtime"
	"time"
)

// process 返回进程级指标快照：内存、GC、goroutine、CPU。
// cpu_percent 是相邻两次 Snapshot 之间 cpu_seconds/wall_seconds×100，
// 首次调用返回自启动以来的平均占用（top 式 %CPU，多核可超 100）。
func (m *Metrics) process() map[string]any {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	cpuSeconds, maxRSS := rusageSample()

	m.procMu.Lock()
	wallDelta := time.Since(m.lastCPUAt).Seconds()
	if m.lastCPUAt.IsZero() {
		wallDelta = time.Since(m.startedAt).Seconds()
	}
	cpuPercent := 0.0
	if wallDelta > 0 {
		cpuPercent = (cpuSeconds - m.lastCPUSeconds) / wallDelta * 100
	}
	m.lastCPUSeconds = cpuSeconds
	m.lastCPUAt = time.Now()
	m.procMu.Unlock()

	return map[string]any{
		"goroutines":        runtime.NumGoroutine(),
		"num_cpu":           runtime.NumCPU(),
		"heap_alloc_bytes":  mem.HeapAlloc,
		"heap_sys_bytes":    mem.HeapSys,
		"stack_inuse":       mem.StackInuse,
		"alloc_total":       mem.TotalAlloc,
		"num_gc":            mem.NumGC,
		"gc_pause_total_ms": float64(mem.PauseTotalNs) / 1e6,
		"gc_cpu_fraction":   mem.GCCPUFraction,
		"cpu_seconds":       cpuSeconds,
		"cpu_percent":       cpuPercent,
		"max_rss_bytes":     maxRSS,
	}
}
