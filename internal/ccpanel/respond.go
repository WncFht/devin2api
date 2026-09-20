package ccpanel

import (
	"encoding/json"
	"net/http"

	"github.com/WncFht/devin2api/internal/obs"
)

// apiResponse 与 ccLoad 的 {success,data,error,count} 信封逐字段一致，
// 移植的前端 parseAPIResponse 只认这个形状。has_more/rejects 是日志列表
// 端点的顶层扩展字段（omitempty，其余端点不出现）。
type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Error   string `json:"error"`
	// Count 是筛选命中总数；nil 时字段缺席——深页翻页不付全窗
	// COUNT(*) 的税，前端按既有缺省降级路径处理页数。
	Count *int `json:"count,omitempty"`
	// HasMore 为真表示索引尾部读取窗之外仍有更早历史（count 只是窗内下界）。
	HasMore bool `json:"has_more,omitempty"`
	// Rejects 附带管线前拒绝环（401/429 等不进索引的拒绝），形状与
	// runtime-metrics 的 http.rejects 相同——列表页用它提示「拒绝不在表里」。
	Rejects any `json:"rejects,omitempty"`
	// ActiveRequestTitleEnabled 是 ccLoad 在活跃请求列表信封外的顶层扩展；
	// 指针类型保证显式 false 也会上 wire（omitempty 对裸 bool 会吞掉 false）。
	ActiveRequestTitleEnabled *bool `json:"active_request_title_enabled,omitempty"`
}

func respondOK(w http.ResponseWriter, data any) {
	writeEnvelope(w, http.StatusOK, apiResponse{Success: true, Data: data})
}

// rejectsView 是管线前拒绝视图：obs.Rejects 内嵌展开（by_reason/recent/
// labels），外加 debuglog 侧的 rejected 留存行写失败数——该行走同步直写
// 绕开 sheddable 队列，dropped_*/io_errors 口径盖不住它，insert_failed
// 是「拒绝证据没落库」这一损耗在面板侧的唯一透出。
type rejectsView struct {
	obs.Rejects
	// InsertFailed 仅在 debug.Stats() 缺席时缺席（omitempty 保留「未知」
	// 与「零失败」的区别）。
	InsertFailed *uint64 `json:"insert_failed,omitempty"`
}

// rejectsView 组装管线前拒绝视图；metrics 为 nil 时返回 nil（端点降级）。
func (h *Handler) rejectsView() *rejectsView {
	if h.metrics == nil {
		return nil
	}
	view := rejectsView{Rejects: h.metrics.Rejects()}
	if stats := h.debug.Stats(); stats != nil {
		failed := decodeDebugStats(stats).RejectedInsertFailed
		view.InsertFailed = &failed
	}
	return &view
}

// debugStats 是 debuglog.Manager.Stats() 自观测 map 里面板消费键的定宽
// 投影。Stats 的 map 形状是它的对外契约——runtime-metrics 的 debuglog
// 组整份透传给前端；面板侧消费的键在这里一次性定宽，键名漂移在编译期
// 爆炸而非静默零值。
type debugStats struct {
	QueuedLogEvents      int    `json:"queued_log_events"`
	QueueCapacity        int    `json:"queue_capacity"`
	DroppedLogEvents     uint64 `json:"dropped_log_events"`
	DroppedPayloadBytes  uint64 `json:"dropped_payload_bytes"`
	LateWrites           uint64 `json:"late_writes"`
	IOErrors             uint64 `json:"io_errors"`
	PendingBytes         int64  `json:"pending_bytes"`
	PendingBytesMax      int64  `json:"pending_bytes_max"`
	PendingBytesCap      int64  `json:"pending_bytes_cap"`
	ErrorsOnly           bool   `json:"errors_only"`
	RejectedInsertFailed uint64 `json:"rejected_insert_failed"`
}

// decodeDebugStats 把 Stats() map 投成定宽结构：走一轮 JSON 编解码而非
// 逐键断言——Stats 产出的值本就是 JSON wire 形状，调用频率低（admin
// 端点 + 30s 采样一拍），一次解码换掉整族断言助手。
func decodeDebugStats(stats map[string]any) debugStats {
	var out debugStats
	raw, err := json.Marshal(stats)
	if err != nil {
		return out
	}
	// Stats 是本进程自产数据，值域封闭；未识别的值按缺席处理。
	_ = json.Unmarshal(raw, &out)
	return out
}

func respondError(w http.ResponseWriter, code int, msg string) {
	writeEnvelope(w, code, apiResponse{Success: false, Error: msg})
}

func writeEnvelope(w http.ResponseWriter, code int, body apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func intPtr(v int) *int { return &v }

// decodeJSON 解码写端点的 JSON 请求体（上限 1MB）；失败时已写 400
// 响应并返回 false。错误文案不参与前端契约（前端只按 success/error
// 信封展示），全部端点统一为 invalid json body。
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json body")
		return false
	}
	return true
}
