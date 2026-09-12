// 本文件实现 RequestMessages 与 Devin Connect RPC 的双向转换。
//
// Package devin 负责一次 Devin GetChatMessage 调用及其响应事件转换，不执行工具或 agent loop。
package devin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/llm"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	clientName    = "chisel"
	clientVersion = "3000.2.17"
)

// Config 保存 Devin adapter 的固定上游配置。
type Config struct {
	// BaseURL 是 Devin Connect 服务的基础地址。
	BaseURL string
	// Token 是 Devin session token；不会写入日志。
	Token string
	// Model 是 Devin chat model UID。
	Model string
	// Proxy 是可选的 HTTP/HTTPS/SOCKS5 代理地址；为空时直连或走系统环境变量。
	Proxy string
	// ForceHTTP1 为 true 时强制 HTTP/1.1，每请求独立连接，避免 HTTP/2 单连接多 stream 并发瓶颈。
	ForceHTTP1 bool
	// Aliases 是客户端模型名到上游真实 UID 的映射；命中时请求模型被重写。
	Aliases map[string]string
}

// Adapter 调用 Devin 的 ApiServerService/GetChatMessage。
type Adapter struct {
	config         Config
	client         devinprotoconnect.ApiServerServiceClient
	apiClient      devinprotoconnect.ApiServerServiceClient
	modelsMu       sync.RWMutex
	models         []adapter.ModelInfo
	modelsExpiry   time.Time
	modelsCacheTTL time.Duration
}

var _ adapter.Adapter = (*Adapter)(nil)

// New 创建 Devin adapter。
func New(config Config) (*Adapter, error) {
	if strings.TrimSpace(config.BaseURL) == "" {
		return nil, errors.New("devin base URL is required")
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, errors.New("devin token is required")
	}
	if strings.TrimSpace(config.Model) == "" {
		return nil, errors.New("devin model is required")
	}
	base, err := httpproxy.NewTransport(config.Proxy, config.ForceHTTP1)
	if err != nil {
		return nil, fmt.Errorf("create proxy transport: %w", err)
	}
	transport := &authTransport{base: base, token: config.Token}

	// SSE 流需要长期保持连接，不能设置 Client.Timeout；
	// 但 Transport 层的 ResponseHeaderTimeout 已限制首包等待时间。
	streamClient := devinprotoconnect.NewApiServerServiceClient(&http.Client{Transport: transport}, config.BaseURL)

	// 普通 API 调用（如模型目录）设置整体超时，避免慢请求长时间占用 goroutine；
	// 需要大于 ResponseHeaderTimeout，给 body 读取留余量。
	apiHTTPClient := &http.Client{Transport: transport, Timeout: 610 * time.Second}
	apiClient := devinprotoconnect.NewApiServerServiceClient(apiHTTPClient, config.BaseURL)

	return &Adapter{
		config:         config,
		client:         streamClient,
		apiClient:      apiClient,
		modelsCacheTTL: 5 * time.Minute,
	}, nil
}

// Stream 将一份中间请求转换为 Devin RPC，并返回一份中间响应事件流。
func (adapter *Adapter) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("validate Devin request: %w", err)
	}
	request = sanitizeRequest(request)
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = adapter.config.Model
	}
	if alias, ok := adapter.config.Aliases[model]; ok && strings.TrimSpace(alias) != "" {
		model = strings.TrimSpace(alias)
	}
	if err := adapter.validateImagesForModel(request, model); err != nil {
		return nil, err
	}
	cfg := adapter.config
	cfg.Model = model
	protoRequest, err := buildRequest(request, cfg)
	if err != nil {
		return nil, err
	}
	recorder := debuglog.FromContext(ctx)
	recordProtoJSON(recorder, "03-devin-request.json", protoRequest)
	stream, err := adapter.getChatMessageWithRetry(ctx, protoRequest)
	if err != nil {
		recorder.WriteError("devin_connect", err)
		// 透传上游 Connect 错误原文，不包一层模糊前缀。
		return nil, connectError(err)
	}
	return &responseStream{upstream: stream, decoder: newResponseDecoder(model, request.StopSequences), recorder: recorder}, nil
}

