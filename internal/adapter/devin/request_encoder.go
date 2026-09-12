// 本文件把中间 llm.RequestMessages 投影为 Devin Connect 的
// GetChatMessageRequest：metadata/completion 参数、会话轨迹 ID 派生、
// 逐消息内容转换（文本/thinking/工具调用/工具结果/图片）、
// 工具调用-结果配对修复。响应方向的解码见 response_decoder.go。
package devin

import (
	"crypto/sha256"
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
	"github.com/WncFht/devin2api/internal/randid"
	"github.com/WncFht/devin2api/internal/upstream"
)

func buildRequest(request llm.RequestMessages, config Config) (*devinproto.GetChatMessageRequest, error) {
	// 上游轨迹标识按会话复用：同一会话的连续请求共享稳定 trajectory/cascade
	// ID，使命中更稳（实测稳定 ~7/8 vs 全随机波动）；缓存匹配本身是
	// 「账号 + 内容前缀」键控，ID 不参与匹配。
	trajectoryID, cascadeID := deriveSessionIDs(request)
	executionID := randid.UUID()
	metadata := upstream.BuildMetadata(config.Token, clientName, clientVersion, "mac", 366)
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
				MessageId: proto.String(randid.UUID()),
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
				MessageId: proto.String(randid.UUID()),
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
			MessageId: proto.String(randid.UUID()),
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
		MessageId: proto.String(randid.UUID()),
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
