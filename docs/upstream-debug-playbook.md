# 上游兼容问题排查手册

本仓库把 Anthropic / OpenAI 协议请求转成 Devin Connect `GetChatMessage` 的 Connect-RPC 调用。上游是闭源黑盒，错误信息几乎全是 `permission_denied: an internal error occurred` 或 `invalid_argument: an internal error occurred` 这种无信息量包装。本文沉淀已验证的排查方法和上游契约，供后续接入新客户端时复用。

## 链路全景

```
client (cc / codex / kimi-cli / ...)
  → ccload :49173            (渠道管理、冷却、格式转换)
    → devin-2api :3003       (协议转换 + sanitize + wire 构造)
      → server.codeium.com   (Devin 上游, Connect-RPC)
```

任何一环出错都会以「重试/失败」的形式表现在客户端。定位的第一步永远是**确定错误在哪一层产生**。

## 错误速查表

| 现象                                                  | 层         | 含义                              | 处理                                                     |
| ----------------------------------------------------- | ---------- | --------------------------------- | -------------------------------------------------------- |
| HTTP 401                                              | devin-2api | api_key 不对                      | 查 `auth.api_key` / 请求头                               |
| HTTP 503 `devin token not configured`                 | devin-2api | 没拿到上游 token                  | 查 `devin.token` / 自动发现链                            |
| `permission_denied`（无 policy 文案）                 | 上游       | 模型 UID 不存在/无权              | 查别名表、模型名拼写                                     |
| `permission_denied` + "blocked by our content policy" | 上游       | 命中特征句指纹库                  | bisect 请求体，把触发句加进 `sanitize.go`                |
| `invalid_argument`                                    | 上游       | **wire 形状不符**（不是内容问题） | 对照本文「已验证 wire 契约」逐条查                       |
| `unexpected EOF` / connection reset                   | 传输       | 上游偶发抖动                      | 建立阶段重试 3 次（仅纯传输错误）；仍失败换模型/稍后再试 |
| Connect code 错误（含 `unavailable`）                 | 上游       | 确定性语义错误                    | **不重试**——"try later" 文案是固定模板                   |
| HTTP 200 + SSE `response.failed`/`error`              | 上游       | 流建立后上游才拒绝                | 同上，看事件里的 code/status 分类                        |
| ccload 渠道被冷却                                     | ccload     | 连续失败计数                      | `SELECT cooldown_until FROM channels WHERE id=293`       |
| 进程活着但端口拒绝连接                                | launchd    | dyld/Gatekeeper 卡住              | `sample <pid>` 确认后 `kill -9`，KeepAlive 会重拉        |

## 标准排查流程

### 1. 拿原始请求体（ccload debug_logs）