// maxConnectAttempts 是 GetChatMessage 建立阶段对瞬时传输错误的最大尝试次数。
const maxConnectAttempts = 3

// getChatMessageWithRetry 在流建立前重试瞬时传输错误（EOF/连接重置/超时）。
// 只对建立阶段重试：流一旦建立，错误通过事件流上报，不再重发请求。
func (adapter *Adapter) getChatMessageWithRetry(ctx context.Context, protoRequest *devinproto.GetChatMessageRequest) (*connect.ServerStreamForClient[devinproto.GetChatMessageResponse], error) {
	var lastErr error
	for attempt := 0; attempt < maxConnectAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		stream, err := adapter.client.GetChatMessage(ctx, connect.NewRequest(protoRequest))
		if err == nil {
			return stream, nil
		}
		lastErr = err
		if !isTransientConnectError(err) {
			break
		}
	}
	return nil, lastErr
}

// isTransientConnectError 判断建立阶段错误是否值得重试：
// 只对非 Connect 协议的传输错误（EOF、连接重置、超时）重试。
// 上游 unavailable 实测是确定性语义错误（router 直连、未开放端点），
// 文案里的 "try again later" 是固定模板，重试永远得到同样的失败；
// 其余 Connect code 均为语义错误，同样不重试。
func isTransientConnectError(err error) bool {
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		return false
	}
	return true
}

// validateImagesForModel 在本地尽早拒绝「无视觉能力模型 + 图片」组合，错误信息对客户端可读。
// 模型目录缓存中有该模型时以目录的 supports_images 为准（上游实测确实回
// invalid_argument），目录未覆盖时退回前缀启发式。
func (adapter *Adapter) validateImagesForModel(request llm.RequestMessages, model string) error {
	if !requestHasImages(request) {
		return nil
	}
	supported, known := adapter.catalogSupportsImages(model)
	if !known {
		supported = modelLikelySupportsImages(model)
	}
	if !supported {
		return fmt.Errorf("model %q does not support image inputs (supports_images=false); use a vision-capable model or remove images", model)
	}
	return nil
}

// catalogSupportsImages 查询模型目录缓存中该 uid 的图片能力。
// 第二个返回值表示目录是否包含该模型。
func (adapter *Adapter) catalogSupportsImages(model string) (supported bool, known bool) {
	adapter.modelsMu.RLock()
	defer adapter.modelsMu.RUnlock()
	for _, m := range adapter.models {
		if m.ID == model {
			return m.SupportsImages, true
		}
	}
	return false, false
}

func requestHasImages(request llm.RequestMessages) bool {
	for _, message := range request.Messages {
		var content []llm.Content
		switch m := message.(type) {
		case llm.UserMessage:
			content = m.Content
		case llm.ToolResultMessage:
			content = m.Content
		default:
			continue
		}
		for _, block := range content {
			if _, ok := block.(llm.ImageContent); ok {
				return true
			}
		}
	}
	return false
}

// modelLikelySupportsImages 用已知无视觉模型名单；不确定时放行让上游裁决。
func modelLikelySupportsImages(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return true
	}
	// 与 GetCascadeModelConfigs.supports_images=false 的常见 uid 对齐。
	noVisionPrefixes := []string{
		"glm-5-2", "glm-5", "glm-4.7", "glm-4-7", "glm-4",
		"deepseek", "kimi-k2", "qwen3-coder",
	}
	for _, p := range noVisionPrefixes {
		if m == p || strings.HasPrefix(m, p+"-") || strings.HasPrefix(m, p+"_") {
			return false
		}
	}
	if strings.HasPrefix(m, "o1") || strings.HasPrefix(m, "o3-mini") || strings.HasPrefix(m, "o4-mini") {
		return false
	}
	return true
}

// connectError 提取 Connect 错误的 code + message，原样返回给 HTTP 客户端。
func connectError(err error) error {
	if err == nil {
		return nil
	}
	var connectErr *connect.Error
	if errors.As(err, &connectErr) {
		msg := strings.TrimSpace(connectErr.Message())
		if msg == "" {
			msg = connectErr.Error()
		}
		return fmt.Errorf("%s: %s", connectErr.Code(), msg)
	}
	return err
}

