# 上游兼容问题排查手册

本仓库把 Anthropic / OpenAI 协议请求转成 Devin Connect `GetChatMessage` 的 Connect-RPC 调用。上游是闭源黑盒，错误信息几乎全是 `permission_denied: an internal error occurred` 或 `invalid_argument: an internal error occurred` 这种无信息量包装。本文沉淀已验证的排查方法和上游契约，供后续接入新客户端时复用。

## 链路全景

```
client (cc / codex / kimi-cli / ...)
  → ccload :49173            (渠道管理、冷却、格式转换)
    → devin-2api :3003       (协议转换 + sanitize + wire 构造)
      → server.codeium.com   (Devin 上游, Connect-RPC)
```

> 链路为作者本机示例：ccload 是作者自用的前置网关（非必需——客户端可直连 devin-2api），`:49173`/`:3003` 端口与渠道 id 是本地部署取值，按自己的拓扑替换。ccload 相关小节只在走同款链路时适用。

任何一环出错都会以「重试/失败」的形式表现在客户端。定位的第一步永远是**确定错误在哪一层产生**。

## 错误速查表

| 现象                                                                                                                                            | 层               | 含义                                                                                                                                                                      | 处理                                                                                                                                                                         |
| ----------------------------------------------------------------------------------------------------------------------------------------------- | ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| HTTP 401                                                                                                                                        | devin-2api       | 凭据未命中令牌仓                                                                                                                                                          | 查令牌仓（面板 auth-tokens 页 / `auth_tokens` 表）：无凭据但仓非空且无匿名通道行也走这条；令牌只由面板管理，仓内只存哈希——旧版 `auth.api_key` 播种出的行仍是有效凭据         |
| HTTP 401 `authentication_error`（上游 `unauthenticated`）                                                                                       | 上游             | 该 lane 的生效凭据为空或失效；代理不本地拦截，`unauthenticated` 先触发 `reloadToken` 重解该号凭据（行覆盖→config 声明→credentials_file 重读）并透明重试一次，仍失败才下发 | 面板改号或改 `devin.accounts` 条目即自愈（自愈链重解生效值），免重启；credentials_file 型等 CLI 续期改写文件亦可                                                             |
| `error_stage=request_build`（如 `tool_choice` 指向 `tools` 里不存在的工具）                                                                     | devin-2api       | 本地请求投影失败：构造 wire 时的参数校验拒绝，从未触达上游                                                                                                                | 看 `error.json` 的 message 定位字段；属客户端请求缺陷，不是上游拒绝                                                                                                          |
| `permission_denied`（无 policy 文案）                                                                                                           | 上游             | 模型 UID 不存在/无权                                                                                                                                                      | **先查 `devin.aliases` 目标是否还活着**（stderr 有 `model absent from upstream catalog` Warn 即此情形），再查模型名拼写                                                      |
| 某模型突然 `not_found`/`permission_denied`                                                                                                      | 上游             | 上游可能给该模型加了版本门                                                                                                                                                | bump `devin.client_version` 到最新 CLI 版本再试                                                                                                                              |
| `permission_denied` + "blocked by our content policy"                                                                                           | 上游             | 命中特征句指纹库                                                                                                                                                          | bisect 请求体，把触发句加进 `sanitize.go`                                                                                                                                    |
| `invalid_argument`（无 `protocol error:` 前缀）                                                                                                 | 上游             | **wire 形状不符**（不是内容问题）                                                                                                                                         | 对照本文「已验证 wire 契约」逐条查                                                                                                                                           |
| `incomplete envelope` / `unexpected EOF` / connection reset（含 `unavailable:`、`invalid_argument: protocol error:` 等 connect.Error 包装形态） | 传输             | 上游连接被截断（TUN/代理换路、上游连接回收），connect-go 统一包成 connect.Error                                                                                           | 建立阶段最多尝试 3 次（`maxConnectAttempts`）；内容产出前断流整体重发一次（`meta.retry_attempts`/`logs.retries` 落库）；记 `devin_transport`。仍失败查连接路径               |
| Connect code 错误且 unwrap 链无 io/net 错误（`unavailable` 固定模板、`invalid_argument` 参数、`permission_denied`、`resource_exhausted`）       | 上游             | 确定性语义错误                                                                                                                                                            | **不重试**——"try later" 文案是固定模板；记 `devin_connect`                                                                                                                   |
| HTTP 200 + SSE `response.failed`/`error`                                                                                                        | 上游             | 流建立后上游才拒绝                                                                                                                                                        | 同上，看事件里的 code/status 分类                                                                                                                                            |
| 客户端报 503（`server_draining`）/ 429 `server is busy` / 401，但 `logs` 表与请求页全是 200                                                     | devin-2api       | 管线前拒绝：排空 / 并发溢出 / 鉴权失败，未进门即被拒，无调试目录但落 `log_source=rejected` 留存行（默认视图剔除）                                                         | 面板系统页「本地拒绝」表查 reason+ 时刻；留存行查 `/admin/logs?log_source=rejected` 或 `sqlite3 ... WHERE log_source='rejected'`；503 必是排空（部署窗口内），429 看并发上限 |
| 客户端报 401/403/429 且 `logs` 行 `error_stage=token_limit`                                                                                     | devin-2api       | 下游令牌准入拒绝：并发槽 / RPM / 5h·日·周·月费用窗口 / 模型白名单 / 处理中令牌失效                                                                                        | 有调试记录，看 `error.json` 的 message 区分哪条限额                                                                                                                          |
| 客户端报 429 且文案含 `rate limited by local gate`                                                                                              | devin-2api       | 本地速率闸门快败（睡到下一可发窗口/滴灌槽的预计等待超 `gate_max_hold_seconds`，或上游续试重打过闸时被闩拦）                                                               | 请求页按 `error_stage=rate_gate` 筛；系统页「速率闸门」看闩态；与上游真 429（`devin_connect`）分开归因                                                                       |
| 流恰好 ~300s 整被掐、`error_stage=client_disconnected`                                                                                          | 客户端           | 客户端侧总超时（CC / ccload 各自的 request timeout）——代理没有 300s 这个限                                                                                                | 调客户端超时配置；已产出内容的流断连后脱钩续命进完成缓存，同键重试秒回（见本节末段的脱钩缓存说明）                                                                           |
| 流在部署窗口 ~600s 处被掐、`error_stage=drain_timeout`、`result=aborted`                                                                        | devin-2api       | 排空上限强掐：SIGTERM 后 600s 仍未跑完的在途请求被带因取消，按面板中断同词汇记 aborted                                                                                    | 部署窗口内是预期行为；窗口外出现说明排空被意外触发，查 stderr `drain timed out` 告警与最近部署记录                                                                           |
| ccload 渠道被冷却                                                                                                                               | ccload           | 连续失败计数                                                                                                                                                              | `SELECT cooldown_until FROM channels WHERE id=<渠道id>`                                                                                                                      |
| 进程活着但端口拒绝连接                                                                                                                          | launchd（macOS） | dyld/Gatekeeper 卡住                                                                                                                                                      | `sample <pid>` 确认后 `kill -9`，KeepAlive 会重拉                                                                                                                            |
| 日志全 `completed` 无 error_stage，但会话"想了很久只回一句"或工具全 `[Tool use interrupted]`                                                    | 客户端           | 打断 - 续跑循环（见下文 Claude Code 条目）——代理交付完整，断在客户端权限层                                                                                                | `/file/01-http-request.json?raw=1` 端点查尾部 `"(no content)"`（gzip 库存 LIKE 不命中）；别往上游查                                                                          |

