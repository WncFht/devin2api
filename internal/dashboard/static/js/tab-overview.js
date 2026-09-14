// 概览页：KPI + 配额余量 + 60 分钟实时流量 + 进行中请求 + 告警。
// 数据分两层轮询：stats/active/matrix 1s（快变），usage/quota/status 60s（慢变）。

import {
  $, api, esc, fmtNum, fmtMs, fmtTime, fmtUnix, fmtUnixShort, fmtIn,
  money, statusClass, slaRate, hitRate, kpi, qbar,
  titleBadge, Tabs, Polls, morph, summarizeRejects, gateLatchUntil,
} from './core.js';
import { Charts } from './charts.js';
import { jumpRequests, activeTable } from './tab-requests.js';

const C = Charts.C, F = Charts.F;
let statsData = null, usageData = null, quotaData = null, statusData = null, matrixData = null;
// 健康矩阵窗口：最近 30 分钟按 10 秒分桶（行=模型，格=桶）。
// 10s 粒度是「看清错误爆发的精确时刻」与格子可点可悬停（~4px）的折中；
// 若拉满 60 分钟需 360 格，格子会挤到 2px 以下失去可操作性。
const MX_BUCKETS = 180, MX_BUCKET_MS = 10000;
// 矩阵渲染状态：mxRowsData 是悬停提示的数据源（格子上只放索引），
// mxSig 是外观签名——1s 轮询下数据没变就跳过整棵字符串重建 + morph diff，
// 上千格子的解析成本不是免费的。
let mxRowsData = [], mxStart = 0, mxSig = '', mxHover = null, mxTip = null, mxFocus = null;
// alertCount 由 renderAlerts 写入，verdict 引用——替代 innerHTML 里
// 数 'err-banner' 字符串的脆写法（class 改名或文案撞词就静默算错）。
let alertCount = 0;

// delta：今日 vs 昨日同指标的环比箭头，昨日为 0 时不显示。
// 中性色：请求量/token 的增减是方向不是好坏，红绿暗示价值判断会误读。
function delta(cur, prev) {
  if (!prev) return '';
  const d = (cur - prev) / prev * 100;
  if (!Number.isFinite(d)) return '';
  return ' <span class="muted">' + (d >= 0 ? '↑' : '↓') + Math.abs(d).toFixed(0) + '%</span>';
}

function renderKpis() {
  if (!statsData || !usageData) return;
  const h = statsData.http || {};
  const r = h.rates || {};
  const s = usageData.snapshot || {};
  const today = s.today || {};
  const yday = ((s.days || []).find(d => {
    const y = new Date(); y.setDate(y.getDate() - 1);
    const pad = n => String(n).padStart(2, '0');
    return d.date === y.getFullYear() + '-' + pad(y.getMonth() + 1) + '-' + pad(y.getDate());
  })) || {};
  const lat = (statsData.usage && statsData.usage.ttfb) || {};
  const dur = (statsData.usage && statsData.usage.duration) || {};
  // 价目整体缺失或在用模型均无目录价时，成本无意义，显示占位符而非 $0.000。
  const priced = !usageData.price_missing && (usageData.models || []).some(m => m.est_cost > 0);
  const cost = priced ? money(usageData.est_cost) : '<span class="muted">—</span>';
  // 成功率走 SLA 口径（剔除客户端责任与 429）：服务端失分才是服务质量信号。
  const sla = slaRate(today);
  morph($('ovKpis'),
    kpi('今日请求', fmtNum(today.requests || 0) + delta(today.requests || 0, yday.requests),
      (sla == null ? '成功率 —' : 'SLA ' + sla.toFixed(1) + '%') +
      ' · 服务端 ' + (today.upstream_faults || 0) + ' · 客户端 ' + (today.client_faults || 0) + ' · 429 ' + (today.rate_limited || 0)) +
    kpi('输出 Tokens', fmtNum(today.output_tokens) + delta(today.output_tokens || 0, yday.output_tokens),
      '输入 ' + fmtNum(today.input_tokens), 'ok') +
    kpi('缓存命中率', hitRate(today),
      '读 ' + fmtNum(today.cache_read_tokens), 'cyan') +
    kpi('估算成本', cost,
      '窗口 ' + fmtNum((s.window || {}).total_tokens) + ' tok', 'warn') +
    kpi('活跃请求', h.active_requests ?? 0,
      'RPM ' + (r.rpm_current ?? 0) + ' / 峰 ' + (r.rpm_peak ?? 0), 'info') +
    kpi('上游 TTFB p50', fmtMs(lat.p50),
      '耗时 p50 ' + fmtMs(dur.p50), 'violet'));
  $('ovUpdated').textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
}

