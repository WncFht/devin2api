#!/usr/bin/env bash
# handoff-triage.test.sh — lib-deploy.sh 交接链路的离线演练：
# spawn_handoff 的死因分诊（EADDRINUSE→rc2 回退 / 启动期死→rc1 中止部署）
# 与「先证接管再放桥」顺序，用 stub 二进制 + stub curl + stub uname +
# stub svc_pid 回放。9-18 断流事故（config 校验死被误诊为缺 reuseport
# 并回退重启）的防回归断言。不跑真实 deploy、不碰端口、不触 systemd：
# healthz 由 stub curl 按 answer_pid 文件应答。
set -euo pipefail
cd "$(dirname "$0")/.."

FAILED=0
pass() { echo "ok    $1"; }
fail() { echo "FAIL  $1"; FAILED=1; }
# check <描述> <命令...>：命令失败记一条失败但不中断，最后统一退出码。
check() {
	local desc="$1"
	shift
	if "$@" >"${WORK}/check.out" 2>&1; then
		pass "${desc}"
	else
		fail "${desc}"
		sed 's/^/    /' "${WORK}/check.out"
	fi
}

WORK="$(mktemp -d -t devin2api-handoff-test.XXXXXX)"
: >"${WORK}/pids"
kill_pids() {
	while IFS= read -r p; do kill "$p" 2>/dev/null || true; done <"${WORK}/pids"
	: >"${WORK}/pids"
}
cleanup() {
	kill_pids
	rm -rf "${WORK}"
}
trap cleanup EXIT

# ---------- stub 环境 ----------
export BIN_DIR="${WORK}/bin" CONFIG_DIR="${WORK}/cfg" STATE_DIR="${WORK}/state"
mkdir -p "${BIN_DIR}" "${CONFIG_DIR}" "${STATE_DIR}/logs"
: >"${STATE_DIR}/logs/stderr.log"
export PORT=39999
export HEALTH_URL="http://localhost:${PORT}/healthz"
export ANSWER_PID_FILE="${WORK}/answer_pid"

# stub 二进制：FAKE_MODE 决定启动命运；listening 时把自身 pid 写进
# ANSWER_PID_FILE 充当「本 socket 正在应答」。
cat >"${BIN_DIR}/devin-2api" <<'EOF'
#!/usr/bin/env bash
case "${FAKE_MODE:-listen}" in
listen)
	echo 'time=x level=INFO msg="paths resolved"' >&2
	echo 'time=x level=INFO msg="HTTP server listening" addr=":39999" version=test reuseport=true' >&2
	echo "${BASHPID}" >"${ANSWER_PID_FILE}"
	exec sleep 300 ;;
bindfail)
	echo 'time=x level=ERROR msg="port already in use" addr=":39999" holder=old' >&2
	exit 1 ;;
configfail)
	echo 'time=x level=ERROR msg="load config failed" error="devin.accounts[1]: credentials_file \"/x/y.toml\": no such file or directory"' >&2
	exit 1 ;;
silent)
	exit 1 ;;
slow)
	exec sleep 60 ;; # 永不 listening：触发 spawn 的 10s 超时分支
esac
EOF
chmod +x "${BIN_DIR}/devin-2api"

STUB="${WORK}/stubbin"
mkdir -p "${STUB}"
cat >"${STUB}/curl" <<'EOF'
#!/usr/bin/env bash
# healthz 应答由 ANSWER_PID_FILE 指定；文件空/缺 = 无人应答。
for arg in "$@"; do
	case "${arg}" in
	*healthz*)
		if [[ -s "${ANSWER_PID_FILE}" ]]; then
			printf '{"pid":%s,"version":"test"}\n' "$(cat "${ANSWER_PID_FILE}")"
			exit 0
		fi
		exit 7 ;;
	esac
