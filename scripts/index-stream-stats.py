#!/usr/bin/env python3
"""logs 表 × debug payload 取证：同流静默间隔 → 上游缓存命中率画像。

扫 devin-2api.db：debug_files 的 01 body 派生 sid/psid（CC user_id
JSON）、sh=sha256(sys[:4K]|firstmsg[:1K]) 流键、sub 标记（system 头部
cc_is_subagent）；debug_chunks 的 04 按 seq 拼接重组 deltaToolCalls
（name 帧后跟 argumentsJson 分块）；logs 表供索引行。按 (sid,sh) 聚流，
以前一响应的 pending 工具分类 gap（agentwait/userq/bookkeep/toolwait/
turnend）；输出 gap→hit% 分桶、miss 三分支归因（structural-shrink /
decay>300s / short-gap）、大前缀 warm/cold first_upstream_ms 分位、
15min 窗峰值并发流。

用法: index-stream-stats.py [--db PATH]
  --db  默认按平台探测：darwin → ~/Library/Application Support/
        devin-2api/devin-2api.db，其他 → ~/.local/state/devin-2api/
        devin-2api.db；WAL 下只读连接不干扰运行实例

口径陷阱：命中率只在 result=="completed" && input+cache_read>0 的行上
有意义——rate_gate 快败/断连的 0-token 行会被误算成 miss 污染统计。
上游 cache_creation 恒 0，判活只能看 cache_read。
"""

import json, os, hashlib, datetime, collections, statistics, sys, bisect
import argparse, sqlite3, gzip


def default_db():
    if sys.platform == "darwin":
        return os.path.expanduser("~/Library/Application Support/devin-2api/devin-2api.db")
    return os.path.expanduser("~/.local/state/devin-2api/devin-2api.db")


def decode(raw):
    """debug_* 表的 BLOB 按 gzip 魔数判帧（EncodePayload 透明压缩）。"""
    if isinstance(raw, (bytes, bytearray)) and raw[:2] == b"\x1f\x8b":
        raw = gzip.decompress(raw)
    return raw


