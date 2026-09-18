#!/usr/bin/env bash
# sqlite-live-verify.sh — :3033 验证实例的 sqlite 迁移验收。
# 对现役实例（已被 deploy-linux.sh 部署为迁移版）做只读对账 +
# 一条确定性写路径探针，不重新部署、不动 state。
#
# 用法: scripts/sqlite-live-verify.sh [port] [state-dir] [config.yaml]
#   默认 :3033 / ~/.local/state/devin-2api / ~/.config/devin-2api/config.yaml
#
# 检查项：
#   1. 实例健康与版本（healthz）
#   2. 迁移落点：devin-2api.db 存在、WAL pragma、全部源文件 .migrated
#   3. 行数对账：logs/quota_samples/auth_tokens/model_registry/runtime_state
#      与 .migrated 源文件逐表核数（logs/quota 允许 +N 新行——实例在跑）
#   4. 端点抽查：/admin/logs 宽窗 count == sqlite COUNT；matrix/usage/
#      stats/quota 非空；debug-logs 详情能投影
#   5. 写路径探针：注册表停用模型 → /v1 打一发 model_disabled（零上游
#      成本）→ logs 表行数 +1、目录落盘 → 清理注册表项
set -euo pipefail

PORT="${1:-3033}"
STATE="${2:-$HOME/.local/state/devin-2api}"
CONFIG="${3:-$HOME/.config/devin-2api/config.yaml}"
BASE="http://127.0.0.1:$PORT"
DB="$STATE/devin-2api.db"
fail=0

ok()   { echo "PASS $1"; }
bad()  { echo "FAIL $1"; fail=1; }

PASSWORD="$(grep -E '^\s*password:' "$CONFIG" | head -1 | sed -E 's/.*password:\s*//; s/["'"'"']//g' | tr -d ' ')"
AUTH=(-H "Authorization: Bearer $PASSWORD")
# /v1 探针凭据：令牌仓只存哈希取不回明文——走面板 admin API 铸一条
# 临时令牌（healthz 确认实例存活后铸，带 15min expires_at 兜底自失效，
# 进程退出经 trap DELETE 回收）；空仓/含匿名行时无凭据也能过准入，
# 铸不到就空凭据发，探针按 4xx 口径照样计 PASS。
API_KEY=""
VTID=""
VAUTH=()
PROBE="sqlite-live-probe"

cleanup() {
	# 早退兜底：探针注册表项与临时令牌能删就删；实例不在/端点失败都不影响收尾。
	curl -s -o /dev/null -m 5 -X DELETE "${AUTH[@]}" \
		"$BASE/admin/model-registry?model=$PROBE" 2>/dev/null || true
	[[ -n "$VTID" ]] && curl -s -o /dev/null -m 5 -X DELETE "${AUTH[@]}" \
		"$BASE/admin/auth-tokens/$VTID" 2>/dev/null || true
}
trap cleanup EXIT

dbq() { # SQL -> stdout（python3 的 sqlite3 模块，不依赖 sqlite3 CLI）
	python3 - "$DB" "$1" <<'PY'
import sqlite3, sys
con = sqlite3.connect(f"file:{sys.argv[1]}?mode=ro", uri=True)
try:
    for row in con.execute(sys.argv[2]):
        print("|".join("" if v is None else str(v) for v in row))
except Exception as e:
    print(f"SQLERR {e}")
con.close()
PY
}

# --- 1. 健康 ---
hz="$(curl -sf "$BASE/healthz" || true)"
[[ -n "$hz" ]] && ok "healthz $(echo "$hz" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(d["version"],"draining" if d.get("draining") else "live")')" || bad "healthz 无响应"

# 铸 /v1 探针临时令牌（明文一次性出示，仓内只存哈希）：expires_at 15min
# 兜底——SIGKILL 等 trap 盖不到的死法，令牌到期也自行失效。
expires_ms=$(( $(date +%s) * 1000 + 900000 ))
resp="$(curl -sf -m 5 -X POST -H 'Content-Type: application/json' "${AUTH[@]}" \
	-d "{\"description\":\"sqlite-live-verify: temp\",\"expires_at\":$expires_ms}" \
	"$BASE/admin/auth-tokens" 2>/dev/null || true)"
