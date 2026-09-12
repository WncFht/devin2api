# 上游活跃探测报告

> 方法：新增 `cmd/probe`（直连 `server.codeium.com` 的 Connect-RPC 实验工具，复用 `outputs/devin-proto-go` 生成绑定），对真实上游逐字段打靶。token 用 config.yaml 里同一个免费档账号。
>
> 前置文档：`2026-09-12-upstream-gaps.md`（二轮：proto/strings 静态清单）、`upstream-debug-playbook.md`（wire 契约）、`upstream-cache.md`（缓存）。本文是**实测**结论，凡是与静态推测冲突的以本文为准。

## 一、gaps 文档验证清单的实测结论

| 待证项                                                                                     | 实测结论                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| ------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| CLI 是否填 `provider_source`/`language`/`prompt_id`/`num_tokens`/`safe_for_code_telemetry` | **都不填**。已有抓包（v3000.2.17，glm-5-2/swe-1-7，CASCADE）顶层字段仅：`metadata/prompt/chatMessagePrompts/chatModelUid/requestType/configuration/tools/trajectoryReference/cascadeId/plannerMode/executionId`（连 `system_prompt_cache_options`/消息级 `prompt_cache_options` 也不发——纯隐式缓存）。                                                                                                                                                                            |
| `output_id`/`thinking_id`/`signature_type`/`phase` 回放                                    | **部分纠正（四轮实测）**：`output_id` 在 OpenAI 路径**有下发**（gpt-5-6-sol 系，值 `msg_*`，与同帧 signature 内 reasoning item 的 `rs_*` 同前缀）；`signature_type` 有三个已观测值 `sealed`/`anthropic`/`openai`（swe-2/claude-thinking/gpt-sol 各一）。`thinking_id`/`phase` 仍未在任何 provider 上观测到。swe-2-max/glm-5-2/swe-1-7-medium/deepseek-v4-pro/gemini-3-1-pro 无 `output_id`。结论修订：**`output_id`+`signature_type` 需要按 provider 存**，不是「无东西可回放」。 |
| router uid 是否先 `AssignModel`                                                            | **证实**。`subagent-default`/`session-titler`/`command-reviser` 是活 router；详见第三节。                                                                                                                                                                                                                                                                                                                                                                                         |
| `invalid_json_str` 触发                                                                    | **无法在 swe-2-max 上诱导**：连 `apply_patch` 这种 freeform 习惯的输出也被包成合法 JSON（`{"path": "*** Begin Patch..."}`）。大概率是 Fireworks 侧约束解码兜底，该字段在免费档近似死字段。                                                                                                                                                                                                                                                                                        |

## 二、swe-2-max 响应帧清点（真实抓取）

一次正常响应的帧序：`deltaThinking`（**单帧整段**，不像 glm-5-2 逐小块流）→ `deltaText`+`deltaTokens` 逐块 → `deltaSignature`+`deltaSignatureType` → `stopReason` → `responseDimensionGroups`。

- **签名格式 `sealed.v1.<base64url>`，`delta_signature_type="sealed"`**。glm-5-2 与 swe-1-7-medium（Decart）**完全无签名帧**。~~签名是 swe-2/Fireworks 特有~~ **四轮修正**：签名按 provider 分三种体制——`sealed`(swe-2)、`anthropic`(claude-thinking)、`openai`(gpt-sol，signature 是序列化 reasoning item)，详见 §十。
- **`stop_reason` 正常结束按 provider 不同**：swe-2/gemini/deepseek = `STOP_PATTERN`，**claude = `MIN_LOG_PROB`**（字面误导，疑似 end_turn 映射，四轮实测），gpt-sol 小样本只见 `UNSPECIFIED`；工具调用 = `FUNCTION_CALL`，`maxTokens=8` 实测烧完 thinking 后 `STOP_REASON_MAX_TOKENS`。`mapStopReason` 靠 default 兜到 Stop，建议显式列 STOP_PATTERN/CONTENT_FILTER/MIN_LOG_PROB。
- **缺 `stopReason` 的干净 EOF = 截断，不是正常结束**。2026-09-12 实测事故：Codex 一轮输出在序言文本后流即 EOF，`deltaToolCalls`/`stopReason`/`responseDimensionGroups` 全部缺失；同请求重放产出完整 3 个 tool_use。decoder 曾把这种情况合成 `end_turn`，导致 Codex `task_complete` 静默收工——已改为显式流错误 "Devin stream ended without stop reason"。`stoppedByPattern`（本地停止序列截断）是唯一例外的合法无停因结束。
- 每帧带 `latency`（累计秒）、`requestId`、`timestamp`、`usage`；`usage.responseHeader.x-request-id` 是 provider 侧请求号（Fireworks=`chatcmpl-*`，Decart=`req_*`）——**排障金矿，建议进 debuglog**。
- `usage.apiProvider`：swe-2-max/glm-5-2 → `FIREWORKS_DEVIN`；swe-1-7-medium → `DECART`。
- 免费档**不下发** `creditCost`/`committed_*`/`actualModelUid`/`completionProfile`/`prompt`/`redact`/`geminiThoughtSignature`/`thinkingId`/`phase`/`arenaInvocationCapReached`。~~`outputId`~~ **四轮修正**：OpenAI 路径（gpt-5-6-sol 系）下发 `outputId=msg_*`；Anthropic 路径在 `usage.messageId` 下发 `msg_*`。
- 工具调用 id 格式按模型分：swe-2-max → `read_file_0`（名字\_序号）；glm-5-2 → `chatcmpl-tool-<hex>`。**不要假设 id 形态**。
- 文本拒绝是普通 deltaText（"我不能做 X，但可以…"），`usage.provider_refusal` 未出现——它应当是 provider 原生 refusal 的透传位，免费档不可见。

