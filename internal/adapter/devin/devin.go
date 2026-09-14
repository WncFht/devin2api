// 本文件实现 RequestMessages 与 Devin Connect RPC 的双向转换。
//
// Package devin 负责一次 Devin GetChatMessage 调用及其响应事件转换，不执行工具或 agent loop。
package devin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/upstream"
)

// 默认客户端身份常量与真实 Devin CLI 抓包逐字段对齐；上游若开始按
// extension_version 做版本门（新模型 gate），可在 config 的 devin.client_*
// 覆盖而不必发版。
const (
	defaultClientName    = "chisel"
	defaultClientVersion = "3000.2.17"
	defaultClientOS      = "mac"
	// catalogRetryBackoff 是模型目录拉取失败且错误未带 reset hint 时的
	// 冷却时长；带 hint 时按 hint 冷却（上游何时解除它自己最清楚）。
	catalogRetryBackoff = 30 * time.Second
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
	// ClientName/ClientVersion/ClientOS 是发给上游的 metadata 身份字段；
	// 为空时回落到默认常量（与真实 CLI 抓包一致）。
	ClientName    string
	ClientVersion string
	ClientOS      string
	// MaxRPM 是发往上游 GetChatMessage 的消息速率上限（条/分钟）；
	// <=0 不做主动限速。上游限流冷却闩不受此项影响，始终生效。
	MaxRPM int
	// GateMaxHold/GateDripInterval/GateDefaultLatch 是冷却闩参数：
	// 闩外排队允许的最长等待、闩内滴灌探针的放行间隔、上游未带
	// reset hint 时的兜底闩时长；<=0 时闸门用默认值。
	GateMaxHold      time.Duration
	GateDripInterval time.Duration
	GateDefaultLatch time.Duration
	// GateStatePath 非空时冷却闩截止时刻落盘到该文件，进程重启后
	// 未过期的闩被恢复——上游限流器把被拒尝试计入窗口，闩内重启
	// 裸发会把限流续长。
	GateStatePath string
	// TokenSource 可选：unauthenticated 时回调重新解析凭据。
	// Devin CLI 会续期改写 credentials.toml，静态缓存的 token 会静默失效；
	// 回调应重读同一来源（配置文件或凭证文件），返回空表示无新凭据。
	TokenSource func() string
	// ModelAssignmentJWT 是 router uid 经 AssignModel 解析出的绑定 jwt，
	// 由 Stream 按请求设置（与 Token/Model 一样是每次调用覆盖的字段）。
	ModelAssignmentJWT string
}

// clientIdentity 返回请求要携带的客户端身份；空字段回落到与真实
// Devin CLI 抓包一致的默认值。
func (config Config) clientIdentity() (name, version, os string) {
	name = strings.TrimSpace(config.ClientName)
	if name == "" {
		name = defaultClientName
	}
	version = strings.TrimSpace(config.ClientVersion)
	if version == "" {
		version = defaultClientVersion
	}
	os = strings.TrimSpace(config.ClientOS)
	if os == "" {
		os = defaultClientOS
	}
	return name, version, os
}

// Adapter 调用 Devin 的 ApiServerService/GetChatMessage。
type Adapter struct {
	// configMu 保护 config：ApplyConfig 热路径整体换值，读侧经
	// currentConfig 取快照。proxy/base_url/force_http1 等烤进
	// transport 的字段虽在结构里但换值不生效（见 ApplyConfig）。
	configMu sync.RWMutex
	config   Config
	// token 是当前生效的上游凭据：unauthenticated 自愈会原地更新，
	// transport 经 tokenFunc 每次请求读取，无需重建 HTTP 客户端。
	tokenMu sync.RWMutex
	token   string
	// streamClient 无 Client.Timeout（SSE 长连接靠 Transport 层超时兜底）；
	// apiClient 有 610s 整体超时，用于模型目录等普通调用。
	streamClient   devinprotoconnect.ApiServerServiceClient
	apiClient      devinprotoconnect.ApiServerServiceClient
	modelsMu       sync.RWMutex
	models         []adapter.ModelInfo
	modelsExpiry   time.Time
	modelsCacheTTL time.Duration
	// modelsRetryUntil/modelsErr 是目录拉取失败的冷却窗口：失败期间
	// 目录始终为空，不冷却会让每个请求（ensureCatalog）都重试一次
	// GetCliModelConfigs，客户端重试风暴原样穿透到上游（stub 实测
	// 20s 内 1.1 万次）。窗口内有旧缓存回旧值，否则回 modelsErr。
	modelsRetryUntil time.Time
	modelsErr        error
	// gate 是上游消息速率闸门：令牌桶主动限速 + 上游限流冷却闩。
	// 每次 GetChatMessage 发送（含自愈/重开重试）前都要过闸。
	gate *rateGate
	// assignments 缓存 (router uid, cascade id) 的 AssignModel 解析结果：
	// assignment jwt 绑 cascade_id（上游实测），同会话内复用省去
	// 每请求一次的解析往返。
	assignmentsMu sync.Mutex
	assignments   map[string]resolvedAssignment
}

