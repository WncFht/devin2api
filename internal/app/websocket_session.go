// 本文件实现 OpenAI Responses WebSocket 连接的会话状态机。
//
// Codex WS v2 的多轮语义：同一连接上反复发送 {"type":"response.create"|"response.append"}，
// 续轮用 previous_response_id + 增量 input（只含本轮新项）。我们的上游没有
// previous_response_id 语义，所以每个后续请求都被展开成完整 transcript 再交给
// 常规 /v1/responses 流水线：
//
//	merged_input = lastRequest.input + lastResponseOutput + 新 input
//
// 合并时按 item id / tool call call_id 去重（参照 CLIProxyAPI 的两轮 dedupe：
// call_id 保首个，id 默认保最后但被 output 引用的 call 项不被顶掉），并做
// function_call ↔ function_call_output 配对校验——上游对孤儿 output 会硬报
// invalid_argument，提前拦截避免把客户端的坏请求算成上游故障。
package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// wsMaxTranscriptBytes 与 HTTP body 的 32MiB 上限对齐：合并后的 transcript
// 逐轮增长，必须有一道闸门防止单连接无限膨胀内存与上游负载。
const wsMaxTranscriptBytes = 32 << 20

var (
	errWSPreviousResponseNotFound = errors.New("previous response is not available on this websocket; resend the full conversation input without previous_response_id")
	errWSUnsupportedRequestType   = errors.New("unsupported websocket request type")
)

// wsSession 保存一条 WebSocket 连接的对话回放状态。连接级生命周期：
// 断开后客户端按协议重放完整 transcript，不需要跨连接存储。
type wsSession struct {
	lastRequest             json.RawMessage
	lastResponseOutput      json.RawMessage
	lastResponseID          string
	pendingToolCallIDs      []string
	replacementReplayNeeded bool
}

func newWSSession() *wsSession {
	return &wsSession{lastResponseOutput: json.RawMessage("[]")}
}

// wsTurnResult 是单轮结束后的提交内容；由 wsResponseWriter 在流结束时产出。
type wsTurnResult struct {
	completedOutput     json.RawMessage
	completedResponseID string
	pendingToolCallIDs  []string
}

// commit 把上一轮结果记入会话；只有成功终结的轮次才推进状态。
func (s *wsSession) commit(request json.RawMessage, result wsTurnResult) {
	s.lastRequest = bytes.Clone(request)
	if len(result.completedOutput) == 0 {
		s.lastResponseOutput = json.RawMessage("[]")
	} else {
		s.lastResponseOutput = bytes.Clone(result.completedOutput)
	}
	s.lastResponseID = strings.TrimSpace(result.completedResponseID)
	s.pendingToolCallIDs = append([]string(nil), result.pendingToolCallIDs...)
}

// requireReplacementReplay 标记下一次无 previous_response_id 的 create 为全量
// 替换（上游中断后客户端重放完整 transcript 的情形）。
func (s *wsSession) requireReplacementReplay() {
	s.replacementReplayNeeded = true
}

