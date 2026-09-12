# 提前结束回合（premature end_turn）事件调查与同类故障普查

> 2026-09-12。起因：codex 经 `codex → ccload → devin-2api → Devin cascade` 链路跑论文笔记任务，commit 成功后"莫名其妙停止"。本文记录取证结论、外部同类问题（CLIProxyAPI issues + Anthropic 官方文档 + Google Antigravity 二进制逆向证据）、我们已有的防线与缺口，以及从 CPA issue/PR 普查映射到本链路的潜在问题清单。

## 一、事件还原与结论

末次请求调试目录 `logs/20260912-103422`（10:34:22 发出）。逐跳取证：

| 跳           | 证据                                                                                         | 结论                                                 |
| ------------ | -------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| codex        | `rollout-2026-09-12T10-28-30-*.jsonl` 记录 `agent_message` + `task_complete`                 | 收到纯文本 message、无 function_call，按协议结束回合 |
| ccload       | `ccload.db` logs 表 `200/ok`，in=587/out=35                                                  | 如实转发                                             |
| devin-2api   | `06-http-response.jsonl` 发出 `message_delta(stop_reason=end_turn)` + `message_stop`         | STOP_PATTERN→end_turn 映射正确                       |
| 上游 cascade | `04-devin-response.jsonl` 只有 `deltaText` 序列，收尾 `stopReason: STOP_REASON_STOP_PATTERN` | **上游从未产生 toolCall 帧，无内容可丢**             |

判定：模型在一次生成里只发出叙述文本"检查 prettier 是否重排了 markdown…"就采到 EOS；各层忠实转发；codex 按"completed 且无 function_call ⇒ 回合结束"正常收尾。这不是链路故障，是模型的 premature end_turn。

同模式当日复发（扫全部 done 事件，条件：最后输入为 tool_result 且响应纯文本）：

- `20260912-073015`："三类失败已经分出来了。现在看'中途停止'……"（调查会话中途停）
- `20260912-073125`："关键点找到了：第 118 行……"（同上）
- `20260912-103422`：本次
- 另有 ~4 次同形态但属合法终答（问天气、"hello-pi" 等）；10:07 会话（debug 未开、无 dir）codex rollout 显示同样以叙述文本 task_complete

量级约 3/~120 请求，集中在"宣布下一步"的叙述句上。

## 二、机制

两个契约差值得记住：

- **本链路的契约**：一次 HTTP 请求 = 一次生成 = 一个 assistant 回合。正常回合收尾帧是 `STOP_REASON_FUNCTION_CALL`（因发出 toolCall 而停，harness 执行后再调）；`STOP_PATTERN` 是自然 EOS。
- **原生 cascade 的契约**：一次 `GetChatMessage` = 一条 `ChatMessage`；历史序列化里叙述文字与 toolCalls 分属独立条目（`internal/adapter/devin/devin.go:558` 注释，Windsurf wire 实证：assistant 轮 = 可选文本消息 + 每个工具调用各一条）。

也就是说"产出一条纯文本消息"在模型的经验分布里是合法且常见的形态——历史里几十条 `SYSTEM: "我先读取…"` 就是这种消息。失误只在于这次纯文本消息后面跟的是 STOP_PATTERN 而非 toolCall 流。两种解释无法从本地日志彻底区分：

1. 模型 slip：本该在同一生成里继续吐 toolCall（其他 ~50 回合都这么做），这次提前 EOS；
2. 原生语义被压扁：若 Devin 原生 harness 对纯文本消息会再调一次（回合内多消息串行），则 stop 是翻译丢掉的语义。无法观察原生 harness，但旁证倾向于 (1)：Devin CLI 里纯文本输出同样是回合结束等用户的信号。

## 三、外部佐证：这是已知问题族

