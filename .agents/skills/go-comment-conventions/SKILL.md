---
name: go-comment-conventions
description: Enforce concise Chinese responsibility and API comments in this Go project. Use when creating, modifying, or reviewing Go packages, files, types, structs, interfaces, fields, tests, constants, functions, or methods so the code remains self-documenting without excessive line-by-line narration.
---

# Go Comment Conventions

Use these rules for hand-written Go code in this repository. Comments should
state responsibility and semantic meaning, not narrate obvious syntax.

## Package and file responsibility

Each package must have one package-level comment, preferably in `doc.go`:

```go
// Package llm 定义与具体模型供应商无关的 LLM 请求、消息和响应抽象。
package llm
```

Every hand-written `.go` file must state its responsibility immediately before
the `package` declaration:

```go
// 本文件定义请求上下文、消息、内容块和工具定义。
package llm
```

Do not repeat the full package documentation in every file. A package needs
one package comment; each file needs its own narrower responsibility comment.
For `_test.go`, describe the test motivation rather than the implementation:

```go
// 本文件验证工具调用参数无效时请求校验会拒绝该请求。
package llm
```

## Types and fields

Every hand-written type declaration needs a concise Chinese comment. This
includes structs, interfaces, named scalar types, aliases, and generic types.
Start the comment with the exact declared name:

```go
// MessageRole 标识消息在对话中的角色。
type MessageRole string

// ResponseMessage 是 AssistantMessage 的语义别名。
type ResponseMessage = AssistantMessage
```

Every struct field needs its own Chinese comment, including unexported fields,
pointer/slice/map fields, and embedded fields. Explain semantic details when
they matter: units, optionality, `nil` meaning, empty-value meaning, provider
ownership, replay requirements, or billing impact.

```go
// Usage 保存一次模型响应的 token 用量。
type Usage struct {
	// Input 是输入 token 数。
	Input int64

	// Reasoning 是推理 token 数；nil 表示供应商没有提供该数据。
	Reasoning *int64
}
```

Do not use empty or tautological comments such as `// Input 输入`. A field
comment does not need to document its Go type when its semantic meaning is
already clear.

## Interfaces, constants, functions, and methods

Document every interface method because it is part of the behavioral contract:

```go
// Message 表示可被中间层统一处理的消息。
type Message interface {
	// Role 返回消息角色。
	Role() MessageRole

	// Validate 检查消息是否满足中间层约束。
	Validate() error
}
```

Document every exported constant, function, and method. Start exported
declaration comments with the exact identifier. Unexported helpers do not need
routine comments unless their behavior is non-obvious or easy to misuse.

```go
// StopReasonToolUse 表示模型因为请求工具调用而停止。
const StopReasonToolUse StopReason = "toolUse"
```

Comments must describe the contract and observable behavior. Do not add methods
or comments merely to imitate an object-oriented style; keep abstractions and
documentation aligned with actual callers.

## Generated and third-party code

Do not edit generated or third-party files solely to satisfy this convention.
This exemption covers files such as `*.pb.go`, `*.gen.go`, files with a
generator header, vendored code, and checked-in external dependencies. If a
generator is owned by this project, improve its template or generator instead
of hand-editing its output.

## Review checklist

When adding or reviewing a Go file, check:

- package responsibility is documented once;
- file responsibility is documented;
- every type and every struct field has a concise Chinese comment;
- every interface method is documented;
- exported constants, functions, and methods are documented;
- tests explain their motivation;
- comments describe semantics rather than obvious assignments;
- generated and third-party files were left untouched.
