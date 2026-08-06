// 本文件定义 OpenAI Responses 请求 JSON 到中间 LLM 模型的转换。
//
// Package responses 定义 OpenAI Responses HTTP 协议与中间 LLM 模型之间的编解码。
package responses

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"

	"github.com/leookun/devin-2api/internal/llm"
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
	// PreviousResponseID 是上游 Responses 会话关联标识。
	PreviousResponseID string `json:"previous_response_id,omitempty"`
}

// Tool 是 OpenAI Responses function 工具定义。
type Tool struct {
	// Type 固定为 function。
	Type string `json:"type"`
	// Name 是工具名称。
	Name string `json:"name"`
	// Description 是工具用途说明。
	Description string `json:"description,omitempty"`
	// Parameters 是工具输入 JSON Schema。
	Parameters json.RawMessage `json:"parameters"`
}

// AdaptedRequest 是 OpenAI 请求转换后的中间请求和生成选项。
type AdaptedRequest struct {
	// Context 是供应商无关的完整对话上下文。
	Context llm.RequestMessages
	// Options 是本次生成所需的协议选项。
	Options RequestOptions
}

// RequestOptions 保存不属于对话历史的生成控制参数。
type RequestOptions struct {
	// Model 是上游模型标识。
	Model string
	// Stream 表示调用方是否请求流式响应。
	Stream bool
	// MaxOutputTokens 是可选的输出 token 上限。
	MaxOutputTokens *int
	// Temperature 是可选的采样温度。
	Temperature *float64
	// PreviousResponseID 是调用方提供的上游响应关联标识。
	PreviousResponseID string
}

// DecodeRequest 将 OpenAI Responses JSON 请求转换为中间请求。
func DecodeRequest(data []byte) (AdaptedRequest, error) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&request); err != nil {
		return AdaptedRequest{}, fmt.Errorf("decode responses request: %w", err)
	}
	if request.Model == "" {
		return AdaptedRequest{}, errors.New("responses request model is required")
	}

	context := llm.RequestMessages{Model: request.Model, SystemPrompt: request.Instructions}
	if err := appendInputMessages(&context, request.Input); err != nil {
		return AdaptedRequest{}, err
	}
	for _, tool := range request.Tools {
		if tool.Type != "function" {
			continue
		}
		schema := tool.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		context.Tools = append(context.Tools, llm.ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	if err := context.Validate(); err != nil {
		return AdaptedRequest{}, fmt.Errorf("validate adapted request: %w", err)
	}
	return AdaptedRequest{
		Context: context,
		Options: RequestOptions{
			Model:              request.Model,
			Stream:             request.Stream,
			MaxOutputTokens:    request.MaxOutputTokens,
			Temperature:        request.Temperature,
			PreviousResponseID: request.PreviousResponseID,
		},
	}, nil
}

func appendInputMessages(context *llm.RequestMessages, raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
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
	for index, item := range items {
		if err := appendInputItem(context, item); err != nil {
			return fmt.Errorf("input[%d]: %w", index, err)
		}
	}
	return nil
}

func appendInputItem(context *llm.RequestMessages, raw json.RawMessage) error {
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
	case "message":
		return appendMessageItem(context, raw, header.Role)
	case "function_call":
		var item struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		arguments := json.RawMessage(item.Arguments)
		context.Messages = append(context.Messages, llm.AssistantMessage{
			Content:     []llm.Content{llm.ToolCall{ID: item.CallID, Name: item.Name, Arguments: arguments}},
			StopReason:  llm.StopReasonToolUse,
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	case "function_call_output":
		var item struct {
			CallID string          `json:"call_id"`
			Output json.RawMessage `json:"output"`
		}
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		output, err := rawOutputText(item.Output)
		if err != nil {
			return err
		}
		toolName := findToolName(context.Messages, item.CallID)
		if toolName == "" {
			return fmt.Errorf("function call output references unknown call_id %q", item.CallID)
		}
		context.Messages = append(context.Messages, llm.ToolResultMessage{
			ToolCallID:  item.CallID,
			ToolName:    toolName,
			Content:     []llm.Content{llm.TextContent{Text: output}},
			TimestampMS: time.Now().UnixMilli(),
		})
		return nil
	default:
		return nil
	}
}

func findToolName(messages []llm.Message, callID string) string {
	for index := len(messages) - 1; index >= 0; index-- {
		assistant, ok := messages[index].(llm.AssistantMessage)
		if !ok {
			continue
		}
		for _, block := range assistant.Content {
			call, ok := block.(llm.ToolCall)
			if ok && call.ID == callID {
				return call.Name
			}
		}
	}
	return ""
}

func appendMessageItem(context *llm.RequestMessages, raw json.RawMessage, role string) error {
	switch role {
	case "user", "assistant", "system", "developer":
	default:
		return nil
	}
	var item struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return err
	}
	content, err := decodeMessageContent(item.Content)
	if err != nil {
		return err
	}
	if len(content) == 0 {
		return nil
	}
	switch role {
	case "user":
		context.Messages = append(context.Messages, llm.UserMessage{Content: content, TimestampMS: time.Now().UnixMilli()})
	case "assistant":
		context.Messages = append(context.Messages, llm.AssistantMessage{Content: content, TimestampMS: time.Now().UnixMilli()})
	case "system", "developer":
		text := contentText(content)
		if context.SystemPrompt != "" && text != "" {
			context.SystemPrompt += "\n"
		}
		context.SystemPrompt += text
	}
	return nil
}

func decodeMessageContent(raw json.RawMessage) ([]llm.Content, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []llm.Content{llm.TextContent{Text: text}}, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("decode message content: %w", err)
	}
	content := make([]llm.Content, 0, len(parts))
	for index, part := range parts {
		var header struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(part, &header); err != nil {
			return nil, fmt.Errorf("content[%d]: %w", index, err)
		}
		switch header.Type {
		case "input_text", "output_text", "text":
			content = append(content, llm.TextContent{Text: header.Text})
		case "input_image", "image_url", "image":
			image, err := decodeImagePart(part)
			if err != nil {
				return nil, fmt.Errorf("content[%d]: %w", index, err)
			}
			content = append(content, image)
		default:
			// 忽略未知 part，避免 IDE 额外字段整请求失败。
			continue
		}
	}
	return content, nil
}