// ListModels 通过 GetCliModelConfigs 拉取可用模型目录，结果带 TTL 缓存。
// CLI 版响应比 Cascade 版多 subagent_default_model_uid/default_override_model_config，
// 且 modelInfo.modelFeatures 提供 tool_calls/thinking/parallel 能力位。
func (a *Adapter) ListModels(ctx context.Context) ([]adapter.ModelInfo, error) {
	a.modelsMu.RLock()
	if a.models != nil && time.Now().Before(a.modelsExpiry) {
		cached := a.models
		a.modelsMu.RUnlock()
		return cached, nil
	}
	a.modelsMu.RUnlock()

	resp, err := a.apiClient.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: &devinproto.ExaCodeiumCommonPb_Metadata{
			ApiKey:           proto.String(a.config.Token),
			ExtensionName:    proto.String(clientName),
			ExtensionVersion: proto.String(clientVersion),
			IdeName:          proto.String(clientName),
			IdeVersion:       proto.String(clientVersion),
			Locale:           proto.String("en"),
			Os:               proto.String("win"),
		},
	}))
	if err != nil {
		return nil, fmt.Errorf("Devin GetCliModelConfigs: %w", err)
	}
	now := time.Now().Unix()
	models := make([]adapter.ModelInfo, 0, len(resp.Msg.GetClientModelConfigs()))
	seen := make(map[string]struct{}, len(resp.Msg.GetClientModelConfigs()))
	for _, c := range resp.Msg.GetClientModelConfigs() {
		if c.GetDisabled() {
			continue
		}
		uid := c.GetModelUid()
		if uid == "" && c.GetModelOrAlias() != nil {
			uid = c.GetModelOrAlias().GetModelUid()
		}
		if uid == "" {
			continue
		}
		if _, ok := seen[uid]; ok {
			continue
		}
		seen[uid] = struct{}{}
		ownedBy := "devin"
		if p := c.GetProvider().String(); p != "" {
			if i := strings.LastIndex(p, "_"); i >= 0 && i+1 < len(p) {
				ownedBy = strings.ToLower(p[i+1:])
			}
		}
		info := adapter.ModelInfo{
			ID: uid, Created: now, OwnedBy: ownedBy, SupportsImages: c.GetSupportsImages(),
			ContextTokens: int(c.GetMaxTokens()),
		}
		if modelInfo := c.GetModelInfo(); modelInfo != nil {
			info.MaxOutputTokens = int(modelInfo.GetMaxOutputTokens())
			if info.ContextTokens == 0 {
				info.ContextTokens = int(modelInfo.GetMaxTokens())
			}
			if features := modelInfo.GetModelFeatures(); features != nil {
				info.SupportsToolCalls = features.GetSupportsToolCalls()
				info.SupportsParallelToolCalls = features.GetSupportsParallelToolCalls()
				info.SupportsThinking = features.GetSupportsThinking()
				info.PreserveThinking = features.GetPreserveThinking()
				if !info.SupportsImages {
					info.SupportsImages = features.GetSupportsImages()
				}
			}
		}
		models = append(models, info)
	}
	// 用户显式配置的 model（如 gpt5.6）即使不在 Devin 返回的列表中，也应可被发现和调用。
	if configured := strings.TrimSpace(a.config.Model); configured != "" {
		if _, ok := seen[configured]; !ok {
			models = append(models, adapter.ModelInfo{
				ID: configured, Created: now, OwnedBy: "devin",
				// 配置模型无法从 Devin 获取图片能力，默认按支持图片处理更友好。
				SupportsImages: true,
			})
		}
	}

	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	// 请求期间可能有其他请求已写入缓存，避免覆盖更热的数据。
	if a.models != nil && time.Now().Before(a.modelsExpiry) {
		return a.models, nil
	}
	a.models = models
	a.modelsExpiry = time.Now().Add(a.modelsCacheTTL)
	return models, nil
}

type authTransport struct {
	base  http.RoundTripper
	token string
}

func (transport *authTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	clone := request.Clone(request.Context())
	clone.Header.Set("Authorization", "Basic "+transport.token+"-"+transport.token)
	return transport.base.RoundTrip(clone)
}

