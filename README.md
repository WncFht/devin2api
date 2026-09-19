# devin-2api

<p align="center">
  <img src="docs/images/logo.png" alt="devin-2api logo" width="128">
</p>

> **English** | [中文](README.zh-CN.md)

devin-2api is an unofficial protocol adapter that exposes the models available to your Devin account ([app.devin.ai](https://app.devin.ai/)) behind OpenAI- and Anthropic-compatible endpoints — so standard clients (Codex, Claude Code, any SDK) can call them through familiar APIs.

> **Disclaimer**: this project is not affiliated with or endorsed by Cognition. It authenticates with your own Devin credentials (session token or durable platform key) against an internal RPC surface. It is intended for personal use with your own account; you are responsible for complying with Devin's terms of service.

## Features

- **Three API surfaces on one upstream** — `POST /v1/responses` (OpenAI Responses, incl. a WebSocket transport with multi-turn sessions for Codex-style clients), `POST /v1/chat/completions` (OpenAI Chat), `POST /v1/messages` (Anthropic Messages)
- **Streaming and non-streaming** responses (typed SSE / JSON)
- **Reasoning that round-trips** — thinking signatures are preserved and replayed across turns: `encrypted_content` reasoning items on Responses, `redacted_thinking` on Anthropic, `reasoning_content` on Chat
- **Tool calling** — custom/freeform tool calls (e.g. `apply_patch`) round-trip untouched; tool names and `tool_choice` are validated locally; strict call↔result re-pairing matches what upstream enforces. Server-managed `web_search` declarations are executed through the upstream search RPC and returned as native `web_search_call`/`server_tool_use` items — a Claude Code WebSearch side request short-circuits into a single managed search
- **Image, document, and video inputs** — `input_image`, `input_file`/`file`/`document`, and `video`/`video_url`/`input_video` parts decode on all three surfaces (Anthropic `image`/`document`/`video` blocks included). Per-model capability flags gate them locally before the wire, so an incapable model fails fast instead of silently dropping the attachment; video is frames only, no audio track
- **Upstream stream recovery** — expired tokens are reloaded from the credentials source, pre-content upstream failures (transport breaks, silent stalls, empty replies) are retried transparently, and early failures surface as real HTTP errors instead of SSE errors after a committed `200`
- **Detached completion cache** — a client disconnect doesn't kill the upstream stream: it keeps running server-side into a completion cache, and a semantically identical retry re-attaches — completed entries replay instantly, running ones replay the buffered prefix then follow live. Entries persist across restarts
- **Multi-account upstream pool** — `devin.accounts` lanes each carry their own credentials, rate gate, and quota tracking. Session affinity keys (`X-Claude-Code-Session-Id`, `X-Session-ID`/`X-Session-Affinity`/`X-Conversation-Id`/`X-Thread-Id` headers, `metadata.user_id`, `prompt_cache_key`/`user`) pin a conversation to a lane, upstream failures fail over to a healthier sibling, and low weekly quota demotes a lane for new sessions. Lanes are managed live from `/web/accounts.html` — an empty pool is a legal starting state
- **Rate-limit gate** — upstream `resource_exhausted` trips a local cooldown latch: queued requests wait briefly then fast-fail `429` + `Retry-After` instead of hammering a limited upstream, drip-released probes detect recovery, and latch state persists across restarts (in `devin-2api.db`). An optional `max_rpm` token bucket shapes outbound pressure before the latch ever trips
- **Optional prefix warming** — replays retained session request bodies on a cadence to renew the upstream prompt cache, so a subagent resuming after a long wait doesn't pay a cold prefill (`devin.warm_prefix_*`, off by default; see `docs/upstream-cache.md`)
- **Normalized error contract** — upstream error codes map to proper HTTP status and per-protocol error types; rate limits become `429` + `Retry-After`; with request logging on (`debug.enabled`, on in the shipped `config.example.yaml`) every request carries `X-Request-Id`/`debug_ref` identifying its debug record
- **`/v1/models` capability flags** — context window plus tool/thinking/image/document/video support surfaced from the upstream model catalog; `devin.aliases` entries appear with `alias_of`, and the panel model registry can disable or redirect individual names
- **Admin panel at `/web`** — request browser, usage/cost aggregation, quota tracking, process metrics, per-request debug payloads, downstream token management, pool lane control, model registry, a redacted config view with hot reload for most fields, and one-click self-update on managed installs
- **Single static binary** — public Docker image on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)

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

Five install routes, all ending at the same binary — they differ in who manages the process and how upgrades work.

#### Option A: install script (recommended on Linux & macOS)

```bash
curl -sSL https://raw.githubusercontent.com/WncFht/devin2api/main/scripts/install.sh | bash
```

One command, no clone, no root. `install.sh` fetches the deploy pipeline at the target tag and hands off to the platform deploy script — the binary lands in `~/.local/bin`, config and state in the platform dirs (option B's layout table), and the service runs under `systemd --user` (Linux) or launchd (macOS) with a zero-downtime REUSEPORT handoff on every restart. Subcommands: `install` (default; `-v <tag>` pins a release), `upgrade`, `rollback <tag>`, `status`, `list-versions`, `uninstall` (removes service + binary, keeps config and logs). On Windows use option B's PowerShell script or option C instead.

On first run `config.yaml` is generated from `config.example.yaml` with a random `dashboard.password`, and you're prompted for the Devin token (left empty the pool starts empty — add accounts later from the panel); downstream `/v1` tokens are created in the panel (`/web/tokens.html`), never in config. To preset values, `cp config.example.yaml config.yaml` and edit beforehand.

On Linux, run `loginctl enable-linger $USER` if the service must outlive your login session.

#### Option B: deploy scripts from a checkout

The same managed service as option A, driven from a clone — useful when you want the repo on disk, since the scripts treat it as home (sync `config.yaml` into the platform config dir, keep a `logs` symlink in the repo pointing at the state dir):

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
bash scripts/deploy/deploy-linux.sh --release latest    # macOS: scripts/deploy/deploy.sh
```

`--release latest` fetches a sha256-verified prebuilt binary, then the script polls `/healthz` until the new version answers and probes `GET /v1/models` to confirm upstream auth works. `--check` reports installed/running/latest versions; `--uninstall` removes the service and binary while keeping config and logs; `--no-restart` swaps the binary without restarting. First-run config generation works the same as option A.

| Platform | Supervisor                               | Layout                                                                                                       | Script                              |
| -------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------ | ----------------------------------- |
| macOS    | launchd agent                            | bin `~/.local/bin` · config+state `~/Library/Application Support/devin-2api`                                 | `scripts/deploy/deploy.sh`          |
| Linux    | `systemd --user`                         | bin `~/.local/bin` · config `~/.config/devin-2api` · state `~/.local/state/devin-2api`                       | `scripts/deploy/deploy-linux.sh`    |
| Windows  | none — console, or NSSM / Task Scheduler | exe `%LOCALAPPDATA%\Programs\devin-2api` · config `%APPDATA%\devin-2api` · state `%LOCALAPPDATA%\devin-2api` | `scripts/deploy/deploy-windows.ps1` |

On Windows `deploy-windows.ps1 -Release latest` generates `config.yaml` bound to loopback on a free port (avoids the firewall prompt and a bare exposure) and starts the instance in its own console window — run it from a local interactive session, not over SSH (the job object kills the instance when the session ends).

#### Option C: prebuilt binary

Assets on [Releases](https://github.com/WncFht/devin2api/releases) are named `devin-2api-{darwin,linux}-{amd64,arm64}`; Windows ships `devin-2api-windows-{amd64,arm64}.zip` bundles (exe + `config.example.yaml` + LICENSE). `checksums.txt` is attached for verification:

```bash
# Linux shown; on macOS use devin-2api-darwin-arm64 or -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing   # expect: devin-2api-linux-amd64: OK
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

On Windows: unzip, edit `config.yaml` (an account may carry only `credentials_file` — step 1 covers the Windsurf-bundled `devin.exe` that produces the credential file), then run `devin-2api.exe -config config.yaml` in a console. Ctrl+C triggers the same graceful drain; closing the window and `taskkill /F` do not — Windows offers no graceful kill for console processes.

Path resolution: config via `-config` flag → `DEVIN2API_CONFIG` → `./config.yaml` → the platform default in option B's table; state via `-state-dir` → `DEVIN2API_STATE_DIR` → the platform default.

#### Option D: Docker

The image is published on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api):

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  -v devin2api-state:/app/state \
  -e DEVIN2API_STATE_DIR=/app/state \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

The state volume keeps `devin-2api.db` (downstream tokens, request logs, quota samples) across container restarts — without it each run starts with an empty store, meaning open `/v1` access.

#### Option E: from source

Generated proto bindings are committed under `outputs/devin-proto-go`, so a clone builds with no extra toolchain:

```bash
go run ./cmd/devin-2api -config config.yaml
```

#### Upgrading

Managed installs (options A–B) can update themselves from the panel — `/web/settings.html` carries a version-update card: check → download → sha256 verify → swap → restart across the same REUSEPORT handoff, so nothing drops and the panel polls progress across the process switch. `POST /admin/update` (with `/admin/update/check`, `/admin/update/status`, `/admin/update/rollback`) exposes the same flow to scripts; rollback replays the swap against the `.backup` binary the update left behind — no download. Everywhere else — a manually started binary, a Windows console, Docker — the endpoints answer `501`: re-run your install route instead (deploy script, new download, or a fresh image pull).

### 4. Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}
```

## Usage

> **Note**: `/v1/*` endpoints are gated by the token store (the `auth_tokens` table in `devin-2api.db`, state dir): clients send `Authorization: Bearer <token>` or `X-Api-Key: <token>` matching an active token row. Tokens are managed only in the panel (`/web/tokens.html`) — plaintext is shown once at creation and the store keeps hashes; config carries no data-plane credential. An empty store means open access — only bind beyond loopback if the store gates access, or you are handing out your Devin quota to the network. Tokens carry an optional `class` (`fg` default / `bg` for unattended batch traffic) that changes rate-gate admission — see `docs/gate-classes.md`.

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
    "model": "swe-2-max",
    "input": "Hello"
  }'
```

Streaming (SSE):

```bash
curl -N http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "swe-2-max",
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
    "model": "swe-2-max",
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
| `devin.model`                                    | Devin chat model UID (e.g. `swe-2-max`)                                                                                                                                                                                                                                                                                                                                                         | Yes (no default in code)                                                          |
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
| `devin.warm_prefix_*`                            | Prefix-replay warming family — replays retained session prefixes on a cadence to renew the upstream prompt cache across long subagent waits; key list in `config.example.yaml`, mechanism in `docs/upstream-cache.md`                                                                                                                                                                           | `warm_prefix_enabled: false`                                                      |
| `devin.session_affinity_ttl_seconds`             | Sliding TTL for session→lane bindings (renewed on every hit); headers `X-Claude-Code-Session-Id`/`X-Session-ID`/`X-Session-Affinity`/`X-Conversation-Id`/`X-Thread-Id` pin a session to a lane                                                                                                                                                                                                  | `3600`                                                                            |
| `devin.quota_low_threshold_percent`              | Weekly-quota percent below which a lane is demoted behind healthy lanes for new sessions (bound sessions unaffected)                                                                                                                                                                                                                                                                            | `15`                                                                              |
| `devin.no_progress_timeout_seconds`              | No-progress watchdog once content has started flowing — upstream can compute tool-call arguments silently for 15–25 min sending only heartbeats, so this must stay well above that                                                                                                                                                                                                              | `2700`                                                                            |
| `devin.pre_event_no_progress_timeout_seconds`    | No-progress watchdog before the first decodable event; total pre-event silence is separately hard-capped at 180s (from first send, cumulative across stream reopens) — raising this can't extend that cap                                                                                                                                                                                       | `600`                                                                             |
| `debug.enabled`                                  | Record per-request debug payload into `devin-2api.db` (`debug_files`/`debug_chunks` tables) in the state dir                                                                                                                                                                                                                                                                                    | `false`                                                                           |
| `debug.retention_days`                           | Days to keep per-request debug records; `<=0` disables time-based cleanup                                                                                                                                                                                                                                                                                                                       | `14`                                                                              |
| `debug.max_total_mb`                             | Total debug payload cap (MB); evicts oldest request groups first                                                                                                                                                                                                                                                                                                                                | `1024`                                                                            |
| `debug.payload_hours`                            | Hours before large stage payloads (03/04/06/attachments) are stripped, keeping meta/error evidence                                                                                                                                                                                                                                                                                              | `24`                                                                              |
| `debug.keep_error_dirs`                          | Newest N failed request groups (with an `error.json` row) protected from size eviction                                                                                                                                                                                                                                                                                                          | `32`                                                                              |
| `debug.errors_only`                              | Keep debug payloads only for failed or suspect requests — clean completions drop their payload at finish (meta/error anchors and `logs` rows stay); panel key `debug_log_errors_only`, hot-changeable                                                                                                                                                                                           | `false`                                                                           |
| `debug.quota_interval_minutes`                   | Quota snapshot interval into the `quota_samples` table; `<=0` disables                                                                                                                                                                                                                                                                                                                          | `5`                                                                               |
| `debug.pprof_listen`                             | Separate listen address for the pprof/fgprof profiling endpoints (e.g. `127.0.0.1:6060`); unauthenticated — loopback only                                                                                                                                                                                                                                                                       | empty (disabled)                                                                  |
| `dashboard.password`                             | `/web` admin password; empty = no login required                                                                                                                                                                                                                                                                                                                                                | none                                                                              |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    accounts:
        - name: "main"
          token: "devin-session-token$..."
    model: "swe-2-max"

debug:
    enabled: false

dashboard:
    password: "" # /web login; empty = open


# Downstream /v1 tokens live only in the panel (/web/tokens.html) — config
# carries no data-plane credential. An empty token store means open access.
```

Notes:

- tokens are never written to logs (redacted as `<redacted>`);
- with no accounts configured the pool is empty: `/v1` requests fail fast with `unavailable` — add an account from the panel (`/web/accounts.html`) or `devin.accounts` + reload, no restart needed either way;
- `config.yaml` is gitignored — keep real tokens out of git anyway; pre-commit runs gitleaks to catch committed secrets.

## Troubleshooting

Every `/v1/*` response carries an `X-Request-Id` header (error bodies also carry `debug_ref`) naming the request's debug record — `GET /admin/debug-logs/{id}/file/error.json` shows the first failure point, and `logs/stderr.log` in the state dir has one summary line per request. Common symptoms:

- **`401`** — no matching token in the store (create one in `/web/tokens.html`), or upstream rejected the account credential; the lane re-resolves it and retries once — check lane status in `/web/accounts.html`
- **`unavailable`** — the account pool is empty; add an account in `/web/accounts.html`, or via `devin.accounts` + config reload
- **`429`** — the local rate gate fast-failed (`Retry-After` says when to retry) or upstream is rate-limiting; the `error_stage` column in the `logs` table (`rate_gate` vs `devin_connect`) distinguishes them
- **SSE stalls near ~300s** — the client's own total timeout, not the proxy's; a disconnected stream keeps running server-side and an identical retry replays it from the completion cache

The full symptom → layer → fix table lives in [`docs/upstream-debug-playbook.md`](docs/upstream-debug-playbook.md)（中文）.

## Documentation

- **User guide (install, clients, configuration, troubleshooting)**: [wncfht.github.io/devin2api](https://wncfht.github.io/devin2api/)
- **Architecture, supported API fields, proto extraction, and other technical details**: [Contributing guide](CONTRIBUTING.md)
- **Upstream protocol reverse-engineering notes, client setup, debugging playbook, deployment & toolchain**: [docs/](docs/README.md)（中文）
- **License**: [MIT](LICENSE)

## Acknowledgments

This project builds on [leookun/devin-2api](https://github.com/leookun/devin-2api) — thanks to the original authors for their work.
