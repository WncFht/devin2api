// 配额页：GET /admin/quota（quota_samples 采样曲线 + 燃烧速率预测）与
// GET /admin/status（账户/plan/容量/IDE/模型状态/供应商六路聚合）。
// 两源独立加载，任一失败只影响对应区块；全部上游字段经 escapeHtml 渲染。
// 呈现对齐旧面板配额页：KPI 卡（label+大值+预测副行）、kv 字段行、
// 供应商 chips、仅列异常模型——原始字段名不上屏。
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
    if (v === null || v === undefined || v === '') return null;
    const n = Number(v);
    return Number.isFinite(n) ? n : null;
  }

  function num(v) {
    const n = Number(v);
    if (!Number.isFinite(n)) return '—';
    return n.toLocaleString(undefined, { maximumFractionDigits: 2 });
  }

  function microUSD(v) {
    const n = Number(v);
    if (!Number.isFinite(n) || n === 0) return null;
    return '$' + (n / 1e6).toFixed(2);
  }

  function yesNo(v) { return v ? t('common.yes') : t('common.no'); }

  function boolBadge(v) {
    const yes = !!v;
    return `<span class="quota-bool quota-bool--${yes ? 'yes' : 'no'}">${esc(yesNo(v))}</span>`;
  }

  // unix 秒与 RFC3339 字符串双形态时刻（quota 点是 unix，status 里是 RFC3339）。
  function anyTime(v) {
    if (v === null || v === undefined || v === '' || v === 0) return null;
    if (typeof v === 'number') return new Date(v * 1000).toLocaleString();
    const d = new Date(v);
    return isNaN(d.getTime()) ? String(v) : d.toLocaleString();
  }

  // 套餐周期这类只关心日的字段渲染成本地日期。
  function fmtDay(v) {
    const d = new Date(v);
    return isNaN(d.getTime()) ? String(v || '') : `${d.getFullYear()}/${d.getMonth() + 1}/${d.getDate()}`;
  }

  // 距 unix 秒时刻的倒计时（"3天4小时"/"5小时12分"），已过期返回 null。
  function untilText(unixSec) {
    if (!unixSec) return null;
    const ms = unixSec * 1000 - Date.now();
    if (!Number.isFinite(ms) || ms <= 0) return null;
    const m = Math.round(ms / 60000);
    const d = Math.floor(m / 1440);
    const h = Math.floor((m % 1440) / 60);
    const mm = m % 60;
    if (d > 0) return t('quota.inDaysHours', { d, h });
    if (h > 0) return t('quota.inHoursMinutes', { h, m: mm });
    return t('quota.inMinutes', { m: Math.max(1, mm) });
  }

  function text(v) {
    if (v === null || v === undefined || v === '') return null;
    return String(v);
  }

  // forecast 形状：remaining/reset_at/burn_per_day 恒在；burn>0 时另有
  // exhausted_at（重置前烧完）或 survives_until_reset（本周期烧不完）。
  function burnText(f) {
    if (!f || f.burn_per_hour == null) return '';
    const rate = Number(f.burn_per_hour);
    if (rate <= 0) return t('quota.burn.refilled');
    const burn = t('quota.burn.rate', { rate: rate.toFixed(2) });
    if (f.survives_until_reset) return t('quota.burn.survives') + ' · ' + burn;
    if (f.exhausted_at) return t('quota.burn.exhaust', { h: Number(f.hours_left || 0).toFixed(1) }) + ' · ' + burn;
    return burn;
  }

  function kpiCard(label, valueHtml, sub, tone) {
    return `<div class="runtime-metric-card">
      <span class="runtime-metric-label">${esc(label)}</span>
      <strong class="runtime-metric-value"${tone ? ` style="color:${tone};"` : ''}>${valueHtml}</strong>
      ${sub ? `<span class="runtime-metric-sub">${sub}</span>` : ''}
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

  // kv 字段行：label 小字在上、值在下；值为 null 的字段整项不渲染。
  function kv(label, valueHtml) {
    if (valueHtml === null || valueHtml === undefined || valueHtml === '') return '';
    return `<div class="quota-kv"><span class="quota-kv-k">${esc(label)}</span><span class="quota-kv-v">${valueHtml}</span></div>`;
  }

  function kvGrid(items) {
    const inner = items.filter(Boolean).join('');
    return inner ? `<div class="quota-kv-grid">${inner}</div>` : '';
  }

  // 月度额度与可用余额合成「已用 X / 月 Y（剩 Z）」；monthly≤0 时只显示可用量。
  // 上游用 -1 表示不按固定额度计费——负值一律渲染成「不限」而不是裸数字。
  function creditUsage(monthly, available) {
    const m = Number(monthly);
    const a = available == null ? NaN : Number(available);
    if (Number.isFinite(a) && a < 0) return t('quota.creditUnlimited');
    if (!Number.isFinite(m) || m <= 0) return Number.isFinite(a) ? t('quota.creditAvail', { n: num(a) }) : null;
    if (!Number.isFinite(a)) return t('quota.creditMonthly', { n: num(m) });
    return t('quota.creditUsed', { used: num(Math.max(0, m - a)), total: num(m), avail: num(a) });
  }

  // ---- KPI ----

  function latestPoint() {
    const pts = quota && quota.points;
    return pts && pts.length ? pts[pts.length - 1] : null;
  }

  function toneFor(remaining) {
    if (remaining === null) return '';
    return remaining > 50 ? 'var(--success-600)' : remaining > 20 ? 'var(--warning-600)' : 'var(--error-600)';
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
    const dp = last ? pct(last.daily_remaining) : null;
    const wp = last ? pct(last.weekly_remaining) : null;
    const resetCard = (label, unixSec) => {
      const inTxt = untilText(unixSec);
      const at = anyTime(unixSec);
      return kpiCard(label, esc(inTxt || at || '—'), inTxt && at ? esc(at) : '');
    };
    grid.innerHTML = [
      kpiCard(t('quota.kpi.dailyRemaining'), dp === null ? '—' : dp.toFixed(1) + '%', esc(burnText(daily)), toneFor(dp)),
      kpiCard(t('quota.kpi.weeklyRemaining'), wp === null ? '—' : wp.toFixed(1) + '%', esc(burnText(weekly)), toneFor(wp)),
      resetCard(t('quota.kpi.dailyReset'), last && last.daily_reset_at),
      resetCard(t('quota.kpi.weeklyReset'), last && last.weekly_reset_at)
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

    const userItems = [
      kv(t('quota.f.name'), esc(text(u.name))),
      kv(t('quota.f.email'), esc(text(u.email))),
      kv(t('quota.f.pro'), u.pro === undefined ? null : boolBadge(u.pro)),
      kv(t('quota.f.teamsTier'), esc(text(u.teams_tier))),
      kv(t('quota.f.userId'), esc(text(u.user_id))),
      kv(t('quota.f.teamId'), esc(text(u.team_id)))
    ];

    const period = (text(p.plan_start) && text(p.plan_end))
      ? esc(fmtDay(p.plan_start) + ' ~ ' + fmtDay(p.plan_end))
      : null;
    const planItems = [
      kv(t('quota.f.planName'), esc(text(p.plan_name || pi.plan_name))),
      kv(t('quota.f.billingStrategy'), esc(text(p.billing_strategy || pi.billing_strategy))),
      kv(t('quota.f.promptCredits'), esc(creditUsage(p.monthly_prompt_credits ?? pi.monthly_prompt_credits, p.available_prompt_credits))),
      kv(t('quota.f.flowCredits'), esc(creditUsage(p.monthly_flow_credits ?? pi.monthly_flow_credits, p.available_flow_credits))),
      kv(t('quota.f.flexCredits'), esc(creditUsage(undefined, p.available_flex_credits))),
      kv(t('quota.f.acu'), p.acu_consumed === undefined && p.acu_limit === undefined ? null : esc(num(p.acu_consumed) + ' / ' + num(p.acu_limit))),
      kv(t('quota.f.overageBalance'), esc(microUSD(p.overage_balance_micros))),
      kv(t('quota.f.planPeriod'), period),
      kv(t('quota.f.isTeams'), (p.is_teams ?? pi.is_teams) === undefined ? null : boolBadge(p.is_teams ?? pi.is_teams)),
      kv(t('quota.f.isEnterprise'), (p.is_enterprise ?? pi.is_enterprise) === undefined ? null : boolBadge(p.is_enterprise ?? pi.is_enterprise)),
      kv(t('quota.f.graceStatus'), esc(text(p.grace_period_status))),
      kv(t('quota.f.graceEnd'), esc(anyTime(p.grace_period_end))),
      kv(t('quota.f.topUpEnabled'), tu.enabled === undefined ? null : boolBadge(tu.enabled)),
      kv(t('quota.f.topUpStatus'), esc(text(tu.transaction_status))),
      kv(t('quota.f.topUpMonthly'), tu.monthly_amount === undefined ? null : esc(num(tu.monthly_amount))),
      kv(t('quota.f.topUpSpent'), tu.spent === undefined ? null : esc(num(tu.spent)))
    ];

    const userGrid = kvGrid(userItems);
    const planGrid = kvGrid(planItems);
    root.innerHTML =
      (userGrid ? subsec('quota.userTitle', userGrid) : '') +
      (planGrid ? subsec('quota.planTitle', planGrid) : '') +
      (!userGrid && !planGrid ? `<div style="color:var(--color-text-secondary);">${esc(t('quota.noAccountData'))}</div>` : '');
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
      const capGrid = kvGrid([
        kv(t('quota.f.hasCapacity'), c.has_capacity === undefined ? null : boolBadge(c.has_capacity)),
        kv(t('quota.f.activeSessions'), c.active_sessions === undefined ? null : esc(num(c.active_sessions))),
        kv(t('quota.f.capacityMessage'), esc(text(c.message)))
      ]);
      if (capGrid) html += subsec('quota.health.capacity', capGrid);
    }

    // IDE 状态：level 缺席分两态——status_error 是拉取失败，否则是真无数据。
    if (status.status_error) {
      html += subsec('quota.health.ide', errBlock(status.status_error));
    } else if (status.ide_status) {
      const ide = status.ide_status;
      const level = ide.level && ide.level !== 'UNSPECIFIED' ? ide.level : '—';
      const ideGrid = kvGrid([
        kv(t('quota.f.ideLevel'), `<span style="color:${levelColor(ide.level)};font-weight:600;">${esc(level)}</span>`),
        kv(t('quota.f.ideMessage'), esc(text(ide.message))),
        kv(t('quota.f.reviewPrompt'), status.show_review_prompt === undefined ? null : boolBadge(status.show_review_prompt))
      ]);
      if (ideGrid) html += subsec('quota.health.ide', ideGrid);
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

    // 模型状态：全 OK 只报汇总行，有异常逐条列出。
    if (status.model_status_error) {
      html += subsec('quota.health.modelStatuses', errBlock(status.model_status_error));
    } else {
      const list = Array.isArray(status.model_statuses) ? status.model_statuses : [];
      const bad = list.filter((s) => !OK_STATUS.has(String(s.status || '').toLowerCase()));
      const inner = list.length === 0
        ? `<div style="color:var(--color-text-secondary);">${esc(t('quota.health.noData'))}</div>`
        : bad.length === 0
        ? `<div style="color:var(--color-text-secondary);">${esc(t('quota.health.allOk', { count: list.length }))}</div>`
        : `<div class="quota-kv-grid">${bad.map((s) =>
            kv(s.model_uid || s.model || '-',
              `<span style="color:${levelColor(s.status)};font-weight:600;">${esc(s.status)}</span>` +
              (s.message ? `<span style="color:var(--color-text-secondary);"> · ${esc(s.message)}</span>` : ''))
          ).join('')}</div>`;
      html += subsec('quota.health.modelStatuses', inner);
    }

    root.innerHTML = html;
  }
})();
