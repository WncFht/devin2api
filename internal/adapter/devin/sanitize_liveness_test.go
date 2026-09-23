// 本文件对 upstreamSanitizeRules 做活性（liveness）检查：每条规则都必须对
// 一条「见证文本」（witness，即该规则声称覆盖的指纹原文或其同构变体）真实
// 改写。断言链覆盖四层失效模式：
//
//  1. trigger 失灵——witness 不含 trigger 时预筛 strings.Contains 直接跳过，
//     pattern 再正确也是死规则（写错 trigger 是唯一无法靠编译期发现的路径）。
//  2. pattern 不命中——规则对其指纹原文零改写。
//  3. 恒等改写——pattern 命中但 replacement 与匹配文本逐字相同（审计误报
//     所指的死法：ReplaceAll 后输出==输入，永远零字节差异）。
//  4. 改写后仍是指纹——输出仍被 pattern 命中，上游照样拦。
//
// witness 表与规则表强制一一对应：新增规则必须同步补见证，否则测试红。
package devin

import (
	"strings"
	"testing"
)

// sanitizeWitnesses 为每条规则给出会被上游拦截的指纹文本见证。选择原则：
// 取指纹文档（docs/upstream-policy-fingerprints.md）实证原句或同构最短文本；
// 对「整段/整句 + 单行兜底」的规则对，兜底规则的见证必须不含整段规则要求的
// 尾部成分，确保兜底路径本身被单独验证。
var sanitizeWitnesses = map[string][]string{
	"a1-cc-full": {
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are Claude Code, Anthropic’s official CLI for Claude",
	},
	"a1-cc-noun":         {"Claude Code, Anthropic's official CLI for Claude."},
	"a2-sdk-full":        {"You are a Claude agent, built on Anthropic's Claude Agent SDK."},
	"a2-sdk-noun":        {"a Claude agent, built on Anthropic’s Claude Agent SDK."},
	"a3-billing":         {"header\nx-anthropic-billing-header: claude-code/2.1.0\nnext"},
	"b-security":         {"IMPORTANT: Assist with authorized security testing only with explicit permission. All findings must stay within defensive use cases."},
	"b-security-line":    {"IMPORTANT: Assist with authorized security testing only with explicit permission."},
	"b-dualluse":         {"Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases."},
	"a4-brand-span":      {"Claude Code is available as a CLI, desktop app, and web app. It's available on Opus 4.5."},
	"a4-fastmode":        {"intro\n- Fast mode for Claude Code works differently\ntail"},
	"a4-cli-line":        {"Claude Code is available as a CLI for daily work.\nnext"},
	"a4-catalogue":       {"The most recent Claude models are listed below — the most capable Claude models."},
	"a4-catalogue-line":  {"The most recent Claude models are Opus 4.5 and Sonnet 4.5.\nnext"},
	"a4-poweredby":       {"You are powered by the model named Opus 4.5. Next sentence."},
	"a4-modelid":         {"The exact model ID is claude-opus-4-5. Next."},
	"a5-cline-boast":     {"You are Cline, a highly skilled software engineer with extensive knowledge in many programming languages, frameworks, design patterns, and best practices."},
	"a6-grok-full":       {"You are Grok 4 released by xAI."},
	"a6-grok-noun":       {"Grok 4.1 released by xAI."},
	"a6-grok2-care":      {"pre <executing_actions_with_care>\nbody\n</executing_actions_with_care> post"},
	"a7-freeform":        {"The patch format uses FREEFORM text here."},
	"a7-json-wrap":       {"do not wrap the patch in JSON."},
	"cc-colon-toolcall":  {"Do not use a colon before tool calls. Write text ending with a period."},
	"cc-autocompact":     {"The system will automatically compress prior messages in your conversation as it approaches context limits. Details follow."},
	"cc-help-line":       {"- /help: Get help with using Claude Code"},
	"cc-agent-tool":      {"Use the Agent tool with specialized agents when the task at hand matches the agent's description. More follows."},
	"cc-claudemd":        {"Anything already documented in CLAUDE.md files."},
	"cc-memory-must":     {"You MUST access memory when the user explicitly asks you to check, recall, or remember."},
	"cc-feedback":        {"To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues tracker."},
	"cc-blast-radius":    {"Carefully consider the reversibility and blast radius of actions."},
	"cc-claudemd-2":      {"durable instructions like CLAUDE.md files"},
	"cc-subagent-emojis": {"For clear communication with the user the assistant MUST avoid using emojis."},
	"codex-opensource-def": {
		"Within this context, Codex refers to the open-source agentic coding interface (not the old Codex language model built by OpenAI).",
		"Codex refers to the open-source coding interface",
	},
	"codex-plan-statuses": {"Do not batch-complete multiple items after the fact. Finish with all items completed or explicitly canceled/deferred before ending the turn."},
	"codex-ansi-escapes": {
		"Don't output ANSI escape codes directly — the CLI renderer applies them.",
		"Don’t output ANSI escape codes directly — the CLI renderer applies them.",
	},
	"codex-permissions": {"pre <permissions instructions>\nsandbox rules\n</permissions instructions> post"},
	"oc-read-soul": {
		"1. Read `SOUL.md` — this is who you are",
	},
	"oc-read-user": {
		"2. Read `USER.md` — this is who you're helping",
	},
	"oc-capture-matters": {
		"Capture what matters. Decisions, context, things to remember. Skip the secrets unless asked to keep them.",
	},
	"oc-main-session-only": {
		"- **ONLY load in main session** (direct chats with your human)",
	},
	"oc-humans-stuff": {
		"You have access to your human's stuff. That doesn't mean you _share_ their stuff. In groups, you're a participant — not their voice, not their proxy. Think before you speak.",
	},
	"oc-group-contribute": {
		"In group chats where you receive every message, be **smart about when to contribute**:",
	},
	"oc-emoji-reactions": {
		"On platforms that support reactions (Discord, Slack), use emoji reactions naturally:",
	},
	"oc-journal-wisdom": {
		"Think of it like a human reviewing their journal and updating their mental model. Daily files are raw notes; MEMORY.md is curated wisdom.",
	},
	"oc-soul-evolve": {
		"_This file is yours to evolve. As you learn who you are, update it._",
	},
	"oc-workbuddy-soul": {
		"{% else %}If SOUL.md is present, embody its persona and tone. Stay consistent.{% endif %}",
	},
}

