// 模型注册表页：GET/PUT/DELETE /admin/model-registry 的 merged 视图。
// 每行 = 对外模型名（目录 ∪ 别名 ∪ 注册表 ∪ 流量）+ 上游目录详情
// （label/供应商/倍率/价格/能力徽标，挂在 row.catalog）+ 启用开关 +
// 重定向选择弹窗；筛选 = 状态 pills + 目录属性（供应商/API/档位/定价/特性
// chips）+ 名称搜索，客户端分页。
(function () {
  const t = window.t;
  let rows = [];
  let filterText = '';
  let filterMode = 'all';
  const activeTags = new Set();
  let currentPage = 1;
  let pageSize = 20;
  let totalPages = 1;

  try {
    pageSize = parseInt(localStorage.getItem('models.pageSize'), 10) || 20;
  } catch (_) { /* ignore */ }

  // 来源 badge 配色：目录=主色、别名=紫、注册表覆盖=琥珀、仅见过流量=灰。
  const SOURCE_COLORS = {
    catalog: ['rgba(59, 130, 246, 0.18)', 'var(--primary-400)'],
    alias: ['rgba(168, 85, 247, 0.18)', '#c084fc'],
    registry: ['rgba(245, 158, 11, 0.18)', '#fbbf24'],
    traffic: ['rgba(148, 163, 184, 0.15)', 'var(--color-text-secondary)']
  };

  // 档级徽章走冷色递升阶（浅蓝→蓝→紫），不用告警色的红/琥珀
  const TIER_BADGE = { free: '#10b981', low: '#60a5fa', medium: '#3b82f6', high: '#8b5cf6' };

  window.initPageBootstrap({
    topbarKey: 'models',
    run: () => {
      document.getElementById('models-filter').addEventListener('input', (e) => {
        filterText = e.target.value.trim().toLowerCase();
        currentPage = 1;
        render();
      });
      document.getElementById('models-filter-pills').addEventListener('click', (e) => {
        const btn = e.target.closest('.time-range-btn');
        if (!btn) return;
        document.querySelectorAll('#models-filter-pills .time-range-btn').forEach((b) => b.classList.remove('active'));
        btn.classList.add('active');
        filterMode = btn.dataset.filter;
        currentPage = 1;
        render();
      });
      ['f-provider', 'f-api', 'f-tier', 'f-pricing', 'f-sort'].forEach((id) => {
        document.getElementById(id).addEventListener('change', () => {
          if (id === 'f-sort') {
            try { localStorage.setItem('models.sort', document.getElementById(id).value); } catch (_) { /* ignore */ }
          }
          currentPage = 1;
          render();
        });
      });
      document.getElementById('models-tag-chips').addEventListener('click', (e) => {
        const chip = e.target.closest('[data-tag]');
        if (!chip) return;
        const tag = chip.dataset.tag;
        if (activeTags.has(tag)) {
          activeTags.delete(tag);
          chip.classList.remove('active');
        } else {
          activeTags.add(tag);
          chip.classList.add('active');
        }
        currentPage = 1;
        render();
      });
      document.getElementById('models_page_size').value = String(pageSize);
      document.getElementById('models_page_size').addEventListener('change', (e) => {
        pageSize = parseInt(e.target.value, 10) || 20;
        try { localStorage.setItem('models.pageSize', String(pageSize)); } catch (_) { /* ignore */ }
        currentPage = 1;
        render();
      });
      document.getElementById('models_jump_page').addEventListener('keydown', (e) => {
        if (e.key !== 'Enter') return;
        const input = e.target;
        const target = parseInt(input.value, 10);
        input.value = '';
        if (!Number.isFinite(target) || target < 1 || target > totalPages) {
          if (window.showError) window.showError(t('logs.invalidPage', { total: totalPages }));
          return;
        }
        if (target !== currentPage) {
          currentPage = target;
          render();
        }
      });
      document.getElementById('add-model-btn').addEventListener('click', openAddModal);
      document.getElementById('models-tbody').addEventListener('click', onTableClick);
      document.querySelector('.logs-pagination-card').addEventListener('click', (e) => {
        const btn = e.target.closest('[data-action]');
        if (!btn) return;
        const actions = {
          'first-page': () => { currentPage = 1; },
          'prev-page': () => { currentPage = Math.max(1, currentPage - 1); },
          'next-page': () => { currentPage = Math.min(totalPages, currentPage + 1); },
          'last-page': () => { currentPage = totalPages; }
        };
        const fn = actions[btn.dataset.action];
        if (fn) {
          fn();
          render();
        }
      });
      document.getElementById('addModelModal').addEventListener('click', (e) => {
        if (e.target.id === 'addModelModal' || e.target.closest('[data-action="close-add-modal"]')) closeAddModal();
        if (e.target.closest('[data-action="confirm-add-model"]')) addModel();
      });
      document.getElementById('redirect-search').addEventListener('input', renderRedirectList);
      document.getElementById('redirect-search').addEventListener('keydown', (e) => {
        if (e.key !== 'Enter') return;
        const first = document.querySelector('#redirect-model-list .redirect-item');
        if (first) applyRedirect(first.dataset.model);
      });
      document.getElementById('redirect-model-list').addEventListener('click', (e) => {
        const item = e.target.closest('.redirect-item');
        if (item) applyRedirect(item.dataset.model);
      });
      document.getElementById('redirectModal').addEventListener('click', (e) => {
        if (e.target.id === 'redirectModal' || e.target.closest('[data-action="close-redirect-modal"]')) closeRedirectModal();
        if (e.target.closest('[data-action="clear-redirect"]')) applyRedirect('');
      });
      document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
          closeAddModal();
          closeRedirectModal();
        }
      });
      const savedSort = localStorage.getItem('models.sort');
      if (savedSort && [...document.getElementById('f-sort').options].some((o) => o.value === savedSort)) {
        document.getElementById('f-sort').value = savedSort;
      }
      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(() => { render(); });
      }
      loadModels();
    }
  });

  async function loadModels() {
    try {
      const data = await window.fetchDataWithAuth('/admin/model-registry');
      rows = (data && data.models) || [];
      fillFilterSelects();
      renderTargets();
      renderSummary();
      render();
    } catch (error) {
      window.showNotification(t('models.msg.loadFailed') + ': ' + error.message, 'error');
    }
  }

  // 供应商/API/定价下拉按数据里出现过的取值生成——目录外模型不参与取项。
  function fillFilterSelects() {
    const providers = new Set();
    const apis = new Set();
    const pricings = new Set();
    rows.forEach((r) => {
      const c = r.catalog;
      if (!c) return;
      if (c.provider) providers.add(c.provider);
      if (c.api_provider) apis.add(c.api_provider);
      if (c.pricing_type) pricings.add(c.pricing_type);
    });
    fillSelect('f-provider', providers);
    fillSelect('f-api', apis);
    fillSelect('f-pricing', pricings);
  }

  function fillSelect(id, values) {
    const sel = document.getElementById(id);
    const cur = sel.value;
    [...sel.querySelectorAll('option:not(:first-child)')].forEach((o) => o.remove());
    [...values].sort().forEach((v) => {
      const opt = document.createElement('option');
      opt.value = v;
      opt.textContent = v;
      sel.appendChild(opt);
    });
    sel.value = [...sel.options].some((o) => o.value === cur) ? cur : '';
  }

  // 重定向目标下拉候选 = 当前全部已知模型名（datalist 原生可搜索）。
  function renderTargets() {
    const list = document.getElementById('models-target-list');
    list.innerHTML = '';
    rows.forEach((r) => {
      const opt = document.createElement('option');
      opt.value = r.model;
      list.appendChild(opt);
    });
  }

  // 重定向的判定看 resolved !== model：注册表 redirect_model 与 config
  // 别名都会让最终解析名偏离原值，「已重定向」筛选要两种都盖住。
  function isRedirected(r) {
    return !!r.resolved && r.resolved !== r.model;
  }

  function renderSummary(shown = rows.length) {
    const enabled = rows.filter((r) => r.enabled).length;
    const redirected = rows.filter(isRedirected).length;
    let text = t('models.summary', {
      total: rows.length,
      enabled,
      disabled: rows.length - enabled,
      redirected
    });
    // 筛选中才追加命中数；全量时总数已在 summary 里
    if (shown < rows.length) {
      text += ' · ' + t('models.count', { shown });
    }
    document.getElementById('models-summary').textContent = text;
  }

  // ---- 目录属性：倍率/徽标/标签筛选，移植自旧面板模型目录页 ----

  function multOf(r) {
    const c = r.catalog;
    if (!c) return null;
    if (c.cost_tier === 'free') return 0;
    if (!c.multiplier_known || !c.credit_multiplier) return 1;
    return Number(c.credit_multiplier);
  }

  function multDisplay(r) {
    const c = r.catalog;
    if (!c) return '—';
    if (c.cost_tier === 'free' && (!c.credit_multiplier || c.credit_multiplier === 0)) {
      return badge('0 (FREE)', '#34d399');
    }
    if (!c.multiplier_known || c.credit_multiplier === 0) {
      return `<span style="color: var(--color-text-secondary);" title="${escapeHtml(t('models.mult.unknownTip'))}">— / ≈1.0</span>`;
    }
    const n = Number(c.credit_multiplier);
    return 'x' + (n % 1 ? n.toFixed(1) : String(n));
  }

  function money(v) {
    if (v === undefined || v === null || v === '') return '—';
    const n = Number(v);
    if (!Number.isFinite(n)) return '—';
    return '$' + n.toFixed(4).replace(/\.?0+$/, '');
  }

  function badge(text, color, title) {
    const titleAttr = title ? ` title="${escapeHtml(title)}"` : '';
    return `<span class="model-badge" style="--badge-color:${color};"${titleAttr}>${escapeHtml(text)}</span>`;
  }

  function catalogBadges(r) {
    const c = r.catalog;
    if (!c) return '';
    let out = '';
    const tier = c.cost_tier;
    if (TIER_BADGE[tier]) {
      out += badge(t('models.badge.' + tier), TIER_BADGE[tier]);
    }
    if (c.promo && c.promo.active) {
      out += badge('PROMO' + (c.promo.label ? ' · ' + c.promo.label : ''), '#ec4899');
    }
    if (c.is_beta) out += badge('BETA', '#a855f7');
    if (c.is_new) out += badge('NEW', '#06b6d4');
    if (c.fast && c.fast.active) out += badge('FAST', '#f97316', c.fast.tooltip || '');
    if (c.supports_images) out += badge('img', '#84cc16');
    if (c.is_premium) out += badge('Premium', '#eab308');
    if (c.is_recommended) out += badge(t('models.badge.rec'), '#3b82f6');
    if (c.is_capacity_limited) out += badge(t('models.badge.capLimited'), '#f97316');
    if (c.beta_warning) out += badge(t('models.badge.warning'), '#ef4444', c.beta_warning);
    if (c.disabled) out += badge(t('models.badge.disabled'), '#6b7280');
    return out;
  }

  function matchTags(r) {
    const c = r.catalog || {};
    for (const tag of activeTags) {
      if (tag === 'free' && c.cost_tier !== 'free') return false;
      if (tag === 'promo' && !(c.promo && c.promo.active)) return false;
      if (tag === 'img' && !c.supports_images) return false;
      if (tag === 'beta' && !c.is_beta) return false;
      if (tag === 'new' && !c.is_new) return false;
      if (tag === 'fast' && !(c.fast && c.fast.active)) return false;
      if (tag === 'premium' && !c.is_premium) return false;
      if (tag === 'rec' && !c.is_recommended) return false;
      if (tag === 'empty_mult') {
        const empty = !r.catalog || !c.multiplier_known || !c.credit_multiplier;
        if (!empty) return false;
      }
      if (tag === 'disabled' && !c.disabled) return false;
    }
    return true;
  }

  function visibleRows() {
    const provider = document.getElementById('f-provider').value;
    const api = document.getElementById('f-api').value;
    const tier = document.getElementById('f-tier').value;
    const pricing = document.getElementById('f-pricing').value;
    const sort = document.getElementById('f-sort').value;

    const list = rows.filter((r) => {
      const c = r.catalog;
      if (provider && (!c || c.provider !== provider)) return false;
      if (api && (!c || c.api_provider !== api)) return false;
      if (tier && (!c || c.cost_tier !== tier)) return false;
      if (pricing && (!c || c.pricing_type !== pricing)) return false;
      if (!matchTags(r)) return false;
      if (filterText) {
        const hay = [r.model, c && c.label, c && c.description, c && c.family, c && c.provider, c && c.api_provider]
          .filter(Boolean).join(' ').toLowerCase();
        if (!hay.includes(filterText)) return false;
      }
      if (filterMode === 'disabled') return !r.enabled;
      if (filterMode === 'redirected') return isRedirected(r);
      if (filterMode === 'override') return !!r.has_override;
      return true;
    });

    const num = (v, fallback) => (Number.isFinite(Number(v)) ? Number(v) : fallback);
    list.sort((a, b) => {
      switch (sort) {
        case 'mult_asc': return (multOf(a) ?? 1e9) - (multOf(b) ?? 1e9);
        case 'mult_desc': return (multOf(b) ?? -1) - (multOf(a) ?? -1);
        case 'in_asc': return num(a.catalog && a.catalog.price_input, 1e9) - num(b.catalog && b.catalog.price_input, 1e9);
        case 'in_desc': return num(b.catalog && b.catalog.price_input, -1) - num(a.catalog && a.catalog.price_input, -1);
        case 'out_asc': return num(a.catalog && a.catalog.price_output, 1e9) - num(b.catalog && b.catalog.price_output, 1e9);
        case 'out_desc': return num(b.catalog && b.catalog.price_output, -1) - num(a.catalog && a.catalog.price_output, -1);
        case 'name': return String((a.catalog && a.catalog.label) || a.model).localeCompare(String((b.catalog && b.catalog.label) || b.model));
        default: return a.model < b.model ? -1 : a.model > b.model ? 1 : 0;
      }
    });
    return list;
  }

  function h(tag, cls, text) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (text !== undefined) el.textContent = text;
    return el;
  }

  function sourceBadge(source) {
    const [bg, fg] = SOURCE_COLORS[source] || SOURCE_COLORS.traffic;
    const b = h('span', null, t('models.src.' + source));
    b.style.cssText = `display:inline-block;padding:1px 8px;margin-right:6px;border-radius:6px;font-size:11px;font-weight:500;background:${bg};color:${fg};`;
    return b;
  }

  function updatePagination(shownCount) {
    totalPages = Math.max(1, Math.ceil(shownCount / pageSize));
    if (currentPage > totalPages) currentPage = totalPages;
    document.getElementById('models_current_page').textContent = currentPage;
    document.getElementById('models_total_pages').textContent = totalPages;
    const jump = document.getElementById('models_jump_page');
    jump.max = totalPages;
    jump.placeholder = `1-${totalPages}`;
    const prevDisabled = currentPage <= 1;
    const nextDisabled = currentPage >= totalPages;
    document.getElementById('models_first').disabled = prevDisabled;
    document.getElementById('models_prev').disabled = prevDisabled;
    document.getElementById('models_next').disabled = nextDisabled;
    document.getElementById('models_last').disabled = nextDisabled;
  }

  function render() {
    const tbody = document.getElementById('models-tbody');
    tbody.innerHTML = '';
    const visible = visibleRows();
    updatePagination(visible.length);
    const pageRows = visible.slice((currentPage - 1) * pageSize, currentPage * pageSize);
    document.getElementById('models-empty').hidden = visible.length > 0;
    renderSummary(visible.length);

    const labels = {
      model: t('models.col.model'),
      provider: t('models.col.provider'),
      tags: t('models.col.tags'),
      multiplier: t('models.col.multiplier'),
      priceIn: t('models.col.priceIn'),
      priceCached: t('models.col.priceCached'),
      priceOut: t('models.col.priceOut'),
      source: t('models.col.source'),
      enabled: t('models.col.enabled'),
      resolved: t('models.col.resolved'),
      actions: t('common.actions')
    };

    pageRows.forEach((r) => {
      const c = r.catalog;
      const tr = document.createElement('tr');
      tr.className = 'mobile-card-row';
      if (!r.enabled) tr.style.opacity = '0.55';

      const nameTd = h('td');
      nameTd.dataset.mobileLabel = labels.model;
      const label = (c && c.label) || r.model;
      const strong = h('div');
      strong.style.overflowWrap = 'anywhere';
      const strongText = h('strong', null, label);
      strong.appendChild(strongText);
      nameTd.appendChild(strong);
      if (label !== r.model) {
        const sub = h('div', null, r.model);
        sub.style.cssText = 'font-family:monospace;font-size:11px;color:var(--color-text-secondary);overflow-wrap:anywhere;';
        nameTd.appendChild(sub);
      }
      if (r.has_override) {
        const dot = h('span', null, '●');
        dot.style.cssText = 'color:#fbbf24;font-size:9px;margin-left:6px;vertical-align:middle;';
        dot.title = t('models.filter.override');
        strong.appendChild(dot);
      }
      // 目录外名字（别名/注册表/流量来源）在名称下补一行等宽小字 uid。
      if (!c) {
        const sub = h('div', null, r.model);
        sub.style.cssText = 'font-family:monospace;font-size:11px;color:var(--color-text-secondary);overflow-wrap:anywhere;';
        nameTd.appendChild(sub);
      }

      const providerTd = h('td');
      providerTd.dataset.mobileLabel = labels.provider;
      if (c) {
        providerTd.appendChild(h('div', null, c.provider || '—'));
        const apiSub = [
          c.api_provider && c.api_provider !== c.provider && c.api_provider !== 'UNSPECIFIED' ? c.api_provider : '',
          c.pricing_type && c.pricing_type !== 'STATIC_CREDIT' ? c.pricing_type : ''
        ].filter(Boolean).join(' · ');
        if (apiSub) {
          const sub = h('div', null, apiSub);
          sub.style.cssText = 'font-size:11px;color:var(--color-text-secondary);font-family:monospace;';
          providerTd.appendChild(sub);
        }
      } else {
        providerTd.appendChild(h('span', null, '—')).style.color = 'var(--color-text-secondary)';
      }

      const tagsTd = h('td');
      tagsTd.dataset.mobileLabel = labels.tags;
      tagsTd.innerHTML = catalogBadges(r);

      const multTd = h('td');
      multTd.dataset.mobileLabel = labels.multiplier;
      multTd.innerHTML = multDisplay(r);

      const inTd = h('td', null, c ? money(c.price_input) : '—');
      inTd.dataset.mobileLabel = labels.priceIn;
      const cachedTd = h('td', null, c ? money(c.price_cached) : '—');
      cachedTd.dataset.mobileLabel = labels.priceCached;
      const outTd = h('td', null, c ? money(c.price_output) : '—');
      outTd.dataset.mobileLabel = labels.priceOut;

      const srcTd = h('td');
      srcTd.dataset.mobileLabel = labels.source;
      (r.sources || []).forEach((s) => srcTd.appendChild(sourceBadge(s)));

      const enabledTd = h('td');
      enabledTd.dataset.mobileLabel = labels.enabled;
      const sw = h('button', 'channel-enable-switch ' + (r.enabled ? 'channel-enable-switch--on' : 'channel-enable-switch--off'));
      sw.type = 'button';
      sw.dataset.action = 'toggle';
      sw.dataset.model = r.model;
      sw.setAttribute('role', 'switch');
      sw.setAttribute('aria-checked', String(r.enabled));
      sw.title = r.enabled ? t('models.action.disable') : t('models.action.enable');
      sw.appendChild(h('span', 'channel-enable-switch__knob'));
      enabledTd.appendChild(sw);

      const resolvedTd = h('td');
      resolvedTd.dataset.mobileLabel = labels.resolved;
      const resolvedWrap = h('div', 'resolved-cell');
      if (r.resolved && r.resolved !== r.model) {
        resolvedWrap.appendChild(h('span', 'model-tag', r.resolved));
      } else {
        resolvedWrap.appendChild(h('span', null, '—')).style.color = 'var(--color-text-secondary)';
      }
      const editBtn = h('button', 'redirect-edit-btn');
      editBtn.type = 'button';
      editBtn.dataset.action = 'redirect';
      editBtn.dataset.model = r.model;
      editBtn.title = t('models.redirect.open');
      editBtn.innerHTML = '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M17 3a2.8 2.8 0 1 1 4 4L7.5 20.5 2 22l1.5-5.5Z"/></svg>';
      resolvedWrap.appendChild(editBtn);
      resolvedTd.appendChild(resolvedWrap);

      const actionsTd = h('td');
      actionsTd.dataset.mobileLabel = labels.actions;
      actionsTd.style.whiteSpace = 'nowrap';
      const test = h('button', 'btn btn-secondary', t('models.action.test'));
      test.type = 'button';
      test.dataset.action = 'test';
      test.dataset.model = r.model;
      test.style.padding = '4px 10px';
      test.title = t('models.action.test');
      actionsTd.appendChild(test);
      const chat = h('button', 'btn btn-secondary', t('models.action.chat'));
      chat.type = 'button';
      chat.dataset.action = 'chat';
      chat.dataset.model = r.model;
      chat.style.padding = '4px 10px';
      chat.style.marginLeft = '6px';
      chat.title = t('models.action.chat');
      actionsTd.appendChild(chat);
      if (r.has_override) {
        // 纯注册表行删覆盖即整行消失，标「删除」；目录/别名/流量行删覆盖
        // 只是回到默认态，标「重置」——同一个 DELETE，语义按后果分。
        const registryOnly = (r.sources || []).every((s) => s === 'registry');
        const reset = h('button', 'btn btn-secondary', registryOnly ? t('models.action.delete') : t('models.action.reset'));
        reset.type = 'button';
        reset.dataset.action = registryOnly ? 'delete' : 'reset';
        reset.dataset.model = r.model;
        reset.style.padding = '4px 10px';
        reset.style.marginLeft = '6px';
        actionsTd.appendChild(reset);
      }

      tr.appendChild(nameTd);
      tr.appendChild(providerTd);
      tr.appendChild(tagsTd);
      tr.appendChild(multTd);
      tr.appendChild(inTd);
      tr.appendChild(cachedTd);
      tr.appendChild(outTd);
      tr.appendChild(srcTd);
      tr.appendChild(enabledTd);
      tr.appendChild(resolvedTd);
      tr.appendChild(actionsTd);
      tbody.appendChild(tr);
    });
  }

  function rowOf(model) {
    return rows.find((r) => r.model === model);
  }

  function onTableClick(e) {
    const btn = e.target.closest('[data-action]');
    if (!btn) return;
    const row = rowOf(btn.dataset.model);
    if (!row) return;
    if (btn.dataset.action === 'toggle') {
      save(row.model, !row.enabled, row.redirect_model || '');
    } else if (btn.dataset.action === 'reset') {
      removeOverride(row.model);
    } else if (btn.dataset.action === 'delete') {
      if (window.confirm(t('models.confirmDelete', { model: row.model }))) removeOverride(row.model);
    } else if (btn.dataset.action === 'test') {
      // 探活模态共享自 logs 页：模型锁定本行，协议默认 anthropic。
      window.openModelTestModal({ model: row.model, clientProtocol: 'anthropic' });
    } else if (btn.dataset.action === 'chat') {
      window.openChatModal({ mode: 'admin', model: row.model, clientProtocol: 'anthropic' });
    } else if (btn.dataset.action === 'redirect') {
      openRedirectModal(row);
    }
  }

  function openAddModal() {
    document.getElementById('new-model-name').value = '';
    document.getElementById('new-model-target').value = '';
    const modal = document.getElementById('addModelModal');
    modal.classList.add('show');
    modal.setAttribute('aria-hidden', 'false');
    setTimeout(() => document.getElementById('new-model-name').focus(), 50);
  }

  function closeAddModal() {
    const modal = document.getElementById('addModelModal');
    modal.classList.remove('show');
    modal.setAttribute('aria-hidden', 'true');
  }

  // ---- 重定向目标选择弹窗：搜索过滤全部已知模型名，点选即存 ----
  let redirectRow = null;

  function openRedirectModal(row) {
    redirectRow = row;
    document.getElementById('redirect-model-name').textContent = row.model;
    const search = document.getElementById('redirect-search');
    search.value = row.redirect_model || '';
    // resolved 偏离但 redirect_model 为空 → config 别名在生效，提示这层区别
    document.getElementById('redirect-alias-note').hidden = !(isRedirected(row) && !row.redirect_model);
    const modal = document.getElementById('redirectModal');
    modal.classList.add('show');
    modal.setAttribute('aria-hidden', 'false');
    renderRedirectList();
    setTimeout(() => { search.focus(); search.select(); }, 50);
  }

  function closeRedirectModal() {
    const modal = document.getElementById('redirectModal');
    modal.classList.remove('show');
    modal.setAttribute('aria-hidden', 'true');
    redirectRow = null;
  }

  function renderRedirectList() {
    if (!redirectRow) return;
    const typed = document.getElementById('redirect-search').value.trim();
    const q = typed.toLowerCase();
    const list = document.getElementById('redirect-model-list');
    list.innerHTML = '';
    const cur = redirectRow.redirect_model || '';
    const items = rows
      .filter((r) => r.model !== redirectRow.model)
      .filter((r) => !q || r.model.toLowerCase().includes(q) ||
        (((r.catalog && r.catalog.label) || '').toLowerCase().includes(q)));
    items.forEach((r) => {
      const item = h('button', 'redirect-item' + (r.model === cur ? ' redirect-item--active' : ''));
      item.type = 'button';
      item.dataset.model = r.model;
      item.appendChild(h('span', 'redirect-item-name', r.model));
      const label = r.catalog && r.catalog.label;
      if (label && label !== r.model) item.appendChild(h('span', 'redirect-item-label', label));
      list.appendChild(item);
    });
    // 输入不是已知模型名时给「直接使用」伪项，保住原 datalist 的自由输入能力
    if (typed && !items.some((r) => r.model.toLowerCase() === q)) {
      const item = h('button', 'redirect-item redirect-item--custom');
      item.type = 'button';
      item.dataset.model = typed;
      item.appendChild(h('span', 'redirect-item-name', t('models.redirect.useInput', { target: typed })));
      list.appendChild(item);
    }
    if (!list.children.length) {
      list.appendChild(h('div', 'redirect-empty', t('models.redirect.empty')));
    }
  }

  function applyRedirect(target) {
    if (!redirectRow) return;
    const row = redirectRow;
    closeRedirectModal();
    if ((row.redirect_model || '') === target) return;
    save(row.model, row.enabled, target);
  }

  async function save(model, enabled, redirectModel) {
    try {
      await window.fetchDataWithAuth('/admin/model-registry', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model, enabled, redirect_model: redirectModel })
      });
      window.showNotification(t('models.msg.saved'), 'success');
      await loadModels();
    } catch (error) {
      window.showNotification(t('models.msg.saveFailed') + ': ' + error.message, 'error');
    }
  }

  async function removeOverride(model) {
    try {
      await window.fetchDataWithAuth('/admin/model-registry?model=' + encodeURIComponent(model), { method: 'DELETE' });
      window.showNotification(t('models.msg.saved'), 'success');
      await loadModels();
    } catch (error) {
      window.showNotification(t('models.msg.saveFailed') + ': ' + error.message, 'error');
    }
  }

  async function addModel() {
    const nameInput = document.getElementById('new-model-name');
    const targetInput = document.getElementById('new-model-target');
    const name = nameInput.value.trim();
    if (!name) {
      window.showNotification(t('models.msg.enterName'), 'error');
      return;
    }
    closeAddModal();
    await save(name, true, targetInput.value.trim());
  }
})();
