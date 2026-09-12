# 提前结束回合（premature end_turn）事件调查与同类故障普查

> 2026-09-12。起因：codex 经 `codex → ccload → devin-2api → Devin cascade` 链路跑任务时反复"说完话没去调工具就停止"。最终定位：**a0d4a80 把助手回合的历史编码从真实客户端的合并形态改成了拆分形态，在渲染上下文里插入了假的回合边界，模型生成到宣告文本末尾时采到 EOS 的概率被显著抬高**。已修复并有因果级证据。本文按「现象 → 逐跳取证 → 根因 → 因果验证 → 修复」重述，后附同类故障普查。

## 一、现象

典型案发现场 `logs/20260912-165011`：模型输出停在「看 `websocket.go:299` 所在的 collectedOutput 上下文：」——一句明确的"我接下来要调工具"宣告，然后回合就结束了，没有任何工具调用。用户需要手动打「继续」才能续跑。

这形态当日高频复发。扫全天 544 个请求的 `04-devin-response.jsonl`：

| 上游 stopReason | 次数 | 其中宣告式中途停止                                                                  |
| --------------- | ---- | ----------------------------------------------------------------------------------- |
| FUNCTION_CALL   | 480  | —                                                                                   |
| STOP_PATTERN    | 51   | ~15 次（修复前时代），全部停在「让我看 X：/先编译一轮：/Let me see…」这类宣告句末尾 |
| MAX_TOKENS      | 1    | —                                                                                   |
| 无 stopReason   | 12   | 均为客户端断连/499/400，无谜团                                                      |

也就是说真正的 bug 不是「STOP_PATTERN 出现」（合法终答也走它），而是**它反复出现在宣告句末尾**——模型刚说完要做什么，回合就结束了。

## 二、逐跳取证：链路没有丢帧

对失败请求逐层核对（表以 165011 为例，其余同形态）：

| 跳           | 证据                                                                                         | 结论                                                 |
| ------------ | -------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| codex/ccload | rollout 记录 `agent_message` + `task_complete`；ccload `200/ok` 如实转发                     | 收到纯文本、无 function_call，按协议结束回合，无过错 |
| devin-2api   | `06-http-response.jsonl` 发出 `message_delta(stop_reason=end_turn)` + `message_stop`         | STOP_PATTERN→end_turn 映射正确                       |
| 上游 cascade | `04-devin-response.jsonl` 只有 `deltaText` 序列，收尾 `stopReason: STOP_REASON_STOP_PATTERN` | **上游从未产生 toolCall 帧，无内容可丢**             |
| 请求侧       | `01`/`03`：客户端未传 `stop_sequences`，编码器也未注入 stop 配置                             | 停止不是我们配的 stop pattern 误伤                   |

结论第一步：每个翻译层都忠实——SSE 没有丢 tool_call 帧，请求没有被截断。问题在上游模型为什么在那个位置采了 EOS。

## 三、根因：助手回合的 wire 形态分叉

对比真实客户端抓包（`outputs/exa.api_server_pb.ApiServerService/GetChatMessage/06/request.txt`，同为 chisel 3000.2.17）与我们发给上游的 `03-devin-request.json`：

- **真实客户端：一个助手回合 = 单条 `ChatMessagePrompt`**。`prompt`+`thinking`+`toolCalls` 数组同体携带；无文本时 `prompt` 字段缺席；抓包中从不出现相邻的 SYSTEM-SYSTEM 对。
- **我们的编码（a0d4a80 引入）：同一回合被拆成多条**——「文本+thinking+ 签名」一条 SYSTEM prompt，之后每个 tool call 各一条只带 thinking 的 SYSTEM prompt，TOOL 结果交错其间。历史里每个回合都变成 `文本 → [边界] → 调用 → 结果 → [边界] → 调用` 的形态。

