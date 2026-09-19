// 本文件负责把 Devin protobuf 响应帧解释为有序的中间响应事件。
package devin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/llm"
)

// responseDecoder 保存一次 Devin 请求内的响应累计状态和内容映射。
type responseDecoder struct {
	// model 是请求使用的 Devin 模型标识。
	model string
	// partial 是当前累计形成的助手消息。
	partial llm.AssistantMessage
	// text 是当前累计的文字块。
	text *llm.TextContent
	// textBuilder 累计文字增量，避免 O(n²) 字符串拼接。
	textBuilder strings.Builder
	// textIdx 是文字块在 partial.Content 中的位置。
	textIdx int
	// textOpen 表示文字块已经开始但尚未结束。
	textOpen bool
	// thinking 是当前累计的思考块。
	thinking *llm.ThinkingContent
	// thinkingBuilder 累计思考正文。
	thinkingBuilder strings.Builder
	// thinkingSigBuilder 累计思考签名。
	thinkingSigBuilder strings.Builder
	// thinkIdx 是思考块在 partial.Content 中的位置。
	thinkIdx int
	// thinkingOpen 表示思考块已经开始但尚未结束。
	thinkingOpen bool
	// tools 保存正在累计参数的工具调用。
	tools []*toolState
	// started 表示已经生成 start 事件。
	started bool
	// finished 表示已经生成 done 或 error 事件。
	finished bool
	// hasStopReason 表示 Devin 已经显式返回停止原因。
	hasStopReason bool
	// stopReason 保存 Devin 声明的最终停止原因，等待上游 EOF 后用于完成响应。
	stopReason llm.StopReason
	// stopPatterns 是客户端请求的停止序列。上游 stopPatterns 实测不生效
	//（模型越过 pattern 继续输出），由解码层对累计文本做本地截断。
	stopPatterns []string
	// maxPatternLen 是最长停止序列的字节长度，决定流式下发时保留的尾部窗口。
	maxPatternLen int
	// textEmitted 是 textBuilder 中已作为 delta 下发的字节数。
	textEmitted int
	// stoppedByPattern 表示生成已被停止序列截断；后续帧只更新用量元数据。
	stoppedByPattern bool
	// stopSequence 是命中的停止序列文本。
	stopSequence string
	// providerLogged 表示已把 provider 侧追踪信息写入 diagnostics。
	providerLogged bool
	// providerRefusal 表示上游声明 provider 拒绝了本次请求（usage.provider_refusal）。
	providerRefusal bool
	// customTools 是本次请求按 freeform/custom 语义声明的工具名集合；
	// 这些工具在 wire 上是单参数 function 包装形态，响应要解包回原文。
	customTools map[string]bool
	// serverTools 是本次请求按服务端托管语义声明的工具名集合
	//（OpenAI {"type":"web_search"} 的 function 诱饵）；命中的调用
	// 标 call.Server，由流的续轮机制代执行而非交给客户端。
	serverTools map[string]bool
	// driftWarned 表示本流已告警过上游 schema 漂移（unknown 字段），
	// 同一流逐帧重复刷同一条告警没有新增信息。
	driftWarned bool
	// drift 保存首次检出的 unknown 字段现场（scope+字段号）——pump
	// 侧消费后在 04 里落 schema_drift 标记行；nil 表示未检出/已标记。
	drift *schemaDrift
	// stopReasonWarned 表示本流已告警过未知 stop_reason 枚举值。
	stopReasonWarned bool
}

// toolState 保存一次 Devin 工具调用的累计状态。
type toolState struct {
	// call 是当前累计形成的工具调用。
	call llm.ToolCall
	// contentIdx 是调用在 partial.Content 中的位置。
	contentIdx int
	// eventID 是下发事件的固定调用标识：ToolCallStart 用什么，后续
	// delta 就用什么——首帧缺 id 时它是合成占位，真实 id 晚到只回填
	// call/内容块，不改事件 id（客户端已按 start 的值对账）。
	eventID string
	// arguments 累计 Devin 返回的工具参数 JSON 片段。
	arguments strings.Builder
	// emitted 表示原始工具调用已经加入 partial 并产生 start 事件。
	emitted bool
	// placeholderID 表示 call.ID 仍是合成占位（首帧无 id）：真实 id
	// 到达后该位置 false。占位期间无 id 续帧与迟到的真 id 帧都按
	// 位置归并给它。
	placeholderID bool
	// wrapped 表示该调用是 custom 声明工具的 function 包装形态：参数片段
	// 是 {"input":"<原文>"} 的 JSON 包装，不是给客户端的参数体本身。
	wrapped bool
}

// newResponseDecoder 建解码器：stopPatterns 滤掉空串并记最大长度
// （供截断尾部窗口的扫描界）；customTools/serverTools 是按 freeform 与
// 服务端托管语义声明的工具名集合——前者按包装格式解包，后者标
// call.Server 交给流的续轮机制代执行。
func newResponseDecoder(model string, stopPatterns []string, customTools, serverTools map[string]bool) *responseDecoder {
	patterns := make([]string, 0, len(stopPatterns))
	maxLen := 0
	for _, pattern := range stopPatterns {
		if pattern == "" {
			continue
		}
		patterns = append(patterns, pattern)
		if len(pattern) > maxLen {
			maxLen = len(pattern)
		}
	}
	return &responseDecoder{model: model, stopPatterns: patterns, maxPatternLen: maxLen, customTools: customTools, serverTools: serverTools}
}

