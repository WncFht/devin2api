# 上游 content-policy 指纹逆向记录

> 范围：Devin 上游（`server.codeium.com`）对提示词文案的策略拦截。目标是搞清楚「什么样的句子会被 `permission_denied: blocked by our content policy` 拦下」，并把已实证的触发句沉淀为 `internal/adapter/devin/sanitize.go` 的改写规则。
>
> 关联文档：`upstream-debug-playbook.md`（分层排查流程）、`upstream-protocol.md`（wire 级协议结论）。本文只记**指纹层**的结论。

## 一、现象与误判链

Claude Code 派生子代理时整批失败，模型自己总结出「subagent 在此会话不可用（模型 swe-2-max 对子代理不可访问）」。这是**双重误诊**：

1. 上游把子代理请求以 content-policy 拒掉（`error.json` 里 `stage: provider_stream`）；
2. Claude Code 把 HTTP 400 映射成通用文案 "There's an issue with the selected model (swe-2-max)"；
3. 主 agent 读到 task-notification 失败 + Agent 工具描述里 "a `model` override is ignored" 一句，输出「模型对子代理不可访问」。

实际请求里 `requested_model` 一直是 `swe-2-max`，模型路由没有任何问题。教训：**客户端层的报错文案不可信，永远先看 `logs/<dir>/error.json` 的 stage 与 message**。

## 二、探测方法

复用链路 `probe → devin-2api :3003 → 上游`，最小负载：

```json
{
    "model": "swe-2-max",
    "max_tokens": 32,
    "stream": false,
    "system": "<被测文本>",
    "messages": [{ "role": "user", "content": "Reply with exactly: OK" }]
}
```

- 200 → PASS；400 + `content policy` 文案 → DENIED；其它 4xx/5xx → 记录原样。
- 判定 DENIED 前重试 2–3 次：上游策略有非确定性（同一 prompt 可能先封后放），单次结果不可信。
- bisect 顺序：整模板 → 段落（`\n\n` 切）→ 行 → 句 → 子句。每步都对「含该段」与「不含该段」两侧分别确认，避免把「组合触发」误判成「单句触发」。
- 模板来源：客户端二进制 `strings` 级提取（Claude Code 的 JS bundle、Codex 的 Rust 二进制），再回放到代理。二进制里相邻模板会被一次切出来，提取器对多字节 UTF-8（`'`、`—`）的破坏会制造「残缺但仍在指纹邻域」的文本——**残缺变体被拒不代表原文有问题**，bisect 到底层时要恢复原始字节再判。

## 三、匹配器行为（已实证）

上游不是简单关键词匹配，是对**句子级特征**做判定：

| 形态                   | 实证例子                                                                                                                       |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------ |
| 完整句触发             | CC subagent 的 emoji 禁令：前半句、后半句单独都放行，拼成整句才拦                                                              |
| 有序相邻句对触发       | Codex 的 plan 状态句对：两句各自放行，**按原顺序紧邻**才拦；倒序或中间插一句即放行                                             |
| 同句共现触发           | Codex 的 ANSI 句：「Don't output ANSI escape codes directly」与「the CLI renderer applies them」须在同一句，缺一或换主语即放行 |
| 锚定无关               | CC 的 `/help:` 行与 feedback 句：行首、列表项、行内形态都拦——规则写行首锚定是漏洞                                              |
| 对残缺近似文本也可能拦 | `Don\nt output ANSI escape codes directly\nthe CLI renderer…`（撇号/破折号被换行替代）仍拦——匹配对轻微扰动鲁棒                 |
| 非确定性               | 同一 payload 偶发先封后放；本文所有 DENIED 判定均为 2–3 连拒                                                                   |

推论：改写只需破坏**特征句本身**（换主语、换语序、截断共现），无需回避主题词。「Codex」「ANSI escape codes」「OpenAI」等裸词单独出现均实测放行。

## 四、已实证指纹清单

### Claude Code 2.1.x（`cc-*` / `a*` 规则）

存量规则见 `sanitize.go` 注释；本次新增/修正：

| 触发句                                                                                                | 规则                 | 触发条件实证                                                                 |
| ----------------------------------------------------------------------------------------------------- | -------------------- | ---------------------------------------------------------------------------- |
| `For clear communication with the user the assistant MUST avoid using emojis.`                        | `cc-subagent-emojis` | 整句触发；「MUST avoid using emojis.」与「For clear communication…」单独放行 |
| `/help: Get help with using Claude Code` 行                                                           | `cc-help-line`       | 裸句/行内/列表三形态全拦 → 去掉了原规则的行首锚定                            |
| `To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues` | `cc-feedback`        | 同上                                                                         |

