// 模型注册表页：GET/PUT/DELETE /admin/model-registry 的 merged 视图。
// 表格行 = 对外模型名（目录 ∪ 别名 ∪ 注册表 ∪ 流量），可切换启用态、
// 设置重定向目标；有覆盖项的行提供「重置」移除覆盖。
(function () {
  const t = window.t;
  let rows = [];
  let filterText = '';
  let filterMode = 'all';

  // 来源 badge 配色：目录=主色、别名=紫、注册表覆盖=琥珀、仅见过流量=灰。
  const SOURCE_COLORS = {
    catalog: ['rgba(59, 130, 246, 0.18)', 'var(--primary-400)'],
    alias: ['rgba(168, 85, 247, 0.18)', '#c084fc'],
    registry: ['rgba(245, 158, 11, 0.18)', '#fbbf24'],
    traffic: ['rgba(148, 163, 184, 0.15)', 'var(--color-text-secondary)']
  };

  window.initPageBootstrap({
    topbarKey: 'models',
    run: () => {
      document.getElementById('models-filter').addEventListener('input', (e) => {
        filterText = e.target.value.trim().toLowerCase();
        render();
      });
      document.getElementById('models-filter-pills').addEventListener('click', (e) => {
        const btn = e.target.closest('.time-range-btn');
        if (!btn) return;
        document.querySelectorAll('#models-filter-pills .time-range-btn').forEach((b) => b.classList.remove('active'));
        btn.classList.add('active');
        filterMode = btn.dataset.filter;
        render();
      });
      document.getElementById('add-model-btn').addEventListener('click', openAddModal);
      document.getElementById('models-tbody').addEventListener('click', onTableClick);
      document.getElementById('models-tbody').addEventListener('change', onTableChange);
      document.getElementById('addModelModal').addEventListener('click', (e) => {
        if (e.target.id === 'addModelModal' || e.target.closest('[data-action="close-add-modal"]')) closeAddModal();
        if (e.target.closest('[data-action="confirm-add-model"]')) addModel();
      });
      document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') closeAddModal();
      });
      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(render);
      }
      loadModels();
    }
  });

  async function loadModels() {
    try {
      const data = await window.fetchDataWithAuth('/admin/model-registry');
      rows = (data && data.models) || [];
      renderTargets();
      renderSummary();
      render();
    } catch (error) {
      window.showNotification(t('models.msg.loadFailed') + ': ' + error.message, 'error');
    }
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

  function renderSummary() {
    const enabled = rows.filter((r) => r.enabled).length;
    const redirected = rows.filter(isRedirected).length;
    document.getElementById('models-summary').textContent = t('models.summary', {
      total: rows.length,
      enabled,
      disabled: rows.length - enabled,
      redirected
    });
  }

  function visibleRows() {
    return rows.filter((r) => {
      if (filterText && !r.model.toLowerCase().includes(filterText)) return false;
      if (filterMode === 'disabled') return !r.enabled;
      if (filterMode === 'redirected') return isRedirected(r);
      if (filterMode === 'override') return !!r.has_override;
      return true;
    });
  }

  function h(tag, cls, text) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (text !== undefined) el.textContent = text;
    return el;
  }

  function sourceBadge(source) {
    const [bg, fg] = SOURCE_COLORS[source] || SOURCE_COLORS.traffic;
    const badge = h('span', null, t('models.src.' + source));
    badge.style.cssText = `display:inline-block;padding:1px 8px;margin-right:6px;border-radius:6px;font-size:11px;font-weight:500;background:${bg};color:${fg};`;
    return badge;
  }

  function render() {
    const tbody = document.getElementById('models-tbody');
    tbody.innerHTML = '';
    const visible = visibleRows();
    document.getElementById('models-empty').hidden = visible.length > 0;

    const labels = {
      model: t('models.col.model'),
      source: t('models.col.source'),
      enabled: t('models.col.enabled'),
      redirect: t('models.col.redirect'),
      resolved: t('models.col.resolved'),
      actions: t('common.actions')
    };

    visible.forEach((r) => {
      const tr = document.createElement('tr');
      tr.className = 'mobile-card-row';
      if (!r.enabled) tr.style.opacity = '0.55';

      const nameTd = h('td');
      nameTd.dataset.mobileLabel = labels.model;
      const nameTag = h('span', 'model-tag', r.model);
      if (!r.enabled) nameTag.style.cssText += 'background:rgba(239,68,68,0.15);color:#f87171;border-color:rgba(239,68,68,0.3);';
      nameTd.appendChild(nameTag);
      if (r.has_override) {
        const dot = h('span', null, '●');
        dot.style.cssText = 'color:#fbbf24;font-size:9px;margin-left:6px;vertical-align:middle;';
        dot.title = t('models.filter.override');
        nameTd.appendChild(dot);
      }

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

      const redirectTd = h('td');
      redirectTd.dataset.mobileLabel = labels.redirect;
      const input = h('input', 'form-input');
      input.type = 'text';
      input.setAttribute('list', 'models-target-list');
      input.dataset.model = r.model;
      input.value = r.redirect_model || '';
      // 无注册表重定向但 resolved 已偏离（config 别名在生效）时，把落点
      // 写进占位符——空输入框读不出「它其实被别名转走了」。
      input.placeholder = isRedirected(r) ? t('models.aliasPlaceholder', { target: r.resolved }) : '—';
      input.spellcheck = false;
      input.style.minWidth = '160px';
      redirectTd.appendChild(input);

      const resolvedTd = h('td');
      resolvedTd.dataset.mobileLabel = labels.resolved;
      if (r.resolved && r.resolved !== r.model) {
        const arrow = h('span', null, '→ ');
        arrow.style.color = 'var(--color-text-secondary)';
        resolvedTd.appendChild(arrow);
        resolvedTd.appendChild(h('span', 'model-tag', r.resolved));
      } else {
        resolvedTd.appendChild(h('span', null, '—')).style.color = 'var(--color-text-secondary)';
      }

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
      tr.appendChild(srcTd);
      tr.appendChild(enabledTd);
      tr.appendChild(redirectTd);
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
      testModel(row, btn);
    }
  }

  function onTableChange(e) {
    const input = e.target.closest('input[data-model]');
    if (!input) return;
    const row = rowOf(input.dataset.model);
    if (!row) return;
    const target = input.value.trim();
    if (target === (row.redirect_model || '')) return;
    save(row.model, row.enabled, target);
  }

  function openAddModal() {
    document.getElementById('new-model-name').value = '';
    document.getElementById('new-model-target').value = '';
    const modal = document.getElementById('addModelModal');
    modal.style.display = 'block';
    modal.setAttribute('aria-hidden', 'false');
    setTimeout(() => document.getElementById('new-model-name').focus(), 50);
  }

  function closeAddModal() {
    const modal = document.getElementById('addModelModal');
    modal.style.display = 'none';
    modal.setAttribute('aria-hidden', 'true');
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

  // 探活打一次真实 /v1/messages（max_tokens=1）：按钮短暂进 loading，
  // 结果 toast 报状态码与耗时；request_id 在日志里按 client_request_id=
  // panel-probe 可查。
  async function testModel(row, btn) {
    btn.disabled = true;
    const original = btn.textContent;
    btn.textContent = t('models.msg.testing');
    try {
      const res = await window.fetchDataWithAuth('/admin/model-test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ model: row.model })
      });
      if (res && res.ok) {
        window.showNotification(t('models.msg.testOk', { model: row.model, latency: res.latency_ms }), 'success');
      } else {
        window.showNotification(
          t('models.msg.testFail', { model: row.model, status: (res && res.status_code) || '?', error: (res && res.error) || '' }),
          'error'
        );
      }
    } catch (error) {
      window.showNotification(t('models.msg.testFail', { model: row.model, status: '-', error: error.message }), 'error');
    } finally {
      btn.disabled = false;
      btn.textContent = original;
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
