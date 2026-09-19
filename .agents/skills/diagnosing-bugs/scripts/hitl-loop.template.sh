#!/usr/bin/env bash
# 人在环复现回路。
# 复制本文件，改下面的步骤，然后运行。
# agent 跑脚本；用户在终端里按提示操作。
#
# 用法：
#   bash hitl-loop.template.sh
#
# 两个辅助函数：
#   step "<instruction>"          → 显示指示，等待回车
#   capture VAR "<question>"      → 显示问题，把回答读进 VAR
#
# 结尾把捕获值打印为 KEY=VALUE 供 agent 解析。
#
# `capture` 把值打印回终端，agent 在那里读它，
# 所以用 capture 收集观察结果；登录这类动作写成 `step` 留给用户。

set -euo pipefail

step() {
  printf '\n>>> %s\n' "$1"
  read -r -p "    [完成后按回车] " _
}

capture() {
  local var="$1" question="$2" answer
  printf '\n>>> %s\n' "$question"
  read -r -p "    > " answer
  printf -v "$var" '%s' "$answer"
}

# --- 在下面改 ---------------------------------------------------------

step "打开 http://localhost:3000 的应用并登录。"

capture ERRORED "点击 'Export' 按钮。报错了吗？(y/n)"

capture ERROR_MSG "粘贴错误信息（没有就填 'none'）："

# --- 在上面改 ---------------------------------------------------------

printf '\n--- 捕获值 ---\n'
printf 'ERRORED=%s\n' "$ERRORED"
printf 'ERROR_MSG=%s\n' "$ERROR_MSG"
