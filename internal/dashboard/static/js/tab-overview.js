// 概览页：KPI + 配额余量 + 60 分钟实时流量 + 进行中请求 + 告警。
// 数据分三层轮询：stats/active 10s（快变），usage/quota/status 60s（慢变）。

const Overview = (() => {
  let statsData = null, usageData = null, quotaData = null, statusData = null;

  function renderKpis() {
    if (!statsData || !usageData) return;
    const h = statsData.http || {};
    const r = h.rates || {};
    const s = usageData.snapshot || {};
    const today = s.today || {};
    const done = Math.max(1, today.requests || 0);
    const okN = done - (today.errors || 0) - (today.disconnected || 0);
    const lat = (statsData.usage && statsData.usage.ttfb) || {};
    const dur = (statsData.usage && statsData.usage.duration) || {};
    // 价目整体缺失或在用模型均无目录价时，成本无意义，显示占位符而非 $0.000。
    const priced = !usageData.price_missing && (usageData.models || []).some(m => m.est_cost > 0);
    const cost = priced ? money(usageData.est_cost) : '<span class="muted">—</span>';
    $('ovKpis').innerHTML =
      kpi('今日请求', fmtNum(today.requests || 0),
        '成功率 ' + (100 * okN / done).toFixed(0) + '% · 错 ' + (today.errors || 0) + ' · 断 ' + (today.disconnected || 0)) +
      kpi('输出 Tokens', fmtNum(today.output_tokens),
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
        '燃烧 ' + Number(d.burn_per_hour || 0).toFixed(2) + '%/h · 重置 ' + fmtUnix(last.daily_reset_at));
    }
    if (last.weekly_remaining != null) {
      html += qbar('周配额', last.weekly_remaining,
        (w.exhausted_at ? '预计 ' + fmtUnix(w.exhausted_at) + ' 耗尽 · ' : '') +
        '燃烧 ' + Number(w.burn_per_hour || 0).toFixed(3) + '%/h · 重置 ' + fmtUnix(last.weekly_reset_at));
    }
    el.innerHTML = html;
  }

  function renderTrend() {
    const tm = statsData && statsData.http && statsData.http.trend_minutes;
    if (!tm || !tm.length) return;
    Charts.render($('ovTrendChart'), {
      dataZoom: Charts.zoom(tm),
      yAxis: [{}, { show: false }],
      series: [
        Charts.bar('请求/30s', '#818cf8', Charts.tsList(tm, 'at', 'requests'), { barMaxWidth: 8 }),
        Charts.bar('错误/30s', '#f87171', Charts.tsList(tm, 'at', 'errors'), { yAxisIndex: 0, barMaxWidth: 8 }),
      ],
    });
  }

  async function loadActive() {
    try {
      const d = await api('/requests/active');
      const list = d.active || [];
      const panel = $('ovActivePanel');
      if (!list.length) { panel.style.display = 'none'; return; }
      panel.style.display = '';
      $('ovActiveBody').innerHTML = Requests.activeTable(list);
    } catch (e) { /* 静默，下轮重试 */ }
  }

  function renderAlerts() {
    if (!statusData) return;
    const d = statusData;
    let rows = '';
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
      renderKpis(); renderTrend();
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

  Tabs.register('overview', () => { refresh(); refreshSlow(); });
  onVisible('overview', refresh, 10000);
  onVisible('overview', refreshSlow, 60000);

  return { refresh };
})();
