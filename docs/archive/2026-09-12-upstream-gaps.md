# 上游协议未覆盖点（静态清单，原始记录）

> **结论已按主题整理进 `../upstream-protocol.md`，以它为准。** 本文保留当日 proto/strings 静态分析的原始清单（含后来被实测修正的条目），供追溯字段出处用。
>
> 来源：`outputs/devin-proto/all-protos.proto`（Windsurf `language_server_macos_arm` 提取的完整 proto bundle）+ `/opt/homebrew/Caskroom/devin-cli/3000.10.21/bin/devin`（v3000.10.21 Rust 二进制）strings 提取 + `internal/adapter/devin/*` 代码对照。
>
> 前置文档：`../upstream-cache.md`（缓存）、`../upstream-compaction.md`（压缩）、`../upstream-debug-playbook.md`（wire 契约与排查）。本文只记录这三份**之外**的新发现。

## 一、wire 协议上我们没用/没消费的字段

### 请求侧（`GetChatMessageRequest`）

| 字段                                                                                   | 现状       | 判断                                                                                                                                                  |
| -------------------------------------------------------------------------------------- | ---------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tool_choice` (#12)                                                                    | **已映射** | 已实现：`required`/`none`→`option_name`、指定名→`tool_name`、`auto`→缺省；Anthropic `any`→`required`（上游不接受 `"any"`，实测 `invalid_argument`）。 |
| `disable_parallel_tool_calls` (#11)                                                    | **已映射** | `parallel_tool_calls=false`→`disable_parallel_tool_calls=true`；上游实测不执行，仅形状对齐。                                                          |
| `prompt_id` (#17)                                                                      | 未设       | ~~可派生~~ **撤销**——实测同 id 连发无 dedup 效果，纯关联字段（live-probes）。CLI 抓包也未发送。                                                       |
| `provider_source` (#18)                                                                | 未设       | ~~填 `CASCADE` 更像真 CLI~~ **撤销**——实测接受但无可观测差异，CLI 抓包自身也不发（live-probes）。                                                     |
| `language` (#19)                                                                       | 未设       | `ExaCodeiumCommonPb_Language` 枚举；IDE 用于语言感知。实测静默接受、CLI 不发。**仅记录**。                                                            |
| `chat_model_name` (#14)                                                                | 未设       | 与 `chat_model_uid` 并存；部分路径按 name 路由。我们只发 uid。实测静默接受。**仅记录**。                                                              |
| `use_internal_chat_model` + `internal_chat_model` (#5/#6)                              | 未用       | 走内部 `ExaCodeiumCommonPb_Model` 枚举而非 uid 字符串的旁路；实测免费 token 返回 `permission_denied`，通道关闭。**仅记录**。                          |
| `experiment_config` (#9)                                                               | 未用       | force_enable/disable experiments。**仅记录**。                                                                                                        |
| `arena_converge_count` / `arena_assignment_jwt` / `model_assignment_jwt` (#24/#25/#26) | 未用       | 见下方「AssignModel 路由」。                                                                                                                          |

### 消息侧（`ChatMessagePrompt`）

| 字段                             | 现状                           | 判断                                                                                                                                                                                                                                                                                                 |
| -------------------------------- | ------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `output_id` (#15)                | **部分下发，我们不存也不回放** | ~~撤销~~ **四轮修正**：OpenAI 路径（gpt-5-6-sol 系）响应帧**带 `outputId=msg_*`**（OpenAI 侧 message id，与 reasoning item 的 `rs_*` 同前缀）；swe-2/glm/deepseek/gemini/claude 路径仍不下发。**建议**：有则存进 item 元数据，回放时回填——对 `/v1/responses` 客户端是原生 `id`。                     |
| `thinking_id` (#16)              | 未观测到                       | 四轮全 provider 未出现。**仅记录**。                                                                                                                                                                                                                                                                 |
| `signature_type` (#18)           | **三种体制已观测**             | ~~不下发~~ **四轮修正**：实测 `sealed`（swe-2，`sealed.v1.<b64>`）、`anthropic`（claude-thinking，原生签名 base64）、`openai`（gpt-sol，signature=序列化 reasoning item JSON 含 `encrypted_content`）。**注意**：张冠李戴的 `signature_type` 触发流内 `invalid_argument`——必须与 provider 配对存取。 |
| `phase` (#19)                    | 未观测到                       | 四轮全 provider 未出现。**仅记录**。                                                                                                                                                                                                                                                                 |
| `num_tokens` (#4)                | 未设                           | 调用方预算填，上游可能用来做 context 预算。实测静默接受、CLI 不发。**仅记录**。                                                                                                                                                                                                                      |
| `safe_for_code_telemetry` (#5)   | 未设                           | 隐私位，默认 false 即可。**仅记录**。                                                                                                                                                                                                                                                                |
| `prompt_annotation_ranges` (#14) | 未设                           | IDE 的 prompt 区域标注。**仅记录**。                                                                                                                                                                                                                                                                 |
| `gemini_thought_signature` (#17) | 未用                           | bytes 形式的 Gemini 签名。四轮实测 gemini-3-1-pro-high（GEMINI_DATABRICKS）**string/bytes 两种签名都不下发**——该 provider 路径上此字段是死的。**仅记录**。                                                                                                                                           |

### 响应侧（`GetChatMessageResponse`）

| 字段                                                                                                                                                                | 现状                            | 判断                                                                                                                                                                    |
| ------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `output_id` (#15) / `thinking_id` (#16)                                                                                                                             | **解码器没读**                  | **四轮修正**：`output_id` 在 OpenAI 路径**有下发**（`msg_*`），值得存；`thinking_id` 全 provider 未出现。                                                               |
| `signature_type` (#21→delta_signature_type) / `phase` (#25)                                                                                                         | **signature_type 已下发但没读** | **四轮修正**：`sealed`/`anthropic`/`openai` 三体制实测到；signature 本身我们已存但 type 丢了——回放张冠李戴会触发上游 `invalid_argument`，**应配对存**。`phase` 未出现。 |
| `gemini_thought_signature` (#20)                                                                                                                                    | 没读                            | bytes 签名；GEMINI_DATABRICKS 路径实测不下发任何签名，死字段（免费档）。                                                                                                |
| `usage.message_id`（ModelUsageStats #7）                                                                                                                            | 没读                            | **四轮新发现**：Anthropic 路径在此下发原生 `msg_*` message id——provider 侧溯源字段，建议进 diagnostics。                                                                |
| `redact` (#8)                                                                                                                                                       | 没读                            | 上游让我们在持久化时抹掉这段输出（遥测/ZDR 场景）。我们做纯转发可以忽略，但若客户端回放给我们时带了 redact 语义我们要留意。**仅记录**。                                 |
| `prompt` (#19)                                                                                                                                                      | 没读                            | 怀疑是回显（upstream 把 prompt 回填让客户端确认），可忽略。                                                                                                             |
| `latency` (#12) / `completion_profile` (#13)                                                                                                                        | 没读                            | TTFT/总耗时，可以记录到 usage 扩展字段做观测。**可选**。                                                                                                                |
| `credit_cost` (#14) / `committed_credit_cost` (#18) / `committed_acu_cost` (#22) / `committed_quota_cost_basis_points` (#26) / `committed_overage_cost_cents` (#27) | 没读                            | 计费明细。如果 `usage` 上想透传成本，可从这组取。**可选**。                                                                                                             |
| `request_id` (#17)                                                                                                                                                  | **已读**                        | 已实现：进 `meta.json` 的 `upstream_request_id` 与进程日志 slog 行；provider 侧 `api_provider`+`x-request-id` 进 `diagnostics`。                                        |
| `arena_invocation_cap_reached` (#24)                                                                                                                                | 没读                            | arena 模式配额到达标志。**仅记录**。                                                                                                                                    |
| `response_dimension_groups` (#28)                                                                                                                                   | 没读                            | UI 展示用的维度组（copyable code / metric / cumulative metric）。客户端用不上。**仅记录**。                                                                             |
| `usage.provider_refusal` (#12 inside ModelUsageStats)                                                                                                               | **已读**                        | 已实现：decoder 检测到 `provider_refusal=true` 时直接 fail，错误信息写明 "upstream provider refused"。                                                                  |
| `usage.billing_model_uid` / `requested_model_uid`                                                                                                                   | 没读                            | 可用来观测模型实际路由（比如 swe-2-max 落到哪个内部模型）。**可选**。                                                                                                   |

### `ChatToolCall`（`ExaCodeiumCommonPb_ChatToolCall`）

| 字段                                              | 现状     | 判断                                                                                                                                                                                                                                                                                                                                                            |
| ------------------------------------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `invalid_json_str` (#4) / `invalid_json_err` (#5) | **没读** | 响应方向 swe-2-max 不可诱导（provider 约束解码兜底）。**四轮新证据（历史方向）**：回放 `is_custom_tool_call=true`+`invalid_json_str=<patch 原文>` 的 call 被上游正常消费、模型读到了 patch 内容——该字段是**有效的历史通道**。若解码端遇到应原文透传而非兜底 `{}`。另注意：历史里 `arguments_json` 非法 JSON 实测结果不稳定（一次 invalid_argument、一次容忍）。 |
| `is_custom_tool_call` (#6)                        | 没读     | 上游标记该调用走 freeform。**四轮**：作为历史字段回放被正常接受（配合 `invalid_json_str`）；响应方向未见下发。**仅记录**。                                                                                                                                                                                                                                      |

### `ChatToolDefinition`（`ExaChatPb_ChatToolDefinition`）

| 字段                                                                                     | 现状   | 判断                                                                                                                                                                                 |
| ---------------------------------------------------------------------------------------- | ------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `is_custom_tool` (#9) / `custom_tool_grammar` (#10) / `custom_tool_grammar_syntax` (#11) | 未映射 | ~~上游可能支持原生 freeform grammar，待实测~~ **撤销**——实测发送 `is_custom_tool`+grammar 上游直接报错（live-probes）。反向证明把 `apply_patch` 包成 JSON schema + sanitize 是正解。 |
| `strict` (#12)                                                                           | 未设   | OpenAI `strict: true` 工具的语义。**可选**。                                                                                                                                         |
| `read_only_hint` (#7)                                                                    | 未设   | 工具只读提示，CLI 权限系统用。上游可能按 hint 影响行为。**仅记录**。                                                                                                                 |
| `attribution_field_names` / `server_name` / `computer_use_config`                        | 未设   | IDE 归属/归属字段/电脑使用。**仅记录**。                                                                                                                                             |

## 二、CLI 侧我们发现的上游客户端行为（此前未记录）

### 1. `AssignModel` RPC —— 模型路由器解析

CLI 在 `chat_model_uid` 是 router（`is_model_router=true`，比如 `fusion`/`swe-2-*` 的 router 形态）时，先调 `AssignModel{model_router_uid, cascade_id, chat_message_prompt}`，拿 `ModelAssignment{assignment_jwt, model_uid, harness_uids}`，然后正式 `GetChatMessage` 里 `chat_model_uid = assignment.model_uid` + `model_assignment_jwt = assignment.assignment_jwt`。

二进制实证：`Resolving model router '' via AssignModel RPC` / `Model router '' resolved to ''` / `AssignModel returned empty assignment` / `Model router '' assigned ''` / `Fusion lead router '' assigned sidekick ''`。

**含义**：我们目前直接把 `chat_model_uid` 当终态。如果用户配 `swe-2-max` 而它在上游是 router，我们发的 uid 可能被上游当成「跳过了路由层」，行为/计费与 CLI 不同。**建议**：拿到 `GetCliModelConfigs` 时读 `is_model_router`/`is_default_model_in_family`，router uid 走 `AssignModel` 解出真实 uid + jwt 再发。

### 2. 内部「功能性」模型 UID（非对话用，但暴露在上游）

二进制里写死的内部 UID（上游全认识，客户端按需调用）：

- `session-titler`：会话标题生成
- `command-reviser`：`/command/revise`（改写 shell 命令的小模型）
- `subagent-free-default`：sidekick/subagent 缺省回落模型
- `smart_friend_model_uid`（`ClientModelConfig` #29）：每个模型的伴生「smart friend」用于 `CHAT_MESSAGE_REQUEST_TYPE_SMART_FRIEND` 请求

**含义**：这些 uid 可以直接当 `chat_model_uid` 用（CLI 就用），可能拿到一个便宜小模型。另外 `request_type` 不一定是 `CASCADE`——CLI 给标题/revise 用别的 requestType（`GENERAL` 或专用）。

### 3. 模型枚举全集（上游 UID 空间）

strings 提取的内部模型名（部分节选）：

- swe-2 家族：`swe-2-low`、`swe-2-high`、`swe-2-max`、`swe-2-high-lite`
- pigeon：`pigeon-v4-vl`、`pigeon-v4-devin`、`pigeon-v6-low`（routers）
- kimi：`kimi-k2p6`、`kimi-k2p7-code`、`kimi-k3-low`、`kimi-k3-high`、`kimi-k3-max`
- glm：`glm-5-3-low`、`glm-5-3-high`、`glm-5-3-max`、`glm-5p2`（router）
- 其它：`decart-swe-1.7`、`hestia2/3`、`chiron`、`swe-1-6`、`swe-1-6-fast`、`claude-opus-4-7-medium`、`gpt-5.4`

这些名字在 `GetCliModelConfigs`/`GetCascadeModelConfigs` 返回里出现为 `model_uid`；`ModelInfo.is_model_router` 标记 router。

### 4. `InferenceConfig`（服务端按模型维度的推理参数）

`ModelInfo.inference_config`（oneof openai/google/anthropic/zai/xai/thinking_machines）告诉客户端这个模型的推理档位选项：

- `OpenAIInferenceConfig`：`reasoning_effort`、`service_tier`、`extended_prompt_cache_retention`、`reasoning_context`
- `AnthropicInferenceConfig`：`thinking`、`effort`、`fast_mode`、`context_1m`
- `ZaiInferenceConfig`：`effort`、`long_context`、`thinking`
- `GoogleInferenceConfig`/`XaiInferenceConfig`/`ThinkingMachinesInferenceConfig`：`reasoning_effort`

**含义**：上游有 per-provider 推理参数通道，但**这些是 ModelInfo 携带的服务端配置而非我们能在请求里改的参数**。`GetChatMessageRequest` 没有对应字段——reasoning effort 不通过 GetChatMessage 暴露，只能改 `chat_model_uid`（选 high/low 档位）实现。**仅记录**：用户要 reasoning effort 时，路径是换 uid 而不是加参数。

### 5. `ModelFeatures`（能力位）

`ClientModelConfig.model_info.model_features`：

- `supports_tool_calls` (#12)、`supports_parallel_tool_calls` (#21)、`supports_thinking` (#15)、`preserve_thinking` (#25)、`interleave_thinking` (#24)、`supports_images` (#11)、`supports_image_captions` (#20)、`supports_videos`/`supports_video_urls`、`supports_documents`/`supports_document_urls`、`summarize_thinking`

**含义**：**已实现**——`ListModels` 已透出 `supports_tool_calls`/`supports_parallel_tool_calls`/`supports_thinking`/`preserve_thinking`/`context_tokens`/`max_output_tokens`/`supports_images`，上层（ccload/客户端）可在发请求前获知模型能力位。

### 6. CLI 内部 `InferenceRequest` 字段（暴露的整形策略）

`InferenceRequest`（CLI 内部统一推理请求，strings 提取）：`model/messages/tools/query_label/is_user_initiated/completion_config/max_trailing_images/execution_id/agent_context/disable_prompt_cache_writes/prefix_mismatch_behavior/append_only_history/system_prefix_len/hosted_tool_search/generation_id`

关键策略位：

- **`max_trailing_images`**：限制只在尾部 N 条消息挂图（我们目前「assistant 之后全部挂图」，CLI 是「尾部 cap 张」）。当用户带大量图时可对照——CLI 超上限写 "Images omitted (exceeded trailing-image cap)" 占位符。
- **`disable_prompt_cache_writes`**：关掉 cache 写（对应 OpenAI `disable_prompt_cache_writes`）。
- **`prefix_mismatch_behavior`**：`error` / `drop_block` 枚举。前缀不匹配时（编辑历史后）是报错还是丢块。
- **`append_only_history`**：是否强制只追加（cache 友好）。
- **`system_prefix_len`**：系统前缀长度预算。
- **`hosted_tool_search`**：上游托管的工具搜索（serve-side tool search，对应 `HostedToolSearchConfig{anthropic_variant, AnthropicSearchVariant{regex,bm25}}`）——CLI 内部字段，wire 上没暴露给 GetChatMessage。

### 6b. CLI 内部专用提示词（非 GetChatMessage 主链）

这些都是 CLI 内部发起的小模型调用，不走 `GetChatMessage` 的 CASCADE 主链；但 prompt 文本暴露了上游对「这类任务」的预期形状，对接子代理/hook 时可参考。

- **`agent-ext/title`**：`You are a session title generator. Given the user's first message, produce a short, descriptive title (max 80 characters). Output ONLY the title text — no quotes, no punctuation wrapper, no explanation.` 失败 `Title generation failed to create stream: / Title generation failed:`。
- **`agent-ext.revise-command`**（`/command/revise`）：`You rewrite shell commands. Given a proposed command and an instruction describing how to change it, output ONLY the revised command — no quotes, no code fences, no explanation. Preserve the parts of the command the instruction does not ask to change.` 用于 ACP `cognition.ai/command/revise`。
- **`agent-ext/looper`**：代码评审循环，给模型喂「另一个 agent 刚实现的 prompt + diff」，要求按正确性/安全/完整性/偷工四项审，强制输出 `<ACCEPT>` 或 `<REJECT>` 收尾，不写 verdict 会被再问一次。prompt 全文见 strings `looper.rs` 区域（`Another agent just implemented the following prompt: <prompt>...</prompt> Here's the diff of what was implemented: <diff>...</diff>`）。
- **`agent-ext/btw`**（`/btw` side chat）：`You are answering a side question (\`/btw\`) forked from the main conversation of Devin, Cognition's agentic coding CLI. The main conversation continues independently — nothing you do here is visible to it. You have read-only access ... Your response streams directly to the user in a compact side panel. Answer the question directly and concisely — no preamble and no closing remarks.`
- **`smart_permission/classifier`**：本地权限分类（`'...' commands always require approval in smart mode`、path matcher 规则），**不调模型**——是本地 Rust 逻辑。
- **`hooks/evaluator`**：评估用户配置的 hook 是否触发，主要是路径匹配，不是模型调用。
- **`skills/*`**：skill 系统字符串集（`<skill name="..." status="running">`、`<available_skills>`、`SkillSource`、`discover_rules`、`resolve_mentions` 等）；`rules/*` 同理。这些是 CLI 的本地配置注入器，把 skills/rules 序列化进 system prompt。