// customToolNames 返回请求里按 freeform/custom 语义声明的工具名集合，
// 供解码器识别包装 function 调用并解包回原文。
func customToolNames(tools []llm.ToolDefinition) map[string]bool {
	var names map[string]bool
	for _, tool := range tools {
		if tool.Custom {
			if names == nil {
				names = make(map[string]bool)
			}
			names[tool.Name] = true
		}
	}
	return names
}

// serverToolNames 返回请求里按服务端托管语义声明的工具名集合，
// 供解码器把对应调用标记为代理代执行（call.Server）。
func serverToolNames(tools []llm.ToolDefinition) map[string]bool {
	var names map[string]bool
	for _, tool := range tools {
		if tool.Server {
			if names == nil {
				names = make(map[string]bool)
			}
			names[tool.Name] = true
		}
	}
	return names
}

// start 初始化 partial 并产出 ResponseEventStart：事件由 Recv 扣留
// （pendingStart）与首批真实事件一起下发，已发过一次 start 的重试流
// 不重复产出。重复调用（reopen 后新 decoder 之外的路径）返回空。
func (decoder *responseDecoder) start() []llm.ResponseEvent {
	if decoder.started || decoder.finished {
		return nil
	}
	decoder.started = true
	decoder.partial = llm.AssistantMessage{
		API: "connect", Provider: "devin", Model: decoder.model,
		StopReason: llm.StopReasonPending, TimestampMS: time.Now().UnixMilli(),
	}
	return []llm.ResponseEvent{{Type: llm.ResponseEventStart, Partial: decoder.snapshot()}}
}

// snapshot 返回 partial 的隔离副本。产出的事件经 channel 跨 goroutine
// 交给编码器（泵协程写 partial、消费协程读 event.Partial），共享活对象
// 会让消费侧并发读到后续帧的写入。Content/Diagnostics 只做整块替换与
// append（内容块是值类型、在 decoder.text/thinking/state.call 缓冲里
// 写好后整体拷入），浅拷贝消息体 + 克隆这两个切片即与后续写入隔离。
// Done 的 Message 与 ToolCallEnd 的 ToolCall 是终态指针（此后不再有
// 写点），不走这里。
func (decoder *responseDecoder) snapshot() *llm.AssistantMessage {
	partial := decoder.partial
	partial.Content = slices.Clone(partial.Content)
	partial.Diagnostics = slices.Clone(partial.Diagnostics)
	return &partial
}

// decode 把一帧上游响应解释为若干中间事件：先刷元数据/usage，再按
// 思考、文本、工具增量、停止原因依次走子解码器，共享同一 events
// 切片追加。已 finished 或停止序列截断后的帧只更新元数据。
func (decoder *responseDecoder) decode(response *devinproto.GetChatMessageResponse) []llm.ResponseEvent {
	if decoder.finished {
		return nil
	}
	decoder.noteSchemaDrift(response)
	decoder.updateMetadata(response)
	if decoder.stoppedByPattern {
		// 停止序列已截断对外输出；继续消费上游帧仅为 usage 统计完整。
		return nil
	}
	// 子解码器共享同一个 events 切片追加事件；多数帧只产 1-2 个事件，
	// 共享切片把每帧的 2-3 次小分配压到接近一次。
	events := make([]llm.ResponseEvent, 0, 4)
	// 上游把签名作为全部正文之后的尾随帧发送；思考块已关闭时
	// 不能新开思考块，要把签名合并回上一个思考块。
	if response.GetDeltaSignature() != "" && response.GetDeltaThinking() == "" && !decoder.thinkingOpen {
		events = decoder.decodeLateSignature(events, response)
	} else if response.GetDeltaThinking() != "" || response.GetDeltaSignature() != "" || response.GetThinkingRedacted() {
		events = decoder.endText(events)
		events = decoder.decodeThinking(events, response)
	}
	if response.GetDeltaText() != "" {
		events = decoder.endThinking(events)
		events = decoder.decodeText(events, response.GetDeltaText())
	}
	for _, delta := range response.GetDeltaToolCalls() {
		events = decoder.endThinking(events)
		events = decoder.endText(events)
		events = decoder.decodeTool(events, delta)
	}
	if reason := response.GetStopReason(); reason != devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_UNSPECIFIED {
		decoder.hasStopReason = true
		decoder.stopReason = mapStopReason(reason)
		if !decoder.stopReasonWarned && reason.Descriptor().Values().ByNumber(reason.Number()) == nil {
			// 枚举值不在我们编译的 proto 定义里：上游新增了停止原因
			// 形态。mapStopReason 的 default 把它静默归 Stop——告警
			// 留下数值，避免新语义被吞成正常结束而无迹可查。
			decoder.stopReasonWarned = true
			slog.Warn("upstream sent undeclared stop_reason value; mapped to stop",
				"value", int(reason.Number()))
		}
	}
	return events
}