// resolvedAssignment 是 AssignModel 对单个 router uid 的解析结果。
type resolvedAssignment struct {
	modelUID string
	jwt      string
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
	adapter := &Adapter{
		config:         config,
		token:          config.Token,
		modelsCacheTTL: 5 * time.Minute,
		gate: newRateGate(config.MaxRPM,
			config.GateMaxHold, config.GateDripInterval, config.GateDefaultLatch,
			config.GateStatePath),
		assignments: make(map[string]resolvedAssignment),
	}
	transport := upstream.NewBasicAuthTransportFunc(base, adapter.currentToken)

	// SSE 流需要长期保持连接，不能设置 Client.Timeout；
	// 但 Transport 层的 ResponseHeaderTimeout 已限制首包等待时间。
	adapter.streamClient = devinprotoconnect.NewApiServerServiceClient(&http.Client{Transport: transport}, config.BaseURL)

	// 普通 API 调用（如模型目录）设置整体超时，避免慢请求长时间占用 goroutine；
	// 需要大于 ResponseHeaderTimeout，给 body 读取留余量。
	apiHTTPClient := &http.Client{Transport: transport, Timeout: 610 * time.Second}
	adapter.apiClient = devinprotoconnect.NewApiServerServiceClient(apiHTTPClient, config.BaseURL)

	return adapter, nil
}

// currentToken 返回当前生效的上游凭据。
func (adapter *Adapter) currentToken() string {
	adapter.tokenMu.RLock()
	defer adapter.tokenMu.RUnlock()
	return adapter.token
}

// TokenFunc 返回读取当前凭据的函数，供面板等共享同一上游账号的组件
// 跟随 adapter 的 unauthenticated 自愈结果——凭据续期后各方拿到的是
// 同一份新 token，而不是启动时的静态快照。
func (adapter *Adapter) TokenFunc() func() string {
	return adapter.currentToken
}

// currentConfig 返回当前生效配置的读快照。
func (adapter *Adapter) currentConfig() Config {
	adapter.configMu.RLock()
	defer adapter.configMu.RUnlock()
	return adapter.config
}

// Aliases 返回当前生效的模型别名映射，供面板做目录缺席校验。
func (adapter *Adapter) Aliases() map[string]string {
	return adapter.currentConfig().Aliases
}

// GateStats 返回速率闸门状态快照，供面板 stats 端点透出。
func (adapter *Adapter) GateStats() GateStats {
	return adapter.gate.stats()
}

// ApplyConfig 热应用新配置：读侧每次请求取快照的字段（model、aliases、
// client_*）与闸门参数/token 直接换值即生效；烤进 transport 的
// base_url/proxy/force_http1 换值不生效，列入 requiresRestart 由调用方
// 回报。返回的两个列表只含值发生变化的字段。
func (adapter *Adapter) ApplyConfig(next Config) (applied, requiresRestart []string) {
	adapter.configMu.Lock()
	prev := adapter.config
	// 运行时字段不归配置管：状态文件路径与 JWT 缓存沿用旧值。
	next.GateStatePath = prev.GateStatePath
	next.ModelAssignmentJWT = prev.ModelAssignmentJWT
	adapter.config = next
	adapter.configMu.Unlock()

	if prev.Model != next.Model {
		applied = append(applied, "devin.model")
	}
	if !maps.Equal(prev.Aliases, next.Aliases) {
		applied = append(applied, "devin.aliases")
	}
	if prev.ClientName != next.ClientName {
		applied = append(applied, "devin.client_name")
	}
	if prev.ClientVersion != next.ClientVersion {
		applied = append(applied, "devin.client_version")
	}
	if prev.ClientOS != next.ClientOS {
		applied = append(applied, "devin.client_os")
	}
	if prev.Token != next.Token {
		adapter.tokenMu.Lock()
		adapter.token = next.Token
		adapter.tokenMu.Unlock()
		applied = append(applied, "devin.token")
	}
	adapter.gate.setParams(next.MaxRPM, next.GateMaxHold, next.GateDripInterval, next.GateDefaultLatch)
	if prev.MaxRPM != next.MaxRPM {
		applied = append(applied, "devin.max_rpm")
	}
	if prev.GateMaxHold != next.GateMaxHold {
		applied = append(applied, "devin.gate_max_hold_seconds")
	}
	if prev.GateDripInterval != next.GateDripInterval {
		applied = append(applied, "devin.gate_drip_interval_seconds")
	}
	if prev.GateDefaultLatch != next.GateDefaultLatch {
		applied = append(applied, "devin.gate_default_latch_seconds")
	}
	if prev.BaseURL != next.BaseURL {
		requiresRestart = append(requiresRestart, "devin.base_url")
	}
	if prev.Proxy != next.Proxy {
		requiresRestart = append(requiresRestart, "devin.proxy")
	}
	if prev.ForceHTTP1 != next.ForceHTTP1 {
		requiresRestart = append(requiresRestart, "devin.force_http1")
	}
	return applied, requiresRestart
}

