// 本文件实现「服务端托管搜索」的执行面：真实 Cascade 客户端的
// web_search 本来就是客户端代调上游 GetWebSearchResults 拿结果再回喂的
// 形态（二进制内嵌 RPC 路径字面量实测），代理在这里扮演同一角色。
// 两个消费方：Flow A（CC WebSearch 专用侧请求）整体短路成一次搜索；
// Flow B（codex 声明 {"type":"web_search"}）在响应侧拦截托管调用、
// 执行后续轮（见 devin.go 的 handleServerCalls）。
package devin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"

	devinproto "local/devinproto"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/debuglog"
	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/randid"
	"github.com/WncFht/devin2api/internal/upstream"
)

// serverSearchResultLimit 是单次托管搜索向上游要的结果数：真实 CLI 的
// web_search schema 默认 num_results=5，与之一致。
const serverSearchResultLimit = 5

// webSearchOutcome 是一次上游搜索的净结果：results 按 URL 去重，
// summary 是上游合成的答案正文（末次非空值生效）。
type webSearchOutcome struct {
	results []llm.WebSearchResult
	summary string
	url     string // 上游给出的查询落地页，进 diagnostics 留证
}

// runWebSearch 代调上游 GetWebSearchResults：allowedDomains 在上游是
// 单域字面量（多值实测返回 0 条），多域时逐域调用、按 URL 去重合并；
// blockedDomains 上游无对应字段，结果侧按 host 后缀过滤。每次调用都过
// 速率闸门——上游对 unary 调用同样计配额。返回的错误已经 llm.Classify
// 分类并写入 error.json。
//
// stem 决定本次调用的请求记录文件名（stem+".json"，多域扇出的第 N 域
// 为 stem+".attemptN.json"）：Flow A 整体短路时搜索就是首个上游请求，
// 传 "03-devin-request" 占主文件位；Flow B 续轮内 chat 重发已占用
// 主文件与 attemptN 编号空间，必须传 StageDevinSearchStem+seq 的独立
// 词干，否则每次搜索都覆盖首个 chat 请求、扇出文件与续轮分片互撞。
// warmKey 是所属保温 lineage 的簿记句柄（与 attemptRunner.send
// 的 noteSend 同口径）；Flow A 无 retained 条目时传入也仅 no-op。
func (adapter *Adapter) runWebSearch(ctx context.Context, query string, allowedDomains, blockedDomains []string, limit uint32, stem string, warmKey warmLineageKey) (webSearchOutcome, error) {
	var outcome webSearchOutcome
	env := attemptEnvFrom(ctx)
	recorder := env.recorder
	name, version, os := adapter.CurrentConfig().ClientIdentity()
	link := adapter.link()
	link.warmer.kickRequest()
	domains := allowedDomains
	if len(domains) == 0 {
		domains = []string{""}
	}
	seen := make(map[string]bool)
	for attempt, domain := range domains {
		if err := adapter.gate.wait(ctx, env, false); err != nil {
			var failure *llm.Failure
			if errors.As(err, &failure) && failure.LocalGate {
				recorder.WriteError(debuglog.ErrStageRateGate, err)
			}
			return outcome, err
		}
		request := &devinproto.GetWebSearchResultsRequest{
			Metadata: upstream.BuildMetadata(adapter.currentToken(), name, version, os, 366),
			Query:    proto.String(query),
			Limit:    proto.Uint32(limit),
		}
		if domain != "" {
			request.Domain = proto.String(domain)
		}
		recorder.NoteUpstreamSend()
		// 搜索是客户端可归因上行：与 attemptRunner.send 同口径推进
		// lastTouch——纯托管搜索流量也要让保温簿记看得见，否则条目在
		// 搜索期间被误判静默。
		adapter.warm.noteSend(warmKey)
		// 请求记录文件名由调用方给的词干派生；非 03 主文件的调用在
		// 04 留归因标记，否则多份搜索响应无法对应到具体请求文件。
		// chat 词干的发送序号跨 lane 共享分配（与 Stream 同一计数）：
		// 号池 failover 后新 lane 的首发续排 attemptN 分片而非覆写
		// 基座；searchN 词干只属于本 lane 的本次调用，按域序排。
		stage := stem + ".json"
		switch {
		case stem == debuglog.StageDevinRequestStem:
			if ordinal := recorder.NextDevinSendOrdinal(); ordinal > 1 {
				stage = fmt.Sprintf("%s.attempt%d.json", stem, ordinal)
			}
		case attempt > 0:
			stage = fmt.Sprintf("%s.attempt%d.json", stem, attempt+1)
		}
		if stage != debuglog.StageDevinRequest {
			recorder.AppendJSONL(debuglog.StageDevinResponse, "server_search_call", map[string]any{"stage": stage, "domain": domain})
		}
		recordProtoJSON(recorder, stage, request)
		// httptrace 随 ctx 进 transport：GotConn 报告本次发送拿到的是
		// 复用连接还是新握手——与 attemptRunner.send 同口径，
		// Flow A 下搜索就是首个上游调用，连接画像必须照样留证。
		var conn httptrace.GotConnInfo
		traceCtx := httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(info httptrace.GotConnInfo) { conn = info },
		})
		response, err := link.api.GetWebSearchResults(traceCtx, connect.NewRequest(request))
		if err != nil {
			adapter.gate.noteUpstreamError(err)
			stage := debuglog.ErrStageDevinConnect
			if isTransientConnectError(err) {
				stage = debuglog.ErrStageDevinTransport
			}
			recorder.WriteError(stage, err)
			return outcome, llm.Classify(err)
		}
		recorder.NoteUpstreamOpen()
		recorder.NoteUpstreamConn(conn.Reused, conn.IdleTime)
		recordProtoJSON(recorder, debuglog.StageDevinResponse, response.Msg)
		for _, item := range response.Msg.GetResults() {
			itemURL := item.GetUrl()
			if itemURL == "" || seen[itemURL] || domainBlocked(itemURL, blockedDomains) {
				continue
			}
			seen[itemURL] = true
			outcome.results = append(outcome.results, llm.WebSearchResult{
				Title: item.GetTitle(), URL: itemURL, Summary: item.GetSummary(),
			})
		}
		if summary := response.Msg.GetSummary(); summary != "" {
			outcome.summary = summary
		}
		if searchURL := response.Msg.GetWebSearchUrl(); searchURL != "" {
			outcome.url = searchURL
		}
	}
	return outcome, nil
}

