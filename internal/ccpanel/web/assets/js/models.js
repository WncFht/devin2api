// 模型注册表页：GET/PUT/DELETE /admin/model-registry 的 merged 视图。
// 表格行 = 对外模型名（目录 ∪ 别名 ∪ 注册表 ∪ 流量），可切换启用态、
// 设置重定向目标；有覆盖项的行提供「重置」移除覆盖。
(function () {
  const t = window.t;
  let rows = [];
  let filterText = '';

  window.initPageBootstrap({
    topbarKey: 'models',
    run: () => {
      document.getElementById('models-filter').addEventListener('input', (e) => {
        filterText = e.target.value.trim().toLowerCase();
        render();
      });
      document.getElementById('add-model-btn').addEventListener('click', addModel);
      document.getElementById('models-tbody').addEventListener('click', onTableClick);
      document.getElementById('models-tbody').addEventListener('change', onTableChange);
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

  function h(tag, cls, text) {
    const el = document.createElement(tag);
    if (cls) el.className = cls;
    if (text !== undefined) el.textContent = text;
    return el;
  }

  function render() {
    const tbody = document.getElementById('models-tbody');
    tbody.innerHTML = '';
    const visible = rows.filter((r) => !filterText || r.model.toLowerCase().includes(filterText));
    document.getElementById('models-empty').hidden = visible.length > 0;

    visible.forEach((r) => {
      const tr = document.createElement('tr');
      tr.className = 'mobile-card-row';

      const nameTd = h('td');
      nameTd.appendChild(h('span', 'ch-name-cell', r.model));

      const srcTd = h('td');
      (r.sources || []).forEach((s) => {
        const badge = h('span', 'redirect-badge');
        badge.style.marginLeft = '0';
        badge.style.marginRight = '6px';
        badge.textContent = s;
        srcTd.appendChild(badge);
      });

      const enabledTd = h('td');
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
      const input = h('input', 'form-input');
      input.type = 'text';
      input.setAttribute('list', 'models-target-list');
      input.dataset.model = r.model;
      input.value = r.redirect_model || '';
      input.placeholder = '—';
      input.spellcheck = false;
      input.style.minWidth = '160px';
      redirectTd.appendChild(input);

      const resolvedTd = h('td', null, r.resolved === r.model ? '' : r.resolved);
      if (r.resolved !== r.model) resolvedTd.className = 'redirect-badge';

      const actionsTd = h('td');
      if (r.has_override) {
        const reset = h('button', 'btn btn-secondary', t('models.action.reset'));
        reset.type = 'button';
        reset.dataset.action = 'reset';
        reset.dataset.model = r.model;
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
    await save(name, true, targetInput.value.trim());
    nameInput.value = '';
    targetInput.value = '';
  }
})();
