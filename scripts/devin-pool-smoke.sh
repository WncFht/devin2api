#!/usr/bin/env bash
# devin-pool-smoke.sh — 多账号池端到端冒烟：双 lane（好号 + 故意坏号）起临时
# 实例，验证 rendezvous 钉选、unauthenticated failover 换号、凭据冷却降级，
# 以及 logs 表 / meta.json / runtime-metrics 的逐账号归因字段。
#
# 用法:
#   DEVIN_TOKEN_GOOD=<tok> scripts/devin-pool-smoke.sh [--port 3199]
#   scripts/devin-pool-smoke.sh --config config.yaml   # 取首个 devin.accounts 凭据/model/base_url
#
# 坏号固定为 "devin-session-token$invalid.badtoken.for-smoke"（DEVIN_TOKEN_BAD
# 可覆盖）。每个会话亲和键经复刻的 rendezvous 打分预知钉选 lane——脚本按
# 需挑选钉到 bad 与 good 的键各三个，两轮请求覆盖「failover 换号」与
# 「冷却降级直发」两条路径。真实上游调用十几次，均为极小请求。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=3199
SRC_CONFIG=""
BAD_TOKEN="${DEVIN_TOKEN_BAD:-devin-session-token\$invalid.badtoken.for-smoke}"
GOOD_TOKEN="${DEVIN_TOKEN_GOOD:-}"
MODEL=""
BASE_URL=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--port)
		PORT="$2"
		shift 2
		;;
	--config)
		SRC_CONFIG="$2"
		shift 2
		;;
	*)
		echo "未知参数: $1" >&2
		exit 2
		;;
	esac
done