// schemaDrift 是首次检出 unknown 字段的现场：scope 指出漂移发生在帧的
// 哪一层，fields 是解出的字段号列表——对照上游新 proto 定位新字段。
type schemaDrift struct {
	Scope  string
	Fields []int
}

// noteSchemaDrift 检查上游帧在我们 proto 定义之外携带的字段：上游 schema
// 演进的新字段经 proto 解码静默落进 unknown 区，「上游加了字段我们看不见」
// 只能靠这里暴露。每流至多告警一次；覆盖帧顶层与两个最常见的语义嵌套
// （usage、tool_call delta）——漂移若发生在这些位置，影响的是计费与调用。
// 检出时现场存入 decoder.drift，由 pump 侧落成 04 标记行。
func (decoder *responseDecoder) noteSchemaDrift(response *devinproto.GetChatMessageResponse) {
	if decoder.driftWarned {
		return
	}
	warn := func(scope string, message proto.Message) {
		if decoder.driftWarned {
			return
		}
		unknown := message.ProtoReflect().GetUnknown()
		if len(unknown) == 0 {
			return
		}
		decoder.driftWarned = true
		fields := unknownFieldNumbers(unknown)
		decoder.drift = &schemaDrift{Scope: scope, Fields: fields}
		slog.Warn("upstream response carried fields outside our proto schema; decode may be drifting",
			"scope", scope, "fields", fields)
	}
	warn("frame", response)
	if usage := response.GetUsage(); usage != nil {
		warn("usage", usage)
	}
	for _, delta := range response.GetDeltaToolCalls() {
		warn("tool_call", delta)
	}
}

// unknownFieldNumbers 从 unknown 区解出顶层字段号列表：告警带上号码才能
// 对照上游新 proto 定位是哪个字段在漂移。解析失败（截断/非法 wire）时
// 返回已解出的前缀——unknown 区来自已 unmarshal 成功的帧，实际不可达。
func unknownFieldNumbers(raw []byte) []int {
	var numbers []int
	for len(raw) > 0 {
		number, _, length := protowire.ConsumeField(raw)
		if length < 0 {
			break
		}
		numbers = append(numbers, int(number))
		raw = raw[length:]
	}
	return numbers
}

// finish 在流终止（正常 EOF 或错误）时产出收尾事件：错误走 fail
// 透传 connect 原文；正常收尾先补齐 thinking/text/tool 的 end 事件
// 再产 Done。重复调用返回空。
func (decoder *responseDecoder) finish(upstreamErr error) []llm.ResponseEvent {
	if decoder.finished {
		return nil
	}
	if upstreamErr != nil {
		// 流中途/结束时的错误透传原文；fail 内部统一分类成记录。
		return decoder.fail(upstreamErr)
	}
	if !decoder.hasStopReason && len(decoder.partial.Content) == 0 && len(decoder.tools) == 0 {
		return decoder.fail(errors.New("devin stream ended without generated content"))
	}
	reason := decoder.stopReason
	if decoder.stoppedByPattern {
		reason = llm.StopReasonStopSequence
		decoder.partial.StopSequence = decoder.stopSequence
	} else if !decoder.hasStopReason {
		// Devin 正常收尾必带 stopReason 帧（实测帧序 delta → stopReason →
		// usage → responseDimensionGroups）。干净 EOF 却没有停止原因说明
		// 流在应用层被截断；此处合成 Stop 会把截断伪装成 end_turn，
		// 下游 agent 会把半完成的任务当作完成（实测复现：Codex 在宣告
		// 继续调用工具后直接 task_complete）。
		return decoder.fail(errors.New("devin stream ended without stop reason"))
	}
	if reason == llm.StopReasonError {
		if decoder.providerRefusal {
			return decoder.fail(errors.New("upstream provider refused the request (provider_refusal)"))
		}
		return decoder.fail(errors.New("devin stopped with an error"))
	}
	return decoder.complete(reason)
}

