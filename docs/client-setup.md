# 客户端接入指南

每个客户端只需要两个值：devin-2api 的地址（`server.listen`，本文示例用 `http://127.0.0.1:3033`）和一个下游令牌——令牌在面板 `/web/tokens.html` 创建，明文创建时一次性出示，仓内只存哈希。模型名直接填 `swe-2-max`（或你在 `devin.aliases`/模型注册表里配的名字）。

作者本机另经一层 ccload（`客户端 → ccload http://127.0.0.1:49173 → devin-2api http://127.0.0.1:3033`）做渠道管理与冷却，不是必需——走同款链路时把各节的地址换成 ccload 入口、令牌换成 ccload 侧 token，ccload 侧注意事项见 `upstream-debug-playbook.md`。本机 `:3003` 是兼容转发 shim（拓扑见 `deployment.md` 末节），旧写法照样通。

## Claude Code

`~/.claude/settings.json`:

```json
{
    "env": {
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:3033",
        "ANTHROPIC_AUTH_TOKEN": "<下游令牌>",
        "ANTHROPIC_MODEL": "swe-2-max",
        "ANTHROPIC_SMALL_FAST_MODEL": "swe-2-max",
        "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "262000",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "230000"
    }
}
```

两个窗口 env 是关键：`swe-2-max` 非 `claude-` 前缀，CC 走 unknown-model 默认窗口（远小于实际上游上限 262000），不声明则自动压缩阈值错位——要么压得太早浪费窗口，要么阈值超出真实上限永远撞 prompt-too-long。`AUTO_COMPACT_WINDOW` 留 ~30k 给压缩请求自身的指令与摘要开销。不配窗口声明也可让 CC 发 `claude-` 前缀名、由 `devin.aliases` 映射回 `swe-2-max`（经 ccload 时等价做法是 `channel_models` 的 redirect）。

可选的容错 env（逆向 CC 二进制得到的行为，视需要添加）：

```json
{
    "env": {
        "_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL": "1",
        "CLAUDE_CODE_MAX_RETRIES": "15",
        "CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS": "300000"
    }
}
```

- `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL`：让 CC 按第一方 Anthropic API 的参数档处理自定义 base URL——重试上限从 10 提到 300、首字节看门狗从 300s 收到 180s。devin-2api 已在所有 `/v1/*` 响应下发 `request-id: req_*`，CC 据此即按第一方对待，通常不必再设。
- `CLAUDE_CODE_MAX_RETRIES`：重试次数上限；非第一方模式被钳到 ≤15。上游限流 episode 可长达十几分钟（分钟桶计数 + 被拒续期），429 走 `anthropic-ratelimit-unified-reset` 头睡到恢复时刻，每次重试各占一档预算——预算越高扛过的 episode 越长。
- `CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS` / `CLAUDE_STREAM_IDLE_TIMEOUT_MS`：首字节与流内空闲看门狗毫秒数；上游长 thinking 场景把首字节放宽到 300000（默认即 300s，第一方档是 180s——注意设了 first-party 假设反而更紧，需要的话用此项显式放宽）。

## pi

`~/.pi/agent/models.json`:

```json
{
    "providers": {
        "devin": {
            "baseUrl": "http://127.0.0.1:3033",
            "api": "anthropic-messages",
            "apiKey": "<下游令牌>",
            "models": [
                {
                    "id": "swe-2-max",
                    "contextWindow": 262000,
                    "maxTokens": 32768,
                    "reasoning": true,
                    "input": ["text", "image"]
                }
            ]
        }
    }
}
```

使用：`pi --provider devin --model swe-2-max`,交互里 `/model` 也可选。

**权限**:pi 没有权限审批体系，工具默认全部直接执行。收窄用 `--tools read,grep,find,ls`(白名单)、`--no-builtin-tools`、`--no-tools`。

## kimi-code

`~/.kimi-code/config.toml`:

```toml
default_model = "swe-2-max"

[providers.devin]
type = "anthropic"
base_url = "http://127.0.0.1:3033"
api_key = "<下游令牌>"

[models."swe-2-max"]
provider = "devin"
model = "swe-2-max"
max_context_size = 262000
capabilities = ["thinking", "tool_use", "image_in"]
```

陌生模型名必须手写 `capabilities`,否则没有工具调用。可选值：`thinking` / `always_thinking` / `tool_use` / `image_in` / `video_in`——`image_in` 已实测 (swe-2-max 支持图片输入，历史图片会被代理转成文本占位);`video_in` 不要开，上游 `ChatMessagePrompt` 没有视频字段。也支持 `type = "openai"`(chat completions) 或 `"openai_responses"`。

**权限**:三档模式 + 细粒度规则。

```toml
default_permission_mode = "auto"   # manual(默认逐条问) / yolo(常规自动，危险问) / auto(全自动)
default_plan_mode = false           # 只读规划模式

[[permission.rules]]
decision = "allow"
pattern = "Read"

[[permission.rules]]
decision = "deny"
pattern = "Bash(rm -rf*)"
```

