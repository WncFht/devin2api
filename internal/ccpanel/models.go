package ccpanel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/authtoken"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/modelreg"
)

// modelRow 是 /admin/model-registry merged 视图的行形状：模型名、来源
// 标记、覆盖层字段（enabled/redirect_model）、最终解析名，外加 catalog
// 子对象（上游目录的 label/价格/倍率/能力标记，目录外的名字为 null）——
// 管理端一眼看到「这个名字实际会路由到哪、值多少钱、有什么能力」。
type modelRow struct {
	Model         string         `json:"model"`
	Enabled       bool           `json:"enabled"`
	RedirectModel string         `json:"redirect_model,omitempty"`
	Resolved      string         `json:"resolved"`
	Sources       []string       `json:"sources"`
	HasOverride   bool           `json:"has_override"`
	Catalog       map[string]any `json:"catalog,omitempty"`
}

// modelNamesUnion 返回对外模型名的并集：目录 uid ∪ 别名键 ∪ 注册表名 ∪
// 流量中出现过的模型名，逐名标记来源。注册表页、渠道模型清单投影共用。
func (h *Handler) modelNamesUnion(r *http.Request) map[string][]string {
	src := map[string][]string{}
	add := func(name, s string) {
		if name != "" {
			src[name] = append(src[name], s)
		}
	}
	for _, uid := range h.ModelUIDs(r.Context()) {
		add(uid, "catalog")
	}
	if ps, ok := h.poolSnapshot(); ok {
		for name := range ps.Aliases {
			add(name, "alias")
		}
	}
	if h.models != nil {
		for _, name := range h.models.Names() {
			add(name, "registry")
		}
	}
	for _, m := range h.modelSet(r.Context(), "") {
		add(m, "traffic")
	}
	return src
}

// adminModelRegistry 实现 GET /admin/model-registry：merged 模型视图。
// resolved = redirect_model ?? 模型名，再过一遍别名表（与 /v1 准入
// 后 adapter 的解析路径一致）。
func (h *Handler) adminModelRegistry(w http.ResponseWriter, r *http.Request) {
	aliases := map[string]string{}
	if ps, ok := h.poolSnapshot(); ok {
		aliases = ps.Aliases
	}
	overrides := map[string]modelreg.Entry{}
	if h.models != nil {
		overrides = h.models.Entries()
	}

	src := h.modelNamesUnion(r)
	// 目录行按 uid 键控，并入注册表行——目录外的名字（别名键、纯注册表项、
	// 未登记的直通流量名）Catalog 为空。
	catalogByUID := map[string]map[string]any{}
	for _, m := range h.CatalogModels(r.Context()) {
		if uid, _ := m["uid"].(string); uid != "" {
			catalogByUID[uid] = m
		}
	}
	rows := make([]modelRow, 0, len(src))
	for name, sources := range src {
		e, has := overrides[name]
		target := name
		if e.RedirectModel != "" {
			target = e.RedirectModel
		}
		rows = append(rows, modelRow{
			Model:         name,
			Enabled:       !e.Disabled,
			RedirectModel: e.RedirectModel,
			Resolved:      devin.ResolveModelAlias(aliases, target),
			Sources:       sources,
			HasOverride:   has,
			Catalog:       catalogByUID[name],
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Model < rows[j].Model })
	respondOK(w, map[string]any{"models": rows})
}

// modelRegistryPut 是 PUT /admin/model-registry 的请求体；enabled 用指针
// 区分「未传」（默认 true）与显式 false。
type modelRegistryPut struct {
	Model         string `json:"model"`
	Enabled       *bool  `json:"enabled"`
	RedirectModel string `json:"redirect_model"`
}

// adminPutModel 实现 PUT /admin/model-registry：upsert 一条覆盖。
// 提交默认值（enabled 且无重定向）等价于删除该覆盖——Store.Set 自动清理。
func (h *Handler) adminPutModel(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		respondError(w, http.StatusServiceUnavailable, "model registry unavailable")
		return
	}
	var req modelRegistryPut
	if !decodeJSON(w, r, &req) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if err := h.models.Set(req.Model, modelreg.Entry{RedirectModel: req.RedirectModel, Disabled: !enabled}); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondOK(w, map[string]any{"model": req.Model})
}

