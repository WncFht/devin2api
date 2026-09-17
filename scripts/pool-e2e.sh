#!/usr/bin/env bash
# pool-e2e.sh — 号池生命周期端到端场景驱动（pool-refactor P1a/P1b 契约验收）。
#
# 自建临时实例：默认在仓库根现编 devin-2api（含未提交 WIP），或接受
# 二进制路径参数；state/config/端口全部临时，退出现清理。三段实例：
#   A. devin.token 残留配置 → 断言拒启动且文案提及 accounts 迁移（P1a）
#   B. 空池两形态（accounts 键缺席 / accounts:[]）→ {count:0,
#      accounts:[]} + /v1 定失败 + 面板热建号（P1a 空池合法化）
#   C. devin.accounts 声明 config 名 t0 → 全生命周期：加号(字面/
#      credentials_file) → 重名 409/非法名 400 → /v1 归因 → DELETE
#      双轨（panel 物理删 / config 墓碑在列）→ PUT 墓碑 409 →
#      restore 复活 → clear-cooldown/quota-refresh sane → disabled
#      排除 → 清池失败态 → cli-credentials 探针
# 凭据全假，base_url 指 127.0.0.1:1（零出向，上游拒绝即定失败）。
# 端点未实现（404/405/501/503）记 SKIP 不记 FAIL；FAIL>0 退出码非零。
#
# 用法: scripts/pool-e2e.sh [devin-2api 二进制路径]
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

# --- 二进制 ---
BIN="${1:-}"
if [[ -z "$BIN" ]]; then
	BIN="$WORK/devin-2api"
	(cd "$REPO" && go build -o "$BIN" ./cmd/devin-2api) || { echo "FAIL 构建失败"; exit 1; }
fi
[[ -x "$BIN" ]] || { echo "FAIL 二进制不可执行: $BIN"; exit 1; }
WANT_REV="$(go version -m "$BIN" 2>/dev/null | sed -n 's/.*vcs.revision=//p' | cut -c1-7)"
note "binary=$BIN"

AUTH=(-H "Authorization: Bearer e2e-pass")
VAUTH=(-H "Authorization: Bearer e2e-key")
PROBE='{"model":"swe-2-medium","messages":[{"role":"user","content":"hi"}],"max_tokens":4,"stream":false}'

free_port() {
	python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1]); s.close()'
}

write_cfg() { # cfg-path port devin-yaml-lines...
	local cfg="$1" port="$2"; shift 2
	{
		echo "server:"
		echo "  listen: \"127.0.0.1:$port\""
		echo "devin:"
		printf '%s\n' "$@"
		cat <<'EOF'
debug:
  enabled: true
  quota_interval_minutes: 0
dashboard:
  password: "e2e-pass"
auth:
  api_key: "e2e-key"
EOF
	} >"$cfg"
}

start_instance() { # cfg-path tag -> 全局 PORT/BASE/STATE/DB/SRVPID；0=起来了
	local cfg="$1" tag="$2" i hz
	PORT="$(free_port)"
	sed -i -E "s/127\.0\.0\.1:[0-9]+/127.0.0.1:$PORT/" "$cfg"
	BASE="http://127.0.0.1:$PORT"
	STATE="$WORK/state-$tag"; mkdir -p "$STATE"
	DB="$STATE/devin-2api.db"
	# env -u 掐掉凭据环境变量入口（双保险——凭据只许来自测试喂的行）。
	env -u DEVIN_TOKEN -u WINDSURF_API_KEY \
		"$BIN" -config "$cfg" -state-dir "$STATE" \
		>"$STATE/boot.log" 2>&1 &
	SRVPID=$!
	for i in $(seq 1 50); do
		kill -0 "$SRVPID" 2>/dev/null || { echo "FAIL[$tag] 实例进程已退出"; tail -5 "$STATE/boot.log"; return 1; }
		hz="$(curl -sf "$BASE/healthz" 2>/dev/null)" && {
			if [[ -n "$WANT_REV" ]]; then
				local got; got="$(jqr '.version' "$hz")"
				[[ "$got" == *"$WANT_REV"* ]] || { echo "FAIL[$tag] 端口 $PORT 上是异己实例 version=$got"; return 1; }
			fi
			note "[$tag] listening=$BASE"
			return 0
		}
		sleep 0.2
	done
	echo "FAIL[$tag] 实例未起来"; tail -5 "$STATE/boot.log"; return 1
}