// reloadToken 在 unauthenticated 后从 TokenSource 重读凭据；
// 拿到非空且不同的新 token 才视为自愈成功。拿不到时记 Warn——
// 凭据静默失效是排障天敌，进程日志里必须留痕。
func (adapter *Adapter) reloadToken() bool {
	source := adapter.currentConfig().TokenSource
	if source == nil {
		return false
	}
	token := strings.TrimSpace(source())
	if token == "" {
		slog.Warn("upstream unauthenticated but TokenSource returned no token")
		return false
	}
	adapter.tokenMu.Lock()
	defer adapter.tokenMu.Unlock()
	if token == adapter.token {
		slog.Warn("upstream unauthenticated and TokenSource returned the same token; credential refresh did not help")
		return false
	}
	adapter.token = token
	slog.Info("reloaded upstream token after unauthenticated error")
	return true
}

// isUnauthenticated 判断错误是否为上游 unauthenticated（凭据失效）。
func isUnauthenticated(err error) bool {
	var connectErr *connect.Error
	return errors.As(err, &connectErr) && connectErr.Code() == connect.CodeUnauthenticated
}

// Stream 将一份中间请求转换为 Devin RPC，并返回一份中间响应事件流。
func (adapter *Adapter) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("validate Devin request: %w", err)
	}
	request, sanitizeHits := sanitizeRequest(request)
	cfg := adapter.currentConfig()
	model := strings.TrimSpace(request.Model)
	if model == "" {
		model = cfg.Model
	}
	if alias, ok := cfg.Aliases[model]; ok && strings.TrimSpace(alias) != "" {
		model = strings.TrimSpace(alias)
	}
	recorder := debuglog.FromContext(ctx)
	// 目录是 router 判定与能力位校验的依据；懒加载时此处补一次拉取。
	adapter.ensureCatalog(ctx)
	adapter.warnIfModelAbsentFromCatalog(model)
	if err := adapter.validateImagesForModel(request, model); err != nil {
		return nil, err
	}
	model, assignmentJWT, err := adapter.resolveModelRouting(ctx, request, model)
	if err != nil {
		recorder.WriteError("devin_connect", err)
		return nil, err
	}
	cfg = adapter.currentConfig()
	cfg.Model = model
	cfg.Token = adapter.currentToken()
	cfg.ModelAssignmentJWT = assignmentJWT
	protoRequest, repairs, err := buildRequest(request, cfg)
	if err != nil {
		return nil, err
	}
	repairs.SanitizeHits = sanitizeHits
	recorder.SetRepairs(repairs)
	recordProtoJSON(recorder, "03-devin-request.json", protoRequest)
	// attempt 计数区分多次发送：自愈重发与 pre-content reopen 都会重建
	// 请求体，attempt2+ 写独立文件并在 04 里留 retry_attempt 分界行，
	// 否则 04 的帧无法归因到具体哪次发送。
	attempt := 1
	// streamCtx 由 responseStream 持有：看门狗判死或客户端断开时
	// cancel 是唯一打断泵协程内阻塞 Receive 的手段。
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := adapter.getChatMessageWithRetry(streamCtx, protoRequest)
	if err != nil && isUnauthenticated(err) && adapter.reloadToken() {
		// 凭据自愈：CLI 会续期改写 credentials.toml，重读 token 后
		// 用新凭据重建请求重试一次。token 未变化时不重试。
		cfg.Token = adapter.currentToken()
		if rebuilt, _, buildErr := buildRequest(request, cfg); buildErr == nil {
			protoRequest = rebuilt
			attempt++
			recorder.AppendJSONL("04-devin-response.jsonl", "retry_attempt", map[string]any{
				"attempt": attempt,
				"cause":   "unauthenticated: token reloaded",
			})
			recordProtoJSON(recorder, fmt.Sprintf("03-devin-request.attempt%d.json", attempt), protoRequest)
			stream, err = adapter.getChatMessageWithRetry(streamCtx, protoRequest)
		}
	}
	if err != nil {
		cancel()
		// 本地限流闸的拒绝记 rate_gate 与上游真拒（devin_connect）区分：
		// 聚合排障时前者说明根本没碰到上游，后者才是上游配额动作。
		stage := "devin_connect"
		var gateErr *rateGateError
		if errors.As(err, &gateErr) {
			stage = "rate_gate"
		}
		recorder.WriteError(stage, err)
		// 透传上游 Connect 错误原文，不包一层模糊前缀。
		return nil, connectError(err)
	}
	return &responseStream{
		frames:   pumpUpstream(streamCtx, stream),
		cancel:   cancel,
		decoder:  newResponseDecoder(model, request.StopSequences, customToolNames(request.Tools)),
		recorder: recorder,
		gate:     adapter.gate,
		// 上游流建立后、产出任何内容前的失败允许整体重发一次：
		// 传输层断裂与 unauthenticated（凭据自愈）重试能改变结果；
		// 语义错误（invalid_argument 等）重试只会复现同样失败，直接放行。
		reopen: func(cause error, continueEmpty bool) (<-chan upstreamFrame, context.CancelFunc, error) {
			retryRequest := request
			var causeText string
			if continueEmpty {
				// 空 end_turn（有 stopReason 零内容，上游实测存在的退化形态）：
				// 追加 "continue" 用户消息重发一次，让模型在同一上下文续说。
				retryRequest.Messages = append(append([]llm.Message{}, request.Messages...),
					llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "continue"}}})
				causeText = "empty end_turn: continue"
				slog.Warn("reopening stream: upstream ended with empty content")
			} else if isTransientConnectError(cause) {
				causeText = "transport: " + cause.Error()
				slog.Warn("reopening stream: transport error before first content", "error", cause)
			} else if isUnauthenticated(cause) && adapter.reloadToken() {
				causeText = "unauthenticated: token reloaded"
				slog.Info("reopening stream: token reloaded after unauthenticated")
			} else {
				return nil, nil, cause
			}
			retryCtx, retryCancel := context.WithCancel(ctx)
			retryCfg := cfg
			retryCfg.Token = adapter.currentToken()
			rebuilt, _, err := buildRequest(retryRequest, retryCfg)
			var reopened *connect.ServerStreamForClient[devinproto.GetChatMessageResponse]
			if err == nil {
				attempt++
				recorder.AppendJSONL("04-devin-response.jsonl", "retry_attempt", map[string]any{
					"attempt":        attempt,
					"cause":          causeText,
					"continue_empty": continueEmpty,
				})
				recordProtoJSON(recorder, fmt.Sprintf("03-devin-request.attempt%d.json", attempt), rebuilt)
				reopened, err = adapter.getChatMessageWithRetry(retryCtx, rebuilt)
			}
			if err != nil {
				retryCancel()
				return nil, nil, err
			}
			return pumpUpstream(retryCtx, reopened), retryCancel, nil
		},
		newDecoder: func() *responseDecoder {
			return newResponseDecoder(model, request.StopSequences, customToolNames(request.Tools))
		},
	}, nil
}