```bash
cd ~/Desktop/src/ccload
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
  -H "Authorization: Bearer 240127" -H "Content-Type: application/json" \
  --data-binary @/tmp/req.bin | tail -5
```

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
# config.yaml 打开 debug.enabled，重启服务
sed -i '' 's/^  enabled: false/  enabled: true/' config.yaml
launchctl kickstart -k gui/$(id -u)/com.devinuser.devin-2api
# 复现一次请求，然后看 logs/<时间戳>/03-devin-request.json
```

`chatMessagePrompts` 里每条消息的 `source`/`prompt`/`toolCalls`/`toolCallId` 是否出现，直接对照下面的契约表。protojson 输出里**字段缺席**和**字段为空串**是两回事——上游对两者行为不同。

另外每个请求在进程日志里有一行 `slog` 汇总（`api`/`status`/`duration_ms`/`model`/`stream`/`client_ip`/`upstream_request_id`/token 用量），debug 录制目录的 `meta.json` 带同样的字段外加 `user_agent`/`key_hash`/TTFB 标记——排障时先扫这一行往往就能定位是哪类失败，不用拆 proto。

### 5. 对照实验

怀疑某个结构约束时，构造最小差异的两份请求同时打。本次实战：同一历史 `call0, call1, result0, result1` 被拒，改成 `call0, result0, call1, result1` 即通过——证实 call→result 必须紧邻。

### 6. 逆向参考

- **WindsurfAPI**（github.com/dwgx/WindsurfAPI）`src/devin-connect.js` 的注释标了哪些字段是 `VERIFIED-FROM-WIRE`——他们的实证结论基本可以直接信。
- **抓 devin CLI 真实流量**：把 `~/.local/share/devin/credentials.toml` 的 `api_server_url` 指向本地捕获服务器（Connect 流式 body 有信封：`flag(1B) + len(4B BE) + protobuf`），回放缓存的 GetUserStatus / GetCliModelConfigs / GetCliTeamSettings 让 CLI 走完启动流程拿到 GetChatMessage 请求体，用 `outputs/devin-proto-go` 生成的绑定解码，`protoscope` / 未知字段检查可发现我们 proto 缺失的字段。**实验完必须恢复 `api_server_url`**，运行中的 CLI 会话会因此断线重连。

## 已验证的上游 wire 契约

这些是用真实请求逐条试出来的硬约束（详见 `internal/adapter/devin/devin.go` 注释）：

1. **call→result 紧邻配对**：assistant 发出的每个 tool call 必须紧跟它的 TOOL 结果消息，「全部调用→全部结果」的分组序列直接 `invalid_argument`。（`pairToolCallsWithResults` 负责重排）
2. **助手轮拆分**：一条 assistant 消息 = 可选文本消息 + **每个 tool call 各一条独立消息**；tool call 消息上 `prompt` 字段（#3）完全省略——空串与缺席不同。
3. **thinking 挂每条 assistant 消息**（#11），签名 #12 跟 thinking 走。
4. **tool result 文本不能为空**，空则占位 `[tool result]`。
5. **完全空的 assistant 轮跳过**（实测诱发上游反复返回空回复）。
6. **工具 schema 剥离**：`Description` 换工具名、剥 annotations（`convertToolDefinition`，防 Cursor 类 MCP-gate 指纹）。
7. **特征句指纹库**（`permission_denied`）：对 system prompt / 消息 / 工具描述做等义改写，规则在 `sanitize.go`，对齐 WindsurfAPI 全量实证规则 + 本项目新增的 tool-call 冒号句。
8. ~~空 system prompt + 带 tools 会被拒~~：**2026-09-12 实测已不成立**——上游不再因此拒绝，代码也已不再注入兜底 system prompt（仅 `withToolDescriptions` 把工具说明并入 system 字段）。保留此条仅为解释旧记录。
9. **前缀缓存**：内容前缀即命中，无需会话状态；`trajectory_id`/`cascade_id` 稳定 + EPHEMERAL 断点可提升命中率（详见 `upstream-cache.md`）。
10. **stepType 恒为 `USER_INPUT`**，末条消息**不要求**是 USER（实测 TOOL 结尾只要配对正确也能过）。
11. **签名是尾随帧**：上游在全部正文之后才发 `DeltaSignature`。解码器把它合并回上一个 thinking 块（`decodeLateSignature`），编码器延迟 thinking 块的收尾直到签名到达——绝不能把签名落成独立的空 thinking 块（Claude Code 会整条丢弃消息，表现为 result 为空但 HTTP 200）。

## 新客户端验证清单

接入新客户端（cc、pi、kimi-code、kimi-cli…）时按此顺序跑：

1. 单轮（先确认基本通路 + 身份句是否被封）：

    ```bash
    curl -s http://localhost:3003/v1/messages -H "Authorization: Bearer 240127" \
      -H "Content-Type: application/json" -H "anthropic-version: 2023-06-01" \
      -d '{"model":"swe-2-max","max_tokens":64,"messages":[{"role":"user","content":"Reply exactly: pong"}]}'
    ```

2. 把该客户端的真实 system prompt 整个塞进去（指纹句风险最大的一步）。
3. 多轮记忆（历史回放是否正常）。
4. 单工具调用 → tool_result 回传 → 最终回答。
5. 单轮并行多工具调用 + 多个 tool_result（最容易踩配对约束）。
6. `stream: true` 同样跑一遍 3–5。

每个客户端的特异风险：

- **Claude Code**：系统提示词整体在指纹库里（CC 2.1.236 实测 7 条指纹行已入 `sanitize.go`，新版 CC 换文案会再封）；`metadata.user_id` 会被当 SessionKey 用。已实测的两个客户端侧坑：
    - **本地模型白名单**：CC 2.1.x 在发请求前就拒绝不认识的模型名（`swe-2-max` 直接被拦，ccload 收不到请求）。解法：ccload `channel_models` 加 `claude-sonnet-4-6` 等可识别名 → `redirect_model=swe-2-max`；CC 侧 `ANTHROPIC_MODEL` 填可识别名。`modelOverrides`/`CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1` 也可，但 redirect 最不侵入。
    - **settings env 覆盖 shell**：`~/.claude/settings.json` 的 `env` 块优先级高于 shell 环境变量，里面若有 `ANTHROPIC_BASE_URL` 会盖掉导出的值（进程在连别的地址、半天无输出即此症状）。用项目级 `.claude/settings.local.json` 注入 env 最干净。
- **Codex**：`apply_patch` 的 FREEFORM 裸词、"do not wrap the patch in JSON"；reasoning item、`custom`/`namespace`/`web_search` 工具类型会被静默丢弃（上游不认），Codex 可能依赖 apply_patch 工具——注意行为偏差。
- **pi**(`@mariozechner/pi-coding-agent`,0.73.x 实测全通):接法 = `~/.pi/agent/models.json` 自定义 provider,`baseUrl` 指 ccload、`api` 用 `anthropic-messages`、`apiKey` 填 ccload token，模型声明 `id:"swe-2-max"` + `contextWindow`/`maxTokens`。**关键特征:pi 的 anthropic-messages provider 会在 system 数组开头塞完整的 Claude Code 指纹提示词**(billing header + "You are Claude Code" 全文),自己真正的系统提示词以 `[System Instructions]` 前缀放进 user 消息——所以 CC 的指纹改写规则自动覆盖 pi，白嫖同一条已打通路径。pi 会发 `thinking:{type:"enabled",budget_tokens:8192}`,上游按需返回 thinking+signature。自带压缩 (`contextTokens > contextWindow - reserveTokens`,默认留 16k reserve/20k recent,`/compact` 手动),无需代理侧压缩。
- **kimi-code**:零修改直接通 (0.42.0 实测)。伪装 CC 请求封套 (`claude-cli` UA、`X-Claude-Code-Session-Id`、CC beta 头),`metadata.user_id` 带 device_id JSON 被用作 SessionKey。陌生模型名要在 `[models.X]` 手写 `capabilities` 才有 tool_use。自带压缩。
- **kimi-cli**:官方已弃用 (并入 kimi-code),不建议投入。
- **通用**:预期风险点 = 身份句指纹 + 工具调用配对约束 + 各自专有字段 (先抓真实请求体看有没有非标准块)。

出现 `permission_denied` → 按第 3 步二分定位触发句，加进 `sanitize.go` 的 `upstreamSanitizeRules`；出现 `invalid_argument` → 开 debug 看 `03-devin-request.json`，对照契约表。

## ccload 侧注意事项

- 渠道 293 = `http://127.0.0.1:3003`，模型表在 `channel_models`，`redirect_model` 可做别名（与 devin-2api 的 `devin.aliases` 二选一即可，现在后者统一管）。
- **`protocol_transform_mode` 用 `local`**（原生直通）：`auto` 会把 `/v1/messages` 转成 `/v1/responses` 再转回来，ccload 的 codex→anthropic 转换会把尾随签名落成独立的空 thinking 块（Claude Code 收到后 result 为空）。改完要重启 ccload 才生效。
- ccload 会统计 SSE 级失败（HTTP 200 + `response.failed`/`error` 事件也算失败），连续失败会把渠道打冷却。devin-2api 的应对分三层：① 首个上游事件前不下发 `start`，上游零帧报错（`permission_denied` 等）走真实 HTTP 4xx，ccload 按客户端错误透传不冷却渠道；② 唯一的例外是上下文超长——为了让 Codex 收到 `response.failed`（它只在 SSE 事件里认 `error.code=="context_length_exceeded"`），会先补发一个合成 `start` 再发 error 事件，事件顶层 `status:413` 让 ccload 仍按客户端级分类、不冷却；③ 流式中途（已有语义输出、连接已提交后）的错误事件同样在 data 里带顶层 `status`，ccload 按真实语义分类且事件原文会继续透传给客户端。
- `.env` 里的 `CCLOAD_API_TOKENS` 是入站客户端 key；`auth_tokens` 表是持久化的 token（明文）。

