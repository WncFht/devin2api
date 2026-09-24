// 本文件实现「附件降级」：目标模型缺文档/视频能力时，把请求里的
// DocumentContent/VideoContent 就地改写为文本块（可抽取的.inline 正文，
// 否则占位标记），让请求在该模型上可用而非被上游 invalid_argument
// 永久打砖——语义错误经 EndStream 回送，一旦发出每个后续请求都会
// 复现同一拒绝，客户端无自愈手段。
//
// 降级分两层：Stream 入口按目录能力位 + 学习表主动降级（本文件
// demoteRequestAttachments）；流内 reopen 路径在上游实测拒收后被动
// 降级重发（attemptRunner.demoteAttachments）。两层共用同一套
// 逐块改写规则。
package devin

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giraffesyo/pdf"

	"github.com/WncFht/devin2api/internal/llm"
)

const (
	// attachmentKindDocument / attachmentKindVideo 是能力学习表与日志里
	// 标识附件种类的键值。
	attachmentKindDocument = "document"
	attachmentKindVideo    = "video"

	// demoteInlineBodyCap 是单个文档内联进文本轨的正文上限；
	// demoteInlineTotalCap 是一次请求全部内联正文的合计上限。超量
	// 只截断不拒绝——把「永久 400」换成「永久 413（prompt too long）」
	// 不算修好。
	demoteInlineBodyCap  = 32 << 10
	demoteInlineTotalCap = 128 << 10
	// demoteMaxB64Len 是愿意解码的 base64 原文上限（≈33MB 解码后），
	// 更大直接占位；demoteMaxPDFBytes 是送进 PDF 抽取器的原始字节
	// 上限——超大 PDF 不解析直接占位。
	demoteMaxB64Len   = 44 << 20
	demoteMaxPDFBytes = 8 << 20
	// demoteURLChars 是占位标记里 echo 远端地址的字符上限。
	demoteURLChars = 200
	// pdfExtractTimeout 是单份 PDF 文本抽取的硬顶：库内部按页与按
	// 操作数检查 ctx，超时即收。
	pdfExtractTimeout = 10 * time.Second
)

// demoteUnsupportedAttachments 是 Stream 入口的主动降级判定：请求含
// 文档/视频块且能力面判定该 uid 不收时，走 demoteRequestAttachments
// 改写。两维各自独立判定——模型可能收文档但拒视频。
func (adapter *Adapter) demoteUnsupportedAttachments(ctx context.Context, request llm.RequestMessages, model string, hits map[string]int) (llm.RequestMessages, int) {
	demoteDocs := requestHasDocuments(request) && adapter.attachmentUnsupported(model, attachmentKindDocument)
	demoteVideos := requestHasVideos(request) && adapter.attachmentUnsupported(model, attachmentKindVideo)
	if !demoteDocs && !demoteVideos {
		return request, 0
	}
	return demoteRequestAttachments(ctx, request, demoteDocs, demoteVideos, hits)
}

// attachmentUnsupported 判定「该 uid 不收某类附件」：目录能力位在场的
// 按位判（proto optional 缺席归并为不支持——与原先本地拒绝同口径，
// 目录刷新即自愈）；目录未覆盖该 uid 时查学习表——上游实测拒收一次
// 后，后续请求直接降级而不再每次拿真请求去试探。
func (adapter *Adapter) attachmentUnsupported(model, kind string) bool {
	var supported, known bool
	switch kind {
	case attachmentKindVideo:
		supported, known = adapter.catalogSupportsVideo(model)
	default:
		supported, known = adapter.catalogSupportsDocuments(model)
	}
	if known && !supported {
		return true
	}
	return adapter.attachmentDeniedRecently(model, kind)
}

