"""生成 docs/perf.md 引用的性能基线图，产物落 docs/images/perf-*.png。

用法：uv run --with matplotlib scripts/perf-charts.py
数据是 2026-09-14 基线快照（outputs/perf/first/，本机桩 interval=0、8 并发）的
硬编码抄录——图的意义是定格那次测量，重测后改这里的数据再跑一遍即可。
样式复用 ~/.claude/skills/plot-skill 的 preset 体系；该 skill 不在时脚本不可用，
但已提交的 PNG 不受影响。
"""

from __future__ import annotations

import sys
from pathlib import Path

import matplotlib.pyplot as plt
import numpy as np

SKILL_SCRIPTS = Path.home() / ".claude/skills/plot-skill/scripts"
sys.path.insert(0, str(SKILL_SCRIPTS))

from style_presets import (  # noqa: E402
    NEUTRALS,
    apply_frame,
    apply_grid,
    apply_rc,
    series_colors,
)

OUT_DIR = Path(__file__).resolve().parents[1] / "docs/images"

# 延迟分解：outputs/perf/first/latency-segments.txt（avg / p99，ms）
SEGMENTS = ["decode", "transform", "connect", "upstream_ttft", "egress"]
SEG_AVG = [3.1, 0.0, 0.2, 0.1, 0.1]
SEG_P99 = [11, 1, 2, 1, 1]

# CPU flat 构成：outputs/perf/first/cpu.pb.gz 的 pprof -top 归类（%）；
# json/应用代码 flat 仅 ~0.5%，并入「其他」避免小扇区标签互相叠压。
CPU_SHARE = {
    "syscall（socket/文件写）": 68.9,
    "runtime 调度与停放": 12.4,
    "netpoller kevent": 5.2,
    "GC 与内存管理": 3.0,
    "其他": 10.5,
}

# 持续压测分位数：outputs/perf/first/load-sustained.txt（28350 请求 / 36s / 787rps）
PERCENTILES = ["p50", "p90", "p99"]
TTFB_MS = [3.7, 7.7, 13.1]
TOTAL_MS = [9.3, 15.5, 25.6]

plt.rcParams["font.sans-serif"] = ["PingFang SC", "Hiragino Sans GB", "Arial Unicode MS"]
plt.rcParams["axes.unicode_minus"] = False


def save(fig: plt.Figure, name: str) -> Path:
    """统一出口：docs/images 下 300dpi PNG。"""
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    out = OUT_DIR / name
    fig.savefig(out, dpi=300, bbox_inches="tight")
    plt.close(fig)
    return out


def chart_segments() -> Path:
    """延迟分解：五段 × (avg, p99) 分组横条，decode 主导一眼可见。"""
    current = apply_rc("tableau-accent")
    colors = series_colors("tableau-accent", 2)
    fig, ax = plt.subplots(figsize=(7.4, 4.2))
    y = np.arange(len(SEGMENTS))[::-1]  # decode 置顶，与管线顺序一致
    height = 0.34
    for idx, (label, values) in enumerate([("avg", SEG_AVG), ("p99", SEG_P99)]):
        offset = (idx - 0.5) * height
        bars = ax.barh(
            y + offset,
            values,
            height=height,
            label=label,
            color=colors[idx],
            edgecolor="white",
            alpha=0.92,
        )
        for bar, value in zip(bars, values):
            ax.text(
                bar.get_width() + 0.15,
                bar.get_y() + bar.get_height() / 2,
                f"{value:g}",
                va="center",
                fontsize=8.5,
                color=NEUTRALS["ink"],
            )
    ax.set_yticks(y, SEGMENTS)
    ax.set_xlabel("耗时（ms）")
    ax.set_title("延迟分解各段耗时（本机桩零延迟基线）")
    ax.set_xlim(0, 13)
    apply_frame(ax, current.frame)
    apply_grid(ax, current.grid, axis="x")
    ax.legend(ncol=2, loc="lower right")
    fig.tight_layout()
    return save(fig, "perf-latency-segments.png")


def chart_cpu() -> Path:
    """CPU flat 构成环形图：syscall 近七成是 I/O 型服务的直接证据。"""
    apply_rc("tableau-paper")
    labels = list(CPU_SHARE)
    values = list(CPU_SHARE.values())
    colors = series_colors("tableau-paper", len(values))
    fig, ax = plt.subplots(figsize=(7.0, 4.8))
    wedges, _, autotexts = ax.pie(
        values,
        labels=labels,
        colors=colors,
        startangle=100,
        wedgeprops={"width": 0.42, "edgecolor": "white", "linewidth": 1.1},
        autopct="%1.1f%%",
        pctdistance=0.78,
    )
    for text in autotexts:
        text.set_color(NEUTRALS["ink"])
        text.set_fontsize(8.5)
    ax.set_title("CPU flat 构成（15s 持续压测采样）")
    ax.set_aspect("equal")
    fig.tight_layout()
    return save(fig, "perf-cpu-flat.png")


def chart_percentiles() -> Path:
    """持续压测 TTFB/总时延分位数分组柱。"""
    current = apply_rc("tableau-paper")
    colors = series_colors("tableau-paper", 2)
    fig, ax = plt.subplots(figsize=(6.8, 4.2))
    x = np.arange(len(PERCENTILES))
    width = 0.34
    for idx, (label, values) in enumerate([("TTFB", TTFB_MS), ("总时延", TOTAL_MS)]):
        offset = (idx - 0.5) * width
        bars = ax.bar(
            x + offset,
            values,
            width=width,
            label=label,
            color=colors[idx],
            edgecolor="white",
            alpha=0.92,
        )
        for bar, value in zip(bars, values):
            ax.text(
                bar.get_x() + bar.get_width() / 2,
                bar.get_height() + 0.5,
                f"{value:g}",
                ha="center",
                fontsize=8.5,
                color=NEUTRALS["ink"],
            )
    ax.set_xticks(x, PERCENTILES)
    ax.set_ylabel("ms")
    ax.set_title("持续压测延迟分位数（787 rps，28350 请求）")
    apply_frame(ax, current.frame)
    apply_grid(ax, current.grid, axis="y")
    ax.legend(ncol=2, loc="upper left")
    fig.tight_layout()
    return save(fig, "perf-load-percentiles.png")


def main() -> None:
    for name in ["segments", "cpu", "percentiles"]:
        path = globals()[f"chart_{name}"]()
        print(f"wrote {path.relative_to(Path.cwd())}")


if __name__ == "__main__":
    main()
