# Admin Panel

The panel at `/web` is the single management surface — request browser, usage, quota, tokens, pool lanes, model registry, and settings. It is also the only place downstream `/v1` tokens exist.

## Logging in

`dashboard.password` in `config.yaml` sets the admin password. Managed installs generate a random one on first run — it is printed during install and stored in `config.yaml`. An empty password means open access: fine on loopback, dangerous the moment `server.listen` binds a real interface.

Everything is Bearer-based, no cookies: `Authorization: Bearer <password>` works for `/admin/*` and `/dashboard/*` endpoints, and `POST /login` returns a token for the panel UI. A downstream token can also log in as a read-only `api_token` identity scoped to its own rows.

## Pages

| Page                 | What it does                                                                                               |
| -------------------- | ---------------------------------------------------------------------------------------------------------- |
| `/web/index.html`    | Request browser — per-request status, latency, tokens, error stage; drill into debug payloads              |
| `/web/tokens.html`   | Downstream token management — create/revoke, per-token RPM/concurrency/cost-window limits, `fg`/`bg` class |
| `/web/accounts.html` | Pool lanes — add/edit/remove accounts, health and quota per lane, failover attribution                     |
| `/web/settings.html` | Debug switches, retention policies, and the version-update card on managed installs                        |

## Downstream tokens

Clients authenticate to `/v1` with tokens created here. Plaintext shows once at creation; the store keeps only hashes. Tokens can carry per-token limits (RPM, concurrency, cost windows, allowed models) and a `class` — `fg` (default) for interactive traffic, `bg` for unattended batch jobs that tolerate longer gate queues.

::: warning Empty store = open access
When the token store has no rows, `/v1` accepts every request. Only bind `server.listen` beyond loopback once at least one token exists — otherwise you are handing out your Devin quota to the network.
:::

## Model registry

The registry controls what `/v1/models` and request routing do with each name: disable a model (requests get `model_disabled`), or redirect one name to another before `devin.aliases` resolution.

## Self-update

On managed installs the version-update card on `/web/settings.html` checks for a new release, downloads it, verifies sha256, swaps the binary, and restarts across the zero-downtime handoff — progress polls across the process switch. See [Upgrading](/installation/upgrading).

## Scripting the panel

`/admin/*` endpoints accept the admin Bearer token for automation: `/admin/logs` (filterable request rows + export), `/admin/runtime-metrics` (gate/pool/process state), `/admin/config` (redacted effective config) and `/admin/config/reload`, `/admin/update*`, `/admin/debug-logs/{id}` (per-request payloads). `GET /admin/api` returns the endpoint catalog.