// domainBlocked 按 host 后缀匹配判定结果 URL 是否落在 blocked 域里：
// "example.com" 同时屏蔽 example.com 与全部子域。
func domainBlocked(rawURL string, blocked []string) bool {
	if len(blocked) == 0 {
		return false
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, domain := range blocked {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// renderSearchResults 把搜索结果渲染成工具结果正文：上游合成的 summary
// 是主载荷；summary 缺席时退化为编号列表，空结果显式说明——模型续轮
// 与历史回放共用这份文本。
func renderSearchResults(query string, outcome webSearchOutcome) string {
	var text strings.Builder
	if outcome.summary != "" {
		text.WriteString(outcome.summary)
		if len(outcome.results) > 0 {
			text.WriteString("\n\nSources:")
			for index, result := range outcome.results {
				fmt.Fprintf(&text, "\n%d. %s — %s", index+1, result.Title, result.URL)
			}
		}
		return text.String()
	}
	if len(outcome.results) == 0 {
		return fmt.Sprintf("The web search for %q returned no results.", query)
	}
	fmt.Fprintf(&text, "Search results for %q:", query)
	for index, result := range outcome.results {
		fmt.Fprintf(&text, "\n%d. %s\n   %s", index+1, result.Title, result.URL)
		if result.Summary != "" {
			fmt.Fprintf(&text, "\n   %s", result.Summary)
		}
	}
	return text.String()
}

// serverSearchStream 是 Flow A 的响应流：搜索在 Stream() 内已同步完成
// （产出前的失败要拿到真实 HTTP 状态码，而不是已提交 200 后的 SSE
// error），这里只把预成形的事件序列逐条放给消费方。
type serverSearchStream struct {
	events []llm.ResponseEvent
	offset int
}

// Recv 逐条放出预成形事件，耗尽后返回 io.EOF。
func (stream *serverSearchStream) Recv(context.Context) (llm.ResponseEvent, error) {
	if stream.offset >= len(stream.events) {
		return llm.ResponseEvent{}, io.EOF
	}
	event := stream.events[stream.offset]
	stream.offset++
	return event, nil
}

// runServerSearch 执行 Flow A 整体短路：tools 只含 web_search_* 的
// 专用侧请求不打 GetChatMessage，直接代调搜索并把结果包装成
// 「server_tool_use → web_search_tool_result → text」的完整响应流——
// 正是 Anthropic 服务端搜索在 /v1/messages 上的原生形态。
func (adapter *Adapter) runServerSearch(ctx context.Context, request llm.RequestMessages, model string) (llm.ResponseStream, error) {
	search := request.ServerSearch
	outcome, err := adapter.runWebSearch(ctx, search.Query, search.AllowedDomains, search.BlockedDomains, serverSearchResultLimit, debuglog.StageDevinRequestStem, adapter.warm.keyOf(request, model))
	if err != nil {
		return nil, err
	}
	return &serverSearchStream{events: serverSearchEvents(model, search.Query, outcome)}, nil
}

// serverSearchEvents 把搜索结果组装成协议中立的完整事件序列：
// 调用块与结果块按 ToolCallID 配对，文本块携带上游合成摘要，
// partial 逐事件物化与真实解码器同一套快照语义。
func serverSearchEvents(model, query string, outcome webSearchOutcome) []llm.ResponseEvent {
	partial := &llm.AssistantMessage{
		API: "connect", Provider: "devin", Model: model,
		StopReason: llm.StopReasonPending, TimestampMS: time.Now().UnixMilli(),
	}
	snapshot := func() *llm.AssistantMessage {
		clone := *partial
		clone.Content = append([]llm.Content(nil), clone.Content...)
		return &clone
	}
	call := llm.ToolCall{ID: randid.Prefixed("srvtoolu_"), Name: "web_search", Server: true}
	arguments, _ := json.Marshal(map[string]string{"query": query})
	call.Arguments = arguments
	result := llm.ServerToolResult{
		ToolCallID: call.ID, ToolName: call.Name,
		SearchResults: outcome.results,
	}
	if outcome.summary != "" {
		result.Content = []llm.Content{llm.TextContent{Text: outcome.summary}}
	}
	text := renderSearchResults(query, outcome)
	if outcome.url != "" {
		details, _ := json.Marshal(map[string]string{"web_search_url": outcome.url})
		partial.Diagnostics = append(partial.Diagnostics, llm.AssistantMessageDiagnostic{
			Type: "server_search", TimestampMS: time.Now().UnixMilli(), Details: details,
		})
	}
	events := []llm.ResponseEvent{{Type: llm.ResponseEventStart, Partial: snapshot()}}
	partial.Content = append(partial.Content, call)
	events = append(events,
		llm.ResponseEvent{Type: llm.ResponseEventToolCallStart, ContentIndex: 0, ToolCallID: call.ID, ToolName: call.Name, Partial: snapshot()},
		llm.ResponseEvent{Type: llm.ResponseEventToolCallDelta, ContentIndex: 0, ToolCallID: call.ID, Delta: string(arguments), Partial: snapshot()},
		llm.ResponseEvent{Type: llm.ResponseEventToolCallEnd, ContentIndex: 0, ToolCall: &call, Partial: snapshot()},
	)
	partial.Content = append(partial.Content, result)
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventServerToolResult, ContentIndex: 1, ServerResult: &result, Partial: snapshot()})
	partial.Content = append(partial.Content, llm.TextContent{})
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextStart, ContentIndex: 2, Partial: snapshot()})
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextDelta, ContentIndex: 2, Delta: text, Partial: snapshot()})
	partial.Content[2] = llm.TextContent{Text: text}
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventTextEnd, ContentIndex: 2, Content: text, Partial: snapshot()})
	partial.StopReason = llm.StopReasonStop
	events = append(events, llm.ResponseEvent{Type: llm.ResponseEventDone, Reason: llm.StopReasonStop, Message: partial})
	return events
}

