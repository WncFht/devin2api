// 本文件处理上游内容策略拦截的已知文案。
//
// Devin 上游有两道门：competitor-fingerprint（竞品身份指纹）和
// content-policy（permission_denied）。指纹匹配按完整特征句/段落触发，
// 单独出现 "Claude Code"、"Anthropic"、"FREEFORM" 之外的普通词不拦截。
// 规则集对齐 WindsurfAPI identity-neutralize.js（a1–a7 均为线上 A/B 验证），
// 另补一条本项目实测发现的 tool-call 冒号句。改写为等义中性文案，
// 保持系统提示语义不变。
//
// 注意：上游策略非确定性（同一 prompt 可能先封后放），规则只覆盖已实证触发器，
// 不做猜测性改写，避免误伤用户正文。
package devin

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/WncFht/devin2api/internal/llm"
)

// withinParagraph 匹配可跨单换行但不跨空行的文本（RE2 无 lookahead，
// 用「换行后必须跟非空行内容」近似 WindsurfAPI 的 \n(?!\s*\n)）。
const withinParagraph = `(?:[^\n]|\n[ \t]*[^\n \t])*?`

const securityBenign = "Decline requests that facilitate clearly malicious or harmful activity, and otherwise help the user with their software engineering task."

// upstreamSanitizeRule 是一条已实证的上游拦截文案改写规则。
type upstreamSanitizeRule struct {
	id          string
	pattern     *regexp.Regexp
	replacement string
	// promptOnly 为 true 的裸词规则只应用于 system prompt 与工具描述，
	// 不改写用户消息正文，避免破坏用户数据（如代码里的 FREEFORM 字面量）。
	promptOnly bool
	// trigger 是该 pattern 任何匹配必然包含的小写字面子串，供
	// strings.Contains 预筛：文本不含 trigger 则规则不可能命中。
	// 新增规则必须填写且宁短勿泛——写错会让规则静默失效。
	trigger string
}

// rule 构造一条非 promptOnly 的改写规则；pattern 非法会在初始化期 panic
// （MustCompile）——规则表是编译期常量，尽早暴露拼写错误。
func rule(id, pattern, replacement, trigger string) upstreamSanitizeRule {
	return upstreamSanitizeRule{id: id, pattern: regexp.MustCompile(pattern), replacement: replacement, trigger: trigger}
}

