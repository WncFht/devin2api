#!/usr/bin/env python3
"""Edge-case matrix: streaming events, tool_choice modes, image/error
tool_results, parallel call+result pairing, namespace flattening probe.

Usage: run_edges.py [edge-name ...]  (no args = all)
Env: TOOLALIGN_BASE / TOOLALIGN_KEY，同 run_matrix.py。
"""

import json
import os
import time
import urllib.error
import urllib.request

BASE = os.environ.get("TOOLALIGN_BASE", "http://127.0.0.1:3033")
HERE = os.path.dirname(os.path.abspath(__file__))


def _default_key():
    for cfg in ("./config.yaml", os.path.join(HERE, "../../config.yaml")):
        try:
            for line in open(cfg):
                s = line.strip()
                if s.startswith("api_key:"):
                    return s.split(":", 1)[1].strip().strip("'\"")
        except OSError:
            continue
    return ""


KEY = os.environ.get("TOOLALIGN_KEY", _default_key())
RESULTS = open(HERE + "/results-edges.jsonl", "a")


def emit(rec):
    RESULTS.write(json.dumps(rec, ensure_ascii=False) + "\n")
    RESULTS.flush()
    print(json.dumps(rec, ensure_ascii=False)[:300])


def post(path, body, timeout=240, tries=3, stream=False):
    headers = {
        "Authorization": "Bearer " + KEY,
        "Content-Type": "application/json",
        "anthropic-version": "2023-06-01",
    }
    for attempt in range(tries):
        try:
            req = urllib.request.Request(
                BASE + path, data=json.dumps(body).encode(), headers=headers)
            r = urllib.request.urlopen(req, timeout=timeout)
            raw = r.read()
            if stream:
                return r.status, raw.decode("utf-8", "replace"), ""
            try:
                return r.status, json.loads(raw), ""
            except json.JSONDecodeError:
                return r.status, {"_raw": raw.decode("utf-8", "replace")[:2000]}, ""
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            if e.code in (408, 409, 425, 429, 500, 502, 503, 504) and attempt < tries - 1:
                time.sleep(6 * (attempt + 1))
                continue
            return e.code, raw[:800], "http_error"
        except Exception as e:  # noqa: BLE001
            if attempt < tries - 1:
                time.sleep(5 * (attempt + 1))
                continue
            return -1, str(e), "transport"


def sse_events(raw):
    """Parse SSE stream into (event, data-json) list."""
    out = []
    event = None
    for line in raw.splitlines():
        if line.startswith("event:"):
            event = line[6:].strip()
        elif line.startswith("data:"):
            data = line[5:].strip()
            if data and data != "[DONE]":
                try:
                    out.append((event, json.loads(data)))
                except json.JSONDecodeError:
                    out.append((event, data))
    return out


CC_TOOLS = json.load(open(HERE + "/cc-tools.json"))
CODEX_TOOLS = json.load(open(HERE + "/codex-tools.json"))
PNG_1x1_RED = (
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
)


# --- 1. CC streaming force-call -------------------------------------------
def cc_stream_bash():
    body = {
        "model": "claude-opus-4-6", "max_tokens": 4096, "stream": True,
        "tools": CC_TOOLS,
        "tool_choice": {"type": "tool", "name": "Bash"},
        "messages": [{"role": "user", "content": "list /tmp"}],
    }
    status, raw, err = post("/v1/messages", body, stream=True)
    rec = {"edge": "cc_stream_force_Bash", "http": status}
    if status != 200:
        rec.update(ok=False, error=raw[:300])
        return emit(rec)
    evs = sse_events(raw)
    names = [e for e, _ in evs]
    starts = [d for e, d in evs if e == "content_block_start"]
    tool_starts = [d for d in starts if (d.get("content_block") or {}).get("type") == "tool_use"]
    rec["event_seq"] = names[:40]
    rec["tool_use_start"] = bool(tool_starts)
    rec["tool_name"] = (tool_starts[0].get("content_block") or {}).get("name") if tool_starts else None
    rec["has_input_json_delta"] = "content_block_delta" in names
    stop = [d for e, d in evs if e == "message_delta"]
    rec["stop_reason"] = (stop[-1].get("delta") or {}).get("stop_reason") if stop else None
    rec["ok"] = rec["tool_use_start"] and rec["tool_name"] == "Bash" and rec["stop_reason"] == "tool_use"
    emit(rec)