// decodeImagePart 兼容 OpenAI Responses / Chat Completions / Anthropic 常见图片 part 形态。
func decodeImagePart(raw json.RawMessage) (llm.ImageContent, error) {
	var envelope struct {
		Type     string          `json:"type"`
		ImageURL json.RawMessage `json:"image_url"`
		Image    json.RawMessage `json:"image"`
		Source   json.RawMessage `json:"source"`
		FileID   string          `json:"file_id"`
		Detail   string          `json:"detail"`
		// 少数客户端把 data URL 直接放在 url / data 字段。
		URL  string `json:"url"`
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return llm.ImageContent{}, err
	}
	if envelope.FileID != "" {
		return llm.ImageContent{}, errors.New("file_id images are not supported; use base64 data URL in image_url")
	}

	candidates := []json.RawMessage{envelope.ImageURL, envelope.Image, envelope.Source}
	for _, candidate := range candidates {
		if len(bytes.TrimSpace(candidate)) == 0 || bytes.Equal(bytes.TrimSpace(candidate), []byte("null")) {
			continue
		}
		if image, err := decodeImageValue(candidate); err == nil {
			return image, nil
		} else if !errors.Is(err, errImageShape) {
			return llm.ImageContent{}, err
		}
	}
	if envelope.URL != "" {
		return decodeDataImage(envelope.URL)
	}
	if envelope.Data != "" {
		return decodeDataImage(envelope.Data)
	}
	return llm.ImageContent{}, errors.New("image part missing image_url/url/data (base64 data URL required)")
}

var errImageShape = errors.New("unrecognized image value shape")

func decodeImageValue(raw json.RawMessage) (llm.ImageContent, error) {
	var asString string
	if json.Unmarshal(raw, &asString) == nil {
		return decodeDataImage(asString)
	}
	var asObject struct {
		URL       string `json:"url"`
		Data      string `json:"data"`
		Base64    string `json:"base64"`
		B64JSON   string `json:"b64_json"`
		MIMEType  string `json:"mime_type"`
		MediaType string `json:"media_type"`
		Type      string `json:"type"` // anthropic source.type = base64
		Detail    string `json:"detail"`
		FileID    string `json:"file_id"`
	}
	if err := json.Unmarshal(raw, &asObject); err != nil {
		return llm.ImageContent{}, errImageShape
	}
	if asObject.FileID != "" {
		return llm.ImageContent{}, errors.New("file_id images are not supported; use base64 data URL")
	}
	if asObject.URL != "" {
		return decodeDataImage(asObject.URL)
	}
	encoded := asObject.Data
	if encoded == "" {
		encoded = asObject.Base64
	}
	if encoded == "" {
		encoded = asObject.B64JSON
	}
	if encoded == "" {
		return llm.ImageContent{}, errImageShape
	}
	mimeType := asObject.MIMEType
	if mimeType == "" {
		mimeType = asObject.MediaType
	}
	if strings.HasPrefix(encoded, "data:") {
		return decodeDataImage(encoded)
	}
	if mimeType == "" {
		mimeType = sniffImageMIME(encoded)
	}
	if mimeType == "" {
		return llm.ImageContent{}, errors.New("image base64 requires mime_type/media_type or data URL prefix")
	}
	return decodeRawBase64(encoded, mimeType)
}

