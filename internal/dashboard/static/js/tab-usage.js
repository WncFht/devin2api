// 用量页：时间范围 chips + 窗口 KPI + 趋势图组 + 维度表 + 限流事件。
// 时间范围全部按自然日对齐，按天表、模型表与趋势图口径一致；
// ≤8 天窗口用 10 分钟桶，更长窗口按日聚合（与旧版口径相同）。
// usageBody 经 morph 更新：chart 容器靠 id 匹配保住 echarts 实例与缩放态，
// 条件性出现的图表（uLat 仅细粒度窗口、uMix/uModelBar 需数据）消失时
// 由 morph 的 onNodeDiscarded 自动 dispose——不再每轮重建实例。

import {
  $, api, esc, fmtNum, fmtMs, fmtTime, fmtRel, money,
  msClass, rateClass, slaRate, hitRate, avgTps, kpi, meta,
  sumTotals, Tabs, Polls, morph, loadPref, savePref,
} from './core.js';
import { Charts } from './charts.js';
import { jumpRequests } from './tab-requests.js';

const C = Charts.C, F = Charts.F;
const RANGES = [['today', '今日'], ['yday', '昨日'], ['3d', '近3天'], ['7d', '近7天'], ['14d', '近14天'], ['all', '全部']];
// 时间范围与模型表排序记忆在 localStorage：重开面板回到上次视角。
let range = loadPref('usage.range', 'today');
let last = null;
// 模型表排序状态：key 取 MCOLS 的取值器名，dir 0/-1/1（无/desc/asc）。
const mSortSaved = loadPref('usage.msort', { key: null, dir: 0 });
let mSortKey = mSortSaved.key, mSortDir = mSortSaved.dir;

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
  morph($('usageRangeChips'), RANGES.map(r =>
    '<button type="button" class="chip' + (r[0] === range ? ' on' : '') + '" data-range="' + r[0] + '">' + r[1] + '</button>').join(''));
}

async function load() {
  try {
    last = await api('/usage');
    render();
  } catch (e) {
    morph($('usageBody'), '<div class="panel"><div class="note">拉取失败: ' + esc(String(e)) + '</div></div>');
  }
}

