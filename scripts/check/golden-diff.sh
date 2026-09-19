#!/usr/bin/env bash
# golden-diff.sh — 存储迁移的金样对账：同一 state 目录副本分别喂新旧两
# 个二进制，对全部 /admin 读取端点做归一化 diff。用于 D2/D3「JSON 契约
# 不变」的验收。
#
# 用法: scripts/check/golden-diff.sh <old-binary> <new-binary> <state-dir> [config.yaml] [--traffic] [--keep-db]
#   old/new 二进制各自起在空闲端口（config.yaml 的 listen 被临时改写），
#   state-dir 被复制两份互不污染。输出逐端点 PASS/DIFF 与首个差异摘要。
#   GD_PORT_BASE 改基准端口（默认 41711，new 侧 +1）——并发跑多份对拍
#   时各给一段，否则 healthz 会串到别人实例上（已踩过）。
#   --keep-db  sqlite→sqlite 对账：state 里的 devin-2api.db 用 sqlite3
#              .backup 拿一致性快照进副本（活 WAL 直接 cp 是撕裂副本），
#              不还原 .migrated、不删库走迁移路径。假设 state 已完成迁移；
#              缺省（无 --keep-db）仍走「还原文件 → 新侧重跑导入」的旧路。
#   GD_SINCE/GD_UNTIL 收窄对账历史窗（RFC3339），默认全开覆盖全部存量行。
#
# 三个阶段：
#   1. reads   —— JSON 端点逐字段对账 + export 的 CSV 逐字节 / JSON 轻归一化
#   2. debug   —— 从源 logs 表（无库则 index.jsonl）抽样目录（普通/失败/
#                failover），各侧用自己的 /admin/logs?q=<dir> 解出数字 id，
#                再对 debug-logs detail / file / merged 对账（兼容 D2 后
#                id 语义从 ms 伪造变自增主键的两侧差异）
#   3. traffic —— --traffic 开启：两侧各打同形确定性请求（注册表停用
#                模型 → model_disabled 本地拒，零上游成本；加一条 401
#                喂 rejects 环），然后重跑 reads + 对新生目录做 debug
#                对账。专测 D2 的写路径。
set -euo pipefail
cd "$(dirname "$0")/../.."

OLD_BIN="${1:?usage: golden-diff.sh <old-bin> <new-bin> <state-dir> [config.yaml] [--traffic] [--keep-db]}"
NEW_BIN="${2:?}"
STATE="${3:?}"
CONFIG="${4:-config.yaml}"
TRAFFIC=0
KEEP_DB=0
for a in "$@"; do
	[[ "$a" == "--traffic" ]] && TRAFFIC=1
	[[ "$a" == "--keep-db" ]] && KEEP_DB=1
done

