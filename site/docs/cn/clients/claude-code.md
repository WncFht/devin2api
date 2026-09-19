# Claude Code

Claude Code 走 Anthropic Messages 面（`POST /v1/messages`）接入 devin-2api。需要两个值：代理地址与 `/web/tokens.html` 创建的下游令牌。

## 配置

`~/.claude/settings.json`：

```json
{
    "env": {
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
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

`8080` 换成你的 `server.listen` 端口。模型名填 `swe-2-max`，或你在 `devin.aliases` / 模型注册表里配的名字。

## 两个窗口 env 为什么关键

`swe-2-max` 非 `claude-` 前缀，CC 走 unknown-model 默认窗口——远小于实际上游上限 262000。不声明则自动压缩阈值错位：要么压得太早浪费窗口，要么阈值超出真实上限、请求直接 prompt-too-long 而不是先压缩。`AUTO_COMPACT_WINDOW` 留 ~30k 给压缩请求自身的指令与摘要开销。

等价替代：让 CC 发 `claude-` 前缀名、由 `devin.aliases` 映射回 `swe-2-max`——窗口计算就跟随 claude 模型档位。

## 可选容错 env

```json
{
    "env": {
        "CLAUDE_CODE_MAX_RETRIES": "15",
        "CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS": "300000"
    }
}
```

- `CLAUDE_CODE_MAX_RETRIES`——上游限流 episode 可长达十几分钟；CC 按 `anthropic-ratelimit-unified-reset` 头睡到恢复时刻，每次重试各占一档预算，预算越高扛过的 episode 越长（非第一方模式被钳到 ≤15）。
- `CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS`——首字节看门狗毫秒数；上游长 thinking 场景放宽到 300000。

## WebSearch

Claude Code 的 WebSearch 原本是一次携带 `web_search` 服务端工具的旁路请求——devin-2api 认出这个形态并短路成一次经上游搜索 RPC 的托管搜索，以原生 `server_tool_use` 块返回。无需配置。