### 7. 错误分类全集

CLI 内部错误枚举（strings）：`Unauthenticated` / `Timeout` / `RateLimited` / `QuotaExhausted` / `UsageLimitReached` / `ServerError` / `user_message` / `ClientError` / `Refusal` / `fallback_credit` / `ContextTooLong` / `PayloadTooLarge` / `Disconnected` / `MalformedResponse` / `AuthFlowError`

**含义**：`ContextTooLong` 和 `PayloadTooLarge` 是独立错误（不是 `invalid_argument`）。**部分已覆盖**：上游实测的 `invalid_argument: "The prompt is too long for this model"` 已被 `common.IsContextLengthError` 归一成 413 + `context_length_exceeded`；但若上游以 `ResourceExhausted`/`out_of_range` 等别的 code 表达 ContextTooLong，目前仍会落到通用错误——遇到时需要在归一表补条目。

### 8. `subagent_default_model_uid`（GetCliModelConfigsResponse #4）

CLI 用 GetCliModelConfigs 拿模型表 + `subagent_default_model_uid` 决定 sidekick/subagent 缺省模型。**已实现**：`ListModels` 已切到 `GetCliModelConfigs`（模型表、能力位、`subagent_default_model_uid` 都可拿到）；sidekick 调度本身未做。

## 三、对我们代理的具体改动建议