done
exit 7
EOF
cat >"${STUB}/uname" <<'EOF'
#!/usr/bin/env bash
# FAKE_UNAME=Darwin 时冒充 macOS，驱动 handoff_restart 的 Darwin 分支。
if [[ "${FAKE_UNAME:-}" == "Darwin" ]]; then echo Darwin; else /usr/bin/uname "$@"; fi
EOF
chmod +x "${STUB}/curl" "${STUB}/uname"
export PATH="${STUB}:${PATH}"

# 托管侧桩：svc_pid 读文件（systemctl MainPID 的替身）；fake_managed_start
# 起一个真进程充当托管实例——写 svc_pid 后经 MANAGED_DELAY 模拟「绑定」
# （落 listening 行 + 接管 healthz 应答），MANAGED_LISTEN=0 则永不自证。
svc_pid() { cat "${WORK}/svc_pid" 2>/dev/null || true; }
svc_restart() { :; }
fake_managed_start() {
	(
		echo "${BASHPID}" >"${WORK}/svc_pid"
		echo "${BASHPID}" >>"${WORK}/pids"
		sleep "${MANAGED_DELAY:-0.4}"
		if [[ "${MANAGED_LISTEN:-1}" == "1" ]]; then
			echo 'time=x level=INFO msg="HTTP server listening" addr=":39999" version=test reuseport=true' >>"${STATE_DIR}/logs/stderr.log"
			cat "${WORK}/svc_pid" >"${ANSWER_PID_FILE}"
		fi
		exec sleep 300
	) &
}
fake_restart() { touch "${WORK}/restart_called"; fake_managed_start; }

source scripts/lib-deploy.sh

# 超时压缩：被测分支逻辑不变，spawn_handoff 就绪窗口压回 10s（生产值
# 180s 只为共享库大库慢启动，harness 的 slow 桩 60s 就死，不用等满）。
export DEVIN2API_HANDOFF_WAIT_ITERS=40
# 同义压缩：把 wait_healthz_pid / wait_managed_listen
# 的轮询上限砍到 8s（生产值 120s 只为防呆，harness 不想等）。
wait_healthz_pid() {
	local url="$1" want="$2" secs="$3" got _
	secs=$((secs > 8 ? 8 : secs))
	for _ in $(seq $((secs * 2))); do
		got="$(healthz_pid "${url}")"
		[[ -n "${got}" && "${got}" == "${want}" ]] && return 0
		sleep 0.5
	done
	return 1
}
wait_managed_listen() {
	local base="$1" mpid="$2" secs="$3" logf _
	secs=$((secs > 8 ? 8 : secs))
	logf="${STATE_DIR}/logs/stderr.log"
	for _ in $(seq $((secs * 2))); do
		if tail -n "+$((base + 1))" "${logf}" 2>/dev/null | grep -q 'msg="HTTP server listening"' &&
			kill -0 "${mpid}" 2>/dev/null; then
			return 0
		fi
		sleep 0.5
	done
	return 1
}

# reset_fixtures：回收上幕残留进程与标记——pid 登记走 WORK/pids 文件，
# 子 shell 里 += 数组传不回父进程。
reset_fixtures() {
	kill_pids
	rm -f "${WORK}/restart_called" "${WORK}/svc_pid" "${STATE_DIR}/.handoff.pid"
	: >"${ANSWER_PID_FILE}"
}

echo "== spawn_handoff 死因分诊 =="

rc=0
FAKE_MODE=bindfail spawn_handoff >/dev/null 2>&1 || rc=$?
check "bindfail → rc=2（EADDRINUSE 可回退）" test "${rc}" = "2"
check "pidfile 已清理" test ! -f "${STATE_DIR}/.handoff.pid"

rc=0
FAKE_MODE=configfail spawn_handoff >/dev/null 2>"${WORK}/err" || rc=$?
check "configfail → rc=1（启动期死）" test "${rc}" = "1"
check "stderr 摘录含 load config failed" grep -q 'load config failed' "${WORK}/err"

rc=0
FAKE_MODE=silent spawn_handoff >/dev/null 2>&1 || rc=$?
check "silent 早夭 → rc=1" test "${rc}" = "1"

