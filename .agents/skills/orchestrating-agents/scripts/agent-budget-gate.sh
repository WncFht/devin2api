#!/usr/bin/env bash
# PreToolUse hook：按会话给 Agent 派生数设总量上限。
#
# 在 .claude/settings.json（或 settings.local.json）里注册：
#
#   "hooks": {
#     "PreToolUse": [{
#       "matcher": "Agent",
#       "hooks": [{"type": "command",
#                  "command": "<绝对路径>/agent-budget-gate.sh"}]
#     }]
#   }
#
# 上限用环境变量 AGENT_BUDGET 调（默认 20）。状态：每次派生在
# ${TMPDIR}/claude-agent-budget/<session_id>/ 下落一个标记文件——
# 统计本会话内全部 Agent 调用，包括子代理内部发起的派生。
#
# 超限时返回 permissionDecision=deny，原因反馈给模型，
# 运行得以用已有代理收尾，而不是无声中断。

set -euo pipefail

input=$(cat)
session=$(printf '%s' "$input" | jq -r '.session_id')
cap=${AGENT_BUDGET:-20}

dir="${TMPDIR:-/tmp}/claude-agent-budget/$session"
lock="$dir.lock"
mkdir -p "$dir"

# mkdir 在 POSIX 上是原子的：抢不到就自旋。并行的 Agent 调用触发并行
# hook，「计数 + 占位」必须串行化，否则两个调用会同时看到同一个计数。
until mkdir "$lock" 2>/dev/null; do :; done
trap 'rmdir "$lock" 2>/dev/null || true' EXIT

count=$(find "$dir" -type f | wc -l | tr -d ' ')
if [ "$count" -lt "$cap" ]; then
  touch "$dir/$count"
  exit 0
fi

jq -n --arg cap "$cap" '{
  hookSpecificOutput: {
    hookEventName: "PreToolUse",
    permissionDecision: "deny",
    permissionDecisionReason: ("Agent 预算已耗尽（每会话 " + $cap + " 个）。请用已有代理收尾，或如实汇报部分结果。")
  }
}'
