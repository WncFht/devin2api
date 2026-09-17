// 本文件移植 ccLoad app/response_merge.go（MIT）：把上游响应体（SSE 流
// 或整段 JSON）合并成「reasoning/content/tools」三段可读视图，支撑调试
// 模态的 merged-response 端点。与上游协议的 collector 并列新增 devin
// 帧回放：04-devin-response.jsonl 逐行喂给同一 builder。
package ccpanel

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

// mergedResponseParts 对应 ccLoad 同名类型：merged-response 的返回形状。
type mergedResponseParts struct {
	Reasoning string `json:"reasoning"`
	Content   string `json:"content"`
	Tools     string `json:"tools,omitempty"`
}

// mergedToolCall 对应 ccLoad 同名类型。
type mergedToolCall struct {
	key   string
	name  string
	value string
}

// devinToolCall 累积一个 devin 调用的增量：deltaToolCalls 首元素带
// {id,name}、后续只带 {argumentsJson:chunk}，数组位置即调用序号。
// 交错到达的多调用靠 per-index 独立缓冲——共用 builder.toolDelta 的
// 单调 key 语义会把不同调用的参数拼串。
type devinToolCall struct {
	id   string
	name string
	args strings.Builder
}

// mergedResponseBuilder 对应 ccLoad 同名类型，外加 devinToolCalls
// per-index 缓冲。
type mergedResponseBuilder struct {
	reasoning          strings.Builder
	content            strings.Builder
	toolCalls          []mergedToolCall
	toolCallIndexes    map[string]int
	toolDelta          strings.Builder
	toolDeltaName      string
	toolDeltaKey       string
	toolNamesByIndex   map[string]string
	openAIToolKeys     map[string]string
	streamState        chatFrontendStreamState
	lastContentItemKey string
	devinToolCalls     map[int]*devinToolCall
}

const codexMessageItemSeparator = "\n\n---\n\n"

// mergeResponseBody 对应 ccLoad 同名函数（encoding/json 版）：先按 SSE
// 事件流解析，再按单文档 JSON 解析，最后按行解析 JSONL（本服务 04
// 阶段文件的记录形态）；都不产出内容时回退为 JSON 美化文本。
func mergeResponseBody(raw string) mergedResponseParts {
	body := stripHTTPResponseEnvelope(strings.ReplaceAll(raw, "\r\n", "\n"))
	if strings.TrimSpace(body) == "" {
		return mergedResponseParts{}
	}

	builder := &mergedResponseBuilder{}
	payloads := parseSSEJSONPayloads(body)
	if len(payloads) > 0 {
		for _, payload := range payloads {
			builder.collectPayload(payload)
		}
		if parts := builder.parts(); hasMergedParts(parts) {
			return parts
		}
		return mergedResponseParts{Content: formatJSONForMergedContent(body)}
	}

	var obj map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(body)), &obj); err == nil {
		builder.collectPayload(obj)
		if parts := builder.parts(); hasMergedParts(parts) {
			return parts
		}
	}

	// 04-devin-response.jsonl 这类逐行 JSON 记录：每行一个独立帧，
	// 簿记行（{seq,time,event,data}）没有 collector 认的键，自然跳过。
	if strings.Contains(body, "\n") {
		for line := range strings.Lines(body) {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var frame map[string]any
			if json.Unmarshal([]byte(line), &frame) == nil {
				builder.collectPayload(frame)
			}
		}
		if parts := builder.parts(); hasMergedParts(parts) {
			return parts
		}
	}

	return mergedResponseParts{Content: formatJSONForMergedContent(body)}
}

// stripHTTPResponseEnvelope 对应 ccLoad 同名函数：剥掉记录里可能混入的
// HTTP 响应头（HTTP/1.1 200 ... 空行分隔）。
func stripHTTPResponseEnvelope(raw string) string {
	headerBreak := strings.Index(raw, "\n\n")
	if headerBreak < 0 {
		return strings.TrimSpace(raw)
	}
	firstLine := strings.TrimSpace(strings.Split(raw, "\n")[0])
	if !strings.HasPrefix(strings.ToUpper(firstLine), "HTTP/") {
		return strings.TrimSpace(raw)
	}
	return strings.TrimSpace(raw[headerBreak+2:])
}

