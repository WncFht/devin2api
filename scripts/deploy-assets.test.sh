#!/usr/bin/env bash
# deploy-assets.test.sh — 部署资产的防回归断言：所有 shell 脚本过 bash -n，
# deploy.sh 的 plist、deploy-linux.sh 的 systemd unit、lib-deploy.sh 的关键
# 行为必须有指定字段。改坏部署路径时 CI 在这里先炸，而不是用户机器上。
set -euo pipefail
cd "$(dirname "$0")/.."

FAILED=0

# check <描述> <命令...>：命令失败记一条失败但不中断，最后统一退出码。
check() {
	local desc="$1"
	shift
	if "$@"; then
		echo "ok    ${desc}"
	else
		echo "FAIL  ${desc}"
		FAILED=1
	fi
}

# grep 断言包装：文件必须含指定固定字符串。
has() {
	grep -qF -- "$2" "$1"
}

echo "== syntax =="
for script in scripts/*.sh scripts/quota/poll.sh; do
	check "bash -n ${script}" bash -n "${script}"
done
check "python 语法 scripts/quota/fit.py" python3 -c "compile(open('scripts/quota/fit.py').read(), 'fit.py', 'exec')"

echo "== lib-deploy.sh（共享逻辑）=="
check "下载校验 checksums.txt" has scripts/lib-deploy.sh "checksums.txt"
check "健康检查轮询版本" has scripts/lib-deploy.sh "wait_healthz_version"
# stray 检查必须按可执行名匹配：pgrep -f 会把 cmdline 含 devin-2api 的
# shell 误报为 stray（曾实测把 deploy 脚本自己报成 stray）。
check "stray 匹配用 pgrep -x" has scripts/lib-deploy.sh "pgrep -x"
if grep -vE '^\s*#' scripts/lib-deploy.sh | grep -qF "pgrep -f"; then
	echo "FAIL  lib-deploy.sh 仍在用 pgrep -f（误报 stray）"
	FAILED=1
else
	echo "ok    无 pgrep -f"
fi
# 进度输出必须走 stderr：$() 捕获会把 stdout 噪音混进 VERSION 变量。
check "下载进度写 stderr" bash -c "grep -c '>&2' scripts/lib-deploy.sh | grep -qv '^0$'"
# spawn 失败必须按死因分诊：EADDRINUSE 才回退，其余一律中止——
# 9-18 断流事故的根因是 config 校验死被误诊为缺 reuseport。
check "spawn 失败按返回码分诊" has scripts/lib-deploy.sh 'spawn_rc'
check "非端口冲突一律中止部署" has scripts/lib-deploy.sh '中止部署'
check "杀桥前先证接管" has scripts/lib-deploy.sh '先证接管再放桥'
check "credentials_file 存在性预检" has scripts/lib-deploy.sh 'config_credentials_files'
# 交接进程靠 DEVIN2API_HANDOFF 过 reuseport 出处准入——标记丢了 spawn 必被拒。
check "交接进程携带 HANDOFF 出处标记" has scripts/lib-deploy.sh 'DEVIN2API_HANDOFF=1'

echo "== deploy.sh（macOS launchd）=="
check "限定 Darwin" has scripts/deploy.sh 'Darwin'
check "plist 保活" has scripts/deploy.sh 'KeepAlive'
check "优雅退出窗口" has scripts/deploy.sh 'ExitTimeOut'
check "kickstart -k 发 SIGTERM" has scripts/deploy.sh 'kickstart -k'
check "部署后断言 healthz pid==托管 pid" has scripts/deploy.sh 'wait_healthz_pid "${HEALTH_URL}" "${NEW_PID}"'
check "禁 kill -9 约定" bash -c "! grep -qF 'kill -9' scripts/deploy.sh"
# launchd 没有 systemd 的 INVOCATION_ID：plist 必须显式注入 MANAGED，
# 否则托管实例被自己设的 reuseport 准入拒掉。
check "plist 注入 MANAGED 出处标记" has scripts/deploy.sh 'DEVIN2API_MANAGED'

echo "== deploy-linux.sh（systemd --user）=="
check "限定 Linux" has scripts/deploy-linux.sh 'Linux'
check "Restart=always" has scripts/deploy-linux.sh 'Restart=always'
check "优雅停止窗口" has scripts/deploy-linux.sh 'TimeoutStopSec='
check "systemctl --user" has scripts/deploy-linux.sh 'systemctl --user'
check "日志落状态目录" has scripts/deploy-linux.sh 'StandardOutput=append:'
# version 匹配不等于托管实例在服役——healthz 应答者必须是 MainPID 本体。
check "部署后断言 healthz pid==MainPID" has scripts/deploy-linux.sh 'wait_healthz_pid "${HEALTH_URL}" "${NEW_PID}"'
check "崩溃循环 NRestarts 告警" has scripts/deploy-linux.sh 'NRestarts'
# 与 plist 口径一致：unit 显式声明 MANAGED（INVOCATION_ID 本就够格，
# 标记让服务定义自描述、防止未来把 env 行挪去非 systemd 托管器时漏证）。
check "unit 注入 MANAGED 出处标记" has scripts/deploy-linux.sh 'Environment=DEVIN2API_MANAGED=1'

echo "== deploy-remote.sh（开发机 → 生产机驱动）=="
check "目标机走 DEVIN2API_HOST" has scripts/deploy-remote.sh 'DEVIN2API_HOST'
check "非交互 SSH（BatchMode）" has scripts/deploy-remote.sh 'BatchMode=yes'
check "worktree 文件集用 git ls-files 定界" has scripts/deploy-remote.sh 'ls-files'
check "生产 config.yaml 取自远端 live 实例" has scripts/deploy-remote.sh 'CONFIG_LIVE}'

echo "== deploy-windows.ps1（Windows 裸进程）=="
check "ps1 存在" test -f scripts/deploy-windows.ps1
check "release zip 资产名" has scripts/deploy-windows.ps1 "devin-2api-windows-amd64.zip"
check "sha256 校验" has scripts/deploy-windows.ps1 "Get-FileHash"
check "healthz 轮询" has scripts/deploy-windows.ps1 "healthz"
check "上游冒烟 /v1/models" has scripts/deploy-windows.ps1 "/v1/models"
check "新控制台窗口启动" has scripts/deploy-windows.ps1 "Start-Process"
check "token 自动发现" has scripts/deploy-windows.ps1 "credentials.toml"
# UTF-8 BOM 是 PS5.1 正确解析中文的前提（无 BOM 按 ANSI 解码会乱码甚至截断字符串）。
check "UTF-8 BOM" bash -c "head -c3 scripts/deploy-windows.ps1 | od -An -tx1 | tr -d ' ' | grep -qx efbbbf"

echo "== release.sh（发版门禁）=="
check "HEAD 必须已推送" has scripts/release.sh 'origin/main'
check "查 CI workflow run" has scripts/release.sh 'workflow_runs'
check "VERSION 回写" has scripts/release.sh 'cmd/devin-2api/VERSION'

echo "== Dockerfile =="
# FROM 的 Go 版本必须 ≥ go.mod 的 go 指令：官方镜像 GOTOOLCHAIN=local，
# FROM 更低时 go mod download 直接失败（1.26.3 镜像撞 go 1.27.1 断过一次）。
check "Dockerfile FROM go 版本 ≥ go.mod go 指令" bash -c '
	from=$(sed -nE "s/^FROM .*golang:([0-9]+(\.[0-9]+){1,2}).*/\1/p" Dockerfile | head -1)
	want=$(sed -nE "s/^go ([0-9]+(\.[0-9]+){1,2}).*/\1/p" go.mod | head -1)
	[[ -n "${from}" && -n "${want}" ]] || exit 1
	oldest=$(printf "%s\n%s\n" "${want}" "${from}" | sort -t. -k1,1n -k2,2n -k3,3n | head -1)
	[[ "${oldest}" == "${want}" ]]
'

echo
if [[ "${FAILED}" == "1" ]]; then
	echo "deploy assets 断言失败" >&2
	exit 1
fi
echo "全部通过"
