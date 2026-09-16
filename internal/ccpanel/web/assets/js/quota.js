// 配额页：GET /admin/quota（quota.jsonl 采样曲线 + 燃烧速率预测）与
// GET /admin/status（账户/plan/容量/IDE/模型状态/供应商六路聚合）。
// 两源独立加载，任一失败只影响对应区块；全部上游字段经 escapeHtml 渲染。
(function () {
  const t = window.t;
  let quota = null;
  let status = null;
  let quotaErr = null;
  let statusErr = null;
  let chart = null;

  window.initPageBootstrap({
    topbarKey: 'quota',
    run: () => {
      document.getElementById('quota-refresh').addEventListener('click', loadAll);
      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(renderAll);
      }
      window.addEventListener('ccload:themechange', renderChart);
      window.addEventListener('resize', () => { if (chart) chart.resize(); });
      loadAll();
    }
  });

  async function loadAll() {
    const btn = document.getElementById('quota-refresh');
    btn.disabled = true;
    const [q, s] = await Promise.allSettled([
      window.fetchDataWithAuth('/admin/quota'),
      window.fetchDataWithAuth('/admin/status')
    ]);
    if (q.status === 'fulfilled') { quota = q.value; quotaErr = null; } else { quotaErr = q.reason; }
    if (s.status === 'fulfilled') { status = s.value; statusErr = null; } else { statusErr = s.reason; }
    renderAll();
    document.getElementById('quota-updated-at').textContent =
      t('quota.updatedAt', { time: new Date().toLocaleString() });
    btn.disabled = false;
  }

  function renderAll() {
    renderKpi();
    renderChart();
    renderAccount();
    renderHealth();
  }

  // ---- 格式化 ----

  function esc(v) { return window.escapeHtml(v); }

  function pct(v) {
    if (v === null || v === undefined || v === '') return '—';
    const n = Number(v);
    if (!Number.isFinite(n)) return '—';
    return (Math.round(n * 10) / 10) + '%';
  }

  function num(v) {
    const n = Number(v);
    if (!Number.isFinite(n)) return '—';
    return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  }

  function microUSD(v) {
    const n = Number(v);
    if (!Number.isFinite(n) || n === 0) return '—';
    return '$' + (n / 1e6).toFixed(2);
  }

  function yesNo(v) { return v ? t('common.yes') : t('common.no'); }

  // unix 秒与 RFC3339 字符串双形态时刻（quota 点是 unix，status 里是 RFC3339）。
  function anyTime(v) {
    if (v === null || v === undefined || v === '' || v === 0) return '—';
    if (typeof v === 'number') return new Date(v * 1000).toLocaleString();
    const d = new Date(v);
    return isNaN(d.getTime()) ? String(v) : d.toLocaleString();
  }

  function text(v) {
    if (v === null || v === undefined || v === '') return '—';
    return String(v);
  }

  function metricCard(label, value, key) {
    return `<div class="runtime-metric-card">
      <span class="runtime-metric-label">${esc(label)}</span>
      <strong class="runtime-metric-value">${esc(value)}</strong>
      <code class="runtime-metric-key">${esc(key)}</code>
    </div>`;
  }

  function errBlock(msg) {
    return `<div class="empty-state">
      <div class="empty-state-title empty-state-title--error">${esc(t('quota.sectionError'))}</div>
      <div>${esc(msg)}</div>
    </div>`;
  }

  function subsec(titleKey, inner) {
    return `<section class="runtime-metrics-subsection">
      <div class="runtime-metrics-subsection-header"><h4>${esc(t(titleKey))}</h4></div>
      ${inner}
    </section>`;
  }

  // ---- KPI ----

  function latestPoint() {
    const pts = quota && quota.points;
    return pts && pts.length ? pts[pts.length - 1] : null;
  }

  // forecast 形状：remaining/reset_at/burn_per_day 恒在；burn>0 时另有
  // exhausted_at（重置前烧完）或 survives_until_reset（本周期烧不完）。
  function forecastView(f) {
    if (!f) return ['—', 'forecast'];
    if (f.exhausted_at) return [anyTime(f.exhausted_at), 'exhausted_at'];
    if (f.survives_until_reset) return [t('quota.kpi.survivesUntilReset'), 'reset_at → ' + anyTime(f.reset_at)];
    return [t('quota.kpi.noBurn'), 'burn ≤ 0'];
  }

  function renderKpi() {
    const grid = document.getElementById('quota-kpi-grid');
    if (quotaErr) {
      grid.innerHTML = `<div style="grid-column:1/-1;">${errBlock(quotaErr.message || quotaErr)}</div>`;
      return;
    }
    const last = latestPoint();
    const daily = quota && quota.daily;
    const weekly = quota && quota.weekly;
    const [etaVal, etaKey] = forecastView(daily);
    grid.innerHTML = [
      metricCard(t('quota.kpi.dailyRemaining'), pct(last && last.daily_remaining), 'daily_quota_remaining'),
      metricCard(t('quota.kpi.weeklyRemaining'), pct(last && last.weekly_remaining), 'weekly_quota_remaining'),
      metricCard(t('quota.kpi.dailyBurn'), daily ? num(daily.burn_per_day) + '%' : '—', 'daily burn_per_day'),
      metricCard(t('quota.kpi.weeklyBurn'), weekly ? num(weekly.burn_per_day) + '%' : '—', 'weekly burn_per_day'),
      metricCard(t('quota.kpi.forecast'), etaVal, etaKey)
    ].join('');
  }

  // ---- 曲线 ----

  function setChartState(loading, err, empty) {
    document.getElementById('quota-chart-loading').style.display = loading ? 'flex' : 'none';
    document.getElementById('quota-chart-error').style.display = err ? 'flex' : 'none';
    const emptyEl = document.getElementById('quota-chart-empty');
    emptyEl.classList.toggle('hidden', !empty);
    emptyEl.style.display = empty ? 'flex' : 'none';
    document.getElementById('quota-chart').style.display = (!loading && !err && !empty) ? 'block' : 'none';
  }

  function renderChart() {
    const errMsg = document.getElementById('quota-chart-error-msg');
    if (quotaErr) {
      errMsg.textContent = quotaErr.message || String(quotaErr);
      setChartState(false, true, false);
      return;
    }
    const points = ((quota && quota.points) || []).filter((p) =>
      p && (p.daily_remaining !== undefined || p.weekly_remaining !== undefined));
    if (!points.length) {
      setChartState(false, false, true);
      return;
    }
    setChartState(false, false, false);
    const dom = document.getElementById('quota-chart');
    if (!chart) chart = echarts.init(dom, null, { renderer: 'canvas' });

    const theme = typeof window.getChartTheme === 'function' ? window.getChartTheme() : {};
    const mk = (name, color, pick) => ({
      name,
      type: 'line',
      smooth: 0.25,
      symbol: 'circle',
      symbolSize: 4,
      showSymbol: false,
      sampling: 'lttb',
      connectNulls: true,
      itemStyle: { color },
      lineStyle: { width: 2, color },
      areaStyle: {
        color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
          { offset: 0, color: color + '38' },
          { offset: 1, color: color + '00' }
        ])
      },
      data: points.filter((p) => pick(p) !== undefined && pick(p) !== null)
        .map((p) => [p.at * 1000, pick(p)])
    });

    chart.setOption({
      textStyle: { color: theme.text },
      tooltip: {
        trigger: 'axis',
        backgroundColor: theme.tooltipBg,
        borderColor: theme.tooltipBorder,
        textStyle: { color: theme.tooltipText },
        valueFormatter: (v) => (v === null || v === undefined ? '—' : Number(v).toFixed(1) + '%')
      },
      legend: { textStyle: { color: theme.mutedText }, top: 0 },
      grid: { left: 44, right: 16, top: 34, bottom: 28 },
      xAxis: {
        type: 'time',
        axisLine: { lineStyle: { color: theme.axisLine } },
        axisLabel: { color: theme.mutedText }
      },
      yAxis: {
        type: 'value',
        min: 0,
        max: 100,
        axisLabel: { color: theme.mutedText, formatter: '{value}%' },
        splitLine: { lineStyle: { color: theme.splitLine } }
      },
      series: [
        mk(t('quota.curveDaily'), '#3b82f6', (p) => p.daily_remaining),
        mk(t('quota.curveWeekly'), '#10b981', (p) => p.weekly_remaining)
      ]
    }, true);
    chart.resize();
  }

  // ---- 账户与计划 ----

  function renderAccount() {
    const root = document.getElementById('quota-account');
    if (statusErr) {
      root.innerHTML = errBlock(statusErr.message || statusErr);
      return;
    }
    if (!status) return; // 仍在加载
    if (status.user_status_error) {
      root.innerHTML = errBlock(status.user_status_error);
      return;
    }
    const u = status.user || {};
    const p = status.plan_status || {};
    const pi = status.plan_info || {};
    const tu = p.top_up_status || {};

    const userCards = [
      metricCard(t('quota.f.name'), text(u.name), 'name'),
      metricCard(t('quota.f.email'), text(u.email), 'email'),
      metricCard(t('quota.f.pro'), u.pro === undefined ? '—' : yesNo(u.pro), 'pro'),
      metricCard(t('quota.f.teamsTier'), text(u.teams_tier), 'teams_tier'),
      metricCard(t('quota.f.usedPromptCredits'), num(u.used_prompt_credits), 'used_prompt_credits'),
      metricCard(t('quota.f.usedFlowCredits'), num(u.used_flow_credits), 'used_flow_credits'),
      metricCard(t('quota.f.maxPremiumChat'), num(u.max_premium_chat), 'max_premium_chat'),
      metricCard(t('quota.f.userId'), text(u.user_id), 'user_id'),
      metricCard(t('quota.f.teamId'), text(u.team_id), 'team_id')
    ];

    const planName = p.plan_name || pi.plan_name;
    const planCards = [
      metricCard(t('quota.f.planName'), text(planName), 'plan_name'),
      metricCard(t('quota.f.billingStrategy'), text(p.billing_strategy || pi.billing_strategy), 'billing_strategy'),
      metricCard(t('quota.f.monthlyPromptCredits'), num(p.monthly_prompt_credits ?? pi.monthly_prompt_credits), 'monthly_prompt_credits'),
      metricCard(t('quota.f.monthlyFlowCredits'), num(p.monthly_flow_credits ?? pi.monthly_flow_credits), 'monthly_flow_credits'),
      metricCard(t('quota.f.availablePrompt'), num(p.available_prompt_credits), 'available_prompt_credits'),
      metricCard(t('quota.f.availableFlow'), num(p.available_flow_credits), 'available_flow_credits'),
      metricCard(t('quota.f.availableFlex'), num(p.available_flex_credits), 'available_flex_credits'),
      metricCard(t('quota.f.usedPromptCredits'), num(p.used_prompt_credits), 'used_prompt_credits'),
      metricCard(t('quota.f.usedFlowCredits'), num(p.used_flow_credits), 'used_flow_credits'),
      metricCard(t('quota.f.usedFlex'), num(p.used_flex_credits), 'used_flex_credits'),
      metricCard(t('quota.f.acu'), num(p.acu_consumed) + ' / ' + num(p.acu_limit), 'acu_consumed / acu_limit'),
      metricCard(t('quota.f.overageBalance'), microUSD(p.overage_balance_micros), 'overage_balance_micros'),
      metricCard(t('quota.f.planPeriod'), text(p.plan_start) + ' ~ ' + text(p.plan_end), 'plan_start ~ plan_end'),
      metricCard(t('quota.f.isTeams'), (p.is_teams ?? pi.is_teams) === undefined ? '—' : yesNo(p.is_teams ?? pi.is_teams), 'is_teams'),
      metricCard(t('quota.f.isEnterprise'), (p.is_enterprise ?? pi.is_enterprise) === undefined ? '—' : yesNo(p.is_enterprise ?? pi.is_enterprise), 'is_enterprise'),
      metricCard(t('quota.f.hasPaidFeatures'), (p.has_paid_features ?? pi.has_paid_features) === undefined ? '—' : yesNo(p.has_paid_features ?? pi.has_paid_features), 'has_paid_features'),
      metricCard(t('quota.f.graceStatus'), text(p.grace_period_status), 'grace_period_status'),
      metricCard(t('quota.f.graceEnd'), anyTime(p.grace_period_end), 'grace_period_end'),
      metricCard(t('quota.f.orphanedCut'), p.was_reduced_by_orphaned_usage === undefined ? '—' : yesNo(p.was_reduced_by_orphaned_usage), 'was_reduced_by_orphaned_usage'),
      metricCard(t('quota.f.topUpEnabled'), tu.enabled === undefined ? '—' : yesNo(tu.enabled), 'top_up_status.enabled'),
      metricCard(t('quota.f.topUpStatus'), text(tu.transaction_status), 'top_up_status.transaction_status'),
      metricCard(t('quota.f.topUpMonthly'), num(tu.monthly_amount), 'top_up_status.monthly_amount'),
      metricCard(t('quota.f.topUpSpent'), num(tu.spent), 'top_up_status.spent'),
      metricCard(t('quota.f.topUpIncrement'), num(tu.increment), 'top_up_status.increment'),
      metricCard(t('quota.f.topUpCriteriaMet'), tu.criteria_met === undefined ? '—' : yesNo(tu.criteria_met), 'top_up_status.criteria_met')
    ];

    root.innerHTML =
      subsec('quota.userTitle', `<div class="runtime-metrics-grid">${userCards.join('')}</div>`) +
      subsec('quota.planTitle', `<div class="runtime-metrics-grid">${planCards.join('')}</div>`);
  }

  // ---- 上游健康 ----

  const OK_STATUS = new Set(['', 'ok', 'available', 'normal', 'active', 'ready', 'enabled', 'operational', 'unspecified']);

  function levelColor(level) {
    const l = String(level || '').toLowerCase();
    if (OK_STATUS.has(l)) return 'var(--success-600)';
    if (['warning', 'degraded', 'warn'].includes(l)) return 'var(--warning-600)';
    return 'var(--error-600)';
  }

  function warnBlock(text) {
    return `<div style="margin-bottom:12px;padding:10px 14px;border-radius:8px;background:rgba(245,158,11,0.10);border:1px solid rgba(245,158,11,0.35);color:var(--warning-600,#d97706);font-size:13px;">${text}</div>`;
  }

  function renderHealth() {
    const root = document.getElementById('quota-health');
    if (statusErr) {
      root.innerHTML = errBlock(statusErr.message || statusErr);
      return;
    }
    if (!status) return;
    let html = '';

    if (status.alias_check_error) {
      html += warnBlock(esc(t('quota.health.aliasCheckError', { msg: status.alias_check_error })));
    }
    if (Array.isArray(status.alias_targets_absent) && status.alias_targets_absent.length) {
      html += warnBlock(esc(t('quota.health.aliasAbsent', { list: status.alias_targets_absent.join(', ') })));
    }
    if (Array.isArray(status.alias_shadows_catalog) && status.alias_shadows_catalog.length) {
      html += warnBlock(esc(t('quota.health.aliasShadow', { list: status.alias_shadows_catalog.join(', ') })));
    }

    // 聊天容量
    if (status.capacity_error) {
      html += subsec('quota.health.capacity', errBlock(status.capacity_error));
    } else {
      const c = status.capacity || {};
      html += subsec('quota.health.capacity', `<div class="runtime-metrics-grid">${[
        metricCard(t('quota.f.hasCapacity'), c.has_capacity === undefined ? '—' : yesNo(c.has_capacity), 'has_capacity'),
        metricCard(t('quota.f.activeSessions'), num(c.active_sessions), 'active_sessions'),
        metricCard(t('quota.f.capacityMessage'), text(c.message), 'message')
      ].join('')}</div>`);
    }

    // IDE 状态
    if (status.status_error) {
      html += subsec('quota.health.ide', errBlock(status.status_error));
    } else {
      const ide = status.ide_status || {};
      const level = text(ide.level);
      const levelHtml = `<span style="color:${levelColor(ide.level)};font-weight:600;">${esc(level)}</span>`;
      html += subsec('quota.health.ide', `<div class="runtime-metrics-grid">${[
        `<div class="runtime-metric-card"><span class="runtime-metric-label">${esc(t('quota.f.ideLevel'))}</span><strong class="runtime-metric-value">${levelHtml}</strong><code class="runtime-metric-key">ide_status.level</code></div>`,
        metricCard(t('quota.f.ideMessage'), text(ide.message), 'ide_status.message'),
        metricCard(t('quota.f.reviewPrompt'), status.show_review_prompt === undefined ? '—' : yesNo(status.show_review_prompt), 'show_review_prompt')
      ].join('')}</div>`);
    }

    // 供应商
    if (status.providers_error) {
      html += subsec('quota.health.providers', errBlock(status.providers_error));
    } else {
      const providers = Array.isArray(status.providers) ? status.providers : [];
      const chips = providers.length
        ? providers.map((p) => `<span class="model-tag" style="margin:0 6px 6px 0;display:inline-block;">${esc(p.display_name || p.provider)}</span>`).join('')
        : `<span style="color:var(--color-text-secondary);">${esc(t('quota.health.noProviders'))}</span>`;
      html += subsec('quota.health.providers', `<div>${chips}</div>`);
    }

    // 模型状态：全 OK 显示汇总，否则逐条列异常
    if (status.model_status_error) {
      html += subsec('quota.health.modelStatuses', errBlock(status.model_status_error));
    } else {
      const list = Array.isArray(status.model_statuses) ? status.model_statuses : [];
      const bad = list.filter((s) => !OK_STATUS.has(String(s.status || '').toLowerCase()));
      const inner = bad.length === 0
        ? `<div style="color:var(--color-text-secondary);">${esc(t('quota.health.allOk', { count: list.length }))}</div>`
        : `<div class="runtime-metrics-grid">${bad.map((s) =>
            `<div class="runtime-metric-card"><span class="runtime-metric-label">${esc(s.model_uid || s.model)}</span><strong class="runtime-metric-value" style="color:${levelColor(s.status)};">${esc(s.status)}</strong><code class="runtime-metric-key">${esc(s.message || '')}</code></div>`).join('')}</div>`;
      html += subsec('quota.health.modelStatuses', inner);
    }

    root.innerHTML = html;
  }
})();
