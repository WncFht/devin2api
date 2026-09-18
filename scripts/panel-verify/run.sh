#!/usr/bin/env bash
# run.sh — ccpanel 前端自包含验证：构建二进制（缺失时）→ 空闲端口起
# 临时实例（临时 config + state dir，假上游凭据，面板检查不打上游）→
# healthz 就绪 → 依次跑 checks/ 套件 → SIGTERM 收尾。
#
# 用法: scripts/panel-verify/run.sh [--port N] [--keep]
#       scripts/panel-verify/run.sh --base URL [--admin-pw X] [--api-token Y]
#   --port  缺省自动挑空闲端口；--keep 跑完保留临时目录便于排查。
#   --base  打已在跑的实例（含生产）：跳过构建与实例生命周期，只跑检查；
#           --admin-pw/--api-token 指定登录凭据（缺省 testpw/testkey，
#           开放面板下任意值都能登 admin）。
# 前置: node >= 20（--base 模式不需要 go）；首次自动 npm install 与
# npx playwright install firefox。
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$DIR/../.." && pwd)"

PORT=""
KEEP=0
BASE=""
ADMIN_PW=""
API_TOKEN=""
while [[ $# -gt 0 ]]; do
	case "$1" in
		--base) BASE="$2"; shift 2 ;;
		--port) PORT="$2"; shift 2 ;;
		--admin-pw) ADMIN_PW="$2"; shift 2 ;;
		--api-token) API_TOKEN="$2"; shift 2 ;;
		--keep) KEEP=1; shift ;;
		*) echo "未知参数: $1" >&2; exit 2 ;;
	esac
done

command -v node >/dev/null || { echo "需要 node >= 20" >&2; exit 1; }

# node 依赖与 firefox 浏览器缺失时自动装；浏览器下载体积大，
# /tmp 小的机器先设 PLAYWRIGHT_BROWSERS_PATH 或 TMPDIR（见 README）。
# executablePath 只回路径不校验存在，得用 fs.accessSync 实测文件。
[[ -d "$DIR/node_modules/playwright" ]] || (cd "$DIR" && npm install --no-fund --no-audit)
if ! (cd "$DIR" && node -e 'require("fs").accessSync(require("playwright").firefox.executablePath())' 2>/dev/null); then
	echo "playwright firefox 未安装，执行 npx playwright install firefox" >&2
	(cd "$DIR" && npx playwright install firefox)
fi

PV_PID=""
WORK=""
cleanup() {
	# 只按捕获的 pid 杀：pkill -f 的模式会匹配发起者自己的命令行。
	[[ -n "$PV_PID" ]] && kill -TERM "$PV_PID" 2>/dev/null || true
	[[ -z "$WORK" || "$KEEP" == "1" ]] || rm -rf "$WORK"
}
trap cleanup EXIT

if [[ -n "$BASE" ]]; then
	BASE="${BASE%/}"
	curl -sf -m 8 "$BASE/healthz" >/dev/null 2>&1 || { echo "$BASE/healthz 不可达" >&2; exit 1; }
	echo "external instance: $BASE"
else
	command -v go >/dev/null || { echo "需要 go" >&2; exit 1; }

	WORK="$(mktemp -d)"

	# 每次现构建：web 资产 go:embed 进二进制，复用仓库根旧二进制会
	# 拿过期前端跑检查（踩过）。产物放临时目录，不污染仓库根。
	(cd "$ROOT" && go build -o "$WORK/devin-2api" ./cmd/devin-2api)

	if [[ -z "$PORT" ]]; then
		PORT="$(node -e 'const s=require("net").createServer().listen(0,"127.0.0.1",()=>{console.log(s.address().port);s.close()})')"
	fi

	# 最小可启动配置：devin.base_url/model 是 adapter 必填；token 填假值
	# 避免启动时从本机 Devin CLI 凭据目录发现真实 token。下游令牌不进
	# 配置——实例就绪后经 /admin/auth-tokens 现铸，明文与密码不同值，
	# api_token 登录才会解出受限角色（同值算 admin）。
	cat >"$WORK/config.yaml" <<EOF
server:
  listen: 127.0.0.1:$PORT
dashboard:
  password: ${ADMIN_PW:-testpw}
debug:
  enabled: true
devin:
  base_url: https://server.codeium.com
  model: swe-2-max
  accounts:
    - name: main
      token: pv-fake-token
EOF

	"$WORK/devin-2api" -config "$WORK/config.yaml" -state-dir "$WORK" >"$WORK/stdout.log" 2>"$WORK/stderr.log" &
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
	BASE="http://127.0.0.1:$PORT"
	echo "instance up on $BASE (work=$WORK)"

	# 现铸下游令牌：明文一次性出示，仓内只存哈希——无法在配置里预置。
	API_TOKEN="$(curl -sf -X POST -H "Authorization: Bearer ${ADMIN_PW:-testpw}" \
		-H 'Content-Type: application/json' -d '{"description":"panel-verify"}' \
		"$BASE/admin/auth-tokens" | node -pe 'JSON.parse(require("fs").readFileSync(0,"utf8")).data.token')" \
		|| { echo "面板铸令牌失败" >&2; exit 1; }
fi

export PV_BASE="$BASE" PV_ADMIN_PW="${ADMIN_PW:-testpw}" PV_API_TOKEN="${API_TOKEN:-testkey}" PV_SHOTS="$DIR/shots"

failed=0
for check in "$DIR"/checks/*.js; do
	echo "== $(basename "$check") =="
	node "$check" || failed=$((failed + 1))
done

if [[ -n "$PV_PID" ]]; then
	kill -TERM "$PV_PID" 2>/dev/null || true
	wait "$PV_PID" 2>/dev/null || true
	PV_PID=""
fi

if [[ $failed -gt 0 ]]; then
	echo "FAIL: $failed check(s) failed（截图在 $DIR/shots）" >&2
	exit 1
fi
echo "PASS: all checks green"
