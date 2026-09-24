// 附件降级的单元与编排层验证：主动（目录能力位/学习表）与被动
// （EndStream invalid_argument 后 reopen 重发）两条路径共用同一套
// 逐块改写规则，这里分别钉住改写形态、写回语义与端到端 wire。
package devin

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	devinproto "local/devinproto"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/adapter"
	"github.com/WncFht/devin2api/internal/llm"
)

// docRequest 造一条「文本 + 文档」的用户消息请求。
func docRequest(text, docData, mime, filename string) llm.RequestMessages {
	return llm.RequestMessages{
		Model: "m",
		Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
			llm.TextContent{Text: text},
			llm.DocumentContent{Data: docData, MIMEType: mime, Filename: filename},
		}}},
	}
}

func demotedTexts(t *testing.T, out llm.RequestMessages) []string {
	t.Helper()
	var texts []string
	for _, message := range out.Messages {
		user, ok := message.(llm.UserMessage)
		if !ok {
			continue
		}
		for _, block := range user.Content {
			if text, ok := block.(llm.TextContent); ok {
				texts = append(texts, text.Text)
			}
		}
	}
	return texts
}

// TestDemoteRequestAttachmentsInline 验证 utf8 文档正文内联进带边界
// 标记的文本块，原块位置保留。
func TestDemoteRequestAttachmentsInline(t *testing.T) {
	req := docRequest("read this", base64.StdEncoding.EncodeToString([]byte("hello body")), "text/plain", "a.txt")
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d, want 1", n)
	}
	texts := demotedTexts(t, out)
	if len(texts) != 2 {
		t.Fatalf("want 2 text blocks, got %d", len(texts))
	}
	want := "\n[document: a.txt (text/plain)]\nhello body\n[/document]\n"
	if texts[1] != want {
		t.Fatalf("demoted text = %q, want %q", texts[1], want)
	}
}

// TestDemoteRequestAttachmentsCOW 钉住结构级 copy-on-write：pool
// 多 lane 共享同一 request 的 Messages/Content 底数组，降级产出
// 必须是新结构且原请求逐字节不变。
func TestDemoteRequestAttachmentsCOW(t *testing.T) {
	doc := llm.DocumentContent{Data: base64.StdEncoding.EncodeToString([]byte("x")), MIMEType: "text/plain", Filename: "a.txt"}
	req := docRequest("t", doc.Data, doc.MIMEType, doc.Filename)
	origContent := req.Messages[0].(llm.UserMessage).Content
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d", n)
	}
	// 原消息与原 Content 底数组都不变。
	if _, still := origContent[1].(llm.DocumentContent); !still {
		t.Fatal("original content block mutated")
	}
	if _, still := req.Messages[0].(llm.UserMessage).Content[1].(llm.DocumentContent); !still {
		t.Fatal("original request message mutated")
	}
	// 新请求的消息/内容是新的底数组。
	newContent := out.Messages[0].(llm.UserMessage).Content
	if _, ok := newContent[1].(llm.TextContent); !ok {
		t.Fatal("demoted block is not TextContent")
	}
	if fmt.Sprintf("%p", origContent) == fmt.Sprintf("%p", newContent) {
		t.Fatal("content backing array shared with original")
	}
	// 无可降级对象时原请求原样返回（同一切片）。
	clean, n := demoteRequestAttachments(context.Background(), req, false, false, nil)
	if n != 0 || fmt.Sprintf("%p", clean.Messages) != fmt.Sprintf("%p", req.Messages) {
		t.Fatalf("no-op demote changed request: n=%d", n)
	}
}

// TestDemoteRequestAttachmentsBinary 验证二进制/不可解码文档落占位
// 标记并带「do not infer contents」禁读声明。
func TestDemoteRequestAttachmentsBinary(t *testing.T) {
	binary := append([]byte{0x89, 0x50, 0x4e, 0x47}, bytes.Repeat([]byte{0xff, 0x00}, 64)...)
	req := docRequest("t", base64.StdEncoding.EncodeToString(binary), "application/octet-stream", "blob.bin")
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d", n)
	}
	text := demotedTexts(t, out)[1]
	if !strings.Contains(text, "content not extracted") || !strings.Contains(text, "do not infer contents") {
		t.Fatalf("placeholder = %q", text)
	}
	if !strings.Contains(text, "blob.bin") || !strings.Contains(text, "application/octet-stream") {
		t.Fatalf("placeholder lacks label: %q", text)
	}
	// 非法 base64 同样落占位。
	req = docRequest("t", "!!!not-base64!!!", "text/plain", "bad.txt")
	out, n = demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 || !strings.Contains(demotedTexts(t, out)[1], "content not extracted") {
		t.Fatalf("bad b64: n=%d text=%q", n, demotedTexts(t, out)[1])
	}
}

