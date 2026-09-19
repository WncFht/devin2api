---
name: go-comment-conventions
description: 强制本 Go 项目使用简洁的中文职责与 API 注释。创建、修改或评审 Go 包、文件、类型、struct、接口、字段、测试、常量、函数或方法时使用，让代码保持自文档化而不逐行复述显而易见的内容。
---

# Go 注释规范

本仓库手写 Go 代码适用以下规则。注释要说明职责与语义，不复述显而易见的语法。

## 包与文件职责

每个包必须有一条包级注释，最好放在 `doc.go`：

```go
// Package llm 定义与具体模型供应商无关的 LLM 请求、消息和响应抽象。
package llm
```

每个手写 `.go` 文件必须在 `package` 声明正上方写明自己的职责：

```go
// 本文件定义请求上下文、消息、内容块和工具定义。
package llm
```

不要在每个文件里重复整个包的文档。包只需要一条包注释；每个文件要有自己更窄的职责注释。`_test.go` 描述测试动机而非实现：

```go
// 本文件验证工具调用参数无效时请求校验会拒绝该请求。
package llm
```

## 类型与字段

每个手写类型声明都要一条简洁中文注释，包括 struct、接口、命名标量类型、别名与泛型类型。注释以声明名原文开头：

```go
// MessageRole 标识消息在对话中的角色。
type MessageRole string

// ResponseMessage 是 AssistantMessage 的语义别名。
type ResponseMessage = AssistantMessage
```

每个 struct 字段都要自己的中文注释，含未导出字段、指针/slice/map 字段与嵌入字段。语义要紧的地方解释清楚：单位、可选性、`nil` 含义、空值含义、供应商所有权、重放要求或计费影响。

```go
// Usage 保存一次模型响应的 token 用量。
type Usage struct {
	// Input 是输入 token 数。
	Input int64

	// Reasoning 是推理 token 数；nil 表示供应商没有提供该数据。
	Reasoning *int64
}
```

禁止 `// Input 输入` 这类空洞的同义反复注释。字段语义已经清楚时，注释不需要复述它的 Go 类型。

## 接口、常量、函数与方法

每个接口方法都要写注释——它是行为契约的一部分：

```go
// Message 表示可被中间层统一处理的消息。
type Message interface {
	// Role 返回消息角色。
	Role() MessageRole

	// Validate 检查消息是否满足中间层约束。
	Validate() error
}
```

每个导出常量、函数、方法都要写注释。导出声明的注释以标识符原文开头。未导出 helper 不需要例行注释，除非行为不显然或容易误用。

```go
// StopReasonToolUse 表示模型因为请求工具调用而停止。
const StopReasonToolUse StopReason = "toolUse"
```

注释必须描述契约与可观察行为。不要为了模仿面向对象风格而加方法或注释；抽象与文档要对齐真实调用方。

## 生成代码与第三方代码

不要仅为满足本规范去改生成或第三方文件。豁免范围包括 `*.pb.go`、`*.gen.go`、带生成器头的文件、vendor 代码与入库的外部依赖。生成器归本项目所有时，改它的模板或生成器，不要手改产物。

## 评审清单

新增或评审 Go 文件时检查：

- 包职责有且仅有一处文档；
- 文件职责已写明；
- 每个类型与每个 struct 字段都有简洁中文注释；
- 每个接口方法都有注释；
- 导出常量、函数、方法都有注释；
- 测试说明了动机；
- 注释描述语义而非显而易见的赋值；
- 生成与第三方文件保持原样。
