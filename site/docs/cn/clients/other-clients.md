# 其他客户端

任何讲 OpenAI Chat Completions、OpenAI Responses 或 Anthropic Messages 的客户端都能接 devin-2api。始终需要两个值：代理地址（`server.listen`，本文示例用 `http://127.0.0.1:8080`）与 `/web/tokens.html` 创建的下游令牌。

## 通用 SDK

- **Anthropic Messages**——SDK 的 base URL 指 `http://127.0.0.1:8080`、api key 填下游令牌，请求落 `POST /v1/messages`。
- **OpenAI**——base URL `http://127.0.0.1:8080/v1`、api key 填下游令牌。`chat.completions.create` 落 `/v1/chat/completions`；Responses API 客户端落 `/v1/responses`。

令牌经 `Authorization: Bearer <token>` 或 `X-Api-Key: <token>` 传递。

## pi

`~/.pi/agent/models.json`：

```json
{
    "providers": {
        "devin": {
            "baseUrl": "http://127.0.0.1:8080",
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

使用：`pi --provider devin --model swe-2-max`，交互里 `/model` 也可选。pi 没有权限审批体系，工具默认全部直接执行——收窄用 `--tools read,grep,find,ls`（白名单）、`--no-builtin-tools`、`--no-tools`。

## kimi-code

`~/.kimi-code/config.toml`：

```toml
default_model = "swe-2-max"

[providers.devin]
type = "anthropic"
base_url = "http://127.0.0.1:8080"
api_key = "<下游令牌>"

[models."swe-2-max"]
provider = "devin"
model = "swe-2-max"
max_context_size = 262000
capabilities = ["thinking", "tool_use", "image_in"]
```

陌生模型名必须手写 `capabilities`，否则没有工具调用。可选值：`thinking` / `always_thinking` / `tool_use` / `image_in` / `video_in`。也支持 `type = "openai"`（chat completions）或 `"openai_responses"`。权限档位与 `[[permission.rules]]` 见其官方文档——`default_permission_mode` 取 `manual` / `yolo` / `auto`。

## 共用注意事项

- **上下文窗口**——客户端问窗口就填真实值 262000；窗口声明错位会让自动压缩时机错乱（见各客户端节）。
- **system prompt 指纹**——各客户端的身份提示词可能被上游内容策略拦截（`permission_denied`）。devin-2api 的 sanitizer 已覆盖 Claude Code 指纹；pi / kimi-code 都伪装 CC 请求头与提示词，自动被同一套规则覆盖。
- **工具调用配对**——上游强制 call→result 紧邻配对，代理自动重排，客户端无感。
- **base URL 写 `http://[::1]:<port>` 最稳**——服务绑 `*` 时 `127.0.0.1` 可能被 IDE 的 IPv4 端口转发静默 shadow（connect 成功但零字节），`localhost` 依赖 resolver 顺序。
