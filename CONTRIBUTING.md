# Contributing

Thanks for considering contributing to `devin-2api`. This guide covers the architecture, the dev environment, and how to submit changes.

## Project positioning (read this first)

`devin-2api` is a **protocol adapter**: HTTP speaks OpenAI Responses / Chat Completions / Anthropic Messages, the upstream is Devin Connect, and a vendor-neutral model layer (`internal/llm`) isolates the two — so new upstreams can be added behind the same HTTP surface by implementing the adapter interface. Agent-loop semantics stay equivalent — not provider request-structure equivalent.

Every change must respect this boundary: an upstream must not bypass the `llm` intermediate layer — the HTTP protocol and any upstream protocol must never be mapped directly:

```text
OpenAI / Anthropic HTTP ──► llm intermediate layer ──► Devin Connect RPC
        (codec)               (semantic model)           (adapter)
```

- The HTTP codec only understands the OpenAI protocol, never Devin;
- `internal/llm` is the single semantic model; each side only performs semantic conversion, never pass-through of structures;
- Adapters only translate `internal/llm` ⇄ the upstream protocol and keep no HTTP-layer knowledge.

**What makes a good upstream? Statelessness.** The ideal upstream keeps no session state — every request is self-contained and carries the full conversation. Devin currently meets this: each `GetChatMessage` call carries the complete, replayable context, which keeps the gateway horizontally scalable and safe to replay.

## Architecture and data flow

```text
                 ┌────────────────────────────────────────────────────────┐
                 │                     devin-2api                         │
  HTTP client    │                                                        │    upstream
 ─────────────►  │  /v1/responses   /v1/chat/completions   /v1/messages   │  ┌──────────────────┐
   OpenAI /      │  (GET /v1/responses upgrades to WebSocket transport)   │  │ Devin Connect     │
   Anthropic     │        │                                               │  │ (server.codeium   │
   JSON / SSE    │        ▼                                               │  │  .com)            │
                 │  <surface>.DecodeRequest ──► llm.RequestMessages      │  │                   │
                 │        │                                               │  │ GetChatMessage    │
                 │        ▼                                               │  │ (Connect, proto)  │
                 │  adapter.Stream(ctx, RequestMessages) ───────────────►│  └──────────────────┘
                 │        │                                               │
                 │        ▼                                               │
                 │  llm.ResponseStream (event stream)                     │
                 │        │                                               │
                 │        ├─ streaming:  writeSSE + <surface>Encoder ──► │
                 │        └─ non-stream: collectFinalMessage ──► JSON    │
                 └────────────────────────────────────────────────────────┘
```

Full request lifecycle:

1. an API surface (`/v1/responses`, `/v1/chat/completions`, or `/v1/messages`) receives the request JSON (body capped at 32 MiB); `GET /v1/responses` upgrades to the OpenAI Responses WebSocket transport instead;
2. that surface's `DecodeRequest` converts the request into `llm.RequestMessages` (system prompt, message history, tool definitions) plus generation options;
3. `adapter.Stream` hands the vendor-neutral context to the configured adapter and returns an `llm.ResponseStream`;
4. the Devin adapter translates the intermediate model into a `GetChatMessageRequest` (protobuf), reads upstream frames over a Connect stream, and a `responseDecoder` interprets each frame into zero or more `llm.ResponseEvent`s;
5. output is split by the request's `stream` option:
    - **streaming**: the surface's `StreamEncoder` expands events into its own typed SSE (`response.output_text.delta` / `chat.completion.chunk` / `content_block_delta`, …);
    - **non-streaming**: `done`/`error` events are aggregated into a final `AssistantMessage` encoded as the surface's JSON shape.

### Intermediate model (internal/llm)

The semantic model shared by all adapters, defined in `internal/llm`:

| Concept            | Description                                                                                          |
| ------------------ | ---------------------------------------------------------------------------------------------------- |
| `RequestMessages`  | Full request context: `SystemPrompt` + chronologically ordered `Messages` + `Tools`                  |
| `Message`          | `UserMessage` / `AssistantMessage` / `ToolResultMessage` (role decided by `Role()`)                  |
| `Content`          | Content blocks: `TextContent` / `ThinkingContent` / `ImageContent` / `ToolCall` / `ServerToolResult` |
| `ToolDefinition`   | Tool name + description + JSON Schema input                                                          |
| `ResponseEvent`    | Incremental events (14 kinds: `start`, `text_delta`, `toolcall_*`, `done`, `error`, …)               |
| `AssistantMessage` | Final aggregated message incl. `Usage`, `StopReason`, provider metadata                              |