## 三、AssignModel 路由链（完整验证）

```
AssignModel{model_router_uid, cascade_id}
  → {assignment_jwt(JWE, A256GCMKW), model_uid, harness_uids}
GetChatMessage{chat_model_uid=assignment.model_uid, model_assignment_jwt, cascade_id=<同一个>}
```

- `subagent-default` → `swe-1-7-medium`（DECART，签名无）；`session-titler`/`command-reviser` → `swe-1-7`。
- **jwt 绑 `cascade_id`**：AssignModel 与 GetChatMessage 的 cascade 不一致 → `invalid_argument`。
- **jwt 不绑 model_uid**：拿 subagent-default 的 jwt + `chat_model_uid=swe-2-max` 同 cascade → 正常用 swe-2-max 跑完。
- 错误分类：非 router 的正规 uid（swe-2-max 等）→ `invalid_argument`；不存在的 router 名（fusion/pigeon-\*/glm-5p2）→ `not_found`；router 直连不走 AssignModel → `unavailable: third-party model provider…`（**伪装的瞬时错误，实则永久**）。
- `session-titler`/`command-reviser`/`swe-1-7-medium` 可以**直接**当 `chat_model_uid` 用（不经过 AssignModel），三者都在响应。

## 四、请求字段逐项实测

| 字段                                                                               | 结果                                                                                                                                                                                                                                                                                                                                            |
| ---------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tool_choice.option_name`                                                          | `none`（模型自述"tools disabled"拒调用）、`auto`（正常自选）、`required`（强制产生 tool call）有效；**`any` → `invalid_argument`**！ Anthropic `any` 必须映射成 `required`。`tool_name="X"` 强制调用指定工具（连无关 prompt 也照调）。**四轮补充**：指名不存在的工具 → 流内 `invalid_argument`；无 tools 时发 `required` → 容忍、正常返回文本。 |
| `disable_parallel_tool_calls=true`                                                 | 被接受但**无效果**：swe-2-max 照样同轮发 `read_file_0`+`list_dir_1` 两个调用。                                                                                                                                                                                                                                                                  |
| `provider_source=CASCADE`                                                          | 接受，无可观测差异。                                                                                                                                                                                                                                                                                                                            |
| `prompt_id`                                                                        | 接受；同 id 两连发无 dedup，照常各跑各的。纯关联字段。                                                                                                                                                                                                                                                                                          |
| `num_tokens`（消息级）                                                             | 接受，无可见差异。                                                                                                                                                                                                                                                                                                                              |
| `language=GO`、`chat_model_name=whatever`                                          | 静默接受。                                                                                                                                                                                                                                                                                                                                      |
| `planner_mode` READ_ONLY/NO_TOOL/EXPLORE/PLANNING/AUTO                             | 全部接受但**纯提示性质**：NO_TOOL 下模型照样发 tool call。                                                                                                                                                                                                                                                                                      |
| `step_type`（trajectory 内）                                                       | FINISH/MQUERY/PLANNER_RESPONSE 都接受——纯轨迹记账字段。                                                                                                                                                                                                                                                                                         |
| `request_type`                                                                     | **只有 CASCADE 能在无会话状态下工作**。GENERAL/SMART_FRIEND/COMMAND/EVAL/CONTEXT_CHECK → `failed_precondition: error with your Cascade session`（需要 StartCascade 建过轨迹）。                                                                                                                                                                 |
| `use_internal_chat_model`+enum                                                     | `permission_denied`——内部枚举通道对免费 token 关闭。                                                                                                                                                                                                                                                                                            |
| `chat_model_uid` 缺席                                                              | `unknown: an internal error occurred`。                                                                                                                                                                                                                                                                                                         |
| 版本化 uid `swe-2-high-09102026`                                                   | `permission_denied`（versionId 是内部引用，不能直接当 uid）。                                                                                                                                                                                                                                                                                   |
| `is_custom_tool`+`custom_tool_grammar`(lark)                                       | `unknown:` provider 错——swe-2-max 不走 freeform 语法通道。                                                                                                                                                                                                                                                                                      |
| `json_schema_string` 非法 JSON                                                     | `unknown:` provider 错（坏 schema 直接打爆 provider 层）。                                                                                                                                                                                                                                                                                      |
| `strict`/`read_only_hint`/`server_name`/`attribution_field_names`                  | 全部静默接受。                                                                                                                                                                                                                                                                                                                                  |
| `experiment_config` 乱填                                                           | 静默接受。                                                                                                                                                                                                                                                                                                                                      |
| `metadata.{session_id,request_id,device_fingerprint,disable_telemetry,user_agent}` | 全部静默接受。`f` 指纹缺失也照常（`-no-fingerprint` 正常返回）。                                                                                                                                                                                                                                                                                |
| `system_prompt_cache_options`/`prompt_cache_options`                               | 见 cache 文档，EPHEMERAL 无副作用。                                                                                                                                                                                                                                                                                                             |
| 空 `prompt`（system）+ tools                                                       | **现在能通过**——契约表第 8 条已过时，空 system 不再是拒因（至少 swe-2-max）。                                                                                                                                                                                                                                                                   |
| SYSTEM_PROMPT 作消息 source                                                        | `unknown:` provider 错——system 只能走顶层 `prompt` 字段。                                                                                                                                                                                                                                                                                       |
| `images`×20（单轮）                                                                | 全接受，模型正确数出 20。无尾部 cap 实测证据；CLI 的 `max_trailing_images` 是客户端策略不是 wire 约束。                                                                                                                                                                                                                                         |
| 非视觉模型 + 图（glm-5-2）                                                         | `invalid_argument`——本地 `validateImagesForModel` 拦截方向正确。                                                                                                                                                                                                                                                                                |

## 五、CompletionConfiguration 真实语义

| 字段                               | 实测                                                                                                                                                                                                |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maxTokens`                        | **生效**，且 thinking 计入预算：maxTokens=8 全烧在 thinking 上、零 text、`MAX_TOKENS` 收尾。                                                                                                        |
| `maxNewlines=400`                  | **不生效**——500 行输出完整返回。CLI 照发是习惯，不是约束。                                                                                                                                          |
| `stopPatterns`                     | **不生效**——让模型输出 "ABC XYZ DEF" 并设 stop=XYZ，全量返回；四轮在 gpt-5-6-sol 复测同样全量返回，非 swe-2 特有。**我们的 StopSequences 透传是空操作**，客户端指望 stop 会踩空（已本地截断实现）。 |
| `numCompletions`>1                 | 流中途 `internal: stream error INTERNAL_ERROR`——CASCADE 只支持 1。                                                                                                                                  |
| `temperature`/`topK`/`topP`/`seed` | 透传无障碍（未做细粒度验证）。                                                                                                                                                                      |

