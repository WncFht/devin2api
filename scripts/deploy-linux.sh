#!/usr/bin/env bash
# deploy-linux.sh — deploy.sh 的 Linux 对应物：systemd --user 托管 devin-2api。
# 用法见 --help；参数语义与 macOS 版一致。
#
# 运行目录 ${XDG_DATA_HOME:-~/.local/share}/devin-2api（二进制+config.yaml+logs），
# unit 写在 ${XDG_CONFIG_HOME:-~/.config}/systemd/user/devin-2api.service；
# 未加载时自动生成并 enable --now——首装与升级同一条命令。
#
# 注意：systemctl --user 需要 user manager（SSH 进来一般可用）；想让服务
# 在未登录时也常驻，跑 loginctl enable-linger $USER（脚本会提示）。
set -euo pipefail
cd "$(dirname "$0")/.."
source scripts/lib-deploy.sh

[[ "$(uname -s)" == "Linux" ]] || {
	echo "deploy-linux.sh 仅适用 Linux；macOS 用 scripts/deploy.sh" >&2
	exit 1
}
command -v systemctl >/dev/null || {
	echo "需要 systemd（找不到 systemctl）；无 systemd 的环境请直接前台运行二进制" >&2
	exit 1
}

UNIT="devin-2api.service"
UNIT_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/systemd/user"
RUNTIME="${DEVIN2API_RUNTIME:-${XDG_DATA_HOME:-${HOME}/.local/share}/devin-2api}"

do_uninstall() {
	local did=0
	if systemctl --user cat "${UNIT}" >/dev/null 2>&1; then
		systemctl --user disable --now "${UNIT}"
		rm -f "${UNIT_DIR}/${UNIT}"
		systemctl --user daemon-reload
		echo "==> unit disabled and removed (${UNIT_DIR}/${UNIT})"
		did=1
	fi
	remove_installed_binary && did=1
	[[ -f "${RUNTIME}/config.yaml" || -d "${RUNTIME}/logs" ]] &&
		echo "    保留 ${RUNTIME} 下 config.yaml 与 logs/；彻底清理: rm -rf '${RUNTIME}'"
	[[ "${did}" == "0" ]] && echo "nothing to remove"
}

parse_deploy_args "$@"

# 单实例约定（同 macOS 版）：排除掉 unit 托管的 MainPID 后列其余进程。
MANAGED_PID="$(systemctl --user show -p MainPID --value "${UNIT}" 2>/dev/null || true)"
warn_strays "${MANAGED_PID:-0}"

if [[ "${UNINSTALL}" == "1" ]]; then
	do_uninstall
	exit 0
fi

if [[ "${CHECK}" == "1" ]]; then
	detect_port 8080
	check_versions
	exit $?
fi

preflight_deploy
detect_port 8080
check_port_available "${MANAGED_PID:-0}"

VERSION="$(build_or_download "${RELEASE_TAG}")"
smoke_version ./devin-2api.new "${VERSION}"
install_binary devin-2api.new

# unit 未安装时生成并 enable --now（RunAtLoad 对应物）——首装场景。
# TimeoutStopSec=60 对齐 launchd ExitTimeOut：SIGTERM 后给 50s 排空 + 退出余量。
# ProtectSystem=strict 把全盘挂只读，ReadWritePaths 只对运行目录放行写——
# credentials.toml 等 token 来源只读不受影响。
FRESH_BOOT=0
if ! systemctl --user cat "${UNIT}" >/dev/null 2>&1; then
	echo "==> first install: 生成 ${UNIT_DIR}/${UNIT}"
	mkdir -p "${UNIT_DIR}"
	cat >"${UNIT_DIR}/${UNIT}" <<EOF
[Unit]
Description=devin-2api — OpenAI/Anthropic-compatible proxy for Devin
After=network-online.target

[Service]
ExecStart=${RUNTIME}/devin-2api -config ${RUNTIME}/config.yaml
WorkingDirectory=${RUNTIME}
Restart=always
RestartSec=5
TimeoutStopSec=60
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=${RUNTIME}
StandardOutput=append:${RUNTIME}/logs/stdout.log
StandardError=append:${RUNTIME}/logs/stderr.log

[Install]
WantedBy=default.target
EOF
	systemctl --user daemon-reload
	systemctl --user enable --now "${UNIT}"
	FRESH_BOOT=1
fi

if [[ "${NO_RESTART}" == "1" ]]; then
	echo "done (binary swapped, restart skipped)"
	exit 0
fi

# 刚 enable --now 的服务已在跑新二进制，restart 只会平白弹它一次。
OLD_PID=""
if [[ "${FRESH_BOOT}" == "1" ]]; then
	echo "==> service enabled and started"
else
	OLD_PID="$(systemctl --user show -p MainPID --value "${UNIT}" 2>/dev/null || true)"
	systemctl --user restart "${UNIT}"
fi

echo "==> waiting for healthz version=${VERSION} (old pid: ${OLD_PID:-?})"
RUNNING="$(wait_healthz_version "${HEALTH_URL}" "${VERSION}" 90)" || {
	echo "healthz 未出现新版本 (last=${RUNNING})" >&2
	dump_recent_log
	exit 1
}

NEW_PID="$(systemctl --user show -p MainPID --value "${UNIT}" 2>/dev/null || true)"
echo "==> running: pid=${NEW_PID:-?} version=${RUNNING}"

# user 服务随最后一个会话退出；要未登录也常驻需开 linger（免 root，
# 部分发行版经 polkit 弹授权）。
if ! loginctl show-user "${USER}" -p Linger --value 2>/dev/null | grep -qx yes; then
	echo "hint: 服务当前随登录会话存活；需常驻请执行: loginctl enable-linger ${USER}" >&2
fi

smoke_rc=0
smoke_upstream || smoke_rc=$?
print_summary "${RUNNING}" "systemctl --user restart ${UNIT}（重启）；--uninstall 卸载"
exit "${smoke_rc}"
