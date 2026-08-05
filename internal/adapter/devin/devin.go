// 本文件实现 RequestMessages 与 Devin Connect RPC 的双向转换。
//
// Package devin 负责一次 Devin GetChatMessage 调用及其响应事件转换，不执行工具或 agent loop。
package devin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"connectrpc.com/connect"
	"github.com/leookun/devin-2api/internal/adapter"
	"github.com/leookun/devin-2api/internal/debuglog"
	"github.com/leookun/devin-2api/internal/llm"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"
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
}

// Adapter 调用 Devin 的 ApiServerService/GetChatMessage。
type Adapter struct {
	config Config
	client devinprotoconnect.ApiServerServiceClient
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
	transport := &authTransport{base: http.DefaultTransport, token: config.Token}
	client := devinprotoconnect.NewApiServerServiceClient(&http.Client{Transport: transport}, config.BaseURL)
	return &Adapter{config: config, client: client}, nil
}

// Stream 将一份中间请求转换为 Devin RPC，并返回一份中间响应事件流。
func (adapter *Adapter) Stream(ctx context.Context, request llm.RequestMessages) (llm.ResponseStream, error) {
	if err := request.Validate(); err != nil {
		return nil, fmt.Errorf("validate Devin request: %w", err)
	}
	protoRequest, err := buildRequest(request, adapter.config)
	if err != nil {
		return nil, err
	}
	recorder := debuglog.FromContext(ctx)
	recordProtoJSON(recorder, "03-devin-request.json", protoRequest)
	stream, err := adapter.client.GetChatMessage(ctx, connect.NewRequest(protoRequest))
	if err != nil {
		recorder.WriteError("devin_connect", err)
		return nil, fmt.Errorf("Devin GetChatMessage: %w", err)
	}
	return &responseStream{upstream: stream, decoder: newResponseDecoder(adapter.config.Model), recorder: recorder}, nil
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

func buildRequest(request llm.RequestMessages, config Config) (*devinproto.GetChatMessageRequest, error) {
	fingerprint, err := randomHex(366)
	if err != nil {
		return nil, fmt.Errorf("generate Devin device fingerprint: %w", err)
	}
	trajectoryID := randomUUID()
	cascadeID := randomUUID()
	executionID := randomUUID()
	metadata := &devinproto.ExaCodeiumCommonPb_Metadata{
		ApiKey:           proto.String(config.Token),
		ExtensionName:    proto.String(clientName),
		ExtensionVersion: proto.String(clientVersion),
		IdeName:          proto.String(clientName),
		IdeVersion:       proto.String(clientVersion),
		Locale:           proto.String("en"),
		Os:               proto.String("mac"),
		F:                proto.String(fingerprint),
	}
	result := &devinproto.GetChatMessageRequest{
		Metadata:     metadata,
		Prompt:       proto.String(withToolDescriptions(request.SystemPrompt, request.Tools)),
		ChatModelUid: proto.String(config.Model),
		RequestType:  devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration: &devinproto.ExaCodeiumCommonPb_CompletionConfiguration{
			NumCompletions: proto.Uint64(1),
			MaxTokens:      proto.Uint64(128000),
			MaxNewlines:    proto.Uint64(400),
			Temperature:    proto.Float64(1),
			TopK:           proto.Uint64(40),
			TopP:           proto.Float64(0.95),
		},
		TrajectoryReference: &devinproto.ExaCortexPb_CortexTrajectoryReference{
			TrajectoryId:   proto.String(trajectoryID),
			TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
			StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
		},
		CascadeId:   proto.String(cascadeID),
		PlannerMode: devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
		ExecutionId: proto.String(executionID),
	}
	for index, message := range request.Messages {
		converted, err := convertMessage(message)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", index, err)
		}
		result.ChatMessagePrompts = append(result.ChatMessagePrompts, converted...)
	}
	for _, tool := range request.Tools {
		converted, err := convertToolDefinition(tool)
		if err != nil {
			return nil, err
		}
		result.Tools = append(result.Tools, converted)
	}
	return result, nil
}

func convertMessage(message llm.Message) ([]*devinproto.ExaChatPb_ChatMessagePrompt, error) {
	switch message := message.(type) {
	case llm.UserMessage:
		return []*devinproto.ExaChatPb_ChatMessagePrompt{promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER, message.Content)}, nil
	case llm.AssistantMessage:
		prompt := promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM, message.Content)
		for _, block := range message.Content {
			if call, ok := block.(llm.ToolCall); ok {
				prompt.ToolCalls = append(prompt.ToolCalls, &devinproto.ExaCodeiumCommonPb_ChatToolCall{
					Id:            proto.String(call.ID),
					Name:          proto.String(call.Name),
					ArgumentsJson: proto.String(string(call.Arguments)),
				})
			}
		}
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	case llm.ToolResultMessage:
		prompt := promptForContent(devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL, message.Content)
		prompt.ToolCallId = proto.String(message.ToolCallID)
		prompt.ToolResultIsError = proto.Bool(message.IsError)
		return []*devinproto.ExaChatPb_ChatMessagePrompt{prompt}, nil
	default:
		return nil, fmt.Errorf("unsupported message type %T", message)
	}
}

func promptForContent(source devinproto.ExaCodeiumCommonPb_ChatMessageSource, content []llm.Content) *devinproto.ExaChatPb_ChatMessagePrompt {
	prompt := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randomID()),
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
			prompt.Images = append(prompt.Images, &devinproto.ExaCodeiumCommonPb_ImageData{
				Base64Data: proto.String(block.Data), MimeType: proto.String(block.MIMEType),
			})
		}
	}
	prompt.Prompt = proto.String(text.String())
	return prompt
}

func randomID() string {
	return randomUUID()
}

func randomUUID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// responseStream 从 Connect 上游按需读取帧并依次返回 decoder 生成的事件。
type responseStream struct {
	// upstream 是 Devin Connect 返回的服务端流。
	upstream devinResponseReceiver
	// decoder 将一个 Devin protobuf 帧转换为零个或多个中间响应事件。
	decoder *responseDecoder
	// recorder 记录 Devin 原始响应帧；nil 表示禁用调试日志。
	recorder *debuglog.Recorder
	// started 表示是否已经请求 decoder 产生 start 事件。
	started bool
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
	for len(stream.queue) == 0 && !stream.finished {
		if err := ctx.Err(); err != nil {
			return llm.ResponseEvent{}, err
		}
		if !stream.started {
			stream.started = true
			stream.queue = append(stream.queue, stream.decoder.start()...)
			break
		}
		if !stream.upstream.Receive() {
			stream.queue = append(stream.queue, stream.decoder.finish(stream.upstream.Err())...)
			stream.finished = true
			break
		}
		response := stream.upstream.Msg()
		recordProtoJSON(stream.recorder, "04-devin-response.jsonl", response)
		stream.queue = append(stream.queue, stream.decoder.decode(response)...)
		stream.finished = stream.decoder.finished
	}
	if len(stream.queue) > 0 {
		event := stream.queue[0]
		stream.queue = stream.queue[1:]
		return event, nil
	}
	return llm.ResponseEvent{}, io.EOF
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
