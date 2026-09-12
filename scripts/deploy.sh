#!/usr/bin/env bash
# deploy.sh — 构建或下载并热替换本机 launchd 托管的 devin-2api。
# 用法: scripts/deploy.sh [--release <tag|latest>] [--no-restart] [--check]
#   --release     安装 GitHub Release 预编译二进制（校验 sha256）；缺省为源码构建
#   --no-restart  只替换二进制，不 kickstart（下次自然重启时生效）
#   --check       只对比 已安装/运行中/最新 release 版本，不做变更；
#                 与 latest 不一致时 exit 1（源码构建的超前版本也会触发）
#
# launchd 服务未加载时自动生成 plist 并 bootstrap，因此首装与升级同一条命令：
# 新机器只要同步本仓库再跑 deploy.sh。
set -euo pipefail
cd "$(dirname "$0")/.."

# launchd label 跟随当前用户名（本机约定 com.<user>.devin-2api）；
# 需要固定名时可用 DEVIN2API_LABEL 覆盖。
LABEL="${DEVIN2API_LABEL:-com.${USER}.devin-2api}"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
HEALTH_URL="http://localhost:3003/healthz"
# 运行目录独立于仓库：launchd 子进程对 ~/Desktop 的每次 open 都会被
# TCC 桌面文件夹授权挂起（仓库在 Desktop 下时 exec/config/logs 全部卡死），
# 因此二进制、config.yaml、logs/ 一律放 Application Support，仓库只保留
# logs -> RUNTIME/logs 的符号链接供排障读取。
RUNTIME="${DEVIN2API_RUNTIME:-${HOME}/Library/Application Support/devin-2api}"
REPO_SLUG="$(git remote get-url origin | sed -E 's#.*github.com[:/]([^/]+/[^/.]+)(\.git)?$#\1#')"

# api.github.com 匿名额度很低且私有 repo 需要凭据，依次尝试 gh keyring /
# git credential；公开 repo 拿不到也无妨（仅受匿名速率限制）。
gh_token() {
	local token="${GH_TOKEN:-}"
	if [[ -z "${token}" ]] && command -v gh >/dev/null; then
		token="$(gh auth token 2>/dev/null || true)"
	fi
	if [[ -z "${token}" ]]; then
		token="$(printf 'protocol=https\nhost=github.com\n' | git credential fill 2>/dev/null | awk -F= '/^password=/{print $2}')"
	fi
	printf '%s' "${token}"
}

latest_release_tag() {
	local token
	token="$(gh_token)"
	curl -sf -m 10 ${token:+-H "Authorization: Bearer ${token}"} \
		"https://api.github.com/repos/${REPO_SLUG}/releases/latest" |
		python3 -c 'import json,sys; print(json.load(sys.stdin)["tag_name"])'
}

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
CHECK=0
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
	--check)
		CHECK=1
		shift
		;;
	*)
		echo "unknown arg: $1" >&2
		exit 2
		;;
	esac
done

if [[ "${CHECK}" == "1" ]]; then
	INSTALLED="$("${RUNTIME}/devin-2api" -version 2>/dev/null || echo '<未安装>')"
	RUNNING="$(curl -sf -m 2 "${HEALTH_URL}" 2>/dev/null | python3 -c 'import json,sys; print(json.load(sys.stdin).get("version","<none>"))' 2>/dev/null || echo '<未运行>')"
	LATEST="$(latest_release_tag 2>/dev/null || echo '<查询失败>')"
	printf 'installed: %s\nrunning:   %s\nlatest:    %s\n' "${INSTALLED}" "${RUNNING}" "${LATEST}"
	[[ "${INSTALLED}" == "${LATEST}" ]]
	exit $?
fi

