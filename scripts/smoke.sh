#!/usr/bin/env bash
# smoke.sh — 临时实例冒烟：源码构建 → 空闲端口起独立实例 → healthz +
# /v1/models 探针 → 立即关闭。对应「冒烟用空闲端口、验证完立即关闭、
# 不保留常驻侧实例」约定的机械化版本，需要 unix shell 环境。
#
# /v1/models 走 adapter 的 GetCliModelConfigs 真实上游调用但不烧 chat
# 配额，一条探针覆盖「配置加载 → 鉴权 → adapter → 上游 RPC」整条链。
#
# 用法: scripts/smoke.sh [--port 3005] [--config <config.yaml 路径>] [--no-upstream]
#   --config 缺省 ./config.yaml；独立状态目录由 mktemp 提供，logs 不污染
#   真实实例。端口被占或实例中途退出都会明确报错。
#   --no-upstream 跳过真上游探针（断言 /v1/models 对空 token 明确 502），
#   给 CI 这类无 token 环境用；两种模式都验证 SIGTERM 优雅退出。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=3005
SRC_CONFIG="config.yaml"
NO_UPSTREAM=0
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
	--no-upstream)
		NO_UPSTREAM=1
		shift
		;;
	*)
		echo "未知参数: $1" >&2
		exit 2
		;;
	esac
done

[[ -f "$SRC_CONFIG" ]] || {
	echo "找不到配置文件: $SRC_CONFIG（用 --config 指定）" >&2
	exit 1
}
if curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1; then
	echo "端口 :$PORT 已有 devin-2api 在监听，换 --port 或先停掉" >&2
	exit 1
fi

WORK="$(mktemp -d)"
SMOKE_PID=""
cleanup() {
	[[ -n "$SMOKE_PID" ]] && kill "$SMOKE_PID" 2>/dev/null || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# 独立状态目录：listen 改到冒烟端口；logs/gate-state 都落临时目录。
# 只换行尾 :port 段、保留既有 host——源配置若绑 127.0.0.1，整值替换成
# ":PORT" 会让冒烟实例短暂暴露到全部接口。
sed -E "/^[[:space:]]*listen:/s/:[0-9]+([\"']?[[:space:]]*)$/:$PORT\1/" "$SRC_CONFIG" >"$WORK/config.yaml"
grep -q "listen[[:space:]]*:[[:space:]]*\"*:$PORT" "$WORK/config.yaml" || {
	echo "未能把 server.listen 改写到 :$PORT，检查配置文件格式" >&2
	exit 1
}

go build -o "$WORK/devin-2api" ./cmd/devin-2api
"$WORK/devin-2api" -config "$WORK/config.yaml" -state-dir "$WORK" >"$WORK/stdout.log" 2>"$WORK/stderr.log" &
SMOKE_PID=$!

for _ in $(seq 1 75); do
	if ! kill -0 "$SMOKE_PID" 2>/dev/null; then
		echo "实例提前退出（端口冲突或配置错误），stderr:" >&2
		tail -5 "$WORK/stderr.log" >&2
		exit 1
	fi
	curl -sf "http://localhost:$PORT/healthz" >/dev/null 2>&1 && break
	sleep 0.2
done
curl -sf "http://localhost:$PORT/healthz" >/dev/null || {
	echo "healthz 15 秒内未通过" >&2
	tail -5 "$WORK/stderr.log" >&2
	exit 1
}

# 配置启用了 auth.api_key 时探针要带 key；从配置文件里按行提取，
# 兼容单/双引号与裸值三种 YAML 写法。
API_KEY="$(sed -nE "s/^[[:space:]]*api_key:[[:space:]]*['\"]?([^'\"[:space:]]+)['\"]?.*/\1/p" "$WORK/config.yaml" | head -1)"
AUTH=()
[[ -n "$API_KEY" ]] && AUTH=(-H "Authorization: Bearer $API_KEY")
if [[ "${NO_UPSTREAM}" == "1" ]]; then
	# 无 token 时端点应明确拒绝而非挂死：断言确切的 502——语义若变
	# （比如改成 401/503），让跑的人知道行为漂移了。
	code="$(curl -s -o /dev/null -m 10 -w '%{http_code}' "${AUTH[@]}" "http://localhost:$PORT/v1/models" || true)"
	[[ "${code}" == "502" ]] || {
		echo "/v1/models 空 token 期望 502，实得 ${code:-<timeout>}" >&2
		exit 1
	}
	UPSTREAM_DESC="/v1/models 空 token 明确 502"
else
	MODELS_BODY="$(curl -sf "${AUTH[@]}" "http://localhost:$PORT/v1/models")" || {
		echo "/v1/models 探针失败（鉴权或上游问题）" >&2
		exit 1
	}
	[[ -n "$MODELS_BODY" ]] || {
		echo "/v1/models 返回空体" >&2
		exit 1
	}
	UPSTREAM_DESC="/v1/models 200（响应前缀: ${MODELS_BODY:0:120}）"
fi

# SIGTERM 后无在途请求应立即干净退出；挂住说明 drain 路径坏了
#（生产上是 KeepAlive/TimeoutStopSec 兜底强杀，CI 在这里先拦住）。
kill -TERM "${SMOKE_PID}"
for _ in $(seq 100); do
	kill -0 "${SMOKE_PID}" 2>/dev/null || break
	sleep 0.2
done
kill -0 "${SMOKE_PID}" 2>/dev/null && {
	echo "SIGTERM 后 20s 仍未退出——drain 路径疑似挂住" >&2
	exit 1
}
wait "${SMOKE_PID}" 2>/dev/null || true
SMOKE_PID=""
echo "smoke ok: :$PORT healthz + ${UPSTREAM_DESC} + SIGTERM 优雅退出通过"
