#!/usr/bin/env python3
"""Per-tool verification matrix against a devin-2api instance.

For every tool a real client declares, run two live checks through the proxy:
  phase1 call    - force the model to invoke the tool; verify a well-formed
                   tool call comes back (name matches, arguments is a JSON
                   object or - for custom tools - raw freeform text).
  phase2 result  - echo the model's assistant turn back with a tool_result;
                   verify the conversation continues (HTTP 200, no upstream
                   semantic rejection).

Usage: run_matrix.py <cc|codex> [--only name1,name2] [--workers N]
Writes results-<suite>.jsonl next to this script.

Env: TOOLALIGN_BASE (default http://127.0.0.1:3033), TOOLALIGN_KEY
(default: auth.api_key from ./config.yaml when present).
"""

import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

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

WRITE_LOCK = threading.Lock()
OUT = None


def emit(record):
    with WRITE_LOCK:
        OUT.write(json.dumps(record, ensure_ascii=False) + "\n")
        OUT.flush()


def post(path, body, timeout=240, tries=4):
    """POST JSON; retry transient/rate-limit failures with backoff."""
    headers = {
        "Authorization": "Bearer " + KEY,
        "Content-Type": "application/json",
        "anthropic-version": "2023-06-01",
    }
    last = None
    for attempt in range(tries):
        try:
            req = urllib.request.Request(
                BASE + path, data=json.dumps(body).encode(), headers=headers)
            with urllib.request.urlopen(req, timeout=timeout) as r:
                raw = r.read()
                try:
                    return r.status, json.loads(raw), ""
                except json.JSONDecodeError:
                    return r.status, {"_raw": raw.decode("utf-8", "replace")}, ""
        except urllib.error.HTTPError as e:
            raw = e.read().decode("utf-8", "replace")
            last = (e.code, raw[:800])
            if e.code in (408, 409, 425, 429, 500, 502, 503, 504) and attempt < tries - 1:
                time.sleep(6 * (attempt + 1))
                continue
            return e.code, raw, "http_error"
        except Exception as e:  # noqa: BLE001 - network boundary
            last = (-1, str(e))
            if attempt < tries - 1:
                time.sleep(5 * (attempt + 1))
                continue
            return -1, str(e), "transport"
    return last[0], last[1], "retry_exhausted"


# ---------------- CC / Anthropic Messages ----------------

def cc_phase1(tool, all_tools):
    body = {
        "model": "claude-opus-4-6",
        "max_tokens": 4096,
        "tools": all_tools,
        "tool_choice": {"type": "tool", "name": tool["name"]},
        "messages": [{"role": "user", "content": "Do the task now."}],
    }
    status, resp, err = post("/v1/messages", body)
    rec = {"suite": "cc", "tool": tool["name"], "phase": "call", "http": status}
    if status != 200:
        rec.update(ok=False, error=str(resp)[:400], kind=err)
        return rec, None
    blocks = resp.get("content", [])
    uses = [b for b in blocks if b.get("type") == "tool_use"]
    rec["stop_reason"] = resp.get("stop_reason")
    rec["n_tool_use"] = len(uses)
    rec["block_types"] = [b.get("type") for b in blocks]
    if not uses:
        rec.update(ok=False, error="no tool_use block")
        return rec, resp
    use = uses[0]
    rec["called_name"] = use.get("name")
    inp = use.get("input")
    rec["input_is_object"] = isinstance(inp, dict)
    rec["input_keys"] = sorted(inp.keys()) if isinstance(inp, dict) else None
    required = (tool.get("input_schema") or {}).get("required", [])
    if isinstance(inp, dict):
        rec["required_missing"] = [k for k in required if k not in inp]
    rec["ok"] = use.get("name") == tool["name"] and isinstance(inp, dict)
    return rec, resp