// TestDemoteRequestAttachmentsURL 验证 URL 文档不抓取：http(s) 回显
// 截断地址，非 http(s) 不回显地址。
func TestDemoteRequestAttachmentsURL(t *testing.T) {
	req := docRequest("t", "", "", "site.txt")
	req.Messages[0].(llm.UserMessage).Content[1] = llm.DocumentContent{URL: "https://example.com/doc.txt?sig=abc", Filename: "site.txt"}
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d", n)
	}
	text := demotedTexts(t, out)[1]
	if !strings.Contains(text, "remote document at https://example.com/doc.txt") || !strings.Contains(text, "not fetched") {
		t.Fatalf("url placeholder = %q", text)
	}
	req.Messages[0].(llm.UserMessage).Content[1] = llm.DocumentContent{URL: "file:///etc/passwd", Filename: "evil"}
	out, _ = demoteRequestAttachments(context.Background(), req, true, false, nil)
	text = demotedTexts(t, out)[1]
	if strings.Contains(text, "file://") || !strings.Contains(text, "remote document") {
		t.Fatalf("non-http url leaked: %q", text)
	}
}

// TestDemoteRequestAttachmentsVideo 验证视频恒占位（网关侧无抽帧等价物）。
func TestDemoteRequestAttachmentsVideo(t *testing.T) {
	req := llm.RequestMessages{Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
		llm.VideoContent{Data: base64.StdEncoding.EncodeToString([]byte("vid")), MIMEType: "video/mp4"},
	}}}}
	out, n := demoteRequestAttachments(context.Background(), req, false, true, nil)
	if n != 1 {
		t.Fatalf("demoted = %d", n)
	}
	text := demotedTexts(t, out)[0]
	if !strings.Contains(text, "video (video/mp4)") || !strings.Contains(text, "content not extracted") {
		t.Fatalf("video placeholder = %q", text)
	}
}

// TestDemoteRequestAttachmentsSelective 验证两维独立：只降级文档时
// 视频块原样保留。
func TestDemoteRequestAttachmentsSelective(t *testing.T) {
	req := llm.RequestMessages{Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
		llm.DocumentContent{Data: base64.StdEncoding.EncodeToString([]byte("doc")), MIMEType: "text/plain", Filename: "a.txt"},
		llm.VideoContent{Data: base64.StdEncoding.EncodeToString([]byte("vid")), MIMEType: "video/mp4"},
	}}}}
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d, want 1", n)
	}
	content := out.Messages[0].(llm.UserMessage).Content
	if _, ok := content[1].(llm.VideoContent); !ok {
		t.Fatal("video block demoted when only docs requested")
	}
}

// TestDemoteRequestAttachmentsPDF 验证 PDF 走抽取器：可读 PDF 内联
// 抽取文本而非占位。
func TestDemoteRequestAttachmentsPDF(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	off1 := b.Len()
	b.WriteString("1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n")
	off2 := b.Len()
	b.WriteString("2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n")
	off3 := b.Len()
	b.WriteString("3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]/Contents 4 0 R/Resources<</Font<</F1 5 0 R>>>>>>endobj\n")
	off4 := b.Len()
	stream := "BT /F1 24 Tf 72 700 Td (Hello PDF) Tj ET"
	fmt.Fprintf(&b, "4 0 obj<</Length %d>>stream\n%s\nendstream\nendobj\n", len(stream), stream)
	off5 := b.Len()
	b.WriteString("5 0 obj<</Type/Font/Subtype/Type1/BaseFont/Helvetica>>endobj\n")
	xref := b.Len()
	b.WriteString("xref\n0 6\n0000000000 65535 f \n")
	fmt.Fprintf(&b, "%010d 00000 n \n%010d 00000 n \n%010d 00000 n \n%010d 00000 n \n%010d 00000 n \n", off1, off2, off3, off4, off5)
	fmt.Fprintf(&b, "trailer<</Size 6/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n", xref)

	req := docRequest("t", base64.StdEncoding.EncodeToString(b.Bytes()), "application/pdf", "doc.pdf")
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d", n)
	}
	text := demotedTexts(t, out)[1]
	if !strings.Contains(text, "[document: doc.pdf (application/pdf)]") || !strings.Contains(text, "Hello PDF") {
		t.Fatalf("pdf demoted text = %q", text)
	}
	// 抽取不出文本（损坏 pdf）时落占位。
	req = docRequest("t", base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 garbage")), "application/pdf", "broken.pdf")
	out, _ = demoteRequestAttachments(context.Background(), req, true, false, nil)
	text = demotedTexts(t, out)[1]
	if !strings.Contains(text, "content not extracted") {
		t.Fatalf("broken pdf should placeholder, got %q", text)
	}
}

