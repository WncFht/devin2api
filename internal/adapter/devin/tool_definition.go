// 本文件把中间工具定义转换为 Devin 原生函数工具，并把被屏蔽的工具说明注入系统提示词。
package devin

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	devinproto "local/devinproto"

	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/llm"
)

var descriptionListItemPattern = regexp.MustCompile(`^(?:[-*+]\s+|\d+[.):]\s+|\[\d+\]\s+)(.+)$`)

// 注入段预算分级（参照 WindsurfAPI 的 full→compact→skinny 阶梯）：
// 软顶以内放行首个够小的形态；最省的 skinny 也过硬顶时报 400——
// 病态工具量下的无界注入会把 prompt 顶到上游体量上限，错误尽早显式。
const (
	toolPreambleSoftBytes = 24000
	toolPreambleHardBytes = 48000
	// toolDescriptionCompactRunes 是 compact 档每条说明的字符预算：
	// 截尾保留开头——「是什么/何时用」的导引信号都在前段。
	toolDescriptionCompactRunes = 400
)

// toolSectionEntry 是注入段的一条工具说明：name 是工具名，description
// 是 formatToolDescription 归一后的全文。
type toolSectionEntry struct{ name, description string }

// withToolDescriptions 把非空工具说明追加到 Devin system prompt，供模型理解
// 原生工具用途。full 档超软顶先降 compact（逐条截断）、再降 skinny（纯名
// 清单——工具名是最后保住的语义信号）；skinny 仍过硬顶返回错误。
func withToolDescriptions(systemPrompt string, tools []llm.ToolDefinition) (string, error) {
	var entries []toolSectionEntry
	for _, tool := range tools {
		description := strings.TrimSpace(tool.Description)
		if description == "" {
			continue
		}
		entries = append(entries, toolSectionEntry{tool.Name, formatToolDescription(description)})
	}
	if len(entries) == 0 {
		return systemPrompt, nil
	}
	section := renderToolSection(entries, 0)
	if len(section) > toolPreambleSoftBytes {
		if compact := renderToolSection(entries, toolDescriptionCompactRunes); len(compact) <= toolPreambleSoftBytes {
			section = compact
		} else {
			section = renderToolSection(entries, -1)
		}
	}
	if len(section) > toolPreambleHardBytes {
		return "", &llm.Failure{
			Code: "invalid_argument",
			Message: fmt.Sprintf("tool_preamble_too_large: tool list needs %d bytes even as a bare name list (limit %d); reduce the number of tools",
				len(section), toolPreambleHardBytes),
		}
	}
	trimmedPrompt := strings.TrimRight(systemPrompt, "\r\n")
	if strings.TrimSpace(trimmedPrompt) == "" {
		return section, nil
	}
	return trimmedPrompt + "\n\n" + section, nil
}

// renderToolSection 按档渲染注入段：truncate 为 0 是 full（说明全文）、
// 正值是 compact（每条说明截到该字符数）、-1 是 skinny（纯名清单）。
func renderToolSection(entries []toolSectionEntry, truncate int) string {
	var section strings.Builder
	if truncate < 0 {
		section.WriteString("# available tools:")
		for _, item := range entries {
			section.WriteString(" ")
			section.WriteString(item.name)
		}
		return section.String()
	}
	section.WriteString("# tools descriptions")
	for _, item := range entries {
		description := item.description
		if truncate > 0 {
			description = truncateRunes(description, truncate)
		}
		section.WriteString("\n<tool name=\"")
		section.WriteString(escapeXMLAttribute(item.name))
		section.WriteString("\">\n")
		section.WriteString(escapeXMLText(description))
		section.WriteString("\n</tool>")
	}
	return section.String()
}

// truncateRunes 按字符数截断并加省略号；优先落在词边界（不回头超过
// 四分之一预算，防止长空白前缀把内容截没）。
func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	cut := limit
	for cut > limit*3/4 && !unicode.IsSpace(runes[cut]) {
		cut--
	}
	return strings.TrimSpace(string(runes[:cut])) + "…"
}

