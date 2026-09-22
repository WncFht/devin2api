// accounts 页纯渲染层（契约见 notes/pool-accounts-contract.md）：
// window.acctView 产出行单元格与展开托盘的 html 串并持有 echarts 实例表。
// 不 fetch、不读写全局状态；输入一律按不可信处理——null/缺键对应块
// 返回 ''，任何字段异常都不许抛（core 在 innerHTML 拼装链上调用）。
(function () {
  const t = window.t;
  const esc = window.esc;
  const num = window.formatNumber;

  // ---- 格式化 helpers ----

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
  // has_override = config 声明的号带活覆盖行——「面板改过」注记徽标，
  // 与状态无关恒在末位（disabled 常由覆盖行造成，早退分支同样带上）。
  function pillList(a) {
    if (!a) return [];
    const now = Date.now();
    const lane = a.lane || {};
    const gate = a.gate || {};
    const out = [];
    const future = (iso) => iso && Date.parse(iso) > now;
    const overridePill = a.has_override === true ? [{ tone: 'idle', text: t('accounts.src.override') }] : [];
    if (a.source === 'tombstoned') return [{ tone: 'idle', text: t('accounts.st.tombstoned') }, ...overridePill];
    if (a.disabled) return [{ tone: 'idle', text: t('accounts.st.disabled') }, ...overridePill];
    if (future(lane.auth_cooldown_until)) {
      out.push({ tone: 'bad', text: t('accounts.st.credential', { left: countdown(lane.auth_cooldown_until) }) });
    }
    if (gate.latched) {
      if (gate.probing) out.push({ tone: 'warn', text: t('accounts.st.probing') });
      else out.push({ tone: 'bad', text: gate.limited_until ? t('accounts.st.latchedUntil', { left: countdown(gate.limited_until) }) : t('accounts.st.latched') });
    }
    const q = a.quota || {};
    const exhausted = (q.daily && q.daily.remaining <= 0) || (q.weekly && q.weekly.remaining <= 0);
    if (exhausted) out.push({ tone: 'bad', text: t('accounts.st.exhausted') });
    if (future(lane.unhealthy_until)) {
      out.push({ tone: 'warn', text: t('accounts.st.cooldown', { left: countdown(lane.unhealthy_until) }) });
    }
    if (!out.length) {
      if (!a.lane && !a.gate) out.push({ tone: 'idle', text: t('accounts.noData') });
      else if (lane.healthy === false) out.push({ tone: 'warn', text: t('accounts.st.unready') });
      else out.push({ tone: 'ok', text: t('accounts.st.ok') });
    }
    if (gate.waiters > 0) out.push({ tone: 'warn', text: t('accounts.pill.waiters', { n: gate.waiters }) });
    if (gate.window_quota > 0 && gate.sendable === false && !gate.latched) {
      out.push({ tone: 'warn', text: t('accounts.pill.deadzone') });
    }
    return out.concat(overridePill);
  }

  function pillsHTML(a, cap) {
    const pills = pillList(a).slice(0, cap || 3);
    return pills.map((p) =>
      `<span class="acct-pill acct-pill--${p.tone}">${esc(p.text)}</span>`).join('');
  }

  // ---- 行单元格 ----

  function cellAccount(a) {
    const u = (a.quota && a.quota.user) || {};
    const idParts = [u.email || u.name, u.plan_name].filter(Boolean);
    const identity = idParts.length ? `<div class="acct-id">${esc(idParts.join(' · '))}</div>` : '';
    const src = (a.source === 'config' || a.source === 'panel')
      ? `<span class="acct-tag">${esc(t('accounts.src.' + a.source))}</span>`
      : '';
    const pri = Number(a.priority) > 0
      ? `<span class="acct-tag" title="${esc(t('accounts.f.priority'))}">P${Number(a.priority)}</span>`
      : '';
    const notes = a.notes
      ? `<span class="acct-notes" title="${esc(String(a.notes))}">${esc(String(a.notes))}</span>`
      : '';
    return `<div class="acct-cell-name">
      <div class="acct-name-line"><span class="acct-name">${esc(a.name || '')}</span>${src}${pri}</div>
      ${identity}${notes}
    </div>`;
  }

  // 状态格 = 状态 pills（倒计时已烘进文案）+ 最近失败一行（24h 内才显示）。
  // 「为什么病了+何时恢复」一格收口，点开行进托盘看证据。
  function cellStatus(a) {
    const pills = pillsHTML(a, 3);
    const lane = a.lane || {};
    const fail = lane.last_failure_at && (Date.now() - Date.parse(lane.last_failure_at) < 24 * 3600e3)
      ? `<div class="acct-fail-line" title="${esc((lane.last_failure_code || '') + (lane.last_failure_message ? ' — ' + lane.last_failure_message : ''))}">${esc(t('accounts.ev.failure'))} ${esc(relTime(lane.last_failure_at))} · ${esc(lane.last_failure_code || t('accounts.st.unknownError'))}</div>`
      : '';
    return `<div class="acct-cell-status"><div class="acct-badges">${pills}</div>${fail}</div>`;
  }

  function miniBar(label, f) {
    if (!f || f.remaining === null || f.remaining === undefined) return '';
    const pct = Math.max(0, Math.min(100, Number(f.remaining)));
    return `<div class="acct-mini-quota" title="${esc(label)} ${pct.toFixed(0)}%${f.reset_at ? ' · ' + esc(t('accounts.f.resetIn', { left: untilText(f.reset_at) || t('accounts.now') })) : ''}">
      <span class="acct-mini-label">${esc(label)}</span>
      <span class="acct-mini-track"><span class="acct-mini-fill tone-bg-${toneFor(pct)}" style="width:${pct}%;"></span></span>
      <span class="acct-mini-val tone-${toneFor(pct)}">${pct.toFixed(0)}%</span>
    </div>`;
  }

  // 配额格：日/周双迷你条 + 一行余量语义（最近重置或燃烧外推）；
  // 冻结序列（stale）显式标出——采样停更是「该号拉取在失败」的信号。
  function cellQuota(a) {
    const q = a.quota;
    // seat_gated 挂在 report.user（采样侧 1h 缓存判定）——individual plan
    // 号恒无采样，用专属文案区别于「还没采到」。
    const gated = !!(q && q.user && q.user.seat_gated === true);
    if (!q || (!q.daily && !q.weekly)) {
      return `<span class="acct-none">${esc(gated ? t('accounts.st.seatGated') : t('accounts.noQuota'))}</span>`;
    }
    const bars = miniBar(t('accounts.f.daily'), q.daily) + miniBar(t('accounts.f.weekly'), q.weekly);
    const sub = q.stale
      ? `<div class="acct-quota-sub acct-quota-sub--stale">${esc(t('accounts.stale'))}</div>`
      : (burnText(q.daily) || burnText(q.weekly)
        ? `<div class="acct-quota-sub">${esc(burnText(q.daily) || burnText(q.weekly))}</div>`
        : '');
    return `<div class="acct-cell-quota">${bars}${sub}</div>`;
  }

  function cellToday(a) {
    const u = a.usage;
    if (!u || !u.today || u.today.requests === undefined || u.today.requests === null) {
      return `<span class="acct-none">—</span>`;
    }
    const td = u.today;
    const rate = td.success_rate !== undefined && td.success_rate !== null
      ? `<span class="tone-${td.success_rate >= 0.99 ? 'healthy' : td.success_rate >= 0.9 ? 'warning' : 'critical'}">${(td.success_rate * 100).toFixed(1)}%</span>`
      : '';
    const tokens = td.tokens ? `<div class="acct-today-sub">${num(td.tokens)} tok</div>` : '';
    return `<div class="acct-cell-today"><div>${esc(t('accounts.today.req', { n: num(td.requests) }))} · ${rate}</div>${tokens}</div>`;
  }

  // 桶着色按份额而非有即染：err 是上游责任失败（owner 归因，客户端断连
  // 已剔），高流量下每格都有个位数基线故障，≥5% 才算 lane 真的出问题；
  // rl 零基线，一次限流即染青。
  function cellTone(c) {
    const tot = (c.ok || 0) + (c.err || 0) + (c.rl || 0);
    if (!tot) return 'idle';
    if ((c.err || 0) / tot >= 0.05) return 'err';
    if (c.rl) return 'rl';
    return 'ok';
  }

  // 48×30min 健康条：桶内 失败>限流>成功；限流染青与真错误分色。
  function cellHealth(a) {
    const cells = a.matrix && a.matrix.cells;
    if (!Array.isArray(cells) || !cells.length) return `<span class="acct-none">—</span>`;
    const strip = cells.map((c) => {
      const at = new Date(c.at);
      const label = isNaN(at) ? '' : at.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
      const tip = `${label} · ${t('accounts.tip.ok', { n: c.ok || 0 })} ${t('accounts.tip.err', { n: c.err || 0 })} ${t('accounts.tip.rl', { n: c.rl || 0 })}${c.sw ? ' ' + t('accounts.tip.sw', { n: c.sw }) : ''}`;
      return `<div class="acct-cell acct-cell--${cellTone(c)}" title="${esc(tip)}"></div>`;
    }).join('');
    const total = cells.reduce((s, c) => s + (c.ok || 0) + (c.err || 0) + (c.rl || 0), 0);
    const foot = total ? t('accounts.recentFoot', { n: total }) : t('accounts.noRecent');
    return `<div class="acct-cell-health"><div class="acct-strip">${strip}</div><div class="acct-strip-foot">${esc(foot)}</div></div>`;
  }

  function cellPerf(a) {
    const u = a.usage || {};
    const lane = a.lane || {};
    const bits = [];
    if (lane.inflight !== undefined && lane.inflight !== null) {
      bits.push(`<div>${esc(t('accounts.m.inflight'))} ${num(lane.inflight)}</div>`);
    }
    const ttfb = [['p50', u.ttfb_p50], ['p90', u.ttfb_p90]]
      .filter(([, v]) => Number.isFinite(Number(v)))
      .map(([k, v]) => `${k} ${Math.round(Number(v))}ms`);
    if (ttfb.length) bits.push(`<div>TTFB ${ttfb.join(' · ')}</div>`);
    if (u.tps_now !== undefined && u.tps_now !== null && Number.isFinite(Number(u.tps_now))) {
      bits.push(`<div>TPS ${fmtN(u.tps_now)}</div>`);
    }
    if (!bits.length) return `<span class="acct-none">—</span>`;
    return `<div class="acct-cell-perf">${bits.join('')}</div>`;
  }

  // ---- 展开托盘：深度证据层（观测密度不丢，默认面降噪） ----

  function gateDetail(a) {
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

  function warmDetail(a) {
    const w = a.warm;
    if (!w) return '';
    if (w.enabled === false) return sec('accounts.sec.warm', `<div class="acct-none">${esc(t('accounts.warmOff'))}</div>`);
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

  // 配额明细：credits/grace/topup 网格 + 日周完整条（托盘内给全量）。
  function quotaDetail(a) {
    const q = a.quota;
    if (!q) return '';
    const bar = (label, f) => {
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
    };
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
    const inner = bar(t('accounts.f.daily'), q.daily) + bar(t('accounts.f.weekly'), q.weekly) + grid;
    return inner ? sec('accounts.sec.quota', inner) : '';
  }

  function usageDetail(a) {
    const items = [];
    const u = a.usage;
    if (!u) return '';
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
    const grid = kvGrid(items);
    return grid ? sec('accounts.sec.metrics', grid) : '';
  }

  // 证据块（同步，不发请求）：lane 健康/连败/绑定 + 最近失败 + 两档冷却 +
  // gate 闩与事件环 + 最近配额采样。recover 按钮由 ops 层追加（带端点探测）。
  function evidenceBody(a) {
    const lane = a.lane || {};
    const gate = a.gate || {};
    const now = Date.now();
    const future = (iso) => iso && Date.parse(iso) > now;
    const row = (label, val) => (val
      ? `<div class="acct-kv"><span class="acct-kv-k">${esc(label)}</span><span class="acct-kv-v">${val}</span></div>` : '');
    const secs = [];
    const laneBits = [
      !a.lane ? `<span class="acct-none">${esc(t('accounts.noData'))}</span>`
        : lane.healthy === false
          ? `<span class="acct-no">${esc(t('accounts.st.unready'))}</span>`
          : `<span class="acct-yes">${esc(t('accounts.st.ok'))}</span>`,
      lane.fail_streak ? esc(t('accounts.ev.failStreak', { n: lane.fail_streak })) : '',
      lane.bound_sessions ? esc(t('accounts.ev.bound', { n: lane.bound_sessions })) : '',
      lane.inflight !== undefined && lane.inflight !== null ? esc(`${t('accounts.m.inflight')} ${num(lane.inflight)}`) : ''
    ].filter(Boolean).join(' · ');
    secs.push(row(t('accounts.ev.lane'), laneBits));
    if (lane.last_failure_at) {
      const code = lane.last_failure_code || t('accounts.st.unknownError');
      const msg = lane.last_failure_message ? ` — ${esc(lane.last_failure_message)}` : '';
      secs.push(row(t('accounts.ev.failure'), `${esc(relTime(lane.last_failure_at))} · ${esc(code)}${msg}`));
    }
    const cds = [];
    if (future(lane.auth_cooldown_until)) cds.push(`${esc(t('accounts.ev.credCooldown'))} ${esc(countdown(lane.auth_cooldown_until))}`);
    if (future(lane.unhealthy_until)) cds.push(`${esc(t('accounts.ev.failCooldown'))} ${esc(countdown(lane.unhealthy_until))}`);
    if (cds.length) secs.push(row(t('accounts.ev.cooldowns'), cds.join(' · ')));
    if (gate.latched) {
      const s = gate.limited_until ? Math.max(0, Math.round((Date.parse(gate.limited_until) - now) / 1000)) : 0;
      const latchTxt = gate.probing ? t('accounts.st.probing') : (s ? t('accounts.ev.latchLeft', { s }) : t('accounts.st.latched'));
      secs.push(row(t('accounts.ev.latch'), esc(latchTxt)));
    }
    const pts = (a.quota && Array.isArray(a.quota.points)) ? a.quota.points : [];
    const last = pts.length ? pts[pts.length - 1] : null;
    if (last && (last.daily_remaining !== undefined || last.weekly_remaining !== undefined)) {
      const pct = (v) => (v === null || v === undefined ? '—' : `${Number(v).toFixed(0)}%`);
      secs.push(row(t('accounts.ev.quota'),
        esc(`${t('accounts.f.daily')} ${pct(last.daily_remaining)} · ${t('accounts.f.weekly')} ${pct(last.weekly_remaining)}`)
        + (last.at ? ` <span class="acct-ev-at">${esc(new Date(last.at * 1000).toLocaleTimeString())}</span>` : '')));
    }
    const evSec = `<div class="acct-ev-events"><h5>${esc(t('accounts.ev.events'))}</h5>${gateEventsHTML(a)}</div>`;
    return `<div class="acct-kv-grid">${secs.join('')}</div>${evSec}`;
  }

  // 闩事件环填进托盘 events 槽（core 调用——gate.events 在 a 上，同步渲染）。
  function gateEventsHTML(a) {
    const gate = a.gate || {};
    const events = Array.isArray(gate.events) ? gate.events.slice(0, 8) : [];
    if (!events.length) return `<div class="acct-none">${esc(t('accounts.ev.noEvents'))}</div>`;
    const now = Date.now();
    const relText = relTime;
    const leftText = countdown;
    const kindText = (ev) => {
      const key = ev.kind === 'latched' && ev.detail === 'extended' ? 'extended' : ev.kind;
      const known = { latched: 1, extended: 1, released: 1, expired: 1, restored: 1 };
      return known[key] ? t('accounts.gate.ev.' + key) : (ev.label || ev.kind || '');
    };
    return events.map((ev) => `<div class="acct-ev-ev"><span class="acct-ev-kind">${esc(kindText(ev))}</span> ${esc(relText(ev.at))}${ev.until && Date.parse(ev.until) > now ? ` · ${esc(leftText(ev.until))}` : ''}</div>`).join('');
  }

  // 托盘骨架：证据 + 闸门 + 保温 + 配额明细 + 用量 + failover 懒拉槽 + 曲线。
  // failover 容器留给 ops.fillFailover 异步填；曲线由 core mount 后 curveInit。
  function trayBlock(a) {
    const sections = [
      sec('accounts.drawer.evidence', evidenceBody(a), 'acct-sec--evidence'),
      gateDetail(a),
      warmDetail(a),
      quotaDetail(a),
      usageDetail(a),
      `<section class="acct-sec acct-sec--failover"><h4>${esc(t('accounts.drawer.failover'))}</h4><div class="acct-fo-slot" data-tray="failover"><div class="acct-none">${esc(t('accounts.drawer.loading'))}</div></div></section>`
    ].filter(Boolean).join('');
    const pts = a.quota && Array.isArray(a.quota.points) ? a.quota.points : [];
    const plottable = pts.some((p) => p && (p.daily_remaining !== undefined || p.weekly_remaining !== undefined));
    const curve = plottable
      ? `<section class="acct-sec acct-sec--curve"><h4>${esc(t('accounts.curveTitle'))}</h4><div class="acct-curve" data-acct="${esc(a.name || '')}"></div></section>`
      : '';
    return `<div class="acct-tray-grid">${sections}</div>${curve}`;
  }

  function rowCells(a) {
    const safe = (fn) => {
      try { return fn(a || {}) || ''; } catch { return ''; }
    };
    return {
      account: safe(cellAccount),
      status: safe(cellStatus),
      quota: safe(cellQuota),
      today: safe(cellToday),
      health: safe(cellHealth),
      perf: safe(cellPerf)
    };
  }

  // ---- echarts 实例表（元素 → {chart, points, sig}）----

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
        mk(t('accounts.curveDaily'), '#7f56f1', (p) => p.daily_remaining),
        mk(t('accounts.curveWeekly'), '#10b981', (p) => p.weekly_remaining)
      ]
    };
  }

  // 幂等同步：同签名直接返回（自动刷新不闪），签名变才 setOption 更新数据；
  // 元素已脱离 DOM 的僵尸实例顺带回收（diff 重渲会换掉 .acct-curve 元素）。
  function curveInit(el, points) {
    if (!el) return;
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

  // relTime/countdown 同时被 ops 层 failover 明细复用（loads 顺序 view→ops→core）
  window.acctView = {
    rowCells,
    trayBlock,
    gateEventsHTML,
    pillList,
    curveInit,
    disposeCharts,
    emptyStateHTML,
    relTime,
    countdown
  };
})();
