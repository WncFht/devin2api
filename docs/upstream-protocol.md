# Devin 上游协议逆向参考

> 按主题组织的活文档：`GetChatMessage` 的字段契约、响应帧形态、错误语义。结论随代码与复测持续更新。
>
> 证据分两级：**实测** = `cmd/probe` 或代理链路对真实上游打靶；**静态** = CLI 二进制 strings / 抓包 / proto bundle 分析。冲突时以实测为准。按日期排列的原始探测记录在 `notes/archive/`（`2026-09-12-upstream-live-probes.md` 是实测档案、`2026-09-12-upstream-gaps.md` 是静态字段清单，notes 不随仓库发布），本文吸收其结论。
>
> 分工：排障流程与客户端接入 → `upstream-debug-playbook.md`；内容策略指纹 → `upstream-policy-fingerprints.md`；前缀缓存 → `upstream-cache.md`；压缩责任 → `upstream-compaction.md`。
>
> 2026-09-15 全量复测：工具名字符集、call↔result 按位置配对、脏历史容忍度（trailing-assistant/thinking-only/dup-result）、`stopPatterns` 忽略、`disable_parallel_tool_calls` 无效、`sealed.v1` 尾随签名帧、swe-2-max 上下文边界约 219K-262K tok、`CheckUserMessageRateLimit` 恒 -1、`is_custom_tool`+lark grammar 确定性 `unknown`——以上结论全部维持。发生变化的条目在行内注明。

## 探测工具

`cmd/probe`：直连 `server.codeium.com` 的 Connect-RPC 实验工具，复用 `outputs/devin-proto-go` 生成绑定，token 与 `config.yaml` 同账号。

- `chat`：逐字段打靶，flag 覆盖 `-model` `-max-tokens` `-temperature` `-top-p` `-top-k` `-stop-pattern` `-num-completions` `-system` `-prompt-id` `-provider-source` `-planner-mode` `-request-type` `-tool-choice` `-disable-parallel` `-custom-tool` `-raw-schema` `-tool-extras` `-images` `-doc-file`/`-doc-mime`/`-doc-url`/`-doc-history` `-prompt-cache-key` `-language` `-chat-model-name` `-meta-extras` `-no-fingerprint` `-no-ids` `-trajectory-id` `-step-index` `-cascade-id` `-assign-jwt` `-internal-model` `-resolve`/`-router` 等。
- `edge <名>`：历史契约边界用例（配对/孤儿/乱序，见「工具调用契约」矩阵）；`hist`/`-shape` 覆盖回合形态变体。
- `replay -variant <名>`：签名回放 A/B（`with-sig`/`no-sig`/`bogus-sig`/`bogus-sig-typed`/`with-ids`/`sig-only`/`mutated-thinking`/`no-thinking`）。
- `rerun -file <03-devin-request.json>`：整包回放调试录制的 wire 请求（先用 `writefile()` 从 `debug_files` 导出，手法见 AGENTS.md「服务排障」节）。注意导出的 03 可能是 zstd delta 帧（magic `28B52FFD`）——此时必须给 `rerun` 加 `-dict <同 dir 01-http-request.json 的导出件>`（01 的 gzip 库存形态可直接喂，`--dict` 走同一魔数分派）；改走 `/admin/debug-logs/{id}/file/03-devin-request.json?raw=1` 端点拉取的已是解后明文，不需要 dict。回放会刷新 `metadata.api_key` 为当前 token 并重铸 `execution_id`——非严格逐字节一致；同 body 6/6 复放被接受。大历史回放很慢：706KB 真实 Codex 会话在 probe 自带 300s ctx 内流不完（上游接受但不在窗口内收尾）。
- `configs`/`status`/`assign`/`misc`：`GetCliModelConfigs`、`CheckChatCapacity`/`CheckUserMessageRateLimit`/`GetModelStatuses`/`GetModelProviders`、`AssignModel`、`GetEmbeddings`/`GetStatus`/`GetConfig`/`GetCommandModelConfigs`/旧 chat 面。
- `bigctx -kb N`：发 ~N KB 单条 user 消息观察错误码，探 prompt 长度上限的边界行为。
- `websearch -query <q>`：直连 `GetWebSearchResults` 打靶（`-limit`/`-domain`/`-mode`/`-provider`/`-wmodel`）。
- `toolchan <变体|all> [-n N] [-prompt 文本]`：工具文档通道可读性判别——合成工具字段魔法值只写进单一被测通道（description/schema 字段描述/system 注入段/required/anyOf），`sw-*` 变体用真实 ScheduleWakeup 定义复现事故与修复形态。
- `registry`：LanguageServerService 的 `GetSystemPromptAndTools`/`GetMcpServerStates`/`GetAllSkills` 按 planner-mode 分档探测——**整族在 server.codeium.com 不 host（两种包名路径全 404）**，工具注册表/system prompt 是客户端编译产物不走线上 RPC，该子命令的价值是阴性结论存档。

抓真实 CLI 流量的方法（`credentials.toml` 指本地捕获服务器 + 回放启动 RPC）见 `upstream-debug-playbook.md`「逆向参考」。

## 请求面：`GetChatMessageRequest`

### 顶层字段

