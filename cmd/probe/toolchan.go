// toolchan 子命令：参数约束信息在不同投递通道下的可读性判别实验。
// 背景：代理把工具描述从 tools[].description/schema 注解剥除、改注
// system prompt 的 "# tools descriptions" 段（指纹对抗），导致只靠
// prose 承载的条件必填（"Required unless `cancel` is true"）对上游
// 模型不可见。本实验用合成工具 queue_reminder 的 seal 字段做判别：
// 其取值 "ZK9" 只写在被测通道里——模型命中 ZK9 说明该通道被读取，
// 字段缺席/自造值说明未被读取。
//
// 用法：go run ./cmd/probe toolchan <variant|all> [-n N] [-model M]
package main

import (
	"context"
	"flag"
	"fmt"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/randid"
)

// init 自注册进 commands——实验子命令不挤 main.go 的分派表，避免多会话
// 同树工作时互相踩这个热点文件。
func init() { commands["toolchan"] = cmdToolchan }

// toolchanDoc 是待传达的工具说明：seal 的需求语义与魔法值只存在于
// 这段文字中，其余字段用途自明。
const toolchanDoc = `Schedules a reminder for the user.

delay_seconds: seconds from now when the reminder fires.
label: one short phrase shown in the reminder list.
cancel: set to true to cancel the pending reminder instead of scheduling a new one.
seal: pass exactly "ZK9". Required unless "cancel" is true.`

// toolchanPromptSection 是生产形态（formatToolDescription 编号句 +
// <tool> 包裹）注入 system prompt 的全文版本。
const toolchanPromptSection = `# tools descriptions
<tool name="queue_reminder">
1. Schedules a reminder for the user.

2. delay_seconds: seconds from now when the reminder fires.
3. label: one short phrase shown in the reminder list.
4. cancel: set to true to cancel the pending reminder instead of scheduling a new one.
5. seal: pass exactly "ZK9".
6. Required unless "cancel" is true.
</tool>`

// toolchanDigestSection 是拟议修复形态的注入段：短导引 + Parameters
// 摘要（字段级描述被 schema 剥除前提升至此），模拟 compact 档下
// prose 被截但摘要完整保留的下发形态。
const toolchanDigestSection = `# tools descriptions
<tool name="queue_reminder">
1. Schedules a reminder for the user.

Parameters:
- delay_seconds (number): seconds from now when the reminder fires.
- label (string): one short phrase shown in the reminder list.
- cancel (boolean): set to true to cancel the pending reminder instead of scheduling a new one.
- seal (string): pass exactly "ZK9". Required unless "cancel" is true.
</tool>`

// toolchanTruncSection 复现事故形态：compact 档 400 runes 截断后，
// 字段语义与 seal 需求全部被砍掉。
const toolchanTruncSection = `# tools descriptions
<tool name="queue_reminder">
1. Schedules a reminder for the user.
</tool>`

const (
	toolchanSchemaBare = `{"type":"object","additionalProperties":false,"properties":{"cancel":{"type":"boolean"},"delay_seconds":{"type":"number"},"label":{"type":"string"},"seal":{"type":"string"}}}`

	toolchanSchemaFieldDesc = `{"type":"object","additionalProperties":false,"properties":{"cancel":{"type":"boolean","description":"set to true to cancel the pending reminder instead of scheduling a new one"},"delay_seconds":{"type":"number","description":"seconds from now when the reminder fires"},"label":{"type":"string","description":"one short phrase shown in the reminder list"},"seal":{"type":"string","description":"pass exactly \"ZK9\". Required unless \"cancel\" is true"}}}`

	toolchanSchemaRequired = `{"type":"object","additionalProperties":false,"properties":{"cancel":{"type":"boolean"},"delay_seconds":{"type":"number"},"label":{"type":"string"},"seal":{"type":"string"}},"required":["delay_seconds","label","seal"]}`

	toolchanSchemaAnyOf = `{"type":"object","additionalProperties":false,"properties":{"cancel":{"type":"boolean"},"delay_seconds":{"type":"number"},"label":{"type":"string"},"seal":{"type":"string"}},"anyOf":[{"required":["cancel"]},{"required":["delay_seconds","label","seal"]}]}`

	toolchanSchemaFieldReq = `{"type":"object","additionalProperties":false,"properties":{"cancel":{"type":"boolean","description":"set to true to cancel the pending reminder instead of scheduling a new one"},"delay_seconds":{"type":"number","description":"seconds from now when the reminder fires"},"label":{"type":"string","description":"one short phrase shown in the reminder list"},"seal":{"type":"string","description":"pass exactly \"ZK9\". Required unless \"cancel\" is true"}},"required":["delay_seconds","label","seal"]}`
)