function renderQuota() {
  const el = $('ovQuotaPanel');
  if (!quotaData) return;
  const pts = quotaData.points || [];
  if (!pts.length) {
    morph(el, '<h3>配额余量</h3><div class="note">暂无配额快照——采样器按 debug.quota_interval_minutes 周期写入。</div>');
    return;
  }
  const last = pts[pts.length - 1];
  const d = quotaData.daily || {}, w = quotaData.weekly || {};
  let html = '<h3>配额余量</h3>';
  if (last.daily_remaining != null) {
    html += qbar('日配额', last.daily_remaining,
      (d.exhausted_at ? '约 ' + Number(d.hours_left || 0).toFixed(1) + 'h 后耗尽 · ' : '') +
      '燃烧 ' + Number(d.burn_per_hour || 0).toFixed(2) + '%/h · 重置 ' + fmtUnixShort(last.daily_reset_at) + '（' + fmtIn(last.daily_reset_at) + '）');
  }
  if (last.weekly_remaining != null) {
    html += qbar('周配额', last.weekly_remaining,
      (w.exhausted_at ? '预计 ' + fmtUnix(w.exhausted_at) + ' 耗尽 · ' : '') +
      '燃烧 ' + Number(w.burn_per_hour || 0).toFixed(3) + '%/h · 重置 ' + fmtUnixShort(last.weekly_reset_at) + '（' + fmtIn(last.weekly_reset_at) + '）');
  }
  morph(el, html);
}

