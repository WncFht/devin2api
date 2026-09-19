// 本文件是面向 agent 与运维的面板端点：API 自描述目录、配置自省与热重载、
// 进程日志增量拉取、请求目录文件读取与 SSE 合并、日志导出、logs 表
// 用量聚合、健康矩阵紧凑条目。
package ccpanel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/store"
)

// requestsFetchCap 是导出/矩阵单次查询的行数上限；
// 更早历史经 /admin/logs 翻页或直接查 logs 表。
const requestsFetchCap = 2000

// matrixErrorMessageCap 是矩阵条目 error_message 的截断字节数：悬停归因
// 只要够辨认首因，完整文案在日志行与索引里。
const matrixErrorMessageCap = 120

// adminAPIIndex 是自描述端点：面向 agent 的面板 API 目录与调试工作流说明，
// 让初次接触的调用方无需读代码即可发现检索入口与日志布局。
// 端点清单由 routes 表生成（doc 非空的行），与挂载集同源不会漂移。
func (h *Handler) adminAPIIndex(w http.ResponseWriter, r *http.Request) {
	rts := h.routes()
	endpoints := make([]map[string]string, 0, len(rts))
	for _, rt := range rts {
		if rt.doc == "" {
			continue
		}
		endpoints = append(endpoints, map[string]string{
			"method":      rt.method,
			"path":        rt.docPath,
			"description": rt.doc,
		})
	}
	respondOK(w, map[string]any{
		"service":   "devin-2api",
		"version":   h.Version(),
		"auth":      "dashboard.password 非空时 POST /login 拿 token（=密码本身），随后 Authorization: Bearer <token>；密码为空时全部端点开放",
		"endpoints": endpoints,
		"debug_workflow": []string{
			"每个 /v1/* 响应带 X-Request-Id 头（=调试目录名）；错误体含 debug_ref 与 stage 字段",
			"凭 X-Request-Id 到 /admin/logs?q=<dir> 找到 log_id（logs 表自增主键），再调 /admin/debug-logs/{id} 拿 meta 与文件清单，逐个 file/ 读取",
			"请求目录 payload 在状态目录的 devin-2api.db（SQLite debug_files/debug_chunks 两表：meta.json、01-06 阶段文件、error.json、attachments/），摘要行在 logs 表；磁盘 logs/ 只剩 stderr.log 等顶层文件",
		},
	})
}

// adminConfigCurrent 返回脱敏后的生效配置视图（文件键名与 config.yaml
// 一致，token/api_key/password 以 sha256 前缀代替明文）。实现见 main 装配。
func (h *Handler) adminConfigCurrent(w http.ResponseWriter, r *http.Request) {
	if h.configOps == nil || h.configOps.Current == nil {
		respondError(w, http.StatusNotFound, "config view unavailable")
		return
	}
	respondOK(w, h.configOps.Current())
}