## 六、签名回放 A/B（swe-2-max）

| 变体                        | 结果                                                            |
| --------------------------- | --------------------------------------------------------------- |
| thinking+ 真实 signature    | 正常，input=195                                                 |
| thinking 无 signature       | 正常，input=182                                                 |
| thinking+**伪造** signature | **正常**，input=180——**上游不校验 sealed 签名**，它只是透传存储 |
| 完全无 thinking 的纯文本    | 正常，input=163                                                 |

含义：回放签名给上游是可选的；我们留签名纯粹是为了客户端（Anthropic 侧 redacted thinking 校验）正确性，不是上游要求。

四轮补充（跨 provider 回放）：

- claude-opus-4-6-thinking 的真 `anthropic` 签名回放 → 正常；**thinking 正文大改 + 原签名回放 → 仍正常**——上游/Anthropic 路径未把签名与 thinking 文本做精确绑定校验（单样本，别当成保证）。
- **错误/不兼容的 `signature_type`（如张冠李戴）→ 流内 `invalid_argument`**——签名类型与模型/provider 要配对，比签名内容本身更敏感。
- OpenAI 路径 reasoning 无独立 thinking 文本，签名即 reasoning item（见 §十），回放 `sig-only`（signature 无 thinking）形态有效。
- **缺 `signature_type` 是被容忍的**：claude-thinking 真 `anthropic` 签名不带 type 回放正常。所以「存 sig 丢 type」当前能工作，但 type 是 provider 路由提示——存起来更稳，且 OpenAI 路径还原 reasoning item 必须靠它区分。
- 同一 `chat_model_uid` 的 `apiProvider` 随请求变化：claude-opus-4-6-thinking 先后命中 `ANTHROPIC` 与 `ANTHROPIC_BEDROCK_GLOBAL`（后者 `usage.messageId=msg_bdrk_*`、`X-Amzn-Requestid` 头）。provider 是 per-request 属性，诊断展示时别当模型固有属性。