## 标准排查流程

### 1. 拿原始请求体（ccload debug_logs）

```bash
cd <ccload 仓库目录>
sqlite3 data/ccload.db \
  "SELECT req_body FROM debug_logs ORDER BY log_id DESC LIMIT 1;" > /tmp/req.bin
sqlite3 data/ccload.db \
  "SELECT resp_body FROM debug_logs ORDER BY log_id DESC LIMIT 1;" > /tmp/resp.bin
```

- `req_body` 是 ccload 实际发给 devin-2api 的 JSON（**注意 ccload 会再注入自己的 system prompt**，所以 debug 必须用这里的原文而不是客户端本地文件）。
- `resp_body` 是 SSE 流的话，看**最后一个事件**：`response.failed` 里的 `message`/`type` 才是真实错误；HTTP 200 不代表成功。
- debug_logs 会轮转清空，要趁新鲜取。

### 2. 绕过 ccload 直连复现

```bash
curl -sN http://localhost:3003/v1/responses \
  -H "Authorization: Bearer <api_key>" -H "Content-Type: application/json" \
  --data-binary @/tmp/req.bin | tail -5
```

传输断裂类故障（envelope 截断/连接重置）可用 `cmd/upstreamstub` 本地复现：起桩监听后把测试实例 `devin.base_url` 指过去，用 `-scenario` 选故障形态，验证重试链路与 stage 归类：

