#!/usr/bin/env bash
# traffic-probe.sh — 给运行中的 devin-2api 实例灌一段混合流量并核对
# sqlite logs 行的字段保真度。用于 D2/D3 后「新写入路径」验收。
#
# 用法: scripts/traffic-probe.sh <port> <state-dir> [config.yaml]
#   覆盖：非流式/流式成功、anthropic /v1/messages、model_disabled 与
#   request_build 本地拒、上游不存在模型、大请求体、WS /v1/responses。
#   每发打完后按 dir 从 logs 表取行，逐字段断言（延迟段/token/stream/
#   account/error_stage/log_source/minute_bucket/conn_reused…）。
#   真上游调用，总量 ~10 发、max_tokens 极小，成本忽略不计。
set -uo pipefail

PORT="${1:?usage: traffic-probe.sh <port> <state-dir> [config]}"
STATE="${2:?}"
CONFIG="${3:-$HOME/.config/devin-2api/config.yaml}"
BASE="http://127.0.0.1:$PORT"
DB="$STATE/devin-2api.db"
MODEL="${GD_PROBE_MODEL:-swe-2-medium}"
PROBE_MODEL="tp-disabled-x"

PW="$(grep -E '^\s*password:' "$CONFIG" | head -1 | sed -E 's/.*password:\s*//; s/["'"'"']//g' | tr -d ' ')"
AK="$(grep -E '^\s*api_key:' "$CONFIG" | head -1 | sed -E 's/.*api_key:\s*//; s/["'"'"']//g' | tr -d ' ')"
AUTH=(-H "Authorization: Bearer $PW")
VAUTH=(-H "Authorization: Bearer $AK")

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
MANIFEST="$WORK/manifest.tsv"
: >"$MANIFEST"

send() { # label curl-extra-args... —— -d/-H 自带；-D 收头拿 X-Request-Id
	local label="$1"; shift
	local hdr="$WORK/h-$label" body="$WORK/b-$label"
	local code
	code="$(curl -s -o "$body" -D "$hdr" -w '%{http_code}' "${VAUTH[@]}" \
		-H 'Content-Type: application/json' "$@" || echo 000)"
	local dir
	dir="$(grep -i '^x-request-id:' "$hdr" | tr -d '\r' | awk '{print $2}')"
	printf '%s\t%s\t%s\n' "$label" "$code" "${dir:-}" >>"$MANIFEST"
	echo "SENT $label -> HTTP $code dir=${dir:-none}"
}

# --- 模型注册表停用项（model_disabled 探针） ---
curl -sf "${AUTH[@]}" -X PUT -H 'Content-Type: application/json' \
	-d "{\"model\":\"$PROBE_MODEL\",\"enabled\":false}" \
	"$BASE/admin/model-registry" >/dev/null || echo "WARN registry PUT failed"

# --- 成功路径 ---
send ok-nonstream-1 -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"reply with just: ok\"}],\"max_tokens\":8,\"stream\":false}"
send ok-nonstream-2 -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"count to 3\"}],\"max_tokens\":16,\"stream\":false}"
send ok-stream -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"say hi\"}],\"max_tokens\":8,\"stream\":true}"
send ok-anthropic -X POST "$BASE/v1/messages" \
	-d "{\"model\":\"$MODEL\",\"max_tokens\":8,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}"

# --- 本地拒绝（零上游） ---
send local-model-disabled -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"$PROBE_MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}],\"stream\":false}"
send local-request-build -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}],\"tools\":[{\"type\":\"function\",\"function\":{\"name\":\"f1\",\"parameters\":{\"type\":\"object\"}}}],\"tool_choice\":{\"type\":\"function\",\"function\":{\"name\":\"nonexistent_fn\"}},\"stream\":false}"

# --- 上游拒绝 ---
send upstream-badmodel -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"no-such-model-tp-zzz\",\"messages\":[{\"role\":\"user\",\"content\":\"x\"}],\"max_tokens\":4,\"stream\":false}"