def cc_phase2(tool, phase1_resp):
    """Echo assistant turn verbatim + tool_result; expect clean continuation."""
    use = next(b for b in phase1_resp["content"] if b.get("type") == "tool_use")
    result_text = json.dumps({"status": "ok", "tool": tool["name"]})
    body = {
        "model": "claude-opus-4-6",
        "max_tokens": 4096,
        "tools": json.load(open(HERE + "/cc-tools.json")),
        "messages": [
            {"role": "user", "content": "Do the task now."},
            {"role": "assistant", "content": phase1_resp["content"]},
            {"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": use["id"],
                 "content": result_text},
            ]},
        ],
    }
    status, resp, err = post("/v1/messages", body)
    rec = {"suite": "cc", "tool": tool["name"], "phase": "result", "http": status}
    if status != 200:
        rec.update(ok=False, error=str(resp)[:400], kind=err)
        return rec
    rec["stop_reason"] = resp.get("stop_reason")
    rec["block_types"] = [b.get("type") for b in resp.get("content", [])]
    text = "".join(b.get("text", "") for b in resp["content"] if b.get("type") == "text")
    rec["text_len"] = len(text)
    rec["ok"] = resp.get("stop_reason") in ("end_turn", "tool_use", "stop_sequence")
    return rec


def run_cc(only, workers):
    all_tools = json.load(open(HERE + "/cc-tools.json"))
    targets = [t for t in all_tools if not only or t["name"] in only]

    def one(tool):
        rec1, resp = cc_phase1(tool, all_tools)
        emit(rec1)
        if resp is None or not rec1.get("ok"):
            emit({"suite": "cc", "tool": tool["name"], "phase": "result",
                  "ok": False, "error": "skipped: phase1 failed"})
            return
        emit(cc_phase2(tool, resp))

    with ThreadPoolExecutor(max_workers=workers) as ex:
        list(ex.map(one, targets))


# ---------------- Codex / OpenAI Responses ----------------

def codex_input_user(text):
    return {"type": "message", "role": "user",
            "content": [{"type": "input_text", "text": text}]}


def codex_phase1(entry, all_tools):
    """entry: the tool dict as codex declared it. Forced via function name."""
    name = entry.get("name")
    if entry.get("type") == "custom":
        # {"type":"custom"} tool_choice is dropped by the adapter; the wire
        # tool is a function under the same name, so named-force works.
        choice = {"type": "function", "name": name}
    else:
        choice = {"type": "function", "name": name}
    body = {
        "model": "swe-2-max",
        "instructions": "You are Codex, a coding assistant.",
        "input": [codex_input_user("Do the task now.")],
        "tools": all_tools,
        "tool_choice": choice,
        "stream": False,
    }
    status, resp, err = post("/v1/responses", body)
    rec = {"suite": "codex", "tool": name or entry.get("type"),
           "phase": "call", "http": status}
    if status != 200:
        rec.update(ok=False, error=str(resp)[:400], kind=err)
        return rec, None
    out = resp.get("output", [])
    rec["output_types"] = [i.get("type") for i in out]
    calls = [i for i in out if i.get("type") in ("function_call", "custom_tool_call")]
    rec["n_calls"] = len(calls)
    if not calls:
        rec.update(ok=False, error="no call item in output",
                   stop_reason=resp.get("status"))
        return rec, resp
    call = calls[0]
    rec["called_name"] = call.get("name")
    rec["call_type"] = call.get("type")
    args = call.get("arguments")
    if args is not None:
        try:
            parsed = json.loads(args)
            rec["args_is_object"] = isinstance(parsed, dict)
            rec["args_keys"] = sorted(parsed.keys()) if isinstance(parsed, dict) else None
        except json.JSONDecodeError:
            rec["args_is_object"] = False
            rec["args_raw"] = args[:200]
    else:
        rec["input_preview"] = str(call.get("input"))[:200]
    rec["ok"] = call.get("name") == name
    return rec, resp