| 场景                            | 桩行为                                                            | 预期归类                                                                                |
| ------------------------------- | ----------------------------------------------------------------- | --------------------------------------------------------------------------------------- |
| `precontent`                    | 元数据帧后半帧前缀截断                                            | transport，pre-content 重发一次                                                         |
| `midcontent`                    | 内容帧后截断                                                      | transport，已产出内容回显续传（≤2 次）                                                  |
| `recover`                       | 前 N 次截断后返回完整流（`-recover-after`）                       | 透明自愈，`retries:1`                                                                   |
| `cleaneof` / `cleaneof-content` | 无尾帧干净收尾（截断等价形态）                                    | transport；pre-content 重发 / post-content 续传                                         |
| `bare-end`                      | 有 EndStream 无 stopReason                                        | `provider_stream`，"ended without generated content"                                    |
| `endstream-error`               | EndStream 携带限流错误                                            | `devin_connect` 语义错误 + 速率闩                                                       |
| `stream`                        | 正常全流基线（`-deltas`/`-delta-bytes`/`-interval`/`-ttfb` 可调） | 非故障形态：completed，验证正常通路与时延分解                                           |
| `badframe` / `badflags`         | 帧体截断 / 垃圾 flag 字节                                         | transport，pre-content 重发一次                                                         |
| `stall`                         | 建流后零帧挂死                                                    | 120s 看门狗判死 → pre-content 重发 / post-content 续传 → transport                      |
| `end-hang`                      | 完整终止序列后 body 不收尾                                        | stopReason 后 15s 尾部宽限到点按正常 EOF 干净收尾                                       |
| `heartbeat`                     | 周期无事件帧续命                                                  | 零事件帧不喂「无进度」期限 → 兜底判死：pre-content 10min 重发 / post-content 45min 续传 |

看门狗是双层的：`upstreamStallTimeout`（120s，任意帧判活的传输活性探测）+ 无进度期限（只认产出事件帧的内容进度探测，两档：产出前 `upstreamNoProgressTimeout`=10min，产出过内容后 `devin.no_progress_timeout_seconds` 默认 45min——上游在工具调用参数阶段可静默计算 15-25min 只发心跳，pre 档必误杀）。stopReason 消费后等待窗口缩到 `upstreamTailGrace`（15s）——connect-go 读 endstream envelope 时会排空 body 等传输 EOF，上游不关连接就靠这层干净收尾。内容已下发后的截断走续传而非整体重发：在飞块物化进 assistant 回显、追加 "continue" 用户消息重发（`maxStreamResumes`=2），客户端先收块 end 接缝再续新块；在飞工具调用与已收 stopReason/停止序列截断的流不续，按错误透传。

客户端断连不杀「已产出内容」的上游流：流脱钩登记进进程内完成缓存（键是 02 投影剔除 session_key/dropped 后的语义哈希，model 用解析后 uid；容量 8，TTL running 45min / completed 60min / failed 5min，触顶逐过期再逐最老 running），后台泵续消费并缓冲全部事件。同键重试在 `Stream` 入口命中即重放——completed 秒回全量、running 重放前缀后按下标追帧、failed 仅在失败可重放（非上游责任/取消类）时重放终态、否则当未命中走新上游。脱钩写 `detached` 标记行进原 dir 的 04，挂接写 `detached_attach` 进重试 dir 的 04（带 `origin_dir` 回指）。pre-content 断开不脱钩（没有可重放前缀）；脱钩泵关掉无进度看门狗（耐心是它的意义，running TTL 是存活上界）但保留 stall 看门狗（零帧=连接真死）。断开判定有两条腿：消费方 Recv 的 ctx.Done 分支 + `Stream` 起的哨兵协程（app 泵投递点两路就绪随机选，断开后可能不再进 Recv——没哨兵那条路径会漏成「无人杀也无人养」的孤儿泵）；两侧持同一把 stream.mu 就地判定，先到者赢。running 条目被 TTL/容量淘汰掐 drain ctx 退场时补一条终局错误记 failed（截断前缀不误标 completed），正常 EOF 才记 completed。条目缓冲另有 8MiB 字节预算（合法流实测最大 ~400KiB）：越界即截断——缓冲冻结成「前缀 + 截断错误事件」，原 dir 04 记 `detached_truncated` 标记行；截断条目 lookup 一律未命中并就地逐出（同键重试走新上游），在飞挂接方重放到显式错误而非无声 EOF，后台泵下轮自检停泵不再白耗上游配额。注意原 dir 完结后 04/05 不再追写——后台泵后续帧的取证只在条目缓冲里，不在盘上。

直连同样失败 → 问题在 devin-2api/上游，与 ccload 无关。

### 3. 二分裁剪请求体

对失败的 JSON 逐块删除重试，定位触发字段。典型裁剪维度（按嫌疑排序）：

- `input`/`messages` 里的历史项（先砍到只剩最后一条）
- `tools` / function 定义
- `system` / `instructions`
- `reasoning` / thinking 块
- `tool_use` / `tool_result` 块（注意配对结构）

快速判定：同一请求去掉 tools 仍失败 → 问题在消息历史；去掉历史只留 system + 一条 user 仍失败 → 问题在 system prompt（多半是指纹句）。

### 4. 看生成的 wire（debug 日志）

