# 客户端接入指南

拓扑：`客户端 → ccload http://127.0.0.1:49173(token)→ devin-2api http://127.0.0.1:3003(key 240127)→ Devin 上游`。

所有客户端统一走 ccload 入口，模型名直接填 `swe-2-max`(ccload `channel_models` 已注册)。直连 devin-2api 也可以，把地址换成 `:3003`、key 换成 `240127` 即可。

## Claude Code

`~/.claude/settings.json`:

```json
{
    "env": {
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:49173",
        "ANTHROPIC_AUTH_TOKEN": "<ccload token>",
        "ANTHROPIC_MODEL": "swe-2-max",
        "ANTHROPIC_SMALL_FAST_MODEL": "swe-2-max",
        "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "262000",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "230000"
    }
}
```

两个窗口 env 是关键：`swe-2-max` 非 `claude-` 前缀，CC 走 unknown-model 默认窗口（远小于实际上游上限 262000），不声明则自动压缩阈值错位——要么压得太早浪费窗口，要么阈值超出真实上限永远撞 prompt-too-long。`AUTO_COMPACT_WINDOW` 留 ~30k 给压缩请求自身的指令与摘要开销。不配窗口声明则用 ccload `channel_models` 的 redirect(发 `claude-sonnet-4-6` → `swe-2-max`) 兜底。

## pi

`~/.pi/agent/models.json`:

```json
{
    "providers": {
        "devin": {
            "baseUrl": "http://127.0.0.1:49173",
            "api": "anthropic-messages",
            "apiKey": "<ccload token>",
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
base_url = "http://127.0.0.1:49173"
api_key = "<ccload token>"

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

`~/.codex/config.toml`(本机已配好):

```toml
model_provider = "OpenAI"
model = "swe-2-max"
model_context_window = 262000
model_auto_compact_token_limit = 230000

[model_providers.OpenAI]
base_url = "http://127.0.0.1:49173/v1"
```

Codex 走 OpenAI Responses 面 (`POST /v1/responses`),ccload 原生转发到 devin-2api。`apply_patch` 通过 `exec_command` shell 命令执行，不走 tool call，无兼容问题。`model_context_window`/`model_auto_compact_token_limit` 必须按真实窗口 262000 配——默认/错配的更大值会让 auto-compact 阈值落在上限之外，超限请求直接失败而不是先压缩（已实测验证：240k 历史 resume 触发 `context compacted`）。

## 共用注意事项

- **system prompt 指纹**:各客户端的身份提示词可能被上游内容策略拦截 (`permission_denied`)。devin-2api 的 `sanitize.go` 已覆盖 Claude Code 指纹;pi / kimi-code 都会伪装 CC 请求头 + 提示词，自动被同一套规则覆盖。
- **工具调用配对**:上游强制 call→result 紧邻配对，代理已自动重排，客户端无感。
- **压缩**:四个客户端都自带上下文压缩，代理无需处理——但自动压缩只在客户端声明的窗口 ≤ 上游真实窗口 (262000) 时才可能先于 prompt-too-long 触发；Codex/CC 的窗口声明见上文各节和 `upstream-debug-playbook.md` 的「客户端上下文窗口配置」。
- **排查**:任何问题先看 `ccload.db` 的 `debug_logs`(取注入后的真实请求体),再开 devin-2api debug 看 `03-devin-request.json`。详见 `upstream-debug-playbook.md`。
