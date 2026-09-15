// probe 是对 Devin 上游做受控实验的命令行工具。
// 用法：go run ./cmd/probe <subcommand> [flags]
// token 从 DEVIN_TOKEN 环境变量读；缺省时回落解析仓库根目录 config.yaml 的 devin.token。
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	devinproto "local/devinproto"
	"local/devinproto/devinprotoconnect"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/WncFht/devin2api/internal/adapter/devin"
	"github.com/WncFht/devin2api/internal/config"
	"github.com/WncFht/devin2api/internal/httpproxy"
	"github.com/WncFht/devin2api/internal/randid"
	"github.com/WncFht/devin2api/internal/upstream"
)

const defaultBaseURL = "https://server.codeium.com"

var marshal = protojson.MarshalOptions{EmitUnpopulated: false, UseEnumNumbers: false}

// commands 是子命令分派表；usage() 的子命令列表须与它保持一致。
var commands = map[string]func(context.Context, devinprotoconnect.ApiServerServiceClient, string, []string) error{
	"configs": cmdConfigs,
	"status":  cmdStatus,
	"assign":  cmdAssign,
	"chat":    cmdChat,
	"replay":  cmdReplay,
	"hist":    cmdHist,
	"rerun":   cmdRerun,
	"bigctx":  cmdBigctx,
	"misc":    cmdMisc,
	"edge":    cmdEdge,
}

