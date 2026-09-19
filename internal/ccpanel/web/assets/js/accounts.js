// 上游账号池页 orchestrator（表格主视图）：数据源装配 + 表格骨架/行 diff +
// 行展开托盘 + 勾选批量条 + 事件/图表生命周期调度。
// 单元格与托盘 html 外包 window.acctView，操作格/表单/批量执行外包
// window.acctOps，对接面冻结在 notes/pool-accounts-contract.md。
// 数据源：GET /admin/accounts（聚合视图，服务器序含 tombstoned——探测到即
// 以它为名单与权威字段源）、/admin/runtime-metrics 的 accounts 组（过渡
// 名单=活 lane 键集 + lane/gate/warm 过渡值）、/admin/quota 的 accounts
// 组（points 恒由此 join）、/admin/logs/matrix?since=24h&slot=1800（cells=
// 服务端 (slot,account) 全窗分桶供健康条/failover 计数；entries 截断后只
// 覆盖窗尾，仅做取证明细）。空池合法：名单空时整页空态由 view/ops 挂加号表单
// 与 CLI 导入。
(function () {
  const t = window.t;
  const esc = window.esc;
  const num = window.formatNumber;
  const WINDOW_HOURS = 24;
  const HEALTH_CELLS = 48;                       // 48×30min 健康条
  const CELL_MS = (WINDOW_HOURS * 3600e3) / HEALTH_CELLS;
  const COLS = 8;                                // 勾选+账号+状态+配额+今日+近24h+性能+操作

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
  // ?account=<name> 深链：行列表只显示该行 + 高亮，脉/KPI 仍给全池视图。
  let filterName = new URLSearchParams(location.search).get('account') || '';
  // 展开与勾选两个会话态集合：按名存活跨渲染——行 diff/重建后托盘与勾选复位。
  const expanded = new Set();
  const selected = new Set();

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
      const listEl = el('accounts-list');
      if (listEl) {
        listEl.addEventListener('click', onListClick);
        listEl.addEventListener('change', onCheckChange);
      }
      for (const id of ['accounts-add', 'accounts-empty']) {
        const node = el(id);
        if (node) node.addEventListener('click', onActClick);
      }
      const batch = el('accounts-batch');
      if (batch) batch.addEventListener('click', onBatchClick);
      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        // locale 换了文案要全量重渲（翻译烘进 html 串），排序选项同刷。
        window.i18n.onLocaleChange(() => { syncSortSel(); renderAll(true); });
      }
      loadAll(true);
      if (typeof window.createAutoRefresh === 'function') {
        // kebab 菜单开着时跳过本轮：菜单是 body 级元素锚在行钮上，
        // 行 diff 重写操作格会让锚点失效。
        window.createAutoRefresh({
          load: () => loadAll(),
          skip: () => !!document.querySelector('.acct-menu')
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
      window.fetchDataWithAuth('/admin/logs/matrix?since=' + encodeURIComponent(since) + '&slot=1800'),
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
  // 也照样成行）；缺席/失败时回落 Object.keys(runtime.accounts).sort()——
  // 只认活 lane，quota-only/matrix-only 幽灵名不成行。每项装配成契约 a。
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
        `<button type="button" class="acct-filter-x" data-clear-filter aria-label="${esc(t('common.clear'))}">&times;</button>`;
    }
  }

  // 逐号 matrix 派生：cells 用服务端 (slot,account) 桶——entries 高流量
  // 被 requestsFetchCap 截断只覆盖窗尾，健康条与 failoverCount 改用全窗
  // 口径；entries 只留取证明细（failover 时间线/最近失败行）。
  function matrixFor(name) {
    const now = Date.now();
    const minT = now - WINDOW_HOURS * 3600e3;
    const cells = Array.from({ length: HEALTH_CELLS }, (_, i) => ({
      at: new Date(minT + i * CELL_MS), ok: 0, err: 0, rl: 0, sw: 0
    }));
    let failoverCount = 0;
    for (const sc of ((matrix && matrix.cells) || [])) {
      if (sc.account !== name) continue;
      const idx = Math.floor((sc.slot * 1000 - minT) / CELL_MS);
      if (idx < 0 || idx >= HEALTH_CELLS) continue;
      cells[idx].ok += sc.ok;
      cells[idx].err += sc.err;
      cells[idx].rl += sc.rl;
      cells[idx].sw += sc.sw;
      failoverCount += sc.sw;
    }
    const entries = ((matrix && matrix.entries) || []).filter((e) => e.account === name);
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
      renderBatch();
      return;
    }
    renderPulse(list);
    renderKpi(list);
    renderTable(list, force);
    renderBatch();
  }

  function renderEmpty() {
    const root = el('accounts-empty');
    if (!root || !window.acctView || !window.acctOps) return;
    if (window.acctView.disposeCharts) window.acctView.disposeCharts();
    expanded.clear();
    selected.clear();
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
    // KPI 与健康条同口径走服务端 cells 全窗桶；entries 截断只覆盖窗尾，
    // 再数它会给出「窗口内只有 2000 条」的假数。
    let requests = 0;
    let errors = 0;
    let switches = 0;
    let rateLimited = 0;
    for (const c of ((matrix && matrix.cells) || [])) {
      requests += c.ok + c.err + c.rl;
      errors += c.err;
      rateLimited += c.rl;
      switches += c.sw;
    }
    const healthy = list.filter((a) => primaryTone(a) === 'ok').length;
    const card = (label, value, sub, tone) => `<div class="kpi-card">
      <span class="kpi-label">${esc(label)}</span>
      <strong class="kpi-value${tone ? ` tone-${tone}` : ''}">${value}</strong>
      ${sub ? `<span class="kpi-sub">${sub}</span>` : ''}
    </div>`;
    grid.innerHTML = [
      card(t('accounts.kpi.healthy'), `${healthy}/${list.length}`, null,
        healthy === list.length ? 'healthy' : 'warning'),
      card(t('accounts.kpi.requests'), num(requests), matrixErr ? esc(t('accounts.partial')) : null, null),
      card(t('accounts.kpi.errors'), num(errors), rateLimited ? t('accounts.kpi.rlSub', { n: rateLimited }) : null,
        errors ? 'critical' : 'healthy'),
      card(t('accounts.kpi.switches'), num(switches), null, null)
    ].join('');
  }

  function errBlock(msg) {
    return `<div class="state-block"><div class="state-title state-title--error">${esc(t('accounts.sectionError'))}</div><div class="state-desc">${esc(msg)}</div></div>`;
  }

  // ---- 表格：行 = 勾选 + 六数据格 + 操作格；展开行是紧跟的整宽托盘行 ----

  const CELLS = ['account', 'status', 'quota', 'today', 'health', 'perf'];
  // tr -> 上轮各格 html 串：逐串比对只写变了的格，骨架与勾选框不动。
  const lastRowCells = new WeakMap();
  // tr -> 展开托盘行元素：行被移除/重排时托盘跟着走。
  const trayByRow = new WeakMap();

  // 表骨架归 core：thead 翻译烘进 html，force（手动刷新/locale 换）整表重建。
  function ensureTable(root, force) {
    const prev = root.querySelector('.acct-table-wrap');
    if (prev && !force) return prev;
    // 清场再挂：初始 loading 占位卡与上轮错误卡与表同一容器，不删会叠在表上方。
    root.innerHTML = '';
    const wrap = document.createElement('div');
    wrap.className = 'acct-table-wrap';
    wrap.innerHTML = `<table class="acct-table"><thead><tr>
      <th class="acct-col-check"><input type="checkbox" data-select-all aria-label="${esc(t('accounts.batch.selectAll'))}"></th>
      <th>${esc(t('accounts.col.account'))}</th>
      <th>${esc(t('accounts.col.status'))}</th>
      <th>${esc(t('accounts.col.quota'))}</th>
      <th>${esc(t('accounts.col.today'))}</th>
      <th>${esc(t('accounts.col.recent'))}</th>
      <th>${esc(t('accounts.col.perf'))}</th>
      <th class="acct-col-acts">${esc(t('accounts.col.actions'))}</th>
    </tr></thead><tbody></tbody></table>`;
    root.appendChild(wrap);
    return wrap;
  }

  function rowCellsOf(a) {
    const cells = window.acctView.rowCells(a) || {};
    const out = {};
    for (const k of CELLS) out[k] = cells[k] || '';
    out.actions = window.acctOps.actionsBlock(a) || '';
    return out;
  }

  function buildRow(a, cells) {
    const tr = document.createElement('tr');
    tr.className = 'acct-row';
    tr.dataset.acct = a.name;
    tr.innerHTML = `<td class="acct-col-check"><input type="checkbox" class="acct-check" data-acct="${esc(a.name)}" aria-label="${esc(a.name)}"></td>`
      + CELLS.map((k) => `<td data-cell="${k}">${cells[k]}</td>`).join('')
      + `<td class="acct-col-acts" data-cell="actions">${cells.actions}</td>`;
    lastRowCells.set(tr, cells);
    return tr;
  }

  // 逐格比对上轮 html：变才 innerHTML，没变整格跳过——pill 倒计时/失败
  // relTime 这类文本变化只重写对应格，健康条与勾选框不再被替换。
  function syncRow(tr, a) {
    const cells = rowCellsOf(a);
    const prev = lastRowCells.get(tr) || {};
    for (const k of Object.keys(cells)) {
      if (prev[k] === cells[k]) continue;
      const td = tr.querySelector(`td[data-cell="${k}"]`);
      if (td) td.innerHTML = cells[k];
    }
    lastRowCells.set(tr, cells);
  }

  function openTray(tr, a) {
    if (!a || trayByRow.has(tr)) return;
    const trayTr = document.createElement('tr');
    trayTr.className = 'acct-tray-row';
    trayTr.innerHTML = `<td colspan="${COLS}"><div class="acct-tray">${window.acctView.trayBlock(a)}</div></td>`;
    tr.after(trayTr);
    trayByRow.set(tr, trayTr);
    tr.classList.add('acct-row--open');
    expanded.add(a.name);
    window.acctOps.decorateTray(trayTr, a);
    const curve = trayTr.querySelector('.acct-curve');
    if (curve) window.acctView.curveInit(curve, (a.quota && a.quota.points) || []);
  }

  function closeTray(tr) {
    const trayTr = trayByRow.get(tr);
    if (trayTr) trayTr.remove();
    trayByRow.delete(tr);
    tr.classList.remove('acct-row--open');
    expanded.delete(tr.dataset.acct);
  }

  function removeRow(tr) {
    closeTray(tr);
    expanded.delete(tr.dataset.acct);
    tr.remove();
  }

  function toggleRow(tr) {
    if (trayByRow.has(tr)) closeTray(tr);
    else openTray(tr, lastByName.get(tr.dataset.acct));
  }

  function spotRows(tbody) {
    for (const tr of tbody.querySelectorAll('tr.acct-row')) {
      tr.classList.toggle('acct-row--spot', !!filterName && tr.dataset.acct === filterName);
    }
  }

  // 勾选态按名对齐：行勾选框逐行回写（diff 路径骨架保留旧勾选态，
  // 名单变化后必须显式同步），全选框三态。
  function syncCheckboxes(tbody) {
    const rows = Array.from(tbody.querySelectorAll('tr.acct-row'));
    let selCount = 0;
    for (const tr of rows) {
      const on = selected.has(tr.dataset.acct);
      if (on) selCount++;
      const cb = tr.querySelector('.acct-check');
      if (cb) cb.checked = on;
    }
    const all = tbody.closest('.acct-table-wrap')
      && tbody.closest('.acct-table-wrap').querySelector('[data-select-all]');
    if (all) {
      all.checked = rows.length > 0 && selCount === rows.length;
      all.indeterminate = selCount > 0 && selCount < rows.length;
    }
  }

  // 自动刷新走 diff（骨架/勾选/托盘不动）；手动刷新 force 全量重建。
  // 托盘内容渲染后不追刷新（同旧抽屉语义）——展开体是取证快照，
  // 读者正在看时不抢滚动位置；行的主格照常 diff。
  function renderTable(list, force) {
    const root = el('accounts-list');
    if (!root) return;
    if (!window.acctView || !window.acctOps) {
      root.innerHTML = `<div class="card accounts-section-card">${errBlock('accounts-view.js / accounts-ops.js not loaded')}</div>`;
      return;
    }
    const shown = filterName ? list.filter((a) => a.name === filterName) : list;
    // 两个会话态集合按名剪枝：掉出名单的号不再占展开/勾选位。
    const names = new Set(list.map((a) => a.name));
    for (const n of Array.from(selected)) if (!names.has(n)) selected.delete(n);
    for (const n of Array.from(expanded)) if (!names.has(n)) expanded.delete(n);
    if (!shown.length) {
      window.acctView.disposeCharts();
      expanded.clear();
      const msg = filterName
        ? t('accounts.filter.missing', { name: filterName })
        : errMsg(runtimeErr || quotaErr || matrixErr);
      root.innerHTML = `<div class="card accounts-section-card">${errBlock(msg)}</div>`;
      return;
    }
    const wrap = ensureTable(root, force);
    const tbody = wrap.querySelector('tbody');
    if (force) {
      window.acctView.disposeCharts();
      tbody.innerHTML = '';
    }
    // diff 路径：先摘出名单外旧行（托盘随行），再按当前序逐行同步 +
    // appendChild 重排（对已挂节点是移动而非重建，DOM/勾选/图表都保留）。
    const wanted = new Set(shown.map((a) => a.name));
    for (const tr of Array.from(tbody.querySelectorAll('tr.acct-row'))) {
      if (!wanted.has(tr.dataset.acct)) removeRow(tr);
    }
    for (const a of shown) {
      let tr = tbody.querySelector(`tr.acct-row[data-acct="${cssEsc(a.name)}"]`);
      if (!tr) {
        tr = buildRow(a, rowCellsOf(a));
        tbody.appendChild(tr);
      } else {
        syncRow(tr, a);
        tbody.appendChild(tr);
      }
      // 展开态跨渲染存活：tray 在就随行重排，缺（force 重建）就重开。
      const trayTr = trayByRow.get(tr);
      if (trayTr) tbody.appendChild(trayTr);
      else if (expanded.has(a.name)) openTray(tr, a);
    }
    spotRows(tbody);
    syncCheckboxes(tbody);
  }

  // ---- 批量条：勾选浮出，按钮走 ops.batchRun（逐号循环单号端点） ----

  function renderBatch() {
    const bar = el('accounts-batch');
    if (!bar) return;
    const n = selected.size;
    bar.hidden = !n;
    if (!n) {
      bar.innerHTML = '';
      return;
    }
    const btn = (act, key, danger) =>
      `<button type="button" class="acct-action-btn${danger ? ' acct-action-btn--danger' : ''}" data-batch="${act}">${esc(t(key))}</button>`;
    bar.innerHTML = `<span class="acct-batch-n">${esc(t('accounts.batch.selected', { n }))}</span>`
      + btn('enable', 'accounts.act.enable')
      + btn('disable', 'accounts.act.disable')
      + btn('clear-cooldown', 'accounts.act.clearCooldown')
      + btn('refresh-quota', 'accounts.act.refreshQuota')
      + btn('delete', 'accounts.act.delete', true)
      + `<button type="button" class="acct-action-btn" data-batch="clear">${esc(t('accounts.batch.clear'))}</button>`;
  }

  function onBatchClick(e) {
    const b = e.target.closest('[data-batch]');
    if (!b || !window.acctOps) return;
    const act = b.dataset.batch;
    const listEl = el('accounts-list');
    if (act === 'clear') {
      selected.clear();
      if (listEl) {
        const tbody = listEl.querySelector('tbody');
        if (tbody) syncCheckboxes(tbody);
      }
      renderBatch();
      return;
    }
    const names = Array.from(selected);
    if (!names.length) return;
    if (act === 'delete') {
      window.Modal.confirm(t('accounts.batch.confirmDelete', { n: names.length }), { danger: true })
        .then((ok) => { if (ok) window.acctOps.batchRun('delete', names); });
      return;
    }
    window.acctOps.batchRun(act, names);
  }

  // ---- 事件委托 ----

  // 列表区点击两分：命中 [data-act] 交 ops.handle；否则落在数据行非交互
  // 区（非按钮/输入/链接）即作行展开切换。
  function onListClick(e) {
    if (e.target.closest('[data-act]')) {
      onActClick(e);
      return;
    }
    if (e.target.closest('input,button,a,select,textarea,label')) return;
    const tr = e.target.closest('tr.acct-row');
    if (tr) toggleRow(tr);
  }

  function onCheckChange(e) {
    const listEl = el('accounts-list');
    const tbody = listEl && listEl.querySelector('tbody');
    if (!tbody) return;
    if (e.target.matches('[data-select-all]')) {
      const on = e.target.checked;
      for (const tr of tbody.querySelectorAll('tr.acct-row')) {
        if (on) selected.add(tr.dataset.acct); else selected.delete(tr.dataset.acct);
      }
    } else {
      const cb = e.target.closest('.acct-check');
      if (!cb) return;
      if (cb.checked) selected.add(cb.dataset.acct); else selected.delete(cb.dataset.acct);
    }
    syncCheckboxes(tbody);
    renderBatch();
  }

  // 行内/表单/空态的 data-act 统一委托 ops.handle；data-acct 命中 lastByName
  // 时把装配好的 a 一并交过去，ops 不回源。第 4 参给被点元素（kebab 定位、
  // 按钮 spinner 复位都要它）。
  function onActClick(e) {
    const btn = e.target.closest('[data-act]');
    if (!btn || !window.acctOps || typeof window.acctOps.handle !== 'function') return;
    const name = btn.dataset.acct || null;
    window.acctOps.handle(btn.dataset.act, name, name ? (lastByName.get(name) || null) : null, btn);
  }
})();