// updateMetadata 把帧上的响应元数据（id/模型/时间戳/usage/诊断）
// 刷进 partial；字段只在「有值且更完整」时覆盖——上游常把 usage
// 分多次增量上报，首值不丢。
func (decoder *responseDecoder) updateMetadata(response *devinproto.GetChatMessageResponse) {
	if response.MessageId != nil {
		decoder.partial.ResponseID = response.GetMessageId()
	}
	if id := response.GetOutputId(); id != "" {
		decoder.partial.OutputID = id
	}
	if id := response.GetRequestId(); id != "" && decoder.partial.UpstreamRequestID == "" {
		decoder.partial.UpstreamRequestID = id
	}
	if response.ActualModelUid != nil {
		decoder.partial.ResponseModel = response.GetActualModelUid()
	}
	if timestamp := response.GetTimestamp(); timestamp != nil {
		decoder.partial.TimestampMS = time.Unix(timestamp.GetSeconds(), int64(timestamp.GetNanos())).UnixMilli()
	}
	if usage := response.GetUsage(); usage != nil {
		if decoder.partial.ResponseModel == "" && usage.ModelUid != nil {
			decoder.partial.ResponseModel = usage.GetModelUid()
		}
		// 显式零字段不得覆盖已入账的非零值——占位零字段的上游/
		// 中继存在（ccLoad#133 同型）；usage 是累计快照，零只接受
		// 「尚无入账」时的占位。
		if usage.GetInputTokens() != 0 || decoder.partial.Usage.Input == 0 {
			decoder.partial.Usage.Input = int64(usage.GetInputTokens())
		}
		if usage.GetOutputTokens() != 0 || decoder.partial.Usage.Output == 0 {
			decoder.partial.Usage.Output = int64(usage.GetOutputTokens())
		}
		if usage.GetCacheReadTokens() != 0 || decoder.partial.Usage.CacheRead == 0 {
			decoder.partial.Usage.CacheRead = int64(usage.GetCacheReadTokens())
		}
		if usage.GetCacheWriteTokens() != 0 || decoder.partial.Usage.CacheWrite == 0 {
			decoder.partial.Usage.CacheWrite = int64(usage.GetCacheWriteTokens())
		}
		decoder.partial.Usage.TotalTokens = decoder.partial.Usage.Input + decoder.partial.Usage.Output + decoder.partial.Usage.CacheRead + decoder.partial.Usage.CacheWrite
		if usage.GetProviderRefusal() {
			decoder.providerRefusal = true
		}
		// provider 侧追踪信息（api_provider + 供应商 HTTP 请求号）只记一次，
		// 排障时可直接把 x-request-id 报给上游/供应商。
		if !decoder.providerLogged {
			apiProvider := strings.TrimPrefix(usage.GetApiProvider().String(), "API_PROVIDER_")
			providerRequestID := usage.GetResponseHeader()["x-request-id"]
			if providerRequestID != "" || (apiProvider != "" && apiProvider != "UNSPECIFIED") {
				details, _ := json.Marshal(map[string]string{
					"api_provider":        apiProvider,
					"provider_request_id": providerRequestID,
					"provider_message_id": usage.GetMessageId(),
					"billing_model_uid":   usage.GetBillingModelUid(),
				})
				decoder.partial.Diagnostics = append(decoder.partial.Diagnostics, llm.AssistantMessageDiagnostic{
					Type:        "upstream_provider",
					TimestampMS: time.Now().UnixMilli(),
					Details:     details,
				})
				decoder.providerLogged = true
			}
		}
	}
	// 计费读数在帧顶层（非 usage 子消息）：任一字段在场即建 Costs，
	// 各字段取最新上报值——它们是快照语义，末帧携带最终读数。
	if response.CreditCost != nil || response.CommittedCreditCost != nil ||
		response.CommittedAcuCost != nil || response.CommittedQuotaCostBasisPoints != nil ||
		response.CommittedOverageCostCents != nil {
		costs := decoder.partial.Usage.Costs
		if costs == nil {
			costs = &llm.UpstreamCosts{}
			decoder.partial.Usage.Costs = costs
		}
		if response.CreditCost != nil {
			costs.CreditCost = int64(response.GetCreditCost())
		}
		if response.CommittedCreditCost != nil {
			costs.CommittedCreditCost = int64(response.GetCommittedCreditCost())
		}
		if response.CommittedAcuCost != nil {
			costs.CommittedAcuCost = response.GetCommittedAcuCost()
		}
		if response.CommittedQuotaCostBasisPoints != nil {
			costs.CommittedQuotaCostBasisPoints = response.GetCommittedQuotaCostBasisPoints()
		}
		if response.CommittedOverageCostCents != nil {
			costs.CommittedOverageCostCents = response.GetCommittedOverageCostCents()
		}
	}
}

// appendServerResult 把一次服务端托管执行的结果追加为 partial 内容块
// 并产出对应事件：块位置即 ContentIndex，编码器按 ToolCallID 与前面的
// Server ToolCall 配对渲染托管结果形态。由 handleServerCalls 在 complete
// 产事件后调用——此时 finished 已置位，本方法只做追加不再产生收尾事件，
// 续轮路径下这份 partial 同时是下一轮解码器的内容种子。
func (decoder *responseDecoder) appendServerResult(result llm.ServerToolResult) llm.ResponseEvent {
	decoder.partial.Content = append(decoder.partial.Content, result)
	index := len(decoder.partial.Content) - 1
	return llm.ResponseEvent{
		Type: llm.ResponseEventServerToolResult, ContentIndex: index,
		ServerResult: &result, Partial: decoder.snapshot(),
	}
}