func buildRequest(request llm.RequestMessages, config Config) (*devinproto.GetChatMessageRequest, error) {
	fingerprint, err := randomHex(366)
	if err != nil {
		return nil, fmt.Errorf("generate Devin device fingerprint: %w", err)
	}
	// 上游轨迹标识按会话复用：同一会话的连续请求共享稳定 trajectory/cascade
	// ID，使命中更稳（实测稳定 ~7/8 vs 全随机波动）；缓存匹配本身是
	// 「账号 + 内容前缀」键控，ID 不参与匹配。
	trajectoryID, cascadeID := deriveSessionIDs(request)
	executionID := randomUUID()
	metadata := &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(config.Token),
		ExtensionName:    proto.String(clientName),
		ExtensionVersion: proto.String(clientVersion),
		IdeName:          proto.String(clientName),
		IdeVersion:       proto.String(clientVersion),
		Locale:           proto.String("en"),
		Os:               proto.String("mac"),
		F:                proto.String(fingerprint),
	}
	completion := &devinproto.ExaCodeiumCommonPb_CompletionConfiguration{
		NumCompletions: proto.Uint64(1),
		MaxTokens:      proto.Uint64(128000),
		MaxNewlines:    proto.Uint64(400),
		Temperature:    proto.Float64(1),
		TopK:           proto.Uint64(40),
		TopP:           proto.Float64(0.95),
	}
	// 客户端显式提供的采样参数透传到上游；缺省保持 CLI 默认值。
	if request.MaxTokens != nil && *request.MaxTokens > 0 {
		completion.MaxTokens = proto.Uint64(uint64(*request.MaxTokens))
	}
	if request.Temperature != nil {
		completion.Temperature = request.Temperature
	}
	if request.TopP != nil {
		completion.TopP = request.TopP
	}
	if request.TopK != nil {
		completion.TopK = proto.Uint64(uint64(*request.TopK))
	}
	if len(request.StopSequences) > 0 {
		completion.StopPatterns = request.StopSequences
	}
	if request.Seed != nil {
		completion.Seed = proto.Uint64(uint64(*request.Seed))
	}
	result := &devinproto.GetChatMessageRequest{
		Metadata: metadata,
		Prompt:   proto.String(withToolDescriptions(request.SystemPrompt, request.Tools)),
		// 上游 prompt 前缀缓存：system prompt 是稳定前缀，标记 EPHEMERAL 断点。
		SystemPromptCacheOptions: ephemeralCacheOptions(),
		ChatModelUid:             proto.String(config.Model),
		RequestType:              devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration:            completion,
		TrajectoryReference: &devinproto.ExaCortexPb_CortexTrajectoryReference{
			TrajectoryId:   proto.String(trajectoryID),
			TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
			StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
		},
		CascadeId:   proto.String(cascadeID),
		PlannerMode: devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
		ExecutionId: proto.String(executionID),
	}
	// 上游实测：option_name 合法值为 none/auto/required；Anthropic 的 "any"
	// 在本层已归一为 required。auto 不发送，与上游缺省行为一致。
	if choice := request.ToolChoice; choice != nil {
		switch choice.Mode {
		case llm.ToolChoiceNone, llm.ToolChoiceRequired:
			result.ToolChoice = &devinproto.ExaChatPb_ChatToolChoice{
				Choice: &devinproto.ExaChatPb_ChatToolChoice_OptionName{OptionName: string(choice.Mode)},
			}
		case llm.ToolChoiceNamed:
			result.ToolChoice = &devinproto.ExaChatPb_ChatToolChoice{
				Choice: &devinproto.ExaChatPb_ChatToolChoice_ToolName{ToolName: choice.ToolName},
			}
		}
	}
	// 上游接受但实测不执行该约束（并行调用照常发出），仅形状对齐。
	if request.DisableParallelToolCalls {
		result.DisableParallelToolCalls = proto.Bool(true)
	}
	// Devin/Cascade 只可靠接受「当前轮」图片；历史图进 Images 会 invalid_argument。
	// 当前轮 = 最后一条 AssistantMessage 之后的所有 user/tool 消息。
	// Anthropic 客户端常把 image 和 tool_result 放在同一条 user 消息里，
	// 解码后拆成 UserMessage + ToolResultMessage 两条；仅挂最后一条会丢失图片。
	lastAssistantIndex := -1
	for index, message := range request.Messages {
		if _, ok := message.(llm.AssistantMessage); ok {
			lastAssistantIndex = index
		}
	}
	for index, message := range request.Messages {
		converted, err := convertMessage(message, index > lastAssistantIndex)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		result.ChatMessagePrompts = append(result.ChatMessagePrompts, converted...)
	}
	// 上游要求 call→result 紧邻配对：assistant 发出的每个 tool call 必须紧跟
	// 它的 TOOL 结果，否则 invalid_argument。客户端历史（OpenAI/Anthropic）是
	// 「全部调用 → 全部结果」的分组结构，这里按 call id 重排成交错配对。
	result.ChatMessagePrompts = pairToolCallsWithResults(result.ChatMessagePrompts)
	result.ChatMessagePrompts = demoteOrphanToolResults(result.ChatMessagePrompts)
	for _, tool := range request.Tools {
		converted, err := convertToolDefinition(tool)
		if err != nil {
			return nil, err
		}
		result.Tools = append(result.Tools, converted)
	}
	// 最后一条消息标记 EPHEMERAL 断点：缓存到此为止的全部历史前缀，
	// 下一轮新消息追加在断点后即可命中缓存。
	if n := len(result.ChatMessagePrompts); n > 0 {
		result.ChatMessagePrompts[n-1].PromptCacheOptions = ephemeralCacheOptions()
	}
	return result, nil
}

