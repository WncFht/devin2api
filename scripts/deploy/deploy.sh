#!/usr/bin/env bash
# deploy.sh — 构建或下载并热替换本机 launchd 托管的 devin-2api（仅 macOS）。
# 用法见 --help。launchd 服务未加载时自动生成 plist 并 bootstrap，
# config.yaml 缺失时自动从 example 生成——首装与升级同一条命令：
# 新机器只要同步本仓库再跑 deploy.sh。Linux 用 scripts/deploy/deploy-linux.sh
# （systemd --user），两者共享 scripts/deploy/lib-deploy.sh。
set -euo pipefail
cd "$(dirname "$0")/../.."
source scripts/deploy/lib-deploy.sh

[[ "$(uname -s)" == "Darwin" ]] || {
	echo "deploy.sh 仅适用 macOS（launchd）；Linux 用 scripts/deploy/deploy-linux.sh" >&2
	exit 1
}

# launchd label 跟随当前用户名（本机约定 com.<user>.devin-2api）；
# 需要固定名时可用 DEVIN2API_LABEL 覆盖。
LABEL="${DEVIN2API_LABEL:-com.${USER}.devin-2api}"
PLIST="${HOME}/Library/LaunchAgents/${LABEL}.plist"
# 平台规范布局：二进制入 ~/.local/bin（用户级 bin 惯例，在 PATH 上可直接
# 调用）；配置与状态同放 Application Support——launchd 子进程对 ~/Desktop
# 的每次 open 都会被 TCC 桌面文件夹授权挂起（仓库在 Desktop 下时
# exec/config/logs 全部卡死），Application Support 不受 TCC 保护；macOS
# 无独立 state dir 惯例，维持 app 目录模型。仓库只保留 logs ->
# STATE_DIR/logs 的符号链接供排障读取。
BIN_DIR="${DEVIN2API_BIN_DIR:-${HOME}/.local/bin}"
CONFIG_DIR="${DEVIN2API_CONFIG_DIR:-${HOME}/Library/Application Support/devin-2api}"
STATE_DIR="${DEVIN2API_STATE_DIR:-${DEVIN2API_RUNTIME:-${HOME}/Library/Application Support/devin-2api}}"
LEGACY_RUNTIME="${HOME}/Library/Application Support/devin-2api"