### 该改（有证据表明不改会出错或丢功能）

1. **存 `signature_type`（与 signature 配对）+ `output_id`**：**四轮修正后重新成立**——`signature_type` 三种体制（sealed/anthropic/openai）实测下发，张冠李戴回放会触发流内 `invalid_argument`；`outputId` 在 OpenAI 路径是 `msg_*` 原生 id。`thinking_id`/`phase` 仍全 provider 未观测，不存。另外 `signature_type=openai` 时 signature 是完整 reasoning item（含 `encrypted_content`）——给 `/v1/responses` 客户端时可还原成标准 reasoning item，是多轮 reasoning 的关键通道。

2. **`ChatToolCall.invalid_json_str`/`invalid_json_err` 处理**：**仍未实现**。模型吐非 JSON 时，参数不落在 `arguments_json`，`complete` 兜底成 `{}`——上游把"坏参数"静默吞掉。响应方向 swe-2-max 无法诱导（约束解码兜底），但**历史方向已验证该字段有效**（live-probes §十一 `custom-tool-call-flag`：模型能读到 patch 原文）。方向不变：`invalid_json_str` 非空时原样透传给客户端。

3. ~~**`tool_arg_leak_recovery` 同款防御**~~：**已实现**——`complete` 检测到 `arguments_json` 不是 JSON object 时先走 `repairLeakedXMLArguments` 把 `<parameter name="X">v</parameter>` 解回 JSON，失败才兜底 `{}`（`response_decoder.go`）。

