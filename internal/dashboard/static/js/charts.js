// ECharts 统一暗色主题与图表工厂。
// 所有图表经 Charts.render(el, option) 产出：同容器重复渲染复用实例、
// 页面切换/容器尺寸变化自动 resize、离开页面时实例挂起不销毁（回来接着用）。

const Charts = (() => {
  // 主题色从 panel.css 的 CSS 变量读，单一事实源；取不到时回落到内置暗色值。
  function cssVar(name, fallback) {
    const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim();
    return v || fallback;
  }
  // 语义色表：JS 侧不再散落令牌抄本，主题改色只动 panel.css。
  // C.axis 是图表次级文字专用灰（--chart-dim），C.line 是 border-strong
  // 在暗底上的实体近似色——画布上画半透明细线会发虚，轴线用实体色。
  const C = {
    accent: cssVar('--accent', '#818cf8'),
    ok: cssVar('--ok', '#34d399'),
    warn: cssVar('--warn', '#fbbf24'),
    err: cssVar('--err', '#f87171'),
    info: cssVar('--info', '#38bdf8'),
    pink: cssVar('--pink', '#f472b6'),
    violet: cssVar('--violet', '#a78bfa'),
    cyan: cssVar('--cyan', '#22d3ee'),
    text: cssVar('--text', '#e5e9f2'),
    dim: cssVar('--text-dim', '#aab1c5'),
    surface: cssVar('--surface', '#12151f'),
    surface3: cssVar('--surface-3', '#1f2434'),
    bg: cssVar('--bg', '#0b0e14'),
    borderStrong: cssVar('--border-strong', 'rgba(148,163,184,.2)'),
    axis: cssVar('--chart-dim', '#8b93a7'),
    line: '#3a415a',
  };
  const palette = [C.accent, C.ok, C.warn, C.err, C.info, C.pink, C.violet, C.cyan];
  const axisColor = C.line;
  const textDim = C.axis;
  // 字号三档与 panel.css 的 --fs-* 令牌同步（变量值是 '12.5px'，parse 成数字）。
  const F = {
    xs: parseFloat(cssVar('--fs-xs')) || 10.5,
    sm: parseFloat(cssVar('--fs-sm')) || 11.5,
    md: parseFloat(cssVar('--fs-md')) || 12.5,
  };
  // slate-400 任意 alpha 变体：分格线/遮罩/十字线共用底色的自由透明度。
  function slate(a) { return 'rgba(148,163,184,' + a + ')'; }

  // base 返回所有图共用的暗色骨架：色板、坐标轴、tooltip、图例。
  function base() {
    return {
      color: palette,
      textStyle: { fontFamily: 'system-ui, -apple-system, sans-serif' },
      grid: { left: 8, right: 12, top: 34, bottom: 8, containLabel: true },
      legend: { top: 0, left: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, itemGap: 14, textStyle: { color: textDim, fontSize: F.sm } },
      // tooltip 容器对齐矩阵悬停卡 .mx-tip 的令牌（surface-3 底、
      // border-strong 边、8px 圆角、同款阴影与内边距）——全站悬浮层
      // 只有一套外观；内容排版由调用方复用 mt-* 结构类保持一致。
      tooltip: {
        trigger: 'axis', confine: true,
        backgroundColor: C.surface3,
        borderColor: C.borderStrong,
        borderWidth: 1,
        padding: [9, 12, 10],
        textStyle: { color: C.text, fontSize: F.sm },
        extraCssText: 'border-radius:8px;box-shadow:0 10px 28px rgba(0,0,0,.5);line-height:1.65;',
        axisPointer: { type: 'line', lineStyle: { color: slate(0.4) } },
      },
      xAxis: {
        type: 'time',
        axisLine: { lineStyle: { color: axisColor } },
        axisTick: { show: false },
        axisLabel: { color: textDim, fontSize: F.xs, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: {
        type: 'value', scale: true,
        axisLabel: { color: textDim, fontSize: F.xs },
        splitLine: { lineStyle: { color: 'rgba(148,163,184,0.08)' } },
        axisLine: { show: false },
      },
    };
  }

  // render 同容器重复渲染时复用实例。定时刷新会重建 option：先把用户当前
  // 的 dataZoom 窗口记下来，渲染后恢复，避免 10s 轮询冲掉正在细看的缩放。
  // 悬停冻结：setOption 会拆掉正在显示的 tooltip。指针在图上时只记最新
  // option 不渲染，mouseleave 补一笔——轮询照跑，画面不打扰悬浮窗。
  const deferred = new Map(), leaveBound = new WeakSet();
  function render(el, option) {
    if (!el || !window.echarts) return null;
    let inst = echarts.getInstanceByDom(el);
    if (inst && el.matches(':hover')) {
      deferred.set(el, option);
      if (!leaveBound.has(el)) {
        leaveBound.add(el);
        el.addEventListener('mouseleave', () => {
          const o = deferred.get(el);
          deferred.delete(el);
          if (o) render(el, o);
        });
      }
      return inst;
    }
    let savedZoom = null;
    if (inst) {
      const cur = (inst.getOption().dataZoom || [])[0];
      if (cur && cur.startValue != null) savedZoom = { startValue: cur.startValue, endValue: cur.endValue };
    } else {
      inst = echarts.init(el, null, { renderer: 'canvas' });
    }
    const opt = Object.assign(base(), option);
    // 轴允许传数组（双 y 轴）：缺省项补暗色轴样式。
    ['xAxis', 'yAxis'].forEach(k => {
      if (Array.isArray(opt[k])) {
        opt[k] = opt[k].map(a => Object.assign({}, base()[k], a));
      } else if (opt[k] && opt[k] !== base()[k]) {
        opt[k] = Object.assign({}, base()[k], opt[k]);
      }
    });
    // tooltip/legend 同轴处理：顶层浅合并会把调用方传入的整个键顶掉，
    // 容器令牌全丢、回落 echarts 默认灰卡——只传差异字段，缺省由 base 补。
    ['tooltip', 'legend'].forEach(k => {
      if (opt[k] && !Array.isArray(opt[k])) opt[k] = Object.assign({}, base()[k], opt[k]);
    });
    if (savedZoom && opt.dataZoom) {
      opt.dataZoom = opt.dataZoom.map(z => Object.assign({}, z, savedZoom));
    }
    inst.setOption(opt, { notMerge: true });
    return inst;
  }

  function area(color, top, bottom) {
    return new echarts.graphic.LinearGradient(0, 0, 0, 1, [
      { offset: 0, color: hexA(color, top == null ? 0.22 : top) },
      { offset: 1, color: hexA(color, bottom == null ? 0 : bottom) },
    ]);
  }
  function hexA(hex, a) {
    const n = parseInt(hex.slice(1), 16);
    return 'rgba(' + (n >> 16) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + a + ')';
  }
  // line 风格统一：细平滑线 + 渐变面积。
  function line(name, color, data, extra) {
    return Object.assign({
      name, type: 'line', smooth: 0.3, symbol: 'none', sampling: 'lttb',
      lineStyle: { width: 1.8, color },
      itemStyle: { color },
      areaStyle: { color: area(color) },
      data,
    }, extra || {});
  }
  function bar(name, color, data, extra) {
    return Object.assign({
      name, type: 'bar', barMaxWidth: 14, itemStyle: { color, borderRadius: [3, 3, 0, 0] }, data,
    }, extra || {});
  }
  // 时间序列点统一成 [ms, value]。
  function ts(at, v) { return [at * 1000, v]; }
  function tsList(pts, atKey, vKey, map) {
    return pts.map(p => ts(p[atKey || 'at'], map ? map(p) : p[vKey]));
  }

  // gapMark：时间序列的空窗标记（markArea 灰底）。
  // 两类都算空窗：连续 ≥minEmpty 个零请求桶（默认 3，调用方按桶宽换算
  // 时长——10s 桶传 9 ≈ 90s 无流量）；相邻桶间隔 > 1.5 倍桶宽（数据缺失段）。
  // 返回值挂在第一条 series 上即可（silent 不挡交互）。
  function gapMark(pts, stepSec, minEmpty) {
    if (!pts || !pts.length || !stepSec) return null;
    const minRun = minEmpty || 3;
    // 整段全空时整块灰底反而像异常，此时不标（图本身就是空的）。
    if (!pts.some(p => p.requests || p.errors)) return null;
    const ranges = [];
    let s = -1;
    pts.forEach((p, i) => {
      const empty = !(p.requests || p.errors);
      if (empty) { if (s < 0) s = i; }
      else if (s >= 0) { if (i - s >= minRun) ranges.push([pts[s].at, pts[i - 1].at]); s = -1; }
    });
    if (s >= 0 && pts.length - s >= minRun) ranges.push([pts[s].at, pts[pts.length - 1].at]);
    for (let i = 1; i < pts.length; i++) {
      if (pts[i].at - pts[i - 1].at > stepSec * 1.5) ranges.push([pts[i - 1].at, pts[i].at]);
    }
    if (!ranges.length) return null;
    return {
      silent: true, itemStyle: { color: slate(0.07) },
      data: ranges.map(r => [{ xAxis: r[0] * 1000 }, { xAxis: r[1] * 1000 }]),
    };
  }

  // latencyMarks：延迟类曲线挂 markLine(均值虚线) + markPoint(峰值 pin)。
  function latencyMarks() {
    return {
      markLine: {
        silent: true, symbol: 'none', lineStyle: { type: 'dashed', width: 1, opacity: 0.55 },
        label: { color: textDim, fontSize: F.xs, formatter: p => 'avg ' + fmtMs(p.value) },
        data: [{ type: 'average' }],
      },
      markPoint: {
        symbol: 'pin', symbolSize: 34,
        label: { fontSize: 9, color: C.bg, formatter: p => fmtMs(p.value) },
        data: [{ type: 'max', name: 'MAX' }],
      },
    };
  }

  // empty：无数据时给图容器渲染居中文本（替代空白图）。
  function empty(el, text) {
    render(el, {
      xAxis: { show: false }, yAxis: { show: false }, series: [],
      graphic: [{ type: 'text', left: 'center', top: 'middle', style: { text: text || '暂无数据', fill: textDim, fontSize: F.md } }],
    });
  }

  // dataZoom：长窗口加底部滑块；任何窗口都支持内部滚轮/拖选。
  function zoom(pts) {
    const z = [{ type: 'inside', xAxisIndex: 0, filterMode: 'none' }];
    if (pts && pts.length > 150) {
      z.push({ type: 'slider', height: 18, bottom: 2, borderColor: 'transparent', backgroundColor: slate(0.06), fillerColor: hexA(C.accent, 0.15), handleStyle: { color: C.accent }, textStyle: { color: textDim, fontSize: F.xs }, dataBackground: { lineStyle: { color: axisColor }, areaStyle: { color: slate(0.08) } } });
    }
    return z;
  }

  window.addEventListener('resize', debounce(() => {
    document.querySelectorAll('.chart').forEach(el => {
      const inst = echarts.getInstanceByDom(el);
      if (inst) inst.resize();
    });
  }, 200));

  return { render, line, bar, ts, tsList, zoom, palette, area, hexA, gapMark, latencyMarks, empty, C, slate, F };
})();