IFS=$'\t' read -r API_KEY VTID <<<"$(printf '%s' "$resp" | python3 -c '
import sys, json
try:
    d = json.load(sys.stdin)
    d = d.get("data") or d   # ccLoad 信封 {success,data,...}
except Exception:
    d = {}
print("{}\t{}".format(d.get("token") or "", d.get("id") or ""))' 2>/dev/null || true)"
if [[ -n "$API_KEY" ]]; then
	VAUTH=(-H "Authorization: Bearer $API_KEY")
fi

# --- 2. 迁移落点 ---
[[ -f "$DB" ]] && ok "devin-2api.db 存在 ($(du -h "$DB" | cut -f1))" || bad "devin-2api.db 缺失"
jr="$(dbq 'PRAGMA journal_mode')"
[[ "$jr" == "wal" ]] && ok "journal_mode=wal" || bad "journal_mode=$jr"
mig=0; miss=""
for f in logs/index.jsonl auth_tokens.json models.json panel-settings.json logs/quota.jsonl; do
	if [[ -f "$STATE/$f.migrated" ]]; then mig=$((mig+1)); else miss="$miss $f"; fi
done
[[ -z "$miss" ]] && ok "源文件全部 .migrated ($mig)" || bad "未迁移:$miss"
[[ -f "$STATE/logs/index.jsonl" ]] && echo "NOTE index.jsonl 仍存在（迁移后新写入不该再生此文件）"

# --- 3. 行数对账 ---
src_lines() { [[ -f "$1" ]] && wc -l <"$1" | tr -d ' ' || echo 0; }
idx_src="$(src_lines "$STATE/logs/index.jsonl.migrated")"
idx_db="$(dbq 'SELECT COUNT(*) FROM logs')"
if [[ "$idx_db" =~ ^[0-9]+$ && "$idx_src" =~ ^[0-9]+$ && "$idx_db" -ge "$idx_src" ]]; then
	ok "logs 表 $idx_db 行 ≥ 源 $idx_src（+ $((idx_db-idx_src)) 迁移后新行）"
else
	bad "logs 表行数 $idx_db < 源 $idx_src"
fi
q_src="$(src_lines "$STATE/logs/quota.jsonl.migrated")"
q_db="$(dbq 'SELECT COUNT(*) FROM quota_samples')"
[[ "$q_db" =~ ^[0-9]+$ && "$q_db" -ge "$q_src" ]] && ok "quota_samples $q_db ≥ 源 $q_src" || bad "quota_samples $q_db < 源 $q_src"
t_src=0; [[ -f "$STATE/auth_tokens.json.migrated" ]] && t_src="$(python3 -c "import json;print(len(json.load(open('$STATE/auth_tokens.json.migrated'))))" 2>/dev/null || echo 0)"
t_db="$(dbq 'SELECT COUNT(*) FROM auth_tokens')"
[[ "$t_db" =~ ^[0-9]+$ && "$t_db" -ge "$t_src" ]] && ok "auth_tokens $t_db ≥ 源 $t_src（含 anonymous 引导行）" || bad "auth_tokens $t_db < 源 $t_src"
echo "INFO model_registry=$(dbq 'SELECT COUNT(*) FROM model_registry') settings=$(dbq 'SELECT COUNT(*) FROM settings') runtime_state=$(dbq 'SELECT COUNT(*) FROM runtime_state') schema_migrations=$(dbq 'SELECT COUNT(*) FROM schema_migrations')"

# --- 4. 端点抽查 ---
# log_source=all：默认视图剔除 rejected 留存行，与 sqlite COUNT(*) 差一行恒 FAIL。
# idx_db 是本段开头读的旧快照，logs 行只增不减——端点计数落在
# [idx_db, 复查 COUNT] 闭区间即一致；生产流量期两次读之间会进新行。
lc="$(curl -sf "${AUTH[@]}" "$BASE/admin/logs?since=2020-01-01T00:00:00Z&log_source=all&limit=1" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("count",-1))' 2>/dev/null || echo CURL_FAIL)"
idx_db_after="$(dbq 'SELECT COUNT(*) FROM logs')"
if [[ "$lc" =~ ^[0-9]+$ && "$idx_db_after" =~ ^[0-9]+$ && "$lc" -ge "$idx_db" && "$lc" -le "$idx_db_after" ]]; then
	ok "/admin/logs 宽窗 count=$lc ∈ [$idx_db, $idx_db_after]"
