# 贡献指南

欢迎为 `devin-2api` 贡献代码。本指南说明项目架构、开发环境，以及如何提交变更。

> [English](CONTRIBUTING.md) | **中文**

## 项目定位（请先读这段）

`devin-2api` 是**协议适配器**：对外是 OpenAI Responses API，对内是 Devin Connect RPC，中间层用一套供应商无关的模型（`internal/llm`）隔离两侧——新增上游只需实现适配器接口，无需改动 HTTP 层。保证 agent loop 语义等价（而非 Provider 请求结构等价）。

任何改动都应尊重这条边界：上游不应该绕过 `llm` 中间层，HTTP 协议与任何上游协议之间都不能直接映射：

```text
OpenAI Responses HTTP ──► llm 中间层 ──► Devin Connect RPC
   (编解码)                (语义模型)      (适配器)
```

- HTTP 编解码只理解 OpenAI 协议，不关心 Devin；
- `internal/llm` 是唯一语义模型，两侧只做语义转换，不透传结构；
- 适配器只翻译 `internal/llm` ⇄ 上游协议，不掺入 HTTP 层的知识。

**什么是最佳上游？无状态。** 理想的上游不保留会话状态——每个请求自包含完整对话。Devin 目前满足这一条件：每次 `GetChatMessage` 都携带完整可重放的上下文，网关因此可水平扩展、可安全重放。

## 架构与数据流

```text
                 ┌────────────────────────────────────────────────────┐
                 │                  devin-2api                        │
  HTTP client    │                                                    │    upstream
 ─────────────►  │  /v1/responses                                    │  ┌──────────────────┐
   Responses     │   │                                                │  │ Devin Connect     │
   JSON / SSE    │   ▼                                                │  │ (server.codeium   │
                 │  responses.DecodeRequest ──► llm.RequestMessages  │  │  .com)            │
                 │        │                                            │  │                   │
                 │        ▼                                            │  │ GetChatMessage    │
                 │  adapter.Stream(ctx, RequestMessages) ────────────►│  │ (Connect, proto)  │
                 │        │                                            │  └──────────────────┘
                 │        ▼                                            │
                 │  llm.ResponseStream (事件流)                         │
                 │        │                                            │
                 │        ├─ 流式:  writeSSE + StreamEncoder ──► SSE  │
                 │        └─ 非流式: collectFinalMessage ──► JSON     │
                 └────────────────────────────────────────────────────┘
```

一次请求的完整链路：

1. `POST /v1/responses` 收到 OpenAI Responses JSON（限制 8 MiB）；
2. `responses.DecodeRequest` 把请求转成 `llm.RequestMessages`（系统提示、消息历史、工具定义）+ 生成选项；
3. `adapter.Stream` 把供应商无关的请求上下文交给已配置的适配器，返回 `llm.ResponseStream`；
4. Devin 适配器把中间模型翻译成 `GetChatMessageRequest`（protobuf），通过 Connect 流式读取上游帧，再由 `responseDecoder` 把每个帧解释为零个或多个 `llm.ResponseEvent`；
5. 按请求的 `stream` 选项分流：
   - **流式**：`StreamEncoder` 把事件展开为 typed SSE（`response.output_text.delta` 等）；
   - **非流式**：收集 `done`/`error` 事件聚合出最终 `AssistantMessage`，编码为 Responses JSON。

### 中间层模型（internal/llm）

所有适配器共享的语义模型，位于 `internal/llm`：

| 概念 | 说明 |
| --- | --- |
| `RequestMessages` | 完整请求上下文：`SystemPrompt` + 按时间排序的 `Messages` + `Tools` |
| `Message` | `UserMessage` / `AssistantMessage` / `ToolResultMessage`（角色由 `Role()` 决定） |
| `Content` | 内容块：`TextContent` / `ThinkingContent` / `ImageContent` / `ToolCall` |
| `ToolDefinition` | 工具名 + 描述 + JSON Schema 输入 |
| `ResponseEvent` | 增量事件（`start`、`text_delta`、`toolcall_*`、`done`、`error` 等 12 种） |
| `AssistantMessage` | 最终聚合消息，含 `Usage`、`StopReason`、供应商元信息 |

消息历史是**可跨供应商重放的完整对话**：思考签名、工具调用 ID、用量等字段在设计上就是为了原样送进下一轮请求（详见 `request.go` 中 `TextSignature`、`ThinkingSignature` 等字段的注释）。

## 开发环境