// maxConnectAttempts 是 GetChatMessage 建立阶段对瞬时传输错误的最大尝试次数。
const maxConnectAttempts = 3

// getChatMessageWithRetry 在流建立前重试瞬时传输错误（EOF/连接重置/超时）。
// 只对建立阶段重试：流一旦建立，错误通过事件流上报，不再重发请求。
func (adapter *Adapter) getChatMessageWithRetry(ctx context.Context, protoRequest *devinproto.GetChatMessageRequest) (*connect.ServerStreamForClient[devinproto.GetChatMessageResponse], error) {
	var lastErr error
	for attempt := 0; attempt < maxConnectAttempts; attempt++ {
		// 每次真实发送（含瞬时错误重试）都要过速率闸：被拒尝试
		// 会推后上游恢复时刻，本地整形是唯一止损点。
		if err := adapter.gate.wait(ctx); err != nil {
			return nil, err
		}
		if attempt > 0 {
			// ±25% 抖动：上游瞬时拥塞时固定节拍的重试会相互叠加。
			base := time.Duration(attempt) * 400 * time.Millisecond
			backoff := time.Duration(float64(base) * (0.75 + 0.5*rand.Float64()))
			select {
			case <-ctx.Done():
				return nil, context.Cause(ctx)
			case <-time.After(backoff):
			}
		}
		stream, err := adapter.streamClient.GetChatMessage(ctx, connect.NewRequest(protoRequest))
		if err == nil {
			return stream, nil
		}
		lastErr = err
		if !isTransientConnectError(err) {
			break
		}
	}
	adapter.gate.noteUpstreamError(lastErr)
	return nil, lastErr
}

