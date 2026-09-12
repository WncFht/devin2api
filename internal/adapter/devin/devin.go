// 本文件实现 RequestMessages 与 Devin Connect RPC 的双向转换。
//
// Package devin 负责一次 Devin GetChatMessage 调用及其响应事件转换，不执行工具或 agent loop。
package devin

import (
	"context"
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
	if err := adapter.validateNotRouterModel(model); err != nil {
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
	// streamCtx 由 responseStream 持有：看门狗判死或客户端断开时
	// cancel 是唯一打断泵协程内阻塞 Receive 的手段。
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := adapter.getChatMessageWithRetry(streamCtx, protoRequest)
	if err != nil {
		cancel()
		recorder.WriteError("devin_connect", err)
		// 透传上游 Connect 错误原文，不包一层模糊前缀。
		return nil, connectError(err)
	}
	return &responseStream{
		frames:   pumpUpstream(stream),
		cancel:   cancel,
		decoder:  newResponseDecoder(model, request.StopSequences),
		recorder: recorder,
	}, nil
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

// validateNotRouterModel 拒绝上游 router uid 的直连请求：目录里标了
// is_model_router 的 uid 必须先经 AssignModel 解出真实模型（未实现），
// 实测直连只换回 unavailable: third-party model provider——伪装成
// 瞬时错误的永久失败。提前报成 invalid_argument，让网关按 4xx 归类。
// 目录未覆盖该模型时放行，交给上游裁决。
func (adapter *Adapter) validateNotRouterModel(model string) error {
	adapter.modelsMu.RLock()
	defer adapter.modelsMu.RUnlock()
	for _, m := range adapter.models {
		if m.ID == model && m.IsModelRouter {
			return fmt.Errorf("invalid_argument: model %q is an upstream router uid and requires AssignModel resolution, which this proxy does not implement; pick a concrete model uid", model)
		}
	}
	return nil
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

// upstreamStallTimeout 是相邻两个上游帧之间允许的最长静默；超时即判定
// 传输层已死（半开连接、上游挂死），按传输错误收尾而不是无限等待。
// 取值需高于上游首批帧的实测延迟（长思考可达 45s+）。
// var 而非 const：测试临时缩短它来覆盖超时路径。
var upstreamStallTimeout = 120 * time.Second

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
// 流终止时 Err() 作为最后一帧发出；缓冲满（调用方已放弃消费）则丢弃，
// 保证泵协程在任何情况下都能退出。
func pumpUpstream(upstream devinResponseReceiver) <-chan upstreamFrame {
	frames := make(chan upstreamFrame, upstreamFrameBuffer)
	go func() {
		defer close(frames)
		for upstream.Receive() {
			frames <- upstreamFrame{response: upstream.Msg()}
		}
		select {
		case frames <- upstreamFrame{err: upstream.Err()}:
		default:
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
	// 静默计时器逐帧复用：一条流可产出数千帧，每帧 NewTimer 是无谓分配。
	// Go 1.23+ 计时器通道无缓冲，Stop/Reset 后不会投递陈旧触发。
	stall := time.NewTimer(upstreamStallTimeout)
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
			continue
		}
		stall.Reset(upstreamStallTimeout)
		select {
		case frame, ok := <-stream.frames:
			stall.Stop()
			if !ok || frame.response == nil {
				// ok==false：缓冲满时终止帧被丢弃（见 pumpUpstream），
				// 按正常 EOF 处理，缺 stop reason 由 decoder 报错。
				var upstreamErr error
				if ok {
					upstreamErr = frame.err
				}
				stream.queue = stream.release(stream.decoder.finish(upstreamErr))
				stream.finished = true
				continue
			}
			recordProtoJSON(stream.recorder, "04-devin-response.jsonl", frame.response)
			stream.queue = stream.release(stream.decoder.decode(frame.response))
			stream.finished = stream.decoder.finished
		case <-stall.C:
			// 上游静默超时：取消底层流打断泵协程；已缓冲未消费的帧
			// 补记进原始日志留证，然后按传输错误收尾。
			stream.cancel()
			stream.drainFrames()
			stream.queue = stream.release(stream.decoder.finish(fmt.Errorf("Devin stream stalled: no frames for %s", upstreamStallTimeout)))
			stream.finished = true
		case <-ctx.Done():
			stall.Stop()
			stream.cancel()
			stream.queue = stream.release(stream.decoder.finish(ctx.Err()))
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