// 健康矩阵：行=模型（首行总计）× 列=10 秒桶，双编码——颜色=桶内
// 最重归因（服务端失分>客户端/限流>全绿），深浅=请求量；空桶灰显，
// 「没流量」与「坏」不再同色。悬停出单格指标卡，点格带 模型+时间窗
// 下钻请求页。
// 数据来自 /requests 原始行而非预聚合：窗口小（~8rpm × 30min），
// 客户端分桶比后端另开一套 ring buffer 便宜且口径可现场核对。
function renderHealth() {
  const el = $('ovHealth');
  if (!el) return;
  const list = (matrixData && matrixData.entries) || [];
  const endSlot = Math.floor(Date.now() / MX_BUCKET_MS);
  const startSlot = endSlot - MX_BUCKETS;
  // 行：总计 + 窗口内请求量 Top6 模型；更多模型并进「其他」一行。
  const byModel = {};
  list.forEach(e => {
    const m = e.model || e.requested_model || '-';
    (byModel[m] = byModel[m] || []).push(e);
  });
  const top = Object.keys(byModel).sort((a, b) => byModel[b].length - byModel[a].length);
  const topSet = new Set(top.slice(0, 6));
  const rows = [{ label: '全部', pick: () => true, model: '' }];
  top.slice(0, 6).forEach(m => rows.push({ label: m, pick: e => (e.model || e.requested_model || '-') === m, model: m }));
  if (top.length > 6) {
    rows.push({ label: '其他 (' + (top.length - 6) + ')', pick: e => !topSet.has(e.model || e.requested_model || '-'), model: null });
  }
  rows.forEach(row => {
    // cells[i] = {n, sev, cli, up, lim, dur, tt, ttN, st}；sev 0绿 1琥珀
    // （客户端/限流） 2红（服务端）。dur/tt/st 只服务悬停卡，不进外观签名。
    const cells = new Array(MX_BUCKETS);
    list.forEach(e => {
      if (!row.pick(e)) return;
      const slot = Math.floor(Date.parse(e.started_at) / MX_BUCKET_MS) - startSlot;
      if (slot < 0 || slot >= MX_BUCKETS) return;
      const c = cells[slot] || (cells[slot] = { n: 0, sev: 0, cli: 0, up: 0, lim: 0, dur: 0, tt: 0, ttN: 0, st: {} });
      c.n++;
      c.dur += e.duration_ms || 0;
      if (e.first_upstream_ms != null) { c.tt += e.first_upstream_ms; c.ttN++; }
      const sc = e.status_code || '?';
      c.st[sc] = (c.st[sc] || 0) + 1;
      const owner = e.owner;
      if (owner === 'upstream') { c.up++; c.sev = 2; }
      else if (owner === 'client') { c.cli++; c.sev = Math.max(c.sev, 1); }
      else if (owner === 'business_limited') { c.lim++; c.sev = Math.max(c.sev, 1); }
    });
    row.cells = cells;
  });
  mxRowsData = rows;
  mxStart = startSlot;
  // 外观签名：行标签（模型进出 Top6 会换行）+ 每格 n（决定深浅，经行
  // 峰值归一）与 sev（决定色相），加 startSlot 与截断标记——桶边界滚动
  // 或截断状态翻转后同一批数据也要整体重画。签名不变就跳过字符串重建 + morph diff。
  const truncated = !!(matrixData && matrixData.truncated);
  const sig = startSlot + '|' + (truncated ? 'T' : 'F') + '|' + JSON.stringify(rows.map(r => [r.label, Array.from(r.cells, c => c ? [c.n, c.sev] : 0)]));
  if (sig !== mxSig) {
    mxSig = sig;
    let html = '';
    // 截断升级成琥珀 banner：埋在 caption 小字里排障时看不见。
    if (truncated) {
      html += '<div class="warn-banner">矩阵覆盖被截断：仅加载最近 ' + list.length +
        ' 条，更早时段可能缺失——完整历史用 grep 查 index.jsonl。</div>';
    }
    rows.forEach((row, ri) => {
      // cells 是稀疏数组（空桶无条目），Array.from 遍历含空位，
      // 直接 cells.map+展开会把空位展开成 undefined 污染 Math.max。
      const rowMax = Math.max(1, ...Array.from(row.cells, c => (c && c.n) || 0));
      let cellsHtml = '';
      for (let i = 0; i < MX_BUCKETS; i++) {
        const c = row.cells[i];
        // data-r/data-i 是 mxRowsData 的索引；data-s/data-u 供下钻钉时间窗。
        // 全部格子可聚焦（roving tabindex，键盘方向键导航 + 焦点悬停卡）；
        // 无数据格同样可读「无请求」，只是 Enter 不下钻。
        const base = ' tabindex="-1" role="gridcell" data-r="' + ri + '" data-i="' + i +
          '" data-s="' + new Date((startSlot + i) * MX_BUCKET_MS).toISOString() + '"';
        const sevWord = !c || !c.n ? '无请求' : c.sev === 2 ? '服务端失分' : c.sev === 1 ? '客户端或限流' : '正常';
        const alabel = ' aria-label="' + esc(row.label + ' ' + fmtTime((startSlot + i) * MX_BUCKET_MS) + ' ' +
          (c && c.n ? c.n + ' 条请求，' : '') + sevWord) + '"';
        if (!c) {
          cellsHtml += '<i class="h-none"' + base + alabel + '></i>';
          continue;
        }
        const cls = c.sev === 2 ? 'h-err' : c.sev === 1 ? 'h-warn' : 'h-ok';
        // 高度按行内峰值归一：每行各自呈现节奏，稀少量模型不被总计行压矮；
        // 20% 下限保证单请求桶仍是可见的条而非刻度线。
        const h = Math.round(20 + 80 * (c.n / rowMax));
        cellsHtml += '<i class="' + cls + '"' + base + alabel + ' data-n="' + c.n + '" data-m="' + esc(row.model || '') +
          '" data-u="' + new Date((startSlot + i + 1) * MX_BUCKET_MS).toISOString() + '" style="height:' + h + '%"></i>';
      }
      // 可下钻的行标签用 button 渲染（键盘可达）；「全部/其他」行无
      // 单一模型可筛，保持 span 不暗示可点。
      html += '<div class="mx-row">' +
        (row.model
          ? '<button type="button" class="mx-label" data-mx="' + esc(row.model) + '" title="' + esc(row.label) + '">' + esc(row.label) + '</button>'
          : '<span class="mx-label" title="' + esc(row.label) + '">' + esc(row.label) + '</span>') +
        '<div class="mx-cells">' + cellsHtml + '</div></div>';
    });
    html += '<div class="mx-legend"><span><i class="h-ok"></i>正常</span><span><i class="h-warn"></i>客户端 / 429</span>' +
      '<span><i class="h-err"></i>服务端失分</span><span><i class="h-none"></i>无请求</span>' +
      '<span>高度 = 行内相对请求量 · 方向键移格，Enter 下钻</span></div>';
    morph(el, html);
    // roving tabindex：整个矩阵只占一个 Tab 位。重建后把焦点还给等价格
    // （行序可能变，按 label 复核）；未持焦时把 Tab 入口钉在左上角第一格。
    if (mxFocus) {
      const again = el.querySelector('i[data-r="' + mxFocus.r + '"][data-i="' + mxFocus.i + '"]');
      if (again && mxRowsData[mxFocus.r] && mxRowsData[mxFocus.r].label === mxFocus.label) {
        again.tabIndex = 0;
        if (!mxEl.contains(document.activeElement)) again.focus();
      } else {
        mxFocus = null;
        const first = el.querySelector('.mx-cells i');
        if (first) first.tabIndex = 0;
      }
    } else {
      const first = el.querySelector('.mx-cells i');
      if (first) first.tabIndex = 0;
    }
    // morph 保节点身份，悬停格在属性级更新下存活；但行序变化（模型跌出
    // Top6）后旧格可能已代表另一行——按 label 复核，对不上就收提示。
    if (mxHover) {
      const again = el.querySelector('i[data-r="' + mxHover.r + '"][data-i="' + mxHover.i + '"]');
      if (again && mxRowsData[mxHover.r] && mxRowsData[mxHover.r].label === mxHover.label) mxShowTip(again);
      else mxHideTip();
    }
  }
  const cap = $('ovHealthCap');
  if (cap) {
    const tot = { up: 0, cli: 0, lim: 0 };
    list.forEach(e => {
      const o = e.owner;
      if (o === 'upstream') tot.up++; else if (o === 'client') tot.cli++; else if (o === 'business_limited') tot.lim++;
    });
    const mins = Math.round(MX_BUCKETS * MX_BUCKET_MS / 60000);
    cap.innerHTML = '<span>' + fmtTime(startSlot * MX_BUCKET_MS) + '</span><span>' + mins + ' 分钟 ' + list.length + ' 请求 · 服务端 ' + tot.up + ' · 客户端 ' + tot.cli + ' · 429 ' + tot.lim + '</span><span>' + fmtTime(endSlot * MX_BUCKET_MS) + '</span>';
  }
}

