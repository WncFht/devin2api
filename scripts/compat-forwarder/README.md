# compat-forwarder — `:3003` 兼容转发 shim

2026-09-18 生产实例迁到 archbox `:3033` 后，仍指向 `:3003` 的下游客户端由转发 shim 兜住，两机的实现与托管单元都收在这里（现网同款逐字副本）。

## 组件

- `tcp-forwarder.py`：无依赖 asyncio TCP 转发器，绑定与目标全部由环境变量注入（`BIND_HOST`/`BIND_PORT`/`TARGET_HOST`/`TARGET_PORT`/`BIND_RETRY_S`/`DIAL_RETRY_S`）。两条不变量：**不开 SO_REUSEPORT**——端口被真实例占用时永远绑不上，不会与 devin-2api 共绑分流；**后端拨号重试 `DIAL_RETRY_S` 秒**——目标重启期间已接入的客户端连接不丢，只是数据延后。
- `devin-2api-compat-3003.service`：archbox 侧 systemd --user unit，跑 `tcp-forwarder.py` 绑 `:3003` → `127.0.0.1:3033`，兜本机陈旧配置。
- `devin-2api-forwarder.py` + `com.fanghaotian.devin-2api-forwarder.plist`：fht-mba 侧 launchd agent。脚本是同一逻辑的硬编码变体（`TARGET_HOST=100.121.76.120`、绑 `*:3003`），与通用版只差常量取值；plist 绑 loopback/tailnet/LAN 入向全覆盖。

## 部署

archbox（现网已生效）：

```bash
install -m755 tcp-forwarder.py ~/.local/bin/
install -m644 devin-2api-compat-3003.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now devin-2api-compat-3003.service
```

fht-mba（现网已生效）：脚本放 `~/.local/bin/devin-2api-forwarder.py`，plist 放 `~/Library/LaunchAgents/` 后 `launchctl bootstrap gui/$UID` 加载；KeepAlive 托管。

## 验证

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3003/healthz   # 期望 200
```

注意 `:3003` 有 listener 并不代表 shim 健康——生产实例若回退到 `:3003` 也会应答；确认进程是 forwarder（`systemctl --user status` / `launchctl list`）再下结论。
