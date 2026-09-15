#!/usr/bin/env bash
# smoke.sh — 临时实例冒烟：源码构建 → 空闲端口起独立实例 → healthz +
# /v1/models 探针 → 立即关闭。对应「冒烟用空闲端口、验证完立即关闭、
# 不保留常驻侧实例」约定的机械化版本，需要 unix shell 环境。
#
# /v1/models 走 adapter 的 GetCliModelConfigs 真实上游调用但不烧 chat
# 配额，一条探针覆盖「配置加载 → 鉴权 → adapter → 上游 RPC」整条链。
#
# 用法: scripts/smoke.sh [--port 3005] [--config <config.yaml 路径>]
#   --config 缺省 ./config.yaml；独立运行目录由 mktemp 提供，logs 不污染
#   真实实例。端口被占或实例中途退出都会明确报错。
set -euo pipefail
cd "$(dirname "$0")/.."

PORT=3005
SRC_CONFIG="config.yaml"
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

# 独立运行目录：listen 改到冒烟端口；logs/gate-state 都落临时目录。
# 只换行尾 :port 段、保留既有 host——源配置若绑 127.0.0.1，整值替换成
# ":PORT" 会让冒烟实例短暂暴露到全部接口。
sed -E "/^[[:space:]]*listen:/s/:[0-9]+([\"']?[[:space:]]*)$/:$PORT\1/" "$SRC_CONFIG" >"$WORK/config.yaml"
grep -q "listen[[:space:]]*:[[:space:]]*\"*:$PORT" "$WORK/config.yaml" || {
	echo "未能把 server.listen 改写到 :$PORT，检查配置文件格式" >&2
	exit 1
}

go build -o "$WORK/devin-2api" ./cmd/devin-2api
"$WORK/devin-2api" -config "$WORK/config.yaml" >"$WORK/stdout.log" 2>"$WORK/stderr.log" &
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
MODELS_BODY="$(curl -sf "${AUTH[@]}" "http://localhost:$PORT/v1/models")" || {
	echo "/v1/models 探针失败（鉴权或上游问题）" >&2
	exit 1
}
[[ -n "$MODELS_BODY" ]] || {
	echo "/v1/models 返回空体" >&2
	exit 1
}
echo "smoke ok: :$PORT healthz + /v1/models 通过（响应前缀: ${MODELS_BODY:0:120}）"
