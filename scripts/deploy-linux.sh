#!/usr/bin/env bash
# deploy-linux.sh — deploy.sh 的 Linux 对应物：systemd --user 托管 devin-2api。
# 用法见 --help；参数语义与 macOS 版一致。
#
# XDG 规范布局：二进制 ${HOME}/.local/bin，config.yaml 入
# ${XDG_CONFIG_HOME:-~/.config}/devin-2api，logs/ 与状态文件入
# ${XDG_STATE_HOME:-~/.local/state}/devin-2api（旧版统一运行目录
# ${XDG_DATA_HOME:-~/.local/share}/devin-2api 由 migrate_legacy_runtime 收编）；
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
BIN_DIR="${DEVIN2API_BIN_DIR:-${HOME}/.local/bin}"
CONFIG_DIR="${DEVIN2API_CONFIG_DIR:-${XDG_CONFIG_HOME:-${HOME}/.config}/devin-2api}"
STATE_DIR="${DEVIN2API_STATE_DIR:-${DEVIN2API_RUNTIME:-${XDG_STATE_HOME:-${HOME}/.local/state}/devin-2api}}"
LEGACY_RUNTIME="${XDG_DATA_HOME:-${HOME}/.local/share}/devin-2api"

# 服务管理动词：lib-deploy.sh 的 handoff_* 族经它们抹平 launchd/systemd 差异。
svc_pid()     { systemctl --user show -p MainPID --value "${UNIT}" 2>/dev/null; }
svc_restart() { systemctl --user restart "${UNIT}"; }

# unit_content：目标服务定义。Environment 注入 reuseport 是重叠交接的前提；
# TimeoutStopSec 须覆盖二进制 drainTimeout（600s）+退出余量。
unit_content() {
	cat <<EOF
[Unit]
Description=devin-2api — OpenAI/Anthropic-compatible proxy for Devin
After=network-online.target

[Service]
ExecStart=${BIN_DIR}/devin-2api -config ${CONFIG_DIR}/config.yaml -state-dir ${STATE_DIR}
WorkingDirectory=${STATE_DIR}
Environment=DEVIN2API_REUSEPORT=1
Restart=always
RestartSec=5
TimeoutStopSec=660
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=${STATE_DIR}
StandardOutput=append:${STATE_DIR}/logs/stdout.log
StandardError=append:${STATE_DIR}/logs/stderr.log

[Install]
WantedBy=default.target
EOF
}

# 日志轮转走独立的 oneshot service + daily timer——oneshot 无状态，
# enable --now 与重载都不影响主服务在途请求。
LOGROTATE_SERVICE="devin-2api-logrotate.service"
LOGROTATE_TIMER="devin-2api-logrotate.timer"

logrotate_service_content() {
	cat <<EOF
[Unit]
Description=devin-2api log rotation (copytruncate)

[Service]
Type=oneshot
ExecStart=${BIN_DIR}/devin-2api-logrotate ${STATE_DIR}/logs
ReadWritePaths=${STATE_DIR}
EOF
}

logrotate_timer_content() {
	cat <<EOF
[Unit]
Description=daily devin-2api log rotation

[Timer]
OnCalendar=daily
Persistent=true

[Install]
WantedBy=timers.target
EOF
}

# sync_logrotate_timer：单元缺失生成、漂移重写后 daemon-reload；
# timer 幂等 enable --now。
sync_logrotate_timer() {
	local changed=0
	for f in "${LOGROTATE_SERVICE}" "${LOGROTATE_TIMER}"; do
		local content_fn="logrotate_${f##*.}_content"
		if [[ ! -f "${UNIT_DIR}/${f}" ]] || ! "${content_fn}" | cmp -s - "${UNIT_DIR}/${f}"; then
			"${content_fn}" >"${UNIT_DIR}/${f}"
			changed=1
		fi
	done
	if [[ "${changed}" == "1" ]]; then
		systemctl --user daemon-reload
		echo "==> logrotate units updated"
	fi
	systemctl --user enable --now "${LOGROTATE_TIMER}" >/dev/null 2>&1 ||
		systemctl --user enable "${LOGROTATE_TIMER}"
}