## 七、其余 RPC 面

| RPC                                       | 结果                                                                                                                                                                                                                                                                                                                                                           |
| ----------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GetCliModelConfigs`                      | 比 cascade 版多 `subagent_default_model_uid`（=`subagent-default`）与 `default_override_model_config`（=`{modelUid:swe-2-high, versionId:swe-2-high-09102026}`）。模型表 209 条，与 cascade 版 (210) 仅差一条 legacy `MODEL_CHAT_GPT_4O_2024_08_06`。**本账号零 router**（is_model_router 全 false）、`inference_config` 全空、`smart_friend_model_uid` 全空。 |
| `CheckUserMessageRateLimit`               | `{hasCapacity:true, messagesRemaining:-1, maxMessages:-1}`——本账号无消息额度限制。                                                                                                                                                                                                                                                                             |
| `GetModelStatuses`                        | 返回异常模型告警（实测时 MODEL_8341 elevated error rate）。可作健康检查源。                                                                                                                                                                                                                                                                                    |
| `GetModelProviders`                       | 12 家 provider 清单（xAI/DeepSeek/Qwen/NVIDIA/ThinkingMachines/Windsurf/OpenAI/Google/Moonshot/Z.ai/MiniMax/Anthropic）。                                                                                                                                                                                                                                      |
| `GetStatus`/`GetConfig`                   | 空响应。                                                                                                                                                                                                                                                                                                                                                       |
| `GetCommandModelConfigs`                  | 6 条 legacy enum uid（command 模式独立模型面）。                                                                                                                                                                                                                                                                                                               |
| `GetEmbeddings`                           | `unavailable`——embedding enum 不对该 token 开放。                                                                                                                                                                                                                                                                                                              |
| `GetStreamingExternalChatCompletions`     | `invalid_argument`——旧版 Windsurf chat 面，走 `model_id` 枚举，同样关着。                                                                                                                                                                                                                                                                                      |
| `supports_parallel_tool_calls` 缺失的模型 | glm/kimi/grok/inkling/deepseek/nemotron 全系——`disable_parallel` 对它们也许更有意义（未逐一测）。                                                                                                                                                                                                                                                              |

## 八、错误分类学（Connect code → 语义）

| code                               | 触发场景                                                                                                                                                                                | 语义                                                                                                                                  |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| `permission_denied`                | 无权模型 uid、内部枚举模型、版本化 uid、内容指纹句                                                                                                                                      | 权限/策略                                                                                                                             |
| `invalid_argument`                 | 坏 wire 形状：孤儿 TOOL、非 router uid 进 AssignModel、tool_choice=any、非视觉 + 图、缺 cascade 的 jwt、**超长 prompt**（文案 `"The prompt is too long for this model"`，带真实描述！） | 请求形状错                                                                                                                            |
| `failed_precondition`              | 非 CASCADE request_type                                                                                                                                                                 | 前置状态缺失（需真实 cascade 会话）                                                                                                   |
| `not_found`                        | 不存在的 router 名                                                                                                                                                                      | 资源不存在                                                                                                                            |
| `unavailable`                      | router 直连、GetEmbeddings                                                                                                                                                              | **看似瞬时实则永久**——重试策略要注意：这类文案带 "try this model again later" 但永远不会好                                            |
| `unknown`                          | provider 层崩坏：坏 schema、is_custom_tool、SYSTEM_PROMPT/UNKNOWN source、缺 model uid                                                                                                  | provider 内部错，同样永久                                                                                                             |
| `internal` (HTTP/2 INTERNAL_ERROR) | numCompletions>1                                                                                                                                                                        | 流中断                                                                                                                                |
| `resource_exhausted`               | 高频请求（~6 次/分）触发整体消息限流                                                                                                                                                    | 真限流；无 Retry-After/RetryInfo，hint 只在文案 `"reset in N seconds"`，N 随触发递增。capacity RPC 与此套限流**互相独立**（见 §十二） |

排障含义：`unavailable`/`unknown` 里的 "experiencing issues / try later" 文案是误导性的固定模板，**真实原因是确定性的请求/权限问题**，不应按瞬时错误重试。已落实：`isTransientConnectError` 现在**只**对非 Connect 的纯传输错误（EOF/连接重置/超时）重试，所有 Connect code（含 unavailable）一律视为语义错误直接透传。

## 九、对代理的改动建议（按优先级；2026-09-12 状态已对齐实现）

1. ~~**stopPatterns 不是真透传**~~：**已实现**——`newResponseDecoder` 接收 `request.StopSequences`，本地做尾部保留窗口 + 命中截断，结束时上报 `StopReasonStopSequence` + 命中 pattern（Anthropic `stop_sequence`、OpenAI `stop`、Responses 正常 completed）。
2. ~~**`unavailable` 从重试集移除**~~：**已实现**——`isTransientConnectError` 现在只对非 Connect 的纯传输错误（EOF/重置/超时）重试，所有 Connect code（含 unavailable）一律不重试。
3. ~~**`tool_choice`/`disable_parallel_tool_calls` 映射**~~：**已实现**——`any`→`required`，`none`/`auto`/`required`/`tool_name` 直传；`parallel_tool_calls=false`→`disable_parallel_tool_calls=true`（上游实测不执行，仅形状对齐）。
4. ~~**response `request_id`+`usage.responseHeader.x-request-id` 进 debuglog**~~：**已实现**——`upstream_request_id` 进 `meta.json`，`api_provider`+provider 侧 request id 进 `diagnostics`，进程日志 slog 行也带 `upstream_request_id`。
5. ~~**`mapStopReason` 显式列**~~：**已实现**——`STOP_PATTERN→Stop`（上游正常收尾）、`CONTENT_FILTER→StopReasonContentFilter`（Anthropic `refusal`、chat `content_filter`、Responses `incomplete`+`content_filter`）。
6. ~~**`numCompletions>1` 前置拒绝**~~：**已实现**——chat 解码层本地报错 `n > 1 is not supported`，不打到上游。
7. `maxTokens` 语义提示：thinking 烧预算，客户端给很小的 max_tokens 会得到零文本——观测事实，无需代码改动。
8. ~~**模型目录从 `GetCliModelConfigs` 取**~~：**已实现**——`ListModels` 已切到 `GetCliModelConfigs`，`ModelInfo` 透出 `supports_tool_calls`/`supports_parallel_tool_calls`/`supports_thinking`/`preserve_thinking`/`context_tokens`/`max_output_tokens`。
9. `subagent-default`/`session-titler`/`command-reviser`/`swe-1-7-medium` 是隐藏可用 uid——备查（router 需 AssignModel+cascade 一致；`swe-1-7-medium` 直连即可，mult=3 最便宜档）。

## 十、四轮实测：跨 provider 帧字段对照（2026-09-12 晚）

| 维度                   | swe-2-max（FIREWORKS_DEVIN） | claude-opus-4-6(-thinking)（ANTHROPIC / **ANTHROPIC_BEDROCK_GLOBAL**，同 uid 按请求路由） | gpt-5-6-sol（OPENAI_SAFETY_RETENTION）                                                                                                                   | gemini-3-1-pro-high（GEMINI_DATABRICKS）             | deepseek-v4-pro-high（FIREWORKS_DEVIN） |
| ---------------------- | ---------------------------- | ----------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- | --------------------------------------- |
| `deltaSignatureType`   | `sealed`                     | `anthropic`                                                                               | `openai`                                                                                                                                                 | **无签名帧**                                         | **无签名帧**                            |
| `deltaSignature` 形态  | `sealed.v1.<b64url>`         | Anthropic 原生 base64 sig                                                                 | **完整 OpenAI Responses reasoning item JSON**：`[{"id":"rs_*","type":"reasoning","encrypted_content":"gAAAAAB…","summary":[],"content":[],"status":""}]` | —                                                    | —                                       |
| `outputId`             | 无                           | 无                                                                                        | **`msg_*`**（与 rs_* 同前缀，是 OpenAI 侧 message id）                                                                                                   | 无                                                   | 无                                      |
| `usage.messageId`      | 无                           | **`msg_*`（Anthropic 原生 message id）**                                                  | 无（id 走顶层 `outputId`）                                                                                                                               | 无                                                   | 无                                      |
| 正常收尾 stopReason    | `STOP_PATTERN`               | **`MIN_LOG_PROB`**（疑似 end_turn 映射，字面名误导）                                      | `UNSPECIFIED`（只有 7 帧小样本）                                                                                                                         | `STOP_PATTERN`                                       | `STOP_PATTERN`                          |
| `usage.responseHeader` | `x-request-id: chatcmpl-*`   | `Request-Id: req_*`（Anthropic 头原样）                                                   | `x-request-id: req_*` + `openai-version`                                                                                                                 | `responseId` + `trafficType: PROVISIONED_THROUGHPUT` | `x-request-id: chatcmpl-*`              |
| thinking 形态          | 单帧整段                     | 多帧流式                                                                                  | 无 thinking 文本（reasoning 只在 signature 的 encrypted_content 里）                                                                                     | 多帧摘要式 deltaThinking                             | 多帧 deltaThinking                      |

要点：

- **`signature_type="openai"` 时 signature 不是 blob，是序列化的 reasoning item**——含 `encrypted_content`。给 `/v1/responses` 客户端回放时可还原成标准 `reasoning` item，这是把 Devin 上游桥接回 OpenAI Responses 多轮 reasoning 的关键通道。
- **`MIN_LOG_PROB` 疑似是 Anthropic 路径的"正常结束"枚举名**（单样本，字面名误导；多次小响应收尾都是它而非 STOP_PATTERN）。`mapStopReason` 的 default→Stop 已兜住，但显式列出更诚实。
- 图片是真实视觉通道：swe-2-max 与 claude-opus-4-6 都正确读出 512×512 左红右蓝测试图（inputTokens 计入图像 token）。**TOOL 消息挂图同样被消费**（tool_result 图像子通道有效）。`mime_type=application/pdf` 走 Images 通道 → `invalid_argument`。
- `cacheReadTokens` 实测出现一次（inputTokens=1 + cacheRead=128，疑似系统前缀被隐式缓存命中）；但完全相同的大 prompt 连发两次又未出现——隐式缓存存在但命中不可控、不可依赖，usage 合并逻辑只需正确透传，别指望它稳定出现。

## 十一、四轮实测：ChatMessagePrompt 历史契约边界（edge 用例矩阵）

全部 swe-2-max。结论：**上游对「脏历史」远比想象宽容，唯二的硬约束是结果必须挂在已存在的 call 之后、且不能先堆多个 call 再批量给结果**。

| 用例（cmd/probe edge <名>）                                                               | 发送形态                                                                                                                 | 结果                                                                                                                                  |
| ----------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------- |
| `interleaved-calls`                                                                       | call,result,call,result（正确配对）                                                                                      | 正常                                                                                                                                  |
| `grouped-calls-results`                                                                   | call,call,result,result                                                                                                  | **`invalid_argument`**——结果必须跟着「最近的未配对 call」走，不能乱序配对                                                             |
| `gap-tool-result`                                                                         | call,**user**,result                                                                                                     | 正常——call 与 result 之间允许夹 user 消息                                                                                             |
| `dup-call-id`                                                                             | 两条 call 同 id + 一条 result                                                                                            | 正常                                                                                                                                  |
| `dup-tool-result`                                                                         | 一条 call + 同 id 两条 result                                                                                            | 正常，两份结果文本都送达                                                                                                              |
| `tool-result-mismatch-call`                                                               | call(c1) + result(zzz)                                                                                                   | 正常——id 不匹配也不报错，结果文本送达（模型把 "orphan" 当 a.txt 内容读，疑似按位置关联到挂起的 c1）                                   |
| `orphan-tool-result`                                                                      | 无 call，TOOL 消息且无 tool_call_id                                                                                      | **`invalid_argument`**                                                                                                                |
| `orphan-result-with-id`                                                                   | 无 call，TOOL 消息带 tool_call_id=zzz                                                                                    | **`invalid_argument`**——没有任何 call 时给 id 也没用                                                                                  |
| `trailing-call-no-result`                                                                 | 历史以未应答 call 结尾                                                                                                   | 正常（上游自己补不了结果但不报错）                                                                                                    |
| `trailing-tool-result`                                                                    | 历史以 tool result 结尾（无后续 user）                                                                                   | 正常                                                                                                                                  |
| `trailing-assistant` / `empty-user-prompt` / `dup-message-id` / `thinking-only-assistant` | 各种宽松形态                                                                                                             | 全部正常                                                                                                                              |
| `thinking-empty-sig`                                                                      | redacted thinking + `sealed.v1.假`                                                                                       | 正常                                                                                                                                  |
| `tool-call-invalid-json-arg`                                                              | 历史 call 的 `arguments_json="{bad json"`                                                                                | **两次结果不一致**：一次 `invalid_argument`（流内），一次模型正常应答并在 thinking 里吐槽参数坏——非确定性，坏参数历史**可能**打爆上游 |
| `custom-tool-call-flag`                                                                   | 历史 call `is_custom_tool_call=true` + `invalid_json_str=patch 原文`                                                     | 正常，模型读到了 patch 内容——**`invalid_json_str` 是有效的历史通道**，解码端若遇到应原样保留而非吞成 `{}`                             |
| `tool-name` 系列                                                                          | `a`/`a_b`/`a-b` 合法；`a.b`、`mcp::x`、`a-b_c.d`、非 ASCII（`工具`）→ 全 `invalid_argument`（文案模糊 "internal error"） | 合法字符集约 **`[A-Za-z0-9_-]`**；我们应在入口校验而非等上游模糊报错                                                                  |
| tool_choice 指名不存在工具                                                                | `tool_name="nonexistent"` + 有别的工具                                                                                   | **`invalid_argument`**（流内第 2 帧后）                                                                                               |
| tool_choice=required 无工具                                                               | 无 tools + required                                                                                                      | **容忍**——正常返回纯文本，不产生 call                                                                                                 |

含义：`pairToolCallsWithResults`/`demoteOrphanToolResults` 是承重墙（grouped 形态上游必炸）；但「孤儿 result」只有当**全程没有任何 call** 时才炸——有挂起 call 时 id 不匹配都能位置绑定，所以 demote 成文本是安全降级不是必要降级。

## 十二、四轮实测：限流信号分叉（重要）

- `CheckUserMessageRateLimit` 持续报 `{messagesRemaining:-1, maxMessages:-1}`（无限），但**真实生成路径有自己的限流**：高频探测约 6 次/分钟后触发 `resource_exhausted: Reached overall message rate limit. Please try again later. Your limit will reset in N seconds.`，且 N 随持续触发递增（实测 2s→36s）。
- 该错误**不带 Retry-After 头、不带 connect.Error.Details**（RetryInfo 为空），唯一机器可用信息是消息文本里的 `reset in N seconds`——要 Retry-After 只能解析文案。
- 含义：配额信号（capacity/limit RPC）与生成可用性是两套系统，「容量检查说有」≠「生成不报 429」。我们的代理不能把 Check* RPC 当成准确闸门；遇到 `resource_exhausted` 应优先提取文案里的 reset 秒数透传给客户端。

## 十三、五轮实测：行动清单剩余项验证（2026-09-12 收尾）

- **空 end_turn 续传**（`edge empty-assistant`）：历史里塞 `SYSTEM`-source 空 assistant 消息 + 追加 user "continue" → 上游正常续说（153 帧、正常 stopReason）。**续传重发路径可行**——`tryReopen(continueEmpty)` 已落地。
- **裸竞品指纹串**：`"Claude Agent SDK"`（system）、`"OpenAI Codex CLI"`（system）、`"Hermes Agent"`（user prompt）三组裸串全部正常完成，无伪 429/permission_denied——**身份检测不在 prompt 裸串这一层**。
- **Connect 响应 header/trailer**：`chat` 正常流与流内错误路径的 headers 均只有标准字段（`Content-Type: application/connect+proto`、`Connect-*-Encoding`、cache 安全头），trailers 为空——**上游不在 HTTP 层给配额/限流信号**，唯一的供应商侧锚点是 `usage.responseHeader.x-request-id`（已进 diagnostics）。
- **quota 软 gate 不可验证**：`CheckUserMessageRateLimit` 恒报 `messagesRemaining:-1`；`GetUserStatus` 的 `daily_remaining=89%` 无法低成本耗尽到 0。且 `resource_exhausted` 已归一 429+Retry-After，本地 gate 只会引入陈旧快照误拒——决议不做。

## 十四、仍未解（本账号观测不到）

- `thinking_id`/`phase`/`credit_cost`/`committed_*`/`provider_refusal`/`redact`/`gemini_thought_signature`/`completion_profile`/`actual_model_uid`——`output_id`/`signature_type` 已移出此清单（见 §十）。
- `invalid_json_str`/`is_custom_tool_call` 的**响应方向**线上形态（历史方向已验证有效）；`arena_*`。
- `prompt`(#19 响应回显）、`response_dimension_groups` 的完整语义（UI 用，无关紧要）。
- `MIN_LOG_PROB` 是否就是 Anthropic end_turn 的唯一映射（单样本）；Gemini 路径 `gemini_thought_signature` bytes 字段始终未出现（Databricks 侧似乎不下发思考签名）。

## 十五、五轮补充实测（2026-09-12 午后，复核 + 新靶点）

probe 新增 `-temperature`/`-top-p`/`-top-k`/`-trajectory-id` flag。全部对照 swe-2-max 默认请求。

### 签名校验严格度按 provider 分级（对 §六/§十 的细化）

| 模型                       | 变体                                              | 结果                                                            |
| -------------------------- | ------------------------------------------------- | --------------------------------------------------------------- |
| claude-sonnet-4-6-thinking | `bogus-sig`（截断伪造 anthropic 签名）            | **流内 `invalid_argument`**（第 2 帧后，文案仍模糊 + trace ID） |
| swe-2-max                  | `mutated-thinking`（真签名 + 改写的 thinking 文） | 正常，`input=213`                                               |
| swe-2-max                  | `sig-only`（签名无 thinking 文本）                | 正常，`input=197`                                               |
| gpt-5-6-sol-medium         | `bogus-sig`                                       | 未有效测——step1 未产出 reasoning（effort 概率性），无签名可伪造 |

结论修订：签名校验严格度 **Anthropic（校验 blob 本体真伪）> Fireworks（完全不校验）**；但 Anthropic 不绑定 thinking 正文（四轮：真签名 + 改写 thinking 仍正常）。OpenAI 路径 signature 本身是序列化 reasoning item，伪造大概率卡在反序列化——未实证。给客户端的推论不变：**签名必须原样往返，不能伪造、不能错配 `signature_type`**。

### `stop_patterns` 上游不执行

`-stop-pattern p`/`ell` 两次：文本 "pong"/"hello" 全量下发，零截断；字段被静默接受并忽略（无 `invalid_argument`）。`STOP_PATTERN` 这个枚举名只是 swe-2 的通用正常收尾，与 `stop_patterns` 命中无关。**decoder 的本地尾部截断是 stop 序列的唯一实现**——若哪天想去掉本地逻辑换成上游透传，这里是反例。

### `max_tokens` 计费口径（对 #25 的细化）

- `outputTokens` 正常路径 = thinking+text 合计（thinking≈50 + "391"≈3 → 报 55）。
- cap 合并计费：`-max-tokens 4` → thinking 5 token+text 0，`outputTokens=4`；`-max-tokens 1` → thinking ~40 token 送达但 `outputTokens=1`（**送达按 chunk 粒度可超 cap，计费按 cap**）。
- `deltaTokens` 只数 text 帧。
- 空文本轮可稳定复现：`max_tokens≤8` 十次全部 `MAX_TOKENS` + thinking-only + 零 text——「有 stopReason 但零可见内容」的真实形态（`emptyEndTurn` 重试针对的是正常 stop 零内容，与此不同源）。

### `cascade_id` 不携带会话状态（#26 直接验证）

同 `cascade_id` 先后两请求：step2 的 thinking 明说 "this is the very first message in our conversation"——上游不按 cascade 关联 prompt 历史，续传只能靠回放 `chat_message_prompts`（`edge empty-assistant` 已证该形态可用）。

### 采样参数无服务端约束

`temperature=0.5 + top_p=0.5` 在 `glm-5-3-max`（preserveThinking）、`swe-2-max`、`claude-sonnet-4-6-thinking` 全部正常应答。CPA#2509 的「thinking 激活强制 temp=1/top_p≥0.95/top_k 缺席」在本上游不存在——要么不校验，要么 Devin 侧已改写。

### 响应 wire 元数据（#28 复核 + provider 头全集）

- Connect 成功响应：headers 仅标准 connect+ 安全头，**trailers 恒空**；错误 meta 同样无配额字段。配额信号只有 Check* RPC + 错误文案（§十二结论维持）。
- `usage.responseHeader` 按 provider 透传原生响应头：Fireworks `x-request-id: chatcmpl-*`；Anthropic `Request-Id: req_*`+`Date`；OpenAI `x-request-id: req_*`+`openai-version`+`openai-processing-ms`；**glm 路径不下发**（且 `apiProvider` 是未命名枚举值 `"58"`——本地 proto 的 APIProvider 枚举落后于服务端）。
- `usage.messageId` 在 Anthropic 路径是 `msg_*` 原生 message id。

### 运行期观测（两条）

- **Fireworks 帧形态**：一轮正常响应 = ~40+ 个仅含 `latency`/`timestamp`/`usage` 的心跳帧 → 单帧整段 `deltaThinking` → `deltaText` → `deltaSignature` → `stopReason` → 收尾 usage。thinking 是批式单帧不是逐 token 流；看门狗按「任意帧到达」重置（`upstreamStallTimeout=120s`），长思考窗口天然安全。
- **瞬时全断事件**：约 3 分钟窗口内所有 RPC（含 Check* 一元调用）全部 `unavailable: unexpected EOF`，随后自愈；10 连发中也偶发 0 帧 EOF。**0 帧 `unavailable` 是本上游真实存在的瞬时态**，`tryReopen` 对纯传输错误的 pre-content 重试覆盖的是正确分类。
