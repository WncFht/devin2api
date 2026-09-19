# Quick Start

Get from zero to a working `/v1` endpoint in four steps. This path covers Linux and macOS with the managed install; on Windows use the [deploy script](/installation/deploy-scripts) or the [prebuilt binary](/installation/prebuilt-binary) instead.

## Prerequisites

- A Devin account ([app.devin.ai](https://app.devin.ai/)) you are allowed to use this way.
- Linux or macOS. The install script needs no root and no clone.

## 1. Get a credential

Each upstream account needs one credential. The lowest-maintenance option is a durable Devin platform key:

- **`api_key` (recommended)** — a `cog_...` key issued at [app.devin.ai](https://app.devin.ai/) → Settings → API keys. It carries no built-in expiry: whenever upstream reports `unauthenticated` the lane mints itself a fresh session token, so an `api_key`-only account self-heals indefinitely.

Other sources — a literal `devin-session-token$...`, or the Devin CLI `credentials.toml` file — are covered in [Account Pool](/configuration/account-pool).

## 2. Install

```bash
curl -sSL https://raw.githubusercontent.com/WncFht/devin2api/main/scripts/install.sh | bash
```

One command puts the binary in `~/.local/bin`, config and state in the platform directories, and starts the service under `systemd --user` (Linux) or launchd (macOS). On first run it generates `config.yaml` with a random `dashboard.password` and prompts for your credential — paste the `cog_...` key, or press Enter to start with an empty pool and add the account from the panel later.

Details and subcommands (`upgrade`, `rollback`, `status`, `uninstall`): [Install Script](/installation/install-script).

## 3. Create a downstream token

Clients authenticate to `/v1` with a token from the panel — config carries no data-plane credential. Open `http://localhost:8080/web/tokens.html`, log in with the generated `dashboard.password` (printed at install; also in `config.yaml`), and create a token. The plaintext shows once; the store keeps only hashes.

If you skipped the credential prompt, add your Devin account at `/web/accounts.html` — no restart needed.

## 4. Point your client at it

Claude Code example (`~/.claude/settings.json`):

```json
{
    "env": {
        "ANTHROPIC_BASE_URL": "http://127.0.0.1:8080",
        "ANTHROPIC_AUTH_TOKEN": "<downstream token>",
        "ANTHROPIC_MODEL": "swe-2-max",
        "ANTHROPIC_SMALL_FAST_MODEL": "swe-2-max",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "262000",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "230000"
    }
}
```

Per-client pages: [Claude Code](/clients/claude-code), [Codex](/clients/codex), [other clients](/clients/other-clients).

## Verify

```bash
curl http://localhost:8080/healthz
# {"status":"ok","version":"...","uptime_seconds":12,"debug_logging":false}

curl http://localhost:8080/v1/responses \
  -H "Authorization: Bearer <downstream token>" \
  -H "Content-Type: application/json" \
  -d '{"model": "swe-2-max", "input": "Hello"}'
```

If something is off, every `/v1` response carries an `X-Request-Id` that names its debug record — see [Troubleshooting](/troubleshooting).
