// 本文件验证 OpenAI / Anthropic tool_choice 到中间模型的解析。
package common

import (
	"encoding/json"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

func TestParseOpenAIToolChoice(t *testing.T) {
	cases := []struct {
		raw      string
		wantMode llm.ToolChoiceMode
		wantName string
		wantNil  bool
	}{
		{`"auto"`, "", "", true},
		{`"none"`, llm.ToolChoiceNone, "", false},
		{`"required"`, llm.ToolChoiceRequired, "", false},
		{`{"type":"function","function":{"name":"read_file"}}`, llm.ToolChoiceNamed, "read_file", false},
		// Responses API 的扁平形态。
		{`{"type":"function","name":"exec"}`, llm.ToolChoiceNamed, "exec", false},
		{`null`, "", "", true},
	}
	for _, c := range cases {
		var dropped []string
		choice, err := ParseOpenAIToolChoice(json.RawMessage(c.raw), &dropped)
		if err != nil {
			t.Fatalf("%s: %v", c.raw, err)
		}
		if len(dropped) != 0 {
			t.Fatalf("%s: dropped=%v, want empty", c.raw, dropped)
		}
		if c.wantNil {
			if choice != nil {
				t.Fatalf("%s: got %#v, want nil", c.raw, choice)
			}
			continue
		}
		if choice == nil || choice.Mode != c.wantMode || choice.ToolName != c.wantName {
			t.Fatalf("%s: got %#v, want mode=%q name=%q", c.raw, choice, c.wantMode, c.wantName)
		}
	}
	if _, err := ParseOpenAIToolChoice(json.RawMessage(`"bogus"`), new([]string)); err == nil {
		t.Fatal("bogus option should error")
	}
	// 托管工具约束（file_search/allowed_tools 等）不可满足：记 dropped 按 auto 放行。
	var dropped []string
	choice, err := ParseOpenAIToolChoice(json.RawMessage(`{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"exec"}]}`), &dropped)
	if err != nil || choice != nil {
		t.Fatalf("allowed_tools => choice=%#v err=%v, want nil choice", choice, err)
	}
	if len(dropped) != 1 || dropped[0] != "tool_choice:allowed_tools" {
		t.Fatalf("allowed_tools => dropped=%v, want [tool_choice:allowed_tools]", dropped)
	}
}

func TestParseAnthropicToolChoice(t *testing.T) {
	// "any" 归一为 required（上游 option_name 不接受 "any"）；
	// 规范字段是 disable_parallel_tool_use。
	choice, disable, err := ParseAnthropicToolChoice(json.RawMessage(`{"type":"any","disable_parallel_tool_use":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil || choice.Mode != llm.ToolChoiceRequired || !disable {
		t.Fatalf("any => %#v disable=%v, want required + disable", choice, disable)
	}
	// 历史拼写 disable_parallel_tool_calls 继续兼容。
	if _, disable, err = ParseAnthropicToolChoice(json.RawMessage(`{"type":"auto","disable_parallel_tool_calls":true}`)); err != nil || !disable {
		t.Fatalf("legacy spelling => disable=%v err=%v", disable, err)
	}
	choice, _, err = ParseAnthropicToolChoice(json.RawMessage(`{"type":"tool","name":"Bash"}`))
	if err != nil {
		t.Fatal(err)
	}
	if choice == nil || choice.Mode != llm.ToolChoiceNamed || choice.ToolName != "Bash" {
		t.Fatalf("tool => %#v, want named Bash", choice)
	}
	choice, _, err = ParseAnthropicToolChoice(json.RawMessage(`{"type":"none"}`))
	if err != nil || choice == nil || choice.Mode != llm.ToolChoiceNone {
		t.Fatalf("none => %#v err=%v", choice, err)
	}
	choice, _, err = ParseAnthropicToolChoice(json.RawMessage(`{"type":"auto"}`))
	if err != nil || choice != nil {
		t.Fatalf("auto => %#v err=%v, want nil", choice, err)
	}
	if _, _, err = ParseAnthropicToolChoice(json.RawMessage(`{"type":"bogus"}`)); err == nil {
		t.Fatal("bogus type should error")
	}
	if _, _, err = ParseAnthropicToolChoice(json.RawMessage(`{"type":"tool"}`)); err == nil {
		t.Fatal("tool without name should error")
	}
}

// TestDemoteDroppedToolChoice 覆盖指名降级判定的三条边界：声明过但被丢
// → 降 auto + 记 dropped；同名条目幸存 → 指名仍可满足不动；从未声明
// → 留在指名校验里报错。前两条差一分毫语义就反：surviving 扫描是
// web_search 诱饵与同名 function 并存场景的保命线。
func TestDemoteDroppedToolChoice(t *testing.T) {
	named := &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: "search"}
	surviving := []llm.ToolDefinition{{Name: "search"}}

	cases := []struct {
		name       string
		choice     *llm.ToolChoice
		tools      []llm.ToolDefinition
		dropped    map[string]bool
		wantMode   llm.ToolChoiceMode
		wantMarker string
	}{
		{"dropped name demotes", named, nil, map[string]bool{"search": true}, llm.ToolChoiceAuto, "tool_choice:search"},
		{"same-name survivor keeps choice", named, surviving, map[string]bool{"search": true}, llm.ToolChoiceNamed, ""},
		{"never-declared name stays", named, nil, nil, llm.ToolChoiceNamed, ""},
		{"nil choice no-op", nil, nil, map[string]bool{"search": true}, "", ""},
		{"non-named mode no-op", &llm.ToolChoice{Mode: llm.ToolChoiceNone}, nil, map[string]bool{"search": true}, llm.ToolChoiceNone, ""},
	}
	for _, c := range cases {
		context := llm.RequestMessages{ToolChoice: c.choice, Tools: c.tools}
		DemoteDroppedToolChoice(&context, c.dropped)
		switch {
		case context.ToolChoice == nil:
			if c.wantMode != "" {
				t.Fatalf("%s: ToolChoice demoted to nil, want mode %q", c.name, c.wantMode)
			}
		case context.ToolChoice.Mode != c.wantMode:
			t.Fatalf("%s: mode = %q, want %q", c.name, context.ToolChoice.Mode, c.wantMode)
		case c.wantMode == llm.ToolChoiceNamed && context.ToolChoice.ToolName != "search":
			t.Fatalf("%s: named choice rewrote name to %q", c.name, context.ToolChoice.ToolName)
		}
		if c.wantMarker == "" {
			if len(context.Dropped) != 0 {
				t.Fatalf("%s: dropped = %v, want none", c.name, context.Dropped)
			}
		} else if len(context.Dropped) != 1 || context.Dropped[0] != c.wantMarker {
			t.Fatalf("%s: dropped = %v, want [%s]", c.name, context.Dropped, c.wantMarker)
		}
	}
}
