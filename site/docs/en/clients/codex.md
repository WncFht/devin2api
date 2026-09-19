# Codex

Codex talks to devin-2api over the OpenAI Responses surface (`POST /v1/responses`).

## Setup

`~/.codex/config.toml`:

```toml
model_provider = "OpenAI"
model = "swe-2-max"
model_context_window = 262000
model_auto_compact_token_limit = 230000

[model_providers.OpenAI]
base_url = "http://127.0.0.1:8080/v1"
stream_max_retries = 100
```

The downstream token goes through the `OPENAI_API_KEY` environment variable.

Two details matter:

- **`model_context_window` / `model_auto_compact_token_limit` must reflect the real window (262000).** A default or mismatched larger value puts the auto-compact threshold beyond the upstream limit, so oversized requests fail instead of compacting first.
- **`stream_max_retries = 100`.** codex-rs terminates on HTTP 429 (`retry_429` is hardcoded false), so devin-2api converts pre-stream rate limits into `response.failed` stream events — Codex reads `try again in Ns` and sleeps until the latch lifts. The default 5 retries covers under a minute of limiting; upstream minute-bucket episodes run longer, so raise it toward the 100 cap.

`apply_patch` arrives as a `type: "custom"` tool call and round-trips through the proxy untouched.

## WebSocket transport (optional)

devin-2api also serves the Responses API over WebSocket — `GET /v1/responses` negotiates the upgrade, and multi-turn sessions live on the connection (`response.create` / `response.append` with incremental input, expanded into full transcripts server-side). Codex opts in with `supports_websockets = true` in `[model_providers.OpenAI]` plus `prefer_websockets` on the model entry. Plain HTTP POST works fine without this.
