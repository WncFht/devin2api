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

echo "== deploy.sh（macOS launchd）=="
check "限定 Darwin" has scripts/deploy.sh 'Darwin'
check "plist 保活" has scripts/deploy.sh 'KeepAlive'
check "优雅退出窗口" has scripts/deploy.sh 'ExitTimeOut'
check "kickstart -k 发 SIGTERM" has scripts/deploy.sh 'kickstart -k'
check "禁 kill -9 约定" bash -c "! grep -qF 'kill -9' scripts/deploy.sh"

echo "== deploy-linux.sh（systemd --user）=="
check "限定 Linux" has scripts/deploy-linux.sh 'Linux'
check "Restart=always" has scripts/deploy-linux.sh 'Restart=always'
check "优雅停止窗口" has scripts/deploy-linux.sh 'TimeoutStopSec='
check "systemctl --user" has scripts/deploy-linux.sh 'systemctl --user'
check "日志落运行目录" has scripts/deploy-linux.sh 'StandardOutput=append:'

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

echo
if [[ "${FAILED}" == "1" ]]; then
	echo "deploy assets 断言失败" >&2
	exit 1
fi
echo "全部通过"