// isTransientConnectError 判断建立阶段错误是否值得重试：
// 只对非 Connect 协议的传输错误（EOF、连接重置、超时）重试。
// 上游 unavailable 实测是确定性语义错误（router 直连、未开放端点），
// 文案里的 "try again later" 是固定模板，重试永远得到同样的失败；
// 其余 Connect code 均为语义错误，同样不重试。
func isTransientConnectError(err error) bool {
	var connectErr *connect.Error
	return !errors.As(err, &connectErr)
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

// warnIfModelAbsentFromCatalog 在目录已加载且目标 uid 缺席时记 Warn。
// 实测 alias 指向死模型时上游只回模糊的 permission_denied: an internal
// error occurred——排障只能靠日志里的这条提示定位到 alias 目标。
// 目录未加载或缺席都放行：用户配置的 model 本就可以不在目录里。
func (adapter *Adapter) warnIfModelAbsentFromCatalog(model string) {
	adapter.modelsMu.RLock()
	defer adapter.modelsMu.RUnlock()
	if len(adapter.models) == 0 {
		return
	}
	for _, m := range adapter.models {
		if m.ID == model {
			return
		}
	}
	slog.Warn("model absent from upstream catalog; upstream will likely return a vague permission_denied",
		"model", model, "hint", "check devin.aliases target or bump devin.client_version")
}

// ensureCatalog 尽力保证模型目录已加载：router 判定、图片能力位校验与
// 缺席告警都以目录为依据，目录从未加载过时这些检查静默失效。
// TTL 缓存使命中期的调用只是读锁；拉取失败放行，维持「交给上游裁决」的旧行为。
func (adapter *Adapter) ensureCatalog(ctx context.Context) {
	if _, err := adapter.ListModels(ctx); err != nil {
		slog.Warn("model catalog unavailable; router detection skipped", "error", err)
	}
}

// resolveModelRouting 对目录里标了 is_model_router 的 uid 调 AssignModel
// 解出真实 model_uid 与绑定 cascade_id 的 assignment jwt——router uid
// 直连上游只回 unavailable: third-party model provider，伪装成瞬时错误
// 的永久失败。目录未覆盖该模型时按原样放行，交给上游裁决。
func (adapter *Adapter) resolveModelRouting(ctx context.Context, request llm.RequestMessages, model string) (resolved string, assignmentJWT string, err error) {
	adapter.modelsMu.RLock()
	isRouter := false
	for _, m := range adapter.models {
		if m.ID == model {
			isRouter = m.IsModelRouter
			break
		}
	}
	adapter.modelsMu.RUnlock()
	if !isRouter {
		return model, "", nil
	}
	// jwt 绑 cascade_id：必须用与本请求 wire 一致的派生值。
	_, cascadeID := deriveSessionIDs(request)
	assignment, err := adapter.assignModel(ctx, model, cascadeID)
	if err != nil {
		return "", "", err
	}
	slog.Info("resolved model router via AssignModel", "router", model, "model", assignment.modelUID)
	return assignment.modelUID, assignment.jwt, nil
}

// assignModel 调上游 AssignModel 把 router uid 解析为真实模型 + assignment
// jwt，结果按 (router uid, cascade id) 缓存。错误分类见
// docs/upstream-protocol.md 路由节：非 router uid → invalid_argument，
// 不存在的 router → not_found。
func (adapter *Adapter) assignModel(ctx context.Context, routerUID, cascadeID string) (resolvedAssignment, error) {
	key := routerUID + "|" + cascadeID
	adapter.assignmentsMu.Lock()
	cached, ok := adapter.assignments[key]
	adapter.assignmentsMu.Unlock()
	if ok {
		return cached, nil
	}
	name, version, os := adapter.currentConfig().clientIdentity()
	resp, err := adapter.apiClient.AssignModel(ctx, connect.NewRequest(&devinproto.AssignModelRequest{
		Metadata:       upstream.BuildMetadata(adapter.currentToken(), name, version, os, 366),
		ModelRouterUid: proto.String(routerUID),
		CascadeId:      proto.String(cascadeID),
	}))
	if err != nil {
		return resolvedAssignment{}, fmt.Errorf("AssignModel(%s): %w", routerUID, connectError(err))
	}
	assignment := resp.Msg.GetAssignment()
	resolved := strings.TrimSpace(assignment.GetModelUid())
	if resolved == "" || assignment.GetAssignmentJwt() == "" {
		return resolvedAssignment{}, fmt.Errorf("invalid_argument: AssignModel(%s) returned empty assignment", routerUID)
	}
	result := resolvedAssignment{modelUID: resolved, jwt: assignment.GetAssignmentJwt()}
	adapter.assignmentsMu.Lock()
	// 有界缓存：会话级键随运行时长累积，触顶整体清空让会话重新解析。
	if len(adapter.assignments) >= 4096 {
		adapter.assignments = make(map[string]resolvedAssignment)
	}
	adapter.assignments[key] = result
	adapter.assignmentsMu.Unlock()
	return result, nil
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

	// 写锁内复查后再拉取：TTL 过期瞬间的并发 miss 收敛为单次上游调用，
	// 等待者拿到同一个结果而不是各自打一遍 GetCliModelConfigs。
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if a.models != nil && time.Now().Before(a.modelsExpiry) {
		return a.models, nil
	}
	// 失败冷却期不再打上游：有旧值回旧值，空缓存回上次错误。
	if time.Now().Before(a.modelsRetryUntil) {
		if a.models != nil {
			return a.models, nil
		}
		return nil, a.modelsErr
	}

	name, version, os := a.config.clientIdentity()
	resp, err := a.apiClient.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: upstream.BuildMetadata(a.currentToken(), name, version, os, 0),
	}))
	if err != nil {
		wrapped := fmt.Errorf("devin GetCliModelConfigs: %w", err)
		// 客户端断连的 ctx 取消不是上游失败，不上冷却。
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			backoff := catalogRetryBackoff
			if seconds, ok := common.RetryAfterSeconds(err.Error()); ok {
				backoff = time.Duration(seconds) * time.Second
			}
			a.modelsRetryUntil = time.Now().Add(backoff)
			a.modelsErr = wrapped
		}
		// 目录刷新失败但有旧缓存时回旧值：catalog 缺席会让面板与
		// 能力位校验同时失去依据，比数据稍旧危害更大。
		if a.models != nil {
			slog.Warn("model catalog refresh failed; serving stale cache", "error", err)
			return a.models, nil
		}
		return nil, wrapped
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
			info.IsModelRouter = modelInfo.GetIsModelRouter()
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

	a.models = models
	a.modelsExpiry = time.Now().Add(a.modelsCacheTTL)
	a.modelsRetryUntil = time.Time{}
	a.modelsErr = nil
	return models, nil
}