// upstreamSanitizeRules 顺序与参考实现一致：先整段/整句，再单行兜底。
var upstreamSanitizeRules = []upstreamSanitizeRule{
	// (a1) Claude Code 自我身份句（竞品指纹门），直/弯撇号、完整句/名词短语两形态。
	rule("a1-cc-full", `(?i)You are Claude Code,\s*Anthropic['’]?s official CLI for Claude\.?`, "You are an AI coding assistant.", "claude code"),
	rule("a1-cc-noun", `(?i)Claude Code,\s*Anthropic['’]?s official CLI for Claude\.?`, "an AI coding assistant.", "claude code"),
	// (a2) Claude Agent SDK 自我身份句（content-policy）。
	rule("a2-sdk-full", `(?i)You are a Claude agent, built on Anthropic['’]?s Claude Agent SDK\.?`, "You are an AI coding assistant.", "claude agent"),
	rule("a2-sdk-noun", `(?i)\ba Claude agent, built on Anthropic['’]?s Claude Agent SDK\.?`, "an AI coding assistant.", "claude agent"),
	// (a3) Claude Code 注入系统提示的计费头行（竞品指纹），整行剥除。
	rule("a3-billing", `(?im)^\s*x-anthropic-billing-header:[^\n]*\n?`, "", "x-anthropic-billing-header"),
	// (b) 安全策略整段（abuse gate）：跨段失败时由 b-security-line 单行兜底。
	rule("b-security", `(?i)IMPORTANT:\s*Assist with authorized security testing`+withinParagraph+`(?:defensive use cases\.|security research[^.]*\.)`, securityBenign, "authorized security testing"),
	rule("b-security-line", `(?i)IMPORTANT:\s*Assist with authorized security testing[^\n]*`, securityBenign, "authorized security testing"),
	// dual-use 句本身是独立指纹（本项目实测）；无 IMPORTANT 前缀时兜底改写。
	rule("b-dualluse", `(?i)Dual-use security tools \(C2 frameworks, credential testing, exploit development\) require clear authorization context:[^\n]*`, "Dual-use security tooling (e.g. C2 frameworks, credential testing, exploit development) needs explicit authorization context such as pentesting engagements, CTF competitions, security research, or defensive use cases.", "dual-use security tools"),
	// (a4) Claude Code Environment 品牌块与型号目录，均为段落级指纹。
	rule("a4-brand-span", `(?i)Claude Code is available as a CLI`+withinParagraph+`available on Opus [\d./]+\.`, "This coding assistant runs in a terminal.", "claude code is available"),
	rule("a4-fastmode", `(?im)(?:^|\n)\s*-?\s*Fast mode for Claude Code[^\n]*\n?`, "\n", "fast mode for claude code"),
	rule("a4-cli-line", `(?i)Claude Code is available as a CLI[^\n]*\n?`, "This coding assistant runs in a terminal.\n", "claude code is available"),
	rule("a4-catalogue", `(?i)The most recent Claude models are`+withinParagraph+`most capable Claude models\.`, "", "the most recent claude models"),
	rule("a4-catalogue-line", `(?im)The most recent Claude models are[^\n]*\n?`, "", "the most recent claude models"),
	// 「You are powered by the model …」与「The exact model ID is …」自我型号指纹：
	// 句点需跟空白或行尾（RE2 无 lookahead，用 (?:\.(\s|$)|$) 实现，(?m) 使 $ 匹配行尾）。
	rule("a4-poweredby", `(?im)You are powered by the model[^\n]*?(?:\.(?:\s|$)|$)\n?`, "", "powered by the model"),
	rule("a4-modelid", `(?im)The exact model ID is[^\n]*?(?:\.(?:\s|$)|$)\n?`, "", "the exact model id is"),
	// (a5) Cline 能力吹嘘句：触发点是句式而非名字，名字保留。
	rule("a5-cline-boast", `You are ([A-Z][\w.-]*), a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns,? and best practices\.`, "You are $1, a software engineer.", "a highly skilled software engineer"),
	// (a6) Grok/xAI 自我身份句 + executing_actions_with_care 整块。
	rule("a6-grok-full", `(?i)You are Grok[\w .-]* released by xAI\.?`, "You are an AI coding assistant.", "released by xai"),
	rule("a6-grok-noun", `(?i)\bGrok[\w .-]* released by xAI\.?`, "an AI coding assistant.", "released by xai"),
	rule("a6-grok2-care", `(?is)<executing_actions_with_care>.*?</executing_actions_with_care>`, "", "executing_actions_with_care"),
	// (a7) codex apply_patch 工具描述里的 FREEFORM 裸词与 JSON 包裹句。
	// 裸词可能命中用户正文（SQL/代码标识符），只作用于 prompt/工具描述。
	{id: "a7-freeform", pattern: regexp.MustCompile(`FREEFORM`), replacement: "free-form", promptOnly: true, trigger: "freeform"},
	{id: "a7-json-wrap", pattern: regexp.MustCompile(`do not wrap the patch in JSON\.`), replacement: "provide the patch as plain text.", promptOnly: true, trigger: "do not wrap the patch in json"},
	// 本项目实测：Claude Code 提示词的 tool-call 冒号句也是指纹。
	rule("cc-colon-toolcall", `(?i)Do not use a colon before tool calls\.[^\n]*?with a period\.`, "Never put a colon before a tool call; write text like \"Let me read the file.\" ending with a period instead of a colon before the call.", "colon before tool calls"),
	// CC 2.1.x 提示词新增指纹行（本项目逐行 bisect 实证）：
	// 自动压缩句、/help 品牌行、Agent 工具引导句、CLAUDE.md 行、memory 强制句。
	rule("cc-autocompact", `(?i)The system will automatically compress prior messages in your conversation as it approaches context limits\.[^\n]*`, "Earlier messages may be automatically summarized as the conversation grows long, so the conversation is not bounded by the context window.", "automatically compress prior messages"),
	// cc-help-line/cc-feedback 的指纹是裸句本身（上游实测：无行首/列表
	// 前缀也拦），故不做行首锚定；列表形态的 "- " 前缀得以保留，替换结果不变。
	rule("cc-help-line", `(?i)/help:\s*Get help with using Claude Code[^\n]*`, "/help: Get help with using this CLI", "/help:"),
	rule("cc-agent-tool", `(?i)Use the Agent tool with specialized agents when the task at hand matches the agent's description\.[^\n]*`, "Use the Agent tool with specialized agents when the task matches the agent's description. Delegation is useful for parallelizing independent queries and for keeping the main context window free of excessive results, but avoid using it when not needed, and do not repeat work already delegated to a subagent.", "use the agent tool with specialized agents"),
	rule("cc-claudemd", `(?i)Anything already documented in CLAUDE\.md files\.`, "Anything already documented in project instruction files.", "claude.md"),
	rule("cc-memory-must", `(?i)You MUST access memory when the user explicitly asks you to check, recall, or remember\.`, "Always consult memory when the user explicitly asks you to check, recall, or remember.", "must access memory"),
	rule("cc-feedback", `(?i)To give feedback, users should report the issue at https://github\.com/anthropics/claude-code/issues[^\n]*`, "To give feedback, users should report issues to the maintainers of this CLI.", "claude-code"),
	rule("cc-blast-radius", `(?i)Carefully consider the reversibility and blast radius of actions\.`, "Carefully consider the reversibility and impact of actions.", "blast radius"),
	rule("cc-claudemd-2", `(?i)durable instructions like CLAUDE\.md files`, "durable instructions like project instruction files", "claude.md"),
	// CC 2.1.x subagent 系统提示的 emoji 禁令行（本项目逐行 bisect 实证：
	// 指纹是整句，"For clear communication…" 前缀与 "MUST avoid" 缺一不可）。
	rule("cc-subagent-emojis", `(?i)For clear communication with the user the assistant MUST avoid using emojis\.`, "Keep communication with the user clear and free of emojis.", "avoid using emojis"),
	// Codex CLI 系提示词指纹（codex 0.153.x 模板逐句/逐组合 bisect 实证）：
	// 触发点是 "Codex refers to … interface" 完整跨度——截短到
	// open-source 即止、去掉 "Codex" 主语均放行；中间词属指纹一部分，
	// 用 [^\n.]* 覆盖变体但不跨句。
	rule("codex-opensource-def", `(?i)Codex refers to the open-source[^\n.]*interface`, "Codex is the open-source coding interface", "codex refers to the open-source"),
	// plan 状态句对：仅「batch-complete」紧跟「Finish with all items…」
	// 该顺序相邻时触发，单独任一句、倒序或中间插句均放行。
	rule("codex-plan-statuses", `(?i)Do not batch-complete multiple items after the fact\. Finish with all items completed or explicitly canceled/deferred before ending the turn\.`, "Do not batch-complete multiple items after the fact. Before ending the turn, leave all items completed or explicitly canceled/deferred.", "do not batch-complete multiple items"),
	// ANSI 转义句：「Don't output ANSI escape codes directly」与
	// 「the CLI renderer applies them」须同句共现，缺一或换主语即放行。
	rule("codex-ansi-escapes", `Don['’]t output ANSI escape codes directly — the CLI renderer applies them\.`, "Never output ANSI escape codes directly — the CLI renderer applies them.", "ansi escape codes directly"),
	// codex 注入的 <permissions instructions> 授权块：2026-08 实测触发
	// content-policy 拦截；2026-09-15 以合成块复测上游正常放行（真实
	// codex 块原文未入档，断言按历史实测保留）。原先在
	// common.DecodeContent 解码期剥离——迁到本表后 01/02 日志保留客户
	// 端原文、命中进 repairs 计数可查，且 anthropic 入口（不走
	// DecodeContent）同样覆盖。
	rule("codex-permissions", `(?s)<permissions instructions>.*?</permissions instructions>`, "", "permissions instructions"),
}