# 端点清单：矩阵/列表/聚合/注册表/配额/运行时指标 + 面板设置/生效配置 +
# dashboard 只读面。上游依赖型（status/model-pricing/model-test/
# update/check）与进程日志（process-log 各侧 stderr 不同）不进来——
# 对账只管确定性存储投影。volatile 字段（时间戳、指针、id、uptime、
# goroutine 类进程态）在 normalize 中剔除——契约对账只关心结构与非瞬态值。
GD_SINCE="${GD_SINCE:-2020-01-01T00:00:00Z}"   # 全开下界：覆盖全部存量行
GD_UNTIL="${GD_UNTIL:-9999-12-31T00:00:00Z}" # 远未来上界：今天永不落窗外
W="since=$GD_SINCE&until=$GD_UNTIL"           # 覆盖全量数据的显式历史窗
ENDPOINTS=(
	"/admin/logs?limit=200"
	"/admin/logs?limit=50&offset=150"
	"/admin/logs?limit=1"
	"/admin/logs?status=4xx&limit=200"
	"/admin/logs?status_code=500&limit=200"
	"/admin/logs?result=failed&limit=200"
	"/admin/logs?model_like=swe&limit=200"
	"/admin/logs?log_source=all&limit=500"
	"/admin/logs?log_source=proxy&$W&limit=200"
	"/admin/logs?$W&limit=500"
	"/admin/logs?$W&limit=500&offset=4000"
	"/admin/logs?range=yesterday&limit=200"
	"/admin/logs?range=this_week&limit=500"
	"/admin/logs?range=last_week&limit=500"
	"/admin/logs?range=custom&start_time=1758000000000&end_time=1789900000000&limit=500"
	"/admin/logs?api=anthropic&$W&limit=300"
	"/admin/logs?api=openai-chat&$W&limit=300"
	"/admin/logs?api=responses-ws&$W&limit=50"
	"/admin/logs?upstream_protocol=devin&$W&limit=200"
	"/admin/logs?auth_token_id=4&$W&limit=300"
	"/admin/logs?q=swe-2&$W&limit=300"
	"/admin/logs?q=devin_connect&$W&limit=100"
	"/admin/logs?status=%21200&$W&limit=300"
	"/admin/logs?status=%3E%3D400&$W&limit=300"
	"/admin/logs?status=200,500&$W&limit=300"
	"/admin/logs?status_class=5xx&$W&limit=300"
	"/admin/logs?error_stage=devin_connect&$W&limit=100"
	"/admin/logs?error_stage=rate_gate&$W&limit=100"
	"/admin/logs?error_stage=client_disconnected&$W&limit=100"
	"/admin/logs?account=gd-nonexist&$W&limit=50"
	"/admin/logs/bootstrap"
	"/admin/logs/matrix"
	"/admin/logs/matrix?since=$GD_SINCE"
	"/admin/logs/matrix?since=2030-01-01T00:00:00Z"
	"/admin/usage"
	"/admin/stats"
	"/admin/stats?range=yesterday"
	"/admin/stats?range=this_month"
	"/admin/stats/filter-options"
	"/admin/metrics"
	"/admin/quota"
	"/admin/auth-tokens"
	"/admin/auth-tokens?range=this_week"
	"/admin/model-registry"
	"/admin/models"
	"/admin/config"
	"/admin/settings"
	"/admin/settings/debug_log_enabled"
	"/admin/runtime-metrics"
	"/admin/active-requests"
	"/dashboard/summary"
	"/dashboard/metrics"
	"/dashboard/stats"
	"/dashboard/stats?range=this_week"
	"/dashboard/stats/filter-options"
	"/dashboard/logs?limit=50"
	"/dashboard/logs?status=4xx&limit=50"
	"/dashboard/logs?range=this_week&limit=100"
	"/dashboard/logs/bootstrap"
	"/dashboard/models"
	"/dashboard/session"
	"/public/protocols"
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
  .window_hours,.burn_per_hour,.burn_per_day,.hours_left,.exhausted_at,
  .survives_until_reset,.remaining,.reset_at,
  .server_time,.now,.generated_at,.mtime,.mod_time,.last_modified,
  .log_id,.stream_avg_ttfb,.non_stream_avg_rt,.last_at,.last_request_at,.qps_current,
  .avg_qps,.avg_rpm,.peak_qps,.peak_rpm,.rpm,.qps,.pace_allowance,
  .last_request_id,.last_success_id,.last_error_id,
  .log_rows,.log_row_retention_days,
  .log_root,.index_bytes,.db_bytes,.recent) else . end);
. | strip | if type=="object" then with_entries(if (.value|type)=="object" or
  (.value|type)=="array" then .value|=strip else . end) else . end
  | walk(if type=="number" and (. != floor) then (.*1e9|round)/1e9 else . end)
  | walk(if type=="object" and (.health_timeline|type)=="array"
      then .health_timeline |= length else . end)
'

# 轻归一化：export 的是历史行本体——time/dir/started_at 是必须一致的
# 数据，只剥 id 系（ms 伪造 vs 自增主键，D2 已知契约变化）。
LIGHT='
def strip: walk(if type=="object" then del(.id,.created_at,.upstream_request_id,
  .client_request_id) else . end);
. | strip
'