// ---------- 矩阵悬停提示 ----------
// 单例 tooltip 锚定在格子上方（GitHub 贡献图 tool-tip 同款：定位跟随
// 锚元素而非光标——格子只有几像素，跟光标会一路闪动）。事件委托挂
// 矩阵容器：mouseover/mouseout 冒泡可委托，mouseenter/leave 不冒泡；
// 数据读 mxRowsData，格子上只有 data-r/data-i 索引，上千个格子不各塞副本。
function mxTipEl() {
  if (!mxTip) {
    mxTip = document.createElement('div');
    mxTip.className = 'mx-tip';
    document.body.appendChild(mxTip);
  }
  return mxTip;
}

// mxCellTip 生成单格浮卡：头行=severity 色点+判词+模型（跟格子同色，
// 一眼对上号），次行=时间窗，分隔线后是 请求/状态码、归因、耗时三行。
function mxCellTip(r, i) {
  const row = mxRowsData[r];
  if (!row) return '';
  const c = row.cells[i];
  const at = new Date((mxStart + i) * MX_BUCKET_MS);
  const until = new Date((mxStart + i + 1) * MX_BUCKET_MS);
  // sev 文案按桶内实际构成细分：sev=1 可能是客户端责任、429 或两者混合。
  const sev = !c || !c.n ? ['none', '无请求']
    : c.sev === 2 ? ['err', '服务端失分']
    : c.sev === 1 ? ['warn', c.cli && c.lim ? '客户端+限流' : c.cli ? '客户端责任' : '429 限流']
    : ['ok', '正常'];
  let html = '<div class="mt-head"><i class="mt-dot d-' + sev[0] + '"></i><span class="mt-sev s-' + sev[0] + '">' + sev[1] +
    '</span><span class="mt-model">' + esc(row.label) + '</span></div>' +
    '<div class="mt-time">' + fmtTime(at) + ' – ' + fmtTime(until) + '</div>';
  if (!c || !c.n) return html + '<div class="mt-empty">该 10 秒内无请求</div>';
  const codes = Object.keys(c.st).sort((a, b) => c.st[b] - c.st[a]);
  const sts = codes.slice(0, 4).map(k => '<span class="' + statusClass(+k) + '">' + esc(k) + '</span>×' + c.st[k]);
  const more = codes.length > 4 ? ' · +' + (codes.length - 4) + '种' : '';
  const own = (label, n, cls) => '<span class="' + (n ? cls : 'muted') + '">' + label + ' ' + n + '</span>';
  let tm = '均耗时 ' + fmtMs(c.dur / c.n);
  if (c.ttN) tm += ' · 均 TTFB ' + fmtMs(c.tt / c.ttN);
  return html + '<div class="mt-sep"></div>' +
    '<div class="mt-line"><span class="k">请求</span><b>' + c.n + '</b><span class="mt-sp"></span>' +
    '<span class="k">状态</span><span>' + sts.join(' · ') + more + '</span></div>' +
    ((c.up || c.cli || c.lim)
      ? '<div class="mt-line">' + own('服务端', c.up, 'status-err') + '<span class="muted">·</span>' +
        own('客户端', c.cli, 'status-warn') + '<span class="muted">·</span>' + own('429', c.lim, 'status-rl') + '</div>'
      : '') +
    '<div class="mt-line muted">' + tm + '</div>';
}

function mxShowTip(cell) {
  const r = +cell.dataset.r, i = +cell.dataset.i;
  const row = mxRowsData[r];
  if (!row) return;
  mxHover = { r, i, label: row.label };
  const tip = mxTipEl();
  tip.innerHTML = mxCellTip(r, i) + '<i class="mt-arr"></i>';
  tip.style.display = 'block';
  // 锚定格子正上方居中，顶部空间不足翻到底部（.below 换箭头方位），
  // 横向钳进视口；箭头始终对准格子中心（随卡片钳位平移）。
  const cr = cell.getBoundingClientRect(), tr = tip.getBoundingClientRect();
  const left = Math.max(8, Math.min(cr.left + cr.width / 2 - tr.width / 2, window.innerWidth - tr.width - 8));
  const below = cr.top - tr.height - 8 < 4;
  tip.style.left = left + 'px';
  tip.style.top = (below ? cr.bottom + 8 : cr.top - tr.height - 8) + 'px';
  tip.classList.toggle('below', below);
  tip.classList.toggle('above', !below);
  tip.querySelector('.mt-arr').style.left =
    Math.max(10, Math.min(cr.left + cr.width / 2 - left - 4, tr.width - 18)) + 'px';
  // 同槽列高亮：跨行对齐同一 10 秒窗，方便对照各模型同一时刻的状态。
  mxEl.querySelectorAll('i.col-hl').forEach(x => x.classList.remove('col-hl'));
  mxEl.querySelectorAll('i[data-i="' + i + '"]').forEach(x => x.classList.add('col-hl'));
}

