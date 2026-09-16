#!/usr/bin/env bash
# remote-logs.sh — 远端实例日志分诊（默认 fht-mba 生产实例）。
#
# 用法:
#   remote-logs.sh [--host H] tail [N]       index.jsonl 末 N 行摘要（结果/token/耗时/错误）
#   remote-logs.sh [--host H] fails [N]      最近 N 个失败请求（error_stage + message）
#   remote-logs.sh [--host H] dir <id>       某调试目录：meta.json + error.json + 阶段文件清单
#   remote-logs.sh [--host H] grep <pat>     index.jsonl 正则过滤
#   remote-logs.sh [--host H] stderr [N]     stderr.log 末 N 行
#
# fht-mba 登录 shell 是 fish：`ssh host 'VAR=x; for ...'` 直发会炸，
# 一律走 `ssh host bash -s` 把脚本喂 stdin（本脚本内部就是这么做的）。
set -u

HOST=fht-mba
if [[ "${1:-}" == "--host" ]]; then HOST="$2"; shift 2; fi
CMD="${1:-tail}"; shift 0
ARG="${2:-}"

# Mac 生产实例 state dir；--host 换机器时若布局不同用 REMOTE_LOGS 覆盖
LOGS="${REMOTE_LOGS:-\$HOME/Library/Application Support/devin-2api/logs}"

run() { ssh "$HOST" bash -s <<<"$1"; }

case "$CMD" in
tail)
	N="${ARG:-30}"
	run "tail -n $N \"$LOGS/index.jsonl\" | python3 -c '
import json,sys
for l in sys.stdin:
    try: r=json.loads(l)
    except: continue
    print(r.get(\"started_at\",\"\")[:19], r.get(\"dir\",\"\"), r.get(\"model\",\"?\"),
          r.get(\"result\",\"?\"), r.get(\"status\",\"\"), r.get(\"error_stage\",\"\"),
          \"in=%s cr=%s ms=%s\" % (r.get(\"input_tokens\"),r.get(\"cache_read_tokens\"),r.get(\"duration_ms\")),
          (r.get(\"error_message\") or \"\")[:80])
'"
	;;
fails)
	N="${ARG:-15}"
	run "grep -h '\"result\":\"' \"$LOGS/index.jsonl\" | grep -vE '\"result\":\"(completed|disconnected)\"' | tail -n $N | python3 -c '
import json,sys
for l in sys.stdin:
    try: r=json.loads(l)
    except: continue
    print(r.get(\"started_at\",\"\")[:19], r.get(\"dir\",\"\"), r.get(\"model\",\"?\"),
          r.get(\"result\",\"?\"), r.get(\"error_stage\",\"\"),
          (r.get(\"error_message\") or \"\")[:100])
'"
	;;
dir)
	[[ -z "$ARG" ]] && { echo "usage: remote-logs.sh dir <request-dir>"; exit 2; }
	run "D=\"$LOGS/$ARG\"; ls -la \"\$D\"; echo ---meta---; cat \"\$D/meta.json\" 2>/dev/null; echo; echo ---error---; cat \"\$D/error.json\" 2>/dev/null"
	;;
grep)
	[[ -z "$ARG" ]] && { echo "usage: remote-logs.sh grep <regex>"; exit 2; }
	run "grep -E '$ARG' \"$LOGS/index.jsonl\" | tail -50"
	;;
stderr)
	N="${ARG:-60}"
	run "tail -n $N \"\$HOME/Library/Application Support/devin-2api/stderr.log\" 2>/dev/null || tail -n $N \"$LOGS/../stderr.log\""
	;;
*)
	sed -n '2,13p' "$0"
	exit 2
	;;
esac