- **Anthropic 官方文档**明确记录 "Empty responses with end_turn"：模型在 tool_result 后返回 2-3 token 空内容 + `end_turn`，"particularly after tool results"；成因包括 tool_result 后紧跟文本块教会模型"该等用户说话"；**官方药方是追加新 user 消息续跑（"Please continue"），而不是原样重试**——模型已判定回合结束，重试同样的请求不会改变结论[^anthropic-stopreasons]。
- **CLIProxyAPI #4886**（open）：6 天实测 66 次空 end_turn（58 次 Anthropic），输入 65K-422K 重 cache-read——与我们 60K cache-read 的形态吻合。核心论点：empty/premature end_turn 是模型判断而非 provider 故障，轮换凭证/模型是错药[^cpa-4886]。
- **CLIProxyAPI #5227**（closed）：Gemini Antigravity 链路上模型整轮只吐 thinking、0 正文 0 调用，下游 Codex/Claude Code/Pi 停摆。关键发现：逆向 Google 官方 `agy` 二进制发现官方 harness 有专门的 `EmptyOutputContinuationCheckHook`（`cortex/executors/posthooks/empty_output_continuation_check.go`），配 `disable_empty_output_continuation`、`continuation_prompt_override`、`max_nominal_continuations`/`max_stop_hook_continuations`；官方 changelog 还记录修过 "forced-continuation deadlock"（等子代理时反复注入 continue 撞上限）——**此类 hook 必须配次数上限**[^cpa-5227]。
- **CLIProxyAPI #5439**（closed）：Claude→Responses 把 `max_tokens` 截断错报成 `response.completed`，"agent clients silently end turns without a final answer"；修法是映射为 `response.incomplete`[^cpa-5439]。
- **CLIProxyAPI #4881**（open PR）：把"真空完成"（0 内容 0 调用 0 token）当可重试故障并轮换；#4886 指出对 Anthropic 系应续跑而非轮换[^cpa-4881]。
- **CLIProxyAPI #4958**（closed）：上游 `refusal` 未翻译变成 `stop`+空内容，客户端无法区分拒答与正常完成[^cpa-4958]。

## 四、我们已有的防线（对照）

`internal/adapter/devin/response_decoder.go` + `internal/api/openai/responses/response.go`：

- 干净 EOF 但无 stopReason → `fail`（"Devin stream ended without stop reason"），注释明确记录过"合成 Stop 会把截断伪装成 end_turn，实测复现 Codex 宣告继续后直接 task_complete"——即"中途断流伪装完成"这类已经防住，**本次事件不同：上游显式声明了 STOP_PATTERN**；
- 无 stopReason 且无任何内容 → fail；`provider_refusal` → fail；
- `length`/`contentFilter` → Responses `response.incomplete`（对应 CPA #5439 已修的问题，我们映射正确）；
- `stopPatterns` 上游不生效 → 本地截断（见 upstream-gaps 6.5）；
- 孤儿 tool_result → `demoteOrphanToolResults` 降级为 user 文本（不伪造 tool_use_id，对比 CPA #5375 的 fabricate 方案）；
- 下游 SSE 10s keepalive 注释帧；上游 `http.Client.Timeout=610s`、`ResponseHeaderTimeout` 限首包。

## 五、缺口与可选对策

**缺口**：声明式 `STOP_PATTERN` + 纯文本（无 toolCall）目前直接作为 end_turn 透传。结构上与"合法终答"不可区分，只能靠内容（叙述意图 vs 收尾陈述）猜——脆弱。

可选做法，按侵入度排序：

1. **什么都不做**，用户侧打"继续"即可（上下文完整）；
2. **prompt 级**：在 codex 的 instructions/AGENTS 里加"叙述下一步动作后必须实际发起工具调用，不得以纯文本结束回合"；
3. **代理层 continuation hook**（agy 先例）：条件 = `end_turn` 且无 toolCall 且最后一条输入为 `tool_result`；追加一条合成 user 消息（如 "Please continue"）重发一次，把两次上游响应聚合成一个下游回合；**必须 `max_continuations ≤ 2`**（agy 的 deadlock 教训）。真·终答的代价是多跑一句废话；
4. 更保守的中间态：只在 `meta.json`/`index.jsonl` 打 `premature_endturn` 标记先观测频率，不干预。

## 六、其他可能踩到的问题（CPA issue/PR 普查 → 本链路映射）

### 事件序列编排（翻译层高发区）

CPA 的 bug 大半是 SSE 事件状态机：#5674（`content_block_stop` 指向从未开启的 index）、#5581（tool_use 未关就开新 content block）、#4544（`message_stop` 后再发 `message_delta`）、#4415（`message_stop` without current message）、#5651（非末块被盖 STOP）、#5229（tool_calls index 应从 0 起而非 anthropic block index）。我们的 anthropic encoder 已修过尾随 `DeltaSignature` 空块问题；**新模型/新内容形态（thinking 交错、签名、多 toolCall）出现时这块是复发区**，回归用真实 codex 流做端到端比对[^cpa-5674][^cpa-5581][^cpa-4544][^cpa-4415][^cpa-5651][^cpa-5229]。

