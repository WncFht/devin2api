// 本文件实现 /admin/runtime-metrics 的进程内历史环：按固定周期把进程级
// 信号（堆/RSS/goroutine/CPU、在途与累计吞吐、日志写在飞积压、闸门闩态、
// 拒绝累计）采进定长内存环，供事故后回看趋势——runtime-metrics 端点本身
// 只是即点快照，CL-277 一类 RSS 画像事故曾被迫外挂 CSV 采样器补历史。
//
// 环只活内存：重启即归零、不落库——要的是零成本的趋势读数，不是又一本账。
// 单写者（采样协程）+ 任意读者（admin handler），读侧短锁整环拷出再过滤。
package ccpanel

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// metricsHistoryInterval 是历史环采样周期；metricsHistoryCap 是环深度，
// 乘积即回看窗（30s×480=4h）——覆盖一次部署交接与多数慢漏型事故的起落
// 全程。metricsHistoryMaxMinutes 是 ?minutes 查询上限，超过环覆盖窗的
// 请求按全环返回。
const (
	metricsHistoryInterval   = 30 * time.Second
	metricsHistoryCap        = 480
	metricsHistoryMaxMinutes = 240
)

// metricsSample 是一拍进程指标采样：平铺标量字段无嵌套——480 点 × 12
// 字段数十 KB，环是白送的观测面。累计计数（completed/rejected/latch）
// 按进程期总量记，差分即速率——短闩/短拒绝峰可能被 30s 采样的瞬时值
// 漏掉，差分仍看得见。
type metricsSample struct {
	At               int64   `json:"at"`
	HeapAllocBytes   uint64  `json:"heap_alloc_bytes"`
	HeapSysBytes     uint64  `json:"heap_sys_bytes"`
	RSSBytes         uint64  `json:"rss_current_bytes"`
	Goroutines       int     `json:"goroutines"`
	CPUPercent       float64 `json:"cpu_percent"`
	ActiveRequests   int64   `json:"active_requests"`
	CompletedTotal   uint64  `json:"completed_total"`
	RejectedTotal    uint64  `json:"rejected_total"`
	LogPendingBytes  int64   `json:"log_pending_bytes"`
	GateLatchedLanes int     `json:"gate_latched_lanes"`
	GateLatchTotal   int     `json:"gate_latch_total"`
}

// metricsHistory 是定长环形缓冲：写满后覆盖最老样本。
type metricsHistory struct {
	mu   sync.Mutex
	ring [metricsHistoryCap]metricsSample
	head int // 下一个写入槽位
	size int
}

// add 追加一条样本；写侧只此一处，由采样协程驱动。
func (r *metricsHistory) add(s metricsSample) {
	r.mu.Lock()
	r.ring[r.head] = s
	r.head = (r.head + 1) % metricsHistoryCap
	if r.size < metricsHistoryCap {
		r.size++
	}
	r.mu.Unlock()
}

// since 返回 at >= cutoff 的样本，最老在前；环内样本按写入序天然单调，
// 从最早存活槽位顺序扫描即得。返回空切片而非 nil——JSON 序列化成 []
// 而不是 null。
func (r *metricsHistory) since(cutoff int64) []metricsSample {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]metricsSample, 0, r.size)
	oldest := (r.head - r.size + metricsHistoryCap) % metricsHistoryCap
	for i := 0; i < r.size; i++ {
		s := r.ring[(oldest+i)%metricsHistoryCap]
		if s.At >= cutoff {
			out = append(out, s)
		}
	}
	return out
}

// StartMetricsHistory 启动进程指标采样协程：立即采一点作启动基线，之后
// 每 metricsHistoryInterval 一拍。纯进程内读取（runtime 状态 + 已注入的
// 快照源），不打上游、不写库——排空期与交接进程照跑，部署重叠窗内它
// 自己的历史同样是有效观测；进程退出即历史归零。
func (h *Handler) StartMetricsHistory() {
	go func() {
		h.captureMetricsSample()
		ticker := time.NewTicker(metricsHistoryInterval)
		defer ticker.Stop()
		for range ticker.C {
			h.captureMetricsSample()
		}
	}()
}

// captureMetricsSample 采一拍进程指标写入历史环，信号集即 runtime-metrics
// 各组里事故回看最缺的部分。未接线的源（nil metrics/debug/闸门）按零值
// 记录——历史环是观测面，单源缺席不该让整拍失败。
func (h *Handler) captureMetricsSample() {
	s := metricsSample{At: time.Now().Unix()}
	if h.metrics != nil {
		snap := h.metrics.Snapshot()
		s.ActiveRequests = snap.ActiveRequests
		s.CompletedTotal = snap.CompletedRequests
		s.RejectedTotal = snap.RejectedRequests
		s.HeapAllocBytes = snap.Process.HeapAllocBytes
		s.HeapSysBytes = snap.Process.HeapSysBytes
		s.RSSBytes = uint64(snap.Process.RSSCurrentBytes)
		s.Goroutines = snap.Process.Goroutines
		s.CPUPercent = snap.Process.CPUPercent
	}
	if stats := h.debug.Stats(); stats != nil {
		s.LogPendingBytes = decodeDebugStats(stats).PendingBytes
	}
	// 闩态按 lane 聚合：任一 lane 在闩都计入 latched 数；LatchTotal 是
	// 全 lane 累计闩次数之和。与 runtime-metrics 的 accounts 组同源
	// 同口径（同一次池快照）。
	if ps, ok := h.poolSnapshot(); ok {
		for _, ls := range ps.Accounts {
			if ls.Gate.Latched {
				s.GateLatchedLanes++
			}
			s.GateLatchTotal += ls.Gate.LatchCount
		}
	}
	h.history.add(s)
}

// adminRuntimeMetricsHistory 实现 GET /admin/runtime-metrics/history：
// 历史环窗口读取——?minutes 缺省 60、上限 metricsHistoryMaxMinutes，
// 返回 at 升序样本与采样间隔，客户端直接铺曲线。环是进程内存：重启
// 归零，uptime 短于请求窗口时只返回已采到的部分。
func (h *Handler) adminRuntimeMetricsHistory(w http.ResponseWriter, r *http.Request) {
	minutes := 60
	if raw := strings.TrimSpace(r.URL.Query().Get("minutes")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			minutes = n
		}
	}
	if minutes > metricsHistoryMaxMinutes {
		minutes = metricsHistoryMaxMinutes
	}
	cutoff := time.Now().Unix() - int64(minutes)*60
	respondOK(w, map[string]any{
		"interval_sec": int(metricsHistoryInterval / time.Second),
		"samples":      h.history.since(cutoff),
	})
}