```bash
# 优先热切换：PUT /admin/settings/debug_log_enabled，不用重启
curl -s -X PUT http://localhost:<port>/admin/settings/debug_log_enabled \
  -H "Authorization: Bearer <dashboard.password>" \
  -H 'Content-Type: application/json' -d '{"value":"true"}'
# 改 config.yaml 后也可 POST /admin/config/reload 热应用，
# 返回里 requires_restart 列出的字段才需要托管重启（冷路径）
# 复现一次请求，然后看该请求的 03-devin-request.json
# （/admin/debug-logs/{id}/file/03-devin-request.json 或
#  sqlite3 devin-2api.db 查 debug_files）
```

`chatMessagePrompts` 里每条消息的 `source`/`prompt`/`toolCalls`/`toolCallId` 是否出现，直接对照下面的契约表。protojson 输出里**字段缺席**和**字段为空串**是两回事——上游对两者行为不同。

另外每个请求在进程日志里有一行 `slog` 汇总（`api`/`status`/`duration_ms`/`model`/`stream`/`client_ip`/`upstream_request_id`/token 用量），debug 记录的 `meta.json` 带同样的字段外加 `user_agent`/`key_hash`/TTFB 标记——排障时先扫这一行往往就能定位是哪类失败，不用拆 proto。

### 5. 对照实验

怀疑某个结构约束时，构造最小差异的两份请求同时打。本次实战：同一历史 `call0, call1, result0, result1` 被拒，改成 `call0, result0, call1, result1` 即通过——证实 call→result 必须紧邻。

### 6. 逆向参考

- **`upstream-protocol.md`**：按主题整理的上游协议逆向结论（字段契约、帧形态、签名体制、错误分类、RPC 面），本文契约表的详细证据都在那里。
- **WindsurfAPI**（github.com/dwgx/WindsurfAPI）`src/devin-connect.js` 的注释标了哪些字段是 `VERIFIED-FROM-WIRE`——他们的实证结论基本可以直接信。
- **抓 devin CLI 真实流量**：把 `~/.local/share/devin/credentials.toml` 的 `api_server_url` 指向本地捕获服务器（Connect 流式 body 有信封：`flag(1B) + len(4B BE) + protobuf`），回放缓存的 GetUserStatus / GetCliModelConfigs / GetCliTeamSettings 让 CLI 走完启动流程拿到 GetChatMessage 请求体，用 `outputs/devin-proto-go` 生成的绑定解码，`protoscope` / 未知字段检查可发现我们 proto 缺失的字段。**实验完必须恢复 `api_server_url`**，运行中的 CLI 会话会因此断线重连。

## 已验证的上游 wire 契约

这些是用真实请求逐条试出来的硬约束（详见 `internal/adapter/devin/devin.go` 注释）：