else
	bad "/admin/logs count=$lc vs sqlite [$idx_db, $idx_db_after]"
fi
mx="$(curl -sf "${AUTH[@]}" "$BASE/admin/logs/matrix" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len(d.get("data") or d.get("entries") or []))' 2>/dev/null || echo 0)"
[[ "$mx" != "0" ]] && ok "matrix 非空 ($mx 格)" || bad "matrix 空"
us="$(curl -sf "${AUTH[@]}" "$BASE/admin/usage" | python3 -c 'import sys,json;d=json.load(sys.stdin).get("data") or {};s=d.get("snapshot") or {};print((s.get("today") or {}).get("requests",-1))' 2>/dev/null || echo CURL_FAIL)"
[[ "$us" != "CURL_FAIL" && "$us" != "-1" ]] && ok "usage snapshot.today.requests=$us" || bad "usage 异常"
qu="$(curl -sf "${AUTH[@]}" "$BASE/admin/quota" 2>/dev/null | head -c 50 || true)"
[[ -n "$qu" ]] && ok "quota 有响应" || bad "quota 无响应"
# debug-logs：库里最新一行的 dir → 面板 id → detail
lastdir="$(dbq 'SELECT dir FROM logs ORDER BY id DESC LIMIT 1')"
lastid="$(curl -sf "${AUTH[@]}" "$BASE/admin/logs?q=$lastdir&limit=5" | python3 -c 'import sys,json;d=json.load(sys.stdin);print((d.get("data") or [{}])[0].get("id",""))' 2>/dev/null || true)"
if [[ -n "$lastid" ]]; then
	dl="$(curl -sf "${AUTH[@]}" "$BASE/admin/debug-logs/$lastid" | python3 -c 'import sys,json;d=json.load(sys.stdin);print(len((d.get("data") or {}).get("files") or []))' 2>/dev/null || echo 0)"
	[[ "$dl" != "0" ]] && ok "debug-logs/$lastid files=$dl" || echo "NOTE debug-logs/$lastid 无文件清单（目录可能已清）"
else
	echo "NOTE 最新行 id 未解析到（dir=$lastdir）"
fi

# --- 5. 写路径探针 ---
before="$(dbq 'SELECT COUNT(*) FROM logs')"
curl -sf "${AUTH[@]}" -X PUT -H 'Content-Type: application/json' \
	-d "{\"model\":\"$PROBE\",\"enabled\":false}" "$BASE/admin/model-registry" >/dev/null || bad "registry PUT 失败"
code="$(curl -s -o /dev/null -w '%{http_code}' "${VAUTH[@]}" -X POST -H 'Content-Type: application/json' \
	-d "{\"model\":\"$PROBE\",\"messages\":[{\"role\":\"user\",\"content\":\"lv\"}],\"stream\":false}" \
	"$BASE/v1/chat/completions" || true)"
sleep 1
after="$(dbq 'SELECT COUNT(*) FROM logs')"
[[ "$code" =~ ^4 ]] && ok "探针 HTTP $code（model_disabled 本地拒）" || bad "探针 HTTP $code 非 4xx"
if [[ "$after" =~ ^[0-9]+$ && "$before" =~ ^[0-9]+$ && "$after" -gt "$before" ]]; then
	ok "logs 表 $before→$after 探针行入库"
else
	bad "探针行未入库 $before→$after"
fi
newdir="$(dbq "SELECT dir FROM logs ORDER BY id DESC LIMIT 1")"
ndf="$(dbq "SELECT COUNT(*) FROM debug_files WHERE dir='$newdir'")"
[[ "$ndf" =~ ^[0-9]+$ && "$ndf" -gt 0 ]] && ok "调试 payload 落库 debug_files[$newdir]=$ndf 行" || echo "NOTE $newdir 无 debug_files（payload 保留策略剔除属正常）"
# 探针注册表项与临时令牌由 EXIT trap 统一回收——覆盖早退与异常路径。

echo "----"
[[ "$fail" == 0 ]] && echo "全部通过" || echo "有 FAIL，见上"
exit $fail
