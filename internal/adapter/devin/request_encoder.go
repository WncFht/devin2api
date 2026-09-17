// 本文件把中间 llm.RequestMessages 投影为 Devin Connect 的
// GetChatMessageRequest：metadata/completion 参数、会话轨迹 ID 派生、
// 逐消息内容转换（文本/thinking/工具调用/工具结果/图片）、
// 工具调用-结果配对修复。响应方向的解码见 response_decoder.go。
package devin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/randid"
	"github.com/WncFht/devin2api/internal/upstream"
)

// callBinding 是单次 GetChatMessage 调用的绑定信息：Model 是经别名/
// router 改写后的上游 model uid，Token 是调用时现取的凭据
// （unauthenticated 自愈会换新），ModelAssignmentJWT 是 AssignModel
// 按请求绑定的 router jwt。三者随调用变化，与静态 Config 分开传——
// 重试只需换 binding.Token。
type callBinding struct {
	Token              string
	Model              string
	ModelAssignmentJWT string
}

// buildRequest 把中间请求投影为上游 wire 格式：静态身份取 config
// （Client* 与 ClientIdentity 默认值），每次调用可变的凭据/路由取
// binding。返回 repairs 记录转换中的静默修复计数，随请求日志落盘。
func buildRequest(request llm.RequestMessages, config Config, binding callBinding) (*devinproto.GetChatMessageRequest, llm.RequestRepairs, error) {
	var repairs llm.RequestRepairs
	// 上游轨迹标识按会话复用：同一会话的连续请求共享稳定 trajectory/cascade
	// ID。实测（2026-09-16 保温实验）：同内容换 SessionKey 派生 ID 后
	// cache_read=0，ID 参与缓存键或路由——稳定派生是命中前提。
	trajectoryID, cascadeID := deriveSessionIDs(request)
	executionID := randid.UUID()
	name, version, os := config.ClientIdentity()
	metadata := upstream.BuildMetadata(binding.Token, name, version, os, 366)
	// tool_choice=none 上游是真执行禁用（实测模型自述「工具被禁用」）：
	// 工具声明与描述注入对模型都是不可用的噪音，不进 wire。
	noTools := request.ToolChoice != nil && request.ToolChoice.Mode == llm.ToolChoiceNone
	systemPrompt := request.SystemPrompt
	if !noTools {
		injected, err := withToolDescriptions(systemPrompt, request.Tools)
		if err != nil {
			return nil, repairs, err
		}
		systemPrompt = injected
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
		Prompt:   proto.String(systemPrompt),
		// 上游 prompt 前缀缓存：system prompt 是稳定前缀，标记 EPHEMERAL 断点。
		SystemPromptCacheOptions: ephemeralCacheOptions(),
		ChatModelUid:             proto.String(binding.Model),
		RequestType:              devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration:            completion,
		TrajectoryReference: &devinproto.ExaCortexPb_CortexTrajectoryReference{
			TrajectoryId: proto.String(trajectoryID),
			// step_index 是真实 CLI 发送的会话内单调步数（抓包实测），
			// 缺省是残留的 wire 形态差异；按 trajectory_id 记账。
			StepIndex:      proto.Int32(nextStepIndex(trajectoryID)),
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
			// 指名调用必须命中 tools 表：上游对不存在的目标只回模糊的
			// invalid_argument，本地提前报成可读错误。
			found := false
			for _, tool := range request.Tools {
				if tool.Name == choice.ToolName {
					found = true
					break
				}
			}
			if !found {
				return nil, repairs, &llm.Failure{Code: "invalid_argument", Message: fmt.Sprintf("tool_choice names tool %q which is not in the tools list", choice.ToolName)}
			}
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
		converted, err := convertMessage(message, index > lastAssistantIndex, &repairs)
		if err != nil {
			return nil, repairs, fmt.Errorf("message %d: %w", index, err)
		}
		// 完全空的助手消息会被跳过（上游见空回复退化），计入修复量。
		if _, isAssistant := message.(llm.AssistantMessage); isAssistant && len(converted) == 0 {
			repairs.DroppedEmptyAssistant++
		}
		result.ChatMessagePrompts = append(result.ChatMessagePrompts, converted...)
	}
	// 上游要求 call→result 紧邻配对：assistant 发出的每个 tool call 必须紧跟
	// 它的 TOOL 结果，否则 invalid_argument。客户端历史（OpenAI/Anthropic）是
	// 「全部调用 → 全部结果」的分组结构，这里按 call id 重排成交错配对。
	result.ChatMessagePrompts, repairs.ReorderedPrompts = pairToolCallsWithResults(result.ChatMessagePrompts)
	if !noTools {
		for _, tool := range request.Tools {
			converted, err := convertToolDefinition(tool)
			if err != nil {
				return nil, repairs, err
			}
			result.Tools = append(result.Tools, converted)
		}
	}
	// 最后一条消息标记 EPHEMERAL 断点：缓存到此为止的全部历史前缀，
	// 下一轮新消息追加在断点后即可命中缓存。
	if n := len(result.ChatMessagePrompts); n > 0 {
		result.ChatMessagePrompts[n-1].PromptCacheOptions = ephemeralCacheOptions()
	}
	// router uid 经 AssignModel 解析出的 jwt 绑本次 cascade_id，
	// 与真实 CLI 的 GetChatMessage 形态一致（见 resolveModelRouting）。
	if binding.ModelAssignmentJWT != "" {
		result.ModelAssignmentJwt = proto.String(binding.ModelAssignmentJWT)
	}
	return result, repairs, nil
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
// 连续性。无 SessionKey 时退回「系统提示头 4KB + 首条消息文本头 1KB +
// 客户端模型名 + 工具声明哈希」内容哈希：同一会话多轮回放前缀不变 →
// 稳定；tools/model 进种子后，共享 system prompt 的客户端（CC）里
// 首消息雷同的不同会话不再必然折叠进同一上游轨迹——会话内 tools/model
// 漂移会换轨迹，但注入段变化本来就让前缀缓存分叉，代价一致。
// 残余盲区：首消息+system+tools+model 全同且无 SessionKey 的请求仍同
// seed——它与「同一会话的重发」在请求边界上不可区分，要隔离须由
// 客户端提供会话标识。
func deriveSessionIDs(request llm.RequestMessages) (trajectoryID string, cascadeID string) {
	sum := sha256.Sum256(sessionSeed(request))
	return uuidFromBytes(sum[:16]), uuidFromBytes(sum[16:32])
}

// SessionAffinityKey 返回会话的账号钉选键：与 deriveSessionIDs 同种子，
// 钉选稳定则 trajectory/cascade 派生、warm 谱系、assignments 三个命名
// 空间随会话落在同一条 lane 上。
func SessionAffinityKey(request llm.RequestMessages) string {
	sum := sha256.Sum256(sessionSeed(request))
	return hex.EncodeToString(sum[:16])
}

// sessionSeed 构造会话稳定种子：SessionKey 优先（CC metadata.user_id、
// Codex prompt_cache_key），空则退回「system 头 4KB + 首条消息文本头
// 1KB + 客户端模型名 + 工具声明哈希」内容哈希——同会话多轮回放前缀
// 不变故稳定，不同会话形态自然分散。残余盲区见 deriveSessionIDs。
func sessionSeed(request llm.RequestMessages) []byte {
	// bytes.Buffer 的 Bytes() 零拷贝交给 Sum256；strings.Builder 则需
	// 先 String() 再 []byte() 多一份全量拷贝。
	var seed bytes.Buffer
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
		// 会话形态维：request.Model 是客户端指名（别名解析前），
		// hashTools 与 warm lineage 键同口径——零字段请求与「同会话
		// 重发」不可区分是固有边界，不在这里发明会话身份。
		seed.WriteByte(0)
		seed.WriteString(request.Model)
		seed.WriteByte(0)
		seed.WriteString(hashTools(request.Tools))
	}
	return seed.Bytes()
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
	return randid.FormatUUID(out)
}

// stepIndexRegistry 按 trajectory_id 记录已发送的上游步数：真实 CLI 每请求
// 发送会话内单调递增的 step_index（抓包实测），不发是残留的 wire 形态
// 差异。计数随进程重启归零，与 CLI 重启行为一致；容量封顶防止会话数
// 长期累积成无界 map，触顶整体清空——轨迹记账字段重置无害。
var stepIndexRegistry = struct {
	sync.Mutex
	counts map[string]int32
}{counts: make(map[string]int32)}

// nextStepIndex 返回该 trajectory 的下一个会话内单调步数；表项无界
// 增长时整体清空重计（65536 条约对应数万并发会话，清空只是步数
// 从 1 重排，上游不校验跨请求连续性）。
func nextStepIndex(trajectoryID string) int32 {
	stepIndexRegistry.Lock()
	defer stepIndexRegistry.Unlock()
	if len(stepIndexRegistry.counts) >= 65536 {
		stepIndexRegistry.counts = make(map[string]int32)
	}
	stepIndexRegistry.counts[trajectoryID]++
	return stepIndexRegistry.counts[trajectoryID]
}

// convertMessage 将中间消息转为 Devin ChatMessagePrompt。
// attachImages 为 true 时才把 ImageContent 写入 Images（仅最新用户轮）；历史图改成文本占位。
// repairs 累计转换中发生的静默修复（历史图剥离等）。
func convertMessage(message llm.Message, attachImages bool, repairs *llm.RequestRepairs) ([]*devinproto.ExaChatPb_ChatMessagePrompt, error) {
	switch message := message.(type) {
	case llm.UserMessage:
		return []*devinproto.ExaChatPb_ChatMessagePrompt{promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER, message.Content, attachImages, repairs)}, nil
	case llm.AssistantMessage:
		// Wire 实证（chisel 3000.2.17 抓包）：一个助手回合合并为单条
		// prompt——prompt/thinking/signature/toolCalls 同体携带，无文本时
		// prompt 字段缺席；真实客户端从不产生相邻 SYSTEM 消息。拆成多条会
		// 在渲染上下文里引入回合边界，模型在「宣告文本」后采到 EOS 提前收轮。
		var signature, signatureType string
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
					signatureType = typed.SignatureType
				}
				redacted = redacted || typed.Redacted
			case llm.ToolCall:
				calls = append(calls, typed)
			}
		}
		// 完全空的助手消息会诱发上游反复返回空回复，跳过。判空要覆盖
		// 全部可回放产物：thinking/签名/redacted/output_id/调用任一
		// 存在都不算空——openai 体制下「只有签名的 thinking 块」是
		// 合法载荷，decodeLateSignature 合成的正是这一形态。
		if text.Len() == 0 && thinking.Len() == 0 && len(calls) == 0 &&
			signature == "" && !redacted && message.OutputID == "" {
			return nil, nil
		}
		prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
			MessageId: proto.String(randid.UUID()),
			Source:    assistantSource.Enum(),
		}
		if text.Len() > 0 {
			prompt.Prompt = proto.String(text.String())
		}
		// signature_type 与 output_id 必须随签名原样回传：实测错配
		// signature_type 会触发上游 invalid_argument。
		if thinking.Len() > 0 || redacted || signature != "" || message.OutputID != "" {
			if thinking.Len() > 0 {
				prompt.Thinking = proto.String(thinking.String())
			}
			if signature != "" {
				prompt.Signature = proto.String(signature)
			}
			if signatureType != "" {
				prompt.SignatureType = proto.String(signatureType)
			}
			if message.OutputID != "" {
				prompt.OutputId = proto.String(message.OutputID)
			}
			prompt.ThinkingRedacted = proto.Bool(redacted)
		}
		for _, call := range calls {
			toolCall := &devinproto.ExaCodeiumCommonPb_ChatToolCall{
				Id: proto.String(call.ID),
				// 历史调用名原样上行：上游的字符集门槛只查 tools 声明，
				// 历史名带 . / : / CJK 实测照收（probe edge
				// history-tool-name）；llm.ToolCall.Validate 不查字符集，
				// 此通道收到的名字本来就可含声明层会拒的字符。
				Name: proto.String(call.Name),
			}
			if call.Custom {
				// custom/freeform 调用的参数体不是 JSON，走 invalid_json_str
				// 通道原样回传（上游对该字段实测容忍非 JSON 原文）。
				toolCall.IsCustomToolCall = proto.Bool(true)
				toolCall.InvalidJsonStr = proto.String(string(call.Arguments))
			} else {
				toolCall.ArgumentsJson = proto.String(string(call.Arguments))
			}
			prompt.ToolCalls = append(prompt.ToolCalls, toolCall)
		}
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	case llm.ToolResultMessage:
		prompt := promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL, message.Content, attachImages, repairs)
		if prompt.GetPrompt() == "" {
			// 上游不接受空的工具结果文本，对齐 WindsurfAPI 的占位。
			prompt.Prompt = proto.String("[tool result]")
		}
		prompt.ToolCallId = proto.String(message.ToolCallID)
		prompt.ToolResultIsError = proto.Bool(message.IsError)
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	default:
		return nil, &llm.Failure{Code: "invalid_argument", Message: fmt.Sprintf("unsupported message type %T", message)}
	}
}