// adminDeleteModel 实现 DELETE /admin/model-registry?model=：移除覆盖。
// 名字走查询参数而非路径段——模型名允许含 "/"，路径段装不下。
func (h *Handler) adminDeleteModel(w http.ResponseWriter, r *http.Request) {
	if h.models == nil {
		respondError(w, http.StatusServiceUnavailable, "model registry unavailable")
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		respondError(w, http.StatusBadRequest, "model is required")
		return
	}
	if err := h.models.Delete(model); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondOK(w, map[string]any{"deleted": true})
}

// modelTestTimeout 是单次探活的上限：探针走真实上游往返，拉长到分钟级
// 才有意义地区分「上游慢」与「上游挂」；同时不能无限占住面板请求。
const modelTestTimeout = 60 * time.Second

// probeMaxTokens 约束探针产出上限：够模型给出可见回复，又控制单次探活成本。
const probeMaxTokens = 1024

// defaultProbeContent 是探活缺省提示词，与前端模态的默认测试内容同值
// （ccLoad 默认探活语）——问日期类事实，能区分「真回了内容」与「回了壳」。
const defaultProbeContent = "sonnet 4.0的发布日期是什么"

// probeRawBodyLimit 是回传给前端的原始响应体截断长度。
const probeRawBodyLimit = 32 << 10

// modelTestRequest 是探活请求体：model 必填；stream/content/client_protocol
// 缺省时退化为最小 ping（非流式、anthropic 协议、固定提示词）。
// client_protocol 取 anthropic/openai/codex，决定打哪个 /v1 入口。
type modelTestRequest struct {
	Model          string `json:"model"`
	Stream         bool   `json:"stream"`
	Content        string `json:"content"`
	ClientProtocol string `json:"client_protocol"`
}

// probeRequestSpec 把探活参数映射到具体 /v1 入口与请求体——三协议各用
// 最简合法载荷：anthropic/openai 是 messages 数组，codex 是字符串 input。
func probeRequestSpec(clientProtocol, model, content string, stream bool) (path string, body []byte, err error) {
	var payload map[string]any
	switch clientProtocol {
	case "openai":
		path = "/v1/chat/completions"
		payload = map[string]any{
			"model":      model,
			"stream":     stream,
			"max_tokens": probeMaxTokens,
			"messages":   []map[string]any{{"role": "user", "content": content}},
		}
	case "codex":
		path = "/v1/responses"
		payload = map[string]any{
			"model":             model,
			"stream":            stream,
			"max_output_tokens": probeMaxTokens,
			"input":             content,
		}
	default:
		clientProtocol = "anthropic"
		path = "/v1/messages"
		payload = map[string]any{
			"model":      model,
			"stream":     stream,
			"max_tokens": probeMaxTokens,
			"messages":   []map[string]any{{"role": "user", "content": content}},
		}
	}
	body, err = json.Marshal(payload)
	return path, body, err
}

// modelChatMessage 是多轮试聊请求里的一帧：role 只允许
// user/assistant/system，其它取值在准入处 400 拒掉。
type modelChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// modelChatRequest 是 POST /admin/model-chat 的请求体：messages 至少一条，
// 其余字段语义与 modelTestRequest 相同。
type modelChatRequest struct {
	Model          string             `json:"model"`
	ClientProtocol string             `json:"client_protocol"`
	Messages       []modelChatMessage `json:"messages"`
	Stream         bool               `json:"stream"`
}

