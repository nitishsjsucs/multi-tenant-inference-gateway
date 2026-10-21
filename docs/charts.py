#!/usr/bin/env python3
"""Render the benchmark charts in docs/images/ from docs/results/benchmark.json.

matplotlib is not a project dependency; use a throwaway virtualenv:

    python3 -m venv /tmp/mtig-charts && /tmp/mtig-charts/bin/pip install matplotlib
    /tmp/mtig-charts/bin/python docs/charts.py

Each chart is written in a light and a dark variant for GitHub's
prefers-color-scheme switch. Every number drawn comes from the results file.
"""

import json
import pathlib

import matplotlib

matplotlib.use("svg")
import matplotlib.pyplot as plt  # noqa: E402
from matplotlib.patches import PathPatch  # noqa: E402
from matplotlib.path import Path  # noqa: E402

DOCS = pathlib.Path(__file__).resolve().parent
RESULTS = DOCS / "results" / "benchmark.json"
IMAGES = DOCS / "images"

# Emphasis form: the prefix-aware picker in the accent hue, baselines in gray.
THEMES = {
    "light": {
        "surface": "#fcfcfb", "ink": "#0b0b0b", "ink2": "#52514e", "muted": "#898781",
        "grid": "#e1e0d9", "axis": "#c3c2b7", "accent": "#2a78d6", "context": "#c3c2b7",
    },
    "dark": {
        "surface": "#1a1a19", "ink": "#f0efec", "ink2": "#c3c2b7", "muted": "#898781",
        "grid": "#2c2c2a", "axis": "#383835", "accent": "#3987e5", "context": "#5f5e59",
    },
}
ACCENT_PICKER = "prefix-aware"
WIDTH_PT = 720  # rendered at width="720" in the README, so 1 pt is about 1 px
BAR_PT = 20     # bar thickness
BAND_PT = 40    # row pitch
RADIUS_PT = 4   # rounded data end

plt.rcParams.update({
    "font.family": ["Inter", "DejaVu Sans"],
    "svg.fonttype": "path",
    "svg.hashsalt": "mtig-charts",
})


def bar(ax, y, width, color, px_x, px_y):
    """A horizontal bar, square at the baseline, 4 pt rounded at the data end."""
    h = BAR_PT / px_y
    rx, ry = min(RADIUS_PT / px_x, width / 2), RADIUS_PT / px_y
    y0, y1, k = y - h / 2, y + h / 2, 0.5523  # k: cubic approximation of a quarter circle
    verts = [
        (0, y0), (width - rx, y0),
        (width - rx + k * rx, y0), (width, y0 + ry - k * ry), (width, y0 + ry),
        (width, y1 - ry),
        (width, y1 - ry + k * ry), (width - rx + k * rx, y1), (width - rx, y1),
        (0, y1), (0, y0),
    ]
    codes = [Path.MOVETO, Path.LINETO, Path.CURVE4, Path.CURVE4, Path.CURVE4, Path.LINETO,
             Path.CURVE4, Path.CURVE4, Path.CURVE4, Path.LINETO, Path.CLOSEPOLY]
    ax.add_patch(PathPatch(Path(verts, codes), facecolor=color, edgecolor="none", zorder=2))


