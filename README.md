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
- **Normalized error contract** — upstream error codes map to proper HTTP status and per-protocol error types; rate limits become `429` + `Retry-After`; every request carries `X-Request-Id`/`debug_ref` pointing at its debug directory
- **`/v1/models` capability flags** — context window, tool/thinking/image support surfaced from the upstream model catalog
- **Admin panel at `/panel`** — request browser, usage/cost aggregation, quota tracking, process metrics, and per-request debug directories
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

| Platform | Supervisor                               | Runtime dir (binary + config + logs)       | Install / upgrade         |
| -------- | ---------------------------------------- | ------------------------------------------ | ------------------------- |
| macOS    | launchd agent                            | `~/Library/Application Support/devin-2api` | `scripts/deploy.sh`       |
| Linux    | `systemd --user`                         | `~/.local/share/devin-2api`                | `scripts/deploy-linux.sh` |
| Windows  | none — console, or NSSM / Task Scheduler | alongside the exe                          | download zip, run exe     |

Both deploy scripts install or upgrade in one shot (`--release latest` fetches a prebuilt binary) and verify `/healthz` reports the new version. They treat the repo as home — syncing `config.yaml` into the runtime dir and keeping a `logs` symlink inside the repo — so clone first, then run:

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
cp config.example.yaml config.yaml   # edit as needed; token may stay empty for auto-discovery
bash scripts/deploy-linux.sh --release latest    # macOS: scripts/deploy.sh
```

On Linux, run `loginctl enable-linger $USER` if the service must outlive your login session.

### 4. Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}
```

## Usage

> **Note**: `/v1/*` endpoints support optional API key authentication. Set `auth.api_key` in `config.yaml` to require clients to send `Authorization: Bearer <api_key>` or `X-Api-Key: <api_key>`. If left empty, the endpoints remain open — only bind beyond loopback if you also set a key, or you are handing out your Devin quota to the network.

Endpoints:

- `POST /v1/responses` — OpenAI Responses (`GET` on the same path negotiates WebSocket transport)
- `POST /v1/chat/completions` — OpenAI Chat Completions
- `POST /v1/messages` — Anthropic Messages
- `GET /v1/models`, `GET /v1/models/{model}` — upstream model catalog with capability flags
- `GET /panel` — admin panel (request browser, usage, quota, process stats); `dashboard.password` protects it

The proxy is **stateless**: every HTTP request must carry the full conversation (`previous_response_id` is accepted but ignored — there is no server-side response store). Over the WebSocket transport, multi-turn sessions are maintained per connection and incremental inputs are expanded into full transcripts transparently.

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

| Field                                            | Description                                                                                                              | Required / Default                                                                                           |
| ------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------ |
| `server.listen`                                  | HTTP listen address                                                                                                      | Yes                                                                                                          |
| `server.max_concurrency`                         | Max concurrent `/v1/*` requests                                                                                          | `1024`                                                                                                       |
| `devin.base_url`                                 | Devin Connect service base URL                                                                                           | Yes, once `devin.token` is set (no default in code; `config.example.yaml` uses `https://server.codeium.com`) |
| `devin.token`                                    | Devin session token (`devin-session-token$...`); empty = discover from env / credentials file                            | No — endpoint returns 503 until set                                                                          |
| `devin.model`                                    | Devin chat model UID (e.g. `glm-5-2`)                                                                                    | Yes, once `devin.token` is set (no default in code)                                                          |
| `devin.aliases`                                  | Client model name to upstream UID map (e.g. `swe-2: swe-2-max`)                                                          | none                                                                                                         |
| `devin.client_name`/`client_version`/`client_os` | Client identity sent in upstream metadata                                                                                | `chisel` / `3000.2.17` / `mac`                                                                               |
| `devin.proxy`                                    | Upstream proxy URL (`http(s)://`, `socks5(h)://`); empty = direct / env vars                                             | none                                                                                                         |
| `devin.force_http1`                              | Per-request TCP connections to upstream (avoids HTTP/2 stream serialization)                                             | `true`                                                                                                       |
| `debug.enabled`                                  | Write per-request debug logs under `logs/` next to the config file                                                       | `false`                                                                                                      |
| `debug.retention_days`                           | Days to keep request log dirs; `<=0` disables time-based cleanup                                                         | `14`                                                                                                         |
| `debug.max_total_mb`                             | Total `logs/` size cap; evicts oldest dirs first                                                                         | `1024`                                                                                                       |
| `debug.payload_hours`                            | Hours before large stage files (03/04/06/attachments) are stripped, keeping meta/error evidence                          | `24`                                                                                                         |
| `debug.keep_error_dirs`                          | Newest N failed dirs (with `error.json`) protected from size eviction                                                    | `32`                                                                                                         |
| `debug.quota_interval_minutes`                   | Quota snapshot interval into `logs/quota.jsonl`; `<=0` disables                                                          | `5`                                                                                                          |
| `dashboard.password`                             | `/panel` admin password; empty = no login required                                                                       | none                                                                                                         |
| `auth.api_key`                                   | API key for `/v1/*` endpoints; empty disables auth. Clients may send `Authorization: Bearer <key>` or `X-Api-Key: <key>` | none (open)                                                                                                  |

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
    password: "" # /panel login; empty = open

auth:
    # Set to a strong key to protect /v1/*; leave empty to keep endpoints open.
    api_key: ""
```

Notes:

- tokens are never written to logs (redacted as `<redacted>`);
- if `devin.token` is empty, `/v1/*` endpoints return `503 provider_configuration`;
- `config.yaml` is gitignored — keep real tokens out of git anyway; pre-commit runs gitleaks to catch committed secrets.

## Documentation

- **Architecture, supported API fields, proto extraction, and other technical details**: [Contributing guide](CONTRIBUTING.md)
- **License**: [MIT](LICENSE)

## Acknowledgments

This project builds on [leookun/devin-2api](https://github.com/leookun/devin-2api) — thanks to the original authors for their work.
