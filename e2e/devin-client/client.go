package example

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"
)

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
