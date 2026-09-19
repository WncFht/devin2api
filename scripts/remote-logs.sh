#!/usr/bin/env bash
# remote-logs.sh — 远端实例日志分诊（目标主机由 DEVIN2API_HOST 或 --host 指定）。
#
# 用法:
#   remote-logs.sh [--host H] tail [N]       logs 表末 N 行摘要（结果/token/耗时/错误）
#   remote-logs.sh [--host H] fails [N]      最近 N 个失败请求（error_stage + message）
#   remote-logs.sh [--host H] dir <id>       某调试目录：meta.json + error.json + 阶段文件清单
#   remote-logs.sh [--host H] grep <pat>     logs 表导出行正则过滤
#   remote-logs.sh [--host H] stderr [N]     stderr.log 末 N 行
#
# 远端登录 shell 若是 fish：`ssh host 'VAR=x; for ...'` 直发会炸，
# 一律走 `ssh host bash -s` 把脚本喂 stdin（本脚本内部就是这么做的）。
set -u

HOST="${DEVIN2API_HOST:?usage: DEVIN2API_HOST=<host> remote-logs.sh [--host H] <cmd>}"
if [[ "${1:-}" == "--host" ]]; then HOST="$2"; shift 2; fi
CMD="${1:-tail}"; shift 0
ARG="${2:-}"

# 默认按 macOS 实例 state dir 布局；--host 换机器时若布局不同用 REMOTE_LOGS / REMOTE_DB 覆盖
LOGS="${REMOTE_LOGS:-\$HOME/Library/Application Support/devin-2api/logs}"
DB="${REMOTE_DB:-\$HOME/Library/Application Support/devin-2api/devin-2api.db}"

run() { ssh "$HOST" bash -s <<<"$1"; }

# logs 表读路径要求远端有 sqlite3（macOS 自带）；缺失时显式报错而非静默空输出。
SQLITE_CHECK='command -v sqlite3 >/dev/null || { echo "remote: sqlite3 not found" >&2; exit 1; }'

case "$CMD" in
tail)
	N="${ARG:-30}"
	run "$SQLITE_CHECK; sqlite3 -readonly -json \"$DB\" \"SELECT * FROM logs ORDER BY id DESC LIMIT $N\" | python3 -c '
import json,sys
rows = json.load(sys.stdin)
rows.reverse()
for r in rows:
    print(r.get(\"started_at\",\"\")[:19], r.get(\"dir\",\"\"), r.get(\"model\",\"?\"),
          r.get(\"result\",\"?\"), r.get(\"status_code\",\"\"), r.get(\"error_stage\",\"\"),
          \"in=%s cr=%s ms=%s\" % (r.get(\"input_tokens\"),r.get(\"cache_read_tokens\"),r.get(\"duration_ms\")),
          (r.get(\"error_message\") or \"\")[:80])
'"
	;;
fails)
	N="${ARG:-15}"
	run "$SQLITE_CHECK; sqlite3 -readonly -json \"$DB\" \"SELECT * FROM logs WHERE result NOT IN ('completed','disconnected') ORDER BY id DESC LIMIT $N\" | python3 -c '
import json,sys
rows = json.load(sys.stdin)
rows.reverse()
for r in rows:
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
	run "$SQLITE_CHECK; sqlite3 -readonly -json \"$DB\" 'SELECT * FROM logs ORDER BY id' | python3 -c '
import json,sys
for r in json.load(sys.stdin): print(json.dumps(r, ensure_ascii=False))
' | grep -E '$ARG' | tail -50"
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
