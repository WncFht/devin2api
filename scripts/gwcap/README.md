# gwcap — 本机出向并发闸（留档，未部署）

把本机发往上游网关 `<mac-host>:3003`（tailnet IPv4/IPv6 地址由 `GWCAP_TARGET_V4`/`GWCAP_TARGET_V6` 注入）的 TCP 连接透明劫持进本地中继，按请求体 `model` 字段对 `swe-2-medium*` 过全局信号量限流，其余透传。曾在一台 Linux 机运行（本机示例，已下线留档）；下线原因是不再需要本地限流，保留供以后复用。

## 组成

- `gw-cap-proxy.py` — 127.0.0.1+::1 `:3399` 中继，`GET /__gwcap/healthz` 本地应答，其余按 model 过闸转发 `GWCAP_UPSTREAM`
- `redirect.nft`（install 时生成）— `inet gwcap` OUTPUT nat 把到网关 `:3003` 的流量 REDIRECT 到 `:3399`，`skuid == gwcap` 豁免防自捕
- `ensure-bypass.sh` — 被劫连接的 unNAT 回包 src=网关 tailscale IP、iif=lo，会撞 tailscaled `ts-input` 反欺骗 drop；本脚本把 `iifname lo tcp sport 3003 accept` 钉在 `ip filter INPUT` 顶部（ts-input 跳板之前）
- `gw-cap-proxy.service` / `gw-cap-redirect.service` / `tailscaled-gwcap.conf` — 双单元 + tailscaled drop-in（tailscaled 重启会重插 ts-input jump，靠它兜底再钉 bypass）

## 使用

```sh
sudo sh scripts/gwcap/install.sh install    # 建 gwcap 用户 + 装文件 + enable 两单元
sudo sh scripts/gwcap/install.sh status     # 看单元/表/healthz
sudo sh scripts/gwcap/install.sh uninstall  # 全拆
```

## 已知坑（2026-09-17 实发过一次）

- `ensure-bypass.sh` 曾缺 `exit 0`：尾部的 `[ ] && [ ] && { }` 短链在「规则已在正确位置」时返回 1，systemd 把 `gw-cap-redirect.service` 标 failed——规则其实装上了，但状态误报；已修。
- bypass 规则只兜 service 启动：tailscaled 非重启途径的重编程（netmap 更新）若重排 INPUT 链，bypass 可能再次被压到 ts-input 之后，症状是网关 `:3003` 全连接挂死、对端抓包零到达。
