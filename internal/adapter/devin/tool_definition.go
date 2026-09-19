// 本文件把中间工具定义转换为 Devin 原生函数工具，并把被屏蔽的工具说明注入系统提示词。
package devin

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"regexp"
	"sort"
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
// 软顶以内放行 full；超出先降 compact（prose 逐条截断、Parameters
// 摘要保留）、再降 skinny（纯名清单——工具名是最后保住的语义信号）；
// compact/skinny 过硬顶时报 400——病态工具量下的无界注入会把 prompt
// 顶到上游体量上限，错误尽早显式。软顶按真实客户端工具集校准：CC
// 28 工具全文+摘要实测 ~75KB（swe-2-max 窗口 262k tok，注入段在
// EPHEMERAL 缓存断点内，冷会话一次性成本）。
const (
	toolPreambleSoftBytes = 96000
	toolPreambleHardBytes = 256000
	// toolDescriptionCompactRunes 是 compact 档每条说明的字符预算：
	// 截尾保留开头——「是什么/何时用」的导引信号都在前段。
	toolDescriptionCompactRunes = 400
	// 摘要侧预算：字段描述截断线、每工具摘要行数、嵌套递归深度。
	// 上游实测（cmd/probe toolchan）模型会读注入段的字段级需求文本——
	// "Required unless `stop` is true" 这类条件必填只活在 prose 里，
	// 摘要把 schema 剥注解时杀死的信息在同一安全通道内救回。
	toolParamDescRunes   = 240
	toolParamDigestLines = 64
	toolParamDigestDepth = 3
)

// toolSectionEntry 是注入段的一条工具说明：name 是工具名，description
// 是 formatToolDescription 归一后的全文，paramsDigest 是从原始
// InputSchema 提升的字段级描述摘要（schema 注解剥离后唯一幸存通道，
// 不受 compact 截断影响）。
type toolSectionEntry struct{ name, description, paramsDigest string }

// toolSectionCache 按工具集内容哈希（hashTools，与 warm 谱系/会话种子
// 同键）缓存渲染完成的注入段：同一客户端的工具集逐请求原样重发，每
// 工具一遍 extractParamDigest 的 schema 重解析是纯重复劳动（CC 28 工具
// ~1ms/req）。name/description/schema 任何漂移都换键，旧条目自然不再
// 命中；封顶整体清空防无界。
var toolSectionCache = struct {
	sync.Mutex
	items map[string]cachedToolSection
}{items: make(map[string]cachedToolSection)}

// cachedToolSection 是缓存条目：section 是渲染结果（空串=无注入内容）；
// oversized 标记 skinny 仍过硬顶的病态输入，oversizedLen 记录当时的
// 渲染长度供重建错误消息。
type cachedToolSection struct {
	section      string
	oversized    bool
	oversizedLen int
}

// withToolDescriptions 把非空工具说明追加到 Devin system prompt，供模型理解
// 原生工具用途。注入段只依赖 tools（systemPrompt 是尾部拼接，不参与段内
// 决策），整条段按工具集内容哈希进 toolSectionCache——无 SessionKey 的
// 请求在 sessionSeed 里已付过一遍 hashTools，这里复算仍远低于重解析。
// full 档超软顶先降 compact（prose 逐条截断）、再降 skinny（纯名清单）；
// skinny 仍过硬顶返回错误。
func withToolDescriptions(systemPrompt string, tools []llm.ToolDefinition) (string, error) {
	if len(tools) == 0 {
		return systemPrompt, nil
	}
	key := hashTools(tools)
	toolSectionCache.Lock()
	cached, hit := toolSectionCache.items[key]
	toolSectionCache.Unlock()
	if !hit {
		cached = buildToolSection(tools)
		toolSectionCache.Lock()
		if len(toolSectionCache.items) >= 64 {
			clear(toolSectionCache.items)
		}
		toolSectionCache.items[key] = cached
		toolSectionCache.Unlock()
	}
	if cached.oversized {
		// Failure 每次重建不缓存指针：下游会给 event.Error 写本请求的
		// DebugRef（app/stream.go），共享指针会让调试目录名跨请求串味。
		return "", &llm.Failure{
			Code: "invalid_argument",
			Message: fmt.Sprintf("tool_preamble_too_large: tool list needs %d bytes even as a bare name list (limit %d); reduce the number of tools",
				cached.oversizedLen, toolPreambleHardBytes),
		}
	}
	if cached.section == "" {
		return systemPrompt, nil
	}
	trimmedPrompt := strings.TrimRight(systemPrompt, "\r\n")
	if strings.TrimSpace(trimmedPrompt) == "" {
		return cached.section, nil
	}
	return trimmedPrompt + "\n\n" + cached.section, nil
}