# --- 大请求体（~90KB prompt） ---
BIG="$(python3 -c 'print("pad "*30000)')"
send ok-bigbody -X POST "$BASE/v1/chat/completions" \
	-d "{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"$BIG\"}],\"max_tokens\":4,\"stream\":false}"

# --- WS /v1/responses（stdlib 客户端，握手 + 一个事件 + 读帧） ---
python3 - "$BASE" "$AK" "$WORK" "$MODEL" <<'PYEOF'
import socket, base64, os, sys, json, urllib.parse, time
base, ak, work, model = sys.argv[1:5]
u = urllib.parse.urlparse(base)
host, port = u.hostname, u.port or 80
s = socket.create_connection((host, port), timeout=15)
key = base64.b64encode(os.urandom(16)).decode()
s.sendall((f"GET /v1/responses HTTP/1.1\r\nHost: {host}:{port}\r\n"
           f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
           f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
           f"Authorization: Bearer {ak}\r\n\r\n").encode())
resp = s.recv(65536)
status = resp.split(b"\r\n", 1)[0].decode(errors="replace")
xrid = ""
for line in resp.split(b"\r\n"):
    if line.lower().startswith(b"x-request-id:"):
        xrid = line.split(b":",1)[1].strip().decode()
print("WS handshake:", status, "xrid:", xrid or "none")
if " 101" not in status and "101" not in status.split()[1:2]:
    open(f"{work}/ws-result.txt","w").write(f"ws\t{status.split()[1] if len(status.split())>1 else '?'}\t{xrid}\n")
    sys.exit(0)
def frame(payload: bytes) -> bytes:
    h = bytearray([0x81]); ln = len(payload)
    mask = os.urandom(4)
    if ln < 126: h.append(0x80 | ln)
    elif ln < 65536: h.append(0x80 | 126); h += ln.to_bytes(2,"big")
    else: h.append(0x80 | 127); h += ln.to_bytes(8,"big")
    h += mask
    return bytes(h) + bytes(b ^ mask[i%4] for i,b in enumerate(payload))
s.sendall(frame(json.dumps({"type":"response.create","response":{"model":model,
    "input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"say ok"}]}],
    "max_output_tokens":8}}).encode()))
s.settimeout(45)
buf = b""
try:
    deadline = time.time()+45
    while time.time() < deadline:
        chunk = s.recv(65536)
        if not chunk: break
        buf += chunk
        if b"response.completed" in buf or b"response.failed" in buf or b'"error"' in buf: break
except socket.timeout:
    pass
try:
    s.sendall(frame(b"")[:0] or b"\x88\x80" + os.urandom(4))  # close frame
except Exception:
    pass
s.close()
open(f"{work}/ws-result.txt","w").write(f"ws\t101\t{xrid}\n")
print("WS frames got:", len(buf))
PYEOF
[[ -f "$WORK/ws-result.txt" ]] && cat "$WORK/ws-result.txt" >>"$MANIFEST"

sleep 2  # 写路径落库

# --- 逐行字段断言 ---
python3 - "$DB" "$STATE" "$MANIFEST" <<'PYEOF'
import sqlite3, sys, json, os
db, state, manifest = sys.argv[1:4]
con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
con.row_factory = sqlite3.Row
cols = [r[1] for r in con.execute("PRAGMA table_info(logs)")]

EXPECT = {
 "ok-nonstream-1":  dict(api="openai-chat", status_code=200, result="completed", stream=0, method="POST", path="/v1/chat/completions"),
 "ok-nonstream-2":  dict(api="openai-chat", status_code=200, result="completed", stream=0),
 "ok-stream":       dict(api="openai-chat", status_code=200, result="completed", stream=1),
 "ok-anthropic":    dict(api="anthropic",  status_code=200, result="completed", path="/v1/messages"),
 "local-model-disabled": dict(result="failed", error_stage="model_disabled", status_code=404),
 "local-request-build":  dict(result="failed", error_stage="request_build"),
 "upstream-badmodel":    dict(result="failed"),
 "ok-bigbody":      dict(api="openai-chat", stream=0),
 "ws":              dict(),
}

