// 本文件实现 ccLoad 日志页与调试模态的服务端契约：
// GET /dashboard|/admin/logs（分页请求日志）、/logs/bootstrap（首屏聚合）、
// GET /admin/debug-logs/{id}、GET /admin/active-requests/{id}/debug-log、
// POST /admin/debug-logs/merged-response。
//
// 数据源是 debuglog 的 index.jsonl + 请求目录阶段文件（替代 ccLoad 的
// logs/debug_logs 两表）。日志行 id 用 started_at 的 epoch 毫秒——stats
// 端点的 last_*_id 同口径，前端凭它经 FindDirByStartedAt 回查调试目录。
package ccpanel

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/WncFht/devin2api/internal/debuglog"
)

// maxMergedDebugResponseBodyBytes 是 merged-response 请求体上限（ccLoad 同名常量同值）。
const maxMergedDebugResponseBodyBytes = 16 * 1024 * 1024

// logCostComponent 对应 ccLoad util.CostComponent：单类 token 的可展示计价过程。
type logCostComponent struct {
	PricePerMillion float64 `json:"price_per_million"`
	Quantity        int64   `json:"quantity"`
	Cost            float64 `json:"cost"`
}

// logCostBreakdown 对应 ccLoad util.StandardCostBreakdown。本服务目录价无
// service_tier/5m-1h 分档，cache_write 按 input 价计（与 cellCost 同口径）。
type logCostBreakdown struct {
	Input      logCostComponent `json:"input"`
	Output     logCostComponent `json:"output"`
	CacheRead  logCostComponent `json:"cache_read"`
	CacheWrite logCostComponent `json:"cache_write"`
	Total      float64          `json:"total"`
}

func newLogCostComponent(quantity int64, pricePerMillion float64) logCostComponent {
	return logCostComponent{
		PricePerMillion: pricePerMillion,
		Quantity:        quantity,
		Cost:            float64(quantity) * pricePerMillion / 1_000_000,
	}
}

