// accounts 页纯渲染层（契约见 notes/pool-accounts-contract.md）：
// window.acctView 产出逐号卡各区块的 html 串并持有 echarts 实例表。
// 不 fetch、不读写全局状态；输入一律按不可信处理——null/缺键对应块
// 返回 ''，任何字段异常都不许抛（pcore 在 innerHTML 拼装链上调用）。
// helpers 自 quota.js 移植（quota 页删并本页），i18n 键全部走 accounts.*。
(function () {
  const t = window.t;
  const esc = window.esc;
  const num = window.formatNumber;

  // ---- 格式化 helpers（quota.js 移植，键改指 accounts.*）----

  function text(v) {
    if (v === null || v === undefined || v === '') return null;
    return String(v);
  }

  function escN(v) {
    return v === null || v === undefined || v === '' ? null : esc(v);
  }

  function fmtN(v) {
    const n = Number(v);
    return Number.isFinite(n) ? num(n) : '—';
  }

  // unix 秒与 RFC3339 双形态时刻。
  function anyTime(v) {
    if (v === null || v === undefined || v === '' || v === 0) return null;
    if (typeof v === 'number') return new Date(v * 1000).toLocaleString();
    const d = new Date(v);
    return isNaN(d.getTime()) ? String(v) : d.toLocaleString();
  }

  // 距 unix 秒时刻的倒计时，已过期返回 null（调用方决定兜底）。
  function untilText(unixSec) {
    if (!unixSec) return null;
    const ms = unixSec * 1000 - Date.now();
    if (!Number.isFinite(ms) || ms <= 0) return null;
    const m = Math.round(ms / 60000);
    const d = Math.floor(m / 1440);
    const h = Math.floor((m % 1440) / 60);
    const mm = m % 60;
    if (d > 0) return t('accounts.inDaysHours', { d, h });
    if (h > 0) return t('accounts.inHoursMinutes', { h, m: mm });
    return t('accounts.inMinutes', { m: Math.max(1, mm) });
  }

  // ISO 时刻倒计时（lane/gate 冷却截止时间），过期回落 "soon"。
  function countdown(iso) {
    const ms = Date.parse(iso) - Date.now();
    if (!Number.isFinite(ms) || ms <= 0) return t('accounts.now');
    const m = Math.ceil(ms / 60000);
    if (m >= 1440) return t('accounts.inDaysHours', { d: Math.floor(m / 1440), h: Math.floor((m % 1440) / 60) });
    if (m >= 60) return t('accounts.inHoursMinutes', { h: Math.floor(m / 60), m: m % 60 });
    return t('accounts.inMinutes', { m });
  }

  function relTime(iso) {
    const ms = Date.now() - Date.parse(iso);
    if (!Number.isFinite(ms)) return '';
    const m = Math.floor(ms / 60000);
    if (m < 1) return t('accounts.justNow');
    if (m < 60) return t('accounts.minAgo', { m });
    const h = Math.floor(m / 60);
    if (h < 24) return t('accounts.hourAgo', { h, m: m % 60 });
    return t('accounts.dayAgo', { d: Math.floor(h / 24), h: h % 24 });
  }

  // forecast：remaining/reset_at/burn_per_hour 恒在；rate>0 时另有
  // exhausted_at（重置前烧完）或 survives_until_reset（本周期烧不完）。
  function burnText(f) {
    if (!f || f.burn_per_hour == null) return '';
    const rate = Number(f.burn_per_hour);
    if (rate <= 0) return t('accounts.burn.refilled');
    const burn = t('accounts.burn.rate', { rate: rate.toFixed(2) });
    if (f.survives_until_reset) return t('accounts.burn.survives') + ' · ' + burn;
    if (f.exhausted_at) return t('accounts.burn.exhaust', { h: Number(f.hours_left || 0).toFixed(1) }) + ' · ' + burn;
    return burn;
  }

  function microUSD(v) {
    const n = Number(v);
    if (!Number.isFinite(n) || n === 0) return null;
    return window.formatCost(n / 1e6);
  }

  function boolBadge(v) {
    return v
      ? `<span class="acct-yes">${esc(t('accounts.yes'))}</span>`
      : `<span class="acct-no">${esc(t('accounts.no'))}</span>`;
  }

  // quotaPoint 无 monthly 上限字段：used_*+available 两口径合成
  // 「已用 X · 可用 Y」；available<0 是上游「不按固定额度计费」语义。
  function creditUsed(used, avail) {
    const u = used === null || used === undefined ? NaN : Number(used);
    const a = avail === null || avail === undefined ? NaN : Number(avail);
    const hasU = Number.isFinite(u);
    const hasA = Number.isFinite(a);
    if (hasA && a < 0) return t('accounts.creditUnlimited');
    if (hasU && hasA) return t('accounts.creditUsed', { used: num(u), avail: num(a) });
    if (hasA) return t('accounts.creditAvail', { n: num(a) });
    if (hasU) return t('accounts.creditUsed', { used: num(u), avail: '—' });
    return null;
  }

  function kv(label, valueHtml) {
    if (valueHtml === null || valueHtml === undefined || valueHtml === '') return '';
    return `<div class="acct-kv"><span class="acct-kv-k">${esc(label)}</span><span class="acct-kv-v">${valueHtml}</span></div>`;
  }

  function kvGrid(items) {
    const inner = items.filter(Boolean).join('');
    return inner ? `<div class="acct-kv-grid">${inner}</div>` : '';
  }

  function sec(titleKey, inner, extraCls) {
    return `<section class="acct-sec${extraCls ? ' ' + extraCls : ''}"><h4>${esc(t(titleKey))}</h4>${inner}</section>`;
  }

  function toneFor(remaining) {
    if (remaining === null || remaining === undefined) return 'none';
    return remaining > 50 ? 'healthy' : remaining > 20 ? 'warning' : 'critical';
  }

  // ---- pills ----

  // 主徽章按优先级取头一个活跃异常；disabled/tombstoned 灰调最高优先
  // （非活 lane，其余状态位都是噪声，直接短路）。次 pill（排队/死区）并列。
  function pillList(a) {
    if (!a) return [];
    const now = Date.now();
    const lane = a.lane || {};
    const gate = a.gate || {};
    const out = [];
    const future = (iso) => iso && Date.parse(iso) > now;
    // 异常 pill 带 act='evidence'——点击开「为什么病了」证据抽屉（ops）。
    const sick = { act: 'evidence' };
    // has_override = config 声明的号带活覆盖行——「面板改过」注记徽标，
    // 与状态无关恒在末位（disabled 常由覆盖行造成，早退分支同样带上）。
    const overridePill = a.has_override === true ? [{ tone: 'idle', text: t('accounts.src.override') }] : [];
    if (a.source === 'tombstoned') return [{ tone: 'idle', text: t('accounts.st.tombstoned') }, ...overridePill];
    if (a.disabled) return [{ tone: 'idle', text: t('accounts.st.disabled') }, ...overridePill];
    if (future(lane.auth_cooldown_until)) {
      out.push({ tone: 'bad', text: t('accounts.st.credential', { left: countdown(lane.auth_cooldown_until) }), ...sick });
    }
    if (gate.latched) {
      out.push({ tone: 'bad', text: gate.limited_until ? t('accounts.st.latchedUntil', { left: countdown(gate.limited_until) }) : t('accounts.st.latched'), ...sick });
    }
    const q = a.quota || {};
    const exhausted = (q.daily && q.daily.remaining <= 0) || (q.weekly && q.weekly.remaining <= 0);
    if (exhausted) out.push({ tone: 'bad', text: t('accounts.st.exhausted'), ...sick });
    if (future(lane.unhealthy_until)) {
      out.push({ tone: 'warn', text: t('accounts.st.cooldown', { left: countdown(lane.unhealthy_until) }), ...sick });
    }
    if (!out.length) {
      if (!a.lane && !a.gate) out.push({ tone: 'idle', text: t('accounts.noData') });
      else if (lane.healthy === false) out.push({ tone: 'warn', text: t('accounts.st.unready'), ...sick });
      else out.push({ tone: 'ok', text: t('accounts.st.ok') });
    }
    if (gate.waiters > 0) out.push({ tone: 'warn', text: t('accounts.pill.waiters', { n: gate.waiters }) });
    if (gate.window_quota > 0 && gate.sendable === false && !gate.latched) {
      out.push({ tone: 'warn', text: t('accounts.pill.deadzone') });
    }
    return out.concat(overridePill);
  }

  // ---- 卡区块 ----

  function headBlock(a) {
    const u = (a.quota && a.quota.user) || {};
    const idParts = [u.email || u.name, u.plan_name].filter(Boolean);
    const identity = idParts.length ? `<span class="acct-identity">${esc(idParts.join(' · '))}</span>` : '';
    // tombstoned 不额外标来源——pillList 已出灰调墓碑徽章，不重复。
    const src = (a.source === 'config' || a.source === 'panel')
      ? `<span class="acct-pill acct-pill--idle">${esc(t('accounts.src.' + a.source))}</span>`
      : '';
    // priority 非零时显式徽标（0 是缺省不吵）；notes 有则随行显示。
    const pri = Number(a.priority) > 0
      ? `<span class="acct-pill acct-pill--idle" title="${esc(t('accounts.f.priority'))}">P${Number(a.priority)}</span>`
      : '';
    const notes = a.notes
      ? `<span class="acct-notes" title="${esc(String(a.notes))}">${esc(String(a.notes))}</span>`
      : '';
    return `<div class="acct-card-head">
      <div class="acct-card-title"><span class="acct-name">${esc(a.name || '')}</span>${identity}${src}${pri}${notes}</div>
      <div class="acct-badges">${pillsBlock(a)}</div>
    </div>`;
  }

  function pillsBlock(a) {
    const name = (a && a.name) || '';
    return pillList(a).map((p) => {
      const act = p.act ? ` data-act="${esc(p.act)}" data-acct="${esc(name)}" role="button" tabindex="0"` : '';
      return `<span class="acct-pill acct-pill--${p.tone}${p.act ? ' acct-pill--link' : ''}"${act}>${esc(p.text)}</span>`;
    }).join('');
  }

  function failureBlock(a) {
    const lane = a.lane || {};
    if (!lane.last_failure_at) return '';
    const code = lane.last_failure_code || t('accounts.st.unknownError');
    const msg = lane.last_failure_message ? ` — ${lane.last_failure_message}` : '';
    return `<div class="acct-failure acct-failure--link" data-act="evidence" data-acct="${esc(a.name || '')}" role="button" tabindex="0" title="${esc(code + msg)}">
      ${esc(t('accounts.lastFailure'))}: ${esc(code)} · ${esc(relTime(lane.last_failure_at))}
    </div>`;
  }

  function gateBlock(a) {
    const g = a.gate;
    if (!g) return '';
    const win = g.window_quota > 0
      ? `${num(g.window_used)} / ${num(g.window_quota)}`
      : t('accounts.f.unlimited');
    const inner = `<div class="acct-kv-grid">` + [
      kv(t('accounts.f.window'), esc(win)),
      kv(t('accounts.f.sendable'), boolBadge(g.sendable !== false)),
      kv(t('accounts.f.waiters'), esc(num(g.waiters || 0))),
      kv(t('accounts.f.latchCount'), esc(num(g.latch_count || 0))),
      kv(t('accounts.f.drip'), esc(num(g.drip_count || 0))),
      kv(t('accounts.f.rejects'), esc(num((g.reject_latched_count || 0) + (g.reject_budget_count || 0)))),
      kv(t('accounts.f.pendingWindows'), esc(num(g.pending_windows || 0)))
    ].join('') + `</div>`;
    return sec('accounts.sec.gate', inner);
  }

  function warmBlock(a) {
    const w = a.warm;
    if (!w) return '';
    if (w.enabled === false) return sec('accounts.sec.warm', `<div class="acct-none">${esc(t('accounts.warmOff'))}</div>`);
    // hits+misses=0 时 hit_rate 是除零兜底的 0——渲染成「—」不误导。
    const pingTotal = (w.ping_hits || 0) + (w.ping_misses || 0);
    const rate = pingTotal > 0 ? w.ping_hit_rate : null;
    const inner = `<div class="acct-kv-grid">` + [
      kv(t('accounts.f.hitRate'), rate === null
        ? `<span class="text-muted">—</span>`
        : `<span class="acct-rate tone-${toneFor(rate)}">${Number(rate).toFixed(0)}%</span>`),
      kv(t('accounts.f.entries'), esc(num(w.entries || 0))),
      kv(t('accounts.f.promoted'), esc(num(w.promoted || 0))),
      kv(t('accounts.f.pings'), esc(num(w.pings_sent || 0)))
    ].join('') + `</div>`;
    return sec('accounts.sec.warm', inner);
  }

  function quotaBar(label, f) {
    if (!f || f.remaining === null || f.remaining === undefined) return '';
    const pct = Math.max(0, Math.min(100, Number(f.remaining)));
    const sub = [
      f.reset_at ? t('accounts.f.resetIn', { left: untilText(f.reset_at) || t('accounts.now') }) : null,
      burnText(f)
    ].filter(Boolean).join(' · ');
    return `<div class="acct-quota-row">
      <span class="acct-quota-label">${esc(label)}</span>
      <div class="acct-quota-track"><div class="acct-quota-fill tone-${toneFor(pct)}" style="width:${pct}%;"></div></div>
      <span class="acct-quota-val tone-${toneFor(pct)}">${pct.toFixed(0)}%</span>
      ${sub ? `<div class="acct-quota-sub">${esc(sub)}</div>` : ''}
    </div>`;
  }

  function quotaBlock(a) {
    const q = a.quota;
    if (!q) return '';
    const bars = quotaBar(t('accounts.f.daily'), q.daily) + quotaBar(t('accounts.f.weekly'), q.weekly);
    const pts = Array.isArray(q.points) ? q.points : [];
    const last = pts.length ? pts[pts.length - 1] : null;
    const u = q.user || {};
    const grid = kvGrid([
      kv(t('accounts.f.pro'), u.pro === undefined || u.pro === null ? null : boolBadge(u.pro)),
      kv(t('accounts.f.teamsTier'), escN(text(u.teams_tier))),
      kv(t('accounts.f.billingStrategy'), escN(text(u.billing_strategy))),
      kv(t('accounts.f.promptCredits'), escN(last && creditUsed(last.used_prompt_credits, last.prompt_credits))),
      kv(t('accounts.f.flowCredits'), escN(last && creditUsed(last.used_flow_credits, last.flow_credits))),
      kv(t('accounts.f.flexCredits'), escN(last && creditUsed(last.used_flex_credits, last.flex_credits))),
      kv(t('accounts.f.acu'), last && (last.acu_consumed !== undefined || last.acu_limit !== undefined)
        ? esc(fmtN(last.acu_consumed) + ' / ' + fmtN(last.acu_limit)) : null),
      kv(t('accounts.f.graceStatus'), escN(last && text(last.grace_period_status))),
      kv(t('accounts.f.graceEnd'), escN(last && anyTime(last.grace_period_end))),
      kv(t('accounts.f.topUpEnabled'), !last || last.top_up_enabled === undefined ? null : boolBadge(last.top_up_enabled)),
      kv(t('accounts.f.topUpStatus'), escN(last && text(last.top_up_transaction_status)))
    ]);
    const inner = bars + grid;
    return inner ? sec('accounts.sec.quota', inner) : '';
  }

  function metricsBlock(a) {
    const items = [];
    const lane = a.lane || {};
    if (lane.inflight !== undefined && lane.inflight !== null) {
      items.push(kv(t('accounts.m.inflight'), esc(num(lane.inflight))));
    }
    const u = a.usage;
    if (u) {
      const td = u.today || {};
      if (td.requests !== undefined && td.requests !== null) {
        items.push(kv(t('accounts.m.requests'), esc(num(td.requests) + (td.tokens ? ' · ' + num(td.tokens) + ' tok' : ''))));
      }
      if (td.success_rate !== undefined && td.success_rate !== null) {
        items.push(kv(t('accounts.m.successRate'), esc(Number(td.success_rate * 100).toFixed(1) + '%')));
      }
      const ttfb = [['avg', u.ttfb_avg], ['p50', u.ttfb_p50], ['p90', u.ttfb_p90]]
        .filter(([, v]) => Number.isFinite(Number(v)))
        .map(([k, v]) => `${k} ${Math.round(Number(v))}ms`)
        .join(' · ');
      if (ttfb) items.push(kv(t('accounts.m.ttfb'), esc(ttfb)));
      if (u.cache_rate !== undefined && u.cache_rate !== null) {
        items.push(kv(t('accounts.m.cacheRate'), esc(Number(u.cache_rate * 100).toFixed(0) + '%')));
      }
      if (u.rpm_now !== undefined && u.rpm_now !== null) items.push(kv(t('accounts.m.rpm'), esc(fmtN(u.rpm_now))));
      if (u.tps_now !== undefined && u.tps_now !== null) items.push(kv(t('accounts.m.tps'), esc(fmtN(u.tps_now))));
    }
    const grid = kvGrid(items);
    return grid ? sec('accounts.sec.metrics', grid) : '';
  }

  function cellTone(c) {
    if (c.err) return 'err';
    if (c.rl) return 'rl';
    if (c.ok) return 'ok';
    return 'idle';
  }

  // 24×1h 健康条：桶内 失败>限流>成功。cells 由 pcore 从 matrix entries 归并。
  function healthBlock(a) {
    const cells = a.matrix && a.matrix.cells;
    if (!Array.isArray(cells) || !cells.length) return '';
    const strip = cells.map((c) => {
      const at = new Date(c.at);
      const label = isNaN(at) ? '' : at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
      const tip = `${label} · ${t('accounts.tip.ok', { n: c.ok || 0 })} ${t('accounts.tip.err', { n: c.err || 0 })} ${t('accounts.tip.rl', { n: c.rl || 0 })}${c.sw ? ' ' + t('accounts.tip.sw', { n: c.sw }) : ''}`;
      return `<div class="acct-cell acct-cell--${cellTone(c)}" title="${esc(tip)}"></div>`;
    }).join('');
    const total = cells.reduce((s, c) => s + (c.ok || 0) + (c.err || 0) + (c.rl || 0), 0);
    const sw = cells.reduce((s, c) => s + (c.sw || 0), 0);
    const foot = total
      ? t('accounts.recentFoot', { n: total }) + (sw ? ' · ' + t('accounts.tip.sw', { n: sw }) : '')
      : t('accounts.noRecent');
    return sec('accounts.sec.recent', `<div class="acct-strip">${strip}</div><div class="acct-strip-foot">${esc(foot)}</div>`);
  }

  // 曲线区整宽行：只产容器，echarts 由 pcore 在 mount 后调 curveInit 填。
  function curveBlock(a) {
    const q = a.quota;
    if (!q) return '';
    const pts = Array.isArray(q.points) ? q.points : [];
    const plottable = pts.some((p) => p && (p.daily_remaining !== undefined || p.weekly_remaining !== undefined));
    const inner = plottable
      ? `<div class="acct-curve" data-acct="${esc(a.name || '')}"></div>`
      : `<div class="acct-none">${esc(t('accounts.curveEmpty'))}</div><div class="acct-strip-foot">${esc(t('accounts.curveEmptyHint'))}</div>`;
    return sec('accounts.curveTitle', inner, 'acct-sec--curve');
  }

  function cardBlocks(a) {
    const safe = (fn) => {
      try { return fn(a || {}) || ''; } catch { return ''; }
    };
    return {
      head: safe(headBlock),
      pills: safe(pillsBlock),
      failure: safe(failureBlock),
      gate: safe(gateBlock),
      warm: safe(warmBlock),
      quota: safe(quotaBlock),
      metrics: safe(metricsBlock),
      health: safe(healthBlock),
      curve: safe(curveBlock)
    };
  }

  // ---- echarts 实例表（元素 → {chart, points}）----

  const charts = new Map();

  function curveOption(points) {
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
    return {
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
        mk(t('accounts.curveDaily'), '#3b82f6', (p) => p.daily_remaining),
        mk(t('accounts.curveWeekly'), '#10b981', (p) => p.weekly_remaining)
      ]
    };
  }

  // 幂等同步：同签名直接返回（自动刷新不闪），签名变才 setOption 更新数据；
  // 元素已脱离 DOM 的僵尸实例顺带回收（diff 重渲会换掉 .acct-curve 元素）。
  function curveInit(el, points) {
    if (!el) return;
    // echarts 懒加载：库未就位先拉再重入；拉取失败下轮自动刷新自然重试
    if (!window.echarts) {
      if (typeof window.ensureECharts === 'function') {
        window.ensureECharts().then(() => curveInit(el, points), () => {});
      }
      return;
    }
    for (const [node, it] of charts) {
      if (!node.isConnected) { it.chart.dispose(); charts.delete(node); }
    }
    const pts = (Array.isArray(points) ? points : []).filter((p) =>
      p && (p.daily_remaining !== undefined || p.weekly_remaining !== undefined));
    if (!pts.length) return;
    const sig = JSON.stringify(pts.map((p) => [p.at, p.daily_remaining ?? null, p.weekly_remaining ?? null]));
    const prev = charts.get(el);
    if (prev) {
      if (prev.sig === sig) return;
      prev.chart.setOption(curveOption(pts));
      prev.sig = sig;
      prev.points = pts;
      return;
    }
    const chart = echarts.init(el, null, { renderer: 'canvas' });
    chart.setOption(curveOption(pts), true);
    chart.resize();
    charts.set(el, { chart, points: pts, sig });
  }

  function disposeCharts() {
    for (const it of charts.values()) it.chart.dispose();
    charts.clear();
  }

  // 主题切换重建 option（颜色取自 getChartTheme），resize 只重排。
  window.addEventListener('ccload:themechange', () => {
    for (const it of charts.values()) {
      it.chart.setOption(curveOption(it.points), true);
      it.chart.resize();
    }
  });
  window.addEventListener('resize', () => {
    for (const it of charts.values()) it.chart.resize();
  });

  // ---- 空池整页态 ----

  function emptyStateHTML() {
    return `<div class="accounts-empty state-block">
      <div class="accounts-empty-icon"><svg viewBox="0 0 24 24" width="40" height="40" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round"><circle cx="12" cy="12" r="9"/><path d="M12 8v8M8 12h8"/></svg></div>
      <div class="accounts-empty-title state-title">${esc(t('accounts.empty.title'))}</div>
      <div class="accounts-empty-hint state-desc">${esc(t('accounts.empty.hint'))}</div>
      <div class="accounts-empty-form"><div id="accounts-empty-add"></div></div>
      <div id="accounts-cli-import"></div>
    </div>`;
  }

  // relTime/countdown 同时被 ops 层证据抽屉复用（loads 顺序 view→ops→core）
  window.acctView = { cardBlocks, pillList, curveInit, disposeCharts, emptyStateHTML, relTime, countdown };
})();
