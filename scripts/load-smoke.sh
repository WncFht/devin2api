#!/usr/bin/env bash
# load-smoke.sh — sqlite 化后的并发冒烟：N 个读 worker 轮询重 /admin 读
# 端点，2 个写 worker 循环改 settings 与 model-registry（db 写路径），
# 可选打几个真 /v1 请求让 logs 写路径参与。判定：实例日志无
# SQLITE_BUSY/锁错误、admin 端点无非预期码；汇总每端点 p50/p95/max。
#
# 用法: scripts/load-smoke.sh <binary> <state-dir> [config.yaml] [并发N=20] [时长s=60]
#   state-dir 被整体复制到 mktemp，原目录不动；实例日志留在 mktemp 下，
#   结束时打印路径（自行 rm -rf 清理）。config 缺省取仓库 config.yaml；
#   无 auth.api_key 或 devin.model 时跳过 /v1 腿并在输出中注明。
set -euo pipefail
cd "$(dirname "$0")/.."

BIN="${1:?usage: load-smoke.sh <binary> <state-dir> [config.yaml] [N=20] [seconds=60]}"
STATE="${2:?}"
CONFIG="${3:-config.yaml}"
N="${4:-20}"
DURATION="${5:-60}"

# 读腿端点：面板重查询（日志列表/聚合/矩阵/配额/运行时指标）。
READ_TAGS=(logs usage stats metrics quota matrix)
READ_EPS=(
	"/admin/logs?limit=200"
	"/admin/usage"
	"/admin/stats"
	"/admin/metrics"
	"/admin/quota"
	"/admin/logs/matrix"
)

WORK="$(mktemp -d /tmp/load-smoke.XXXXXX)"
ST="$WORK/state"
mkdir -p "$ST/results"
cp -r "$STATE"/. "$ST/"