// noteAttachmentDenied 把上游「该 uid 拒收某类附件」的 invalid_argument
// 登记进能力学习表。与 deadModels 同锁域（modelsMu）但独立键空间：
// deadModels 的查验会 404 整个请求（那是「模型不存在」的判决），附件
// 学习只摘「不收这类附件」一个维度，模型本身照常服务。
func (adapter *Adapter) noteAttachmentDenied(model string, err error) {
	kind, ok := attachmentDenialKind(err)
	if !ok || model == "" {
		return
	}
	adapter.modelsMu.Lock()
	defer adapter.modelsMu.Unlock()
	if adapter.attachmentDenied == nil {
		adapter.attachmentDenied = make(map[string]time.Time)
	}
	key := model + "|" + kind
	until := time.Now().Add(deadModelTTL)
	if _, marked := adapter.attachmentDenied[key]; !marked {
		slog.Warn("model attachment capability learned: upstream denied inputs",
			"model", model, "kind", kind, "denied_until", until.Format(time.RFC3339))
	}
	adapter.attachmentDenied[key] = until
}

// attachmentDeniedRecently 查学习表该 (uid, 附件种类) 是否在保鲜期内。
func (adapter *Adapter) attachmentDeniedRecently(model, kind string) bool {
	adapter.modelsMu.RLock()
	defer adapter.modelsMu.RUnlock()
	until, marked := adapter.attachmentDenied[model+"|"+kind]
	return marked && time.Now().Before(until)
}

// demoteRequestAttachments 把请求里的文档/视频块改写为文本块，返回新
// 请求与降级块数。消息结构级 copy-on-write：pool.openOnLane 会让同一
// 份 request 的 Messages/Content 底数组被多条 lane 共享，原位改写会把
// 本 lane 的降级决策漏给兄弟 lane——动过的消息克隆新 struct + 新
// Content 切片 + 新 Messages 切片，未动消息复用原值。
// demoteDocs/demoteVideos 分开受控：目录能力位按维度独立，上游拒收
// 哪类就只降级哪类。hits 收下合成文本的指纹改写计数（可传 nil）。
func demoteRequestAttachments(ctx context.Context, request llm.RequestMessages, demoteDocs, demoteVideos bool, hits map[string]int) (llm.RequestMessages, int) {
	if hits == nil {
		hits = map[string]int{}
	}
	budget := demoteInlineTotalCap
	demoted := 0
	var messages []llm.Message
	for index, message := range request.Messages {
		var content []llm.Content
		switch typed := message.(type) {
		case llm.UserMessage:
			content = typed.Content
		case llm.ToolResultMessage:
			content = typed.Content
		default:
			// 助手消息不携带文档/视频块（IR 校验限定），原样保留。
			if messages != nil {
				messages = append(messages, message)
			}
			continue
		}
		needs := false
		for _, block := range content {
			switch block.(type) {
			case llm.DocumentContent:
				needs = demoteDocs
			case llm.VideoContent:
				needs = demoteVideos
			}
			if needs {
				break
			}
		}
		if !needs {
			if messages != nil {
				messages = append(messages, message)
			}
			continue
		}
		if messages == nil {
			messages = make([]llm.Message, 0, len(request.Messages))
			messages = append(messages, request.Messages[:index]...)
		}
		newContent := make([]llm.Content, 0, len(content))
		for _, block := range content {
			switch typed := block.(type) {
			case llm.DocumentContent:
				if demoteDocs {
					newContent = append(newContent, demoteDocumentBlock(ctx, typed, &budget, hits))
					demoted++
					continue
				}
			case llm.VideoContent:
				if demoteVideos {
					newContent = append(newContent, demoteVideoBlock(typed, hits))
					demoted++
					continue
				}
			}
			newContent = append(newContent, block)
		}
		messages = append(messages, cloneMessageWithContent(message, newContent))
	}
	if messages == nil {
		return request, 0
	}
	request.Messages = messages
	return request, demoted
}