覆盖验证：CC 2.1.236 二进制提取的 **65 个提示词模板全部探测通过**——含全部 7 种内置 agent（general-purpose、Explore、Plan、statusline-setup、web-reader、worker、comment-analyst）、协作类（coordinator、worker fork、teammate、background observer）、内部辅助（compact、session 命名、memory 抽取/选择、hook 评估、auto-mode 审查、security monitor）、斜杠命令与注入块（/init、/ultrareview、/security-review、plan mode、output style、websearch 辅助、side-question）。

### Codex 0.153.3（`codex-*` 规则，本次新增）

从 codex 二进制提取约 37 个 `You are …` 模板逐一探测，4 个模板被拒，归因 3 条指纹：

| 模板（首句）                                                                                                               | 结果   | 归因指纹                |
| -------------------------------------------------------------------------------------------------------------------------- | ------ | ----------------------- |
| `You are GPT-5.2 running in the Codex CLI…`（cli-52）                                                                      | DENIED | fp-1 + fp-2（两条都含） |
| `You are a coding agent running in the Codex CLI…`（cli-agent）                                                            | DENIED | fp-1                    |
| `You are a coding agent. You must keep going until…`（keepgoing1）                                                         | DENIED | fp-3                    |
| `You are a coding agent. Please keep going until…`（keepgoing2）                                                           | DENIED | fp-3                    |
| 其余 33 个（gpt5/gpt6 系、root/team/reviewer/awaiter/compact/secreview/memwrite/sideconv/judge/planmode/personalities 等） | PASS   | —                       |

三条指纹的实证细节：

- **fp-1 `codex-opensource-def`**：`Within this context, Codex refers to the open-source agentic coding interface (not the old Codex language model built by OpenAI).` 整句拦。剥离子句：`Codex refers to the open-source` 放行、`refers to the open-source agentic coding interface`（无 Codex）放行、`open-source agentic coding interface` 放行——触发点是 `Codex refers to … interface` 这个完整跨度的搭配。改写为 `Codex is the open-source coding interface` 后整句通过。
- **fp-2 `codex-plan-statuses`**：`Do not batch-complete multiple items after the fact.` + `Finish with all items completed or explicitly canceled/deferred before ending the turn.` **按此顺序相邻**才拦。六句逐一放行、倒序放行、中间插一句放行、0+1+2 三句放行但 0+1+2+3 拦——定位到句对后改写第二句为 `Before ending the turn, leave all items completed or explicitly canceled/deferred.`。
- **fp-3 `codex-ansi-escapes`**：`Don't output ANSI escape codes directly — the CLI renderer applies them.` 拦（撇号为 `'`、破折号为 `—`）。`Don't output ANSI escape codes directly.`（去尾）放行、`Never output …`（换主语）放行、`— the CLI applies them`（去 renderer）放行——两个子句须同句共现。改写主语为 `Never output` 后整句通过。

四套模板清洗后整体回放全部 200。

## 五、请求路径差异

- 指纹拦截发生在**上游**，对 devin-2api 的三条入口（`/v1/messages`、`/v1/responses`、`/v1/chat/completions`）无差别——sanitize 在 adapter 层、所有入口共用，规则一处生效。
- 日志里 34 条 `codex-tui` UA 的 `/v1/responses` 失败请求，`instructions` 实为 Claude Code subagent 提示词（ccload 把 CC 流量转换成了 responses 形态），归因是已修的 emoji 指纹——**不是 Codex 原生提示词**。
- 真实 Codex 原生流量在当前日志窗口内未出现；`swe-2-max` 的模型目录项走 `You are Codex, a coding agent based on GPT-5` 系模板（实测 PASS）。cli-52/cli-agent/keepgoing 三套是 Codex 对其它模型家族的备选模板——用户换模型条目或 Codex 改家族映射时会踩到，所以仍补了规则。

## 六、Feature 级限制实测（skill / subagent / MCP / 杂项）

提示词指纹之外，对 feature 面的逐项实验结论（全部走真实链路，swe-2-max）：

### 实测通过

| 面           | 实验                                                                                                               | 结果                                                                                                  |
| ------------ | ------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------- |
| MCP 工具名   | 声明 `mcp__ide__getDiagnostics` 并强制调用                                                                         | 模型正确发 `tool_use`，id 形如 `mcp__ide__getDiagnostics_0`，名与参数（`{"file":"main.go"}`）完整回传 |
| MCP 工具数量 | 声明 50 个 `mcp__srvN__tool` / 150 个普通工具                                                                      | 200，无数量上限迹象                                                                                   |
| MCP 结果带图 | `tool_result` 内嵌 image 块（截图模式）                                                                            | 200，图片正常进 wire `images` 字段                                                                    |
| 图片输入     | 64×64 PNG 作为 user 内容                                                                                           | 200，模型可描述内容（`supports_images:true` 属实）                                                    |
| thinking     | `thinking:{type:enabled}` + 回放伪造 `sealed.v1.` 签名                                                             | 200，上游不校验历史签名的真实性                                                                       |
| max_tokens   | 200000                                                                                                             | 200（上限内静默接受）                                                                                 |
| 注入文本     | `<system-reminder>` skill 列表、agent 类型列表、`# MCP Server Instructions` 块、SKILL.md/agent.md frontmatter 正文 | 全部 200                                                                                              |
| CC 工具描述  | 真实请求的 29 个工具描述（Agent/Skill/Workflow/TaskCreate/CronCreate/SendMessage/ReportFindings/mcp__ide__\* 等）  | 全部 200（描述经 `# tools descriptions` 段并入 system prompt）                                        |
| Codex 模板   | skills-usage 段（`## Skills` / `### Available skills`）、autonomous loop ×2                                        | 200                                                                                                   |