// parseSSEJSONPayloads 对应 ccLoad 同名函数：把 SSE 帧的 data: 载荷
// 逐帧解析成 JSON 对象；多行 data 按 SSE 规范拼接。
func parseSSEJSONPayloads(body string) []map[string]any {
	payloads := make([]map[string]any, 0)
	dataLines := make([]string, 0, 1)

	flush := func() {
		if len(dataLines) == 0 {
			return
		}
		raw := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if raw == "" || raw == "[DONE]" {
			return
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(raw), &obj); err == nil {
			payloads = append(payloads, obj)
		}
	}

	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			dataLines = append(dataLines, value)
			continue
		}
		if strings.TrimSpace(line) == "" {
			flush()
		}
	}
	flush()
	return payloads
}

// collectPayload 对应 ccLoad 同名函数：一个事件对象按各上游协议形态
// 分发给对应 collector，末尾追加本服务的 devin 帧形态。
func (b *mergedResponseBuilder) collectPayload(obj map[string]any) {
	if obj == nil {
		return
	}
	if response, ok := obj["response"].(map[string]any); ok {
		if _, ok := response["candidates"].([]any); ok {
			obj = response
		}
	}
	if _, ok := obj["candidates"].([]any); ok {
		b.collectGeminiContent(obj)
		return
	}
	if thinking := extractSSEThinkingDelta(obj); thinking != "" {
		b.appendReasoning(thinking)
	}
	if b.appendCodexTextDelta(obj) {
		// Codex text deltas carry item identity; keep item boundaries visible.
	} else if delta := extractSSEDeltaText(obj); delta != "" {
		b.appendTextDelta(delta)
	}
	b.collectOpenAIMessage(obj)
	b.collectGeminiContent(obj)
	b.collectCodexPayload(obj)
	b.collectAnthropicPayload(obj)
	b.collectDevinFrame(obj)
}

// collectDevinFrame 是本服务新增的 collector：04-devin-response.jsonl
// 的上游原始帧——deltaThinking 是思考增量、deltaText 是正文增量、
// deltaToolCalls 的数组位置即调用序号（首帧带 {id,name}，后续帧只带
// {argumentsJson:chunk}）。
func (b *mergedResponseBuilder) collectDevinFrame(obj map[string]any) {
	if thinking := stringFromAny(obj["deltaThinking"]); thinking != "" {
		b.appendReasoning(thinking)
	}
	if text := stringFromAny(obj["deltaText"]); text != "" {
		b.appendTextDelta(text)
	}
	calls, ok := obj["deltaToolCalls"].([]any)
	if !ok {
		return
	}
	for i, callValue := range calls {
		call, ok := callValue.(map[string]any)
		if !ok {
			continue
		}
		if b.devinToolCalls == nil {
			b.devinToolCalls = make(map[int]*devinToolCall)
		}
		tc := b.devinToolCalls[i]
		if tc == nil {
			tc = &devinToolCall{}
			b.devinToolCalls[i] = tc
		}
		if id := stringFromAny(call["id"]); id != "" {
			tc.id = id
		}
		if name := stringFromAny(call["name"]); name != "" {
			tc.name = name
		}
		tc.args.WriteString(stringFromAny(call["argumentsJson"]))
	}
}

// collectOpenAIMessage 对应 ccLoad 同名函数：OpenAI chat 的 choices/
// message/tool_calls 形态。
func (b *mergedResponseBuilder) collectOpenAIMessage(obj map[string]any) {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return
	}
	for _, choiceValue := range choices {
		choice, ok := choiceValue.(map[string]any)
		if !ok {
			continue
		}
		choiceIndex := indexKeyFromAny(choice["index"])
		if delta, ok := choice["delta"].(map[string]any); ok {
			b.collectToolCalls(delta["tool_calls"], true, choiceIndex)
		}
		if message, ok := choice["message"].(map[string]any); ok {
			b.appendReasoningString(message["reasoning_content"])
			b.appendReasoningString(message["reasoning"])
			b.appendContentValue(message["content"])
			b.collectToolCalls(message["tool_calls"], false, choiceIndex)
		}
	}
}

