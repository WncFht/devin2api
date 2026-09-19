# Upgrading

How you upgrade depends on who manages the process.

## Managed installs (install script / deploy scripts)

Services installed by `install.sh` or the deploy scripts can update themselves from the panel: `/web/settings.html` carries a version-update card — check → download → sha256 verify → swap → restart across the same REUSEPORT handoff, so nothing drops and the panel polls progress across the process switch.

The same flow is exposed to scripts:

| Endpoint                      | Effect                                                                      |
| ----------------------------- | --------------------------------------------------------------------------- |
| `POST /admin/update`          | Update to latest (or `{"tag": "vX.Y.Z"}`)                                   |
| `POST /admin/update/check`    | Report installed / running / latest versions                                |
| `GET /admin/update/status`    | Poll progress of an in-flight update                                        |
| `POST /admin/update/rollback` | Roll back to the `.backup` binary the last update left behind — no download |

All take the admin Bearer token (`dashboard.password`). Errors: `409` if an update is already in flight, `404` on rollback with no `.backup`, `400` on a bad tag.

From the shell you can also just re-run your install route — `install.sh upgrade` (or `rollback <tag>`), or `deploy-*.sh --release latest` again.

## Everything else

On unmanaged setups — a manually started binary, a Windows console, Docker — the update endpoints answer `501`. Upgrade by re-running your install route:

| Route           | Upgrade command                                     |
| --------------- | --------------------------------------------------- |
| Prebuilt binary | Download the new asset and restart the process      |
| Docker          | `docker pull ghcr.io/wncfht/devin2api` and recreate |
| From source     | `git pull` and rebuild / restart                    |

The state directory (`devin-2api.db`) survives every upgrade path — tokens, logs, quota history, and detached completion cache entries carry over.
