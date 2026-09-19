// 上游账号池页 orchestrator：数据源装配 + 卡骨架拼装 + 事件/图表生命周期调度。
// 块内渲染全部外包 window.acctView，操作列/表单/空态外包 window.acctOps，
// 对接面冻结在 notes/pool-accounts-contract.md。
// 数据源：GET /admin/accounts（7e 已冻结：身份+lane/gate/warm/inflight+quota
// 摘要的聚合视图，服务器序含 tombstoned——探测到即以它为名单与权威字段源）、
// /admin/runtime-metrics 的 accounts 组（过渡名单=活 lane 键集 + lane/gate/
// warm 过渡值）、/admin/quota 的 accounts 组（points 恒由此 join）、
// /admin/logs/matrix?since=24h（健康格/failover 归因）。空池合法：名单空时
// 整页空态由 view/ops 挂加号表单与 CLI 导入。
(function () {
  const t = window.t;
  const esc = window.escapeHtml;
  const num = window.formatNumber || ((v) => String(v));
  const WINDOW_HOURS = 24;

  let runtime = null;
  let quota = null;
  let matrix = null;
  let runtimeErr = null;
  let quotaErr = null;
  let matrixErr = null;
  // adminApi 三态：undefined=未探测，false=端点缺席（本会话不再探），true=可用。
  // 501/503 属「已注册未落地」——不缓存判定，下轮重试以便实现上线即升级。
  let adminApi;
  // adminList 保 GET 响应数组原样（服务器序：config 声明序→panel created_at,name）；
  // adminMap 是同内容按名索引。GET 缺席时两者 null，过渡期名单回落活 lane 键集。
  let adminList = null;
  let adminMap = null;
  // 最近一轮渲染的装配对象按名索引——点击委托把 data-acct 还原成 a 交给 ops。
  let lastByName = new Map();
  // 首拉完成前不渲染——HTML 自带 loading 占位，避免空名单闪出整页空态。
  let firstLoad = false;
  // 排序选择器持久偏好：server=服务器序（契约默认）、name/priority/health。
  const SORT_KEY = 'accounts.sort';
  let sortMode = 'server';
  try { sortMode = localStorage.getItem(SORT_KEY) || 'server'; } catch (_) { /* 无痕模式容忍 */ }
  // ?account=<name> 深链：卡列表只显示该卡 + 高亮，脉/KPI 仍给全池视图。
  let filterName = new URLSearchParams(location.search).get('account') || '';

  const el = (id) => document.getElementById(id);
  const errMsg = (e) => (e && (e.message || String(e))) || '';
  // GET /admin/accounts 是账号操作端点组的共享探测：ops 侧按钮显隐
  // 靠这份结果（reportGetProbe），不再自己发第二份 GET。
  const reportProbe = (status) => {
    if (window.acctOps && typeof window.acctOps.reportGetProbe === 'function') {
      window.acctOps.reportGetProbe(status);
    }
  };
  const cssEsc = (s) => (window.CSS && CSS.escape ? CSS.escape(s) : String(s).replace(/["\\\]]/g, ''));

  window.initPageBootstrap({
    topbarKey: 'accounts',
    run: () => {
      // reload 注入先于任何挂载：ops 表单/动作成功回调走这里回到 loadAll。
      if (window.acctOps) window.acctOps.reload = loadAll;
      const addEl = el('accounts-add');
      if (addEl && window.acctOps && typeof window.acctOps.mountAddForm === 'function') {
        window.acctOps.mountAddForm(addEl);
      }
      const refresh = el('accounts-refresh');
      if (refresh) refresh.addEventListener('click', () => loadAll(true));
      syncSortSel();
      const sortSel = el('accounts-sort');
      if (sortSel) {
        sortSel.addEventListener('change', () => {
          sortMode = sortSel.value;
          try { localStorage.setItem(SORT_KEY, sortMode); } catch (_) { /* 忽略 */ }
          renderAll();
        });
      }
      const filterChip = el('accounts-filter');
      if (filterChip) {
        filterChip.addEventListener('click', (e) => {
          if (e.target.closest('[data-clear-filter]')) clearFilter();
        });
      }
      for (const id of ['accounts-list', 'accounts-add', 'accounts-empty']) {
        const node = el(id);
        if (node) {
          node.addEventListener('click', onActClick);
          // 徽标/失败行是 span/div（role=button）：Enter/Space 补成 click。
          node.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' && e.key !== ' ') return;
            const act = e.target.closest('[data-act]');
            if (!act || act.tagName === 'A' || act.tagName === 'BUTTON') return;
            e.preventDefault();
            act.click();
          });
        }
      }
      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        // locale 换了文案要全量重渲（翻译烘进 html 串），排序选项同刷。
        window.i18n.onLocaleChange(() => { syncSortSel(); renderAll(true); });
      }
      loadAll(true);
      if (typeof window.createAutoRefresh === 'function') {
        // 抽屉打开时跳过本轮：读者正在看懒拉明细，不抢滚动位置。
        window.createAutoRefresh({
          load: () => loadAll(),
          skip: () => !!document.querySelector('.acct-drawer[open]')
        }).init();
      }
    }
  });

  async function loadAll(force) {
    const btn = el('accounts-refresh');
    if (btn) btn.disabled = true;
    const since = new Date(Date.now() - WINDOW_HOURS * 3600e3).toISOString();
    const [r, q, m, ad] = await Promise.allSettled([
      window.fetchDataWithAuth('/admin/runtime-metrics'),
      window.fetchDataWithAuth('/admin/quota'),
      window.fetchDataWithAuth('/admin/logs/matrix?since=' + encodeURIComponent(since)),
      adminApi === false ? Promise.resolve(null) : fetchAdminAccounts()
    ]);
    if (r.status === 'fulfilled') { runtime = r.value; runtimeErr = null; } else { runtimeErr = r.reason; }
    if (q.status === 'fulfilled') { quota = q.value; quotaErr = null; } else { quotaErr = q.reason; }
    if (m.status === 'fulfilled') { matrix = m.value; matrixErr = null; } else { matrixErr = m.reason; }
    if (ad.status === 'fulfilled' && ad.value) {
      adminList = ad.value;
      adminMap = new Map();
      for (const acc of ad.value) {
        if (acc && typeof acc.name === 'string') adminMap.set(acc.name, acc);
      }
    } else {
      adminList = null;
      adminMap = null;
    }
    firstLoad = true;
    renderAll(!!force);
    const stamp = el('accounts-updated-at');
    if (stamp) stamp.textContent = t('accounts.updatedAt', { time: new Date().toLocaleString() });
    if (btn) btn.disabled = false;
  }

  // GET /admin/accounts：404/405 说明端点不存在——缓存 adminApi=false 本会话
  // 不再发请求；其余失败（网络/5xx/501 未落地/格式不符）不缓存判定，下轮重试。
  // 成功返回 accounts 数组原样（空数组也是成功：空池），失败/无货返回 null。
  async function fetchAdminAccounts() {
    let res;
    try {
      res = await window.fetchWithAuth('/admin/accounts');
    } catch (_) {
      reportProbe(null);
      return null;
    }
    reportProbe(res.status);
    if (res.status === 404 || res.status === 405) { adminApi = false; return null; }
    if (!res.ok) return null;
    let payload;
    try { payload = await res.json(); } catch (_) { return null; }
    const data = payload && payload.success ? payload.data : null;
    const list = Array.isArray(data && data.accounts) ? data.accounts
      : (Array.isArray(data) ? data : null);
    if (!list) return null;
    adminApi = true;
    return list;
  }

  // ---- 数据归并 ----

  // 名单：GET 可用时用其数组原序（含 tombstoned——它们不在活 lane 键集里
  // 也照样成卡）；缺席/失败时回落 Object.keys(runtime.accounts).sort()——
  // 只认活 lane，quota-only/matrix-only 幽灵名不成卡。每项装配成契约 a。
  function accountList() {
    const rm = (runtime && runtime.accounts) || {};
    const qa = (quota && quota.accounts) || {};
    const names = adminList
      ? adminList.map((acc) => acc && acc.name).filter(Boolean)
      : Object.keys(rm).sort();
    return names.map((name) => {
      const slot = rm[name] || {};
      const ad = adminMap && adminMap.get(name);
      const a = {
        name,
        source: null,
        credential: null,
        disabled: null,
        has_override: null,
        config_declared: null,
        lane: slot.lane || null,
        gate: slot.gate || null,
        warm: slot.warm || null,
        quota: qa[name] || null,
        usage: null, // usage 组是 P2 聚合字段，过渡形态恒 null
        matrix: matrixFor(name),
        // errors 是给 view 分块渲染源失败的旁路：某源挂了对应块出错误行
        // 而不是误显空态。
        errors: {
          runtime: runtimeErr ? errMsg(runtimeErr) : null,
          quota: quotaErr ? errMsg(quotaErr) : null,
          matrix: matrixErr ? errMsg(matrixErr) : null
        }
      };
      if (ad) {
        // GET 项是权威源：上报字段覆盖过渡值（source/credential/disabled/
        // has_override/config_declared/token_sha/credentials_file/lane/gate/
        // warm/usage/created_at/updated_at）；null/undefined 视为未上报不盖。
        // name/inflight/quota/matrix/errors 不走通用覆盖，各自单独立口径。
        for (const k of Object.keys(ad)) {
          if (k === 'name' || k === 'inflight' || k === 'quota' || k === 'matrix' || k === 'errors') continue;
          if (ad[k] !== undefined && ad[k] !== null) a[k] = ad[k];
        }
        // GET 的 quota 块只有 {daily,weekly,user} 无 points——points 恒由
        // /admin/quota join，故 quota 是两块浅合而不是整块覆盖。
        if (ad.quota && typeof ad.quota === 'object') {
          a.quota = Object.assign({}, qa[name] || {}, ad.quota);
        }
        // inflight 是 GET 顶层 int（排空期 lane=null 也报实际值）；折进
        // a.lane.inflight 供 view 一处读——lane 缺席时物化 {inflight}。
        if (ad.inflight !== undefined && ad.inflight !== null) {
          if (!a.lane) a.lane = {};
          a.lane.inflight = ad.inflight;
        }
      }
      return a;
    });
  }

  // 排序选择器：server=服务器序（契约默认），其余三档本地重排不改 adminList。
  function sortList(list) {
    if (sortMode === 'server' || list.length < 2) return list;
    const arr = list.slice();
    const byName = (x, y) => String(x.name).localeCompare(String(y.name));
    if (sortMode === 'name') return arr.sort(byName);
    if (sortMode === 'priority') return arr.sort((x, y) => (Number(y.priority) || 0) - (Number(x.priority) || 0) || byName(x, y));
    if (sortMode === 'health') {
      const rank = { bad: 0, warn: 1, ok: 2, idle: 3 };
      return arr.sort((x, y) => (rank[primaryTone(x)] ?? 4) - (rank[primaryTone(y)] ?? 4) || byName(x, y));
    }
    return arr;
  }

  function syncSortSel() {
    const sel = el('accounts-sort');
    if (!sel) return;
    sel.innerHTML = ['server', 'name', 'priority', 'health']
      .map((k) => `<option value="${k}">${esc(t('accounts.sort.' + k))}</option>`)
      .join('');
    sel.value = sortMode;
  }

  function clearFilter() {
    filterName = '';
    try {
      const url = new URL(location.href);
      url.searchParams.delete('account');
      history.replaceState(null, '', url);
    } catch (_) { /* file:// 等场景容忍 */ }
    renderAll();
  }

  function syncFilterChip() {
    const chip = el('accounts-filter');
    if (!chip) return;
    chip.hidden = !filterName;
    if (filterName) {
      chip.innerHTML = `<span>${esc(t('accounts.filter.only', { name: filterName }))}</span>` +
        `<button type="button" class="acct-filter-x" data-clear-filter aria-label="${esc(t('accounts.filter.clear'))}">&times;</button>`;
    }
  }

  // 逐号 matrix 派生：entries 原样过滤（view 算今日指标/failover 抽屉），
  // cells 是 24×1h 健康桶（优先级 err>rl>ok，同桶 rl+err 按 err 计），
  // failoverCount 是该号条目 account_switches 总和。
  function matrixFor(name) {
    const now = Date.now();
    const minT = now - WINDOW_HOURS * 3600e3;
    const cells = Array.from({ length: WINDOW_HOURS }, (_, i) => ({
      at: new Date(minT + i * 3600e3), ok: 0, err: 0, rl: 0, sw: 0
    }));
    const entries = [];
    let failoverCount = 0;
    for (const e of ((matrix && matrix.entries) || [])) {
      if (e.account !== name) continue;
      entries.push(e);
      if (e.account_switches) failoverCount += e.account_switches;
      const ts = Date.parse(e.started_at);
      if (!Number.isFinite(ts) || ts < minT) continue;
      const c = cells[Math.min(WINDOW_HOURS - 1, Math.floor((ts - minT) / 3600e3))];
      if (e.rate_limited) c.rl++;
      else if (e.result === 'completed' && e.status_code < 400) c.ok++;
      else c.err++;
      if (e.account_switches) c.sw += e.account_switches;
    }
    return { cells, entries, failoverCount };
  }

  // ---- 渲染 ----

  function renderAll(force) {
    if (!firstLoad) return;
    syncFilterChip();
    const list = sortList(accountList());
    lastByName = new Map(list.map((a) => [a.name, a]));
    // 空态只在确知池为空时启用：GET 已通时名单即权威（runtime 挂不挂都行）；
    // 过渡期需 runtime 拉到且键集空。runtime 故障走列表区错误块，不能把
    // 故障误显成 onboarding。
    const poolKnown = adminApi === true || !runtimeErr;
    const showEmpty = !list.length && poolKnown;
    const pulse = el('accounts-pulse');
    const overview = pulse && pulse.closest('section');
    if (overview) overview.hidden = showEmpty;
    const listEl = el('accounts-list');
    if (listEl) listEl.hidden = showEmpty;
    const empty = el('accounts-empty');
    if (empty) empty.hidden = !showEmpty;
    if (showEmpty) {
      renderEmpty();
      return;
    }
    renderPulse(list);
    renderKpi(list);
    renderCards(list, force);
  }

  function renderEmpty() {
    const root = el('accounts-empty');
    if (!root || !window.acctView || !window.acctOps) return;
    if (window.acctView.disposeCharts) window.acctView.disposeCharts();
    root.innerHTML = window.acctView.emptyStateHTML();
    window.acctOps.mountEmptyAdd(root.querySelector('#accounts-empty-add') || root);
    window.acctOps.maybeCliImport(root.querySelector('#accounts-cli-import') || root);
  }

  function primaryTone(a) {
    const pills = (window.acctView && window.acctView.pillList ? window.acctView.pillList(a) : []) || [];
    return pills.length && pills[0] ? pills[0].tone : 'idle';
  }

  function renderPulse(list) {
    const root = el('accounts-pulse');
    if (!root) return;
    if (!list.length) {
      const msg = runtimeErr ? errMsg(runtimeErr) : t('accounts.noPool');
      root.innerHTML = `<span class="accounts-pulse-empty">${esc(msg)}</span>`;
      return;
    }
    const pillsOf = (a) => (window.acctView && window.acctView.pillList ? window.acctView.pillList(a) : []) || [];
    root.innerHTML = list.map((a) => {
      const pills = pillsOf(a);
      const p = pills[0];
      return `<div class="accounts-pulse-seg accounts-pulse-seg--${p ? p.tone : 'idle'}"
        title="${esc(a.name)} · ${esc(p ? p.text : '')}"></div>`;
    }).join('');
  }

  function renderKpi(list) {
    const grid = el('accounts-kpi-grid');
    if (!grid) return;
    if (runtimeErr && quotaErr && matrixErr && adminApi !== true) {
      grid.innerHTML = errBlock(errMsg(runtimeErr));
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
      <strong class="runtime-metric-value${tone ? ` acct-tone--${tone}` : ''}">${value}</strong>
      ${sub ? `<span class="runtime-metric-sub">${sub}</span>` : ''}
    </div>`;
    grid.innerHTML = [
      card(t('accounts.kpi.lanes'), list.length, null, null),
      card(t('accounts.kpi.healthy'), healthy, list.length ? t('accounts.kpi.of', { n: list.length }) : null,
        healthy === list.length ? 'success' : 'warning'),
      card(t('accounts.kpi.requests'), num(requests), matrixErr ? esc(t('accounts.partial')) : null, null),
      card(t('accounts.kpi.errors'), num(errors), rateLimited ? t('accounts.kpi.rlSub', { n: rateLimited }) : null,
        errors ? 'error' : 'success'),
      card(t('accounts.kpi.switches'), num(switches), null, switches ? 'warning' : null)
    ].join('');
  }

  function errBlock(msg) {
    return `<div class="empty-state"><div class="empty-state-title empty-state-title--error">${esc(t('accounts.sectionError'))}</div><div>${esc(msg)}</div></div>`;
  }

  // 卡骨架归 core：每块内容落进固定 data-slot 容器（drawer 槽留给 ops 的
  // 抽屉，core 的逐块 diff 不碰它——开着的抽屉扛得住自动刷新）。view 出块
  // html，ops 出 actions 行；curve 块内含 .acct-curve 挂载点由 view 起图。
  const SLOTS = ['head', 'failure', 'actions', 'gate', 'warm', 'quota', 'metrics', 'health', 'curve'];
  // cardEl -> 上轮各 slot 的 html 串：逐串比对，只写变了的 slot，骨架不动。
  const lastBlocks = new WeakMap();

  function cardBlocksOf(a) {
    const b = window.acctView.cardBlocks(a) || {};
    return {
      head: b.head || '',
      failure: b.failure || '',
      actions: window.acctOps.actionsBlock(a) || '',
      gate: b.gate || '',
      warm: b.warm || '',
      quota: b.quota || '',
      metrics: b.metrics || '',
      health: b.health || '',
      curve: b.curve || ''
    };
  }

  function buildCard(a, blocks) {
    const tpl = document.createElement('template');
    tpl.innerHTML = `<div class="card acct-card" data-acct="${esc(a.name)}">
      <div data-slot="head">${blocks.head}</div>
      <div data-slot="failure">${blocks.failure}</div>
      <div data-slot="actions">${blocks.actions}</div>
      <div class="acct-grid">
        <div data-slot="gate">${blocks.gate}</div>
        <div data-slot="warm">${blocks.warm}</div>
        <div data-slot="quota">${blocks.quota}</div>
        <div data-slot="metrics">${blocks.metrics}</div>
        <div data-slot="health">${blocks.health}</div>
        <div class="acct-sec--curve" data-slot="curve">${blocks.curve}</div>
      </div>
      <div data-slot="drawer"></div>
    </div>`;
    const cardEl = tpl.content.firstElementChild;
    lastBlocks.set(cardEl, blocks);
    return cardEl;
  }

  function initCurve(cardEl, a) {
    const curve = cardEl.querySelector('.acct-curve');
    if (curve) window.acctView.curveInit(curve, (a.quota && a.quota.points) || []);
  }

  // 逐 slot 比对上轮 html：变才 innerHTML，没变整块跳过——pill 倒计时/
  // 失败 relTime 这类文本变化只重写对应 slot，echarts 容器不再被替换。
  function syncCard(cardEl, a) {
    const blocks = cardBlocksOf(a);
    const prev = lastBlocks.get(cardEl) || {};
    for (const slot of SLOTS) {
      if (prev[slot] === blocks[slot]) continue;
      const slotEl = cardEl.querySelector(`[data-slot="${slot}"]`);
      if (slotEl) slotEl.innerHTML = blocks[slot];
    }
    lastBlocks.set(cardEl, blocks);
    initCurve(cardEl, a); // curveInit 幂等：points 签名不变即 no-op
  }

  function spotCards(root) {
    for (const c of root.children) {
      c.classList.toggle('acct-card--spot', !!filterName && c.dataset.acct === filterName);
    }
  }

  // 自动刷新走 diff（骨架/图表/抽屉不动）；手动刷新 force 全量重建。
  function renderCards(list, force) {
    const root = el('accounts-list');
    if (!root) return;
    if (!window.acctView || !window.acctOps) {
      root.innerHTML = `<div class="card accounts-section-card">${errBlock('accounts-view.js / accounts-ops.js not loaded')}</div>`;
      return;
    }
    const shown = filterName ? list.filter((a) => a.name === filterName) : list;
    const allCards = root.children.length > 0
      && Array.from(root.children).every((c) => c.classList.contains('acct-card'));
    if (force || !shown.length || !allCards) {
      window.acctView.disposeCharts();
      if (!shown.length) {
        const msg = filterName
          ? t('accounts.filter.missing', { name: filterName })
          : errMsg(runtimeErr || quotaErr || matrixErr);
        root.innerHTML = `<div class="card accounts-section-card">${errBlock(msg)}</div>`;
        return;
      }
      root.innerHTML = '';
      for (const a of shown) {
        const cardEl = buildCard(a, cardBlocksOf(a));
        root.appendChild(cardEl);
        initCurve(cardEl, a);
      }
      spotCards(root);
      return;
    }
    // diff 路径：先摘出名单外旧卡，再按当前序逐卡同步+appendChild 重排
    // （appendChild 对已挂节点是移动而非重建，DOM 与 chart 都保留）。
    const wanted = new Set(shown.map((a) => a.name));
    for (const c of Array.from(root.children)) {
      if (!wanted.has(c.dataset.acct)) c.remove();
    }
    for (const a of shown) {
      let cardEl = root.querySelector(`.acct-card[data-acct="${cssEsc(a.name)}"]`);
      if (!cardEl) {
        cardEl = buildCard(a, cardBlocksOf(a));
        root.appendChild(cardEl);
        initCurve(cardEl, a);
      } else {
        syncCard(cardEl, a);
        root.appendChild(cardEl);
      }
    }
    spotCards(root);
  }

  // 卡内/表单/空态的 data-act 统一委托 ops.handle；data-acct 命中 lastByName
  // 时把装配好的 a 一并交过去，ops 不回源。第 4 参给被点元素（抽屉定位卡、
  // 按钮 spinner 复位都要它）。
  function onActClick(e) {
    const btn = e.target.closest('[data-act]');
    if (!btn || !window.acctOps || typeof window.acctOps.handle !== 'function') return;
    const name = btn.dataset.acct || null;
    window.acctOps.handle(btn.dataset.act, name, name ? (lastByName.get(name) || null) : null, btn);
  }
})();
