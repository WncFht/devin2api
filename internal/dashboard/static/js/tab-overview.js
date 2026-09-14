// 概览页：KPI + 配额余量 + 60 分钟实时流量 + 进行中请求 + 告警。
// 数据分三层轮询：stats/active 10s（快变），usage/quota/status 60s（慢变）。

const Overview = (() => {
  let statsData = null, usageData = null, quotaData = null, statusData = null;

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
    const done = Math.max(1, today.requests || 0);
    const okN = done - (today.errors || 0) - (today.disconnected || 0);
    const lat = (statsData.usage && statsData.usage.ttfb) || {};
    const dur = (statsData.usage && statsData.usage.duration) || {};
    // 价目整体缺失或在用模型均无目录价时，成本无意义，显示占位符而非 $0.000。
    const priced = !usageData.price_missing && (usageData.models || []).some(m => m.est_cost > 0);
    const cost = priced ? money(usageData.est_cost) : '<span class="muted">—</span>';
    $('ovKpis').innerHTML =
      kpi('今日请求', fmtNum(today.requests || 0) + delta(today.requests || 0, yday.requests),
        '成功率 ' + (100 * okN / done).toFixed(0) + '% · 错 ' + (today.errors || 0) + ' · 断 ' + (today.disconnected || 0)) +
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

  // 健康时间线：120 个 30s 桶的双编码条（高度=相对请求量，颜色=最差结果）。
  function renderHealth() {
    const tm = statsData && statsData.http && statsData.http.trend_minutes;
    const el = $('ovHealth');
    if (!el || !tm || !tm.length) return;
    const max = Math.max(1, ...tm.map(p => p.requests + p.errors));
    let html = '';
    tm.forEach(p => {
      const n = (p.requests || 0) + (p.errors || 0);
      const cls = !n ? 'h-none' : p.errors ? 'h-err' : 'h-ok';
      const h = n ? Math.max(18, Math.round(n / max * 100)) : 12;
      html += '<i class="' + cls + '" style="height:' + h + '%" title="' +
        fmtTime(p.at * 1000) + ' · ' + n + ' 请求' + (p.errors ? ' · ' + p.errors + ' 错误' : '') + '"></i>';
    });
    el.innerHTML = html;
    const cap = $('ovHealthCap');
    if (cap) {
      const tot = tm.reduce((a, p) => a + (p.requests || 0), 0);
      const errs = tm.reduce((a, p) => a + (p.errors || 0), 0);
      cap.innerHTML = '<span>' + fmtTime(tm[0].at * 1000) + '</span><span>60 分钟 ' + tot + ' 请求 · ' + errs + ' 错误</span><span>' + fmtTime(tm[tm.length - 1].at * 1000) + '</span>';
    }
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
      const until = g.limited_until ? fmtTime(g.limited_until) + '（' + fmtIn(Date.parse(g.limited_until) / 1000) + '）' : '时刻未知';
      rows += '<div class="err-banner">速率闸门闩中：上游限流冷却至 ' + esc(until) +
        '，闩内请求本地快败 429（本次已累计 ' + (g.reject_latched_count || 0) + ' 条）</div>';
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
      renderKpis(); renderTrend(); renderHealth(); renderAlerts();
    } catch (e) { /* 保留旧数据 */ }
  }
  async function loadUsage() {
    try { usageData = await api('/usage'); renderKpis(); } catch (e) {}
  }
  async function loadQuota() {
    try { quotaData = await api('/quota'); renderQuota(); } catch (e) {}
  }
  async function loadStatus() {
    try { statusData = await api('/status'); renderAlerts(); } catch (e) {}
  }

  function refresh() { loadStats(); loadActive(); }
  function refreshSlow() { loadUsage(); loadQuota(); loadStatus(); }

  // 侧栏端口标识：取自当前地址栏，面板换端口时自动跟随。
  const gp = $('gwPort');
  if (gp) gp.textContent = ':' + (location.port || '80');

  Tabs.register('overview', () => { refresh(); refreshSlow(); });
  Polls.add('overview', refresh, 10000);
  Polls.add('overview', refreshSlow, 60000);

  return { refresh };
})();
