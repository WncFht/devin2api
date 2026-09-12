---
name: llm-core-types
description: Preserve and extend this project's provider-neutral LLM core types. Use when changing internal/llm request messages, content blocks, tool definitions, assistant responses, usage, or streaming response events; when adding OpenAI or Anthropic adapters; or when deciding whether a field belongs in the core model or in a provider adapter.
---

# LLM Core Types

This project models the semantic contract of an agent loop. It is deliberately
not an OpenAI, Anthropic, or other provider wire DTO. Provider adapters translate
to and from this model; do not let a provider's naming or transport details
become required by the core package.

## Source of truth

The definitions live in:

- `internal/llm/request.go`: request context, messages, content blocks, and tools.
- `internal/llm/response.go`: assistant result, usage, diagnostics, and stream events.

Keep Chinese field comments in these files. Validate changes with the focused
tests in `internal/llm/*_test.go` and then run `go test ./...`.

## Request model

The top-level request is:

```text
RequestMessages
├── SystemPrompt string       independent system instruction
├── Messages []Message        ordered, replayable conversation history
└── Tools []ToolDefinition    tools exposed for this request
```

`SystemPrompt` is separate from `Messages`. A provider adapter may place it in
the provider-specific system field or convert it to the provider's equivalent,
but the core model must not assume that system text is an ordinary user or
assistant message.

`Messages` represents the complete context supplied to an adapter. The core
model does not infer server-side conversation state or session persistence.

### Messages

The concrete message types are:

- `UserMessage`: `Content []Content`, `TimestampMS int64`.
- `AssistantMessage`: generated content plus response metadata; it is also the
  final assistant response type.
- `ToolResultMessage`: `ToolCallID`, `ToolName`, `Content`, optional `Details`,
  optional tool `Usage`, `AddedToolNames`, `IsError`, and `TimestampMS`.

`Message` is a behavioral interface with `Role() MessageRole` and
`Validate() error`. Structs carry the data and implement the contract. Do not
add a method merely to make an abstraction look object-oriented; add it only if
the caller needs that behavior. `ResponseMessage` is currently an alias for
`AssistantMessage`, not a second parallel message hierarchy.

The message roles currently are `user`, `assistant`, and `toolResult`.

### Content blocks

`Content` is a behavioral interface with `ContentType() ContentType` and
`Validate() error`. The concrete blocks are:

- `TextContent`: `Text` and provider-preserved `TextSignature`.
- `ThinkingContent`: visible `Thinking`, opaque `ThinkingSignature`, and
  `Redacted` for hidden or encrypted reasoning.
- `ImageContent`: base64 `Data` and `MIMEType`.
- `ToolCall`: `ID`, `Name`, completed JSON-object `Arguments`, and optional
  `ThoughtSignature`.

Content is an ordered slice because a single message can contain multiple
blocks, for example text followed by an image or thinking followed by a tool
call. Preserve signatures and opaque provider payloads when replaying history;
adapters may need them even when they cannot interpret them.

### Tool definition

`ToolDefinition` is intentionally narrow:

```text
Name string
Description string
InputSchema json.RawMessage
```

`InputSchema` is a provider-neutral JSON Schema object. Do not reintroduce
provider-specific constrained-sampling or generation settings into this core
type. Requirements for schema enforcement belong to the provider adapter and
the provider's request conversion.

## Response model

`AssistantMessage` is the aggregate result. Its core fields are:

- ordered `Content`;
- provider metadata: `API`, `Provider`, `Model`, `ResponseModel`, `ResponseID`;
- non-primary `Diagnostics`;
- cumulative `Usage`;
- `StopReason`, `ErrorMessage`, and `TimestampMS`.

`StopReason` is one of `pending`, `stop`, `length`, `toolUse`, `error`, or
`aborted`. A tool call is content inside the assistant message, not a separate
top-level response family.

`Usage` contains input, output, cache read/write, optional reasoning tokens,
total tokens, and a `UsageCost` breakdown. Nil optional counters mean that the
provider did not report that dimension; zero means it was reported as zero.

`AssistantMessageDiagnostic` and `DiagnosticErrorInfo` carry conversion or
provider diagnostics without changing the main response semantics.

## Streaming response events

`ResponseEvent` is the provider-neutral incremental protocol. Its event kinds
are:

```text
start
text_start / text_delta / text_end
thinking_start / thinking_delta / thinking_end
toolcall_start / toolcall_delta / toolcall_end
done
error
```

The event carries a `ContentIndex`, optional accumulated `Partial` assistant
message, and event-specific fields (`Delta`, completed `Content`, `ToolCall`,
`Reason`, `Message`, or `Error`). The aggregate `Partial` is useful for usage
and metadata that arrive during a stream.

An empty `Delta` is valid for `toolcall_delta`: providers such as Anthropic may
emit an empty partial JSON fragment. Do not apply the non-empty delta rule used
by text and thinking deltas to tool-call deltas.

## Adapter boundaries

Adapters are responsible for:

- translating `SystemPrompt`, complete `Messages`, and `Tools` into the target
  request format;
- assembling provider tool-call fragments into `ToolCall.Arguments`;
- translating provider stream chunks into `ResponseEvent` values;
- preserving signatures, IDs, annotations, and opaque provider data where the
  core fields provide a lossless place for them;
- converting provider usage, stop reasons, and errors into the normalized types.

Do not use the core model to claim that an upstream service is stateful. The
presence of a complete `Messages` history only states what this adapter sends;
statefulness requires observing session IDs, request contents, and server
behavior separately.

When a new provider feature appears, first decide whether it is a general LLM
semantic (add a narrow core field or content block) or a provider transport
detail (keep it in the adapter). Preserve the core's provider-neutral meaning
and avoid adding fields solely because one wire protocol has them.