# 文件端点的 text 字段内嵌整份 meta/error JSON——traffic leg 两侧各自
# 生成，时间戳必然漂移；解析内层后剥瞬态字段再比结构。
FILEJQ='
def inner: walk(if type=="object" then del(.started_at,.finished_at,
  .duration_ms,.request_ready_ms,.upstream_sent_ms,.upstream_open_ms,
  .first_upstream_ms,.first_client_ms,.upstream_done_ms,.upstream_request_id,
  .client_request_id) else . end);
.data.text? |= (try (fromjson|inner) catch .)
'

WORK="$(mktemp -d)"
PIDS=()
cleanup() {
	for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null || true; done
	rm -rf "$WORK"
}
trap cleanup EXIT

BASE_DB=""
# 探针令牌明文：boot() 内按哈希注入两侧令牌仓（先定义，boot 调用时取值）。
GD_PROBE_KEY="gd-probe-key"
if [[ "$KEEP_DB" == 1 && -f "$STATE/devin-2api.db" ]]; then
	# sqlite→sqlite 对账：.backup 拿一次一致性快照供两侧共用——活 WAL
	# 直 cp 是撕裂副本；两侧各拍一次又会被快照间隙的新写入做成假 DIFF
	# （活实例在跑时 logs/usage 必漂）。
	BASE_DB="$WORK/base.db"
	sqlite3 "file:$STATE/devin-2api.db?mode=ro" ".backup '$BASE_DB'"
fi

boot() { # bin statedir port -> pid
	local bin="$1" st="$2" port="$3"
	mkdir -p "$st"
	cp -r "$STATE"/. "$st/"
	rm -f "$st"/devin-2api.db "$st"/devin-2api.db-wal "$st"/devin-2api.db-shm
	if [[ -n "$BASE_DB" ]]; then
		# keep-db：两侧喂同一份快照，不还原 .migrated、不重跑导入。
		cp "$BASE_DB" "$st/devin-2api.db"
	else
		# state 源可能已被迁移过（导入器把源文件改名 .migrated）——在副本里
		# 还原回文件形态，让旧侧有数据可读、新侧自己跑一遍导入；
		# 不复制活库：活实例的 WAL 在写入中，拷贝是撕裂快照。
		find "$st" -name '*.migrated' -exec sh -c 'mv "$1" "${1%.migrated}"' _ {} \;
	fi
	# config 不随 state 目录走（部署布局里二者分家），用 4 号参数或仓库 config.yaml；
	# listen 行整行替换为 127.0.0.1:<port>，冒烟端口不外绑。
	sed -E "s/^[[:space:]]*listen:.*/  listen: \"127.0.0.1:$port\"/" "$CONFIG" >"$st/config.yaml"
	# 探针令牌注入：keep-db 走 sqlite INSERT，导入路径走 auth_tokens.json
	# 追加（字段与 legacy 文件同形）；两侧同料注入 → 行全等。无库无文件
	# = 空仓开放，凭据照样放行。
	gd_probe_hash="$(printf '%s' "$GD_PROBE_KEY" | sha256sum | cut -d' ' -f1)"
	if [[ -f "$st/devin-2api.db" ]]; then
		sqlite3 "$st/devin-2api.db" \
			"INSERT OR IGNORE INTO auth_tokens (token,description) VALUES ('$gd_probe_hash','golden-diff probe')" 2>/dev/null || true
	elif [[ -f "$st/auth_tokens.json" ]]; then
		python3 - "$st/auth_tokens.json" "$gd_probe_hash" <<'PY'
import json, sys
path, tok = sys.argv[1], sys.argv[2]
with open(path) as f:
    doc = json.load(f)
ids = [t.get("id", 0) for t in doc.get("tokens", [])]
doc.setdefault("tokens", []).append({
    "id": max(ids, default=0) + 1, "token": tok,
    "description": "golden-diff probe",
    "created_at": "2020-01-01T00:00:00Z", "is_active": True})
doc["next_id"] = max(doc.get("next_id", 0), max(ids, default=0) + 2)
with open(path, "w") as f:
    json.dump(doc, f, indent=2)
PY
	fi
	# 关掉启动即采的配额采样：两侧各采一条会造成 forecast 窗口末点漂移，
	# 对账只验证导入的历史点与同一套 Go 预测代码。
	sed -i -E 's/^([[:space:]]*quota_interval_minutes:).*/\1 0/' "$st/config.yaml"
	"$bin" -config "$st/config.yaml" -state-dir "$st" >"$st/boot.log" 2>&1 &
	echo $!
}