function mxHideTip() {
  if (!mxHover) return;
  mxHover = null;
  if (mxTip) mxTip.style.display = 'none';
  mxEl.querySelectorAll('i.col-hl').forEach(x => x.classList.remove('col-hl'));
}

// 判词：把闸门闩态、SLA 与告警压成一行结论——好的面板先回答
// 「要不要担心」，细节留给下面的卡片。
function renderVerdict() {
  const el = $('ovVerdict');
  if (!el) return;
  if (!statsData && !usageData) { el.innerHTML = ''; return; }
  const probs = [];
  const g = statsData && statsData.gate;
  if (g && g.latched) {
    probs.push(['err', '速率闸门闩中，冷却至 ' + gateLatchUntil(g)]);
  }
  // 本地拒绝突刺：管线前拒绝（排空/并发/鉴权）不进 index，SLA 与
  // 矩阵都看不见——部署窗口的 503 风暴只在事件环里。≥3 条/10 分钟
  // 才算突刺，个别乱入的 401 不告警。
  const rej = summarizeRejects(statsData && statsData.http && statsData.http.rejects, 10 * 60000);
  if (rej.n >= 3) {
    probs.push(['warn', '近 10 分钟本地拒绝 ' + rej.n + ' 条（' + rej.parts.join(' · ') + '）——不进请求索引，详见系统页']);
  }
  const today = (usageData && usageData.snapshot && usageData.snapshot.today) || {};
  const sla = slaRate(today);
  if (sla != null && sla < 95) {
    probs.push(['err', '今日服务端成功率 ' + sla.toFixed(1) + '%，失分 ' + (today.upstream_faults || 0) + ' 条']);
  } else if (sla != null && sla < 99.5) {
    probs.push(['warn', '今日服务端成功率 ' + sla.toFixed(1) + '%，有失分 ' + (today.upstream_faults || 0) + ' 条']);
  }
  if (alertCount > 0) probs.push(['warn', alertCount + ' 条告警待处理']);
  const active = (statsData && statsData.http && statsData.http.active_requests) || 0;
  if (!probs.length) {
    el.innerHTML = '<div class="verdict">运行平稳 · 今日 ' + fmtNum(today.requests || 0) +
      ' 请求 · SLA ' + (sla == null ? '—' : sla.toFixed(1) + '%') + ' · 在途 ' + active + ' 条 · 无告警</div>';
    return;
  }
  const level = probs.some(p => p[0] === 'err') ? 'v-err' : 'v-warn';
  el.innerHTML = '<div class="verdict ' + level + '">' + probs.map(p => esc(p[1])).join('；') + '</div>';
}

// 双百分位行：耗时与上游 TTFB 的 p50→max 并排。
function renderLatency() {
  const el = $('ovLatPanel');
  if (!el) return;
  const u = statsData && statsData.usage;
  const dur = u && u.duration, tt = u && u.ttfb;
  const row = (label, s) => (s && s.samples)
    ? '<div class="mini"><span class="k">' + label + '</span><span class="v">p50 ' + fmtMs(s.p50) +
      ' · p90 ' + fmtMs(s.p90) + ' · p95 ' + fmtMs(s.p95) + ' · p99 ' + fmtMs(s.p99) +
      ' · max ' + fmtMs(s.max) + '（n=' + s.samples + '）</span></div>'
    : '';
  const html = row('总耗时', dur) + row('上游 TTFB', tt);
  el.style.display = html ? '' : 'none';
  morph($('ovLatBody'), html);
}

// 闩时段在服务端随事件环同锁还原（gate.latch_ranges），这里只做展示
// 裁剪：裁到 [t0, now]、滤空段、映射成 markArea data 形态——起点在
// 事件环外不可考的时段（start 缺省）按视窗左缘补齐。
function gateLatchRanges(g, t0) {
  if (!g) return [];
  const now = Date.now();
  return (g.latch_ranges || [])
    .map(r => [r.start ? Date.parse(r.start) : t0, Date.parse(r.end)])
    .map(r => [Math.max(r[0], t0), Math.min(r[1], now)])
    .filter(r => r[1] > r[0])
    .map(r => [{ xAxis: r[0] }, { xAxis: r[1] }]);
}

