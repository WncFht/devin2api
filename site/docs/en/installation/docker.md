# Docker

The image is published on [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api).

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  -v devin2api-state:/app/state \
  -e DEVIN2API_STATE_DIR=/app/state \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

::: warning Mount the state volume
The state volume keeps `devin-2api.db` (downstream tokens, request logs, quota samples) across container restarts. Without it each run starts with an empty token store — which means open `/v1` access to anyone who can reach the port.
:::

Prepare `config.yaml` before the first run — copy `config.example.yaml` and declare an account under `devin.accounts`, or leave the pool empty and add accounts from the panel afterward. The container listens on the port in `server.listen`; the example maps `8080`.