if [[ -n "${RELEASE_TAG}" ]]; then
	if [[ "${RELEASE_TAG}" == "latest" ]]; then
		RELEASE_TAG="$(latest_release_tag)" || {
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
	# 公开 repo 下 token 为空也无妨；留着 auth 头以兼容 repo 转 private 的场景，
	# github.com 重定向到 S3 预签名 URL 时 curl 不会跨主机转发 Authorization。
	TOKEN="$(gh_token)"
	# -C - 断点续传：失败留下半成品，重跑本脚本接着下（慢/抖网络到 GitHub 时
	# 有用）；若残留的是别的版本残片，下面的 sha256 校验会拦下并删除。
	curl -fL -C - ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} -o devin-2api.new "${BASE}/${ASSET}" || {
		echo "download failed — ${RELEASE_TAG} 无二进制资产（老发版只有镜像）或网络中断" >&2
		exit 1
	}
	EXPECTED="$(curl -sfL ${TOKEN:+-H "Authorization: Bearer ${TOKEN}"} "${BASE}/checksums.txt" | awk -v a="${ASSET}" '$2==a {print $1}')"
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

mkdir -p "${RUNTIME}/logs"
mv devin-2api.new "${RUNTIME}/devin-2api"
cmp -s config.yaml "${RUNTIME}/config.yaml" 2>/dev/null ||
	echo "==> config.yaml 与运行目录不一致，以仓库版本覆盖"
cp config.yaml "${RUNTIME}/config.yaml"
echo "==> installed ${RUNTIME}/devin-2api (config.yaml synced from repo)"

# 服务未加载时生成 plist 并 bootstrap——首装场景（新机器同步仓库后直接
# 跑本脚本即可）。plist 内容与 notes/macos-deployment.md 保持一致。
FRESH_BOOT=0
if ! launchctl print "gui/$(id -u)/${LABEL}" >/dev/null 2>&1; then
	if [[ ! -f "${PLIST}" ]]; then
		echo "==> first install: 生成 ${PLIST}"
		mkdir -p "$(dirname "${PLIST}")"
		cat >"${PLIST}" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>${LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>${RUNTIME}/devin-2api</string>
		<string>-config</string>
		<string>${RUNTIME}/config.yaml</string>
	</array>
	<key>WorkingDirectory</key><string>${RUNTIME}</string>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>ExitTimeOut</key><integer>60</integer>
	<key>StandardOutPath</key><string>${RUNTIME}/logs/stdout.log</string>
	<key>StandardErrorPath</key><string>${RUNTIME}/logs/stderr.log</string>
</dict>
</plist>
EOF
	fi
	launchctl bootstrap "gui/$(id -u)" "${PLIST}"
	FRESH_BOOT=1
fi

if [[ "${NO_RESTART}" == "1" ]]; then
	echo "done (binary swapped, restart skipped)"
	exit 0
fi

# 刚 bootstrap 的服务已在跑新二进制，kickstart 只会平白弹它一次。
OLD_PID=""
if [[ "${FRESH_BOOT}" == "1" ]]; then
	echo "==> service bootstrapped (RunAtLoad 已启动新进程)"
else
	OLD_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^[ \t]*pid = /{print $3}' || true)"
	launchctl kickstart -k "gui/$(id -u)/${LABEL}"
fi

echo "==> waiting for healthz version=${VERSION} (old pid: ${OLD_PID:-?})"
# 优雅重启期间旧进程继续应答 healthz（旧版本 + draining 标记），
# 首次 200 不代表新实例已接管——必须轮询到版本匹配才确认。
# 排空上限 50s + 新进程 Gatekeeper/启动，预留 ~90s。
RUNNING=""
for _ in $(seq 1 180); do
	HEALTH="$(curl -sf -m 2 "${HEALTH_URL}" 2>/dev/null || true)"
	if [[ -n "${HEALTH}" ]]; then
		RUNNING="$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("version","<none>"))' <<<"${HEALTH}")"
		[[ "${RUNNING}" == "${VERSION}" ]] && break
	fi
	sleep 0.5
done
[[ "${RUNNING}" == "${VERSION}" ]] || {
	echo "healthz 未出现新版本 (last=${RUNNING:-<none>}); check logs/stderr.log" >&2
	exit 1
}

NEW_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^[ \t]*pid = /{print $3}' || true)"
echo "==> running: pid=${NEW_PID:-?} version=${RUNNING}"
echo "done"