// TestDemoteRequestAttachmentsBudgets 验证尺寸预算：单文档截断到
// demoteInlineBodyCap，请求总量耗尽后剩余文档落占位——把永久 400
// 换成永久 413 不算修好。
func TestDemoteRequestAttachmentsBudgets(t *testing.T) {
	big := strings.Repeat("x", demoteInlineBodyCap+1024)
	req := docRequest("t", base64.StdEncoding.EncodeToString([]byte(big)), "text/plain", "big.txt")
	out, _ := demoteRequestAttachments(context.Background(), req, true, false, nil)
	text := demotedTexts(t, out)[1]
	if !strings.Contains(text, "[document truncated]") || len(text) > demoteInlineBodyCap+128 {
		t.Fatalf("truncation missing or oversize: len=%d", len(text))
	}
	if !utf8.ValidString(text) {
		t.Fatal("truncated body broke utf8")
	}
	// 总预算耗尽后后续文档占位。
	var blocks []llm.Content
	for i := 0; i < 6; i++ {
		blocks = append(blocks, llm.DocumentContent{
			Data:     base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 30<<10))),
			MIMEType: "text/plain", Filename: fmt.Sprintf("f%d.txt", i)})
	}
	req = llm.RequestMessages{Messages: []llm.Message{llm.UserMessage{Content: blocks}}}
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 6 {
		t.Fatalf("demoted = %d, want 6", n)
	}
	content := out.Messages[0].(llm.UserMessage).Content
	last := content[5].(llm.TextContent).Text
	if !strings.Contains(last, "content not extracted") {
		t.Fatalf("budget-exhausted doc should placeholder, got %q", last[:80])
	}
}

// TestDemoteRequestAttachmentsSanitizes 验证合成文本过指纹改写：正文
// 携带 message 桶指纹句（Claude Code 自我身份句）必须被改写——降级
// 发生在 sanitizeRequest 之后，不走这道会把指纹直接送上游。
func TestDemoteRequestAttachmentsSanitizes(t *testing.T) {
	body := "intro\nYou are Claude Code, Anthropic's official CLI for Claude.\noutro"
	req := docRequest("t", base64.StdEncoding.EncodeToString([]byte(body)), "text/plain", "cc.txt")
	hits := map[string]int{}
	out, _ := demoteRequestAttachments(context.Background(), req, true, false, hits)
	text := demotedTexts(t, out)[1]
	if strings.Contains(text, "official CLI for Claude") || !strings.Contains(text, "AI coding assistant") {
		t.Fatalf("fingerprint not sanitized: %q", text)
	}
	if len(hits) == 0 {
		t.Fatal("sanitize hits not recorded")
	}
}

// TestDemoteRequestAttachmentsToolResult 验证工具结果消息里的文档块
// 同样降级且 ToolCallID 等字段随克隆保留。
func TestDemoteRequestAttachmentsToolResult(t *testing.T) {
	req := llm.RequestMessages{Messages: []llm.Message{
		llm.ToolResultMessage{ToolCallID: "call-9", IsError: true, Content: []llm.Content{
			llm.DocumentContent{Data: base64.StdEncoding.EncodeToString([]byte("out")), MIMEType: "text/plain", Filename: "r.txt"},
		}},
	}}
	out, n := demoteRequestAttachments(context.Background(), req, true, false, nil)
	if n != 1 {
		t.Fatalf("demoted = %d", n)
	}
	result := out.Messages[0].(llm.ToolResultMessage)
	if result.ToolCallID != "call-9" || !result.IsError {
		t.Fatalf("tool result fields lost: %+v", result)
	}
	if _, ok := result.Content[0].(llm.TextContent); !ok {
		t.Fatal("tool result doc not demoted to text")
	}
}