yaml_scalar() { # yaml_scalar <key> <file> — 首个匹配行的裸/引号标量
	sed -nE "s/^[[:space:]]*$1:[[:space:]]*['\"]?([^'\"[:space:]]+)['\"]?.*/\1/p" "$2" | head -1
}
if [[ -n "$SRC_CONFIG" ]]; then
	[[ -f "$SRC_CONFIG" ]] || {
		echo "找不到配置文件: $SRC_CONFIG" >&2
		exit 1
	}
	[[ -n "$GOOD_TOKEN" ]] || GOOD_TOKEN="$(yaml_scalar token "$SRC_CONFIG")"
	# accounts 条目可只给 credentials_file：解出 windsurf_api_key 当好号
	# 凭据（~/ 展开与相对路径锚定 config 目录，与 config.go 口径一致）。
	if [[ -z "$GOOD_TOKEN" ]]; then
		creds="$(yaml_scalar credentials_file "$SRC_CONFIG")"
		creds="${creds/#\~/$HOME}"
		[[ -n "$creds" && "$creds" != /* ]] && creds="$(cd "$(dirname "$SRC_CONFIG")" && pwd)/$creds"
		[[ -n "$creds" && -f "$creds" ]] &&
			GOOD_TOKEN="$(sed -nE 's/^[[:space:]]*windsurf_api_key[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$creds" | head -1)"
	fi
	MODEL="$(yaml_scalar model "$SRC_CONFIG")"
	BASE_URL="$(yaml_scalar base_url "$SRC_CONFIG")"
fi
MODEL="${MODEL:-swe-2-max}"
BASE_URL="${BASE_URL:-https://server.codeium.com}"
[[ -n "$GOOD_TOKEN" ]] || {
	echo "缺好号凭据：设 DEVIN_TOKEN_GOOD 或用 --config 指向含 devin.accounts 条目的配置" >&2
	exit 1
}
command -v jq >/dev/null || {
	echo "需要 jq 解析 logs 表导出 / runtime-metrics" >&2
	exit 1
}
command -v sqlite3 >/dev/null || {
	echo "需要 sqlite3 导出 logs 表 / 读 runtime_state" >&2
	exit 1
}
if curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1; then
	echo "端口 :$PORT 已有 devin-2api 在监听，换 --port 或先停掉" >&2
	exit 1
fi

WORK="$(mktemp -d)"
STATE_DIR="$WORK/state"
LOGS="$STATE_DIR/logs"
INSTANCE_PID=""
cleanup() {
	[[ -n "$INSTANCE_PID" ]] && kill "$INSTANCE_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

RUNID="$(date +%s%N | tail -c 9)"
API_KEY="smoke-key-$RUNID"
DASH_PASS="smoke-pass-$RUNID"
cat >"$WORK/config.yaml" <<EOF
auth:
  api_key: '$API_KEY'
dashboard:
  password: '$DASH_PASS'
debug:
  enabled: true
devin:
  base_url: '$BASE_URL'
  model: '$MODEL'
  force_http1: true
  accounts:
    - name: good
      token: '$GOOD_TOKEN'
    - name: bad
      token: '$BAD_TOKEN'
server:
  listen: ':$PORT'
  max_concurrency: 64
EOF

go build -o "$WORK/devin-2api" ./cmd/devin-2api
"$WORK/devin-2api" -config "$WORK/config.yaml" -state-dir "$STATE_DIR" >"$WORK/stdout.log" 2>"$WORK/stderr.log" &
INSTANCE_PID=$!
for _ in $(seq 1 75); do
	if ! kill -0 "$INSTANCE_PID" 2>/dev/null; then
		echo "实例提前退出，stderr:" >&2
		tail -10 "$WORK/stderr.log" >&2
		exit 1
	fi
	curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break
	sleep 0.2
done
curl -sf "http://localhost:$PORT/healthz" >/dev/null || {
	echo "healthz 15 秒内未通过" >&2
	tail -10 "$WORK/stderr.log" >&2
	exit 1
}
echo "实例就绪 :$PORT（state: $STATE_DIR）"

# 复刻 SessionAffinityKey + orderedLanes 的全健康档排序：亲和键是
# sha256(SessionKey) 前 16 字节的 hex，lane 分 = sha256(亲和键|lane名)
# 升序取首。hex 字符串比较与 bytes.Compare 同序。
affinity_key() { printf '%s' "$1" | sha256sum | cut -c1-32; }
lane_score() { printf '%s' "$1|$2" | sha256sum | cut -d' ' -f1; }
pinned_lane() {
	local aff sg sb
	aff="$(affinity_key "$1")"
	sg="$(lane_score "$aff" good)"
	sb="$(lane_score "$aff" bad)"
	if [[ "$sg" < "$sb" ]]; then echo good; else echo bad; fi
}

GOOD_KEYS=()
BAD_KEYS=()
for i in $(seq 1 80); do
	key="smk$RUNID-$i"
	if [[ "$(pinned_lane "$key")" == bad ]]; then
		[[ ${#BAD_KEYS[@]} -lt 3 ]] && BAD_KEYS+=("$key")
	else
		[[ ${#GOOD_KEYS[@]} -lt 3 ]] && GOOD_KEYS+=("$key")
	fi
	[[ ${#BAD_KEYS[@]} -eq 3 && ${#GOOD_KEYS[@]} -eq 3 ]] && break
done
if [[ ${#BAD_KEYS[@]} -ne 3 || ${#GOOD_KEYS[@]} -ne 3 ]]; then
	echo "80 个候选键没凑齐双 lane 钉选" >&2
	exit 1
fi
echo "钉选分布（复刻 rendezvous）：bad-pinned: ${BAD_KEYS[*]}  good-pinned: ${GOOD_KEYS[*]}"

RESULTS="$WORK/results.txt"
: >"$RESULTS"
chat() { # chat <round> <userkey> → "<reqid> <http_code>" 落 results
	local reqid="smoke-$1-$2" code
	code="$(curl -sS -o "$WORK/resp-$reqid.json" -m 300 -w '%{http_code}' \
		-H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
		-H "X-Client-Request-Id: $reqid" \
		-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"Reply with exactly one word: ok\"}],\"user\":\"$2\",\"max_tokens\":8}" \
		"http://localhost:$PORT/v1/chat/completions" 2>"$WORK/curl-$reqid.err" || echo 000)"
	echo "$reqid $code" >>"$RESULTS"
}

# 注意：实例本身也是本脚本的 & 后台任务，裸 wait 会连它一起等——
# 只等 chat 请求的 pid。
CHAT_PIDS=()
run_chats() { # run_chats <round> <key...>
	CHAT_PIDS=()
	local round="$1"
	shift
	for key in "$@"; do
		chat "$round" "$key" &
		CHAT_PIDS+=($!)
	done
	wait "${CHAT_PIDS[@]}"
}

# 第一轮：首个 bad-pinned 单独跑——此时 bad 尚健康，bad 先失败再 failover
# 到 good（冷却随失败同步落）；其后 bad 已降级，bad-2/3 与 good-* 并发直发。
echo "== 第一轮（bad-pinned #1 单独触发 failover）"
chat r1 "${BAD_KEYS[0]}"
echo "== 第一轮其余 5 键并发（bad 应已在冷却，全部直发 good）"
run_chats r1 "${BAD_KEYS[1]}" "${BAD_KEYS[2]}" "${GOOD_KEYS[@]}"
# 第二轮：同键重发——bad 仍在冷却，全部应落 good 且无换号。
echo "== 第二轮（全部 6 键重发，验证钉选稳定 + 降级持续）"
run_chats r2 "${BAD_KEYS[@]}" "${GOOD_KEYS[@]}"
sleep 1

# logs 表导出成 JSONL（docs 的「先导出再喂」口径），下游 jq 断言与文件时代同形。
INDEX="$WORK/index.jsonl"
sqlite3 -readonly -json "$STATE_DIR/devin-2api.db" "SELECT * FROM logs ORDER BY id" | jq -c '.[]' >"$INDEX" || {
	echo "logs 表导出失败（sqlite3 报错或 db 不可读）" >&2
	: >"$INDEX"
}

PASS=0
FAIL=0
check() { # check <断言名> <0 通过|非0 失败>
	if [[ "$2" == 0 ]]; then
		echo "PASS  $1"
		PASS=$((PASS + 1))
	else
		echo "FAIL  $1"
		FAIL=$((FAIL + 1))
	fi
}
index_field() { # index_field <reqid> <jq-expr>
	[[ -f "$INDEX" ]] || return 0
	jq -r "select(.client_request_id==\"$1\") | $2" "$INDEX" 2>/dev/null | head -1 || true
}

echo "== 断言"
if [[ -f "$INDEX" ]]; then
	echo "-- 索引摘要（crid/status/account/switches）"
	jq -r 'select((.client_request_id // "") | startswith("smoke-")) | "  \(.client_request_id)  \(.status_code)  account=\(.account // "-")  switches=\(.account_switches // 0)"' "$INDEX"
fi
bad_http=0
while read -r reqid code; do
	if [[ "$code" != 200 ]]; then
		bad_http=1
		echo "  $reqid → HTTP $code（body: $(head -c 200 "$WORK/resp-$reqid.json" 2>/dev/null)）"
	fi
done <"$RESULTS"
[[ "$(wc -l <"$RESULTS")" == 12 ]] || bad_http=1
check "12 个请求全部 HTTP 200" "$bad_http"

index_total=0
smoke_entries=0
if [[ -f "$INDEX" ]]; then
	index_total="$(wc -l <"$INDEX")"
	smoke_entries="$(jq -c 'select((.client_request_id // "") | startswith("smoke-"))' "$INDEX" | wc -l)"
fi
st=1
[[ "$index_total" -ge 12 && "$smoke_entries" -eq 12 ]] && st=0
check "logs 表导出含 12 条冒烟请求（总 $index_total 条）" "$st"

bad_account=0
for key in "${BAD_KEYS[@]}" "${GOOD_KEYS[@]}"; do
	for round in r1 r2; do
		acc="$(index_field "smoke-$round-$key" '.account // "none"')"
		if [[ "$acc" != good && "$acc" != bad ]]; then
			bad_account=1
			echo "  smoke-$round-$key account=$acc"
		fi
	done
done
check "每条索引 account ∈ {good,bad}" "$bad_account"

# bad-pinned #1 首轮：钉选到 bad → unauthenticated → failover 到 good。
acc="$(index_field "smoke-r1-${BAD_KEYS[0]}" '.account')"
sw="$(index_field "smoke-r1-${BAD_KEYS[0]}" '.account_switches // 0')"
st=1
[[ "$acc" == good && "$sw" -ge 1 ]] && st=0
check "bad-pinned 首击 failover：account=good 且 account_switches>=1（实测 $acc/$sw）" "$st"

# 其余 bad-pinned 首轮：bad 已冷却降级，good 直发——换号数应为 0。
demote_ok=0
for key in "${BAD_KEYS[1]}" "${BAD_KEYS[2]}"; do
	acc="$(index_field "smoke-r1-$key" '.account')"
	sw="$(index_field "smoke-r1-$key" '.account_switches // 0')"
	if [[ "$acc" != good || "$sw" != 0 ]]; then
		demote_ok=1
		echo "  smoke-r1-$key: account=$acc switches=$sw（期望 good/0——bad 应已被降级）"
	fi
done
check "bad-pinned 后续首轮直发 good（冷却降级生效）" "$demote_ok"

# 第二轮：全部键 good 直发；同键跨轮 account 一致即钉选稳定。
round2_ok=0
for key in "${BAD_KEYS[@]}" "${GOOD_KEYS[@]}"; do
	acc="$(index_field "smoke-r2-$key" '.account')"
	sw="$(index_field "smoke-r2-$key" '.account_switches // 0')"
	if [[ "$acc" != good || "$sw" != 0 ]]; then
		round2_ok=1
		echo "  smoke-r2-$key: account=$acc switches=$sw"
	fi
done
check "第二轮全部 account=good 且无换号" "$round2_ok"

# meta.json 归因：failover 请求留 upstream_account=good + attempts[0]=bad。
dir="$(index_field "smoke-r1-${BAD_KEYS[0]}" '.dir')"
meta_acc="none"
meta_att="[]"
first_att="none"
first_code="none"
if [[ -n "$dir" && -f "$LOGS/$dir/meta.json" ]]; then
	meta_acc="$(jq -r '.upstream_account // "none"' "$LOGS/$dir/meta.json")"
	meta_att="$(jq -c '.upstream_attempts // []' "$LOGS/$dir/meta.json")"
	first_att="$(jq -r '.[0].account // "none"' <<<"$meta_att")"
	first_code="$(jq -r '.[0].code // "none"' <<<"$meta_att")"
fi
st=1
[[ "$meta_acc" == good && "$first_att" == bad ]] && st=0
check "meta.json 归因：upstream_account=good 且 attempts[0]=bad（code=$first_code）" "$st"
echo "  meta.json upstream_attempts: $meta_att"

# runtime-metrics 的 accounts 段必须同时含两条 lane。
accounts_json="$(curl -sf -H "Authorization: Bearer $DASH_PASS" "http://localhost:$PORT/admin/runtime-metrics" | jq -c '.data.accounts | keys' 2>/dev/null || echo '[]')"
st=1
[[ "$(jq -r 'index("good") != null and index("bad") != null' <<<"$accounts_json")" == true ]] && st=0
check "runtime-metrics accounts 含 good+bad（实测 $accounts_json）" "$st"

# 闸门闩态按 lane 入库（runtime_state 键 gate:<name>；无闩事件时无行——仅报告）。
for name in good bad; do
	row="$(sqlite3 "$STATE_DIR/devin-2api.db" "SELECT value FROM runtime_state WHERE key='gate:$name'" 2>/dev/null || true)"
	if [[ -n "$row" ]]; then
		echo "INFO  gate:$name 已持久化: $(head -c 200 <<<"$row")"
	else
		echo "INFO  gate:$name 无持久态（本路径仅在闩事件时写）"
	fi
done

if [[ "$FAIL" -eq 0 ]]; then
	echo "== pool smoke ok: $PASS 断言全过"
else
	echo "== pool smoke FAILED: $FAIL 断言未过（$PASS 过）——工作目录保留供排查: $WORK" >&2
	trap - EXIT
	kill "$INSTANCE_PID" 2>/dev/null || true
	exit 1
fi