// normalizeRequest 把一条客户端 WS 帧规范化成可交给 /v1/responses 的完整请求体。
// 守卫顺序与 ccLoad responses_websocket_session.go 对齐：先结构性校验，再处理
// 替换/续链/合并三种形态。
func (s *wsSession) normalizeRequest(payload []byte) (json.RawMessage, error) {
	if !json.Valid(payload) {
		return nil, errors.New("invalid websocket request JSON")
	}
	requestType := wsJSONString(payload, "type")
	if requestType != "response.create" && requestType != "response.append" {
		return nil, fmt.Errorf("%w: %q", errWSUnsupportedRequestType, requestType)
	}
	previousID := strings.TrimSpace(wsJSONString(payload, "previous_response_id"))
	// previous_response_id 只认 resp_* 形态——msg_*/item_*/chatcmpl_* 是
	// 别的协议的标识，续链必然找不到，按坏请求报。
	if previousID != "" && !strings.HasPrefix(previousID, "resp_") {
		return nil, fmt.Errorf("previous_response_id %q is not a response id", previousID)
	}

	if len(s.lastRequest) == 0 {
		if requestType == "response.append" {
			return nil, errors.New("response.append received before response.create")
		}
		if previousID != "" {
			return nil, errWSPreviousResponseNotFound
		}
		return s.normalizeInitialRequest(payload)
	}

	// 续轮的 input 必须是数组（增量项列表）；缺省按空增量处理（客户端可能
	// 只带 previous_response_id 触发续轮），字符串形态只在首轮合法。
	nextInput, hasInput, inputIsArray := wsJSONField(payload, "input")
	if hasInput && !inputIsArray {
		return nil, errors.New("websocket request requires array field: input")
	}
	if !hasInput {
		nextInput = json.RawMessage("[]")
	}
	if previousID != "" && previousID != s.lastResponseID {
		return nil, fmt.Errorf("%w: %q", errWSPreviousResponseNotFound, previousID)
	}
	if s.replacementReplayNeeded && requestType == "response.create" && previousID == "" {
		s.replacementReplayNeeded = false
		return s.finalizeReplacement(payload)
	}
	if len(s.pendingToolCallIDs) > 0 && !wsInputSatisfiesToolCalls(nextInput, s.pendingToolCallIDs) {
		if previousID != "" || requestType == "response.append" {
			return nil, errors.New("incremental websocket request is missing output for a pending tool call")
		}
		return s.finalizeReplacement(payload)
	}
	if previousID == "" && wsInputContainsCompletedTranscript(nextInput) {
		// 客户端自带的 input 已是完整回放（含历史 model 产出），与它合并只会
		// 得到重复/乱序的 transcript——直接当替换处理。
		return s.finalizeReplacement(payload)
	}

	merged, err := mergeWSInput(s.lastRequest, s.lastResponseOutput, nextInput)
	if err != nil {
		return nil, err
	}
	normalized, err := s.normalizeReplacementRequest(payload)
	if err != nil {
		return nil, err
	}
	normalized, err = wsJSONSetRaw(normalized, "input", merged)
	if err != nil {
		return nil, fmt.Errorf("set merged websocket input: %w", err)
	}
	return wsFinalizeRequest(normalized)
}

// finalizeReplacement 走替换语义规范化后统一过 finalize（配对校验 + 字节上限）。
// 增量合并路径不走这里——它先把客户端增量合并进历史再 finalize 一次，
// 提前对未合并的增量做配对校验会误报孤儿 output。
func (s *wsSession) finalizeReplacement(payload []byte) (json.RawMessage, error) {
	normalized, err := s.normalizeReplacementRequest(payload)
	if err != nil {
		return nil, err
	}
	return wsFinalizeRequest(normalized)
}

// normalizeInitialRequest 处理首轮 response.create：剥离 WS 信封字段，
// 强制 stream=true，input 缺省补空数组，model 必填。
func (s *wsSession) normalizeInitialRequest(payload []byte) (json.RawMessage, error) {
	if strings.TrimSpace(wsJSONString(payload, "model")) == "" {
		return nil, errors.New("missing model in response.create request")
	}
	normalized, err := wsJSONDelete(payload, "type", "previous_response_id", "generate")
	if err != nil {
		return nil, err
	}
	if _, hasInput, _ := wsJSONField(normalized, "input"); !hasInput {
		normalized, err = wsJSONSetRaw(normalized, "input", json.RawMessage("[]"))
		if err != nil {
			return nil, err
		}
	}
	normalized, err = wsJSONSetBool(normalized, "stream", true)
	if err != nil {
		return nil, err
	}
	return wsFinalizeRequest(normalized)
}

// normalizeReplacementRequest 处理「这轮 input 自带完整 transcript」的形态：
// 不合并历史，但继承上一轮的 model/instructions（增量帧常省略这两个字段）。
func (s *wsSession) normalizeReplacementRequest(payload []byte) (json.RawMessage, error) {
	normalized, err := wsJSONDelete(payload, "type", "previous_response_id", "generate")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(wsJSONString(normalized, "model")) == "" {
		if model := strings.TrimSpace(wsJSONString(s.lastRequest, "model")); model != "" {
			normalized, err = wsJSONSetString(normalized, "model", model)
			if err != nil {
				return nil, err
			}
		}
	}
	if _, hasInstructions, _ := wsJSONField(normalized, "instructions"); !hasInstructions {
		if instructions, has, _ := wsJSONField(s.lastRequest, "instructions"); has {
			normalized, err = wsJSONSetRaw(normalized, "instructions", instructions)
			if err != nil {
				return nil, err
			}
		}
	}
	normalized, err = wsJSONSetBool(normalized, "stream", true)
	if err != nil {
		return nil, err
	}
	return normalized, nil
}