1. **call→result 紧邻配对**：assistant 发出的每个 tool call 必须紧跟它的 TOOL 结果消息，「全部调用→全部结果」的分组序列直接 `invalid_argument`。（`pairToolCallsWithResults` 负责重排）
2. **助手回合合并为单条消息**：一个 assistant 回合 = 一条 `ChatMessagePrompt`，`prompt`+`thinking`+`signature`+`toolCalls` 数组同体携带（真实客户端抓包形态，从不出现相邻 SYSTEM 对）；无文本时 `prompt` 字段完全省略——空串与缺席不同。拆成多条会在渲染上下文插入假回合边界，显著抬高模型在宣告句末尾采 EOS 的概率（premature end_turn 事故，调查档案见 `notes/archive/2026-09-12-premature-endturn.md`）。同一拆线形态有第二条成因：`/v1/responses` 解码器曾把一回合铺平的多个 input item 逐条成消息（issue #2，`d53dfde` 起解码层合并相邻 AssistantMessage，档案 `notes/archive/2026-09-14-issue2-responses-turn-fragmentation.md`）——查相邻 SYSTEM 对要同时怀疑编码层与解码层。
3. **thinking 挂每条 assistant 消息**（#11），签名 #12 跟 thinking 走。
4. **tool result 文本不能为空**，空则占位 `[tool result]`。
5. **完全空的 assistant 轮跳过**（实测诱发上游反复返回空回复）。
6. **工具 schema 剥离**：`Description` 换工具名、剥 annotations（`convertToolDefinition`，防 Cursor 类 MCP-gate 指纹）。被剥掉的信息经 `withToolDescriptions` 搬进 system prompt 尾的 `# tools descriptions` 段：每个工具一条 `<tool name="…">`，内含编号化说明全文 + `Parameters:` 字段摘要（从原始 `input_schema` 的字段级 `description`/`title` 提升——条件必填如 "Required unless `stop` is true" 只活在字段 prose 里，剥离后摘要就是它唯一的幸存通道；行头对 prose 声明必填的字段补标 `required`，超长描述截头时 required 尾句必保留）。注入段按预算分级：96KB 软顶内全文，超出降 compact（说明截 400 runes、摘要完整），再超降 skinny（纯名清单），skinny 过 256KB 硬顶报 `tool_preamble_too_large` 400。实证记录见 `notes/archive/2026-09-17-schedulewakeup-conditional-required.md`。
7. **特征句指纹库**（`permission_denied`）：对 system prompt / 消息 / 工具描述做等义改写，规则在 `sanitize.go`——对齐 WindsurfAPI 实证规则 + 本项目 bisect 新增的 CC/Codex 指纹（tool-call 冒号句、CC 2.1.x 提示词行、subagent emoji 禁令、Codex 模板三条等，逐条实证记录见 `upstream-policy-fingerprints.md`）。
8. ~~空 system prompt + 带 tools 会被拒~~：**2026-09-12 实测已不成立**——上游不再因此拒绝，代码也已不再注入兜底 system prompt（仅 `withToolDescriptions` 把工具说明并入 system 字段）。保留此条仅为解释旧记录。
9. **前缀缓存**：内容前缀即命中，无需会话状态；`trajectory_id`/`cascade_id` 稳定 + EPHEMERAL 断点可提升命中率（详见 `upstream-cache.md`）。
10. **stepType 恒为 `USER_INPUT`**，末条消息**不要求**是 USER（实测 TOOL 结尾只要配对正确也能过）。
11. **签名是尾随帧，且按 provider 分体制**：上游在全部正文之后才发 `DeltaSignature`+`DeltaSignatureType`。已观测三种体制：`sealed`（swe-2，`sealed.v1.<b64>`）、`anthropic`（claude-thinking，原生签名 base64）、`openai`（gpt-sol，签名是序列化 reasoning item）。回放时 type 必须与 provider 配对存取——张冠李戴触发流内 `invalid_argument`。解码器把签名合并回上一个 thinking 块（`decodeLateSignature`），编码器延迟 thinking 块的收尾直到签名到达——绝不能落成独立的空 thinking 块（Claude Code 会整条丢弃消息，表现为 result 为空但 HTTP 200）。副作用：当签名帧隔着 text/tool_use 块才到时，下发的 SSE 是「嵌套」块序——thinking 的 `signature_delta`/`content_block_stop` 插在 tool_use 的 delta 中间，同一时刻有两块未收尾，偏离 Anthropic 逐块顺序约定；claude-cli 2.1.269 整日实测容忍，但严格单块假设的解析器可能把迟到的 stop 误当当前块结束，属已知取舍（提前关块会丢签名，危害更大）。
12. **缺 stopReason 的干净 EOF = 截断，不是正常结束**：正常结束必有 stopReason 帧（swe-2/gemini/deepseek = `STOP_PATTERN`，claude = `MIN_LOG_PROB`——字面误导，实为 end_turn 映射，工具调用 = `FUNCTION_CALL`）；文本后直接 EOF、`deltaToolCalls`/`responseDimensionGroups` 全缺是截断。decoder 直接报流错误（"Devin stream ended without stop reason"）而非合成 end_turn——唯一例外是 `stoppedByPattern`（本地停止序列截断）。
13. **工具名字符集 ≈ `[A-Za-z0-9_-]`**：点/冒号/CJK 工具名（`mcp::x`、`a.b`、`工具`）被上游以模糊的 `invalid_argument` 拒绝；`mcp__a__b` 合法。
14. **freeform/custom 工具无原生声明通道**：`is_custom_tool`+`custom_tool_grammar` 声明 → 确定性 `unknown`（0 帧）。可用形态是「包装 function」：单 `input` 字符串参数的 schema，模型把原文填进 `{"input":"…"}`，响应侧解包（`unwrapCustomToolArguments`）。历史方向 `invalid_json_str`+`is_custom_tool_call` 则原生有效。

## 新客户端验证清单

接入新客户端（cc、pi、kimi-code、kimi-cli…）时按此顺序跑：

1. 单轮（先确认基本通路 + 身份句是否被封）：

    ```bash
    curl -s http://localhost:3003/v1/messages -H "Authorization: Bearer <api_key>" \
      -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
      -d '{"model":"swe-2-max","max_tokens":64,"messages":[{"role":"user","content":"Reply exactly: pong"}]}'
    ```

2. 把该客户端的真实 system prompt 整个塞进去（指纹句风险最大的一步）。
3. 多轮记忆（历史回放是否正常）。
4. 单工具调用 → tool_result 回传 → 最终回答。
5. 单轮并行多工具调用 + 多个 tool_result（最容易踩配对约束）。
6. `stream: true` 同样跑一遍 3–5。

每个客户端的特异风险：