OLDP="${GD_PORT_BASE:-41711}" NEWP=$((OLDP+1))
for spec in "OLD:$OLD_BIN:$OLDP" "NEW:$NEW_BIN:$NEWP"; do
	IFS=: read -r tag bin port <<<"$spec"
	st="$WORK/$tag"
	pid="$(boot "$bin" "$st" "$port")"
	PIDS+=("$pid")
	# healthz 通不算数：reuseport 允许两个实例并绑同端口，健康应答
	# 可能来自别的对拍实例。身份核对 = 我方 pid 存活 + healthz 返回
	# 的 version 含本二进制的 vcs.revision 前缀（buildinfo 取不到时
	# 只能退化为存活校验）。
	want_rev="$(go version -m "$bin" 2>/dev/null | sed -n 's/.*vcs.revision=//p' | cut -c1-7)"
	ok=0
	for i in $(seq 1 50); do
		kill -0 "$pid" 2>/dev/null || { echo "$tag 进程已退出" >&2; cat "$st/boot.log" >&2; exit 1; }
		hz="$(curl -sf "http://127.0.0.1:$port/healthz" 2>/dev/null)" || { sleep 0.2; continue; }
		if [[ -n "$want_rev" ]]; then
			got="$(echo "$hz" | jq -r '.version // ""')"
			[[ "$got" == *"$want_rev"* ]] || { echo "$tag 端口 $port 上是异己实例 version=$got（期望含 $want_rev）——换 GD_PORT_BASE" >&2; exit 1; }
		fi
		ok=1; break
	done
	[[ $ok == 1 ]] || { echo "$tag 实例未起来" >&2; cat "$st/boot.log" >&2; exit 1; }
done

PASSWORD="$(grep -E '^\s*password:' "$CONFIG" | head -1 | sed -E 's/.*password:\s*//; s/["'"'"']//g' | tr -d ' ' || true)"
AUTH=()
[[ -n "$PASSWORD" ]] && AUTH=(-H "Authorization: Bearer $PASSWORD")
# /v1 探针凭据：两侧各注入同一哈希的令牌行（注入点见 boot()），走
# 真实 Resolve 路径且 logs.key_hash/auth-tokens 对账零漂移——逐侧现
# 铸会得到不同明文哈希，post-traffic 读端点必出假 DIFF。
VAUTH=(-H "Authorization: Bearer $GD_PROBE_KEY")

fail=0

fetch() { # port path -> body or CURL_FAIL
	curl -sf "${AUTH[@]}" "http://127.0.0.1:$1$2" 2>/dev/null || echo CURL_FAIL
}

report() { # tag a b —— a/b 已是归一化后的字符串（或原始字节串）
	local tag="$1" a="$2" b="$3"
	if [[ "$a" == "$b" ]]; then
		echo "PASS $tag"
	else
		echo "DIFF $tag"
		diff <(echo "$a" | jq . 2>/dev/null || echo "$a") \
			<(echo "$b" | jq . 2>/dev/null || echo "$b") | head -30 || true
		fail=1
	fi
}

