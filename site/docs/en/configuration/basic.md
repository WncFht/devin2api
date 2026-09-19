# Basic Configuration

devin-2api reads a single YAML file at startup. Unknown fields are rejected, so a typo fails loudly at boot instead of silently doing nothing.

## Where the file lives

Managed installs (install script / deploy scripts) place `config.yaml` in the platform config dir and pass `-config` explicitly — see the [layout table](/installation/deploy-scripts#platform-layout). A manually run binary resolves `-config` → `DEVIN2API_CONFIG` → `./config.yaml` → the platform default.

## Minimal file

```yaml
server:
    listen: ":8080"

devin:
    base_url: "https://server.codeium.com"
    accounts:
        - name: "main"
          api_key: "cog_..."
    model: "swe-2-max"

debug:
    enabled: true

dashboard:
    password: "" # /web login; empty = open
```

Even less works: `devin.accounts` may be an empty list — the service boots with an empty pool and you add accounts live from `/web/accounts.html`.

`config.example.yaml` in the repo ships this same skeleton with every optional key commented and its default documented — copy it and uncomment what you need rather than writing keys from memory.

## Applying changes

Most fields hot-reload: edit the file, then `POST /admin/config/reload` (admin Bearer). `server.listen` is the exception — it requires a restart. A failed validation keeps the previous config serving; a broken new file never replaces the last good one.

Settings changed from the panel (debug switches, retention, account edits) are stored as overrides in `devin-2api.db` and always win over file values — they re-apply after every reload.

## Secrets hygiene

Downstream `/v1` tokens never go in config — they live only in the panel. `config.yaml` does carry upstream credentials (`token`, `api_key`), so keep it out of git; the repo gitignores it and pre-commit runs gitleaks.
