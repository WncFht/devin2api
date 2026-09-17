#!/usr/bin/env bash
# golden-diff.sh — 存储迁移的金样对账：同一 state 目录副本分别喂新旧两
# 个二进制，对全部 /admin 读取端点做归一化 diff。用于 D2/D3「JSON 契约
# 不变」的验收。
#
# 用法: scripts/golden-diff.sh <old-binary> <new-binary> <state-dir>
#   old/new 二进制各自起在空闲端口（config.yaml 的 listen 被临时改写），
#   state-dir 被复制两份互不污染。输出逐端点 PASS/DIFF 与首个差异摘要。
set -euo pipefail
cd "$(dirname "$0")/.."

OLD_BIN="${1:?usage: golden-diff.sh <old-bin> <new-bin> <state-dir> [config.yaml]}"
NEW_BIN="${2:?}"
STATE="${3:?}"
CONFIG="${4:-config.yaml}"

# 端点清单：矩阵/列表/聚合/注册表/配额/运行时指标。volatile 字段（时间戳、
# 指针、id、uptime、goroutine 类进程态）在 normalize 中剔除——契约对账
# 只关心结构与非瞬态值。
ENDPOINTS=(
	"/admin/logs?limit=200"
	"/admin/logs?status=4xx&limit=200"
	"/admin/logs?status_code=500&limit=200"
	"/admin/logs?model_like=swe&limit=200"
	"/admin/logs?log_source=all&limit=500"
	"/admin/logs/matrix"
	"/admin/usage"
	"/admin/stats"
	"/admin/metrics"
	"/admin/quota"
	"/admin/auth-tokens"
	"/admin/model-registry"
	"/admin/runtime-metrics"
	"/admin/active-requests"
	"/dashboard/summary"
	"/admin/api"
)

# 归一化：递归排序键；剥掉逐请求/逐进程必然漂移的字段。
NORMALIZE='
def strip: walk(if type=="object" then del(.id,.time,.at,.created_at,.updated_at,
  .last_used_at,.started_at,.dir,.debug_ref,.duration,.duration_ms,.uptime_ms,
  .goroutines,.heap_alloc,.rss,.cpu_percent,.gc_count,.pid,.version,.commit,
  .first_byte_time,.next_offset,.expires_at,.client_request_id,.upstream_request_id,
  .request_id,.minute_bucket,.daily_reset_at,.weekly_reset_at,.grace_period_end,
  .cost_5h_anchor,.cost_daily_period_start,.cost_monthly_period_start,
  .cost_weekly_period_start,
  .duration_seconds,.cpu_usage_percent,.cpu_user_seconds,.gc_cpu_percent,
  .gc_pause_total_ns,.heap_alloc_bytes,.heap_sys_bytes,.max_rss_bytes,
  .rss_bytes,.uptime_seconds,
  .log_root,.index_bytes,.db_bytes) else . end);
. | strip | if type=="object" then with_entries(if (.value|type)=="object" or
  (.value|type)=="array" then .value|=strip else . end) else . end
'

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
	for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
	rm -rf "$WORK"
}
trap cleanup EXIT

boot() { # bin statedir port -> pid
	local bin="$1" st="$2" port="$3"
	mkdir -p "$st"
	cp -r "$STATE"/. "$st/"
	# config 不随 state 目录走（部署布局里二者分家），用 4 号参数或仓库 config.yaml；
	# listen 行整行替换为 127.0.0.1:<port>，冒烟端口不外绑。
	sed -E "s/^[[:space:]]*listen:.*/  listen: \"127.0.0.1:$port\"/" "$CONFIG" >"$st/config.yaml"
	"$bin" -config "$st/config.yaml" -state-dir "$st" >"$st/boot.log" 2>&1 &
	echo $!
}

PASSWORD=""
for spec in "OLD:$OLD_BIN:31211" "NEW:$NEW_BIN:31212"; do
	IFS=: read -r tag bin port <<<"$spec"
	st="$WORK/$tag"
	pid="$(boot "$bin" "$st" "$port")"
	PIDS+=("$pid")
	for i in $(seq 1 50); do
		curl -sf "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && break
		sleep 0.2
		[[ $i == 50 ]] && { echo "$tag 实例未起来" >&2; cat "$st/boot.log" >&2; exit 1; }
	done
done

PASSWORD="$(grep -E '^\s*password:' "$CONFIG" | head -1 | sed -E 's/.*password:\s*//; s/["'"'"']//g' | tr -d ' ')"
AUTH=()
[[ -n "$PASSWORD" ]] && AUTH=(-H "Authorization: Bearer $PASSWORD")

fail=0
for ep in "${ENDPOINTS[@]}"; do
	a="$(curl -sf "${AUTH[@]}" "http://127.0.0.1:31211$ep" | jq -S "$NORMALIZE" 2>/dev/null || echo CURL_FAIL)"
	b="$(curl -sf "${AUTH[@]}" "http://127.0.0.1:31212$ep" | jq -S "$NORMALIZE" 2>/dev/null || echo CURL_FAIL)"
	if [[ "$a" == "$b" ]]; then
		echo "PASS $ep"
	else
		echo "DIFF $ep"
		diff <(echo "$a" | jq .) <(echo "$b" | jq .) | head -30 || true
		fail=1
	fi
done
exit $fail