// upstreamStallTimeout 是相邻两个上游帧之间允许的最长静默；超时即判定
// 传输层已死（半开连接、上游挂死），按传输错误收尾而不是无限等待。
// 取值需高于上游首批帧的实测延迟（长思考可达 45s+）。
// var 而非 const：测试临时缩短它来覆盖超时路径。
var upstreamStallTimeout = 120 * time.Second

// startHoldTimeout 是 start 事件（message_start/response.created）允许被
// 扣留的最长时间。扣留的目的是给上游「产出内容前就失败」留一个返回真实
// HTTP 状态码的窗口——实测这类失败全部在 ~9s 内落定；而下游客户端在
// ~30s 无数据时弃连，且中间网关只在首个协议事件后才向客户端放通字节
// （保活注释行不算）。15s 位于两者之间：快速失败仍拿到真实状态码，
// 长思考则先把 start 发出去让客户端保持存活。
var startHoldTimeout = 15 * time.Second

// upstreamFrameBuffer 是泵协程可超前读取的帧数：上游生产与客户端
// 消费解耦，同时保留对上游的背压上限。
const upstreamFrameBuffer = 64

// upstreamFrame 是泵协程的一次产出：response 为正常数据帧；
// response 为 nil 表示流终止，err 为终止错误（正常 EOF 时为 nil）。
type upstreamFrame struct {
	response *devinproto.GetChatMessageResponse
	err      error
}