### 历史形态兼容

- **孤儿 tool_result**：CPA #5375——Responses 历史里无 `call_id` 的 `function_call_output` 被伪造 `toolu_*` id 塞进 Claude 历史 → 400 且每次重放都炸（任务永久 wedge）。我们的 demote 策略不伪造 id，方向正确；但注意 codex 跨任务 replay 的内容会原样进来[^cpa-5375]。
- **"ends with model turn" 400**：#5607、#5358——Vertex/Gemini 拒绝以 model(assistant) 轮结尾的请求，触发场景是尾随车挂 system-reminder 或 reasoning+compaction item。**对应我们的风险点：codex `auto_compact` 压缩后产生的新历史形态**（compaction/summary item），若 cascade 侧对尾部形态敏感会表现为周期性 400；目前 demote 容错较强，未观察到[^cpa-5607][^cpa-5358]。
- **reminder 注入位置**：#4970——currentDate reminder 被插到 leading tool_result 之前触发 Anthropic 400。我们若以后在 ccload 加注入要注意插入点[^cpa-4970]。

### 缓存

- #5300：`prompt_cache_key` 自动派生（hash 首条 user 消息 + 截断 system prompt）把批负载打散到不同上游节点，前缀缓存命中率 0.4%。**ccload 若做 content-hash 亲和要留意此坑**；我们上游缓存是 best-effort 前缀匹配（~75-90%），实测 cache_read 正常增长[^cpa-5300]。
- #5727：anthropic-client→codex provider 时 `cache_control` 被接受但不建缓存（cache_creation=0）。我们透传 `PromptCacheOptions` 只在最后一条挂 ephemeral——符合上游用法[^cpa-5727]。

### 流与超时

- #5545：上游静默 ~30s 在 keepalive 边界被断流（ChatGPT 后端特性；terra/astra 推理时**完全无中间事件**，20%/15% 掉线率）。我们下游有 10s keepalive 注释帧；上游→我们这段若 Devin 长时间静默被断，表现为 EOF→fail（不会伪装成功）。swe-2-max 有 thinking delta，风险低，但**换用无中间事件的模型时要盯**[^cpa-5545]。
- **我们自有的硬顶**：上游 `http.Client.Timeout=610s` 是整请求上限——单次生成超过 ~10 分钟会被截断失败。当前 swe-2-max 单轮 <2min，余量够，但要知道这个顶的存在。
- **上游中途死寂无 watchdog**：`ResponseHeaderTimeout` 只管首包；流建立后若上游挂起（CPA #5360 的"无响应体吊死"），我们会一直等到 610s 顶。可加"N 秒无上游帧→fail"的读侧看门狗[^cpa-5360]。
- #4642：终态成功前缓冲重试（withhold stream until terminal success）的 opt-in 设计——对我们属重型方案；我们已有的"首个上游事件前错误以真实 4xx 返回"覆盖了更常见的一半[^cpa-4642]。
- #4884/#5028：上游缺 `[DONE]`/`response.completed` 时合成终止会把断流伪装成干净完成——我们已选另一边（缺 stopReason 直接 fail）[^cpa-4884][^cpa-5028]。

### 请求形状

- #5562/#5551：超大 tool schema（172 个工具/73K 字符描述）使 sol 返回空 SSE + `length`。codex 27 个工具离此很远，但若接入塞巨型 schema 的客户端要留意[^cpa-5562][^cpa-5551]。
- #5301/#5054：`stop_sequences`、`top_k` 等参数对不认的后端应 fail-closed 而非静默丢弃——我们已把上游不生效的 `stopPatterns` 做本地截断，同类参数按此原则处理[^cpa-5301][^cpa-5054]。

### 重试/冷却语义

- #4881 vs #4886 之争的实质：**"重试同一请求"对模型判断类失败无效**。我们单渠道（devin channel 独占上游），ccload 轮换无对象；真要兜底只有 continuation 一条路[^cpa-4881][^cpa-4886]。
- ccload 侧观察：10:07 起 `gpt-5.6-luna` 全部渠道冷却导致 503 风暴（与本次事件无关，但说明渠道冷却会表现为连发 503）。

## 参考文献