// decodeThinking 处理思考增量：未开块时先建块并发 ThinkingStart，
// 正文/签名分别进独立 builder（上游分通道上报），签名每帧同步回写
// partial——隔块到达的尾随签名由 finish 阶段的延迟逻辑兜底。
func (decoder *responseDecoder) decodeThinking(events []llm.ResponseEvent, response *devinproto.GetChatMessageResponse) []llm.ResponseEvent {
	if !decoder.thinkingOpen {
		decoder.thinking = &llm.ThinkingContent{Redacted: response.GetThinkingRedacted()}
		decoder.thinkingBuilder.Reset()
		decoder.thinkingSigBuilder.Reset()
		decoder.partial.Content = append(decoder.partial.Content, *decoder.thinking)
		decoder.thinkIdx = len(decoder.partial.Content) - 1
		decoder.thinkingOpen = true
		events = append(events, llm.ResponseEvent{Type: llm.ResponseEventThinkingStart, ContentIndex: decoder.thinkIdx, Partial: decoder.snapshot()})
	}
	if delta := response.GetDeltaThinking(); delta != "" {
		decoder.thinkingBuilder.WriteString(delta)
	}
	if sig := response.GetDeltaSignature(); sig != "" {
		decoder.thinkingSigBuilder.WriteString(sig)
	}
	if sigType := response.GetDeltaSignatureType(); sigType != "" {
		// signature_type 决定签名载荷的格式（sealed/anthropic/openai），
		// 重放时必须原样回传——实测错配触发上游 invalid_argument。
		decoder.thinking.SignatureType = sigType
	}
	decoder.thinking.Redacted = decoder.thinking.Redacted || response.GetThinkingRedacted()
	// 思考正文在 endThinking 再 materialize；签名通常较短，每帧同步签名避免 startReasoning 拿不到。
	decoder.thinking.Signature = decoder.thinkingSigBuilder.String()
	decoder.partial.Content[decoder.thinkIdx] = *decoder.thinking
	if delta := response.GetDeltaThinking(); delta != "" {
		events = append(events, llm.ResponseEvent{Type: llm.ResponseEventThinkingDelta, ContentIndex: decoder.thinkIdx, Delta: delta, Partial: decoder.snapshot()})
	}
	return events
}

// decodeText 处理文本增量：未开块时先建块发 TextStart；命中停止
// 序列时截断下发并把命中位置记为 stoppedByPattern，后续帧只刷
// 元数据不再产正文。
func (decoder *responseDecoder) decodeText(events []llm.ResponseEvent, delta string) []llm.ResponseEvent {
	if !decoder.textOpen {
		decoder.text = &llm.TextContent{}
		decoder.textBuilder.Reset()
		decoder.textEmitted = 0
		decoder.partial.Content = append(decoder.partial.Content, *decoder.text)
		decoder.textIdx = len(decoder.partial.Content) - 1
		decoder.textOpen = true
		events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextStart, ContentIndex: decoder.textIdx, Partial: decoder.snapshot()})
	}
	// 用 Builder 累加，避免每帧产生越来越大的新字符串。
	decoder.textBuilder.WriteString(delta)
	if len(decoder.stopPatterns) == 0 {
		events = append(events, decoder.emitTextDelta(delta))
		return events
	}
	return decoder.scanTextForStops(events)
}

// scanTextForStops 在累计文本中查找停止序列并决定本次可下发的 delta。
// 已下发前缀保证不含任何匹配起点（见 emitTextUpTo 的安全窗口推导），
// 因此只需从 textEmitted 起搜索。命中时截断文本、关闭文字块并标记
// stoppedByPattern；未命中时保留尾部 maxPatternLen-1 字节不下发——
// 它们可能是某个序列跨帧的不完整前缀。
func (decoder *responseDecoder) scanTextForStops(events []llm.ResponseEvent) []llm.ResponseEvent {
	text := decoder.textBuilder.String()
	earliest := -1
	for _, pattern := range decoder.stopPatterns {
		if idx := strings.Index(text[decoder.textEmitted:], pattern); idx >= 0 {
			pos := decoder.textEmitted + idx
			if earliest < 0 || pos < earliest {
				earliest = pos
				decoder.stopSequence = pattern
			}
		}
	}
	if earliest >= 0 {
		if earliest > decoder.textEmitted {
			events = append(events, decoder.emitTextDelta(text[decoder.textEmitted:earliest]))
		}
		decoder.stoppedByPattern = true
		decoder.textBuilder.Reset()
		decoder.textBuilder.WriteString(text[:earliest])
		return decoder.endText(events)
	}
	// safe 是字节下界，可能落在多字节 rune 中间：回退到最近的 rune
	// 起点，残续字节留在 holdback 窗口等下一帧补齐——否则下发半个
	// UTF-8 序列会被编码端替成 U+FFFD，残字节再产一个，客户端永久
	// 丢字符。
	if safe := len(text) - decoder.maxPatternLen + 1; safe > decoder.textEmitted {
		for safe < len(text) && !utf8.RuneStart(text[safe]) {
			safe--
		}
		if safe > decoder.textEmitted {
			events = append(events, decoder.emitTextDelta(text[decoder.textEmitted:safe]))
		}
	}
	return events
}

// emitTextDelta 下发一段文本增量并推进 textEmitted 计数。
func (decoder *responseDecoder) emitTextDelta(delta string) llm.ResponseEvent {
	decoder.textEmitted += len(delta)
	return llm.ResponseEvent{Type: llm.ResponseEventTextDelta, ContentIndex: decoder.textIdx, Delta: delta, Partial: decoder.snapshot()}
}