4. ~~**`usage.provider_refusal` 检查**~~：**已实现**——decoder 在 `provider_refusal=true` 时直接 `fail("upstream provider refused the request (provider_refusal)")`。

5. **`tool_choice` / `disable_parallel_tool_calls` 透传**：~~现在被静默丢弃~~ **已实现**（2026-09-12）。映射规则按实测修正：OpenAI `required`→`option_name="required"`、`none`→`"none"`、function 对象→`tool_name`；Anthropic `auto`→缺省、**`any`→`required`**（上游不接受 `"any"`，实测 invalid_argument）、`tool`→`tool_name`。`parallel_tool_calls=false`→`disable_parallel_tool_calls=true`（上游实测不执行，仅形状对齐）。

6. **`max_trailing_images` 上限**：CLI 对同一轮挂图有尾部 cap（`max_trailing_images`），超了写 "Images omitted (exceeded trailing-image cap)"。我们无上限——但实测单轮 20 张上游正常接受，CLI 的 cap 是客户端策略不是 wire 约束。暂不实现；若未来出现大图压爆再议。

6.5 **`stop_sequences` 本地截断**（原"gaps"外新增，已实现）：上游 `stopPatterns` 实测不生效，已在 `responseDecoder` 做尾部保留 + 本地截断，命中时上报 `stopSequence`/`stop_sequence`。

