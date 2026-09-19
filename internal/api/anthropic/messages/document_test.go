package messages

import (
	"encoding/base64"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

func docText(text string) string {
	return base64.StdEncoding.EncodeToString([]byte(text))
}

func TestDocumentDecode(t *testing.T) {
	body := `{
		"model": "claude-opus-5",
		"max_tokens": 64,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "read this"},
				{"type": "document", "title": "secret.pdf", "source": {"type": "base64", "media_type": "application/pdf", "data": "` + docText("%PDF-1.4 fake") + `"}}
			]},
			{"role": "assistant", "content": [{"type": "text", "text": "ok"}]},
			{"role": "user", "content": [
				{"type": "document", "source": {"type": "url", "url": "https://example.com/x.pdf"}},
				{"type": "tool_result", "tool_use_id": "toolu_1", "content": [
					{"type": "document", "source": {"type": "text", "media_type": "text/plain", "data": "hello doc"}},
					{"type": "resource", "resource": {"uri": "file:///tmp/r.pdf", "mimeType": "application/pdf", "blob": "` + docText("%PDF-1.4 res") + `"}}
				]}
			]}
		]
	}`
	adapted, err := DecodeRequest([]byte(body), false)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var docs []llm.DocumentContent
	for _, msg := range adapted.Context.Messages {
		var blocks []llm.Content
		switch msg := msg.(type) {
		case llm.UserMessage:
			blocks = msg.Content
		case llm.ToolResultMessage:
			blocks = msg.Content
		}
		for _, block := range blocks {
			if doc, ok := block.(llm.DocumentContent); ok {
				docs = append(docs, doc)
			}
		}
	}
	if len(docs) != 4 {
		t.Fatalf("want 4 documents, got %d: %+v", len(docs), docs)
	}
	if docs[0].MIMEType != "application/pdf" || docs[0].Filename != "secret.pdf" || docs[0].Data == "" {
		t.Fatalf("base64 doc wrong: %+v", docs[0])
	}
	if docs[1].URL != "https://example.com/x.pdf" || docs[1].MIMEType != "" {
		t.Fatalf("url doc wrong (mime must be empty): %+v", docs[1])
	}
	if docs[2].MIMEType != "text/plain" || docs[2].Data != docText("hello doc") {
		t.Fatalf("text-source doc wrong: %+v", docs[2])
	}
	if docs[3].MIMEType != "application/pdf" || docs[3].Filename != "file:///tmp/r.pdf" {
		t.Fatalf("resource-blob doc wrong: %+v", docs[3])
	}
	for i, doc := range docs {
		if err := doc.Validate(); err != nil {
			t.Fatalf("doc %d invalid: %v", i, err)
		}
	}
}