rc=0
FAKE_MODE=slow spawn_handoff >/dev/null 2>&1 || rc=$?
check "超时未就绪 → rc=1" test "${rc}" = "1"

reset_fixtures
tpid="$(FAKE_MODE=listen spawn_handoff 2>/dev/null)" && rc=0 || rc=$?
check "listen → rc=0 且 stdout 只回 pid" test "${rc}" = "0" -a -n "${tpid}"
check "pidfile 已登记" grep -qx "${tpid}" "${STATE_DIR}/.handoff.pid"
kill "${tpid}" 2>/dev/null || true

echo "== handoff_restart 分诊 =="

# bind 冲突 → 回退经典重启（restart_fn 被调用，与旧行为一致）
reset_fixtures
rc=0
( export FAKE_MODE=bindfail; handoff_restart 999001 fake_restart ) >"${WORK}/out" 2>&1 || rc=$?
check "bindfail → rc=0 回退经典重启" test "${rc}" = "0"
check "回退调用了 restart_fn" test -f "${WORK}/restart_called"
check "回退消息归因缺 reuseport" grep -q '未开 reuseport' "${WORK}/out"

# 启动期死 → 中止部署：9-18 事故形态，restart_fn 不得被调用（旧实例不被触碰）
reset_fixtures
rc=0
( export FAKE_MODE=configfail; handoff_restart 999001 fake_restart ) >"${WORK}/out" 2>&1 || rc=$?
check "configfail → 中止部署 rc!=0" test "${rc}" != "0"
check "未调 restart_fn（旧实例未被触碰）" test ! -f "${WORK}/restart_called"
check "摘录透出 credentials_file" grep -q 'credentials_file' "${WORK}/out"

reset_fixtures
rc=0
( export FAKE_MODE=silent; handoff_restart 999001 fake_restart ) >/dev/null 2>&1 || rc=$?
check "silent → 中止部署 rc!=0" test "${rc}" != "0"
check "未调 restart_fn" test ! -f "${WORK}/restart_called"

echo "== 先证接管再放桥（Linux 分支）=="

reset_fixtures
echo "999001" >"${WORK}/svc_pid"
rc=0
( export FAKE_MODE=listen; handoff_restart 999001 fake_restart ) >"${WORK}/out" 2>&1 || rc=$?
check "happy path rc=0" test "${rc}" = "0"
check "restart_fn 已调" test -f "${WORK}/restart_called"
check "pidfile 已清（交接确认死亡后退役）" test ! -f "${STATE_DIR}/.handoff.pid"

# 托管实例永不自证（进程在但不 listening 不应答）→ 桥保留降级服役
reset_fixtures
echo "999001" >"${WORK}/svc_pid"
rc=0
( export FAKE_MODE=listen MANAGED_LISTEN=0; handoff_restart 999001 fake_restart ) >"${WORK}/out" 2>&1 || rc=$?
check "managed 不自证 → rc=0 桥保留" test "${rc}" = "0"
check "pidfile 保留" test -f "${STATE_DIR}/.handoff.pid"
tpid="$(cat "${STATE_DIR}/.handoff.pid" 2>/dev/null || true)"
check "交接进程仍存活（桥未被杀）" kill -0 "${tpid}"
kill "${tpid}" 2>/dev/null || true

echo "== 先证接管再放桥（Darwin 分支，stub uname）=="

reset_fixtures
echo "999001" >"${WORK}/svc_pid"
rc=0
( export FAKE_MODE=listen FAKE_UNAME=Darwin; handoff_restart 999001 fake_restart ) >"${WORK}/out" 2>&1 || rc=$?
check "Darwin happy path rc=0" test "${rc}" = "0"
check "Darwin pidfile 已清" test ! -f "${STATE_DIR}/.handoff.pid"

