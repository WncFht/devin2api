#!/usr/bin/env bash
# 按固定间隔轮询上游 GetUserStatus，把 planStatus 摘要追加为 JSONL。
# 配额只剩整数百分比语义，采样间隔决定每次翻转的「括号宽度」——
# 反推额度时需要比面板默认 5 分钟更密的点，本脚本默认 30 秒。
#
# 用法: poll.sh [间隔秒] [输出文件] [config.yaml]
# token 从 config.yaml 的 devin.token 读取；Ctrl-C 停止。
set -u

INTERVAL="${1:-30}"
OUT="${2:-quota-probe.jsonl}"
CONF="${3:-$HOME/Library/Application Support/devin-2api/config.yaml}"
TOKEN=$(grep -E '^[[:space:]]+token:' "$CONF" | head -1 | sed 's/.*token:[[:space:]]*//')
URL='https://server.codeium.com/exa.seat_management_pb.SeatManagementService/GetUserStatus'

while true; do
  curl -s -X POST "$URL" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
    -H 'Connect-Protocol-Version: 1' \
    -d '{"metadata":{"api_key":"'"$TOKEN"'","extension_name":"windsurf","extension_version":"1.48.2","ide_name":"windsurf","ide_version":"1.48.2","locale":"en","os":"windows"}}' \
  | python3 -c "
import json,sys,time
try:
    ps=json.load(sys.stdin)['userStatus']['planStatus']
except Exception as e:
    print(f'parse failed: {e}',file=sys.stderr); sys.exit(0)
row={'t':time.time(),
 'daily':ps.get('dailyQuotaRemainingPercent'),
 'weekly':ps.get('weeklyQuotaRemainingPercent'),
 'daily_reset':ps.get('dailyQuotaResetAtUnix'),
 'weekly_reset':ps.get('weeklyQuotaResetAtUnix'),
 'acu_consumed':ps.get('acuConsumed'),
 'acu_limit':ps.get('acuLimit'),
 'overage_micros':ps.get('overageBalanceMicros')}
line=json.dumps(row)
print(line)
open('$OUT','a').write(line+'\n')
"
  sleep "$INTERVAL"
done
