// 账号页操作层（pool 重构）：操作按钮列、改凭据 modal、加号表单、CLI 凭据导入。
// 经 window.acctOps 供 accounts.js 调用——core 负责点击委托与 loadAll 时调
// acctOps.detect() 探测后端；reload 由 core 注入（acctOps.reload = loadAll）。
// 端点缺席口径（冻结契约）：GET 探测 404/405/501/503/网络错 = 整组缺席 →
// 全部变更按钮隐藏。变更请求的 404 是域错误（无名/无活 lane/死墓碑），
// 只弹 error 文案；仅 405/501/503 才按「该动作端点缺席」隐藏对应按钮。
// PUT 语义：字段缺席=不变，显式空串=清 overlay 覆盖回落 config 值。
(function () {
  'use strict';
  const t = window.t;
  const esc = window.escapeHtml;
  const BASE = '/admin/accounts';
  const PROBE_ABSENT = new Set([404, 405, 501, 503]); // GET 名单无域 404，可安全当缺席
  const ACT_ABSENT = new Set([405, 501, 503]);        // 变更路径的 404 是域错误，不算缺席

  let supported = null; // null=未探测，渲染期乐观显示
  let detectPromise = null;
  const absentActs = new Set();
  const formMounts = new Set(); // 挂过的加号表单容器：detect 落定后重评估（core 只在 run() 挂一次）
  let formSeq = 0;

  function reload() {
    if (window.acctOps && typeof window.acctOps.reload === 'function') window.acctOps.reload();
  }

  function refreshForms() {
    formMounts.forEach((el) => {
      if (!el.isConnected) {
        formMounts.delete(el);
        return;
      }
      mountForm(el);
    });
  }

  function setSupported(v) {
    if (supported === v) return;
    supported = v;
    refreshForms();
  }

  function absentErr(status) {
    const err = new Error(`HTTP ${status}`);
    err.absent = true;
    return err;
  }

  async function apiCall(url, options = {}) {
    const res = await fetchWithAuth(url, options);
    if (ACT_ABSENT.has(res.status)) throw absentErr(res.status);
    const text = await res.text();
    let payload = null;
    if (text) {
      try { payload = JSON.parse(text); } catch (_) { payload = null; }
    }
    if (!res.ok || (payload && payload.success === false)) {
      const msg = payload && typeof payload.error === 'string' && payload.error;
      throw new Error(msg || `HTTP ${res.status}`);
    }
    return payload && 'data' in payload ? payload.data : payload;
  }

  async function detect() {
    try {
      const res = await fetchWithAuth(BASE);
      setSupported(!PROBE_ABSENT.has(res.status));
    } catch (_) {
      setSupported(false);
    }
    return supported;
  }

  function ensureDetected() {
    if (supported === null && !detectPromise) {
      detectPromise = detect().finally(() => { detectPromise = null; });
    }
    return detectPromise;
  }

  function cooldownActive(lane) {
    const now = Date.now();
    return ['auth_cooldown_until', 'unhealthy_until']
      .some((k) => lane[k] && Date.parse(lane[k]) > now);
  }

  function logsHref(name) {
    return `/web/logs.html?account=${encodeURIComponent(name)}`;
  }

  function actionsBlock(a) {
    if (!a || !a.name) return '';
    ensureDetected();
    const name = a.name;
    const btns = [];
    const link = (act, text) =>
      `<a class="acct-action-btn" data-act="${act}" data-acct="${esc(name)}" href="${logsHref(name)}">${esc(text)}</a>`;
    const btn = (act, key, danger) => absentActs.has(act) ? '' :
      `<button type="button" class="acct-action-btn${danger ? ' acct-action-btn--danger' : ''}" data-act="${act}" data-acct="${esc(name)}">${esc(t(key))}</button>`;
    btns.push(link('view-logs', t('accounts.act.viewLogs')));
    if (supported !== false) {
      if (a.source === 'tombstoned') {
        // 墓碑卡动作面只剩 restore（+上面恒有的 view-logs）
        btns.push(btn('restore', 'accounts.act.restore'));
      } else {
        const failover = a.matrix && a.matrix.failoverCount;
        if (failover > 0) btns.push(link('failover', `${t('accounts.drawer.failover')} · ${failover}`));
        btns.push(btn('edit', 'accounts.act.edit'));
        btns.push(btn(a.disabled ? 'enable' : 'disable',
          a.disabled ? 'accounts.act.enable' : 'accounts.act.disable'));
        if (a.lane) {
          if (cooldownActive(a.lane)) btns.push(btn('clear-cooldown', 'accounts.act.clearCooldown'));
          btns.push(btn('refresh-quota', 'accounts.act.refreshQuota'));
        }
        btns.push(btn('delete', 'accounts.act.delete', true));
      }
    }
    return `<div class="acct-actions">${btns.join('')}</div>`;
  }

  async function runMutation(act, url, method, body) {
    try {
      await apiCall(url, {
        method,
        ...(body ? { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) } : {}),
      });
      window.showNotification(t('common.success'), 'success');
      reload();
    } catch (e) {
      if (e.absent) {
        absentActs.add(act);
        reload();
        return;
      }
      window.showNotification(e.message, 'error');
    }
  }

  function confirmDelete(a) {
    const key = a.source === 'panel' ? 'accounts.act.confirmDeletePanel' : 'accounts.act.confirmDelete';
    if (!window.confirm(t(key, { name: a.name }))) return;
    runMutation('delete', `${BASE}/${encodeURIComponent(a.name)}`, 'DELETE');
  }

  // ---- 改凭据 modal：.modal/.show 惯例，懒建一次挂 body ----
  let editModal = null;
  let editCtx = null;

  function buildEditModal() {
    const el = document.createElement('div');
    el.id = 'acct-edit-modal';
    el.className = 'modal';
    el.setAttribute('role', 'dialog');
    el.setAttribute('aria-modal', 'true');
    el.setAttribute('aria-hidden', 'true');
    el.innerHTML = `
      <div class="modal-content modal-content--sm">
        <div class="modal-header">
          <h2 class="modal-title"><span data-m="title"></span> · <span data-m="name"></span></h2>
          <button type="button" class="close-btn" data-m="close">&times;</button>
        </div>
        <div class="modal-body">
          <form class="acct-form" data-m="form">
            <div class="acct-form-row">
              <label class="acct-form-radio"><input type="radio" name="acct-edit-kind" value="token"> <span data-m="tokenLabel"></span></label>
              <label class="acct-form-radio"><input type="radio" name="acct-edit-kind" value="credFile"> <span data-m="credLabel"></span></label>
            </div>
            <div class="acct-form-row"><input class="acct-form-input" data-e="token" spellcheck="false" autocomplete="off"></div>
            <div class="acct-form-row"><input class="acct-form-input" data-e="credFile" spellcheck="false" autocomplete="off" hidden></div>
            <div class="acct-form-err" data-m="err" hidden></div>
          </form>
        </div>
        <div class="modal-footer">
          <button type="button" class="btn btn-secondary" data-m="cancel"></button>
          <button type="button" class="btn btn-primary" data-m="save"></button>
        </div>
      </div>`;
    document.body.appendChild(el);
    const form = el.querySelector('[data-m="form"]');
    el.addEventListener('click', (e) => {
      if (e.target === el || e.target.closest('[data-m="close"],[data-m="cancel"]')) closeEditModal();
    });
    form.addEventListener('change', (e) => {
      if (e.target.name === 'acct-edit-kind') syncEditKind();
    });
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      submitEdit();
    });
    // requestSubmit 让 footer 按钮也能触发原生必填校验
    el.querySelector('[data-m="save"]').addEventListener('click', () => form.requestSubmit());
    return el;
  }

  function syncEditKind() {
    const kind = editModal.querySelector('input[name="acct-edit-kind"]:checked').value;
    const tokenIn = editModal.querySelector('[data-e="token"]');
    const credIn = editModal.querySelector('[data-e="credFile"]');
    tokenIn.hidden = kind !== 'token';
    credIn.hidden = kind !== 'credFile';
    // config 源允许空值提交（=清覆盖回落 config）；panel 源必须给值，否则号无凭据
    const needValue = editCtx && editCtx.source === 'panel';
    tokenIn.required = needValue && kind === 'token';
    credIn.required = needValue && kind === 'credFile';
  }

  function setFormErr(scope, msg) {
    const err = scope.querySelector('[data-m="err"],[data-err]');
    if (!err) return;
    err.textContent = msg;
    err.hidden = !msg;
  }

  function openEditModal(a) {
    editCtx = { name: a.name, source: a.source };
    const el = editModal || (editModal = buildEditModal());
    const kind = a.credential === 'credentials_file' ? 'credFile' : 'token';
    el.querySelector('[data-m="title"]').textContent = t('accounts.act.edit');
    el.querySelector('[data-m="name"]').textContent = a.name;
    el.querySelector('[data-m="close"]').setAttribute('aria-label', t('common.close'));
    el.querySelector('[data-m="tokenLabel"]').textContent = t('accounts.add.token');
    el.querySelector('[data-m="credLabel"]').textContent = t('accounts.add.credFile');
    el.querySelector('[data-m="cancel"]').textContent = t('common.cancel');
    el.querySelector('[data-m="save"]').textContent = t('common.save');
    el.querySelector('[data-e="token"]').placeholder = t('accounts.add.token');
    el.querySelector('[data-e="credFile"]').placeholder = t('accounts.add.credFile');
    el.querySelector(`input[name="acct-edit-kind"][value="${kind}"]`).checked = true;
    el.querySelector('[data-e="token"]').value = '';
    el.querySelector('[data-e="credFile"]').value = '';
    setFormErr(el, '');
    syncEditKind();
    el.classList.add('show');
    el.setAttribute('aria-hidden', 'false');
    setTimeout(() => editModal.querySelector(`[data-e="${kind}"]`).focus(), 50);
  }

  function closeEditModal() {
    if (!editModal) return;
    editModal.classList.remove('show');
    editModal.setAttribute('aria-hidden', 'true');
    editCtx = null;
  }

  async function submitEdit() {
    const kind = editModal.querySelector('input[name="acct-edit-kind"]:checked').value;
    const token = editModal.querySelector('[data-e="token"]').value.trim();
    const cred = editModal.querySelector('[data-e="credFile"]').value.trim();
    // 两键恒发：选中档发值，另一档发空串清覆盖（PUT 空串=回落 config）
    const body = {
      token: kind === 'token' ? token : '',
      credentials_file: kind === 'credFile' ? cred : '',
    };
    try {
      await apiCall(`${BASE}/${encodeURIComponent(editCtx.name)}`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      closeEditModal();
      window.showNotification(t('common.success'), 'success');
      reload();
    } catch (e) {
      if (e.absent) {
        absentActs.add('edit');
        closeEditModal();
        reload();
        return;
      }
      setFormErr(editModal, e.message);
    }
  }

  // ---- 加号表单：#accounts-add 与空态 #accounts-empty-add 共用一套渲染 ----
  function mountForm(el) {
    if (!el) return;
    formMounts.add(el);
    ensureDetected();
    if (supported === false) {
      el.innerHTML = '';
      return;
    }
    const uid = ++formSeq;
    el.innerHTML = `
      <form class="acct-form" data-add>
        <div class="acct-form-row">
          <input class="acct-form-input" data-f="name" placeholder="${esc(t('accounts.add.name'))}" required maxlength="32" pattern="[A-Za-z0-9_-]{1,32}" spellcheck="false" autocomplete="off">
        </div>
        <div class="acct-form-row">
          <label class="acct-form-radio"><input type="radio" name="acct-kind-${uid}" value="token" checked> ${esc(t('accounts.add.token'))}</label>
          <label class="acct-form-radio"><input type="radio" name="acct-kind-${uid}" value="credFile"> ${esc(t('accounts.add.credFile'))}</label>
        </div>
        <div class="acct-form-row"><input class="acct-form-input" data-f="token" placeholder="${esc(t('accounts.add.token'))}" required spellcheck="false" autocomplete="off"></div>
        <div class="acct-form-row"><input class="acct-form-input" data-f="credFile" placeholder="${esc(t('accounts.add.credFile'))}" hidden spellcheck="false" autocomplete="off"></div>
        <div class="acct-form-err" data-err hidden></div>
        <div class="acct-form-row"><button type="submit" class="btn btn-primary">${esc(t('accounts.add.submit'))}</button></div>
      </form>`;
    const form = el.querySelector('form');
    const tokenIn = form.querySelector('[data-f="token"]');
    const credIn = form.querySelector('[data-f="credFile"]');
    form.addEventListener('change', (e) => {
      if (e.target.name !== `acct-kind-${uid}`) return;
      const kind = e.target.value;
      tokenIn.hidden = kind !== 'token';
      credIn.hidden = kind !== 'credFile';
      tokenIn.required = kind === 'token';
      credIn.required = kind === 'credFile';
    });
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      setFormErr(form, '');
      const submitBtn = form.querySelector('button[type="submit"]');
      submitBtn.disabled = true;
      try {
        const kind = form.querySelector(`input[name="acct-kind-${uid}"]:checked`).value;
        const cred = (kind === 'token' ? tokenIn.value : credIn.value).trim();
        await apiCall(BASE, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name: form.querySelector('[data-f="name"]').value.trim(),
            ...(kind === 'token' ? { token: cred } : { credentials_file: cred }),
          }),
        });
        window.showNotification(t('accounts.add.success'), 'success');
        form.reset();
        reload();
      } catch (e2) {
        if (e2.absent) {
          setSupported(false);
          reload();
          return;
        }
        setFormErr(form, e2.message);
      } finally {
        submitBtn.disabled = false;
      }
    });
  }

  // ---- CLI 凭据导入：探针报存在才渲染，点击预填可见的加号表单 ----
  function prefillCredFile(path, suggestedName) {
    const forms = Array.from(document.querySelectorAll('form.acct-form[data-add]'));
    const form = forms.find((f) => f.offsetParent !== null) || forms[0];
    if (!form) return;
    const radio = form.querySelector('input[type="radio"][value="credFile"]');
    if (radio) {
      radio.checked = true;
      radio.dispatchEvent(new Event('change', { bubbles: true }));
    }
    form.querySelector('[data-f="credFile"]').value = path;
    const nameIn = form.querySelector('[data-f="name"]');
    if (suggestedName) nameIn.value = suggestedName;
    nameIn.focus();
  }

  async function maybeCliImport(el) {
    if (!el) return;
    let data = null;
    try {
      const res = await fetchWithAuth(`${BASE}/cli-credentials`);
      if (!res.ok) return;
      const payload = await res.json();
      data = payload && payload.data !== undefined ? payload.data : payload;
    } catch (_) {
      return;
    }
    if (!data || !data.available || !data.path) return;
    const importBtn = document.createElement('button');
    importBtn.type = 'button';
    importBtn.className = 'acct-action-btn';
    importBtn.textContent = t('accounts.empty.importCli');
    importBtn.addEventListener('click', () => {
      prefillCredFile(data.path, data.suggested_name);
      importBtn.textContent = t('accounts.empty.imported');
      importBtn.disabled = true;
    });
    el.appendChild(importBtn);
  }

  function handle(act, name, a) {
    a = a || { name };
    const url = `${BASE}/${encodeURIComponent(name)}`;
    switch (act) {
      case 'view-logs':
      case 'failover':
        window.location.href = logsHref(name);
        return;
      case 'edit':
        openEditModal(a);
        return;
      case 'disable':
        runMutation(act, url, 'PUT', { disabled: true });
        return;
      case 'enable':
        runMutation(act, url, 'PUT', { disabled: false });
        return;
      case 'delete':
        confirmDelete(a);
        return;
      case 'restore':
        runMutation(act, `${url}/restore`, 'POST');
        return;
      case 'clear-cooldown':
        runMutation(act, `${url}/clear-cooldown`, 'POST');
        return;
      case 'refresh-quota':
        runMutation(act, `${url}/quota/refresh`, 'POST');
        return;
    }
  }

  window.acctOps = {
    actionsBlock,
    handle,
    detect,
    mountAddForm: mountForm,
    mountEmptyAdd: mountForm,
    maybeCliImport,
  };
})();