// buildToolSection 渲染注入段并套用预算降级阶梯；返回条目供缓存。
func buildToolSection(tools []llm.ToolDefinition) cachedToolSection {
	var entries []toolSectionEntry
	for _, tool := range tools {
		description := strings.TrimSpace(tool.Description)
		digest := extractParamDigest(tool.InputSchema)
		if description == "" && digest == "" {
			continue
		}
		entries = append(entries, toolSectionEntry{tool.Name, formatToolDescription(description), digest})
	}
	if len(entries) == 0 {
		return cachedToolSection{}
	}
	section := renderToolSection(entries, 0)
	if len(section) > toolPreambleSoftBytes {
		if compact := renderToolSection(entries, toolDescriptionCompactRunes); len(compact) <= toolPreambleHardBytes {
			section = compact
		} else {
			section = renderToolSection(entries, -1)
		}
	}
	if len(section) > toolPreambleHardBytes {
		return cachedToolSection{oversized: true, oversizedLen: len(section)}
	}
	return cachedToolSection{section: section}
}

// renderToolSection 按档渲染注入段：truncate 为 0 是 full（说明全文 +
// 摘要）、正值是 compact（说明截到该字符数，摘要始终完整）、-1 是
// skinny（纯名清单）。
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
		if item.paramsDigest != "" {
			section.WriteString("\n\nParameters:\n")
			section.WriteString(escapeXMLText(item.paramsDigest))
		}
		section.WriteString("\n</tool>")
	}
	return section.String()
}

// extractParamDigest 从原始 InputSchema 提取字段级描述渲染成摘要行：
// stripSchemaValueAnnotations 会把这些注解从 wire schema 剥掉，这里在
// 剥离发生前把它们搬进注入段——条件必填、取值约束这类只存在于字段
// prose 的语义由此存活。properties 嵌套递归（数组项经 items 下钻），
// 输出按字段名排序保证确定性。
func extractParamDigest(schema json.RawMessage) string {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return ""
	}
	var lines []string
	collectParamLines(root, "", 0, &lines)
	return strings.Join(lines, "\n")
}

// collectParamLines 递归收集 properties 字段的描述行：prefix 是嵌套
// 路径（"a." / "a[]."），depth 封顶防病态 schema。description 缺席时
// 退回 title；字段在同级 required[] 里标 (required)。
func collectParamLines(node any, prefix string, depth int, lines *[]string) {
	if depth > toolParamDigestDepth || len(*lines) >= toolParamDigestLines {
		return
	}
	object, ok := node.(map[string]any)
	if !ok {
		return
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		return
	}
	required := make(map[string]bool)
	if list, ok := object["required"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				required[name] = true
			}
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if len(*lines) >= toolParamDigestLines {
			return
		}
		property, ok := properties[name].(map[string]any)
		if !ok {
			continue
		}
		doc, _ := property["description"].(string)
		if doc == "" {
			doc, _ = property["title"].(string)
		}
		doc = strings.Join(strings.Fields(doc), " ")
		if doc != "" {
			var line strings.Builder
			line.WriteString("- ")
			line.WriteString(prefix)
			line.WriteString(name)
			line.WriteString(" (")
			line.WriteString(schemaTypeName(property["type"]))
			if required[name] || claimsRequired(doc) {
				line.WriteString(", required")
			}
			line.WriteString("): ")
			line.WriteString(fieldDocDigest(doc))
			*lines = append(*lines, line.String())
		}
		collectParamLines(property, prefix+name+".", depth+1, lines)
		if items, ok := property["items"].(map[string]any); ok {
			collectParamLines(items, prefix+name+"[].", depth+1, lines)
		}
	}
}

// requiredUnlessPattern 提取字段描述里的统一条件子句
// （"Required unless `stop` is true." → "`stop` is true"）。
var requiredUnlessPattern = regexp.MustCompile(`(?i)\brequired\s+unless\s+([^.;]+)`)

// conditionalRequiredFieldPattern 从统一 unless 子句里解出条件字段名：
// 只接 "`X` is true"/"\"X\" is true" 形态（实测唯一出现的极性）。
var conditionalRequiredFieldPattern = regexp.MustCompile("^[`'\"](\\w+)[`'\"]\\s+is\\s+true$")