// decodeTool 处理工具调用增量：按 id 定位或新建 toolState（无 id
// 首帧合成占位 id 续接）；custom 声明工具的包装参数与原生调用
// 分路，参数体在 complete 时一次性成形。
func (decoder *responseDecoder) decodeTool(events []llm.ResponseEvent, delta *devinproto.ExaCodeiumCommonPb_ChatToolCall) []llm.ResponseEvent {
	state := decoder.findTool(delta)
	if state == nil {
		id := delta.GetId()
		placeholder := id == ""
		if placeholder {
			// 上游偶发首帧不带 id：合成稳定占位，保证 ToolCallStart
			// 事件过得了 Validate，后续按位置续接参数增量。
			id = fmt.Sprintf("call_%d", len(decoder.tools))
		}
		state = &toolState{
			call:          llm.ToolCall{ID: id, Name: delta.GetName(), Arguments: json.RawMessage(`{}`)},
			contentIdx:    -1,
			eventID:       id,
			placeholderID: placeholder,
		}
		decoder.tools = append(decoder.tools, state)
	}
	if delta.GetName() != "" {
		state.call.Name = delta.GetName()
		if decoder.customTools[delta.GetName()] {
			// custom 声明工具的 wire 形态是包装 function：按 custom_tool_call
			// 语义标记，参数片段在 complete 统一解包。
			state.call.Custom = true
			state.wrapped = true
		}
		if decoder.serverTools[delta.GetName()] {
			// 托管声明工具：模型发出的调用由代理代调上游专用 RPC
			// 执行（handleServerCalls），标记供编码器选托管渲染形态。
			state.call.Server = true
		}
	}
	if delta.GetIsCustomToolCall() {
		state.call.Custom = true
	}
	if state.placeholderID && delta.GetId() != "" {
		// 占位调用的真实 id 晚到：回填已发出的内容块，让最终消息携带
		// 真 id 供下轮回放；事件 ToolCallID 保持首次值（占位）不换——
		// 客户端已按 start 的 id 对账，中途换 id 会让 delta 悬空。
		state.placeholderID = false
		state.call.ID = delta.GetId()
		decoder.partial.Content[state.contentIdx] = state.call
	}
	fragment := delta.GetArgumentsJson()
	hasFragment := delta.ArgumentsJson != nil
	if invalid := delta.GetInvalidJsonStr(); invalid != "" {
		// custom/freeform 工具的参数体本来就不是 JSON（如补丁文本），
		// 原样透传给客户端而不是吞成 {}。
		state.call.Custom = true
		fragment = invalid
		hasFragment = true
	}
	if hasFragment {
		state.arguments.WriteString(fragment)
	}
	return decoder.decodeNativeTool(events, state, fragment, hasFragment)
}

// decodeNativeTool 保留 Devin 原生工具名称和参数增量语义。
func (decoder *responseDecoder) decodeNativeTool(events []llm.ResponseEvent, state *toolState, fragment string, hasFragment bool) []llm.ResponseEvent {
	if !state.emitted {
		state.contentIdx = len(decoder.partial.Content)
		state.emitted = true
		decoder.partial.Content = append(decoder.partial.Content, state.call)
		events = append(events, llm.ResponseEvent{
			Type: llm.ResponseEventToolCallStart, ContentIndex: state.contentIdx,
			ToolCallID: state.eventID, ToolName: state.call.Name, Partial: decoder.snapshot(),
		})
	}
	// 工具参数在 complete 中一次性解析并写入，避免每帧 O(n) 拷贝/校验。
	// wrapped 调用的片段是 JSON 包装碎片而非参数原文，不下发增量——
	// 解包后的完整 input 在 complete 以单条 delta 补发。
	if hasFragment && !state.wrapped {
		events = append(events, llm.ResponseEvent{
			Type: llm.ResponseEventToolCallDelta, ContentIndex: state.contentIdx,
			ToolCallID: state.eventID, Delta: fragment, Partial: decoder.snapshot(),
		})
	}
	return events
}