6.6 **错误重试修正**（已实现）：`unavailable` 从瞬时重试集移除——上游该码是确定性语义错误的伪装（"try later" 文案是固定模板）。

6.7 **四轮新增：TOOL 结果契约**（live-probes §十一）：上游硬约束=「TOOL 消息必须出现在至少一个 call 之后」「不能 call,call,result,result 分组」。我们的 `pairToolCallsWithResults`/`demoteOrphanToolResults` 方向已被证明必要；剩余缺口在入口侧——OpenAI Chat `findToolName` 对未知 call id 硬 400、Responses `function_call_output` 只认 `call_id` 一种字段名——应统一走孤儿降级而非入口拒绝。

6.8 **四轮新增：`resource_exhausted` 文案解析**：真实限流错误（`"Your limit will reset in N seconds"`）无 Retry-After 头、无 RetryInfo detail，唯一可用 hint 在文案。建议解析 `reset in (\d+) seconds` → 下游 `Retry-After` + `429`；同时注意 `CheckUserMessageRateLimit`/`CheckChatCapacity` 与实际限流**互相独立**，不能当闸门。

6.9 **四轮新增：工具名/工具引用前置校验**：上游合法字符集约 `[A-Za-z0-9_-]`（`a.b`/`mcp::x`/非 ASCII 全 `invalid_argument` 且文案模糊）；`tool_choice` 指名不存在工具也报模糊 `invalid_argument`。入口本地校验能把模糊流内错变成清晰的 400。