| 字段                                                                                          | 实测结论                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                          |
| --------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `metadata`                                                                                    | `session_id`/`request_id`/`device_fingerprint`/`disable_telemetry`/`user_agent` 全部静默接受；`f` 指纹缺失也照常（真实 CLI 每请求发随机 `f`，我们已一致）                                                                                                                                                                                                                                                                                                                                                         |
| `chat_model_uid`                                                                              | **必填**，缺席 → `unknown: an internal error occurred`；版本化 uid（`swe-2-high-09102026`）→ `permission_denied`（versionId 是内部引用）                                                                                                                                                                                                                                                                                                                                                                          |
| `request_type`                                                                                | **只有 CASCADE 能在无会话状态下工作**；GENERAL/SMART_FRIEND/COMMAND/EVAL/CONTEXT_CHECK → `failed_precondition`（需 StartCascade 建过轨迹）                                                                                                                                                                                                                                                                                                                                                                        |
| `trajectoryReference`                                                                         | `trajectory_id` 可复用做会话续接标识；`step_index` 是单调计数器，真实 CLI 发送，上游接受                                                                                                                                                                                                                                                                                                                                                                                                                          |
| `tool_choice`                                                                                 | `option_name` 合法值 = `none`/`auto`/`required`（**`any` → `invalid_argument`**，Anthropic `any` 必须映射 `required`，2026-09-15 复测维持）；`none` 上游真执行——强制诱导下仍不发 call，模型自述「工具调用被禁用」；`tool_name="X"` 强制调用，指名不存在的工具 → 流内 `invalid_argument`；无 tools 时发 `required` 被容忍；**per-model 例外：swe-1-7（非 lightning）拒绝约束型 tool_choice——`required` 与 `tool_name` 都打回 `invalid_argument`→上游 502，仅 `none`/`auto` 可用**（lightning 与 swe-2-max 全接受） |
| `disable_parallel_tool_calls`                                                                 | 接受但**无效果**：照样同轮发多个调用——仅形状对齐                                                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `planner_mode`                                                                                | READ_ONLY/NO_TOOL/EXPLORE/PLANNING/AUTO 全部接受但**纯提示性质**（NO_TOOL 下照样发 tool call）                                                                                                                                                                                                                                                                                                                                                                                                                    |
| `prompt_id`                                                                                   | 接受；同 id 连发无 dedup——纯关联字段，CLI 自己也不发                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `prompt_cache_key`（#27，3.10.31 descriptor 新增）                                            | 任意字符串静默接受（2026-09-19 实测）；adapter 现填 `SessionAffinityKey(request)`——与 trajectory/cascade ID 同 sessionSeed 的 sha256 截断 hex，会话内稳定、对上游不透明                                                                                                                                                                                                                                                                                                                                           |
| `provider_source` / `language` / `chat_model_name`                                            | 全部静默接受、无可观测差异；CLI 抓包确认**都不发**（CLI 顶层只发 `metadata/prompt/chatMessagePrompts/chatModelUid/requestType/configuration/tools/trajectoryReference/cascadeId/plannerMode/executionId`，连 cache options 都不发——纯隐式缓存）                                                                                                                                                                                                                                                                   |
| `use_internal_chat_model` + 枚举                                                              | `permission_denied`——内部枚举通道对免费 token 关闭                                                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `experiment_config` / `strict` / `read_only_hint` / `server_name` / `attribution_field_names` | 全部静默接受                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                      |
| `system_prompt_cache_options` / 消息级 `prompt_cache_options`                                 | EPHEMERAL 无副作用，见 `upstream-cache.md`                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `cascade_id`                                                                                  | **不携带会话状态**——同 cascade 两请求互不可见（模型自述 "first message"）；续传只能靠回放 `chat_message_prompts`；AssignModel 的 jwt 绑 cascade_id（见路由节）                                                                                                                                                                                                                                                                                                                                                    |

### `ChatMessagePrompt`（消息）