def panel(ax, t, rows, key, xmax, ticks, fmt, ranges=None, show_labels=True, title=None):
    """One horizontal bar panel: a row per picker, value at the bar tip."""
    n = len(rows)
    ax.set_xlim(0, xmax)
    ax.set_ylim(n - 0.5, -0.5)
    bbox = ax.get_position()
    fig_w, fig_h = ax.figure.get_size_inches() * 72
    px_x = bbox.width * fig_w / xmax
    px_y = bbox.height * fig_h / n
    ax.set_facecolor(t["surface"])
    for side in ("top", "right", "bottom"):
        ax.spines[side].set_visible(False)
    ax.spines["left"].set_color(t["axis"])
    ax.spines["left"].set_linewidth(1)
    ax.set_xticks(ticks)
    ax.set_xticklabels([fmt(v) for v in ticks])
    ax.tick_params(axis="x", length=0, colors=t["muted"], labelsize=10, pad=6)
    ax.tick_params(axis="y", length=0, colors=t["ink2"], labelsize=11.5, pad=8)
    for v in ticks:
        if v:
            ax.axvline(v, color=t["grid"], linewidth=1, zorder=0)
    ax.set_yticks(range(n))
    ax.set_yticklabels([r["picker"] for r in rows] if show_labels else [])
    if title:
        ax.set_title(title, loc="left", color=t["ink"], fontsize=11.5, fontweight="semibold", pad=8)
    for i, r in enumerate(rows):
        v = r[key]
        accent = r["picker"] == ACCENT_PICKER
        bar(ax, i, v, t["accent"] if accent else t["context"], px_x, px_y)
        end = v
        if ranges:
            lo, hi = ranges[r["picker"]][key]
            cap = 0.22
            ax.plot([lo, hi], [i, i], color=t["ink2"], linewidth=1.5, solid_capstyle="butt", zorder=3)
            ax.plot([lo, lo], [i - cap, i + cap], color=t["ink2"], linewidth=1.5, zorder=3)
            ax.plot([hi, hi], [i - cap, i + cap], color=t["ink2"], linewidth=1.5, zorder=3)
            end = max(v, hi)
        ax.text(end + 6 / px_x, i, fmt(v), va="center", ha="left", color=t["ink"], fontsize=11,
                fontweight="semibold" if accent else "normal", zorder=4)


def header(fig, t, title, subtitle, top_pt):
    h = fig.get_size_inches()[1] * 72
    fig.text(24 / WIDTH_PT, 1 - 22 / h, title, color=t["ink"], fontsize=14, fontweight="semibold", va="top")
    fig.text(24 / WIDTH_PT, 1 - 44 / h, subtitle, color=t["ink2"], fontsize=10.5, va="top")


def save(fig, name, mode):
    IMAGES.mkdir(parents=True, exist_ok=True)
    fig.savefig(IMAGES / f"{name}-{mode}.svg", facecolor=fig.get_facecolor(),
                metadata={"Date": None, "Creator": None})
    plt.close(fig)


def hit_rate_chart(data, mode):
    t = THEMES[mode]
    rows = data["seed1"]["rows"]
    top, bottom, left, right = 76, 34, 118, 64
    h = top + bottom + BAND_PT * len(rows)
    fig = plt.figure(figsize=(WIDTH_PT / 72, h / 72), facecolor=t["surface"])
    header(fig, t, "Prefix-cache hit rate (higher is better)",
           "Share of prompt tokens served from the replicas' prefix cache · seed 1 · simulated vLLM replicas", top)
    ax = fig.add_axes([left / WIDTH_PT, bottom / h, 1 - (left + right) / WIDTH_PT, 1 - (top + bottom) / h])
    panel(ax, t, rows, "cache_hit_rate_pct", 100, [0, 25, 50, 75, 100], lambda v: f"{v:g}%")
    save(fig, "benchmark-hit-rate", mode)


def ttft_chart(data, mode):
    t = THEMES[mode]
    rows = data["seed1"]["rows"]
    ranges = {r["picker"]: r for r in data["seeds_1_to_3_ranges"]["rows"]}
    keys = [("ttft_p50_ms", "p50"), ("ttft_p95_ms", "p95"), ("ttft_p99_ms", "p99")]
    top, bottom, left, right, gap = 104, 34, 118, 24, 40
    h = top + bottom + BAND_PT * len(rows)
    fig = plt.figure(figsize=(WIDTH_PT / 72, h / 72), facecolor=t["surface"])
    header(fig, t, "Time to first token, ms (lower is better)",
           "Bars: seed 1 · whiskers: min–max over seeds 1–3 · simulated vLLM replicas", top)
    xmax = 150
    pw = (WIDTH_PT - left - right - gap * (len(keys) - 1)) / len(keys)
    for j, (key, label) in enumerate(keys):
        x0 = left + j * (pw + gap)
        ax = fig.add_axes([x0 / WIDTH_PT, bottom / h, pw / WIDTH_PT, 1 - (top + bottom) / h])
        panel(ax, t, rows, key, xmax, [0, 50, 100], lambda v: f"{v:g}", ranges=ranges,
              show_labels=(j == 0), title=label)
    save(fig, "benchmark-ttft", mode)


def main():
    data = json.loads(RESULTS.read_text())
    for mode in THEMES:
        hit_rate_chart(data, mode)
        ttft_chart(data, mode)


if __name__ == "__main__":
    main()
