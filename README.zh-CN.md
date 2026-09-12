# devin-2api

> [English](README.md) | **中文**

devin-2api 是一个轻量的 [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses) 转发工具。它对外暴露标准的 `/v1/responses` 接口，通过适配器把你的 LLM 请求透明转发到 Devin（[app.devin.ai](https://app.devin.ai/)）——让外部程序可以通过标准的 OpenAI 协议调用 Devin 内部的模型。

## 特性

- **OpenAI 兼容**的 `/v1/responses` 接口——外部程序可通过标准 OpenAI 协议调用 Devin 模型
- **支持流式与一次性响应**（typed SSE / JSON）
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
# {"status":"ok"}
```

## 用法

> **注意**：`/v1/*` 接口支持可选的 API Key 鉴权。在 `config.yaml` 中设置 `auth.api_key` 后，客户端需通过 `Authorization: Bearer <api_key>` 或 `X-Api-Key: <api_key>` 传递密钥；留空则不校验，请只在可信网络内暴露。

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

请求体遵循 OpenAI Responses API（`input`、`instructions`、`tools`、`stream` 等）。支持的具体字段子集见[贡献指南](CONTRIBUTING.zh-CN.md)。

## 配置

配置文件为 YAML，启动时加载一次；未知字段会被拒绝。

| 字段             | 说明                                                                                                        | 必填                                                                                               |
| ---------------- | ----------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `server.listen`  | HTTP 监听地址                                                                                               | 是                                                                                                 |
| `devin.base_url` | Devin Connect 服务地址                                                                                      | 配置了 `devin.token` 后必填（代码无默认值；`config.example.yaml` 用 `https://server.codeium.com`） |
| `devin.token`    | Devin 会话 token（`devin-session-token$...`）                                                               | 否——未配置时接口返回 503                                                                           |
| `devin.model`    | Devin chat model UID（如 `glm-5-2`）                                                                        | 配置了 `devin.token` 后必填（代码无默认值）                                                        |
| `debug.enabled`  | 在配置文件同目录的 `logs/` 下写按请求的调试日志                                                             | 否                                                                                                 |
| `auth.api_key`   | `/v1/*` 接口的访问密钥；留空则不校验。客户端可通过 `Authorization: Bearer <key>` 或 `X-Api-Key: <key>` 传递 | 否                                                                                                 |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    token: "devin-session-token$..."
    model: "glm-5-2"

debug:
    enabled: false

auth:
    # 填入强密码以保护 /v1/*；留空则不校验。
    api_key: ""
```

注意：

- token 等敏感字段在日志中会被脱敏为 `<redacted>`，不会泄露；
- `devin.token` 为空时，`/v1/responses` 返回 `503 provider_configuration`；
- `config.yaml` 已被 git 追踪——填入真实 token 后不要提交（必要时加入 `.gitignore`）。

## 文档

- **架构、API 字段子集、proto 提取等技术细节**：[贡献指南](CONTRIBUTING.zh-CN.md)
- **开源协议**：[MIT](LICENSE)
