# devin-2api

> **English** | [中文](README.zh-CN.md)

devin-2api is a lightweight forwarding tool for the [OpenAI Responses API](https://platform.openai.com/docs/api-reference/responses). It exposes a standard `/v1/responses` endpoint and transparently forwards your LLM requests to Devin ([app.devin.ai](https://app.devin.ai/)) through an adapter — letting external programs call Devin's models through the standard OpenAI protocol.

## Features

- **OpenAI-compatible** `/v1/responses` endpoint — call Devin's models through the standard OpenAI Responses protocol
- **Streaming and non-streaming** responses (typed SSE / JSON)
- **Adapter-based design** — easily extended to new upstreams
- **Easy to deploy** — single static binary, public Docker image on [Docker Hub](https://hub.docker.com/r/devinuser123/devin-2api)
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

Local:

```bash
go run ./cmd/devin-2api -config config.yaml
```

Docker (image published on [Docker Hub](https://hub.docker.com/r/devinuser123/devin-2api)):

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  devinuser123/devin-2api --config /app/config.yaml
```

### 4. Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok"}
```

## Usage

> **Note**: `/v1/*` endpoints support optional API key authentication. Set `auth.api_key` in `config.yaml` to require clients to send `Authorization: Bearer <api_key>` or `X-Api-Key: <api_key>`. If left empty, the endpoints remain open (only expose them to trusted networks).

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

The request body follows the OpenAI Responses API (`input`, `instructions`, `tools`, `stream`, …). See the [Contributing guide](CONTRIBUTING.md) for the exact subset of fields supported.

## Configuration

Configuration is a YAML file loaded once at startup. Unknown fields are rejected.

| Field            | Description                                                                                                              | Required                                                                                                     |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------ |
| `server.listen`  | HTTP listen address                                                                                                      | Yes                                                                                                          |
| `devin.base_url` | Devin Connect service base URL                                                                                           | Yes, once `devin.token` is set (no default in code; `config.example.yaml` uses `https://server.codeium.com`) |
| `devin.token`    | Devin session token (`devin-session-token$...`)                                                                          | No — endpoint returns 503 until set                                                                          |
| `devin.model`    | Devin chat model UID (e.g. `glm-5-2`)                                                                                    | Yes, once `devin.token` is set (no default in code)                                                          |
| `debug.enabled`  | Write per-request debug logs under `logs/` next to the config file                                                       | No                                                                                                           |
| `auth.api_key`   | API key for `/v1/*` endpoints; empty disables auth. Clients may send `Authorization: Bearer <key>` or `X-Api-Key: <key>` | No                                                                                                           |

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    token: "devin-session-token$..."
    model: "glm-5-2"

debug:
    enabled: false

auth:
    # Set to a strong key to protect /v1/*; leave empty to keep endpoints open.
    api_key: ""
```

Notes:

- tokens are never written to logs (redacted as `<redacted>`);
- if `devin.token` is empty, `/v1/responses` returns `503 provider_configuration`;
- `config.yaml` is tracked by git — don't commit a real token (add it to `.gitignore` if needed).

## Documentation

- **Architecture, supported API fields, proto extraction, and other technical details**: [Contributing guide](CONTRIBUTING.md)
- **License**: [MIT](LICENSE)
