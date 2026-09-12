# 上游活跃探测报告（三轮逆向）

> 方法：新增 `cmd/probe`（直连 `server.codeium.com` 的 Connect-RPC 实验工具，复用 `outputs/devin-proto-go` 生成绑定），对真实上游逐字段打靶。token 用 config.yaml 里同一个免费档账号。
>
> 前置文档：`2026-09-12-upstream-gaps.md`（二轮：proto/strings 静态清单）、`upstream-debug-playbook.md`（wire 契约）、`upstream-cache.md`（缓存）。本文是**实测**结论，凡是与静态推测冲突的以本文为准。

## 一、gaps 文档验证清单的实测结论

| 待证项                                                                                     | 实测结论                                                                                                                                                                                                                                                                                               |
| ------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| CLI 是否填 `provider_source`/`language`/`prompt_id`/`num_tokens`/`safe_for_code_telemetry` | **都不填**。已有抓包（v3000.2.17，glm-5-2/swe-1-7，CASCADE）顶层字段仅：`metadata/prompt/chatMessagePrompts/chatModelUid/requestType/configuration/tools/trajectoryReference/cascadeId/plannerMode/executionId`（连 `system_prompt_cache_options`/消息级 `prompt_cache_options` 也不发——纯隐式缓存）。 |
| `output_id`/`thinking_id`/`signature_type`/`phase` 回放                                    | **上游在免费档根本不下发这四个字段**（swe-2-max、glm-5-2、swe-1-7-medium 各数十次流均未出现），CLI 抓包的历史消息里也没有。结论：**无东西可回放**，gaps 建议 1 撤销。                                                                                                                                  |
| router uid 是否先 `AssignModel`                                                            | **证实**。`subagent-default`/`session-titler`/`command-reviser` 是活 router；详见第三节。                                                                                                                                                                                                              |
| `invalid_json_str` 触发                                                                    | **无法在 swe-2-max 上诱导**：连 `apply_patch` 这种 freeform 习惯的输出也被包成合法 JSON（`{"path": "*** Begin Patch..."}`）。大概率是 Fireworks 侧约束解码兜底，该字段在免费档近似死字段。                                                                                                             |

## 二、swe-2-max 响应帧清点（真实抓取）

一次正常响应的帧序：`deltaThinking`（**单帧整段**，不像 glm-5-2 逐小块流）→ `deltaText`+`deltaTokens` 逐块 → `deltaSignature`+`deltaSignatureType` → `stopReason` → `responseDimensionGroups`。

- **签名格式 `sealed.v1.<base64url>`，`delta_signature_type="sealed"`**。glm-5-2 与 swe-1-7-medium（Decart）**完全无签名帧**——签名是 swe-2/Fireworks 特有。
- **`stop_reason` 正常结束 = `STOP_REASON_STOP_PATTERN`**（不是 STOP），工具调用 = `FUNCTION_CALL`，`maxTokens=8` 实测烧完 thinking 后 `STOP_REASON_MAX_TOKENS`。`mapStopReason` 靠 default 兜到 Stop，建议显式列上 STOP_PATTERN/CONTENT_FILTER。
- 每帧带 `latency`（累计秒）、`requestId`、`timestamp`、`usage`；`usage.responseHeader.x-request-id` 是 provider 侧请求号（Fireworks=`chatcmpl-*`，Decart=`req_*`）——**排障金矿，建议进 debuglog**。
- `usage.apiProvider`：swe-2-max/glm-5-2 → `FIREWORKS_DEVIN`；swe-1-7-medium → `DECART`。
- 免费档**不下发** `creditCost`/`committed_*`/`actualModelUid`/`completionProfile`/`prompt`/`redact`/`geminiThoughtSignature`/`outputId`/`thinkingId`/`phase`/`arenaInvocationCapReached`。
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

