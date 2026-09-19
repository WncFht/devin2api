# Other Clients

Any client that speaks OpenAI Chat Completions, OpenAI Responses, or Anthropic Messages can use devin-2api. Two values are always needed: the proxy address (`server.listen`, examples here use `http://127.0.0.1:8080`) and a downstream token from `/web/tokens.html`.

## Generic SDKs

- **Anthropic Messages** — point the SDK's base URL at `http://127.0.0.1:8080` and the API key at your downstream token; requests land on `POST /v1/messages`.
- **OpenAI** — base URL `http://127.0.0.1:8080/v1`, API key = downstream token. `chat.completions.create` hits `/v1/chat/completions`; Responses-API clients hit `/v1/responses`.

Send the token as `Authorization: Bearer <token>` or `X-Api-Key: <token>`.

## pi

`~/.pi/agent/models.json`:

```json
{
    "providers": {
        "devin": {
            "baseUrl": "http://127.0.0.1:8080",
            "api": "anthropic-messages",
            "apiKey": "<downstream token>",
            "models": [
                {
                    "id": "swe-2-max",
                    "contextWindow": 262000,
                    "maxTokens": 32768,
                    "reasoning": true,
                    "input": ["text", "image"]
                }
            ]
        }
    }
}
```

Run with `pi --provider devin --model swe-2-max`, or pick the model interactively via `/model`. pi has no permission system — tools all execute by default; narrow with `--tools read,grep,find,ls` (allowlist), `--no-builtin-tools`, or `--no-tools`.

## kimi-code

`~/.kimi-code/config.toml`:

```toml
default_model = "swe-2-max"

[providers.devin]
type = "anthropic"
base_url = "http://127.0.0.1:8080"
api_key = "<downstream token>"

[models."swe-2-max"]
provider = "devin"
model = "swe-2-max"
max_context_size = 262000
capabilities = ["thinking", "tool_use", "image_in"]
```

Unknown model names need an explicit `capabilities` list or tool calling never engages. Valid values: `thinking` / `always_thinking` / `tool_use` / `image_in` / `video_in`. `type = "openai"` (chat completions) and `"openai_responses"` also work. Permission modes and `[[permission.rules]]` are documented upstream — `default_permission_mode` accepts `manual` / `yolo` / `auto`.

## Shared notes

- **Context windows** — declare the real 262000 wherever the client asks; a misdeclared window breaks auto-compaction timing (see each client's section).
- **System-prompt fingerprints** — upstream content policy can reject a client's identity prompt (`permission_denied`). devin-2api's sanitizer covers Claude Code's fingerprint; pi and kimi-code masquerade as Claude Code, so the same rules cover them.
- **Tool-call pairing** — upstream requires call→result adjacency; the proxy re-pairs automatically and clients don't notice.
- **Prefer `http://[::1]:<port>` for the base URL** when the service binds `*`: `127.0.0.1` can be silently shadowed by an IDE's IPv4 port forward (connect succeeds, zero bytes flow), and `localhost` depends on resolver order.
