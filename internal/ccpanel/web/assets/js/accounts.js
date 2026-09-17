// 上游账号池页：三路数据源合并出逐 lane 视图——
//   /admin/runtime-metrics 的 accounts 组（gate/warm/lane 快照）
//   /admin/quota 的 accounts 组（quota_samples 逐号曲线 + 预测 + 身份快照）
//   /admin/logs/matrix?since=24h（逐请求 account 归因，聚合健康格与换号）
// 只读观测页：状态优先级参考 sub2api（倒计时即状态）、ccload 的行内
// pill 堆叠、cliproxy 的全池脉冲条与 <details> 收明细。字段未经 escapeHtml
// 不上屏。
(function () {
  const t = window.t;
  let runtime = null;
  let quota = null;
  let matrix = null;
  let runtimeErr = null;
  let quotaErr = null;
  let matrixErr = null;
  const WINDOW_HOURS = 24;

  window.initPageBootstrap({
    topbarKey: 'accounts',
    run: () => {
      document.getElementById('accounts-refresh').addEventListener('click', loadAll);
      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(renderAll);
      }
      loadAll();
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadAll }).init();
      }
    }
  });

  async function loadAll() {
    const btn = document.getElementById('accounts-refresh');
    btn.disabled = true;
    const since = new Date(Date.now() - WINDOW_HOURS * 3600e3).toISOString();
    const [r, q, m] = await Promise.allSettled([
      window.fetchDataWithAuth('/admin/runtime-metrics'),
      window.fetchDataWithAuth('/admin/quota'),
      window.fetchDataWithAuth('/admin/logs/matrix?since=' + encodeURIComponent(since))
    ]);
    if (r.status === 'fulfilled') { runtime = r.value; runtimeErr = null; } else { runtimeErr = r.reason; }
    if (q.status === 'fulfilled') { quota = q.value; quotaErr = null; } else { quotaErr = q.reason; }
    if (m.status === 'fulfilled') { matrix = m.value; matrixErr = null; } else { matrixErr = m.reason; }
    renderAll();
    document.getElementById('accounts-updated-at').textContent =
      t('accounts.updatedAt', { time: new Date().toLocaleString() });
    btn.disabled = false;
  }

  // ---- 数据归并 ----

  // 逐 lane 视图：名序稳定（排序），三源任一出现即建卡——config 里有但
  // 无流量的号、被移出号池但留有配额史的号都能落位。
  function accountList() {
    const rm = (runtime && runtime.accounts) || {};
    const qa = (quota && quota.accounts) || {};
    const names = new Set([...Object.keys(rm), ...Object.keys(qa)]);
    for (const e of (matrix && matrix.entries) || []) {
      if (e.account) names.add(e.account);
    }
    return [...names].sort().map((name) => {
      const slot = rm[name] || {};
      return {
        name,
        gate: slot.gate || null,
        warm: slot.warm || null,
        lane: slot.lane || null,
        report: qa[name] || null,
        user: (qa[name] && qa[name].user) || null
      };
    });
  }

  function matrixEntries(name) {
    return ((matrix && matrix.entries) || []).filter((e) => e.account === name);
  }

  // 状态主徽章按优先级取头一个活跃异常；次 pill（排队/死区）无条件并列。
  // 判定优先级：凭据冷却 > 闸门闩锁 > 配额耗尽 > 短冷却 > 不可发 > 正常。
  function pills(a) {
    const now = Date.now();
    const lane = a.lane || {};
    const gate = a.gate || {};
    const out = [];
    const future = (iso) => iso && Date.parse(iso) > now;
    if (future(lane.auth_cooldown_until)) {
      out.push({ tone: 'bad', text: t('accounts.st.credential', { left: countdown(lane.auth_cooldown_until) }) });
    }
    if (gate.latched) {
      out.push({ tone: 'bad', text: gate.limited_until ? t('accounts.st.latchedUntil', { left: countdown(gate.limited_until) }) : t('accounts.st.latched') });
    }
    const rep = a.report || {};
    const exhausted = (rep.daily && rep.daily.remaining <= 0) || (rep.weekly && rep.weekly.remaining <= 0);
    if (exhausted) out.push({ tone: 'bad', text: t('accounts.st.exhausted') });
    if (future(lane.unhealthy_until)) {
      out.push({ tone: 'warn', text: t('accounts.st.cooldown', { left: countdown(lane.unhealthy_until) }) });
    }
    if (!out.length) {
      if (lane.healthy === false) out.push({ tone: 'warn', text: t('accounts.st.unready') });
      else out.push({ tone: 'ok', text: t('accounts.st.ok') });
    }
    if (gate.waiters > 0) out.push({ tone: 'warn', text: t('accounts.pill.waiters', { n: gate.waiters }) });
    if (gate.window_quota > 0 && gate.sendable === false && !gate.latched) {
      out.push({ tone: 'warn', text: t('accounts.pill.deadzone') });
    }
    return out;
  }

  // 24 格健康条：桶=小时，优先级 失败>限流>成功——限流（本地闸门快败）
  // 与真失败分桶，同桶都有按失败计，避免限流粉饰上游错误。
  function healthCells(name) {
    const now = Date.now();
    const minT = now - WINDOW_HOURS * 3600e3;
    const cells = Array.from({ length: WINDOW_HOURS }, (_, i) => ({
      at: new Date(minT + i * 3600e3), ok: 0, err: 0, rl: 0, sw: 0
    }));
    for (const e of matrixEntries(name)) {
      const ts = Date.parse(e.started_at);
      if (!Number.isFinite(ts) || ts < minT) continue;
      const c = cells[Math.min(WINDOW_HOURS - 1, Math.floor((ts - minT) / 3600e3))];
      if (e.rate_limited) c.rl++;
      else if (e.result === 'completed' && e.status_code < 400) c.ok++;
      else c.err++;
      if (e.account_switches) c.sw += e.account_switches;
    }
    return cells;
  }

  function cellTone(c) {
    if (c.err) return 'err';
    if (c.rl) return 'rl';
    if (c.ok) return 'ok';
    return 'idle';
  }

  // ---- 格式化 ----

  const esc = window.escapeHtml;
  const num = window.formatNumber || ((v) => String(v));

  function fmtTime(unixSec) {
    return new Date(unixSec * 1000).toLocaleTimeString();
  }

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

  function toneFor(remaining) {
    if (remaining === null || remaining === undefined) return 'var(--color-text-secondary)';
    return remaining > 50 ? 'var(--success-600)' : remaining > 20 ? 'var(--warning-600)' : 'var(--error-600)';
  }

  // forecast 形状：remaining/reset_at/burn_per_hour 恒在；rate>0 时另有
  // exhausted_at（重置前烧完）或 survives_until_reset（本周期烧不完）。
  function forecastText(f) {
    if (!f || f.burn_per_hour == null) return null;
    const rate = Number(f.burn_per_hour);
    if (rate <= 0) return t('accounts.fc.stable');
    if (f.survives_until_reset) return t('accounts.fc.survives');
    if (f.exhausted_at) {
      return t('accounts.fc.exhaust', { time: fmtTime(f.exhausted_at), h: Number(f.hours_left || 0).toFixed(1) });
    }
    return null;
  }

  function kv(label, valueHtml) {
    if (valueHtml === null || valueHtml === undefined || valueHtml === '') return '';
    return `<div class="acct-kv"><span class="acct-kv-k">${esc(label)}</span><span class="acct-kv-v">${valueHtml}</span></div>`;
  }

  function errBlock(msg) {
    return `<div class="empty-state"><div class="empty-state-title empty-state-title--error">${esc(t('accounts.sectionError'))}</div><div>${esc(msg)}</div></div>`;
  }

  // ---- 渲染 ----

  function renderAll() {
    const list = accountList();
    renderPulse(list);
    renderKpi(list);
    renderCards(list);
  }

  function primaryTone(a) {
    const p = pills(a)[0];
    return p ? p.tone : 'idle';
  }

  function renderPulse(list) {
    const root = document.getElementById('accounts-pulse');
    if (!list.length) {
      root.innerHTML = `<span class="accounts-pulse-empty">${esc(t('accounts.noPool'))}</span>`;
      return;
    }
    root.innerHTML = list.map((a) => {
      const p = pills(a)[0];
      return `<div class="accounts-pulse-seg accounts-pulse-seg--${primaryTone(a)}"
        title="${esc(a.name)} · ${esc(p ? p.text : '')}"></div>`;
    }).join('');
  }

  function renderKpi(list) {
    const grid = document.getElementById('accounts-kpi-grid');
    if (runtimeErr && quotaErr && matrixErr) {
      grid.innerHTML = errBlock(runtimeErr.message || String(runtimeErr));
      return;
    }
    const entries = (matrix && matrix.entries) || [];
    let requests = 0;
    let errors = 0;
    let switches = 0;
    let rateLimited = 0;
    for (const e of entries) {
      requests++;
      if (e.rate_limited) rateLimited++;
      else if (!(e.result === 'completed' && e.status_code < 400)) errors++;
      if (e.account_switches) switches += e.account_switches;
    }
    const healthy = list.filter((a) => primaryTone(a) === 'ok').length;
    const card = (label, value, sub, tone) => `<div class="runtime-metric-card">
      <span class="runtime-metric-label">${esc(label)}</span>
      <strong class="runtime-metric-value"${tone ? ` style="color:${tone};"` : ''}>${value}</strong>
      ${sub ? `<span class="runtime-metric-sub">${sub}</span>` : ''}
    </div>`;
    grid.innerHTML = [
      card(t('accounts.kpi.lanes'), list.length, null, null),
      card(t('accounts.kpi.healthy'), healthy, list.length ? t('accounts.kpi.of', { n: list.length }) : null,
        healthy === list.length ? 'var(--success-600)' : 'var(--warning-600)'),
      card(t('accounts.kpi.requests'), num(requests), matrixErr ? esc(t('accounts.partial')) : null, null),
      card(t('accounts.kpi.errors'), num(errors), rateLimited ? t('accounts.kpi.rlSub', { n: rateLimited }) : null,
        errors ? 'var(--error-600)' : 'var(--success-600)'),
      card(t('accounts.kpi.switches'), num(switches), null, switches ? 'var(--warning-600)' : null)
    ].join('');
  }

  function identityLine(a) {
    const u = a.user || {};
    const parts = [u.email || u.name, u.plan_name].filter(Boolean);
    return parts.length ? `<span class="acct-identity">${esc(parts.join(' · '))}</span>` : '';
  }

  function failureLine(a) {
    const lane = a.lane || {};
    if (!lane.last_failure_at) return '';
    const code = lane.last_failure_code || t('accounts.st.unknownError');
    const msg = lane.last_failure_message ? ` — ${lane.last_failure_message}` : '';
    return `<div class="acct-failure" title="${esc(code + msg)}">
      ${esc(t('accounts.lastFailure'))}: ${esc(code)} · ${esc(relTime(lane.last_failure_at))}
    </div>`;
  }

  function gateBlock(a) {
    const g = a.gate;
    if (runtimeErr) return errBlock(runtimeErr.message || String(runtimeErr));
    if (!g) return `<div class="acct-none">${esc(t('accounts.noData'))}</div>`;
    const win = g.window_quota > 0
      ? `${num(g.window_used)} / ${num(g.window_quota)}`
      : t('accounts.f.unlimited');
    const bool = (v) => v ? `<span class="acct-yes">${esc(t('accounts.yes'))}</span>` : `<span class="acct-no">${esc(t('accounts.no'))}</span>`;
    return `<div class="acct-kv-grid">` + [
      kv(t('accounts.f.window'), esc(win)),
      kv(t('accounts.f.sendable'), bool(g.sendable !== false)),
      kv(t('accounts.f.waiters'), esc(num(g.waiters || 0))),
      kv(t('accounts.f.latchCount'), esc(num(g.latch_count || 0))),
      kv(t('accounts.f.drip'), esc(num(g.drip_count || 0))),
      kv(t('accounts.f.rejects'), esc(num((g.reject_latched_count || 0) + (g.reject_hold_count || 0))))
    ].join('') + `</div>`;
  }

  function warmBlock(a) {
    const w = a.warm;
    if (runtimeErr) return '';
    if (!w) return `<div class="acct-none">${esc(t('accounts.noData'))}</div>`;
    if (w.enabled === false) return `<div class="acct-none">${esc(t('accounts.warmOff'))}</div>`;
    // 无 ping 样本时命中率按未知渲染：hits+misses=0 时 rate 是除零兜底
    // 的 0，直接上屏会把「还没测过」误读成「全没中」。
    const pingTotal = (w.ping_hits || 0) + (w.ping_misses || 0);
    const rate = pingTotal > 0 ? w.ping_hit_rate : null;
    return `<div class="acct-kv-grid">` + [
      kv(t('accounts.f.hitRate'), rate === null
        ? `<span style="color:var(--color-text-secondary);">—</span>`
        : `<span style="color:${toneFor(rate)};font-weight:600;">${Number(rate).toFixed(0)}%</span>`),
      kv(t('accounts.f.entries'), esc(num(w.entries || 0))),
      kv(t('accounts.f.promoted'), esc(num(w.promoted || 0))),
      kv(t('accounts.f.pings'), esc(num(w.pings_sent || 0)))
    ].join('') + `</div>`;
  }

  function quotaBar(label, f) {
    if (!f || f.remaining === undefined || f.remaining === null) {
      return `<div class="acct-quota-row"><span class="acct-quota-label">${esc(label)}</span><span class="acct-none">${esc(t('accounts.noData'))}</span></div>`;
    }
    const pct = Math.max(0, Math.min(100, Number(f.remaining)));
    const sub = [
      f.reset_at ? t('accounts.f.resetIn', { left: countdown(new Date(f.reset_at * 1000).toISOString()) }) : null,
      forecastText(f)
    ].filter(Boolean).join(' · ');
    return `<div class="acct-quota-row">
      <span class="acct-quota-label">${esc(label)}</span>
      <div class="acct-quota-track"><div class="acct-quota-fill" style="width:${pct}%;background:${toneFor(pct)};"></div></div>
      <span class="acct-quota-val" style="color:${toneFor(pct)};">${pct.toFixed(0)}%</span>
      ${sub ? `<div class="acct-quota-sub">${esc(sub)}</div>` : ''}
    </div>`;
  }

  function quotaBlock(a) {
    if (quotaErr) return errBlock(quotaErr.message || String(quotaErr));
    const rep = a.report;
    if (!rep) return `<div class="acct-none">${esc(t('accounts.noQuota'))}</div>`;
    return quotaBar(t('accounts.f.daily'), rep.daily) + quotaBar(t('accounts.f.weekly'), rep.weekly);
  }

  function recentBlock(a) {
    if (matrixErr) return errBlock(matrixErr.message || String(matrixErr));
    const cells = healthCells(a.name);
    const strip = cells.map((c) => {
      const label = c.at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
      const tip = `${label} · ${t('accounts.tip.ok', { n: c.ok })} ${t('accounts.tip.err', { n: c.err })} ${t('accounts.tip.rl', { n: c.rl })}${c.sw ? ' ' + t('accounts.tip.sw', { n: c.sw }) : ''}`;
      return `<div class="acct-cell acct-cell--${cellTone(c)}" title="${esc(tip)}"></div>`;
    }).join('');
    const total = cells.reduce((s, c) => s + c.ok + c.err + c.rl, 0);
    const sw = cells.reduce((s, c) => s + c.sw, 0);
    const foot = total
      ? t('accounts.recentFoot', { n: total }) + (sw ? ' · ' + t('accounts.tip.sw', { n: sw }) : '')
      : t('accounts.noRecent');
    return `<div class="acct-strip">${strip}</div><div class="acct-strip-foot">${esc(foot)}</div>`;
  }

  function renderCards(list) {
    const root = document.getElementById('accounts-list');
    if (!list.length) {
      const msg = runtimeErr ? errBlock(runtimeErr.message || String(runtimeErr)) : `<div class="empty-state"><div class="empty-state-title">${esc(t('accounts.noPool'))}</div></div>`;
      root.innerHTML = `<div class="glass-card" style="padding:var(--space-6);">${msg}</div>`;
      return;
    }
    root.innerHTML = list.map((a) => {
      const badges = pills(a).map((p) => `<span class="acct-pill acct-pill--${p.tone}">${esc(p.text)}</span>`).join('');
      return `<div class="glass-card acct-card">
        <div class="acct-card-head">
          <div class="acct-card-title">
            <span class="acct-name">${esc(a.name)}</span>${identityLine(a)}
          </div>
          <div class="acct-badges">${badges}</div>
        </div>
        ${failureLine(a)}
        <div class="acct-grid">
          <section class="acct-sec"><h4>${esc(t('accounts.sec.gate'))}</h4>${gateBlock(a)}</section>
          <section class="acct-sec"><h4>${esc(t('accounts.sec.warm'))}</h4>${warmBlock(a)}</section>
          <section class="acct-sec"><h4>${esc(t('accounts.sec.quota'))}</h4>${quotaBlock(a)}</section>
          <section class="acct-sec"><h4>${esc(t('accounts.sec.recent'))}</h4>${recentBlock(a)}</section>
        </div>
      </div>`;
    }).join('');
  }
})();
