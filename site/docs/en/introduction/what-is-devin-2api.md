# What is devin-2api?

devin-2api is an unofficial protocol adapter that exposes the models available to your Devin account ([app.devin.ai](https://app.devin.ai/)) behind OpenAI- and Anthropic-compatible endpoints — so standard clients (Codex, Claude Code, any SDK) can call them through familiar APIs.

::: warning Disclaimer
This project is not affiliated with or endorsed by Cognition. It authenticates with your own Devin credentials (session token or durable platform key) against an internal RPC surface. It is intended for personal use with your own account; you are responsible for complying with Devin's terms of service.
:::

## What you get

- **Three API surfaces on one upstream** — `POST /v1/responses` (OpenAI Responses, including a WebSocket transport with multi-turn sessions for Codex-style clients), `POST /v1/chat/completions` (OpenAI Chat), and `POST /v1/messages` (Anthropic Messages). Streaming and non-streaming responses on all three.
- **Reasoning that round-trips** — thinking signatures are preserved and replayed across turns: `encrypted_content` reasoning items on Responses, `redacted_thinking` on Anthropic, `reasoning_content` on Chat.
- **Tool calling** — custom/freeform tool calls (e.g. `apply_patch`) round-trip untouched; tool names and `tool_choice` are validated locally; strict call↔result re-pairing matches what upstream enforces. Server-managed `web_search` declarations run through the upstream search RPC and return as native `web_search_call`/`server_tool_use` items — a Claude Code WebSearch side request short-circuits into a single managed search.
- **Image, document, and video inputs** — `input_image`, `input_file`/`file`/`document`, and `video`/`video_url`/`input_video` parts decode on all three surfaces (Anthropic `image`/`document`/`video` blocks included). Per-model capability flags gate them locally before the wire, so an incapable model fails fast instead of silently dropping the attachment. Video is frames only, no audio track.
- **Upstream stream recovery** — expired tokens reload from the credentials source, pre-content upstream failures (transport breaks, silent stalls, empty replies) retry transparently, and early failures surface as real HTTP errors instead of SSE errors after a committed `200`.
- **Detached completion cache** — a client disconnect doesn't kill the upstream stream: it keeps running server-side into a completion cache, and a semantically identical retry re-attaches — completed entries replay instantly, running ones replay the buffered prefix then follow live. Entries persist across restarts.
- **Multi-account upstream pool** — `devin.accounts` lanes each carry their own credentials, rate gate, and quota tracking. Session affinity pins a conversation to a lane, upstream failures fail over to a healthier sibling, and low weekly quota demotes a lane for new sessions. Lanes are managed live from the panel — an empty pool is a legal starting state.
- **Rate-limit gate** — upstream `resource_exhausted` trips a local cooldown latch: queued requests wait briefly then fast-fail `429` + `Retry-After` instead of hammering a limited upstream, drip-released probes detect recovery, and latch state persists across restarts.
- **Normalized error contract** — upstream error codes map to proper HTTP status and per-protocol error types; rate limits become `429` + `Retry-After`; every request carries an `X-Request-Id`/`debug_ref` identifying its debug record.
- **`/v1/models` capability flags** — context window plus tool/thinking/image/document/video support surfaced from the upstream model catalog; `devin.aliases` entries appear with `alias_of`, and the panel model registry can disable or redirect individual names.
- **Admin panel at `/web`** — request browser, usage/cost aggregation, quota tracking, process metrics, per-request debug payloads, downstream token management, pool lane control, model registry, a redacted config view with hot reload, and one-click self-update on managed installs.
- **Single static binary** — plus a public Docker image on GHCR.

## What it is not

- **A hosted service.** You run it on your own machine or server with your own Devin credentials.
- **A response store.** Over plain HTTP the proxy is stateless: every request must carry the full conversation — `previous_response_id` is rejected with an explicit error rather than silently dropping context. The WebSocket transport keeps multi-turn sessions per connection and expands incremental inputs into full transcripts.
- **An official Devin API.** It adapts an internal RPC surface that can change without notice; expect occasional breakage that a release then fixes.
