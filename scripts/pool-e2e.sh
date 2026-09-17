#!/usr/bin/env bash
# pool-e2e.sh — 号池生命周期端到端场景驱动（pool-refactor P1b 契约验收）。
#
# 自建临时实例：默认在仓库根现编 devin-2api（含未提交 WIP），或接受
# 二进制路径参数；state/config/端口全部临时，退出现清理。配置最小化：
# 面板密码 + auth.api_key（播种下游令牌）+ devin.token 假号（必须显式
# 给——留空会走自动发现链摸到本机真实 credentials.toml）+ 假 base_url
# （127.0.0.1:1，零出向，上游拒绝即定失败）+ quota_interval_minutes:0。
#
# 十步场景：空池 → 加号(字面/credentials_file) → /v1 归因 → 删除/墓碑 →
# 恢复 → clear-cooldown/quota/refresh → disabled 排除 → 清池失败态 →
# cli-credentials 探针。端点未实现（404/405/501/503）记 SKIP 不记 FAIL；
# FAIL>0 退出码非零。
#
# 用法: scripts/pool-e2e.sh [devin-2api 二进制路径]
#   PE2E_PORT=<port>  固定端口（默认 socket bind(0) 抢空闲口）
set -uo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
SRVPID=""
cleanup() {
	[[ -n "$SRVPID" ]] && kill "$SRVPID" 2>/dev/null
	rm -rf "$WORK"
}
trap cleanup EXIT

pass=0; failed=0; skipped=0
ok()   { echo "PASS $*"; pass=$((pass+1)); }
bad()  { echo "FAIL $*"; failed=$((failed+1)); }
skip() { echo "SKIP $*"; skipped=$((skipped+1)); }
note() { echo "INFO $*"; }

# 端点缺席口径：路由未注册 404、方法不符 405、stub 501、ops 未接线 503。
unimpl() { [[ "$1" == 404 || "$1" == 405 || "$1" == 501 || "$1" == 503 ]]; }

# --- JSON 抽取：jq 优先，缺席退 python3 子集解释器 ---
if command -v jq >/dev/null 2>&1; then
	jqr() { jq -r "$1" <<<"$2" 2>/dev/null | sed 's/^null$//'; }
else
	note "jq 缺席，JSON 抽取走 python3 子集（sqlite 查询本来就靠它）"
	jqr() { # filter json-string —— 支持 .a.b .a[] [N] select(.k=="v") keys[] length //lit
		python3 - "$1" "$2" <<'PY'
import json, re, sys
try:
	d = json.loads(sys.argv[2])
except Exception:
	sys.exit(0)
def emit(v):
	if isinstance(v, (dict, list)):
		print(json.dumps(v))
	elif v is None:
		pass
	elif isinstance(v, bool):
		print('true' if v else 'false')
	else:
		print(v)
for stage in [s.strip() for s in sys.argv[1].split('|')]:
	if stage == 'length':
		d = len(d) if isinstance(d, (list, dict, str)) else 0
	elif stage.startswith('//'):
		if d is None or d is False:
			lit = stage[2:].strip()
			try:
				d = json.loads(lit)
			except Exception:
				d = lit.strip('"')
	elif stage.startswith('select('):
		m = re.match(r'select\(\.(\w+)\s*==\s*"(.*)"\)', stage)
		d = [e for e in (d if isinstance(d, list) else [])
		     if isinstance(e, dict) and e.get(m.group(1)) == m.group(2)]
	elif stage in ('keys[]', 'keys'):
		d = sorted(d.keys()) if isinstance(d, dict) else []
	elif stage.startswith('.'):
		for tok in re.finditer(r'\.(\w+)|\[(\d*)\]', stage):
			key, idx = tok.group(1), tok.group(2)
			if key:
				if isinstance(d, list):
					d = [e.get(key) if isinstance(e, dict) else None for e in d]
				elif isinstance(d, dict):
					d = d.get(key)
				else:
					d = None
			elif idx == '':
				if isinstance(d, dict):
					d = list(d.values())
				elif not isinstance(d, list):
					d = []
			else:
				i = int(idx)
				d = d[i] if isinstance(d, list) and len(d) > i else None
if isinstance(d, list):
	for e in d:
		emit(e)
else:
	emit(d)
PY
	}
fi

