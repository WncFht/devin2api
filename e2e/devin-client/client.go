// Package devinclient 是 outputs/devin-proto-go 生成绑定的最小使用样例：
// 绕开内部 llm 抽象直接构造 Connect 请求、消费服务端流。
// 无生产调用方——它的价值是让 go build ./... 顺带验证生成产物可用，
// 以及给手动联调上游时提供一个复制起点。
package devinclient

import (
	"context"
	"net/http"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// NewChatRequest 构造一个带单个只读工具的 GetChatMessage 请求。
func NewChatRequest(prompt string) *connect.Request[devinproto.GetChatMessageRequest] {
	return connect.NewRequest(&devinproto.GetChatMessageRequest{
		Prompt: proto.String(prompt),
		Tools: []*devinproto.ExaChatPb_ChatToolDefinition{{
			Name:             proto.String("read_file"),
			Description:      proto.String("Read a file from the workspace"),
			JsonSchemaString: proto.String(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			ReadOnlyHint:     proto.Bool(true),
		}},
	})
}

// StreamChat 发出请求并收集整条流的响应帧。
func StreamChat(ctx context.Context, baseURL, prompt string) ([]*devinproto.GetChatMessageResponse, error) {
	client := devinprotoconnect.NewApiServerServiceClient(http.DefaultClient, baseURL)
	stream, err := client.GetChatMessage(ctx, NewChatRequest(prompt))
	if err != nil {
		return nil, err
	}

	var responses []*devinproto.GetChatMessageResponse
	for stream.Receive() {
		responses = append(responses, stream.Msg())
	}
	return responses, stream.Err()
}