// maxServerSearchHops 是单次响应内允许的托管续轮上限：模型理论上可以
// 每轮再发搜索，封顶防止一个请求在代理内部无限滚上游配额。
const maxServerSearchHops = 8

// executeServerCall 执行一次服务端托管调用并把结果成形为
// ServerToolResult：参数缺失与搜索失败都落成 IsError 结果（模型在续轮
// 里看到失败正文自行兜底），而不是把整支流打成错误。
func (stream *responseStream) executeServerCall(ctx context.Context, call llm.ToolCall) llm.ServerToolResult {
	result := llm.ServerToolResult{ToolCallID: call.ID, ToolName: call.Name}
	var arguments struct {
		Query      string `json:"query"`
		NumResults uint32 `json:"num_results"`
		Domain     string `json:"domain"`
	}
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil || arguments.Query == "" {
		result.IsError = true
		result.ErrorCode = "invalid_arguments"
		result.Content = []llm.Content{llm.TextContent{Text: "web search call is missing a valid query argument"}}
		return result
	}
	var allowed []string
	if arguments.Domain != "" {
		allowed = []string{arguments.Domain}
	}
	limit := arguments.NumResults
	if limit == 0 {
		limit = serverSearchResultLimit
	}
	outcome, err := stream.search(ctx, arguments.Query, allowed, nil, limit)
	if err != nil {
		result.IsError = true
		result.ErrorCode = "search_failed"
		if failure := llm.Classify(err); failure != nil && failure.Code != "" {
			result.ErrorCode = failure.Code
		}
		result.Content = []llm.Content{llm.TextContent{Text: fmt.Sprintf("web search failed: %s", err)}}
		return result
	}
	result.SearchResults = outcome.results
	result.Content = []llm.Content{llm.TextContent{Text: renderSearchResults(arguments.Query, outcome)}}
	return result
}

