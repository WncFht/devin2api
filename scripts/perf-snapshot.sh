#!/usr/bin/env bash
# perf-snapshot.sh — 一次性能快照：源码构建 → upstreamstub 正常全流后端 →
# 空闲端口临时实例（pprof 开）→ loadtest 压测 → 抓取 CPU/heap/fgprof
# 剖析 → 聚合 index.jsonl 的延迟分解字段。全程不碰真实上游与配额，
# 产物落 outputs/perf/<时间戳>/，临时目录退出即清。
#
# 用法: scripts/perf-snapshot.sh [--requests 100] [--concurrency 8]
#   [--deltas 200] [--interval 5ms] [--ttfb 50ms] [--profile-seconds 25]
#   [--debug off] [--out <dir>]
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=3005
# stub 默认端口避开手工调试惯用的 48090（upstreamstub 故障注入演练常用它），
# 减少与残留调试进程撞车的概率；启动前仍会预检。
STUB_PORT=48190
PPROF_PORT=6060
CONCURRENCY=8
REQUESTS=100
DELTAS=200
DELTA_BYTES=32
INTERVAL=0
TTFB=0
PROFILE_SECONDS=25
DEBUG=on
OUT=""
while [[ $# -gt 0 ]]; do
	case "$1" in
	--port) PORT="$2"; shift 2 ;;
	--stub-port) STUB_PORT="$2"; shift 2 ;;
	--pprof-port) PPROF_PORT="$2"; shift 2 ;;
	--concurrency) CONCURRENCY="$2"; shift 2 ;;
	--requests) REQUESTS="$2"; shift 2 ;;
	--deltas) DELTAS="$2"; shift 2 ;;
	--delta-bytes) DELTA_BYTES="$2"; shift 2 ;;
	--interval) INTERVAL="$2"; shift 2 ;;
	--ttfb) TTFB="$2"; shift 2 ;;
	--profile-seconds) PROFILE_SECONDS="$2"; shift 2 ;;
	--debug) DEBUG="$2"; shift 2 ;;
	--out) OUT="$2"; shift 2 ;;
	*) echo "未知参数: $1" >&2; exit 2 ;;
	esac
done

[[ -z "$OUT" ]] && OUT="outputs/perf/$(date +%Y%m%d-%H%M%S)"

# 端口预检：任一端口被占直接失败——stub 端口被残留调试进程占用时，
# 静默继续会让实例打到错误的 stub 场景上（实测踩过：end-hang 残留桩
# 把每个请求拖成 tail-grace 30s）。
for port in "$PORT" "$STUB_PORT" "$PPROF_PORT"; do
	if nc -z 127.0.0.1 "$port" 2>/dev/null; then
		echo "端口 :$port 已被占用，换参数或先停掉占用进程" >&2
		exit 1
	fi
done

mkdir -p "$OUT"
WORK="$(mktemp -d)"
PIDS=()
cleanup() {
	for pid in "${PIDS[@]}"; do kill "$pid" 2>/dev/null || true; done
	rm -rf "$WORK"
}
trap cleanup EXIT

echo "== 构建 devin-2api / upstreamstub / loadtest =="
go build -o "$WORK/devin-2api" ./cmd/devin-2api
go build -o "$WORK/upstreamstub" ./cmd/upstreamstub
go build -o "$WORK/loadtest" ./cmd/loadtest

DEBUG_ENABLED=false
[[ "$DEBUG" == "on" ]] && DEBUG_ENABLED=true
cat >"$WORK/config.yaml" <<EOF
server:
  listen: ":$PORT"
  max_concurrency: 1024
debug:
  enabled: $DEBUG_ENABLED
  pprof_listen: "127.0.0.1:$PPROF_PORT"
devin:
  base_url: "http://127.0.0.1:$STUB_PORT"
  token: "perf-snapshot-dummy"
  model: "stub"
  max_rpm: 0
  force_http1: true
dashboard:
  password: ""
auth:
  api_key: ""
EOF

"$WORK/upstreamstub" -listen "127.0.0.1:$STUB_PORT" -scenario stream \
	-deltas "$DELTAS" -delta-bytes "$DELTA_BYTES" -interval "$INTERVAL" -ttfb "$TTFB" \
	>"$WORK/stub.log" 2>&1 &
STUB_PID=$!
PIDS+=($STUB_PID)
"$WORK/devin-2api" -config "$WORK/config.yaml" >"$WORK/instance.log" 2>&1 &
PIDS+=($!)

# stub 与实例分别验活：stub 绑定失败会在 log.Fatal 后退出，
# 等 healthz 前先确认它还活着，否则白白等满超时。
sleep 0.3
if ! kill -0 "$STUB_PID" 2>/dev/null; then
	echo "upstreamstub 未能启动：" >&2
	cat "$WORK/stub.log" >&2
	exit 1
fi

for _ in $(seq 1 75); do
	curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break
	sleep 0.2
