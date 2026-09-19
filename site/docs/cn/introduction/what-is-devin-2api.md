# devin-2api 是什么？

devin-2api 是一个非官方协议适配器：把你 Devin 账号（[app.devin.ai](https://app.devin.ai/)）可用的模型挂到 OpenAI 与 Anthropic 兼容的端点之后，让标准客户端（Codex、Claude Code、任意 SDK）用熟悉的 API 调用它们。

::: warning 免责声明
本项目与 Cognition 没有任何关联或背书。它用你自己的 Devin 凭据（session token 或 durable 平台 key）对接一个内部 RPC 面。仅供个人账号自用；遵守 Devin 服务条款的责任在你。
:::

## 你能得到什么

- **一个上游、三个 API 面**——`POST /v1/responses`（OpenAI Responses，含为 Codex 式客户端准备的多轮会话 WebSocket 传输）、`POST /v1/chat/completions`（OpenAI Chat）、`POST /v1/messages`（Anthropic Messages）。三个面都支持流式与非流式。
- **推理跨轮往返**——thinking 签名被保留并在后续轮次重放：Responses 面是 `encrypted_content` 推理项，Anthropic 面是 `redacted_thinking`，Chat 面是 `reasoning_content`。
- **工具调用**——custom/freeform 工具调用（如 `apply_patch`）原样往返；工具名与 `tool_choice` 本地校验；严格的 call↔result 重排与上游强制口径一致。服务端托管的 `web_search` 声明经上游搜索 RPC 执行，以原生 `web_search_call`/`server_tool_use` 项返回——Claude Code 的 WebSearch 旁路请求会被短路成一次托管搜索。
- **图片、文档、视频输入**——`input_image`、`input_file`/`file`/`document`、`video`/`video_url`/`input_video` 部件在三个面都能解码（含 Anthropic 的 `image`/`document`/`video` 块）。按模型能力位在上行前本地把关——不支持的模型快速失败，而不是静默丢附件。视频只取帧，不含音轨。
- **上游流恢复**——过期 token 自动从凭据源重载；产出内容前的上游故障（传输断裂、静默停滞、空响应）透明重试；早期故障以真实 HTTP 错误暴露，而不是 `200` 已提交后再发 SSE 错误。
- **脱钩完成缓存**——客户端断连不会掐断上游流：它在服务端继续跑并进入完成缓存，语义相同的重试直接续接——已完成的条目即刻重放，进行中的先重放缓冲前缀再跟随实时流。条目跨进程重启保留。
- **多账号上游池**——`devin.accounts` 每条 lane 自带凭据、速率闸门与配额追踪。会话亲和把一段对话钉在同一条 lane，上游故障 failover 到更健康的兄弟号，周配额过低时 lane 对新会话降权。lane 在面板实时管理——空池是合法起始状态。
- **限流闸门**——上游 `resource_exhausted` 触发本地冷却闩：排队请求短暂等待后快败 `429` + `Retry-After`，而不是继续捶打被限流的上游；滴灌探针检测恢复；闩状态跨重启持久化。
- **归一化错误契约**——上游错误码映射成正确的 HTTP 状态与各协议错误类型；限流变成 `429` + `Retry-After`；每个请求带 `X-Request-Id`/`debug_ref` 指明它的调试记录。
- **`/v1/models` 能力位**——上下文窗口与工具/thinking/图片/文档/视频支持来自上游模型目录；`devin.aliases` 条目带 `alias_of` 出现；面板模型注册表可停用或重定向单个名字。
- **`/web` 管理面板**——请求浏览、用量/成本聚合、配额追踪、进程指标、逐请求调试 payload、下游令牌管理、号池 lane 控制、模型注册表、脱敏配置视图与热重载、托管安装一键自更新。
- **单一静态二进制**——另有 GHCR 公开 Docker 镜像。

## 它不是什么

- **不是托管服务**——你在自己的机器上跑、用自己的 Devin 凭据。
- **不是响应存储**——纯 HTTP 下代理是无状态的：每个请求必须带全量对话，`previous_response_id` 会被显式拒绝而不是静默丢上下文。WebSocket 传输在连接内维持多轮会话，把增量输入展开成全量 transcript。
- **不是官方 Devin API**——它适配的是一个可能随时变化的内部 RPC 面，偶尔会坏，坏了我发版修。