// logEntry 对应 ccLoad model.LogEntry 经 dashboardLogEntry 投影后的平铺
// wire 形状。字段含义差异：model=客户端请求模型、actual_model=解析后发给
// 上游的 uid（与 model 相同则省略，同 ccLoad「未重定向」语义）、
// api_key_used/api_key_hash 都是本服务的 key_hash（无明文可脱敏）、
// api=index.jsonl 的入口端点原值、upstream_protocol 恒 "devin"、
// cost_multiplier 恒 1；单上游无渠道维（channel_* 字段不投）。
type logEntry struct {
	ID            int64  `json:"id"`
	Time          int64  `json:"time"` // unix 秒（ccLoad JSONTime 序列化口径）
	Model         string `json:"model"`
	ActualModel   string `json:"actual_model,omitempty"`
	ResponseModel string `json:"response_model,omitempty"`
	LogSource     string `json:"log_source,omitempty"`
	StatusCode    int    `json:"status_code"`
	Message       string `json:"message"`
	// ErrorMessage 是首个失败的完整错误文案（index.jsonl 同源，≤300B）；
	// message 列只放 result[:error_stage] 短形态，长文案经本字段透出。
	ErrorMessage             string            `json:"error_message,omitempty"`
	Duration                 float64           `json:"duration"`
	IsStreaming              bool              `json:"is_streaming"`
	UpstreamWebsocket        bool              `json:"upstream_websocket,omitempty"`
	FirstByteTime            float64           `json:"first_byte_time,omitempty"`
	APIKeyUsed               string            `json:"api_key_used"`
	APIKeyHash               string            `json:"api_key_hash,omitempty"`
	AuthTokenID              int64             `json:"auth_token_id"`
	AuthTokenDescription     string            `json:"auth_token_description"`
	API                      string            `json:"api,omitempty"`
	UpstreamProtocol         string            `json:"upstream_protocol,omitempty"`
	ClientIP                 string            `json:"client_ip"`
	BaseURL                  string            `json:"base_url,omitempty"`
	InputTokens              int64             `json:"input_tokens"`
	OutputTokens             int64             `json:"output_tokens"`
	ReasoningTokens          int64             `json:"reasoning_tokens,omitempty"`
	CacheReadInputTokens     int64             `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64             `json:"cache_creation_input_tokens"`
	Cache5mInputTokens       int64             `json:"cache_5m_input_tokens"`
	Cache1hInputTokens       int64             `json:"cache_1h_input_tokens"`
	Cost                     float64           `json:"cost"`
	CostMultiplier           float64           `json:"cost_multiplier"`
	CostBreakdown            *logCostBreakdown `json:"cost_breakdown,omitempty"`
}

// projectLogEntry 把一条 index.jsonl 摘要投影成日志页行。message 列复刻
// ccLoad 语义（成功="ok"，失败=result[:error_stage]）——前端要求它非空才
// 渲染调试入口。
func (h *Handler) projectLogEntry(e debuglog.IndexEntry, prices map[string]CatalogPrice) logEntry {
	started, _ := time.Parse(time.RFC3339Nano, e.StartedAt)
	actual := e.Model
	if actual == e.RequestedModel {
		actual = ""
	}
	message := "ok"
	if e.Result != "completed" {
		message = e.Result
		if e.ErrorStage != "" {
			message = e.Result + ": " + e.ErrorStage
		}
	}
	// 面板探活行（X-Client-Request-Id: panel-probe）记 manual_test——
	// 与 ccLoad 同语义：带「手动测试」徽标，不计入默认 proxy 视图。
	logSource := "proxy"
	if e.ClientRequestID == debuglog.ProbeClientRequestID {
		logSource = "manual_test"
	}
	entry := logEntry{
		ID:                       started.UnixMilli(),
		Time:                     started.Unix(),
		Model:                    e.RequestedModel,
		ActualModel:              actual,
		ResponseModel:            e.ResponseModel,
		LogSource:                logSource,
		StatusCode:               e.StatusCode,
		Message:                  message,
		ErrorMessage:             e.ErrorMessage,
		Duration:                 float64(e.DurationMS) / 1000,
		IsStreaming:              e.Stream,
		UpstreamWebsocket:        e.API == "responses-ws",
		APIKeyUsed:               e.KeyHash,
		APIKeyHash:               e.KeyHash,
		API:                      e.API,
		UpstreamProtocol:         "devin",
		ClientIP:                 e.ClientIP,
		BaseURL:                  h.BaseURL(),
		InputTokens:              e.InputTokens,
		OutputTokens:             e.OutputTokens,
		ReasoningTokens:          e.ReasoningTokens,
		CacheReadInputTokens:     e.CacheReadTokens,
		CacheCreationInputTokens: e.CacheWriteTokens,
		Cache5mInputTokens:       e.CacheWriteTokens,
		CostMultiplier:           1,
	}
	// key_hash 反查令牌投影 auth_token_id/description；master key 与开放
	// 模式的行（无对应令牌）留零值，ccLoad 的 NULL 列同语义。
	if h.tokens != nil && e.KeyHash != "" {
		if t, ok := h.tokens.LookupByKeyHash(e.KeyHash); ok {
			entry.AuthTokenID = t.ID
			entry.AuthTokenDescription = t.Description
		}
	}
	if e.FirstUpstreamMS != nil {
		entry.FirstByteTime = float64(*e.FirstUpstreamMS) / 1000
	}
	billing := e.Model
	if billing == "" {
		billing = e.RequestedModel
	}
	if p, ok := prices[billing]; ok {
		breakdown := &logCostBreakdown{
			Input:      newLogCostComponent(e.InputTokens, p.Input),
			Output:     newLogCostComponent(e.OutputTokens, p.Output),
			CacheRead:  newLogCostComponent(e.CacheReadTokens, p.Cached),
			CacheWrite: newLogCostComponent(e.CacheWriteTokens, p.Input),
		}
		breakdown.Total = breakdown.Input.Cost + breakdown.Output.Cost +
			breakdown.CacheRead.Cost + breakdown.CacheWrite.Cost
		// ccLoad buildLogCostBreakdown：Cost<=0 不出 breakdown。
		if breakdown.Total > 0 {
			entry.Cost = breakdown.Total
			entry.CostBreakdown = breakdown
		}
	}
	return entry
}

// logScope 把日志查询的数据范围折成 (key_hash, excluded)：kh 非空时只放
// 该 key_hash 的索引行；excluded 表示筛选条件不可能命中，直接回空集。
// 范围来源与 queryScope 同源（api_token 身份 + auth_token_id、
// log_source 筛选），作用对象换成索引行——model/api 等行级维度由
// 调用方的 match 处理。
func (h *Handler) logScope(r *http.Request) (kh string, excluded bool) {
	q := r.URL.Query()
	switch strings.TrimSpace(q.Get("log_source")) {
	case "", "all", "proxy", "manual_test":
	default:
		return "", true
	}
	if id := identityFrom(r); id.Role == "api_token" {
		kh = id.KeyHash
		if kh == "" {
			return "", true
		}
	}
	if raw := strings.TrimSpace(q.Get("auth_token_id")); raw != "" {
		tkh := ""
		if tid, err := strconv.ParseInt(raw, 10, 64); err == nil && h.tokens != nil {
			if t, ok := h.tokens.Get(tid); ok {
				tkh = t.KeyHash()
			}
		}
		if tkh == "" || (kh != "" && tkh != kh) {
			return "", true
		}
		kh = tkh
	}
	return kh, false
}

// parseRequestFilter 从查询串构建索引侧结构化筛选：q 为子串，status 是
// 状态表达式（499/4xx/>=400/!200，逗号 OR），status_class/result/model/
// error_stage/since/until 为精确或时间条件。dashboardLogs 与导出端点共用。
func parseRequestFilter(r *http.Request) debuglog.RequestFilter {
	q := r.URL.Query()
	get := func(key string) string { return strings.TrimSpace(q.Get(key)) }
	filter := debuglog.RequestFilter{
		Query:       get("q"),
		StatusClass: get("status_class"),
		Status:      get("status"),
		Result:      get("result"),
		Model:       get("model"),
		ErrorStage:  get("error_stage"),
	}
	if since := get("since"); since != "" {
		if parsed, err := time.Parse(time.RFC3339, since); err == nil {
			filter.Since = parsed
		}
	}
	if until := get("until"); until != "" {
		if parsed, err := time.Parse(time.RFC3339, until); err == nil {
			filter.Until = parsed
		}
	}
	return filter
}

// requestFilter 是 logs/export/matrix 三个列表类端点共用的筛选解析：
// 旧面板词汇（q/status 表达式/status_class/result/model/error_stage/
// since/until）与 ccLoad 词汇（range/start_time/end_time/status_code）
// 并存。时间窗逐侧落定：显式 since/until(RFC3339) 优先——matrix 下钻
// 钉历史窗口靠它；缺席侧回落到 resolveRange（range 契约，默认 today）。
func (h *Handler) requestFilter(r *http.Request) debuglog.RequestFilter {
	filter := parseRequestFilter(r)
	since, until, _ := resolveRange(r, time.Now())
	if filter.Since.IsZero() {
		filter.Since = since
	}
	if filter.Until.IsZero() {
		filter.Until = until
	}
	if filter.Status == "" {
		if code, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("status_code"))); err == nil && code > 0 {
			filter.Status = strconv.Itoa(code)
		}
	}
	return filter
}

// respondLogEntries 写日志列表信封：data=行数组、count=窗内命中总数、
// has_more=索引尾部窗外仍有更早历史；另附带 rejects 管线前拒绝环
// （形状同 runtime-metrics 的 http.rejects）——拒绝不进索引，列表页靠
// 它提示「表里看不到 401/429」。
func (h *Handler) respondLogEntries(w http.ResponseWriter, entries []logEntry, total int, hasMore bool) {
	var rejects any
	if h.metrics != nil {
		rejects = h.metrics.Rejects()
	}
	writeEnvelope(w, http.StatusOK, apiResponse{
		Success: true, Data: entries, Count: total, HasMore: hasMore, Rejects: rejects,
	})
}

// dashboardLogs 实现 ccLoad 的 /dashboard|/admin/logs（HandleErrors）：
// data=日志行数组（新在前），count=窗口内命中总数；limit 默认 200、上限 1000。
// count 以 index.jsonl 尾部读取窗（≈4MB/万行）为准——窗口外仍有历史时
// （ListRequests.HasMore）count 是下界，与 ccLoad 的 SQL COUNT(*) 口径有偏差。
func (h *Handler) dashboardLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	// 一级筛选走索引读取（时间窗 + 状态码），二级筛选在内存做——
	// token 维度由 logScope 判空或收敛成 key_hash 比较。
	kh, excluded := h.logScope(r)
	if h.debug == nil || excluded {
		h.respondLogEntries(w, []logEntry{}, 0, false)
		return
	}
	result := h.debug.ListRequests(-1, h.requestFilter(r))
	match := h.logRowMatch(r, kh)

	prices := h.CatalogPrices(r.Context())
	entries := make([]logEntry, 0, min(limit, len(result.Entries)))
	total := 0
	for _, e := range result.Entries {
		if !match(e) {
			continue
		}
		if total >= offset && len(entries) < limit {
			entries = append(entries, h.projectLogEntry(e, prices))
		}
		total++
	}
	h.respondLogEntries(w, entries, total, result.HasMore)
}

// logRowMatch 是 logs 列表/导出共用的行级（内存）筛选：kh 非空只放该
// key_hash 的行；log_source 按探针行口径（proxy 排除探针行、manual_test
// 只留探针行、""/all 全放，与 ccLoad 一致）；api/upstream_protocol 精确、
// model_like 子串（model 精确筛选已在索引侧由 filter.Model 完成，覆盖
// RequestedModel/Model/ResponseModel 三个字段）。
func (h *Handler) logRowMatch(r *http.Request, kh string) func(debuglog.IndexEntry) bool {
	q := r.URL.Query()
	api := strings.TrimSpace(q.Get("api"))
	upstream := strings.ToLower(strings.TrimSpace(q.Get("upstream_protocol")))
	modelLike := strings.TrimSpace(q.Get("model_like"))
	src := strings.TrimSpace(q.Get("log_source"))
	return func(e debuglog.IndexEntry) bool {
		if kh != "" && e.KeyHash != kh {
			return false
		}
		isProbe := e.ClientRequestID == debuglog.ProbeClientRequestID
		if src == "proxy" && isProbe {
			return false
		}
		if src == "manual_test" && !isProbe {
			return false
		}
		if api != "" && api != "all" && e.API != api {
			return false
		}
		if upstream != "" && upstream != "all" && upstream != "devin" {
			return false
		}
		if modelLike != "" && !strings.Contains(e.RequestedModel, modelLike) && !strings.Contains(e.Model, modelLike) && !strings.Contains(e.ResponseModel, modelLike) {
			return false
		}
		return true
	}
}

// dashboardLogsBootstrap 实现 /dashboard|/admin/logs/bootstrap
// （ccLoad LogsBootstrapResponse 形状）：logs 页首屏聚合。
// ccLoad 用 range（默认 this_month）圈定 distinct 集合；本服务 rollup 覆盖
// 索引全生命周期，模型/状态码集合不按 range 截窗（超集，对筛选 UI 无害）。
func (h *Handler) dashboardLogsBootstrap(w http.ResponseWriter, r *http.Request) {
	tokens := []any{}
	if h.tokens != nil {
		// api_token 身份只看得到自己的令牌项——令牌下拉是全仓清单，
		// 全量返回会把其他令牌的描述/额度泄漏给下游持钥人。
		if id := identityFrom(r); id.Role == "api_token" {
			if t, ok := h.tokens.Get(id.TokenID); ok {
				tokens = append(tokens, t.API())
			}
		} else {
			for _, t := range h.tokens.List() {
				tokens = append(tokens, t.API())
			}
		}
	}
	respondOK(w, map[string]any{
		"auth_tokens":  tokens,
		"models":       h.ru.modelSet(h.debug, identityFrom(r).KeyHash),
		"status_codes": h.ru.statusCodeSet(h.debug),
	})
}

// adminDebugLog 实现 GET /admin/debug-logs/{id}：id 是 started_at 的 epoch
// 毫秒；目录不存在（retention 清理或 id 伪造）时回 ccLoad 的 404+data 形状。
func (h *Handler) adminDebugLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid log_id")
		return
	}
	dir, ok := h.debug.FindDirByStartedAt(id)
	if !ok {
		h.respondDebugLogUnavailable(w)
		return
	}
	respondOK(w, h.debugLogResponse(dir, id))
}

// adminActiveRequestDebugLog 实现 GET /admin/active-requests/{id}/debug-log：
// id 是活跃请求表里的目录名 FNV 哈希（与 adminActiveRequests 同口径），
// 反查后投影同一份调试响应——请求未完结时 meta/06 都是半成品快照。
func (h *Handler) adminActiveRequestDebugLog(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		respondError(w, http.StatusBadRequest, "invalid request_id")
		return
	}
	if h.debug != nil {
		for _, ar := range h.debug.ActiveRequests() {
			if activeRequestID(ar.Dir) == id {
				respondOK(w, h.debugLogResponse(ar.Dir, id))
				return
			}
		}
	}
	h.respondDebugLogUnavailable(w)
}

// adminMergedResponse 实现 POST /admin/debug-logs/merged-response：
// 请求体 {resp_body}（前端可能 gzip 上传），返回合并后的可读响应。
func (h *Handler) adminMergedResponse(w http.ResponseWriter, r *http.Request) {
	body, err := readMaybeCompressedJSONBody(r, maxMergedDebugResponseBodyBytes)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var req struct {
		RespBody string `json:"resp_body"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request")
		return
	}
	// 上传的 resp_body 是用户贴的调试内容，合并前先过 token 字面值脱敏——
	// 面板回显口径与目录读路径一致。
	respondOK(w, mergeResponseBody(string(h.maskToken([]byte(req.RespBody)))))
}