# --- 2. Codex streaming apply_patch ---------------------------------------
def codex_stream_apply_patch():
    body = {
        "model": "swe-2-max",
        "instructions": "You are Codex.",
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text": "patch file /tmp/a.txt to say hi"}]}],
        "tools": CODEX_TOOLS,
        "tool_choice": {"type": "function", "name": "apply_patch"},
        "stream": True,
    }
    status, raw, err = post("/v1/responses", body, stream=True)
    rec = {"edge": "codex_stream_apply_patch", "http": status}
    if status != 200:
        rec.update(ok=False, error=raw[:300])
        return emit(rec)
    evs = sse_events(raw)
    names = [e for e, _ in evs]
    rec["has_input_delta"] = any("custom_tool_call_input.delta" == e for e, _ in evs)
    rec["has_input_done"] = any("custom_tool_call_input.done" == e for e, _ in evs)
    done_items = [d for e, d in evs if e == "response.output_item.done"]
    custom = [d for d in done_items if (d.get("item") or {}).get("type") == "custom_tool_call"]
    rec["custom_call_done"] = bool(custom)
    rec["input_preview"] = (custom[0]["item"].get("input") or "")[:120] if custom else None
    rec["ok"] = rec["has_input_done"] and rec["custom_call_done"]
    emit(rec)


# --- 3. tool_choice=none both protocols ------------------------------------
def tool_choice_none():
    body = {
        "model": "claude-opus-4-6", "max_tokens": 2048,
        "tools": CC_TOOLS,
        "tool_choice": {"type": "none"},
        "messages": [{"role": "user", "content":
                      "You MUST call the Bash tool right now. No text, just the call."}],
    }
    status, resp, err = post("/v1/messages", body)
    rec = {"edge": "cc_tool_choice_none", "http": status}
    if status == 200:
        uses = [b for b in resp.get("content", []) if b.get("type") == "tool_use"]
        rec["n_tool_use"] = len(uses)
        rec["ok"] = len(uses) == 0
    else:
        rec.update(ok=False, error=str(resp)[:300])
    emit(rec)

    body = {
        "model": "swe-2-max", "instructions": "You are Codex.",
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text":
                                "You MUST call exec_command right now."}]}],
        "tools": CODEX_TOOLS, "tool_choice": "none", "stream": False,
    }
    status, resp, err = post("/v1/responses", body)
    rec = {"edge": "codex_tool_choice_none", "http": status}
    if status == 200:
        calls = [i for i in resp.get("output", [])
                 if i.get("type") in ("function_call", "custom_tool_call")]
        rec["n_calls"] = len(calls)
        rec["ok"] = len(calls) == 0
    else:
        rec.update(ok=False, error=str(resp)[:300])
    emit(rec)


# --- 4. tool_result image + is_error ---------------------------------------
def tool_result_image_error():
    assistant = [{"type": "tool_use", "id": "toolu_img1", "name": "Read",
                  "input": {"file_path": "/tmp/x.png"}}]
    body = {
        "model": "claude-opus-4-6", "max_tokens": 2048, "tools": CC_TOOLS,
        "messages": [
            {"role": "user", "content": "read /tmp/x.png and tell me its color"},
            {"role": "assistant", "content": assistant},
            {"role": "user", "content": [{
                "type": "tool_result", "tool_use_id": "toolu_img1",
                "content": [{"type": "image",
                             "source": {"type": "base64",
                                        "media_type": "image/png",
                                        "data": PNG_1x1_RED}}]}]},
        ],
    }
    status, resp, err = post("/v1/messages", body)
    rec = {"edge": "cc_tool_result_image", "http": status}
    if status == 200:
        text = "".join(b.get("text", "") for b in resp["content"] if b.get("type") == "text")
        rec["text"] = text[:200]
        rec["mentions_red"] = "red" in text.lower()
        rec["ok"] = True
    else:
        rec.update(ok=False, error=str(resp)[:300])
    emit(rec)

    assistant = [{"type": "tool_use", "id": "toolu_err1", "name": "Bash",
                  "input": {"command": "ls /nonexistent"}}]
    body = {
        "model": "claude-opus-4-6", "max_tokens": 2048, "tools": CC_TOOLS,
        "messages": [
            {"role": "user", "content": "list /nonexistent"},
            {"role": "assistant", "content": assistant},
            {"role": "user", "content": [{
                "type": "tool_result", "tool_use_id": "toolu_err1",
                "is_error": True,
                "content": "ls: cannot access '/nonexistent': No such file or directory"}]},
        ],
    }
    status, resp, err = post("/v1/messages", body)
    rec = {"edge": "cc_tool_result_is_error", "http": status}
    if status == 200:
        rec["stop_reason"] = resp.get("stop_reason")
        rec["ok"] = True
    else:
        rec.update(ok=False, error=str(resp)[:300])
    emit(rec)


