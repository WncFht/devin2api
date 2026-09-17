// 本文件定义 OpenAI Responses 请求 JSON 到中间 LLM 模型的转换。
//
// Package responses 定义 OpenAI Responses HTTP 协议与中间 LLM 模型之间的编解码。
package responses

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/WncFht/devin2api/internal/api/common"
	"github.com/WncFht/devin2api/internal/llm"
)

// Request 是 OpenAI Responses 请求中本适配器支持的字段集合。
type Request struct {
	// Model 是请求使用的模型标识。
	Model string `json:"model"`
	// Instructions 是独立于 input 的系统提示词。
	Instructions string `json:"instructions,omitempty"`
	// Input 是字符串或 Responses input item 数组。
	Input json.RawMessage `json:"input"`
	// Tools 是 OpenAI function 工具定义。
	Tools []Tool `json:"tools,omitempty"`
	// Stream 表示是否请求流式响应。
	Stream bool `json:"stream,omitempty"`
	// MaxOutputTokens 是可选的输出 token 上限。
	MaxOutputTokens *int `json:"max_output_tokens,omitempty"`
	// Temperature 是可选的采样温度。
	Temperature *float64 `json:"temperature,omitempty"`
	// PreviousResponseID 是调用方提供的上游响应关联标识。本代理无服务端
	// 响应存储（store=false），HTTP 路径上非空即在 app 层 400 拒绝——
	// 否则增量 input 会被当全量，上下文静默丢失；WS 会话路径在规范化
	// 阶段已剥离该字段做本地合并，不受影响。
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	// TopP 是可选的 nucleus 采样参数。
	TopP *float64 `json:"top_p,omitempty"`
	// User 是可选的调用方用户标识。
	User string `json:"user,omitempty"`
	// PromptCacheKey 是可选的调用方缓存键。
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// ToolChoice 控制工具调用行为："auto"/"none"/"required" 或 function 对象。
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	// ParallelToolCalls 为 false 时禁止并行工具调用。
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
}

// responsesRequestFields 是 DecodeRequest 已消费的顶层字段；其余字段
// （reasoning/store/service_tier/include 等）上游没有对应物，
// 记入 Dropped 透出而不是静默吞掉。previous_response_id 虽被消费
// 用于显式拒绝，标记为已读避免空值也落进 dropped。
var responsesRequestFields = map[string]bool{
	"model": true, "instructions": true, "input": true, "tools": true,
	"stream": true, "max_output_tokens": true, "temperature": true,
	"top_p": true, "user": true, "prompt_cache_key": true,
	"tool_choice": true, "parallel_tool_calls": true,
	"previous_response_id": true,
}

// Tool 是 OpenAI Responses 工具定义；type 支持 function、custom（freeform）、
// namespace（子工具组）与 web_search（服务端托管搜索）。
type Tool struct {
	// Type 是工具类型：function、custom、namespace 或 web_search 系。
	Type string `json:"type"`
	// Name 是工具名称；namespace 声明里它是命名空间名。
	Name string `json:"name"`
	// Namespace 是部分客户端在 namespace 声明上替代 name 的字段。
	Namespace string `json:"namespace,omitempty"`
	// Description 是工具用途说明。
	Description string `json:"description,omitempty"`
	// Parameters 是 function 工具输入 JSON Schema；custom 工具没有该字段。
	Parameters json.RawMessage `json:"parameters"`
	// Format 是 custom 工具的输入语法声明（如 apply_patch 的 lark grammar），
	// 是模型能看到的唯一格式规范，随说明一并注入。
	Format *struct {
		Syntax     string `json:"syntax"`
		Definition string `json:"definition"`
	} `json:"format,omitempty"`
	// Tools 是 type:"namespace" 声明的子工具列表，逐个展平收录。
	Tools []Tool `json:"tools,omitempty"`
	// Strict 是 function 的严格 schema 模式标记；上游同名位实测接受，透传。
	Strict *bool `json:"strict,omitempty"`
}

// customToolInputSchema 把 freeform 工具包装成上游接受的 function 形态：
// 上游 is_custom_tool 声明通道实测确定性 unknown，改为声明单字符串参数的
// function，模型将原文填入 input（实测 apply_patch 补丁按此下发）。
var customToolInputSchema = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)

