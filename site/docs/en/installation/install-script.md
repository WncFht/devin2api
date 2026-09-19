# Install Script

The recommended route on Linux and macOS: one command, no clone, no root.

```bash
curl -sSL https://raw.githubusercontent.com/WncFht/devin2api/main/scripts/install.sh | bash
```

`install.sh` is a thin bootstrap: it fetches the deploy pipeline at the target tag and hands off to the platform deploy script. The result is a managed service — the binary lands in `~/.local/bin`, config and state in the platform directories (layout table in [Deploy Scripts](/installation/deploy-scripts)), and the process runs under `systemd --user` (Linux) or launchd (macOS) with a zero-downtime REUSEPORT handoff on every restart.

## Subcommands

| Command             | Effect                                            |
| ------------------- | ------------------------------------------------- |
| `install` (default) | Install or reinstall; `-v <tag>` pins a release   |
| `upgrade`           | Move to the latest release                        |
| `rollback <tag>`    | Reinstall a previous release                      |
| `status`            | Show installed / running / latest versions        |
| `list-versions`     | List available release tags                       |
| `uninstall`         | Remove service and binary (keeps config and logs) |

Run them as `bash install.sh <subcommand>` after saving the script, or re-run the `curl | bash` line with the subcommand appended: `curl -sSL .../install.sh | bash -s upgrade`.

## First run

On first run `config.yaml` is generated from `config.example.yaml` with a random `dashboard.password`, and you're prompted for the Devin credential — paste it, or leave it empty to boot with an empty pool and add accounts later from the panel (`/web/accounts.html`). Downstream `/v1` tokens are created in the panel (`/web/tokens.html`), never in config. To preset values before installing, `cp config.example.yaml config.yaml` next to the script's config location and edit beforehand — or edit the generated file after.

## Staying alive after logout (Linux)

A `systemd --user` service stops when your last login session ends. To keep it running while logged out:

```bash
loginctl enable-linger $USER
```

## Windows

`install.sh` covers Linux and macOS only. On Windows use the [PowerShell deploy script](/installation/deploy-scripts#windows) or the [prebuilt binary](/installation/prebuilt-binary).