## 运维坑

- **重启腰斩在途流**：`launchctl kickstart -k` 和 `kill -9` 会立刻掐断所有进行中的 SSE 响应，客户端视角就是"回答突然停止"。改配置/二进制前先在 ccload 侧停流量或挑空闲窗口；调试时优先用备用端口起第二个实例（`listen: ":3004"`）验证，不要动在线实例。另外 `ExitTimeOut` 已设为 60s，优雅退出期间在途流会继续跑完，不要用 `kill -9` 抢时间。
- **不要手动跑 `./devin-2api` 抢 :3003**：手动实例和 launchd 的 KeepAlive 会互相抢端口（每 5s 崩溃循环），谁抢到谁服务，交替时全部在途流被掐。所有实例必须经 launchd 启停。
- **launchd + 新编译二进制**：`go build` 覆盖二进制后立刻 kickstart，dyld 可能卡在 Gatekeeper 检查（进程 `S` 态、无监听、无日志）。`sample <pid>` 看栈确认后 `kill -9` 等 KeepAlive 重拉即可；稳妥做法是先 build 再停旧进程。
- **CLI 抓包实验后遗症**：恢复 `credentials.toml` 后，已开的 CLI 会话需发任意消息重连。

## 客户端上下文窗口配置（自动压缩前提）

