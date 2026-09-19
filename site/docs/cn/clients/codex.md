# Codex

Codex 走 OpenAI Responses 面（`POST /v1/responses`）接入 devin-2api。

## 配置

`~/.codex/config.toml`：

```toml
model_provider = "OpenAI"
model = "swe-2-max"
model_context_window = 262000
model_auto_compact_token_limit = 230000

[model_providers.OpenAI]
base_url = "http://127.0.0.1:8080/v1"
stream_max_retries = 100
```

下游令牌经 `OPENAI_API_KEY` 环境变量提供。

两个细节必须对：

- **`model_context_window` / `model_auto_compact_token_limit` 按真实窗口 262000 配。** 缺省或错配的更大值会让 auto-compact 阈值落在上限之外——超限请求直接失败，而不是先压缩。
- **`stream_max_retries = 100`。** codex-rs 对 HTTP 429 一律终止（`retry_429` 硬编码 false），代理已把 pre-stream 限流转成 `response.failed` 流内事件——Codex 读事件里的 `try again in Ns` 睡到解闩再续。默认 5 次只覆盖不到一分钟的限流窗口，上游分钟桶 episode 更长，建议加到上限 100。

`apply_patch` 是 `type: "custom"` 工具调用，经代理原样往返。

## WebSocket 链路（可选）

devin-2api 的 Responses 面同时走 WebSocket——`GET /v1/responses` 协商升级，多轮会话活在连接上（`response.create` / `response.append` 增量输入，服务端展开成全量 transcript）。Codex 侧在 `[model_providers.OpenAI]` 写 `supports_websockets = true`、模型条目带 `prefer_websockets` 即启用。不配则走普通 HTTP POST，没有任何功能差异。