// decodeLateSignature 把思考块关闭后才到达的签名帧合并回上一个思考块。
// 没有思考块可挂时合成一个空块：openai 体制下签名是唯一思考产物
// （无 deltaThinking，推理内容密封在签名的 reasoning item 里），
// 丢弃它会让 /v1/responses 下游永远拿不到 reasoning item。
func (decoder *responseDecoder) decodeLateSignature(events []llm.ResponseEvent, response *devinproto.GetChatMessageResponse) []llm.ResponseEvent {
	signature := response.GetDeltaSignature()
	for index := len(decoder.partial.Content) - 1; index >= 0; index-- {
		thinking, ok := decoder.partial.Content[index].(llm.ThinkingContent)
		if !ok {
			continue
		}
		thinking.Signature += signature
		if sigType := response.GetDeltaSignatureType(); sigType != "" {
			thinking.SignatureType = sigType
		}
		thinking.Redacted = thinking.Redacted || response.GetThinkingRedacted()
		decoder.partial.Content[index] = thinking
		return append(events, llm.ResponseEvent{
			Type: llm.ResponseEventSignature, ContentIndex: index,
			Delta: signature, Partial: decoder.snapshot(),
		})
	}
	thinking := llm.ThinkingContent{
		Signature:     signature,
		SignatureType: response.GetDeltaSignatureType(),
		Redacted:      response.GetThinkingRedacted(),
	}
	decoder.partial.Content = append(decoder.partial.Content, thinking)
	index := len(decoder.partial.Content) - 1
	// 事件共享同一 Partial 指针，编码器在 start/end 边界就会读到块内签名；
	// 再发 signature 事件会与之叠加翻倍，且合成块永远没有 thinking_end，
	// 只发签名事件会让编码器侧的 item 悬挂到流终止报错。
	return append(events,
		llm.ResponseEvent{Type: llm.ResponseEventThinkingStart, ContentIndex: index, Partial: decoder.snapshot()},
		llm.ResponseEvent{Type: llm.ResponseEventThinkingEnd, ContentIndex: index, Partial: decoder.snapshot()},
	)
}

// endThinking 收尾当前思考块：builder 里的完整正文与签名一次性
// 物化回 partial，发 ThinkingEnd。思考块可被跨块尾随签名复开——
// 见 decodeLateSignature。
func (decoder *responseDecoder) endThinking(events []llm.ResponseEvent) []llm.ResponseEvent {
	// thinkingOpen 只在 thinking 缓冲建块后置位，开块即非空。
	if !decoder.thinkingOpen {
		return events
	}
	decoder.thinkingOpen = false
	// 只在思考块结束时一次性生成完整思考与签名。
	decoder.thinking.Thinking = decoder.thinkingBuilder.String()
	decoder.thinking.Signature = decoder.thinkingSigBuilder.String()
	decoder.partial.Content[decoder.thinkIdx] = *decoder.thinking
	return append(events, llm.ResponseEvent{
		Type: llm.ResponseEventThinkingEnd, ContentIndex: decoder.thinkIdx,
		Content: decoder.thinking.Thinking, Partial: decoder.snapshot(),
	})
}

// endText 收尾当前文本块：builder 中未下发的尾部（停止序列截断
// 后剩余窗口内容已重置为截断文本，不会多发）补发为最后一条 delta，
// 再发 TextEnd。
func (decoder *responseDecoder) endText(events []llm.ResponseEvent) []llm.ResponseEvent {
	// textOpen 只在 text 缓冲建块后置位，开块即非空。
	if !decoder.textOpen {
		return events
	}
	decoder.textOpen = false
	// 有停止序列时尾部窗口可能还有未下发内容；截断时 builder 已被重置
	// 为截断文本且 textEmitted 不超过其长度，不会多发。
	if pending := decoder.textBuilder.String(); decoder.textEmitted < len(pending) {
		events = append(events, decoder.emitTextDelta(pending[decoder.textEmitted:]))
	}
	// 只在内容块结束时一次性生成完整文字，避免 O(n²) 拷贝。
	decoder.text.Text = decoder.textBuilder.String()
	decoder.partial.Content[decoder.textIdx] = *decoder.text
	return append(events, llm.ResponseEvent{
		Type: llm.ResponseEventTextEnd, ContentIndex: decoder.textIdx,
		Content: decoder.text.Text, Partial: decoder.snapshot(),
	})
}

// findTool 定位工具调用增量所属的 toolState：带 id 的帧按 id 精确匹配；
// 匹配不到且末位调用仍持占位 id 时，视为该占位调用迟到的真实 id（上游
// 同一调用的帧连续发送）。无 id 帧按位置并入末位调用——但帧上带了一个
// 与末位不同的名字时，它是首帧缺 id 的下一次调用而非续帧，归还会把两次
// 独立调用的参数粘在一起。
func (decoder *responseDecoder) findTool(delta *devinproto.ExaCodeiumCommonPb_ChatToolCall) *toolState {
	id := delta.GetId()
	for _, state := range decoder.tools {
		if id != "" && state.call.ID == id {
			return state
		}
	}
	if len(decoder.tools) == 0 {
		return nil
	}
	last := decoder.tools[len(decoder.tools)-1]
	if id != "" {
		if last.placeholderID {
			return last
		}
		return nil
	}
	if name := delta.GetName(); name != "" && last.call.Name != "" && name != last.call.Name {
		return nil
	}
	return last
}

