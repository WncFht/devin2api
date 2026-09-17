#!/usr/bin/env bash
# run.sh — ccpanel 前端自包含验证：构建二进制（缺失时）→ 空闲端口起
# 临时实例（临时 config + state dir，假上游凭据，面板检查不打上游）→
# healthz 就绪 → 依次跑 checks/ 套件 → SIGTERM 收尾。
#
# 用法: scripts/panel-verify/run.sh [--port N] [--keep]
#   --port  缺省自动挑空闲端口；--keep 跑完保留临时目录便于排查。
# 前置: node >= 20、go；首次自动 npm install 与 npx playwright install firefox。
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$DIR/../.." && pwd)"

PORT=""
KEEP=0
while [[ $# -gt 0 ]]; do
	case "$1" in
		--port) PORT="$2"; shift 2 ;;
		--keep) KEEP=1; shift ;;
		*) echo "未知参数: $1" >&2; exit 2 ;;
	esac
done

command -v node >/dev/null || { echo "需要 node >= 20" >&2; exit 1; }
command -v go >/dev/null || { echo "需要 go" >&2; exit 1; }

# node 依赖与 firefox 浏览器缺失时自动装；浏览器下载体积大，
# /tmp 小的机器先设 PLAYWRIGHT_BROWSERS_PATH 或 TMPDIR（见 README）。
# executablePath 只回路径不校验存在，得用 fs.accessSync 实测文件。
[[ -d "$DIR/node_modules/playwright" ]] || (cd "$DIR" && npm install --no-fund --no-audit)
if ! (cd "$DIR" && node -e 'require("fs").accessSync(require("playwright").firefox.executablePath())' 2>/dev/null); then
	echo "playwright firefox 未安装，执行 npx playwright install firefox" >&2
	(cd "$DIR" && npx playwright install firefox)
fi

[[ -x "$ROOT/devin-2api" ]] || (cd "$ROOT" && go build ./cmd/devin-2api)

if [[ -z "$PORT" ]]; then
	PORT="$(node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
fi

WORK="$(mktemp -d)"
PV_PID=""
cleanup() {
	# 只按捕获的 pid 杀：pkill -f 的模式会匹配发起者自己的命令行。
	[[ -n "$PV_PID" ]] && kill -TERM "$PV_PID" 2>/dev/null || true
	[[ "$KEEP" == "1" ]] || rm -rf "$WORK"
}
trap cleanup EXIT

# 最小可启动配置：devin.base_url/model 是 adapter 必填；token 填假值
# 避免启动时从本机 Devin CLI 凭据目录发现真实 token。password 非空 +
# api_key 与密码不同值，api_token 登录才会解出受限角色（同值算 admin）。
cat >"$WORK/config.yaml" <<EOF
server:
  listen: 127.0.0.1:$PORT
dashboard:
  password: testpw
auth:
  api_key: testkey
debug:
  enabled: true
devin:
  base_url: https://server.codeium.com
  model: swe-2-max
  token: pv-fake-token
EOF

"$ROOT/devin-2api" -config "$WORK/config.yaml" -state-dir "$WORK" >"$WORK/stdout.log" 2>"$WORK/stderr.log" &
PV_PID=$!

ready=0
for _ in $(seq 1 75); do
	if ! kill -0 "$PV_PID" 2>/dev/null; then
		echo "实例提前退出，stderr:" >&2
		tail -10 "$WORK/stderr.log" >&2
		exit 1
	fi
	curl -sf "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && { ready=1; break; }
	sleep 0.2
done
[[ "$ready" == "1" ]] || { echo "healthz 15 秒内未就绪" >&2; exit 1; }
echo "instance up on 127.0.0.1:$PORT (work=$WORK)"

export PV_BASE="http://127.0.0.1:$PORT" PV_ADMIN_PW=testpw PV_API_TOKEN=testkey PV_SHOTS="$DIR/shots"

failed=0
for check in "$DIR"/checks/*.js; do
	echo "== $(basename "$check") =="
	node "$check" || failed=$((failed + 1))
done

kill -TERM "$PV_PID" 2>/dev/null || true
wait "$PV_PID" 2>/dev/null || true
PV_PID=""

if [[ $failed -gt 0 ]]; then
	echo "FAIL: $failed check(s) failed（截图在 $DIR/shots）" >&2
	exit 1
fi
echo "PASS: all checks green"