// TestAttachmentDenialKind 钉住上游附件拒收文案的识别口径。
func TestAttachmentDenialKind(t *testing.T) {
	cases := []struct {
		err      error
		wantKind string
		wantOK   bool
	}{
		{connect.NewError(connect.CodeInvalidArgument, errors.New(`model "swe-2-max" does not support file inputs (supports_documents=false)`)), attachmentKindDocument, true},
		{connect.NewError(connect.CodeInvalidArgument, errors.New(`model "swe-2-max" does not support video inputs (supports_video=false)`)), attachmentKindVideo, true},
		{connect.NewError(connect.CodeInvalidArgument, errors.New("tool_choice names tool")), "", false},
		{connect.NewError(connect.CodePermissionDenied, errors.New("does not support file inputs")), "", false},
		{errors.New("does not support file inputs"), "", false},
		{nil, "", false},
	}
	for index, c := range cases {
		kind, ok := attachmentDenialKind(c.err)
		if kind != c.wantKind || ok != c.wantOK {
			t.Fatalf("case %d: got (%q,%v), want (%q,%v)", index, kind, ok, c.wantKind, c.wantOK)
		}
	}
	// *llm.Failure 形态（Classify 归一后的包装）同样可识别。
	failure := &llm.Failure{Code: "invalid_argument", Message: `model "m" does not support file inputs`}
	if kind, ok := attachmentDenialKind(failure); !ok || kind != attachmentKindDocument {
		t.Fatalf("typed failure: got (%q,%v)", kind, ok)
	}
}

// TestAttachmentLearning 验证学习表登记与查验：登记后同 (uid,kind)
// 在保鲜期内命中，异 kind 不连坐；attachmentUnsupported 把「目录
// 已知不支持」与「学习表命中」两个来源都算作不支持。
func TestAttachmentLearning(t *testing.T) {
	a := &Adapter{}
	a.bindFlightLocks()
	denied := connect.NewError(connect.CodeInvalidArgument, errors.New(`model "m1" does not support file inputs`))
	a.noteAttachmentDenied("m1", denied)
	if !a.attachmentDeniedRecently("m1", attachmentKindDocument) {
		t.Fatal("learned denial not visible")
	}
	if a.attachmentDeniedRecently("m1", attachmentKindVideo) {
		t.Fatal("video denial should not be learned from document error")
	}
	// 无登记、非附件拒收、空 uid 都不进表。
	a.noteAttachmentDenied("m1", connect.NewError(connect.CodeInvalidArgument, errors.New("other")))
	a.noteAttachmentDenied("", denied)
	if len(a.attachmentDenied) != 1 {
		t.Fatalf("attachmentDenied size = %d, want 1", len(a.attachmentDenied))
	}
	// attachmentUnsupported：目录已知不支持 → true（学习表无关）。
	a.models = []adapter.ModelInfo{{ID: "m2", SupportsDocuments: false}}
	if !a.attachmentUnsupported("m2", attachmentKindDocument) {
		t.Fatal("catalog-known-unsupported should be unsupported")
	}
	// 目录缺席 + 无学习 → false（交给上游裁决）。
	if a.attachmentUnsupported("absent-model", attachmentKindDocument) {
		t.Fatal("absent model without learning should not be unsupported")
	}
	// 目录缺席 + 已学习 → true。
	if !a.attachmentUnsupported("m1", attachmentKindDocument) {
		t.Fatal("learned denial should mark unsupported")
	}
	// 目录已知支持且未学习 → false。
	a.models = append(a.models, adapter.ModelInfo{ID: "m3", SupportsDocuments: true})
	if a.attachmentUnsupported("m3", attachmentKindDocument) {
		t.Fatal("catalog-supported should not be unsupported")
	}
}