单次启动用 `-y`/`--yolo` 或 `--auto`;另有 `[[hooks]] event="PreToolUse"` 可挂自定义审批脚本。

## Codex

`~/.codex/config.toml`:

```toml
model_provider = "OpenAI"
model = "swe-2-max"
model_context_window = 262000
model_auto_compact_token_limit = 230000

[model_providers.OpenAI]
base_url = "http://127.0.0.1:3033/v1"
# 下游令牌经 OPENAI_API_KEY 环境变量提供
```

Codex 走 OpenAI Responses 面 (`POST /v1/responses`),直连与经 ccload 转发均可。`apply_patch` 是 `type:"custom"` 工具调用（0.154.0 起经代理包装过境，实测补丁落盘；更早版本走 `exec_command` shell 兜底），无兼容问题。`model_context_window`/`model_auto_compact_token_limit` 必须按真实窗口 262000 配——默认/错配的更大值会让 auto-compact 阈值落在上限之外，超限请求直接失败而不是先压缩（已实测验证：240k 历史 resume 触发 `context compacted`）。

上游限流（429）对 Codex 只经流内错误事件重试——codex-rs 对 HTTP 429 一律终止（`retry_429` 硬编码 false），代理已把 pre-stream 429 转成 `response.failed` 事件下发，Codex 按事件里的 `try again in Ns` 睡到解闩再续。默认 `stream_max_retries = 5` 大约只覆盖不到一分钟的限流窗口；常见的一分钟桶限流建议在 `[model_providers.OpenAI]` 块内加一行 `stream_max_retries = 100`（上限 100）。

### Codex WebSocket 链路（可选）

Codex 支持 Responses-over-WS：一条连接上反复 `response.create`/`response.append`，`previous_response_id` + 增量 input。ccload→devin-2api 的 WS 多轮已实现并实测通过（2026-09-12，`responses-ws` 全链路 `completed`）。本机链配置（经 ccload 的渠道形态，本机示例）：

- ccload 侧建一条独立的 WS 渠道（本机示例：channel 294 `devin-ws`，独立于 293 `devin` 的 anthropic 渠道）：url `http://127.0.0.1:3003/v1`、protocols `["codex"]`、`websockets=1`，模型 `swe-2-max-ws` → redirect `swe-2-max`。
- `~/.codex/models.ccload.json` 的 `swe-2-max-ws` 条目带 `prefer_websockets: true`；还原备份在同目录 `models.ccload.json.bak-ws-test`。
- Codex 侧需 `supports_websockets=true`：写进 `[model_providers.OpenAI]`，或启动时 `-c` override。

验证命令：

```bash
codex exec -m swe-2-max-ws \
  -c 'model_providers.OpenAI.supports_websockets=true' \
  --skip-git-repo-check "run echo ws-chain-test"
```

## 共用注意事项

- **system prompt 指纹**:各客户端的身份提示词可能被上游内容策略拦截 (`permission_denied`)。devin-2api 的 `sanitize.go` 已覆盖 Claude Code 指纹;pi / kimi-code 都会伪装 CC 请求头 + 提示词，自动被同一套规则覆盖。
- **工具调用配对**:上游强制 call→result 紧邻配对，代理已自动重排，客户端无感。
- **压缩**:四个客户端都自带上下文压缩，代理无需处理——但自动压缩只在客户端声明的窗口 ≤ 上游真实窗口 (262000) 时才可能先于 prompt-too-long 触发；Codex/CC 的窗口声明见上文各节和 `upstream-debug-playbook.md` 的「客户端上下文窗口配置」。
- **排查**:经 ccload 链时先看 `ccload.db` 的 `debug_logs`(取注入后的真实请求体);再看 devin-2api debug 的 `03-devin-request.json`。详见 `upstream-debug-playbook.md`。
- **base URL 写 `http://[::1]:<port>` 最稳**：服务绑 `*`（IPv6 双栈 socket）时 `::1` 直连本机；`127.0.0.1` 会被 IDE 的 IPv4 端口转发静默 shadow（VS Code Remote-SSH autoForwardPorts 会把 loopback 绑成隧道，特征是 connect 成功但零字节——curl 000 而非 refused），`localhost` 则依赖 resolver 顺序可能先撞 v4 squatter。诊断与处置见 `upstream-debug-playbook.md` 运维坑节。
- **跨机访问走 tailnet IP，不走 loopback 转发**：本机示例形如 `http://<tailnet-ip>:3033`（按自己的 tailnet 替换）；`:3003` 旧地址由转发 shim 继续兜住，存量配置不急着改——完整拓扑见 `deployment.md` 末节。本机出向曾挂本地并发闸 gwcap（swe-2-medium 限流），现已下线、仅留档 `scripts/gwcap/`——压测/批跑直接打满上游 `devin.max_rpm` 即可。