// conditionalRequiredSpec 检测「全部必填标记字段共享同一 unless 子句、且
// 子句指向同层一个未标记字段」的条件必填模式（ScheduleWakeup 形态：
// required unless `stop` is true）。命中时返回条件字段名与被条件必填的
// 字段名清单，供 wire schema 合成 anyOf [{required:[cond]},
// {required:[fields]}]——prose 行头标记把命中率推到 ~50% 平台后不再涨，
// schema 层约束是模型原生解析的硬信号（sw-anyof 探针实证可读）。
func conditionalRequiredSpec(schema json.RawMessage) (string, []string, bool) {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return "", nil, false
	}
	object, ok := root.(map[string]any)
	if !ok {
		return "", nil, false
	}
	// 客户端已声明组合结构时不叠加猜测语义。
	if _, exists := object["anyOf"]; exists {
		return "", nil, false
	}
	if _, exists := object["oneOf"]; exists {
		return "", nil, false
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		return "", nil, false
	}
	required := make(map[string]bool)
	if list, ok := object["required"].([]any); ok {
		for _, item := range list {
			if name, ok := item.(string); ok {
				required[name] = true
			}
		}
	}
	clauses := make(map[string]bool)
	var marked []string
	for name, raw := range properties {
		property, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		doc, _ := property["description"].(string)
		if doc == "" {
			doc, _ = property["title"].(string)
		}
		doc = strings.Join(strings.Fields(doc), " ")
		if !required[name] && !claimsRequired(doc) {
			continue
		}
		clause := ""
		if m := requiredUnlessPattern.FindStringSubmatch(doc); m != nil {
			clause = strings.TrimRight(strings.TrimSpace(m[1]), ".")
		}
		clauses[clause] = true
		marked = append(marked, name)
	}
	if len(marked) == 0 || len(clauses) != 1 {
		return "", nil, false
	}
	var clause string
	for sole := range clauses {
		clause = sole
	}
	m := conditionalRequiredFieldPattern.FindStringSubmatch(clause)
	if m == nil {
		return "", nil, false
	}
	condField := m[1]
	if _, exists := properties[condField]; !exists {
		return "", nil, false
	}
	// 条件字段自身必填/被标记 → anyOf 分支恒真或递归语义不成立，不合成。
	if required[condField] {
		return "", nil, false
	}
	for _, name := range marked {
		if name == condField {
			return "", nil, false
		}
	}
	sort.Strings(marked)
	return condField, marked, true
}

// injectConditionalRequired 给 wire schema 顶层补 anyOf 条件必填
// [{required:[condField]}, {required:[marked…]}]，并把 marked 字段从顶层
// required[] 降级进分支——否则条件分支（如 stop:true）仍被顶层 required
// 强制带上全量字段，条件语义被吃掉。顶层非对象 map 或已有 anyOf/oneOf
// 时原样返回。
func injectConditionalRequired(schema json.RawMessage, condField string, marked []string) json.RawMessage {
	var object map[string]any
	if err := json.Unmarshal(schema, &object); err != nil {
		return schema
	}
	if _, exists := object["anyOf"]; exists {
		return schema
	}
	if _, exists := object["oneOf"]; exists {
		return schema
	}
	markedSet := make(map[string]bool, len(marked))
	for _, name := range marked {
		markedSet[name] = true
	}
	if list, ok := object["required"].([]any); ok {
		kept := make([]any, 0, len(list))
		for _, item := range list {
			if name, ok := item.(string); ok && markedSet[name] {
				continue
			}
			kept = append(kept, item)
		}
		if len(kept) == 0 {
			delete(object, "required")
		} else {
			object["required"] = kept
		}
	}
	object["anyOf"] = []any{
		map[string]any{"required": []string{condField}},
		map[string]any{"required": marked},
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return schema
	}
	return encoded
}

// claimsRequired 判定字段描述是否声明了必填语义——条件必填
// （"Required unless `stop` is true"）只活在 prose 里、不进 required[]，
// 在摘要行头补标 required 防止模型把它当可选字段跳过。先把否定形态
// （"not required"/"optional" 等）抹掉再匹配，避免误标。
func claimsRequired(doc string) bool {
	lower := strings.ToLower(doc)
	for _, neg := range []string{"not required", "n't required", "no longer required", "never required", "optional"} {
		lower = strings.ReplaceAll(lower, neg, "")
	}
	return strings.Contains(lower, "required") || strings.Contains(lower, "必填")
}