// TestDemoteUnsupportedAttachmentsPredicate 验证入口判定的两路来源：
// 目录能力位在场按位判，缺席查学习表。
func TestDemoteUnsupportedAttachmentsPredicate(t *testing.T) {
	a := &Adapter{}
	a.bindFlightLocks()
	a.models = []adapter.ModelInfo{{ID: "no-doc", SupportsDocuments: false, SupportsVideo: true}}
	req := docRequest("t", base64.StdEncoding.EncodeToString([]byte("body")), "text/plain", "a.txt")
	// 目录已知不支持 → 降级。
	out, n := a.demoteUnsupportedAttachments(context.Background(), req, "no-doc", nil)
	if n != 1 {
		t.Fatalf("catalog-unsupported: demoted = %d", n)
	}
	_ = out
	// 目录缺席 + 未学习 → 原样。
	out, n = a.demoteUnsupportedAttachments(context.Background(), req, "ghost", nil)
	if n != 0 {
		t.Fatalf("unknown model should not demote proactively, got %d", n)
	}
	// 学习表命中 → 降级。
	a.noteAttachmentDenied("ghost", connect.NewError(connect.CodeInvalidArgument, errors.New(`does not support file inputs`)))
	_, n = a.demoteUnsupportedAttachments(context.Background(), req, "ghost", nil)
	if n != 1 {
		t.Fatalf("learned model should demote, got %d", n)
	}
	// 无附件请求不降级也不动结构。
	_, n = a.demoteUnsupportedAttachments(context.Background(), stubRequest(), "no-doc", nil)
	if n != 0 {
		t.Fatalf("no attachments: demoted = %d", n)
	}
}

// TestDemoteAttachmentsWriteBack 验证反应式路径的写回语义：
// demoteAttachments 改写 r.request，此后 rebuild 产出的 wire 请求
// 不再携带 Documents——extend/tryResume 的续发继承降级形态。
func TestDemoteAttachmentsWriteBack(t *testing.T) {
	req := docRequest("t", base64.StdEncoding.EncodeToString([]byte("body")), "text/plain", "a.txt")
	r := &attemptRunner{adapter: &Adapter{}, request: req, cfg: Config{Model: "m"}, binding: callBinding{Model: "m"}}
	if n := r.demoteAttachments(context.Background(), attachmentKindDocument); n != 1 {
		t.Fatalf("demoteAttachments = %d", n)
	}
	built, err := r.rebuild(nil)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	for _, prompt := range built.GetChatMessagePrompts() {
		if len(prompt.GetDocuments()) != 0 {
			t.Fatal("demoted request still carries documents")
		}
	}
	found := false
	for _, prompt := range built.GetChatMessagePrompts() {
		if strings.Contains(prompt.GetPrompt(), "[document: a.txt") {
			found = true
		}
	}
	if !found {
		t.Fatal("demoted marker missing from wire prompt")
	}
	// 二次调用幂等（没有可降对象返回 0）。
	if n := r.demoteAttachments(context.Background(), attachmentKindDocument); n != 0 {
		t.Fatalf("second demote = %d", n)
	}
}

// stubModelEntryDocs 造一条带文档/视频能力位的目录条目。
func stubModelEntryDocs(uid string, docs, videos bool) *devinproto.ExaCodeiumCommonPb_ClientModelConfig {
	entry := stubModelEntry(uid, false)
	entry.ModelInfo = &devinproto.ExaCodeiumCommonPb_ModelInfo{
		ModelFeatures: &devinproto.ExaCodeiumCommonPb_ModelFeatures{
			SupportsDocuments: proto.Bool(docs),
			SupportsVideo:     proto.Bool(videos),
		},
	}
	return entry
}