// probeChatRequestSpec 把多轮消息投影到三个 /v1 入口的合法载荷：openai 的
// messages 原样透传；anthropic 不允许 system 混在 messages 里，提为顶层
// system 字段（多条以 \n\n 拼接）；codex 折成 responses 的 message item
// 数组——assistant 回合的 content 用 output_text，其余用 input_text。
func probeChatRequestSpec(clientProtocol, model string, msgs []modelChatMessage, stream bool) (path string, body []byte, err error) {
	var payload map[string]any
	switch clientProtocol {
	case "openai":
		path = "/v1/chat/completions"
		payload = map[string]any{
			"model":      model,
			"stream":     stream,
			"max_tokens": probeMaxTokens,
			"messages":   msgs,
		}
	case "codex":
		path = "/v1/responses"
		input := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			contentType := "input_text"
			if m.Role == "assistant" {
				contentType = "output_text"
			}
			input = append(input, map[string]any{
				"type": "message",
				"role": m.Role,
				"content": []map[string]any{
					{"type": contentType, "text": m.Content},
				},
			})
		}
		payload = map[string]any{
			"model":             model,
			"stream":            stream,
			"max_output_tokens": probeMaxTokens,
			"input":             input,
		}
	default:
		path = "/v1/messages"
		var sysParts []string
		chatMsgs := make([]modelChatMessage, 0, len(msgs))
		for _, m := range msgs {
			if m.Role == "system" {
				sysParts = append(sysParts, m.Content)
			} else {
				chatMsgs = append(chatMsgs, m)
			}
		}
		payload = map[string]any{
			"model":      model,
			"stream":     stream,
			"max_tokens": probeMaxTokens,
			"messages":   chatMsgs,
		}
		if len(sysParts) > 0 {
			payload["system"] = strings.Join(sysParts, "\n\n")
		}
	}
	body, err = json.Marshal(payload)
	return path, body, err
}

// probeRecorder 在 httptest.ResponseRecorder 上记录首个字节写出的时刻——
// 流式探活的首字延迟（first_byte_duration_ms）由此得来。
type probeRecorder struct {
	*httptest.ResponseRecorder
	firstAt time.Time
}

func (r *probeRecorder) Write(p []byte) (int, error) {
	if r.firstAt.IsZero() {
		r.firstAt = time.Now()
	}
	return r.ResponseRecorder.Write(p)
}

// resolvedModel 返回模型经注册表覆盖与别名链后的最终解析名，与 /v1
// 准入后的解析路径一致；探活结果的 actual_model 用它（Anthropic 响应体
// 回显的是请求名，拿不到真实落点）。
func (h *Handler) resolvedModel(name string) string {
	target := name
	if h.models != nil {
		if e, ok := h.models.Entries()[name]; ok && e.RedirectModel != "" {
			target = e.RedirectModel
		}
	}
	if ps, ok := h.poolSnapshot(); ok {
		return devin.ResolveModelAlias(ps.Aliases, target)
	}
	return target
}

// adminModelTest 实现 POST /admin/model-test：面板探活入口，返回形状与
// ccLoad HandleChannelTest 对齐（success/message/status_code/duration_ms/
// first_byte_duration_ms/actual_model/response_text/api_response/error/
// raw_response），前端的探活模态原样可消费。
func (h *Handler) adminModelTest(w http.ResponseWriter, r *http.Request) {
	h.runModelProbe(w, r)
}

// runModelProbe 是探活共用 runner：经注入的进程内根路由发一次真实 /v1
// 请求，过完整鉴权/并发闸门/注册表准入/重定向/别名/上游管线——返回的是
// 真实往返结果而非配置静态检查。探针在 logs 表里以
// client_request_id=panel-probe 留痕，request_id 回传供定位调试目录。
func (h *Handler) runModelProbe(w http.ResponseWriter, r *http.Request) {
	if h.probeHandler == nil {
		respondError(w, http.StatusServiceUnavailable, "model test unavailable")
		return
	}
	var req modelTestRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		respondError(w, http.StatusBadRequest, "model is required")
		return
	}
	content := req.Content
	if strings.TrimSpace(content) == "" {
		content = defaultProbeContent
	}
	path, body, err := probeRequestSpec(strings.ToLower(strings.TrimSpace(req.ClientProtocol)), model, content, req.Stream)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.serveProbeRequest(w, r, path, body, model, req.ClientProtocol, req.Stream)
}