6.10 **四轮新增：`MIN_LOG_PROB` 显式映射**：claude 路径正常收尾枚举是 `MIN_LOG_PROB` 而非 `STOP_PATTERN`，default 兜底虽对但应显式列出避免误读。

### 可选（能改善但非必须）

6. **`request_id`/`latency`/`completion_profile`/`credit_cost` 记录到 debuglog**：**已实现**——`request_id` 进 `meta.json` 的 `upstream_request_id`，provider 侧 `api_provider`+`x-request-id` 进 `diagnostics`（`upstream_provider`），usage 汇总进 meta.json。`latency`/`credit_cost` 免费档不下发，暂无内容可记。

7. **`AssignModel` 路由解析**：如果 `GetCliModelConfigs` 回来的 `is_model_router=true`，增加一次 `AssignModel` RPC 拿 `assignment_jwt`+真实 `model_uid` 再发 `GetChatMessage`。目前没踩到（因为 `swe-2-max` 直连能用），但如果上游哪天把 swe-2-max 改成 router，我们会静默落到不同路径。

8. **`provider_source=CASCADE`**：~~先实测差异~~ **实测无可观测差异**，CLI 自己也不发。不实现。

9. **`prompt_id`**：~~上游 dedup 可能更稳~~ **实测无 dedup 效果**（同 id 连发各跑各的）。纯关联字段，不实现。