- Go 1.26.3（见 `go.mod`）
- [Task](https://taskfile.dev/)：proto → Go 绑定生成入口（`Taskfile.yml`）
- 生成 Go 绑定需要 `protoc` + `protoc-gen-go` + `protoc-gen-connect-go`（版本见 `Taskfile.yml`）：

```bash
brew install protobuf
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.19.1
```

## 常用命令

```bash
# 生成 proto Go 绑定（clone 后必须先执行，生成代码不追踪进 git）
task generate

# 运行全部测试
go test ./...

# 本地启动（需先准备 config.yaml，见 README.zh-CN.md「快速开始」）
go run ./cmd/devin-2api -config config.yaml
```

## API 支持范围

`/v1/responses` 支持 OpenAI Responses API 的子集：

- `input` 可以是纯字符串，也可以是 input item 数组（`message`、`function_call`、`function_call_output`）；
- `content` 支持字符串或 part 数组（`input_text`、`output_text`、`text`、`input_image` 的 base64 data URL）；
- 生成选项：`instructions`、`tools`（function 类型 + JSON Schema `parameters`）、`stream`；
- `stream: false` 返回完整 JSON Response；`stream: true` 返回 typed SSE（`response.created`、`response.output_item.added`、`response.output_text.delta`、`response.completed` 等）。

### 工具定义传递

中间层工具会转换为 Devin 原生 function 工具（保留名称与 JSON Schema 约束，但会剥离 `description`/`title` 等自然语言注解——上游可能把它们误当作分类提示）；非空的工具描述还会以 `<tool name="...">` 块注入系统提示，让模型理解工具用途。详见 `internal/adapter/devin/tool_definition.go`。

## 调试日志

`debug.enabled: true` 时，每次请求会在配置文件同目录的 `logs/<进入时刻>/` 下生成一份分阶段日志，便于定位「HTTP ⇄ 中间层 ⇄ 上游」任一环节的问题：

```text
meta.json                  # 请求结果摘要（状态码、模型、耗时）
01-http-request.json       # 原始 HTTP 请求（脱敏）
02-request-messages.json   # 转换后的中间请求上下文
03-devin-request.json      # 发给上游的 proto 请求（JSON 化）
04-devin-response.jsonl    # 上游原始响应帧
05-response-events.jsonl   # 中间层响应事件
06-http-response.jsonl     # 最终写回客户端的响应/SSE 事件
error.json                 # 首个失败阶段与错误信息
attachments/               # 外置的图片附件（重复附件按 SHA-256 去重）
```

同一秒内并发请求使用递增后缀目录区分。

## 提交前检查

1. **测试通过**：`go test ./...`
2. **格式正确**：`gofmt -l .` 无输出
3. **注释规范**：遵守仓库的 Go 注释约定（`.agent/skills/go-comment-conventions`）——导出符号有说明注释，字段注释说明「为什么」，而非复述代码表面行为
4. **没有真实 token**：`config.yaml` 已被 git 追踪，确认提交内容不包含真实的 `devin.token`（必要时加入 `.gitignore`）

## 提交流程

1. Fork 仓库，从 `main` 创建功能分支（如 `fix/sse-close`、`feat/stream-options`）；
2. 每次提交只做一件事，commit message 用祈使句说明「做了什么、为什么」；
3. 如果改动会改变协议语义或适配器行为，同步更新 README 与测试；
4. 发起 Pull Request，在描述中说明：
   - 变更目的与验证方式；
   - 对「HTTP ⇄ 中间层 ⇄ 上游」哪一层有影响；
   - 是否涉及协议升级（上游描述符变化，见下文）。

## 发布版本

发布遵循 [SemVer](https://semver.org/)。项目处于 0.x 阶段，breaking change 只升 minor（`v0.1.0` → `v0.2.0`），不升 major。

发布即打一个 `v` 前缀的 tag。用 `gh release create` 给当前 `HEAD` 打 tag 并推送，同时创建带自动 release notes 的 GitHub Release 页面——这会触发 `release.yml` workflow（先跑完整测试，再构建并推送 Docker 镜像到 Docker Hub `devinuser123/devin-2api`，amd64 + arm64）：

```bash
gh release create v0.1.0 --generate-notes
```

注意：

- workflow 会给镜像打 `0.1.0`、`0.1`、`0`、`latest` 四个 tag（预发布版本如 `v0.2.0-rc.1` 会跳过 `latest`）；
- workflow 读取 `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` 两个 Repository secrets——维护者需配置一次（token 在 [hub.docker.com/settings/security](https://hub.docker.com/settings/security) 创建，不是登录密码）；
- tag 一旦推送视为不可变；发坏的版本通过发布新版本号修复，不要重写 tag。

## 更新上游协议（proto 提取）

完整链路由 `Taskfile.yml` 固化，分两步：

```text
task extract BINARY=<上游二进制>   ① 二进制 → outputs/devin-proto/    （不可复现，依赖抓包）
task generate                      ② proto → outputs/devin-proto-go/（可复现，标准工具链）
```

### ① protoextract：二进制 → proto

从编译后的二进制中扫描内嵌的 `FileDescriptorProto`，重建 .proto 源码，用于在没有原始 .proto 文件的情况下还原上游协议（本项目用它提取了 Devin 的 63 个描述符，见 `outputs/devin-proto/`）。

用法有两种：

```bash
# 方式一（推荐）：Taskfile 封装，输出固定为 outputs/devin-proto/
task extract BINARY=/Applications/Devin.app/Contents/Resources/app/extensions/windsurf/bin/language_server_macos_arm

# 方式二：直接调用底层工具，可指定任意输出目录（适合先提取到临时目录做对比）
go run ./cmd/protoextract <source-binary> <output-directory>
```

参数：

- `<source-binary>` — 要分析的编译产物（普通文件，如 Devin/Windsurf 的 language server 二进制）；
- `<output-directory>` — 输出目录，**会被清空后重建**；用方式一时固定为 `outputs/devin-proto/`，用方式二时建议指向临时目录（如 `/tmp/extract-test`）先对比再决定是否覆盖。

输出：

- `descriptors.pb` — 保留原始包名、语法、选项和文件边界的完整描述符集；
- `all-protos.proto` — 可直接编译的单文件扁平化 bundle；
- `manifest.json` — 每个描述符的元数据与符号映射。

**更新协议的标准步骤**（上游应用升级后，二进制可能包含新描述符）：

1. 提取到临时目录，与仓库版本对比差异：
   ```bash
   go run ./cmd/protoextract <new-binary> /tmp/extract-test
   diff <(grep '"name"' outputs/devin-proto/manifest.json | sort) \
        <(grep '"name"' /tmp/extract-test/manifest.json | sort)
   ```
2. 确认新增/变化的描述符符合预期，再覆盖 `outputs/devin-proto/`（方式一）或复制临时产物；
3. `task generate` 重新生成 Go 绑定；
4. 检查提取质量：`manifest.json` 的 `descriptor_count`、`missing_dependencies`，并用 `protoc --descriptor_set_out=/dev/null all-protos.proto` 验证可编译。

注意：该命令会**清空输出目录**，内置了根路径、家目录、源码目录等破坏性保护。步骤①依赖上游二进制与抓包，**不可复现**，产物必须提交进 git。

### ② task generate：proto → Go 绑定

`outputs/devin-proto/all-protos.proto` 生成 Connect 客户端绑定（`outputs/devin-proto-go/`，go.mod 通过 `replace local/devinproto => ./outputs/devin-proto-go` 引用）：

```bash
task generate
```

生成代码**不追踪进 git**（见 `.gitignore`），clone 后或上游描述符更新时先执行该命令。生成参数固化在 `Taskfile.yml` 中（protoc + `Mall-protos.proto=local/devinproto` 映射）；Docker 构建在 builder 阶段自动完成生成（`golang:1.26.3-alpine` → `alpine:3.22`，二进制位于 `/app/devin-2api`）。

## 代码结构速查

```text
cmd/
  devin-2api/       # 服务入口：加载配置、组装依赖、启动 HTTP
  protoextract/     # 工具：从编译产物中提取内嵌的 protobuf 描述符
internal/
  adapter/          # 适配器边界（interface Adapter）
    devin/          # Devin Connect 适配器：请求/响应双向转换 + 工具定义清洗
  api/openai/
    responses/      # OpenAI Responses HTTP 编解码（JSON 请求、JSON/SSE 响应）
  app/              # chi 路由组装、请求生命周期、错误处理
  config/           # YAML 配置加载与校验
  debuglog/         # 请求级分阶段调试日志（脱敏 + 图片附件外置）
  llm/              # 供应商无关的中间模型（请求、响应、事件流）
outputs/
  devin-proto/      # 从 Devin 二进制提取的原始描述符，已提交
  devin-proto-go/   # 生成的 Go 绑定，不追踪；由 task generate 生成
e2e/
  devin-client/     # Devin connect 客户端端到端调用示例
```

新适配器（对应其他上游）应实现 `internal/adapter` 的 `Adapter` 接口，并完全建立在 `internal/llm` 的语义模型之上，不引入 HTTP 层知识。