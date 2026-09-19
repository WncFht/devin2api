# Prebuilt Binary

Download a release asset and run it directly — no service manager involved.

Assets on [Releases](https://github.com/WncFht/devin2api/releases) are named `devin-2api-{darwin,linux}-{amd64,arm64}`; Windows ships `devin-2api-windows-{amd64,arm64}.zip` bundles (exe + `config.example.yaml` + LICENSE). `checksums.txt` is attached for verification.

## Linux / macOS

```bash
# Linux shown; on macOS use devin-2api-darwin-arm64 or -darwin-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/devin-2api-linux-amd64
curl -fLO https://github.com/WncFht/devin2api/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing   # expect: devin-2api-linux-amd64: OK
chmod +x devin-2api-linux-amd64
./devin-2api-linux-amd64 -config config.yaml
```

## Windows

Unzip the bundle, edit `config.yaml` (an account may carry only `credentials_file` — see [Account Pool](/configuration/account-pool) for the Windsurf-bundled `devin.exe` that produces the credential file), then run in a console:

```powershell
.\devin-2api.exe -config config.yaml
```

Ctrl+C triggers the same graceful drain as SIGTERM on other platforms. Closing the window or `taskkill /F` does not — Windows offers no graceful kill for console processes.

## Path resolution

The binary resolves paths by platform convention:

- **Config file** — `-config` flag → `DEVIN2API_CONFIG` → `./config.yaml` (if present) → platform default from the [layout table](/installation/deploy-scripts#platform-layout)
- **State directory** — `-state-dir` flag → `DEVIN2API_STATE_DIR` → platform default

So `unzip && ./devin-2api.exe` picks up `./config.yaml` automatically, while a managed install always passes both flags explicitly.
