package ccpanel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/WncFht/devin2api/internal/selfupdate"
)

// SetUpdateOps 注入自更新服务（/admin/update*）。迟绑定：Service 装配
// 依赖 dbStore 与版本号，构造顺序在 New 之后。
func (h *Handler) SetUpdateOps(svc *selfupdate.Service) {
	h.updateOps = svc
}

// adminUpdateCheck 返回 {current, latest, update_available|error}：
// latest 经 GitHub /releases/latest 302 解析，进程内缓存 20min，
// ?force=1 绕过缓存。
func (h *Handler) adminUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if h.updateOps == nil {
		respondError(w, http.StatusNotImplemented, "self-update unavailable")
		return
	}
	respondOK(w, h.updateOps.Check(r.Context(), r.URL.Query().Get("force") == "1"))
}

// adminUpdateStart 发起前向更新：body {"tag": "vX.Y.Z"} 可省（空体/
// 空 tag 取最新 release）。准入校验后 202 立即返回，下载/校验/编排
// 进程拉起在后台推进——进度轮询走 /admin/update/status。
func (h *Handler) adminUpdateStart(w http.ResponseWriter, r *http.Request) {
	if h.updateOps == nil {
		respondError(w, http.StatusNotImplemented, "self-update unavailable")
		return
	}
	var req struct {
		Tag string `json:"tag"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			respondError(w, http.StatusBadRequest, "invalid json body")
			return
		}
	}
	st, err := h.updateOps.Start(r.Context(), req.Tag)
	if err != nil {
		respondUpdateErr(w, err)
		return
	}
	writeEnvelope(w, http.StatusAccepted, apiResponse{Success: true, Data: st})
}

// adminUpdateRollback 用 install.backup 二进制做对称换回（无需下载）：
// .backup 缺席 404，在途冲突 409。
func (h *Handler) adminUpdateRollback(w http.ResponseWriter, r *http.Request) {
	if h.updateOps == nil {
		respondError(w, http.StatusNotImplemented, "self-update unavailable")
		return
	}
	st, err := h.updateOps.Rollback(r.Context())
	if err != nil {
		respondUpdateErr(w, err)
		return
	}
	writeEnvelope(w, http.StatusAccepted, apiResponse{Success: true, Data: st})
}

// adminUpdateStatus 返回 {supported, current, unit?, rollback_available,
// update?, stale?}——重启窗口内旧/编排/新实例应答的是同一条 db 记录，
// 面板轮询无感跨越进程切换。
func (h *Handler) adminUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if h.updateOps == nil {
		respondError(w, http.StatusNotImplemented, "self-update unavailable")
		return
	}
	respondOK(w, h.updateOps.StatusView(r.Context()))
}

// respondUpdateErr 把自更新 sentinel 映射到状态码：409 在途冲突、
// 501 平台/托管形态不支持、404 无回滚备份、400 坏 tag。
func respondUpdateErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, selfupdate.ErrBusy):
		respondError(w, http.StatusConflict, err.Error())
	case errors.Is(err, selfupdate.ErrUnsupported):
		respondError(w, http.StatusNotImplemented, err.Error())
	case errors.Is(err, selfupdate.ErrNoBackup):
		respondError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, selfupdate.ErrBadTag):
		respondError(w, http.StatusBadRequest, err.Error())
	default:
		respondError(w, http.StatusInternalServerError, err.Error())
	}
}
