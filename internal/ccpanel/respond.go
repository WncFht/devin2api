package ccpanel

import (
	"encoding/json"
	"net/http"
)

// apiResponse 与 ccLoad 的 {success,data,error,count} 信封逐字段一致，
// 移植的前端 parseAPIResponse 只认这个形状。has_more/rejects 是日志列表
// 端点的顶层扩展字段（omitempty，其余端点不出现）。
type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Error   string `json:"error"`
	Count   int    `json:"count"`
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

func respondError(w http.ResponseWriter, code int, msg string) {
	writeEnvelope(w, code, apiResponse{Success: false, Error: msg})
}

func writeEnvelope(w http.ResponseWriter, code int, body apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
