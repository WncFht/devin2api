// 本文件采集进程级运行指标：goroutine、堆内存、GC、CPU 占用、RSS。
// 用途是区分「代理本身成为瓶颈」与「上游/客户端慢」——同类代理的
// 进程级指标采集同款思路，数据源为 runtime.ReadMemStats 与
// 平台相关的进程采样（见 process_darwin.go / process_linux.go /
// process_windows.go；其它平台走 process_other.go 零值兜底）。
package obs

import (
	"runtime"
	"time"
)

// minCPUSampleWindow 是 cpu_percent 的最小采样窗：相邻两次 Snapshot 间隔
// 过近时 cpu_seconds 差值除以近零的 wall 会炸出离谱百分比。窗口不足时本次
// 报 0 且不推进采样基准——cpu 差值累积进下一窗口，长期均值不失真。
const minCPUSampleWindow = time.Millisecond

// ProcessSnapshot 是进程级指标快照：内存、GC、goroutine、CPU、RSS。
type ProcessSnapshot struct {
	Goroutines     int     `json:"goroutines"`
	NumCPU         int     `json:"num_cpu"`
	HeapAllocBytes uint64  `json:"heap_alloc_bytes"`
	HeapSysBytes   uint64  `json:"heap_sys_bytes"`
	StackInuse     uint64  `json:"stack_inuse"`
	AllocTotal     uint64  `json:"alloc_total"`
	NumGC          uint32  `json:"num_gc"`
	GCPauseTotalMS float64 `json:"gc_pause_total_ms"`
	GCCPUFraction  float64 `json:"gc_cpu_fraction"`
	CPUSeconds     float64 `json:"cpu_seconds"`
	CPUPercent     float64 `json:"cpu_percent"`
	MaxRSSBytes    int64   `json:"max_rss_bytes"`
	// 瞬时 RSS：随真实占用起伏，区别于只涨不降的 max_rss_bytes 峰值；
	// 无瞬时数据源的平台（见 process_*.go 的 currentRSSBytes）恒为 0。
	RSSCurrentBytes int64 `json:"rss_current_bytes"`
}

// process 返回进程级指标快照：内存、GC、goroutine、CPU。
// cpu_percent 是相邻两次 Snapshot 之间 cpu_seconds/wall_seconds×100，
// 首次调用返回自启动以来的平均占用（top 式 %CPU，多核可超 100）。
func (m *Metrics) process() ProcessSnapshot {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	cpuSeconds, maxRSS := rusageSample()
	currentRSS := currentRSSBytes()

	m.procMu.Lock()
	wallDelta := time.Since(m.lastCPUAt).Seconds()
	if m.lastCPUAt.IsZero() {
		wallDelta = time.Since(m.startedAt).Seconds()
	}
	cpuPercent := 0.0
	if wallDelta >= minCPUSampleWindow.Seconds() {
		cpuPercent = (cpuSeconds - m.lastCPUSeconds) / wallDelta * 100
		m.lastCPUSeconds = cpuSeconds
		m.lastCPUAt = time.Now()
	}
	m.procMu.Unlock()

	return ProcessSnapshot{
		Goroutines:      runtime.NumGoroutine(),
		NumCPU:          runtime.NumCPU(),
		HeapAllocBytes:  mem.HeapAlloc,
		HeapSysBytes:    mem.HeapSys,
		StackInuse:      mem.StackInuse,
		AllocTotal:      mem.TotalAlloc,
		NumGC:           mem.NumGC,
		GCPauseTotalMS:  float64(mem.PauseTotalNs) / 1e6,
		GCCPUFraction:   mem.GCCPUFraction,
		CPUSeconds:      cpuSeconds,
		CPUPercent:      cpuPercent,
		MaxRSSBytes:     maxRSS,
		RSSCurrentBytes: currentRSS,
	}
}