上游 `GetCliModelConfigs` 报告 `swe-2-max` 真实窗口 **262000**。客户端若以为窗口更大，auto-compact 阈值会设在上限之外，永远撞 prompt-too-long 而不压缩。已验证的可用配置：

- **Codex** `~/.codex/config.toml`：`model_context_window = 262000`，`model_auto_compact_token_limit = 230000`。resume 实验确认 240k 历史触发 `context compacted` 后正常续答。
- **Claude Code** `~/.claude/settings.json` env：`CLAUDE_CODE_MAX_CONTEXT_TOKENS=262000`（非 `claude-` 前缀模型的窗口声明）、`CLAUDE_CODE_AUTO_COMPACT_WINDOW=230000`。实测 ~202k 用量后自动压缩（阈值≈window-28k buffer），压缩后上下文降到 ~17k。
- CC 另有单条 prompt ≤80% 窗口的客户端保护（~209k tokens），超限直接 "Prompt is too long" 不发请求；`-c -p` resume 时若投影总量超窗也同样拒绝，不会自动压缩——这是边界保护不是 bug。
- Codex 只在 SSE `response.failed` 事件里按 `error.code=="context_length_exceeded"` 触发错误恢复式压缩；裸 HTTP 413 错误体不会触发（走 generic request error）。因此 devin-2api 对流式请求刻意先补合成 `start` 再发 error 事件（顶层 `status:413` + `code=context_length_exceeded`）。注意路径差异：**直连 :3003 时** Codex 能收到 `response.failed`；**经 ccload 时** `response.created`/`in_progress` 不算语义输出、不会促使 ccload 提交响应，ccload 仍在写出前截住 error 事件并物化成 HTTP 413 给客户端——与裸 413 效果等价（客户端级、零冷却），只是拿不到 SSE 形态。Anthropic 面不同：`message_start` 算语义输出会提交，error 事件随后原文透传。非流式请求统一是干净的 HTTP 413。
- ccload 的 `/v1/models` 不透传 `context_tokens` 等元数据，客户端无法经 discovery 学到窗口，只能靠上述本地配置。