reads() { # 标签前缀
	local p="$1" ep a b extra
	for ep in "${ENDPOINTS[@]}"; do
		extra=''
		case "$ep" in
			# config 视图里 listen 是两实例各自的临时端口、file_mtime/
			# loaded_at/path 是逐进程态——剥离后比配置本体。
			/admin/config) extra='| walk(if type=="object" then del(.file_mtime,.loaded_at,.path,.listen) else . end)' ;;
		esac
		if [[ -n "$p" ]]; then
			# traffic 段的探针请求两侧各自真实计时：新侧写路径多一次
			# SQL INSERT，duration 相差几 ms——omitempty 让 0ms 侧整个
			# dur_s/gen_ms 字段缺省、3ms 侧出现，聚合均值也吃进这
			# 几 ms。post/ 轮把这些探针时长衍生物剥掉，只对结构。
			extra="$extra"'| walk(if type=="object" then del(.dur_s,.tps,.gen_ms,.avg_duration_ms,.avg_duration_seconds,.avg_first_byte_time_seconds) else . end)'
		fi
		a="$(fetch "$OLDP" "$ep" | jq -S "$NORMALIZE $extra" 2>/dev/null || echo CURL_FAIL)"
		b="$(fetch "$NEWP" "$ep" | jq -S "$NORMALIZE $extra" 2>/dev/null || echo CURL_FAIL)"
		report "$p$ep" "$a" "$b"
	done
}

reads ""

# --- export：CSV 全历史列逐字节对账；JSON 走轻归一化（保 time/dir）。
# 显式 since/until——无参默认 range=today 只剩当天几行，历史覆盖为零。
EXP_Q="$W"
a="$(fetch "$OLDP" "/admin/logs/export?format=csv&$EXP_Q")"
b="$(fetch "$NEWP" "/admin/logs/export?format=csv&$EXP_Q")"
report "/admin/logs/export?format=csv" "$a" "$b"

a="$(fetch "$OLDP" "/admin/logs/export?format=json&$EXP_Q" | jq -S "$LIGHT" 2>/dev/null || echo CURL_FAIL)"
b="$(fetch "$NEWP" "/admin/logs/export?format=json&$EXP_Q" | jq -S "$LIGHT" 2>/dev/null || echo CURL_FAIL)"
report "/admin/logs/export?format=json" "$a" "$b"

# --- debug 对账：目录身份（dir 名）稳定，数字 id 各侧自己解 ---
# 抽样：最近一条普通 + 最近一条带 error_stage + 最近一条 failover 换号。
# 样本源自动适配：源 state 有 devin-2api.db 走 logs 表（db 时代），
# 否则读 index.jsonl（含 .migrated——文件时代/迁移对账路径）。
SAMPLE="$(python3 - "$STATE" <<'PY'
import json, sys, os, sqlite3
want = {"any": None, "error": None, "switch": None, "old": None}
state = sys.argv[1]
db = os.path.join(state, "devin-2api.db")
rows = []
if os.path.exists(db):
    con = sqlite3.connect("file:%s?mode=ro" % db, uri=True)
    rows = con.execute(
        "SELECT dir, error_stage, account_switches FROM logs "
        "WHERE dir!='' ORDER BY id").fetchall()
else:
    path = os.path.join(state, "logs", "index.jsonl")
    if not os.path.exists(path) and os.path.exists(path + ".migrated"):
        path += ".migrated"  # 源已迁移：索引在 .migrated 里
    try:
        for line in open(path):
            try:
                e = json.loads(line)
            except json.JSONDecodeError:
                continue
            rows.append((e.get("dir"), e.get("error_stage"),
                         e.get("account_switches")))
    except FileNotFoundError:
        pass
for d, stage, sw in rows:
    if not d:
        continue
    if want["old"] is None:
        want["old"] = d
    want["any"] = d
    if stage:
        want["error"] = d
    if sw:
        want["switch"] = d
seen = set()
for k, v in want.items():
    if v and v not in seen:
        seen.add(v)
        print(f"{k}\t{v}")
PY
)"

