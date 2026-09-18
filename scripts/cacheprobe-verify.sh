#!/usr/bin/env bash
# cacheprobe-verify.sh — 缓存门探针的桩侧验证：源码构建 → upstreamstub
# 语义缓存桩（-cache-mode）→ mktemp devin-2api → probe cacheprobe 两种
# mode 各跑一遍 → 断言 verdict hint 与 stub 语义一致。全程不碰真实上游
# 与配额——桩是按「两种竞争缓存模型」各自语义记账的 oracle，探针须能
# 在两边都判对。
#
# 用法: scripts/cacheprobe-verify.sh [--out <dir>]
#   断言：trajectory 桩下 verdict=trajectory-gated；content 桩下
#   verdict=content-addressed；两 mode（proxy 经实例 / upstream 直连）同断言。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=31195
STUB_PORT=31196
OUT="outputs/cacheprobe/$(date +%Y%m%d-%H%M%S)"
while [[ $# -gt 0 ]]; do
	case "$1" in
	--out) OUT="$2"; shift 2 ;;
	*) echo "未知参数: $1" >&2; exit 2 ;;
	esac
done

for port in "$PORT" "$STUB_PORT"; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null; then
		echo "端口 :$port 已被占用，先停掉占用进程" >&2
		exit 1
	fi
done

mkdir -p "$OUT"
WORK="$(mktemp -d)"
PIDS=()
cleanup() {
	for pid in ${PIDS[@]+"${PIDS[@]}"}; do kill "$pid" 2>/dev/null || true; done
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "== 构建 devin-2api / upstreamstub / probe =="
go build -o "$WORK/devin-2api" ./cmd/devin-2api
go build -o "$WORK/upstreamstub" ./cmd/upstreamstub
go build -o "$WORK/probe" ./cmd/probe

cat >"$WORK/config.yaml" <<EOF
server:
  listen: ":$PORT"
  max_concurrency: 16
debug:
  enabled: true
devin:
  base_url: "http://127.0.0.1:$STUB_PORT"
  accounts:
    - name: main
      token: "cacheprobe-dummy"
  model: "stub"
  max_rpm: 0
  force_http1: true
dashboard:
  password: ""
auth:
  api_key: "probe-key"
EOF

wait_healthz() {
	for _ in $(seq 1 75); do
		curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1 && return 0
		sleep 0.2
	done
	echo "实例 15 秒内未就绪" >&2
	return 1
}

expect_hint() {
	local file="$1" want="$2" label="$3"
	if ! grep -q "\"hint\": \"$want" "$file"; then
		echo "FAIL $label: verdict hint 不是 $want——" >&2
		grep '"hint"' "$file" >&2 || true
		return 1
	fi
	echo "PASS $label"
}

FAIL=0
for cache_mode in trajectory content; do
	case "$cache_mode" in
	trajectory) WANT="trajectory-gated" ;;
	content) WANT="content-addressed" ;;
	esac
	echo "== stub -cache-mode $cache_mode（期望 verdict: $WANT）=="

	"$WORK/upstreamstub" -listen "127.0.0.1:$STUB_PORT" -scenario stream -deltas 4 -delta-bytes 16 \
		-cache-mode "$cache_mode" >"$WORK/stub-$cache_mode.log" 2>&1 &
	STUB_PID=$!
	PIDS+=($STUB_PID)

	"$WORK/devin-2api" -config "$WORK/config.yaml" -state-dir "$WORK/state-$cache_mode" \
		>"$WORK/instance-$cache_mode.log" 2>&1 &
	INST_PID=$!
	PIDS+=($INST_PID)
	sleep 0.3
	kill -0 "$STUB_PID" 2>/dev/null || { cat "$WORK/stub-$cache_mode.log" >&2; exit 1; }
	wait_healthz || { tail -10 "$WORK/instance-$cache_mode.log" >&2; exit 1; }

	# proxy 模式：经实例 /v1/chat/completions，prompt_cache_key 控轨迹。
	"$WORK/probe" cacheprobe -mode proxy -proxy-url "http://127.0.0.1:$PORT" -proxy-key probe-key \
		-model stub -out "$OUT/proxy-$cache_mode.json" >"$OUT/proxy-$cache_mode.stdout" 2>"$OUT/proxy-$cache_mode.stderr" \
		|| { echo "FAIL proxy/$cache_mode: probe 退出非零" >&2; cat "$OUT/proxy-$cache_mode.stderr" >&2; FAIL=1; }
	expect_hint "$OUT/proxy-$cache_mode.json" "$WANT" "proxy/$cache_mode" || FAIL=1

	# upstream 模式：probe 直连桩，显式轨迹 id——同 stub 不同入口。
	DEVIN_TOKEN=dummy "$WORK/probe" -base-url "http://127.0.0.1:$STUB_PORT" \
		cacheprobe -mode upstream -model stub -out "$OUT/upstream-$cache_mode.json" \
		>"$OUT/upstream-$cache_mode.stdout" 2>"$OUT/upstream-$cache_mode.stderr" \
		|| { echo "FAIL upstream/$cache_mode: probe 退出非零" >&2; cat "$OUT/upstream-$cache_mode.stderr" >&2; FAIL=1; }
	expect_hint "$OUT/upstream-$cache_mode.json" "$WANT" "upstream/$cache_mode" || FAIL=1

	kill "$INST_PID" "$STUB_PID" 2>/dev/null || true
	wait "$INST_PID" 2>/dev/null || true
	wait "$STUB_PID" 2>/dev/null || true
	PIDS=()
done

cat <<EOF

== 完成: $OUT ==
  proxy-{trajectory,content}.json     经实例的腿测量与 verdict
  upstream-{trajectory,content}.json  直连上游（桩）的腿测量与 verdict
EOF
[[ "$FAIL" == "0" ]] || { echo "有断言失败" >&2; exit 1; }