// webSearchBaitSchema 是托管 web_search 的 function 诱饵 schema——真实
// Cascade CLI 的 web_search 形态（抓包实测 {query*,num_results?,domain?}），
// 上游模型原生认识这套参数；模型发出调用后由代理代调上游搜索 RPC。
var webSearchBaitSchema = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"num_results":{"type":"integer"},"domain":{"type":"string"}},"required":["query"],"additionalProperties":false}`)

// AdaptedRequest 是 OpenAI 请求转换后的中间请求和生成选项。
type AdaptedRequest struct {
	// Context 是供应商无关的完整对话上下文。
	Context llm.RequestMessages
	// Options 是本次生成所需的协议选项。
	Options RequestOptions
}

// RequestOptions 保存不属于对话历史的生成控制参数。
type RequestOptions struct {
	// Stream 表示调用方是否请求流式响应。
	Stream bool
	// PreviousResponseID 是调用方提供的上游响应关联标识。
	PreviousResponseID string
	// ToolNameMap 记录 namespace 展平名到客户端面向名的还原
	//（"collaboration__spawn_agent" → "collaboration.spawn_agent"），
	// 响应编码器据此把 wire 名改回 codex 按带点全名分发的形态。
	ToolNameMap map[string]string
}

// DecodeRequest 将 OpenAI Responses JSON 请求转换为中间请求。
// collectDropped 为 true 时对请求体做二次全量扫描收集顶层未消费字段
// （field:* 标记）；为 false 跳过——Dropped 的唯一读者是 debuglog 请求
// 投影，debug 关时整棵字段树白建。其余 Dropped 写入点都在低频分支，
// 不随该开关门控。
func DecodeRequest(data []byte, collectDropped bool) (AdaptedRequest, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&request); err != nil {
		return AdaptedRequest{}, fmt.Errorf("decode responses request: %w", err)
	}
	if decoder.More() {
		// 顶层 JSON 后还有内容说明 body 不是单个请求对象——多半
		// 是客户端 bug 或代理误拼接，静默忽略会掩盖截断/串包。
		return AdaptedRequest{}, errors.New("responses request has trailing data after JSON body")
	}
	if request.Model == "" {
		return AdaptedRequest{}, errors.New("responses request model is required")
	}

	context := llm.RequestMessages{Model: request.Model, SystemPrompt: request.Instructions}
	if collectDropped {
		context.Dropped = append(context.Dropped, common.UnconsumedFields(data, responsesRequestFields)...)
	}
	if request.MaxOutputTokens != nil && *request.MaxOutputTokens > 0 {
		context.MaxTokens = request.MaxOutputTokens
	}
	context.Temperature = request.Temperature
	context.TopP = request.TopP
	context.SessionKey = common.SessionKey(request.PromptCacheKey, request.User)
	// 工具声明先于 input 解析：namespace 展平的双向映射既要在响应侧还原
	// 带点全名，也要在本函数内回写历史 function_call 名与 tool_choice 指名。
	nameMaps := &toolNameMaps{restore: map[string]string{}, flatten: map[string]string{}}
	appendToolDefinitions(&context, request.Tools, "", "", nameMaps)
	toolChoice, err := parseResponsesToolChoice(request.ToolChoice, &context, nameMaps)
	if err != nil {
		return AdaptedRequest{}, err
	}
	context.ToolChoice = toolChoice
	if request.ParallelToolCalls != nil && !*request.ParallelToolCalls {
		context.DisableParallelToolCalls = true
	}
	if err := appendInputMessages(&context, request.Input, nameMaps); err != nil {
		return AdaptedRequest{}, err
	}
	// 空 input 放行进上游只会换回一条上游语义错误——与 chat/anthropic
	// 两个前端一致，本地 400 让调用方立刻拿到可行动的报错。
	if len(context.Messages) == 0 {
		return AdaptedRequest{}, errors.New("responses request input is required")
	}
	// 相邻 assistant 回合先合并（与 chat/anthropic 两面同走 IR 层共享
	// 实现）：假回合边界会让 wire 抬高提前 EOS 概率。
	context.MergeAdjacentAssistantTurns()
	// 孤儿 tool result 在 IR 校验前统一降级为 USER 文本——校验要求
	// ToolCallID 非空，而孤儿的调用 id 本来就是缺的。
	context.DemoteOrphanToolResults()
	if err := context.Validate(); err != nil {
		return AdaptedRequest{}, &llm.Failure{Code: "invalid_argument", Message: "validate adapted request: " + err.Error(), Cause: err}
	}
	return AdaptedRequest{
		Context: context,
		Options: RequestOptions{
			Stream:             request.Stream,
			PreviousResponseID: request.PreviousResponseID,
			ToolNameMap:        nameMaps.restore,
		},
	}, nil
}

// toolNameMaps 记录 namespace 展平的双向映射：restore 把 wire 展平名
// （{ns}__{sub}）还原为客户端面向的带点全名（{ns}.{sub}）供响应编码；
// flatten 相反，供回放历史调用名与 tool_choice 指名改写。
type toolNameMaps struct {
	restore map[string]string
	flatten map[string]string
}

// add 登记一对展平名/面向名；相同（非命名空间工具）不登记。
func (maps *toolNameMaps) add(flat, dotted string) {
	if flat == dotted {
		return
	}
	maps.restore[flat] = dotted
	maps.flatten[dotted] = flat
}

// wire 把客户端面向名改写为 wire 展平名（无映射时原样返回）。
func (maps *toolNameMaps) wire(name string) string {
	if flat, ok := maps.flatten[name]; ok {
		return flat
	}
	return name
}

// appendToolDefinitions 把 Responses tools 声明投影进中间模型：
// function/custom 原样收录；namespace 的子工具递归展平为 {ns}__{sub}
// （上游只接受扁平工具名，带点全名声明直接被拒），映射进 nameMaps；
// web_search* 是托管语义声明——落成带真实 Cascade schema 的 Server 诱饵，
// 模型发出的调用由代理代调上游 GetWebSearchResults；其余服务端类型
// （file_search/mcp/tool_search/computer_use_* 等）无桥接通道，记 dropped。
func appendToolDefinitions(context *llm.RequestMessages, tools []Tool, flatPrefix, dottedPrefix string, nameMaps *toolNameMaps) {
	// 先整层收集客户端声明名：web_search 诱饵的去重判定要查全表——同名
	// function/custom 可能排在诱饵声明之后，只回扫已收录项会漏判，
	// wire 上两个同名声明会被上游拒绝。
	declared := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if tool.Type == "function" || tool.Type == "custom" {
			declared[flatPrefix+tool.Name] = true
		}
	}
	for _, tool := range tools {
		switch tool.Type {
		case "function":
			schema := tool.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			definition := llm.ToolDefinition{
				Name:        flatPrefix + tool.Name,
				Description: tool.Description,
				InputSchema: schema,
			}
			if tool.Strict != nil {
				definition.Strict = *tool.Strict
			}
			nameMaps.add(definition.Name, dottedPrefix+tool.Name)
			context.Tools = append(context.Tools, definition)
		case "custom":
			description := tool.Description
			if tool.Format != nil && tool.Format.Definition != "" {
				description += "\n\nInput grammar (" + tool.Format.Syntax + "):\n" + tool.Format.Definition
			}
			definition := llm.ToolDefinition{
				Name:        flatPrefix + tool.Name,
				Description: description,
				InputSchema: customToolInputSchema,
				Custom:      true,
			}
			nameMaps.add(definition.Name, dottedPrefix+tool.Name)
			context.Tools = append(context.Tools, definition)
		case "namespace":
			namespace := tool.Name
			if namespace == "" {
				namespace = tool.Namespace
			}
			if namespace == "" || len(tool.Tools) == 0 {
				context.Dropped = append(context.Dropped, "tool:namespace")
				continue
			}
			appendToolDefinitions(context, tool.Tools, flatPrefix+namespace+"__", dottedPrefix+namespace+".", nameMaps)
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
			// 同名工具已在声明表时不叠加：客户端自实现的 web_search
			// function 保持客户端语义，不被劫持为托管执行。占位进
			// declared 防同表多个 web_search 声明各放一份诱饵。
			if declared[flatPrefix+"web_search"] {
				context.Dropped = append(context.Dropped, "tool:"+tool.Type)
				continue
			}
			declared[flatPrefix+"web_search"] = true
			nameMaps.add(flatPrefix+"web_search", dottedPrefix+"web_search")
			context.Tools = append(context.Tools, llm.ToolDefinition{
				Name:        flatPrefix + "web_search",
				Description: "Search the web for up-to-date information; returns a synthesized answer with sources.",
				InputSchema: webSearchBaitSchema,
				Server:      true,
			})
		default:
			context.Dropped = append(context.Dropped, "tool:"+tool.Type)
		}
	}
}

// parseResponsesToolChoice 解析 Responses tool_choice：标准形态委托 common
// 解析，这里补 codex 特有的 {type:custom|namespace,name,namespace} 与
// {type:web_search*} 指名——前者指向展平后的子工具，后者指向桥接的
// web_search 诱饵（未声明诱饵时是不可满足的指名，记 dropped 降级 auto）。
func parseResponsesToolChoice(raw json.RawMessage, context *llm.RequestMessages, nameMaps *toolNameMaps) (*llm.ToolChoice, error) {
	var probe struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Function  struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		name := probe.Name
		if name == "" {
			name = probe.Function.Name
		}
		namespace := probe.Namespace
		if namespace == "" {
			namespace = probe.Function.Namespace
		}
		switch probe.Type {
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
			for _, tool := range context.Tools {
				if tool.Name == "web_search" && tool.Server {
					return &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: "web_search"}, nil
				}
			}
			context.Dropped = append(context.Dropped, "tool_choice:"+probe.Type)
			return nil, nil
		case "custom", "namespace":
			if name != "" {
				return &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: nameMaps.wire(qualifyToolName(namespace, name))}, nil
			}
		case "function":
			if namespace != "" && name != "" {
				return &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: nameMaps.wire(qualifyToolName(namespace, name))}, nil
			}
		}
	}
	choice, err := common.ParseOpenAIToolChoice(raw, &context.Dropped)
	if err != nil {
		return nil, err
	}
	if choice != nil && choice.Mode == llm.ToolChoiceNamed {
		choice.ToolName = nameMaps.wire(choice.ToolName)
	}
	return choice, nil
}

// qualifyToolName 把独立的 {name, namespace} 字段合回 wire 展平名——
// codex 回放命名空间调用时可能携带独立 namespace 字段而非带点全名。
// 已含命名空间前缀或 mcp__ 前缀的名字原样返回（对齐 cliproxyapi 的同名规则）。
func qualifyToolName(namespace, name string) string {
	if namespace == "" || name == "" ||
		strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, namespace) {
		return name
	}
	if strings.HasSuffix(namespace, "__") {
		return namespace + name
	}
	return namespace + "__" + name
}

// wireToolName 把回放的调用名改写为 wire 展平名：独立 namespace 字段先
// 合名（name 已带前缀时 qualifyToolName 原样返回带点全名），再过展平
// 映射把带点全名换回 {ns}__{sub}——与 parseResponsesToolChoice 同型。
// 不中的名字原样（本来就是展平名或非命名空间工具）。
func wireToolName(name, namespace string, nameMaps *toolNameMaps) string {
	return nameMaps.wire(qualifyToolName(namespace, name))
}

// webSearchResultEntry 是回放 web_search_call item 里 results 数组的元素形态。
type webSearchResultEntry struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Summary string `json:"summary"`
}

// renderWebSearchResults 把回放的搜索结果渲染成 TOOL 结果正文，与
// devin 适配器 renderSearchResults 的无摘要分支同形态。
func renderWebSearchResults(query string, results []webSearchResultEntry) string {
	if len(results) == 0 {
		return fmt.Sprintf("The web search for %q returned no results.", query)
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Search results for %q:", query)
	for index, result := range results {
		fmt.Fprintf(&text, "\n%d. %s\n   %s", index+1, result.Title, result.URL)
		if result.Summary != "" {
			fmt.Fprintf(&text, "\n   %s", result.Summary)
		}
	}
	return text.String()
}

// appendInputMessages 处理 input 为字符串/消息数组的两种形态。
// nameMaps 携带 namespace 展平映射：历史里的 function_call 名是客户端
// 面向的 {ns}.{sub}（或 name+namespace 分字段形态），进 wire 前改回
// {ns}__{sub} 与声明名保持一致。
func appendInputMessages(context *llm.RequestMessages, raw json.RawMessage, nameMaps *toolNameMaps) error {
	if common.JSONBlank(raw) {
		return nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		context.Messages = append(context.Messages, llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: text}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("decode responses input: %w", err)
	}
	// reasoning item 在 input 里位于它所属输出项（assistant message /
	// function_call）之前：summary 文本先缓冲，挂到紧随其后的 assistant
	// 产出上。encrypted_content 为 sealed.* 时是我们自己发出的上游签名，
	// 随思考回放在 wire 上交给上游；外来不透明载荷不可解，忽略。
	var pending pendingReasoning
	for index, item := range items {
		if err := appendInputItem(context, item, &pending, nameMaps); err != nil {
			return fmt.Errorf("input[%d]: %w", index, err)
		}
	}
	// 输入尾部孤儿 reasoning：其后没有可挂的 assistant 产出。
	dropPendingReasoning(context, &pending)
	return nil
}

// pendingReasoning 缓冲 reasoning item 的 summary 文本与可回放签名。
type pendingReasoning struct {
	texts         []string
	signature     string
	signatureType string
}

// consumePendingThinking 取出累积的 reasoning summary，作为 ThinkingContent 前置块。
// 只有签名没有可见文本时按 redacted 处理，与上游的 sealed 表示一致。
func consumePendingThinking(pending *pendingReasoning) []llm.Content {
	if len(pending.texts) == 0 && pending.signature == "" {
		return nil
	}
	block := llm.ThinkingContent{
		Thinking:          strings.Join(pending.texts, "\n"),
		ThinkingSignature: pending.signature,
		SignatureType:     pending.signatureType,
		Redacted:          len(pending.texts) == 0,
	}
	*pending = pendingReasoning{}
	return []llm.Content{block}
}

// dropPendingReasoning 丢弃未挂到 assistant 产出的 reasoning 缓冲并留痕——
// 与输入尾部孤儿共用 "reasoning:orphan" 标记；此前中途截断的丢弃完全不可见，
// 解码是过滤层，丢弃必须进 Dropped 才能对账。
func dropPendingReasoning(context *llm.RequestMessages, pending *pendingReasoning) {
	if len(pending.texts) > 0 || pending.signature != "" {
		context.Dropped = append(context.Dropped, "reasoning:orphan")
	}
	*pending = pendingReasoning{}
}

// classifyReasoningSignature 识别回放进 input 的 encrypted_content 属于哪种
// 上游签名体制：sealed.* 与序列化 reasoning item 数组（openai 型）都是我们
// 自己下发过的形态，原样回放；其余外来不透明载荷不可解，丢弃。
func classifyReasoningSignature(encrypted string) (signature, signatureType string, keep bool) {
	if signatureType = common.ClassifySignatureType(encrypted); signatureType != "" {
		return encrypted, signatureType, true
	}
	return "", "", false
}

// appendInputItem 按 item type 分派单条 input 元素（message/reasoning/
// function_call 等），未知类型记入 Dropped 后跳过。
func appendInputItem(context *llm.RequestMessages, raw json.RawMessage, pending *pendingReasoning, nameMaps *toolNameMaps) error {
	var header struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return fmt.Errorf("decode input item: %w", err)
	}
	if header.Type == "" && header.Role != "" {
		header.Type = "message"
	}
	switch header.Type {
	case "reasoning":
		var item struct {
			Summary []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"summary"`
			// 新版 Responses 把推理正文放在 content[].reasoning_text，
			// 只读 summary 会静默丢掉整段思考（CPA#5378 同型）。
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			EncryptedContent string `json:"encrypted_content"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		for _, part := range item.Summary {
			if part.Type == "summary_text" && part.Text != "" {
				pending.texts = append(pending.texts, part.Text)
			}
		}
		for _, part := range item.Content {
			if part.Type == "reasoning_text" && part.Text != "" {
				pending.texts = append(pending.texts, part.Text)
			}
		}
		if signature, signatureType, keep := classifyReasoningSignature(item.EncryptedContent); keep {
			pending.signature = signature
			pending.signatureType = signatureType
		} else if item.EncryptedContent != "" {
			// 外来不透明载荷不可解，记录而不透传。
			context.Dropped = append(context.Dropped, "reasoning:encrypted_content")
		}
		return nil
	case "message":
		return appendMessageItem(context, raw, header.Role, pending)
	case "function_call":
		var item struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		arguments, custom := common.NormalizeToolArguments(json.RawMessage(item.Arguments))
		content := append(consumePendingThinking(pending),
			llm.ToolCall{ID: item.CallID, Name: wireToolName(item.Name, item.Namespace, nameMaps), Arguments: arguments, Custom: custom})
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			StopReason:  llm.StopReasonToolUse,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	case "custom_tool_call":
		// freeform 工具调用的 input 是原文不是 JSON（如 apply_patch 补丁），
		// 走 Custom 通道原样上行到 invalid_json_str。
		var item struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Input     string `json:"input"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		content := append(consumePendingThinking(pending),
			llm.ToolCall{ID: item.CallID, Name: wireToolName(item.Name, item.Namespace, nameMaps), Arguments: json.RawMessage(item.Input), Custom: true})
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			StopReason:  llm.StopReasonToolUse,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	case "web_search_call":
		// 托管搜索的回放形态是「调用+结果折叠进同一 item」，展开回
		// SYSTEM 调用 + TOOL 结果对上行——丢掉会让模型把自己上一轮
		// 带搜索的结论当成无源答案，重复搜索。
		var item struct {
			ID     string `json:"id"`
			Action struct {
				Query string `json:"query"`
			} `json:"action"`
			Results []webSearchResultEntry `json:"results"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		// 我们下发的 item id 是 ws_+调用 id，剥前缀还原；外来形态无
		// ws_ 前缀时原样作调用 id——wire 上只要 call/result 配对一致。
		callID := strings.TrimPrefix(item.ID, "ws_")
		if callID == "" {
			context.Dropped = append(context.Dropped, "item:web_search_call")
			return nil
		}
		arguments, _ := json.Marshal(map[string]string{"query": item.Action.Query})
		content := append(consumePendingThinking(pending),
			llm.ToolCall{ID: callID, Name: "web_search", Arguments: arguments})
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     content,
			StopReason:  llm.StopReasonToolUse,
			TimestampMS: time.Now().UnixMilli(),
		})
		context.Messages = append(context.Messages, llm.ToolResultMessage{
			ToolCallID:  callID,
			Content:     []llm.Content{llm.TextContent{Text: renderWebSearchResults(item.Action.Query, item.Results)}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	case "function_call_output", "custom_tool_call_output":
		// reasoning 与产出之间插入结果项 → reasoning 成孤儿，丢弃缓冲。
		dropPendingReasoning(context, pending)
		// 客户端对调用 ID 字段名有四种植法（call_id 是规范，其余来自
		// Chat 习惯/驼峰序列化/id 即调用 id 的实现），按序兼容取第一个非空。
		var item struct {
			CallID      string          `json:"call_id"`
			ToolCallID  string          `json:"tool_call_id"`
			CallIDCamel string          `json:"callId"`
			ID          string          `json:"id"`
			Output      json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		callID := item.CallID
		if callID == "" {
			callID = item.ToolCallID
		}
		if callID == "" {
			callID = item.CallIDCamel
		}
		if callID == "" {
			callID = item.ID
		}
		content, err := decodeToolOutput(context, item.Output)
		if err != nil {
			return err
		}
		// 调用 id 缺失或对不上前置 function_call 的结果先按原样进 IR；
		// 解码尾的 DemoteOrphanToolResults 统一降级为 USER 文本。
		context.Messages = append(context.Messages, llm.ToolResultMessage{
			ToolCallID:  callID,
			Content:     content,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	default:
		// tool_search_output / mcp_* 等服务端工具产物没有对应中间类型；
		// 静默丢弃会丢上下文，降级为 USER 文本保住内容。
		context.Dropped = append(context.Dropped, "item:"+header.Type)
		context.Messages = append(context.Messages, llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: "[input item type=" + header.Type + "]\n" + string(raw)}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	}
}

// decodeToolOutput 解码 function_call_output/custom_tool_call_output 的
// output：字符串直接成文本；part 数组（可含 input_image——实测上游
// tool_result 图像子通道有效）按消息内容解码；其余 JSON 原样转文本。
// part 解码失败的容忍是刻意的——output 字段本来就允许任意 JSON，降格为
// 字面文本保住内容（消息路径同形态是 400，因为那里 content 语义是确定的），
// 但形似 part 序列却解不动的要留 Dropped 对账。
func decodeToolOutput(context *llm.RequestMessages, raw json.RawMessage) ([]llm.Content, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Content{llm.TextContent{Text: text}}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("function call output is required")
	}
	if trimmed[0] == '[' {
		content, err := common.DecodeContent(raw, &context.Dropped)
		if err == nil && len(content) > 0 {
			return content, nil
		}
		if err != nil && toolOutputLooksLikeParts(raw) {
			context.Dropped = append(context.Dropped, "tool_output:malformed_parts")
		}
	}
	return []llm.Content{llm.TextContent{Text: string(raw)}}, nil
}

// toolOutputLooksLikeParts 判定数组元素带 type 键——即调用方按 content
// part 意图编码（区别于本就任意的 JSON 数组），解码失败值得记 Dropped。
func toolOutputLooksLikeParts(raw json.RawMessage) bool {
	var elements []map[string]json.RawMessage
	if json.Unmarshal(raw, &elements) != nil {
		return false
	}
	for _, element := range elements {
		if _, has := element["type"]; has {
			return true
		}
	}
	return false
}

// appendMessageItem 把一条 message item 按 role 解码进会话；未知 role 记
// Dropped 并降级为 USER 文本保住内容——与本函数未知 item type 同口径。
func appendMessageItem(context *llm.RequestMessages, raw json.RawMessage, role string, pending *pendingReasoning) error {
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		context.Dropped = append(context.Dropped, "message_role:"+role)
		context.Messages = append(context.Messages, llm.UserMessage{
			Content:     []llm.Content{llm.TextContent{Text: "[message role=" + role + "]\n" + string(raw)}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	}
	var item struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	content, err := common.DecodeContent(item.Content, &context.Dropped)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		// content 为空数组或全部 part 被丢弃：消息不静默消失——记
		// Dropped 并继续走各 role 分支。user 落空文本占位保住轮次结构，
		// assistant 保留空消息（wire 端按 DroppedEmptyAssistant 计），
		// system/developer 对 SystemPrompt 无贡献。与 anthropic 面同口径。
		context.Dropped = append(context.Dropped, "empty_message:"+role)
		if role == "user" {
			content = []llm.Content{llm.TextContent{Text: ""}}
		}
	}
	switch role {
	case "user":
		// 非 assistant 产出介入 → 缓冲的 reasoning 成孤儿，丢弃。
		dropPendingReasoning(context, pending)
		context.Messages = append(context.Messages, llm.UserMessage{Content: content, TimestampMS: time.Now().UnixMilli()})
	case "assistant":
		content = append(consumePendingThinking(pending), content...)
		assistant := llm.AssistantMessage{Content: content, TimestampMS: time.Now().UnixMilli()}
		if strings.HasPrefix(item.ID, "msg_") {
			// msg_* 是 OpenAI 侧 message item 的真实标识，上游 output_id
			// 回放用同一个值。
			assistant.OutputID = item.ID
		}
		context.Messages = append(context.Messages, assistant)
	case "system", "developer":
		dropPendingReasoning(context, pending)
		text := common.ContentText(content)
		context.SystemPrompt = common.AppendSystemPrompt(context.SystemPrompt, text)
	}
	return nil
}
