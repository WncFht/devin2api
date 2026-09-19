# Docker

镜像发布在 [GHCR](https://github.com/WncFht/devin2api/pkgs/container/devin2api)。

```bash
docker run --rm -p 8080:8080 \
  -v "$PWD/config.yaml:/app/config.yaml" \
  -v devin2api-state:/app/state \
  -e DEVIN2API_STATE_DIR=/app/state \
  ghcr.io/wncfht/devin2api --config /app/config.yaml
```

::: warning 状态卷必须挂
状态卷让 `devin-2api.db`（下游令牌仓、请求日志、配额快照）跨容器重启存活。不挂卷则每次运行从空仓起步——意味着任何能碰到端口的人都能直接用 `/v1`。
:::

首跑前准备好 `config.yaml`——复制 `config.example.yaml` 并在 `devin.accounts` 声明账号，或空池起跑、事后在面板加号。容器内监听地址由 `server.listen` 决定，示例映射 8080。