# Darwin：managed 不落 listening 行 → wait_managed_listen 超时 → 桥保留
reset_fixtures
echo "999001" >"${WORK}/svc_pid"
rc=0
( export FAKE_MODE=listen FAKE_UNAME=Darwin MANAGED_LISTEN=0; handoff_restart 999001 fake_restart ) >"${WORK}/out" 2>&1 || rc=$?
check "Darwin managed 不自证 → rc=0 桥保留" test "${rc}" = "0"
check "Darwin pidfile 保留" test -f "${STATE_DIR}/.handoff.pid"
tpid="$(cat "${STATE_DIR}/.handoff.pid" 2>/dev/null || true)"
check "Darwin 交接进程仍存活" kill -0 "${tpid}"
kill "${tpid}" 2>/dev/null || true

echo "== preflight credentials_file 存在性 =="

PFL="${WORK}/pfl"
mkdir -p "${PFL}/repo" "${PFL}/cfg" "${PFL}/home"
write_cfg() {
	cat >"$1" <<EOF
server:
  listen: ':13099'
devin:
  accounts:
    - name: a
      credentials_file: '$2'
    - name: b
      token: 'tok'
EOF
}
run_preflight() {
	( cd "${PFL}/repo" && HOME="${PFL}/home" CONFIG_DIR="${PFL}/cfg" RELEASE_TAG=v0.0.0 preflight_deploy )
}

# 仓库 config（无 live）声明死引用 → 中止
write_cfg "${PFL}/repo/config.yaml" "${PFL}/missing.toml"
rm -f "${PFL}/cfg/config.yaml"
rc=0
run_preflight >"${WORK}/out" 2>&1 || rc=$?
check "repo 死引用 → 中止" test "${rc}" != "0"
check "报缺失路径" grep -q 'missing.toml' "${WORK}/out"

# live 权威：repo 好 + live 坏 → 中止（新实例只读 live，9-18 事故形态）
write_cfg "${PFL}/repo/config.yaml" "${PFL}/exists.toml"
touch "${PFL}/exists.toml"
write_cfg "${PFL}/cfg/config.yaml" "${PFL}/missing2.toml"
rc=0
run_preflight >"${WORK}/out" 2>&1 || rc=$?
check "live 坏 + repo 好 → 中止（live 权威）" test "${rc}" != "0"
check "报 live 缺失路径" grep -q 'missing2.toml' "${WORK}/out"

# live 好 + repo 坏 → 通过
write_cfg "${PFL}/cfg/config.yaml" "${PFL}/exists.toml"
write_cfg "${PFL}/repo/config.yaml" "${PFL}/missing3.toml"
rc=0
run_preflight >/dev/null 2>&1 || rc=$?
check "live 好 + repo 坏 → 通过" test "${rc}" = "0"

# ~/ 展开（与 Go expandHomeDir 同义）
rm -f "${PFL}/cfg/config.yaml"
write_cfg "${PFL}/repo/config.yaml" '~/creds.toml'
rm -f "${PFL}/home/creds.toml"
rc=0
run_preflight >/dev/null 2>&1 || rc=$?
check "~ 展开缺失 → 中止" test "${rc}" != "0"
touch "${PFL}/home/creds.toml"
rc=0
run_preflight >/dev/null 2>&1 || rc=$?
check "~ 展开存在 → 通过" test "${rc}" = "0"

# 相对路径锚定 config 所在目录（与 Go filepath.Join(configDir,...) 同义）
write_cfg "${PFL}/repo/config.yaml" 'rel-creds.toml'
rc=0
run_preflight >/dev/null 2>&1 || rc=$?
check "相对路径缺失 → 中止" test "${rc}" != "0"
touch "${PFL}/repo/rel-creds.toml"
rc=0
run_preflight >/dev/null 2>&1 || rc=$?
check "相对路径存在 → 通过" test "${rc}" = "0"

echo
if [[ "${FAILED}" == "1" ]]; then
	echo "handoff-triage 存在失败项" >&2
	exit 1
fi
echo "全部通过"