// adminConfigReload 重读配置文件并热应用；校验失败 422 且旧配置继续服役。
// 返回 applied（已生效）与 requires_restart（要重启才生效）两组字段名，
// 让调用方明确知道哪些改动仍在 pending。
func (h *Handler) adminConfigReload(w http.ResponseWriter, r *http.Request) {
	if h.configOps == nil || h.configOps.Reload == nil {
		respondError(w, http.StatusNotFound, "config reload unavailable")
		return
	}
	report, err := h.configOps.Reload()
	if err != nil {
		respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	respondOK(w, report)
}

// adminProcessLog 返回进程 stderr 日志尾部（slog 行），支持 ?offset=
// 增量拉取；响应带 next_offset 供下一轮续读。
func (h *Handler) adminProcessLog(w http.ResponseWriter, r *http.Request) {
	if h.debug == nil {
		respondError(w, http.StatusNotFound, "debug log disabled")
		return
	}
	offset, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	data, next, err := h.debug.ReadProcessLog(offset)
	if err != nil {
		respondError(w, http.StatusNotFound, "process log unavailable")
		return
	}
	respondOK(w, map[string]any{
		"text":        string(h.maskToken(data)),
		"next_offset": next,
	})
}

// resolveDebugDir 把 ccLoad 契约的 log_id 解析为调试目录名与请求时刻
// （毫秒）：先按 logs 表自增主键查，未命中再按迁移前的 started_at 毫秒戳
// 兜底（目录名内嵌该时刻）。均未命中回 ok=false（retention 清理或伪造）。
func (h *Handler) resolveDebugDir(r *http.Request, id int64) (dir string, timeMS int64, ok bool) {
	if id <= 0 {
		return "", 0, false
	}
	if h.store != nil {
		d, ms, found, err := h.store.LogDirByID(r.Context(), id)
		if err != nil {
			slog.Warn("ccpanel: log dir lookup failed", "error", err)
		} else if found {
			return d, ms, true
		}
	}
	if h.debug != nil {
		if d, found := h.debug.FindDirByStartedAt(r.Context(), id); found {
			return d, id, true
		}
	}
	return "", 0, false
}

// adminDebugLogFile 返回请求目录内单个文件的内容；JSON/JSONL 原文回传，
// 由前端按需美化。大小超上限时截断并标记 truncated。?raw=1 原样回字节：
// 图片等附件要保真，JSON 视图装不下它们——载荷是客户端请求日志，可能
// 含 HTML/SVG，sandbox 让渲染出的文档处于 opaque origin（脚本拿不到
// 面板会话），nosniff 禁掉嗅探覆盖。读出的字节都过 maskToken 兜底脱敏。
func (h *Handler) adminDebugLogFile(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	dir, _, ok := h.resolveDebugDir(r, id)
	if !ok {
		respondError(w, http.StatusNotFound, "request log not found or already cleaned")
		return
	}
	name := chi.URLParam(r, "*")
	data, total, truncated, err := h.debug.ReadFile(r.Context(), dir, name)
	if err != nil {
		respondError(w, http.StatusNotFound, "file not found")
		return
	}
	data = h.maskToken(data)
	if r.URL.Query().Get("raw") == "1" {
		w.Header().Set("Content-Type", http.DetectContentType(data))
		w.Header().Set("Content-Security-Policy", "sandbox")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(data)
		return
	}
	// 二进制标 binary 由前端给下载/预览入口——强转 string 再经
	// json.Encoder 会把非法 UTF-8 烧成 U+FFFD，附件内容全毁。
	if !utf8.Valid(data) {
		respondOK(w, map[string]any{
			"name":      name,
			"size":      total,
			"truncated": truncated,
			"binary":    true,
		})
		return
	}
	respondOK(w, map[string]any{
		"name":      name,
		"size":      total,
		"truncated": truncated,
		"text":      string(data),
	})
}

// adminDebugLogMerged 把请求目录内 06-http-response.jsonl 的记录帧经
// rebuildClientWire 还原成线上字节流后，合并成可读的最终响应文本
// （reasoning/content/tools），原始帧仍可读。
// truncated 透传读取截断位：>4MB 的 06 只合并前 4MB，没有它调用方会把
// 残缺流当成完整响应。
func (h *Handler) adminDebugLogMerged(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	dir, _, ok := h.resolveDebugDir(r, id)
	if !ok {
		respondError(w, http.StatusNotFound, "request log not found or already cleaned")
		return
	}
	data, _, truncated, err := h.debug.ReadFile(r.Context(), dir, debuglog.StageHTTPResponse)
	if err != nil {
		respondError(w, http.StatusNotFound, "response stream file not found")
		return
	}
	parts := mergeResponseBody(string(rebuildClientWire(h.maskToken(data))))
	respondOK(w, map[string]any{
		"reasoning": parts.Reasoning,
		"content":   parts.Content,
		"tools":     parts.Tools,
		"truncated": truncated,
	})
}

// adminLogsExport 把筛选后的请求摘要导出为 JSON 数组或 CSV；
// 命中数超 requestsFetchCap 时带 X-Truncated: true 头（导出体本身
// 无元数据位）。筛选口径与列表端点完全一致（同一 logQuery 下推）。
func (h *Handler) adminLogsExport(w http.ResponseWriter, r *http.Request) {
	// 导出只读 logs 表：debug 开关管的是 payload 录制，不该让调试
	// 关闭时日志导出虚假 404。
	if h.store == nil {
		respondError(w, http.StatusNotFound, "log store unavailable")
		return
	}
	lq, excluded := h.logQuery(r)
	lq.Limit = requestsFetchCap
	var rows []*store.LogRow
	var total int64
	if !excluded {
		var err error
		rows, total, err = h.store.SearchLogs(r.Context(), lq)
		if err != nil {
			slog.Warn("ccpanel: logs export query failed", "error", err)
			respondError(w, http.StatusInternalServerError, "logs query failed")
			return
		}
	}
	if total > int64(len(rows)) {
		w.Header().Set("X-Truncated", "true")
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="requests.csv"`)
		writeRequestsCSV(w, rows)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(rows)
}

// writeRequestsCSV 把请求摘要写成 CSV；指针字段用空串表示缺失。
func writeRequestsCSV(w http.ResponseWriter, entries []*store.LogRow) {
	out := bufio.NewWriter(w)
	defer func() { _ = out.Flush() }()
	_, _ = out.WriteString("dir,started_at,method,path,api,model,requested_model,response_model,status,result,duration_ms,first_upstream_ms,first_client_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,total_tokens,stream,key_hash,client_request_id,error_stage,retries,account,account_switches,error_message,upstream_done_ms\n")
	for _, e := range entries {
		firstUpstream, firstClient, upstreamDone := "", "", ""
		if e.FirstUpstreamMS != nil {
			firstUpstream = strconv.FormatInt(*e.FirstUpstreamMS, 10)
		}
		if e.FirstClientMS != nil {
			firstClient = strconv.FormatInt(*e.FirstClientMS, 10)
		}
		if e.UpstreamDoneMS != nil {
			upstreamDone = strconv.FormatInt(*e.UpstreamDoneMS, 10)
		}
		_, _ = fmt.Fprintf(out, "%s,%s,%s,%s,%s,%s,%s,%s,%d,%s,%d,%s,%s,%d,%d,%d,%d,%d,%d,%v,%s,%s,%s,%d,%s,%d,%s,%s\n",
			csvEscape(e.Dir), csvEscape(e.StartedAt.Format(time.RFC3339Nano)), csvEscape(e.Method), csvEscape(e.Path),
			csvEscape(e.API), csvEscape(e.Model), csvEscape(e.RequestedModel), csvEscape(e.ResponseModel),
			e.StatusCode, csvEscape(e.Result), e.DurationMS, firstUpstream, firstClient,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
			e.Stream, csvEscape(e.KeyHash), csvEscape(e.ClientRequestID), csvEscape(e.ErrorStage), e.Retries,
			csvEscape(e.Account), e.AccountSwitches, csvEscape(e.ErrorMessage), upstreamDone)
	}
}

// csvEscape 转义含逗号/引号/换行的字段；以 = + - @ 开头的值前加单引号，
// 防止客户端可控字段（client_request_id）在 Excel 里被当公式执行。
func csvEscape(s string) string {
	if s != "" && strings.ContainsRune("=+-@", rune(s[0])) {
		s = "'" + s
	}
	if !strings.ContainsAny(s, ",\"\n") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// matrixEntry 是健康矩阵用的条目投影：只带分桶（started_at）与归因/悬停
// （归因口径、状态码计数、均耗时/均 TTFB）所需字段。完整 IndexEntry 约
// 30 个字段，投影把单条载荷压小一个量级。
type matrixEntry struct {
	StartedAt      string `json:"started_at"`
	Model          string `json:"model,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	StatusCode     int    `json:"status_code"`
	Result         string `json:"result"`
	ErrorStage     string `json:"error_stage,omitempty"`
	// ErrorMessage 是首因错误文案的截断版（matrixErrorMessageCap），
	// 供格子悬停直接展示「为什么败」，不必逐格回查调试目录。
	ErrorMessage string `json:"error_message,omitempty"`
	// Owner 是失败责任归因（client/business_limited/upstream），由
	// debuglog.ErrorOwner 统一计算——前端不再按 status/result/stage
	// 复刻判定，与 usage 聚合的 client_faults/upstream_faults 同口径。
	Owner string `json:"owner,omitempty"`
	// Account 是最终服务本请求的上游账号（号池 lane 名），
	// AccountSwitches 是 failover 换号次数——矩阵按号分桶归因用。
	Account         string `json:"account,omitempty"`
	AccountSwitches int    `json:"account_switches,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	FirstUpstreamMS *int64 `json:"first_upstream_ms,omitempty"`
	RateLimited     bool   `json:"rate_limited,omitempty"`
}

// adminLogsMatrix 给概览健康矩阵提供紧凑条目：窗口内条目不分页，
// 上限直接用满 requestsFetchCap——走 /admin/logs?limit= 的列表
// 口径在高流量下盖不满 30 分钟分桶窗口。
func (h *Handler) adminLogsMatrix(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		respondOK(w, map[string]any{"entries": []matrixEntry{}, "total": 0, "truncated": false, "disabled": true})
		return
	}
	lq, excluded := h.logQuery(r)
	lq.Limit = requestsFetchCap
	var rows []*store.LogRow
	var total int64
	if !excluded {
		var err error
		rows, total, err = h.store.SearchLogs(r.Context(), lq)
		if err != nil {
			slog.Warn("ccpanel: logs matrix query failed", "error", err)
			respondError(w, http.StatusInternalServerError, "logs query failed")
			return
		}
	}
	entries := make([]matrixEntry, 0, len(rows))
	for _, e := range rows {
		entries = append(entries, matrixEntry{
			StartedAt:       e.StartedAt.Format(time.RFC3339Nano),
			Model:           e.Model,
			RequestedModel:  e.RequestedModel,
			StatusCode:      e.StatusCode,
			Result:          e.Result,
			ErrorStage:      e.ErrorStage,
			ErrorMessage:    truncate(e.ErrorMessage, matrixErrorMessageCap),
			Owner:           debuglog.ErrorOwner(e),
			Account:         e.Account,
			AccountSwitches: e.AccountSwitches,
			DurationMS:      e.DurationMS,
			FirstUpstreamMS: e.FirstUpstreamMS,
			RateLimited:     e.RateLimited,
		})
	}
	// 截断判定简化为「命中总数超过返回条数」：SQL 计数精确，不再存在
	// 尾部窗边界漏读的情形。
	respondOK(w, map[string]any{
		"entries":   entries,
		"total":     total,
		"truncated": total > int64(len(entries)),
	})
}

// adminUsage 返回 logs 表聚合快照，并按模型目录价附估算成本。
// 价格是 catalog 标价（$/1M tokens），est_cost 为参考值而非上游账单。
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	if h.store == nil {
		respondOK(w, map[string]any{"disabled": true})
		return
	}
	snap, err := h.usageCache.Get(r.Context())
	if err != nil {
		slog.Warn("ccpanel: usage stats query failed", "error", err)
		respondError(w, http.StatusInternalServerError, "usage query failed")
		return
	}
	catalog := h.modelCatalogMap(r.Context())
	var totalCost float64
	models := make([]map[string]any, 0, len(snap.Models))
	for _, m := range snap.Models {
		// 行字段与 dimensionAgg 的 json tag 一一对应：marshal 往返代替
		// 手抄清单，聚合侧新增维度自动透出。
		raw, _ := json.Marshal(m)
		var row map[string]any
		_ = json.Unmarshal(raw, &row)
		if c, ok := catalog[m.Name]; ok {
			// cache_write 实测按 input 价计费：配额翻转拟合的隐含单价 ≈ input 价，
			// 并非 Anthropic 惯例的 1.25×；catalog 无独立 cache_write 价格维。
			cost := TokenCost(m.InputTokens, m.OutputTokens, m.CacheRead, m.CacheWrite, c.CatalogPrice)
			row["est_cost"] = cost
			totalCost += cost
			if c.contextTokens > 0 && m.Requests > 0 {
				row["context_tokens"] = c.contextTokens
				// 上下文填充率：平均单请求占用 token（输入+两向缓存）占
				// 窗口上限的比例——衡量「窗口挤不挤」，不是累计量。
				row["avg_context_tokens"] = float64(m.InputTokens+m.CacheRead+m.CacheWrite) / float64(m.Requests)
				row["context_fill_pct"] = row["avg_context_tokens"].(float64) / float64(c.contextTokens) * 100
			}
		}
		models = append(models, row)
	}
	respondOK(w, map[string]any{
		"snapshot":      snap,
		"models":        models,
		"est_cost":      totalCost,
		"cost_basis":    "catalog price per 1M tokens (estimate, not invoice)",
		"price_missing": len(catalog) == 0,
	})
}
