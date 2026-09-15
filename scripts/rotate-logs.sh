#!/usr/bin/env bash
# rotate-logs.sh — copytruncate 轮转 <logs-dir>/ 下超过阈值的 *.log
# （stderr.log/stdout.log）。
#
# 这两个文件的写 fd 归服务管理器持有（launchd StandardErrorPath、systemd
# StandardError=append:），进程自己无法 reopen；rename 类轮转会让 fd 跟着
# 旧 inode 走、新写全丢。copytruncate（复制后原地截断）是唯一不丢写的
# 无信号方案——截断瞬间并发写入的一行仍可能丢，属该语义的最小代价。
#
# 由 deploy.sh（launchd StartInterval agent）与 deploy-linux.sh（systemd
# --user timer）每日驱动；也可手动执行。
set -euo pipefail

dir="${1:?usage: rotate-logs.sh <logs-dir>}"
threshold=$((50 * 1024 * 1024)) # 50MB：prod 实测 ~11MB/天，一周一轮转

shopt -s nullglob
for log in "${dir}"/*.log; do
	size="$(stat -c %s "${log}" 2>/dev/null || stat -f %z "${log}")"
	(( size >= threshold )) || continue
	rm -f "${log}.3.gz"
	[[ -f "${log}.2.gz" ]] && mv "${log}.2.gz" "${log}.3.gz"
	[[ -f "${log}.1.gz" ]] && mv "${log}.1.gz" "${log}.2.gz"
	cp "${log}" "${log}.1" && : >"${log}" && gzip -f "${log}.1"
done