- **Claude Code**：主会话与 subagent 系统提示词都在指纹库里（CC 2.1.236 主提示词 7 条 + subagent 提示词的 emoji 禁令整句已入 `sanitize.go`，新版 CC 换文案会再封）；`metadata.user_id` 会被当 SessionKey 用。subagent 被拒时 CC 报 "issue with the selected model"，主 agent 会自述「subagent 不可用」——不是模型问题，查 `error.json` 的 permission_denied。已实测的两个客户端侧坑：
    - **本地模型白名单**：CC 2.1.x 在发请求前就拒绝不认识的模型名（`swe-2-max` 直接被拦，ccload 收不到请求）。解法：ccload `channel_models` 加 `claude-sonnet-4-6` 等可识别名 → `redirect_model=swe-2-max`；CC 侧 `ANTHROPIC_MODEL` 填可识别名。`modelOverrides`/`CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1` 也可，但 redirect 最不侵入。
    - **settings env 覆盖 shell**：`~/.claude/settings.json` 的 `env` 块优先级高于 shell 环境变量，里面若有 `ANTHROPIC_BASE_URL` 会盖掉导出的值（进程在连别的地址、半天无输出即此症状）。用项目级 `.claude/settings.local.json` 注入 env 最干净。
    - **打断 - 续跑死循环**（claude-cli 2.1.269/agent-sdk 实测，2026-09-13 一次 48 分钟 ~30 轮）：工具调用经安全评审子调用（非流式、`stop_sequences:["</block>"]`、prompt 要求整个响应以 `<block>` 开头，经本代理走同一模型）判 `<block>no`（不阻止）后，仍在评审之后的权限层被程序性打断（评审结束到打断 ~0.2s，非人工 Esc），客户端把该轮记成 thinking-only assistant（tool_use 整块丢弃）并自动发 `"(no content)"` user 消息续跑 → 模型在历史里看不见自己的调用被打断，盲重试新命令 → 再打断，直到某轮响应不带 tool_use（end_turn）或用户重开会话才落地。用户视角 = `Thought for Xm` 转很久只冒一行字（该时长是整轮墙钟，含中间不可见的循环）。代理侧一切正常（tool_use 完整、`stop_reason=tool_use`、message_stop 齐全）；伴随症状是评审子调用里偶发 `max_tokens` 烧完返回零 text 的空 verdict（thinking 计入预算，评审 max_tokens=2112 被思考吃光）。日志签名：`01-http-request.json` 尾部 `user:"(no content)"` + 历史里连续 thinking-only assistant 消息。
- **Codex**：`apply_patch` 的 FREEFORM 裸词、"do not wrap the patch in JSON"；0.153.3 模板另有三条系统提示指纹（open-source 定义句、plan 状态句对、ANSI 转义句，均已入 `sanitize.go`，实证细节见 `upstream-policy-fingerprints.md`）；`type:"custom"` 工具（apply_patch）已支持——上行包装成单 `input` 参数 function、下行解包回 `custom_tool_call` 原文（上游 `is_custom_tool` 声明通道坏，绕行见 `upstream-protocol.md`）；`namespace` 子工具递归展平为 `{ns}__{sub}`、`web_search*` 桥接为托管搜索（代理代调上游 `GetWebSearchResults` 后续轮，见 `internal/adapter/devin/websearch.go`）；`mcp`/`local_shell`/`file_search` 等无桥接类型仍记 `Dropped` 丢弃。
- **pi**(`@mariozechner/pi-coding-agent`,0.73.x 实测全通):接法 = `~/.pi/agent/models.json` 自定义 provider,`baseUrl` 指 ccload、`api` 用 `anthropic-messages`、`apiKey` 填 ccload token，模型声明 `id:"swe-2-max"` + `contextWindow`/`maxTokens`。**关键特征:pi 的 anthropic-messages provider 会在 system 数组开头塞完整的 Claude Code 指纹提示词**(billing header + "You are Claude Code" 全文),自己真正的系统提示词以 `[System Instructions]` 前缀放进 user 消息——所以 CC 的指纹改写规则自动覆盖 pi，白嫖同一条已打通路径。pi 会发 `thinking:{type:"enabled",budget_tokens:8192}`,上游按需返回 thinking+signature。自带压缩 (`contextTokens > contextWindow - reserveTokens`,默认留 16k reserve/20k recent,`/compact` 手动),无需代理侧压缩。
- **kimi-code**:零修改直接通 (0.42.0 实测)。伪装 CC 请求封套 (`claude-cli` UA、`X-Claude-Code-Session-Id`、CC beta 头),`metadata.user_id` 带 device_id JSON 被用作 SessionKey。陌生模型名要在 `[models.X]` 手写 `capabilities` 才有 tool_use。自带压缩。
- **kimi-cli**:官方已弃用 (并入 kimi-code),不建议投入。
- **通用**:预期风险点 = 身份句指纹 + 工具调用配对约束 + 各自专有字段 (先抓真实请求体看有没有非标准块)。

