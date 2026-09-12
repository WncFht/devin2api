// 用量页：时间范围 chips + 窗口 KPI + 趋势图组 + 维度表 + 限流事件。
// 时间范围全部按自然日对齐，按天表、模型表与趋势图口径一致；
// ≤8 天窗口用 10 分钟桶，更长窗口按日聚合（与旧版口径相同）。

const Usage = (() => {
  const RANGES = [['today', '今日'], ['yday', '昨日'], ['3d', '近3天'], ['7d', '近7天'], ['14d', '近14天'], ['all', '全部']];
  let range = 'today';
  let last = null;

  function rangeDays(r) {
    const fmt = d => d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
    const now = new Date(); now.setHours(0, 0, 0, 0);
    const shift = n => fmt(new Date(now.getTime() - n * 86400000));
    switch (r) {
      case 'today': return [shift(0), shift(0)];
      case 'yday': return [shift(1), shift(1)];
      case '3d': return [shift(2), shift(0)];
      case '7d': return [shift(6), shift(0)];
      case '14d': return [shift(13), shift(0)];
    }
    return null;
  }
  function rangeSecs(r) {
    const days = rangeDays(r); if (!days) return null;
    return [new Date(days[0] + 'T00:00:00').getTime() / 1000, new Date(days[1] + 'T00:00:00').getTime() / 1000 + 86400];
  }

  function renderChips() {
    $('usageRangeChips').innerHTML = RANGES.map(r =>
      '<span class="chip' + (r[0] === range ? ' on' : '') + '" data-range="' + r[0] + '">' + r[1] + '</span>').join('');
  }

  async function load() {
    try {
      last = await api('/usage');
      render();
    } catch (e) {
      $('usageBody').innerHTML = '<div class="panel"><div class="note">拉取失败: ' + esc(String(e)) + '</div></div>';
    }
  }

  function render() {
    const d = last;
    if (!d) return;
    const body = $('usageBody');
    if (d.disabled) { body.innerHTML = '<div class="panel"><div class="note">调试日志未启用，无用量统计。</div></div>'; return; }
    const s = d.snapshot || {};
    const days = rangeDays(range);
    const secs = rangeSecs(range);
    const inRange = p => !secs || (p.at >= secs[0] && p.at < secs[1]);
    const inDays = dt => !days || (dt >= days[0] && dt <= days[1]);
    const fine = secs != null && (secs[1] - secs[0]) <= 8 * 86400;
    const pts = fine ? (s.points || []).filter(inRange) : (s.days || []).filter(p => inDays(p.date)).slice().reverse();
    const label = (RANGES.find(r => r[0] === range) || [])[1] || '';
    const totals = range === 'all' ? (s.window || {}) : sumTotals(pts);

    let html = '<div class="kpis">' +
      kpi(label + '请求', fmtNum(totals.requests || 0), '错误 ' + (totals.errors || 0) + ' · 断连 ' + (totals.disconnected || 0) + ' · 429 ' + (totals.rate_limited || 0)) +
      kpi('输入', fmtNum(totals.input_tokens), '缓存读 ' + fmtNum(totals.cache_read_tokens), 'info') +
      kpi('输出', fmtNum(totals.output_tokens), '推理 ' + fmtNum(totals.reasoning_tokens), 'ok') +
      kpi('缓存命中率', hitRate(totals), '写 ' + fmtNum(totals.cache_write_tokens), 'cyan') +
      kpi('decode 均速', avgTps(totals), '可信流式条目加权', 'violet');
    if (range === 'all') {
      html += kpi('窗口累计 Token', fmtNum(totals.total_tokens), d.est_cost > 0 ? '估算 ' + '$' + Number(d.est_cost).toFixed(2) + '（目录价）' : '', 'warn');
    }
    html += '</div>';

    // 主趋势图 + 辅助图
    if (pts.length) {
      html += '<div class="panel"><h3>' + (fine ? '10 分钟' : '逐日') + '趋势 <span class="sub">' + esc(label) + ' · 拖选/滚轮缩放</span></h3>' +
        '<div id="uFlow" class="chart chart-h260"></div>' +
        '<div class="chart-grid section-gap">' +
        '<div class="chart-box"><div class="chart-cap">decode 均速 / 缓存命中率</div><div id="uPerf" class="chart chart-h220"></div></div>' +
        (fine ? '<div class="chart-box"><div class="chart-cap">上游 TTFB / 总耗时 p95</div><div id="uLat" class="chart chart-h220"></div></div>' : '') +
        '</div></div>';
    }

    // Token 构成 + 模型分布（横向条）
    const md = s.model_days || {};
    let modelRows = [];
    if (range !== 'all') {
      modelRows = Object.keys(md).map(model => {
        const t = sumTotals(Object.keys(md[model]).filter(inDays).map(k => md[model][k]));
        return { name: model, ...t };
      }).filter(m => m.requests > 0).sort((a, b) => b.requests - a.requests);
    }
    const tokenMix = [
      ['输入', totals.input_tokens], ['缓存读', totals.cache_read_tokens], ['缓存写', totals.cache_write_tokens],
      ['输出', totals.output_tokens], ['推理', totals.reasoning_tokens],
    ].filter(x => x[1] > 0);
    const barRows = (range === 'all' ? (d.models || []).map(m => ({ name: m.name, output_tokens: m.output_tokens, requests: m.requests })) : modelRows)
      .slice().sort((a, b) => b.output_tokens - a.output_tokens).slice(0, 8);
    if (tokenMix.length || barRows.length) {
      html += '<div class="chart-grid"><div class="chart-box"><div class="chart-cap">Token 构成 · ' + esc(label) + '</div><div id="uMix" class="chart chart-h260"></div></div>' +
        '<div class="chart-box"><div class="chart-cap">模型输出 Token Top ' + barRows.length + ' · ' + esc(label) + '</div><div id="uModelBar" class="chart chart-h260"></div></div></div>';
    }

    // 错误阶段 chips（点击跳请求页筛选）
    const stages = s.error_stages || {};
    const stageKeys = Object.keys(stages);
    if (stageKeys.length) {
      html += '<div class="panel"><h3>错误阶段分布 <span class="sub">窗口累计 · 点击筛选请求</span></h3><div class="chip-row" style="margin:0">';
      stageKeys.sort((a, b) => stages[b] - stages[a]).forEach(k => {
        html += '<span class="chip" data-stage="' + qa(k) + '">' + esc(k) + ' <strong>' + stages[k] + '</strong></span>';
      });
      html += '</div></div>';
    }

    // 429 采样
    const rl = s.rate_limit_events || [];
    if (rl.length) {
      const maxRPM = rl.reduce((m, e) => Math.max(m, e.rpm || 0), 0);
      html += '<div class="panel"><h3>上游限流 429 <span class="sub">当时速率 = 该时刻前 60s 发出的请求数</span></h3>' +
        '<div class="grid" style="margin-bottom:10px">' + meta('采样事件', rl.length + (rl.length >= 256 ? '（保留最近 256）' : '')) + meta('观测上限 ≈', maxRPM + ' req/min') + '</div>' +
        '<div class="tbl-wrap" style="max-height:280px"><table><thead><tr><th>时间</th><th>模型</th><th>当时速率</th></tr></thead><tbody>';
      rl.slice().reverse().forEach(e => {
        html += '<tr><td class="mono">' + fmtTime(e.at * 1000) + '</td><td class="mono"><span class="lnk" data-model="' + qa(e.model) + '">' + esc(e.model || '-') + '</span></td><td class="mono">' + e.rpm + ' req/min</td></tr>';
      });
      html += '</tbody></table></div><div class="note">速率按已落盘请求的启动时间统计，在途未完成的请求不计，读数略偏低。本地并发拒绝不进索引，此处全是上游限流。</div></div>';
    }

    // 按模型表
    const allRows = range === 'all' ? (d.models || []) : modelRows;
    if (allRows.length) {
      const wide = range === 'all';
      html += '<div class="panel"><h3>按模型 <span class="sub">' + esc(label) + (wide ? ' · 含成本估算' : '') + ' · 点击模型筛选请求</span></h3>' +
        '<div class="scroll-x"><table><thead><tr><th>模型</th><th>请求</th><th>成功率</th><th>429</th><th>输入</th><th>输出</th><th>缓存读</th><th>命中率</th><th>均速</th>' +
        (wide ? '<th>估算成本</th><th>均耗时</th><th>均TTFB</th><th>最近</th>' : '') + '</tr></thead><tbody>';
      allRows.forEach(m => {
        const sr = m.success_rate != null ? m.success_rate : (m.requests ? (m.requests - m.errors - m.disconnected) / m.requests : 0);
        html += '<tr><td class="mono"><span class="lnk" data-model="' + qa(m.name) + '">' + esc(m.name) + '</span></td>' +
          '<td class="num">' + m.requests + ' <span class="muted">(err ' + m.errors + ')</span></td>' +
          '<td class="num">' + (sr * 100).toFixed(0) + '%</td>' +
          '<td class="num">' + (m.rate_limited || 0) + '</td>' +
          '<td class="mono">' + fmtNum(m.input_tokens) + '</td>' +
          '<td class="mono">' + fmtNum(m.output_tokens) + '</td>' +
          '<td class="mono">' + fmtNum(m.cache_read_tokens) + '</td>' +
          '<td class="mono">' + hitRate(m) + '</td>' +
          '<td class="mono">' + avgTps(m) + '</td>' +
          (wide ? '<td>' + (m.est_cost != null ? money(m.est_cost) : '<span class="muted">—</span>') + '</td>' +
            '<td class="mono">' + fmtMs(Math.round(m.avg_duration_ms || 0)) + '</td>' +
            '<td class="mono">' + fmtMs(Math.round(m.avg_ttfb_ms || 0)) + '</td>' +
            '<td class="mono muted">' + fmtTime(m.last_at) + '</td>' : '') + '</tr>';
      });
      html += '</tbody></table></div></div>';
    }

    // 按 key + 按天
    if (s.keys && s.keys.length) {
      html += '<div class="panel"><h3>按 API Key 哈希 <span class="sub">窗口累计 · 点击筛选请求</span></h3><div class="scroll-x"><table><thead><tr><th>Key 哈希</th><th>请求</th><th>错误</th><th>输出Token</th><th>最近</th></tr></thead><tbody>';
      s.keys.forEach(k => {
        html += '<tr><td class="mono"><span class="lnk" data-key="' + qa(k.name) + '">' + esc(k.name) + '</span></td><td class="num">' + k.requests + '</td><td class="num">' + k.errors + '</td><td class="mono">' + fmtNum(k.output_tokens) + '</td><td class="mono muted">' + fmtTime(k.last_at) + '</td></tr>';
      });
      html += '</tbody></table></div></div>';
    }
    if (s.days && s.days.length > 1) {
      html += '<div class="panel"><h3>按天</h3><div class="scroll-x"><table><thead><tr><th>日期</th><th>请求</th><th>错误</th><th>断连</th><th>429</th><th>输入</th><th>输出</th><th>命中率</th><th>Token合计</th></tr></thead><tbody>';
      s.days.slice(0, 14).forEach(day => {
        html += '<tr><td class="mono">' + esc(day.date) + '</td><td class="num">' + day.requests + '</td><td class="num">' + day.errors + '</td><td class="num">' + day.disconnected + '</td><td class="num">' + (day.rate_limited || 0) + '</td><td class="mono">' + fmtNum(day.input_tokens) + '</td><td class="mono">' + fmtNum(day.output_tokens) + '</td><td class="mono">' + hitRate(day) + '</td><td class="mono">' + fmtNum(day.total_tokens) + '</td></tr>';
      });
      html += '</tbody></table></div></div>';
    }
    if (d.cost_basis) {
      html += '<div class="note">估算成本按模型目录价（' + esc(d.cost_basis) + '）；非上游账单。' + (d.price_missing ? '模型价目暂不可用，未计入成本。' : '') + '窗口起点: ' + esc(s.window_start || '-') + ' · 聚合 ' + (s.entries || 0) + ' 条</div>';
    }
    body.innerHTML = html;
    drawCharts(pts, fine, tokenMix, barRows);
  }

  function drawCharts(pts, fine, tokenMix, barRows) {
    if (!pts.length && !tokenMix.length) return;
    const xs = p => (p.at !== undefined ? p.at : new Date(p.date + 'T00:00:00').getTime() / 1000);
    if (pts.length) {
      Charts.render($('uFlow'), {
        dataZoom: Charts.zoom(pts),
        yAxis: [{}, { splitLine: { show: false }, axisLabel: { formatter: v => fmtNum(v), color: '#8b93a7', fontSize: 10.5 } }],
        series: [
          Charts.bar('请求', '#818cf8', pts.map(p => Charts.ts(xs(p), p.requests))),
          Charts.bar('错误', '#f87171', pts.map(p => Charts.ts(xs(p), p.errors))),
          Charts.bar('429', '#f472b6', pts.map(p => Charts.ts(xs(p), p.rate_limited))),
          Charts.line('输出 token', '#34d399', pts.map(p => Charts.ts(xs(p), p.output_tokens)), { yAxisIndex: 1 }),
        ],
      });
      Charts.render($('uPerf'), {
        yAxis: [{}, { min: 0, max: 100, splitLine: { show: false }, axisLabel: { formatter: '{value}%', color: '#8b93a7', fontSize: 10.5 } }],
        series: [
          Charts.line('decode 均速', '#22d3ee', pts.map(p => Charts.ts(xs(p), p.gen_ms > 0 ? +(p.gen_tokens / (p.gen_ms / 1000)).toFixed(1) : null))),
          Charts.line('缓存命中率', '#fbbf24', pts.map(p => {
            const dd = (p.cache_read_tokens || 0) + (p.input_tokens || 0);
            return Charts.ts(xs(p), dd > 0 ? +(p.cache_read_tokens / dd * 100).toFixed(1) : null);
          }), { yAxisIndex: 1, areaStyle: undefined }),
        ],
      });
      if (fine) {
        Charts.render($('uLat'), {
          series: [
            Charts.line('TTFB 均值', '#818cf8', pts.map(p => Charts.ts(xs(p), p.avg_ttfb_ms || null))),
            Charts.line('TTFB p95', '#fbbf24', pts.map(p => Charts.ts(xs(p), p.ttfb_p95_ms || null))),
            Charts.line('耗时 p95', '#f87171', pts.map(p => Charts.ts(xs(p), p.duration_p95_ms || null))),
          ],
          tooltip: { trigger: 'axis', valueFormatter: v => v == null ? '-' : fmtMs(v), backgroundColor: 'rgba(18,21,31,.96)', borderColor: 'rgba(148,163,184,.25)', textStyle: { color: '#e5e9f2', fontSize: 12 } },
          yAxis: { axisLabel: { formatter: v => v >= 1000 ? (v / 1000) + 's' : v, color: '#8b93a7', fontSize: 10.5 }, splitLine: { lineStyle: { color: 'rgba(148,163,184,0.08)' } } },
        });
      }
    }
    if (tokenMix.length && $('uMix')) {
      Charts.render($('uMix'), {
        tooltip: { trigger: 'item', backgroundColor: 'rgba(18,21,31,.96)', borderColor: 'rgba(148,163,184,.25)', textStyle: { color: '#e5e9f2', fontSize: 12 }, valueFormatter: v => fmtNum(v) },
        legend: { bottom: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, textStyle: { color: '#8b93a7', fontSize: 11 } },
        series: [{
          type: 'pie', radius: ['52%', '74%'], center: ['50%', '44%'],
          itemStyle: { borderColor: '#12151f', borderWidth: 2, borderRadius: 4 },
          label: { show: false }, emphasis: { label: { show: true, color: '#e5e9f2', fontSize: 12, formatter: '{b}\n{d}%' } },
          data: tokenMix.map((x, i) => ({ name: x[0], value: x[1], itemStyle: { color: Charts.palette[i] } })),
        }],
      });
    }
    if (barRows.length && $('uModelBar')) {
      const rows = barRows.slice().reverse();
      Charts.render($('uModelBar'), {
        grid: { left: 8, right: 40, top: 8, bottom: 8, containLabel: true },
        xAxis: { type: 'value', axisLabel: { formatter: v => fmtNum(v), color: '#8b93a7', fontSize: 10.5 }, splitLine: { lineStyle: { color: 'rgba(148,163,184,0.08)' } } },
        yAxis: { type: 'category', data: rows.map(r => r.name), axisLabel: { color: '#aab1c5', fontSize: 10.5, width: 130, overflow: 'truncate' }, axisLine: { lineStyle: { color: '#3a415a' } }, axisTick: { show: false } },
        tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' }, backgroundColor: 'rgba(18,21,31,.96)', borderColor: 'rgba(148,163,184,.25)', textStyle: { color: '#e5e9f2', fontSize: 12 }, formatter: ps => ps.map(p => p.name + '<br/>输出 ' + fmtNum(p.value) + ' tok').join('') },
        series: [{ type: 'bar', barMaxWidth: 14, itemStyle: { borderRadius: [0, 4, 4, 0], color: new echarts.graphic.LinearGradient(0, 0, 1, 0, [{ offset: 0, color: 'rgba(129,140,248,.55)' }, { offset: 1, color: '#818cf8' }]) }, label: { show: true, position: 'right', color: '#8b93a7', fontSize: 10, formatter: p => fmtNum(p.value) }, data: rows.map(r => r.output_tokens) }],
      });
    }
  }

  // 事件委托：chips / stage / model / key 链接
  document.getElementById('page-usage').addEventListener('click', e => {
    const rc = e.target.closest('[data-range]');
    if (rc) { range = rc.dataset.range; renderChips(); render(); return; }
    const st = e.target.closest('[data-stage]');
    if (st) { jumpRequests({ error_stage: st.dataset.stage }); return; }
    const mo = e.target.closest('[data-model]');
    if (mo) { jumpRequests({ model: mo.dataset.model }); return; }
    const ky = e.target.closest('[data-key]');
    if (ky) { jumpRequests({ q: ky.dataset.key }); return; }
  });

  renderChips();
  Tabs.register('usage', load);
  onVisible('usage', load, 60000);

  return {};
})();
