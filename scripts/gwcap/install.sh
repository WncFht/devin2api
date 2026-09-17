#!/bin/sh
# gwcap 装机/卸载/状态——本机出向并发闸，当前为留档未部署状态。
# 用法：sudo sh scripts/gwcap/install.sh [install|uninstall|status]
# 机制：nftables `inet gwcap` output nat 把到网关 100.105.212.52 / fd7a:115c:a1e0::e501:d434 的 tcp/3003 REDIRECT 到
# 127.0.0.1+::1 :3399 的 gw-cap-proxy（gwcap 用户自己的上游连接按 skuid 豁免），
# 代理对 model 命中 swe-2-medium 的请求过全局信号量（代理默认 4，部署单元以
# Environment=GWCAP_LIMIT=20 覆盖），其余透传。
# 注意：gw-cap-redirect.service 的 ExecStartPost 还会在 `ip filter INPUT` 顶部插
# `iifname "lo" tcp sport 3003 accept`——代理回包 unNAT 后 src=网关 tailscale IP、
# iif=lo，会被 tailscaled 的 ts-input 反欺骗规则丢弃；nft base-chain 的 accept
# 是链局部的，独立链提前 accept 挡不住，只能插进 ip filter INPUT 的跳板之前。
set -eu

LIB=/usr/local/libexec/gwcap
UNITS=/etc/systemd/system
HERE=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

case "${1:-install}" in
install)
  if ! id gwcap >/dev/null 2>&1; then
    useradd -r -U -M -s /usr/bin/nologin gwcap
  fi
  uid=$(id -u gwcap)
  install -D -m 0755 "$HERE/gw-cap-proxy.py" "$LIB/gw-cap-proxy.py"
  install -D -m 0755 "$HERE/ensure-bypass.sh" "$LIB/ensure-bypass.sh"
  install -D -m 0644 "$HERE/tailscaled-gwcap.conf" "$UNITS/tailscaled.service.d/gwcap.conf"
  printf '%s\n' \
    'table inet gwcap {' \
    '  chain output {' \
    '    type nat hook output priority dstnat; policy accept;' \
    "    ip daddr 100.105.212.52 tcp dport 3003 meta skuid != $uid redirect to :3399" \
    "    ip6 daddr fd7a:115c:a1e0::e501:d434 tcp dport 3003 meta skuid != $uid redirect to :3399" \
    '  }' \
    '}' | tee "$LIB/redirect.nft" >/dev/null
  install -D -m 0644 "$HERE/gw-cap-proxy.service" "$UNITS/gw-cap-proxy.service"
  install -D -m 0644 "$HERE/gw-cap-redirect.service" "$UNITS/gw-cap-redirect.service"
  systemctl daemon-reload
  systemctl enable --now gw-cap-proxy.service gw-cap-redirect.service
  echo "installed: gwcap uid=$uid, proxy :3399, cap=20 on swe-2-medium* (unit env GWCAP_LIMIT)"
  ;;
uninstall)
  systemctl disable --now gw-cap-redirect.service gw-cap-proxy.service 2>/dev/null || true
  nft delete table inet gwcap 2>/dev/null || true
  rm -f "$UNITS/gw-cap-proxy.service" "$UNITS/gw-cap-redirect.service" "$UNITS/tailscaled.service.d/gwcap.conf"
  rm -rf "$LIB"
  systemctl daemon-reload
  userdel gwcap 2>/dev/null || true
  echo "uninstalled"
  ;;
status)
  systemctl --no-pager --full status gw-cap-proxy.service gw-cap-redirect.service || true
  nft list table inet gwcap 2>/dev/null || echo "no inet gwcap table"
  curl -fsS --max-time 3 http://127.0.0.1:3399/__gwcap/healthz || echo "healthz unreachable"
  echo
  ;;
*)
  echo "usage: $0 [install|uninstall|status]" >&2
  exit 2
  ;;
esac