// TestOrchestrationProactiveDemote 端到端：目录声明不收文档时，首个
// wire 请求就已降级——Documents 字段为空、正文带 [document:] 标记，
// 会话种子沿用降级前形态（trajectory/cascade 与未降级请求一致）。
func TestOrchestrationProactiveDemote(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntryDocs("stub-model", false, true)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			return stubSend(stream, stubMeta(), stubDelta("ok"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Identity: LaneIdentity{Token: "tok"}})

	req := docRequest("summarize", base64.StdEncoding.EncodeToString([]byte("doc body")), "text/plain", "a.txt")
	req.Model = "stub-model"
	wantTrajectory, wantCascade := deriveSessionIDs(req)
	stream, err := adapter.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "ok" {
		t.Fatalf("deltas = %q", got)
	}
	if stub.chatCalls.Load() != 1 {
		t.Fatalf("chat calls = %d, want 1", stub.chatCalls.Load())
	}
	wire := stub.requests[0]
	for _, prompt := range wire.GetChatMessagePrompts() {
		if len(prompt.GetDocuments()) != 0 {
			t.Fatal("proactive demote left documents on wire")
		}
	}
	var combined string
	for _, prompt := range wire.GetChatMessagePrompts() {
		combined += prompt.GetPrompt()
	}
	if !strings.Contains(combined, "[document: a.txt (text/plain)]") || !strings.Contains(combined, "doc body") {
		t.Fatalf("wire prompt lacks demoted doc: %q", combined)
	}
	// 冻结种子：wire 的 trajectory/cascade 与降级前请求派生一致。
	if got := wire.GetTrajectoryReference().GetTrajectoryId(); got != wantTrajectory {
		t.Fatalf("trajectory = %q, want frozen %q", got, wantTrajectory)
	}
	if got := wire.GetCascadeId(); got != wantCascade {
		t.Fatalf("cascade = %q, want frozen %q", got, wantCascade)
	}
	// 学习表无登记——主动路径没吃过上游拒绝。
	if adapter.attachmentDeniedRecently("stub-model", attachmentKindDocument) {
		t.Fatal("proactive demote should not register learning")
	}
}

// TestOrchestrationReactiveDemote 端到端：目录声明支持但上游 EndStream
// 实测拒收（incident 形态），pre-content reopen 降级重发——第二次
// wire 请求无 Documents、带标记文本，流正常产出让客户端无感；
// 拒收事实进学习表，同 uid 第三发请求在入口直接降级。
func TestOrchestrationReactiveDemote(t *testing.T) {
	stub := &stubUpstream{
		catalog: []*devinproto.ExaCodeiumCommonPb_ClientModelConfig{stubModelEntryDocs("stub-model", true, true)},
		chat: func(call int, req *devinproto.GetChatMessageRequest, stream *connect.ServerStream[devinproto.GetChatMessageResponse]) error {
			if call == 1 {
				// incident 原话：EndStream 尾帧送达的语义拒绝。
				return connect.NewError(connect.CodeInvalidArgument,
					errors.New(`model "stub-model" does not support file inputs (supports_documents=false)`))
			}
			return stubSend(stream, stubMeta(), stubDelta("recovered"), stubStop())
		},
	}
	srv := stubServer(t, stub, nil)
	adapter := stubAdapter(t, srv, Config{Model: "stub-model", Identity: LaneIdentity{Token: "tok"}})

	req := docRequest("summarize", base64.StdEncoding.EncodeToString([]byte("doc body")), "text/plain", "a.txt")
	req.Model = "stub-model"
	stream, err := adapter.Stream(context.Background(), req)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := stubDeltas(t, stubDrain(t, stream)); got != "recovered" {
		t.Fatalf("deltas = %q", got)
	}
	if stub.chatCalls.Load() != 2 {
		t.Fatalf("chat calls = %d, want 2 (deny + demoted resend)", stub.chatCalls.Load())
	}
	// 首发带文档（目录称支持），重发降级。
	if len(stub.requests[0].GetChatMessagePrompts()[0].GetDocuments()) == 0 {
		t.Fatal("first call should carry documents (catalog declared support)")
	}
	resend := stub.requests[1]
	var combined string
	for _, prompt := range resend.GetChatMessagePrompts() {
		if len(prompt.GetDocuments()) != 0 {
			t.Fatal("resend still carries documents")
		}
		combined += prompt.GetPrompt()
	}
	if !strings.Contains(combined, "[document: a.txt") {
		t.Fatalf("resend prompt lacks demoted marker: %q", combined)
	}
	// 拒收事实已进学习表：同 uid 下一发在入口主动降级。
	if !adapter.attachmentDeniedRecently("stub-model", attachmentKindDocument) {
		t.Fatal("denial not learned")
	}
	out, n := adapter.demoteUnsupportedAttachments(context.Background(), req, "stub-model", nil)
	if n != 1 || requestHasDocuments(out) {
		t.Fatalf("learned model should demote proactively: n=%d", n)
	}
}