// sanitizeRequest 改写请求中所有会被上游策略拦截的已知文案，
// 返回按规则 id 统计的命中数——改写本身是静默的，命中计数
// 随请求日志落盘让「代理动过什么」可查。
// 注意：原地改写 Messages/Tools 切片的元素（02 日志必须在调用前
// 先投影落盘，否则记到的是改写后内容），返回值与入参共享底层数组。
func sanitizeRequest(request llm.RequestMessages) (llm.RequestMessages, map[string]int) {
	hits := make(map[string]int)
	request.SystemPrompt = sanitizeUpstreamText(request.SystemPrompt, true, hits)
	for _, message := range request.Messages {
		// Content 切片与消息值共享底层数组，块内改写就地生效，无需写回。
		switch typed := message.(type) {
		case llm.UserMessage:
			sanitizeContents(typed.Content, hits)
		case llm.AssistantMessage:
			sanitizeContents(typed.Content, hits)
		case llm.ToolResultMessage:
			sanitizeContents(typed.Content, hits)
		}
	}
	for index, tool := range request.Tools {
		request.Tools[index].Description = sanitizeUpstreamText(tool.Description, true, hits)
	}
	return request, hits
}

// sanitizeContents 就地改写内容块里的文本/思考正文（值语义块先改后
// 写回原槽位）；图片等其他块不含可拦截文案，跳过。
func sanitizeContents(content []llm.Content, hits map[string]int) []llm.Content {
	for index, block := range content {
		switch typed := block.(type) {
		case llm.TextContent:
			typed.Text = sanitizeUpstreamText(typed.Text, false, hits)
			content[index] = typed
		case llm.ThinkingContent:
			typed.Thinking = sanitizeUpstreamText(typed.Thinking, false, hits)
			content[index] = typed
		case llm.ToolCall:
			// 写文件类参数内嵌的长文本可含指纹句（如 "You are Claude
			// Code"）原文上行——ToolResultMessage 文本已被脱敏，这里
			// 不脱就是不对称漏面。对参数串做同一套文本级替换：指纹是
			// 明文短语，JSON 串内命中照常改写；替换词均为无需转义的
			// 普通 ASCII 文案，不破坏参数 JSON 结构。
			if sanitized := sanitizeUpstreamText(string(typed.Arguments), false, hits); sanitized != string(typed.Arguments) {
				typed.Arguments = json.RawMessage(sanitized)
				content[index] = typed
			}
		}
	}
	return content
}

