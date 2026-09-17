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
- **Rate-limit gate** — upstream `resource_exhausted` trips a local cooldown latch: queued requests wait briefly then fast-fail `429` + `Retry-After` instead of hammering a limited upstream, drip-released probes detect recovery, and latch state persists across restarts (`logs/gate-state.json`). An optional `max_rpm` token bucket shapes outbound pressure before the latch ever trips
- **Normalized error contract** — upstream error codes map to proper HTTP status and per-protocol error types; rate limits become `429` + `Retry-After`; with request logging on (`debug.enabled`, on in the shipped `config.example.yaml`) every request carries `X-Request-Id`/`debug_ref` pointing at its debug directory
- **`/v1/models` capability flags** — context window, tool/thinking/image support surfaced from the upstream model catalog
- **Admin panel at `/web`** — request browser, usage/cost aggregation, quota tracking, process metrics, per-request debug directories, and a redacted config view with hot reload for most fields
- **Easy to deploy** — single static binary, public Docker image on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)

## Quick start

### 1. Provide a Devin token

devin-2api authenticates to Devin with your Devin session token (`devin-session-token$...`). If `devin.token` is left empty in `config.yaml`, the adapter discovers one automatically, in order:

1. `DEVIN_TOKEN` or `WINDSURF_API_KEY` environment variable;
2. the Devin CLI credential file — `~/.local/share/devin/credentials.toml` on macOS/Linux; `%APPDATA%\devin\credentials.toml` (then `%LOCALAPPDATA%\devin\credentials.toml`) on Windows. The Windows CLI is not distributed standalone but ships inside the [Windsurf desktop app](https://devin.ai/download) — after installing it, `& "C:\Program Files\Windsurf\resources\app\extensions\windsurf\devin\bin\devin.exe" auth login` produces the file above.

On macOS you can also extract the token from the Devin app's local state:

```bash
sqlite3 ~/Library/"Application Support"/Devin/User/globalStorage/state.vscdb \
  "SELECT json_extract(value, '$.apiKey') FROM ItemTable WHERE key='windsurfAuthStatus';"
```

Tokens expire. When upstream answers `unauthenticated`, the adapter re-reads the same source chain — so if the Devin CLI refreshes `credentials.toml`, the proxy heals itself without a restart.

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` and fill in your token (starting from `config.example.yaml`, you only need to fill in `devin.token` — the base URL and model are pre-filled as examples).

### 3. Run

Prebuilt binary (from [Releases](https://github.com/WncFht/devin2api/releases), `checksums.txt` attached for verification). Assets are named `devin-2api-{darwin,linux}-{amd64,arm64}`; Windows ships as same-named `.zip` bundles (exe + `config.example.yaml` + LICENSE):

```bash
# Linux shown; on macOS use devin-2api-darwin-arm64 or -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

On Windows: unzip `devin-2api-windows-amd64.zip`, edit `config.yaml` (the token may stay empty — step 1 item 2 covers the Windsurf-bundled `devin.exe` that produces the credential file), then run `devin-2api.exe -config config.yaml` in a console. Ctrl+C triggers the same graceful drain; closing the window and `taskkill /F` do not — Windows offers no graceful kill for console processes.

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

On first run `config.yaml` is generated from `config.example.yaml` with a random `auth.api_key`/`dashboard.password`, and you're prompted for the Devin token (left empty it falls back to auto-discovery); to preset values, `cp config.example.yaml config.yaml` and edit beforehand. `--check` reports installed/running/latest versions; `--uninstall` removes the service and binary while keeping config and logs.

On Linux, run `loginctl enable-linger $USER` if the service must outlive your login session.

### 4. Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}
```

## Usage

> **Note**: `/v1/*` endpoints are gated by the token store (`auth_tokens.json` in the state dir): clients send `Authorization: Bearer <token>` or `X-Api-Key: <token>` matching an active token row. `auth.api_key` in `config.yaml` is only a seed source — it is written in as a normal token row on boot and every config reload. An empty store means open access — only bind beyond loopback if the store gates access, or you are handing out your Devin quota to the network.

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

| Field                                            | Description                                                                                                                                                                                       | Required / Default                                                                |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------- |
| `server.listen`                                  | HTTP listen address                                                                                                                                                                               | Yes                                                                               |
| `server.max_concurrency`                         | Max concurrent `/v1/*` requests                                                                                                                                                                   | `1024`                                                                            |
| `devin.base_url`                                 | Devin Connect service base URL                                                                                                                                                                    | Yes (no default in code; `config.example.yaml` uses `https://server.codeium.com`) |
| `devin.token`                                    | Devin session token (`devin-session-token$...`); empty = discover from env / credentials file                                                                                                     | No — endpoints return 401 until a token is discoverable                           |
| `devin.model`                                    | Devin chat model UID (e.g. `glm-5-2`)                                                                                                                                                             | Yes (no default in code)                                                          |
| `devin.aliases`                                  | Client model name → upstream UID map (`swe-2: swe-2-max`); match order exact → case-insensitive → `"*"` catch-all; aliases appear in `/v1/models` with `alias_of`                                 | none                                                                              |
| `devin.client_name`/`client_version`/`client_os` | Client identity sent in upstream metadata                                                                                                                                                         | `chisel` / `3000.2.17` / `mac`                                                    |
| `devin.proxy`                                    | Upstream proxy URL (`http(s)://`, `socks5(h)://`); empty = direct / env vars                                                                                                                      | none                                                                              |
| `devin.force_http1`                              | Per-request TCP connections to upstream (avoids HTTP/2 stream serialization)                                                                                                                      | `true`                                                                            |
| `devin.max_rpm`                                  | Message rate limit to upstream (msgs/min, token bucket); `<=0` unlimited — the 429 cooldown latch applies either way                                                                              | `0` (unlimited; `config.example.yaml` ships `80`)                                 |
| `devin.gate_max_hold_seconds`                    | Max queued wait inside a cooldown latch before fast-fail `429` + `Retry-After`                                                                                                                    | `15`                                                                              |
| `devin.gate_drip_interval_seconds`               | Probe release interval inside a latch — paces upstream arrivals and unlatch detection while limited                                                                                               | `8`                                                                               |
| `devin.gate_default_latch_seconds`               | Fallback latch duration when upstream `resource_exhausted` doesn't declare a reset time                                                                                                           | `60`                                                                              |
| `devin.gate_window_offset_seconds`               | Estimated position of the upstream minute-bucket boundary inside the local minute (which second it falls on)                                                                                      | `0` (local `:00`; observed boundary is local `:59`)                               |
| `devin.gate_window_guard_seconds`                | Dead zone on both sides of the estimated bucket boundary — requests inside it sleep until the next window                                                                                         | `2`                                                                               |
| `debug.enabled`                                  | Write per-request debug logs under `logs/` next to the config file                                                                                                                                | `false`                                                                           |
| `debug.retention_days`                           | Days to keep request log dirs; `<=0` disables time-based cleanup                                                                                                                                  | `14`                                                                              |
| `debug.max_total_mb`                             | Total `logs/` size cap; evicts oldest dirs first                                                                                                                                                  | `1024`                                                                            |
| `debug.payload_hours`                            | Hours before large stage files (03/04/06/attachments) are stripped, keeping meta/error evidence                                                                                                   | `24`                                                                              |
| `debug.keep_error_dirs`                          | Newest N failed dirs (with `error.json`) protected from size eviction                                                                                                                             | `32`                                                                              |
| `debug.quota_interval_minutes`                   | Quota snapshot interval into `logs/quota.jsonl`; `<=0` disables                                                                                                                                   | `5`                                                                               |
| `debug.pprof_listen`                             | Separate listen address for the pprof/fgprof profiling endpoints (e.g. `127.0.0.1:6060`); unauthenticated — loopback only                                                                         | empty (disabled)                                                                  |
| `dashboard.password`                             | `/web` admin password; empty = no login required                                                                                                                                                  | none                                                                              |
| `auth.api_key`                                   | Seed credential written into the token store on boot/reload; `/v1/*` admission is decided by the store (empty store = open). Clients send `Authorization: Bearer <token>` or `X-Api-Key: <token>` | none                                                                              |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
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
- if `devin.token` is empty, requests still go upstream and return `401` with type `authentication_error` — once a token shows up in any discovery source the next request succeeds, no restart needed;
- `config.yaml` is gitignored — keep real tokens out of git anyway; pre-commit runs gitleaks to catch committed secrets.

## Documentation

- **Architecture, supported API fields, proto extraction, and other technical details**: [Contributing guide](CONTRIBUTING.md)
- **Upstream protocol reverse-engineering notes, client setup, debugging playbook, deployment & toolchain**: [docs/](docs/README.md)（中文）
- **License**: [MIT](LICENSE)

## Acknowledgments

This project builds on [leookun/devin-2api](https://github.com/leookun/devin-2api) — thanks to the original authors for their work.