[^anthropic-stopreasons]: Anthropic. Handling stop reasons — Empty responses with end_turn. [docs.anthropic.com](https://docs.anthropic.com/en/docs/build-with-claude/handling-stop-reasons)

[^cpa-4886]: CLIProxyAPI issue #4886. Empty Claude end_turn turns need a continuation retry rather than credential rotation. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4886)

[^cpa-5227]: CLIProxyAPI issue #5227. feat(antigravity): handle pure-thinking empty outputs and evaluate auto-continuation hook. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5227)

[^cpa-5439]: CLIProxyAPI issue #5439. Claude Responses incorrectly completes max_tokens-truncated turns. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5439)

[^cpa-4881]: CLIProxyAPI PR #4881. fix(auth): treat empty upstream completions as retriable failures. [github.com](https://github.com/router-for-me/CLIProxyAPI/pull/4881)

[^cpa-4958]: CLIProxyAPI issue #4958. Anthropic refusal is translated to finish_reason "stop" with empty content. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4958)

[^cpa-5674]: CLIProxyAPI issue #5674. fix(antigravity): empty text part while thinking is open emits content_block_stop for an index that was never started. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5674)

[^cpa-5581]: CLIProxyAPI issue #5581. fix(translator/openai-claude): stop open tool_use block before starting new content block. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5581)

[^cpa-4544]: CLIProxyAPI issue #4544. bug(claude translator): late usage-only chunk can emit a second terminal message_delta after message_stop. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4544)

[^cpa-4415]: CLIProxyAPI issue #4415. Claude Code reports "Received message_stop without a current message" with gpt-5.6-sol. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4415)

[^cpa-5651]: CLIProxyAPI issue #5651. fix(translator/openai-gemini): finishReason "STOP" stamped on every non-final chunk. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5651)

[^cpa-5229]: CLIProxyAPI issue #5229. fix(claude-openai): index streamed tool_calls from zero, not from the Anthropic block index. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5229)

[^cpa-5375]: CLIProxyAPI issue #5375. Responses -> Claude fabricates tool_use_id for orphan function_call_output and wedges replayed tasks. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5375)

[^cpa-5607]: CLIProxyAPI issue #5607. Vertex AI returns 400 "Requests ending with a model turn are not supported" on tool_result with trailing system-reminder. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5607)

[^cpa-5358]: CLIProxyAPI issue #5358. Antigravity/Gemini backend still returns 400 "Requests ending with a model turn" when a trailing reasoning item precedes compaction_trigger. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5358)

[^cpa-4970]: CLIProxyAPI issue #4970. Claude cloaking: currentDate reminder is prepended before leading tool_result blocks, Anthropic rejects the request (400). [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4970)

[^cpa-5300]: CLIProxyAPI issue #5300. Codex: auto-derived prompt_cache_key defeats upstream prefix caching for batch workloads. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5300)

[^cpa-5727]: CLIProxyAPI issue #5727. anthropic-client → codex provider: cache_control is accepted but never creates a cache. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5727)

[^cpa-5545]: CLIProxyAPI issue #5545. Codex Responses SSE stream dies after ~30s of upstream silence on deep-reasoning models. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5545)

[^cpa-5360]: CLIProxyAPI issue #5360. 上游挂起（无响应体）时凭证轮换不触发，单个凭证吊死至反代超时。[github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5360)

[^cpa-4642]: CLIProxyAPI issue #4642. Recover transient terminal SSE failures before downstream commitment. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4642)

[^cpa-4884]: CLIProxyAPI issue #4884. Unconditional synthetic [DONE] on OpenAI-compat stream EOF masks premature disconnections as clean completions. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/4884)

[^cpa-5028]: CLIProxyAPI issue #5028. 502 "upstream stream closed before [DONE]" when upstream emits response.completed without [DONE]. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5028)

[^cpa-5562]: CLIProxyAPI issue #5562. gpt-5.6-sol returns empty SSE length finish for large OpenAI tool schemas. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5562)

[^cpa-5551]: CLIProxyAPI issue #5551. ChatGPT Codex channel: tool schema with full 13-branch oneOf returns empty stream. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5551)

[^cpa-5301]: CLIProxyAPI issue #5301. Claude → Codex must fail closed on unsupported stop_sequences and top_k controls. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5301)

[^cpa-5054]: CLIProxyAPI issue #5054. Claude-to-OpenAI translator emits single-element stop_sequences as bare string; strict backends reject with 400. [github.com](https://github.com/router-for-me/CLIProxyAPI/issues/5054)