// collectGeminiContent 对应 ccLoad 同名函数：candidates/parts，
// thought=true 的 part 进 reasoning。
func (b *mergedResponseBuilder) collectGeminiContent(obj map[string]any) {
	candidates, ok := obj["candidates"].([]any)
	if !ok {
		return
	}
	for _, candidateValue := range candidates {
		candidate, ok := candidateValue.(map[string]any)
		if !ok {
			continue
		}
		content, ok := candidate["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, ok := content["parts"].([]any)
		if !ok {
			continue
		}
		for _, partValue := range parts {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			targetThinking, _ := part["thought"].(bool)
			if targetThinking {
				b.appendReasoningString(part["text"])
			} else {
				b.appendContentValue(part["text"])
			}
		}
	}
}

// collectCodexPayload 对应 ccLoad 同名函数：Responses API 的
// response.* 事件族。
func (b *mergedResponseBuilder) collectCodexPayload(obj map[string]any) {
	typ, _ := obj["type"].(string)
	switch typ {
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		index := indexKeyFromAny(obj["output_index"])
		b.appendToolDelta(toolKeyFromPayload(obj), b.toolNameForIndex(index, "tool_call"), obj["delta"])
	case "response.function_call_arguments.done":
		index := indexKeyFromAny(obj["output_index"])
		b.appendToolCall(toolKeyFromPayload(obj), b.toolNameForIndex(index, "tool_call"), obj["arguments"])
	case "response.output_item.added":
		b.rememberCodexToolName(obj)
	case "response.output_item.done":
		if item, ok := obj["item"].(map[string]any); ok && b.collectCodexOutputItem(item, obj) {
			return
		}
	}

	output, ok := obj["output"].([]any)
	if !ok {
		return
	}
	for _, itemValue := range output {
		item, ok := itemValue.(map[string]any)
		if !ok {
			continue
		}
		if b.collectCodexOutputItem(item, nil) {
			continue
		}
		itemKey := stringFromAny(item["id"])
		if itemKey == "" {
			itemKey = stringFromAny(item["item_id"])
		}
		content, ok := item["content"].([]any)
		if !ok {
			continue
		}
		for _, partValue := range content {
			part, ok := partValue.(map[string]any)
			if !ok {
				continue
			}
			b.beginContentItem(itemKey)
			b.appendContentValue(part["text"])
		}
	}
}

// collectAnthropicPayload 对应 ccLoad 同名函数：content_block_delta 的
// input_json_delta 与整段 content 块文本。
func (b *mergedResponseBuilder) collectAnthropicPayload(obj map[string]any) {
	if delta, ok := obj["delta"].(map[string]any); ok {
		key := toolKeyFromIndex(indexKeyFromAny(obj["index"]))
		b.appendToolDelta(key, "tool_call", delta["partial_json"])
	}
	content, ok := obj["content"].([]any)
	if !ok {
		return
	}
	for _, blockValue := range content {
		block, ok := blockValue.(map[string]any)
		if !ok {
			continue
		}
		b.appendContentValue(block["text"])
	}
}

// collectToolCalls 对应 ccLoad 同名函数：OpenAI function.tool_calls 的
// 流式/整段两种形态。
func (b *mergedResponseBuilder) collectToolCalls(value any, streaming bool, choiceIndex string) {
	calls, ok := value.([]any)
	if !ok {
		return
	}
	for _, callValue := range calls {
		call, ok := callValue.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := call["function"].(map[string]any); ok {
			key := toolKeyFromOpenAIToolCall(call)
			if streaming {
				key = b.openAIStreamingToolKey(choiceIndex, call)
			}
			if streaming && key != "" {
				b.appendToolDelta(key, stringFromAny(fn["name"]), fn["arguments"])
			} else {
				b.appendToolCall(key, fn["name"], fn["arguments"])
			}
		}
	}
}

// appendTextDelta 对应 ccLoad 同名函数：文本增量过 think-tag 拆分，
// thinking 段进 reasoning。
func (b *mergedResponseBuilder) appendTextDelta(delta string) {
	for _, part := range splitChatTextDeltaParts(delta, &b.streamState) {
		if part.text == "" {
			continue
		}
		if part.kind == "thinking" {
			b.appendReasoning(part.text)
		} else {
			b.appendContent(part.text)
		}
	}
}

// appendCodexTextDelta 对应 ccLoad 同名函数：response.output_text.delta
// / refusal.delta 携带 item 身份，维持条目边界。
func (b *mergedResponseBuilder) appendCodexTextDelta(obj map[string]any) bool {
	typ, _ := obj["type"].(string)
	if typ != "response.output_text.delta" && typ != "response.refusal.delta" {
		return false
	}
	delta := stringFromAny(obj["delta"])
	if delta == "" {
		return true
	}
	itemKey := stringFromAny(obj["item_id"])
	if itemKey == "" {
		itemKey = stringFromAny(obj["output_index"])
	}
	b.beginContentItem(itemKey)
	b.appendTextDelta(delta)
	return true
}

// beginContentItem 对应 ccLoad 同名函数：Codex 多 item 之间插分隔线。
func (b *mergedResponseBuilder) beginContentItem(itemKey string) {
	if itemKey == "" {
		return
	}
	if b.lastContentItemKey != "" && b.lastContentItemKey != itemKey && b.content.Len() > 0 {
		b.content.WriteString(codexMessageItemSeparator)
	}
	b.lastContentItemKey = itemKey
}

// appendContentValue 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) appendContentValue(value any) {
	switch v := value.(type) {
	case string:
		b.appendContent(v)
	case []any:
		for _, item := range v {
			b.appendContentValue(item)
		}
	case map[string]any:
		if text := stringFromAny(v["text"]); text != "" {
			b.appendContent(text)
		} else if content := stringFromAny(v["content"]); content != "" {
			b.appendContent(content)
		}
	}
}