// ---- 真实 ScheduleWakeup 素材（dump 20260917-231039 提取）----

// swSchemaStripped 是生产剥离后的 wire schema：五字段裸类型，无
// required、无字段描述。
const swSchemaStripped = `{"additionalProperties":false,"properties":{"delaySeconds":{"type":"number"},"noop":{"type":"boolean"},"prompt":{"type":"string"},"reason":{"type":"string"},"stop":{"type":"boolean"}},"type":"object"}`

// swSchemaAnyOf 是 anyOf 条件必填合成形态：stop 分支或全量分支。
const swSchemaAnyOf = `{"additionalProperties":false,"properties":{"delaySeconds":{"type":"number"},"noop":{"type":"boolean"},"prompt":{"type":"string"},"reason":{"type":"string"},"stop":{"type":"boolean"}},"type":"object","anyOf":[{"required":["stop"]},{"required":["delaySeconds","noop","prompt","reason"]}]}`

// swSchemaReqUnion 是全量 required 降级形态。
const swSchemaReqUnion = `{"additionalProperties":false,"properties":{"delaySeconds":{"type":"number"},"noop":{"type":"boolean"},"prompt":{"type":"string"},"reason":{"type":"string"},"stop":{"type":"boolean"}},"type":"object","required":["delaySeconds","noop","prompt","reason"]}`

// swTruncSection 是 dump 03 里实际上线的注入段（399 chars 截断版）。
const swTruncSection = `# tools descriptions
<tool name="ScheduleWakeup">
1. Schedule when to resume work in /loop dynamic mode — the user invoked /loop without an interval, asking you to self-pace iterations of a specific task.

2. Do NOT schedule a short-interval wakeup to poll for background work you started — when harness-tracked work finishes, you are re-invoked automatically, so polling is wasted.
3. Instead schedule a long fallback (1200s+) so the loop survives…
</tool>`

// swDigestSection 是拟议修复形态：同样截断的导引 + Parameters 摘要。
const swDigestSection = `# tools descriptions
<tool name="ScheduleWakeup">
1. Schedule when to resume work in /loop dynamic mode — the user invoked /loop without an interval, asking you to self-pace iterations of a specific task.

2. Do NOT schedule a short-interval wakeup to poll for background work you started — when harness-tracked work finishes, you are re-invoked automatically, so polling is wasted.
3. Instead schedule a long fallback (1200s+) so the loop survives…

Parameters:
- delaySeconds (number): Seconds from now to wake up. Clamped to [60, 3600] by the runtime. Required unless "stop" is true.
- noop (boolean): true = nothing changed (you checked and there is nothing to report). false = something happened worth keeping (edited a file, posted a message, advanced state, surfaced a finding). Consecutive noop:true ticks are collapsed in the user's terminal view and tracked as a streak. Required unless "stop" is true.
- prompt (string): The /loop input to fire on wake-up. Pass the same /loop input verbatim each turn so the next firing re-enters the skill and continues the loop. For autonomous /loop (no user prompt), pass the literal sentinel "<<autonomous-loop-dynamic>>" instead (the dynamic-pacing variant, not the CronCreate-mode "<<autonomous-loop>>"). Required unless "stop" is true.
- reason (string): One short sentence explaining the chosen delay. Goes to telemetry and is shown to the user. Be specific. Required unless "stop" is true.
- stop (boolean): Set to true to end the dynamic loop immediately instead of scheduling another wakeup. When true, all other fields are ignored and no further wakeups fire.
</tool>`