side_id() { # port dir -> 该侧数字 log id；q 在显式历史窗内解析，
	# 拿不到回落 meta.json 的 started_at ms（FindDirByStartedAt 兜底路径；
	# meta 按源 state 形态取：logs/<dir>/ 文件或 debug_files 表）
	local id
	id="$(fetch "$1" "/admin/logs?q=$2&$W&limit=5" | jq -r '.data[0].id // empty' 2>/dev/null)"
	if [[ -z "$id" ]]; then
		id="$(python3 - "$STATE" "$2" <<'PY'
import json, sys, os, sqlite3, gzip, datetime
state, d = sys.argv[1], sys.argv[2]
raw = None
fp = os.path.join(state, "logs", d, "meta.json")
if os.path.exists(fp):
    raw = open(fp, "rb").read()
else:
    db = os.path.join(state, "devin-2api.db")
    if os.path.exists(db):
        row = sqlite3.connect("file:%s?mode=ro" % db, uri=True).execute(
            "SELECT content FROM debug_files WHERE dir=? AND name='meta.json'",
            (d,)).fetchone()
        raw = row[0] if row else None
try:
    if raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    ms = int(datetime.datetime.fromisoformat(
        json.loads(raw)["started_at"].replace("Z", "+00:00")).timestamp() * 1000)
    print(ms)
except Exception:
    pass
PY
)"
	fi
	echo "$id"
}

debug_diff() { # dir —— 每侧解 id 后对 detail/file/merged 三件套
	local dir="$1" oi ni
	oi="$(side_id "$OLDP" "$dir")"; ni="$(side_id "$NEWP" "$dir")"
	if [[ -z "$oi" || -z "$ni" ]]; then
		echo "SKIP debug/$dir (id 解析失败 old=${oi:-none} new=${ni:-none})"
		return
	fi
	local ea eb
	ea="$(fetch "$OLDP" "/admin/debug-logs/$oi" | jq -S "$NORMALIZE" 2>/dev/null || echo CURL_FAIL)"
	eb="$(fetch "$NEWP" "/admin/debug-logs/$ni" | jq -S "$NORMALIZE" 2>/dev/null || echo CURL_FAIL)"
	report "debug-logs/$dir" "$ea" "$eb"
	for f in meta.json error.json 01-http-request.json; do
		ea="$(fetch "$OLDP" "/admin/debug-logs/$oi/file/$f" | jq -S "$FILEJQ" 2>/dev/null || echo CURL_FAIL)"
		eb="$(fetch "$NEWP" "/admin/debug-logs/$ni/file/$f" | jq -S "$FILEJQ" 2>/dev/null || echo CURL_FAIL)"
		report "debug-logs/$dir/file/$f" "$ea" "$eb"
	done
	ea="$(fetch "$OLDP" "/admin/debug-logs/$oi/file/meta.json?raw=1")"
	eb="$(fetch "$NEWP" "/admin/debug-logs/$ni/file/meta.json?raw=1")"
	report "debug-logs/$dir/file/meta.json?raw=1" "$ea" "$eb"
	ea="$(fetch "$OLDP" "/admin/debug-logs/$oi/merged" | jq -S . 2>/dev/null || echo CURL_FAIL)"
	eb="$(fetch "$NEWP" "/admin/debug-logs/$ni/merged" | jq -S . 2>/dev/null || echo CURL_FAIL)"
	report "debug-logs/$dir/merged" "$ea" "$eb"
}

if [[ -n "$SAMPLE" ]]; then
	while IFS=$'\t' read -r kind dir; do
		debug_diff "$dir"
	done <<<"$SAMPLE"
else
	echo "SKIP debug/* (logs 表/index.jsonl 均无样本)"
fi