// appendReasoningString 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) appendReasoningString(value any) {
	if text := stringFromAny(value); text != "" {
		b.appendReasoning(text)
	}
}

// appendToolCall 对应 ccLoad 同名函数：整段工具调用落库，先冲刷
// 同 key 的流式增量缓冲。
func (b *mergedResponseBuilder) appendToolCall(key string, name any, value any) {
	text := stringFromAny(value)
	if text == "" {
		return
	}
	if b.toolDelta.Len() > 0 {
		if key != "" && b.toolDeltaKey == key {
			b.clearToolDelta()
		} else {
			b.flushToolDelta()
		}
	}
	b.storeToolCall(key, stringFromAny(name), text)
}

// appendReasoning 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) appendReasoning(text string) {
	b.reasoning.WriteString(text)
}

// appendContent 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) appendContent(text string) {
	b.content.WriteString(text)
}

// parts 对应 ccLoad 同名函数：冲刷流式工具增量与 devin per-index
// 缓冲后成形三段输出。
func (b *mergedResponseBuilder) parts() mergedResponseParts {
	b.flushToolDelta()
	if len(b.devinToolCalls) > 0 {
		indexes := make([]int, 0, len(b.devinToolCalls))
		for i := range b.devinToolCalls {
			indexes = append(indexes, i)
		}
		sort.Ints(indexes)
		for _, i := range indexes {
			tc := b.devinToolCalls[i]
			args := strings.TrimSpace(tc.args.String())
			if args == "" {
				continue
			}
			key := ""
			if tc.id != "" {
				key = "id:" + tc.id
			}
			b.storeToolCall(key, tc.name, args)
		}
	}
	return mergedResponseParts{
		Reasoning: strings.TrimSpace(b.reasoning.String()),
		Content:   formatJSONForMergedContent(strings.TrimSpace(b.content.String())),
		Tools:     formatMergedToolDiagnostics(strings.TrimSpace(b.toolCallsMarkdown())),
	}
}