def ts_iso(s):
    return datetime.datetime.fromisoformat(s).timestamp()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--db", default=default_db())
    args = ap.parse_args()
    try:
        db = sqlite3.connect("file:%s?mode=ro" % args.db, uri=True)
    except sqlite3.OperationalError as e:
        print("index-stream-stats: %s（db=%s）" % (e, args.db), file=sys.stderr)
        sys.exit(2)

    # 04 流式 JSONL 在 debug_chunks 按 seq 分片（逐片 EncodePayload）——
    # 全量捞出按 dir 拼回单文本（解码成 str），与文件时代读整文件等价。
    chunks4 = {}
    for d, blob in db.execute(
            "SELECT dir, data FROM debug_chunks "
            "WHERE name='04-devin-response.jsonl' ORDER BY dir, seq"):
        chunks4[d] = chunks4.get(d, "") + decode(blob).decode("utf-8", "replace")

    # ---------- pass 1: per-request profile ----------
    reqs = {}
    for d, content in db.execute(
            "SELECT dir, content FROM debug_files "
            "WHERE name='01-http-request.json'"):
        try:
            b = json.loads(decode(content))["body"]
        except Exception:
            continue
        uid = b.get("metadata", {}).get("user_id", "")
        sid = psid = ""
        uid_ok = False
        if uid.startswith("{"):
            try:
                u = json.loads(uid)
                sid = u.get("session_id", "")
                psid = u.get("parent_session_id", "")
                uid_ok = True
            except Exception:
                pass
        sys_ = b.get("system")
        st = sys_ if isinstance(sys_, str) else "".join(
            x.get("text", "") if isinstance(x, dict) else "" for x in (sys_ or []))
        msgs = b.get("messages", [])
        first_txt = ""
        for m in msgs:
            c = m.get("content")
            t = c if isinstance(c, str) else "".join(
                b2.get("text", "") for b2 in (c or []) if isinstance(b2, dict))
            if t:
                first_txt = t
                break
        sh = hashlib.sha256((st[:4096] + "|" + first_txt[:1024]).encode()).hexdigest()[:10]
        syshash = hashlib.sha256(st[:4096].encode()).hexdigest()[:10]
        msghash = hashlib.sha256(first_txt[:1024].encode()).hexdigest()[:10]
        alltxt = json.dumps(msgs)
        markers = set()
        if "[Request interrupted by user" in alltxt:
            markers.add("INTERRUPT")
        if "<task-notification" in alltxt:
            markers.add("TASK_NOTIF")
        if "doesn't want to" in alltxt:
            markers.add("REJECT")
        if "continued from a previous" in alltxt or "compact_boundary" in alltxt:
            markers.add("COMPACT_REF")
        # tool calls + reassembled args (name frame followed by argumentsJson chunks)
        tools = []
        callargs = {}
        if d in chunks4:
            try:
                cur = None
                for line in chunks4[d].splitlines():
                    if '"deltaToolCalls"' not in line:
                        continue
                    for tc in json.loads(line).get("deltaToolCalls", []):
                        if tc.get("name"):
                            tools.append(tc["name"])
                            cur = tc["name"]
                            callargs.setdefault(cur, "")
                        elif tc.get("argumentsJson") and cur:
                            callargs[cur] += tc["argumentsJson"]
            except Exception:
                pass
        reqs[d] = dict(sid=sid, psid=psid, uid_ok=uid_ok, has_uid=bool(uid),
                       sh=sh, syshash=syshash, msghash=msghash,
                       sub="cc_is_subagent=true" in st[:300],
                       nmsg=len(msgs), tools=tools, callargs=callargs,
                       markers=markers, model=b.get("model", ""))

    # ---------- logs 表 join（原 index.jsonl 的继任） ----------
    rows = []
    for r in db.execute(
            "SELECT dir, started_at, api, path, input_tokens, cache_read_tokens,"
            " first_upstream_ms, duration_ms, result FROM logs"):
        d = r[0]
        if d not in reqs:
            continue
        prof = reqs[d]
        rows.append(dict(d=d, t=ts_iso(r[1]), api=r[2] or "",
                         path=r[3] or "",
                         inp=r[4] or 0,
                         cr=r[5] or 0,
                         fu=r[6] or 0,
                         dur=r[7] or 0,
                         result=r[8] or "",
                         **{k: prof[k] for k in ("sid", "psid", "uid_ok", "has_uid",
                            "sh", "syshash", "msghash", "sub", "nmsg", "tools",
                            "callargs", "markers")}))

    print("== window ==")
    if rows:
        ts0, ts1 = min(r["t"] for r in rows), max(r["t"] for r in rows)
        print("profiled requests:", len(rows), " span_min:", round((ts1 - ts0) / 60, 1),
              " from", datetime.datetime.fromtimestamp(ts0), "to",
              datetime.datetime.fromtimestamp(ts1))
    print("api:", collections.Counter(r["api"] or r["path"] for r in rows).most_common())
    anth = [r for r in rows if r["api"] == "anthropic"]
    print("anthropic n=%d  uid_ok=%d (%.0f%%)  has_uid_any=%d" %
          (len(anth), sum(r["uid_ok"] for r in anth),
           100 * sum(r["uid_ok"] for r in anth) / max(1, len(anth)),
           sum(r["has_uid"] for r in anth)))
    print("sub-flagged:", sum(r["sub"] for r in rows))
    psid_n = sum(1 for r in rows if r["psid"])
    print("psid present:", psid_n, " psid==sid:",
          sum(1 for r in rows if r["psid"] and r["psid"] == r["sid"]))
    print("markers:", collections.Counter(m for r in rows for m in r["markers"]))
    toolset = collections.Counter()
    lasttool = collections.Counter()
    for r in rows:
        for t in set(r["tools"]):
            toolset[t] += 1
        if r["tools"]:
            lasttool[r["tools"][-1]] += 1
    print("tool union counts:", toolset.most_common(25))
    print("last-tool counts:", lasttool.most_common(25))

    # run_in_background detection for Agent calls
    def agent_bg(r):
        for name, args_ in r["callargs"].items():
            if name in ("Agent", "Task") and '"run_in_background"' in args_ and \
               "true" in args_.split('"run_in_background"')[1][:20]:
                return True
        return False

    n_agent = sum(1 for r in rows if "Agent" in r["tools"])
    n_agent_bg = sum(1 for r in rows if "Agent" in r["tools"] and agent_bg(r))
    print('responses containing "Agent" call:', n_agent, " of which run_in_background:",
          n_agent_bg)

    # ---------- streams & pairs ----------
    streams = collections.defaultdict(list)
    for r in rows:
        if r["sid"]:
            streams[(r["sid"], r["sh"])].append(r)
    for k in streams:
        streams[k].sort(key=lambda x: x["t"])

    AGENTBLOCK = {"Agent", "Task", "Workflow", "wait_agent"}
    USERPACE = {"AskUserQuestion", "ExitPlanMode", "request_user_input"}
    BOOKKEEP = {"SendMessage", "TaskCreate", "TaskUpdate", "TaskList", "TaskGet",
                "TaskStop", "TaskOutput", "NotebookEdit"}

    def cls(tools):
        s = set(tools)
        if s & USERPACE:
            return "userq"
        if s & AGENTBLOCK:
            return "agentwait"
        if s & BOOKKEEP:
            return "bookkeep"
        if s:
            return "toolwait"
        return "turnend"

    pairs = []
    for key, rs in streams.items():
        for a, b in zip(rs, rs[1:]):
            pairs.append(dict(key=key, gap=b["t"] - a["t"], a=a, b=b,
                              cls=cls(a["tools"]), last=a["tools"][-1] if a["tools"] else "",
                              hit=b["cr"] > 0,
                              shrink=(b["inp"] + b["cr"]) < 0.6 * (a["inp"] + a["cr"]),
                              sub=a["sub"]))

    def gapstats(ps, label):
        if not ps:
            print("%s: n=0" % label)
            return
        g = sorted(p["gap"] for p in ps)
        n = len(g)
        print("%s: n=%d min=%ds p50=%ds p90=%ds max=%ds >180s:%d >600s:%d hit%%=%d" %
              (label, n, int(g[0]), int(g[n // 2]), int(g[int(n * 0.9)]), int(g[-1]),
               sum(1 for x in g if x > 180), sum(1 for x in g if x > 600),
               100 * sum(p["hit"] for p in ps) / n))

    print("\n== pairs by end-class ==")
    bycls = collections.defaultdict(list)
    for p in pairs:
        bycls[p["cls"]].append(p)
    for c in ("agentwait", "bookkeep", "toolwait", "userq", "turnend"):
        gapstats(bycls[c], c)

    print("\n== sync-Agent pairs (union has Agent/Task/Workflow, not bg) ==")
    agp = [p for p in pairs
           if set(p["a"]["tools"]) & AGENTBLOCK and not agent_bg(p["a"])]
    for p in sorted(agp, key=lambda x: -x["gap"]):
        print("  gap=%6ds %s tools=%s bg=%s -> hit=%s" %
              (int(p["gap"]), p["a"]["d"], p["a"]["tools"], agent_bg(p["a"]), p["hit"]))

    print("\n== SendMessage-follow pairs ==")
    smp = [p for p in pairs if "SendMessage" in p["a"]["tools"]]
    for p in sorted(smp, key=lambda x: -x["gap"])[:20]:
        print("  gap=%6ds %s tools=%s sub=%s -> hit=%s" %
              (int(p["gap"]), p["a"]["d"], p["a"]["tools"][-4:], p["sub"], p["hit"]))
    gapstats(smp, "SendMessage-follow")

    print("\n== Task*-follow pairs (TaskCreate/Update/List/Get/Stop/Output) ==")
    tkp = [p for p in pairs if set(p["a"]["tools"]) &
           {"TaskCreate", "TaskUpdate", "TaskList", "TaskGet", "TaskStop", "TaskOutput"}]
    gapstats(tkp, "Task*-follow")
    for p in sorted(tkp, key=lambda x: -x["gap"])[:10]:
        print("  gap=%6ds tools=%s" % (int(p["gap"]), p["a"]["tools"][-4:]))

    print("\n== userq-follow pairs ==")
    uqp = [p for p in pairs if set(p["a"]["tools"]) & USERPACE]
    for p in sorted(uqp, key=lambda x: -x["gap"]):
        print("  gap=%6ds %s last=%s -> hit=%s" %
              (int(p["gap"]), p["a"]["d"], p["last"], p["hit"]))
    gapstats(uqp, "userq")

    print("\n== turnend pairs gap distribution ==")
    te = bycls["turnend"]
    g = sorted(p["gap"] for p in te)
    if g:
        n = len(g)
        print("n=%d p50=%ds p90=%ds p99=%ds max=%ds" %
              (n, int(g[n // 2]), int(g[int(n * 0.9)]), int(g[min(n - 1, int(n * 0.99))]),
               int(g[-1])))
        for lo, hi in [(0, 60), (60, 180), (180, 300), (300, 480), (480, 600),
                       (600, 1200), (1200, 2700), (2700, 99999)]:
            c = sum(1 for x in g if lo < x <= hi)
            print("  (%4d,%5d]: %d" % (lo, hi, c))
    print("top turnend gaps:", [int(x) for x in g[-10:]])

    print("\n== INTERRUPT-marked next-requests ==")
    intp = [p for p in pairs if "INTERRUPT" in p["b"]["markers"]]
    for p in sorted(intp, key=lambda x: x["gap"]):
        print("  gap=%6ds %s prevcls=%s hit=%s" %
              (int(p["gap"]), p["b"]["d"], p["cls"], p["hit"]))

    print("\n== hit% by gap (same-stream pairs) ==")
    for lo, hi in [(0, 60), (60, 180), (180, 300), (300, 480), (480, 600),
                   (600, 1200), (1200, 99999)]:
        ps = [p for p in pairs if lo < p["gap"] <= hi]
        if ps:
            print("  (%4d,%5d]: n=%d hit%%=%d" %
                  (lo, hi, len(ps), 100 * sum(p["hit"] for p in ps) / len(ps)))

    print("\n== top-15 gaps any pair ==")
    for p in sorted(pairs, key=lambda x: -x["gap"])[:15]:
        print("  gap=%6ds %s(last=%s,sub=%s) -> %s hit=%s in=%d nmsg=%d marks=%s" %
              (int(p["gap"]), p["a"]["d"], p["last"], p["sub"], p["b"]["d"],
               p["hit"], p["b"]["inp"], p["b"]["nmsg"],
               "+".join(sorted(p["b"]["markers"])) or "-"))

    # ---------- streams stats ----------
    print("\n== stream stats ==")
    print("total streams (sid,sh):", len(streams))
    multi = {k: v for k, v in streams.items() if len(v) >= 2}
    ones = {k: v for k, v in streams.items() if len(v) == 1}
    print("promotable (>=2):", len(multi), " one-shot:", len(ones),
          " one-shot sub-flagged:", sum(1 for k in ones if streams[k][0]["sub"]))
    sizes = sorted(max(r["inp"] + r["cr"] for r in v) for v in multi.values())
    if sizes:
        p50 = sizes[len(sizes) // 2]
        p90 = sizes[int(len(sizes) * 0.9)]
        print("retained prefix tok p50=%d p90=%d max=%d  (p90 est %.2f MB)" %
              (p50, p90, sizes[-1], p90 * 4 / 1e6))
        over8k = sum(1 for s in sizes if s >= 8192)
        print("promotable with prefix>=8192:", over8k)
    # peak live in 15min window (event-anchored)
    evs = sorted((r["t"], k) for k, v in streams.items() for r in v)
    stream_ts = {k: sorted(r["t"] for r in v) for k, v in streams.items()}
    best = 0
    for t, _ in evs:
        c = 0
        for k, tl in stream_ts.items():
            i = bisect.bisect_left(tl, t)
            if i < len(tl) and tl[i] <= t + 900:
                c += 1
        best = max(best, c)
    print("peak live streams in 15min:", best)
    print("warming RPM bound (peak/3):", round(best / 3.0, 1))
    # same but only streams with prefix>=8192 and >=2 reqs
    stream_ts2 = {k: v for k, v in stream_ts.items()
                  if len(v) >= 2 and max(r["inp"] + r["cr"] for r in streams[k]) >= 8192}
    best2 = 0
    for t, _ in evs:
        c = 0
        for k, tl in stream_ts2.items():
            i = bisect.bisect_left(tl, t)
            if i < len(tl) and tl[i] <= t + 900:
                c += 1
        best2 = max(best2, c)
    print("peak live promotable+>=8K streams in 15min:", best2,
          " RPM bound:", round(best2 / 3.0, 1))

    # ---------- miss classification ----------
    print("\n== miss classification (cr=0, in>3000) ==")
    miss = [r for r in rows if r["cr"] == 0 and r["inp"] > 3000]
    print("total misses:", len(miss))
    prev_of = {p["b"]["d"]: p for p in pairs}
    cat = collections.Counter()
    for r in miss:
        p = prev_of.get(r["d"])
        if p is None:
            cat["first-in-stream/no-pair"] += 1
        elif p["shrink"]:
            cat["structural-shrink"] += 1
        elif p["gap"] > 300:
            cat["decay>300s"] += 1
        else:
            cat["short-gap<=300s"] += 1
    print(cat.most_common())
    print("decay misses detail:")
    for r in miss:
        p = prev_of.get(r["d"])
        if p and not p["shrink"] and p["gap"] > 300:
            print("  %s gap=%ds in=%d" % (r["d"], int(p["gap"]), r["inp"]))

    # ---------- warm/cold TTFB >20K ----------
    print("\n== first_upstream_ms for >20K prefixes ==")
    big = [r for r in rows if r["inp"] + r["cr"] > 20000]
    warm = sorted(r["fu"] for r in big if r["cr"] > 0)
    cold = sorted(r["fu"] for r in big if r["cr"] == 0)
    if warm:
        print("warm n=%d p50=%d" % (len(warm), warm[len(warm) // 2]))
    if cold:
        print("cold n=%d p50=%d" % (len(cold), cold[len(cold) // 2]))

    # ---------- multi-sh sessions & compact alignment ----------
    print("\n== multi-sh sessions ==")
    bysid = collections.defaultdict(list)
    for r in rows:
        if r["sid"]:
            bysid[r["sid"]].append(r)
    aligned = notaligned = 0
    for sid, lst in bysid.items():
        shs = collections.defaultdict(list)
        for r in lst:
            shs[r["sh"]].append(r)
        if len(shs) <= 1:
            continue
        hc = {sh: any("COMPACT_REF" in x["markers"] for x in v) for sh, v in shs.items()}
        if len(set(hc.values())) > 1:
            aligned += 1
        else:
            notaligned += 1
            subs = {sh: v[0]["sub"] for sh, v in shs.items()}
            syss = {sh: v[0]["syshash"] for sh, v in shs.items()}
            print("  NOT-aligned sid=%s shs=%d subs=%s same_sys=%s" %
                  (sid[:8], len(shs), subs, len(set(syss.values())) == 1))
    print("aligned:", aligned, " not-aligned:", notaligned)

    # per-session-day compaction count
    comp_per = collections.Counter()
    for sid, lst in bysid.items():
        shs = collections.defaultdict(list)
        for r in lst:
            shs[r["sh"]].append(r)
        for sh, v in shs.items():
            if any("COMPACT_REF" in x["markers"] for x in v):
                day = datetime.datetime.fromtimestamp(v[0]["t"]).strftime("%Y-%m-%d")
                comp_per[(sid, day)] += 1
    if comp_per:
        print("max compactions per session-day:", comp_per.most_common(5))

    # ---------- nested Agent dispatch in sub streams ----------
    print("\n== sub-flagged streams calling Agent ==")
    n_nested = 0
    for r in rows:
        if r["sub"] and "Agent" in r["tools"]:
            n_nested += 1
            print("  %s tools=%s" % (r["d"], r["tools"]))
    print("count:", n_nested)

    # ---------- subagent done -> revive gaps ----------
    print("\n== sub-stream done->next gaps ==")
    subpairs = [p for p in pairs if p["sub"] and not p["a"]["tools"]]
    gapstats(subpairs, "sub done->next")
    subsend = [p for p in pairs if p["sub"] and "SendMessage" in p["a"]["tools"]]
    gapstats(subsend, "sub SendMessage->next")
    for p in sorted(subpairs + subsend, key=lambda x: -x["gap"])[:15]:
        print("  gap=%6ds tools=%s -> hit=%s" %
              (int(p["gap"]), p["a"]["tools"][-3:], p["hit"]))


if __name__ == "__main__":
    main()