// respondDebugLogUnavailable 对应 ccLoad 的 404+debugLogUnavailableInfo：
// 前端按 data 里的两个 system setting 渲染「调试日志不可用」空态。
// retention 投影用目录保留天数（ccLoad 语义是调试数据的整体寿命）。
func (h *Handler) respondDebugLogUnavailable(w http.ResponseWriter) {
	enabled := "false"
	var retentionMin int64
	if h.debug != nil {
		if h.debug.Enabled() {
			enabled = "true"
		}
		retentionMin = int64(h.debug.Policy().Days) * 24 * 60
	}
	writeEnvelope(w, http.StatusNotFound, apiResponse{
		Success: false,
		Error:   "debug log unavailable",
		Data: map[string]any{
			"reason": "debug_log_not_found",
			"debug_log_enabled": map[string]any{
				"key": "debug_log_enabled", "value": enabled, "value_type": "bool",
			},
			"debug_log_retention_minutes": map[string]any{
				"key":   "debug_log_retention_minutes",
				"value": strconv.FormatInt(retentionMin, 10), "value_type": "int",
			},
		},
	})
}

// debugLogResponse 把一个请求目录投影成 ccLoad debugLogResponse 形状。
// 本服务的「协议转换」恒成立（客户端协议 → connect-RPC）：
// original_* 来自 01（客户端原文），req_* 来自 03（上游 wire 请求，
// 含 attemptN 重试分片），resp_* 来自 04（上游原始帧），translated_*
// 由 06 的记录帧重建为客户端线上形态。上游请求/响应头 connect client
// 未录制，如实给 "{}"（区别于 maskedHeaderUnavailable 的「脱敏失败」）。
func (h *Handler) debugLogResponse(dir string, logID int64) map[string]any {
	resp := map[string]any{
		"log_id":       logID,
		"req_method":   http.MethodPost,
		"req_headers":  "{}",
		"resp_status":  0,
		"resp_headers": "{}",
	}

	var meta struct {
		StartedAt  string `json:"started_at"`
		StatusCode int    `json:"status_code"`
		Result     string `json:"result"`
	}
	// Detail 一次拿 meta.json 与文件清单；files 投给前端文件页签
	// （含进行中请求的半成品文件）。投影只用三个标量字段，自由文本
	// 不外流，meta 本体不需要过 maskToken。
	if detail, err := h.debug.Detail(dir); err == nil {
		_ = json.Unmarshal(detail.Meta, &meta)
		resp["files"] = detail.Files
	}
	// 读路径按最近见过的 token 字面值兜底脱敏——写路径的 secretKey
	// 名单只管结构化键名，自由文本（body 原文、上游错误文案）里的
	// token 在这里罩住；自愈轮换后旧 token 仍在 recentTokens 集合内。
	readStage := func(name string) ([]byte, error) {
		data, _, _, err := h.debug.ReadFile(dir, name)
		return h.maskToken(data), err
	}
	if started, err := time.Parse(time.RFC3339Nano, meta.StartedAt); err == nil {
		resp["created_at"] = started.Unix()
	} else {
		resp["created_at"] = logID / 1000
	}
	resp["translated_resp_status"] = meta.StatusCode
	resp["translated_resp_headers"] = "{}"

	// 01：客户端原始请求 → original_*；缺席时 protocol_transformed 留缺省，
	// 前端「请求」页签回落到 req_*（上游 wire）而不是空面板。
	if data, err := readStage(debuglog.StageHTTPRequest); err == nil {
		var original struct {
			Path    string          `json:"path"`
			Headers json.RawMessage `json:"headers"`
			Body    json.RawMessage `json:"body"`
		}
		if json.Unmarshal(data, &original) == nil {
			resp["protocol_transformed"] = true
			resp["original_req_url"] = original.Path
			resp["original_req_headers"] = maskSensitiveHeaderJSON(string(original.Headers))
			addDebugResponseBody(resp, "original_req_body", debugBodyBytes(original.Body))
		}
	}

	// 03 + attemptN：上游 wire 请求体。多次重发按序拼接——ccLoad 的
	// req_body 只记最后一次尝试，我们把每次尝试都留痕（重试排障要对比）。
	var reqBody bytes.Buffer
	if names, err := debuglog.DevinRequestStages(filepath.Join(h.debug.Root(), dir)); err == nil {
		for _, name := range names {
			data, err := readStage(name)
			if err != nil {
				continue
			}
			if reqBody.Len() > 0 {
				reqBody.WriteString("\n\n")
			}
			reqBody.Write(data)
		}
	}
	// 上游 procedure 按 wire 体形辨：搜索调用无 chatMessagePrompts。
	baseURL := h.BaseURL()
	reqURL := baseURL + "/exa.api_server_pb.ApiServerService/GetChatMessage"
	if reqBody.Len() > 0 && !bytes.Contains(reqBody.Bytes(), []byte(`"chatMessagePrompts"`)) {
		reqURL = baseURL + "/exa.api_server_pb.ApiServerService/GetWebSearchResults"
	}
	resp["req_url"] = reqURL
	addDebugResponseBody(resp, "req_body", reqBody.Bytes())

	// 04：上游原始帧原文 → resp_body。
	if data, err := readStage(debuglog.StageDevinResponse); err == nil {
		addDebugResponseBody(resp, "resp_body", data)
	}

	// resp_status 口径：completed→200（上游流式响应没有单发 HTTP 状态可记，
	// 正常走完即视为 200）；devin_connect 是上游语义拒绝，借下发状态码近似
	// 上游 wire 语义；其余（transport/rate_gate/本地层）记 0——
	// 「未形成完整上游响应」由 upstream_error 补充说明。
	var errStage, errMessage string
	if data, err := readStage(debuglog.ErrorFile); err == nil {
		var e struct {
			Stage   string `json:"stage"`
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil {
			errStage, errMessage = e.Stage, e.Message
		}
	}
	switch {
	case meta.Result == "completed":
		resp["resp_status"] = 200
	case errStage == debuglog.ErrStageDevinConnect && meta.StatusCode > 0:
		resp["resp_status"] = meta.StatusCode
	}
	if errMessage != "" {
		resp["upstream_error"] = errStage + ": " + errMessage
	}

	// 06：下发客户端的记录帧重建线上字节流。
	if data, err := readStage(debuglog.StageHTTPResponse); err == nil {
		addDebugResponseBody(resp, "translated_resp_body", rebuildClientWire(data))
	}
	return resp
}

// debugBodyBytes 把 01 里 body 的 RawMessage 还原成字节：JSON 原文原样
// 返回；被记成 JSON 字符串的非 JSON 体（写入侧对非法 JSON 的退化形态）
// 解码回文本，避免前端看到一层多余的引号包装。
func debugBodyBytes(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > 1 && raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []byte(s)
		}
	}
	return raw
}

