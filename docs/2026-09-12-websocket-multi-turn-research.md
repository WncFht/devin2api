# WebSocket 多轮支持调研：ccLoad / CLIProxyAPI / sub2api

> 调研目的：为 `internal/app/websocket.go` 的单轮实现升级为「持久连接多轮」找参照系。
> 结论：**ccLoad 是最适合我们的蓝本**(session 状态机 442 行自包含、错误分类最完整）;CPA 更复杂（多凭据/上游 WS 双模），可抄的点在边角；sub2api 的 WS 复杂度全在上游客户端侧，不适用。

## 现状缺口

我们的 `responsesWebSocket`(websocket.go:149）只处理第一条 `response.create`，后台读协程把后续帧 log 掉。而 Codex WS v2 的真实用法是**一条连接上反复 `response.create`/`response.append`,`previous_response_id` + 增量 input**——第二轮起我们必挂。

## 三家实现对比

| 维度                     | devin-2api（现状）      | ccLoad                                                                       | CLIProxyAPI                                  | sub2api                 |
| ------------------------ | ----------------------- | ---------------------------------------------------------------------------- | -------------------------------------------- | ----------------------- |
| 服务端 WS                | 单轮，单向映射 SSE→WS   | 连接级会话 + **进程级 session store**                                        | 连接级会话 + 双模上游                        | 仅管理面板 WS           |
| 增量合并                 | 无                      | `lastRequest.input + lastResponseOutput + 新input`,dedupe by item.id/call_id | 同左，更多边角（compaction/prewarm followup) | n/a                     |
| prewarm `generate:false` | 无                      | 本地合成 created+completed,**计入 lastRequest/lastResponseID**               | 同左（独立 prewarm.go)                       | n/a                     |
| 错误分类                 | error 事件 + close 1000 | **五类**（见下）                                                             | 类似 + close code 镜像上游                   | n/a                     |
| 并发槽                   | 连接级持槽（middleware) | **按轮次获取/释放** + 连接数限流（全局 + 按 token)                           | 连接级                                       | n/a                     |
| 断线恢复                 | 无（close)              | `interrupted` 时提交部分 output + pendingToolCallIDs；重连同 Session-Id 可续 | pinned auth 重放                             | n/a                     |
| 上游                     | Connect-RPC 流          | HTTP 回放 / native WS 双模                                                   | 同左 + auth pinning                          | WS 客户端池化（不适用） |

## ccLoad 关键机制（蓝本）

### 会话状态机 `responses_websocket_session.go`

`responsesWebsocketSession{lastRequest, lastResponseOutput, lastResponseID, pendingToolCallIDs, replacementReplayRequired}`。

`normalizeRequest(payload)` 守卫顺序（很重要）:

1. 非法 JSON / 非 create|append → `invalid_request`
2. `lastRequest` 为空：append 或带 previous_response_id → `previous_response_not_found`
3. `previous_response_id` 非空且 ≠ lastResponseID → `previous_response_not_found`(**带 param 字段**)
4. `replacementReplayRequired` 且 create 无 previousID → **全量替换**(normalizeFullReplacement)
5. `pendingToolCallIDs` 非空且新 input 不含对应 output:incremental → 报错；create 无 previousID → 全量替换
6. 无 previousID 且 `inputContainsCompletedTranscript(input)`(compaction/compaction_summary/function_call/assistant message/压缩摘要前缀）→ 全量替换
7. 否则增量合并：`mergeResponsesWebsocketInput(lastRequest.input, lastResponseOutput, newInput)`，按 item `id` 与 tool call `call_id` dedupe

`finalize` 统一过两道：`validateResponsesWebsocketToolCallPairing`(output 必须有对应 call，防上游 400 被当渠道错误烧候选）+ transcript 字节上限。

### 输出捕获 `responsesWebsocketBridgeWriter`

- `response.output_item.done` 按 `output_index` 累积 item 快照（带累计字节上限）
- `response.completed`/`done`/`incomplete`:`completed=true`，用 collected item **reconcile** completed.output（完整 tool call 以 collected 为准——应对上游 completed 里截断/变体的 arguments);completed.output 为空则用 collected 兜底
- `collectedOutput()` 过滤不完整 tool call（缺 call_id/name/arguments)
- **裸 error 事件（无 HTTP status 的，如 server_is_overloaded）不转发给下游**——客户端收到也无法终结回合，继续等终结事件；回合层翻译成 `upstream_stream_interrupted` + close 1011
- `message_too_big` 错误 → 回 close 1009（客户端据 close code 决策，如降级 SSE)