// formatToolDescription 把自然语言句子改为有序条目，并保留代码块和 JSON 示例的原有结构。
func formatToolDescription(description string) string {
	description = strings.ReplaceAll(description, "\r\n", "\n")
	description = strings.ReplaceAll(description, "\r", "\n")
	lines := strings.Split(description, "\n")
	output := make([]string, 0, len(lines))
	prose := make([]string, 0, len(lines))
	itemNumber := 1
	inCodeFence := false

	appendSentences := func(value string) {
		for _, sentence := range splitDescriptionSentences(value) {
			output = append(output, fmt.Sprintf("%d. %s", itemNumber, sentence))
			itemNumber++
		}
	}
	appendBlankLine := func() {
		if len(output) > 0 && output[len(output)-1] != "" {
			output = append(output, "")
		}
	}
	flushProse := func() {
		if len(prose) == 0 {
			return
		}
		paragraph := strings.TrimSpace(strings.Join(prose, "\n"))
		prose = prose[:0]
		if paragraph == "" {
			return
		}
		if json.Valid([]byte(paragraph)) {
			output = append(output, paragraph)
			return
		}
		appendSentences(paragraph)
	}

	for _, line := range lines {
		trimmedLine := strings.TrimSpace(line)
		if strings.HasPrefix(trimmedLine, "```") || strings.HasPrefix(trimmedLine, "~~~") {
			flushProse()
			output = append(output, line)
			inCodeFence = !inCodeFence
			continue
		}
		if inCodeFence {
			output = append(output, line)
			continue
		}
		if trimmedLine == "" {
			flushProse()
			appendBlankLine()
			continue
		}
		if listItem := descriptionListItemPattern.FindStringSubmatch(trimmedLine); listItem != nil {
			flushProse()
			appendSentences(strings.TrimSpace(listItem[1]))
			continue
		}
		prose = append(prose, line)
	}
	flushProse()
	return strings.TrimSpace(strings.Join(output, "\n"))
}

// splitDescriptionSentences 按句子边界切分段落：句读终止符后须跟
// 空白才算边界（e.g./i.e. 等缩写误切由 endsWithAbbreviation 兜住），
// 中文全角标点无空格尾随即视作边界。
func splitDescriptionSentences(paragraph string) []string {
	sentences := make([]string, 0, 1)
	start := 0
	for offset := 0; offset < len(paragraph); {
		character, size := utf8.DecodeRuneInString(paragraph[offset:])
		end := offset + size
		if isSentenceTerminator(character) {
			next := end
			for next < len(paragraph) {
				nextCharacter, nextSize := utf8.DecodeRuneInString(paragraph[next:])
				if !unicode.IsSpace(nextCharacter) {
					break
				}
				next += nextSize
			}
			hasSentenceBoundary := next > end || character == '。' || character == '！' || character == '？'
			if hasSentenceBoundary && next < len(paragraph) && !endsWithAbbreviation(paragraph[start:end]) {
				sentences = append(sentences, strings.TrimSpace(paragraph[start:end]))
				start = next
				offset = next
				continue
			}
		}
		offset = end
	}
	if remaining := strings.TrimSpace(paragraph[start:]); remaining != "" {
		sentences = append(sentences, remaining)
	}
	return sentences
}

// isSentenceTerminator 判定句读终止符（中英两套）。
func isSentenceTerminator(character rune) bool {
	switch character {
	case '.', '!', '?', '。', '！', '？':
		return true
	default:
		return false
	}
}

// endsWithAbbreviation 判定片段尾词是否为不结束句子的常见缩写
// （e.g./i.e./etc./vs./称谓），防止终止符规则把缩写句点当句界。
func endsWithAbbreviation(fragment string) bool {
	fields := strings.Fields(strings.ToLower(fragment))
	if len(fields) == 0 {
		return false
	}
	word := strings.Trim(fields[len(fields)-1], "\"'()[]{}")
	switch word {
	case "e.g.", "i.e.", "etc.", "vs.", "mr.", "mrs.", "dr.", "prof.", "no.":
		return true
	default:
		return false
	}
}

