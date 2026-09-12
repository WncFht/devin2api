// ECharts 统一暗色主题与图表工厂。
// 所有图表经 Charts.render(el, option) 产出：同容器重复渲染复用实例、
// 页面切换/容器尺寸变化自动 resize、离开页面时实例挂起不销毁（回来接着用）。

const Charts = (() => {
  const palette = ['#818cf8', '#34d399', '#fbbf24', '#f87171', '#38bdf8', '#f472b6', '#a78bfa', '#22d3ee'];
  const axisColor = '#3a415a';
  const textDim = '#8b93a7';

  // base 返回所有图共用的暗色骨架：色板、坐标轴、tooltip、图例。
  function base() {
    return {
      color: palette,
      textStyle: { fontFamily: 'system-ui, -apple-system, sans-serif' },
      grid: { left: 8, right: 12, top: 34, bottom: 8, containLabel: true },
      legend: { top: 0, left: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, itemGap: 14, textStyle: { color: textDim, fontSize: 11 } },
      tooltip: {
        trigger: 'axis',
        backgroundColor: 'rgba(18,21,31,.96)',
        borderColor: 'rgba(148,163,184,.25)',
        textStyle: { color: '#e5e9f2', fontSize: 12 },
        axisPointer: { type: 'line', lineStyle: { color: 'rgba(148,163,184,.4)' } },
      },
      xAxis: {
        type: 'time',
        axisLine: { lineStyle: { color: axisColor } },
        axisTick: { show: false },
        axisLabel: { color: textDim, fontSize: 10.5, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: {
        type: 'value',
        axisLabel: { color: textDim, fontSize: 10.5 },
        splitLine: { lineStyle: { color: 'rgba(148,163,184,0.08)' } },
        axisLine: { show: false },
      },
    };
  }

  // render 同容器重复渲染时复用实例。定时刷新会重建 option：先把用户当前
  // 的 dataZoom 窗口记下来，渲染后恢复，避免 10s 轮询冲掉正在细看的缩放。
  function render(el, option) {
    if (!el || !window.echarts) return null;
    let inst = echarts.getInstanceByDom(el);
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

  // dataZoom：长窗口加底部滑块；任何窗口都支持内部滚轮/拖选。
  function zoom(pts) {
    const z = [{ type: 'inside', xAxisIndex: 0, filterMode: 'none' }];
    if (pts && pts.length > 150) {
      z.push({ type: 'slider', height: 18, bottom: 2, borderColor: 'transparent', backgroundColor: 'rgba(148,163,184,0.06)', fillerColor: 'rgba(129,140,248,0.15)', handleStyle: { color: '#818cf8' }, textStyle: { color: textDim, fontSize: 10 }, dataBackground: { lineStyle: { color: axisColor }, areaStyle: { color: 'rgba(148,163,184,0.08)' } } });
    }
    return z;
  }

  window.addEventListener('resize', debounce(() => {
    document.querySelectorAll('.chart').forEach(el => {
      const inst = echarts.getInstanceByDom(el);
      if (inst) inst.resize();
    });
  }, 200));

  return { render, line, bar, ts, tsList, zoom, palette, area, hexA };
})();