// collectCodexOutputItem 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) collectCodexOutputItem(item map[string]any, event map[string]any) bool {
	itemType := stringFromAny(item["type"])
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return false
	}
	value := item["arguments"]
	if itemType == "custom_tool_call" {
		value = item["input"]
	}
	b.appendToolCall(toolKeyFromCodexItem(item, event), item["name"], value)
	return true
}

// rememberCodexToolName 对应 ccLoad 同名函数：output_item.added 里记下
// output_index → 工具名映射，供后续 delta/done 帧找回名字。
func (b *mergedResponseBuilder) rememberCodexToolName(obj map[string]any) {
	item, ok := obj["item"].(map[string]any)
	if !ok {
		return
	}
	itemType := stringFromAny(item["type"])
	if itemType != "function_call" && itemType != "custom_tool_call" {
		return
	}
	index := indexKeyFromAny(obj["output_index"])
	if index == "" {
		index = indexKeyFromAny(item["output_index"])
	}
	name := stringFromAny(item["name"])
	if index == "" || name == "" {
		return
	}
	if b.toolNamesByIndex == nil {
		b.toolNamesByIndex = make(map[string]string)
	}
	b.toolNamesByIndex[index] = name
}

// toolNameForIndex 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) toolNameForIndex(index string, fallback string) string {
	if index != "" && b.toolNamesByIndex != nil {
		if name := b.toolNamesByIndex[index]; name != "" {
			return name
		}
	}
	return fallback
}

// appendToolDelta 对应 ccLoad 同名函数：流式工具参数增量按 key 累积。
func (b *mergedResponseBuilder) appendToolDelta(key string, name string, value any) {
	text := stringFromAny(value)
	if text == "" {
		return
	}
	if b.toolDelta.Len() > 0 && b.toolDeltaKey != "" && key != "" && b.toolDeltaKey != key {
		b.flushToolDelta()
	}
	if b.toolDeltaName == "" {
		b.toolDeltaName = name
	}
	if key != "" {
		b.toolDeltaKey = key
	}
	b.toolDelta.WriteString(text)
}

// flushToolDelta 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) flushToolDelta() {
	text := strings.TrimSpace(b.toolDelta.String())
	if text == "" {
		b.clearToolDelta()
		return
	}
	b.storeToolCall(b.toolDeltaKey, b.toolDeltaName, text)
	b.clearToolDelta()
}

// clearToolDelta 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) clearToolDelta() {
	b.toolDelta.Reset()
	b.toolDeltaName = ""
	b.toolDeltaKey = ""
}

// storeToolCall 对应 ccLoad 同名函数：同 key 覆盖（整段到达时替换掉
// 之前累积的增量），无 key 追加。
func (b *mergedResponseBuilder) storeToolCall(key string, name string, value string) {
	if key != "" {
		if b.toolCallIndexes == nil {
			b.toolCallIndexes = make(map[string]int)
		}
		if idx, ok := b.toolCallIndexes[key]; ok {
			if name != "" {
				b.toolCalls[idx].name = name
			}
			b.toolCalls[idx].value = value
			return
		}
		b.toolCallIndexes[key] = len(b.toolCalls)
	}
	b.toolCalls = append(b.toolCalls, mergedToolCall{
		key:   key,
		name:  name,
		value: value,
	})
}

// toolCallsMarkdown 对应 ccLoad 同名函数。
func (b *mergedResponseBuilder) toolCallsMarkdown() string {
	if len(b.toolCalls) == 0 {
		return ""
	}
	sections := make([]string, 0, len(b.toolCalls))
	for _, call := range b.toolCalls {
		sections = append(sections, formatToolCallMarkdown(call.name, call.value))
	}
	return strings.Join(sections, "\n\n")
}

// hasMergedParts 对应 ccLoad 同名函数。
func hasMergedParts(parts mergedResponseParts) bool {
	return parts.Reasoning != "" || parts.Content != "" || parts.Tools != ""
}