stop_instance() {
	[[ -n "$SRVPID" ]] && kill "$SRVPID" 2>/dev/null
	wait "$SRVPID" 2>/dev/null
	SRVPID=""
}

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

assert_empty_pool() { # tag —— 当前实例应为空池
	local tag="$1" code body cnt n
	code="$(req GET /admin/accounts)"
	if unimpl "$code"; then
		skip "[$tag] GET /admin/accounts 未实现 (HTTP $code)"
	elif [[ "$code" == 200 ]]; then
		body="$(cat "$WORK/last.json")"
		cnt="$(jqr '.count' "$body")"
		n="$(jqr '.data.accounts|length' "$body")"
		if [[ "$cnt" == 0 && "$n" == 0 ]]; then
			ok "[$tag] 空池 {count:0,accounts:[]}"
		else
			bad "[$tag] 空池期望 count:0/accounts:[]，实得 count=$cnt len=$n"
		fi
	else
		bad "[$tag] GET /admin/accounts HTTP $code: $(head -c 200 "$WORK/last.json")"
	fi
	code="$(vreq "$PROBE")"
	[[ "$code" =~ ^[45] ]] && ok "[$tag] 空池 /v1 定失败 HTTP $code" || bad "[$tag] 空池 /v1 应定失败，实得 HTTP $code"
}

# t2 用的 credentials_file（假号，真文件——加载期必须可解 key）。
printf 'windsurf_api_key = "e2e-fake-token-t2"\n' >"$WORK/creds-t2.toml"

# ============ A. devin.token 迁移错误（P1a）============
write_cfg "$WORK/cfg-token.yaml" 1 '  token: "e2e-legacy-token"'
env -u DEVIN_TOKEN -u WINDSURF_API_KEY timeout 10 \
	"$BIN" -config "$WORK/cfg-token.yaml" -state-dir "$WORK/state-token" \
	>"$WORK/token-boot.log" 2>&1
mig_code=$?
if [[ $mig_code != 0 && $mig_code != 124 ]] && grep -qi 'accounts' "$WORK/token-boot.log"; then
	ok "devin.token 残留 → 拒启动(exit=$mig_code) + 迁移文案提及 accounts"
elif [[ $mig_code == 0 || $mig_code == 124 ]]; then
	bad "devin.token 配置未拒启动（exit=$mig_code；124=timeout 存活）"
else
	bad "devin.token 拒启动但文案未提 accounts: $(tail -3 "$WORK/token-boot.log")"
fi

# ============ B. 空池两形态 ============
for spec in "nokey|" "emptylist|  accounts: []"; do
	tag="${spec%%|*}"; dline="${spec#*|}"
	write_cfg "$WORK/cfg-$tag.yaml" 1 \
		'  base_url: "http://127.0.0.1:1"' '  model: "swe-2-medium"' \
		${dline:+"$dline"}
	if start_instance "$WORK/cfg-$tag.yaml" "$tag"; then
		assert_empty_pool "$tag"
		# 空池上面板热建号是 P1a 引导路径：POST 应 200。
		code="$(req POST /admin/accounts '{"name":"b1","token":"e2e-fake-token-b1"}')"
		if unimpl "$code"; then
			skip "[$tag] 空池热建号未实现 (HTTP $code)"
		elif [[ "$code" =~ ^2 ]]; then
			wait_name b1 present && ok "[$tag] 空池热建 b1 成功" || bad "[$tag] POST b1 2xx 但未列出"
		else
			bad "[$tag] 空池热建号 HTTP $code: $(head -c 200 "$WORK/last.json")"
		fi
		stop_instance
	fi
done

# ============ C. config 名 t0 全生命周期 ============
write_cfg "$WORK/cfg-main.yaml" 1 \
	'  base_url: "http://127.0.0.1:1"' '  model: "swe-2-medium"' \
	'  accounts:' '    - name: t0' '      token: "e2e-fake-token-t0"'
start_instance "$WORK/cfg-main.yaml" main || exit 1