// fieldDocDigest 把字段描述压进单行预算：超长时头部截断，但原文里含
// "required"/"必填" 的句子若被截掉则补回末尾——条件必填约定俗成写在字段
// 描述尾部（"Required unless `stop` is true"），纯头部截断会系统性杀死它
// （e2e 实测：ScheduleWakeup 的 noop/prompt 恰好是描述最长的两个字段，
// 尾句截掉后模型即省略这两字段）。
func fieldDocDigest(doc string) string {
	if utf8.RuneCountInString(doc) <= toolParamDescRunes {
		return doc
	}
	truncated := truncateRunes(doc, toolParamDescRunes)
	last := ""
	for _, sentence := range splitDescriptionSentences(doc) {
		if strings.Contains(strings.ToLower(sentence), "required") || strings.Contains(sentence, "必填") {
			last = sentence
		}
	}
	if last == "" || strings.Contains(truncated, last) {
		return truncated
	}
	return truncated + " " + truncateRunes(last, toolParamDescRunes)
}

// schemaTypeName 把 schema type 字段渲染成短标记：字符串原样、数组
// 以 | 连接、缺省 any。
func schemaTypeName(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		names := make([]string, 0, len(typed))
		for _, item := range typed {
			if name, ok := item.(string); ok {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			return strings.Join(names, "|")
		}
	}
	return "any"
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
// droppedRefs 随条目缓存该 schema 归一化时剥掉的本地 $ref 数，命中时
// 照样计入本请求 repairs——缓存命中不等于没有发生过语义漂移。
// 条目数超上限时整体清空重建，避免无界增长。
var toolDefinitionCache = struct {
	sync.Mutex
	items map[string]cachedToolDefinition
}{items: make(map[string]cachedToolDefinition)}

// cachedToolDefinition 是缓存条目：wire 产物加投影副作用计数。
type cachedToolDefinition struct {
	definition  *devinproto.ExaChatPb_ChatToolDefinition
	droppedRefs int
}

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
// outputs/corpus/GetChatMessage/{01..07}/request.txt
// 证明真实 CLI（chisel 3000.2.17）在 tools[].description 发完整自然语言
// 描述（23 个工具全带），我们的掏空形态是有意偏离——schema 内的自然语言
// 注解已实证触发上游工具分类（isNaturalLanguageAnnotation），description
// 字段喂养的是同一条上游工具认知通道；把真描述转投 system prompt 是在
// 保留语义信息的同时避开该通道的指纹/分类面。属指纹对抗遗留决策：上游
// 从未被实证拒绝真描述，若要恢复真描述应先跑探针验证再改这里。
func convertToolDefinition(tool llm.ToolDefinition, repairs *llm.RequestRepairs) (*devinproto.ExaChatPb_ChatToolDefinition, error) {
	// 缓存键覆盖全部影响 wire 形态的字段：透传位不同的同名同 schema
	// 工具不能共享条目。
	cacheKey := tool.Name + "\x00" + string(tool.InputSchema) + "\x00" +
		strconv.FormatBool(tool.Strict) + "\x00" + strconv.FormatBool(tool.ReadOnlyHint) + "\x00" +
		tool.ServerName + "\x00" + strings.Join(tool.AttributionFieldNames, "\x01")
	toolDefinitionCache.Lock()
	cached, hit := toolDefinitionCache.items[cacheKey]
	toolDefinitionCache.Unlock()
	if hit {
		repairs.SchemaRefDropped += cached.droppedRefs
		return cached.definition, nil
	}
	schema, err := stripSchemaAnnotations(tool.InputSchema)
	if err != nil {
		return nil, fmt.Errorf("sanitize Devin tool %q schema: %w", tool.Name, err)
	}
	droppedRefs := 0
	schema, err = normalizeSchema(schema, &droppedRefs)
	if err != nil {
		return nil, fmt.Errorf("normalize Devin tool %q schema: %w", tool.Name, err)
	}
	repairs.SchemaRefDropped += droppedRefs
	// prose 统一 unless 子句命中时合成 anyOf 条件必填：strip 已把字段描述
	// 剥掉，检测必须跑在原始 schema 上，合成产物写进剥离后的 wire schema。
	if condField, marked, ok := conditionalRequiredSpec(tool.InputSchema); ok {
		schema = injectConditionalRequired(schema, condField, marked)
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
	toolDefinitionCache.items[cacheKey] = cachedToolDefinition{definition: converted, droppedRefs: droppedRefs}
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
// dropped 累计本次剥掉的 $ref 键数（含深度保险丝截断剥除），随缓存条目
// 存进 toolDefinitionCache 供 repairs 对账。
func normalizeSchema(schema json.RawMessage, dropped *int) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(schema, &value); err != nil {
		return nil, err
	}
	normalized := normalizeSchemaValue(value, value, map[string]bool{}, 0, dropped)
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
// 嵌套无限膨胀。dropped 累计全部剥除的本地 $ref 键数。
func normalizeSchemaValue(value any, root any, resolving map[string]bool, depth int, dropped *int) any {
	if depth > maxSchemaRefDepth {
		// 保险丝截断的子树原样放行会把残留 $ref 带上 wire——截断痕迹
		// 重新引入本保险丝要防的上游 invalid_argument（兄弟合并还会把
		// 截断目标的 $ref 反向注回父层）。与解不开/循环引用同语义剥键。
		stripLocalSchemaRefs(value, dropped)
		return value
	}
	switch typed := value.(type) {
	case []any:
		for index, item := range typed {
			typed[index] = normalizeSchemaValue(item, root, resolving, depth+1, dropped)
		}
		return typed
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#") {
			if target, found := resolveLocalRef(root, ref); found && !resolving[ref] {
				resolving[ref] = true
				resolved := normalizeSchemaValue(target, root, resolving, depth+1, dropped)
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
				// 本地引用解不开或构成循环：丢 $ref 保其余键。非 "#" 前缀
				// 的 $ref 被上面守卫挡在分支外，原样透传上 wire。
				delete(typed, "$ref")
				*dropped++
			}
		}
		for key, child := range typed {
			// 业务值字面量里的 map 不是 schema，跳过防止误展开其中的 $ref 键。
			if isSchemaLiteral(key) {
				continue
			}
			typed[key] = normalizeSchemaValue(child, root, resolving, depth+1, dropped)
		}
		return typed
	default:
		return value
	}
}

// stripLocalSchemaRefs 剥掉子树里全部本地 $ref（"#/…"）键：深度保险丝
// 截断后不再递归展开，残留 ref 是截断痕迹而非客户端语义。dropped 累计
// 剥除数。isSchemaLiteral 业务字面量（const/enum/default 等）不剥——
// 同名键在字面量里是数据；非本地 $ref（外部 URL）与正常深度同口径透传。
// 遍历成本以截断子树自身大小为界，不做引用展开，无膨胀风险。
func stripLocalSchemaRefs(value any, dropped *int) {
	switch typed := value.(type) {
	case map[string]any:
		if ref, ok := typed["$ref"].(string); ok && strings.HasPrefix(ref, "#") {
			delete(typed, "$ref")
			*dropped++
		}
		for key, child := range typed {
			if isSchemaLiteral(key) {
				continue
			}
			stripLocalSchemaRefs(child, dropped)
		}
	case []any:
		for _, item := range typed {
			stripLocalSchemaRefs(item, dropped)
		}
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

// schemaObjectValuedKeywords 是 JSON Schema 语法上值本身可为 map 的
// 关键字：这些键带 map 值时，「关键字用法」与「属性名恰叫该关键字」
// 两种解读都成立（{"properties":{…}} 既可读作真 schema 也可读作名为
// properties 的属性），按真 schema 保守不包裹。type/required/enum/
// description 等只收标量或数组的关键字不在此列——它们带 map 值必为
// 属性名（{"type":{"type":"string"}} 是名为 type 的属性而非非法
// type 声明）。
var schemaObjectValuedKeywords = map[string]bool{
	"properties": true, "patternProperties": true,
	"items": true, "additionalProperties": true, "contains": true,
	"not": true, "if": true, "then": true, "else": true,
	"propertyNames": true, "dependentRequired": true, "dependentSchemas": true,
	// const/default 收任意字面量，对象默认值与属性重名同样不可区分。
	"const": true, "default": true,
}

// isBarePropertyMap 判定对象是否是上游会拒绝的「裸属性 map」：
// 没有 schema 关键字、没有 $/x- 前缀键，且每个值都是对象（即属性子 schema）。
// 先判 child 形态再判关键字：map 值说明键是属性名，除非该键是一个
// 语法上就收 map 值的 schema 关键字（见 schemaObjectValuedKeywords）——
// 只收标量/数组的关键字带 map 值是属性而非声明。
func isBarePropertyMap(object map[string]any) bool {
	if len(object) == 0 {
		return false
	}
	for key, child := range object {
		if strings.HasPrefix(key, "$") || strings.HasPrefix(strings.ToLower(key), "x-") {
			return false
		}
		if _, ok := child.(map[string]any); !ok {
			return false
		}
		if schemaObjectValuedKeywords[key] {
			return false
		}
	}
	return true
}