// adminModelChat 实现 POST /admin/model-chat：多轮试聊入口，与探活共用
// serveProbeRequest 的执行与返回形状，差别只在请求体把单条 content 换成
// messages 数组（role ∈ user/assistant/system，至少一条）。
func (h *Handler) adminModelChat(w http.ResponseWriter, r *http.Request) {
	if h.probeHandler == nil {
		respondError(w, http.StatusServiceUnavailable, "model chat unavailable")
		return
	}
	var req modelChatRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		respondError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Messages) == 0 {
		respondError(w, http.StatusBadRequest, "messages is required")
		return
	}
	for _, m := range req.Messages {
		switch m.Role {
		case "user", "assistant", "system":
		default:
			respondError(w, http.StatusBadRequest, "invalid message role: "+m.Role)
			return
		}
	}
	path, body, err := probeChatRequestSpec(strings.ToLower(strings.TrimSpace(req.ClientProtocol)), model, req.Messages, req.Stream)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.serveProbeRequest(w, r, path, body, model, req.ClientProtocol, req.Stream)
}

// serveProbeRequest 是探针执行的共用尾段：把已构造好的 /v1 请求发给注入的
// 进程内根路由，并组装前端消费的结果对象（success/status_code/duration_ms/
// first_byte_duration_ms/actual_model/response_text/api_response/error/
// raw_response）。凭据解析、60s 超时、panel-probe 留痕、流式首字节计时对
// 单轮探活与多轮试聊完全一致。
func (h *Handler) serveProbeRequest(w http.ResponseWriter, r *http.Request, path string, body []byte, model, clientProtocol string, stream bool) {
	// 凭据三态：仓空（开放模式）任意占位凭据都能过 authenticate；仓非空
	// 经 probeCredential 取——复用内存里的探针令牌、命中匿名通道行
	//（anonymous 不出示凭据）、或现场铸一条新令牌。三态都拿不到时
	// 给配置引导。
	key := ""
	anonymous := false
	if h.tokens != nil && !h.tokens.Empty() {
		credential, isAnonymous, err := h.probeCredential()
		if err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		key, anonymous = credential, isAnonymous
	}
	if key == "" && !anonymous {
		key = "panel-probe"
	}
	probeCtx, cancel := context.WithTimeout(r.Context(), modelTestTimeout)
	defer cancel()
	probeReq := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(probeCtx)
	if anonymous {
		// 无凭据准入：不带 Authorization 才会命中匿名通道行。
	} else {
		probeReq.Header.Set("Authorization", "Bearer "+key)
	}
	probeReq.Header.Set("Content-Type", "application/json")
	probeReq.Header.Set("X-Client-Request-Id", debuglog.ProbeClientRequestID)
	rec := &probeRecorder{ResponseRecorder: httptest.NewRecorder()}
	started := time.Now()
	h.probeHandler.ServeHTTP(rec, probeReq)

	res := rec.Result()
	respBody := rec.Body.Bytes()
	ok := res.StatusCode >= 200 && res.StatusCode < 300
	out := map[string]any{
		"success":         ok,
		"status_code":     res.StatusCode,
		"duration_ms":     time.Since(started).Milliseconds(),
		"is_streaming":    stream,
		"actual_model":    h.resolvedModel(model),
		"client_protocol": probeProtocolName(clientProtocol),
		"request_id":      res.Header.Get("X-Request-Id"),
	}
	if stream && !rec.firstAt.IsZero() {
		out["first_byte_duration_ms"] = rec.firstAt.Sub(started).Milliseconds()
	}
	if ok {
		if stream {
			out["message"] = "API测试成功（流式）"
		} else {
			out["message"] = "API测试成功"
		}
		// 响应摘要从管线原文合并：SSE 流折叠成可读正文，整段 JSON 取
		// content 块；思考型模型正文为空时退到 reasoning 段兜底。
		merged := mergeResponseBody(string(respBody))
		switch {
		case merged.Content != "":
			out["response_text"] = merged.Content
		case merged.Reasoning != "":
			out["response_text"] = merged.Reasoning
		}
		if !stream {
			var parsed any
			if json.Unmarshal(respBody, &parsed) == nil {
				out["api_response"] = parsed
			}
		}
	} else {
		out["error"] = probeErrorSummary(respBody)
		if trimmed := truncateProbeBody(respBody); trimmed != "" {
			out["raw_response"] = trimmed
		}
	}
	respondOK(w, out)
}