// escapeXMLAttribute 转义工具名/参数名进 XML 属性值的特殊字符。
func escapeXMLAttribute(value string) string {
	var escaped strings.Builder
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

// escapeXMLText 仅处理会破坏 XML 文本边界的字符，保留代码示例中的普通引号。
func escapeXMLText(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

// toolDefinitionCache 以 (name, schema) 缓存 convertToolDefinition 的产物：
// 同一客户端的工具集逐请求原样重发，strip+normalize 双程是纯重复劳动。
// 命中返回同一 proto 指针——调用方只 marshal 不修改，共享安全。
// 条目数超上限时整体清空重建，避免无界增长。
var toolDefinitionCache = struct {
	sync.Mutex
	items map[string]*devinproto.ExaChatPb_ChatToolDefinition
}{items: make(map[string]*devinproto.ExaChatPb_ChatToolDefinition)}

// convertToolDefinition 保留工具身份和 JSON Schema 约束，仅移除自然语言注释。
// 工具名的字符集校验在 llm.ToolDefinition.Validate 入口完成（上游实测只
// 接受 [A-Za-z0-9_-]，a.b / mcp::x / CJK 全被 invalid_argument 模糊拒绝），
// 到达这里的名字必然合法——历史回放里的非法名上游反而照收
// （probe edge history-tool-name 实测），字符集门槛只立在声明上。
// 不做静默改名——改写会让客户端历史回灌的 tool_call 名对不上，也毁掉
// 名字本身的语义信号。
// Name/Description 均发原名——真实描述不走 description 字段（来历见下），
// 而是经 withToolDescriptions 并入 system prompt。
//
// description 字段发名不发真描述的考证：抓包
// outputs/exa.api_server_pb.ApiServerService/GetChatMessage/{01..07}/request.txt
// 证明真实 CLI（chisel 3000.2.17）在 tools[].description 发完整自然语言
// 描述（23 个工具全带），我们的掏空形态是有意偏离——schema 内的自然语言
// 注解已实证触发上游工具分类（isNaturalLanguageAnnotation），description
// 字段喂养的是同一条上游工具认知通道；把真描述转投 system prompt 是在
// 保留语义信息的同时避开该通道的指纹/分类面。属指纹对抗遗留决策：上游
// 从未被实证拒绝真描述，若要恢复真描述应先跑探针验证再改这里。
func convertToolDefinition(tool llm.ToolDefinition) (*devinproto.ExaChatPb_ChatToolDefinition, error) {
	// 缓存键覆盖全部影响 wire 形态的字段：透传位不同的同名同 schema
	// 工具不能共享条目。
	cacheKey := tool.Name + "\x00" + string(tool.InputSchema) + "\x00" +
		strconv.FormatBool(tool.Strict) + "\x00" + strconv.FormatBool(tool.ReadOnlyHint) + "\x00" +
		tool.ServerName + "\x00" + strings.Join(tool.AttributionFieldNames, "\x01")
	toolDefinitionCache.Lock()
	cached := toolDefinitionCache.items[cacheKey]
	toolDefinitionCache.Unlock()
	if cached != nil {
		return cached, nil
	}
	schema, err := stripSchemaAnnotations(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("sanitize Devin tool %q schema: %w", tool.Name, err)
	}
	schema, err = normalizeSchema(schema)
	if err != nil {
		return nil, fmt.Errorf("normalize Devin tool %q schema: %w", tool.Name, err)
	}
	converted := &devinproto.ExaChatPb_ChatToolDefinition{
		Name:             proto.String(tool.Name),
		Description:      proto.String(tool.Name),
		JsonSchemaString: proto.String(string(schema)),
	}
	// 可选透传位（2026-09-15 字段二分实测上游全收）；false/空即缺省不
	// 发，wire 保持最小。is_custom_tool/computer_use_config 同批实测被
	// 拒，永远不透传。
	if tool.Strict {
		converted.Strict = proto.Bool(true)
	}
	if tool.ReadOnlyHint {
		converted.ReadOnlyHint = proto.Bool(true)
	}
	if tool.ServerName != "" {
		converted.ServerName = proto.String(tool.ServerName)
	}
	if len(tool.AttributionFieldNames) > 0 {
		converted.AttributionFieldNames = tool.AttributionFieldNames
	}
	toolDefinitionCache.Lock()
	if len(toolDefinitionCache.items) >= 512 {
		clear(toolDefinitionCache.items)
	}
	toolDefinitionCache.items[cacheKey] = converted
	toolDefinitionCache.Unlock()
	return converted, nil
}

// stripSchemaAnnotations 剥掉 input schema 里上游不接受的注解键
// （见 stripSchemaValueAnnotations）；非法 JSON 原样报错。
func stripSchemaAnnotations(schema json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	cleaned := stripSchemaValueAnnotations(value, false)
	encoded, err := json.Marshal(cleaned)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// stripSchemaValueAnnotations 递归剥注解：propertyNames 标记当前
// 处于 properties 键名层级——属性名本身是要保留的键而非注解。
func stripSchemaValueAnnotations(value any, propertyNames bool) any {
	switch typed := value.(type) {
	case []any:
		for index, item := range typed {
			typed[index] = stripSchemaValueAnnotations(item, false)
		}
		return typed
	case map[string]any:
		cleaned := make(map[string]any, len(typed))
		for key, child := range typed {
			if isNaturalLanguageAnnotation(key) && !propertyNames {
				continue
			}
			if isSchemaLiteral(key) && !propertyNames {
				cleaned[key] = child
				continue
			}
			cleaned[key] = stripSchemaValueAnnotations(child, key == "properties")
		}
		return cleaned
	default:
		return value
	}
}

// isSchemaLiteral 标识内容属于业务值而非可递归清理的 Schema 定义。
func isSchemaLiteral(key string) bool {
	switch key {
	case "const", "default", "enum", "example", "examples":
		return true
	default:
		return false
	}
}

// isNaturalLanguageAnnotation 标识已确认会触发 Devin 上游工具分类的 Schema 元数据。
func isNaturalLanguageAnnotation(key string) bool {
	switch key {
	case "description", "title", "$comment":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(key), "x-")
	}
}

// normalizeSchema 消除上游确定性拒绝的两种 schema 形态（实测）：
//  1. 本地 $ref（"#/$defs/x" 等 JSON-pointer）inline 展开，顶层
//     $defs/definitions/$schema 一并剥掉；
//  2. 顶层没有任何 schema 关键字、值全为对象的「裸属性 map」包一层
//     {"type":"object","properties":…}。
//
// 循环或解不开的引用丢掉 $ref 键、保留同层其余约束（等价 any），比整请求
// 打回上游拿模糊 invalid_argument 更可排障。其余形态上游全容忍，不做改写。
func normalizeSchema(schema json.RawMessage) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	normalized := normalizeSchemaValue(value, value, map[string]bool{}, 0)
	if object, ok := normalized.(map[string]any); ok {
		delete(object, "$defs")
		delete(object, "definitions")
		delete(object, "$schema")
		if isBarePropertyMap(object) {
			normalized = map[string]any{"type": "object", "properties": object}
		}
	}
	return json.Marshal(normalized)
}

// maxSchemaRefDepth 限制 $ref 展开深度，病态嵌套 schema 不至于无限膨胀。
const maxSchemaRefDepth = 32

// normalizeSchemaValue 递归展开 $ref 并归一 schema 结构：root 是
// 解析引用用的根文档，resolving 检测循环引用，depth 封顶防止病态
// 嵌套无限膨胀。
func normalizeSchemaValue(value any, root any, resolving map[string]bool, depth int) any {
	if depth > maxSchemaRefDepth {
		return value
	}
	switch typed := value.(type) {
	case []any:
		for index, item := range typed {
			typed[index] = normalizeSchemaValue(item, root, resolving, depth+1)
		}
		return typed
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#") {
			if target, found := resolveLocalRef(root, ref); found && !resolving[ref] {
				resolving[ref] = true
				resolved := normalizeSchemaValue(target, root, resolving, depth+1)
				delete(resolving, ref)
				delete(typed, "$ref")
				// $ref 与兄弟键并存时合并：兄弟键覆盖被引用方的同名字段。
				if resolvedObject, ok := resolved.(map[string]any); ok {
					for key, child := range resolvedObject {
						if _, exists := typed[key]; !exists {
							typed[key] = child
						}
					}
				}
			} else {
				// 外部 URL、解不开的路径或循环引用：丢 $ref 保其余键。
				delete(typed, "$ref")
			}
		}
		for key, child := range typed {
			// 业务值字面量里的 map 不是 schema，跳过防止误展开其中的 $ref 键。
			if isSchemaLiteral(key) {
				continue
			}
			typed[key] = normalizeSchemaValue(child, root, resolving, depth+1)
		}
		return typed
	default:
		return value
	}
}

