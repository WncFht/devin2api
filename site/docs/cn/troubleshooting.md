# 排障

每个 `/v1/*` 响应带 `X-Request-Id` 头（错误体另带 `debug_ref`），值即本次请求的调试记录名——它是一切排查的入口。

## 定位首个失败点

开启请求日志（`debug.enabled`）后，每个请求在 `devin-2api.db` 留一条记录。两步快查：

1. `GET /admin/logs?q=<request-id>` → `logs` 行：状态、`error_stage`（哪一层败的）、`error_message`。
2. `GET /admin/debug-logs/{id}/file/error.json` → 管线内首个失败点（`{id}` 是 logs 行的 `id` 列）。

状态目录的 `logs/stderr.log` 每请求一行摘要。完整的「症状 → 层 → 处置」手册在仓库 `docs/upstream-debug-playbook.md`。

## 常见症状

| 症状                        | 大概率原因                                 | 处置                                                                                                                 |
| --------------------------- | ------------------------------------------ | -------------------------------------------------------------------------------------------------------------------- |
| `401`                       | 令牌仓无匹配行，或上游拒了账号凭据         | 到 `/web/tokens.html` 建令牌；查 `/web/accounts.html` 的 lane 状态——`unauthenticated` 时 lane 会重解析凭据并重试一次 |
| `unavailable`               | 账号池为空                                 | `/web/accounts.html` 加号，或 `devin.accounts` + 配置 reload                                                         |
| `429` + `Retry-After`       | 本地速率闸门快败，或上游在限流             | `error_stage` 区分两者：`rate_gate` = 本地闩（按响应头说的时刻重试），`devin_connect` = 上游拒绝                     |
| SSE 在 ~300s 附近停滞       | 客户端自己的总超时，不是代理的             | 断开的流在服务端继续跑；语义相同的重试从完成缓存续接                                                                 |
| `prompt-too-long`           | 客户端声明的上下文窗口高过真实值 262000    | 窗口按 262000 声明、auto-compact 阈值压在它之下——见各客户端页                                                        |
| `permission_denied`         | 上游内容策略拦了客户端指纹或提示词本身     | sanitizer 已覆盖 Claude Code 指纹；其它内容可能自己触线——看 `error.json` 里的上游原话                                |
| `previous_response_id` 被拒 | HTTP 下代理无状态                          | 每请求带全量对话，或改用按连接维持会话的 WebSocket 传输                                                              |
| connect 成功但零字节        | `127.0.0.1` 被 IDE 的 IPv4 端口转发 shadow | 服务绑 `*` 时客户端指 `http://[::1]:<port>`                                                                          |
| `/admin/update` 回 `501`    | 实例非托管                                 | 自更新只对一键脚本 / 部署脚本装的服务有效；其它形态按安装路线重跑                                                    |

## 还没解决

- `GET /admin/runtime-metrics` 看实时闸门状态、逐 lane 健康与进程指标。
- `/admin/debug-logs/{id}` 暴露全阶段记录（`01-http-request` 到 `06-http-response`）——从客户端字节到上游 wire 的完整管线。
- 到 [GitHub](https://github.com/WncFht/devin2api/issues) 开 issue，附上 `X-Request-Id` 与 `error.json` 内容（payload 未脱敏——分享前先读 `meta.json` 确认）。
