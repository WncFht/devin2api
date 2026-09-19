package messages

import (
	"encoding/base64"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

func vidData(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func TestVideoDecode(t *testing.T) {
	body := `{
		"model": "kimi-k3-high",
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "watch this"},
				{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": "` + vidData("fake-mp4") + `"}}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
			{"role": "user", "content": [
				{"type": "video", "source": {"type": "url", "url": "https://example.com/x.mp4"}},
				{"type": "video_url", "video_url": "https://example.com/y.mp4"},
				{"type": "video_url", "video_url": {"url": "data:video/webm;base64,` + vidData("webm") + `"}},
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": [
					{"type": "video", "source": {"type": "base64", "media_type": "video/mp4", "data": "` + vidData("inner") + `"}},
					{"type": "resource", "resource": {"uri": "file:///tmp/r.mp4", "mimeType": "video/mp4", "blob": "` + vidData("res") + `"}}
				]}
			]}
		]
	}`
	adapted, err := DecodeRequest([]byte(body), false)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var vids []llm.VideoContent
	for _, msg := range adapted.Context.Messages {
		var blocks []llm.Content
		switch msg := msg.(type) {
		case llm.UserMessage:
			blocks = msg.Content
		case llm.ToolResultMessage:
			blocks = msg.Content
		}
		for _, block := range blocks {
			if vid, ok := block.(llm.VideoContent); ok {
				vids = append(vids, vid)
			}
		}
	}
	if len(vids) != 6 {
		t.Fatalf("want 6 videos, got %d: %+v", len(vids), vids)
	}
	if vids[0].MIMEType != "video/mp4" || vids[0].Data == "" {
		t.Fatalf("base64 video wrong: %+v", vids[0])
	}
	if vids[1].URL != "https://example.com/x.mp4" || vids[1].MIMEType != "" {
		t.Fatalf("url-source video wrong (mime must be empty): %+v", vids[1])
	}
	if vids[2].URL != "https://example.com/y.mp4" || vids[2].MIMEType != "" {
		t.Fatalf("video_url string wrong: %+v", vids[2])
	}
	if vids[3].URL != "" || vids[3].MIMEType != "video/webm" || vids[3].Data != vidData("webm") {
		t.Fatalf("data: video_url must decode to base64 form: %+v", vids[3])
	}
	if vids[4].Data != vidData("inner") || vids[4].MIMEType != "video/mp4" {
		t.Fatalf("tool_result inner video wrong: %+v", vids[4])
	}
	if vids[5].Data != vidData("res") || vids[5].MIMEType != "video/mp4" {
		t.Fatalf("resource-blob video wrong: %+v", vids[5])
	}
	for i, vid := range vids {
		if err := vid.Validate(); err != nil {
			t.Fatalf("video %d invalid: %v", i, err)
		}
	}
}

func TestVideoURLRejectsMIMEType(t *testing.T) {
	// 上游规则与文档同制：url 形态携带 mime_type 直接拒整请求。
	vid := llm.VideoContent{URL: "https://example.com/x.mp4", MIMEType: "video/mp4"}
	if err := vid.Validate(); err == nil {
		t.Fatal("url+mime must fail validation")
	}
}