### 实测发现的限制

| 限制                                          | 层           | 表现                                                                                                                  | 处理                                                                                                                                                                            |
| --------------------------------------------- | ------------ | --------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `/v1/responses` 只认 `type:"function"` 工具   | 本地 adapter | `custom`（Codex apply_patch freeform）、`local_shell`、`web_search`、`mcp` 类型**静默丢弃**，wire 上 `tools:[]`       | **已修（custom）**：`type:"custom"` 声明包装成单 `input` 参数 function 上行、响应解包回原文（见 `upstream-protocol.md`）；`local_shell`/`web_search`/`mcp` 仍丢弃并记 `Dropped` |
| `tool_result` 的 `resource`/`document` 内容块 | 本地 adapter | 原本整块丢弃 → 上游只见 `[tool result]` 占位，MCP 服务器返回的 resource 文本全丢                                      | **已修**：`resource.text` 展开为文本、`resource.blob`+image mime 降级为 ImageContent、其余降级为 `[resource: <uri>]`（见下方提交）；`document` 块仍丢弃（无文本可提取）         |
| assistant/user 消息中的未知内容块             | 本地 adapter | `server_tool_use`、`web_search_tool_result`、`code_execution_tool_result`、`document` 等 `default: continue` 静默跳过 | 已知取舍：上游无对应概念；若 MCP/服务端工具结果对客户端重要需在 adapter 层物化成文本                                                                                            |
| 退化图片（1×1 PNG，73B）                      | 上游         | `invalid_argument`（stage=response_event）                                                                            | 上游对图片有最小有效性校验；正常截图不受影响                                                                                                                                    |
| 速率                                          | 上游         | `resource_exhausted: overall message rate limit … reset in 10 seconds`                                                | 短时窗口限速，探测密集时会撞上，按 `reset in N seconds` 退避即可                                                                                                                |

### 结论

- **skill/subagent/MCP 三个 feature 面在上游没有独立限制**——子代理请求与主会话同构（system + messages + tools），MCP 工具名是普通函数名，skill 注入是普通文本。唯一的 feature 级拦截面仍是提示词指纹（第三、四节）。
- 限制集中在**本地 adapter 的协议覆盖度**：非 function 工具类型、非 text/image 内容块被静默丢弃，用户无感知。排查「某 feature 没生效」时先查 `02-request-messages.json` 与 `03-devin-request.json` 对比输入是否完整到达 wire。

## 七、维护流程（新症状 → 新规则）

1. 客户端报「模型不可用/无权限」类文案时，先查 `logs/<dir>/error.json`：`provider_stream` + `content policy` 即指纹问题。
2. 从 `01-http-request.json` 取 `system`/`instructions` 原文，按第二节方法 bisect 到句级；注意先区分「单句触发」「句对触发」「同句共现」三种形态（两两组合测试不可省）。
3. 写规则时给 `trigger` 填匹配必然包含的小写子串（写错会让规则静默失效），改写文案必须是实测通过的等义句。
4. 验证 = 原模板整体回放 200 + 改写句单独回放 200。

## 八、已知局限

- 策略非确定且**在漂移**：本文全部结论基于 2026-09-12 当天探测，重试后仍可能漏掉低频拦截。当日复测时 4 条已实证指纹中 3 条原文已放行（`cc-subagent-emojis`、`codex-opensource-def`、`codex-ansi-escapes`），仅 `codex-plan-statuses` 句对仍拦——指纹库可能按灰度/时效调整，已写入的 sanitize 规则继续保留（等义改写无害），但 DENIED 清单应视为时效性证据而非永久事实。
- 覆盖有限：CC 侧只覆盖 2.1.236 的 65 个模板，Codex 侧只覆盖 0.153.3 提取到的 ~37 个模板；**工具描述、用户正文、memory 注入内容**未系统扫——用户自定义内容里若巧合命中同类句式，同样会被拦（这正是「不做猜测性改写」原则的代价）。
- 版本漂移：客户端升级改文案即可能出现新指纹；旧指纹若上游放宽也可能变成多余改写（无害）。