// addDebugResponseBody 对应 ccLoad 同名函数：utf8 直接出字符串，
// 否则 base64 + <key>_encoding 标记。
func addDebugResponseBody(resp map[string]any, key string, body []byte) {
	if utf8.Valid(body) {
		resp[key] = string(body)
		return
	}
	resp[key] = base64.StdEncoding.EncodeToString(body)
	resp[key+"_encoding"] = "base64"
}

// rebuildClientWire 把 06-http-response.jsonl 的记录帧还原成线上字节流：
//   - 簿记帧 event="response"/"error" 且为唯一记录 → data 即完整非流式响应体；
//   - event="[DONE]"/空名 → data-only 帧（OpenAI chat 形态）；
//   - 其余 → event: X + data: {…} 命名帧。
//
// "error" 的二义：非首条时是 anthropic 流内错误帧（event: error），
// 单条时是管线前错误响应体（writeLoggedError 只在未提交时写簿记帧）。
func rebuildClientWire(data []byte) []byte {
	type record struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	var records []record
	for line := range bytes.Lines(data) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var rec record
		if json.Unmarshal(line, &rec) == nil {
			records = append(records, rec)
		}
	}
	var out bytes.Buffer
	for _, rec := range records {
		switch {
		case rec.Event == "response" || (rec.Event == "error" && len(records) == 1):
			out.Write(rec.Data)
			if !bytes.HasSuffix(rec.Data, []byte("\n")) {
				out.WriteByte('\n')
			}
		case rec.Event == "[DONE]":
			out.WriteString("data: [DONE]\n\n")
		case rec.Event == "":
			out.WriteString("data: ")
			out.Write(rec.Data)
			out.WriteString("\n\n")
		default:
			out.WriteString("event: " + rec.Event + "\ndata: ")
			out.Write(rec.Data)
			out.WriteString("\n\n")
		}
	}
	return out.Bytes()
}

