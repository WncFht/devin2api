# devin-2api

> **English** | [中文](README.zh-CN.md)

devin-2api is a lightweight forwarding tool that exposes Devin ([app.devin.ai](https://app.devin.ai/)) behind OpenAI- and Anthropic-compatible endpoints — letting external programs call Devin's models through standard protocols.

## Features

- **Three API surfaces on one upstream** — `POST /v1/responses` (OpenAI Responses, incl. a WebSocket transport with multi-turn sessions for Codex-style clients), `POST /v1/chat/completions` (OpenAI Chat), `POST /v1/messages` (Anthropic Messages)
- **Streaming and non-streaming** responses (typed SSE / JSON)
- **Reasoning that round-trips** — thinking signatures are preserved and replayed across turns in each provider's native shape (`sealed`/`anthropic`/`openai`); surfaced as `encrypted_content` reasoning items on Responses, `redacted_thinking` on Anthropic, and `reasoning_content` on Chat
- **Faithful tool calling** — custom/freeform tool calls round-trip untouched; tool names and `tool_choice` are validated locally; strict call↔result re-pairing matches what upstream enforces; orphan tool results demote to text instead of failing the request
- **Resilient upstream streams** — failures before the first content byte (transport breaks, expired token reloaded from the credentials file, silent stalls, empty end_turn replies) are retried transparently; stream start is deferred so early upstream failures surface as real HTTP errors instead of SSE errors after a committed `200`
- **Normalized error contract** — upstream Connect codes map to proper HTTP status and protocol error types; rate limits become `429` + `Retry-After` parsed from the reset hint; every request carries `X-Request-Id`/`debug_ref` pointing at its debug directory
- **`/v1/models` capability flags** — context window, tool/thinking/image support surfaced from upstream model config
- **Admin panel at `/panel`** — request browser, usage/cost aggregation, quota tracking, process metrics, and per-request debug directories
- **Matches the real Devin CLI fingerprint** — request metadata replicates the CLI's client identity (`devin.client_*` makes it configurable when upstream bumps version gates)
- **Adapter-based design** — easily extended to new upstreams
- **Easy to deploy** — single static binary, public Docker image on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)
- **Optional debug logs** per request for troubleshooting

## Quick start

### 1. Get a Devin token

devin-2api authenticates to Devin with your Devin session token. On macOS, extract it from the Devin app's local state:

```bash
sqlite3 ~/Library/"Application Support"/Devin/User/globalStorage/state.vscdb \
  "SELECT json_extract(value, '$.apiKey') FROM ItemTable WHERE key='windsurfAuthStatus';"
```

The output is a token in the `devin-session-token$...` format.

### 2. Configure

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` and fill in your token (starting from `config.example.yaml`, you only need to fill in `devin.token` — the base URL and model are pre-filled as examples).

### 3. Run

Prebuilt binary (from [Releases](https://github.com/WncFht/devin2api/releases), `checksums.txt` attached for verification):

```bash
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-darwin-arm64
chmod +x devin-2api-darwin-arm64
./devin-2api-darwin-arm64 -config config.yaml
```

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

### 4. Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"v0.3.0","uptime_seconds":12,"debug_logging":false}
```

## Usage

> **Note**: `/v1/*` endpoints support optional API key authentication. Set `auth.api_key` in `config.yaml` to require clients to send `Authorization: Bearer <api_key>` or `X-Api-Key: <api_key>`. If left empty, the endpoints remain open (only expose them to trusted networks).

Endpoints:

- `POST /v1/responses` — OpenAI Responses (`GET` on the same path negotiates WebSocket transport)
- `POST /v1/chat/completions` — OpenAI Chat Completions
- `POST /v1/messages` — Anthropic Messages
- `GET /v1/models`, `GET /v1/models/{model}` — upstream model list with capability flags
- `GET /panel` — admin panel (request browser, usage, quota, process stats); `dashboard.password` protects it

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
| `devin.token`                                    | Devin session token (`devin-session-token$...`)                                                                          | No — endpoint returns 503 until set                                                                          |
| `devin.model`                                    | Devin chat model UID (e.g. `glm-5-2`)                                                                                    | Yes, once `devin.token` is set (no default in code)                                                          |
| `devin.aliases`                                  | Client model name to upstream UID map (e.g. `swe-2: swe-2-max`)                                                          | none                                                                                                         |
| `devin.client_name`/`client_version`/`client_os` | Client identity sent in upstream metadata (bump `client_version` when upstream gates a model on a newer CLI)             | `chisel` / `3000.2.17` / `mac`                                                                               |
| `devin.proxy`                                    | Upstream proxy URL (`http(s)://`, `socks5(h)://`); empty = direct / env vars                                             | none                                                                                                         |
| `devin.force_http1`                              | Per-request TCP connections to upstream (avoids HTTP/2 stream serialization)                                             | `true`                                                                                                       |
| `debug.enabled`                                  | Write per-request debug logs under `logs/` next to the config file                                                       | `false`                                                                                                      |
| `debug.retention_days`                           | Days to keep request log dirs; `<=0` disables time-based cleanup                                                         | `14`                                                                                                         |
| `debug.max_total_mb`                             | Total `logs/` size cap; evicts oldest dirs first                                                                         | `1024`                                                                                                       |
| `debug.payload_hours`                            | Hours before large stage files (03/04/06/attachments) are stripped, keeping meta/error evidence                          | `24`                                                                                                         |
| `debug.keep_error_dirs`                          | Newest N failed dirs (with `error.json`) protected from size eviction                                                    | `32`                                                                                                         |
| `debug.quota_interval_minutes`                   | Quota snapshot interval into `logs/quota.jsonl`; `<=0` disables                                                          | `10`                                                                                                         |
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
