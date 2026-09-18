// 本文件验证 Devin 工具说明注入和 Schema 清理不会改变调用方提供的工具语义。
package devin

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/WncFht/devin2api/internal/llm"
)

// TestWithToolDescriptionsNumbersProseAndPreservesCode 的测试动机是避免连续能力声明触发上游策略误判，同时保持代码示例完整。
func TestWithToolDescriptionsNumbersProseAndPreservesCode(t *testing.T) {
	prompt, err := withToolDescriptions("", []llm.ToolDefinition{{
		Name: "read&inspect",
		Description: `Read the contents of a file. Supports text files and images (jpg, png).

` + "```json\n" + `{"path":"a&b.txt"}` + "\n```",
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := `# tools descriptions
<tool name="read&amp;inspect">
1. Read the contents of a file.
2. Supports text files and images (jpg, png).

` + "```json\n" + `{"path":"a&amp;b.txt"}` + "\n```\n" + `</tool>`
	if prompt != want {
		t.Fatalf("prompt = %q, want %q", prompt, want)
	}
}

// TestFormatToolDescriptionHandlesChineseAndJSON 的测试动机是覆盖无空格中文句界，同时防止 JSON 示例被误拆为自然语言条目。
func TestFormatToolDescriptionHandlesChineseAndJSON(t *testing.T) {
	description := "读取文件。支持图片。\n\n{\"example\":\"Keep. Together.\"}"
	want := "1. 读取文件。\n2. 支持图片。\n\n{\"example\":\"Keep. Together.\"}"
	if formatted := formatToolDescription(description); formatted != want {
		t.Fatalf("formatted = %q, want %q", formatted, want)
	}
}

// TestFormatToolDescriptionRenumbersExistingLists 的测试动机是防止客户端已有的列表编号被当成句末标点拆散。
func TestFormatToolDescriptionRenumbersExistingLists(t *testing.T) {
	description := "Usage:\n1. Read a file.\n- Supports images."
	want := "1. Usage:\n2. Read a file.\n3. Supports images."
	if formatted := formatToolDescription(description); formatted != want {
		t.Fatalf("formatted = %q, want %q", formatted, want)
	}
}

// TestConvertToolDefinitionStripsAnnotationsButKeepsSchema 的测试动机是防止清理自然语言时破坏业务字段和输入约束。
func TestConvertToolDefinitionStripsAnnotationsButKeepsSchema(t *testing.T) {
	converted, err := convertToolDefinition(llm.ToolDefinition{
		Name:        "search",
		Description: "Search an MCP server with arbitrary arguments.",
		InputSchema: json.RawMessage(`{
				"type":"object",
				"title":"top title annotation",
				"description":"top annotation",
				"x-description":"extension annotation",
				"properties":{
					"description":{"type":"string","description":"business field annotation"},
					"title":{"type":"string","title":"business field title annotation"},
					"mode":{"type":"string","enum":["fast","deep"],"default":"fast"},
					"metadata":{"type":"object","default":{"description":"literal business value"}}
				},
				"required":["description","title"],
				"additionalProperties":false
			}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if converted.GetName() != "search" || converted.GetDescription() != "search" {
		t.Fatalf("identity = %q/%q", converted.GetName(), converted.GetDescription())
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(converted.GetJsonSchemaString()), &schema); err != nil {
		t.Fatal(err)
	}
	for _, annotation := range []string{"description", "title", "x-description"} {
		if _, exists := schema[annotation]; exists {
			t.Fatalf("top-level annotation %q was not removed: %#v", annotation, schema)
		}
	}
	properties := schema["properties"].(map[string]any)
	descriptionField := properties["description"].(map[string]any)
	if descriptionField["type"] != "string" {
		t.Fatalf("business description field = %#v", descriptionField)
	}
	if _, exists := descriptionField["description"]; exists {
		t.Fatalf("nested annotation was not removed: %#v", descriptionField)
	}
	titleField := properties["title"].(map[string]any)
	if titleField["type"] != "string" {
		t.Fatalf("business title field = %#v", titleField)
	}
	if _, exists := titleField["title"]; exists {
		t.Fatalf("nested title annotation was not removed: %#v", titleField)
	}
	mode := properties["mode"].(map[string]any)
	if mode["default"] != "fast" || schema["additionalProperties"] != false {
		t.Fatalf("schema constraints were changed: %#v", schema)
	}
	metadata := properties["metadata"].(map[string]any)
	defaultValue := metadata["default"].(map[string]any)
	if defaultValue["description"] != "literal business value" {
		t.Fatalf("schema literal was changed: %#v", defaultValue)
	}
}

// TestExtractParamDigestHoistsFieldDescriptions 的测试动机是字段级
// prose 是条件必填的唯一载体，剥注解后必须有幸存通道。
func TestExtractParamDigestHoistsFieldDescriptions(t *testing.T) {
	digest := extractParamDigest(json.RawMessage(`{
		"type":"object",
		"properties":{
			"delaySeconds":{"type":"number","description":"Seconds from now. Required unless \"stop\" is true."},
			"stop":{"type":"boolean","title":"End the loop."},
			"nested":{"type":"object","properties":{"inner":{"type":"string","description":"inner doc"}}},
			"itemsList":{"type":"array","items":{"type":"object","properties":{"name":{"type":"string","description":"item name doc"}}}},
			"nodoc":{"type":"string"},
			"bad":{"type":"string","description":""},
			"cond":{"type":"string","description":"Fill it. Required unless \"stop\" is true."},
			"neg":{"type":"string","description":"Purely optional, not required."}
		},
		"required":["delaySeconds"]
	}`))
	want := `- cond (string, required): Fill it. Required unless "stop" is true.
- delaySeconds (number, required): Seconds from now. Required unless "stop" is true.
- itemsList[].name (string): item name doc
- neg (string): Purely optional, not required.
- nested.inner (string): inner doc
- stop (boolean): End the loop.`
	if digest != want {
		t.Fatalf("digest = %q, want %q", digest, want)
	}
}

// TestFieldDocDigestRescuesRequiredTail 的测试动机是 e2e 实证的事故里
// 程碑：条件必填写在长字段描述尾部时头部截断会把它杀掉（noop/prompt
// 被模型整体省略），摘要必须把这种句子救回。
func TestFieldDocDigestRescuesRequiredTail(t *testing.T) {
	long := strings.Repeat("Padding detail about semantics. ", 12) + "Required unless `stop` is true."
	got := fieldDocDigest(long)
	if !strings.HasSuffix(got, "Required unless `stop` is true.") {
		t.Fatalf("required tail lost: %q", got)
	}
	// 截断线内已含 required 句时不重复追加。
	short := "Fill me. Required always. " + strings.Repeat("extra ", 50)
	got = fieldDocDigest(short)
	if strings.Count(got, "Required always.") != 1 {
		t.Fatalf("required sentence duplicated: %q", got)
	}
}

// TestRenderToolSectionDigestSurvivesCompact 的测试动机是 compact
// 截断只应裁导引 prose，不能吃掉摘要里的必填语义（ScheduleWakeup
// 事故形态：3396 字符描述被截在「何时用」讲完处，字段需求全丢）。
func TestRenderToolSectionDigestSurvivesCompact(t *testing.T) {
	entries := []toolSectionEntry{{
		name:         "ScheduleWakeup",
		description:  strings.Repeat("Sentence about usage. ", 40), // 800 chars > compact 预算
		paramsDigest: `- noop (boolean): Required unless "stop" is true.`,
	}}
	compact := renderToolSection(entries, toolDescriptionCompactRunes)
	if !strings.Contains(compact, "Parameters:\n- noop (boolean): Required unless \"stop\" is true.\n</tool>") {
		t.Fatalf("digest missing from compact entry: %q", compact)
	}
	if strings.Contains(compact, strings.Repeat("Sentence about usage. ", 21)) {
		t.Fatalf("description was not truncated: %q", compact)
	}
	// skinny 档连摘要也不发——纯名清单是最后兜底。
	skinny := renderToolSection(entries, -1)
	if skinny != "# available tools: ScheduleWakeup" {
		t.Fatalf("skinny = %q", skinny)
	}
}

// TestWithToolDescriptionsFieldOnlyToolGetsEntry 的测试动机是无顶层
// 描述但有字段文档的工具（部分 MCP 工具形态）也要产出条目。
func TestWithToolDescriptionsFieldOnlyToolGetsEntry(t *testing.T) {
	prompt, err := withToolDescriptions("", []llm.ToolDefinition{{
		Name:        "field_only",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","description":"the x field"}}}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	want := "# tools descriptions\n<tool name=\"field_only\">\n\n\nParameters:\n- x (string): the x field\n</tool>"
	if prompt != want {
		t.Fatalf("prompt = %q, want %q", prompt, want)
	}
}

// TestWithToolDescriptionsRealSetStaysFull 的测试动机是用真实 CC
// 28 工具集（dump 提取）钉住 full 档覆盖率：软顶内不截断，
// ScheduleWakeup 的条件必填经摘要存活。
func TestWithToolDescriptionsRealSetStaysFull(t *testing.T) {
	data, err := os.ReadFile("testdata/cc_tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	tools := make([]llm.ToolDefinition, 0, len(fixture.Tools))
	for _, item := range fixture.Tools {
		tools = append(tools, llm.ToolDefinition{Name: item.Name, Description: item.Description, InputSchema: item.InputSchema})
	}
	prompt, err := withToolDescriptions("sys", tools)
	if err != nil {
		t.Fatal(err)
	}
	// 全文存活的标志：ScheduleWakeup 描述尾部的 delaySeconds 选型章节。
	if !strings.Contains(prompt, "Picking delaySeconds") {
		t.Fatalf("full description was truncated: prompt tail = %q", prompt[len(prompt)-400:])
	}
	if !strings.Contains(prompt, "- noop (boolean, required)") || !strings.Contains(prompt, "Required unless `stop` is true") {
		t.Fatalf("ScheduleWakeup digest missing conditional-required info")
	}
	if len(prompt) > toolPreambleSoftBytes {
		t.Fatalf("section overflowed soft cap: %d > %d", len(prompt), toolPreambleSoftBytes)
	}
}

// TestConditionalRequiredSpecDetectsUniformUnless 的测试动机是合成
// anyOf 的触发条件必须精确：全部必填标记字段共享同一 unless 子句、
// 子句解出同层存在的条件字段，三者缺一即不猜——猜错的 anyOf 会把
// 本来合法的调用判成 schema 违约。
func TestConditionalRequiredSpecDetectsUniformUnless(t *testing.T) {
	cond, marked, ok := conditionalRequiredSpec(json.RawMessage(`{
		"type":"object",
		"properties":{
			"delaySeconds":{"type":"number","description":"Seconds. Required unless ` + "`stop` is true" + `."},
			"noop":{"type":"boolean","description":"Idle tick. Required unless ` + "`stop` is true" + `."},
			"prompt":{"type":"string","description":"The loop input. Required unless ` + "`stop` is true" + `."},
			"reason":{"type":"string","description":"Why this delay. Required unless ` + "`stop` is true" + `."},
			"stop":{"type":"boolean","description":"Set true to end the loop."}
		}
	}`))
	if !ok || cond != "stop" {
		t.Fatalf("cond = %q ok = %v", cond, ok)
	}
	wantMarked := []string{"delaySeconds", "noop", "prompt", "reason"}
	if len(marked) != len(wantMarked) {
		t.Fatalf("marked = %v", marked)
	}
	for i, name := range wantMarked {
		if marked[i] != name {
			t.Fatalf("marked[%d] = %q, want %q", i, marked[i], name)
		}
	}
}

// TestConditionalRequiredSpecRejectsAmbiguous 覆盖拒合成的边界：子句
// 不一致、条件字段缺席、条件字段自身必填、已有 anyOf/oneOf——这些形态
// 合成 anyOf 都会写出错误或冗余约束。
func TestConditionalRequiredSpecRejectsAmbiguous(t *testing.T) {
	cases := map[string]string{
		"mixed clauses": `{"type":"object","properties":{
			"a":{"type":"string","description":"Fill. Required unless ` + "`x` is true" + `."},
			"b":{"type":"string","description":"Fill. Required unless ` + "`y` is true" + `."},
			"x":{"type":"boolean"},"y":{"type":"boolean"}}}`,
		"clause field missing": `{"type":"object","properties":{
			"a":{"type":"string","description":"Fill. Required unless ` + "`ghost` is true" + `."}}}`,
		"no clause": `{"type":"object","properties":{
			"a":{"type":"string","description":"Fill. Required."},
			"stop":{"type":"boolean"}}}`,
		"cond field required": `{"type":"object","properties":{
			"a":{"type":"string","description":"Fill. Required unless ` + "`stop` is true" + `."},
			"stop":{"type":"boolean","description":"Always."}},
			"required":["stop"]}`,
		"cond field marked": `{"type":"object","properties":{
			"a":{"type":"string","description":"Fill. Required unless ` + "`stop` is true" + `."},
			"stop":{"type":"boolean","description":"Required unless ` + "`a` is true" + `."}}}`,
		"has anyOf": `{"type":"object","properties":{
			"a":{"type":"string","description":"Fill. Required unless ` + "`stop` is true" + `."},
			"stop":{"type":"boolean"}},
			"anyOf":[{"required":["stop"]}]}`,
	}
	for name, schema := range cases {
		if cond, marked, ok := conditionalRequiredSpec(json.RawMessage(schema)); ok {
			t.Fatalf("%s: synthesized (cond=%q marked=%v), want rejected", name, cond, marked)
		}
	}
}

// TestInjectConditionalRequiredDemotesMarked 的测试动机是合成分支的
// 前提：marked 字段必须离开顶层 required[]——否则 stop 分支仍被顶层
// 约束强制带上全量字段，anyOf 形同虚设。
func TestInjectConditionalRequiredDemotesMarked(t *testing.T) {
	got := injectConditionalRequired(json.RawMessage(`{
		"type":"object",
		"properties":{"a":{"type":"string"},"b":{"type":"string"},"stop":{"type":"boolean"}},
		"required":["a","stop_anchor"],
		"additionalProperties":false
	}`), "stop", []string{"a", "b"})
	var object map[string]any
	if err := json.Unmarshal(got, &object); err != nil {
		t.Fatal(err)
	}
	// "a" 被降级出顶层 required，无关字段保留。
	required, _ := object["required"].([]any)
	if len(required) != 1 || required[0] != "stop_anchor" {
		t.Fatalf("required = %v, want [stop_anchor]", required)
	}
	branches, _ := object["anyOf"].([]any)
	if len(branches) != 2 {
		t.Fatalf("anyOf = %v", object["anyOf"])
	}
	branch0 := branches[0].(map[string]any)["required"].([]any)
	if len(branch0) != 1 || branch0[0] != "stop" {
		t.Fatalf("cond branch = %v", branch0)
	}
	branch1 := branches[1].(map[string]any)["required"].([]any)
	if len(branch1) != 2 || branch1[0] != "a" || branch1[1] != "b" {
		t.Fatalf("marked branch = %v", branch1)
	}
}

// TestNormalizeSchemaFuseStripsDeepRefs 的测试动机是深度保险丝不能把
// $ref 放上线：截断子树里残留的本地 ref 是截断痕迹而非客户端语义，
// 会重新引入上游 invalid_argument（33 跳 ref 链还会经兄弟合并把深
// ref 反向注回 depth-32 父层）。剥键后兄弟约束必须原样保留。
func TestNormalizeSchemaFuseStripsDeepRefs(t *testing.T) {
	// ref 链：d0→…→d32→d33，d32 在 depth33 触发保险丝；修复前它的
	// $ref 会经 resolved 融合一路注回顶层上 wire。
	defs := make(map[string]any, 34)
	for i := 0; i <= 32; i++ {
		defs[fmt.Sprintf("d%d", i)] = map[string]any{"$ref": fmt.Sprintf("#/$defs/d%d", i+1)}
	}
	defs["d33"] = map[string]any{"type": "string"}
	chained, err := normalizeSchema(mustJSON(t, map[string]any{
		"$defs": defs,
		"$ref":  "#/$defs/d0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(chained), "$ref") {
		t.Fatalf("ref chain: wire schema still carries $ref: %s", chained)
	}

	// 结构深嵌：properties 每层耗 2 depth，17+ 层让 $ref 落在保险丝
	// 截断子树的内部（不是被截节点自身的键，顶层单行 delete 够不到）。
	nested := map[string]any{"$ref": "#/$defs/x"}
	for i := 0; i < 20; i++ {
		nested = map[string]any{"properties": map[string]any{"p": nested}}
	}
	deep, err := normalizeSchema(mustJSON(t, map[string]any{
		"type":       "object",
		"properties": map[string]any{"p": nested},
		"$defs":      map[string]any{"x": map[string]any{"type": "string"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(deep), "$ref") {
		t.Fatalf("deep nesting: wire schema still carries $ref: %s", deep)
	}

	// 兄弟约束在截断处必须保留：items 链 33 层把带 $ref 的节点正好放到
	// 保险丝边界上，剥 ref 后 type/minimum 兄弟键照常下发。
	target := map[string]any{"$ref": "#/$defs/x", "type": "string", "minLength": float64(3)}
	wrapped := target
	for i := 0; i < 33; i++ {
		wrapped = map[string]any{"items": wrapped}
	}
	sibling, err := normalizeSchema(mustJSON(t, map[string]any{
		"type":  "array",
		"items": wrapped,
		"$defs": map[string]any{"x": map[string]any{"type": "string"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(sibling)
	if strings.Contains(text, "$ref") {
		t.Fatalf("items chain: wire schema still carries $ref: %s", text)
	}
	if !strings.Contains(text, `"minLength":3`) {
		t.Fatalf("sibling constraint lost at fuse cap: %s", text)
	}

	// const 字面量里的 "$ref" 是业务数据不是引用：正常深度不剥，
	// 保险丝截断处同样不剥。
	literal, err := normalizeSchema(mustJSON(t, map[string]any{
		"type":  "object",
		"const": map[string]any{"$ref": "#/$defs/x"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(literal), "$ref") {
		t.Fatalf("const literal $ref was stripped: %s", literal)
	}
}

// mustJSON 把测试构造的 map 编成 schema 输入。
func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestConvertToolDefinitionSynthesizesAnyOfOnRealFixture 用真实 CC 28
// 工具集钉住端到端行为：ScheduleWakeup 的 wire schema 必须长出
// anyOf 条件必填（e2e 实测这把全字段命中率从 ~50% 推到 13/16，且
// stop 分支调用保持干净 {stop:true}），其余工具不得被波及。
func TestConvertToolDefinitionSynthesizesAnyOfOnRealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/cc_tools.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	sawScheduleWakeup := false
	for _, item := range fixture.Tools {
		converted, err := convertToolDefinition(llm.ToolDefinition{Name: item.Name, InputSchema: item.InputSchema})
		if err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		var schema map[string]any
		if err := json.Unmarshal([]byte(converted.GetJsonSchemaString()), &schema); err != nil {
			t.Fatalf("%s: wire schema unparseable", item.Name)
		}
		_, hasAnyOf := schema["anyOf"]
		if item.Name != "ScheduleWakeup" {
			if hasAnyOf {
				t.Fatalf("%s: unexpected anyOf synthesized", item.Name)
			}
			continue
		}
		sawScheduleWakeup = true
		if !hasAnyOf {
			t.Fatalf("ScheduleWakeup: anyOf missing from wire schema")
		}
		encoded, _ := json.Marshal(schema["anyOf"])
		want := `[{"required":["stop"]},{"required":["delaySeconds","noop","prompt","reason"]}]`
		if string(encoded) != want {
			t.Fatalf("ScheduleWakeup anyOf = %s, want %s", encoded, want)
		}
	}
	if !sawScheduleWakeup {
		t.Fatal("fixture lost ScheduleWakeup")
	}
}
