package ccpanel

import (
	"encoding/json"
	"net/http"
)

// apiResponse 与 ccLoad 的 {success,data,error,count} 信封逐字段一致，
// 移植的前端 parseAPIResponse 只认这个形状。
type apiResponse struct {
	Success bool   `json:"success"`
	Data    any    `json:"data"`
	Error   string `json:"error"`
	Count   int    `json:"count"`
}

func respondOK(w http.ResponseWriter, data any) {
	writeEnvelope(w, http.StatusOK, apiResponse{Success: true, Data: data})
}

func respondOKCount(w http.ResponseWriter, data any, count int) {
	writeEnvelope(w, http.StatusOK, apiResponse{Success: true, Data: data, Count: count})
}

func respondError(w http.ResponseWriter, code int, msg string) {
	writeEnvelope(w, code, apiResponse{Success: false, Error: msg})
}

func writeEnvelope(w http.ResponseWriter, code int, body apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