// stringFromAny 对应 ccLoad 同名函数。
func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}

// indexKeyFromAny 对应 ccLoad 同名函数：index 字段兼容字符串/数字。
func indexKeyFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

// toolKeyFromIndex 对应 ccLoad 同名函数。
func toolKeyFromIndex(index string) string {
	if index == "" {
		return ""
	}
	return "index:" + index
}

// toolKeyFromPayload 对应 ccLoad 同名函数。
func toolKeyFromPayload(obj map[string]any) string {
	if obj == nil {
		return ""
	}
	if id := stringFromAny(obj["item_id"]); id != "" {
		return "id:" + id
	}
	if callID := stringFromAny(obj["call_id"]); callID != "" {
		return "call:" + callID
	}
	if key := toolKeyFromIndex(indexKeyFromAny(obj["output_index"])); key != "" {
		return key
	}
	return ""
}

// toolKeyFromCodexItem 对应 ccLoad 同名函数。
func toolKeyFromCodexItem(item map[string]any, event map[string]any) string {
	if id := stringFromAny(item["id"]); id != "" {
		return "id:" + id
	}
	if callID := stringFromAny(item["call_id"]); callID != "" {
		return "call:" + callID
	}
	if event != nil {
		if key := toolKeyFromIndex(indexKeyFromAny(event["output_index"])); key != "" {
			return key
		}
	}
	return toolKeyFromIndex(indexKeyFromAny(item["output_index"]))
}

// toolKeyFromOpenAIToolCall 对应 ccLoad 同名函数。
func toolKeyFromOpenAIToolCall(call map[string]any) string {
	if id := stringFromAny(call["id"]); id != "" {
		return "id:" + id
	}
	return toolKeyFromIndex(indexKeyFromAny(call["index"]))
}

// openAIStreamingToolKey 对应 ccLoad 同名函数：OpenAI 流式 tool_calls
// 的 id 只出现在首帧，按 choice:index 槽位记住后续增量的归属 key。
func (b *mergedResponseBuilder) openAIStreamingToolKey(choiceIndex string, call map[string]any) string {
	index := indexKeyFromAny(call["index"])
	if index == "" {
		return toolKeyFromOpenAIToolCall(call)
	}
	slot := choiceIndex + ":" + index
	if id := stringFromAny(call["id"]); id != "" {
		key := "id:" + id
		if b.openAIToolKeys == nil {
			b.openAIToolKeys = make(map[string]string)
		}
		b.openAIToolKeys[slot] = key
		return key
	}
	if b.openAIToolKeys != nil {
		if key := b.openAIToolKeys[slot]; key != "" {
			return key
		}
	}
	return toolKeyFromIndex(slot)
}

// formatJSONForMergedContent 对应 ccLoad 同名函数：能解析成 JSON 的
// 文本美化后包进 json 代码围栏，否则原样返回。
func formatJSONForMergedContent(text string) string {
	raw := strings.TrimSpace(text)
	if raw == "" || (!strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[")) {
		return text
	}
	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return text
	}
	formatted, err := json.MarshalIndent(parsed, "", "  ")
	if err != nil {
		return text
	}
	return codeFence("json", string(formatted))
}

// formatMergedToolDiagnostics 对应 ccLoad 同名函数。
func formatMergedToolDiagnostics(text string) string {
	raw := strings.TrimSpace(text)
	if raw == "" || strings.Contains(raw, "### ") {
		return raw
	}
	return formatToolCallMarkdown("tool_call", raw)
}

// formatToolCallMarkdown 对应 ccLoad 同名函数：工具调用渲染成
// ### name + 代码围栏；参数是 JSON 时美化，cmd 字段特判为 bash。
func formatToolCallMarkdown(name string, value string) string {
	toolName := strings.TrimSpace(name)
	if toolName == "" {
		toolName = "tool_call"
	}

	values, ok := parseToolArgumentJSONValues(value)
	if ok {
		sections := make([]string, 0, len(values))
		for _, parsed := range values {
			sections = append(sections, formatSingleToolCallMarkdown(toolName, parsed, value))
		}
		return strings.Join(sections, "\n\n")
	}

	return "### " + toolName + "\n\n" + codeFence(toolCallRawLanguage(toolName, value), value)
}

