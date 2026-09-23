// 本文件验证上游文案改写规则的单条行为契约。
package devin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

// TestSanitizeCodexOpensourceDef 锁 codex-opensource-def 的行为：触发点是
// "Codex refers to … interface" 完整跨度（中间词可变，截短即放行），
// 改写为 "Codex is the open-source coding interface"。测试贯通 trigger
// 预筛、pattern 命中与命中计数——缺一环规则即静默失效。
func TestSanitizeCodexOpensourceDef(t *testing.T) {
	rewrite := func(text string) (string, int) {
		hits := map[string]int{}
		return sanitizeUpstreamText(text, true, hits), hits["codex-opensource-def"]
	}
	// 模板原句与中间词变体同属该指纹跨度。原句拆行拼接——整句字面量
	// 在传输层会被本规则自身改写（指纹跨度 [^\n.]* 跨不过源码换行）。
	for _, text := range []string{
		"Within this context, Codex refers to the open-source agentic coding " +
			"interface (not the old Codex language model built by OpenAI).",
		"Codex refers to the open-source coding " +
			"interface",
	} {
		got, hits := rewrite(text)
		if hits != 1 || strings.Contains(got, "refers to") || !strings.Contains(got, "Codex is the open-source coding interface") {
			t.Fatalf("rewrite(%q) = %q, hits %d", text, got, hits)
		}
	}
	// 不完整跨度上游实测放行，规则不得误伤用户正文。
	partial := "Codex refers to the open-source"
	if got, hits := rewrite(partial); got != partial || hits != 0 {
		t.Fatalf("partial span must not rewrite: %q, hits %d", got, hits)
	}
	// 改写后文本不再命中：良性句幂等，重复改写不会越改越乱。
	benign := "Codex is the open-source coding interface"
	if got, hits := rewrite(benign); got != benign || hits != 0 {
		t.Fatalf("rewritten text must be idempotent: %q, hits %d", got, hits)
	}
}

// ccColonFingerprint 是 cc-colon-toolcall 规则的触发句，拼接构造——
// 整句字面量在传输层会被同一条规则改写，落不进测试文件。
var ccColonFingerprint = "Do not use a colon before tool calls." +
	" Write the closing sentence with a period."