// complete 产出正常收尾事件序列：先置 StopReason，再逐个收尾
// 打开的 thinking/text/tool 块（工具参数此时一次性成形并修偏），
// 最后发 Done——partial 即最终消息。
func (decoder *responseDecoder) complete(reason llm.StopReason) []llm.ResponseEvent {
	events := make([]llm.ResponseEvent, 0, len(decoder.tools)*3+3)
	decoder.partial.StopReason = reason
	events = decoder.endThinking(events)
	events = decoder.endText(events)
	for _, state := range decoder.tools {
		// 在结束时一次性把 Builder 中的完整参数转成 JSON，避免中间反复解析/拷贝。
		state.call.Arguments = json.RawMessage(state.arguments.String())
		if state.wrapped {
			// custom 声明工具的包装参数体：解出 input 原文下发；
			// 模型偏离包装 schema 时（裸文本/多键）整体按 freeform 原文透传。
			state.call.Custom = true
			state.call.Arguments = unwrapCustomToolArguments(state.arguments.String())
			// 补发单条完整 delta，让按增量累计输入的下游状态收敛到一致。
			events = append(events, llm.ResponseEvent{
				Type: llm.ResponseEventToolCallDelta, ContentIndex: state.contentIdx,
				ToolCallID: state.eventID, Delta: string(state.call.Arguments), Partial: decoder.snapshot(),
			})
		} else if state.call.Custom {
			// 原文即参数体（invalid_json_str 通道或客户端回灌的畸形 JSON），
			// 不走 JSON 校验与 XML 修复。
		} else if !llm.IsJSONObject(state.call.Arguments) {
			// swe 系模型偶尔把 XML 参数语法泄漏进 arguments_json（CLI 实测），
			// 先尝试把 <parameter name="X">v</parameter> 解回 JSON 再兜底 {}。
			if repaired, ok := repairLeakedXMLArguments(state.arguments.String()); ok {
				state.call.Arguments = repaired
			} else {
				state.call.Arguments = json.RawMessage(`{}`)
			}
		}
		decoder.partial.Content[state.contentIdx] = state.call
		events = append(events, llm.ResponseEvent{Type: llm.ResponseEventToolCallEnd, ContentIndex: state.contentIdx, ToolCall: &state.call, Partial: decoder.snapshot()})
	}
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventDone, Reason: reason, Message: &decoder.partial})
	decoder.finished = true
	return events
}

// fail 产出错误终止事件：partial 标记 error、携带原文与分类记录
// （消费方经 Failure 取 type/status/retry 结构事实，不再按文本反推），
// Done 语义由消费方按 Reason=error 映射为协议错误帧。仅由 finish 调用——
// 重复抑制在 finish 入口守卫。
func (decoder *responseDecoder) fail(err error) []llm.ResponseEvent {
	decoder.partial.StopReason = llm.StopReasonError
	decoder.partial.ErrorMessage = err.Error()
	decoder.partial.Failure = llm.Classify(err)
	decoder.finished = true
	return []llm.ResponseEvent{{Type: llm.ResponseEventError, Reason: llm.StopReasonError, Error: &decoder.partial}}
}

// mapStopReason 把上游 stop_reason 枚举归一到中间模型；上游把
// 补全时代的多种终止形态都塞在枚举里，同语义值合并映射。
func mapStopReason(reason devinproto.ExaCodeiumCommonPb_StopReason) llm.StopReason {
	switch reason {
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_TOKENS,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MAX_NEWLINES,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_INCOMPLETE,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_PARTIAL:
		// INCOMPLETE/PARTIAL 都表示模型没有生成完整回复，按长度截断处理。
		return llm.StopReasonLength
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FUNCTION_CALL:
		return llm.StopReasonToolUse
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_CONTENT_FILTER:
		return llm.StopReasonContentFilter
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_ERROR,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_NONFINITE_LOGIT_OR_PROB:
		return llm.StopReasonError
	case devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_STOP_PATTERN,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_MIN_LOG_PROB,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_EXIT_SCOPE,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_FIRST_NON_WHITESPACE_LINE,
		devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_NON_INSERTION:
		// STOP_PATTERN 是上游自身 stop-pattern 机制的正常结束（实测正常回复以此收尾），
		// 其余为补全时代的正常终止形态。
		return llm.StopReasonStop
	default:
		return llm.StopReasonStop
	}
}

// unwrapCustomToolArguments 解出包装 schema {"input":"<原文>"} 中的原文；
// 模型偏离包装（裸文本或 input 非字符串）时按 freeform 语义整体透传。
func unwrapCustomToolArguments(raw string) json.RawMessage {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil {
		return json.RawMessage(raw)
	}
	if input, ok := object["input"]; ok {
		var text string
		if err := json.Unmarshal(input, &text); err == nil {
			return json.RawMessage(text)
		}
	}
	return json.RawMessage(raw)
}

// leakedXMLParameterPattern 匹配泄漏进 arguments_json 的 XML 参数片段（CLI 实测格式）。
var leakedXMLParameterPattern = regexp.MustCompile(`<(?:antml:)?parameter\s+name="([A-Za-z_][\w-]*)"[^>]*>([\s\S]*?)</(?:antml:)?parameter>`)

// repairLeakedXMLArguments 把混进 arguments 的 XML 参数标签提取成 JSON 对象；
// 不含参数标签时返回 false，调用方走原有兜底路径。
func repairLeakedXMLArguments(raw string) (json.RawMessage, bool) {
	matches := leakedXMLParameterPattern.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return nil, false
	}
	object := make(map[string]string, len(matches))
	for _, match := range matches {
		object[match[1]] = strings.TrimSpace(match[2])
	}
	data, _ := json.Marshal(object)
	return data, true
}