pick_port() { # 连接成功=已占用，换下一个；连接失败=空闲，采用
	local p
	for _ in $(seq 1 40); do
		p=$((33000 + RANDOM % 12000))
		(exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null || { echo "$p"; return; }
	done
	echo 31399
}

# listen 行整行替换为 127.0.0.1:<port>，冒烟端口不外绑。空闲端口检测到
# bind 之间仍有窗口（共享机上别的冒烟实例可能同时抢端口），撞上就换端口
# 重试，最多 5 次。
BPID=""
for attempt in 1 2 3 4 5; do
	PORT="$(pick_port)"
	sed -E "s/^[[:space:]]*listen:.*/  listen: \"127.0.0.1:$PORT\"/" "$CONFIG" >"$ST/config.yaml"
	"$BIN" -config "$ST/config.yaml" -state-dir "$ST" >"$ST/boot.log" 2>&1 &
	BPID=$!
	sleep 0.6
	if kill -0 "$BPID" 2>/dev/null && ! grep -q 'port already in use' "$ST/boot.log"; then
		break
	fi
	kill "$BPID" 2>/dev/null || true
	BPID=""
	[[ $attempt == 5 ]] && { echo "实例 bind 连续失败，日志：" >&2; tail -30 "$ST/boot.log" >&2; exit 1; }
done
WPIDS=()
cleanup() {
	kill "$BPID" 2>/dev/null || true
	for p in "${WPIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

# 首次 boot 要跑 legacy 导入，healthz 宽限到 120s。
for i in $(seq 1 600); do
	curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
	sleep 0.2
	[[ $i == 600 ]] && { echo "实例未起来（120s），日志：" >&2; tail -30 "$ST/boot.log" >&2; exit 1; }
done

# 凭据/模型从 config 抽取；api_key 首个命中即 auth.api_key。grep 无命中
# 返回 1，pipefail 下不交 || true 会杀脚本——缺行应落为空串走「跳过 /v1 腿」。
PASSWORD="$(grep -E '^\s*password:' "$CONFIG" | head -1 | sed -E 's/.*password:\s*//; s/["'"'"']//g' | tr -d ' ' || true)"
APIKEY="$(grep -E '^\s*api_key:' "$CONFIG" | head -1 | sed -E 's/.*api_key:\s*//; s/["'"'"']//g' | tr -d ' ' || true)"
MODEL="$(grep -E '^\s*model:' "$CONFIG" | head -1 | sed -E 's/.*model:\s*//; s/["'"'"']//g' | tr -d ' ' || true)"

END_TS=$(( $(date +%s) + DURATION ))

hit() { # tag method url [body] [token=PASSWORD] -> 追加 "tag code seconds" 到 $OUT
	local tag="$1" method="$2" url="$3" body="${4:-}" token="${5:-$PASSWORD}"
	local args=(-s -o /dev/null -w '%{http_code} %{time_total}' -X "$method" -H 'Content-Type: application/json')
	[ -n "$token" ] && args+=(-H "Authorization: Bearer $token")
	[ -n "$body" ] && args+=(--data "$body")
	local line
	line="$(curl "${args[@]}" "$url" 2>/dev/null)" || true
	[ -n "$line" ] || line="000 0"
	echo "$tag $line" >>"$OUT"
}

read_worker() {
	local id="$1" i=0 idx
	OUT="$ST/results/read-$id.log"
	while (( $(date +%s) < END_TS )); do
		idx=$((i % ${#READ_EPS[@]}))
		hit "${READ_TAGS[$idx]}" GET "http://127.0.0.1:$PORT${READ_EPS[$idx]}"
		i=$((i + 1))
	done
}

write_worker() {
	local id="$1" tog="$2"
	OUT="$ST/results/write-$id.log"
	while (( $(date +%s) < END_TS )); do
		hit settings PUT "http://127.0.0.1:$PORT/admin/settings/debug_log_enabled" "{\"value\":\"$tog\"}"
		# enabled:false 真写一行覆盖，随后 DELETE 清掉，不留垃圾。
		hit model_put PUT "http://127.0.0.1:$PORT/admin/model-registry" '{"model":"load-smoke-probe","enabled":false}'
		hit model_del DELETE "http://127.0.0.1:$PORT/admin/model-registry?model=load-smoke-probe"
		[ "$tog" = true ] && tog=false || tog=true
	done
}

v1_worker() { # 真上游请求让 logs 写路径参与；下游令牌是 auth.api_key 而非面板密码
	OUT="$ST/results/v1.log"
	local i
	for i in 1 2 3 4 5; do
		sleep $((DURATION / 6))
		(( $(date +%s) < END_TS )) || break
		hit v1chat POST "http://127.0.0.1:$PORT/v1/chat/completions" \
			"{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":1}" "$APIKEY"
	done
}

echo "== load-smoke: port=$PORT N=$N duration=${DURATION}s work=$WORK"
for i in $(seq 1 "$N"); do read_worker "$i" & WPIDS+=("$!"); done
for i in 1 2; do write_worker "$i" "$([ "$i" = 1 ] && echo true || echo false)" & WPIDS+=("$!"); done
if [ -n "$APIKEY" ] && [ -n "$MODEL" ]; then
	v1_worker & WPIDS+=("$!")
else
	echo "== config 缺 auth.api_key 或 devin.model，跳过 /v1 写腿"
fi

wait "${WPIDS[@]}" 2>/dev/null || true

fail=0
if ! kill -0 "$BPID" 2>/dev/null || ! curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1; then
	echo "FAIL 实例已退出或 healthz 不通"; fail=1
else
	echo "OK 实例存活"
fi

if grep -inE 'SQLITE_BUSY|database is locked|database table is locked' "$ST/boot.log" >"$WORK/busy.hits" 2>/dev/null; then
	fail=1
	echo "FAIL 实例日志出现锁错误："
	grep -inE -B2 -A2 'SQLITE_BUSY|database is locked|database table is locked' "$ST/boot.log" | head -40
else
	echo "OK 无 BUSY/locked"
fi
echo "info: request failed 行数 = $(grep -c 'request failed' "$ST/boot.log" || true)，level=ERROR 行数 = $(grep -c 'level=ERROR' "$ST/boot.log" || true)"

all="$WORK/all.log"
cat "$ST"/results/*.log >"$all" 2>/dev/null || true
printf '%-10s %6s %8s %8s %8s  %s\n' endpoint count p50_ms p95_ms max_ms codes
declare -A BAD=()
for tag in "${READ_TAGS[@]}" settings model_put model_del v1chat; do
	vals="$(awk -v t="$tag" '$1==t {print $3*1000}' "$all" | sort -n)"
	[ -z "$vals" ] && continue
	hist="$(awk -v t="$tag" '$1==t {c[$2]++} END {for (k in c) printf "%s:%d ", k, c[k]}' "$all")"
	non200="$(awk -v t="$tag" '$1==t && $2!="200" {n++} END {print n+0}' "$all")"
	read -r cnt p50 p95 mx <<<"$(echo "$vals" | awk '{a[NR]=$1} END {printf "%d %.0f %.0f %.0f", NR, a[int((NR+1)/2)], a[int((NR*95+99)/100)], a[NR]}')"
	printf '%-10s %6d %8.0f %8.0f %8.0f  %s\n' "$tag" "$cnt" "$p50" "$p95" "$mx" "$hist"
	# admin 腿非 200 一律 FAIL；v1chat 走真上游，非 200 只告警（闸门/上游
	# 拒绝不属迁移缺陷），000（实例不可达）仍 FAIL。
	if [ "$tag" = v1chat ]; then
		[ "$non200" -gt 0 ] && echo "WARN v1chat 非 200 计数=$non200（上游/闸门原因，不计 FAIL）"
		echo "$hist" | grep -q '000:' && BAD[$tag]=1
	else
		[ "$non200" -gt 0 ] && BAD[$tag]=1
	fi
done
if (( ${#BAD[@]} > 0 )); then
	fail=1
	echo "FAIL 非预期码端点: ${!BAD[*]}"
fi

echo "== 工作目录（实例日志/结果）保留于: $WORK"
(( fail == 0 )) && echo "LOAD-SMOKE PASS" || echo "LOAD-SMOKE FAIL"
exit "$fail"