// pumpUpstream 把阻塞的 Receive 归一化为 channel 帧序列：Receive 只能被
// ctx 取消打断，交给协程后 Recv 才能在等待期间响应静默看门狗与客户端断开。
// 流终止时 Err() 作为最后一帧无条件投递：终帧只有一帧，阻塞等消费方
// 排空缓冲，丢弃它会以「正常 EOF」的形态吃掉真实流错误（含静默截断）。
// 所有发送带 ctx.Done 分支：消费方放弃后协程必须能退出。
func pumpUpstream(ctx context.Context, upstream devinResponseReceiver) <-chan upstreamFrame {
	frames := make(chan upstreamFrame, upstreamFrameBuffer)
	go func() {
		defer close(frames)
		for upstream.Receive() {
			select {
			case frames <- upstreamFrame{response: upstream.Msg()}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case frames <- upstreamFrame{err: upstream.Err()}:
		case <-ctx.Done():
		}
	}()
	return frames
}

// responseStream 从泵协程读取上游帧并依次返回 decoder 生成的事件。
type responseStream struct {
	// frames 是泵协程产出的上游帧通道；终止帧 response 为 nil。
	frames <-chan upstreamFrame
	// cancel 中止上游流：看门狗判死、客户端 ctx 取消或流正常结束时调用，
	// 打断泵协程内可能仍阻塞的 Receive。
	cancel context.CancelFunc
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
	// 扣留上限由 startHold 控制：超时后 start 单独下发。
	pendingStart []llm.ResponseEvent
	// startHold 在 pendingStart 填充时武装，到期释放扣留的 start。
	startHold *time.Timer
	// startReleased 标记 start 已下发给客户端：pre-content 重试重建
	// 解码器后必须丢弃新 start，否则客户端会收到第二个 message_start。
	startReleased bool
	// finished 表示 decoder 已经生成最终事件，不再读取上游。
	finished bool
	// queue 保存已经转换、等待调用方读取的中间响应事件。
	queue []llm.ResponseEvent
	// producedEvents 表示上游帧已产出过任何事件：一旦为真说明内容已
	// 开始对外流动，此后失败只能透传，不能整体重发。
	producedEvents bool
	// upstreamConfirmed 标记上游已产出首个非错误帧：限流闩以此为据
	// 提前解闩（边际态下拒绝是概率执行，成功帧即窗口已过的证据）。
	upstreamConfirmed bool
	// gate 是上游消息速率闸门：流内 resource_exhausted 也要喂冷却闩。
	gate *rateGate
	// retried 表示已经做过一次 pre-content 整体重试（上限 1 次）。
	retried bool
	// reopen 在可重试的 pre-content 失败（传输断裂、凭据自愈后的
	// unauthenticated、静默看门狗判死）时重发请求并返回新泵；
	// continueEmpty 表示空 end_turn 续传：追加 "continue" 用户消息。
	reopen func(cause error, continueEmpty bool) (<-chan upstreamFrame, context.CancelFunc, error)
	// newDecoder 重建响应解码器供重试使用；nil 时不可重试。
	newDecoder func() *responseDecoder
	// stall 是跨 Recv 复用的静默看门狗计时器；首次等待时创建。
	stall *time.Timer
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
	// 静默计时器挂在流上跨 Recv 复用：每次入等待循环前 Reset 覆盖
	// 帧间隔。Go 1.23+ 计时器通道无缓冲，Stop/Reset 后不会投递陈旧触发，
	// 已触发（stall.C 分支）的计时器 Reset 重新武装即可。
	stall := stream.stall
	if stall == nil {
		stall = time.NewTimer(upstreamStallTimeout)
		stream.stall = stall
	} else {
		stall.Reset(upstreamStallTimeout)
	}
	defer stall.Stop()
	for len(stream.queue) == 0 && !stream.finished {
		if err := ctx.Err(); err != nil {
			return llm.ResponseEvent{}, err
		}
		if !stream.started {
			// start() 初始化 decoder.partial，必须先于 decode 调用；
			// 事件本身扣留在 pendingStart，等待第一批真实事件一起下发。
			stream.started = true
			stream.pendingStart = stream.decoder.start()
			if stream.startReleased {
				// 重试流上客户端已见过一个 start，重复下发会违反协议。
				stream.pendingStart = nil
			} else if stream.startHold == nil {
				stream.startHold = time.NewTimer(startHoldTimeout)
			} else {
				stream.startHold.Reset(startHoldTimeout)
			}
			continue
		}
		stall.Reset(upstreamStallTimeout)
		var startHold <-chan time.Time
		if stream.startHold != nil {
			startHold = stream.startHold.C
		}
		select {
		case <-startHold:
			// 上游建流后静默超时：先把扣留的 start 发出去——对客户端
			// 这是首个可见字节，链路各段的空闲计时器随之刷新。
			if len(stream.pendingStart) == 0 {
				continue
			}
			stream.queue = stream.pendingStart
			stream.pendingStart = nil
			stream.startReleased = true
			continue
		case frame, ok := <-stream.frames:
			stall.Stop()
			if !ok || frame.response == nil {
				var upstreamErr error
				if ok {
					upstreamErr = frame.err
				} else if ctxErr := context.Cause(ctx); ctxErr != nil {
					// ok==false 只剩「泵协程随 ctx 取消退出」一种来源
					//（终帧无条件投递）。把取消透传给 finish，避免以
					// 正常 EOF 的形态吞掉被截断的流。
					upstreamErr = ctxErr
				}
				if upstreamErr != nil && stream.tryReopen(upstreamErr, false) {
					continue
				}
				events := stream.release(stream.decoder.finish(upstreamErr))
				if upstreamErr == nil && emptyEndTurn(events) && stream.tryReopen(nil, true) {
					continue
				}
				stream.recordUpstreamFailure(upstreamErr)
				stream.queue = events
				stream.finished = true
				continue
			}
			if !stream.upstreamConfirmed {
				stream.upstreamConfirmed = true
				stream.gate.noteUpstreamSuccess()
			}
			recordProtoJSON(stream.recorder, "04-devin-response.jsonl", frame.response)
			events := stream.decoder.decode(frame.response)
			if len(events) > 0 {
				stream.producedEvents = true
			}
			stream.queue = stream.release(events)
			stream.finished = stream.decoder.finished
		case <-stall.C:
			// 上游静默超时：取消底层流打断泵协程；已缓冲未消费的帧
			// 补记进原始日志留证，然后按传输错误收尾。
			stream.cancel()
			stream.drainFrames()
			stallErr := fmt.Errorf("devin stream stalled: no frames for %s", upstreamStallTimeout)
			if stream.tryReopen(stallErr, false) {
				continue
			}
			stream.recordUpstreamFailure(stallErr)
			stream.queue = stream.release(stream.decoder.finish(stallErr))
			stream.finished = true
		case <-ctx.Done():
			stall.Stop()
			stream.cancel()
			stream.queue = stream.release(stream.decoder.finish(context.Cause(ctx)))
			stream.finished = true
		}
	}
	if stream.finished {
		stream.cancel()
	}
	if len(stream.queue) > 0 {
		event := stream.queue[0]
		stream.queue = stream.queue[1:]
		return event, nil
	}
	return llm.ResponseEvent{}, io.EOF
}

// tryReopen 在「上游已失败但尚未产出任何内容」时整体重发请求一次：
// 此时客户端只见过扣留的 start 事件，重发没有可见副作用。返回 true
// 表示新流已接管，调用方重置解码器后继续消费。
// continueEmpty 为空 end_turn 续传：流正常结束但零内容时重发并
// 追加 "continue" 用户消息（空轮是上游实测退化形态，CPA#4886 同构）。
func (stream *responseStream) tryReopen(cause error, continueEmpty bool) bool {
	if stream.retried || stream.producedEvents || stream.reopen == nil {
		return false
	}
	if cause == nil && !continueEmpty {
		return false
	}
	frames, cancel, err := stream.reopen(cause, continueEmpty)
	if err != nil {
		return false
	}
	stream.retried = true
	stream.frames = frames
	stream.cancel = cancel
	if stream.newDecoder != nil {
		stream.decoder = stream.newDecoder()
	}
	stream.started = false
	stream.pendingStart = nil
	stream.finished = false
	stream.queue = nil
	// 新流的首个非错误帧重新获得解闩资格——上一流的确认不能
	// 替代这次重试是否真的打穿了限流。
	stream.upstreamConfirmed = false
	return true
}

// emptyEndTurn 判断 finish 产出的事件是否构成「正常 stop 但零内容」：
// 上游偶发直接以 stopReason 收尾且不带任何 delta。StopSequence 不算——
// 零内容命中停止序列更可能是预期的截断而非退化轮。
func emptyEndTurn(events []llm.ResponseEvent) bool {
	for _, event := range events {
		if event.Type != llm.ResponseEventDone {
			continue
		}
		return event.Message != nil &&
			event.Message.StopReason == llm.StopReasonStop &&
			len(event.Message.Content) == 0
	}
	return false
}

// recordUpstreamFailure 把不可重试的上游侧失败记为请求目录的首个失败点：
// 传输层断裂（EOF/重置/超时/静默判死，即非 connect.Error）记
// devin_transport；Connect 协议语义错误记 devin_connect。ctx 取消不记——
// 客户端断连由 HTTP 外层记 client_disconnected，不应被上游 stage 抢占。
// WriteError 是 first-write-wins，此处记录后外层 http_stream 只作补充。
func (stream *responseStream) recordUpstreamFailure(cause error) {
	if cause == nil {
		return
	}
	// 限流结论与日志开关无关：上游报了 resource_exhausted 就上闩。
	stream.gate.noteUpstreamError(cause)
	if stream.recorder == nil {
		return
	}
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return
	}
	stage := "devin_connect"
	if isTransientConnectError(cause) {
		stage = "devin_transport"
	}
	stream.recorder.WriteError(stage, cause)
}