10. **`ModelFeatures` 透出**：**已实现**——`ListModels` 切到 `GetCliModelConfigs`，`ModelInfo` 新增 `supports_tool_calls`/`supports_parallel_tool_calls`/`supports_thinking`/`preserve_thinking`/`context_tokens`/`max_output_tokens`，`/v1/models` 与面板同步透出。

### 仅记录（上游内部用，我们不用动）

11. `use_internal_chat_model`/`internal_chat_model`/`experiment_config`/`arena_*`/`language`/`chat_model_name`/`num_tokens`/`safe_for_code_telemetry`/`prompt_annotation_ranges`/`gemini_thought_signature`/`attribution_field_names`/`server_name`/`computer_use_config`/`read_only_hint`/`strict`/`is_custom_tool_call`/`custom_tool_grammar*`/`redact`/`prompt`(#19 echo)/`response_dimension_groups`/`arena_invocation_cap_reached`/`committed_*_cost`/`output_id` 之外的其它 UI/telemetry 字段。

## 四、验证清单（下次抓到真实 CLI 流量时核）

> **2026-09-12 已用 `cmd/probe` 对真实上游逐条实测，结论见 `2026-09-12-upstream-live-probes.md`**。要点：
>
> - CLI 请求（v3000.2.17 抓包）**不填** `provider_source`/`language`/`prompt_id`/`num_tokens`/`safe_for_code_telemetry`；实测逐项打在 swe-2-max 上均静默接受。
> - ~~`output_id`/`thinking_id`/`signature_type`/`phase` 在免费档响应中不下发~~ **四轮修正**：`output_id`/`signature_type` 按 provider 有下发（live-probes §十）；`thinking_id`/`phase` 仍未见。签名 `sealed.v1` 上游不校验内容，但 **`signature_type` 张冠李戴会触发 `invalid_argument`**。
> - `AssignModel` 链路完整验证：jwt 绑 `cascade_id`、不绑 model_uid；`subagent-default`→`swe-1-7-medium`。
> - `invalid_json_str` 在 swe-2-max 响应方向不可诱导（上游约束解码兜底），但历史方向有效（live-probes §十一）。
> - 新增事实：`tool_choice.option_name` 合法值 {none,auto,required}（`any` 报错）、`stopPatterns`/`maxNewlines`/`disable_parallel_tool_calls` 上游不生效、`numCompletions>1` 崩流、非 CASCADE `request_type` 需真实会话。
> - **四轮新增**：TOOL 消息契约矩阵（孤儿 result 必炸、分组 call/result 必炸、id 不匹配可容忍）、限流 `resource_exhausted` 无 RetryInfo 只有文案秒数、TOOL 消息挂图有效、PDF mime 走 Images 通道被拒、隐式缓存的 `cacheReadTokens` 偶发出现。