Message history is a **complete, replayable conversation across providers**: thinking signatures, tool-call IDs, and usage fields are designed to be passed verbatim into the next request round (see the comments on `TextSignature`, `ThinkingSignature`, etc. in `request.go`).

## Dev environment

- Go 1.27.1 (see `go.mod`)
- [Task](https://taskfile.dev/): the proto → Go binding generation entrypoint (`Taskfile.yml`)
- Generating the bindings requires `protoc` + `protoc-gen-go` + `protoc-gen-connect-go` (versions pinned in `Taskfile.yml`):

```bash
brew install protobuf
go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.20.0
```

- Formatting toolchain: `npm install` pins prettier/markdownlint-cli2/git-format-staged from `package.json`, `brew install autocorrect`, then `pre-commit install` — commits run markdownlint --fix then `autocorrect | prettier` on `*.md`, and gofmt on `*.go`; the latter two rewrite only staged content in the index via git-format-staged, never the working tree. Manual checks: `npm run format:check` / `npm run lint:md`
- Run once so `git blame` skips the mass-format commit: `git config blame.ignoreRevsFile .git-blame-ignore-revs`

## Common commands

```bash
# Regenerate the proto Go bindings (committed to git — only needed after outputs/devin-proto changes)
task generate

# Run the full test suite
go test ./...

# Run locally (needs config.yaml first, see README → Quick start)
go run ./cmd/devin-2api -config config.yaml
```

## Supported API surface

All three surfaces decode into the same `llm.RequestMessages` and re-encode the same `llm.ResponseEvent` stream — so upstream quirks (tool-call pairing, fingerprint sanitizing, thinking signatures) are handled once, centrally.

- `POST /v1/responses` — subset of the OpenAI Responses API: `input` (string or items: `message`, `function_call`, `function_call_output`, `custom_tool_call`, `custom_tool_call_output`, `reasoning`), `instructions`, `tools`, `stream`, `max_output_tokens`, `temperature`, `top_p`, `tool_choice`, `parallel_tool_calls`, `previous_response_id`, `prompt_cache_key`, `user`. `content` parts: `input_text`, `output_text`, `text`, `input_image` (base64 data URL). `GET /v1/responses` upgrades to the OpenAI Responses WebSocket transport (`responses_websockets=2026-02-06`), mapping each SSE event to one text frame.
- `POST /v1/chat/completions` — OpenAI Chat: `messages`, `tools`, `tool_choice`, `stream`/`stream_options.include_usage`, `max_tokens`/`max_completion_tokens`, `temperature`, `top_p`, `top_k`, `seed`, `stop`, `parallel_tool_calls`, `prompt_cache_key`, `user`, `n`, plus legacy `functions`/`function_call`. Unconsumed top-level fields (`response_format`, `reasoning_effort`, `store`, …) are recorded as dropped, not silently ignored.
- `POST /v1/messages` — Anthropic Messages: `system`, `messages` (text / `image` / `tool_use` / `tool_result` / `thinking`+`signature` / `redacted_thinking` blocks), `tools`, `tool_choice`, `max_tokens`, `stream`, `temperature`, `top_p`, `top_k`, `stop_sequences`, `metadata.user_id` (used as the session key for cache affinity). The top-level `thinking` param has no upstream counterpart and is recorded as dropped.
- `GET /v1/models` + `GET /v1/models/{model}` — upstream model list with capability flags (`context_tokens`, `max_output_tokens`, `supports_tool_calls`, `supports_parallel_tool_calls`, `supports_thinking`, `preserve_thinking`, `supports_images`).

### Tool description passing

Intermediate tools are converted into Devin native function tools (name and JSON Schema constraints preserved, while natural-language annotations such as `description`/`title` — which upstream may misinterpret as classification hints — are stripped); non-empty tool descriptions are additionally injected into the system prompt as `<tool name="...">` blocks so the model understands their purpose. See `internal/adapter/devin/tool_definition.go`.

## Debug logging

When `debug.enabled: true`, each request gets a staged log directory under `<state-dir>/logs/<request-time>/` (the repo `logs/` symlink points at that directory), useful for pinpointing failures at any hop of "HTTP ⇄ intermediate ⇄ upstream":

```text
meta.json                  # request outcome summary (status, model, duration)
01-http-request.json       # raw HTTP request (redacted)
02-request-messages.json   # converted intermediate request context
03-devin-request.json      # proto request sent upstream (as JSON); retries add
                           # .attemptN.json shards, managed search calls use the
                           # .searchN stem under the same prefix
04-devin-response.jsonl    # raw upstream response frames
05-response-events.jsonl   # intermediate response events
06-http-response.jsonl     # final response/SSE events written to the client
error.json                 # first failing stage and error
attachments/               # externalized image attachments (deduped by SHA-256)
```

Concurrent requests in the same second are distinguished by an incrementing suffix in the directory name.

`logs/index.jsonl` appends one summary line per completed request (result, model, `error_stage`, token classes, key hash) — it survives retention cleanup and backs the panel's usage aggregation. `logs/quota.jsonl` holds quota snapshots sampled every `debug.quota_interval_minutes`. Retention is tiered: `debug.retention_days` deletes whole dirs by age, `debug.max_total_mb` evicts oldest first, `debug.payload_hours` strips the large stage files (03/04/06/attachments) while keeping meta/error/01/02/05 evidence, and `debug.keep_error_dirs` protects the newest N failed dirs during size eviction.

The admin panel at `/web` (login: `dashboard.password`) renders these logs as a request browser and exposes `/admin/*` for programmatic access — `/admin/api` returns the endpoint catalog; `PUT /admin/settings/debug_log_enabled` hot-switches request logging without a restart.

## Before submitting

1. **Tests pass**: `go test ./...`
2. **Linted**: `golangci-lint run` is clean (`.golangci.yml`: default:none + explicit bodyclose/errcheck/govet/revive/staticcheck/unused) — CI runs the same job
3. **Formatted**: `gofmt -l .` produces no output (or `golangci-lint fmt` for gofmt+goimports)
4. **Comment conventions**: follow the repo's Go comment conventions (`.agents/skills/go-comment-conventions`) — exported symbols get doc comments, field comments explain "why", not restate the code
5. **Docs linted**: commits touching `*.md` run the pre-commit pipeline; if a hook rewrites a file, re-stage it and commit again
6. **No real tokens**: `config.yaml` is gitignored; keep it that way and make sure no real `devin.token` ends up in any committed file (pre-commit runs gitleaks to catch committed secrets)

## Submitting changes

1. Fork the repo and create a feature branch off `main` (e.g. `fix/sse-close`, `feat/stream-options`);
2. One logical change per commit; write commit messages in the imperative, saying what and why;
3. If the change alters protocol semantics or adapter behavior, update the README and tests accordingly;
4. Open a Pull Request and describe:
    - the purpose and how you verified it;
    - which layer it touches ("HTTP ⇄ intermediate ⇄ upstream");
    - whether an upstream protocol upgrade is involved (descriptor changes, see below).

## Releasing

Releases follow [SemVer](https://semver.org/). While the project is in the 0.x phase, breaking changes bump the minor version (`v0.1.0` → `v0.2.0`), not the major one.

A release is a `v`-prefixed tag published via `scripts/release.sh`. The script derives the next version from Conventional Commits since the last tag, then gates on `HEAD == origin/main` and a green `CI` workflow run for that commit — tags are only ever placed on pushed, tested commits:

```bash
scripts/release.sh            # dry-run: next version, changelog, gate status
scripts/release.sh --publish  # create + push the annotated tag
```

Pushing the tag triggers the `release.yml` workflow (full test suite, per-platform binaries built with `-X main.version=<tag>`, packaged into a multi-arch image pushed to GHCR `ghcr.io/wncfht/devin2api`, plus a GitHub Release whose notes come from the tag annotation).

Notes:

- a stable tag `v0.1.0` publishes image tags `0.1.0`, `0.1`, `0`, and `latest`; pre-releases like `v0.2.0-rc.1` only get the exact `0.2.0-rc.1` tag (no floating aliases);
- GHCR is always published via `GITHUB_TOKEN` — no secrets to configure. Setting `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` additionally mirrors the image to Docker Hub;
- tags are immutable once pushed (enforced by a repo ruleset); fix a bad release by releasing a new version, never by rewriting the tag.

## Updating the upstream protocol (proto extraction)

The full pipeline is fixed in `Taskfile.yml` as two steps:

```text
task extract BINARY=<upstream-binary>   ① binary → outputs/devin-proto/    (not reproducible, depends on packet capture)
task generate                           ② proto  → outputs/devin-proto-go/ (reproducible, standard toolchain)
```

### ① protoextract: binary → proto

Scans compiled binaries for embedded `FileDescriptorProto`s and reconstructs .proto sources — used to recover the upstream protocol when no original .proto files exist (this project used it to extract Devin's 63 descriptors, see `outputs/devin-proto/`).

Two ways to run it:

```bash
# Option 1 (recommended): Taskfile wrapper, output fixed to outputs/devin-proto/
task extract BINARY=/Applications/Devin.app/Contents/Resources/app/extensions/windsurf/bin/language_server_macos_arm

# Option 2: call the underlying tool directly with any output directory
# (useful for extracting to a temp dir and comparing first)
go run ./cmd/protoextract <source-binary> <output-directory>
```

Arguments:

- `<source-binary>` — the compiled artifact to analyze (a regular file, e.g. the Devin/Windsurf language server binary);
- `<output-directory>` — the output directory, **emptied and rebuilt**; fixed to `outputs/devin-proto/` via Option 1, or a temp dir (e.g. `/tmp/extract-test`) via Option 2 to compare before overwriting.

Outputs:

- `descriptors.pb` — the complete descriptor set preserving original package names, syntax, options, and file boundaries;
- `all-protos.proto` — a flattened single-file bundle that compiles as-is;
- `manifest.json` — per-descriptor metadata and symbol mappings.

**Standard procedure after an upstream upgrade** (the new binary may embed new descriptors):

1. Extract to a temp dir and diff against the committed version:

    ```bash
    go run ./cmd/protoextract <new-binary> /tmp/extract-test
    diff <(grep '"name"' outputs/devin-proto/manifest.json | sort) \
         <(grep '"name"' /tmp/extract-test/manifest.json | sort)
    ```

2. Confirm the added/changed descriptors are expected, then overwrite `outputs/devin-proto/` (Option 1) or copy the temp outputs;
3. Run `task generate` to regenerate the Go bindings;
4. Verify extraction quality: check `descriptor_count` and `missing_dependencies` in `manifest.json`, and compile-check `all-protos.proto` with `protoc --descriptor_set_out=/dev/null all-protos.proto`.

The command **empties the output directory** and guards against destructive paths (filesystem root, home directory, the source binary, etc.). Step ① depends on the upstream binary and packet capture, so it is **not reproducible** and its outputs must be committed to git.

### ② task generate: proto → Go bindings

The Connect client bindings (`outputs/devin-proto-go/`, referenced via `replace local/devinproto => ./outputs/devin-proto-go` in go.mod) are generated from `outputs/devin-proto/all-protos.proto`:

```bash
task generate
```

The generated code is **committed to git** (it is the `replace` target in `go.mod`), so a fresh clone builds without running this — re-run it only when the upstream descriptors change. The generation parameters are fixed in `Taskfile.yml` (protoc with the `Mall-protos.proto=local/devinproto` mapping); Docker builds copy the committed bindings instead of regenerating (`golang:1.27.1-alpine` → `alpine:3.22`, binary at `/app/devin-2api`).

## Code layout at a glance

```text
cmd/
  devin-2api/       # entrypoint: load config, assemble deps, serve HTTP
  protoextract/     # tool: extract embedded protobuf descriptors from binaries
  protocensus/      # tool: census/diff descriptor sets across debug log dirs (task census)
  probe/            # tool: direct Connect-RPC live probes against the upstream
  upstreamstub/     # tool: local upstream stub replaying transport failure scenarios
  loadtest/         # tool: load generator for the HTTP surfaces
internal/
  adapter/          # adapter boundary (interface Adapter, Unavailable placeholder)
    devin/          # Devin Connect adapter: request/response conversion + tool-definition sanitizing
  api/
    anthropic/
      messages/     # Anthropic Messages HTTP codec
    openai/
      chat/         # OpenAI Chat Completions HTTP codec
      responses/    # OpenAI Responses HTTP codec (JSON request, JSON/SSE response)
    common/         # shared surface plumbing: error normalization, tool-choice parsing
  app/              # chi routing, request lifecycle, error handling
  authtoken/        # downstream API token store (auth_tokens.json): /v1 admission concurrency/cost/model limits
  ccpanel/          # admin panel (ccLoad contract): /web, /public, /dashboard/*, /admin/*
  config/           # YAML config loading and validation
  debuglog/         # per-request staged debug logs (redaction + externalized images)
  httpproxy/        # upstream HTTP client construction (proxy, force_http1)
  llm/              # vendor-neutral intermediate model (request, response, event stream)
  modelreg/         # global model registry (models.json): disable/redirect overlays before alias resolution
  obs/              # process/HTTP metrics behind /admin/runtime-metrics
  randid/           # random ID generation (X-Request-Id / debug dir names)
  upstream/         # shared upstream wire helpers (request metadata, auth transport)
outputs/
  devin-proto/      # raw descriptors extracted from the Devin binary, committed
  devin-proto-go/   # generated Go bindings, committed; regenerated by task generate
e2e/
  devin-client/     # end-to-end Devin connect client call example (hand-written)
```

New adapters (for other upstreams) should implement the `Adapter` interface in `internal/adapter`, built entirely on the `internal/llm` semantic model, without introducing HTTP-layer knowledge.
