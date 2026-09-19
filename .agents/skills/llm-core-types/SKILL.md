---
name: llm-core-types
description: 维护并扩展本项目的供应商中立 LLM 核心类型。改动 internal/llm 的请求消息、内容块、工具定义、assistant 响应、usage 或流式响应事件，新增 OpenAI 或 Anthropic adapter，或判断某个字段该进核心模型还是进供应商 adapter 时使用。
---

# LLM 核心类型

本项目建模的是 agent 循环的语义契约，刻意不做成 OpenAI、Anthropic 或其他供应商的 wire DTO。供应商 adapter 负责与本模型互转；不要让供应商的命名或传输细节变成核心包的必选项。

## 事实源

定义位于：

- `internal/llm/request.go`：请求上下文、消息、内容块和工具。
- `internal/llm/response.go`：assistant 结果、usage、诊断信息和流事件。

这些文件里的中文字段注释要保留。用 `internal/llm/*_test.go` 的聚焦测试验证改动，然后跑 `go test ./...`。

## 请求模型

顶层请求是：

```text
RequestMessages
├── SystemPrompt string       独立的系统指令
├── Messages []Message        有序、可重放的对话历史
└── Tools []ToolDefinition    本次请求暴露的工具
```

`SystemPrompt` 独立于 `Messages`。供应商 adapter 可以把它放进供应商专属的 system 字段或转成供应商等价物，但核心模型不得假设系统文本是一条普通的 user 或 assistant 消息。

`Messages` 表示提供给 adapter 的完整上下文。核心模型不推断服务端会话状态或 session 持久化。

### 消息

具体消息类型有：

- `UserMessage`：`Content []Content`、`TimestampMS int64`。
- `AssistantMessage`：生成的内容加响应元数据；它也是最终 assistant 响应类型。
- `ToolResultMessage`：`ToolCallID`、`ToolName`、`Content`、可选 `Details`、可选工具 `Usage`、`AddedToolNames`、`IsError` 和 `TimestampMS`。

`Message` 是行为接口，含 `Role() MessageRole` 与 `Validate() error`。struct 承载数据并实现契约。不要为了显得面向对象而加方法；只有调用方需要该行为时才加。`ResponseMessage` 目前是 `AssistantMessage` 的别名，不是第二条平行的消息层级。

当前消息角色为 `user`、`assistant` 和 `toolResult`。

### 内容块

`Content` 是行为接口，含 `ContentType() ContentType` 与 `Validate() error`。具体内容块有：

- `TextContent`：`Text` 与供应商保留的 `TextSignature`。
- `ThinkingContent`：可见的 `Thinking`、不透明的 `ThinkingSignature`，以及用于隐藏或加密推理的 `Redacted`。
- `ImageContent`：base64 `Data` 与 `MIMEType`。
- `ToolCall`：`ID`、`Name`、完整 JSON 对象的 `Arguments`，以及可选 `ThoughtSignature`。

Content 是有序 slice，因为一条消息可以含多个块，例如文本后接图片、thinking 后接工具调用。重放历史时保留签名与不透明的供应商 payload；adapter 即使解释不了也可能需要它们。

### 工具定义

`ToolDefinition` 刻意收窄：

```text
Name string
Description string
InputSchema json.RawMessage
```

`InputSchema` 是供应商中立的 JSON Schema 对象。不要把供应商专属的受限采样或生成参数重新引入这个核心类型。schema 强制要求归供应商 adapter 与供应商的请求转换管。

## 响应模型

`AssistantMessage` 是聚合结果。其核心字段有：

- 有序 `Content`；
- 供应商元数据：`API`、`Provider`、`Model`、`ResponseModel`、`ResponseID`；
- 非主结果的 `Diagnostics`；
- 累计 `Usage`；
- `StopReason`、`ErrorMessage` 与 `TimestampMS`。

`StopReason` 取值为 `pending`、`stop`、`length`、`toolUse`、`error` 或 `aborted` 之一。工具调用是 assistant 消息内部的内容，不是独立的顶层响应家族。

`Usage` 含输入、输出、缓存读写、可选推理 token、总 token 数，以及 `UsageCost` 分解。可选计数器为 nil 表示供应商没有上报该维度；为零表示上报值就是零。

`AssistantMessageDiagnostic` 与 `DiagnosticErrorInfo` 承载转换或供应商诊断，不改变主响应语义。

## 流式响应事件

`ResponseEvent` 是供应商中立的增量协议。事件种类有：

```text
start
text_start / text_delta / text_end
thinking_start / thinking_delta / thinking_end
toolcall_start / toolcall_delta / toolcall_end
done
error
```

事件携带 `ContentIndex`、可选的累计 `Partial` assistant 消息，以及事件专属字段（`Delta`、完整 `Content`、`ToolCall`、`Reason`、`Message` 或 `Error`）。聚合的 `Partial` 对流式过程中到达的 usage 与元数据有用。

`toolcall_delta` 的空 `Delta` 合法：Anthropic 这类供应商可能发出空的 JSON 片段。不要把 text 与 thinking delta 的非空规则套到 tool-call delta 上。

## adapter 边界

adapter 负责：

- 把 `SystemPrompt`、完整 `Messages` 和 `Tools` 翻译成目标请求格式；
- 把供应商的工具调用片段组装成 `ToolCall.Arguments`；
- 把供应商流式 chunk 翻译成 `ResponseEvent` 值；
- 在核心字段提供无损存放位置时，保留签名、ID、注解与不透明供应商数据；
- 把供应商的 usage、停止原因和错误转成规范化类型。

不要用核心模型声称上游服务是有状态的。完整 `Messages` 历史的存在只说明这个 adapter 发送了什么；是否有状态要另行观察 session ID、请求内容与服务端行为。

出现新的供应商特性时，先判断它是通用 LLM 语义（加窄核心字段或内容块）还是供应商传输细节（留在 adapter 里）。保住核心的供应商中立含义，不要仅因为某个 wire 协议有某字段就加进来。