def codex_phase2(entry, all_tools, phase1_resp):
    """Replay prior output items + matching tool output item."""
    out = phase1_resp["output"]
    call = next(i for i in out if i.get("type") in ("function_call", "custom_tool_call"))
    call_id = call.get("call_id") or call.get("id")
    if call.get("type") == "custom_tool_call":
        output_item = {"type": "custom_tool_call_output", "call_id": call_id,
                       "output": "patch applied"}
    else:
        output_item = {"type": "function_call_output", "call_id": call_id,
                       "output": json.dumps({"status": "ok"})}
    replay_items = []
    for item in out:
        if item.get("type") in ("function_call", "custom_tool_call", "reasoning", "message"):
            replay_items.append(item)
    body = {
        "model": "swe-2-max",
        "instructions": "You are Codex, a coding assistant.",
        "input": [codex_input_user("Do the task now.")] + replay_items + [output_item],
        "tools": all_tools,
        "stream": False,
    }
    status, resp, err = post("/v1/responses", body)
    rec = {"suite": "codex", "tool": entry.get("name") or entry.get("type"),
           "phase": "result", "http": status}
    if status != 200:
        rec.update(ok=False, error=str(resp)[:400], kind=err)
        return rec
    rec["status_field"] = resp.get("status")
    rec["output_types"] = [i.get("type") for i in resp.get("output", [])]
    rec["ok"] = resp.get("status") in ("completed", "incomplete")
    return rec


def run_codex(only, workers):
    all_tools = json.load(open(HERE + "/codex-tools.json"))
    targets = []
    for t in all_tools:
        if t.get("type") in ("function", "custom") and t.get("name"):
            targets.append(t)
        elif t.get("type") == "namespace":
            for sub in t.get("tools", []):
                targets.append({"type": "namespace-sub", "name": sub["name"],
                                "parent": t["name"]})
        elif t.get("type"):
            targets.append({"type": t["type"], "name": None})
    if only:
        targets = [t for t in targets if (t.get("name") or t.get("type")) in only]

    def one(entry):
        name = entry.get("name")
        if entry["type"] == "namespace-sub":
            # Sub-tools are dropped with the namespace: named-forcing a name
            # upstream never saw must surface as an error - that is the check.
            body = {
                "model": "swe-2-max",
                "instructions": "You are Codex.",
                "input": [codex_input_user("hi")],
                "tools": all_tools,
                "tool_choice": {"type": "function", "name": name},
                "stream": False,
            }
            status, resp, err = post("/v1/responses", body)
            emit({"suite": "codex", "tool": f"{entry['parent']}.{name}",
                  "phase": "call", "http": status,
                  "ok": status == 200 and any(
                      i.get("type") in ("function_call", "custom_tool_call")
                      for i in (resp.get("output") or [])),
                  "error": None if status == 200 else str(resp)[:400]})
            return
        if entry["type"] == "web_search":
            # Hosted tool: no name to force. Induce by prompt and see whether
            # any web_search_call output item appears.
            body = {
                "model": "swe-2-max",
                "instructions": "You are Codex.",
                "input": [codex_input_user(
                    "Use the web_search tool to look up today's date.")],
                "tools": all_tools,
                "tool_choice": "required",
                "stream": False,
            }
            status, resp, err = post("/v1/responses", body)
            types = [i.get("type") for i in (resp.get("output") or [])] if status == 200 else []
            emit({"suite": "codex", "tool": "web_search", "phase": "call",
                  "http": status, "ok": "web_search_call" in types,
                  "output_types": types,
                  "error": None if status == 200 else str(resp)[:400]})
            return
        rec1, resp = codex_phase1(entry, all_tools)
        emit(rec1)
        if resp is None or not rec1.get("ok"):
            emit({"suite": "codex", "tool": name, "phase": "result",
                  "ok": False, "error": "skipped: phase1 failed"})
            return
        emit(codex_phase2(entry, all_tools, resp))

    with ThreadPoolExecutor(max_workers=workers) as ex:
        list(ex.map(one, targets))


if __name__ == "__main__":
    suite = sys.argv[1]
    only = None
    workers = 3
    args = sys.argv[2:]
    if "--only" in args:
        only = set(args[args.index("--only") + 1].split(","))
    if "--workers" in args:
        workers = int(args[args.index("--workers") + 1])
    OUT = open(f"{HERE}/results-{suite}.jsonl", "a")
    if suite == "cc":
        run_cc(only, workers)
    elif suite == "codex":
        run_codex(only, workers)
    OUT.close()
