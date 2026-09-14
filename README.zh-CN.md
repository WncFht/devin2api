# devin-2api

> [English](README.md) | **中文**

devin-2api 是一个非官方协议适配器，把你 Devin 账号（[app.devin.ai](https://app.devin.ai/)）可用的模型包装在 OpenAI / Anthropic 兼容接口后面——让标准客户端（Codex、Claude Code、任意 SDK）通过熟悉的 API 调用它们。

> **声明**：本项目与 Cognition 无任何关联、未获其背书。它使用你自己的 Devin 会话 token 调用内部 RPC 接口，仅供个人账号自用；请自行遵守 Devin 的服务条款。

## 特性

- **一个上游，三个 API 面**——`POST /v1/responses`（OpenAI Responses，含 Codex 式客户端的 WebSocket transport 与多轮会话）、`POST /v1/chat/completions`（OpenAI Chat）、`POST /v1/messages`（Anthropic Messages）
- **支持流式与一次性响应**（typed SSE / JSON）
- **思考签名跨轮回放**——按各 provider 原生形态保存并回传：Responses 面落成 `encrypted_content` reasoning item，Anthropic 面落成 `redacted_thinking`，Chat 面落成 `reasoning_content`
- **忠实的工具调用**——custom/freeform 工具调用（如 `apply_patch`）原文往返；工具名与 `tool_choice` 本地校验；按上游强制的 call↔result 交错序重新配对
- **上游流韧性**——token 过期自动从凭据来源重读；产出内容前的上游失败（传输断裂、静默卡死、空回复）透明重试；早期失败返回真实 HTTP 错误，而不是已提交 200 后的 SSE error
- **限流闸门**——上游 `resource_exhausted` 触发本地冷却闩：排队请求短暂等待后快速失败 `429` + `Retry-After`，不再捶打已被限流的上游；闩内按滴灌节奏放探针探测恢复；闩状态落盘 `logs/gate-state.json`，重启后未过期自动恢复。可选 `max_rpm` 令牌桶在触闩前先行整形出站压力
- **归一化错误契约**——上游错误码映射为正确的 HTTP 状态与各协议错误类型；限流归一为 `429` + `Retry-After`；每个请求带 `X-Request-Id`/`debug_ref` 直指调试目录
- **`/v1/models` 能力位透出**——上下文窗口、工具/thinking/图片支持等来自上游模型目录
- **`/panel` 管理面板**——请求浏览、用量/成本聚合、配额追踪、进程指标、按请求调试目录，以及多数字段可热加载的脱敏配置视图
- **部署简单**——单一静态二进制，[GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api) 公开镜像

## 快速开始

### 1. 提供 Devin token

devin-2api 使用你的 Devin 会话 token（`devin-session-token$...`）向上游鉴权。`config.yaml` 里 `devin.token` 留空时按顺序自动发现：

1. `DEVIN_TOKEN` 或 `WINDSURF_API_KEY` 环境变量；
2. Devin CLI 凭证文件——macOS/Linux 为 `~/.local/share/devin/credentials.toml`；Windows 为 `%APPDATA%\devin\credentials.toml`（其次 `%LOCALAPPDATA%\devin\credentials.toml`）。Windows 版 CLI 不单独发行，但随 [Windsurf 桌面端](https://devin.ai/download)（即 Devin app）内置：安装后执行 `& "C:\Program Files\Windsurf\resources\app\extensions\windsurf\devin\bin\devin.exe" auth login` 即生成该文件。

macOS 下也可从 Devin 应用本地状态提取：

```bash
sqlite3 ~/Library/"Application Support"/Devin/User/globalStorage/state.vscdb \
  "SELECT json_extract(value, '$.apiKey') FROM ItemTable WHERE key='windsurfAuthStatus';"
```

token 会过期。上游回 `unauthenticated` 时适配器会重读同一条来源链——Devin CLI 续期改写 `credentials.toml` 后，代理无需重启即自愈。

### 2. 配置

```bash
cp config.example.yaml config.yaml
```

编辑 `config.yaml`，填入你的 token（基于 `config.example.yaml` 起步时只需填 `devin.token`——base_url 和 model 已有示例值）。

### 3. 启动

预编译二进制（见 [Releases](https://github.com/WncFht/devin2api/releases)，附 `checksums.txt` 可校验）。资产命名 `devin-2api-{darwin,linux}-{amd64,arm64}`；Windows 为同名 `.zip` 包（内含 exe + `config.example.yaml` + LICENSE）：

```bash
# Linux 示例；macOS 换成 devin-2api-darwin-arm64 或 -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

Windows：解压 `devin-2api-windows-amd64.zip`，编辑 `config.yaml`（token 可留空——第 1 节第 2 条让 Windsurf 内嵌的 `devin.exe` 产出凭证文件），在控制台运行 `devin-2api.exe -config config.yaml`。Ctrl+C 触发优雅排空；关窗和 `taskkill /F` 不走排空——Windows 对控制台进程只有强杀路径。

源码运行（生成的 proto 绑定已提交在 `outputs/devin-proto-go`，clone 后可直接构建，无需工具链）：

```bash
go run ./cmd/devin-2api -config config.yaml
```

Docker（镜像已发布至 [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)）：

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

以服务方式运行（可选）：

| 平台    | 托管方式                                 | 运行目录（二进制 + 配置 + 日志）           | 安装 / 升级                  |
| ------- | ---------------------------------------- | ------------------------------------------ | ---------------------------- |
| macOS   | launchd 代理                             | `~/Library/Application Support/devin-2api` | `scripts/deploy.sh`          |
| Linux   | `systemd --user`                         | `~/.local/share/devin-2api`                | `scripts/deploy-linux.sh`    |
| Windows | 无——控制台运行，或用 NSSM / 任务计划程序 | `%LOCALAPPDATA%\Programs\devin-2api`       | `scripts/deploy-windows.ps1` |

两个部署脚本都是「首装与升级同一条命令」：`--release latest` 拉预编译二进制，装完轮询 `/healthz` 确认新版本接管，再打一发 `GET /v1/models` 验证上游鉴权真的通了。脚本以仓库为家——同步 `config.yaml` 进运行目录、在仓库内维护指向运行目录日志的 `logs` 符号链接，所以先 clone 再跑：

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
bash scripts/deploy-linux.sh --release latest    # macOS 用 scripts/deploy.sh
```

首跑时 `config.yaml` 会自动从 `config.example.yaml` 生成（写入随机 `auth.api_key`/`dashboard.password`，并提示粘贴 Devin token——留空走自动发现）；想提前定制可先 `cp config.example.yaml config.yaml` 手动编辑。`--check` 对比已安装/运行中/最新版本，`--uninstall` 移除服务与二进制（保留 config 与日志）。

Linux 下若需要未登录也常驻，执行 `loginctl enable-linger $USER`。

### 4. 验证

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}
```

## 用法

> **注意**：`/v1/*` 接口支持可选的 API Key 鉴权。在 `config.yaml` 中设置 `auth.api_key` 后，客户端需通过 `Authorization: Bearer <api_key>` 或 `X-Api-Key: <api_key>` 传递密钥；留空则不校验——监听到非 loopback 地址前务必先设密钥，否则等于把你的 Devin 配额开放给整个网络。

接口列表：

- `POST /v1/responses`——OpenAI Responses（同路径 `GET` 可协商 WebSocket transport）
- `POST /v1/chat/completions`——OpenAI Chat Completions
- `POST /v1/messages`——Anthropic Messages
- `GET /v1/models`、`GET /v1/models/{model}`——上游模型目录与能力位
- `GET /panel`——管理面板（请求浏览、用量、配额、进程指标）；`dashboard.password` 保护

本代理是**无状态**的：每个 HTTP 请求都要携带完整对话历史（`previous_response_id` 会被解析但忽略——没有服务端响应存储）。WebSocket transport 下由会话状态机按连接维护多轮上下文，增量 input 会被透明展开为完整 transcript。

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
| `devin.token`                                    | Devin 会话 token（`devin-session-token$...`）；留空则从环境变量 / 凭据文件自动发现                          | 否——未配置时接口返回 503                                                                           |
| `devin.model`                                    | Devin chat model UID（如 `glm-5-2`）                                                                        | 配置了 `devin.token` 后必填（代码无默认值）                                                        |
| `devin.aliases`                                  | 客户端模型名 → 上游真实 UID 映射（如 `swe-2: swe-2-max`）                                                   | 无                                                                                                 |
| `devin.client_name`/`client_version`/`client_os` | 发给上游 metadata 的客户端身份                                                                              | `chisel` / `3000.2.17` / `mac`                                                                     |
| `devin.proxy`                                    | 上游代理地址（`http(s)://`、`socks5(h)://`）；留空直连或走环境变量                                          | 无                                                                                                 |
| `devin.force_http1`                              | 每请求独立 TCP 连上游（避免 HTTP/2 单连接多 stream 串行化）                                                 | `true`                                                                                             |
| `devin.max_rpm`                                  | 发往上游的消息速率上限（条/分钟，令牌桶）；`<=0` 不限速——429 冷却闩始终生效                                 | `0`（不限速；`config.example.yaml` 发货 `80`）                                                     |
| `devin.gate_max_hold_seconds`                    | 闩内排队允许的最长等待秒数，超出快速失败 `429` + `Retry-After`                                              | `15`                                                                                               |
| `devin.gate_drip_interval_seconds`               | 闩内放行探针的间隔秒数——决定限流期间打到上游的速率与解闩探测频率                                            | `8`                                                                                                |
| `devin.gate_default_latch_seconds`               | 上游 `resource_exhausted` 未声明 reset 时刻时的兜底闩时长秒数                                               | `60`                                                                                               |
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
- **上游协议逆向、客户端接入、排障手册、部署与工具链**：[docs/](docs/README.md)
- **开源协议**：[MIT](LICENSE)

## 致谢

本项目在 [leookun/devin-2api](https://github.com/leookun/devin-2api) 的基础上继续开发，感谢原作者的工作。
