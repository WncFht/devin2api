# Account Pool

`devin.accounts` declares the upstream account pool — one lane per Devin account, each with its own credentials, rate gate, and quota tracking. A single-account setup is a one-entry pool; an empty pool is legal (boot first, add accounts from the panel later — `/v1` returns `unavailable` until one exists).

## Declaring a lane

```yaml
devin:
    accounts:
        - name: "main"
          api_key: "cog_..."
          priority: 10
        - name: "backup"
          token: "devin-session-token$..."
          credentials_file: "/home/user/.local/share/devin/credentials.toml"
```

`name` is the lane's identity in logs, gate state, and the panel: 1–32 chars of letters/digits/hyphens/underscores, unique per pool. Optional per-lane fields: `priority` orders picks for new sessions (higher first, default 0); `max_rpm` overrides the lane's own minute-window quota (0 inherits `devin.max_rpm`).

Two entries referencing the same effective token, the same `credentials_file`, or the same `api_key` are rejected as a config error.

## Credential sources

Each lane takes one or more of:

- **`api_key` (recommended)** — a durable Devin platform key (`cog_...`) issued at [app.devin.ai](https://app.devin.ai/) → Settings → API keys. No built-in expiry: when upstream reports `unauthenticated` the lane mints itself a fresh session token from it, so an `api_key`-only lane self-heals indefinitely.
- **`token`** — a literal Devin session token (`devin-session-token$...`). Works directly, but session tokens carry a server-side TTL — once one dies, the lane can only recover if another source yields a fresh credential. Where to get one: the `windsurf_api_key` value inside `credentials.toml` after `devin auth login` (see below), or extractable from the Devin app's local state.
- **`credentials_file`** — points at the Devin CLI credential file (`~/.local/share/devin/credentials.toml` on macOS/Linux; `%APPDATA%\devin\credentials.toml` on Windows), which holds a session token the CLI renews on its own logins — re-reading the file follows CLI renewals automatically. The `devin` CLI ships inside the [Devin desktop app](https://devin.ai/download) under `resources/app/extensions/windsurf/devin/bin/` — run `devin auth login`, finish the browser sign-in, and the file appears. `~/` expands; relative paths anchor to the config file's directory, not the process CWD.

Combining sources on one lane is fine — e.g. `api_key` + `token`: the literal token serves until it dies, then the durable key mints a replacement.

When upstream answers `unauthenticated`, the lane re-resolves its credential in order — `credentials_file` re-read (follows CLI renewals) → row/config token if it changed → `api_key` mint — so every source except a bare literal token heals the proxy without a restart.

## How requests pick a lane

- **Session affinity** — a conversation pins to one lane so its upstream prompt cache stays warm. The affinity key is taken from headers first (`X-Claude-Code-Session-Id`, `X-Session-ID`, `X-Session-Affinity`, `X-Conversation-Id`, `X-Thread-Id`), then body fields (`metadata.user_id`, `prompt_cache_key`, `user`). Bindings live on a sliding TTL (`devin.session_affinity_ttl_seconds`, default 1h).
- **Failover** — an upstream failure on the picked lane retries on a healthier sibling; the client sees one request.
- **Quota demotion** — when a lane's weekly quota drops below `devin.quota_low_threshold_percent` (default 15%), it ranks behind healthy lanes for new sessions; already-bound sessions stay put.

## Managing lanes live

`/web/accounts.html` adds, edits, and removes lanes without a restart — the panel can also take a `credentials_content` paste instead of a file path, and free-form `notes`. Panel edits to a config-declared lane write an override row that always wins over the file; panel-deleting a config name leaves a tombstone that suppresses the declaration.
