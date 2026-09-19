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
# MANAGED 是 reuseport 准入的出处声明（systemd 的 INVOCATION_ID 已可作证，
# 这里显式声明让服务定义自描述、与其它托管器口径一致）。TimeoutStopSec
# 须覆盖二进制 drainTimeout（600s）+退出余量。
unit_content() {
	cat <<EOF
[Unit]
Description=devin-2api — OpenAI/Anthropic-compatible proxy for Devin
After=network-online.target

[Service]
ExecStart=${BIN_DIR}/devin-2api -config ${CONFIG_DIR}/config.yaml -state-dir ${STATE_DIR}
WorkingDirectory=${STATE_DIR}
Environment=DEVIN2API_REUSEPORT=1
Environment=DEVIN2API_MANAGED=1
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
# unit 变更时 restart 顺带载入新定义（daemon-reload 已在前面做过）——
# 不用退回经典重启：在跑实例已开 reuseport 时交接桥能盖住整个排空窗口，
# 没开时 spawn 失败在 handoff_restart 内部自动退化，语义与原来相同。
OLD_PID=""
NRESTARTS_BEFORE="$(systemctl --user show -p NRestarts --value "${UNIT}" 2>/dev/null || echo 0)"
if [[ "${FRESH_BOOT}" == "1" ]]; then
	echo "==> service enabled and started"
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
# version 匹配不等于托管实例在服役：交接/野实例与托管实例同二进制，
# 能答出新版本制造假绿——应答 healthz 的必须是 MainPID 本体。
if [[ -z "${NEW_PID}" || "${NEW_PID}" == "0" ]] || ! wait_healthz_pid "${HEALTH_URL}" "${NEW_PID}" 60; then
	echo "healthz 应答者不是托管实例 (MainPID=${NEW_PID:-?})——交接或野实例假绿，部署按失败处理" >&2
	dump_recent_log
	exit 1
fi
NRESTARTS_AFTER="$(systemctl --user show -p NRestarts --value "${UNIT}" 2>/dev/null || echo 0)"
# NRestarts 只计 systemd 自动重启（实测 manual restart 不增）——部署窗口内
# 自增即崩溃循环留痕。
if [[ "${NRESTARTS_AFTER:-0}" -gt "${NRESTARTS_BEFORE:-0}" ]]; then
	warn "部署期间托管实例被 systemd 自动重启 $((NRESTARTS_AFTER - NRESTARTS_BEFORE)) 次——疑似崩溃循环，请查 ${STATE_DIR}/logs/stderr.log"
fi
echo "==> running: pid=${NEW_PID:-?} version=${RUNNING}"

# -wal 高水位化石回收：RESTART/PASSIVE checkpoint 只回写不截文件，reuseport
# 交接又让 db 永远没有「最后连接关闭」时机——文件涨到多大就冻在多大。
# TRUNCATE 是唯一收缩路径：帧全部回写 db 后才 ftruncate 到零，回写在前
# 所以崩溃安全。新实例已接管、旧进程在排空/已退出——此时做 best-effort
# 回收：busy_timeout 等排空期残留的 reader/writer 让位，等不到或 sqlite3
# 失败只告警跳过（化石留给下次部署），绝不因回收失败拖垮部署。
DB_FILE="${STATE_DIR}/devin-2api.db"
WAL_FILE="${DB_FILE}-wal"
if [[ -f "${DB_FILE}" && -f "${WAL_FILE}" ]] && command -v sqlite3 >/dev/null; then
	WAL_BYTES="$(stat -c %s "${WAL_FILE}" 2>/dev/null || echo 0)"
	# 阈值与 Maintain 的 64MB 一致：之下的 wal 由运行期 RESTART 维持即可。
	if [[ "${WAL_BYTES}" -gt 67108864 ]]; then
		CKPT_OUT="$(timeout 30 sqlite3 "${DB_FILE}" \
			"PRAGMA busy_timeout=10000; PRAGMA wal_checkpoint(TRUNCATE);" 2>/dev/null)" || CKPT_OUT=""
		CKPT_ROW="$(printf '%s\n' "${CKPT_OUT}" | tail -n 1)"
		if [[ "${CKPT_ROW%%|*}" == "0" ]]; then
			echo "==> wal_checkpoint(TRUNCATE): ${CKPT_ROW##*|} 页回写，-wal $((WAL_BYTES / 1048576))MB → $(($(stat -c %s "${WAL_FILE}" 2>/dev/null || echo 0) / 1048576))MB"
		else
			warn "wal_checkpoint(TRUNCATE) 未完成（${CKPT_ROW:-sqlite3 失败或超时}）——-wal 保留 $((WAL_BYTES / 1048576))MB，不影响部署结果"
		fi
	fi
fi

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