done
curl -sf "http://localhost:$PORT/healthz" >/dev/null || {
	echo "实例 15 秒内未就绪" >&2
	tail -10 "$WORK/instance.log" >&2
	exit 1
}
curl -sf "http://127.0.0.1:$PPROF_PORT/debug/pprof/" >/dev/null || {
	echo "pprof 端口未就绪" >&2
	exit 1
}
URL="http://localhost:$PORT/v1/chat/completions"

echo "== 预热 =="
"$WORK/loadtest" -url "$URL" -c 2 -n 10 | sed 's/^/  /'

echo "== 延迟测量: -c $CONCURRENCY -n $REQUESTS =="
"$WORK/loadtest" -url "$URL" -c "$CONCURRENCY" -n "$REQUESTS" | tee "$OUT/report.txt"

# 持续负载盖住两次采样窗口：CPU profile 与 fgprof 各取一段，
# 前后各留 1s 让负载完全铺开/收尾。
echo "== 持续负载 $((2 * PROFILE_SECONDS + 6))s + 剖析采样 ${PROFILE_SECONDS}s x2 =="
"$WORK/loadtest" -url "$URL" -c "$CONCURRENCY" -duration "$((2 * PROFILE_SECONDS + 6))s" \
	>"$OUT/load-sustained.txt" 2>&1 &
LOAD_PID=$!
PIDS+=($LOAD_PID)
sleep 2
curl -sf "http://127.0.0.1:$PPROF_PORT/debug/pprof/profile?seconds=$PROFILE_SECONDS" -o "$OUT/cpu.pb.gz"
echo "  cpu.pb.gz 已采集"
curl -sf "http://127.0.0.1:$PPROF_PORT/debug/fgprof?seconds=$PROFILE_SECONDS" -o "$OUT/fgprof.pb.gz"
echo "  fgprof.pb.gz 已采集"
wait "$LOAD_PID" || true
PIDS=("${PIDS[@]/$LOAD_PID/}")
cat "$OUT/load-sustained.txt" | sed 's/^/  /'

curl -sf "http://127.0.0.1:$PPROF_PORT/debug/pprof/heap" -o "$OUT/heap.pb.gz"
curl -sf "http://127.0.0.1:$PPROF_PORT/debug/pprof/goroutine?debug=1" -o "$OUT/goroutine.txt"

# index.jsonl 的延迟分解：ready/sent/open/first_upstream/first_client
# 五点相减得四段耗时分布，回答「延迟加在链路的哪一段」。
INDEX="$WORK/logs/index.jsonl"
if [[ -f "$INDEX" ]] && command -v python3 >/dev/null; then
	cp "$INDEX" "$OUT/index.jsonl"
	python3 - "$INDEX" >"$OUT/latency-segments.txt" <<'PY'
import json, sys, statistics
segs = {"decode": [], "transform": [], "connect": [], "upstream_ttft": [], "egress": []}
for line in open(sys.argv[1]):
    e = json.loads(line)
    keys = ["request_ready_ms", "upstream_sent_ms", "upstream_open_ms", "first_upstream_ms", "first_client_ms"]
    if not all(e.get(k) is not None for k in keys):
        continue
    segs["decode"].append(e["request_ready_ms"])
    segs["transform"].append(e["upstream_sent_ms"] - e["request_ready_ms"])
    segs["connect"].append(e["upstream_open_ms"] - e["upstream_sent_ms"])
    segs["upstream_ttft"].append(e["first_upstream_ms"] - e["upstream_open_ms"])
    segs["egress"].append(e["first_client_ms"] - e["first_upstream_ms"])
print(f"{'segment':<16} {'avg':>7} {'p50':>7} {'p90':>7} {'p99':>7}  (ms)")
for name, vals in segs.items():
    if not vals:
        continue
    vals.sort()
    p = lambda q: vals[min(int(len(vals) * q), len(vals) - 1)]
    print(f"{name:<16} {statistics.mean(vals):7.1f} {p(.5):7} {p(.9):7} {p(.99):7}")
PY
	echo "== 服务端延迟分解（index.jsonl）=="
	cat "$OUT/latency-segments.txt"
fi

cat <<EOF

== 完成: $OUT ==
  report.txt           客户端压测报告（TTFB/总时长分位数）
  latency-segments.txt 服务端延迟分解（decode/transform/connect/ttft/egress）
  cpu.pb.gz            CPU 剖析（负载期间 ${PROFILE_SECONDS}s）
  fgprof.pb.gz         wall-clock 剖析（含 off-CPU 等待）
  heap.pb.gz / goroutine.txt / index.jsonl / load-sustained.txt
查看: go tool pprof -http=:8081 $OUT/cpu.pb.gz
      go tool pprof -http=:8081 $OUT/fgprof.pb.gz
PGO:  cp $OUT/cpu.pb.gz cmd/devin-2api/default.pgo  # 下次构建自动启用
EOF
