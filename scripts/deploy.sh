#!/usr/bin/env bash
# deploy.sh — 构建并热替换本机 launchd 托管的 devin-2api。
# 用法: scripts/deploy.sh [--no-restart]
#   --no-restart  只构建替换二进制，不 kickstart（下次自然重启时生效）
set -euo pipefail
cd "$(dirname "$0")/.."

LABEL="com.devinuser.devin-2api"
HEALTH_URL="http://localhost:3003/healthz"
VERSION="$(git describe --tags --always --dirty)"

echo "==> build devin-2api ${VERSION}"
go build -ldflags "-X main.version=${VERSION}" -o devin-2api.new ./cmd/devin-2api

echo "==> smoke: 新二进制 -version"
./devin-2api.new -version | grep -qx "${VERSION}" || {
	echo "version mismatch in built binary" >&2
	rm -f devin-2api.new
	exit 1
}

mv devin-2api.new devin-2api
echo "==> binary replaced"

if [[ "${1:-}" == "--no-restart" ]]; then
	echo "done (binary swapped, restart skipped)"
	exit 0
fi

OLD_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^\s*pid = /{print $3}' || true)"
launchctl kickstart -k "gui/$(id -u)/${LABEL}"

echo "==> waiting for healthz (old pid: ${OLD_PID:-?})"
for _ in $(seq 1 20); do
	HEALTH="$(curl -sf -m 2 "${HEALTH_URL}" 2>/dev/null || true)"
	if [[ -n "${HEALTH}" ]]; then
		break
	fi
	sleep 0.5
done
[[ -n "${HEALTH:-}" ]] || {
	echo "healthz did not come up in 10s; check logs/stderr.log" >&2
	exit 1
}

RUNNING="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("version","<none>"))' <<<"${HEALTH}")"
NEW_PID="$(launchctl print "gui/$(id -u)/${LABEL}" | awk '/^\s*pid = /{print $3}')"
echo "==> running: pid=${NEW_PID} version=${RUNNING}"
if [[ "${RUNNING}" != "${VERSION}" ]]; then
	echo "WARN: healthz version ${RUNNING} != built ${VERSION} (端口可能被其它实例抢占)" >&2
	exit 1
fi
echo "done"
