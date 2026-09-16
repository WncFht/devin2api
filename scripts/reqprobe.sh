#!/usr/bin/env bash
# reqprobe.sh <label> <endpoint> <json-body-file>
#
# 对运行中实例 POST 一个 JSON 请求，按 X-Request-Id 定位调试目录，
# 打印三段证据摘要：02 投影（dropped/tool_choice/IR 形态）、
# 03 上游 wire（工具名集合）、响应体（SSE 事件计数或 final JSON 形状）。
#
# 与 smoke.sh 互补：smoke 管实例生命周期，reqprobe 管单请求取证。
#
# 环境变量：
#   REQPROBE_BASE     实例地址（默认 http://127.0.0.1:3033，可指 tailscale prod）
#   REQPROBE_KEY      API key（默认从 ./config.yaml 的 auth.api_key 提取）
#   REQPROBE_LOGS     调试目录根（默认 ./logs，repo 根的符号链接指向实例 state dir）
#   REQPROBE_TIMEOUT  curl 秒数（默认 120）
set -u

LABEL="${1:?usage: reqprobe.sh <label> <endpoint> <body-file>}"
EP="${2:?missing endpoint, e.g. /v1/messages}"
BODY="${3:?missing json body file}"

BASE="${REQPROBE_BASE:-http://127.0.0.1:3033}"
LOGS="${REQPROBE_LOGS:-./logs}"
TIMEOUT="${REQPROBE_TIMEOUT:-120}"

KEY="${REQPROBE_KEY:-}"
if [[ -z "$KEY" && -f ./config.yaml ]]; then
	KEY="$(sed -nE "s/^[[:space:]]*api_key:[[:space:]]*['\"]?([^'\"[:space:]]+)['\"]?.*/\1/p" ./config.yaml | head -1)"
fi
AUTH=()
[[ -n "$KEY" ]] && AUTH=(-H "Authorization: Bearer $KEY")

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

CODE=$(curl -s -m "$TIMEOUT" -D "$WORK/hdr.txt" -o "$WORK/body.txt" -w '%{http_code}' \
	-H 'Content-Type: application/json' "${AUTH[@]}" \
	"$BASE$EP" --data-binary "@$BODY")
RID=$(grep -i '^x-request-id:' "$WORK/hdr.txt" | tr -d '\r' | awk '{print $2}')
DIR="$LOGS/$RID"

echo "=== $LABEL | HTTP $CODE | dir=$RID"
[[ -d "$DIR" ]] || echo "  (warn) $DIR 不存在——实例可能没开 debug 日志或 LOGS 指错"

python3 - "$DIR" "$WORK/body.txt" <<'PYEOF'
import json, sys, os
d, bodyf = sys.argv[1], sys.argv[2]
# 02: 投影层——dropped 项、tool_choice、IR 消息形态序列
try:
    m = json.load(open(os.path.join(d, '02-request-messages.json')))
    msgs = m['messages'] if isinstance(m, dict) else m
    if isinstance(m, dict):
        for k in ('dropped_items', 'dropped', 'server_search', 'tool_choice'):
            v = m.get(k)
            if v:
                print(f'  {k}:', json.dumps(v, ensure_ascii=False)[:200])
        tools = m.get('tools') or []
        if tools:
            print('  ir_tools:', [t.get('name') for t in tools])
    seq = []
    for msg in msgs:
        kinds = []
        for c in (msg.get('content') or []):
            if isinstance(c, dict):
                k = c.get('type')
                if k in ('toolCall', 'tool_call', 'tool_use'):
                    k = f"call:{c.get('name')}"
                if k in ('serverToolResult', 'server_tool_result'):
                    k = 'srvResult'
                kinds.append(k)
        seq.append(f"{msg.get('role')}[{','.join(map(str, kinds))}]")
    print('  msgs:', ' | '.join(seq)[:400])
except Exception as e:
    print('  02:', type(e).__name__, str(e)[:80])
# 03: 上游 wire——全部出现的 name 字段 + 顶层 tools 名
try:
    w = json.load(open(os.path.join(d, '03-devin-request.json')))
    names = []
    def walk(o):
        if isinstance(o, dict):
            for k, v in o.items():
                if k == 'name' and isinstance(v, str) and v:
                    names.append(v)
                walk(v)
        elif isinstance(o, list):
            for v in o:
                walk(v)
    walk(w)
    print('  wire_names:', sorted(set(names))[:20])
    if 'tools' in w:
        print('  wire_tools:', [t.get('name') for t in w['tools']])
except Exception as e:
    print('  03:', type(e).__name__, str(e)[:80])
# 响应体：SSE 事件计数 + block 类型，或 final JSON 形状
try:
    raw = open(bodyf).read()
    if raw.startswith('event:') or '\nevent:' in raw:
        from collections import Counter
        evs = [l[6:].strip() for l in raw.splitlines() if l.startswith('event:')]
        print('  sse:', dict(Counter(evs)))
        for l in raw.splitlines():
            if l.startswith('data:') and 'content_block_start' in l:
                try:
                    blk = json.loads(l[5:])['content_block']
                    print('   block_start:', blk.get('type'), blk.get('name', ''))
                except Exception:
                    pass
    else:
        r = json.loads(raw)
        if 'output' in r:  # responses API
            print('  status:', r.get('status'), '| output:',
                  [(i.get('type'), i.get('name', '')) for i in r['output']])
        elif 'content' in r:  # anthropic
            print('  stop:', r.get('stop_reason'), '| content:',
                  [c.get('type') for c in r['content']])
        else:
            print('  err:', json.dumps(r, ensure_ascii=False)[:300])
except Exception as e:
    print('  body:', type(e).__name__, str(e)[:80])
PYEOF