// swFullSection 是完整描述注入（full 档存活的对照）。
const swFullSection = `# tools descriptions
<tool name="ScheduleWakeup">
1. Schedule when to resume work in /loop dynamic mode — the user invoked /loop without an interval, asking you to self-pace iterations of a specific task.

2. Do NOT schedule a short-interval wakeup to poll for background work you started — when harness-tracked work finishes, you are re-invoked automatically, so polling is wasted.
3. Instead schedule a long fallback (1200s+) so the loop survives if the work hangs or never notifies.
4. The exception is external work the harness cannot track (a CI run, a deploy, a remote queue) — there, pick a delay matched to how fast that state actually changes.
5. Pass the same /loop prompt back via "prompt" each turn so the next firing repeats the task.
6. For an autonomous /loop (no user prompt), pass the literal sentinel "<<autonomous-loop-dynamic>>" as "prompt" instead — the runtime resolves it back to the autonomous-loop instructions at fire time.
7. To end the loop, call this tool with "stop: true" (omit every other field) — the loop ends immediately and no further wakeups fire.
8. Set "noop: true" if nothing changed — you checked and there's nothing to report.
9. Set "noop: false" if something happened worth keeping.
10. Omit "noop" when stopping ("stop: true").

Parameters:
- delaySeconds (number): Seconds from now to wake up. Clamped to [60, 3600] by the runtime. Required unless "stop" is true.
- noop (boolean): Required unless "stop" is true.
- prompt (string): The /loop input to fire on wake-up. Required unless "stop" is true.
- reason (string): One short sentence explaining the chosen delay. Required unless "stop" is true.
- stop (boolean): Set to true to end the dynamic loop immediately instead of scheduling another wakeup.
</tool>`

// toolchanVariant 一组实验变体：systemExtra 追加到 system prompt 尾，
// desc/schema 是 wire 上 tools[] 的两槽。
type toolchanVariant struct {
	desc           string
	schema         string
	systemExtra    string
	swName         bool // true 时用真实工具名 ScheduleWakeup 与 sw 系素材
	histMissingReq bool // true 时改发「缺 required 字段的历史调用」回放
}

var toolchanVariants = map[string]toolchanVariant{
	// 对照基线：哪里都没写需求，模型只能盲猜 seal。
	"none": {desc: "queue_reminder", schema: toolchanSchemaBare},
	// C1: 真实 CLI 通道——完整描述进 tools[].description。
	"desc": {desc: toolchanDoc, schema: toolchanSchemaBare},
	// C2: schema 字段级描述（我们 strip 掉的那条）。
	"field": {desc: "queue_reminder", schema: toolchanSchemaFieldDesc},
	// C3: 现行生产通道——全文注入 system prompt。
	"prompt": {desc: "queue_reminder", schema: toolchanSchemaBare, systemExtra: toolchanPromptSection},
	// C3b: 现行 compact 截断形态（事故复现）。
	"prompt-trunc": {desc: "queue_reminder", schema: toolchanSchemaBare, systemExtra: toolchanTruncSection},
	// C3c: 拟议修复形态——短导引 + Parameters 摘要。
	"prompt-digest": {desc: "queue_reminder", schema: toolchanSchemaBare, systemExtra: toolchanDigestSection},
	// C4a: 裸 required[]——只传「要填」，不传取值。
	"req": {desc: "queue_reminder", schema: toolchanSchemaRequired},
	// C4b: anyOf 条件必填合成——stop 分支或全量分支。
	"anyof": {desc: "queue_reminder", schema: toolchanSchemaAnyOf},
	// C2+C4: 字段描述 + required[] 双满（真实 CLI schema 丰富度）。
	"fieldreq": {desc: "queue_reminder", schema: toolchanSchemaFieldReq},
	// ---- sw-* 系：真实 ScheduleWakeup 定义的事故复现与修复对照 ----
	// schema 取 dump 03 的剥离形态（无字段描述、无 required）；
	// 各变体只差信息通道。
	"sw-prod":   {desc: "ScheduleWakeup", schema: swSchemaStripped, systemExtra: swTruncSection, swName: true},
	"sw-full":   {desc: "ScheduleWakeup", schema: swSchemaStripped, systemExtra: swFullSection, swName: true},
	"sw-digest": {desc: "ScheduleWakeup", schema: swSchemaStripped, systemExtra: swDigestSection, swName: true},
	"sw-anyof":  {desc: "ScheduleWakeup", schema: swSchemaAnyOf, systemExtra: swTruncSection, swName: true},
	// sw-req-union：把条件必填降级成全量 required——模型会填满，
	// 但 stop 语义被错误吞并（预期 stop 调用也带上 delay 等）。
	"sw-req-union": {desc: "ScheduleWakeup", schema: swSchemaReqUnion, systemExtra: swTruncSection, swName: true},
	// hist-missing-req：历史回放缺 required 字段的已完成调用，
	// 探上游是否服务端校验 schema 约束（决定合成 required/anyOf
	// 是纯建议还是硬门槛）。
	"hist-missing-req": {desc: "queue_reminder", schema: toolchanSchemaRequired, histMissingReq: true},
}

