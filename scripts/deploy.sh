#!/usr/bin/env bash
# deploy.sh — 构建或下载并热替换本机 launchd 托管的 devin-2api。
# 用法: scripts/deploy.sh [--release <tag|latest>] [--no-restart]
#   --release     安装 GitHub Release 预编译二进制（校验 sha256）；缺省为源码构建
#   --no-restart  只替换二进制，不 kickstart（下次自然重启时生效）
set -euo pipefail
cd "$(dirname "$0")/.."

# launchd label 跟随当前用户名（本机约定 com.<user>.devin-2api）；
# 需要固定名时可用 DEVIN2API_LABEL 覆盖。
LABEL="${DEVIN2API_LABEL:-com.${USER}.devin-2api}"
HEALTH_URL="http://localhost:3003/healthz"
REPO_SLUG="$(git remote get-url origin | sed -E 's#.*github.com[:/]([^/]+/[^/.]+)(\.git)?$#\1#')"

# 单实例约定：launchd 托管的 :3003 是唯一合法实例。部署前先列出其它
# devin-2api 进程（手动 ./devin-2api、遗忘的冒烟实例）——它们会抢端口、
# 分流请求，且不受 SIGTERM 优雅退出保护。
LAUNCHD_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/pid = /{print $3}' || true)"
STRAYS="$(pgrep -fl 'devin-2api' | awk -v keep="${LAUNCHD_PID:-0}" '$1 != keep' | grep -v 'devin-2api.new' || true)"
if [[ -n "${STRAYS}" ]]; then
	echo "WARN: 非 launchd 托管的 devin-2api 进程（单实例约定，建议 kill <pid> 优雅关闭）:" >&2
	echo "${STRAYS}" >&2
fi

RELEASE_TAG=""
NO_RESTART=0
while [[ $# -gt 0 ]]; do
	case "$1" in
	--release)
		RELEASE_TAG="${2:?--release 需要 tag（如 v0.2.0）或 latest}"
		shift 2
		;;
	--no-restart)
		NO_RESTART=1
		shift
		;;
	*)
		echo "unknown arg: $1" >&2
		exit 2
		;;
	esac
done

if [[ -n "${RELEASE_TAG}" ]]; then
	if [[ "${RELEASE_TAG}" == "latest" ]]; then
		# api.github.com 匿名额度很低，优先走 gh keyring / git credential 取 token
		TOKEN="${GH_TOKEN:-}"
		if [[ -z "${TOKEN}" ]] && command -v gh >/dev/null; then
			TOKEN="$(gh auth token 2>/dev/null || true)"
		fi
		if [[ -z "${TOKEN}" ]]; then
			TOKEN="$(printf 'protocol=https\nhost=github.com\n' | git credential fill 2>/dev/null | awk -F= '/^password=/{print $2}')"
		fi
		RELEASE_TAG="$(curl -sf -m 10 ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} \
			"https://api.github.com/repos/${REPO_SLUG}/releases/latest" |
			python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])')" || {
			echo "failed to resolve latest release tag (auth or rate limit)" >&2
			exit 1
		}
	fi
	case "$(uname -s)-$(uname -m)" in
	Darwin-arm64) ASSET="devin-2api-darwin-arm64" ;;
	Darwin-x86_64) ASSET="devin-2api-darwin-amd64" ;;
	Linux-aarch64 | Linux-arm64) ASSET="devin-2api-linux-arm64" ;;
	Linux-x86_64) ASSET="devin-2api-linux-amd64" ;;
	*) echo "unsupported platform: $(uname -s)-$(uname -m)" >&2; exit 1 ;;
	esac
	BASE="https://github.com/${REPO_SLUG}/releases/download/${RELEASE_TAG}"
	echo "==> download ${ASSET} @ ${RELEASE_TAG}"
	curl -fL -o devin-2api.new "${BASE}/${ASSET}" || {
		echo "download failed — ${RELEASE_TAG} 无二进制资产（老发版只有镜像）" >&2
		exit 1
	}
	EXPECTED="$(curl -sfL "${BASE}/checksums.txt" | awk -v a="${ASSET}" '$2==a {print $1}')"
	ACTUAL="$(shasum -a 256 devin-2api.new | awk '{print $1}')"
	[[ -n "${EXPECTED}" && "${EXPECTED}" == "${ACTUAL}" ]] || {
		echo "sha256 mismatch (expected ${EXPECTED:-<missing>}, got ${ACTUAL})" >&2
		rm -f devin-2api.new
		exit 1
	}
	chmod +x devin-2api.new
	VERSION="${RELEASE_TAG}"
else
	VERSION="$(git describe --tags --always --dirty)"
	echo "==> build devin-2api ${VERSION}"
	go build -ldflags "-X main.version=${VERSION}" -o devin-2api.new ./cmd/devin-2api
fi

echo "==> smoke: 新二进制 -version"
./devin-2api.new -version | grep -qx "${VERSION}" || {
	echo "version mismatch in new binary" >&2
	rm -f devin-2api.new
	exit 1
}

mv devin-2api.new devin-2api
echo "==> binary replaced"

if [[ "${NO_RESTART}" == "1" ]]; then
	echo "done (binary swapped, restart skipped)"
	exit 0
fi

OLD_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^[ \t]*pid = /{print $3}' || true)"
launchctl kickstart -k "gui/$(id -u)/${LABEL}"

echo "==> waiting for healthz (old pid: ${OLD_PID:-?})"
# 新进程要回放 index.jsonl（数千条）并过 Gatekeeper 检查，实测 14s+。
for _ in $(seq 1 60); do
	HEALTH="$(curl -sf -m 2 "${HEALTH_URL}" 2>/dev/null || true)"
	if [[ -n "${HEALTH}" ]]; then
		break
	fi
	sleep 0.5
done
[[ -n "${HEALTH:-}" ]] || {
	echo "healthz did not come up in 30s; check logs/stderr.log" >&2
	exit 1
}

RUNNING="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("version","<none>"))' <<<"${HEALTH}")"
NEW_PID="$(launchctl print "gui/$(id -u)/${LABEL}" | awk '/^[ \t]*pid = /{print $3}')"
echo "==> running: pid=${NEW_PID} version=${RUNNING}"
if [[ "${RUNNING}" != "${VERSION}" ]]; then
	echo "WARN: healthz version ${RUNNING} != built ${VERSION} (端口可能被其它实例抢占)" >&2
	exit 1
fi
echo "done"