### 错误分类（下游可见）

| 场景                                                        | 下游见到                                                                                                                              |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| 请求非法（JSON/type/input 缺失）                            | `{"type":"error","status":400,"error":{"type":"invalid_request_error","code":"invalid_request"}}`，连接不断                           |
| previous_response_id 不匹配/空会话                          | error 400 `previous_response_not_found`,`param:previous_response_id`，消息里带 session TTL 提示，连接不断                             |
| 会话/并发/预算上限                                          | error 429 `rate_limit`，带 `setting`/`setting_value`/`current`/`unit` 结构化字段                                                      |
| 上游产出任何 output 前失败                                  | error 502 `upstream_unavailable` + close 1011（重连重试本轮）                                                                         |
| 流中途断（无终结事件/裸 error)                              | error `upstream_stream_interrupted`（含上游原始 message) + close 1011;**已收到的 output item + pendingToolCallIDs 提交进 transcript** |
| message_too_big                                             | close 1009                                                                                                                            |
| 上游 terminal payload(response.failed / 带 status 的 error) | 原样转发，连接继续                                                                                                                    |
| 鉴权中途失效                                                | close 1008 policy_violation                                                                                                           |
| 服务关闭                                                    | close 1001 going_away "server shutting down"                                                                                          |

### 连接管理

- 独立 reader goroutine → channel，主循环 select 消息；每条消息续约 idle read deadline(5min),PongHandler 也续约；ping 循环 2min(`WriteControl`，不经写锁）,ping 失败 → 取消连接
- **连接数限流与请求并发分开**：连接限流（全局 512?+ 按 token）在 upgrade 时；上游并发槽按轮次 acquire/release
- session store:`Session-Id`(+可选 `Thread-Id`)→ sha256 key 的进程内 map,stable session 有 TTL（默认几十分钟）,transient session（无 Session-Id）连接断开即回收；transcript 全局字节预算（LRU 逐出 idle transient);**下游重连同 Session-Id 可恢复 transcript**;HTTP POST 带 Session-Id 时同样走 store(`normalizeHTTPRequests`)，但「无 Session-Id 的 HTTP 请求不进 store」
- `x-codex-turn-state`:CPA 在 upgrade 响应头回带；ccLoad 未见对应处理（CPA 独有）

### CPA 独有（对我们不适用/可选）

- native upstream WS 直透 + pinned auth per provider + `upstreamMode` 状态机 + `WithRequiredUpstreamWebsocket`（续轮必须留在同一上游 WS)→ 我们上游是 Connect-RPC,**不需要**
- `UpstreamDisconnectChan`：上游 WS 主动断开时把 close code 镜像给下游 → 不需要
- `response.append` 与 prewarm-followup 的细分分支 → ccLoad 的简化版已覆盖
- wsrelay(浏览器当上游）→ 不适用
- 上游 WS 客户端池化（sub2api 的 RequiresReaderLoop 等）→ 不适用

## CPA 深挖补充（精确到行为级）

**最重要的发现：CPA 生产路径硬编码 `allowIncrementalInputWithPreviousResponseID=false`**——即对 HTTP 回放型上游（我们的形态），每个后续请求**总是展开成完整 transcript** 再发，从不把 `previous_response_id` 透给上游。这正是我们要的：我们的上游没有 previous_response_id 语义。

**合并算法精确版**(req.go:342-528):

1. 拼接顺序固定：`lastRequest.input`（大小写不敏感找 input 键，重名取最后一个，模拟 encoding/json)→ `lastResponseOutput`（必须是合法 JSON 数组才拼；若其中含 compaction 标记，先删掉 prev-input 里的 `compaction_trigger` 项）→ 新 input
2. 两轮 dedupe（顺序敏感）:
    - 第一轮：tool-call 类型（function_call/custom_tool_call）按 `call_id` 去重，**保留首个**
    - 第二轮：所有项按 `id` 去重，默认**保留最后**；但 `call_id` 被某个 output 项引用的 call 项不会被未引用项顶掉（保配对相邻性）