| 字段                                                                                 | 实测结论                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                    |
| ------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `source`                                                                             | SYSTEM_PROMPT 作消息 source → `unknown` provider 错——system 只能走顶层 `prompt`                                                                                                                                                                                                                                                                                                                                                                                                                                             |
| `output_id`                                                                          | **OpenAI 路径有下发**（gpt-5-6-sol 系，`msg_*`，与同帧 signature 内 reasoning item 的 `rs_*` 同前缀）；回放有效。swe-2/glm/deepseek/gemini/claude 路径无                                                                                                                                                                                                                                                                                                                                                                    |
| `signature` / `signature_type`                                                       | 见「签名体制」节——回放必须配对                                                                                                                                                                                                                                                                                                                                                                                                                                                                                              |
| `thinking_id` / `phase`                                                              | 全 provider 未观测到                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                        |
| `prompt_annotation_ranges` / `safe_for_code_telemetry` / `num_tokens`                | 未设（IDE 用），仅记录——`num_tokens`(#4)/`safe_for_code_telemetry`(#5) 是消息级字段，不是顶层字段                                                                                                                                                                                                                                                                                                                                                                                                                           |
| `documents`（#21，`DocumentData{base64_data/mime_type/filename/url}`，3.10.31 新增） | 真实文档通道（2026-09-19 实测）：**per-model 按 `modelFeatures.supports_documents` 门控**——swe-2-max → `invalid_argument: model "swe-2-max" does not support file inputs`，claude-opus-5/sonnet-5/gpt-5.6/gpt-6-astra 系接受且模型真读到内容；**历史轮 documents 照常消费**（与历史图 invalid_argument 不同制，无「仅当前轮」限制）；`url` 形态有效但 **url 与 mime_type 互斥**（`"document url must not set a mime_type"`）；`supports_document_urls` 是独立能力位（本账号目录里与 supports_documents 恒共线，未单独门控） |
| `videos`（#20，`VideoData{base64_data/mime_type/url}`，3.10.31 新增）                | 仅 schema 面确认存在；Anthropic/OpenAI 客户端 API 无视频内容块可投到此通道，adapter 未接线，未实测                                                                                                                                                                                                                                                                                                                                                                                                                          |

### `ChatToolDefinition` / `ChatToolCall`（声明与调用）

| 字段                                                                                    | 实测结论                                                                                                                                                                                                                                                                  |
| --------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `is_custom_tool` + `custom_tool_grammar`(lark)                                          | **声明通道坏**：3/3 确定性 `unknown`（0 帧，provider 层错；2026-09-15 复测仍确定性 unknown，维持「坏」结论）。绕行方案已落地：客户端 custom 工具声明成单 `input` 字符串参数的 function，模型把原文填进 `{"input":"…"}`（实测 apply_patch 补丁按此下发），响应侧解包回原文 |
| `invalid_json_str` / `is_custom_tool_call`（历史方向）                                  | **有效**：回放 `is_custom_tool_call=true`+`invalid_json_str=<patch 原文>` 的 call 被正常消费、模型读到 patch 内容。解码端遇到应原文透传而非吞成 `{}`                                                                                                                      |
| `invalid_json_str`（响应方向）                                                          | swe-2-max 上不可诱导——provider 约束解码兜底，freeform 习惯的输出也被包成合法 JSON（`{"path":"*** Begin Patch…"}`）。免费档近似死字段                                                                                                                                      |
| `json_schema_string` 非法 JSON                                                          | `unknown` provider 错（坏 schema 直接打爆 provider 层）                                                                                                                                                                                                                   |
| `json_schema_string` 极简占位 `{"type":"object"}`                                       | 正常接受且模型真发起调用（FUNCTION_CALL 收尾）——anthropic 客户端执行工具（bash_/text_editor_ 系无 input_schema）按此形态转发的前提成立                                                                                                                                    |
| `arguments_json` 历史里非法 JSON                                                        | **非确定性**：一次流内 `invalid_argument`、一次正常应答（模型在 thinking 里吐槽参数坏）——坏参数历史可能打爆上游                                                                                                                                                           |
| `strict`/`read_only_hint`/`server_name`/`attribution_field_names`/`computer_use_config` | 静默接受，仅记录                                                                                                                                                                                                                                                          |

## 响应帧形态

### 帧序与 provider 对照

swe-2-max（Fireworks）正常响应帧序：~40 个仅含 `latency`/`timestamp`/`usage` 的心跳帧 → **单帧整段** `deltaThinking` → `deltaText`+`deltaTokens` 逐块 → `deltaSignature`+`deltaSignatureType` → `stopReason` → `responseDimensionGroups`。签名是**全部正文之后的尾随帧**。

跨 provider 对照（实测）：

| 维度                   | swe-2-max (FIREWORKS_DEVIN) | claude-opus-4-6-thinking (ANTHROPIC / ANTHROPIC_BEDROCK_GLOBAL) | gpt-5-6-sol (OPENAI_SAFETY_RETENTION)                                                                 | gemini-3-1-pro-high (GEMINI_DATABRICKS) | deepseek-v4-pro-high (FIREWORKS_DEVIN) |
| ---------------------- | --------------------------- | --------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- | --------------------------------------- | -------------------------------------- |
| `signature_type`       | `sealed`                    | `anthropic`                                                     | `openai`                                                                                              | 无签名帧                                | 无签名帧                               |
| signature 形态         | `sealed.v1.<b64url>`        | Anthropic 原生 base64                                           | **序列化 reasoning item JSON**（`[{"id":"rs_*","type":"reasoning","encrypted_content":"gAAAAAB…"}]`） | —                                       | —                                      |
| `outputId`             | 无                          | 无                                                              | `msg_*`                                                                                               | 无                                      | 无                                     |
| `usage.messageId`      | 无                          | `msg_*`（Anthropic 原生 id）                                    | 无                                                                                                    | 无                                      | 无                                     |
| 正常收尾 stopReason    | `STOP_PATTERN`              | **`MIN_LOG_PROB`**（end_turn 映射，字面误导）                   | 小样本 `UNSPECIFIED`                                                                                  | `STOP_PATTERN`                          | `STOP_PATTERN`                         |
| `usage.responseHeader` | `x-request-id: chatcmpl-*`  | `Request-Id: req_*`                                             | `x-request-id: req_*` + `openai-version`                                                              | `responseId` + `trafficType`            | `x-request-id: chatcmpl-*`             |
| thinking 形态          | 单帧整段                    | 多帧流式                                                        | 摘要走 deltaThinking，推理本体密封在 signature                                                        | 多帧摘要式                              | 多帧                                   |

补充：

- `apiProvider` 是 **per-request 属性**不是模型固有属性：claude-opus-4-6-thinking 同 uid 先后命中 `ANTHROPIC` 与 `ANTHROPIC_BEDROCK_GLOBAL`（后者 `msg_bdrk_*` + `X-Amzn-Requestid`）。本地 proto 的 APIProvider 枚举止于 `68=BEDROCK_MANTLE_FEDERAL`，服务端枚举持续在涨：**58 无定义**（glm-5-3-max 返回未命名 `"58"`），**68 之后又长出 69+**（swe-1-7 生产侧 18864 帧实测 `"69"`）。
- `usage.responseHeader` 不下发是 **provider-58 的特征而非 glm 模型的特征**——CLI 抓包里 glm-5-2 走 `FIREWORKS_DEVIN_SERVERLESS` 时有 `x-request-id: chatcmpl-*`。
- gemini 系（含 3-7/3-8-flash-medium，provider 已切 `GOOGLE_GENAI_VERTEX_GLOBAL`）不下发任何签名——`gemini_thought_signature` 在免费档是死字段。
- 免费档不下发 `creditCost`/`committed_*`/`actualModelUid`/`completionProfile`/`prompt`(回显)/`redact`/`provider_refusal`（文本拒绝是普通 deltaText）。
- 工具调用 id 格式按模型分：swe-2-max → `read_file_0`，glm-5-2 → `chatcmpl-tool-<hex>`——**不要假设 id 形态**。
- `cacheReadTokens` 是常态不是偶发：现 82.4% 请求命中（74991/91048，其中 74612 例 >1000 token）——EPHEMERAL 断点 + 稳定轨迹 ID 生效后缓存命中是默认路径，详见 `upstream-cache.md`。
- `premature_end_turn` 标记真实存在（修复 `d53dfde` 后口径占比 0.017%，15/91048），面板已透出。它是 shape 候选信号而非判定：任务正常收官轮（`tool_result` 输入 → 纯文本 → `STOP`）与真 premature 同形不可分，命中需逐条复核（口径见 `prematureEndTurn` 注释）；已实锤的病因是 responses 解码碎片化（issue #2，`d53dfde`）。

### stopReason 词表

- 正常收尾按 provider 分：`STOP_PATTERN`（swe-2/gemini/deepseek，与 stop_patterns 命中无关——枚举名误导）、`MIN_LOG_PROB`（claude，4/4 样本确认）、`UNSPECIFIED`（gpt-sol 小样本）。
- `FUNCTION_CALL` = 工具调用；`MAX_TOKENS` = 烧完预算（thinking 计入）；`CONTENT_FILTER` 存在。
- **缺 `stopReason` 的干净 EOF = 截断不是正常结束**（实测事故：Codex 把截断当完成 → task_complete）。decoder 报流错误而非合成 end_turn；唯一例外是本地 `stoppedByPattern`。

### 签名体制与回放规则

三种体制（`deltaSignatureType`）：`sealed`（swe-2/Fireworks）、`anthropic`（claude 系，原生签名）、`openai`（gpt-sol 系，signature = 序列化 Responses reasoning item，含 `encrypted_content`——回放给 `/v1/responses` 客户端可还原标准 reasoning item，是多轮 reasoning 的关键通道）。

回放校验严格度（实测 A/B）：**Anthropic（校验 blob 本体真伪，伪造 → 流内 `invalid_argument`）> OpenAI（只查 `signature_type` 配对，内容不验）> Fireworks（完全不校验）**。三者都不校验「签名与 thinking 正文绑定」（改写正文 + 原签名回放照常）。

规则：

- **`signature_type` 必须与 provider 配对**——张冠李戴触发流内 `invalid_argument`，比签名内容本身更敏感。
- 缺 `signature_type` 被容忍（OpenAI 路径 signature 字段被忽略），但它是 provider 路由提示且 OpenAI 路径还原 reasoning item 必须靠它——存起来更稳。
- 签名尾随帧到达时 thinking 块通常已关闭，解码侧合并回上一块（`decodeLateSignature`）；绝不能落成独立空 thinking 块（Claude Code 会整条丢弃消息）。
- **签名恒单帧到达**：2026-09-15 复核 1826 条流（20 条 probe 实测 + 1806 条历史 `04-devin-response.jsonl`），`deltaSignature` 全部恰好出现一次且与 `deltaSignatureType` 同帧——「签名跨帧分片」从未观测到。
- **签名位置按 provider 分**：anthropic 型落在 thinking 块结束处（正文之前的流中段）；sealed 型（swe-2/Fireworks）落在全部 `deltaText` 之后的流末尾——下游编码器必须把思考块 hold 到 flush 才发签名，否则 swe-2 的签名无处安放。
- 下游编码侧注意（本项目实现约束，非上游事实）：官方 Anthropic SDK 对 `signature` 是**赋值**非追加（`content.signature = delta.signature`）——`signature_delta` 只能发一条且须带完整签名，逐增量下发等于只留末片。

## 会话与路由

### AssignModel 路由链

```
AssignModel{model_router_uid, cascade_id}
  → {assignment_jwt(JWE, A256GCMKW), model_uid, harness_uids}
GetChatMessage{chat_model_uid=assignment.model_uid, model_assignment_jwt, cascade_id=<同一个>}
```

- `subagent-default` → `swe-1-7-medium`（DECART）；`session-titler`/`command-reviser` → `swe-1-7`。
- jwt **绑 `cascade_id`**（不一致 → `invalid_argument`）但**不绑 model_uid**（拿 subagent 的 jwt 跑 swe-2-max 正常）。
- 错误分类：非 router 正规 uid 进 AssignModel → `invalid_argument`；不存在的 router 名 → `not_found`；router 直连不走 AssignModel → `unavailable`（**伪装瞬时实则永久**）。
- `session-titler`/`command-reviser`/`swe-1-7-medium` 可直接当 `chat_model_uid`（不经 AssignModel）。
- 本账号 `GetCliModelConfigs` 零 router（`is_model_router` 全 false）、`inference_config` 全空——但 catalog 不列 router uid，`subagent-default` 仍被 AssignModel 正常解析（catalog 缺席 ≠ 不可用）。

### 隐藏可用 uid / 内部功能模型

二进制写死的内部 uid（上游全认识）：`session-titler`、`command-reviser`、`subagent-free-default`、`subagent-default`、`smart_friend_model_uid`（每模型伴生）。`request_type` 不一定是 CASCADE——CLI 给标题/revise 用别的 type。

模型枚举空间节选（strings）：swe-2-low/high/max/high-lite；pigeon-v4-vl/v4-devin/v6-low（routers）；kimi-k2p6/k2p7-code/k3-low/high/max；glm-5-3-low/high/max、glm-5p2（router）；decart-swe-1.7、hestia2/3、chiron、swe-1-6(-fast)、claude-opus-4-7-medium、gpt-5.4。

## 工具调用契约

### call↔result 配对矩阵（`edge` 用例实测，swe-2-max）

**唯二硬约束：结果必须挂在已存在的 call 之后；不能先堆多个 call 再批量给结果。** 其余「脏历史」远比想象宽容。

| 形态                                                                    | 结果                                                                                                                    |
| ----------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| call,result,call,result（正确配对）                                     | 正常                                                                                                                    |
| call,call,result,result（分组）                                         | **`invalid_argument`**——结果必须跟「最近的未配对 call」紧邻                                                             |
| 单条 assistant 消息内含两个 call + 后续两条连续 result                  | 正常——致命形态不是「分组」本身，而是「前 call 的 result 未到、下一条 call 消息引入新 id」；同消息内多 call 一次声明无碍 |
| call,user,result（中间夹 user）                                         | 正常                                                                                                                    |
| 两条 call 同 id + 一条 result                                           | 正常                                                                                                                    |
| 一条 call + 同 id 两条 result                                           | 正常，两份文本都送达                                                                                                    |
| call(c1) + result(zzz) id 不匹配                                        | 正常——按位置绑定到挂起的 c1                                                                                             |
| 无任何 call 的孤儿 result（含带 tool_call_id）                          | **`invalid_argument`**                                                                                                  |
| 历史以未应答 call 结尾                                                  | 正常（上游不补但不报错）                                                                                                |
| 历史以 tool result 结尾                                                 | 正常                                                                                                                    |
| trailing-assistant / 空 user / dup message id / thinking-only assistant | 全部正常                                                                                                                |
| redacted thinking + 伪造 `sealed.v1.` 签名                              | 正常                                                                                                                    |
| 空 assistant(SYSTEM source) + user "continue"                           | 正常续说——`continueEmpty` 重发路径的形态依据                                                                            |
| 半截 assistant（句中截断/无签名 thinking）+ user "continue"             | 正常续说（含 mid-word 接续）——`tryResume` post-commit 续传的形态依据（`edge stall-resume-*`）                           |

含义：`RequestMessages.DemoteOrphanToolResults`（IR 层）+ `pairToolCallsWithResults`（wire 层）是承重墙；孤儿 result 的 demote 是安全降级（有挂起 call 时上游按位置容忍 id 不匹配）。

### 工具名与 tool_choice

- 合法字符集约 **`[A-Za-z0-9_-]`**：`a.b`/`mcp::x`/`a-b_c.d`/非 ASCII → 全 `invalid_argument`（文案模糊 "internal error"）；`mcp__a__b` 合法。入口本地校验把模糊流内错变成可读 400。
- `tool_name` 指名不存在工具 → 流内 `invalid_argument`（第 2 帧后）。
- `required` 无 tools → 容忍，正常返回文本。
- MCP 工具名（`mcp__ide__getDiagnostics`）可声明可强制调用；50/150 个工具声明无上限迹象。

### custom/freeform 工具（Codex apply_patch）

- **声明通道** `is_custom_tool`+grammar → 确定性 `unknown`（坏）。
- **绕行方案（已实现）**：声明为单 `input` string 参数的 function（`{"properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`），`format.definition` 的 lark grammar 注入工具 description；模型实测把完整 patch 填进 `{"input":"*** Begin Patch\n…"}`；响应侧按声明名集合解包回原文，下游还原 `custom_tool_call`/`custom_tool_call_input.*` 事件。
- **历史通道** `invalid_json_str`+`is_custom_tool_call` 有效（见字段表）。
- 模型偏离包装 schema（裸文本/多键）时按 freeform 语义整体透传原文。

## `CompletionConfiguration` 语义

| 字段                               | 实测                                                                                                                                                                                                       |
| ---------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `maxTokens`                        | **生效**，thinking 计入预算（max=8 全烧 thinking、零 text、`MAX_TOKENS` 收尾）。**送达按 chunk 粒度可超 cap，计费按 cap**：max=1 时 ~40 thinking token 送达但 `outputTokens=1`。`deltaTokens` 只数 text 帧 |
| `stopPatterns`                     | **不生效**（swe-2 与 gpt-sol 复测同）——字段静默接受被忽略；`STOP_PATTERN` 枚举与 stop 命中无关。本地尾部截断是 stop 序列的唯一实现                                                                         |
| `maxNewlines`                      | 不生效（500 行照发），CLI 照发是习惯不是约束                                                                                                                                                               |
| `numCompletions`>1                 | 5 帧后 `invalid_argument: protocol error: incomplete envelope: unexpected EOF`（2026-09-15 复测，确定性 ×2）——CASCADE 只支持 1                                                                             |
| `temperature`/`topK`/`topP`/`seed` | 透传无障碍；`temp=0.5+top_p=0.5` 在 glm-5-3-max(preserveThinking)/swe-2/claude-thinking 全正常——无「thinking 强制采样参数」约束                                                                            |

空文本轮可稳定复现：`max_tokens≤8` 全部 `MAX_TOKENS` + thinking-only + 零 text——「有 stopReason 零内容」的真实形态，与 emptyEndTurn（正常 stop 零内容）不同源。

## 限流与配额

- **两套独立系统**：`CheckUserMessageRateLimit` 恒报 `{messagesRemaining:-1}`（无限，2026-09-15 复测维持），但真实生成路径有限流——触发 `resource_exhausted: …reset in N seconds|minutes`（seconds 与 minutes 两种粒度都出现过，解析侧均已支持）；触发阈值与封禁形态按「分钟桶量化 + 概率执行」模型，详见 `upstream-rate-limit.md`。结论：「容量检查说有」≠「生成不报 429」。
- 该错误**无 Retry-After 头、无 RetryInfo detail**——唯一机器可用信息是文案里的秒数，已解析透传。
- Connect 响应 header/trailer 只有标准字段，**trailers 恒空**——上游不在 HTTP 层给配额信号；唯一供应商侧锚点是 `usage.responseHeader.x-request-id`（已进 diagnostics）。
- **瞬时全断态真实存在**：~3 分钟窗口内所有 RPC（含一元）全部 `unavailable: unexpected EOF` 后自愈；10 连发偶发 0 帧 EOF；流式响应在 envelope 头写到一半时被砍（`invalid_argument: protocol error: incomplete envelope`，实测同波次多条独立连接同步死亡，本机 TUN 代理栈在路径上）——`tryReopen` 对传输断裂的 pre-content 重试覆盖的是正确分类。

## 错误分类学（Connect code → 语义）

| code                  | 触发                                                                                                                                                                                                                                  | 语义                                                                                            |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- |
| `permission_denied`   | 无权 uid、内部枚举、版本化 uid、内容指纹句                                                                                                                                                                                            | 权限/策略                                                                                       |
| `invalid_argument`    | 坏 wire 形状：孤儿 TOOL、分组 call/result、非 router uid 进 AssignModel、`tool_choice=any`、缺 cascade 的 jwt、指名不存在工具、超长 prompt（`"The prompt is too long for this model"` 带真实文案）、`numCompletions>1` 的流内截断形态 | 请求形状错                                                                                      |
| `failed_precondition` | 非 CASCADE request_type；非法 `request_type` 枚举值（999）→ `"please update your editor"` 版本门文案                                                                                                                                  | 前置状态缺失（需真实 cascade 会话）                                                             |
| `not_found`           | 不存在的 router 名；**枚举内但本账号未 provision 的 router uid 同归于此**（pigeon-v4-devin/v6-low、glm-5p2 实测全 `not_found`）                                                                                                       | 资源不存在                                                                                      |
| `unavailable`         | router 直连、GetEmbeddings；**也有真瞬时**（0 帧 EOF 风暴、连接被中间设备截断）                                                                                                                                                       | "try later" 文案是固定模板，多数为永久语义错；unwrap 链带 io/net 错误的形态是传输断裂，可重试   |
| `unknown`             | provider 层崩坏：坏 schema、`is_custom_tool` 声明、SYSTEM_PROMPT source、缺 model uid；非法 `internal_model` 枚举（99999）**先正常流 105 帧再死**——枚举字段无前置校验                                                                 | provider 内部错，永久                                                                           |
| `internal`            | （旧观测：numCompletions>1 曾报 `INTERNAL_ERROR`；2026-09-15 复测已变为 `invalid_argument: incomplete envelope`）                                                                                                                     | 流中断                                                                                          |
| `resource_exhausted`  | 高频请求                                                                                                                                                                                                                              | 真限流，hint 只在文案 `reset in N seconds/minutes`（seconds 与 minutes 两种粒度，代码均已解析） |

含义：`unavailable`/`unknown` 的 "experiencing issues / try later" 文案是误导性模板，真实原因是确定性请求/权限问题。已落实：`isTransientConnectError` 按 unwrap 链区分——connect-go 把传输断裂统一包成 connect.Error（RoundTrip 断 → `unavailable` 包 EOF、envelope 截断 → `invalid_argument: protocol error:`、tcp 重置 → 包 `*net.OpError`），链上带 io.EOF/io.ErrUnexpectedEOF/net.Error 的判传输断裂（建立阶段重试 + pre-content 重发、记 `devin_transport`）；链上干净的 connect.Error 才是上表语义拒绝（记 `devin_connect`，不重试）。

错误体的机器可读面（2026-09-15 全量复核）：**所有 connect 错误零 details、零 Retry-After、trailers 恒空**——code 与 message 是唯一信号；message 多为 opaque 模板 `"an internal error occurred (trace ID: …)"`（permission_denied/invalid_argument/unknown 通用），真实描述性文案只在少数类出现（prompt-too-long、editor 版本门）。`trace ID` 尾缀是排障锚点。**静默吞掉不报错**的字段：`experiment_config` 的伪实验串、`provider_source` 非法枚举——解析侧勿指望上游兜底校验。

## 其它 RPC 面

| RPC                                                                                      | 实测                                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ---------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GetCliModelConfigs`                                                                     | 模型表 209 条（cascade 版 210，差一条 legacy `MODEL_CHAT_GPT_4O_2024_08_06`）；对比 09-13 快照新增 18 个 uid、无删除：`gpt-5-3-codex-{low,medium,high,xhigh}[-priority]`×8、`gpt-5-4-{none,low,medium,high,xhigh}[-priority]`×10。多 `subagent_default_model_uid`（=`subagent-default`）与 `default_override_model_config`（=`{swe-2-high, swe-2-high-09102026}`）；本账号零 router、`inference_config`/`smart_friend_model_uid` 全空 |
| `CheckUserMessageRateLimit`                                                              | 恒 `{hasCapacity:true, messagesRemaining:-1}`（2026-09-15 复测维持）——与实际限流独立                                                                                                                                                                                                                                                                                                                                                  |
| `GetModelStatuses`                                                                       | 返回异常模型告警——实测 MODEL_8341 elevated error rate，09-15 快照新增 MODEL_15133 同款告警（MODEL_8341 仍在）；可作健康检查源                                                                                                                                                                                                                                                                                                         |
| `GetModelProviders`                                                                      | 12 家（xAI/DeepSeek/Qwen/NVIDIA/ThinkingMachines/Windsurf/OpenAI/Google/Moonshot/Z.ai/MiniMax/Anthropic）                                                                                                                                                                                                                                                                                                                             |
| `GetStatus`/`GetConfig`                                                                  | 空响应                                                                                                                                                                                                                                                                                                                                                                                                                                |
| `GetCommandModelConfigs`                                                                 | 6 条 legacy enum uid                                                                                                                                                                                                                                                                                                                                                                                                                  |
| `GetEmbeddings`                                                                          | `unavailable`——不对该 token 开放                                                                                                                                                                                                                                                                                                                                                                                                      |
| `GetStreamingExternalChatCompletions`                                                    | `invalid_argument`——旧版 Windsurf chat 面，走 `model_id` 枚举，关着                                                                                                                                                                                                                                                                                                                                                                   |
| `GetWebSearchResults`                                                                    | 托管搜索实际在用（`websearch.go`）：`allowed_domains` 在上游是单域字面量（多值实测返回 0 条），多域逐域调用按 URL 去重；`blocked_domains` 上游无对应字段，结果侧按 host 后缀过滤；unary 调用同样计速率配额，每次调用过本地闸门                                                                                                                                                                                                        |
| `LanguageServerService`（`GetSystemPromptAndTools`/`GetMcpServerStates`/`GetAllSkills`） | 整族在 server.codeium.com 不 host（`probe registry` 实测全 404）——工具注册表与 system prompt 是客户端编译产物，不走线上 RPC                                                                                                                                                                                                                                                                                                           |

`ModelFeatures` 能力位透出范围：`/v1/models` 投影 6 个位——`supports_images`/`supports_documents`/`supports_tool_calls`/`supports_parallel_tool_calls`/`supports_thinking`/`preserve_thinking`（`modelCapabilities`，与 adapter `ListModels` 采集集相同）；3.10.31 descriptor 刷新后 `supports_documents`(#29)/`supports_document_urls`(#30)/`supports_video`(#27)/`supports_video_urls`(#28)/`summarize_thinking`(#31) 都进了 vendored proto——documents 两位已投影（`/v1/models` + ccpanel upstream 视图），`supports_documents` 还驱动 adapter 的本地门控（无文档能力模型 + 文档 → `invalid_argument`，镜像上游文案）；`interleave_thinking`/video/summarize 位不投影。glm/kimi/grok/inkling/deepseek/nemotron 全系缺 `supports_parallel_tool_calls`。

`InferenceConfig`（oneof openai/google/anthropic/zai/xai/thinking_machines）是**服务端按模型下发的配置面**，不是请求参数——reasoning effort 不走 `GetChatMessage`，只能换 uid 档位。

## 图片与文档

- 真实视觉通道：swe-2-max 与 claude-opus-4-6 都正确读图（inputTokens 计入）；**TOOL 消息挂图同样被消费**（tool_result 图像子通道有效）。
- 单轮 20 张全接受——CLI 的 `max_trailing_images` 是客户端策略不是 wire 约束。
- `mime_type=application/pdf` 走 Images 通道 → `invalid_argument`；**2026-09-19 复测推翻「document 块是死路」**：3.10.31 descriptor 刷新带来 `ChatMessagePrompt.documents`(#21) 真实文档通道——PDF/txt 经 `DocumentData{base64_data,mime_type,filename}` 上行后模型正确复述内容；`url` 形态同样有效但与 `mime_type` 互斥（同设 → `invalid_argument`）；按 `supports_documents` 能力位 per-model 门控（swe-2-max 拒收，claude-5/gpt-5.6/gpt-6-astra 系放行）；历史轮文档照常可读，无图片的「仅当前轮」限制。
- 退化图片（1×1 PNG）→ `invalid_argument`——上游对图片有最小有效性校验。
- 非视觉模型 + 图曾被判 `invalid_argument`，**2026-09-15 复测已推翻**——glm-5-3-low、deepseek-v4-pro-high 带图直接放行应答，`validateImagesForModel` 的上游依据消失，本地闸门成了唯一拦截者。软信号：这些模型对图内容的回答全错——图片是否真实送达模型侧存疑，非视觉模型放图即使不被拒也可能只是静默无效。

## CLI 侧情报（静态）

### `InferenceRequest` 整形策略（CLI 内部统一推理请求）

`max_trailing_images`（尾部 N 条挂图，超限写 "Images omitted" 占位）、`disable_prompt_cache_writes`、`prefix_mismatch_behavior`（error/drop_block）、`append_only_history`（cache 友好）、`system_prefix_len`、`hosted_tool_search`（服务端托管工具搜索，wire 未暴露）。

### 内部专用提示词（不走 GetChatMessage 主链，但暴露预期形状）

- `agent-ext/title`：80 字以内纯标题，无引号无标点包裹。
- `agent-ext.revise-command`：只输出改写后命令。
- `agent-ext/looper`：评审循环，强制 `<ACCEPT>`/`<REJECT>` 收尾。
- `agent-ext/btw`：fork 主会话的只读侧聊，紧凑输出。
- `smart_permission/classifier`、`hooks/evaluator`：本地逻辑不调模型。
- `skills/*`、`rules/*`：本地配置注入器，序列化进 system prompt。

### CLI 错误枚举

`Unauthenticated/Timeout/RateLimited/QuotaExhausted/UsageLimitReached/ServerError/ClientError/Refusal/ContextTooLong/PayloadTooLarge/Disconnected/MalformedResponse/AuthFlowError`——`ContextTooLong` 与 `PayloadTooLarge` 是独立错误。上游实测的 `invalid_argument:"prompt is too long"` 已归一 413；若上游换 code 表达需在归一表补条目。

## 未观测清单（本账号不可见）

- 响应侧：`thinking_id`/`phase`/`credit_cost`/`committed_*`/`provider_refusal`/`redact`/`gemini_thought_signature`/`completion_profile`/`actual_model_uid`/`arena_*`；`invalid_json_str`/`is_custom_tool_call` 的响应方向线上形态；`thinkingRedacted` 从未置位（`edge thinking-empty-sig` 的红 acted 形态回放被接受，但上游自己不产生）。
- `deltaSignature` 跨帧分片：1826 条流全部单帧到达——编码侧为分片预留的 hold-open 是防御纵深，不是已观测需求。
- `prompt`(#19 回显）、`response_dimension_groups` 完整语义（UI 用，无关紧要）。
- protocensus 注意：请求侧 `file`/`size`/`sha256` 等 unknown-keys 告警是 **debuglog 附件引用封套的误报**，不是真 proto 空洞。

## 代理侧落点（实现位置速查）

| 契约                                                 | 落点                                                                                                                     |
| ---------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| call→result 重排 / 孤儿 result 降级                  | `pairToolCallsWithResults`（wire）/ `RequestMessages.DemoteOrphanToolResults`（IR，解码尾）                              |
| 助手回合合并单条 ChatMessagePrompt                   | `buildRequest`（request_encoder.go）                                                                                     |
| stop 序列本地截断                                    | `responseDecoder` 尾部窗口 + `stoppedByPattern`                                                                          |
| 缺 stopReason 判截断                                 | `responseDecoder.finish`                                                                                                 |
| 签名合并 / signature_type 配对回放                   | `decodeLateSignature` / `classifyReasoningSignature`                                                                     |
| custom 工具包装与解包                                | `customToolInputSchema`（responses request.go）/ `unwrapCustomToolArguments` + `customTools` 集合（response_decoder.go） |
| 工具名字符集校验                                     | `toolNameCharset`（llm/request.go）                                                                                      |
| `any`→`required` 等 tool_choice 映射                 | `common.ParseOpenAIToolChoice` + adapter buildRequest                                                                    |
| 传输断裂（unwrap 链判据）重试 / 语义拒绝不重试       | `isTransientConnectError` / `tryReopen`                                                                                  |
| 限流文案秒数 → Retry-After                           | 错误归一层                                                                                                               |
| provider 侧 request id / api_provider 进 diagnostics | `responseDecoder.updateMetadata`                                                                                         |
| XML 参数泄漏修复                                     | `repairLeakedXMLArguments`                                                                                               |
| 托管 web_search（代调上游搜索 + 续轮）               | `runWebSearch` / `handleServerCalls`（websearch.go / devin.go）                                                          |