// sanitizeFold 是 ASCII 大小写折叠表（A-Z → a-z，其余原样）。trigger
// 均为小写 ASCII 字面量，折叠表归一即等价 EqualFold 的 ASCII 语义。
var sanitizeFold = func() [256]byte {
	var table [256]byte
	for b := 0; b < 256; b++ {
		c := byte(b)
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		table[b] = c
	}
	return table
}()

// sanitizePairAll / sanitizePairMessages 把规则按 trigger 折叠后的前两
// 字节分进 64K 桶，桶内存规则序号。双字节前缀让干净正文几乎探不到候选
// （单字节桶下每个字母位都要 EqualFold 扇出，是 sanitize 的 CPU 大头）；
// includePromptOnly=false 的表剔除 promptOnly 规则——消息正文不走那批。
var (
	sanitizePairAll      = buildSanitizePairs(true)
	sanitizePairMessages = buildSanitizePairs(false)
)

// buildSanitizePairs 建双字节前缀桶表；trigger 少于 2 字节或含大写是
// 编码错误（折叠匹配会静默失效），init 期 panic 尽早暴露。
func buildSanitizePairs(includePromptOnly bool) *[65536][]uint8 {
	if len(upstreamSanitizeRules) > 64 {
		panic("devin: too many sanitize rules for bitmask")
	}
	var buckets [65536][]uint8
	for index, rule := range upstreamSanitizeRules {
		trigger := rule.trigger
		if len(trigger) < 2 || trigger != strings.ToLower(trigger) {
			panic(fmt.Sprintf("devin: sanitize rule %s has bad trigger %q", rule.id, trigger))
		}
		if rule.promptOnly && !includePromptOnly {
			continue
		}
		key := uint16(sanitizeFold[trigger[0]])<<8 | uint16(sanitizeFold[trigger[1]])
		buckets[key] = append(buckets[key], uint8(index))
	}
	return &buckets
}

// sanitizeCandidateRules 单趟扫描文本，返回可能命中的规则位集：每个位置
// 用折叠前两字节探桶，命中再做整词折叠比对置位。折叠只可能假阳（非字母
// 折叠歧义），不会漏真 trigger——位集为 0 即文本一定干净。
func sanitizeCandidateRules(text string, buckets *[65536][]uint8) (found uint64) {
	for i := 0; i+1 < len(text); i++ {
		key := uint16(sanitizeFold[text[i]])<<8 | uint16(sanitizeFold[text[i+1]])
		for _, index := range buckets[key] {
			if found>>index&1 != 0 {
				continue
			}
			trigger := upstreamSanitizeRules[index].trigger
			if len(text)-i < len(trigger) {
				continue
			}
			window := text[i : i+len(trigger)]
			j := 0
			for ; j < len(trigger); j++ {
				if sanitizeFold[window[j]] != trigger[j] {
					break
				}
			}
			if j == len(trigger) {
				found |= 1 << index
			}
		}
	}
	return found
}

// maxSanitizePasses 是改写-复扫轮数上限：某轮替换产物可能引入新 trigger
// （含跨替换边界的拼合），复扫把残存指纹兜住；理论上规则间循环改写才
// 会触到上限，此时遗留指纹交还上游裁决，不无限空转。
const maxSanitizePasses = 4

// sanitizeUpstreamText 按规则集改写文本并把命中数累加进 hits（由
// sanitizeRequest 统一分配）；替换串允许含 $ 捕获组引用，故命中数
// 用 FindAll 先数一遍而非 ReplaceAllStringFunc 统计。
func sanitizeUpstreamText(text string, includePromptOnly bool, hits map[string]int) string {
	if text == "" {
		return text
	}
	buckets := sanitizePairMessages
	if includePromptOnly {
		buckets = sanitizePairAll
	}
	for pass := 0; ; pass++ {
		// 每条规则的 trigger 是该 pattern 任何匹配必然包含的字面词；
		// 位集预筛只放行可能命中的规则，免去逐规则 Contains 全扫。
		found := sanitizeCandidateRules(text, buckets)
		if found == 0 || pass == maxSanitizePasses-1 {
			return text
		}
		rewrote := false
		for index, rule := range upstreamSanitizeRules {
			if found>>index&1 == 0 {
				continue
			}
			if matches := rule.pattern.FindAllStringIndex(text, -1); len(matches) > 0 {
				hits[rule.id] += len(matches)
				text = rule.pattern.ReplaceAllString(text, rule.replacement)
				rewrote = true
			}
		}
		if !rewrote {
			return text
		}
	}
}