// cloneMessageWithContent 克隆一条消息并替换其 Content 切片：
// 值形态消息复制后其余字段（ToolCallID/IsError/TimestampMS）随结构
// 体一并带走，不回写原值。
func cloneMessageWithContent(message llm.Message, content []llm.Content) llm.Message {
	switch typed := message.(type) {
	case llm.UserMessage:
		typed.Content = content
		return typed
	case llm.ToolResultMessage:
		typed.Content = content
		return typed
	default:
		return message
	}
}

// demoteDocumentBlock 把单个文档块降级为文本：能抽取的（utf8 文本 /
// PDF 解析成功）内联正文，否则占位标记。标记带边界括号形态
// 「[document: name (mime)] … [/document]」——WindsurfAPI 实测过无标记
// 内联的附件正文会被模型当成用户指令执行，「do not infer contents」
// 是给模型的显式禁读声明。
func demoteDocumentBlock(ctx context.Context, doc llm.DocumentContent, budget *int, hits map[string]int) llm.Content {
	name := sanitizeAttachmentLabel(doc.Filename)
	if name == "" {
		name = "untitled"
	}
	mime := sanitizeAttachmentLabel(doc.MIMEType)
	label := name
	if mime != "" {
		label += " (" + mime + ")"
	}
	if doc.URL != "" {
		// 远端文档不抓取（SSRF 面）：只回显 http(s) 地址截断体，
		// 其余 scheme 连地址都不带出。
		var text string
		if strings.HasPrefix(doc.URL, "http://") || strings.HasPrefix(doc.URL, "https://") {
			text = fmt.Sprintf("\n[document: %s — remote document at %s; content not fetched — do not infer contents]\n", label, truncateUTF8(doc.URL, demoteURLChars))
		} else {
			text = fmt.Sprintf("\n[document: %s — remote document; content not fetched — do not infer contents]\n", label)
		}
		return llm.TextContent{Text: sanitizeUpstreamText(text, false, hits)}
	}
	raw, ok := decodeAttachmentData(doc.Data)
	var body string
	if ok {
		switch {
		case isPDFPayload(mime, raw):
			if len(raw) <= demoteMaxPDFBytes {
				body, _ = extractPDFText(ctx, raw)
			}
			if body == "" && utf8.Valid(raw) && !bytes.HasPrefix(raw, []byte("%PDF-")) {
				// mime 自称 pdf 但无魔数且全 utf8 = 误标文本，退回内联；
				// 真 pdf（有魔数）抽取失败走占位，不把二进制语法倒给模型。
				body = string(raw)
			}
		case utf8.Valid(raw):
			body = string(raw)
		}
	}
	if body == "" {
		size := len(raw)
		if !ok {
			size = len(doc.Data) / 4 * 3 // 解码失败时按 b64 体量估算
		}
		text := fmt.Sprintf("\n[document: %s, %d bytes — content not extracted; do not infer contents]\n", label, size)
		return llm.TextContent{Text: sanitizeUpstreamText(text, false, hits)}
	}
	room := demoteInlineBodyCap
	if *budget < room {
		room = *budget
	}
	if room <= 0 {
		// 总量预算耗尽：后续文档不再内联，占位保留知情。
		text := fmt.Sprintf("\n[document: %s, %d bytes — content not extracted; do not infer contents]\n", label, len(raw))
		return llm.TextContent{Text: sanitizeUpstreamText(text, false, hits)}
	}
	truncated := len(body) > room
	if truncated {
		body = truncateUTF8(body, room)
	}
	*budget -= len(body)
	var b strings.Builder
	b.Grow(len(body) + 80)
	b.WriteString("\n[document: ")
	b.WriteString(label)
	b.WriteString("]\n")
	b.WriteString(body)
	if truncated {
		b.WriteString("\n[document truncated]")
	}
	b.WriteString("\n[/document]\n")
	return llm.TextContent{Text: sanitizeUpstreamText(b.String(), false, hits)}
}