// TestSanitizeRuleLiveness 逐条验证规则活性，并对表本身做结构检查。
func TestSanitizeRuleLiveness(t *testing.T) {
	if len(sanitizeWitnesses) != len(upstreamSanitizeRules) {
		var missing, extra []string
		covered := map[string]bool{}
		for _, rule := range upstreamSanitizeRules {
			if _, ok := sanitizeWitnesses[rule.id]; !ok {
				missing = append(missing, rule.id)
			}
			covered[rule.id] = true
		}
		for id := range sanitizeWitnesses {
			if !covered[id] {
				extra = append(extra, id)
			}
		}
		t.Fatalf("witness/rule table mismatch, missing witnesses: %v, stale witnesses: %v", missing, extra)
	}

	for index, rule := range upstreamSanitizeRules {
		if rule.trigger == "" {
			t.Errorf("%s: empty trigger", rule.id)
		}
		// 幂等：replacement 自身不得再被 pattern 命中，否则改写产物仍带指纹。
		if rule.pattern.MatchString(rule.replacement) {
			t.Errorf("%s: replacement still matches pattern", rule.id)
		}
		for _, witness := range sanitizeWitnesses[rule.id] {
			// 预筛一致性：trigger 必须是 witness 的小写子串（否则规则被
			// strings.Contains 预筛静默跳过，pattern 写得再对也不生效）。
			if !strings.Contains(strings.ToLower(witness), rule.trigger) {
				t.Errorf("%s: witness %q lacks trigger %q", rule.id, witness, rule.trigger)
			}
			if sanitizeCandidateRules(witness, sanitizePairAll)>>uint(index)&1 == 0 {
				t.Errorf("%s: witness %q missed by pair prefilter", rule.id, witness)
			}
			if !rule.pattern.MatchString(witness) {
				t.Errorf("%s: pattern does not match witness %q", rule.id, witness)
				continue
			}
			got := rule.pattern.ReplaceAllString(witness, rule.replacement)
			// 恒等改写即死规则：输出必须与输入存在真实字节差异。
			if got == witness {
				t.Errorf("%s: rewrite is identity on witness %q", rule.id, witness)
			}
			// 改写产物不得再命中指纹 pattern。
			if rule.pattern.MatchString(got) {
				t.Errorf("%s: rewrite output %q still matches pattern", rule.id, got)
			}
			// 端到端：走 sanitizeUpstreamText 全链路（含 promptOnly 路径），
			// hits 计数必须记录该规则——预筛/正则任一环断裂都会在这里暴露。
			hits := map[string]int{}
			out := sanitizeUpstreamText(witness, true, hits)
			if hits[rule.id] == 0 || out == witness {
				t.Errorf("%s: pipeline rewrite(%q) = %q, hits %d", rule.id, witness, out, hits[rule.id])
			}
			if rule.promptOnly {
				// 裸词规则不得改写消息正文（用户数据保护边界）。
				msgHits := map[string]int{}
				if out := sanitizeUpstreamText(witness, false, msgHits); out != witness || msgHits[rule.id] != 0 {
					t.Errorf("%s: promptOnly rule fired on message path", rule.id)
				}
			}
		}
	}
}