// 实时流量：三层叠放——每 10s 瞬时速率柱（低饱和背景，表达离散到达
// 节奏）+ 30s 滑动均值 RPS 曲线（渐变面积前景，表达速率趋势）+
// 错误速率红条（barGap -100% 叠在同槽位上）。统一 req/s 单 y 轴；
// 十字线 tooltip 同时给原始条数与速率两种读数；底部不放缩放滑块，
// 滚轮/拖选缩放保留（inside zoom）。
function renderTrend() {
  const tm = statsData && statsData.http && statsData.http.trend_minutes;
  const el = $('ovTrendChart');
  if (!tm || !tm.length) { Charts.empty(el); return; }
  // 桶宽从数据推（相邻点间隔），后端粒度再调前端不用跟着改。
  const sec = tm.length > 1 ? tm[1].at - tm[0].at : 10;
  const req = tm.map(p => [p.at * 1000, p.requests / sec]);
  const err = tm.map(p => [p.at * 1000, p.errors / sec]);
  // RPS 曲线 = 当前点往前共 3 桶（30s）的均值速率：瞬时速率在低流量下
  // 只能取 0/0.1/0.2 几个台阶值，滑动窗口把台阶抹成趋势。
  const roll = tm.map((p, i) => {
    const w = tm.slice(Math.max(0, i - 2), i + 1);
    return [p.at * 1000, w.reduce((a, b) => a + b.requests, 0) / w.length / sec];
  });
  // 右轴 req/min 刻度镜像左轴 ×60：同一条 RPS 曲线按两个单位读数。
  // 两侧显式钉死 min/max/interval 保证刻度严格对齐；左轴上限取整齐值。
  const peak = Math.max(0.01, ...roll.map(p => p[1]), ...req.map(p => p[1]), ...err.map(p => p[1]));
  const NICE = [0.05, 0.1, 0.15, 0.2, 0.25, 0.3, 0.4, 0.5, 0.75, 1, 1.5, 2, 3, 4, 5, 7.5, 10, 15, 20, 30, 50, 100];
  const yMax = NICE.find(v => v >= peak * 1.15) || peak * 1.15;
  const series = [
    Charts.bar('请求速率', Charts.hexA(C.accent, 0.30), req, { barMaxWidth: 8, z: 1 }),
    Charts.bar('错误速率', C.err, err, { barMaxWidth: 8, barGap: '-100%', z: 2 }),
    Charts.line('RPS 30s均值', C.accent, roll, {
      z: 3,
      lineStyle: { width: 2, color: C.accent },
      areaStyle: { color: Charts.area(C.accent, 0.26, 0.02) },
    }),
  ];
  const gm = Charts.gapMark(tm, sec, 9);
  if (gm) series[0].markArea = gm;
  // 闩时段叠琥珀底色：拒绝风暴/流量塌陷与「当时在闩内」在图上直接对得上。
  const latchRanges = gateLatchRanges(statsData.gate, tm[0].at * 1000);
  if (latchRanges.length) {
    series[1].markArea = { silent: true, itemStyle: { color: Charts.hexA(C.warn, 0.12) }, data: latchRanges };
  }
  Charts.render(el, {
    dataZoom: [{ type: 'inside', xAxisIndex: 0, filterMode: 'none' }],
    yAxis: [
      { min: 0, max: yMax, interval: yMax / 4, name: 'req/s', nameTextStyle: { color: C.axis, fontSize: F.xs },
        axisLabel: { color: C.axis, fontSize: F.xs, formatter: v => +v.toFixed(2) } },
      { min: 0, max: yMax * 60, interval: yMax * 15, position: 'right', name: 'req/min', nameTextStyle: { color: C.axis, fontSize: F.xs },
        splitLine: { show: false },
        axisLabel: { color: C.axis, fontSize: F.xs, formatter: v => String(Math.round(v)) } },
    ],
    // tooltip 容器令牌由 base 统一供给（trigger/confine/axisPointer 同缺省），
    // 这里只给内容 formatter——排版与矩阵悬停卡同构：mt-head 色点+时间窗
    // 卡头、mt-sep 分隔、mt-line 指标行；marker 圆点视觉同 mt-dot。
    tooltip: {
      formatter: ps => {
        if (!ps || !ps.length) return '';
        const byName = {};
        ps.forEach(p => byName[p.seriesName] = p);
        const reqP = byName['请求速率'], errP = byName['错误速率'], rpsP = byName['RPS 30s均值'];
        const t0 = new Date(ps[0].axisValue);
        const n = reqP ? Math.round(reqP.value[1] * sec) : 0;
        const hasErr = errP && errP.value[1] > 0;
        // 卡头色点沿用矩阵语义：含错误红、有流量绿、空桶灰。
        const dot = hasErr ? 'd-err' : n ? 'd-ok' : 'd-none';
        let h = '<div class="mx-tip-inner"><div class="mt-head"><i class="mt-dot ' + dot + '"></i>' +
          '<span class="mt-sev mono">' + fmtTime(t0) + ' – ' + fmtTime(new Date(t0.getTime() + sec * 1000)) + '</span>' +
          '<span class="mt-model">' + sec + 's 桶</span></div><div class="mt-sep"></div>';
        if (reqP) h += '<div class="mt-line">' + reqP.marker + '<span class="k">请求</span><b>' + n + '</b> 条</div>';
        if (rpsP) h += '<div class="mt-line">' + rpsP.marker + '<span class="k">速率</span><b>' + rpsP.value[1].toFixed(2) + '</b> rps · ' + (rpsP.value[1] * 60).toFixed(1) + ' rpm</div>';
        if (hasErr) h += '<div class="mt-line">' + errP.marker + '<span class="k">错误</span><b class="status-err">' + Math.round(errP.value[1] * sec) + '</b> 条</div>';
        return h + '</div>';
      },
    },
    series,
  });
  // 面板副标题右侧放实时读数：当前 30s 均值速率的两种单位。
  const sub = $('ovTrendSub');
  if (sub) {
    const cur = roll[roll.length - 1][1];
    sub.textContent = '当前 ' + cur.toFixed(2) + ' rps · ' + (cur * 60).toFixed(1) + ' req/min';
  }
}