// resolveLocalRef 解析 "#/a/b" 形态的本地 JSON-pointer，处理 ~0/~1 转义。
func resolveLocalRef(root any, ref string) (any, bool) {
	if ref == "#" {
		return root, true
	}
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	current := root
	for _, segment := range strings.Split(ref[2:], "/") {
		segment = strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		if current, ok = object[segment]; !ok {
			return nil, false
		}
	}
	return current, true
}

// schemaKeywords 是判断「该 map 是不是 schema」的关键字集合；
// 判定只需要存在性，不要求穷尽 JSON Schema 全部关键字。
var schemaKeywords = map[string]bool{
	"type": true, "properties": true, "items": true, "required": true,
	"additionalProperties": true, "allOf": true, "anyOf": true, "oneOf": true,
	"not": true, "enum": true, "const": true, "format": true, "pattern": true,
	"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
	"exclusiveMinimum": true, "exclusiveMaximum": true, "multipleOf": true,
	"minItems": true, "maxItems": true, "uniqueItems": true, "contains": true,
	"minProperties": true, "maxProperties": true, "patternProperties": true,
	"propertyNames": true, "dependentRequired": true, "dependentSchemas": true,
	"prefixItems": true, "if": true, "then": true, "else": true,
	"readOnly": true, "writeOnly": true, "deprecated": true,
	"description": true, "title": true, "default": true, "examples": true,
}

// isBarePropertyMap 判定对象是否是上游会拒绝的「裸属性 map」：
// 没有 schema 关键字、没有 $/x- 前缀键，且每个值都是对象（即属性子 schema）。
func isBarePropertyMap(object map[string]any) bool {
	if len(object) == 0 {
		return false
	}
	for key, child := range object {
		if schemaKeywords[key] || strings.HasPrefix(key, "$") || strings.HasPrefix(strings.ToLower(key), "x-") {
			return false
		}
		if _, ok := child.(map[string]any); !ok {
			return false
		}
	}
	return true
}