# --- traffic leg：两侧打同形请求再对账，专测写路径（--traffic 开启） ---
if [[ "$TRAFFIC" == 1 ]]; then
	PROBE="gd-probe-x"
	for port in "$OLDP" "$NEWP"; do
		curl -sf "${AUTH[@]}" -X PUT -H 'Content-Type: application/json' \
			-d "{\"model\":\"$PROBE\",\"enabled\":false}" \
			"http://127.0.0.1:$port/admin/model-registry" >/dev/null || echo "WARN registry PUT failed :$port" >&2
	done
	# model_disabled 本地拒：带调试目录与 index 行，不触上游。4xx 会被
	# curl -f 吃掉，用裸 -s + -D 同时拿状态码、X-Request-Id 与错误体。
	for tag in O N; do
		[[ "$tag" == O ]] && port=$OLDP || port=$NEWP
		curl -s "${VAUTH[@]}" -X POST -H 'Content-Type: application/json' \
			-D "$WORK/$tag-hdr" -o "$WORK/$tag-body.json" \
			-d "{\"model\":\"$PROBE\",\"messages\":[{\"role\":\"user\",\"content\":\"gd\"}],\"stream\":false}" \
			"http://127.0.0.1:$port/v1/chat/completions" || true
	done
	O_DIR="$(grep -i '^x-request-id:' "$WORK/O-hdr" | tr -d '\r' | awk '{print $2}')"
	N_DIR="$(grep -i '^x-request-id:' "$WORK/N-hdr" | tr -d '\r' | awk '{print $2}')"
	O_CODE="$(head -1 "$WORK/O-hdr" | awk '{print $2}')"
	N_CODE="$(head -1 "$WORK/N-hdr" | awk '{print $2}')"
	report "traffic/model_disabled_status" "$O_CODE" "$N_CODE"
	report "traffic/model_disabled_body" \
		"$(jq -S 'del(.error.debug_ref,.error.at,.debug_ref,.request_id)' "$WORK/O-body.json" 2>/dev/null || cat "$WORK/O-body.json")" \
		"$(jq -S 'del(.error.debug_ref,.error.at,.debug_ref,.request_id)' "$WORK/N-body.json" 2>/dev/null || cat "$WORK/N-body.json")"
	# 401 管线前拒绝喂 rejects 环（无目录无行）。
	O_401="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer gd-wrong' \
		-H 'Content-Type: application/json' -d '{"model":"x","messages":[]}' \
		"http://127.0.0.1:$OLDP/v1/chat/completions" || true)"
	N_401="$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Authorization: Bearer gd-wrong' \
		-H 'Content-Type: application/json' -d '{"model":"x","messages":[]}' \
		"http://127.0.0.1:$NEWP/v1/chat/completions" || true)"
	report "traffic/401_status" "$O_401" "$N_401"
	# 写路径落盘后重跑全量读端点。
	sleep 1
	reads "post/"
	# 新生目录的 debug 对账（两侧 dir 名不同，各自解）。
	if [[ -n "$O_DIR" && -n "$N_DIR" ]]; then
		oi="$(side_id "$OLDP" "$O_DIR")"; ni="$(side_id "$NEWP" "$N_DIR")"
		if [[ -n "$oi" && -n "$ni" ]]; then
			# 新生目录两侧各自落盘：size 随时间戳位数漂移，只比结构。
			a="$(fetch "$OLDP" "/admin/debug-logs/$oi" | jq -S "$NORMALIZE | walk(if type==\"object\" then del(.size) else . end)" 2>/dev/null || echo CURL_FAIL)"
			b="$(fetch "$NEWP" "/admin/debug-logs/$ni" | jq -S "$NORMALIZE | walk(if type==\"object\" then del(.size) else . end)" 2>/dev/null || echo CURL_FAIL)"
			report "traffic/debug-logs" "$a" "$b"
			a="$(fetch "$OLDP" "/admin/debug-logs/$oi/file/meta.json" | jq -S "$FILEJQ | del(.data.size)" 2>/dev/null || echo CURL_FAIL)"
			b="$(fetch "$NEWP" "/admin/debug-logs/$ni/file/meta.json" | jq -S "$FILEJQ | del(.data.size)" 2>/dev/null || echo CURL_FAIL)"
			report "traffic/debug-logs/meta.json" "$a" "$b"
		else
			echo "SKIP traffic/debug-logs (id 解析失败 old=${oi:-none} new=${ni:-none})"
		fi
	else
		echo "SKIP traffic/debug-logs (X-Request-Id 未捕获: old=${O_DIR:-none} new=${N_DIR:-none})"
	fi
	for port in "$OLDP" "$NEWP"; do
		curl -sf "${AUTH[@]}" -X DELETE "http://127.0.0.1:$port/admin/model-registry?model=$PROBE" >/dev/null || true
	done
fi

exit $fail