// wsGenerateDisabled 判定 Codex 的预热帧：{"type":"response.create","generate":false,...}
// 预热不打上游，本地合成 created+completed，但 input 要计入 transcript（客户端
// 下一轮会 previous_response_id 指向它）。
func wsGenerateDisabled(payload []byte) bool {
	raw, has, _ := wsJSONField(payload, "generate")
	if !has {
		return false
	}
	var enabled bool
	if json.Unmarshal(raw, &enabled) != nil {
		return false
	}
	return !enabled
}

// mergeWSInput 拼接 lastRequest.input + lastResponseOutput + 增量 input。
// parts 里非数组的部分跳过（lastResponseOutput 可能是 null/对象等非法形态）。
func mergeWSInput(lastRequest, lastResponseOutput, appendInput json.RawMessage) (json.RawMessage, error) {
	var items []json.RawMessage
	seenCallIDs := make(map[string]struct{})
	appendParts := func(part json.RawMessage) error {
		var array []json.RawMessage
		if err := json.Unmarshal(part, &array); err != nil {
			return fmt.Errorf("websocket transcript input must be an array: %w", err)
		}
		for _, raw := range array {
			raw = bytes.TrimSpace(raw)
			fields := wsParseItem(raw)
			if fields == nil && !json.Valid(raw) {
				return errors.New("websocket transcript contains invalid item JSON")
			}
			// 第一轮 dedupe：tool call 项按 call_id 保首个——重复 call 只留第一次出现。
			if wsFieldsAreToolCall(fields) {
				if callID := strings.TrimSpace(wsRawString(fields["call_id"])); callID != "" {
					if _, dup := seenCallIDs[callID]; dup {
						continue
					}
					seenCallIDs[callID] = struct{}{}
				}
			}
			items = append(items, raw)
		}
		return nil
	}
	if input, has, isArray := wsJSONField(lastRequest, "input"); has && isArray {
		if err := appendParts(input); err != nil {
			return nil, fmt.Errorf("invalid previous request input: %w", err)
		}
	}
	if trimmed := bytes.TrimSpace(lastResponseOutput); len(trimmed) > 2 {
		if err := appendParts(trimmed); err != nil {
			return nil, fmt.Errorf("invalid previous response output: %w", err)
		}
	}
	if err := appendParts(appendInput); err != nil {
		return nil, fmt.Errorf("invalid request input: %w", err)
	}
	items = dedupeWSInputItems(items)
	return json.Marshal(items)
}