// demoteVideoBlock 把视频块降级为占位标记：视频体永远不进文本轨
// （抽帧/转写在网关侧没有等价物），只交代体量与来源让模型知情。
func demoteVideoBlock(video llm.VideoContent, hits map[string]int) llm.Content {
	mime := sanitizeAttachmentLabel(video.MIMEType)
	label := "video"
	if mime != "" {
		label += " (" + mime + ")"
	}
	var text string
	if video.URL != "" {
		if strings.HasPrefix(video.URL, "http://") || strings.HasPrefix(video.URL, "https://") {
			text = fmt.Sprintf("\n[%s — remote video at %s; content not fetched — do not infer contents]\n", label, truncateUTF8(video.URL, demoteURLChars))
		} else {
			text = fmt.Sprintf("\n[%s — remote video; content not fetched — do not infer contents]\n", label)
		}
	} else {
		text = fmt.Sprintf("\n[%s, %d bytes — content not extracted; do not infer contents]\n", label, len(video.Data)/4*3)
	}
	return llm.TextContent{Text: sanitizeUpstreamText(text, false, hits)}
}

// attachmentDenialKind 判定错误是否上游的附件能力拒收：EndStream 尾帧
// 送达的 invalid_argument 经 llm.Classify 归一后按文案短语识别
// （"does not support file inputs" / "does not support video inputs"，
// 实测 swe-2-max）。返回附件种类供定向降级与学习表登记。
func attachmentDenialKind(err error) (string, bool) {
	failure := llm.Classify(err)
	if failure == nil || failure.Code != "invalid_argument" {
		return "", false
	}
	switch {
	case strings.Contains(failure.Message, "does not support file inputs"):
		return attachmentKindDocument, true
	case strings.Contains(failure.Message, "does not support video inputs"):
		return attachmentKindVideo, true
	}
	return "", false
}

// sanitizeAttachmentLabel 清洗进标记方括号的用户可控字符串（文件名/
// mime）：剥控制字符与方括号防伪造闭合标签，截断防爆行。
func sanitizeAttachmentLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '[' || r == ']' {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
	return truncateUTF8(s, 100)
}

// decodeAttachmentData 把文档块的 Data 字段解成原始字节：容忍 data:
// URL 前缀与空白（resource.blob 通道绕过解码侧归一化，前缀会原样
// 漏到这里）。b64 体量超上限时不解码直接判败。
func decodeAttachmentData(data string) ([]byte, bool) {
	data = strings.TrimSpace(data)
	if strings.HasPrefix(data, "data:") {
		if _, encoded, ok := strings.Cut(data, ","); ok {
			data = encoded
		}
	}
	if len(data) == 0 || len(data) > demoteMaxB64Len {
		return nil, false
	}
	if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
		return raw, true
	}
	raw, err := base64.RawStdEncoding.DecodeString(data)
	return raw, err == nil
}

// isPDFPayload 判定文档应按 PDF 处理：声明 mime 或 %PDF- 魔数任一成立。
func isPDFPayload(mime string, raw []byte) bool {
	return mime == "application/pdf" || bytes.HasPrefix(raw, []byte("%PDF-"))
}

// extractPDFText 用 giraffesyo/pdf 抽取文档文本：ctx 传入页面级与
// 操作级的取消检查（库内建），recover 兜库自身的解析 panic——恶意
// PDF 是真实输入面，一次崩溃不能带走整个网关进程。
func extractPDFText(ctx context.Context, raw []byte) (text string, err error) {
	defer func() {
		if r := recover(); r != nil {
			text, err = "", fmt.Errorf("pdf extract panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, pdfExtractTimeout)
	defer cancel()
	doc, err := pdf.Extract(ctx, bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", err
	}
	return doc.Text(), nil
}

// truncateUTF8 按字节上限截断 UTF-8 文本，落在完整 rune 边界上。
func truncateUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	if limit < 0 {
		limit = 0
	}
	for limit > 0 && !utf8.ValidString(s[:limit]) {
		limit--
	}
	return s[:limit]
}