do_uninstall() {
	local did=0
	retire_stale_transient
	if systemctl --user cat "${UNIT}" >/dev/null 2>&1; then
		systemctl --user disable --now "${UNIT}"
		rm -f "${UNIT_DIR}/${UNIT}"
		echo "==> unit disabled and removed (${UNIT_DIR}/${UNIT})"
		did=1
	fi
	if systemctl --user cat "${LOGROTATE_TIMER}" >/dev/null 2>&1; then
		systemctl --user disable --now "${LOGROTATE_TIMER}"
		did=1
	fi
	if [[ -f "${UNIT_DIR}/${LOGROTATE_SERVICE}" || -f "${UNIT_DIR}/${LOGROTATE_TIMER}" ]]; then
		rm -f "${UNIT_DIR}/${LOGROTATE_SERVICE}" "${UNIT_DIR}/${LOGROTATE_TIMER}"
		rm -f "${BIN_DIR}/devin-2api-logrotate"
		did=1
	fi
	[[ "${did}" == "1" ]] && systemctl --user daemon-reload
	remove_installed_binary && did=1
	[[ -f "${CONFIG_DIR}/config.yaml" || -d "${STATE_DIR}/logs" ]] &&
		echo "    保留 ${CONFIG_DIR}/config.yaml 与 ${STATE_DIR}/logs/；彻底清理: rm -rf '${CONFIG_DIR}' '${STATE_DIR}'"
	[[ "${did}" == "0" ]] && echo "nothing to remove"
}

parse_deploy_args "$@"

# 单实例约定（同 macOS 版）：排除掉 unit 托管的 MainPID 与已登记交接
# 进程后列其余进程；交接进程有残留则就地回收。
MANAGED_PID="$(svc_pid || true)"
STALE_TPID="$(cat "$(handoff_pidfile)" 2>/dev/null || true)"
warn_strays "${MANAGED_PID:-0}" ${STALE_TPID:+"${STALE_TPID}"}
retire_stale_transient

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
install_rotate_script
# 旧版单运行目录（~/.local/share/devin-2api）迁移：config 入 CONFIG_DIR、
# logs 入 STATE_DIR、删旧二进制；目标已存在不覆盖。
migrate_legacy_runtime "${LEGACY_RUNTIME}"

# unit 与模板对齐：缺失生成、漂移重写后 daemon-reload——restart 使用
# 已载入的新定义（env 变更本次 restart 即生效，与 launchd 需
# bootout+bootstrap 不同）。ProtectSystem=strict 把全盘挂只读，
# ReadWritePaths 只对状态目录放行写——credentials.toml 等 token 来源
# 只读不受影响。
FRESH_BOOT=0
UNIT_RELOAD=0
if [[ ! -f "${UNIT_DIR}/${UNIT}" ]]; then
	echo "==> first install: 生成 ${UNIT_DIR}/${UNIT}"
	mkdir -p "${UNIT_DIR}"
	unit_content >"${UNIT_DIR}/${UNIT}"
elif ! unit_content | cmp -s - "${UNIT_DIR}/${UNIT}"; then
	echo "==> unit 模板有更新，重写 ${UNIT_DIR}/${UNIT}"
	unit_content >"${UNIT_DIR}/${UNIT}"
	UNIT_RELOAD=1
fi
if ! systemctl --user cat "${UNIT}" >/dev/null 2>&1; then
	systemctl --user daemon-reload
	systemctl --user enable --now "${UNIT}"
	FRESH_BOOT=1
elif [[ "${UNIT_RELOAD}" == "1" ]]; then
	systemctl --user daemon-reload
fi
sync_logrotate_timer

if [[ "${NO_RESTART}" == "1" ]]; then
	echo "done (binary swapped, restart skipped)"
	exit 0
fi

# 刚 enable --now 的服务已在跑新二进制，restart 只会平白弹它一次。
# unit 变更时本次 restart 即载入新定义——在跑实例多半还没拿到 reuseport
# env，走经典重启，下次部署起走重叠交接。
OLD_PID=""
if [[ "${FRESH_BOOT}" == "1" ]]; then
	echo "==> service enabled and started"
elif [[ "${UNIT_RELOAD}" == "1" ]]; then
	OLD_PID="$(svc_pid || true)"
	wait_inflight_idle "${HEALTH_URL}" 30
	svc_restart
else
	OLD_PID="$(svc_pid || true)"
	handoff_restart "${OLD_PID:-0}"
fi

echo "==> waiting for healthz version=${VERSION} (old pid: ${OLD_PID:-?})"
# 交接路径几秒内即达；回退路径最坏要等 600s 排空 + Restart 重拉。
RUNNING="$(wait_healthz_version "${HEALTH_URL}" "${VERSION}" 660)" || {
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
# 第二遍收编：排空窗口期老实例往旧路径补写的请求目录/jsonl 尾账，
# 此刻老进程已死，合并后旧目录应能整体 rmdir。
migrate_legacy_runtime "${LEGACY_RUNTIME}"
print_summary "${RUNNING}" "systemctl --user restart ${UNIT}（重启）；--uninstall 卸载"
exit "${smoke_rc}"