机制解释：渲染后的上下文里，每次「宣告文本」后面都跟着一个消息边界再才是 toolCall。模型在自己的经验分布里学到的是合并形态——「宣告文本 → toolCall」是同一生成内部的连续流；我们的历史却反复演示「宣告文本 → 消息边界」。于是生成到宣告句末尾时，「回合到此结束」的后验概率被系统性抬高，模型采 EOS 而不是继续吐 toolCall。

a0d4a80 的本意是修上游 `invalid_argument`，但真正的约束只是 **call→result 必须交错相邻**（「全部调用→全部结果」的分组排列被拒）；「必须把调用拆到独立 prompt」是当时的误归因——probe `hist -shape merged` 实测单 prompt 携带 2 个调用 + 两份结果，上游正常接受。

## 四、因果验证：A/B 回放

`probe rerun`（新子命令）直接回放落盘的 `03-devin-request.json`；用脚本按「同 thinking 的连续 SYSTEM 调用 prompt 合并为一」的规则把拆分历史变换成合并形态。三次真实中途停止的请求，同一段历史、同一时刻、同一账号，唯一变量是 wire 形态：

| 请求   | 拆分形态（原始）         | 合并形态                 |
| ------ | ------------------------ | ------------------------ |
| 165011 | 8 次回放：3 次宣告即 EOS | 8 次：全部 FUNCTION_CALL |
| 165447 | 6 次：3 次宣告即 EOS     | 6 次：全部 FUNCTION_CALL |
| 165746 | 6 次：4 次宣告即 EOS     | 6 次：全部 FUNCTION_CALL |
| 合计   | **10/20 提前停止**       | **0/20**                 |

两个值得注意的细节：

- 合并形态下模型仍然输出同样的宣告文本（「更新 `websocket.go` 里 `collectedOutput` 的调用：」），但文本后面紧跟 toolCall 流——宣告和调用回到同一生成内部。
- 对照组：拆分时代一个本来正常 FUNCTION_CALL 结束的请求回放 4/4 正常——拆分不是处处炸，是条件性抬高宣告位置的 EOS 概率，与机制解释吻合。

生产侧同步佐证：拆分时代（当日 480 请求）约 3.1% 是宣告式中途停止；合并部署（17:28）后 66 请求中 5 次 STOP_PATTERN 全部是完整最终回答，**0 次宣告式停止**。生产样本量仍小，但与实验方向一致。

### 排除的其他嫌疑

- **signature/outputId/thinkingRedacted 回放**：真实抓包从不带这些字段，但 merged-nosig 变体回放同样 6/6 FUNCTION_CALL——签名不是停止诱因。保留回放是因为既有签名矩阵实验证明部分模型路径需要（type 配对强制，payload 不校验）。
- **messageId 稳定性**：抓包 05→06 同一逻辑消息的 messageId 全部变化——真实客户端也是每请求随机，与我们一致。
- **trajectoryReference.stepIndex**：真实客户端携带（会话内单调计数，capture06 为 6），我们没发。这是残留的形态差距，属轨迹记账字段、不进渲染上下文，与停止无关联证据；忠实还原需要会话级计数状态，记为已知差异。

## 五、修复

`convertMessage`（`internal/adapter/devin/request_encoder.go`）：助手回合恢复合并编码——一条 prompt 携带全部 text/thinking/签名/toolCalls，空助手消息跳过；`pairToolCallsWithResults` 适配「单 prompt 多调用」的配对；`demoteOrphanToolResults` 不变。新增 `TestBuildRequestMergesParallelToolCalls` 覆盖多调用合并 + 按序配对。

提交：`66ade29`（修复）、`02153b3`（回归测试）、`e829265`（probe rerun + 本文档数据）。

## 六、契约差与外部佐证：这是已知问题族

本链路的契约：一次 HTTP 请求 = 一次生成 = 一个 assistant 回合。正常回合收尾帧是 `STOP_REASON_FUNCTION_CALL`；`STOP_PATTERN` 是自然 EOS。修复后「宣告文本 → toolCall」在一次生成内连续产出，与真实客户端一致。

