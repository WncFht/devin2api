# compat-forwarder — `:3003` 兼容转发 shim（已退役）

**2026-09-23 退役**：全量客户端改指 `:3033` 直连后两侧 shim 同步拆除（拆除前 `:3003` 零在途连接）。以下仅留档。

生产实例迁到新机器 `:3033` 后，仍指向 `:3003` 的下游客户端由转发 shim 兜住，两机的实现与托管单元都收在这里（部署目标的机器专属取值见 notes 私有拓扑备忘）。

## 组件

- `tcp-forwarder.py`：无依赖 asyncio TCP 转发器，目标与端口全部由环境变量注入（`BIND_PORT`/`TARGET_HOST`/`TARGET_PORT`/`BIND_RETRY_S`/`DIAL_RETRY_S`），监听地址恒绑通配（`::`/`0.0.0.0`）。两条不变量：**不开 SO_REUSEPORT**——端口被真实例占用时永远绑不上，不会与 devin-2api 共绑分流；**后端拨号重试 `DIAL_RETRY_S` 秒**——目标重启期间已接入的客户端连接不丢，只是数据延后。
- `devin-2api-compat-3003.service`：生产机侧 systemd --user unit，跑 `tcp-forwarder.py` 绑 `:3003` → `127.0.0.1:3033`，兜本机陈旧配置。
- `devin-2api-forwarder.py` + `com.devin2api.forwarder.plist`：旧 Mac 侧 launchd agent。脚本是同一逻辑的硬编码变体（`TARGET_HOST` 填生产网关 tailnet 地址、绑 `*:3003`），与通用版只差常量取值；plist 绑 loopback/tailnet/LAN 入向全覆盖。

## 部署

Linux 侧（systemd --user）：

```bash
install -m755 tcp-forwarder.py ~/.local/bin/
install -m644 devin-2api-compat-3003.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now devin-2api-compat-3003.service
```

macOS 侧：脚本放 `~/.local/bin/devin-2api-forwarder.py`，plist 放 `~/Library/LaunchAgents/` 后 `launchctl bootstrap gui/$UID` 加载；KeepAlive 托管。

## 验证

```bash
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:3003/healthz   # 期望 200
```

注意 `:3003` 有 listener 并不代表 shim 健康——生产实例若回退到 `:3003` 也会应答；确认进程是 forwarder（`systemctl --user status` / `launchctl list`）再下结论。
