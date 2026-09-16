#!/usr/bin/env python3
"""cache-probe.py — 上游 prompt cache 受控实验骨架。

把「14 个 warmtest 脚本共享的同一副骨架」参数化：每个 arm 一条独立
缓存 lineage（唯一 user_id + padded system prompt），事件按绝对偏移
时刻表调度进线程池（修掉了 ladder.py 顺序阻塞把后续事件全部压后的
bug），逐条 emit JSONL 落盘——长跑实验被 OOM 杀也不丢已采数据。

用法:
  cache-probe.py --plan plan.json --out results.jsonl
  cache-probe.py --keepalive 3 --ping-interval 180 --horizon 2520 --out r.jsonl

plan.json 形态（两种模式共用一套调度器）：
  {"arms": [{"name": "A1", "pad_reps": 90}, ...],
   "events": [{"t": 0, "arm": "A1", "op": "seed"},
              {"t": 180, "arm": "A1", "op": "ping"},
              {"t": 900, "arm": "A1", "op": "probe"}, ...]}
  op: seed=首发(建立前缀)  ping=逐字重放(mt=1, 续期)  probe=追加一轮(mt=8, 验证命中)

--keepalive N 是常用形态的快捷方式：N 条臂 seed 后每 interval verbatim
ping，horizon 末尾做一次 probe，另加一条只 seed 不 ping 的对照臂。

环境变量: CACHE_PROBE_BASE（默认 http://127.0.0.1:3033/v1/messages）、
CACHE_PROBE_KEY（默认 ./config.yaml 的 auth.api_key）、CACHE_PROBE_MODEL
（默认 swe-2-max）。
"""

import json, os, sys, time, urllib.request, datetime as dt
from concurrent.futures import ThreadPoolExecutor

BASE = os.environ.get("CACHE_PROBE_BASE", "http://127.0.0.1:3033/v1/messages")
MODEL = os.environ.get("CACHE_PROBE_MODEL", "swe-2-max")
PAD = ("lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod "
       "tempor incididunt ut labore et dolore magna aliqua ")


def _default_key():
    try:
        for line in open("./config.yaml"):
            s = line.strip()
            if s.startswith("api_key:"):
                return s.split(":", 1)[1].strip().strip("'\"")
    except OSError:
        pass
    return ""


KEY = os.environ.get("CACHE_PROBE_KEY", _default_key())


class Arm:
    """一条缓存 lineage：seed 建前缀，ping 逐字重放续期，probe 追加验证。"""

    def __init__(self, name, pad_reps=90, run=""):
        self.name = name
        self.uid = f"cacheprobe-{run}-{name}"
        self.system = (f"Cache probe harness arm {name}. Marker: {run}-{name}. "
                       + PAD * pad_reps)
        self.msgs = [{"role": "user", "content": f"Arm {name} hello. Reply exactly: OK"}]
        self.last_reply = "OK"
        self.probes = 0


def make_emit(out_path, t0):
    def emit(rec):
        rec["t"] = round(time.time() - t0, 1)
        line = json.dumps(rec, ensure_ascii=False)
        with open(out_path, "a") as f:
            f.write(line + "\n")
        print(dt.datetime.now().strftime("%H:%M:%S"), line, flush=True)
    return emit


def send(arm, tag, msgs, mt, emit):
    body = {"model": MODEL, "max_tokens": mt, "system": arm.system,
            "messages": msgs, "metadata": {"user_id": arm.uid}}
    req = urllib.request.Request(
        BASE, data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {KEY}",
                 "Content-Type": "application/json",
                 "anthropic-version": "2023-06-01"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=300) as r:
            resp = json.loads(r.read())
        u = resp.get("usage", {})
        text = "".join(b.get("text", "") for b in resp.get("content", [])
                       if isinstance(b, dict))
        emit({"arm": arm.name, "tag": tag, "status": 200,
              "in": u.get("input_tokens"),
              "cr": u.get("cache_read_input_tokens"),
              "cw": u.get("cache_creation_input_tokens"),
              "out": u.get("output_tokens"),
              "stop": resp.get("stop_reason"),
              "ms": round((time.time() - t0) * 1000)})
        return text or "OK"
    except Exception as ex:
        emit({"arm": arm.name, "tag": tag, "status": "err",
              "err": str(ex)[:200], "ms": round((time.time() - t0) * 1000)})
        return arm.last_reply


def run_event(arm, op, emit):
    if op == "seed":
        arm.last_reply = send(arm, "seed", arm.msgs, 8, emit)
    elif op == "ping":
        send(arm, "ping", arm.msgs, 1, emit)
    elif op == "probe":
        arm.probes += 1
        ext = (arm.msgs + [{"role": "assistant", "content": arm.last_reply},
                           {"role": "user", "content": f"probe {arm.probes}: again, reply OK"}])
        arm.last_reply = send(arm, f"probe{arm.probes}", ext, 8, emit)
        arm.msgs = ext
    else:
        raise ValueError(f"unknown op {op}")


def keepalive_plan(n_arms, interval, horizon):
    """N 臂保温 + 1 对照臂：seed 后按 interval 逐字 ping，horizon 处 probe。"""
    arms = ([{"name": f"A{i+1}", "pad_reps": 90} for i in range(n_arms)]
            + [{"name": "B", "pad_reps": 90}])
    events = [{"t": 0, "arm": a["name"], "op": "seed"} for a in arms]
    t = interval
    while t < horizon:
        for a in arms[:-1]:
            events.append({"t": t, "arm": a["name"], "op": "ping"})
        t += interval
    for a in arms:
        events.append({"t": horizon, "arm": a["name"], "op": "probe"})
    return {"arms": arms, "events": events}


def main():
    import argparse
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", help="plan.json 路径")
    ap.add_argument("--out", required=True, help="结果 JSONL 路径")
    ap.add_argument("--keepalive", type=int, metavar="N",
                    help="快捷模式：N 条保温臂 + 1 对照臂")
    ap.add_argument("--ping-interval", type=int, default=180)
    ap.add_argument("--horizon", type=int, default=2520)
    ap.add_argument("--workers", type=int, default=8)
    args = ap.parse_args()

    if args.keepalive:
        plan = keepalive_plan(args.keepalive, args.ping_interval, args.horizon)
    elif args.plan:
        plan = json.load(open(args.plan))
    else:
        ap.error("--plan 或 --keepalive 必须给一个")

    run = str(int(time.time()))
    arms = {a["name"]: Arm(a["name"], a.get("pad_reps", 90), run)
            for a in plan["arms"]}
    events = sorted(plan["events"], key=lambda e: e["t"])

    t0 = time.time()
    emit = make_emit(args.out, t0)
    emit({"tag": "run", "run": run, "model": MODEL, "base": BASE,
          "arms": len(arms), "events": len(events)})

    pool = ThreadPoolExecutor(max_workers=args.workers)
    for e in events:
        delay = e["t"] - (time.time() - t0)
        if delay > 0:
            time.sleep(delay)
        pool.submit(run_event, arms[e["arm"]], e["op"], emit)
    pool.shutdown(wait=True)
    emit({"tag": "done"})


if __name__ == "__main__":
    main()
