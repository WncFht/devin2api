package devin

import (
	"testing"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
)

func TestPromptForContentVideos(t *testing.T) {
	var repairs llm.RequestRepairs
	user := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER
	prompt := promptForContent(user, []llm.Content{
		llm.TextContent{Text: "watch"},
		llm.VideoContent{Data: "aGk=", MIMEType: "video/mp4"},
		llm.VideoContent{URL: "https://example.com/x.mp4"},
	}, false, &repairs)
	vids := prompt.GetVideos()
	if len(vids) != 2 {
		t.Fatalf("want 2 videos, got %d", len(vids))
	}
	if vids[0].GetBase64Data() != "aGk=" || vids[0].GetMimeType() != "video/mp4" {
		t.Fatalf("base64 video wire wrong: %+v", vids[0])
	}
	if vids[1].GetUrl() != "https://example.com/x.mp4" || vids[1].GetMimeType() != "" {
		t.Fatalf("url video wire wrong (mime must be unset): %+v", vids[1])
	}
	// 历史轮视频照常挂（attachImages=false 只挡图，实测 kimi-k3 读历史轮帧）。
	history := promptForContent(user, []llm.Content{
		llm.ImageContent{Data: "aGk=", MIMEType: "image/png"},
		llm.VideoContent{Data: "aGk=", MIMEType: "video/mp4"},
	}, false, &repairs)
	if len(history.GetVideos()) != 1 || len(history.GetImages()) != 0 {
		t.Fatalf("history: want 1 video 0 images, got %d/%d", len(history.GetVideos()), len(history.GetImages()))
	}
}