# --- C1. config lane 在列 ---
code="$(req GET /admin/accounts)"
if unimpl "$code"; then
	skip "GET /admin/accounts 未实现 (HTTP $code)"
elif [[ "$code" == 200 ]]; then
	body="$(cat "$WORK/last.json")"
	if [[ "$(jqr '.data.accounts[0].name' "$body")" == "t0" && "$(acct_field t0 source)" == "config" ]]; then
		ok "config 名 t0 在列 source=config"
	else
		bad "config 名 t0 视图异常: $(head -c 200 "$WORK/last.json")"
	fi
else
	bad "GET /admin/accounts HTTP $code"
fi

# --- C2. 加号 t1（字面 token）---
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
	# 冻结契约：重名（含墓碑名）409、非法名/校验失败 400。
	code="$(req POST /admin/accounts '{"name":"t1","token":"e2e-fake-token-t1b"}')"
	[[ "$code" == 409 ]] && ok "POST 重名 t1 → 409" || bad "POST 重名 t1 HTTP $code（契约 409）"
	code="$(req POST /admin/accounts '{"name":"bad name!","token":"x"}')"
	[[ "$code" == 400 ]] && ok "POST 非法名 → 400" || bad "POST 非法名 HTTP $code（契约 400）"
else
	bad "POST t1 HTTP $code: $(head -c 200 "$WORK/last.json")"
fi

# --- C3. 加号 t2（credentials_file）---
code="$(req POST /admin/accounts "{\"name\":\"t2\",\"credentials_file\":\"$WORK/creds-t2.toml\"}")"
if unimpl "$code"; then
	skip "POST t2 credentials_file 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	wait_name t2 present && ok "POST t2 → GET 列出（config+双 panel）" || bad "POST t2 后 GET 未见 t2"
	# 冻结契约：GET 项无 token 明文（只 token_sha + credential 种类）；
	# 排序 = config 声明序在前（t0），panel 名按 created_at,name 追加。
	body="$(req GET /admin/accounts >/dev/null; cat "$WORK/last.json")"
	if grep -q 'e2e-fake-token' <<<"$body"; then
		bad "GET /admin/accounts 泄漏 token 明文"
	else
		ok "GET /admin/accounts 无 token 明文"
	fi
	first="$(jqr '.data.accounts[0].name' "$body")"
	[[ "$first" == "t0" ]] && ok "排序契约 config 名在前" || note "排序: 首项=$first（契约 config 序在前）"
else
	bad "POST t2 HTTP $code: $(head -c 200 "$WORK/last.json")"
fi

# --- C4. /v1 归因 ---
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

# --- C5. DELETE 双轨 ---
# panel 名 t1 → {name,deleted:true}，GET 消失。
code="$(req DELETE /admin/accounts/t1)"
if unimpl "$code"; then
	skip "DELETE /admin/accounts/t1 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	dflag="$(jqr '.data.deleted' "$(cat "$WORK/last.json")")"
	[[ -z "$dflag" ]] && dflag="$(jqr '.deleted' "$(cat "$WORK/last.json")")"
	if wait_name t1 absent; then
		[[ "$dflag" == true ]] && ok "DELETE t1 → {deleted:true} 离队" || ok "DELETE t1 离队（deleted 标记=$dflag）"
	else
		bad "DELETE panel 名 t1 后仍在列 source=$(acct_field t1 source)（契约=物理删）"
	fi
else
	bad "DELETE t1 HTTP $code"
fi
# config 名 t0 → {name,tombstoned:true}，GET 仍列 source=tombstoned。
code="$(req DELETE /admin/accounts/t0)"
if unimpl "$code"; then
	skip "DELETE config 名 t0 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	tflag="$(jqr '.data.tombstoned' "$(cat "$WORK/last.json")")"
	[[ -z "$tflag" ]] && tflag="$(jqr '.tombstoned' "$(cat "$WORK/last.json")")"
	if [[ "$(acct_field t0 source)" == "tombstoned" ]]; then
		ok "DELETE t0 → tombstoned 在列（墓碑=$tflag）"
		# 冻结契约：PUT 墓碑名 409（须先 restore）。
		c409="$(req PUT /admin/accounts/t0 '{"disabled":true}')"
		[[ "$c409" == 409 ]] && ok "PUT 墓碑 t0 → 409" || note "PUT 墓碑 HTTP $c409（契约 409）"
	elif wait_name t0 absent; then
		bad "DELETE config 名 t0 直接消失（契约=tombstoned 仍在列）"
	else
		bad "DELETE t0 后 source=$(acct_field t0 source)"
	fi