func decodeDataImage(value string) (llm.ImageContent, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return llm.ImageContent{}, errors.New("image url/data is empty")
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		return llm.ImageContent{}, errors.New("http(s) image URLs are not fetched yet; embed as data:image/...;base64,...")
	}
	if !strings.HasPrefix(value, "data:") {
		// 纯 base64：尝试按魔数嗅探。
		if mimeType := sniffImageMIME(value); mimeType != "" {
			return decodeRawBase64(value, mimeType)
		}
		return llm.ImageContent{}, errors.New("only data URL or raw base64 images are supported")
	}
	meta, encoded, ok := strings.Cut(value, ",")
	if !ok {
		return llm.ImageContent{}, errors.New("image must be a base64 data URL")
	}
	meta = strings.TrimPrefix(meta, "data:")
	// 允许 data:image/png;base64,xxx 与 data:image/png;charset=utf-8;base64,xxx
	isBase64 := strings.Contains(meta, ";base64") || !strings.Contains(meta, ";")
	if strings.Contains(meta, ";base64") {
		isBase64 = true
	}
	mimeType := meta
	if i := strings.Index(mimeType, ";"); i >= 0 {
		mimeType = mimeType[:i]
	}
	mimeType = strings.TrimSpace(mimeType)
	if mimeType == "" {
		mimeType = "image/png"
	}
	if _, _, err := mime.ParseMediaType(mimeType); err != nil {
		return llm.ImageContent{}, fmt.Errorf("invalid image MIME type: %w", err)
	}
	if !isBase64 {
		return llm.ImageContent{}, errors.New("image data URL must be base64 encoded")
	}
	return decodeRawBase64(encoded, mimeType)
}

func decodeRawBase64(encoded, mimeType string) (llm.ImageContent, error) {
	encoded = strings.TrimSpace(encoded)
	// 去掉空白/换行（部分客户端会折行）。
	encoded = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, encoded)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		// URL-safe base64
		data, err = base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(encoded)
			if err != nil {
				data, err = base64.RawURLEncoding.DecodeString(encoded)
			}
		}
		if err != nil {
			return llm.ImageContent{}, fmt.Errorf("decode image data: %w", err)
		}
	}
	if len(data) == 0 {
		return llm.ImageContent{}, errors.New("image data is empty")
	}
	// 上游按纯 base64 字符串接收，不带 data: 前缀。
	return llm.ImageContent{
		Data:     base64.StdEncoding.EncodeToString(data),
		MIMEType: mimeType,
	}, nil
}

func sniffImageMIME(encoded string) string {
	encoded = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, encoded)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(encoded)
	}
	if err != nil || len(raw) < 4 {
		return ""
	}
	switch {
	case len(raw) >= 3 && raw[0] == 0xff && raw[1] == 0xd8 && raw[2] == 0xff:
		return "image/jpeg"
	case len(raw) >= 8 && raw[0] == 0x89 && raw[1] == 0x50 && raw[2] == 0x4e && raw[3] == 0x47:
		return "image/png"
	case len(raw) >= 6 && raw[0] == 0x47 && raw[1] == 0x49 && raw[2] == 0x46:
		return "image/gif"
	case len(raw) >= 12 && raw[0] == 0x52 && raw[1] == 0x49 && raw[2] == 0x46 && raw[3] == 0x46 &&
		raw[8] == 0x57 && raw[9] == 0x45 && raw[10] == 0x42 && raw[11] == 0x50:
		return "image/webp"
	default:
		return ""
	}
}

func rawOutputText(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", errors.New("function call output is required")
	}
	return string(raw), nil
}

func contentText(content []llm.Content) string {
	var builder strings.Builder
	for _, block := range content {
		if text, ok := block.(llm.TextContent); ok {
			builder.WriteString(text.Text)
		}
	}
	return builder.String()
}