出现 `permission_denied` → 按第 3 步二分定位触发句，加进 `sanitize.go` 的 `upstreamSanitizeRules`；出现 `invalid_argument` → 开 debug 看 `03-devin-request.json`，对照契约表。

## ccload 侧注意事项

- 本机示例渠道 id=293（`http://127.0.0.1:3003`），模型表在 `channel_models`，`redirect_model` 可做别名（与 devin-2api 的 `devin.aliases` 二选一即可，现在后者统一管）。
- **`protocol_transform_mode` 用 `local`**（原生直通）：`auto` 会把 `/v1/messages` 转成 `/v1/responses` 再转回来，ccload 的 codex→anthropic 转换会把尾随签名落成独立的空 thinking 块（Claude Code 收到后 result 为空）。它是 `channels` 表列，写库即热生效（走缓存失效）——需要重启的只有 `system_settings`。
- ccload 会统计 SSE 级失败（HTTP 200 + `response.failed`/`error` 事件也算失败），连续失败会把渠道打冷却。devin-2api 的应对分三层：① 首个上游事件前不下发 `start`，上游零帧报错（`permission_denied` 等）走真实 HTTP 4xx，ccload 按客户端错误透传不冷却渠道；② 例外有两个，都只在 `StreamErrorEvents` 面（OpenAI 流式）先补合成 `start` 再发 error 事件（`internal/app/stream.go`）：上下文超长——为了让 Codex 收到 `response.failed`（它只在 SSE 事件里认 `error.code=="context_length_exceeded"`），事件顶层 `status:413` 让 ccload 仍按客户端级分类、不冷却；**流式面 429 同理**——OpenAI 流式客户端的可重试通道只有流内事件（Codex 把 HTTP 429 硬编码为不重试，只把流内错误进重试循环），429 也走 200 + error 事件下发；③ 流式中途（已有语义输出、连接已提交后）的错误事件同样在 data 里带顶层 `status`，ccload 按真实语义分类且事件原文会继续透传给客户端。
- `.env` 里的 `CCLOAD_API_TOKENS` 是入站客户端 key；`auth_tokens` 表是持久化的 token（明文）。

## 运维坑