// dedupeWSInputItems 第二轮去重：按 item id 默认保留最后出现（客户端可能
// 在增量里修正已发项），但被某个 output 项 call_id 引用的 call 项不会被
// 未引用项顶掉（保 function_call ↔ output 的配对相邻性）。
// 每条 item 只解析一次成字段树，后续三轮扫描全部读树。
func dedupeWSInputItems(items []json.RawMessage) []json.RawMessage {
	fieldsList := make([]map[string]json.RawMessage, len(items))
	referencedCallIDs := make(map[string]struct{})
	for i, raw := range items {
		fieldsList[i] = wsParseItem(raw)
		if wsFieldsAreToolCallOutput(fieldsList[i]) {
			if callID := strings.TrimSpace(wsRawString(fieldsList[i]["call_id"])); callID != "" {
				referencedCallIDs[callID] = struct{}{}
			}
		}
	}
	// 反向扫描决定每个 id 保留哪个下标：默认最后一个；更早但 call_id 被引用的
	// 项优先级高于未被引用的重复项。
	keepAt := make(map[string]int)
	callIDReferenced := func(i int) bool {
		_, ok := referencedCallIDs[strings.TrimSpace(wsRawString(fieldsList[i]["call_id"]))]
		return ok
	}
	for i := len(items) - 1; i >= 0; i-- {
		id := strings.TrimSpace(wsRawString(fieldsList[i]["id"]))
		if id == "" {
			continue
		}
		if existing, seen := keepAt[id]; seen {
			// existing 是更靠后的重复项。若 existing 未被引用而当前项被引用，
			// 改保留当前项（更早但配对着）。
			if !callIDReferenced(existing) && callIDReferenced(i) {
				keepAt[id] = i
			}
			continue
		}
		keepAt[id] = i
	}
	out := items[:0]
	for i, raw := range items {
		id := strings.TrimSpace(wsRawString(fieldsList[i]["id"]))
		if id != "" && keepAt[id] != i {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// wsFinalizeRequest 是每条规范化请求的统一出口：配对校验 + 字节上限。
func wsFinalizeRequest(payload json.RawMessage) (json.RawMessage, error) {
	if err := wsValidateToolCallPairing(payload); err != nil {
		return nil, err
	}
	if len(payload) > wsMaxTranscriptBytes {
		return nil, fmt.Errorf("websocket transcript exceeds %d byte limit; compact and replay the conversation", wsMaxTranscriptBytes)
	}
	return payload, nil
}

// wsValidateToolCallPairing 拒绝「有 output 无 call」的 transcript——上游对
// 这种形态硬报 invalid_argument，提前拦截以免把客户端坏请求算成上游故障。
// call 只需出现在数组任意位置（不要求在 output 之前）。
func wsValidateToolCallPairing(payload json.RawMessage) error {
	input, has, isArray := wsJSONField(payload, "input")
	if !has || !isArray {
		return nil
	}
	var array []json.RawMessage
	if err := json.Unmarshal(input, &array); err != nil {
		return nil
	}
	calls := make(map[string]struct{})
	var outputs []string
	for _, raw := range array {
		fields := wsParseItem(raw)
		switch {
		case wsFieldsAreToolCall(fields):
			if callID := strings.TrimSpace(wsRawString(fields["call_id"])); callID != "" {
				calls[callID] = struct{}{}
			}
		case wsFieldsAreToolCallOutput(fields):
			if callID := strings.TrimSpace(wsRawString(fields["call_id"])); callID != "" {
				outputs = append(outputs, callID)
			}
		}
	}
	for _, callID := range outputs {
		if _, ok := calls[callID]; !ok {
			return fmt.Errorf("websocket transcript has tool call output for unknown call_id %q", callID)
		}
	}
	return nil
}

// wsInputSatisfiesToolCalls 检查增量 input 是否包含所有 pending call 的 output。
func wsInputSatisfiesToolCalls(input json.RawMessage, pending []string) bool {
	outputs := make(map[string]struct{}, len(pending))
	var array []json.RawMessage
	if json.Unmarshal(input, &array) != nil {
		return false
	}
	for _, raw := range array {
		fields := wsParseItem(raw)
		if wsFieldsAreToolCallOutput(fields) {
			if callID := strings.TrimSpace(wsRawString(fields["call_id"])); callID != "" {
				outputs[callID] = struct{}{}
			}
		}
	}
	for _, callID := range pending {
		if _, ok := outputs[callID]; !ok {
			return false
		}
	}
	return true
}

// wsInputContainsCompletedTranscript 判定增量 input 实际已含完整回放历史。
// 出现以下任一项即为完整形态：compaction 标记、model 产出项（function_call、
// assistant message）、或 Codex 本地压缩摘要前缀的 user 消息。
func wsInputContainsCompletedTranscript(input json.RawMessage) bool {
	var array []json.RawMessage
	if json.Unmarshal(input, &array) != nil {
		return false
	}
	for _, raw := range array {
		fields := wsParseItem(raw)
		switch strings.TrimSpace(wsRawString(fields["type"])) {
		case "compaction", "compaction_summary", "function_call", "custom_tool_call":
			return true
		}
		role := strings.TrimSpace(wsRawString(fields["role"]))
		if role == "assistant" {
			return true
		}
		if role == "user" && strings.HasPrefix(wsMessageText(fields), wsCodexCompactionSummaryPrefix+"\n") {
			return true
		}
	}
	return false
}

// wsCodexCompactionSummaryPrefix 是 Codex 本地压缩产出的摘要前缀；命中它说明
// input 是压缩后的完整回放，不该再和历史合并。
const wsCodexCompactionSummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:"

// wsMessageText 提取 message 项的纯文本（content 为字符串或 input_text/text 块数组）。
// fields 是调用方已解析的 item 字段树。
func wsMessageText(fields map[string]json.RawMessage) string {
	content, has := fields["content"]
	if !has {
		return ""
	}
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var parts []json.RawMessage
	if json.Unmarshal(content, &parts) != nil {
		return ""
	}
	var builder strings.Builder
	for _, part := range parts {
		partType := wsJSONString(part, "type")
		if partType == "input_text" || partType == "text" {
			builder.WriteString(wsJSONString(part, "text"))
		}
	}
	return builder.String()
}

// wsPendingToolCallIDs 从 output 数组里收集「完整」tool call 的 call_id：
// call_id/name 非空且 arguments（function_call）或 input（custom_tool_call）
// 为字符串。这些 call 等待客户端回送 output，是下一轮 input 的配对义务。
func wsPendingToolCallIDs(output json.RawMessage) []string {
	var array []json.RawMessage
	if json.Unmarshal(output, &array) != nil {
		return nil
	}
	seen := make(map[string]struct{})
	var callIDs []string
	for _, raw := range array {
		fields := wsParseItem(raw)
		if !wsFieldsAreCompleteToolCall(fields) {
			continue
		}
		callID := strings.TrimSpace(wsRawString(fields["call_id"]))
		if callID == "" {
			continue
		}
		if _, dup := seen[callID]; dup {
			continue
		}
		seen[callID] = struct{}{}
		callIDs = append(callIDs, callID)
	}
	return callIDs
}

// wsParseItem 把一条 transcript item 解成顶层字段树；非 object JSON
// （数组/标量/非法）返回 nil，调用方按「无字段」处理。
func wsParseItem(raw json.RawMessage) map[string]json.RawMessage {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	return fields
}

func wsFieldsAreToolCall(fields map[string]json.RawMessage) bool {
	t := strings.TrimSpace(wsRawString(fields["type"]))
	return t == "function_call" || t == "custom_tool_call"
}

func wsFieldsAreToolCallOutput(fields map[string]json.RawMessage) bool {
	t := strings.TrimSpace(wsRawString(fields["type"]))
	return t == "function_call_output" || t == "custom_tool_call_output"
}

func wsFieldsAreCompleteToolCall(fields map[string]json.RawMessage) bool {
	if !wsFieldsAreToolCall(fields) {
		return false
	}
	if strings.TrimSpace(wsRawString(fields["call_id"])) == "" || strings.TrimSpace(wsRawString(fields["name"])) == "" {
		return false
	}
	field := "arguments"
	if strings.TrimSpace(wsRawString(fields["type"])) == "custom_tool_call" {
		field = "input"
	}
	var s string
	return json.Unmarshal(fields[field], &s) == nil
}

// --- 以下为最小 JSON 手术工具：项目此前不依赖 gjson/sjson，这几个 helper
// 用 encoding/json 覆盖本文件需要的全部读取/改写。 ---

// wsJSONField 返回指定字段的原始 JSON；isArray 单独标出便于数组形态校验。
func wsJSONField(payload json.RawMessage, key string) (value json.RawMessage, has bool, isArray bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(payload, &object) != nil {
		return nil, false, false
	}
	raw, ok := object[key]
	if !ok {
		return nil, false, false
	}
	trimmed := bytes.TrimSpace(raw)
	return raw, true, len(trimmed) > 0 && trimmed[0] == '['
}

func wsJSONString(payload json.RawMessage, key string) string {
	raw, has, _ := wsJSONField(payload, key)
	if !has {
		return ""
	}
	return wsRawString(raw)
}

// wsRawString 把单个 RawMessage 解为字符串；非字符串或空值返回 ""。
func wsRawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// wsJSONDelete 删除若干顶层字段；字段不存在时跳过。
func wsJSONDelete(payload json.RawMessage, keys ...string) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, fmt.Errorf("decode websocket request: %w", err)
	}
	for _, key := range keys {
		delete(object, key)
	}
	return json.Marshal(object)
}

func wsJSONSetRaw(payload json.RawMessage, key string, value json.RawMessage) (json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil {
		return nil, fmt.Errorf("decode websocket request: %w", err)
	}
	object[key] = value
	return json.Marshal(object)
}

func wsJSONSetString(payload json.RawMessage, key string, value string) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return wsJSONSetRaw(payload, key, raw)
}

func wsJSONSetBool(payload json.RawMessage, key string, value bool) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return wsJSONSetRaw(payload, key, raw)
}