function render() {
  const d = last;
  if (!d) return;
  const body = $('usageBody');
  if (d.disabled) { morph(body, '<div class="panel"><div class="note">调试日志未启用，无用量统计。</div></div>'); return; }
  const s = d.snapshot || {};
  const days = rangeDays(range);
  const secs = rangeSecs(range);
  const inRange = p => !secs || (p.at >= secs[0] && p.at < secs[1]);
  const inDays = dt => !days || (dt >= days[0] && dt <= days[1]);
  const fine = secs != null && (secs[1] - secs[0]) <= 8 * 86400;
  const pts = fine ? (s.points || []).filter(inRange) : (s.days || []).filter(p => inDays(p.date)).slice().reverse();
  const label = (RANGES.find(r => r[0] === range) || [])[1] || '';
  const totals = range === 'all' ? (s.window || {}) : sumTotals(pts);

  // 成功率统一走 SLA 口径：服务端失分/承诺内请求（剔除客户端责任与 429）。
  const sla = slaRate(totals);
  let html = '<div class="kpis">' +
    kpi(label + '请求', fmtNum(totals.requests || 0), (sla == null ? '成功率 —' : 'SLA ' + sla.toFixed(1) + '%') +
      ' · 服务端 ' + (totals.upstream_faults || 0) + ' · 客户端 ' + (totals.client_faults || 0) + ' · 429 ' + (totals.rate_limited || 0)) +
    kpi('输入', fmtNum(totals.input_tokens), '缓存读 ' + fmtNum(totals.cache_read_tokens), 'info') +
    kpi('输出', fmtNum(totals.output_tokens), '推理 ' + fmtNum(totals.reasoning_tokens), 'ok') +
    kpi('缓存命中率', hitRate(totals), '写 ' + fmtNum(totals.cache_write_tokens), 'cyan') +
    kpi('decode 均速', avgTps(totals), '可信流式条目加权', 'violet');
  if (range === 'all') {
    html += kpi('窗口累计 Token', fmtNum(totals.total_tokens), d.est_cost > 0 ? '估算 ' + money(d.est_cost) + '（目录价）' : '', 'warn');
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
  // Top8 之外的模型合并为「其他」一行，保证横向条与总量口径一致（sub2api Σ Other）。
  const allBar = (range === 'all' ? (d.models || []).map(m => ({ name: m.name, output_tokens: m.output_tokens, requests: m.requests })) : modelRows)
    .slice().sort((a, b) => b.output_tokens - a.output_tokens);
  let barRows = allBar.slice(0, 8);
  if (allBar.length > 8) {
    const rest = allBar.slice(8);
    barRows.push({ name: '其他 (' + rest.length + ')', output_tokens: rest.reduce((a, m) => a + (m.output_tokens || 0), 0), requests: rest.reduce((a, m) => a + (m.requests || 0), 0), other: true });
  }
  // 单模型窗口下 Top 条图退化成一根独柱——没有对比就没有信息量，藏掉。
  const showMix = tokenMix.length > 0, showBar = barRows.length > 1;
  if (showMix || showBar) {
    html += '<div class="chart-grid">' +
      (showMix ? '<div class="chart-box"><div class="chart-cap">Token 构成 · ' + esc(label) + '</div><div id="uMix" class="chart chart-h260"></div></div>' : '') +
      (showBar ? '<div class="chart-box"><div class="chart-cap">模型输出 Token Top ' + barRows.length + ' · ' + esc(label) + '</div><div id="uModelBar" class="chart chart-h260"></div></div>' : '') +
      '</div>';
  }

  // 错误阶段 chips（点击跳请求页筛选）
  const stages = s.error_stages || {};
  const stageKeys = Object.keys(stages);
  if (stageKeys.length) {
    html += '<div class="panel"><h3>错误阶段分布 <span class="sub">窗口累计 · 点击筛选请求</span></h3><div class="chip-row flat">';
    stageKeys.sort((a, b) => stages[b] - stages[a]).forEach(k => {
      html += '<button type="button" class="chip" data-stage="' + esc(k) + '">' + esc(k) + ' <strong>' + stages[k] + '</strong></button>';
    });
    html += '</div></div>';
  }

  // 429 采样：stage 区分来源——rate_gate 是本地闸门快败（其"当时速率"
  // 是到达速率，含被拒请求），其余为上游真 429（近似上游收到的发送
  // 速率）。观测上限只取上游行：本地快败没碰到上游，不代表上游阈值。
  const rl = s.rate_limit_events || [];
  if (rl.length) {
    const up = rl.filter(e => e.stage !== 'rate_gate');
    const maxRPM = up.reduce((m, e) => Math.max(m, e.rpm || 0), 0);
    html += '<div class="panel"><h3>限流 429 <span class="sub">当时速率 = 该时刻前 60s 内启动的请求数 · 本地闸门行是到达速率</span></h3>' +
      '<div class="grid" style="margin-bottom:10px">' + meta('采样事件', rl.length + (rl.length >= 256 ? '（保留最近 256）' : '')) + meta('上游 / 本地闸门', up.length + ' / ' + (rl.length - up.length)) + meta('上游观测上限 ≈', maxRPM + ' req/min') + '</div>' +
      '<div class="tbl-wrap" style="max-height:280px"><table><thead><tr><th>时间</th><th>来源</th><th>模型</th><th>当时速率</th></tr></thead><tbody>';
    rl.slice().reverse().forEach(e => {
      const src = e.stage === 'rate_gate'
        ? '<span class="badge badge-off" title="本地闸门快败：未触达上游，stage=rate_gate">本地闸门</span>'
        : '<span class="badge badge-medium" title="上游真实限流' + (e.stage ? '，stage=' + esc(e.stage) : '') + '">上游</span>';
      html += '<tr><td class="mono">' + fmtTime(e.at * 1000) + '</td><td>' + src + '</td><td class="mono"><button type="button" class="lnk" data-model="' + esc(e.model) + '">' + esc(e.model || '-') + '</button></td><td class="mono">' + e.rpm + ' req/min</td></tr>';
    });
    html += '</tbody></table></div><div class="note">速率按已落盘请求的启动时间统计，在途未完成的请求不计，读数略偏低。鉴权/并发/排空等管线前拒绝不进索引（见系统页「本地拒绝」）；本地闸门行统计的是到达洪峰，不代表上游阈值。</div></div>';
  }

  // 按模型表：表头三态排序 + 加权合计行 + 阈值着色。
  // 成功率列是 SLA 口径（剔除客户端责任与 429 的服务端成功率）；
  // 长度分布/上下文填充/成本占比只在全窗口下可得（model_days 不带
  // 样本与目录字段，与 est_cost 同一约束）。
  const allRows = range === 'all' ? (d.models || []) : modelRows;
  if (allRows.length) {
    const wide = range === 'all';
    const hitRateVal = m => {
      const dd = (m.cache_read_tokens || 0) + (m.input_tokens || 0);
      return dd > 0 ? m.cache_read_tokens / dd * 100 : -1;
    };
    const srOf = m => slaRate(m) ?? -1;
    const tpsOf = m => m.gen_ms > 0 ? m.gen_tokens / (m.gen_ms / 1000) : -1;
    // 排序取值器：缺省值排到最后。
    const MCOLS = {
      requests: m => m.requests || 0, sr: srOf, rate_limited: m => m.rate_limited || 0,
      input_tokens: m => m.input_tokens || 0, output_tokens: m => m.output_tokens || 0,
      cache_read_tokens: m => m.cache_read_tokens || 0, hit: hitRateVal, tps: tpsOf,
      in_p50: m => m.input_p50 || 0, fill: m => m.context_fill_pct || 0,
      est_cost: m => m.est_cost ?? -1, avg_duration_ms: m => m.avg_duration_ms || 0,
      avg_ttfb_ms: m => m.avg_ttfb_ms || 0, last_at: m => new Date(m.last_at || 0).getTime() || 0,
    };
    const rows = allRows.slice();
    if (mSortKey && mSortDir && MCOLS[mSortKey]) {
      // 缺省哨兵 -1 恒排最后：升序时若按裸值比它们会冒充最小值跑到最前。
      rows.sort((a, b) => {
        const av = MCOLS[mSortKey](a), bv = MCOLS[mSortKey](b);
        if (av === -1) return 1;
        if (bv === -1) return -1;
        return (av - bv) * mSortDir;
      });
    }
    const sth = (k, label) => '<th class="sortable" tabindex="0" role="button" data-msort="' + k + '">' + label +
      '<span class="sort-ind">' + (mSortKey === k ? (mSortDir === -1 ? ' ↓' : ' ↑') : '') + '</span></th>';
    const tt = sumTotals(allRows);
    const ttCost = allRows.reduce((a, m) => a + (m.est_cost || 0), 0);
    html += '<div class="panel"><h3>按模型 <span class="sub">' + esc(label) +
      (wide ? ' · 含长度分布/上下文填充/成本估算' : '') +
      ' · SLA=剔除客户端与429后的服务端成功率 · 点击模型筛选请求 · 点列头排序</span></h3>' +
      '<div class="scroll-x"><table><thead><tr><th>模型</th>' +
      sth('requests', '请求') + sth('sr', 'SLA') + sth('rate_limited', '429') +
      sth('input_tokens', '输入') + sth('output_tokens', '输出') + sth('cache_read_tokens', '缓存读') +
      sth('hit', '命中率') + sth('tps', '均速') +
      (wide ? sth('in_p50', '长度 p50/p95') + sth('fill', '上下文填充') +
        sth('est_cost', '估算成本') + sth('avg_duration_ms', '均耗时') + sth('avg_ttfb_ms', '均TTFB') + sth('last_at', '最近') : '') +
      '</tr></thead><tbody>';
    rows.forEach(m => {
      const sr = slaRate(m);
      const tps = tpsOf(m);
      // 长度分布单元格两行：↓输入 / ↑输出 各自的 p50/p95（蓄水池分位）。
      const lenCell = (m.input_p50 || m.output_p50)
        ? '↓' + fmtNum(m.input_p50) + ' / ' + fmtNum(m.input_p95) +
          '<div class="muted">↑' + fmtNum(m.output_p50) + ' / ' + fmtNum(m.output_p95) + '</div>'
        : '<span class="muted">—</span>';
      const fillCell = (m.context_fill_pct != null)
        ? m.context_fill_pct.toFixed(0) + '%<div class="muted" title="平均单请求占用 ÷ 上下文窗口">均 ' + fmtNum(Math.round(m.avg_context_tokens || 0)) + '/' + fmtNum(m.context_tokens) + '</div>'
        : '<span class="muted">—</span>';
      const costCell = (m.est_cost != null)
        ? money(m.est_cost) + (ttCost > 0 ? '<div class="share-bar" title="成本占比 ' + (m.est_cost / ttCost * 100).toFixed(1) + '%"><i style="width:' + Math.min(100, m.est_cost / ttCost * 100).toFixed(1) + '%"></i></div>' : '')
        : '<span class="muted">—</span>';
      html += '<tr><td class="mono"><button type="button" class="lnk" data-model="' + esc(m.name) + '">' + esc(m.name) + '</button></td>' +
        '<td class="num">' + m.requests + ' <span class="muted">(服 ' + (m.upstream_faults || 0) + ' 客 ' + (m.client_faults || 0) + ')</span></td>' +
        '<td class="num ' + (sr == null ? 'muted' : rateClass(sr)) + '">' + (sr == null ? '—' : sr.toFixed(0) + '%') + '</td>' +
        '<td class="num">' + (m.rate_limited || 0) + '</td>' +
        '<td class="mono">' + fmtNum(m.input_tokens) + '</td>' +
        '<td class="mono">' + fmtNum(m.output_tokens) + '</td>' +
        '<td class="mono">' + fmtNum(m.cache_read_tokens) + '</td>' +
        '<td class="mono">' + hitRate(m) + '</td>' +
        '<td class="mono">' + (tps >= 0 ? tps.toFixed(1) + ' tok/s' : '-') + '</td>' +
        (wide ? '<td class="mono">' + lenCell + '</td><td class="mono">' + fillCell + '</td>' +
          '<td>' + costCell + '</td>' +
          '<td class="mono ' + msClass(m.avg_duration_ms, 30000, 60000) + '">' + fmtMs(Math.round(m.avg_duration_ms || 0)) + '</td>' +
          '<td class="mono ' + msClass(m.avg_ttfb_ms, 5000, 10000) + '">' + fmtMs(Math.round(m.avg_ttfb_ms || 0)) + '</td>' +
          '<td class="mono muted" title="' + esc(m.last_at || '') + '">' + fmtRel(m.last_at) + '</td>' : '') + '</tr>';
    });
    // 加权合计行：命中率/decode 均速按总量加权重算，不按行平均；
    // SLA 用合计后的归因计数重算，同样不取行平均。
    const ttSla = slaRate(tt);
    html += '<tr class="total"><td class="mono muted">Σ 合计</td>' +
      '<td class="num">' + tt.requests + ' <span class="muted">(服 ' + tt.upstream_faults + ' 客 ' + tt.client_faults + ')</span></td>' +
      '<td class="num ' + (ttSla == null ? 'muted' : rateClass(ttSla)) + '">' + (ttSla == null ? '—' : ttSla.toFixed(0) + '%') + '</td>' +
      '<td class="num">' + tt.rate_limited + '</td>' +
      '<td class="mono">' + fmtNum(tt.input_tokens) + '</td><td class="mono">' + fmtNum(tt.output_tokens) + '</td>' +
      '<td class="mono">' + fmtNum(tt.cache_read_tokens) + '</td>' +
      '<td class="mono">' + hitRate(tt) + '</td><td class="mono">' + avgTps(tt) + '</td>' +
      (wide ? '<td></td><td></td><td>' + money(ttCost) + '</td><td></td><td></td><td></td>' : '') + '</tr>';
    html += '</tbody></table></div></div>';
  }

  // 按 key + 按天
  if (s.keys && s.keys.length) {
    html += '<div class="panel"><h3>按 API Key 哈希 <span class="sub">窗口累计 · 点击筛选请求</span></h3><div class="scroll-x"><table><thead><tr><th>Key 哈希</th><th>请求</th><th>错误</th><th>输出Token</th><th>最近</th></tr></thead><tbody>';
    s.keys.forEach(k => {
      html += '<tr><td class="mono"><button type="button" class="lnk" data-key="' + esc(k.name) + '">' + esc(k.name) + '</button></td><td class="num">' + k.requests + '</td><td class="num">' + k.errors + '</td><td class="mono">' + fmtNum(k.output_tokens) + '</td><td class="mono muted" title="' + esc(k.last_at || '') + '">' + fmtRel(k.last_at) + '</td></tr>';
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
  morph(body, html);
  drawCharts(pts, fine, tokenMix, barRows);
}

function drawCharts(pts, fine, tokenMix, barRows) {
  if (!pts.length && !tokenMix.length) return;
  const xs = p => (p.at !== undefined ? p.at : new Date(p.date + 'T00:00:00').getTime() / 1000);
  if (pts.length) {
    const flowSeries = [
      Charts.bar('请求', C.accent, pts.map(p => Charts.ts(xs(p), p.requests))),
      Charts.bar('错误', C.err, pts.map(p => Charts.ts(xs(p), p.errors))),
      Charts.bar('429', C.pink, pts.map(p => Charts.ts(xs(p), p.rate_limited))),
      Charts.line('输出 token', C.ok, pts.map(p => Charts.ts(xs(p), p.output_tokens)), { yAxisIndex: 1 }),
    ];
    const gm = Charts.gapMark(pts, fine ? 600 : 86400);
    if (gm) flowSeries[0].markArea = gm;
    Charts.render($('uFlow'), {
      dataZoom: Charts.zoom(pts),
      zoomKey: range,
      yAxis: [{}, { splitLine: { show: false }, axisLabel: { formatter: v => fmtNum(v), color: C.axis, fontSize: F.xs } }],
      series: flowSeries,
    });
    Charts.render($('uPerf'), {
      yAxis: [{}, { min: 0, max: 100, splitLine: { show: false }, axisLabel: { formatter: '{value}%', color: C.axis, fontSize: F.xs } }],
      series: [
        Charts.line('decode 均速', C.cyan, pts.map(p => Charts.ts(xs(p), p.gen_ms > 0 ? +(p.gen_tokens / (p.gen_ms / 1000)).toFixed(1) : null))),
        Charts.line('缓存命中率', C.warn, pts.map(p => {
          const dd = (p.cache_read_tokens || 0) + (p.input_tokens || 0);
          return Charts.ts(xs(p), dd > 0 ? +(p.cache_read_tokens / dd * 100).toFixed(1) : null);
        }), { yAxisIndex: 1, areaStyle: undefined }),
      ],
    });
    if (fine) {
      Charts.render($('uLat'), {
        series: [
          Charts.line('TTFB 均值', C.accent, pts.map(p => Charts.ts(xs(p), p.avg_ttfb_ms || null)), Charts.latencyMarks()),
          Charts.line('TTFB p95', C.warn, pts.map(p => Charts.ts(xs(p), p.ttfb_p95_ms || null))),
          Charts.line('耗时 p95', C.err, pts.map(p => Charts.ts(xs(p), p.duration_p95_ms || null))),
        ],
        tooltip: { trigger: 'axis', valueFormatter: v => v == null ? '-' : fmtMs(v) },
        yAxis: { axisLabel: { formatter: v => v >= 1000 ? (v / 1000) + 's' : v, color: C.axis, fontSize: F.xs }, splitLine: { lineStyle: { color: Charts.slate(0.08) } } },
      });
    }
  }
  if (tokenMix.length && $('uMix')) {
    Charts.render($('uMix'), {
      tooltip: { trigger: 'item', valueFormatter: v => fmtNum(v) },
      legend: { bottom: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, textStyle: { color: C.axis, fontSize: F.sm } },
      series: [{
        type: 'pie', radius: ['52%', '74%'], center: ['50%', '44%'],
        itemStyle: { borderColor: C.surface, borderWidth: 2, borderRadius: 4 },
        label: { show: false }, emphasis: { label: { show: true, color: C.text, fontSize: F.md, formatter: '{b}\n{d}%' } },
        data: tokenMix.map((x, i) => ({ name: x[0], value: x[1], itemStyle: { color: Charts.palette[i] } })),
      }],
    });
  }
  if (barRows.length && $('uModelBar')) {
    const rows = barRows.slice().reverse();
    Charts.render($('uModelBar'), {
      grid: { left: 8, right: 40, top: 8, bottom: 8, containLabel: true },
      xAxis: { type: 'value', axisLabel: { formatter: v => fmtNum(v), color: C.axis, fontSize: F.xs }, splitLine: { lineStyle: { color: Charts.slate(0.08) } } },
      yAxis: { type: 'category', data: rows.map(r => r.name), axisLabel: { color: C.dim, fontSize: F.xs, width: 130, overflow: 'truncate' }, axisLine: { lineStyle: { color: C.line } }, axisTick: { show: false } },
      tooltip: { trigger: 'axis', axisPointer: { type: 'shadow' }, formatter: ps => ps.map(p => esc(p.name) + '<br/>输出 ' + fmtNum(p.value) + ' tok').join('') },
      series: [{ type: 'bar', barMaxWidth: 14, itemStyle: { borderRadius: [0, 4, 4, 0], color: new window.echarts.graphic.LinearGradient(0, 0, 1, 0, [{ offset: 0, color: Charts.hexA(C.accent, 0.55) }, { offset: 1, color: C.accent }]) }, label: { show: true, position: 'right', color: C.axis, fontSize: 10, formatter: p => fmtNum(p.value) }, data: rows.map(r => r.output_tokens) }],
    });
  }
}

function applySort(k) {
  if (mSortKey !== k) { mSortKey = k; mSortDir = -1; }
  else mSortDir = mSortDir === -1 ? 1 : 0;
  savePref('usage.msort', { key: mSortKey, dir: mSortDir });
  render();
}

// 事件委托：chips / stage / model / key 链接 / 表头排序
document.getElementById('page-usage').addEventListener('click', e => {
  const st2 = e.target.closest('th[data-msort]');
  if (st2) { applySort(st2.dataset.msort); return; }
  const rc = e.target.closest('[data-range]');
  if (rc) { range = rc.dataset.range; savePref('usage.range', range); renderChips(); render(); return; }
  const st = e.target.closest('[data-stage]');
  if (st) { jumpRequests({ error_stage: st.dataset.stage }); return; }
  const mo = e.target.closest('[data-model]');
  if (mo) { jumpRequests({ model: mo.dataset.model }); return; }
  const ky = e.target.closest('[data-key]');
  if (ky) { jumpRequests({ q: ky.dataset.key }); return; }
});

// role=button 的 th 不自带键盘激活：Enter/Space 映射到同一排序动作，
// Space 要 preventDefault 否则页面滚动。
document.getElementById('page-usage').addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const st = e.target.closest('th[data-msort]');
  if (st) { e.preventDefault(); applySort(st.dataset.msort); }
});

renderChips();
Tabs.register('usage', load);
Polls.add('usage', load, 60000);