// ephemeralCacheOptions 返回上游 prompt 缓存的 EPHEMERAL 断点标记。
func ephemeralCacheOptions() *devinproto.ExaChatPb_PromptCacheOptions {
	return &devinproto.ExaChatPb_PromptCacheOptions{
		Type: devinproto.ExaChatPb_CacheControlType_ExaChatPb_CacheControlType_CACHE_CONTROL_TYPE_EPHEMERAL.Enum(),
	}
}

// deriveSessionIDs 为一次请求派生上游 trajectory/cascade ID。
// SessionKey（CC metadata.user_id 内含 session_id、Codex prompt_cache_key
// 为线程级）本身即会话级标识，直接做种——压缩改写消息内容也不影响轨迹
// 连续性。无 SessionKey 时退回「系统提示头 4KB + 首条消息文本头 1KB」
// 内容哈希：同一会话多轮回放前缀不变 → 稳定，不同会话 → 自然分散。
func deriveSessionIDs(request llm.RequestMessages) (trajectoryID string, cascadeID string) {
	var seed strings.Builder
	if request.SessionKey != "" {
		seed.WriteString(request.SessionKey)
	} else {
		head := request.SystemPrompt
		if len(head) > 4096 {
			head = head[:4096]
		}
		seed.WriteString(head)
		for _, message := range request.Messages {
			text := firstMessageText(message)
			if text == "" {
				continue
			}
			if len(text) > 1024 {
				text = text[:1024]
			}
			seed.WriteByte(0)
			seed.WriteString(text)
			break
		}
	}
	sum := sha256.Sum256([]byte(seed.String()))
	return uuidFromBytes(sum[:16]), uuidFromBytes(sum[16:32])
}

// firstMessageText 提取消息的首个文本块，用于会话种子。
func firstMessageText(message llm.Message) string {
	var content []llm.Content
	switch typed := message.(type) {
	case llm.UserMessage:
		content = typed.Content
	case llm.AssistantMessage:
		content = typed.Content
	case llm.ToolResultMessage:
		content = typed.Content
	}
	for _, block := range content {
		if text, ok := block.(llm.TextContent); ok && text.Text != "" {
			return text.Text
		}
	}
	return ""
}