// formatSingleToolCallMarkdown 对应 ccLoad 同名函数。
func formatSingleToolCallMarkdown(toolName string, parsed any, original string) string {
	if obj, ok := parsed.(map[string]any); ok {
		if cmd, ok := obj["cmd"].(string); ok && strings.TrimSpace(cmd) != "" {
			return "### exec_command\n\n" + codeFence("bash", cmd)
		}
		formatted, err := json.MarshalIndent(obj, "", "  ")
		if err == nil {
			return "### " + toolName + "\n\n" + codeFence("json", string(formatted))
		}
	}
	return "### " + toolName + "\n\n" + codeFence(toolCallRawLanguage(toolName, original), original)
}

// toolCallRawLanguage 对应 ccLoad 同名函数：apply_patch 参数按 diff 高亮。
func toolCallRawLanguage(toolName string, value string) string {
	name := strings.ToLower(strings.TrimSpace(toolName))
	text := strings.TrimSpace(value)
	if name == "apply_patch" || strings.HasPrefix(text, "*** Begin Patch") {
		return "diff"
	}
	return ""
}

// parseToolArgumentJSONValues 对应 ccLoad 同名函数：参数值可能是一串
// 拼接的 JSON 文档（多段 done），逐个解出。
func parseToolArgumentJSONValues(value string) ([]any, bool) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return nil, false
	}

	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	values := make([]any, 0, 1)
	for {
		var parsed any
		if err := decoder.Decode(&parsed); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, false
		}
		values = append(values, parsed)
	}
	return values, len(values) > 0
}

// codeFence 对应 ccLoad 同名函数：围栏长度自适应内容里的反引号。
func codeFence(language, value string) string {
	fence := "```"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	return fence + language + "\n" + value + "\n" + fence
}

// chatFrontendStreamState 对应 ccLoad 同名类型：跨增量记住
// think-tag 开合状态。
type chatFrontendStreamState struct {
	thinkTagOpen bool
}

// chatTextDeltaPart 对应 ccLoad 同名类型。
type chatTextDeltaPart struct {
	kind string
	text string
}

// splitChatTextDeltaParts 对应 ccLoad 同名函数：把文本增量按
// <think>/<thinking> 标签切成 thinking/text 片段，标签可跨增量。
func splitChatTextDeltaParts(delta string, state *chatFrontendStreamState) []chatTextDeltaPart {
	if state == nil {
		if thinking, text := splitThinkTaggedText(delta); thinking != "" {
			parts := []chatTextDeltaPart{{kind: "thinking", text: thinking}}
			if text != "" {
				parts = append(parts, chatTextDeltaPart{kind: "text", text: text})
			}
			return parts
		}
		return []chatTextDeltaPart{{kind: "text", text: delta}}
	}

	parts := make([]chatTextDeltaPart, 0, 1)
	remaining := delta
	for remaining != "" {
		if state.thinkTagOpen {
			closeIdx, closeLen := findThinkCloseTag(remaining)
			if closeIdx < 0 {
				parts = appendNonEmptyChatTextPart(parts, "thinking", remaining)
				return parts
			}
			parts = appendNonEmptyChatTextPart(parts, "thinking", remaining[:closeIdx])
			remaining = remaining[closeIdx+closeLen:]
			state.thinkTagOpen = false
			continue
		}

		openIdx, openLen := findThinkOpenTag(remaining)
		if openIdx < 0 {
			parts = appendNonEmptyChatTextPart(parts, "text", remaining)
			return parts
		}
		parts = appendNonEmptyChatTextPart(parts, "text", remaining[:openIdx])
		remaining = remaining[openIdx+openLen:]
		state.thinkTagOpen = true
	}
	return parts
}

