#!/usr/bin/env python3
"""从配额采样 + 请求索引反推 Devin 日/周额度的美元大小。

数据链路：poll.sh 采的 GetUserStatus 快照（整数百分比，含入账延迟）为因变量，
index.jsonl 里每个付费请求按目录价折算 est$（含 cache_write，按 input 价），
在每个重置窗口内对「累计 est$ vs 已消耗百分点」做最小二乘——斜率的倒数即
该窗口 1% 对应的美元额。2026-09-13 两窗口拟合见 docs/quota-billing.md（原始记录 notes/archive/2026-09-13-quota-billing-fit.md）。

依赖: uv run --with numpy --with matplotlib scripts/quota/fit.py \
        --status quota-probe.jsonl --index index.jsonl --catalog models.json [--out fit.png]
catalog 参数也可以是面板地址，如 http://localhost:3003/admin/model-registry
（该端点需 -H 'Authorization: Bearer <dashboard.password>'，用 --key 传入）。
"""
import argparse
import datetime
import json
import urllib.request

import numpy as np


def parse_ts(s):
    """index.jsonl 的 started_at 是本地时区 ISO 串，转 unix 秒。"""
    try:
        return datetime.datetime.fromisoformat(s.replace("Z", "+00:00")).timestamp()
    except ValueError:
        return 0.0


def load_catalog(src, key):
    """读模型目录；支持本地 JSON 或面板 /admin/model-registry URL。"""
    if src.startswith("http"):
        req = urllib.request.Request(src)
        if key:
            req.add_header("Authorization", f"Bearer {key}")
        data = json.load(urllib.request.urlopen(req))
    else:
        data = json.load(open(src))
    models = data["models"] if isinstance(data, dict) else data
    if isinstance(models, dict):
        return models
    # /admin/model-registry 行把目录挂在 catalog 子字段（别名/纯注册表行
    # catalog 为 null，天然滤掉）；平面 [{uid,...}] 快照原样兼容。
    if models and isinstance(models[0], dict) and "catalog" in models[0]:
        return {r["catalog"]["uid"]: r["catalog"] for r in models if r.get("catalog")}
    return {m["uid"]: m for m in models}


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--status", required=True, help="poll.sh 产出的配额 JSONL")
    ap.add_argument("--index", required=True, help="logs/index.jsonl")
    ap.add_argument("--catalog", required=True, help="模型目录 JSON 或 /admin/model-registry URL")
    ap.add_argument("--key", default="", help="面板密码 dashboard.password（catalog 为 URL 时用）")
    ap.add_argument("--out", default="", help="输出 PNG 路径（给了才画图）")
    ap.add_argument("--lag", type=int, default=60, help="入账延迟秒数（燃烧前移量）")
    args = ap.parse_args()

    cat = load_catalog(args.catalog, args.key)

    calls = []
    for line in open(args.index):
        e = json.loads(line)
        t = parse_ts(e.get("started_at", ""))
        m = cat.get(e.get("model"), {})
        # 无价格维的模型（cost_tier=free 如 swe-2-max）按 $0 计，天然不进账单。
        # 快照里 free 模型的 price_input 落为 null，须按数值判断而非键存在。
        if t <= 0 or not isinstance(m.get("price_input"), (int, float)):
            continue
        p_in, p_cr, p_out = m["price_input"], m.get("price_cached") or 0, m["price_output"]
        usd = (
            (e.get("input_tokens", 0) + e.get("cache_write_tokens", 0)) * p_in
            + e.get("cache_read_tokens", 0) * p_cr
            + e.get("output_tokens", 0) * p_out
        ) / 1e6
        calls.append((t, usd, e.get("model")))
    calls.sort()

    samples = [json.loads(l) for l in open(args.status)]
    T = np.array([s["t"] for s in samples])
    daily = np.array([s["daily"] if s["daily"] is not None else np.nan for s in samples])
    weekly = np.array([float(s["weekly"]) for s in samples])

    cum = np.zeros(len(T))
    for t, usd, _ in calls:
        cum += usd * (t <= T - args.lag)

    # 窗口边界：daily 回升即重置；字段归零后为 null，重新出现时也算新窗口。
    bounds = [0]
    for i in range(1, len(T)):
        prev, cur = daily[i - 1], daily[i]
        if not np.isnan(cur) and (np.isnan(prev) or cur > prev):
            bounds.append(i)
    bounds.append(len(T))

    fig = None
    if args.out:
        import matplotlib

        matplotlib.use("Agg")
        import matplotlib.dates as mdates
        import matplotlib.pyplot as plt

        fig, ax = plt.subplots(figsize=(13, 6))
        tt = [datetime.datetime.fromtimestamp(x) for x in T]
        ax.step(tt, daily, where="post", lw=1.5, label="daily remaining %")
        ax.step(tt, weekly, where="post", lw=1.5, label="weekly remaining %")
        ax.set_ylabel("quota remaining %")
        ax.grid(alpha=0.3)
        ax.xaxis.set_major_formatter(mdates.DateFormatter("%m-%d %H:%M"))

    print(f"{'window':<22}{'channel':<8}{'pts/$':>8}{'quota$':>9}{'R2':>7}")
    for w in range(len(bounds) - 1):
        lo, hi = bounds[w], bounds[w + 1]
        for name, yv, censor in (("daily", daily, True), ("weekly", weekly, False)):
            ok = ~np.isnan(yv[lo:hi])
            if censor:  # daily 归零后字段变 null/0，截断样本会拉歪斜率
                ok &= yv[lo:hi] > 0
            jj = np.arange(lo, hi)[ok]
            if len(jj) < 3:
                continue
            cc = cum[jj] - cum[jj[0]]
            y = yv[jj]
            drop = y[0] - y
            A = np.column_stack([np.ones(len(jj)), cc])
            coef, _, _, _ = np.linalg.lstsq(A, drop, rcond=None)
            if coef[1] <= 0:
                continue
            pred = A @ coef
            r2 = 1 - float(((drop - pred) ** 2).sum()) / (float(((drop - drop.mean()) ** 2).sum()) + 1e-9)
            t0 = datetime.datetime.fromtimestamp(T[lo]).strftime("%m-%d %H:%M")
            print(f"{t0 + ' 起':<22}{name:<8}{coef[1]:>8.3f}{100 / coef[1]:>9.2f}{r2:>7.3f}")
            if fig:
                ax.plot([tt[i] for i in jj], y[0] - pred, ls="--", lw=1, alpha=0.7)
    if fig:
        ax.legend(fontsize=8)
        fig.tight_layout()
        fig.savefig(args.out, dpi=140)
        print(f"plot -> {args.out}")


if __name__ == "__main__":
    main()
