# Troubleshooting

Every `/v1/*` response carries an `X-Request-Id` header (error bodies also carry `debug_ref`) naming the request's debug record. That ID is the entry point for everything below.

## Finding the first failure

With request logging on (`debug.enabled`), each request leaves a record in `devin-2api.db`. Two quick reads:

1. `GET /admin/logs?q=<request-id>` → the `logs` row: status, `error_stage` (which layer failed), `error_message`.
2. `GET /admin/debug-logs/{id}/file/error.json` → the first failure point inside the pipeline (`{id}` is the `id` column from the logs row).

`logs/stderr.log` in the state dir has one summary line per request. The full symptom → layer → fix playbook lives in the repo at `docs/upstream-debug-playbook.md` (Chinese).

## Common symptoms

| Symptom                           | Likely cause                                                                | What to do                                                                                                                                               |
| --------------------------------- | --------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401`                             | No matching token in the store, or upstream rejected the account credential | Create a token in `/web/tokens.html`; check lane status in `/web/accounts.html` — the lane re-resolves credentials and retries once on `unauthenticated` |
| `unavailable`                     | Account pool is empty                                                       | Add an account in `/web/accounts.html`, or via `devin.accounts` + config reload                                                                          |
| `429` + `Retry-After`             | Local rate gate fast-failed, or upstream is rate-limiting                   | `error_stage` distinguishes them: `rate_gate` = local latch (retry when the header says), `devin_connect` = upstream rejection                           |
| SSE stalls near ~300s             | The client's own total timeout, not the proxy's                             | A disconnected stream keeps running server-side; an identical retry re-attaches from the completion cache                                                |
| `prompt-too-long`                 | Client's declared context window sits above the real 262000                 | Set the client's window to 262000 and its auto-compact threshold below it — see the client pages                                                         |
| `permission_denied`               | Upstream content policy rejected a client fingerprint or the prompt itself  | The sanitizer covers Claude Code's fingerprint; other content can trip policy on its own — check `error.json` for the upstream message                   |
| `previous_response_id` rejected   | The proxy is stateless over HTTP                                            | Send the full conversation on every request, or use the WebSocket transport which keeps sessions per connection                                          |
| Connect succeeds, zero bytes flow | `127.0.0.1` shadowed by an IDE IPv4 port forward                            | Point the client at `http://[::1]:<port>` when the service binds `*`                                                                                     |
| `501` from `/admin/update`        | The instance isn't managed                                                  | Self-update only works on install-script / deploy-script installs; upgrade by re-running your install route                                              |

## Still stuck

- `GET /admin/runtime-metrics` shows live gate state, per-lane health, and process metrics.
- `/admin/debug-logs/{id}` exposes the full stage records (`01-http-request` through `06-http-response`) — the pipeline from client bytes to upstream wire and back.
- Open an issue at [GitHub](https://github.com/WncFht/devin2api/issues) with the `X-Request-Id` and `error.json` content (payloads aren't redacted — check `meta.json` before sharing).
