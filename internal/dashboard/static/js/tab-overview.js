// 概览页：KPI + 配额余量 + 60 分钟实时流量 + 进行中请求 + 告警。
// 数据分三层轮询：stats/active 10s（快变），usage/quota/status 60s（慢变）。

const Overview = (() => {
  let statsData = null, usageData = null, quotaData = null, statusData = null, matrixData = null;
  // 健康矩阵窗口：最近 60 分钟按 1 分钟分桶（行=模型，格=桶）。
  const MX_BUCKETS = 60, MX_BUCKET_MS = 60000;

  // delta：今日 vs 昨日同指标的环比箭头，昨日为 0 时不显示。
  function delta(cur, prev) {
    if (!prev) return '';
    const d = (cur - prev) / prev * 100;
    if (!Number.isFinite(d)) return '';
    const up = d >= 0;
    return ' <span style="color:var(--' + (up ? 'ok' : 'err') + ')">' + (up ? '↑' : '↓') + Math.abs(d).toFixed(0) + '%</span>';
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
    $('ovKpis').innerHTML =
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
        '耗时 p50 ' + fmtMs(dur.p50), 'violet');
    $('ovUpdated').textContent = '更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
  }

  function renderQuota() {
    const el = $('ovQuotaPanel');
    if (!quotaData) return;
    const pts = quotaData.points || [];
    if (!pts.length) {
      el.innerHTML = '<h3>配额余量</h3><div class="note">暂无配额快照——采样器按 debug.quota_interval_minutes 周期写入。</div>';
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
    el.innerHTML = html;
  }

  // 健康矩阵：行=模型（首行总计）× 列=1 分钟桶，双编码——颜色=桶内
  // 最重归因（服务端失分>客户端/限流>全绿），深浅=请求量；空桶灰显，
  // 「没流量」与「坏」不再同色。点格带 模型+时间窗 下钻请求页。
  // 数据来自 /requests 原始行而非预聚合：窗口小（~8rpm × 60min），
  // 客户端分桶比后端另开一套 ring buffer 便宜且口径可现场核对。
  function renderHealth() {
    const el = $('ovHealth');
    if (!el) return;
    const list = (matrixData && matrixData.requests) || [];
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
    let html = '';
    rows.forEach(row => {
      // cells[i] = {n, sev, cli, up, lim}；sev 0绿 1琥珀（客户端/限流） 2红（服务端）。
      const cells = new Array(MX_BUCKETS);
      list.forEach(e => {
        if (!row.pick(e)) return;
        const slot = Math.floor(Date.parse(e.started_at) / MX_BUCKET_MS) - startSlot;
        if (slot < 0 || slot >= MX_BUCKETS) return;
        const c = cells[slot] || (cells[slot] = { n: 0, sev: 0, cli: 0, up: 0, lim: 0 });
        c.n++;
        const owner = errorOwner(e);
        if (owner === 'upstream') { c.up++; c.sev = 2; }
        else if (owner === 'client') { c.cli++; c.sev = Math.max(c.sev, 1); }
        else if (owner === 'business_limited') { c.lim++; c.sev = Math.max(c.sev, 1); }
      });
      // cells 是稀疏数组（空桶无条目），Array.from 遍历含空位，
      // 直接 cells.map+展开会把空位展开成 undefined 污染 Math.max。
      const rowMax = Math.max(1, ...Array.from(cells, c => (c && c.n) || 0));
      let cellsHtml = '';
      for (let i = 0; i < MX_BUCKETS; i++) {
        const c = cells[i];
        const at = new Date((startSlot + i) * MX_BUCKET_MS);
        if (!c) {
          cellsHtml += '<i class="h-none" title="' + fmtTime(at) + ' · 无请求"></i>';
          continue;
        }
        const cls = c.sev === 2 ? 'h-err' : c.sev === 1 ? 'h-warn' : 'h-ok';
        // 深浅按行内峰值归一：每行各自呈现节奏，稀少量模型不被总计行压暗。
        const alpha = (0.3 + 0.7 * (c.n / rowMax)).toFixed(2);
        const until = new Date((startSlot + i + 1) * MX_BUCKET_MS);
        cellsHtml += '<i class="' + cls + '" data-n="' + c.n + '" data-m="' + esc(row.model || '') +
          '" data-s="' + at.toISOString() + '" data-u="' + until.toISOString() +
          '" style="opacity:' + alpha + '" title="' + fmtTime(at) + ' · ' + c.n + ' 请求' +
          (c.up ? ' · 服务端 ' + c.up : '') + (c.cli ? ' · 客户端 ' + c.cli : '') + (c.lim ? ' · 429 ' + c.lim : '') + '"></i>';
      }
      html += '<div class="mx-row"><span class="mx-label"' + (row.model ? ' data-mx="' + esc(row.model) + '"' : '') +
        ' title="' + esc(row.label) + '">' + esc(row.label) + '</span><div class="mx-cells">' + cellsHtml + '</div></div>';
    });
    el.innerHTML = html;
    const cap = $('ovHealthCap');
    if (cap) {
      const tot = { up: 0, cli: 0, lim: 0 };
      list.forEach(e => {
        const o = errorOwner(e);
        if (o === 'upstream') tot.up++; else if (o === 'client') tot.cli++; else if (o === 'business_limited') tot.lim++;
      });
      const more = (matrixData && matrixData.total > list.length) ? '（窗口早于列表扫描上限 ' + list.length + ' 条，矩阵可能截断）' : '';
      cap.innerHTML = '<span>' + fmtTime(startSlot * MX_BUCKET_MS) + '</span><span>60 分钟 ' + list.length + ' 请求 · 服务端 ' + tot.up + ' · 客户端 ' + tot.cli + ' · 429 ' + tot.lim + more + '</span><span>' + fmtTime(endSlot * MX_BUCKET_MS) + '</span>';
    }
  }

  // 判词：把闸门闩态、SLA 与告警压成一行结论（对齐 CPAMC hero verdict）——
  // 好的面板先回答「要不要担心」，细节留给下面的卡片。
  function renderVerdict() {
    const el = $('ovVerdict');
    if (!el) return;
    if (!statsData && !usageData) { el.innerHTML = ''; return; }
    const probs = [];
    const g = statsData && statsData.gate;
    if (g && g.latched) {
      const until = g.limited_until ? fmtTime(g.limited_until) + '（剩 ' + fmtInPrecise(Date.parse(g.limited_until) / 1000) + '）' : '时刻未知';
      probs.push(['err', '速率闸门闩中，冷却至 ' + until]);
    }
    const today = (usageData && usageData.snapshot && usageData.snapshot.today) || {};
    const sla = slaRate(today);
    if (sla != null && sla < 95) {
      probs.push(['err', '今日服务端成功率 ' + sla.toFixed(1) + '%，失分 ' + (today.upstream_faults || 0) + ' 条']);
    } else if (sla != null && sla < 99.5) {
      probs.push(['warn', '今日服务端成功率 ' + sla.toFixed(1) + '%，有失分 ' + (today.upstream_faults || 0) + ' 条']);
    }
    const alertN = (($('ovAlertBody') || {}).innerHTML || '').split('err-banner').length - 1;
    if (alertN > 0) probs.push(['warn', alertN + ' 条告警待处理']);
    const active = (statsData && statsData.http && statsData.http.active_requests) || 0;
    if (!probs.length) {
      el.innerHTML = '<div class="verdict">运行平稳 · 今日 ' + fmtNum(today.requests || 0) +
        ' 请求 · SLA ' + (sla == null ? '—' : sla.toFixed(1) + '%') + ' · 在途 ' + active + ' 条 · 无告警</div>';
      return;
    }
    const level = probs.some(p => p[0] === 'err') ? 'v-err' : 'v-warn';
    el.innerHTML = '<div class="verdict ' + level + '">' + probs.map(p => esc(p[1])).join('；') + '</div>';
  }

  // 双百分位行：耗时与上游 TTFB 的 p50→max 并排（对齐 sub2api ops 大屏）。
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
    $('ovLatBody').innerHTML = html;
  }

  function renderTrend() {
    const tm = statsData && statsData.http && statsData.http.trend_minutes;
    if (!tm || !tm.length) { Charts.empty($('ovTrendChart')); return; }
    const bars = [
      Charts.bar('请求/30s', '#818cf8', Charts.tsList(tm, 'at', 'requests'), { barMaxWidth: 8 }),
      Charts.bar('错误/30s', '#f87171', Charts.tsList(tm, 'at', 'errors'), { barMaxWidth: 8 }),
    ];
    const gm = Charts.gapMark(tm, 30);
    if (gm) bars[0].markArea = gm;
    Charts.render($('ovTrendChart'), {
      dataZoom: Charts.zoom(tm),
      series: bars,
    });
  }

  async function loadActive() {
    try {
      const d = await api('/requests/active');
      const list = d.active || [];
      titleBadge(list.length);
      const panel = $('ovActivePanel');
      if (!list.length) { panel.style.display = 'none'; return; }
      panel.style.display = '';
      $('ovActiveBody').innerHTML = Requests.activeTable(list);
    } catch (e) { /* 静默，下轮重试 */ }
  }

  function renderAlerts() {
    let rows = '';
    // 闸门闩态来自 stats（10s 轮询）——闩中意味着正在对客户端快败
    // 429，是面板上最需要置顶的信号。
    const g = statsData && statsData.gate;
    if (g && g.latched) {
      const until = g.limited_until ? fmtTime(g.limited_until) + '（剩 ' + fmtInPrecise(Date.parse(g.limited_until) / 1000) + '）' : '时刻未知';
      rows += '<div class="err-banner">速率闸门闩中：上游限流冷却至 ' + esc(until) +
        '，闩内请求本地快败 429（本次已快败 ' + (g.reject_latched_count || 0) + ' 条 · 滴灌放行 ' + (g.drip_count || 0) + ' 条）</div>';
    }
    const d = statusData;
    if (!d) { $('ovAlertPanel').style.display = rows ? '' : 'none'; $('ovAlertBody').innerHTML = rows; return; }
    (d.alias_targets_absent || []).forEach(a => {
      rows += '<div class="err-banner">别名目标缺席：' + esc(a) + ' — 上游目录无此 uid，经别名的请求会被 permission_denied（改 devin.aliases）</div>';
    });
    if (d.alias_check_error) rows += '<div class="err-banner">别名校验失败: ' + esc(d.alias_check_error) + '</div>';
    if (d.user_status_error) rows += '<div class="err-banner">账户用量拉取失败: ' + esc(d.user_status_error) + '</div>';
    if (d.capacity && d.capacity.has_capacity === false) {
      rows += '<div class="err-banner">无可用容量: ' + esc(d.capacity.message || '上游容量满') + '（活跃会话 ' + (d.capacity.active_sessions ?? '-') + '）</div>';
    }
    if (d.ide_status && d.ide_status.level && !/^(OK|UNSPECIFIED|STATUS_LEVEL_OK)$/i.test(d.ide_status.level)) {
      rows += '<div class="err-banner">IDE 状态 ' + esc(d.ide_status.level) + ': ' + esc(d.ide_status.message || '') + '</div>';
    }
    (d.model_statuses || []).forEach(s => {
      const st = String(s.status || '-');
      if (/WARN|ERROR|FATAL|DOWN/i.test(st)) {
        rows += '<div class="err-banner">模型 ' + esc(s.model_uid || s.model || '-') + ': ' + esc(st) +
          (s.message ? ' — ' + esc(s.message) : '') + '</div>';
      }
    });
    const panel = $('ovAlertPanel');
    panel.style.display = rows ? '' : 'none';
    $('ovAlertBody').innerHTML = rows;
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
  // 矩阵数据走 /requests 原始行（since 钉住窗口起点，list 超过单页
  // 上限时 caption 会标注截断）。
  async function loadMatrix() {
    try {
      const since = new Date(Math.floor(Date.now() / MX_BUCKET_MS) * MX_BUCKET_MS - MX_BUCKETS * MX_BUCKET_MS).toISOString();
      matrixData = await api('/requests?since=' + encodeURIComponent(since) + '&limit=500');
      renderHealth();
    } catch (e) { /* 保留旧矩阵 */ }
  }

  function refresh() { loadStats(); loadActive(); loadMatrix(); }
  function refreshSlow() { loadUsage(); loadQuota(); loadStatus(); }

  // 侧栏端口标识：取自当前地址栏，面板换端口时自动跟随。
  const gp = $('gwPort');
  if (gp) gp.textContent = ':' + (location.port || '80');

  // 矩阵下钻：点格 → 请求页钉住 模型+该分钟时间窗；点行首模型名 → 只筛模型。
  document.getElementById('page-overview').addEventListener('click', e => {
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

  Tabs.register('overview', () => { refresh(); refreshSlow(); });
  Polls.add('overview', refresh, 10000);
  Polls.add('overview', refreshSlow, 60000);

  return { refresh };
})();
