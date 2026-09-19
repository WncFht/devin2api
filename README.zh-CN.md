# devin-2api

> [English](README.md) | **中文**

devin-2api 是一个非官方协议适配器，把你 Devin 账号（[app.devin.ai](https://app.devin.ai/)）可用的模型包装在 OpenAI / Anthropic 兼容接口后面——让标准客户端（Codex、Claude Code、任意 SDK）通过熟悉的 API 调用它们。

> **声明**：本项目与 Cognition 无任何关联、未获其背书。它使用你自己的 Devin 会话 token 调用内部 RPC 接口，仅供个人账号自用；请自行遵守 Devin 的服务条款。

## 特性

- **一个上游，三个 API 面**——`POST /v1/responses`（OpenAI Responses，含 Codex 式客户端的 WebSocket transport 与多轮会话）、`POST /v1/chat/completions`（OpenAI Chat）、`POST /v1/messages`（Anthropic Messages）
- **支持流式与一次性响应**（typed SSE / JSON）
- **思考签名跨轮回放**——按各 provider 原生形态保存并回传：Responses 面落成 `encrypted_content` reasoning item，Anthropic 面落成 `redacted_thinking`，Chat 面落成 `reasoning_content`
- **工具调用**——custom/freeform 工具调用（如 `apply_patch`）原文往返；工具名与 `tool_choice` 本地校验；按上游强制的 call↔result 交错序重新配对
- **上游流恢复**——token 过期自动从凭据来源重读；产出内容前的上游失败（传输断裂、静默卡死、空回复）透明重试；早期失败返回真实 HTTP 错误，而不是已提交 200 后的 SSE error
- **限流闸门**——上游 `resource_exhausted` 触发本地冷却闩：排队请求短暂等待后快速失败 `429` + `Retry-After`，不再捶打已被限流的上游；闩内按滴灌节奏放探针探测恢复；闩状态持久化在 `devin-2api.db`，重启后未过期自动恢复。可选 `max_rpm` 令牌桶在触闩前先行整形出站压力
- **可选前缀保温**——按节拍重放保留谱系的最近请求体给上游 prompt cache 续期，subagent 长等结束后恢复轮次不再吃冷 prefill（`devin.warm_prefix_*`，默认关闭；机制见 `docs/upstream-cache.md`）
- **归一化错误契约**——上游错误码映射为正确的 HTTP 状态与各协议错误类型；限流归一为 `429` + `Retry-After`；请求日志开启时（`debug.enabled`，随仓库示例配置默认开启）每个请求带 `X-Request-Id`/`debug_ref` 直指其调试记录
- **`/v1/models` 能力位透出**——上下文窗口、工具/thinking/图片支持等来自上游模型目录
- **`/web` 管理面板**——请求浏览、用量/成本聚合、配额追踪、进程指标、按请求调试 payload，以及多数字段可热加载的脱敏配置视图
- **单一静态二进制**——[GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api) 公开镜像

## 快速开始

### 1. 提供 Devin 凭据

devin-2api 按 `devin.accounts` 里每条声明一个上游账号向上游鉴权——单号部署就是只写一条的池，空池同样合法：先起服务，事后在面板 `/web/accounts.html` 加号即可，免重启。每条账号给三种凭据来源的至少一种（可叠加）：

- **`api_key`（推荐）**——Devin 平台 durable key（`cog_...`），在 [app.devin.ai](https://app.devin.ai/) → Settings → API keys 手工签发。它本身不带过期语义（签发后一直有效，撤销才失效）：上游回 `unauthenticated` 时 lane 用它现场铸一枚新 session token，只配 `api_key` 的号可以无限自愈、零维护。
- **`token`**——字面量 Devin 会话 token（`devin-session-token$...`）。直接可用，但 session token 的寿命由服务端管——死后 lane 只能靠其它来源拿到新凭据才能恢复。从哪拿：`devin auth login` 之后它就是 `credentials.toml` 里的 `windsurf_api_key` 值（见下），或从 Devin 应用本地状态提取（macOS 命令在下方）。
- **`credentials_file`**——指向 Devin CLI 凭证文件（macOS/Linux 为 `~/.local/share/devin/credentials.toml`；Windows 为 `%APPDATA%\devin\credentials.toml`），里面存 CLI 自己登录续期的 session token——重读文件即自动跟随 CLI 续期。`devin` CLI 随 [Devin 桌面端](https://devin.ai/download)（即 Windsurf app）内置，位于 `resources/app/extensions/windsurf/devin/bin/` 下——Windows 为 `C:\Program Files\Windsurf\`、Linux `.deb` 包为 `/usr/share/devin-desktop/`——执行 `devin auth login` 完成浏览器登录即生成该文件。

同一账号可叠加来源——例如 `api_key` + `token`：字面 token 先服役，死后由 durable key 铸新 token 顶上。

macOS 下也可从 Devin 应用本地状态提取 session token：

```bash
sqlite3 ~/Library/"Application Support"/Devin/User/globalStorage/state.vscdb \
  "SELECT json_extract(value, '$.apiKey') FROM ItemTable WHERE key='windsurfAuthStatus';"
```

上游回 `unauthenticated` 时 lane 按序重解该号凭据——`credentials_file` 重读（跟随 CLI 续期）→ 行/config 的 token 有变化则换上 → `api_key` 铸新——除裸字面 token 外每种来源都能让代理免重启自愈。

### 2. 配置

```bash
cp config.example.yaml config.yaml
```

编辑 `config.yaml`，在 `devin.accounts` 下声明你的账号（基于 `config.example.yaml` 起步时只需一条 `{name, token}`——base_url 和 model 已有示例值）。

### 3. 启动

预编译二进制（见 [Releases](https://github.com/WncFht/devin2api/releases)，附 `checksums.txt` 可校验）。资产命名 `devin-2api-{darwin,linux}-{amd64,arm64}`；Windows 为同名 `.zip` 包（内含 exe + `config.example.yaml` + LICENSE）：

```bash
# Linux 示例；macOS 换成 devin-2api-darwin-arm64 或 -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing   # 期望输出：devin-2api-linux-amd64: OK
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

Windows：解压 `devin-2api-windows-amd64.zip`，编辑 `config.yaml`（账号可只给 `credentials_file`——第 1 节让 Windsurf 内嵌的 `devin.exe` 产出凭证文件），在控制台运行 `devin-2api.exe -config config.yaml`。Ctrl+C 触发优雅排空；关窗和 `taskkill /F` 不走排空——Windows 对控制台进程只有强杀路径。

源码运行（生成的 proto 绑定已提交在 `outputs/devin-proto-go`，clone 后可直接构建，无需工具链）：

```bash
go run ./cmd/devin-2api -config config.yaml
```

Docker（镜像已发布至 [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)）：

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  -v devin2api-state:/app/state \
  -e DEVIN2API_STATE_DIR=/app/state \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

状态卷让 `devin-2api.db`（下游令牌仓、请求日志、配额快照）跨容器重启存活——不挂卷则每次运行从空仓起步，即 `/v1` 开放访问。

以服务方式运行（可选）：

| 平台    | 托管方式                                 | 布局                                                                                                      | 安装 / 升级                  |
| ------- | ---------------------------------------- | --------------------------------------------------------------------------------------------------------- | ---------------------------- |
| macOS   | launchd 代理                             | 二进制 `~/.local/bin` · 配置 + 状态 `~/Library/Application Support/devin-2api`                            | `scripts/deploy.sh`          |
| Linux   | `systemd --user`                         | 二进制 `~/.local/bin` · 配置 `~/.config/devin-2api` · 状态 `~/.local/state/devin-2api`                    | `scripts/deploy-linux.sh`    |
| Windows | 无——控制台运行，或用 NSSM / 任务计划程序 | exe `%LOCALAPPDATA%\Programs\devin-2api` · 配置 `%APPDATA%\devin-2api` · 状态 `%LOCALAPPDATA%\devin-2api` | `scripts/deploy-windows.ps1` |

二进制按平台惯例解析路径：配置走 `-config` flag → `DEVIN2API_CONFIG` → `./config.yaml` → 上表平台默认；状态目录走 `-state-dir` → `DEVIN2API_STATE_DIR` → 平台默认。两个部署脚本都是「首装与升级同一条命令」：`--release latest` 拉预编译二进制，装完轮询 `/healthz` 确认新版本接管，再打一发 `GET /v1/models` 验证上游鉴权真的通了。脚本以仓库为家——同步 `config.yaml` 进平台配置目录、在仓库内维护指向状态目录的 `logs` 符号链接，所以先 clone 再跑：

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
bash scripts/deploy-linux.sh --release latest    # macOS 用 scripts/deploy.sh
```

首跑时 `config.yaml` 会自动从 `config.example.yaml` 生成（写入随机 `dashboard.password`，并提示粘贴 Devin token——留空则空池起跑，事后在面板加号；下游 /v1 令牌在面板 `/web/tokens.html` 创建，不入配置）；想提前定制可先 `cp config.example.yaml config.yaml` 手动编辑。`--check` 对比已安装/运行中/最新版本，`--uninstall` 移除服务与二进制（保留 config 与日志）。

Linux 下若需要未登录也常驻，执行 `loginctl enable-linger $USER`。

### 4. 验证

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}
```

## 用法

> **注意**：`/v1/*` 接口由令牌仓（状态目录 `devin-2api.db` 的 `auth_tokens` 表）统一准入——客户端需通过 `Authorization: Bearer <token>` 或 `X-Api-Key: <token>` 传递命中有效令牌行的凭据。令牌只在面板（`/web/tokens.html`）管理——明文创建时一次性出示，仓内只存哈希，配置里不放数据面凭据。仓为空时接口开放——监听到非 loopback 地址前务必确认仓内有有效令牌，否则等于把你的 Devin 配额开放给整个网络。令牌可带 `class`（`fg` 默认 / `bg` 无人值守批跑），改变速率闸门准入口径——见 `docs/gate-classes.md`。

接口列表：

- `POST /v1/responses`——OpenAI Responses（同路径 `GET` 可协商 WebSocket transport）
- `POST /v1/chat/completions`——OpenAI Chat Completions
- `POST /v1/messages`——Anthropic Messages
- `GET /v1/models`、`GET /v1/models/{model}`——上游模型目录与能力位
- `GET /web/*`——管理面板（请求浏览、用量、配额、进程指标）；`dashboard.password` 保护

本代理是**无状态**的：每个 HTTP 请求都要携带完整对话历史——`previous_response_id` 会被显式拒绝而不是静默丢上下文（没有服务端响应存储，响应如实上报 `store=false`）。WebSocket transport 下由会话状态机按连接维护多轮上下文，增量 input 会被透明展开为完整 transcript。

使用你的 OpenAI Responses API 客户端调用 `http://localhost:8080/v1/responses` 即可。

一次性响应：

```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "swe-2-max",
    "input": "你好"
  }'
```

流式响应（SSE）：

```bash
curl -N http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "swe-2-max",
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
    "model": "swe-2-max",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "你好"}]
  }'
```

各协议面支持的字段子集见[贡献指南](CONTRIBUTING.md)。

## 配置

配置文件为 YAML，启动时加载一次；未知字段会被拒绝。

| 字段                                             | 说明                                                                                                                                                                                                                                                       | 必填 / 默认值                                                               |
| ------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------- |
| `server.listen`                                  | HTTP 监听地址                                                                                                                                                                                                                                              | 是                                                                          |
| `server.max_concurrency`                         | `/v1/*` 并发请求上限                                                                                                                                                                                                                                       | `1024`                                                                      |
| `devin.base_url`                                 | Devin Connect 服务地址                                                                                                                                                                                                                                     | 必填（代码无默认值；`config.example.yaml` 用 `https://server.codeium.com`） |
| `devin.accounts`                                 | 上游账号池条目 `{name, token, credentials_file, api_key, priority, max_rpm}`——`token`/`credentials_file`/`api_key` 至少给一个；`priority` 排选号序（0 为默认），`max_rpm` 覆盖该号速率上限；空列表是合法空池（账号可在面板 `/web/accounts.html` 在线管理） | 否——无账号时 `/v1` 返回 `unavailable`                                       |
| `devin.model`                                    | Devin chat model UID（如 `swe-2-max`）                                                                                                                                                                                                                     | 必填（代码无默认值）                                                        |
| `devin.aliases`                                  | 客户端模型名 → 上游真实 UID 映射（如 `swe-2: swe-2-max`）；匹配顺序：精确 → 大小写不敏感 → `"*"` 兜底；别名列进 `/v1/models` 并带 `alias_of`                                                                                                               | 无                                                                          |
| `devin.client_name`/`client_version`/`client_os` | 发给上游 metadata 的客户端身份                                                                                                                                                                                                                             | `chisel` / `3000.2.17` / `mac`                                              |
| `devin.proxy`                                    | 上游代理地址（`http(s)://`、`socks5(h)://`）；留空直连或走环境变量                                                                                                                                                                                         | 无                                                                          |
| `devin.force_http1`                              | 每请求独立 TCP 连上游（避免 HTTP/2 单连接多 stream 串行化）                                                                                                                                                                                                | `true`                                                                      |
| `devin.max_rpm`                                  | 发往上游的消息速率上限（条/分钟，令牌桶）；`<=0` 不限速——429 冷却闩始终生效                                                                                                                                                                                | `0`（不限速；`config.example.yaml` 发货 `80`）                              |
| `devin.gate_max_hold_seconds`                    | 闩外排队允许的最长等待秒数——睡到下一可发窗口的预计等待超预算即快速失败 `429` + `Retry-After`（闩内不排队，按滴灌槽距离快败）                                                                                                                               | `30`                                                                        |
| `devin.gate_drip_interval_seconds`               | 闩内放行探针的间隔秒数——决定限流期间打到上游的速率与解闩探测频率                                                                                                                                                                                           | `8`                                                                         |
| `devin.gate_default_latch_seconds`               | 上游 `resource_exhausted` 未声明 reset 时刻时的兜底闩时长秒数                                                                                                                                                                                              | `60`                                                                        |
| `devin.gate_window_offset_seconds`               | 上游分钟桶界在本地分钟内的估计位置（第几秒）                                                                                                                                                                                                               | `0`（本地 `:00`；实测桶界在本地 `:59` 附近）                                |
| `devin.gate_window_guard_seconds`                | 估计桶界两侧的停发死区秒数——死区内请求睡到下一窗口开放                                                                                                                                                                                                     | `2`                                                                         |
| `devin.gate_bg_max_hold_seconds`                 | `bg` 类令牌的最长闸内排队秒数——无人值守批跑等得起（fg 仍走 `gate_max_hold_seconds`）；语义见 `docs/gate-classes.md`                                                                                                                                        | `120`                                                                       |
| `devin.gate_bg_reserve_margin`                   | bg 准入动态预留公式中的固定安全边际（条）——桶尾 `reserve` 个槽对 bg 不可达，fg 恒有保底余量                                                                                                                                                                | `4`                                                                         |
| `devin.warm_prefix_*`                            | 前缀保温一族键——按节拍重放保留谱系的最近请求体给上游 prompt cache 续期，压住 subagent 长等后恢复轮次的冷 prefill；键清单见 `config.example.yaml`，机制见 `docs/upstream-cache.md`                                                                          | `warm_prefix_enabled: false`                                                |
| `devin.session_affinity_ttl_seconds`             | 会话→lane 绑定的滑动 TTL（每次命中续期）；`X-Claude-Code-Session-Id`/`X-Session-ID`/`X-Session-Affinity`/`X-Conversation-Id`/`X-Thread-Id` 头把会话钉到 lane                                                                                               | `3600`                                                                      |
| `devin.quota_low_threshold_percent`              | 周配额低于该百分比时，新会话选号把该 lane 降到健康 lane 之后（已绑定会话不受影响）                                                                                                                                                                         | `15`                                                                        |
| `debug.enabled`                                  | 按请求记录调试 payload 进 `devin-2api.db`（`debug_files`/`debug_chunks` 表，状态目录）                                                                                                                                                                     | `false`                                                                     |
| `debug.retention_days`                           | 按请求调试记录保留天数；`<=0` 不按时间清理                                                                                                                                                                                                                 | `14`                                                                        |
| `debug.max_total_mb`                             | 调试 payload 总量上限（MB），超限从最旧请求组开始删                                                                                                                                                                                                        | `1024`                                                                      |
| `debug.payload_hours`                            | 大体积阶段记录（03/04/06 与 attachments/ 名下）保留小时数，超时剥离负载保留 meta/error 证据                                                                                                                                                                | `24`                                                                        |
| `debug.keep_error_dirs`                          | 容量淘汰时保护的最新失败请求组数（含 `error.json` 行）                                                                                                                                                                                                     | `32`                                                                        |
| `debug.errors_only`                              | 只保留失败或可疑请求的调试 payload——干净完成的请求完结即剥 payload（meta/error 锚点与 `logs` 行照常保留）；面板键 `debug_log_errors_only`，可热改                                                                                                          | `false`                                                                     |
| `debug.quota_interval_minutes`                   | 配额快照采样间隔 → `quota_samples` 表；`<=0` 不采样                                                                                                                                                                                                        | `5`                                                                         |
| `debug.pprof_listen`                             | pprof/fgprof 剖析端点的独立监听地址（如 `127.0.0.1:6060`）；端点无鉴权——只绑回环地址                                                                                                                                                                       | 空（不启用）                                                                |
| `dashboard.password`                             | `/web` 管理面板密码；留空免登录                                                                                                                                                                                                                            | 无                                                                          |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    accounts:
        - name: "main"
          token: "devin-session-token$..."
    model: "swe-2-max"

debug:
    enabled: false

dashboard:
    password: "" # /web 登录密码；留空免登录


# 下游 /v1 令牌只在面板（/web/tokens.html）管理——配置里不放数据面凭据。
# 令牌仓为空即 /v1 开放访问。
```

注意：

- token 等敏感字段在日志中会被脱敏为 `<redacted>`，不会泄露；
- 不配任何账号即空池：`/v1` 请求快速失败返回 `unavailable`——面板 `/web/accounts.html` 加号或 `devin.accounts` 声明后 reload 即恢复，均免重启；
- `config.yaml` 已在 `.gitignore` 中——真实 token 不要入库；pre-commit 挂了 gitleaks 会拦误提交的 secret。

## 排障

每个 `/v1/*` 响应带 `X-Request-Id` 头（错误体另带 `debug_ref`），值即该请求的调试记录名——`GET /admin/debug-logs/{id}/file/error.json` 给首个失败点，状态目录 `logs/stderr.log` 每请求一行摘要。常见症状：

- **`401`**——令牌仓无匹配令牌（在 `/web/tokens.html` 创建），或上游判死该号凭据；lane 会重解凭据并重试一次——在 `/web/accounts.html` 看 lane 状态
- **`unavailable`**——账号池为空；在 `/web/accounts.html` 加号，或 `devin.accounts` 声明后 config reload
- **`429`**——本地速率闸门快败（`Retry-After` 给重试时刻）或上游真限流；`logs` 表 `error_stage` 列（`rate_gate` vs `devin_connect`）区分两者
- **SSE 在 ~300s 附近被掐**——客户端自己的总超时，不是代理的；断开的流在服务端续跑，同请求重试会从完成缓存重放

完整的症状 → 层 → 处置速查表见 [`docs/upstream-debug-playbook.md`](docs/upstream-debug-playbook.md)。

## 文档

- **架构、API 字段子集、proto 提取等技术细节**：[贡献指南](CONTRIBUTING.md)
- **上游协议逆向、客户端接入、排障手册、部署与工具链**：[docs/](docs/README.md)
- **开源协议**：[MIT](LICENSE)

## 致谢

本项目在 [leookun/devin-2api](https://github.com/leookun/devin-2api) 的基础上继续开发，感谢原作者的工作。