3. 手工序列化 `[` + raw 拼接 + `]`(item.Raw 原样保留）

**transcript 替换检测两级**:

- `shouldReplaceWebsocketTranscript`：无 previous_response_id 且 input 里有 function_call/custom_tool_call、assistant message、或「全部 user/developer + 含压缩摘要前缀」→ 整体替换 lastRequest（合并跳过）
- `inputContainsFullTranscript`(compaction/compaction_summary)→ input 原样用，连合并都跳过

**orphan 修复**(toolcall_repair.go，可选二期）：孤儿 function_call_output → 从**会话级调用缓存**找回 call 项插在前面；孤儿 call → 插缓存 output 或丢弃；空 call_id 的 output 仅当带 name(Codex heartbeat 委托结果）才保留。缓存按 `X-Client-Request-Id`/`X-Codex-Turn-Metadata.session_id`/`Session-Id` 跨连接共享，refcount,256 项 FIFO,30min TTL。我们 v1 用 ccLoad 的「校验 + 报错」更简单。

**错误协议分歧**:

- previous_response_not_found:CPA 用 **409**(error 体完整 JSON 直接嫁接进 error 节点）,ccLoad 用 **400 + param 字段 + TTL 提示**。两家连接都不断。
- close code:CPA 只用 **1009**(message_too_big）和 **1012**(service restart = 需 HTTP 重放）;ccLoad 用 **1011**(internal error = 重连重放）。非终结错误两家都不关连接。
- CPA 对「流结束但无 completed」回内部 408 后裸关闭；ccLoad 翻译成 interrupted+1011——后者对客户端更友好。

**保活**:CPA 只在流空闲时 ping(ticker 每收一帧 reset，默认关闭！)，全程无 read deadline/pong handler；客户端断连靠写失败/ctx 发现。**ccLoad 的保活明显更强**（常驻 2min ping + PongHandler 续约 5min idle deadline)，选 ccLoad 的。

**并发**：单连接内严格串行——一个 goroutine 跑 read→normalize→execute→forward，次轮在 TCP 缓冲区排队，无显式 busy 错误。我们也照此：读循环本身就是串行的。

**x-codex-turn-state**:CPA 在 upgrade 响应头原样回带（一行事，值得抄）。

## sub2api 深挖补充（有服务端多轮，非只客户端）

纠正初判：sub2api 的 `internal/handler/openai_gateway_handler.go` + `internal/service/openai_ws_forwarder_ingress.go` 是**完整的服务端多轮 WS**(coder/websocket，不是 gorilla)，三种上游模式：passthrough（每客户端连接独占一条上游 WS)、ctx_pool（池化上游 WS 复用）、http_bridge（上游 HTTP SSE——**与我们形态同构**)。它独有、值得吸收的点：

- **`wroteDownstream` 不变式**：「已向下游写过任何字节后，永不重试、永不合成 in-band 错误」——部分输出已送达后只能 close(1011/1013 按原因）。这是比 ccLoad 更硬的一条纪律，避免客户端看到半截流后又收到一个完整错误事件造成状态混乱。
- **close code 表更细**:1000（正常/turn 间空闲超时）、1001（请求取消/passthrough 超时）、1008(policy：非法首帧、`response.append` 被拒、`msg_*`/`item_` 型 previous_response_id、会话中切模型、重叠 create)、1011（通用上游失败）、1013(failover 耗尽/会话被抢占）。**它直接拒绝 `response.append`(1008)**，而 ccLoad 当增量处理——我们选 ccLoad 语义（更兼容）。
- **previous_response_id 分类校验**：只认 `resp_*`,`msg_*`/`item_`/`chatcmpl_` 前缀直接拒（便宜的形态校验，值得抄）。
- **http_bridge 模式**：上游是每轮 HTTP SSE 时，中断会**合成 error/response.failed 事件**下发（而非裸 close)——与我们「上游是 Connect-RPC 流」的形态一致，错误语义采用这个而非 ctx_pool 的裸 close。
- **会话跨连接恢复**:response_id→owner/conn + session→turnState 的 Redis+ 本地缓存 store,`Session-Id`/`Thread-Id` 作为 execution scope，新连接可续旧会话，旧连接被新连接抢占时收 1013。我们单进程无 Redis,v1 连接级即可。
- **ingress lease**：按 API key 的连接数上限（超限 429+Retry-After:5)——对应我们「连接级限流与并发槽分离」。
- **首帧超时 30s**、读上限 64MiB、turn 间空闲超时关 1000。
- 上游 WS 池化全部（reader-loop 应答 ping、握手兼容键、预热、90s 空闲回收）→ 我们上游非 WS，全部不适用。

## 我们的改进映射（融合三家后定稿）

1. **重构 `responsesWebSocket` 为读循环**:reader goroutine → channel（分离「读消息」与「跑 turn」，解决「turn 进行中无人读 → 控制帧不消费、断连靠写失败才发现」);for 循环串行处理 create/append（单连接天然串行，无 busy 错误——三家一致）
2. **新增 `wsSession`**（连接级状态机，以 ccLoad `responses_websocket_session.go` 为蓝本，dedupe 换成 CPA 两轮版）:
    - 字段：lastRequest/lastResponseOutput/lastResponseID/pendingToolCallIDs/replacementReplayRequired/pendingPrewarmID
    - `normalizeRequest` 守卫顺序沿用 ccLoad 七步；`previous_response_id` 非 `resp_*` 前缀直接拒（sub2api 的形态校验）;previous_response_not_found 用 **400 + param 字段**(ccLoad 式，CPA 的 409 无参数语义弱）
    - merge = `lastRequest.input + lastResponseOutput + 新input`；替换检测用 ccLoad 的宽判据（function_call/assistant/压缩前缀/compaction)；dedupe 用 CPA 两轮（call_id 保首个 + id 保最后但 referenced-call 不被顶）
    - `pendingPrewarmID` 短路（CPA 式）:`previous_response_id == pendingPrewarmID` → 把 prewarm input 物化进 transcript 后按替换处理
3. **`wsResponseWriter` 升级为 bridgeWriter**(ccLoad 版）:`output_item.done` 按 output_index 收集（带累计字节上限）+ `response.completed` 的 output 用 collected reconcile（完整 tool call 以 collected 为准）+ `message_too_big` → close 1009 + **非终结裸 error 事件不转发**
4. **prewarm**：首回合 `generate:false` → 本地合成 created+completed(`resp_prewarm_` id、空 output、零 usage)，计入 session，不打上游
5. **错误分类**（取三家交集 + sub2api 的 wroteDownstream 纪律）:
    - invalid_request / previous_response_not_found(400,param)→ error 事件，**连接不断**
    - 上游在产出任何下游字节前失败 → error 事件 `upstream_unavailable` + close **1011**
    - 流中途断（无终结事件/裸 error)→ error 事件 `upstream_stream_interrupted`（带上游原文）+ close 1011;**已收 output + pendingToolCallIDs 提交进 session**(ccLoad 的关键设计）
    - message_too_big → close 1009
    - 上游 terminal 错误（response.failed/带 status 的 error)→ 原样转发后继续
    - **wroteDownstream 纪律**：已写过下游字节后，失败只能 close，不合成 in-band 错误（sub2api)；未写过则可发 error 事件
6. **并发槽改按轮次**:WS upgrade 不再经 `concurrencyMiddleware` 整连接持槽（拆出 tryAcquire，每轮 acquire/release)；连接数另加简单上限（进程级即可，按 API key 分组可选）
7. **限制**：每帧 `SetReadLimit`(32MiB，对齐 HTTP body)+ 合并后 transcript 卡同上限（ccLoad `enforceResponsesWebsocketTranscriptLimit`)
8. **liveness**(ccLoad 式，CPA 太弱）:ping 循环 ~2min(`WriteControl` 不经写锁）+ PongHandler 续约 read deadline(5min)+ 每条客户端消息续约；ping 失败 → 取消连接；首帧 30s 超时（sub2api)
9. **其他一行事**:`x-codex-turn-state` upgrade 响应回带（CPA);shutdown close 1001
10. **错误体转换**:HTTP 路径的 `{"error":{...}}` 包一层 `{"type":"error","status":N,"error":{...}}`(ccLoad/CPA/sub2api 三家的客户端协议形状）

## 决策点（已定）

- **gjson/sjson**:merge/dedupe/检测全是 JSON 手术，三家全用 tidwall。引入 `tidwall/gjson`+`sjson` 两个纯函数库，直接移植 ccLoad 版，比 stdlib 重写稳妥。
- **session 范围**:v1 连接级。重连契约本来就是「客户端重放全量 transcript」;ccLoad/sub2api 的进程级 store(Session-Id 续连、interrupted 部分提交跨连接生效、HTTP 混入 normalizeHTTPRequests）是增强，二期再议——届时把 `wsSession` 挪进 store 即可，状态机本身不用改。
- **`response.append`**：按 ccLoad 语义当增量处理（sub2api 的 1008 拒绝更严但兼容性差）。
- **不改 HTTP POST 路径**:Codex HTTP 模式每轮重放全量 input,`previous_response_id` 继续被解析忽略（现状注释已写明）。

## 不做清单

上游 WS 池化/pinning(native passthrough)、跨进程 store(Redis)、`x-codex-turn-state` 上游透传、wsrelay 式浏览器上游、per-IP 限流（sub2api 只在管理面板有）、压缩协商（客户端 accept 默认无压缩即可；要加就 `EnableCompression`，注意 gorilla flate 尾部校验坑——CPA 注释提过）。

## 实现状态（2026-09-12 落地）

已按上表落地，文件：`internal/app/websocket.go`（读循环 + bridgeWriter + 错误分类 + 预热 + 保活）、`internal/app/websocket_session.go`（wsSession 状态机，stdlib JSON 手术而非 gjson——`wsJSONField/wsJSONString/wsJSONDelete/wsJSONSet*` 六个 helper 覆盖全部需求）、`internal/app/app.go`（WS 路由脱离 concurrencyMiddleware，per-turn 槽 + `wsConns` 256 连接上限）。测试 `internal/app/app_websocket_test.go` 十条：合并/错链/预热/pending tool call/中断替换/错误分类/failed 保连接/per-turn 槽/连接上限/405。

与调研表的偏差：

- **上游 5xx 不再断连**：决策表曾写「error 事件 + close 1011」，实现改为 `error` 事件 + 连接保持 + `requireReplacementReplay`。理由：session 是连接级的，断连必丢 transcript，客户端两种路径的恢复成本相同（重放），保持连接省一次握手；sub2api http_bridge 形态也是合成事件而非裸 close。
- **shutdown close 1001 未做**：hijacked 连接不受 `http.Server.Shutdown` 管理，做它要另起连接注册表 + 信号钩子；且对客户端行为无差异（异常断开与 1001 都走重连重放）。列入二期。
- **gjson/sjson 未引入**：六个 stdlib helper 已覆盖读取/改写，零新依赖。
- **嵌套字段注意点**：`wsJSONField` 只认顶层键，`response.id`/`error.code` 这类必须先取外层再取内层（`wsNestedJSONString`/`completedOutputFromEvent`）——测试曾因此抓到 prev_id 永远 404 的真 bug。