// appendNonEmptyChatTextPart 对应 ccLoad 同名函数。
func appendNonEmptyChatTextPart(parts []chatTextDeltaPart, kind, text string) []chatTextDeltaPart {
	if text == "" {
		return parts
	}
	return append(parts, chatTextDeltaPart{kind: kind, text: text})
}

// findThinkOpenTag 对应 ccLoad 同名函数。
func findThinkOpenTag(text string) (idx int, length int) {
	return findFirstTag(text, []string{"<think>", "<thinking>"})
}

// findThinkCloseTag 对应 ccLoad 同名函数。
func findThinkCloseTag(text string) (idx int, length int) {
	return findFirstTag(text, []string{"</think>", "</thinking>"})
}

// findFirstTag 对应 ccLoad 同名函数：返回最靠前的标签位置。
func findFirstTag(text string, tags []string) (idx int, length int) {
	bestIdx := -1
	bestLen := 0
	for _, tag := range tags {
		pos := strings.Index(text, tag)
		if pos < 0 {
			continue
		}
		if bestIdx < 0 || pos < bestIdx {
			bestIdx = pos
			bestLen = len(tag)
		}
	}
	return bestIdx, bestLen
}

// extractSSEThinkingDelta 对应 ccLoad 同名函数：从单事件对象提取
// 思考增量（覆盖 OpenAI reasoning_content/Gemini thought/Anthropic
// thinking_delta/Codex reasoning_summary）。
func extractSSEThinkingDelta(obj map[string]any) string {
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
					return reasoning
				}
			}
		}
	}

	if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			if content, ok := candidate["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]any); ok {
						if thought, _ := part["thought"].(bool); thought {
							if text, ok := part["text"].(string); ok && text != "" {
								return text
							}
						}
					}
				}
			}
		}
	}

	if typ, _ := obj["type"].(string); typ == "content_block_delta" {
		if delta, ok := obj["delta"].(map[string]any); ok {
			if thinking, ok := delta["thinking"].(string); ok && thinking != "" {
				return thinking
			}
		}
	}
	if typ, _ := obj["type"].(string); typ == "response.reasoning_summary_text.delta" {
		if delta, ok := obj["delta"].(string); ok && delta != "" {
			return delta
		}
	}
	return ""
}

// splitThinkTaggedText 对应 ccLoad 同名函数：整段文本以 think 标签
// 开头时拆出思考与正文。
func splitThinkTaggedText(text string) (thinking string, answer string) {
	trimmed := strings.TrimSpace(text)
	for _, tag := range []string{"think", "thinking"} {
		openTag := "<" + tag + ">"
		closeTag := "</" + tag + ">"
		if !strings.HasPrefix(trimmed, openTag) || !strings.Contains(trimmed, closeTag) {
			continue
		}
		end := strings.Index(trimmed, closeTag)
		if end < 0 {
			continue
		}
		thinking = strings.TrimSpace(trimmed[len(openTag):end])
		answer = strings.TrimSpace(trimmed[end+len(closeTag):])
		return thinking, answer
	}
	return "", text
}

// extractSSEDeltaText 对应 ccLoad 同名函数：从单事件对象提取文本
// 增量（覆盖 OpenAI/Gemini/Anthropic/Codex）。
func extractSSEDeltaText(obj map[string]any) string {
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if content, ok := delta["content"].(string); ok && content != "" {
					return content
				}
			}
		}
	}
	if candidates, ok := obj["candidates"].([]any); ok && len(candidates) > 0 {
		if candidate, ok := candidates[0].(map[string]any); ok {
			if content, ok := candidate["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]any); ok {
						if text, ok := part["text"].(string); ok && text != "" {
							return text
						}
					}
				}
			}
		}
	}
	typ, _ := obj["type"].(string)
	switch typ {
	case "content_block_delta":
		if delta, ok := obj["delta"].(map[string]any); ok {
			if tx, ok := delta["text"].(string); ok && tx != "" {
				return tx
			}
		}
	case "response.output_text.delta":
		if delta, ok := obj["delta"].(string); ok && delta != "" {
			return delta
		}
	}
	return ""
}