// uuidFromBytes 将 16 字节格式化为 UUID 字符串（version/variant 位固定）。
func uuidFromBytes(b []byte) string {
	var out [16]byte
	copy(out[:], b)
	out[6] = (out[6] & 0x0f) | 0x40
	out[8] = (out[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", out[0:4], out[4:6], out[6:8], out[8:10], out[10:16])
}

// convertMessage 将中间消息转为 Devin ChatMessagePrompt。
// attachImages 为 true 时才把 ImageContent 写入 Images（仅最新用户轮）；历史图改成文本占位。
func convertMessage(message llm.Message, attachImages bool) ([]*devinproto.ExaChatPb_ChatMessagePrompt, error) {
	switch message := message.(type) {
	case llm.UserMessage:
		return []*devinproto.ExaChatPb_ChatMessagePrompt{promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER, message.Content, attachImages)}, nil
	case llm.AssistantMessage:
		// Wire 实证（WindsurfAPI）：助手轮 = 可选文本消息 + 每个工具调用各一条
		// 独立消息。工具调用消息不写 prompt 字段（字段 3 缺席而非空串）；
		// thinking(#11) 出现在每条 assistant 消息上。
		var signature string
		var redacted bool
		var text, thinking strings.Builder
		var calls []llm.ToolCall
		for _, block := range message.Content {
			switch typed := block.(type) {
			case llm.TextContent:
				text.WriteString(typed.Text)
			case llm.ThinkingContent:
				// 一条 assistant 消息可带多个 thinking 块（interleaved）；
				// wire 模型每 prompt 只有单份 thinking，顺序拼接、签名取最后非空。
				if thinking.Len() > 0 && typed.Thinking != "" {
					thinking.WriteString("\n")
				}
				thinking.WriteString(typed.Thinking)
				if typed.ThinkingSignature != "" {
					signature = typed.ThinkingSignature
				}
				redacted = redacted || typed.Redacted
			case llm.ToolCall:
				calls = append(calls, typed)
			}
		}
		var prompts []*devinproto.ExaChatPb_ChatMessagePrompt
		if text.Len() > 0 {
			prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
				MessageId: proto.String(randomID()),
				Source:    assistantSource.Enum(),
				Prompt:    proto.String(text.String()),
			}
			if thinking.Len() > 0 || redacted {
				if thinking.Len() > 0 {
					prompt.Thinking = proto.String(thinking.String())
				}
				if signature != "" {
					prompt.Signature = proto.String(signature)
				}
				prompt.ThinkingRedacted = proto.Bool(redacted)
			}
			prompts = append(prompts, prompt)
		}
		for index, call := range calls {
			prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
				MessageId: proto.String(randomID()),
				Source:    assistantSource.Enum(),
				ToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
					Id:            proto.String(call.ID),
					Name:          proto.String(call.Name),
					ArgumentsJson: proto.String(string(call.Arguments)),
				}},
			}
			if thinking.Len() > 0 || redacted {
				if thinking.Len() > 0 {
					prompt.Thinking = proto.String(thinking.String())
				}
				// 无文本消息时签名挂到首条工具调用消息，避免丢失。
				if index == 0 && text.Len() == 0 {
					if signature != "" {
						prompt.Signature = proto.String(signature)
					}
					prompt.ThinkingRedacted = proto.Bool(redacted)
				}
			}
			prompts = append(prompts, prompt)
		}
		// 完全空的助手消息会诱发上游反复返回空回复，跳过。
		return prompts, nil
	case llm.ToolResultMessage:
		prompt := promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL, message.Content, attachImages)
		if prompt.GetPrompt() == "" {
			// 上游不接受空的工具结果文本，对齐 WindsurfAPI 的占位。
			prompt.Prompt = proto.String("[tool result]")
		}
		prompt.ToolCallId = proto.String(message.ToolCallID)
		prompt.ToolResultIsError = proto.Bool(message.IsError)
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	default:
		return nil, fmt.Errorf("unsupported message type %T", message)
	}
}

// assistantSource 是助手消息在 Devin wire 上的来源枚举（上游命名为 SYSTEM，值 2）。
var assistantSource = devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM

// pairToolCallsWithResults 把「连续调用消息 + 连续结果消息」的分组序列
// 重排为 call_i, result_i, call_j, result_j 的交错序列。
// 已配对的交错序列保持不变；找不到匹配结果的调用原样保留位置。
func pairToolCallsWithResults(prompts []*devinproto.ExaChatPb_ChatMessagePrompt) []*devinproto.ExaChatPb_ChatMessagePrompt {
	toolSource := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
	isCallPrompt := func(p *devinproto.ExaChatPb_ChatMessagePrompt) bool {
		return p.GetSource() == assistantSource && len(p.GetToolCalls()) > 0
	}
	isResultPrompt := func(p *devinproto.ExaChatPb_ChatMessagePrompt) bool {
		return p.GetSource() == toolSource
	}
	var out []*devinproto.ExaChatPb_ChatMessagePrompt
	for i := 0; i < len(prompts); {
		if !isCallPrompt(prompts[i]) {
			out = append(out, prompts[i])
			i++
			continue
		}
		var calls []*devinproto.ExaChatPb_ChatMessagePrompt
		for i < len(prompts) && isCallPrompt(prompts[i]) {
			calls = append(calls, prompts[i])
			i++
		}
		byID := make(map[string]*devinproto.ExaChatPb_ChatMessagePrompt)
		j := i
		for j < len(prompts) && isResultPrompt(prompts[j]) {
			byID[prompts[j].GetToolCallId()] = prompts[j]
			j++
		}
		consumed := make(map[string]struct{}, len(calls))
		for _, call := range calls {
			out = append(out, call)
			id := call.GetToolCalls()[0].GetId()
			if result, ok := byID[id]; ok {
				out = append(out, result)
				consumed[id] = struct{}{}
			}
		}
		// 未能配对的孤立结果按原序保留，不丢消息。
		for k := i; k < j; k++ {
			if _, ok := consumed[prompts[k].GetToolCallId()]; !ok {
				out = append(out, prompts[k])
			}
		}
		i = j
	}
	return out
}

// demoteOrphanToolResults 把找不到对应 tool call 的孤立 TOOL 结果
// （客户端压缩丢掉 function_call 时产生）降级为 USER 文本消息。
// 上游对无配对的 TOOL prompt 返回 invalid_argument；降级保住结果内容。
func demoteOrphanToolResults(prompts []*devinproto.ExaChatPb_ChatMessagePrompt) []*devinproto.ExaChatPb_ChatMessagePrompt {
	callIDs := make(map[string]struct{})
	for _, prompt := range prompts {
		for _, call := range prompt.GetToolCalls() {
			callIDs[call.GetId()] = struct{}{}
		}
	}
	toolSource := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL
	userSource := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER
	for index, prompt := range prompts {
		if prompt.GetSource() != toolSource {
			continue
		}
		if _, ok := callIDs[prompt.GetToolCallId()]; ok {
			continue
		}
		demoted := &devinproto.ExaChatPb_ChatMessagePrompt{
			MessageId: proto.String(randomID()),
			Source:    userSource.Enum(),
			Prompt:    proto.String("[tool result, original call lost]\n" + prompt.GetPrompt()),
		}
		demoted.PromptCacheOptions = prompt.GetPromptCacheOptions()
		demoted.Images = prompt.GetImages()
		prompts[index] = demoted
	}
	return prompts
}

func promptForContent(source devinproto.ExaCodeiumCommonPb_ChatMessageSource, content []llm.Content, attachImages bool) *devinproto.ExaChatPb_ChatMessagePrompt {
	prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randomID()),
		Source:    source.Enum(),
	}
	var text strings.Builder
	for _, block := range content {
		switch block := block.(type) {
		case llm.TextContent:
			text.WriteString(block.Text)
		case llm.ThinkingContent:
			prompt.Thinking = proto.String(block.Thinking)
			if block.ThinkingSignature != "" {
				prompt.Signature = proto.String(block.ThinkingSignature)
			}
			prompt.ThinkingRedacted = proto.Bool(block.Redacted)
		case llm.ImageContent:
			if !attachImages {
				// 与 WindsurfAPI 一致：历史图不进 Images，避免上游 invalid_argument。
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString("[Image omitted from history]")
				continue
			}
			// Devin/Windsurf ImageData：纯 base64（无 data: 前缀）+ mime_type。
			data := block.Data
			if strings.HasPrefix(data, "data:") {
				if _, encoded, ok := strings.Cut(data, ","); ok {
					data = encoded
				}
			}
			mimeType := block.MIMEType
			if mimeType == "" {
				mimeType = "image/png"
			}
			prompt.Images = append(prompt.Images, &devinproto.ExaCodeiumCommonPb_ImageData{
				Base64Data: proto.String(data),
				MimeType:   proto.String(mimeType),
			})
		}
	}
	prompt.Prompt = proto.String(text.String())
	return prompt
}

