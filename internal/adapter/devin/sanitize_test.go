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
