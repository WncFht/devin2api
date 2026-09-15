// 本文件验证上游文案改写规则的单条行为契约。
package devin

import (
	"strings"
	"testing"
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
	// 模板原句与中间词变体同属该指纹跨度。
	for _, text := range []string{
		"Within this context, Codex refers to the open-source agentic coding interface (not the old Codex language model built by OpenAI).",
		"Codex refers to the open-source coding interface",
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