// probeCredential 决定探活出示的凭据（调用方保证仓非空）：内存里已铸的
// 探针令牌仍有效时复用；存在匿名通道行时用无凭据准入（anonymous=true
// 不带 Authorization）；否则现场铸一条 "panel: probe" 令牌并记住明文。
// 探针行被删/停用后下一次探活自动落到匿名或重铸分支。
func (h *Handler) probeCredential() (credential string, anonymous bool, err error) {
	h.probeTokenMu.Lock()
	defer h.probeTokenMu.Unlock()
	if h.probeToken != "" {
		if _, ok := h.tokens.Resolve(h.probeToken); ok {
			return h.probeToken, false, nil
		}
		// 行被删或停用：内存明文作废，转入匿名/重铸分支。
		h.probeToken = ""
	}
	// 匿名通道存在时探活走无凭据准入——零副作用，不留新行。
	if _, ok := h.tokens.Resolve(""); ok {
		return "", true, nil
	}
	plain, err := h.mintProbeToken()
	if err != nil {
		return "", false, fmt.Errorf("no usable credential: mint probe token failed (%v); enable the anonymous channel to run probes", err)
	}
	h.probeToken = plain
	return plain, false, nil
}

// mintProbeToken 铸一条新的探针令牌并清理陈旧探针行：上一 boot 铸的行
// 明文已随进程消失，留着是死行。"panel: probe" 描述是清理锚点——管理员
// 改过描述的行脱离本机制，按普通令牌对待。
func (h *Handler) mintProbeToken() (string, error) {
	t := &authtoken.Token{Description: "panel: probe", IsActive: true}
	plain, err := h.tokens.Create(t)
	if err != nil {
		return "", err
	}
	for _, old := range h.tokens.List() {
		if old.ID != t.ID && old.Description == "panel: probe" {
			if err := h.tokens.Delete(old.ID); err != nil {
				slog.Warn("delete stale probe token failed", "id", old.ID, "error", err)
			}
		}
	}
	return plain, nil
}

// probeProtocolName 把请求里的 client_protocol 归一到三协议名。
func probeProtocolName(clientProtocol string) string {
	switch strings.ToLower(strings.TrimSpace(clientProtocol)) {
	case "openai":
		return "openai"
	case "codex":
		return "codex"
	default:
		return "anthropic"
	}
}

// truncateProbeBody 截断回传给前端的原始响应体。
func truncateProbeBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	if len(s) > probeRawBodyLimit {
		s = s[:probeRawBodyLimit] + "\n...(truncated)"
	}
	return s
}

// probeErrorSummary 从探针错误响应里摘一句可读原因：优先 error.message
// （Anthropic 形态错误体），摘不到就截原文前 200 字节。
func probeErrorSummary(body []byte) string {
	var shaped struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Stage string `json:"stage"`
	}
	if err := json.Unmarshal(body, &shaped); err == nil && shaped.Error.Message != "" {
		if shaped.Stage != "" {
			return shaped.Stage + ": " + shaped.Error.Message
		}
		return shaped.Error.Message
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
