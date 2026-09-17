// 本文件是面向 agent 与运维的面板端点：API 自描述目录、配置自省与热重载、
// 进程日志增量拉取、请求目录文件读取与 SSE 合并、日志导出、index.jsonl
// 用量聚合、健康矩阵紧凑条目。
package ccpanel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// requestsFetchCap 是请求列表单次扫描的索引行数上限；
// 过滤与导出在这批记录内进行，更早历史用 grep 查 index.jsonl 原文件。
const requestsFetchCap = 2000

// matrixErrorMessageCap 是矩阵条目 error_message 的截断字节数：悬停归因
// 只要够辨认首因，完整文案在日志行与索引里。
const matrixErrorMessageCap = 120

// adminAPIIndex 是自描述端点：面向 agent 的面板 API 目录与调试工作流说明，
// 让初次接触的调用方无需读代码即可发现检索入口与日志布局。
func (h *Handler) adminAPIIndex(w http.ResponseWriter, r *http.Request) {
	respondOK(w, map[string]any{
		"service": "devin-2api",
		"version": h.Version(),
		"auth":    "dashboard.password 非空时 POST /login 拿 token（=密码本身），随后 Authorization: Bearer <token>；密码为空时全部端点开放",
		"endpoints": []map[string]string{
			{"method": "GET", "path": "/admin/status", "description": "账户/套餐/容量/渠道/模型状态告警 + devin.aliases 校验（alias_targets_absent 目标缺席 / alias_shadows_catalog 遮蔽真 uid）"},
			{"method": "GET", "path": "/admin/models", "description": "模型目录含能力位与价格"},
			{"method": "GET", "path": "/admin/runtime-metrics", "description": "进程运行指标（RPM/QPS/goroutine/内存/GC/CPU）+ http.rejects 管线前拒绝（分原因计数+最近事件，不进索引）+ 日志管道自观测 + gate 速率闸门状态 + warm 前缀保温簿记"},
			{"method": "GET", "path": "/admin/config", "description": "脱敏后的生效配置视图（token/api_key/password 以 sha256 前缀代替）；stale=true 表示文件在最后一次加载后被修改"},
			{"method": "POST", "path": "/admin/config/reload", "description": "重读 config.yaml 并热应用；返回 applied/requires_restart 两组字段名；校验失败 422 旧配置继续服役"},
			{"method": "GET", "path": "/admin/usage", "description": "index.jsonl 聚合：今日/窗口累计、model_days 模型×日矩阵、按模型/按 key、错误阶段、10 分钟粒度趋势、p50/p95/p99、目录价估算成本"},
			{"method": "GET", "path": "/admin/logs?limit=&offset=&q=&status=&status_class=&result=&model=&error_stage=&since=&until=", "description": "最近请求（新在前）；q 子串（含 error_message）或结构化过滤；status 表达式 499/!200/>=400/4xx 逗号 OR；since/until 钉时间窗；has_more 提示尾部窗外仍有更早历史，rejects 附管线前拒绝环（401/429 不进索引）"},
			{"method": "GET", "path": "/admin/logs/matrix?since=", "description": "健康矩阵紧凑条目：只投影分桶与归因所需字段，不分页（扫描上限 2000）；truncated 为真表示 since 窗口覆盖不完整"},
			{"method": "GET", "path": "/admin/logs/export?format=json|csv&筛选参数同上", "description": "导出筛选后的请求摘要（CSV 或 JSON 数组）；触及扫描上限带 X-Truncated: true"},
			{"method": "GET", "path": "/admin/logs/bootstrap", "description": "日志页筛选初始化：模型清单、状态码观察值等一次拉齐"},
			{"method": "GET", "path": "/admin/stats?range=", "description": "面板统计聚合（rpm_stats/按模型/按令牌用量等，dashboardStats 同形）"},
			{"method": "GET", "path": "/admin/stats/filter-options", "description": "stats 页筛选项候选（模型名等）"},
			{"method": "GET", "path": "/admin/metrics", "description": "dashboardMetrics 同形：概要计数与速率"},
			{"method": "GET", "path": "/admin/active-requests", "description": "进行中请求活快照：阶段状态、模型、已下发字节、已写文件、丢弃数"},
			{"method": "GET", "path": "/admin/active-requests/{id}/debug-log", "description": "进行中请求的调试投影（目录已建即按 debug-logs/{id} 口径投影）"},
			{"method": "GET", "path": "/admin/debug-logs/{id}", "description": "单请求 meta.json + 文件清单；id 是 started_at 的 epoch 毫秒"},
			{"method": "GET", "path": "/admin/debug-logs/{id}/merged", "description": "把 06-http-response.jsonl 的 SSE 帧合并成可读的最终响应（reasoning/content/tools）"},
			{"method": "POST", "path": "/admin/debug-logs/merged-response", "description": "上传体合并版：body {\"resp_body\"}（前端可 gzip），与 GET merged 共用同一合并器"},
			{"method": "GET", "path": "/admin/debug-logs/{id}/file/{name}", "description": "读取请求目录内文件（顶层或 attachments/），超 4MB 截断；?raw=1 原样回字节（CSP sandbox + nosniff）"},
			{"method": "POST", "path": "/admin/active-requests/{id}/abort", "description": "中断进行中请求（取消 ctx）；无活跃请求时 404"},
			{"method": "GET", "path": "/admin/process-log?offset=", "description": "进程 stderr 日志尾部；offset>0 增量拉取，响应带 next_offset"},
			{"method": "GET", "path": "/admin/quota", "description": "配额历史快照（logs/quota.jsonl）+ 按燃烧速率外推的耗尽时间"},
			{"method": "GET", "path": "/admin/settings", "description": "运行时设置全表：键、当前值、默认、是否有面板覆盖"},
			{"method": "GET", "path": "/admin/settings/{key}", "description": "单个运行时设置（含覆盖来源标记）"},
			{"method": "PUT", "path": "/admin/settings/{key}", "description": "运行时设置覆盖（debug.enabled/保留策略等），body {\"value\": \"...\"}；对 config.yaml 恒赢"},
			{"method": "POST", "path": "/admin/settings/{key}/reset", "description": "删除该键的面板覆盖，回落 config.yaml/默认值"},
			{"method": "POST", "path": "/admin/settings/batch", "description": "批量设置覆盖，body {\"key\": \"value\", ...}"},
			{"method": "GET", "path": "/admin/auth-tokens?range=", "description": "下游令牌表 + range 内时间窗聚合统计（覆盖累计字段）；行含 anonymous 标记匿名通道"},
			{"method": "POST", "path": "/admin/auth-tokens", "description": "创建下游令牌（并发槽/RPM/5h|日|周|月费用窗口/模型白名单），明文仅此一次返回；anonymous=true 建匿名通道行（无凭据准入，不返回明文）"},
			{"method": "PUT", "path": "/admin/auth-tokens/{id}", "description": "更新令牌（启用/各窗口限额/max_rpm/白名单等）"},
			{"method": "DELETE", "path": "/admin/auth-tokens/{id}", "description": "删除令牌（幂等）"},
			{"method": "GET", "path": "/admin/model-registry", "description": "模型注册表：启用/停用、redirect_model（别名解析前改写）、覆盖标记；各行 catalog 字段透出目录价"},
			{"method": "PUT", "path": "/admin/model-registry", "description": "写注册条目（启用/禁用/redirect_model）"},
			{"method": "DELETE", "path": "/admin/model-registry", "description": "删注册条目"},
			{"method": "GET", "path": "/admin/model-pricing?model=", "description": "单模型目录价投影（found=false 表示无目录价）"},
			{"method": "POST", "path": "/admin/model-test", "description": "模型连通性探针（结果记 log_source=manual_test 的索引行）"},
			{"method": "POST", "path": "/admin/model-chat", "description": "面板内对话式模型测试（同 manual_test 归因）"},
			{"method": "POST", "path": "/admin/update/check", "description": "检查上游 release 是否有新版本"},
		},
		"debug_workflow": []string{
			"每个 /v1/* 响应带 X-Request-Id 头（=调试目录名）；错误体含 debug_ref 与 stage 字段",
			"凭 X-Request-Id 到 /admin/logs?q=<dir> 找到 log_id（started_at epoch 毫秒），再调 /admin/debug-logs/{id} 拿 meta 与文件清单，逐个 file/ 读取",
			"也可直接读磁盘 logs/index.jsonl（每完成请求一行摘要）与 logs/{dir}/（meta.json、01-06 阶段文件、error.json、attachments/）",
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

// resolveDebugDir 把 ccLoad 契约的 log_id（started_at epoch 毫秒）解析为
// 调试目录名；未命中回 ok=false（目录被 retention 清理或 id 伪造）。
func (h *Handler) resolveDebugDir(r *http.Request) (dir string, ok bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 || h.debug == nil {
		return "", false
	}
	return h.debug.FindDirByStartedAt(id)
}

// adminDebugLogFile 返回请求目录内单个文件的内容；JSON/JSONL 原文回传，
// 由前端按需美化。大小超上限时截断并标记 truncated。?raw=1 原样回字节：
// 图片等附件要保真，JSON 视图装不下它们——载荷是客户端请求日志，可能
// 含 HTML/SVG，sandbox 让渲染出的文档处于 opaque origin（脚本拿不到
// 面板会话），nosniff 禁掉嗅探覆盖。读出的字节都过 maskToken 兜底脱敏。
func (h *Handler) adminDebugLogFile(w http.ResponseWriter, r *http.Request) {
	dir, ok := h.resolveDebugDir(r)
	if !ok {
		respondError(w, http.StatusNotFound, "request log not found or already cleaned")
		return
	}
	name := chi.URLParam(r, "*")
	data, total, truncated, err := h.debug.ReadFile(dir, name)
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

// adminDebugLogMerged 把请求目录内 06-http-response.jsonl 的 SSE 帧合并成
// 可读的最终响应文本（reasoning/content/tools），原始帧仍可读。
// truncated 透传读取截断位：>4MB 的 06 只合并前 4MB，没有它调用方会把
// 残缺流当成完整响应。
func (h *Handler) adminDebugLogMerged(w http.ResponseWriter, r *http.Request) {
	dir, ok := h.resolveDebugDir(r)
	if !ok {
		respondError(w, http.StatusNotFound, "request log not found or already cleaned")
		return
	}
	data, _, truncated, err := h.debug.ReadFile(dir, debuglog.StageHTTPResponse)
	if err != nil {
		respondError(w, http.StatusNotFound, "response stream file not found")
		return
	}
	parts := mergeResponseBody(string(h.maskToken(data)))
	respondOK(w, map[string]any{
		"reasoning": parts.Reasoning,
		"content":   parts.Content,
		"tools":     parts.Tools,
		"truncated": truncated,
	})
}

// adminLogsExport 把筛选后的请求摘要导出为 JSON 数组或 CSV；
// 触及扫描上限时带 X-Truncated: true 头（导出体本身无元数据位）。
// 筛选口径与列表端点完全一致：requestFilter 索引级 + logRowMatch 行级
// （api/log_source/model_like/auth_token_id 同样在导出生效）。
func (h *Handler) adminLogsExport(w http.ResponseWriter, r *http.Request) {
	if h.debug == nil {
		respondError(w, http.StatusNotFound, "debug log disabled")
		return
	}
	kh, excluded := h.logScope(r)
	var result debuglog.ListResult
	if !excluded {
		result = h.debug.ListRequests(requestsFetchCap, h.requestFilter(r))
	}
	match := h.logRowMatch(r, kh)
	entries := make([]debuglog.IndexEntry, 0, len(result.Entries))
	for _, e := range result.Entries {
		if match(e) {
			entries = append(entries, e)
		}
	}
	if result.HasMore {
		w.Header().Set("X-Truncated", "true")
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="requests.csv"`)
		writeRequestsCSV(w, entries)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(entries)
}

// writeRequestsCSV 把请求摘要写成 CSV；指针字段用空串表示缺失。
func writeRequestsCSV(w http.ResponseWriter, entries []debuglog.IndexEntry) {
	out := bufio.NewWriter(w)
	defer func() { _ = out.Flush() }()
	_, _ = out.WriteString("dir,started_at,method,path,api,model,requested_model,response_model,status,result,duration_ms,first_upstream_ms,first_client_ms,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,total_tokens,stream,key_hash,client_request_id,error_stage,retries,account,account_switches,error_message\n")
	for _, e := range entries {
		firstUpstream, firstClient := "", ""
		if e.FirstUpstreamMS != nil {
			firstUpstream = strconv.FormatInt(*e.FirstUpstreamMS, 10)
		}
		if e.FirstClientMS != nil {
			firstClient = strconv.FormatInt(*e.FirstClientMS, 10)
		}
		_, _ = fmt.Fprintf(out, "%s,%s,%s,%s,%s,%s,%s,%s,%d,%s,%d,%s,%s,%d,%d,%d,%d,%d,%d,%v,%s,%s,%s,%d,%s,%d,%s\n",
			csvEscape(e.Dir), csvEscape(e.StartedAt), csvEscape(e.Method), csvEscape(e.Path),
			csvEscape(e.API), csvEscape(e.Model), csvEscape(e.RequestedModel), csvEscape(e.ResponseModel),
			e.StatusCode, csvEscape(e.Result), e.DurationMS, firstUpstream, firstClient,
			e.InputTokens, e.OutputTokens, e.CacheReadTokens, e.CacheWriteTokens, e.ReasoningTokens, e.TotalTokens,
			e.Stream, csvEscape(e.KeyHash), csvEscape(e.ClientRequestID), csvEscape(e.ErrorStage), e.Retries,
			csvEscape(e.Account), e.AccountSwitches, csvEscape(e.ErrorMessage))
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
// 扫描上限直接用满 requestsFetchCap——走 /admin/logs?limit= 的列表
// 口径在高流量下盖不满 30 分钟分桶窗口。
func (h *Handler) adminLogsMatrix(w http.ResponseWriter, r *http.Request) {
	if h.debug == nil {
		respondOK(w, map[string]any{"entries": []matrixEntry{}, "total": 0, "truncated": false, "disabled": true})
		return
	}
	filter := h.requestFilter(r)
	result := h.debug.ListRequests(requestsFetchCap, filter)
	entries := make([]matrixEntry, 0, len(result.Entries))
	for _, e := range result.Entries {
		entries = append(entries, matrixEntry{
			StartedAt:       e.StartedAt,
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
	// 截断判定不同于列表的 has_more（后者只说文件比尾部窗大）：窗口内
	// 条目打满扫描上限，或尾部窗最早一行仍晚于 since（尾部边界落在
	// 请求窗口内部，窗内可能有条目根本没被读到），才算覆盖不完整。
	truncated := len(result.Entries) >= requestsFetchCap
	if !truncated && result.HasMore && !filter.Since.IsZero() {
		if tailStart, err := time.Parse(time.RFC3339Nano, result.IndexTailStart); err == nil {
			truncated = tailStart.After(filter.Since)
		}
	}
	respondOK(w, map[string]any{
		"entries":   entries,
		"total":     len(entries),
		"truncated": truncated,
	})
}

// adminUsage 返回 index.jsonl 聚合快照，并按模型目录价附估算成本。
// 价格是 catalog 标价（$/1M tokens），est_cost 为参考值而非上游账单。
func (h *Handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	if h.debug == nil {
		respondOK(w, map[string]any{"disabled": true})
		return
	}
	snap := h.debug.UsageStats()
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
			cost := (float64(m.InputTokens+m.CacheWrite)*c.input + float64(m.CacheRead)*c.cached + float64(m.OutputTokens)*c.output) / 1e6
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