# --- 5. Parallel calls + grouped results (responses) -----------------------
def codex_parallel_pairing():
    """call,call,result,result — the wire reorder path must keep it working."""
    body = {
        "model": "swe-2-max",
        "instructions": "You are Codex.",
        "input": [
            {"type": "message", "role": "user",
             "content": [{"type": "input_text", "text": "read a.txt and b.txt"}]},
            {"type": "function_call", "call_id": "c1", "name": "exec_command",
             "arguments": json.dumps({"cmd": "cat a.txt"})},
            {"type": "function_call", "call_id": "c2", "name": "exec_command",
             "arguments": json.dumps({"cmd": "cat b.txt"})},
            {"type": "function_call_output", "call_id": "c1", "output": "aaa"},
            {"type": "function_call_output", "call_id": "c2", "output": "bbb"},
        ],
        "tools": CODEX_TOOLS, "stream": False,
    }
    status, resp, err = post("/v1/responses", body)
    rec = {"edge": "codex_parallel_pair", "http": status}
    rec["ok"] = status == 200
    if status == 200:
        rec["status_field"] = resp.get("status")
    else:
        rec["error"] = str(resp)[:300]
    emit(rec)


# --- 6. namespace flatten probe ---------------------------------------------
def namespace_flatten_probe():
    """If we flattened collaboration.spawn_agent to a legal declared name and
    rewrote call names back, would the model call it? Probe the declaration."""
    flat = [dict(t) for t in CODEX_TOOLS if t.get("type") in ("function", "custom")]
    flat.append({
        "type": "function", "name": "collaboration__spawn_agent",
        "description": "Spawn a sub-agent to work on a task (collaboration.spawn_agent).",
        "parameters": {"type": "object",
                       "properties": {"task_name": {"type": "string"},
                                      "message": {"type": "string"}},
                       "required": ["task_name", "message"],
                       "additionalProperties": False},
    })
    body = {
        "model": "swe-2-max", "instructions": "You are Codex.",
        "input": [{"type": "message", "role": "user",
                   "content": [{"type": "input_text", "text": "spawn an agent to count files"}]}],
        "tools": flat,
        "tool_choice": {"type": "function", "name": "collaboration__spawn_agent"},
        "stream": False,
    }
    status, resp, err = post("/v1/responses", body)
    rec = {"edge": "namespace_flatten_spawn", "http": status}
    if status == 200:
        calls = [i for i in resp.get("output", []) if i.get("type") == "function_call"]
        rec["called"] = [c.get("name") for c in calls]
        rec["ok"] = bool(calls) and calls[0].get("name") == "collaboration__spawn_agent"
    else:
        rec.update(ok=False, error=str(resp)[:300])
    emit(rec)


if __name__ == "__main__":
    import sys
    which = sys.argv[1:] if len(sys.argv) > 1 else None
    steps = {
        "cc_stream": cc_stream_bash,
        "codex_stream_patch": codex_stream_apply_patch,
        "none": tool_choice_none,
        "image_error": tool_result_image_error,
        "parallel": codex_parallel_pairing,
        "ns_flatten": namespace_flatten_probe,
    }
    for name, fn in steps.items():
        if which and name not in which:
            continue
        try:
            fn()
        except Exception as e:  # noqa: BLE001
            emit({"edge": name, "ok": False, "error": f"harness: {e}"})