// assistantSource 是助手消息在 Devin wire 上的来源枚举（上游命名为 SYSTEM，值 2）。
var assistantSource = devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM

// pairToolCallsWithResults 把「连续调用消息 + 连续结果消息」的分组序列
// 重排为 call_i, result_i, call_j, result_j 的交错序列。
// 已配对的交错序列保持不变；找不到匹配结果的调用原样保留位置。
// 第二个返回值是位置发生变化的 prompt 数（已交错的历史为 0）。
func pairToolCallsWithResults(prompts []*devinproto.ExaChatPb_ChatMessagePrompt) ([]*devinproto.ExaChatPb_ChatMessagePrompt, int) {
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
		byID := make(map[string][]*devinproto.ExaChatPb_ChatMessagePrompt)
		j := i
		for j < len(prompts) && isResultPrompt(prompts[j]) {
			id := prompts[j].GetToolCallId()
			byID[id] = append(byID[id], prompts[j])
			j++
		}
		// consumed 记已配对的 result 指针而非 id：同 id 多份结果按到达
		// 顺序消费（重复 call-id 实测被上游容忍但按位置绑定），单值
		// byID 会让先到的结果被后到的覆盖丢失。
		consumed := make(map[*devinproto.ExaChatPb_ChatMessagePrompt]struct{}, len(calls))
		for _, callPrompt := range calls {
			out = append(out, callPrompt)
			for _, call := range callPrompt.GetToolCalls() {
				id := call.GetId()
				queue := byID[id]
				if len(queue) == 0 {
					continue
				}
				out = append(out, queue[0])
				consumed[queue[0]] = struct{}{}
				byID[id] = queue[1:]
			}
		}
		// 未能配对的孤立结果按原序保留，不丢消息。
		for k := i; k < j; k++ {
			if _, ok := consumed[prompts[k]]; !ok {
				out = append(out, prompts[k])
			}
		}
		i = j
	}
	return out, countMovedPrompts(prompts, out)
}

