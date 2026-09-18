#!/usr/bin/env python3
"""Scan devin-2api.db debug_files 的 01-http-request.json 语料，统计
protocol-drift 形状命中率。

Per-request hit counting — a request counts once if it exhibits the shape.
Counts only structural shapes; never emits message content.

用法: drift-corpus-scan.py [--db PATH] [--since YYYYMMDD] [--until YYYYMMDD]
  --db      默认按平台探测（darwin → ~/Library/Application Support/
            devin-2api/devin-2api.db，其他 → ~/.local/state/devin-2api/
            devin-2api.db）；WAL 下只读连接不干扰运行实例
  --since/--until  dir 名内嵌日期 YYYYMMDD，字典序过滤（默认全开）
  语料为空时 exit 2——空结果多半是 db 路径指错或 payload 已被保留
  策略剥离，静默输出空表易误读为「无漂移」。
"""
import json
import gzip
import collections
import os
import sqlite3
import sys
import argparse


def default_db():
    if sys.platform == "darwin":
        return os.path.expanduser("~/Library/Application Support/devin-2api/devin-2api.db")
    return os.path.expanduser("~/.local/state/devin-2api/devin-2api.db")


def decode(raw):
    """debug_files.content 按 gzip 魔数判帧（EncodePayload 透明压缩）。"""
    if isinstance(raw, (bytes, bytearray)) and raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    return raw


IMAGE_TYPES = {"image", "image_url", "input_image"}
WS_TYPES = {"web_search_tool_result", "server_tool_use"}


def load_body(d):
    b = d.get("body")
    if isinstance(b, str):
        try:
            return json.loads(b)
        except Exception:
            return {}
    return b if isinstance(b, dict) else {}


def walk_blocks(obj):
    if isinstance(obj, dict):
        if isinstance(obj.get("type"), str):
            yield obj
        for v in obj.values():
            yield from walk_blocks(v)
    elif isinstance(obj, list):
        for v in obj:
            yield from walk_blocks(v)


def classify(path, body):
    if "/chat/completions" in path:
        return "chat"
    if "/responses" in path:
        return "responses"
    if "/messages" in path:
        return "anthropic"
    if "input" in body:
        return "responses"
    return "unknown"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=default_db())
    ap.add_argument("--since", default="00000000")
    ap.add_argument("--until", default="99999999")
    args = ap.parse_args()

    try:
        db = sqlite3.connect("file:%s?mode=ro" % args.db, uri=True)
    except sqlite3.OperationalError as e:
        print("drift-corpus-scan: %s（db=%s）" % (e, args.db), file=sys.stderr)
        sys.exit(2)

    proto_totals = collections.Counter()
    P = collections.defaultdict(collections.Counter)

    n_scanned = 0
    for d, content in db.execute(
            "SELECT dir, content FROM debug_files "
            "WHERE name='01-http-request.json' AND dir>=? AND dir<? "
            "ORDER BY dir", (args.since, args.until + "~")):
        try:
            req = json.loads(decode(content))
        except Exception:
            continue
        n_scanned += 1
        body = load_body(req)
        proto = classify(req.get("path", ""), body)
        proto_totals[proto] += 1
        c = P[proto]

        if body.get("stream") is True:
            c["stream_true"] += 1

        msgs = body.get("messages") or []
        if isinstance(msgs, list) and msgs:
            sysdev_first = sysdev_mid = asst_img = user_img = False
            for i, m in enumerate(msgs):
                if not isinstance(m, dict):
                    continue
                role = m.get("role")
                if role in ("system", "developer"):
                    if i == 0:
                        sysdev_first = True
                    else:
                        sysdev_mid = True
                if role == "assistant" and not asst_img:
                    if any(b.get("type") in IMAGE_TYPES for b in walk_blocks(m.get("content"))):
                        asst_img = True
                if role == "user" and not user_img:
                    if any(b.get("type") in IMAGE_TYPES for b in walk_blocks(m.get("content"))):
                        user_img = True
            if sysdev_first:
                c["sysdev_at_pos0"] += 1
            if sysdev_mid:
                c["sysdev_midstream"] += 1
            if asst_img:
                c["assistant_image"] += 1
            if user_img:
                c["user_image"] += 1
            if proto == "anthropic":
                if any(blk.get("type") in WS_TYPES for m in msgs if isinstance(m, dict)
                       for blk in walk_blocks(m)):
                    c["web_search_or_server_tool"] += 1

        if proto == "responses":
            saw = False
            for it in (body.get("input") or []):
                if isinstance(it, dict):
                    if it.get("type") == "reasoning" or "encrypted_content" in json.dumps(it):
                        saw = True
            if any("encrypted" in str(x) for x in (body.get("include") or [])):
                saw = True
            if saw:
                c["reasoning_or_encrypted"] += 1

        tc = body.get("tool_choice")
        if tc is not None:
            c["tc_present"] += 1
            if isinstance(tc, str):
                c["tc_str_" + tc] += 1
            elif isinstance(tc, dict):
                c["tc_dict_" + str(tc.get("type"))] += 1

        if "seed" in body:
            c["seed_present"] += 1
            if isinstance(body["seed"], (int, float)) and body["seed"] < 0:
                c["seed_negative"] += 1

        if "n" in body:
            c["n_present"] += 1
            nv = body["n"]
            if isinstance(nv, (int, float)) and (nv > 1 or nv <= 0):
                c["n_out_of_range"] += 1

        mt_keys = {"chat": ("max_completion_tokens", "max_tokens"),
                   "responses": ("max_output_tokens",),
                   "anthropic": ("max_tokens",)}.get(proto, ())
        vals = [body[k] for k in mt_keys if body.get(k) is not None]
        if not vals:
            c["maxtok_missing"] += 1
        elif any(isinstance(v, (int, float)) and v <= 0 for v in vals):
            c["maxtok_le0"] += 1

        if "parallel_tool_calls" in body:
            c["ptc_present"] += 1
            if body["parallel_tool_calls"] is False:
                c["ptc_false"] += 1

    if n_scanned == 0:
        print("drift-corpus-scan: 语料为空（db=%s since=%s until=%s）——"
              "检查 db 路径或 payload 保留策略" %
              (args.db, args.since, args.until), file=sys.stderr)
        sys.exit(2)

    print("corpus:", dict(proto_totals))
    for p in ("anthropic", "chat", "responses", "unknown"):
        tot = proto_totals.get(p, 0)
        if not tot:
            continue
        print(f"--- {p} (n={tot}) ---")
        for k, v in sorted(P[p].items()):
            print(f"  {k}: {v}/{tot} = {100*v/tot:.2f}%")


if __name__ == "__main__":
    main()
