#!/usr/bin/env bash
# deploy.sh — 构建或下载并热替换本机 launchd 托管的 devin-2api（仅 macOS）。
# 用法见 --help。launchd 服务未加载时自动生成 plist 并 bootstrap，
# config.yaml 缺失时自动从 example 生成——首装与升级同一条命令：
# 新机器只要同步本仓库再跑 deploy.sh。Linux 用 scripts/deploy-linux.sh
# （systemd --user），两者共享 scripts/lib-deploy.sh。
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib-deploy.sh

[[ "$(uname -s)" == "Darwin" ]] || {
	echo "deploy.sh 仅适用 macOS（launchd）；Linux 用 scripts/deploy-linux.sh" >&2
	exit 1
}

# launchd label 跟随当前用户名（本机约定 com.<user>.devin-2api）；
# 需要固定名时可用 DEVIN2API_LABEL 覆盖。
LABEL="${DEVIN2API_LABEL:-com.${USER}.devin-2api}"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
# 运行目录独立于仓库：launchd 子进程对 ~/Desktop 的每次 open 都会被
# TCC 桌面文件夹授权挂起（仓库在 Desktop 下时 exec/config/logs 全部卡死），
# 因此二进制、config.yaml、logs/ 一律放 Application Support，仓库只保留
# logs -> RUNTIME/logs 的符号链接供排障读取。
RUNTIME="${DEVIN2API_RUNTIME:-${HOME}/Library/Application Support/devin-2api}"

do_uninstall() {
	local did=0
	if launchctl print "gui/$(id -u)/${LABEL}" >/dev/null 2>&1; then
		launchctl bootout "gui/$(id -u)/${LABEL}"
		echo "==> service booted out (${LABEL})"
		did=1
	fi
	if [[ -f "${PLIST}" ]]; then
		rm -f "${PLIST}"
		echo "==> removed ${PLIST}"
		did=1
	fi
	remove_installed_binary && did=1
	[[ -f "${RUNTIME}/config.yaml" || -d "${RUNTIME}/logs" ]] &&
		echo "    保留 ${RUNTIME} 下 config.yaml 与 logs/；彻底清理: rm -rf '${RUNTIME}'"
	[[ "${did}" == "0" ]] && echo "nothing to remove"
}

parse_deploy_args "$@"

# 单实例约定：launchd 托管的实例是唯一合法实例。部署前先列出其它
# devin-2api 进程（手动 ./devin-2api、遗忘的冒烟实例）——它们会抢端口、
# 分流请求，且不受 SIGTERM 优雅退出保护。
LAUNCHD_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/pid = /{print $3}' || true)"
warn_strays "${LAUNCHD_PID:-0}"

if [[ "${UNINSTALL}" == "1" ]]; then
	do_uninstall
	exit 0
fi

if [[ "${CHECK}" == "1" ]]; then
	detect_port 3003
	check_versions
	exit $?
fi

# 预检：依赖、config 引导（缺失自动生成）、token/端口冲突——全部在下载
# 之前拦截，错误原地带修复路径，不让它漂到 healthz 超时。
preflight_deploy
detect_port 3003
check_port_available "${LAUNCHD_PID:-0}"

VERSION="$(build_or_download "${RELEASE_TAG}")"
smoke_version ./devin-2api.new "${VERSION}"
install_binary devin-2api.new

# 服务未加载时生成 plist 并 bootstrap——首装场景（新机器同步仓库后直接
# 跑本脚本即可）。plist 内容与 docs/deployment.md 保持一致。
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
# 排空上限 50s + 新进程启动，预留 ~90s。
RUNNING="$(wait_healthz_version "${HEALTH_URL}" "${VERSION}" 90)" || {
	echo "healthz 未出现新版本 (last=${RUNNING})" >&2
	dump_recent_log
	exit 1
}

NEW_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^[ \t]*pid = /{print $3}' || true)"
echo "==> running: pid=${NEW_PID:-?} version=${RUNNING}"

# healthz 只证明进程活着；真链路冒烟打 /v1/models 验证上游鉴权。
smoke_rc=0
smoke_upstream || smoke_rc=$?
print_summary "${RUNNING}" "launchctl kickstart -k gui/$(id -u)/${LABEL}（重启）；--uninstall 卸载"
exit "${smoke_rc}"