fails = []
for line in open(manifest):
    label, code, dir_ = line.rstrip("\n").split("\t")
    if not dir_:
        fails.append(f"{label}: 无 dir（HTTP {code}）"); continue
    row = con.execute("SELECT * FROM logs WHERE dir=?", (dir_,)).fetchone()
    if not row:
        fails.append(f"{label}: logs 无行 dir={dir_}"); continue
    r = dict(row)
    exp = EXPECT.get(label, {})
    errs = []
    for k, v in exp.items():
        if r.get(k) != v:
            errs.append(f"{k}={r.get(k)!r} 期望 {v!r}")
    # 通用断言
    if r.get("log_source") != "proxy":
        errs.append(f"log_source={r.get('log_source')}")
    if r.get("time") and r.get("minute_bucket") is not None and r["minute_bucket"] != r["time"]//60000:
        errs.append(f"minute_bucket={r['minute_bucket']} != time//60000")
    if r.get("result") == "completed":
        for seg in ("first_upstream_ms","first_client_ms"):
            if r.get(seg) is None:
                errs.append(f"{seg}=NULL")
        if r.get("duration_ms") is not None and r.get("first_upstream_ms") is not None \
           and r["first_upstream_ms"] > r["duration_ms"]:
            errs.append("first_upstream_ms > duration_ms")
        if r.get("stream") == 0 and not r.get("total_tokens"):
            errs.append("非流式成功但 total_tokens=0")
        if not r.get("account"):
            errs.append("account 空")
    if r.get("result") == "failed" and not r.get("error_stage"):
        errs.append("failed 但 error_stage 空")
    # meta.json 对照
    meta_p = os.path.join(state, "logs", dir_, "meta.json")
    meta_ok = "no-meta"
    if os.path.exists(meta_p):
        m = json.load(open(meta_p))
        meta_ok = f"meta.account={m.get('upstream_account')} status={m.get('status_code')}"
        if m.get("upstream_account") and r.get("account") and m["upstream_account"] != r["account"]:
            errs.append(f"meta.account={m['upstream_account']} != logs.account={r['account']}")
        if m.get("status_code") != r.get("status_code"):
            errs.append(f"meta.status={m['status_code']} != row {r['status_code']}")
    files = sorted(os.listdir(os.path.join(state, "logs", dir_))) if os.path.isdir(os.path.join(state,"logs",dir_)) else []
    summary = (f"{label}: dir={dir_} http={code} status={r.get('status_code')} "
               f"result={r.get('result')} stage={r.get('error_stage') or '-'} "
               f"api={r.get('api')} model={r.get('model')} acct={r.get('account')} "
               f"sw={r.get('account_switches')} tok(in/out/cr/cw/reas/tot)="
               f"{r.get('input_tokens')}/{r.get('output_tokens')}/{r.get('cache_read_tokens')}/"
               f"{r.get('cache_write_tokens')}/{r.get('reasoning_tokens')}/{r.get('total_tokens')} "
               f"lat(rdy/sent/open/fu/fc/dur)={r.get('request_ready_ms')}/{r.get('upstream_sent_ms')}/"
               f"{r.get('upstream_open_ms')}/{r.get('first_upstream_ms')}/{r.get('first_client_ms')}/{r.get('duration_ms')} "
               f"stream={r.get('stream')} ws={r.get('upstream_websocket')} reused={r.get('conn_reused')} "
               f"idle={r.get('conn_idle_ms')} retries={r.get('retries')} ls={r.get('log_source')} "
               f"mb={r.get('minute_bucket')} crid={r.get('client_request_id') or '-'} files={len(files)} {meta_ok}")
    print(("FAIL " if errs else "OK   ") + summary)
    for e in errs:
        print("      ! " + e)
        fails.append(f"{label}: {e}")
con.close()
print("----")
print("ASSERT-FAILS:", len(fails))
for f in fails: print("  " + f)
PYEOF

# 清理探针注册表项
curl -sf "${AUTH[@]}" -X DELETE "$BASE/admin/model-registry?model=$PROBE_MODEL" >/dev/null || true
echo "done"