async function loadActive() {
  try {
    const d = await api('/requests/active');
    const list = d.active || [];
    titleBadge(list.length);
    const panel = $('ovActivePanel');
    if (!list.length) { panel.style.display = 'none'; return; }
    panel.style.display = '';
    morph($('ovActiveBody'), activeTable(list));
  } catch (e) { /* 静默，下轮重试 */ }
}

function renderAlerts() {
  const banners = [];
  // 闸门闩态来自 stats（10s 轮询）——闩中意味着正在对客户端快败
  // 429，是面板上最需要置顶的信号。
  const g = statsData && statsData.gate;
  if (g && g.latched) {
    banners.push('<div class="err-banner">速率闸门闩中：上游限流冷却至 ' + esc(gateLatchUntil(g)) +
      '，闩内请求本地快败 429（本次已快败 ' + (g.reject_latched_count || 0) + ' 条 · 滴灌放行 ' + (g.drip_count || 0) + ' 条）</div>');
  }
  const d = statusData;
  if (d) {
    (d.alias_targets_absent || []).forEach(a => {
      banners.push('<div class="err-banner">别名目标缺席：' + esc(a) + ' — 上游目录无此 uid，经别名的请求会被 permission_denied（改 devin.aliases）</div>');
    });
    if (d.alias_check_error) banners.push('<div class="err-banner">别名校验失败: ' + esc(d.alias_check_error) + '</div>');
    if (d.user_status_error) banners.push('<div class="err-banner">账户用量拉取失败: ' + esc(d.user_status_error) + '</div>');
    if (d.status_error) banners.push('<div class="err-banner">IDE 状态拉取失败: ' + esc(d.status_error) + '</div>');
    if (d.providers_error) banners.push('<div class="err-banner">渠道目录拉取失败: ' + esc(d.providers_error) + '</div>');
    if (d.model_status_error) banners.push('<div class="err-banner">模型状态拉取失败: ' + esc(d.model_status_error) + '</div>');
    if (d.capacity && d.capacity.has_capacity === false) {
      banners.push('<div class="err-banner">无可用容量: ' + esc(d.capacity.message || '上游容量满') + '（活跃会话 ' + (d.capacity.active_sessions ?? '-') + '）</div>');
    }
    if (d.ide_status && d.ide_status.level && !/^(OK|UNSPECIFIED|STATUS_LEVEL_OK)$/i.test(d.ide_status.level)) {
      banners.push('<div class="err-banner">IDE 状态 ' + esc(d.ide_status.level) + ': ' + esc(d.ide_status.message || '') + '</div>');
    }
    (d.model_statuses || []).forEach(s => {
      const st = String(s.status || '-');
      if (/WARN|ERROR|FATAL|DOWN/i.test(st)) {
        banners.push('<div class="err-banner">模型 ' + esc(s.model_uid || s.model || '-') + ': ' + esc(st) +
          (s.message ? ' — ' + esc(s.message) : '') + '</div>');
      }
    });
  }
  alertCount = banners.length;
  const panel = $('ovAlertPanel');
  panel.style.display = banners.length ? '' : 'none';
  morph($('ovAlertBody'), banners.join(''));
}

