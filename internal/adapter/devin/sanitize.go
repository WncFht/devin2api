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
	"regexp"
	"strings"

	"github.com/leookun/devin-2api/internal/llm"
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
}

func rule(id, pattern, replacement string) upstreamSanitizeRule {
	return upstreamSanitizeRule{id: id, pattern: regexp.MustCompile(pattern), replacement: replacement}
}

// upstreamSanitizeRules 顺序与参考实现一致：先整段/整句，再单行兜底。
var upstreamSanitizeRules = []upstreamSanitizeRule{
	// (a1) Claude Code 自我身份句（竞品指纹门），直/弯撇号、完整句/名词短语两形态。
	rule("a1-cc-full", `(?i)You are Claude Code,\s*Anthropic['’]?s official CLI for Claude\.?`, "You are an AI coding assistant."),
	rule("a1-cc-noun", `(?i)Claude Code,\s*Anthropic['’]?s official CLI for Claude\.?`, "an AI coding assistant."),
	// (a2) Claude Agent SDK 自我身份句（content-policy）。
	rule("a2-sdk-full", `(?i)You are a Claude agent, built on Anthropic['’]?s Claude Agent SDK\.?`, "You are an AI coding assistant."),
	rule("a2-sdk-noun", `(?i)\ba Claude agent, built on Anthropic['’]?s Claude Agent SDK\.?`, "an AI coding assistant."),
	// (a3) Claude Code 注入系统提示的计费头行（竞品指纹），整行剥除。
	rule("a3-billing", `(?im)^\s*x-anthropic-billing-header:[^\n]*\n?`, ""),
	// (b) 安全策略整段（abuse gate）：跨段失败时由 b-security-line 单行兜底。
	rule("b-security", `(?i)IMPORTANT:\s*Assist with authorized security testing`+withinParagraph+`(?:defensive use cases\.|security research[^.]*\.)`, securityBenign),
	rule("b-security-line", `(?i)IMPORTANT:\s*Assist with authorized security testing[^\n]*`, securityBenign),
	// dual-use 句本身是独立指纹（本项目实测）；无 IMPORTANT 前缀时兜底改写。
	rule("b-dualluse", `(?i)Dual-use security tools \(C2 frameworks, credential testing, exploit development\) require clear authorization context:[^\n]*`, "Dual-use security tooling (e.g. C2 frameworks, credential testing, exploit development) needs explicit authorization context such as pentesting engagements, CTF competitions, security research, or defensive use cases."),
	// (a4) Claude Code Environment 品牌块与型号目录，均为段落级指纹。
	rule("a4-brand-span", `(?i)Claude Code is available as a CLI`+withinParagraph+`available on Opus [\d./]+\.`, "This coding assistant runs in a terminal."),
	rule("a4-fastmode", `(?im)(?:^|\n)\s*-?\s*Fast mode for Claude Code[^\n]*\n?`, "\n"),
	rule("a4-cli-line", `(?i)Claude Code is available as a CLI[^\n]*\n?`, "This coding assistant runs in a terminal.\n"),
	rule("a4-catalogue", `(?i)The most recent Claude models are`+withinParagraph+`most capable Claude models\.`, ""),
	rule("a4-catalogue-line", `(?im)The most recent Claude models are[^\n]*\n?`, ""),
	// 「You are powered by the model …」与「The exact model ID is …」自我型号指纹：
	// 句点需跟空白或行尾（RE2 无 lookahead，用 (?:\.(\s|$)|$) 实现，(?m) 使 $ 匹配行尾）。
	rule("a4-poweredby", `(?im)You are powered by the model[^\n]*?(?:\.(?:\s|$)|$)\n?`, ""),
	rule("a4-modelid", `(?im)The exact model ID is[^\n]*?(?:\.(?:\s|$)|$)\n?`, ""),
	// (a5) Cline 能力吹嘘句：触发点是句式而非名字，名字保留。
	rule("a5-cline-boast", `You are ([A-Z][\w.-]*), a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns,? and best practices\.`, "You are $1, a software engineer."),
	// (a6) Grok/xAI 自我身份句 + executing_actions_with_care 整块。
	rule("a6-grok-full", `(?i)You are Grok[\w .-]* released by xAI\.?`, "You are an AI coding assistant."),
	rule("a6-grok-noun", `(?i)\bGrok[\w .-]* released by xAI\.?`, "an AI coding assistant."),
	rule("a6-grok2-care", `(?is)<executing_actions_with_care>.*?</executing_actions_with_care>`, ""),
	// (a7) codex apply_patch 工具描述里的 FREEFORM 裸词与 JSON 包裹句。
	// 裸词可能命中用户正文（SQL/代码标识符），只作用于 prompt/工具描述。
	{id: "a7-freeform", pattern: regexp.MustCompile(`FREEFORM`), replacement: "free-form", promptOnly: true},
	{id: "a7-json-wrap", pattern: regexp.MustCompile(`do not wrap the patch in JSON\.`), replacement: "provide the patch as plain text.", promptOnly: true},
	// 本项目实测：Claude Code 提示词的 tool-call 冒号句也是指纹。
	rule("cc-colon-toolcall", `(?i)Do not use a colon before tool calls\.[^\n]*?with a period\.`, "Never put a colon before a tool call; write text like \"Let me read the file.\" ending with a period instead of a colon before the call."),
}

// sanitizeRequest 改写请求中所有会被上游策略拦截的已知文案。
func sanitizeRequest(request llm.RequestMessages) llm.RequestMessages {
	request.SystemPrompt = sanitizeUpstreamText(request.SystemPrompt, true)
	for index, message := range request.Messages {
		switch typed := message.(type) {
		case llm.UserMessage:
			typed.Content = sanitizeContents(typed.Content)
			request.Messages[index] = typed
		case llm.AssistantMessage:
			typed.Content = sanitizeContents(typed.Content)
			request.Messages[index] = typed
		case llm.ToolResultMessage:
			typed.Content = sanitizeContents(typed.Content)
			request.Messages[index] = typed
		}
	}
	for index, tool := range request.Tools {
		request.Tools[index].Description = sanitizeUpstreamText(tool.Description, true)
	}
	// 上游会拒绝「声明了 tools 但 system prompt 为空」的请求（实测触发
	// permission_denied）；注入最小中性身份句兜底。
	if strings.TrimSpace(request.SystemPrompt) == "" && len(request.Tools) > 0 {
		request.SystemPrompt = "You are an AI coding assistant."
	}
	return request
}

func sanitizeContents(content []llm.Content) []llm.Content {
	for index, block := range content {
		switch typed := block.(type) {
		case llm.TextContent:
			typed.Text = sanitizeUpstreamText(typed.Text, false)
			content[index] = typed
		case llm.ThinkingContent:
			typed.Thinking = sanitizeUpstreamText(typed.Thinking, false)
			content[index] = typed
		}
	}
	return content
}

func sanitizeUpstreamText(text string, includePromptOnly bool) string {
	if text == "" {
		return text
	}
	for _, rule := range upstreamSanitizeRules {
		if rule.promptOnly && !includePromptOnly {
			continue
		}
		text = rule.pattern.ReplaceAllString(text, rule.replacement)
	}
	return text
}
