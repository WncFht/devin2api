# Claude Code

Claude Code talks to devin-2api over the Anthropic Messages surface (`POST /v1/messages`). You need the proxy address and a downstream token from `/web/tokens.html`.

## Setup

`~/.claude/settings.json`:

```json
{
    "env": {
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
        "ANTHROPIC_AUTH_TOKEN": "<downstream token>",
        "ANTHROPIC_MODEL": "swe-2-max",
        "ANTHROPIC_SMALL_FAST_MODEL": "swe-2-max",
        "CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": "1",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "262000",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "230000"
    }
}
```

Replace `8080` with your `server.listen` port. Model name: `swe-2-max`, or any name you set in `devin.aliases` / the model registry.

## Why the two window variables matter

`swe-2-max` doesn't carry a `claude-` prefix, so Claude Code falls back to its unknown-model context window — far below the real upstream limit of 262000. Without the declarations, auto-compact either fires too early (wasting the window) or sits above the true limit (requests die of prompt-too-long instead of compacting first). `AUTO_COMPACT_WINDOW` at 230000 leaves ~30k headroom for the compaction request's own instructions and summary.

The alternative: let Claude Code send a `claude-`-prefixed name and map it back via `devin.aliases` — the window math then follows the claude model's profile.

## Optional resilience variables

```json
{
    "env": {
        "CLAUDE_CODE_MAX_RETRIES": "15",
        "CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS": "300000"
    }
}
```

- `CLAUDE_CODE_MAX_RETRIES` — upstream rate-limit episodes can run for several minutes; Claude Code sleeps per the `anthropic-ratelimit-unified-reset` header and each retry spends one budget slot. A higher budget rides out longer episodes (non-first-party mode clamps at 15).
- `CLAUDE_STREAM_FIRST_BYTE_TIMEOUT_MS` — first-byte watchdog in milliseconds; upstream can think silently for a while, so 300000 keeps long prefills from tripping it.

## WebSearch

Claude Code's WebSearch normally runs as a separate side request carrying a `web_search` server tool — devin-2api detects that shape and short-circuits it into a single managed search through the upstream search RPC, returning the result as a native `server_tool_use` block. No configuration needed.