// countMovedPrompts 统计重排后位置发生变化的 prompt 数，作为配对修复量。
// prompt 指针在流程中唯一（每条消息独立构造），可安全作 map 键。
func countMovedPrompts(in, out []*devinproto.ExaChatPb_ChatMessagePrompt) int {
	original := make(map[*devinproto.ExaChatPb_ChatMessagePrompt]int, len(in))
	for index, prompt := range in {
		original[prompt] = index
	}
	moved := 0
	for position, prompt := range out {
		if original[prompt] != position {
			moved++
		}
	}
	return moved
}

// promptForContent 把 UserMessage/ToolResultMessage 的内容块投影为单条
// prompt。两类消息的 Validate 已限定 content 只含 text/image，
// thinking/工具调用不会到达这里——助手侧产物走 convertMessage 的
// AssistantMessage 分支单独组装。
func promptForContent(source devinproto.ExaCodeiumCommonPb_ChatMessageSource, content []llm.Content, attachImages bool, repairs *llm.RequestRepairs) *devinproto.ExaChatPb_ChatMessagePrompt {
	prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    source.Enum(),
	}
	// 单文本块是绝大多数消息的形态：直挂原文串省去 Builder 整段拷贝
	// （历史重发时这条路径拷贝全部上下文，是投影分配大头）。
	if len(content) == 1 {
		if single, ok := content[0].(llm.TextContent); ok {
			prompt.Prompt = proto.String(single.Text)
			return prompt
		}
	}
	var text strings.Builder
	for _, block := range content {
		switch block := block.(type) {
		case llm.TextContent:
			text.WriteString(block.Text)
		case llm.ImageContent:
			if !attachImages {
				// 与 WindsurfAPI 一致：历史图不进 Images，避免上游 invalid_argument。
				if text.Len() > 0 {
					text.WriteByte('\n')
				}
				text.WriteString("[Image omitted from history]")
				repairs.OmittedHistoryImages++
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