// 客户端身份三元组在 main 里从 config 的 devin.client_* 解析一次
// （缺省回落到与真实 Devin CLI 抓包一致的默认值），probe 流量与
// 代理走同一套指纹，实验结果才可迁移。
var clientName, clientVersion, clientOS string

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	// 先校验子命令再解析 token：未知子命令应直接打 usage，
	// 不能被「无 token」错误抢在前面。
	run, ok := commands[os.Args[1]]
	if !ok {
		usage()
		os.Exit(2)
	}
	// config.yaml 读取失败按零值继续：token 与身份仍有环境变量、
	// CLI 凭证文件与默认常量的完整回落链。注意 Load 走全量 Validate
	// （KnownFields+server.listen 必填）——校验失败的 yaml 被整体丢弃，
	// 连其中本可用的 devin.token 也不可见；probe 场景下靠回落链兜底。
	cfg, _ := config.Load("config.yaml")
	token := resolveToken(cfg)
	if token == "" {
		fmt.Fprintln(os.Stderr, "no token: set DEVIN_TOKEN or devin.token in config.yaml")
		os.Exit(1)
	}
	clientName, clientVersion, clientOS = (devin.Config{
		ClientName:    cfg.Devin.ClientName,
		ClientVersion: cfg.Devin.ClientVersion,
		ClientOS:      cfg.Devin.ClientOS,
	}).ClientIdentity()
	baseURL := cfg.Devin.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	// 代理与 HTTP/1.1 强制和生产链路同源：probe 走 h2 而服务走 h1 时，
	// 帧行为/并发结论不可直接迁移。
	forceHTTP1 := true
	if cfg.Devin.ForceHTTP1 != nil {
		forceHTTP1 = *cfg.Devin.ForceHTTP1
	}
	transport, err := httpproxy.NewTransport(cfg.Devin.Proxy, forceHTTP1)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
	client := devinprotoconnect.NewApiServerServiceClient(
		&http.Client{Transport: upstream.NewBasicAuthTransportFunc(transport, func() string { return token })},
		baseURL)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	if err := run(ctx, client, token, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "ERR:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `subcommands:
  configs                     dump GetCliModelConfigs (raw + router/feature summary)
  status                      CheckChatCapacity + CheckUserMessageRateLimit + GetModelStatuses + GetModelProviders
  assign <uid> [uid...]       AssignModel for each router uid
  chat [flags]                one GetChatMessage stream, dump all frames
    -model uid                chat_model_uid (default swe-2-max)
    -prompt text              user prompt (default "Reply exactly: pong")
    -system text              system prompt (default "You are a helpful assistant.")
    -system-as-message        send system prompt as SYSTEM_PROMPT-source message, drop top-level prompt
    -system-empty             send prompt field as explicit empty string
    -tool name                add a JSON-schema tool (repeatable: -tool a -tool b)
    -tool-schema json         schema for the corresponding -tool (positional)
    -custom-tool name         add is_custom_tool with lark grammar (name)
    -raw-schema               send invalid json_schema_string on tools
    -tool-extras              strict+read_only_hint+server_name+attribution on tools
    -tool-choice opt:v|tool:v tool_choice oneof
    -disable-parallel         disable_parallel_tool_calls=true
    -provider-source N|name   provider_source enum
    -prompt-id s              prompt_id field
    -num-tokens n             per-message num_tokens on last user msg
    -planner-mode N|name      planner_mode enum
    -step-type N|name         trajectory step_type enum
    -step-index n             trajectoryReference.step_index (session-monotonic counter)
    -request-type N|name      request_type enum
    -language N|name          language enum
    -chat-model-name s        chat_model_name field
    -no-fingerprint           omit metadata.f
    -no-ids                   omit trajectory/cascade ids
    -trajectory-id s          explicit trajectory_id (share across calls)
    -cascade-id s             explicit cascade_id (share across calls to test concurrency)
    -max-tokens n             configuration.max_tokens
    -num-completions n        configuration.num_completions
    -stop-pattern s           configuration.stop_patterns[0]
    -temperature f            configuration.temperature (default 1)
    -top-p f                  configuration.top_p (default 0.95)
    -top-k n                  configuration.top_k (default 40)
    -images n                 attach n copies of a tiny png to the user msg
    -internal-model N         use_internal_chat_model + internal_chat_model=N
    -assign-jwt s             model_assignment_jwt
    -resolve                  run AssignModel first, use returned uid+jwt
    -resolve-only             run AssignModel but keep chat_model_uid (jwt/model mismatch test)
    -router uid               router uid for -resolve (defaults to -model)
    -meta-extras              send session_id/request_id/device_fingerprint/disable_telemetry
    -frames                   print every frame protojson (default: field inventory + text)
    -dump dir                 write each frame protojson to dir/NN.json
  replay [flags]              two-step: call once, then replay assistant msg with variants
    -model uid                chat_model_uid (default swe-2-max)
    -prompt text              step-1 user prompt
    -variant name             with-sig|with-ids|no-sig|bogus-sig|bogus-sig-typed|sig-only|mutated-thinking|no-thinking
  hist [flags]                synthetic text+call+result history in a chosen wire shape
    -shape name               merged|merged-single|split|split-single (default merged)
    -model uid                chat_model_uid (default swe-2-max)
  rerun -file 03.json [-n N]  replay a captured GetChatMessageRequest N times,
                              print stop_reason + calls + text tail per run
  bigctx [flags]              send ~N KB single user message, observe error code
    -kb n                     payload size (default 1024)
    -model uid                chat_model_uid (default swe-2-max)
  misc                        adjacent endpoints: embeddings/extchat/status/config/command configs
  edge <case> [flags]         targeted edge-case histories (case names in source switch)
    -model uid                chat_model_uid (default swe-2-max)
    -image-file path          attach a real png instead of the tiny 1x1 blue png
    -prompt text              user prompt text for prompt-driven cases (e.g. user-image-prompt)`)
}

// resolveToken 解析上游凭据：DEVIN_TOKEN 环境变量优先（临时换 token
// 不改配置），随后是 config.yaml 的 devin.token（Load 内部已含
// env/CLI 凭证兜底）；config.yaml 缺失时直接走 Devin CLI 凭证发现链。
func resolveToken(cfg config.Config) string {
	if token := os.Getenv("DEVIN_TOKEN"); token != "" {
		return token
	}
	if cfg.Devin.Token != "" {
		return cfg.Devin.Token
	}
	return config.ResolveDevinToken()
}

// metadata 组装上游 Metadata；fingerprint=false 时不带设备指纹字段。
// 身份字段取 main 解析出的 clientName/Version/OS，与代理发出的请求同源。
func metadata(token string, fingerprint bool) *devinproto.ExaCodeiumCommonPb_Metadata {
	fingerprintBytes := 0
	if fingerprint {
		fingerprintBytes = 366
	}
	return upstream.BuildMetadata(token, clientName, clientVersion, clientOS, fingerprintBytes)
}

func j(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// defaultCompletionConfig 返回与真实 CLI 抓包一致的默认补全配置；
// 各子命令在其上按实验变量覆写个别字段。
func defaultCompletionConfig() *devinproto.ExaCodeiumCommonPb_CompletionConfiguration {
	return &devinproto.ExaCodeiumCommonPb_CompletionConfiguration{
		NumCompletions: proto.Uint64(1),
		MaxTokens:      proto.Uint64(128000),
		MaxNewlines:    proto.Uint64(400),
		Temperature:    proto.Float64(1),
		TopK:           proto.Uint64(40),
		TopP:           proto.Float64(0.95),
	}
}

// ---- 消息构造器：hist/edge 的各 case 共用同一套 wire 形态 ----

func userMsg(text string) *devinproto.ExaChatPb_ChatMessagePrompt {
	return &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum(),
		Prompt:    proto.String(text),
	}
}

// assistantMsg 返回空的 SYSTEM 源消息——上游 wire 里 assistant 回合的
// 承载形态；Prompt/Thinking/ToolCalls 由调用方按实验变量填。
func assistantMsg() *devinproto.ExaChatPb_ChatMessagePrompt {
	return &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM.Enum(),
	}
}

func assistantTextMsg(text string) *devinproto.ExaChatPb_ChatMessagePrompt {
	m := assistantMsg()
	m.Prompt = proto.String(text)
	return m
}

func assistantCallMsg(id, name, argsJSON string) *devinproto.ExaChatPb_ChatMessagePrompt {
	m := assistantMsg()
	m.ToolCalls = []*devinproto.ExaCodeiumCommonPb_ChatToolCall{toolCall(id, name, argsJSON)}
	return m
}

func toolResultMsg(callID, text string) *devinproto.ExaChatPb_ChatMessagePrompt {
	return &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId:  proto.String(randid.UUID()),
		Source:     devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL.Enum(),
		Prompt:     proto.String(text),
		ToolCallId: proto.String(callID),
	}
}

func toolCall(id, name, argsJSON string) *devinproto.ExaCodeiumCommonPb_ChatToolCall {
	return &devinproto.ExaCodeiumCommonPb_ChatToolCall{
		Id: proto.String(id), Name: proto.String(name), ArgumentsJson: proto.String(argsJSON),
	}
}

// ---- configs ----

func cmdConfigs(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, _ []string) error {
	resp, err := client.GetCliModelConfigs(ctx, connect.NewRequest(&devinproto.GetCliModelConfigsRequest{
		Metadata: metadata(token, true),
	}))
	if err != nil {
		return fmt.Errorf("GetCliModelConfigs: %w", err)
	}
	raw, _ := marshal.Marshal(resp.Msg)
	_ = os.MkdirAll("outputs/probe", 0o755)
	if err := os.WriteFile("outputs/probe/cli-model-configs.json", raw, 0o644); err != nil {
		return err
	}
	fmt.Println("subagent_default_model_uid:", resp.Msg.GetSubagentDefaultModelUid())
	fmt.Printf("%-28s %-6s %-7s %-8s %-9s %-8s %s\n", "uid", "router", "family", "tools", "thinking", "images", "smart_friend")
	for _, c := range resp.Msg.GetClientModelConfigs() {
		uid := c.GetModelUid()
		if uid == "" && c.GetModelOrAlias() != nil {
			uid = c.GetModelOrAlias().GetModelUid()
		}
		mi := c.GetModelInfo()
		var router, family, tools, thinking, images, friend string
		if mi != nil {
			router = fmt.Sprint(mi.GetIsModelRouter())
			family = mi.GetModelFamilyUid()
			feat := mi.GetModelFeatures()
			if feat != nil {
				tools = fmt.Sprint(feat.GetSupportsToolCalls())
				thinking = fmt.Sprint(feat.GetSupportsThinking())
				images = fmt.Sprint(feat.GetSupportsImages())
			}
		}
		friend = c.GetSmartFriendModelUid()
		fmt.Printf("%-28s %-6s %-7s %-8s %-9s %-8s %s\n", uid, router, family, tools, thinking, images, friend)
	}
	fmt.Println("raw ->", "outputs/probe/cli-model-configs.json")
	return nil
}

// ---- status ----

func cmdStatus(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, _ []string) error {
	if r, err := client.CheckChatCapacity(ctx, connect.NewRequest(&devinproto.CheckChatCapacityRequest{Metadata: metadata(token, true)})); err != nil {
		fmt.Println("CheckChatCapacity ERR:", err)
	} else {
		b, _ := marshal.Marshal(r.Msg)
		fmt.Println("CheckChatCapacity:", string(b))
	}
	if r, err := client.CheckUserMessageRateLimit(ctx, connect.NewRequest(&devinproto.CheckUserMessageRateLimitRequest{Metadata: metadata(token, true), ModelUid: proto.String("swe-2-max")})); err != nil {
		fmt.Println("CheckUserMessageRateLimit ERR:", err)
	} else {
		b, _ := marshal.Marshal(r.Msg)
		fmt.Println("CheckUserMessageRateLimit:", string(b))
	}
	if r, err := client.GetModelStatuses(ctx, connect.NewRequest(&devinproto.GetModelStatusesRequest{Metadata: metadata(token, true)})); err != nil {
		fmt.Println("GetModelStatuses ERR:", err)
	} else {
		b, _ := marshal.Marshal(r.Msg)
		fmt.Println("GetModelStatuses:", string(b))
	}
	if r, err := client.GetModelProviders(ctx, connect.NewRequest(&devinproto.GetModelProvidersRequest{})); err != nil {
		fmt.Println("GetModelProviders ERR:", err)
	} else {
		b, _ := marshal.Marshal(r.Msg)
		fmt.Println("GetModelProviders:", string(b))
	}
	return nil
}

// ---- assign ----

func cmdAssign(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("assign needs at least one uid")
	}
	for _, uid := range args {
		resp, err := client.AssignModel(ctx, connect.NewRequest(&devinproto.AssignModelRequest{
			Metadata:       metadata(token, true),
			ModelRouterUid: proto.String(uid),
			CascadeId:      proto.String(randid.UUID()),
		}))
		if err != nil {
			fmt.Printf("%-28s ERR %v\n", uid, err)
			continue
		}
		b, _ := marshal.Marshal(resp.Msg)
		fmt.Printf("%-28s -> %s\n", uid, string(b))
	}
	return nil
}

// ---- chat ----

type toolList []string

func (t *toolList) String() string { return strings.Join(*t, ",") }
func (t *toolList) Set(v string) error {
	*t = append(*t, v)
	return nil
}

func cmdChat(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, args []string) error {
	fs := flag.NewFlagSet("chat", flag.ContinueOnError)
	model := fs.String("model", "swe-2-max", "")
	userPrompt := fs.String("prompt", "Reply exactly: pong", "")
	system := fs.String("system", "You are a helpful assistant.", "")
	var tools toolList
	fs.Var(&tools, "tool", "")
	var toolSchemas toolList
	fs.Var(&toolSchemas, "tool-schema", "json schema for the corresponding -tool (positional)")
	customTool := fs.String("custom-tool", "", "add is_custom_tool with lark grammar (name)")
	rawSchema := fs.Bool("raw-schema", false, "send invalid json_schema_string on tools")
	toolExtras := fs.Bool("tool-extras", false, "strict+read_only_hint+server_name+attribution on tools")
	sysAsMsg := fs.Bool("system-as-message", false, "send system prompt as SYSTEM_PROMPT-source message, drop top-level prompt")
	emptySys := fs.Bool("system-empty", false, "send prompt field as explicit empty string")
	toolChoice := fs.String("tool-choice", "", "")
	disableParallel := fs.Bool("disable-parallel", false, "")
	providerSource := fs.String("provider-source", "", "")
	promptID := fs.String("prompt-id", "", "")
	numTokens := fs.Int("num-tokens", 0, "")
	plannerMode := fs.String("planner-mode", "", "")
	stepType := fs.String("step-type", "", "")
	requestType := fs.String("request-type", "", "")
	language := fs.String("language", "", "")
	chatModelName := fs.String("chat-model-name", "", "")
	noFingerprint := fs.Bool("no-fingerprint", false, "")
	noIDs := fs.Bool("no-ids", false, "")
	maxTokens := fs.Int("max-tokens", 0, "")
	temperature := fs.Float64("temperature", -1, "configuration.temperature (<0 = keep default 1)")
	topP := fs.Float64("top-p", -1, "configuration.top_p (<0 = keep default 0.95)")
	topK := fs.Int("top-k", -1, "configuration.top_k (<0 = keep default 40)")
	trajectoryID := fs.String("trajectory-id", "", "explicit trajectory_id (share across calls for session continuation)")
	stepIndex := fs.Int("step-index", -1, "trajectoryReference.step_index (session-monotonic counter, real CLI sends it)")
	images := fs.Int("images", 0, "")
	internalModel := fs.Int("internal-model", 0, "")
	assignJWT := fs.String("assign-jwt", "", "model_assignment_jwt")
	resolveModel := fs.Bool("resolve", false, "run AssignModel first, use returned uid+jwt")
	resolveOnly := fs.Bool("resolve-only", false, "run AssignModel but keep chat_model_uid (jwt/model mismatch test)")
	routerUID := fs.String("router", "", "router uid for -resolve (defaults to -model)")
	metaExtras := fs.Bool("meta-extras", false, "send session_id/request_id/device_fingerprint/disable_telemetry")
	numCompletions := fs.Int("num-completions", 0, "configuration.num_completions")
	stopPattern := fs.String("stop-pattern", "", "configuration.stop_patterns[0]")
	cascadeID := fs.String("cascade-id", "", "explicit cascade_id (share across calls to test concurrency)")
	frames := fs.Bool("frames", false, "")
	dumpDir := fs.String("dump", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var sharedCascade string
	if *resolveModel {
		sharedCascade = randid.UUID()
		router := *routerUID
		if router == "" {
			router = *model
		}
		ar, err := client.AssignModel(ctx, connect.NewRequest(&devinproto.AssignModelRequest{
			Metadata:       metadata(token, true),
			ModelRouterUid: proto.String(router),
			CascadeId:      proto.String(sharedCascade),
		}))
		if err != nil {
			return fmt.Errorf("AssignModel(%s): %w", *model, err)
		}
		a := ar.Msg.GetAssignment()
		fmt.Printf("== assigned: uid=%s harness=%v jwt_len=%d\n", a.GetModelUid(), a.GetHarnessUids(), len(a.GetAssignmentJwt()))
		if !*resolveOnly {
			*model = a.GetModelUid()
		}
		*assignJWT = a.GetAssignmentJwt()
	}

	sysPrompt := *system
	if *sysAsMsg {
		sysPrompt = ""
	}
	m := metadata(token, !*noFingerprint)
	if *metaExtras {
		m.SessionId = proto.String(randid.UUID())
		m.RequestId = proto.Uint64(42)
		if fp, ferr := randid.Hex(32); ferr == nil {
			m.DeviceFingerprint = proto.String(fp)
		}
		m.DisableTelemetry = proto.Bool(true)
		m.UserAgent = proto.String("devin/" + clientVersion)
	}
	req := &devinproto.GetChatMessageRequest{
		Metadata:     m,
		Prompt:       nonEmpty(sysPrompt),
		ChatModelUid: nonEmpty(*model),
	}
	if *emptySys {
		req.Prompt = proto.String("")
	}
	req.RequestType = devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum()
	req.Configuration = defaultCompletionConfig()
	req.PlannerMode = devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum()
	req.ExecutionId = proto.String(randid.UUID())
	if *numCompletions > 0 {
		req.Configuration.NumCompletions = proto.Uint64(uint64(*numCompletions))
	}
	if *stopPattern != "" {
		req.Configuration.StopPatterns = []string{*stopPattern}
	}
	if *maxTokens > 0 {
		req.Configuration.MaxTokens = proto.Uint64(uint64(*maxTokens))
	}
	if *temperature >= 0 {
		req.Configuration.Temperature = proto.Float64(*temperature)
	}
	if *topP >= 0 {
		req.Configuration.TopP = proto.Float64(*topP)
	}
	if *topK >= 0 {
		req.Configuration.TopK = proto.Uint64(uint64(*topK))
	}
	if !*noIDs {
		if *cascadeID != "" {
			req.CascadeId = proto.String(*cascadeID)
		} else if sharedCascade != "" {
			req.CascadeId = proto.String(sharedCascade)
		} else {
			req.CascadeId = proto.String(randid.UUID())
		}
		trajID := randid.UUID()
		if *trajectoryID != "" {
			trajID = *trajectoryID
		}
		req.TrajectoryReference = &devinproto.ExaCortexPb_CortexTrajectoryReference{
			TrajectoryId:   proto.String(trajID),
			TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
			StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
		}
		if *stepIndex >= 0 {
			req.TrajectoryReference.StepIndex = proto.Int32(int32(*stepIndex))
		}
	}
	msg := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum(),
		Prompt:    proto.String(*userPrompt),
	}
	if *numTokens > 0 {
		msg.NumTokens = proto.Uint32(uint32(*numTokens))
	}
	for i := 0; i < *images; i++ {
		msg.Images = append(msg.Images, &devinproto.ExaCodeiumCommonPb_ImageData{
			Base64Data: proto.String(tinyPNG()),
			MimeType:   proto.String("image/png"),
		})
	}
	if *sysAsMsg {
		req.ChatMessagePrompts = append(req.ChatMessagePrompts, &devinproto.ExaChatPb_ChatMessagePrompt{
			MessageId: proto.String(randid.UUID()),
			Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM_PROMPT.Enum(),
			Prompt:    proto.String(*system),
		})
	}
	req.ChatMessagePrompts = append(req.ChatMessagePrompts, msg)

	schemaIdx := 0
	for _, name := range tools {
		schema := `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`
		if *rawSchema {
			schema = `this is not json`
		}
		if schemaIdx < len(toolSchemas) && toolSchemas[schemaIdx] != "" {
			schema = toolSchemas[schemaIdx]
		}
		schemaIdx++
		td := &devinproto.ExaChatPb_ChatToolDefinition{
			Name:             proto.String(name),
			Description:      proto.String(name + " tool"),
			JsonSchemaString: proto.String(schema),
		}
		if *toolExtras {
			td.Strict = proto.Bool(true)
			td.ReadOnlyHint = proto.Bool(true)
			td.ServerName = proto.String("mcp-server")
			td.AttributionFieldNames = []string{"path"}
		}
		req.Tools = append(req.Tools, td)
	}
	if *customTool != "" {
		req.Tools = append(req.Tools, &devinproto.ExaChatPb_ChatToolDefinition{
			Name:                    proto.String(*customTool),
			Description:             proto.String("apply a patch"),
			IsCustomTool:            proto.Bool(true),
			CustomToolGrammar:       proto.String(`start: "PATCH" /[a-zA-Z0-9_.\/-]+/ "END"`),
			CustomToolGrammarSyntax: proto.String("lark"),
		})
	}
	if *toolChoice != "" {
		tc := &devinproto.ExaChatPb_ChatToolChoice{}
		if v, ok := strings.CutPrefix(*toolChoice, "tool:"); ok {
			tc.Choice = &devinproto.ExaChatPb_ChatToolChoice_ToolName{ToolName: v}
		} else {
			v, _ := strings.CutPrefix(*toolChoice, "opt:")
			tc.Choice = &devinproto.ExaChatPb_ChatToolChoice_OptionName{OptionName: v}
		}
		req.ToolChoice = tc
	}
	if *disableParallel {
		req.DisableParallelToolCalls = proto.Bool(true)
	}
	if *providerSource != "" {
		v, err := enumByName[devinproto.ExaCodeiumCommonPb_ProviderSource](*providerSource, devinproto.ExaCodeiumCommonPb_ProviderSource_value)
		if err != nil {
			return err
		}
		req.ProviderSource = v.Enum()
	}
	if *promptID != "" {
		req.PromptId = proto.String(*promptID)
	}
	if *plannerMode != "" {
		v, err := enumByName[devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode](*plannerMode, devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_value)
		if err != nil {
			return err
		}
		req.PlannerMode = v.Enum()
	}
	if *stepType != "" {
		v, err := enumByName[devinproto.ExaCortexPb_CortexStepType](*stepType, devinproto.ExaCortexPb_CortexStepType_value)
		if err != nil {
			return err
		}
		if req.TrajectoryReference == nil {
			req.TrajectoryReference = &devinproto.ExaCortexPb_CortexTrajectoryReference{
				TrajectoryId:   proto.String(randid.UUID()),
				TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
			}
		}
		req.TrajectoryReference.StepType = v.Enum()
	}
	if *requestType != "" {
		v, err := enumByName[devinproto.ChatMessageRequestType](*requestType, devinproto.ChatMessageRequestType_value)
		if err != nil {
			return err
		}
		req.RequestType = v.Enum()
	}
	if *language != "" {
		v, err := enumByName[devinproto.ExaCodeiumCommonPb_Language](*language, devinproto.ExaCodeiumCommonPb_Language_value)
		if err != nil {
			return err
		}
		req.Language = v.Enum()
	}
	if *chatModelName != "" {
		req.ChatModelName = proto.String(*chatModelName)
	}
	if *internalModel != 0 {
		req.UseInternalChatModel = proto.Bool(true)
		req.InternalChatModel = devinproto.ExaCodeiumCommonPb_Model(*internalModel).Enum()
	}
	if *assignJWT != "" {
		req.ModelAssignmentJwt = proto.String(*assignJWT)
	}
	return runStream(ctx, client, req, *frames, *dumpDir)
}

// enumByName 支持数字、全名与唯一后缀（"CASCADE" → "…_TYPE_CASCADE"）。
// 后缀命中多个值时 map 遍历顺序不定会随机挑一个——歧义必须显式报错。
func enumByName[T interface {
	~int32
	Enum() *T
}](s string, values map[string]int32) (T, error) {
	var zero T
	if n, err := strconv.ParseInt(s, 10, 32); err == nil {
		return T(n), nil
	}
	up := strings.ToUpper(s)
	if v, ok := values[up]; ok {
		return T(v), nil
	}
	var found T
	matches := 0
	for name, v := range values {
		if strings.HasSuffix(name, "_"+up) {
			found = T(v)
			matches++
		}
	}
	switch matches {
	case 1:
		return found, nil
	case 0:
		return zero, fmt.Errorf("unknown enum %q", s)
	default:
		return zero, fmt.Errorf("ambiguous enum %q matches %d values", s, matches)
	}
}

func tinyPNG() string {
	// 1x1 纯蓝 PNG
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	return base64.StdEncoding.EncodeToString(b)
}

func runStream(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, req *devinproto.GetChatMessageRequest, showFrames bool, dumpDir string) error {
	reqJSON, _ := marshal.Marshal(req)
	fmt.Println("== request:", string(reqJSON)[:min(len(reqJSON), 2000)])
	stream, err := client.GetChatMessage(ctx, connect.NewRequest(req))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	fieldSeen := map[string]int{}
	usageSeen := map[string]string{}
	var text strings.Builder
	var thinking strings.Builder
	var calls []map[string]any
	n := 0
	stopReason := devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_UNSPECIFIED
	if dumpDir != "" {
		_ = os.MkdirAll(dumpDir, 0o755)
	}
	for stream.Receive() {
		msg := stream.Msg()
		n++
		b, _ := marshal.Marshal(msg)
		if dumpDir != "" {
			_ = os.WriteFile(fmt.Sprintf("%s/%03d.json", dumpDir, n), b, 0o644)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		for k := range m {
			fieldSeen[k]++
		}
		if u, ok := m["usage"].(map[string]any); ok {
			for k, v := range u {
				usageSeen[k] = j(v)
			}
		}
		if msg.GetStopReason() != devinproto.ExaCodeiumCommonPb_StopReason_ExaCodeiumCommonPb_StopReason_STOP_REASON_UNSPECIFIED {
			stopReason = msg.GetStopReason()
		}
		if showFrames {
			fmt.Printf("-- frame %d: %s\n", n, string(b))
		}
		text.WriteString(msg.GetDeltaText())
		thinking.WriteString(msg.GetDeltaThinking())
		for _, tc := range msg.GetDeltaToolCalls() {
			calls = append(calls, map[string]any{
				"id": tc.GetId(), "name": tc.GetName(), "args": tc.GetArgumentsJson(),
				"invalid_json_str": tc.GetInvalidJsonStr(), "invalid_json_err": tc.GetInvalidJsonErr(),
				"is_custom_tool_call": tc.GetIsCustomToolCall(),
			})
		}
	}
	if err := stream.Err(); err != nil {
		fmt.Printf("== stream err after %d frames: %v\n", n, err)
		dumpConnectErr(err)
	}
	fmt.Println("== headers:", j(stream.ResponseHeader()))
	fmt.Println("== trailers:", j(stream.ResponseTrailer()))
	if err := stream.Err(); err != nil {
		return nil
	}
	fmt.Printf("== %d frames\n", n)
	fmt.Println("== fields:", j(fieldSeen))
	fmt.Println("== usage:", j(usageSeen))
	fmt.Println("== stopReason:", stopReason)
	if thinking.Len() > 0 {
		fmt.Println("== thinking:", thinking.String()[:min(thinking.Len(), 300)])
	}
	fmt.Println("== text:", text.String())
	for _, c := range calls {
		fmt.Println("== call:", j(c))
	}
	return nil
}

// ---- replay: capture assistant output then replay with variants ----

func cmdReplay(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, args []string) error {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	model := fs.String("model", "swe-2-max", "")
	variant := fs.String("variant", "with-sig", "")
	q1Text := fs.String("prompt", "Think briefly, then reply with the single word: zebra", "")
	if err := fs.Parse(args); err != nil {
		return err
	}

	mk := func(msgs []*devinproto.ExaChatPb_ChatMessagePrompt) *devinproto.GetChatMessageRequest {
		return &devinproto.GetChatMessageRequest{
			Metadata:      metadata(token, true),
			Prompt:        proto.String("You are a helpful assistant."),
			ChatModelUid:  proto.String(*model),
			RequestType:   devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
			Configuration: defaultCompletionConfig(),
			CascadeId:     proto.String(randid.UUID()),
			PlannerMode:   devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
			ExecutionId:   proto.String(randid.UUID()),
			TrajectoryReference: &devinproto.ExaCortexPb_CortexTrajectoryReference{
				TrajectoryId:   proto.String(randid.UUID()),
				TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
				StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
			},
			ChatMessagePrompts: msgs,
		}
	}

	// step 1: ask a question that triggers thinking
	q1 := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum(),
		Prompt:    proto.String(*q1Text),
	}
	stream, err := client.GetChatMessage(ctx, connect.NewRequest(mk([]*devinproto.ExaChatPb_ChatMessagePrompt{q1})))
	if err != nil {
		return fmt.Errorf("step1 connect: %w", err)
	}
	var aText, aThinking, aSig, aSigType, aOutputID, aThinkingID, aPhase string
	var aRedacted bool
	var frames1 int
	for stream.Receive() {
		m := stream.Msg()
		frames1++
		aText += m.GetDeltaText()
		aThinking += m.GetDeltaThinking()
		aSig += m.GetDeltaSignature()
		if m.GetDeltaSignatureType() != "" {
			aSigType = m.GetDeltaSignatureType()
		}
		if m.GetOutputId() != "" {
			aOutputID = m.GetOutputId()
		}
		if m.GetThinkingId() != "" {
			aThinkingID = m.GetThinkingId()
		}
		if m.GetPhase() != "" {
			aPhase = m.GetPhase()
		}
		aRedacted = aRedacted || m.GetThinkingRedacted()
	}
	if err := stream.Err(); err != nil {
		return fmt.Errorf("step1 stream: %w", err)
	}
	fmt.Printf("step1: frames=%d text=%q thinking_len=%d sig_len=%d sig_type=%q output_id=%q thinking_id=%q phase=%q redacted=%v\n",
		frames1, aText, len(aThinking), len(aSig), aSigType, aOutputID, aThinkingID, aPhase, aRedacted)

	// step 2: replay the assistant message with the requested variant
	asst := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM.Enum(),
		Prompt:    proto.String(aText),
	}
	switch *variant {
	case "with-sig":
		if aThinking != "" {
			asst.Thinking = proto.String(aThinking)
		}
		if aSig != "" {
			asst.Signature = proto.String(aSig)
		}
	case "no-sig":
		if aThinking != "" {
			asst.Thinking = proto.String(aThinking)
		}
	case "bogus-sig":
		if aThinking != "" {
			asst.Thinking = proto.String(aThinking)
		}
		if aSig != "" {
			asst.Signature = proto.String("bogus-" + aSig[:min(len(aSig), 16)])
		}
	case "bogus-sig-typed":
		// 伪造签名 + 正确 signature_type：分离「type 错配」与「内容伪造」两个变量。
		if aThinking != "" {
			asst.Thinking = proto.String(aThinking)
		}
		if aSig != "" {
			asst.Signature = proto.String("bogus-" + aSig[:min(len(aSig), 16)])
			asst.SignatureType = proto.String(aSigType)
		}
	case "with-ids":
		if aThinking != "" {
			asst.Thinking = proto.String(aThinking)
		}
		if aSig != "" {
			asst.Signature = proto.String(aSig)
		}
		asst.OutputId = proto.String(aOutputID)
		asst.ThinkingId = proto.String(aThinkingID)
		if aSigType != "" {
			asst.SignatureType = proto.String(aSigType)
		}
		if aPhase != "" {
			asst.Phase = proto.String(aPhase)
		}
	case "sig-only":
		// signature without thinking text (openai reasoning-blob form)
		if aSig != "" {
			asst.Signature = proto.String(aSig)
		}
		if aSigType != "" {
			asst.SignatureType = proto.String(aSigType)
		}
	case "mutated-thinking":
		// thinking text altered but real signature kept (sanitizer-analog)
		if aThinking != "" {
			asst.Thinking = proto.String("COMPLETELY DIFFERENT reasoning about bananas.")
		}
		if aSig != "" {
			asst.Signature = proto.String(aSig)
		}
	case "no-thinking":
		// bare text only
	}
	q2 := &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum(),
		Prompt:    proto.String("What word did you just say? One word only."),
	}
	return runStream(ctx, client, mk([]*devinproto.ExaChatPb_ChatMessagePrompt{q1cpy(q1), asst, q2}), false, "")
}

func q1cpy(m *devinproto.ExaChatPb_ChatMessagePrompt) *devinproto.ExaChatPb_ChatMessagePrompt {
	return &devinproto.ExaChatPb_ChatMessagePrompt{
		MessageId: proto.String(randid.UUID()),
		Source:    m.Source,
		Prompt:    m.Prompt,
	}
}

// ---- hist: synthetic assistant-turn wire shapes ----

func cmdHist(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, args []string) error {
	fs := flag.NewFlagSet("hist", flag.ContinueOnError)
	shape := fs.String("shape", "merged", "")
	model := fs.String("model", "swe-2-max", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	call1 := toolCall("chatcmpl-tool-aaa1", "exec", `{"command":"ls"}`)
	call2 := toolCall("chatcmpl-tool-bbb2", "read_file", `{"path":"README.md"}`)
	thinking := "I should list the directory and read the readme in parallel."
	var asst []*devinproto.ExaChatPb_ChatMessagePrompt
	switch *shape {
	case "merged":
		m := assistantMsg()
		m.Prompt = proto.String("I'll list files and read the readme at once:")
		m.Thinking = proto.String(thinking)
		m.ToolCalls = []*devinproto.ExaCodeiumCommonPb_ChatToolCall{call1, call2}
		asst = []*devinproto.ExaChatPb_ChatMessagePrompt{m}
	case "merged-single":
		m := assistantMsg()
		m.Prompt = proto.String("I'll list files first:")
		m.Thinking = proto.String(thinking)
		m.ToolCalls = []*devinproto.ExaCodeiumCommonPb_ChatToolCall{call1}
		asst = []*devinproto.ExaChatPb_ChatMessagePrompt{m}
	case "split":
		t := assistantMsg()
		t.Prompt = proto.String("I'll list files and read the readme at once:")
		t.Thinking = proto.String(thinking)
		c1 := assistantMsg()
		c1.Thinking = proto.String(thinking)
		c1.ToolCalls = []*devinproto.ExaCodeiumCommonPb_ChatToolCall{call1}
		c2 := assistantMsg()
		c2.Thinking = proto.String(thinking)
		c2.ToolCalls = []*devinproto.ExaCodeiumCommonPb_ChatToolCall{call2}
		asst = []*devinproto.ExaChatPb_ChatMessagePrompt{t, c1, c2}
	case "split-single":
		t := assistantMsg()
		t.Prompt = proto.String("I'll list files first:")
		t.Thinking = proto.String(thinking)
		c1 := assistantMsg()
		c1.Thinking = proto.String(thinking)
		c1.ToolCalls = []*devinproto.ExaCodeiumCommonPb_ChatToolCall{call1}
		asst = []*devinproto.ExaChatPb_ChatMessagePrompt{t, c1}
	default:
		return fmt.Errorf("unknown shape %q", *shape)
	}
	var msgs []*devinproto.ExaChatPb_ChatMessagePrompt
	msgs = append(msgs, userMsg("list the files and read README.md"))
	if *shape == "split" {
		// 生产形态：文本 prompt 后按 call→result 交错（上游拒绝分组排列）。
		msgs = append(msgs, asst[0], asst[1], toolResultMsg("chatcmpl-tool-aaa1", "a.txt\nb.txt\nREADME.md"), asst[2], toolResultMsg("chatcmpl-tool-bbb2", "# hello\n"))
	} else {
		msgs = append(msgs, asst...)
		msgs = append(msgs, toolResultMsg("chatcmpl-tool-aaa1", "a.txt\nb.txt\nREADME.md"))
		if *shape == "merged" {
			msgs = append(msgs, toolResultMsg("chatcmpl-tool-bbb2", "# hello\n"))
		}
	}
	msgs = append(msgs, userMsg("What files did you see? One line."))
	req := &devinproto.GetChatMessageRequest{
		Metadata:      metadata(token, true),
		Prompt:        proto.String("You are a helpful assistant."),
		ChatModelUid:  proto.String(*model),
		RequestType:   devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration: defaultCompletionConfig(),
		CascadeId:     proto.String(randid.UUID()),
		PlannerMode:   devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
		ExecutionId:   proto.String(randid.UUID()),
		TrajectoryReference: &devinproto.ExaCortexPb_CortexTrajectoryReference{
			TrajectoryId:   proto.String(randid.UUID()),
			TrajectoryType: devinproto.ExaCortexPb_CortexTrajectoryType_ExaCortexPb_CortexTrajectoryType_CORTEX_TRAJECTORY_TYPE_CASCADE.Enum(),
			StepType:       devinproto.ExaCortexPb_CortexStepType_ExaCortexPb_CortexStepType_CORTEX_STEP_TYPE_USER_INPUT.Enum(),
		},
		ChatMessagePrompts: msgs,
		Tools: []*devinproto.ExaChatPb_ChatToolDefinition{
			{Name: proto.String("exec"), Description: proto.String("run a command"), JsonSchemaString: proto.String(`{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}`)},
			{Name: proto.String("read_file"), Description: proto.String("read a file"), JsonSchemaString: proto.String(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
		},
	}
	return runStream(ctx, client, req, false, "")
}

// ---- rerun: replay a captured request N times ----

// cmdRerun 回放 03-devin-request.json 抓到的完整上游请求：每次换新的
// executionId，统计 stopReason / toolCalls / 文本尾部，用于同一段历史在
// 不同 wire 形态下的 A/B 对照（拆分 vs 合并）。
func cmdRerun(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, args []string) error {
	fs := flag.NewFlagSet("rerun", flag.ContinueOnError)
	file := fs.String("file", "", "protojson GetChatMessageRequest (logs/*/03-devin-request.json)")
	n := fs.Int("n", 8, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	raw, err := os.ReadFile(*file)
	if err != nil {
		return err
	}
	base := &devinproto.GetChatMessageRequest{}
	if err := protojson.Unmarshal(raw, base); err != nil {
		return fmt.Errorf("unmarshal %s: %w", *file, err)
	}
	// 抓包里的 apiKey 可能已轮换，用当前 token 覆盖。
	if base.GetMetadata() != nil {
		base.Metadata.ApiKey = proto.String(token)
	}
	for run := 0; run < *n; run++ {
		req := proto.Clone(base).(*devinproto.GetChatMessageRequest)
		req.ExecutionId = proto.String(randid.UUID())
		stream, err := client.GetChatMessage(ctx, connect.NewRequest(req))
		if err != nil {
			fmt.Printf("run %d: connect: %v\n", run, err)
			continue
		}
		stop := "UNSPECIFIED"
		var text strings.Builder
		var calls int
		for stream.Receive() {
			m := stream.Msg()
			if s := m.GetStopReason().String(); !strings.HasSuffix(s, "UNSPECIFIED") {
				// 枚举 String() 对未知值可能返回纯数字——无 STOP_REASON_
				// 前缀时切片会越界，退化用原始串。
				if i := strings.LastIndex(s, "STOP_REASON_"); i >= 0 {
					stop = s[i+len("STOP_REASON_"):]
				} else {
					stop = s
				}
			}
			text.WriteString(m.GetDeltaText())
			calls += len(m.GetDeltaToolCalls())
		}
		if err := stream.Err(); err != nil {
			fmt.Printf("run %d: stream: %v\n", run, err)
			continue
		}
		tail := text.String()
		if len(tail) > 90 {
			tail = tail[len(tail)-90:]
		}
		fmt.Printf("run %d: stop=%s calls=%d tail=%q\n", run, stop, calls, tail)
	}
	return nil
}

// ---- bigctx ----

func cmdBigctx(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, args []string) error {
	fs := flag.NewFlagSet("bigctx", flag.ContinueOnError)
	kb := fs.Int("kb", 1024, "")
	model := fs.String("model", "swe-2-max", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	filler := strings.Repeat("lorem ipsum dolor sit amet ", *kb*1024/27)
	req := &devinproto.GetChatMessageRequest{
		Metadata:      metadata(token, true),
		Prompt:        proto.String("You are a helpful assistant."),
		ChatModelUid:  proto.String(*model),
		RequestType:   devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration: defaultCompletionConfig(),
		ExecutionId:   proto.String(randid.UUID()),
		ChatMessagePrompts: []*devinproto.ExaChatPb_ChatMessagePrompt{{
			MessageId: proto.String(randid.UUID()),
			Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum(),
			Prompt:    proto.String(filler + "\nReply: ok"),
		}},
	}
	return runStream(ctx, client, req, false, "")
}

// ---- misc: adjacent endpoints ----

func cmdMisc(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, _ []string) error {
	emb, err := client.GetEmbeddings(ctx, connect.NewRequest(&devinproto.GetEmbeddingsRequest{
		Request: &devinproto.ExaCodeiumCommonPb_EmbeddingsRequest{
			Prompts: []string{"hello world"},
			Model:   devinproto.ExaCodeiumCommonPb_Model_ExaCodeiumCommonPb_Model_MODEL_EMBED_6591.Enum(),
		},
		EmbeddingModel: devinproto.ExaCodeiumCommonPb_Model_ExaCodeiumCommonPb_Model_MODEL_EMBED_6591.Enum(),
	}))
	if err != nil {
		fmt.Println("GetEmbeddings ERR:", err)
		dumpConnectErr(err)
	} else {
		b, _ := marshal.Marshal(emb.Msg)
		fmt.Println("GetEmbeddings:", trunc(string(b), 400))
	}

	st, err := client.GetStreamingExternalChatCompletions(ctx, connect.NewRequest(&devinproto.GetChatCompletionsRequest{
		Metadata: metadata(token, true),
		ChatMessagePrompts: []*devinproto.ExaChatPb_ChatMessagePrompt{{
			MessageId: proto.String(randid.UUID()),
			Source:    devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum(),
			Prompt:    proto.String("say hi"),
		}},
		SystemPrompt: proto.String("You are helpful."),
		CompletionsRequest: &devinproto.ExaCodeiumCommonPb_CompletionsRequest{
			Configuration: &devinproto.ExaCodeiumCommonPb_CompletionConfiguration{
				NumCompletions: proto.Uint64(1), MaxTokens: proto.Uint64(64),
			},
		},
	}))
	if err != nil {
		fmt.Println("GetStreamingExternalChatCompletions connect ERR:", err)
	} else {
		n := 0
		for st.Receive() {
			n++
			b, _ := marshal.Marshal(st.Msg())
			fmt.Printf("extchat frame %d: %s\n", n, trunc(string(b), 300))
			if n > 8 {
				break
			}
		}
		fmt.Println("extchat stream err:", st.Err())
	}

	if r, err := client.GetStatus(ctx, connect.NewRequest(&devinproto.GetStatusRequest{Metadata: metadata(token, true)})); err != nil {
		fmt.Println("GetStatus ERR:", err)
	} else {
		b, _ := marshal.Marshal(r.Msg)
		fmt.Println("GetStatus:", trunc(string(b), 600))
	}
	if r, err := client.GetConfig(ctx, connect.NewRequest(&devinproto.GetConfigRequest{})); err != nil {
		fmt.Println("GetConfig ERR:", err)
	} else {
		b, _ := marshal.Marshal(r.Msg)
		fmt.Println("GetConfig:", trunc(string(b), 600))
	}
	if r, err := client.GetCommandModelConfigs(ctx, connect.NewRequest(&devinproto.GetCommandModelConfigsRequest{Metadata: metadata(token, true)})); err != nil {
		fmt.Println("GetCommandModelConfigs ERR:", err)
	} else {
		uids := []string{}
		for _, c := range r.Msg.GetClientModelConfigs() {
			uids = append(uids, c.GetModelUid())
		}
		fmt.Println("GetCommandModelConfigs uids:", uids)
	}
	return nil
}

// dumpConnectErr 打印 connect.Error 的 code/details/meta，用于确认上游错误是否
// 携带结构化 detail（RetryInfo 等）——决定 connectError 是否值得保留这些信息。
func dumpConnectErr(err error) {
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		fmt.Println("  not a *connect.Error")
		return
	}
	fmt.Println("  code:", connectErr.Code())
	fmt.Println("  raw message:", connectErr.Error())
	for i, d := range connectErr.Details() {
		v, verr := d.Value()
		fmt.Printf("  detail[%d]: type=%s bytes=%d", i, d.Type(), len(d.Bytes()))
		if verr != nil {
			fmt.Printf(" value_err=%v", verr)
		} else {
			fmt.Printf(" value=%v", v)
		}
		fmt.Println()
	}
	for k, vals := range connectErr.Meta() {
		fmt.Printf("  meta[%s]=%v\n", k, vals)
	}
}

// ---- edge cases ----

func cmdEdge(ctx context.Context, client devinprotoconnect.ApiServerServiceClient, token string, argv []string) error {
	fs := flag.NewFlagSet("edge", flag.ContinueOnError)
	model := fs.String("model", "swe-2-max", "")
	imageFile := fs.String("image-file", "", "png file to attach instead of tinyPNG")
	prompt := fs.String("prompt", "", "user prompt text for prompt-driven cases")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	imageB64 := tinyPNG()
	if *imageFile != "" {
		raw, rerr := os.ReadFile(*imageFile)
		if rerr != nil {
			return rerr
		}
		imageB64 = base64.StdEncoding.EncodeToString(raw)
	}
	args := fs.Args()
	if len(args) == 0 {
		return fmt.Errorf("edge needs a case name")
	}
	req := &devinproto.GetChatMessageRequest{
		Metadata:      metadata(token, true),
		Prompt:        proto.String("You are a helpful assistant."),
		ChatModelUid:  proto.String(*model),
		RequestType:   devinproto.ChatMessageRequestType_CHAT_MESSAGE_REQUEST_TYPE_CASCADE.Enum(),
		Configuration: defaultCompletionConfig(),
		PlannerMode:   devinproto.ExaCodeiumCommonPb_ConversationalPlannerMode_ExaCodeiumCommonPb_ConversationalPlannerMode_CONVERSATIONAL_PLANNER_MODE_DEFAULT.Enum(),
		ExecutionId:   proto.String(randid.UUID()),
	}
	switch args[0] {
	case "orphan-tool-result":
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"),
			{MessageId: proto.String(randid.UUID()),
				Source: devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_TOOL.Enum(),
				Prompt: proto.String("orphan result text")},
			userMsg("what did the tool return?"),
		}
	case "unknown-source":
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"),
			{MessageId: proto.String(randid.UUID()),
				Source: devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_UNKNOWN.Enum(),
				Prompt: proto.String("mystery")},
			userMsg("continue"),
		}
	case "dup-message-id":
		dup := randid.UUID()
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"), userMsg("second message"),
		}
		req.ChatMessagePrompts[1].MessageId = proto.String(dup)
		req.ChatMessagePrompts[0].MessageId = proto.String(dup)
	case "empty-user-prompt":
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"),
			{MessageId: proto.String(randid.UUID()),
				Source: devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_USER.Enum()},
			userMsg("continue"),
		}
	case "empty-assistant":
		// 空 end_turn 回放进历史再追加 continue——CPA#4886 类故障的续传路径。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Say hi"),
			{MessageId: proto.String(randid.UUID()),
				Source: devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM.Enum(),
				Prompt: proto.String("")},
			userMsg("continue"),
		}
	case "experiment":
		req.ExperimentConfig = &devinproto.ExaCodeiumCommonPb_ExperimentConfig{
			ForceEnableExperimentStrings:  []string{"bogus_exp_xyz"},
			ForceDisableExperimentStrings: []string{"another_bogus"},
		}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{userMsg("Reply exactly: pong")}
	case "trailing-assistant":
		// 历史以 assistant 文本结尾（Anthropic prefill 形态）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("List two colors."), assistantTextMsg("1. Blue"),
		}
	case "trailing-tool-result":
		// 历史以 tool 结果结尾且无后续 user（IDE 恢复会话的形态）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read file a.txt"),
			assistantCallMsg("call_1", "read_file", `{"path":"a.txt"}`),
			toolResultMsg("call_1", "file contents here"),
		}
	case "thinking-only-assistant":
		// assistant 只有 thinking 没有 text/call：我们回放时被 convertMessage 整个丢弃的形态。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"),
			{MessageId: proto.String(randid.UUID()),
				Source:   devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM.Enum(),
				Thinking: proto.String("I should greet politely.")},
			userMsg("continue"),
		}
	case "thinking-empty-sig":
		// redacted thinking：无正文有签名（我们回放 Anthropic redacted_thinking 的形态）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"),
			{MessageId: proto.String(randid.UUID()),
				Source:           devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM.Enum(),
				Prompt:           proto.String("sure"),
				Signature:        proto.String("sealed.v1.ZmFrZSBmb3IgdGVzdA"),
				ThinkingRedacted: proto.Bool(true)},
			userMsg("continue"),
		}
	case "interleaved-calls":
		// 已按 call,result 配对的正确交错顺序（正向对照）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt then b.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			toolResultMsg("c1", "aaa"),
			assistantCallMsg("c2", "read_file", `{"path":"b.txt"}`),
			toolResultMsg("c2", "bbb"),
			userMsg("what did you find?"),
		}
	case "grouped-calls-results":
		// 未配对的分组形态：call,call,result,result（客户端原样历史）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt then b.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			assistantCallMsg("c2", "read_file", `{"path":"b.txt"}`),
			toolResultMsg("c1", "aaa"),
			toolResultMsg("c2", "bbb"),
			userMsg("what did you find?"),
		}
	case "trailing-call-no-result":
		// 历史以「未得到结果的 tool call」结尾。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
		}
	case "dup-tool-result":
		// 同一 call_id 两条结果。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			toolResultMsg("c1", "first"),
			toolResultMsg("c1", "second"),
			userMsg("ok?"),
		}
	case "tool-result-mismatch-call":
		// 结果的 call_id 指向不存在的调用。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			toolResultMsg("zzz", "orphan"),
			userMsg("ok?"),
		}
	case "orphan-result-with-id":
		// 结果带 tool_call_id 但全程没有任何 call：区分「无 id」与「无匹配 call」。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("hi"),
			toolResultMsg("zzz", "orphan"),
			userMsg("what did the tool return?"),
		}
	case "tool-call-invalid-json-arg":
		// 回放历史里的非法 JSON 参数。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", "read_file", `{bad json`),
			userMsg("ok?"),
		}
	case "tool-name":
		// 可疑工具名逐个打：tool-name "mcp::x" / "a b" / "工具" / "a.b" / ""
		if len(args) < 2 {
			return fmt.Errorf("tool-name needs a name argument (use empty string for empty)")
		}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{userMsg("call the tool now")}
		req.Tools = []*devinproto.ExaChatPb_ChatToolDefinition{{
			Name:             proto.String(args[1]),
			Description:      proto.String("test tool"),
			JsonSchemaString: proto.String(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}
		req.ToolChoice = &devinproto.ExaChatPb_ChatToolChoice{
			Choice: &devinproto.ExaChatPb_ChatToolChoice_OptionName{OptionName: "required"},
		}
	case "gap-tool-result":
		// call 与 result 之间夹一条 USER（mid-conversation system 降级形态）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			userMsg("[system] reminder: be concise"),
			toolResultMsg("c1", "file contents here"),
			userMsg("what did you find?"),
		}
	case "dup-call-id":
		// 两条 call prompt 用同一个 call id（客户端重复记录调用）。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			assistantCallMsg("c1", "read_file", `{"path":"a.txt"}`),
			toolResultMsg("c1", "file contents"),
			userMsg("ok?"),
		}
	case "tool-result-image":
		// TOOL prompt 挂图片：上游是否消费 tool 结果里的图。
		tr := toolResultMsg("c1", "screenshot attached")
		tr.Images = []*devinproto.ExaCodeiumCommonPb_ImageData{{
			Base64Data: proto.String(imageB64), MimeType: proto.String("image/png"),
		}}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Take a screenshot then tell me the dominant color."),
			assistantCallMsg("c1", "take_screenshot", `{}`),
			tr,
			userMsg("What color is it? Answer in one word."),
		}
	case "user-image-file":
		// USER prompt 挂图片对照组：验证上游确实消费用户消息里的图。
		m := userMsg("What is the dominant color of the attached image? Answer in one word.")
		m.Images = []*devinproto.ExaCodeiumCommonPb_ImageData{{
			Base64Data: proto.String(imageB64), MimeType: proto.String("image/png"),
		}}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{m}
	case "user-image-prompt":
		// 与 user-image-file 同形但问法可控（-prompt）：定向探测图是否
		// 真被消费——比如问图里不存在的内容、要求复述 OCR 文本等。
		text := *prompt
		if text == "" {
			text = "What is the dominant color of the attached image? Answer in one word."
		}
		m := userMsg(text)
		m.Images = []*devinproto.ExaCodeiumCommonPb_ImageData{{
			Base64Data: proto.String(imageB64), MimeType: proto.String("image/png"),
		}}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{m}
	case "pdf-as-image":
		// 文档通道探测：mime_type=application/pdf 是否被 Images 通道接受。
		m := userMsg("What is in this document? One sentence.")
		m.Images = []*devinproto.ExaCodeiumCommonPb_ImageData{{
			Base64Data: proto.String(tinyPNG()), MimeType: proto.String("application/pdf"),
		}}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{m}
	case "custom-tool-call-flag":
		// 历史回放带 is_custom_tool_call=true + invalid_json_str 的 call。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Apply this patch: *** Begin Patch\n*** Update File: x.go\n@@\n+x\n*** End Patch"),
			{MessageId: proto.String(randid.UUID()),
				Source: devinproto.ExaCodeiumCommonPb_ChatMessageSource_ExaCodeiumCommonPb_ChatMessageSource_CHAT_MESSAGE_SOURCE_SYSTEM.Enum(),
				ToolCalls: []*devinproto.ExaCodeiumCommonPb_ChatToolCall{{
					Id:               proto.String("c1"),
					Name:             proto.String("apply_patch"),
					IsCustomToolCall: proto.Bool(true),
					InvalidJsonStr:   proto.String("*** Begin Patch\n*** Update File: x.go\n@@\n+x\n*** End Patch"),
				}}},
			toolResultMsg("c1", "applied"),
			userMsg("did it apply?"),
		}
	case "parallel-call-id-frames":
		// 诱导单轮并行调用：观察 delta_tool_calls 的 id 帧形态——每个
		// 调用的首帧是否都带 id、无 id 续帧怎么分布，决定解码器
		// 「无 id 帧按位置归并末位调用」规则的正确性。必须逐帧看，
		// 所以直接走 frames 输出。
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Call read_file twice in the same turn: once with path a.txt, once with path b.txt. Issue both tool calls together, not sequentially."),
		}
		req.Tools = []*devinproto.ExaChatPb_ChatToolDefinition{{
			Name:             proto.String("read_file"),
			Description:      proto.String("read_file"),
			JsonSchemaString: proto.String(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		}}
		req.ToolChoice = &devinproto.ExaChatPb_ChatToolChoice{
			Choice: &devinproto.ExaChatPb_ChatToolChoice_OptionName{OptionName: "auto"},
		}
		return runStream(ctx, client, req, true, "")
	case "n-tools-limit":
		// 工具声明数量上限探测：n-tools-limit 200 逐档试，看上游在第
		// 几档开始 invalid_argument 或静默截断（真实客户端工具集约 23 个，
		// 我们的代理会把客户端的完整 MCP 工具集原样转发）。
		if len(args) < 2 {
			return fmt.Errorf("n-tools-limit needs a count argument")
		}
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 1 {
			return fmt.Errorf("n-tools-limit bad count %q", args[1])
		}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{userMsg("Call tool_0 now.")}
		for i := 0; i < n; i++ {
			req.Tools = append(req.Tools, &devinproto.ExaChatPb_ChatToolDefinition{
				Name:             proto.String(fmt.Sprintf("tool_%d", i)),
				Description:      proto.String("t"),
				JsonSchemaString: proto.String(`{"type":"object"}`),
			})
		}
		req.ToolChoice = &devinproto.ExaChatPb_ChatToolChoice{
			Choice: &devinproto.ExaChatPb_ChatToolChoice_OptionName{OptionName: "required"},
		}
	case "history-tool-name":
		// 历史 tool_call 带非法字符名（tool-name case 只测过声明名）。
		// 上游若同拒，适配层须转义历史名；若放行，转义反而损毁语义。
		// 用法：history-tool-name <name>（默认 a.b）。
		name := "a.b"
		if len(args) >= 2 {
			name = args[1]
		}
		req.ChatMessagePrompts = []*devinproto.ExaChatPb_ChatMessagePrompt{
			userMsg("Read a.txt"),
			assistantCallMsg("c1", name, `{"path":"a.txt"}`),
			toolResultMsg("c1", "file contents"),
			userMsg("ok?"),
		}
	default:
		return fmt.Errorf("unknown edge case %q", args[0])
	}
	return runStream(ctx, client, req, false, "")
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return proto.String(s)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
