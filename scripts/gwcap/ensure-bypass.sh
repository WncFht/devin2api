#!/bin/sh
# 把 gwcap 回包放行规则钉在 ip filter INPUT 最顶(ts-input 跳板之前)。
# 背景:出向 :3003 被 inet gwcap REDIRECT 到 :3399 后,代理回包 un-NAT 成
# src=100.105.212.52、iif=lo——落在 ts-input 的 `saddr 100.64/10 iifname!=tailscale0
# drop` 上。本规则必须先于 jump ts-input 执行才有效;而 tailscaled 每次(重)启动
# 都会把 jump 重插到 INPUT 顶部,把本规则压到后面 -> 回包全灭,连接挂死。
# 调用方:gw-cap-redirect.service ExecStartPost/ExecStopPost,
#        tailscaled.service.d/gwcap.conf ExecStartPost。
# 用法:ensure-bypass.sh [ensure|remove]
set -u
mode="${1:-ensure}"
case "$mode" in
ensure | remove) ;;
*)
  echo "usage: $0 [ensure|remove]" >&2
  exit 2
  ;;
esac

del_all() {
  for h in $(nft -a list chain ip filter INPUT 2>/dev/null | awk '/comment "gwcap-bypass-ts-input"/{print $NF}'); do
    nft delete rule ip filter INPUT handle "$h" 2>/dev/null || true
  done
}
insert_top() {
  # INPUT 链可能在 tailscaled 首次编程前还不存在,先保底建出来。
  nft list chain ip filter INPUT >/dev/null 2>&1 || {
    nft add table ip filter 2>/dev/null || true
    nft add chain ip filter INPUT '{ type filter hook input priority filter; policy accept; }' 2>/dev/null || true
  }
  nft insert rule ip filter INPUT iifname "lo" tcp sport 3003 accept comment "gwcap-bypass-ts-input"
}
first_handle() { nft -a list chain ip filter INPUT 2>/dev/null | awk '/# handle/ && !/chain /{print $NF; exit}'; }
my_handle() { nft -a list chain ip filter INPUT 2>/dev/null | awk '/comment "gwcap-bypass-ts-input"/{print $NF; exit}'; }

del_all
[ "$mode" = remove ] && exit 0

# tailscaled 的 ExecStartPost 可能早于它的 netfilter 编程;等 jump ts-input
# 出现(最多 ~10s)再钉顶,避免被后插的 jump 压下去。
for _ in $(seq 1 40); do
  nft list chain ip filter INPUT 2>/dev/null | grep -q 'jump ts-input' && break
  sleep 0.25
done
insert_top
sleep 1
# 复查:若我们不是 INPUT 第一条(又被竞态插队),清干净再钉一次——
# 直接 insert_top 会留下两条同名规则
[ -n "$(first_handle)" ] && [ "$(first_handle)" != "$(my_handle)" ] && {
  del_all
  insert_top
}

exit 0