外部同类问题印证「过早 EOS」是协议翻译层的通病：

- **Anthropic 官方文档**明确记录 "Empty responses with end_turn"：模型在 tool_result 后返回 2-3 token 空内容 + `end_turn`，"particularly after tool results"；成因包括 tool_result 后紧跟文本块教会模型"该等用户说话"；**官方药方是追加新 user 消息续跑（"Please continue"），而不是原样重试**[^anthropic-stopreasons]。
- **CLIProxyAPI #4886**（open）：6 天实测 66 次空 end_turn（58 次 Anthropic），输入 65K-422K 重 cache-read——与我们 60K-140K cache-read 的形态吻合。核心论点：empty/premature end_turn 是模型判断而非 provider 故障，轮换凭证/模型是错药[^cpa-4886]。
- **CLIProxyAPI #5227**（closed）：Gemini Antigravity 链路上模型整轮只吐 thinking、0 正文 0 调用。逆向 Google 官方 `agy` 二进制发现官方 harness 有专门的 `EmptyOutputContinuationCheckHook`，配 `disable_empty_output_continuation`、`continuation_prompt_override`、`max_nominal_continuations`；官方 changelog 还记录修过 "forced-continuation deadlock"——**此类 hook 必须配次数上限**[^cpa-5227]。
- **CLIProxyAPI #5439**（closed）：Claude→Responses 把 `max_tokens` 截断错报成 `response.completed`；修法是映射为 `response.incomplete`[^cpa-5439]。
- **CLIProxyAPI #4881**（open PR）：把"真空完成"（0 内容 0 调用 0 token）当可重试故障并轮换；#4886 指出对 Anthropic 系应续跑而非轮换[^cpa-4881]。
- **CLIProxyAPI #4958**（closed）：上游 `refusal` 未翻译变成 `stop`+空内容，客户端无法区分拒答与正常完成[^cpa-4958]。

## 七、已有的防线（对照）

`internal/adapter/devin/response_decoder.go` + `internal/api/openai/responses/response.go`：

- 干净 EOF 但无 stopReason → `fail`（"Devin stream ended without stop reason"）——「中途断流伪装成完成」已防住；本次事件不同：上游显式声明了 STOP_PATTERN；
- 无 stopReason 且无任何内容 → fail；`provider_refusal` → fail；
- `length`/`contentFilter` → Responses `response.incomplete`（对应 CPA #5439 已修的问题，我们映射正确）；
- `stopPatterns` 上游不生效 → 本地截断（见 upstream-gaps 6.5）；
- 孤儿 tool_result → `demoteOrphanToolResults` 降级为 user 文本（不伪造 tool_use_id，对比 CPA #5375 的 fabricate 方案）；
- 下游 SSE 10s keepalive 注释帧；上游 `http.Client.Timeout=610s`、`ResponseHeaderTimeout` 限首包。

## 八、残余缺口与可选对策

合并修复把宣告式停止从 ~3% 压到观测为 0，但 `STOP_PATTERN` + 纯文本（无 toolCall）目前仍直接作为 end_turn 透传——合法终答与残余中途停止在结构上仍不可区分，只能靠内容猜。

可选做法，按侵入度排序：

1. **什么都不做**，用户侧打「继续」即可（上下文完整）；当前频率下这是合理缺省；
2. **代理层 continuation hook**（agy 先例）：条件 = `end_turn` 且无 toolCall 且最后一条输入为 `tool_result`；追加一条合成 user 消息（"Please continue"）重发一次，把两次上游响应聚合成一个下游回合；**必须 `max_continuations ≤ 2`**（agy 的 deadlock 教训）。真·终答的代价是多跑一句废话；
3. 更保守的中间态：在 `index.jsonl` 对「STOP_PATTERN + 尾部像宣告句」打 `premature_endturn` 标记先观测频率，不干预。

## 九、其他可能踩到的问题（CPA issue/PR 普查 → 本链路映射）

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