- **重启腰斩在途流**：托管重启（`kickstart -k`、`systemctl --user restart`）和 `kill -9` 会立刻掐断所有进行中的 SSE 响应，客户端视角就是"回答突然停止"。改配置/二进制前先在前置网关侧停流量或挑空闲窗口；调试时优先用备用端口起第二个实例（`listen: ":3004"`）验证，不要动在线实例。另外停止超时已设为 660s（launchd `ExitTimeOut` / systemd `TimeoutStopSec`，覆盖二进制 600s 排空上限），优雅退出期间在途流会继续跑完，不要用 `kill -9` 抢时间。
- **不要手动跑 `./devin-2api` 抢监听端口**：手动实例和托管器的自动重拉（launchd KeepAlive / systemd Restart=always）会互相抢端口（每 5s 崩溃循环），谁抢到谁服务，交替时全部在途流被掐。所有实例必须经托管器启停。bind 连续失败（重启风暴）会落 `logs/bind-failure.json` 标记（`first_at`/`last_at`/`count`/`holder`）并暴露到 `/admin/runtime-metrics`——排查「服务反复起不来」先看这两处。
- **meta.json 的 `repairs` 计数有基线、不是故障**：CC 类客户端每请求重发同一套系统提示词，指纹改写与投影修复必然命中——实测基线 ~6–17 hits/req。要盯的是命中规则 id 集合的漂移（出现新 id = 客户端换了提示词文案，可能要吃新指纹），而不是总数的正常涨落。
- **macOS 特有——launchd + 新编译二进制**：`go build` 覆盖二进制后立刻 kickstart，dyld 可能卡在 Gatekeeper 检查（进程 `S` 态、无监听、无日志）。`sample <pid>` 看栈确认后 `kill -9` 等 KeepAlive 重拉即可；稳妥做法是先 build 再停旧进程。详见 `deployment.md`。
- **CLI 抓包实验后遗症**：恢复 `credentials.toml` 后，已开的 CLI 会话需发任意消息重连。
- **VS Code Remote-SSH autoForwardPorts 抢 loopback（accept-then-hang）**：Mac 上开着指向 archbox 的 Remote-SSH 窗口且两端有同端口监听时，VS Code `Code Helper` 会把 Mac `127.0.0.1:3003` 绑成回 archbox 的隧道——IPv4 精确绑定赢过服务的 `*:3003` IPv6 wildcard，隧道那头不应答，**特征是 TCP connect 成功但零字节（curl 000、fetch 永不返回），不是 connection refused**，比端口冲突难诊断一个量级。分诊：`lsof -iTCP:3003 -sTCP:LISTEN -P -n` 看持有者 + 三路径 curl 对比（`127.0.0.1` / `[::1]` / tailscale IP）。处置：VS Code Ports 面板删残留 + `remote.autoForwardPorts=false`（防复活），客户端 base URL 改 `http://[::1]:3003`（IPv6 loopback 免疫 v4 squatter）。注：该事故时生产在 Mac；2026-09-18 起生产在 archbox `:3033`，archbox 上 `:3003` 变为 compat shim → `127.0.0.1:3033`（打它等价打生产，无害但要自知）；VS Code 隧道劫持的坑在 fht-mba 侧仍然存在（Mac :3003 现由 forwarder 持有，squatter 抢绑同样会黑掉 shim 流量）。
- **上游方向 IPv6 污染嫌疑**：`server.codeium.com` 在 fht-mba 上曾解析出 mihomo fake-ip AAAA `2001:2::5`（bogon，逃出 tun 直连 GFW）——间歇性中流 `incomplete envelope` 截断 + 抓到 `2001:2::/48` 对端时先查上游出口 IPv6（GitHub 方向同款注入 `2001:2::4` 已有 ProxyCommand 绕行，见 AGENTS.md 拓扑节）。
- **fht-mba 登录 shell 是 fish**：`ssh fht-mba 'VAR=x; cmd'` / 单行 `for` / heredoc 全炸（`Unsupported use of '='`、`Expected a string`），`bash -lc '…'` 嵌套引号是地狱。规范解法 `ssh fht-mba bash -s <<'EOF' … EOF`（脚本走 stdin，绕开 login shell）。Mac 侧另注意：递归 grep `~/.claude`/`~/.codex` 会超 120s 被挪后台，改定点文件列表逐个查。
- **从日志辨认 subagent/teammate 流**：CC 的 `metadata.user_id` 是 JSON `{device_id, account_uuid, session_id, parent_session_id?}`——`session_id`/`parent_session_id` 可聚主流/子流；subagent 标记 `cc_is_subagent=true` 烤在 `system[0]` 头 300 字符的 `x-anthropic-billing-header` 里；**teammate（并行协作代理）流不带 sub 标记**，与主流共享 `(session_id, system hash)`——按「同流新消息即 supersession」判退役会误判，`scripts/index-stream-stats.py` 的流画像已按此口径实现。
- **面板探活是真实计费上游调用**：`/admin/model-test`（ccpanel 探活）走真实 /v1 管线，`client_request_id=panel-probe` 落 `logs` 表（`log_source='manual_test'`）——做 token 成本/命中率聚合时别误算进用户流量。

## 客户端上下文窗口配置（自动压缩前提）

上游 `GetCliModelConfigs` 报告 `swe-2-max` 真实窗口 **262000**。客户端若以为窗口更大，auto-compact 阈值会设在上限之外，永远撞 prompt-too-long 而不压缩。已验证的可用配置：

- **Codex** `~/.codex/config.toml`：`model_context_window = 262000`，`model_auto_compact_token_limit = 230000`。resume 实验确认 240k 历史触发 `context compacted` 后正常续答。
- **Claude Code** `~/.claude/settings.json` env：`CLAUDE_CODE_MAX_CONTEXT_TOKENS=262000`（非 `claude-` 前缀模型的窗口声明）、`CLAUDE_CODE_AUTO_COMPACT_WINDOW=230000`。实测 ~202k 用量后自动压缩（阈值≈window-28k buffer），压缩后上下文降到 ~17k。
- CC 另有单条 prompt ≤80% 窗口的客户端保护（~209k tokens），超限直接 "Prompt is too long" 不发请求；`-c -p` resume 时若投影总量超窗也同样拒绝，不会自动压缩——这是边界保护不是 bug。
- Codex 只在 SSE `response.failed` 事件里按 `error.code=="context_length_exceeded"` 触发错误恢复式压缩；裸 HTTP 413 错误体不会触发（走 generic request error）。因此 devin-2api 对流式请求刻意先补合成 `start` 再发 error 事件（顶层 `status:413` + `code=context_length_exceeded`）。注意路径差异：**直连 :3003 时** Codex 能收到 `response.failed`；**经 ccload 时** `response.created`/`in_progress` 不算语义输出、不会促使 ccload 提交响应，ccload 仍在写出前截住 error 事件并物化成 HTTP 413 给客户端——与裸 413 效果等价（客户端级、零冷却），只是拿不到 SSE 形态。Anthropic 面不同：`message_start` 算语义输出会提交，error 事件随后原文透传。非流式请求统一是干净的 HTTP 413。
- ccload 的 `/v1/models` 不透传 `context_tokens` 等元数据，客户端无法经 discovery 学到窗口，只能靠上述本地配置。
