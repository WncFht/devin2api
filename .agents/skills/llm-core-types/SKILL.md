---
name: llm-core-types
description: 维护并演进本项目的供应商中立 LLM 中间表示（IR）。改动 internal/llm 的请求消息、内容块、工具定义、assistant 响应、failure 分类或流式响应事件，新增下游协议前端（internal/api/*）或上游适配器（internal/adapter/*），或判断某个字段该进核心模型还是留在某一侧时使用。
---

# LLM 核心类型（内部 IR）

`internal/llm` 建模的是 **agent 循环的本体**——一轮对话里有哪些角色、模型产出哪些内容块、工具如何调用与回填、一次生成如何增量展开、失败如何归因。它不是任何一家 wire 协议的 DTO 并集，也不是「OpenAI 消息格式换个名字」。两侧都是投影：下游协议前端（`internal/api/anthropic/messages`、`internal/api/openai/{chat,responses}`）把客户端 wire 投影进 IR、把 IR 投影回客户端 SSE；上游适配器（`internal/adapter/devin`）把 IR 投影成上游 wire、把上游帧投影回 IR 事件。N 个前端对 M 个适配器共享同一个 IR，翻译成本才是 N+M 而不是 N×M。

改这个包之前先回答一个问题：**我要建模的东西是不是 agent 循环里真实存在的一环？** 是，才进 IR；只是某家协议的字段形态，留在对应前端或适配器里。

## 事实源与验证

- `internal/llm/request.go`：请求上下文（`RequestMessages`）、消息联合（`UserMessage`/`AssistantMessage`/`ToolResultMessage`）、内容块联合、工具定义、请求级正规化 pass（`DemoteOrphanToolResults`、`MergeAdjacentAssistantTurns`）。
- `internal/llm/response.go`：聚合响应（`AssistantMessage`）、`Usage`/`UpstreamCosts`、诊断、`ResponseEvent` 增量协议与校验。
- `internal/llm/failure.go`：`Failure` 分类记录、`Classify`/`FailureOf` 生产与派生。
- `internal/llm/stream.go`：`ResponseStream` 接口。

文件里的中文字段注释是规范的一部分，改动时同步更新。验证跑 `go test ./internal/llm ./internal/adapter/devin ./internal/api/...`，再 `go test ./...` 与 `golangci-lint run`。wire 金帧测试在 `internal/adapter/devin/testdata/*.jsonl`，回放的是上游原始帧——Go 字段改名不影响它们，IR 语义变更才需要补帧。

## 本体（现状快照，以代码为准）

消息三角色：`user` / `assistant` / `toolResult`。内容块五种：`TextContent`、`ThinkingContent`、`ImageContent`、`ToolCall`、`ServerToolResult`。流事件是块生命周期协议：`start` → 每块 `*_start`/`*_delta`/`*_end` → `done`/`error`，外加两个迟到注解：`signature`（块关闭后仍可到达的签名增量）与 `server_tool_result`（托管结果注入）。`AssistantMessage` 同时是流内累计快照（`Partial`）与终态（`Message`/`Error`）。

## 设计规则

这些规则回答「X 该不该进 IR、以什么形态进」。每条都对应一次真实踩坑或一次刻意取舍。

### 建模循环本体，不建 wire 字段并集

判定标准：一个概念如果在「换一家供应商还存不存在」的答案里活下来，它属于 IR；只存在于某家协议的编码层，属于那一侧的前端/适配器。`ToolCall.Arguments` 是 JSON 对象而不是某家的 `function.arguments` 字符串、`ToolResultMessage` 是独立消息而不是 tool role 的 content 包装——都是这条规则的产物。反例是 `UpstreamCosts`：上游计费读数本属供应商细节，但「这单花了多少」是跨供应商可比的账单事实，故以原值透传形态进 `Usage.Costs`，不参与 IR 语义推导。

### 块有序、原子、可签名

`Content` 是有序 slice，顺序即模型产出序，禁止跨层重排。块一旦成形不再合并（`MergeAdjacentAssistantTurns` 合并的是消息级铺平，不是块）。四种模型产出块（text/thinking/image/toolCall）统一携带 `Signature`/`SignatureType` 不透明回传位——Gemini 对 text/inlineData/functionCall part 都发 `thought_signature` 且缺省即 400，OpenAI 的 reasoning item 整体密封进 `encrypted_content`。「只有 thinking 有签名」是 Anthropic 时代的局部观测，不是本体事实；签名是**逐块**的，不是逐消息、也不是 thinking 专属。`SignatureType` 必须随签名原样回传（错配实测触发上游 invalid_argument），它决定签名的解析规则——`openai` 型是可解的 reasoning item 序列化，其余是不透明 blob。`ServerToolResult` 不签名：它是代理侧合成块，不承载上游回放义务。

### 流事件覆盖块的完整生命周期，含迟到注解

事件协议的最小完备集是「块什么时候开始、增量怎么来、什么时候算完、完事后还能被补注什么」。`signature` 事件是迟到注解的形态：`ContentIndex` 指向目标块，`Delta` 携带签名片段，签名状态不写进事件字段——`Partial` 里目标块的 `Signature`/`SignatureType` 随事件同步更新，事件只是「它变了」的通知。新一类「块关闭后仍可到达」的信息（citation、安全标注、后续修订）按同一形态进：复用 `Delta` 做载荷、`Partial` 做状态载体的注解事件，不要为每种注解开平行字段族。编码器按协议能力投影——Anthropic 把思考签名渲成 `signature_delta`，Responses 攒进 `encrypted_content`，Chat 直接丢弃；IR 内不丢。

### 服务端托管工具是标记，不是类型家族

`ToolCall.Server` 一个 bool 表达「这个调用由代理/上游执行而非客户端」，`ServerToolResult` 一个块类型承载结果并与调用按 `ToolCallID` 配对。不要为每种托管工具开平行块类型——`SearchResults` 承载搜索族的结构化明细，下一个托管族（code_interpreter 之类）另设自己的明细字段，正文统一走 `Content []Content`（回放成 `ToolResultMessage` 与编码器无结构化条目时的兜底渲染共用，`TextBody()` 取纯文本）。请求的托管意图是 `ServerSearch`/`ToolDefinition.Server`，执行是适配器内的续轮，结果是配对的块——三个阶段三个位置，不要揉成一个字段。

### 失败 = 生产侧事实 + 消费侧派生

`Failure` 是生产侧写进消息的分类记录：`Code`（错误码词表）、`Message`、责任位（`UpstreamFault`/`LocalGate`/`ClientFixable`/`Canceled`/`Timeout`）、重试提示（`RetryAfter*`/`RateLimited`/`ContextLength`/`ResetHint`）、闸门簿记（`Gate*`）。生产侧 `Classify` 把具体错误落成事实；消费侧 `FailureOf`/`derive` 在记录缺席时按 `ErrorMessage` 文本兜底分类——事实在产生点写死，派生永不回流改记录。`Cause` 保留原始 error 供进程内排障，不进序列化（blob 影子按字段落盘）。注意 `Code` 词表当前就是 Connect code 词表——这不是泄漏是记账：唯一上游 Devin 走 Connect，词表即契约。**第二个上游落地是重新命名空间的触发点**（把词表抽成 IR 自有枚举、Connect 码由适配器映射），在那之前改动是负资产。

### 每个有损边都要留收据

协议翻译必然有损，损失必须可对账：请求解码丢弃/降级的字段进 `RequestMessages.Dropped`（`"kind:detail"` 词表，其中 `cache_control:`/`anthropic_beta:` 前缀的 marker 参与会话亲和种子）；wire 投影的静默修复进 `RequestRepairs` 计数（重排/丢空消息/剥历史图/$ref 截断/sanitize 命中）；响应转换的旁路信息进 `AssistantMessage.Diagnostics`。新加任何「吞掉/改写客户端或上游语义」的代码路径，先问收据开在哪——`Dropped` 是逐条审计、`Repairs` 是计数、`Diagnostics` 是带详情的记录，按需要的粒度选，不许静默。

### 不变量挂在类型上，正规化是具名 pass

`Validate()` 表达「这个值单独看是否成立」（必填字段、枚举域、块类型白名单、签名与 redacted 的耦合约束），由事件编码入口与请求入口统一调用——校验不在各消费方重复发明。跨消息的不变量（孤儿 tool result 降级、相邻 assistant 铺平合并）是 `RequestMessages` 上的具名方法，在 Validate 之前由适配器显式调用；不写成隐式 hook，让「请求被正规化过」这件事在代码路径上可见。

### 前端是纯投影，适配器是唯一的语义泄漏点

`internal/api/*` 不持有状态、不做执行决策：事件进、SSE 出；协议表达不了的概念（Chat 的签名、托管结果）按「留在 IR 内不下发」处理，不反向改 IR 迁就协议。`internal/adapter/*` 是唯一允许知道「上游到底要什么」的地方——会话亲和、保温、续轮、托管执行、速率闸门全在适配器内部消化，其复杂性不外溢到 IR 的字段形状里。反过来也成立：IR 不得为某适配器的实现细节开字段（`CallerKeyHash`、`SessionKey` 这种是客户端提供的语义，不是适配器私事）。

### 状态引用永不建模成状态

`SessionKey`、`CallerKeyHash`、`ResponseID`、`OutputID`、`UpstreamRequestID` 都是**引用**——它们回答「这次调用与哪次相关、上游怎么追踪」，不断言上游是否有服务端会话状态。`Messages` 永远携带完整历史；上游是否有状态由观测回答（请求体比对、session id 行为），不写成类型假设。新增 id 类字段时想清楚它标识的是「本条消息」「本次响应」还是「跨请求会话」，别把传输层的相关性编码成持久化承诺。

## 演进决策

新需求进来时按这个顺序判断形态：

1. **循环本体的新成员**（新的内容块种类、新的消息角色、新的事件种类）——先例：`ServerToolResult`（托管执行结果是有实体的一环）。门槛最高，要求概念在三端都有对应物或可预见对应物。
2. **现有块的新字段**——要求该字段是「回放义务」或「跨供应商可比事实」：`Signature`/`SignatureType`、`OutputID`、`UpstreamRequestID` 属此类。
3. **逃逸舱/透传**——供应商私有、回放必须原样往返、IR 不需要理解：不透明 string 字段或 `json.RawMessage`，命名直白，注释写明来源与回放约束。
4. **留在某一侧**——只有一家协议有、且无回放义务的：前端内部结构（`pendingReasoning`）或适配器私有（`webSearchOutcome`）。这是默认归宿，前面三档都要论证过才升级。

### 多上游落地检查单

第二个上游适配器进来时，按序处理这些已知挂账点：

- `Failure.Code` 词表去 Connect 化：`Classify` 的入参是 `error` 但分类输出绑定 Connect code 词表，新上游的错误面要先抽 IR 自有词表再建映射。
- `SignatureType` 词表扩充：`sealed`/`anthropic`/`openai` 是实测值，新体制的签名格式标识进同一字段，编码器按值分派解析。
- `Server` 工具族明细：`SearchResults` 之外新托管族按「另设明细字段」模式加，不复用。
- 事件注解面：新上游若有块级迟到信息，走 `signature` 同款注解事件，不开新事件族之外的平行机制。
- 每侧对照检查「留在某一侧」的字段有没有被第一家协议的形态污染——`ServerSearch` 的 domain 语义按上游能力裁剪过，新上游能力面不同就要重审。