// TestSanitizeToolCallArgumentsKeepsJSONValid 钉 G1 修复：指纹句落在
// tool_call 参数的 JSON 串值内时，改写按叶子值进行、由编码器重新转义——
// 文本级替换会把替换词里的裸 " 灌进串值造出非法 argumentsJson。
// 契约：产物必须是合法 JSON 对象、指纹被中性化、命中照常记账。
func TestSanitizeToolCallArgumentsKeepsJSONValid(t *testing.T) {
	content := []llm.Content{llm.ToolCall{
		ID:   "call_1",
		Name: "Write",
		Arguments: json.RawMessage(
			`{"file_path":"/tmp/notes.md","content":"` + ccColonFingerprint + `","nested":{"lines":["` + ccColonFingerprint + `"],"n":1e3}}`),
	}}
	hits := map[string]int{}
	sanitizeContents(content, hits)
	call := content[0].(llm.ToolCall)
	if !llm.IsJSONObject(call.Arguments) {
		t.Fatalf("sanitized arguments broke JSON: %s", call.Arguments)
	}
	var decoded struct {
		Content string `json:"content"`
		Nested  struct {
			Lines []string    `json:"lines"`
			N     json.Number `json:"n"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(call.Arguments, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, leaf := range append([]string{decoded.Content}, decoded.Nested.Lines...) {
		if strings.Contains(leaf, "Do not use a colon") || !strings.Contains(leaf, "Never put a colon") {
			t.Fatalf("fingerprint not neutralized in leaf: %q", leaf)
		}
	}
	// 未触及的字面量必须原样存活——UseNumber 保住 1e3 原文而非 float 化。
	if decoded.Nested.N.String() != "1e3" {
		t.Fatalf("number literal rewritten: %s", decoded.Nested.N)
	}
	if hits["cc-colon-toolcall"] != 2 {
		t.Fatalf("hits = %v, want cc-colon-toolcall:2", hits)
	}
}

// TestSanitizeToolCallArgumentsCleanBytesUnmoved 钉「无命中零漂移」：
// 参数体不含任何 trigger 时原字节返回——marshal 会归并键序/空白，
// 能不动就不动（历史回放的 argumentsJson 逐字节稳定是缓存友好面）。
func TestSanitizeToolCallArgumentsCleanBytesUnmoved(t *testing.T) {
	raw := `{"z":1,"a":"plain text","arr":[1,2]}`
	content := []llm.Content{llm.ToolCall{ID: "c1", Name: "x", Arguments: json.RawMessage(raw)}}
	sanitizeContents(content, map[string]int{})
	if got := string(content[0].(llm.ToolCall).Arguments); got != raw {
		t.Fatalf("clean arguments mutated: %s", got)
	}
}

// TestSanitizeToolCallArgumentsCustomStaysTextLevel 验证 Custom/freeform
// 参数体（非 JSON）仍走文本级改写——无 JSON 结构可破坏。
func TestSanitizeToolCallArgumentsCustomStaysTextLevel(t *testing.T) {
	raw := "*** Begin Patch\n" + ccColonFingerprint + "\n*** End Patch"
	content := []llm.Content{llm.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage(raw), Custom: true}}
	hits := map[string]int{}
	sanitizeContents(content, hits)
	got := string(content[0].(llm.ToolCall).Arguments)
	if strings.Contains(got, "Do not use a colon") || !strings.Contains(got, "Never put a colon") {
		t.Fatalf("custom args not sanitized: %q", got)
	}
	if hits["cc-colon-toolcall"] != 1 {
		t.Fatalf("hits = %v", hits)
	}
}

// ngramTokensForTest 复刻上游归一化：小写 + 非字母数字折成空白 + 折叠，
// 返回 token 流串——改写产物里不得再含条目序列。
func ngramTokensForTest(text string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(text) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// TestSanitizeOpenClawNgrams 逐条钉 2026-09-23 实证的 OpenClaw/ZCode
// 模板指纹：原始模板行命中一次、改写产物归一化后不再含条目、重复
// 改写幂等。原始行拼接构造——整句落进文件会被同套规则在透镜下改写。
func TestSanitizeOpenClawNgrams(t *testing.T) {
	cases := []struct {
		id        string
		entry     string // 上游归一化条目
		raw       string // 模板原文形态
		wantPiece string // 改写后应出现的中性文案片段
	}{
		{"oc-read-soul", "1 read soul md this is who you are",
			"1. Read `SOUL.md` — this is who " + "you are", "it defines who you are"},
		{"oc-read-user", "2 read user md this is who you re helping",
			"2. Read `USER.md` — this is who " + "you're helping", "it describes who"},
		{"oc-capture-matters",
			"capture what matters decisions context things to remember skip the secrets unless asked to keep them",
			"Capture what matters. Decisions, context, things to remember. " + "Skip the secrets unless asked to keep them.",
			"Write down what matters"},
		{"oc-main-session-only", "only load in main session direct chats with your human",
			"- **ONLY load in main session** " + "(direct chats with your human)", "Load only in the main session"},
		{"oc-humans-stuff",
			"you have access to your human s stuff that doesn t mean you share their stuff in groups you re a participant not their voice not their proxy think before you speak",
			"You have access to your human's stuff. That doesn't mean you _share_ their stuff. " +
				"In groups, you're a participant — not their voice, not their proxy. " + "Think before you speak.",
			"You can access your human's stuff"},
		{"oc-group-contribute", "in group chats where you receive every message be smart about when to contribute",
			"In group chats where you receive every message, be **smart about when to contribute**:", "you see every message"},
		{"oc-emoji-reactions", "on platforms that support reactions discord slack use emoji reactions naturally",
			"On platforms that support reactions (Discord, Slack), use emoji " + "reactions naturally:", "platforms supporting reactions"},
		{"oc-journal-wisdom",
			"think of it like a human reviewing their journal and updating their mental model daily files are raw notes memory md is curated wisdom",
			"Think of it like a human reviewing their journal and updating their mental model. " +
				"Daily files are raw notes; MEMORY.md is curated " + "wisdom.",
			"the distilled version"},
		{"oc-soul-evolve", "this file is yours to evolve as you learn who you are update it",
			"_This file is yours to evolve. As you learn who you are, " + "update it._", "yours to shape"},
		{"oc-workbuddy-soul", "if soul md is present embody its persona and tone",
			"If SOUL.md is present, embody its persona " + "and tone.", "SOUL.md exists"},
	}
	for _, tc := range cases {
		hits := map[string]int{}
		got := sanitizeUpstreamText(tc.raw, true, hits)
		if hits[tc.id] != 1 || !strings.Contains(got, tc.wantPiece) {
			t.Fatalf("%s: rewrite(%q) = %q, hits %v", tc.id, tc.raw, got, hits)
		}
		if strings.Contains(ngramTokensForTest(got), tc.entry) {
			t.Fatalf("%s: rewritten text still contains entry: %q", tc.id, got)
		}
		if again := sanitizeUpstreamText(got, true, map[string]int{}); again != got {
			t.Fatalf("%s: rewrite not idempotent: %q -> %q", tc.id, got, again)
		}
	}
}

// TestSanitizeOpenClawBoundaries 钉 ngram 边界语义：词被字母黏连
// （wisdoms 里的 wisdom）、token 缺失/插入都不命中——上游同语义。
func TestSanitizeOpenClawBoundaries(t *testing.T) {
	for _, text := range []string{
		// "wisdom" 黏尾：上游 token 是 wisdoms ≠ wisdom。
		"Daily files are raw notes; MEMORY.md is curated wisdoms.",
		// 断句：条目跨两整句，少后半即非指纹。
		"Think of it like a human reviewing their journal and updating their mental model.",
		// 词间插入一个实词即破坏连续匹配。
		"In group chats where you slowly receive every message, be smart about when to contribute:",
		// 无数字序号的 Read 行上游实测放行（"1" 是条目一部分）。
		"Read `SOUL.md` — this is who you are",
	} {
		hits := map[string]int{}
		if got := sanitizeUpstreamText(text, true, hits); got != text {
			t.Fatalf("non-fingerprint text mutated: %q -> %q (hits %v)", text, got, hits)
		}
	}
}

// TestSanitizeOpenClawPromptOnly 钉 oc-* 规则的作用域：上游只扫 prompt
// 字段，故 fingerprint 落在 user 消息与工具调用参数里必须原样保留
// （写盘文件内容不能被静默改写），在 SystemPrompt/工具描述里才改写。
func TestSanitizeOpenClawPromptOnly(t *testing.T) {
	fp := "_This file is yours to evolve. As you learn who " + "you are, update it._"
	request := llm.RequestMessages{
		SystemPrompt: fp,
		Messages: []llm.Message{
			llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: fp}}},
			llm.AssistantMessage{Content: []llm.Content{llm.ToolCall{
				ID: "c1", Name: "Write",
				Arguments: json.RawMessage(`{"file_path":"/w/SOUL.md","content":"` + fp + `"}`),
			}}},
		},
		Tools: []llm.ToolDefinition{{Name: "x", Description: fp}},
	}
	out, hits := sanitizeRequest(request)
	if hits["oc-soul-evolve"] != 2 {
		t.Fatalf("expected 2 hits (system+tool desc), got %v", hits)
	}
	if strings.Contains(out.SystemPrompt, "yours to evolve") {
		t.Fatalf("system prompt not sanitized: %q", out.SystemPrompt)
	}
	user := out.Messages[0].(llm.UserMessage).Content[0].(llm.TextContent).Text
	if user != fp {
		t.Fatalf("user message mutated: %q", user)
	}
	args := string(out.Messages[1].(llm.AssistantMessage).Content[0].(llm.ToolCall).Arguments)
	if !strings.Contains(args, "yours to evolve") {
		t.Fatalf("tool args must stay verbatim: %q", args)
	}
	if strings.Contains(out.Tools[0].Description, "yours to evolve") {
		t.Fatalf("tool description not sanitized: %q", out.Tools[0].Description)
	}
}