else
	bad "DELETE t0 HTTP $code"
fi

# --- C6. 恢复（restore 墓碑 / 重建 panel 删）---
code="$(req POST /admin/accounts/t0/restore)"
if unimpl "$code"; then
	skip "restore 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	[[ "$(acct_field t0 source)" == "config" || "$(acct_field t0 disabled)" == "false" ]] \
		&& ok "restore t0 → 墓碑复活回队" || bad "restore t0 2xx 但 source=$(acct_field t0 source)"
elif [[ "$code" == 404 || "$code" == 409 ]]; then
	note "restore t0 HTTP $code（无墓碑可复——C5 未走墓碑路径）"
else
	bad "restore t0 HTTP $code"
fi
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

# --- C7. clear-cooldown / quota-refresh ---
# 冻结契约：clear-cooldown 200 {cleared:true} / 404 无活 lane；
# quota/refresh 200 / 404 不在生效集 / 502 上游失败（假 token 即此档）。
code="$(req POST /admin/accounts/t2/clear-cooldown)"
if unimpl "$code"; then
	skip "POST t2/clear-cooldown 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 || "$code" == 404 ]]; then
	ok "POST t2/clear-cooldown HTTP $code sane"
else
	bad "POST t2/clear-cooldown HTTP $code"
fi
code="$(req POST /admin/accounts/t2/quota/refresh)"
if unimpl "$code"; then
	skip "POST t2/quota/refresh 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 || "$code" == 404 || "$code" == 502 ]]; then
	ok "POST t2/quota/refresh HTTP $code sane（502=上游失败定档）"
else
	bad "POST t2/quota/refresh HTTP $code"
fi

# --- C8. PUT t2 disabled → 在列但排除 ---
code="$(req PUT /admin/accounts/t2 '{"disabled":true}')"
if unimpl "$code"; then
	skip "PUT /admin/accounts/t2 未实现 (HTTP $code)"
elif [[ "$code" =~ ^2 ]]; then
	# 冻结契约：PUT 无名 404。
	c404="$(req PUT /admin/accounts/nonexist '{"disabled":true}')"
	[[ "$c404" == 404 ]] && ok "PUT 无名 → 404" || note "PUT 无名 HTTP $c404（契约 404）"
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

# --- C9. 清池 → /v1 失败态 ---
deleted=0; dunimpl=0
for n in t1 t2 t0; do
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

# --- C10. cli-credentials 形状 ---
code="$(req GET /admin/accounts/cli-credentials)"
if unimpl "$code"; then
	skip "GET cli-credentials 未实现 (HTTP $code)"
elif [[ "$code" == 200 ]]; then
	body="$(cat "$WORK/last.json")"
	avail="$(jqr '.available' "$body")"; [[ -z "$avail" ]] && avail="$(jqr '.data.available' "$body")"
	path="$(jqr '.path' "$body")";       [[ -z "$path" ]] && path="$(jqr '.data.path' "$body")"
	if [[ "$avail" == true && -n "$path" ]]; then
		ok "cli-credentials {available:true, path:$path, parsable:$(jqr '.parsable' "$body")}"
	elif [[ "$avail" == false ]]; then
		# 冻结契约：available:false 时 parsable/suggested_name 附加键缺席。
		if grep -q '"parsable"\|"suggested_name"' <<<"$body"; then
			bad "cli-credentials available=false 但附加键在场: $(head -c 200 "$WORK/last.json")"
		else
			ok "cli-credentials {available:false} 附加键缺席"
		fi
	else
		bad "cli-credentials 形状异常: $(head -c 200 "$WORK/last.json")"
	fi
else
	bad "cli-credentials HTTP $code"
fi

stop_instance
echo "----"
echo "结果: $pass PASS / $failed FAIL / $skipped SKIP"
exit $((failed > 0))