// handleServerCalls 在回合产出服务端托管调用时代执行并决定去向：
//   - 纯托管回合：剥掉 Done、执行搜索、把结果块注回事件流，然后续轮
//     （重发扩展后的请求，新解码器以已有内容为种子）——返回 true。
//   - 混合回合（托管+客户端调用并存）：执行搜索并注入结果，Done 保留——
//     客户端调用还等客户端执行，续轮责任不在代理——返回 false。
//   - 触顶/续轮失败：结果块照常注入（触顶按 max_uses_exceeded 错误结果），
//     Done 保留收尾——返回 false。
//
// 三种去向里结果块都进入 partial，回放侧把它们翻译成 call+result 对上行。
func (stream *responseStream) handleServerCalls(ctx context.Context, events *[]llm.ResponseEvent) bool {
	if stream.search == nil {
		return false
	}
	doneIndex := -1
	for index, event := range *events {
		if event.Type == llm.ResponseEventDone {
			doneIndex = index
		}
	}
	if doneIndex < 0 {
		return false
	}
	// 已注入结果的调用不重复执行：续轮解码器以旧内容为种子，上轮的
	// Server 调用块还在 partial 里，靠结果块的 ToolCallID 配对识别。
	answered := make(map[string]bool)
	var serverCalls, clientCalls []llm.ToolCall
	for _, block := range stream.decoder.partial.Content {
		if result, ok := block.(llm.ServerToolResult); ok {
			answered[result.ToolCallID] = true
		}
	}
	for _, block := range stream.decoder.partial.Content {
		call, ok := block.(llm.ToolCall)
		if !ok || answered[call.ID] {
			continue
		}
		if call.Server {
			serverCalls = append(serverCalls, call)
		} else {
			clientCalls = append(clientCalls, call)
		}
	}
	if len(serverCalls) == 0 {
		return false
	}
	resultEvents := make([]llm.ResponseEvent, 0, len(serverCalls))
	for _, call := range serverCalls {
		var result llm.ServerToolResult
		if stream.hops >= maxServerSearchHops {
			result = llm.ServerToolResult{
				ToolCallID: call.ID, ToolName: call.Name, IsError: true,
				ErrorCode: "max_uses_exceeded",
				Content:   []llm.Content{llm.TextContent{Text: "web search limit reached for this response"}},
			}
		} else {
			result = stream.executeServerCall(ctx, call)
		}
		resultEvents = append(resultEvents, stream.decoder.appendServerResult(result))
	}
	// 续轮 wire 上每个 Server 调用都要有配对结果：partial 里的结果块
	//（含前序各跳已回答的）按内容序收集成 TOOL 消息序列——只带本跳
	// 结果会把早先已回答的调用裸发上行，触发上游 invalid_argument。
	resultMessages := make([]llm.Message, 0, len(serverCalls))
	for _, block := range stream.decoder.partial.Content {
		result, ok := block.(llm.ServerToolResult)
		if !ok {
			continue
		}
		resultMessages = append(resultMessages, llm.ToolResultMessage{
			ToolCallID: result.ToolCallID, IsError: result.IsError, TimestampMS: time.Now().UnixMilli(),
			Content: result.Content,
		})
	}
	// 结果事件插在 Done 之前；续轮路径把 Done 从对外事件里摘除。
	// Done 先拷出：向 [:doneIndex] 追加会覆写共享底层数组里它的原位。
	done := (*events)[doneIndex]
	tail := append((*events)[:doneIndex], resultEvents...)
	if len(clientCalls) > 0 || stream.hops >= maxServerSearchHops {
		*events = append(tail, done)
		return false
	}
	*events = tail
	// 续轮的 wire 形态：assistant 回显不含结果块（结果走 TOOL 消息），
	// 解码器种子则带上结果块保住 ContentIndex 连续性。
	assistant := stream.decoder.partial
	wireContent := make([]llm.Content, 0, len(assistant.Content))
	for _, block := range assistant.Content {
		if _, isResult := block.(llm.ServerToolResult); isResult {
			continue
		}
		wireContent = append(wireContent, block)
	}
	assistant.Content = wireContent
	extra := make([]llm.Message, 0, len(resultMessages)+1)
	extra = append(extra, assistant)
	extra = append(extra, resultMessages...)
	frames, cancel, decoder, err := stream.extend("server_tool continuation", extra, stream.decoder.partial.Content)
	if err != nil {
		stream.recorder.AppendJSONL(debuglog.StageDevinResponse, "server_tool_continuation_failed", map[string]any{"error": err.Error()})
		// 续轮失败时回合照常收尾：客户端拿到了调用与结果，下一轮
		// 请求的历史回放天然携带它们，代理不必硬续。
		*events = append(*events, done)
		return false
	}
	stream.hops++
	// 上游按跳分别计 credit_cost：Done 的 Usage 只带本跳读数，已完成
	// 续轮跳的累计花费进 costsCarry，收尾时并入最终消息。累加必须在
	// extend 成功之后——续轮失败时本跳 Done 照常下发，其 CreditCost
	// 就是终值，提前入账会被 applyCostsCarry 再叠一遍（双计）。
	if costs := assistant.Usage.Costs; costs != nil {
		stream.costsCarry += costs.CreditCost
	}
	// 换流不变量见 swap；续轮与 pre-content 重开的差异只在解码器
	// 播种——这里传入的是 continueTurn 已播种旧内容的解码器。
	stream.swap(frames, cancel, decoder)
	return true
}

// applyCostsCarry 把前面各跳累计的 CreditCost 并入 Done 消息的 Usage：
// token 数反映最后一跳的上下文（含搜索结果的完整输入），计费字段则
// 按跳求和——token 是「这轮多大」，cost 是「这次总共花了多少」。
func (stream *responseStream) applyCostsCarry(events []llm.ResponseEvent) {
	if stream.costsCarry == 0 {
		return
	}
	for index, event := range events {
		if event.Type != llm.ResponseEventDone || event.Message == nil {
			continue
		}
		if event.Message.Usage.Costs == nil {
			event.Message.Usage.Costs = &llm.UpstreamCosts{}
		}
		event.Message.Usage.Costs.CreditCost += stream.costsCarry
		events[index] = event
	}
}
