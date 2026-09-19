# Deploy Scripts

The same managed service as the [install script](/installation/install-script), driven from a clone — useful when you want the repo on disk, since the scripts treat it as home (they sync `config.yaml` into the platform config dir and keep a `logs` symlink in the repo pointing at the state dir).

```bash
git clone https://github.com/WncFht/devin2api && cd devin2api
bash scripts/deploy/deploy-linux.sh --release latest    # macOS: scripts/deploy/deploy.sh
```

`--release latest` downloads a sha256-verified prebuilt binary, then the script polls `/healthz` until the new version answers and probes `GET /v1/models` to confirm upstream auth works.

## Options

| Flag              | Effect                                                                      |
| ----------------- | --------------------------------------------------------------------------- |
| `--release <tag>` | Install the given release (or `latest`); also builds from source if no flag |
| `--check`         | Report installed / running / latest versions                                |
| `--uninstall`     | Remove service and binary (keeps config and logs)                           |
| `--no-restart`    | Swap the binary without restarting                                          |

First-run config generation works the same as the install script: `config.yaml` is created with a random `dashboard.password` and a credential prompt.

## Platform layout

| Platform | Supervisor                               | Layout                                                                                                       | Script                              |
| -------- | ---------------------------------------- | ------------------------------------------------------------------------------------------------------------ | ----------------------------------- |
| macOS    | launchd agent                            | bin `~/.local/bin` · config+state `~/Library/Application Support/devin-2api`                                 | `scripts/deploy/deploy.sh`          |
| Linux    | `systemd --user`                         | bin `~/.local/bin` · config `~/.config/devin-2api` · state `~/.local/state/devin-2api`                       | `scripts/deploy/deploy-linux.sh`    |
| Windows  | none — console, or NSSM / Task Scheduler | exe `%LOCALAPPDATA%\Programs\devin-2api` · config `%APPDATA%\devin-2api` · state `%LOCALAPPDATA%\devin-2api` | `scripts/deploy/deploy-windows.ps1` |

## Windows

```powershell
.\scripts\deploy\deploy-windows.ps1 -Release latest
```

The script generates `config.yaml` bound to loopback on a free port (avoids the firewall prompt and a bare exposure) and starts the instance in its own console window.

::: warning
Run it from a local interactive session, not over SSH — the job object kills the instance when the session ends.
:::

Windows has no service supervisor out of the box: the exe runs in a console, and Ctrl+C triggers the same graceful drain as SIGTERM elsewhere. Closing the window or `taskkill /F` kills it without draining. For unattended operation, register it with NSSM or Task Scheduler.
