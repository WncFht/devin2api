package devin

import (
	"testing"

	devinproto "local/devinproto"

	"github.com/WncFht/devin2api/internal/llm"
)

func TestPromptForContentDocuments(t *testing.T) {
	var repairs llm.RequestRepairs
	user := devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER
	prompt := promptForContent(user, []llm.Content{
		llm.TextContent{Text: "read"},
		llm.DocumentContent{Data: "aGk=", MIMEType: "text/plain", Filename: "a.txt"},
		llm.DocumentContent{URL: "https://example.com/x.pdf", Filename: "x.pdf"},
	}, false, &repairs)
	docs := prompt.GetDocuments()
	if len(docs) != 2 {
		t.Fatalf("want 2 documents, got %d", len(docs))
	}
	if docs[0].GetBase64Data() != "aGk=" || docs[0].GetMimeType() != "text/plain" || docs[0].GetFilename() != "a.txt" {
		t.Fatalf("base64 doc wire wrong: %+v", docs[0])
	}
	if docs[1].GetUrl() != "https://example.com/x.pdf" || docs[1].GetMimeType() != "" || docs[1].GetFilename() != "x.pdf" {
		t.Fatalf("url doc wire wrong (mime must be unset): %+v", docs[1])
	}
	// 历史轮文档照常挂（attachImages=false 只挡图）。
	history := promptForContent(user, []llm.Content{
		llm.ImageContent{Data: "aGk=", MIMEType: "image/png"},
		llm.DocumentContent{Data: "aGk=", MIMEType: "application/pdf", Filename: "h.pdf"},
	}, false, &repairs)
	if len(history.GetDocuments()) != 1 || len(history.GetImages()) != 0 {
		t.Fatalf("history: want 1 doc 0 images, got %d/%d", len(history.GetDocuments()), len(history.GetImages()))
	}
}