var toolchanOrder = []string{"none", "desc", "field", "prompt", "prompt-trunc", "prompt-digest", "req", "anyof", "fieldreq",
	"sw-prod", "sw-full", "sw-digest", "sw-anyof", "sw-req-union", "hist-missing-req"}

// cmdToolchan 逐变体发 GetChatMessage：tool_choice 指名 queue_reminder
// 强制调用，用户消息只给 delay_seconds/label 的动机不提 seal——
// 从 argumentsJson 里 seal 的存在性与取值判定通道可读性。
func cmdToolchan(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, _ devinprotoconnect.ExaLanguageServerPb_LanguageServerServiceClient, token string, argv []string) error {
	fs := flag.NewFlagSet("toolchan", flag.ContinueOnError)
	model := fs.String("model", "swe-2-max", "")
	reps := fs.Int("n", 1, "repetitions per variant")
	userText := fs.String("prompt", `Please queue a reminder 300 seconds from now labeled "build check".`, "")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	promptSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "prompt" {
			promptSet = true
		}
	})
	args := fs.Args()
	if len(args) == 0 {
		return fmt.Errorf("toolchan needs a variant name or 'all' (variants: %v)", toolchanOrder)
	}
	var run []string
	if args[0] == "all" {
		run = toolchanOrder
	} else {
		run = args
	}
	for _, name := range run {
		variant, ok := toolchanVariants[name]
		if !ok {
			return fmt.Errorf("unknown variant %q (have %v)", name, toolchanOrder)
		}
		for rep := 0; rep < *reps; rep++ {
			toolName := "queue_reminder"
			text := *userText
			if variant.swName {
				toolName = "ScheduleWakeup"
				if !promptSet {
					// sw 系默认用户意图：只给 delay 动机，prompt/reason/
					// noop 的填写完全取决于被测通道的信息到达率。
					text = "Schedule a wakeup 1200 seconds from now to keep checking the deploy."
				}
			}
			systemPrompt := "You are a helpful assistant."
			if variant.systemExtra != "" {
				systemPrompt += "\n\n" + variant.systemExtra
			}
			prompts := []*devinproto.ExaChatPb_ChatMessagePrompt{userMsg(text)}
			toolChoice := &devinproto.ExaChatPb_ChatToolChoice{
				Choice: &devinproto.ExaChatPb_ChatToolChoice_ToolName{ToolName: toolName},
			}
			if variant.histMissingReq {
				// 历史里 assistant 的 queue_reminder 调用缺 required 的
				// label/seal；不走 tool_choice，看上游是否拒绝该回放。
				prompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
					userMsg("Set a reminder for 300 seconds labeled 'build check'."),
					assistantCallMsg("c1", "queue_reminder", `{"delay_seconds":300}`),
					toolResultMsg("c1", "queued"),
					userMsg("ok?"),
				}
				toolChoice = nil
			}
			req := &devinproto.GetChatMessageRequest{
				Metadata:           metadata(token, true),
				Prompt:             proto.String(systemPrompt),
				ChatModelUid:       proto.String(aliasModel(*model)),
				RequestType:        devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
				Configuration:      defaultCompletionConfig(),
				PlannerMode:        devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
				ExecutionId:        proto.String(randid.UUID()),
				ChatMessagePrompts: prompts,
				Tools: []*devinproto.ExaChatPb_ChatToolDefinition{{
					Name:             proto.String(toolName),
					Description:      proto.String(variant.desc),
					JsonSchemaString: proto.String(variant.schema),
				}},
				ToolChoice: toolChoice,
			}
			fmt.Printf("=== variant %s rep %d\n", name, rep)
			if err := runStream(ctx, client, req, false, ""); err != nil {
				return err
			}
		}
	}
	return nil
}