# 服务管理动词：lib-deploy.sh 的 handoff_* 族经它们抹平 launchd/systemd 差异。
svc_pid()     { launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^[ \t]*pid = /{print $3}'; }
svc_restart() { launchctl kickstart -k "gui/$(id -u)/${LABEL}"; }

# svc_reload_restart：plist 变更时「让新实例跑起来」的动作——kickstart 不载入
# 新 plist，必须 bootout+bootstrap。bootout 返回不等进程退完：有交接桥时旧实例
# 的监听已让出（reuseport 组共享），无桥回退时旧 socket 还占着端口，先等旧 pid
# 消失再 bootstrap，省得 KeepAlive 在 EADDRINUSE 上空转。
svc_reload_restart() {
	launchctl bootout "gui/$(id -u)/${LABEL}" 2>/dev/null || true
	local _ old_pid="${OLD_PID:-}"
	for _ in $(seq 140); do
		[[ -z "${old_pid}" ]] && break
		kill -0 "${old_pid}" 2>/dev/null || break
		sleep 0.5
	done
	launchctl bootstrap "gui/$(id -u)" "${PLIST}"
}

# plist_content：目标服务定义。EnvironmentVariables 注入 reuseport 是
# 重叠交接的前提；MANAGED 是 reuseport 准入的出处声明（launchd 无
# systemd INVOCATION_ID，必须由服务定义显式注入）。ExitTimeOut 须覆盖
# 二进制 drainTimeout（600s）+退出余量。
plist_content() {
	cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>${LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>${BIN_DIR}/devin-2api</string>
		<string>-config</string>
		<string>${CONFIG_DIR}/config.yaml</string>
		<string>-state-dir</string>
		<string>${STATE_DIR}</string>
	</array>
	<key>WorkingDirectory</key><string>${STATE_DIR}</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>DEVIN2API_REUSEPORT</key><string>1</string>
		<key>DEVIN2API_MANAGED</key><string>1</string>
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>ExitTimeOut</key><integer>660</integer>
	<key>StandardOutPath</key><string>${STATE_DIR}/logs/stdout.log</string>
	<key>StandardErrorPath</key><string>${STATE_DIR}/logs/stderr.log</string>
</dict>
</plist>
EOF
}

# logrotate 用独立 agent：StartInterval 每天跑一次 copytruncate 轮转，
# 与主服务解耦——它的 bootout/bootstrap 不影响服务在途请求。
LOGROTATE_LABEL="${LABEL}.logrotate"
LOGROTATE_PLIST="${HOME}/Library/LaunchAgents/${LOGROTATE_LABEL}.plist"

logrotate_plist_content() {
	cat <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>${LOGROTATE_LABEL}</string>
	<key>ProgramArguments</key>
	<array>
		<string>${BIN_DIR}/devin-2api-logrotate</string>
		<string>${STATE_DIR}/logs</string>
	</array>
	<key>StartInterval</key><integer>86400</integer>
	<key>StandardOutPath</key><string>${STATE_DIR}/logs/logrotate.out</string>
	<key>StandardErrorPath</key><string>${STATE_DIR}/logs/logrotate.err</string>
</dict>
</plist>
EOF
}

# sync_logrotate_agent：agent 缺失生成、漂移重写后 bootout+bootstrap
# 生效（StartInterval 任务无状态，重载成本为零）。
sync_logrotate_agent() {
	local changed=0
	if [[ ! -f "${LOGROTATE_PLIST}" ]]; then
		logrotate_plist_content >"${LOGROTATE_PLIST}"
		changed=1
	elif ! logrotate_plist_content | cmp -s - "${LOGROTATE_PLIST}"; then
		logrotate_plist_content >"${LOGROTATE_PLIST}"
		changed=1
	fi
	if launchctl print "gui/$(id -u)/${LOGROTATE_LABEL}" >/dev/null 2>&1; then
		[[ "${changed}" == "1" ]] && launchctl bootout "gui/$(id -u)/${LOGROTATE_LABEL}" 2>/dev/null || true
	else
		changed=1
	fi
	if [[ "${changed}" == "1" ]]; then
		launchctl bootstrap "gui/$(id -u)" "${LOGROTATE_PLIST}"
		echo "==> logrotate agent loaded (${LOGROTATE_LABEL}, daily)"
	fi
}

do_uninstall() {
	local did=0
	retire_stale_transient
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
	if launchctl print "gui/$(id -u)/${LOGROTATE_LABEL}" >/dev/null 2>&1; then
		launchctl bootout "gui/$(id -u)/${LOGROTATE_LABEL}"
		did=1
	fi
	if [[ -f "${LOGROTATE_PLIST}" ]]; then
		rm -f "${LOGROTATE_PLIST}"
		rm -f "${BIN_DIR}/devin-2api-logrotate"
		did=1
	fi
	remove_installed_binary && did=1
	[[ -f "${CONFIG_DIR}/config.yaml" || -d "${STATE_DIR}/logs" ]] &&
		echo "    保留 ${CONFIG_DIR}/config.yaml 与 ${STATE_DIR}/logs/；彻底清理: rm -rf '${CONFIG_DIR}' '${STATE_DIR}'"
	[[ "${did}" == "0" ]] && echo "nothing to remove"
}

parse_deploy_args "$@"

# 单实例约定：launchd 托管的实例是唯一合法实例。部署前先列出其它
# devin-2api 进程（手动 ./devin-2api、遗忘的冒烟实例）——它们会抢端口、
# 分流请求，且不受 SIGTERM 优雅退出保护。交接进程有 pidfile 登记，属
# 豁免项；有残留则就地回收。
LAUNCHD_PID="$(svc_pid)"
STALE_TPID="$(cat "$(handoff_pidfile)" 2>/dev/null || true)"
warn_strays "${LAUNCHD_PID:-0}" ${STALE_TPID:+"${STALE_TPID}"}
retire_stale_transient

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
install_rotate_script
# 旧版单运行目录布局迁移：macOS 下只剩清理 LEGACY_RUNTIME 里的旧二进制
# 与 .handoff.pid（config/logs 本就与 CONFIG_DIR/STATE_DIR 同路径）。
migrate_legacy_runtime "${LEGACY_RUNTIME}"

# plist 与模板对齐：缺失生成、漂移重写。job 定义只在 bootstrap 时载入，
# kickstart 不重读文件——已加载服务的 plist 变更只能 bootout+bootstrap
# 生效，该路径本身即一次经典重启（首装与升级同一条命令）。
FRESH_BOOT=0
PLIST_RELOAD=0
if [[ ! -f "${PLIST}" ]]; then
	echo "==> first install: 生成 ${PLIST}"
	mkdir -p "$(dirname "${PLIST}")"
	plist_content >"${PLIST}"
elif ! plist_content | cmp -s - "${PLIST}"; then
	echo "==> plist 模板有更新，重写 ${PLIST}"
	plist_content >"${PLIST}"
	PLIST_RELOAD=1
fi
if ! launchctl print "gui/$(id -u)/${LABEL}" >/dev/null 2>&1; then
	launchctl bootstrap "gui/$(id -u)" "${PLIST}"
	FRESH_BOOT=1
fi
sync_logrotate_agent

if [[ "${NO_RESTART}" == "1" ]]; then
	echo "done (binary swapped, restart skipped)"
	exit 0
fi

# 刚 bootstrap 的服务已在跑新二进制，kickstart 只会平白弹它一次。
# plist 变更必须 bootout+bootstrap（svc_reload_restart）——同样走交接桥：
# 在跑实例已开 reuseport 时桥先接管新连接，盖住 bootout→排尽→bootstrap
# 整段空窗；没开时 spawn 失败，handoff_restart 内部退化为等空闲+同一动作。
OLD_PID=""
if [[ "${FRESH_BOOT}" == "1" ]]; then
	echo "==> service bootstrapped (RunAtLoad 已启动新进程)"
elif [[ "${PLIST_RELOAD}" == "1" ]]; then
	OLD_PID="$(svc_pid)"
	handoff_restart "${OLD_PID}" svc_reload_restart
else
	OLD_PID="$(svc_pid)"
	handoff_restart "${OLD_PID:-0}"
fi

echo "==> waiting for healthz version=${VERSION} (old pid: ${OLD_PID:-?})"
# 交接路径几秒内即达；回退路径最坏要等 600s 排空 + KeepAlive 重拉。
RUNNING="$(wait_healthz_version "${HEALTH_URL}" "${VERSION}" 660)" || {
	echo "healthz 未出现新版本 (last=${RUNNING})" >&2
	dump_recent_log
	exit 1
}

NEW_PID="$(launchctl print "gui/$(id -u)/${LABEL}" 2>/dev/null | awk '/^[ \t]*pid = /{print $3}' || true)"
# version 匹配不等于托管实例在服役：交接/野实例与托管实例同二进制，
# 能答出新版本制造假绿——应答 healthz 的必须是 launchd 托管 pid 本体。
if [[ -z "${NEW_PID}" ]] || ! wait_healthz_pid "${HEALTH_URL}" "${NEW_PID}" 60; then
	echo "healthz 应答者不是 launchd 托管实例 (pid=${NEW_PID:-?})——交接或野实例假绿，部署按失败处理" >&2
	dump_recent_log
	exit 1
fi
echo "==> running: pid=${NEW_PID:-?} version=${RUNNING}"

# healthz 只证明进程活着；真链路冒烟打 /v1/models 验证上游鉴权。
smoke_rc=0
smoke_upstream || smoke_rc=$?
# 第二遍收编排空窗口期老实例往旧路径补写的尾账（macOS 下旧目录与
# STATE_DIR 同路径，此调用近似空转）。
migrate_legacy_runtime "${LEGACY_RUNTIME}"
print_summary "${RUNNING}" "launchctl kickstart -k gui/$(id -u)/${LABEL}（重启）；--uninstall 卸载"
exit "${smoke_rc}"