// maskedHeaderUnavailable 对应 ccLoad 同名常量：脱敏失败的可辨识占位。
const maskedHeaderUnavailable = `{"_masked":"unavailable"}`

// isSensitiveHeader 对应 ccLoad 同名函数：认证类请求头判定。
func isSensitiveHeader(key string) bool {
	return strings.EqualFold(key, "Authorization") ||
		strings.EqualFold(key, "X-Refresh-Token") ||
		strings.EqualFold(key, "X-Api-Key") ||
		strings.EqualFold(key, "Api-Key") ||
		strings.EqualFold(key, "X-Goog-Api-Key") ||
		strings.EqualFold(key, "Proxy-Authorization")
}

// maskHeaderValue 对应 ccLoad 同名函数：≤8 字符全掩，否则留头尾各 4。
func maskHeaderValue(v string) string {
	if len(v) <= 8 {
		return "******"
	}
	return v[:4] + "******" + v[len(v)-4:]
}

// maskSensitiveHeaderJSON 对应 ccLoad 同名函数（encoding/json 版）：
// 对 JSON string 形态的 headers 脱敏；字符串值与字符串数组元素都处理。
// 解析/写回失败返回 maskedHeaderUnavailable——静默换成 "{}" 会让人
// 误以为上游真的没发请求头。
func maskSensitiveHeaderJSON(jsonStr string) string {
	if jsonStr == "" {
		return jsonStr
	}
	var headers map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &headers); err != nil || headers == nil {
		return maskedHeaderUnavailable
	}
	for key, value := range headers {
		if !isSensitiveHeader(key) {
			continue
		}
		switch v := value.(type) {
		case string:
			headers[key] = maskHeaderValue(v)
		case []any:
			items := make([]any, len(v))
			for i, item := range v {
				if s, ok := item.(string); ok {
					items[i] = maskHeaderValue(s)
				} else {
					items[i] = item
				}
			}
			headers[key] = items
		}
	}
	out, err := json.Marshal(headers)
	if err != nil {
		return maskedHeaderUnavailable
	}
	return string(out)
}

// readMaybeCompressedJSONBody 对应 ccLoad 同名函数：读 JSON 请求体，
// 支持 Content-Encoding: gzip，超 limit 报错。
func readMaybeCompressedJSONBody(req *http.Request, limit int64) ([]byte, error) {
	if req == nil || req.Body == nil {
		return nil, errors.New("empty request body")
	}
	defer func() { _ = req.Body.Close() }()

	var reader io.Reader = req.Body
	switch strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(req.Body)
		if err != nil {
			return nil, errors.New("invalid gzip request body")
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	default:
		return nil, errors.New("unsupported content encoding")
	}

	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, errors.New("read request body failed")
	}
	if int64(len(body)) > limit {
		return nil, errors.New("request body too large")
	}
	if len(body) == 0 {
		return nil, errors.New("empty request body")
	}
	return body, nil
}