// drainFrames 把看门狗判死时已缓冲未消费的上游帧补记进原始日志——
// 「死前最后输出了什么」是判断上游挂死形态的关键证据。
func (stream *responseStream) drainFrames() {
	for {
		select {
		case frame, ok := <-stream.frames:
			if !ok || frame.response == nil {
				return
			}
			recordProtoJSON(stream.recorder, "04-devin-response.jsonl", frame.response)
		default:
			return
		}
	}
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

// protoJSON 把 protojson 序列化推迟到日志写协程：recordProtoJSON 的调用方
// 是上游泵/解码 goroutine，同步 marshal 每帧会挤占流处理；包装成
// json.Marshaler 后 sanitize 在 worker 内 marshal+预筛+脱敏。marshal 失败
// 的兜底是记录文件里的 serialization_error 条目（实际不可达：protojson
// 对构造好的消息不报错）。
type protoJSON struct{ message proto.Message }

func (p protoJSON) MarshalJSON() ([]byte, error) { return protojson.Marshal(p.message) }

func recordProtoJSON(recorder *debuglog.Recorder, name string, message proto.Message) {
	if recorder == nil || message == nil {
		return
	}
	if strings.HasSuffix(name, ".jsonl") {
		recorder.AppendValueJSONL(name, protoJSON{message})
		return
	}
	recorder.WriteJSON(name, protoJSON{message})
}