async function loadStats() {
  try {
    statsData = await api('/stats');
    const v = statsData.version || '';
    if (v) $('versionTag').textContent = v;
    renderKpis(); renderTrend(); renderLatency(); renderAlerts(); renderVerdict();
  } catch (e) { /* 保留旧数据 */ }
}
async function loadUsage() {
  try { usageData = await api('/usage'); renderKpis(); renderVerdict(); } catch (e) {}
}
async function loadQuota() {
  try { quotaData = await api('/quota'); renderQuota(); } catch (e) {}
}
async function loadStatus() {
  try { statusData = await api('/status'); renderAlerts(); renderVerdict(); } catch (e) {}
}
// 矩阵数据走 /requests/matrix 紧凑投影（只带分桶与归因字段，不分页、
// 扫描上限用满 2000）——替代原先借用列表端点 limit=500 盖不满窗口的口径。
// truncated 为真（上限打满或尾部窗没回溯到 since）时渲染层提示截断。
async function loadMatrix() {
  try {
    const since = new Date(Math.floor(Date.now() / MX_BUCKET_MS) * MX_BUCKET_MS - MX_BUCKETS * MX_BUCKET_MS).toISOString();
    matrixData = await api('/requests/matrix?since=' + encodeURIComponent(since));
    renderHealth();
  } catch (e) { /* 保留旧矩阵 */ }
}

function refresh() { loadStats(); loadActive(); }
function refreshSlow() { loadUsage(); loadQuota(); loadStatus(); }

// 侧栏端口标识：取自当前地址栏，面板换端口时自动跟随。
const gp = $('gwPort');
if (gp) gp.textContent = location.port ? ':' + location.port : '';

// 矩阵下钻：点格 → 请求页钉住 模型+该 10 秒时间窗；点行首模型名 → 只筛模型。
document.getElementById('page-overview').addEventListener('click', e => {
  mxHideTip();
  const cell = e.target.closest('.mx-cells i[data-n]');
  if (cell) {
    const kv = { since: cell.dataset.s, until: cell.dataset.u };
    if (cell.dataset.m) kv.model = cell.dataset.m;
    jumpRequests(kv);
    return;
  }
  const lbl = e.target.closest('.mx-label[data-mx]');
  if (lbl) jumpRequests({ model: lbl.dataset.mx });
});

// 矩阵悬停提示：委托 mouseover/mouseout 到容器。移出格子到非格子目标
// （邻格由它的 mouseover 接力）、滚屏、切页时收起；tooltip 自身
// pointer-events:none 不会成为 relatedTarget 造成闪烁。
const mxEl = $('ovHealth');
mxEl.addEventListener('mouseover', e => {
  const cell = e.target.closest('.mx-cells i');
  if (cell) mxShowTip(cell);
});
mxEl.addEventListener('mouseout', e => {
  const to = e.relatedTarget;
  if (!(to instanceof Element) || !to.closest('.mx-cells i')) mxHideTip();
});
// 键盘导航：roving tabindex——矩阵只占一个 Tab 位，方向键在格间移动
// （上下键按同一 10s 槽换行），焦点落格即出悬停卡，Enter 对有数据的
// 格子执行下钻（与鼠标点击同一路径）。
mxEl.addEventListener('focusin', e => {
  const cell = e.target.closest('.mx-cells i');
  if (!cell) return;
  mxEl.querySelectorAll('i[tabindex="0"]').forEach(x => { if (x !== cell) x.tabIndex = -1; });
  cell.tabIndex = 0;
  const row = mxRowsData[+cell.dataset.r];
  mxFocus = { r: +cell.dataset.r, i: +cell.dataset.i, label: row && row.label };
  mxShowTip(cell);
});
mxEl.addEventListener('focusout', e => {
  const to = e.relatedTarget;
  if (!(to instanceof Element) || !to.closest('.mx-cells i')) {
    mxFocus = null;
    mxHideTip();
  }
});
mxEl.addEventListener('keydown', e => {
  const cell = e.target.closest('.mx-cells i');
  if (!cell) return;
  if (e.key === 'Enter' || e.key === ' ') {
    if (!cell.dataset.n) return;
    e.preventDefault();
    const kv = { since: cell.dataset.s, until: cell.dataset.u };
    if (cell.dataset.m) kv.model = cell.dataset.m;
    jumpRequests(kv);
    return;
  }
  const dr = { ArrowRight: [0, 1], ArrowLeft: [0, -1], ArrowDown: [1, 0], ArrowUp: [-1, 0] }[e.key];
  if (!dr) return;
  e.preventDefault();
  const target = mxEl.querySelector('i[data-r="' + (+cell.dataset.r + dr[0]) + '"][data-i="' + (+cell.dataset.i + dr[1]) + '"]');
  if (target) target.focus();
});
window.addEventListener('scroll', mxHideTip, true);
window.addEventListener('hashchange', mxHideTip);

Tabs.register('overview', () => { refresh(); refreshSlow(); loadMatrix(); });
Polls.add('overview', refresh, 1000);
// 矩阵桶粒度 10s：独立慢轮询即可，不跟 stats/active 的 1s 节奏空拉。
Polls.add('overview', loadMatrix, 5000);
Polls.add('overview', refreshSlow, 60000);