func randomID() string {
	return randomUUID()
}

func randomUUID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// responseStream 从 Connect 上游按需读取帧并依次返回 decoder 生成的事件。
type responseStream struct {
	// upstream 是 Devin Connect 返回的服务端流。
	upstream devinResponseReceiver
	// decoder 将一个 Devin protobuf 帧转换为零个或多个中间响应事件。
	decoder *responseDecoder
	// recorder 记录 Devin 原始响应帧；nil 表示禁用调试日志。
	recorder *debuglog.Recorder
	// started 表示是否已经请求 decoder 产生 start 事件。
	started bool
	// pendingStart 保存 decoder.start() 生成但尚未下发的事件。
	// start 推迟到第一批真实事件前发出：上游在产出内容前报错时，
	// 首个对外事件是 error，HTTP 层才能返回真实错误状态码，
	// 而不是已提交的 200 + SSE error（下游网关会把后者误判为渠道故障）。
	pendingStart []llm.ResponseEvent
	// finished 表示 decoder 已经生成最终事件，不再读取上游。
	finished bool
	// queue 保存已经转换、等待调用方读取的中间响应事件。
	queue []llm.ResponseEvent
}

// devinResponseReceiver 描述 responseStream 消费 Devin 服务端流所需的最小能力。
type devinResponseReceiver interface {
	// Receive 前进到下一帧，并报告是否成功取得消息。
	Receive() bool
	// Msg 返回最近一次成功取得的响应帧。
	Msg() *devinproto.GetChatMessageResponse
	// Err 返回流结束时的错误；正常 EOF 返回 nil。
	Err() error
}

func (stream *responseStream) Recv(ctx context.Context) (llm.ResponseEvent, error) {
	for len(stream.queue) == 0 && !stream.finished {
		if err := ctx.Err(); err != nil {
			return llm.ResponseEvent{}, err
		}
		if !stream.started {
			// start() 初始化 decoder.partial，必须先于 decode 调用；
			// 事件本身扣留在 pendingStart，等待第一批真实事件一起下发。
			stream.started = true
			stream.pendingStart = stream.decoder.start()
			continue
		}
		if !stream.upstream.Receive() {
			stream.queue = stream.release(stream.decoder.finish(stream.upstream.Err()))
			stream.finished = true
			break
		}
		response := stream.upstream.Msg()
		recordProtoJSON(stream.recorder, "04-devin-response.jsonl", response)
		stream.queue = stream.release(stream.decoder.decode(response))
		stream.finished = stream.decoder.finished
	}
	if len(stream.queue) > 0 {
		event := stream.queue[0]
		stream.queue = stream.queue[1:]
		return event, nil
	}
	return llm.ResponseEvent{}, io.EOF
}

// release 把 decoder 产出的第一批事件交给调用方：非错误批次前置扣留的
// start 事件；若首批就是错误事件（上游在产出内容前失败），丢弃 start，
// 让错误成为流的第一个对外事件。
func (stream *responseStream) release(events []llm.ResponseEvent) []llm.ResponseEvent {
	if len(events) == 0 || len(stream.pendingStart) == 0 {
		return events
	}
	start := stream.pendingStart
	stream.pendingStart = nil
	if events[0].Type == llm.ResponseEventError {
		return events
	}
	return append(start, events...)
}

func recordProtoJSON(recorder *debuglog.Recorder, name string, message proto.Message) {
	if recorder == nil || message == nil {
		return
	}
	data, err := protojson.Marshal(message)
	if err != nil {
		recorder.WriteError("devin_proto_encode", err)
		return
	}
	if strings.HasSuffix(name, ".jsonl") {
		recorder.AppendValueJSONL(name, json.RawMessage(data))
		return
	}
	recorder.WriteJSON(name, json.RawMessage(data))
}