dbq() { # SQL -> stdout（python3 sqlite3，只读 URI）
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

req() { # METHOD PATH [json-body] -> http_code；响应体落 $WORK/last.json
	local m="$1" p="$2" data="${3:-}"
	local args=(-s -o "$WORK/last.json" -w '%{http_code}' -X "$m")
	[[ -n "$data" ]] && args+=(-H 'Content-Type: application/json' -d "$data")
	curl "${args[@]}" "${AUTH[@]}" "$BASE$p" 2>/dev/null || echo 000
}

vreq() { # json-body -> http_code（/v1 走下游令牌）
	curl -s -o "$WORK/last-v.json" -w '%{http_code}' -X POST \
		-H 'Content-Type: application/json' "${VAUTH[@]}" \
		-d "$1" "$BASE/v1/chat/completions" 2>/dev/null || echo 000
}

# --- 二进制与端口 ---
BIN="${1:-}"
if [[ -z "$BIN" ]]; then
	BIN="$WORK/devin-2api"
	(cd "$REPO" && go build -o "$BIN" ./cmd/devin-2api) || { echo "FAIL 构建失败"; exit 1; }
fi
[[ -x "$BIN" ]] || { echo "FAIL 二进制不可执行: $BIN"; exit 1; }
note "binary=$BIN"

PORT="${PE2E_PORT:-}"
if [[ -z "$PORT" ]]; then
	PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()')"
fi
BASE="http://127.0.0.1:$PORT"
STATE="$WORK/state"; mkdir -p "$STATE"
DB="$STATE/devin-2api.db"

cat >"$WORK/config.yaml" <<EOF
server:
  listen: "127.0.0.1:$PORT"
devin:
  token: "e2e-fake-token-default"
  base_url: "http://127.0.0.1:1"
  model: "swe-2-medium"
debug:
  enabled: true
  quota_interval_minutes: 0
dashboard:
  password: "e2e-pass"
auth:
  api_key: "e2e-key"
EOF
# t2 用的 credentials_file（假号，真文件——加载期必须可解 key）。
printf 'windsurf_api_key = "e2e-fake-token-t2"\n' >"$WORK/creds-t2.toml"

# --- 起实例 ---
# env -u 掐掉自动发现链的环境变量入口（config token 非空本就不走，双保险）。
env -u DEVIN_TOKEN -u WINDSURF_API_KEY \
	"$BIN" -config "$WORK/config.yaml" -state-dir "$STATE" \
	>"$WORK/boot.log" 2>&1 &
SRVPID=$!

want_rev="$(go version -m "$BIN" 2>/dev/null | sed -n 's/.*vcs.revision=//p' | cut -c1-7)"
up=0
for _ in $(seq 1 50); do
	kill -0 "$SRVPID" 2>/dev/null || { echo "FAIL 实例进程已退出"; cat "$WORK/boot.log"; exit 1; }
	hz="$(curl -sf "$BASE/healthz" 2>/dev/null)" && {
		if [[ -n "$want_rev" ]]; then
			got="$(jqr '.version' "$hz")"
			[[ "$got" == *"$want_rev"* ]] || { echo "FAIL 端口 $PORT 上是异己实例 version=$got"; exit 1; }
		fi
		up=1; break
	}
	sleep 0.2
done
[[ $up == 1 ]] || { echo "FAIL 实例未起来"; cat "$WORK/boot.log"; exit 1; }
note "listening=$BASE state=$STATE"

AUTH=(-H "Authorization: Bearer e2e-pass")
VAUTH=(-H "Authorization: Bearer e2e-key")

PROBE='{"model":"swe-2-medium","messages":[{"role":"user","content":"hi"}],"max_tokens":4,"stream":false}'

names() { # GET /admin/accounts -> 逐行名字（或空）
	local code; code="$(req GET /admin/accounts)"
	[[ "$code" == 200 ]] || return 1
	jqr '.data.accounts[].name' "$(cat "$WORK/last.json")"
}

wait_name() { # name want(present|absent) -> 0/1，轮询 5s 覆盖 ApplyConfigs 异步
	local n="$1" want="$2" i got
	for i in $(seq 1 25); do
		got="$(names)" || { sleep 0.2; continue; }
		if [[ "$want" == present ]] && grep -qx "$n" <<<"$got"; then return 0; fi
		if [[ "$want" == absent ]] && ! grep -qx "$n" <<<"$got"; then return 0; fi
		sleep 0.2
	done
	return 1
}

acct_field() { # name field -> 值（缺席空）
	local code; code="$(req GET /admin/accounts)"
	[[ "$code" == 200 ]] || return 0
	jqr ".data.accounts[]|select(.name==\"$1\")|.$2" "$(cat "$WORK/last.json")"
}

log_accounts() { # 最近 N 行 logs.account 去重
	dbq "SELECT DISTINCT account FROM (SELECT account FROM logs ORDER BY id DESC LIMIT ${1:-5})"
}

wait_logrow() { # -> 0 当 logs 表出现新行（基线 $1）
	local i c
	for i in $(seq 1 15); do
		c="$(dbq 'SELECT COUNT(*) FROM logs')"
		[[ "$c" =~ ^[0-9]+$ && "$c" -gt "$1" ]] && return 0
		sleep 0.2
	done
	return 1
}

# ============ 1. 空池 ============
code="$(req GET /admin/accounts)"
if unimpl "$code"; then
	skip "GET /admin/accounts 未实现 (HTTP $code)"
elif [[ "$code" == 200 ]]; then
	list="$(jqr '.data.accounts[].name' "$(cat "$WORK/last.json")" | sort | paste -sd, -)"
	if [[ -z "$list" ]]; then
		ok "空池 GET /admin/accounts 空列表"
	elif [[ "$list" == "default" ]]; then
		ok "空池仅剩过渡 default lane（devin.token 配置仍在生效）"
	else
		bad "空池期望空列表，实得: $list"
	fi
else
	bad "GET /admin/accounts HTTP $code: $(head -c 200 "$WORK/last.json")"
fi

before="$(dbq 'SELECT COUNT(*) FROM logs' || echo 0)"
code="$(vreq "$PROBE")"
if [[ "$code" =~ ^[45] ]]; then
	ok "空池 /v1 定失败 HTTP $code"
	wait_logrow "${before:-0}" && note "探针行入库 account=$(log_accounts | paste -sd, -)"
else
	bad "空池 /v1 应定失败，实得 HTTP $code"
fi

# ============ 2. 加号 t1（字面 token）============
code="$(req POST /admin/accounts '{"name":"t1","token":"e2e-fake-token-t1"}')"
if unimpl "$code"; then
	skip "POST /admin/accounts 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	wait_name t1 present && ok "POST t1 → GET 列出" || bad "POST t1 后 GET 未见 t1"
	rt="$(curl -sf "${AUTH[@]}" "$BASE/admin/runtime-metrics" 2>/dev/null || true)"
	if jqr '.accounts|keys[]' "$rt" | grep -qx t1; then
		ok "runtime-metrics accounts.t1 在册"
	else
		bad "runtime-metrics 缺 accounts.t1（有: $(jqr '.accounts|keys[]' "$rt" | paste -sd, -)）"
	fi
else
	bad "POST t1 HTTP $code: $(head -c 200 "$WORK/last.json")"
fi

# ============ 3. 加号 t2（credentials_file）============
code="$(req POST /admin/accounts "{\"name\":\"t2\",\"credentials_file\":\"$WORK/creds-t2.toml\"}")"
if unimpl "$code"; then
	skip "POST t2 credentials_file 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	wait_name t2 present && ok "POST t2 → GET 列出（双 panel lane）" || bad "POST t2 后 GET 未见 t2"
else
	bad "POST t2 HTTP $code: $(head -c 200 "$WORK/last.json")"
fi

# ============ 4. 钉选/failover 归因 ============
before="$(dbq 'SELECT COUNT(*) FROM logs' || echo 0)"
code="$(vreq "$PROBE")"
if [[ "$code" =~ ^[45] ]]; then
	if wait_logrow "${before:-0}"; then
		accts="$(log_accounts | paste -sd, -)"
		if [[ -n "$accts" ]]; then
			ok "logs.account 归因非空: $accts"
		else
			bad "logs 行 account 字段为空"
		fi
	else
		bad "探针行未入库"
	fi
	lq="$(req GET '/admin/logs?limit=5')"
	[[ "$lq" == 200 ]] && note "/admin/logs 行 account: $(jqr '.data[].account' "$(cat "$WORK/last.json")" | paste -sd, -)"
else
	bad "归因探针 HTTP $code（假 base_url 下应为 4xx/5xx）"
fi

# ============ 5. DELETE t1 → 墓碑语义 ============
code="$(req DELETE /admin/accounts/t1)"
if unimpl "$code"; then
	skip "DELETE /admin/accounts/t1 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	note "DELETE t1 响应: $(head -c 200 "$WORK/last.json")"
	if wait_name t1 absent; then
		ok "DELETE t1 后离队（panel 名物理删）"
	elif [[ "$(acct_field t1 source)" == "tombstoned" ]]; then
		ok "DELETE t1 → tombstoned 在列"
	else
		bad "DELETE t1 后仍在列 source=$(acct_field t1 source)"
	fi
else
	bad "DELETE t1 HTTP $code"
fi

# ============ 6. 恢复 ============
code="$(req POST /admin/accounts/t1/restore)"
if [[ "$code" =~ ^2 ]]; then
	wait_name t1 present && ok "restore t1 回队" || bad "restore 2xx 但 t1 未回队"
elif unimpl "$code" || [[ "$code" =~ ^4 ]]; then
	# panel 名物理删后 restore 无对象（404）是合理路径——退回重建。
	code2="$(req POST /admin/accounts '{"name":"t1","token":"e2e-fake-token-t1"}')"
	if [[ "$code2" =~ ^2 ]]; then
		wait_name t1 present && ok "重建 t1 回队（restore HTTP $code 不可恢复 panel 删）" || bad "重建 2xx 但 t1 未回队"
	elif unimpl "$code2"; then
		skip "恢复路径未实现（restore $code / POST $code2）"
	else
		bad "恢复失败 restore=$code recreate=$code2"
	fi
else
	bad "restore t1 HTTP $code（5xx）"
fi

# ============ 7. clear-cooldown / quota-refresh ============
for ep in clear-cooldown quota/refresh; do
	code="$(req POST "/admin/accounts/t2/$ep")"
	if unimpl "$code"; then
		skip "POST t2/$ep 未实现 (HTTP $code)"
	elif [[ "$code" =~ ^5 ]]; then
		bad "POST t2/$ep HTTP $code（要 sane，不是 5xx）"
	else
		ok "POST t2/$ep HTTP $code sane"
	fi
done

# ============ 8. PUT t2 disabled → 在列但排除 ============
code="$(req PUT /admin/accounts/t2 '{"disabled":true}')"
if unimpl "$code"; then
	skip "PUT /admin/accounts/t2 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	if wait_name t2 present && [[ "$(acct_field t2 disabled)" == "true" ]]; then
		before="$(dbq 'SELECT COUNT(*) FROM logs' || echo 0)"
		vreq "$PROBE" >/dev/null
		wait_logrow "${before:-0}" || true
		last="$(log_accounts 1)"
		if [[ "$last" != "t2" ]]; then
			ok "PUT t2 disabled → 在列且排除（归因 lane=$last）"
		else
			bad "disabled t2 仍在服务"
		fi
	else
		bad "PUT disabled 后 t2 缺席或 disabled!=true（$(acct_field t2 disabled)）"
	fi
else
	bad "PUT t2 HTTP $code: $(head -c 200 "$WORK/last.json")"
fi

# ============ 9. 清池 → /v1 失败态 ============
deleted=0; dunimpl=0
for n in t1 t2 default; do
	code="$(req DELETE "/admin/accounts/$n")"
	unimpl "$code" && { dunimpl=1; continue; }
	[[ "$code" =~ ^2 ]] && deleted=$((deleted+1))
done
if [[ $dunimpl == 1 && $deleted == 0 ]]; then
	skip "清池 DELETE 未实现"
elif [[ $deleted -gt 0 ]]; then
	sleep 0.5
	code="$(vreq "$PROBE")"
	if [[ "$code" =~ ^[45] ]]; then
		ok "清池后 /v1 定失败 HTTP $code（删 $deleted 名）"
	else
		bad "清池后 /v1 应失败，实得 HTTP $code"
	fi
else
	bad "清池 DELETE 全部失败"
fi

# ============ 10. cli-credentials 形状 ============
code="$(req GET /admin/accounts/cli-credentials)"
if unimpl "$code"; then
	skip "GET cli-credentials 未实现 (HTTP $code)"
elif [[ "$code" == 200 ]]; then
	body="$(cat "$WORK/last.json")"
	avail="$(jqr '.available' "$body")"; [[ -z "$avail" ]] && avail="$(jqr '.data.available' "$body")"
	path="$(jqr '.path' "$body")";       [[ -z "$path" ]] && path="$(jqr '.data.path' "$body")"
	if [[ "$avail" =~ ^(true|false)$ ]]; then
		ok "cli-credentials 形状 {available:$avail, path:${path:-<empty>}}"
	else
		bad "cli-credentials 形状异常: $(head -c 200 "$WORK/last.json")"
	fi
else
	bad "cli-credentials HTTP $code"
fi

echo "----"
echo "结果: $pass PASS / $failed FAIL / $skipped SKIP"
exit $((failed > 0))
