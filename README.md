# devin-2api

> **English** | [中文](README.zh-CN.md)

devin-2api is an unofficial protocol adapter that exposes the models available to your Devin account ([app.devin.ai](https://app.devin.ai/)) behind OpenAI- and Anthropic-compatible endpoints — so standard clients (Codex, Claude Code, any SDK) can call them through familiar APIs.

> **Disclaimer**: this project is not affiliated with or endorsed by Cognition. It authenticates with your own Devin session token against an internal RPC surface. It is intended for personal use with your own account; you are responsible for complying with Devin's terms of service.

## Features

- **Three API surfaces on one upstream** — `POST /v1/responses` (OpenAI Responses, incl. a WebSocket transport with multi-turn sessions for Codex-style clients), `POST /v1/chat/completions` (OpenAI Chat), `POST /v1/messages` (Anthropic Messages)
- **Streaming and non-streaming** responses (typed SSE / JSON)
- **Reasoning that round-trips** — thinking signatures are preserved and replayed across turns: `encrypted_content` reasoning items on Responses, `redacted_thinking` on Anthropic, `reasoning_content` on Chat
- **Faithful tool calling** — custom/freeform tool calls (e.g. `apply_patch`) round-trip untouched; tool names and `tool_choice` are validated locally; strict call↔result re-pairing matches what upstream enforces
- **Resilient upstream streams** — expired tokens are reloaded from the credentials source, pre-content upstream failures (transport breaks, silent stalls, empty replies) are retried transparently, and early failures surface as real HTTP errors instead of SSE errors after a committed `200`
- **Rate-limit gate** — upstream `resource_exhausted` trips a local cooldown latch: queued requests wait briefly then fast-fail `429` + `Retry-After` instead of hammering a limited upstream, drip-released probes detect recovery, and latch state persists across restarts (in `devin-2api.db`). An optional `max_rpm` token bucket shapes outbound pressure before the latch ever trips
- **Optional prefix warming** — replays retained session request bodies on a cadence to renew the upstream prompt cache, so a subagent resuming after a long wait doesn't pay a cold prefill (`devin.warm_prefix_*`, off by default; see `docs/upstream-cache.md`)
- **Normalized error contract** — upstream error codes map to proper HTTP status and per-protocol error types; rate limits become `429` + `Retry-After`; with request logging on (`debug.enabled`, on in the shipped `config.example.yaml`) every request carries `X-Request-Id`/`debug_ref` identifying its debug record
- **`/v1/models` capability flags** — context window, tool/thinking/image support surfaced from the upstream model catalog
- **Admin panel at `/web`** — request browser, usage/cost aggregation, quota tracking, process metrics, per-request debug payloads, and a redacted config view with hot reload for most fields
- **Easy to deploy** — single static binary, public Docker image on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)

## Quick start

### 1. Provide a Devin credential

devin-2api authenticates to Devin with one credential per upstream account in `devin.accounts` — the pool model covers single-account setups too (`accounts` is just a one-entry list), and an empty pool is legal: boot first, add accounts later from the panel (`/web/accounts.html`) without a restart. Each entry takes one (or more) of three credential sources:

- **`api_key` (recommended)** — a durable Devin platform key (`cog_...`) issued at [app.devin.ai](https://app.devin.ai/) → Settings → API keys. It carries no built-in expiry (valid until revoked): whenever upstream reports `unauthenticated` the lane mints itself a fresh session token from it, so an `api_key`-only account self-heals indefinitely with zero maintenance.
- **`token`** — a literal Devin session token (`devin-session-token$...`). Works directly, but session tokens carry a server-side TTL — once one dies, the lane can only recover if another source yields a fresh credential. Where to get one: it's the `windsurf_api_key` value inside `credentials.toml` after `devin auth login` (see below), or extractable from the Devin app's local state (macOS snippet below).
- **`credentials_file`** — points at the Devin CLI credential file (`~/.local/share/devin/credentials.toml` on macOS/Linux; `%APPDATA%\devin\credentials.toml` on Windows), which holds a session token the CLI renews on its own logins — re-reading the file follows CLI renewals automatically. The `devin` CLI ships inside the [Devin desktop app](https://devin.ai/download) under `resources/app/extensions/windsurf/devin/bin/` — e.g. `C:\Program Files\Windsurf\` on Windows, `/usr/share/devin-desktop/` from the Linux `.deb` — run `devin auth login`, finish the browser sign-in, and the file above appears.

Combining sources on one account is fine — e.g. `api_key` + `token`: the literal token serves until it dies, then the durable key mints a replacement.

On macOS you can also extract a session token from the Devin app's local state:

```bash
sqlite3 ~/Library/"Application Support"/Devin/User/globalStorage/state.vscdb \
  "SELECT json_extract(value, '$.apiKey') FROM ItemTable WHERE key='windsurfAuthStatus';"
```

When upstream answers `unauthenticated`, the lane re-resolves that account's credential — `credentials_file` re-read (follows CLI renewals) → row/config token if it changed → `api_key` mint — so every source except a bare literal token heals the proxy without a restart.

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` and declare your account under `devin.accounts` (starting from `config.example.yaml`, a single `{name, token}` entry is enough — the base URL and model are pre-filled as examples).

### 3. Run

Prebuilt binary (from [Releases](https://github.com/WncFht/devin2api/releases), `checksums.txt` attached for verification). Assets are named `devin-2api-{darwin,linux}-{amd64,arm64}`; Windows ships as same-named `.zip` bundles (exe + `config.example.yaml` + LICENSE):

```bash
# Linux shown; on macOS use devin-2api-darwin-arm64 or -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

On Windows: unzip `devin-2api-windows-amd64.zip`, edit `config.yaml` (an account may carry only `credentials_file` — step 1 covers the Windsurf-bundled `devin.exe` that produces the credential file), then run `devin-2api.exe -config config.yaml` in a console. Ctrl+C triggers the same graceful drain; closing the window and `taskkill /F` do not — Windows offers no graceful kill for console processes.

From source (generated proto bindings are committed under `outputs/devin-proto-go`, no toolchain needed):

```bash
go run ./cmd/devin-2api -config config.yaml
```

Docker (image published on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)):

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

Run as a service (optional):

| Platform | Supervisor                               | Layout                                                                                                       | Install / upgrade            |
| -------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------ | ---------------------------- |
| macOS    | launchd agent                            | bin `~/.local/bin` · config+state `~/Library/Application Support/devin-2api`                                 | `scripts/deploy.sh`          |
| Linux    | `systemd --user`                         | bin `~/.local/bin` · config `~/.config/devin-2api` · state `~/.local/state/devin-2api`                       | `scripts/deploy-linux.sh`    |
| Windows  | none — console, or NSSM / Task Scheduler | exe `%LOCALAPPDATA%\Programs\devin-2api` · config `%APPDATA%\devin-2api` · state `%LOCALAPPDATA%\devin-2api` | `scripts/deploy-windows.ps1` |

The binary resolves its paths per platform convention: config via `-config` flag → `DEVIN2API_CONFIG` → `./config.yaml` → the platform default above; state via `-state-dir` → `DEVIN2API_STATE_DIR` → platform default. Both deploy scripts install or upgrade in one shot (`--release latest` fetches a prebuilt binary), verify `/healthz` reports the new version, then probe `GET /v1/models` to confirm upstream auth actually works. They treat the repo as home — syncing `config.yaml` into the platform config dir and keeping a `logs` symlink inside the repo pointing at the state dir — so clone first, then run:

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
bash scripts/deploy-linux.sh --release latest    # macOS: scripts/deploy.sh
```

On first run `config.yaml` is generated from `config.example.yaml` with a random `auth.api_key`/`dashboard.password`, and you're prompted for the Devin token (left empty the pool starts empty — add accounts later from the panel); to preset values, `cp config.example.yaml config.yaml` and edit beforehand. `--check` reports installed/running/latest versions; `--uninstall` removes the service and binary while keeping config and logs.

On Linux, run `loginctl enable-linger $USER` if the service must outlive your login session.

### 4. Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}
```

## Usage

> **Note**: `/v1/*` endpoints are gated by the token store (the `auth_tokens` table in `devin-2api.db`, state dir): clients send `Authorization: Bearer <token>` or `X-Api-Key: <token>` matching an active token row. `auth.api_key` in `config.yaml` is only a seed source — it is written in as a normal token row on boot and every config reload. An empty store means open access — only bind beyond loopback if the store gates access, or you are handing out your Devin quota to the network. Tokens carry an optional `class` (`fg` default / `bg` for unattended batch traffic) that changes rate-gate admission — see `docs/gate-classes.md`.

Endpoints:

- `POST /v1/responses` — OpenAI Responses (`GET` on the same path negotiates WebSocket transport)
- `POST /v1/chat/completions` — OpenAI Chat Completions
- `POST /v1/messages` — Anthropic Messages
- `GET /v1/models`, `GET /v1/models/{model}` — upstream model catalog with capability flags
- `GET /web/*` — admin panel (request browser, usage, quota, process stats); `dashboard.password` protects it

The proxy is **stateless**: every HTTP request must carry the full conversation — `previous_response_id` is rejected with an explicit error rather than silently dropping context (there is no server-side response store; the response reports `store=false`). Over the WebSocket transport, multi-turn sessions are maintained per connection and incremental inputs are expanded into full transcripts transparently.

Call `http://localhost:8080/v1/responses` with your OpenAI Responses API client.

Non-streaming:

```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5-2",
    "input": "Hello"
  }'
```

Streaming (SSE):

```bash
curl -N http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "glm-5-2",
    "input": "Hello",
    "stream": true
  }'
```

The request body follows the OpenAI Responses API (`input`, `instructions`, `tools`, `stream`, …). Anthropic Messages clients call `/v1/messages` instead:

```bash
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "glm-5-2",
    "max_tokens": 256,
    "messages": [{"role": "user", "content": "Hello"}]
  }'
```

See the [Contributing guide](CONTRIBUTING.md) for the exact subset of fields supported per surface.

## Configuration

Configuration is a YAML file loaded once at startup. Unknown fields are rejected.

| Field                                            | Description                                                                                                                                                                                                                                                                                                                                                                                     | Required / Default                                                                |
| ------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `server.listen`                                  | HTTP listen address                                                                                                                                                                                                                                                                                                                                                                             | Yes                                                                               |
| `server.max_concurrency`                         | Max concurrent `/v1/*` requests                                                                                                                                                                                                                                                                                                                                                                 | `1024`                                                                            |
| `devin.base_url`                                 | Devin Connect service base URL                                                                                                                                                                                                                                                                                                                                                                  | Yes (no default in code; `config.example.yaml` uses `https://server.codeium.com`) |
| `devin.accounts`                                 | Upstream account pool entries `{name, token, credentials_file, api_key, priority, max_rpm}` — `token`/`credentials_file`/`api_key` at least one (`api_key` = durable `cog_…` platform key, self-mints session tokens); `priority` orders lane picks (0 = default), `max_rpm` overrides the per-account rate cap; empty list = legal empty pool (manage accounts live from `/web/accounts.html`) | No — `/v1` returns `unavailable` until an account exists                          |
| `devin.model`                                    | Devin chat model UID (e.g. `glm-5-2`)                                                                                                                                                                                                                                                                                                                                                           | Yes (no default in code)                                                          |
| `devin.aliases`                                  | Client model name → upstream UID map (`swe-2: swe-2-max`); match order exact → case-insensitive → `"*"` catch-all; aliases appear in `/v1/models` with `alias_of`                                                                                                                                                                                                                               | none                                                                              |
| `devin.client_name`/`client_version`/`client_os` | Client identity sent in upstream metadata                                                                                                                                                                                                                                                                                                                                                       | `chisel` / `3000.2.17` / `mac`                                                    |
| `devin.proxy`                                    | Upstream proxy URL (`http(s)://`, `socks5(h)://`); empty = direct / env vars                                                                                                                                                                                                                                                                                                                    | none                                                                              |
| `devin.force_http1`                              | Per-request TCP connections to upstream (avoids HTTP/2 stream serialization)                                                                                                                                                                                                                                                                                                                    | `true`                                                                            |
| `devin.max_rpm`                                  | Message rate limit to upstream (msgs/min, token bucket); `<=0` unlimited — the 429 cooldown latch applies either way                                                                                                                                                                                                                                                                            | `0` (unlimited; `config.example.yaml` ships `80`)                                 |
| `devin.gate_max_hold_seconds`                    | Max seconds a request may queue outside the latch for its next send window before fast-fail `429` + `Retry-After` (inside a latch requests fast-fail instead of holding)                                                                                                                                                                                                                        | `30`                                                                              |
| `devin.gate_drip_interval_seconds`               | Probe release interval inside a latch — paces upstream arrivals and unlatch detection while limited                                                                                                                                                                                                                                                                                             | `8`                                                                               |
| `devin.gate_default_latch_seconds`               | Fallback latch duration when upstream `resource_exhausted` doesn't declare a reset time                                                                                                                                                                                                                                                                                                         | `60`                                                                              |
| `devin.gate_window_offset_seconds`               | Estimated position of the upstream minute-bucket boundary inside the local minute (which second it falls on)                                                                                                                                                                                                                                                                                    | `0` (local `:00`; observed boundary is local `:59`)                               |
| `devin.gate_window_guard_seconds`                | Dead zone on both sides of the estimated bucket boundary — requests inside it sleep until the next window                                                                                                                                                                                                                                                                                       | `2`                                                                               |
| `devin.gate_bg_max_hold_seconds`                 | Queued-wait budget for `bg`-class tokens inside the gate — unattended batch traffic can afford to wait (fg still uses `gate_max_hold_seconds`); see `docs/gate-classes.md`                                                                                                                                                                                                                      | `120`                                                                             |
| `devin.gate_bg_reserve_margin`                   | Fixed safety margin (requests) in the bg admission reserve formula — the last `reserve` window slots stay unreachable to bg so fg always has headroom                                                                                                                                                                                                                                           | `4`                                                                               |
| `devin.warm_prefix_*`                            | Prefix-replay warming family (12 keys: `warm_prefix_enabled`, cadence/jitter, retained-body caps, min prefix tokens, four idle-TTL tiers, two pending-name classifiers) — replays retained session bodies to renew the upstream prompt cache across long subagent waits; full list in `config.example.yaml`, mechanism in `docs/upstream-cache.md`                                              | `warm_prefix_enabled: false`                                                      |
| `devin.session_affinity_ttl_seconds`             | Sliding TTL for session→lane bindings (renewed on every hit); headers `X-Claude-Code-Session-Id`/`X-Session-ID`/`X-Session-Affinity`/`X-Conversation-Id`/`X-Thread-Id` pin a session to a lane                                                                                                                                                                                                  | `3600`                                                                            |
| `devin.quota_low_threshold_percent`              | Weekly-quota percent below which a lane is demoted behind healthy lanes for new sessions (bound sessions unaffected)                                                                                                                                                                                                                                                                            | `15`                                                                              |
| `debug.enabled`                                  | Record per-request debug payload into `devin-2api.db` (`debug_files`/`debug_chunks` tables) in the state dir                                                                                                                                                                                                                                                                                    | `false`                                                                           |
| `debug.retention_days`                           | Days to keep per-request debug records; `<=0` disables time-based cleanup                                                                                                                                                                                                                                                                                                                       | `14`                                                                              |
| `debug.max_total_mb`                             | Total debug payload cap (MB); evicts oldest request groups first                                                                                                                                                                                                                                                                                                                                | `1024`                                                                            |
| `debug.payload_hours`                            | Hours before large stage payloads (03/04/06/attachments) are stripped, keeping meta/error evidence                                                                                                                                                                                                                                                                                              | `24`                                                                              |
| `debug.keep_error_dirs`                          | Newest N failed request groups (with an `error.json` row) protected from size eviction                                                                                                                                                                                                                                                                                                          | `32`                                                                              |
| `debug.errors_only`                              | Keep debug payloads only for failed or suspect requests — clean completions drop their payload at finish (meta/error anchors and `logs` rows stay); panel key `debug_log_errors_only`, hot-changeable                                                                                                                                                                                           | `false`                                                                           |
| `debug.quota_interval_minutes`                   | Quota snapshot interval into the `quota_samples` table; `<=0` disables                                                                                                                                                                                                                                                                                                                          | `5`                                                                               |
| `debug.pprof_listen`                             | Separate listen address for the pprof/fgprof profiling endpoints (e.g. `127.0.0.1:6060`); unauthenticated — loopback only                                                                                                                                                                                                                                                                       | empty (disabled)                                                                  |
| `dashboard.password`                             | `/web` admin password; empty = no login required                                                                                                                                                                                                                                                                                                                                                | none                                                                              |
| `auth.api_key`                                   | Seed credential written into the token store on boot/reload; `/v1/*` admission is decided by the store (empty store = open). Clients send `Authorization: Bearer <token>` or `X-Api-Key: <token>`                                                                                                                                                                                               | none                                                                              |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    accounts:
        - name: "main"
          token: "devin-session-token$..."
    model: "glm-5-2"

debug:
    enabled: false

dashboard:
    password: "" # /web login; empty = open

auth:
    # Set to a strong key to protect /v1/*; leave empty to keep endpoints open.
    api_key: ""
```

Notes:

- tokens are never written to logs (redacted as `<redacted>`);
- with no accounts configured the pool is empty: `/v1` requests fail fast with `unavailable` — add an account from the panel (`/web/accounts.html`) or `devin.accounts` + reload, no restart needed either way;
- `config.yaml` is gitignored — keep real tokens out of git anyway; pre-commit runs gitleaks to catch committed secrets.

## Documentation

- **Architecture, supported API fields, proto extraction, and other technical details**: [Contributing guide](CONTRIBUTING.md)
- **Upstream protocol reverse-engineering notes, client setup, debugging playbook, deployment & toolchain**: [docs/](docs/README.md)（中文）
- **License**: [MIT](LICENSE)

## Acknowledgments

This project builds on [leookun/devin-2api](https://github.com/leookun/devin-2api) — thanks to the original authors for their work.
