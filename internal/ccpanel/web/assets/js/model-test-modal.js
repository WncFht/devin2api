// 模型探活模态：logs 行内「测试」与模型注册表「探活」共用一份实现。
// 首次 openModelTestModal 时把 DOM 注入 body——页面无需内嵌标记；
// 模型清单取自 GET /admin/model-registry，探针打 POST /admin/model-test
// （与 ccLoad 渠道测试同契约，走真实 /v1 管线）。
(function () {
  const t = (key, params) => (typeof window.t === 'function' ? window.t(key, params) : key);
  const esc = (s) => (typeof window.escapeHtml === 'function' ? window.escapeHtml(String(s ?? '')) : String(s ?? ''));

  const IDS = {
    modal: 'modelTestModal',
    model: 'mtmModel',
    protocol: 'mtmProtocol',
    content: 'mtmContent',
    stream: 'mtmStream',
    progress: 'mtmProgress',
    result: 'mtmResult',
    resultContent: 'mtmResultContent',
    resultDetails: 'mtmResultDetails',
    runBtn: 'mtmRunBtn',
    hint: 'mtmHint'
  };

  let injected = false;
  let state = null;

  function ensureModal() {
    if (injected) return;
    injected = true;
    const host = document.createElement('div');
    host.innerHTML = `
  <div id="${IDS.modal}" class="modal" role="dialog" aria-modal="true" aria-hidden="true">
    <div class="modal-content test-modal-content">
      <div class="modal-header">
        <h2 class="modal-title"><span data-i18n="probe.title">模型探活</span> · <span id="mtmTitle"></span></h2>
        <button type="button" class="close-btn" data-mtm-action="close" data-i18n-aria-label="common.close" aria-label="关闭">&times;</button>
      </div>

      <div class="form-group">
        <label class="form-label" for="${IDS.model}" data-i18n="logs.testModel">测试模型</label>
        <select id="${IDS.model}" class="form-input">
          <option value="" data-i18n="common.loading">加载中...</option>
        </select>
        <small id="${IDS.hint}" class="logs-test-key-hint"></small>
      </div>

      <div class="form-group">
        <label class="form-label" for="${IDS.protocol}" data-i18n="probe.protocol">客户端协议</label>
        <select id="${IDS.protocol}" class="form-input">
          <option value="anthropic">Anthropic (/v1/messages)</option>
          <option value="openai">OpenAI (/v1/chat/completions)</option>
          <option value="codex">Codex (/v1/responses)</option>
        </select>
      </div>

      <div class="form-group">
        <label class="form-label" for="${IDS.content}" data-i18n="logs.testContent">测试内容</label>
        <input type="text" id="${IDS.content}" class="form-input" data-i18n-placeholder="logs.testContentPlaceholder" placeholder="输入测试消息内容">
      </div>

      <div class="form-group">
        <label class="logs-stream-toggle">
          <input type="checkbox" id="${IDS.stream}" checked>
          <span class="form-label" data-i18n="logs.enableStream">启用流式响应</span>
        </label>
      </div>

      <div id="${IDS.progress}" class="test-progress">
        <div class="loading-spinner"></div>
        <p data-i18n="probe.testing">正在测试...</p>
      </div>

      <div id="${IDS.result}" class="test-result">
        <div id="${IDS.resultContent}"></div>
        <div id="${IDS.resultDetails}" class="test-details"></div>
      </div>

      <div class="form-actions">
        <button type="button" class="btn btn-secondary" data-mtm-action="close" data-i18n="common.close">关闭</button>
        <button type="button" id="${IDS.runBtn}" class="btn btn-primary" data-mtm-action="run" data-i18n="logs.startTest">开始测试</button>
      </div>
    </div>
  </div>`;
    document.body.appendChild(host.firstElementChild);

    const modal = document.getElementById(IDS.modal);
    modal.addEventListener('click', (e) => {
      const actionEl = e.target.closest('[data-mtm-action]');
      const action = actionEl ? actionEl.dataset.mtmAction : '';
      if (action === 'run') {
        runTest();
        return;
      }
      if (action === 'close' || e.target === modal) {
        closeModal();
        return;
      }
      const toggle = e.target.closest('[data-mtm-toggle]');
      if (toggle) {
        const target = document.getElementById(toggle.dataset.mtmToggle);
        if (target) target.style.display = target.style.display === 'none' ? 'block' : 'none';
      }
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && modal.classList.contains('show')) closeModal();
    });
    // 注入晚于 i18n 首次扫描——data-i18n 属性已就位，再补一次全页翻译
    // 让模态立即按当前语言渲染；之后的语言切换由 translatePage 兜底。
    if (window.i18n && typeof window.i18n.translatePage === 'function') {
      window.i18n.translatePage();
    }
  }

  function el(id) {
    return document.getElementById(id);
  }

  function closeModal() {
    el(IDS.modal).classList.remove('show');
    el(IDS.modal).setAttribute('aria-hidden', 'true');
    state = null;
  }

  function resetResult() {
    el(IDS.progress).classList.remove('show');
    const result = el(IDS.result);
    result.classList.remove('show', 'success', 'error');
    el(IDS.runBtn).disabled = false;
  }

  // openModelTestModal({model, clientProtocol, content, hint}) 打开探活模态。
  // clientProtocol 决定协议下拉预选；content 是测试内容初值（缺省 ping）。
  async function openModelTestModal(opts) {
    ensureModal();
    opts = opts || {};
    state = {
      model: opts.model || '',
      clientProtocol: opts.clientProtocol || 'anthropic',
      content: opts.content || 'ping',
      hint: opts.hint || ''
    };
    document.getElementById('mtmTitle').textContent = state.model;
    el(IDS.content).value = state.content;
    el(IDS.stream).checked = true;
    el(IDS.protocol).value = ['anthropic', 'openai', 'codex'].includes(state.clientProtocol)
      ? state.clientProtocol
      : 'anthropic';
    el(IDS.hint).textContent = state.hint;
    resetResult();
    const modelSelect = el(IDS.model);
    modelSelect.innerHTML = `<option value="">${esc(t('common.loading'))}</option>`;
    el(IDS.modal).classList.add('show');
    el(IDS.modal).setAttribute('aria-hidden', 'false');

    try {
      const registry = await window.fetchDataWithAuth('/admin/model-registry');
      const names = ((registry && registry.models) || [])
        .map((m) => (typeof m === 'string' ? m : m.model))
        .filter(Boolean);
      modelSelect.innerHTML = '';
      const list = names.length ? names : (state.model ? [state.model] : []);
      list.forEach((name) => {
        const opt = document.createElement('option');
        opt.value = name;
        opt.textContent = name;
        modelSelect.appendChild(opt);
      });
      modelSelect.value = list.includes(state.model) ? state.model : list[0];
    } catch (err) {
      modelSelect.innerHTML = '';
      if (state.model) {
        const opt = document.createElement('option');
        opt.value = state.model;
        opt.textContent = state.model;
        modelSelect.appendChild(opt);
        modelSelect.value = state.model;
      }
      el(IDS.hint).textContent = t('probe.loadModelsFailed') + ': ' + err.message;
    }
  }

  async function runTest() {
    if (!state) return;
    const model = el(IDS.model).value;
    if (!model) {
      if (window.showError) window.showError(t('probe.selectModel'));
      return;
    }
    el(IDS.progress).classList.add('show');
    el(IDS.result).classList.remove('show', 'success', 'error');
    el(IDS.runBtn).disabled = true;
    try {
      const result = await window.fetchDataWithAuth('/admin/model-test', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          model,
          stream: el(IDS.stream).checked,
          content: el(IDS.content).value.trim() || 'ping',
          client_protocol: el(IDS.protocol).value
        })
      });
      renderResult(result || { success: false, error: t('probe.emptyResult') });
    } catch (err) {
      renderResult({ success: false, error: t('probe.requestFailed') + ': ' + err.message });
    } finally {
      el(IDS.progress).classList.remove('show');
      el(IDS.runBtn).disabled = false;
    }
  }

  function toggleBlock(id, labelKey) {
    return `<button type="button" class="btn btn-secondary btn-sm" data-mtm-toggle="${id}" style="margin-bottom: 8px;">${esc(t(labelKey))}</button>`;
  }

  function block(id, text, color) {
    return `<div id="${id}" style="display: none; padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: ${color}; white-space: pre-wrap; font-family: monospace; font-size: 0.85em; max-height: 400px; overflow-y: auto;">${esc(text)}</div>`;
  }

  function renderResult(result) {
    const resultDiv = el(IDS.result);
    const contentDiv = el(IDS.resultContent);
    const detailsDiv = el(IDS.resultDetails);
    resultDiv.classList.remove('success', 'error');
    resultDiv.classList.add('show');

    if (result.success) {
      resultDiv.classList.add('success');
      contentDiv.innerHTML = `<strong>${esc(t('probe.succeeded'))}</strong>`;
      const meta = [`${esc(t('probe.duration'))}: ${result.duration_ms}ms`];
      if (result.first_byte_duration_ms) meta.push(`${esc(t('probe.firstByte'))}: ${result.first_byte_duration_ms}ms`);
      if (result.status_code) meta.push(`${esc(t('probe.statusCode'))}: ${result.status_code}`);
      if (result.actual_model) meta.push(`${esc(t('probe.actualModel'))}: ${esc(result.actual_model)}`);
      if (result.request_id) meta.push(`${esc(t('probe.requestId'))}: ${esc(result.request_id)}`);
      let details = `<p>${meta.join(' | ')}</p>`;
      if (result.response_text) {
        details += `<div style="margin-top: 12px;"><h4 style="margin-bottom: 8px; color: var(--neutral-700);">${esc(t('probe.responseText'))}</h4>` +
          `<div style="padding: 12px; background: var(--neutral-50); border-radius: 4px; border: 1px solid var(--neutral-200); color: var(--neutral-700); white-space: pre-wrap; font-family: monospace; font-size: 0.9em; max-height: 300px; overflow-y: auto;">${esc(result.response_text)}</div></div>`;
      }
      if (result.api_response) {
        const id = 'mtm-resp-' + Date.now();
        details += `<div style="margin-top: 12px;"><h4 style="margin-bottom: 8px; color: var(--neutral-700);">${esc(t('probe.fullResponse'))}</h4>` +
          toggleBlock(id, 'probe.toggleJson') + block(id, JSON.stringify(result.api_response, null, 2), 'var(--neutral-700)') + `</div>`;
      }
      detailsDiv.innerHTML = details;
      return;
    }

    resultDiv.classList.add('error');
    contentDiv.innerHTML = `<strong>${esc(t('probe.failed'))}</strong>`;
    let details = `<p style="color: var(--error-600); margin-top: 8px;">${esc(result.error || t('probe.unknownError'))}</p>`;
    const meta = [];
    if (result.status_code) meta.push(`${esc(t('probe.statusCode'))}: ${result.status_code}`);
    if (result.duration_ms) meta.push(`${esc(t('probe.duration'))}: ${result.duration_ms}ms`);
    if (result.request_id) meta.push(`${esc(t('probe.requestId'))}: ${esc(result.request_id)}`);
    if (meta.length) details += `<p style="margin-top: 8px;">${meta.join(' | ')}</p>`;
    if (result.raw_response) {
      const id = 'mtm-raw-' + Date.now();
      details += `<div style="margin-top: 12px;"><h4 style="margin-bottom: 8px; color: var(--neutral-700);">${esc(t('probe.rawResponse'))}</h4>` +
        toggleBlock(id, 'probe.toggle') + block(id, result.raw_response, 'var(--error-700)') + `</div>`;
    }
    detailsDiv.innerHTML = details;
  }

  window.openModelTestModal = openModelTestModal;
  window.closeModelTestModal = closeModal;
})();
