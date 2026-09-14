#!/usr/bin/env bash
# PreToolUse hook: hard cap on Agent spawns per session.
#
# Register in .claude/settings.json (or settings.local.json):
#
#   "hooks": {
#     "PreToolUse": [{
#       "matcher": "Agent",
#       "hooks": [{"type": "command",
#                  "command": "<abs path>/agent-budget-gate.sh"}]
#     }]
#   }
#
# Cap via env AGENT_BUDGET (default 20). State: one marker file per spawn in
# ${TMPDIR}/claude-agent-budget/<session_id>/ — counts every Agent call in the
# session, including spawns issued inside subagents.
#
# Past the cap the hook returns permissionDecision=deny; the reason is fed back
# to the model, so the run winds down with existing agents instead of dying.

set -euo pipefail

input=$(cat)
session=$(printf '%s' "$input" | jq -r '.session_id')
cap=${AGENT_BUDGET:-20}

dir="${TMPDIR:-/tmp}/claude-agent-budget/$session"
lock="$dir.lock"
mkdir -p "$dir"

# mkdir is atomic on POSIX: spin until we hold the lock. Parallel Agent calls
# fire parallel hooks, so the count-and-claim must be serialized.
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
    permissionDecisionReason: ("Agent budget exhausted (" + $cap + " per session). Finish with existing agents, or report partial results.")
  }
}'
