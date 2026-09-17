// wire_golden_test.go — buildRequest 产出的上游 wire 形状字节级钉版。
// canonical 输入 → protojson → 归一化挥发字段（executionId / metadata.f /
// stepIndex 全局计数）→ 比对 testdata/wire/<name>.json。
// 投影层任何静默漂移（字段增删、枚举值、工具声明形状、工具描述注入）
// 都在提交时炸出来，而不是靠运行时 repairs 计数后发现。
// 重新生成：UPDATE_GOLDEN=1 go test ./internal/adapter/devin -run TestWireGolden
package devin

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/WncFht/devin2api/internal/llm"
)

// wireGoldenCases 每个用例钉一份完整上游请求；SessionKey 互不相同，
// 避免 trajectory 派生互相影响。
var wireGoldenCases = map[string]struct {
	request llm.RequestMessages
	binding callBinding
}{
	// 单轮文本 + 工具声明：钉 prompt 拼装、工具声明形状、
	// 工具描述注入 system prompt 的位置。
	"chat_with_tools": {
		request: llm.RequestMessages{
			SessionKey:   "golden-wire-chat",
			SystemPrompt: "You are a coding assistant.",
			Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "Read main.go then summarize."},
			}}},
			Tools: []llm.ToolDefinition{{
				Name:         "read_file",
				Description:  "Read a file from the workspace",
				InputSchema:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
				ReadOnlyHint: true,
			}},
		},
		binding: callBinding{Token: "golden-token", Model: "glm-5-2"},
	},
	// assistant 工具调用 + 工具结果 + 追轮：钉 call→result 配对重排、
	// step/角色枚举、TOOL 结果正文形态。
	"tool_roundtrip": {
		request: llm.RequestMessages{
			SessionKey:   "golden-wire-tools",
			SystemPrompt: "You are a coding assistant.",
			Messages: []llm.Message{
				llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "check the file"}}},
				llm.AssistantMessage{Content: []llm.Content{
					llm.TextContent{Text: "Let me read it."},
					llm.ToolCall{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"main.go"}`)},
				}},
				llm.ToolResultMessage{ToolCallID: "call_1", Content: []llm.Content{llm.TextContent{Text: "package main\n"}}, TimestampMS: 1758000000000},
				llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "now fix it"}}},
			},
			Tools: []llm.ToolDefinition{{
				Name:        "read_file",
				Description: "Read a file from the workspace",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
			}},
		},
		binding: callBinding{Token: "golden-token", Model: "glm-5-2"},
	},
	// tool_choice=none：钉「工具声明与描述注入整体不进 wire」的真禁用语义。
	"tool_choice_none": {
		request: llm.RequestMessages{
			SessionKey:   "golden-wire-none",
			SystemPrompt: "You are a coding assistant.",
			ToolChoice:   &llm.ToolChoice{Mode: llm.ToolChoiceNone},
			Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "answer without tools"},
			}}},
			Tools: []llm.ToolDefinition{{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		},
		binding: callBinding{Token: "golden-token", Model: "glm-5-2"},
	},
	// thinking 重放：钉 ThinkingContent 的 signature/signature_type 映射。
	"thinking_replay": {
		request: llm.RequestMessages{
			SessionKey:   "golden-wire-thinking",
			SystemPrompt: "You are a coding assistant.",
			Messages: []llm.Message{
				llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "think about it"}}},
				llm.AssistantMessage{Content: []llm.Content{
					llm.ThinkingContent{Thinking: "reasoning here", ThinkingSignature: "sig-payload", SignatureType: "anthropic"},
					llm.TextContent{Text: "the answer"},
				}},
				llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "go on"}}},
			},
		},
		binding: callBinding{Token: "golden-token", Model: "glm-5-2"},
	},
	// 采样参数 + 指名 tool_choice + router jwt：钉 configuration 透传与
	// ModelAssignmentJwt / ToolName 字段落位。
	"sampling_and_named_choice": {
		request: llm.RequestMessages{
			SessionKey:    "golden-wire-sampling",
			SystemPrompt:  "You are a coding assistant.",
			MaxTokens:     intPtr(4096),
			Temperature:   floatPtr(0.2),
			TopP:          floatPtr(0.8),
			TopK:          intPtr(10),
			Seed:          int64Ptr(42),
			StopSequences: []string{"STOP"},
			ToolChoice:    &llm.ToolChoice{Mode: llm.ToolChoiceNamed, ToolName: "read_file"},
			Messages: []llm.Message{llm.UserMessage{Content: []llm.Content{
				llm.TextContent{Text: "hi"},
			}}},
			Tools: []llm.ToolDefinition{{
				Name:        "read_file",
				Description: "Read a file",
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
		},
		binding: callBinding{Token: "golden-token", Model: "glm-5-2", ModelAssignmentJWT: "golden-router-jwt"},
	},
	// 当前轮图片：钉 Images 只挂最后一条助手消息之后的 user/tool 消息。
	"current_turn_image": {
		request: llm.RequestMessages{
			SessionKey:   "golden-wire-image",
			SystemPrompt: "You are a coding assistant.",
			Messages: []llm.Message{
				llm.UserMessage{Content: []llm.Content{llm.TextContent{Text: "look"}}},
				llm.AssistantMessage{Content: []llm.Content{llm.TextContent{Text: "send the image"}}},
				llm.UserMessage{Content: []llm.Content{
					llm.TextContent{Text: "here"},
					llm.ImageContent{Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==", MIMEType: "image/png"},
				}},
			},
		},
		binding: callBinding{Token: "golden-token", Model: "glm-5-2"},
	},
}

func intPtr(v int) *int           { return &v }
func int64Ptr(v int64) *int64     { return &v }
func floatPtr(v float64) *float64 { return &v }

// normalizeVolatileWire 把每次调用必然不同的字段替换成占位符：
// executionId 与每个 prompt 的 messageId 是 per-request UUID，
// metadata.f 是随机设备指纹，trajectoryReference.stepIndex 依赖进程级
// 计数器（-shuffle=on 下顺序不固定）。字段存在性仍被钉住：已知位置
// 缺失时替换前断言失败；messageId 递归处理覆盖未来嵌套位置。
func normalizeVolatileWire(t *testing.T, doc map[string]any) {
	t.Helper()
	setVolatile := func(parent map[string]any, key string) {
		if _, ok := parent[key]; !ok {
			t.Fatalf("expected volatile field %q present in wire doc", key)
		}
		parent[key] = "<volatile>"
	}
	setVolatile(doc, "executionId")
	metadata, ok := doc["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("metadata missing in wire doc")
	}
	setVolatile(metadata, "f")
	trajectory, ok := doc["trajectoryReference"].(map[string]any)
	if !ok {
		t.Fatalf("trajectoryReference missing in wire doc")
	}
	setVolatile(trajectory, "stepIndex")
	replaceKeyRecursively(doc, "messageId")
}

// replaceKeyRecursively 把树里所有同名键的值置为 <volatile>。
func replaceKeyRecursively(node any, key string) {
	switch typed := node.(type) {
	case map[string]any:
		for k, v := range typed {
			if k == key {
				typed[k] = "<volatile>"
				continue
			}
			replaceKeyRecursively(v, key)
		}
	case []any:
		for _, item := range typed {
			replaceKeyRecursively(item, key)
		}
	}
}

// marshalCanonicalJSON 输出确定性 JSON：键序固定、关 HTML 转义
// （golden 里 <volatile>、schema 字符串保持可读）。
func marshalCanonicalJSON(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func TestWireGolden(t *testing.T) {
	dir := filepath.Join("testdata", "wire")
	update := os.Getenv("UPDATE_GOLDEN") == "1"
	for name, testCase := range wireGoldenCases {
		t.Run(name, func(t *testing.T) {
			converted, _, err := buildRequest(testCase.request, Config{}, testCase.binding)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := protojson.Marshal(converted)
			if err != nil {
				t.Fatal(err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				t.Fatal(err)
			}
			normalizeVolatileWire(t, doc)
			canonical, err := marshalCanonicalJSON(doc)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join(dir, name+".json")
			if update {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, canonical, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden %s（UPDATE_GOLDEN=1 重新生成）: %v", golden, err)
			}
			if string(canonical) != string(want) {
				t.Fatalf("wire 形状漂移，与 %s 不一致：\n%s", golden, canonical)
			}
		})
	}
}
