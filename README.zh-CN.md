# devin-2api

> [English](README.md) | **中文**

devin-2api 是一个轻量转发工具，把 Devin（[app.devin.ai](https://app.devin.ai/)）包装在 OpenAI / Anthropic 兼容接口后面——让外部程序可以通过标准协议调用 Devin 内部的模型。

## 特性

- **一个上游，三个 API 面**——`POST /v1/responses`（OpenAI Responses，含 Codex 式客户端的 WebSocket transport 与多轮会话）、`POST /v1/chat/completions`（OpenAI Chat）、`POST /v1/messages`（Anthropic Messages）
- **支持流式与一次性响应**（typed SSE / JSON）
- **思考签名跨轮回放**——按各 provider 原生形态（`sealed`/`anthropic`/`openai`）保存并回传；在 Responses 面落成 `encrypted_content` reasoning item，Anthropic 面落成 `redacted_thinking`，Chat 面落成 `reasoning_content`
- **忠实的工具调用**——custom/freeform 工具调用原文往返；工具名与 `tool_choice` 本地校验；按上游强制的 call↔result 交错序重新配对；孤儿工具结果降级为文本而非整请求失败
- **上游流韧性**——首个内容字节前的失败（传输断裂、从凭据文件重读的过期 token、静默卡死、空 end_turn）透明重试一次；start 事件延后下发，早期上游失败返回真实 HTTP 错误而非已提交 200 后的 SSE error
- **归一化错误契约**——上游 Connect 错误码映射为正确的 HTTP 状态与协议错误类型；限流归一为 `429` + 从文案解析出的 `Retry-After`；每个请求带 `X-Request-Id`/`debug_ref` 直指调试目录
- **`/v1/models` 能力位透出**——上下文窗口、工具/thinking/图片支持等来自上游模型配置
- **`/panel` 管理面板**——请求浏览、用量/成本聚合、配额追踪、进程指标、按请求调试目录
- **对齐真实 Devin CLI 指纹**——请求 metadata 复刻 CLI 的客户端身份（`devin.client_*` 可配置，上游加版本门时 bump `client_version` 即可）
- **适配器模式**——极易扩展新的上游
- **部署简单**——单一静态二进制，[GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api) 公开镜像
- **可选调试日志**——按请求记录，便于排查问题

## 快速开始

### 1. 获取 Devin token

devin-2api 使用你的 Devin 会话 token 向 Devin 鉴权。macOS 下可从 Devin 应用本地状态提取：

```bash
sqlite3 ~/Library/"Application Support"/Devin/User/globalStorage/state.vscdb \
  "SELECT json_extract(value, '$.apiKey') FROM ItemTable WHERE key='windsurfAuthStatus';"
```

输出为 `devin-session-token$...` 格式的完整 token。

### 2. 配置

```bash
cp config.example.yaml config.yaml
```

编辑 `config.yaml`，填入你的 token（基于 `config.example.yaml` 起步时只需填 `devin.token`——base_url 和 model 已有示例值）。

### 3. 启动

预编译二进制（见 [Releases](https://github.com/WncFht/devin2api/releases)，附 `checksums.txt` 可校验）：

```bash
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-darwin-arm64
chmod +x devin-2api-darwin-arm64
./devin-2api-darwin-arm64 -config config.yaml
```

源码运行（需先从仓库内的描述符生成 proto 绑定——clone 后一次性步骤，依赖 `protoc` + `protoc-gen-go` + `protoc-gen-connect-go` + `task`，版本见 `Taskfile.yml`）：

```bash
task generate
go run ./cmd/devin-2api -config config.yaml
```

Docker（镜像已发布至 [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)）：

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

### 4. 验证

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"v0.3.0","uptime_seconds":12,"debug_logging":false}
```

## 用法

> **注意**：`/v1/*` 接口支持可选的 API Key 鉴权。在 `config.yaml` 中设置 `auth.api_key` 后，客户端需通过 `Authorization: Bearer <api_key>` 或 `X-Api-Key: <api_key>` 传递密钥；留空则不校验，请只在可信网络内暴露。

接口列表：

- `POST /v1/responses`——OpenAI Responses（同路径 `GET` 可协商 WebSocket transport）
- `POST /v1/chat/completions`——OpenAI Chat Completions
- `POST /v1/messages`——Anthropic Messages
- `GET /v1/models`、`GET /v1/models/{model}`——上游模型列表与能力位
- `GET /panel`——管理面板（请求浏览、用量、配额、进程指标）；`dashboard.password` 保护

使用你的 OpenAI Responses API 客户端调用 `http://localhost:8080/v1/responses` 即可。

一次性响应：

```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5-2",
    "input": "你好"
  }'
```

流式响应（SSE）：

```bash
curl -N http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5-2",
    "input": "你好",
    "stream": true
  }'
```

请求体遵循 OpenAI Responses API（`input`、`instructions`、`tools`、`stream` 等）。Anthropic Messages 客户端改打 `/v1/messages`：

```bash
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "glm-5-2",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

各协议面支持的字段子集见[贡献指南](CONTRIBUTING.md)。

## 配置

配置文件为 YAML，启动时加载一次；未知字段会被拒绝。

| 字段                                             | 说明                                                                                                        | 必填 / 默认值                                                                                      |
| ------------------------------------------------ | ----------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `server.listen`                                  | HTTP 监听地址                                                                                               | 是                                                                                                 |
| `server.max_concurrency`                         | `/v1/*` 并发请求上限                                                                                        | `1024`                                                                                             |
| `devin.base_url`                                 | Devin Connect 服务地址                                                                                      | 配置了 `devin.token` 后必填（代码无默认值；`config.example.yaml` 用 `https://server.codeium.com`） |
| `devin.token`                                    | Devin 会话 token（`devin-session-token$...`）                                                               | 否——未配置时接口返回 503                                                                           |
| `devin.model`                                    | Devin chat model UID（如 `glm-5-2`）                                                                        | 配置了 `devin.token` 后必填（代码无默认值）                                                        |
| `devin.aliases`                                  | 客户端模型名 → 上游真实 UID 映射（如 `swe-2: swe-2-max`）                                                   | 无                                                                                                 |
| `devin.client_name`/`client_version`/`client_os` | 发给上游 metadata 的客户端身份（上游给新模型加版本门时 bump `client_version` 即可）                         | `chisel` / `3000.2.17` / `mac`                                                                     |
| `devin.proxy`                                    | 上游代理地址（`http(s)://`、`socks5(h)://`）；留空直连或走环境变量                                          | 无                                                                                                 |
| `devin.force_http1`                              | 每请求独立 TCP 连上游（避免 HTTP/2 单连接多 stream 串行化）                                                 | `true`                                                                                             |
| `debug.enabled`                                  | 在配置文件同目录的 `logs/` 下写按请求的调试日志                                                             | `false`                                                                                            |
| `debug.retention_days`                           | 请求日志目录保留天数；`<=0` 不按时间清理                                                                    | `14`                                                                                               |
| `debug.max_total_mb`                             | `logs/` 总量上限（MB），超限从最旧目录开始删                                                                | `1024`                                                                                             |
| `debug.payload_hours`                            | 大体积阶段文件（03/04/06 与 attachments/）保留小时数，超时剥离负载保留 meta/error 证据                      | `24`                                                                                               |
| `debug.keep_error_dirs`                          | 容量淘汰时保护的最新失败目录数（含 `error.json`）                                                           | `32`                                                                                               |
| `debug.quota_interval_minutes`                   | 配额快照采样间隔 → `logs/quota.jsonl`；`<=0` 不采样                                                         | `5`                                                                                                |
| `dashboard.password`                             | `/panel` 管理面板密码；留空免登录                                                                           | 无                                                                                                 |
| `auth.api_key`                                   | `/v1/*` 接口的访问密钥；留空则不校验。客户端可通过 `Authorization: Bearer <key>` 或 `X-Api-Key: <key>` 传递 | 无（开放）                                                                                         |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    token: "devin-session-token$..."
    model: "glm-5-2"

debug:
    enabled: false

dashboard:
    password: "" # /panel 登录密码；留空免登录

auth:
    # 填入强密码以保护 /v1/*；留空则不校验。
    api_key: ""
```

注意：

- token 等敏感字段在日志中会被脱敏为 `<redacted>`，不会泄露；
- `devin.token` 为空时，`/v1/*` 接口返回 `503 provider_configuration`；
- `config.yaml` 已在 `.gitignore` 中——真实 token 不要入库；pre-commit 挂了 gitleaks 会拦误提交的 secret。

## 文档

- **架构、API 字段子集、proto 提取等技术细节**：[贡献指南](CONTRIBUTING.md)
- **开源协议**：[MIT](LICENSE)

## 致谢

本项目在 [leookun/devin-2api](https://github.com/leookun/devin-2api) 的基础上继续开发，感谢原作者的工作。