| 字段                                                                               | 结果                                                                                                                                                                                                                                   |
| ---------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `tool_choice.option_name`                                                          | `none`（模型自述"tools disabled"拒调用）、`auto`（正常自选）、`required`（强制产生 tool call）有效；**`any` → `invalid_argument`**！ Anthropic `any` 必须映射成 `required`。`tool_name="X"` 强制调用指定工具（连无关 prompt 也照调）。 |
| `disable_parallel_tool_calls=true`                                                 | 被接受但**无效果**：swe-2-max 照样同轮发 `read_file_0`+`list_dir_1` 两个调用。                                                                                                                                                         |
| `provider_source=CASCADE`                                                          | 接受，无可观测差异。                                                                                                                                                                                                                   |
| `prompt_id`                                                                        | 接受；同 id 两连发无 dedup，照常各跑各的。纯关联字段。                                                                                                                                                                                 |
| `num_tokens`（消息级）                                                             | 接受，无可见差异。                                                                                                                                                                                                                     |
| `language=GO`、`chat_model_name=whatever`                                          | 静默接受。                                                                                                                                                                                                                             |
| `planner_mode` READ_ONLY/NO_TOOL/EXPLORE/PLANNING/AUTO                             | 全部接受但**纯提示性质**：NO_TOOL 下模型照样发 tool call。                                                                                                                                                                             |
| `step_type`（trajectory 内）                                                       | FINISH/MQUERY/PLANNER_RESPONSE 都接受——纯轨迹记账字段。                                                                                                                                                                                |
| `request_type`                                                                     | **只有 CASCADE 能在无会话状态下工作**。GENERAL/SMART_FRIEND/COMMAND/EVAL/CONTEXT_CHECK → `failed_precondition: error with your Cascade session`（需要 StartCascade 建过轨迹）。                                                        |
| `use_internal_chat_model`+enum                                                     | `permission_denied`——内部枚举通道对免费 token 关闭。                                                                                                                                                                                   |
| `chat_model_uid` 缺席                                                              | `unknown: an internal error occurred`。                                                                                                                                                                                                |
| 版本化 uid `swe-2-high-09102026`                                                   | `permission_denied`（versionId 是内部引用，不能直接当 uid）。                                                                                                                                                                          |
| `is_custom_tool`+`custom_tool_grammar`(lark)                                       | `unknown:` provider 错——swe-2-max 不走 freeform 语法通道。                                                                                                                                                                             |
| `json_schema_string` 非法 JSON                                                     | `unknown:` provider 错（坏 schema 直接打爆 provider 层）。                                                                                                                                                                             |
| `strict`/`read_only_hint`/`server_name`/`attribution_field_names`                  | 全部静默接受。                                                                                                                                                                                                                         |
| `experiment_config` 乱填                                                           | 静默接受。                                                                                                                                                                                                                             |
| `metadata.{session_id,request_id,device_fingerprint,disable_telemetry,user_agent}` | 全部静默接受。`f` 指纹缺失也照常（`-no-fingerprint` 正常返回）。                                                                                                                                                                       |
| `system_prompt_cache_options`/`prompt_cache_options`                               | 见 cache 文档，EPHEMERAL 无副作用。                                                                                                                                                                                                    |
| 空 `prompt`（system）+ tools                                                       | **现在能通过**——契约表第 8 条已过时，空 system 不再是拒因（至少 swe-2-max）。                                                                                                                                                          |
| SYSTEM_PROMPT 作消息 source                                                        | `unknown:` provider 错——system 只能走顶层 `prompt` 字段。                                                                                                                                                                              |
| `images`×20（单轮）                                                                | 全接受，模型正确数出 20。无尾部 cap 实测证据；CLI 的 `max_trailing_images` 是客户端策略不是 wire 约束。                                                                                                                                |
| 非视觉模型 + 图（glm-5-2）                                                         | `invalid_argument`——本地 `validateImagesForModel` 拦截方向正确。                                                                                                                                                                       |

## 五、CompletionConfiguration 真实语义

| 字段                               | 实测                                                                                                                          |
| ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `maxTokens`                        | **生效**，且 thinking 计入预算：maxTokens=8 全烧在 thinking 上、零 text、`MAX_TOKENS` 收尾。                                  |
| `maxNewlines=400`                  | **不生效**——500 行输出完整返回。CLI 照发是习惯，不是约束。                                                                    |
| `stopPatterns`                     | **不生效**——让模型输出 "ABC XYZ DEF" 并设 stop=XYZ，全量返回。**我们的 StopSequences 透传是空操作**，客户端指望 stop 会踩空。 |
| `numCompletions`>1                 | 流中途 `internal: stream error INTERNAL_ERROR`——CASCADE 只支持 1。                                                            |
| `temperature`/`topK`/`topP`/`seed` | 透传无障碍（未做细粒度验证）。                                                                                                |

## 六、签名回放 A/B（swe-2-max）

| 变体                        | 结果                                                            |
| --------------------------- | --------------------------------------------------------------- |
| thinking+ 真实 signature    | 正常，input=195                                                 |
| thinking 无 signature       | 正常，input=182                                                 |
| thinking+**伪造** signature | **正常**，input=180——**上游不校验 sealed 签名**，它只是透传存储 |
| 完全无 thinking 的纯文本    | 正常，input=163                                                 |

含义：回放签名给上游是可选的；我们留签名纯粹是为了客户端（Anthropic 侧 redacted thinking 校验）正确性，不是上游要求。

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

| code                               | 触发场景                                                                                                                                                                                | 语义                                                                                       |
| ---------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------ |
| `permission_denied`                | 无权模型 uid、内部枚举模型、版本化 uid、内容指纹句                                                                                                                                      | 权限/策略                                                                                  |
| `invalid_argument`                 | 坏 wire 形状：孤儿 TOOL、非 router uid 进 AssignModel、tool_choice=any、非视觉 + 图、缺 cascade 的 jwt、**超长 prompt**（文案 `"The prompt is too long for this model"`，带真实描述！） | 请求形状错                                                                                 |
| `failed_precondition`              | 非 CASCADE request_type                                                                                                                                                                 | 前置状态缺失（需真实 cascade 会话）                                                        |
| `not_found`                        | 不存在的 router 名                                                                                                                                                                      | 资源不存在                                                                                 |
| `unavailable`                      | router 直连、GetEmbeddings                                                                                                                                                              | **看似瞬时实则永久**——重试策略要注意：这类文案带 "try this model again later" 但永远不会好 |
| `unknown`                          | provider 层崩坏：坏 schema、is_custom_tool、SYSTEM_PROMPT/UNKNOWN source、缺 model uid                                                                                                  | provider 内部错，同样永久                                                                  |
| `internal` (HTTP/2 INTERNAL_ERROR) | numCompletions>1                                                                                                                                                                        | 流中断                                                                                     |

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

## 十、仍未解（本账号观测不到）

- `output_id`/`thinking_id`/`signature_type`/`phase`/`credit_cost`/`committed_*`/`provider_refusal`/`redact`/`gemini_thought_signature`/`completion_profile`/`actual_model_uid`——大概率只在付费档或 Anthropic-provider 模型上出现。
- `invalid_json_str`/`is_custom_tool_call`/`arena_*` 的线上形态。
- `prompt`(#19 响应回显）、`response_dimension_groups` 的完整语义（UI 用，无关紧要）。
