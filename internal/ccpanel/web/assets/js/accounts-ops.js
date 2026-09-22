// 账号页操作层（表格重构）：行尾 测试钮 + ⋯ kebab 菜单、改凭据 modal、
// 加号 modal 表单、空态内嵌表单、CLI 凭据导入、托盘装饰（recover 钮 +
// failover 懒拉）、批量动作循环。
// 经 window.acctOps 供 accounts.js 调用——core 负责点击委托与行展开，
// loadAll 的 fetchAdminAccounts 顺手把 GET 探测结果经 acctOps.reportGetProbe
// 喂过来；reload 由 core 注入（acctOps.reload = loadAll）。
// 端点缺席口径（冻结契约）：GET 探测 404/405/501/503/网络错 = 整组缺席 →
// 全部变更按钮隐藏。变更请求的 404 是域错误（无名/无活 lane/死墓碑），
// 只弹 error 文案；仅 405/501/503 才按「该动作端点缺席」隐藏对应按钮。
// PUT 语义：字段缺席=不变，显式空串=清 overlay 覆盖回落 config 值。
(function () {
  'use strict';
  const t = window.t;
  const esc = window.esc;
  const num = window.formatNumber;
  const BASE = '/admin/accounts';
  const PROBE_ABSENT = new Set([404, 405, 501, 503]); // GET 名单无域 404，可安全当缺席
  const ACT_ABSENT = new Set([405, 501, 503]);        // 变更路径的 404 是域错误，不算缺席

  let supported = null; // null=未探测，渲染期乐观显示
  const absentActs = new Set();
  // 挂载点登记表：el → 'button'（页首 +添加账号 钮）| 'form'（空态内嵌/modal 宿主），
  // 探测落定后按各自形态重渲。
  const formMounts = new Map();
  let formSeq = 0;

  function reload() {
    if (window.acctOps && typeof window.acctOps.reload === 'function') window.acctOps.reload();
  }

  function refreshForms() {
    formMounts.forEach((kind, el) => {
      if (!el.isConnected) {
        formMounts.delete(el);
        return;
      }
      if (kind === 'button') mountAddButton(el); else mountForm(el);
    });
  }

  function setSupported(v) {
    if (supported === v) return;
    supported = v;
    refreshForms();
  }

  // core 的 fetchAdminAccounts 是共享 GET 探测：每轮 loadAll 报一次 status，
  // null 表示网络错（口径同旧 detect 的 catch 分支），随后随下轮探测自愈。
  function reportGetProbe(status) {
    setSupported(status !== null && !PROBE_ABSENT.has(status));
  }

  function absentErr(status) {
    const err = new Error(`HTTP ${status}`);
    err.absent = true;
    return err;
  }

  // 缺席判定必须先于信封解析：405/501/503 的响应体不一定是信封形状。
  async function apiCall(url, options = {}) {
    const res = await fetchWithAuth(url, options);
    if (ACT_ABSENT.has(res.status)) throw absentErr(res.status);
    const payload = await window.parseAPIResponse(res);
    if (!payload.success) throw new Error(payload.error || `HTTP ${res.status}`);
    return 'data' in payload ? payload.data : payload;
  }

  function cooldownActive(lane) {
    const now = Date.now();
    return ['auth_cooldown_until', 'unhealthy_until']
      .some((k) => lane[k] && Date.parse(lane[k]) > now);
  }

  function logsHref(name) {
    return `/web/logs.html?account=${encodeURIComponent(name)}`;
  }

  // ---- 行尾操作格：主钮 测试 + ⋯ kebab ----

  function actionsBlock(a) {
    if (!a || !a.name) return '';
    const name = a.name;
    const parts = [];
    if (supported !== false && a.source !== 'tombstoned' && !absentActs.has('test')) {
      parts.push(`<button type="button" class="acct-action-btn" data-act="test" data-acct="${esc(name)}">${esc(t('accounts.act.test'))}</button>`);
    }
    parts.push(`<button type="button" class="acct-action-btn acct-kebab" data-act="kebab" data-acct="${esc(name)}" aria-haspopup="menu" aria-label="${esc(t('accounts.act.more'))}">⋯</button>`);
    return `<div class="acct-actions">${parts.join('')}</div>`;
  }

  // kebab 项按号态出：view-logs 恒在（走 logs 端点，与账号端点组可用性无关）；
  // 其余变更项受 supported 探测与 absentActs 逐键裁剪。
  function kebabItems(a) {
    const items = [{ act: 'view-logs', key: 'accounts.act.viewLogs' }];
    if (supported === false) return items;
    const add = (act, key, danger) => {
      if (!absentActs.has(act)) items.push({ act, key, danger });
    };
    if (a.source === 'tombstoned') {
      add('restore', 'accounts.act.restore');
      return items;
    }
    add('edit', 'accounts.act.edit');
    add('duplicate', 'accounts.act.duplicate');
    add(a.disabled ? 'enable' : 'disable',
      a.disabled ? 'accounts.act.enable' : 'accounts.act.disable');
    if (a.lane) {
      if (cooldownActive(a.lane)) add('clear-cooldown', 'accounts.act.clearCooldown');
      add('refresh-quota', 'accounts.act.refreshQuota');
    }
    add('delete', 'accounts.act.delete', true);
    return items;
  }

  // body 级共享菜单：fixed 定位贴 ⋯ 钮，外点/Esc/滚动/resize 即关。
  // 菜单项点击不走 core 委托（菜单在行外），自己的监听器转调 handle。
  let kebabEl = null;
  let kebabA = null;

  function closeKebab() {
    if (!kebabEl) return;
    kebabEl.remove();
    kebabEl = null;
    kebabA = null;
    document.removeEventListener('click', onKebabDoc, true);
    document.removeEventListener('keydown', onKebabKey, true);
    window.removeEventListener('resize', closeKebab);
    window.removeEventListener('scroll', onKebabScroll, true);
  }

  function onKebabDoc(e) {
    if (kebabEl && !kebabEl.contains(e.target)) closeKebab();
  }

  function onKebabKey(e) {
    if (e.key === 'Escape') closeKebab();
  }

  function onKebabScroll() {
    closeKebab();
  }

  function openKebab(srcEl, a) {
    if (!srcEl || !a || !a.name) return;
    if (kebabEl) closeKebab();
    const items = kebabItems(a);
    if (!items.length) return;
    const menu = document.createElement('div');
    menu.className = 'acct-menu';
    menu.setAttribute('role', 'menu');
    menu.innerHTML = items.map((it) =>
      `<button type="button" role="menuitem" class="acct-menu-item${it.danger ? ' acct-menu-item--danger' : ''}" data-act="${it.act}">${esc(t(it.key))}</button>`).join('');
    document.body.appendChild(menu);
    const r = srcEl.getBoundingClientRect();
    const mw = menu.offsetWidth;
    const mh = menu.offsetHeight;
    menu.style.left = `${Math.max(8, Math.min(r.right - mw, window.innerWidth - mw - 8))}px`;
    menu.style.top = r.bottom + 4 + mh > window.innerHeight - 8
      ? `${Math.max(8, r.top - mh - 4)}px`
      : `${r.bottom + 4}px`;
    kebabEl = menu;
    kebabA = a;
    menu.addEventListener('click', (e) => {
      const item = e.target.closest('[data-act]');
      if (!item || !kebabA) return;
      const ctx = kebabA;
      closeKebab();
      handle(item.dataset.act, ctx.name, ctx, item);
    });
    // capture 阶段已过本次点击的传播路径，现在挂 capture 监听不会自关。
    document.addEventListener('click', onKebabDoc, true);
    document.addEventListener('keydown', onKebabKey, true);
    window.addEventListener('resize', closeKebab);
    window.addEventListener('scroll', onKebabScroll, true);
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

  // 批量动作：无批量后端，逐号顺序循环单号端点（顺序是刻意的——
  // refresh-quota 之类会打上游，不并发施压）。汇总报 ok/total + 逐条错。
  async function batchRun(act, names) {
    const spec = {
      enable: { method: 'PUT', path: '', body: { disabled: false } },
      disable: { method: 'PUT', path: '', body: { disabled: true } },
      'clear-cooldown': { method: 'POST', path: '/clear-cooldown' },
      'refresh-quota': { method: 'POST', path: '/quota/refresh' },
      delete: { method: 'DELETE', path: '' },
    }[act];
    if (!spec || !names || !names.length) return;
    let ok = 0;
    const errs = [];
    for (const n of names) {
      try {
        await apiCall(`${BASE}/${encodeURIComponent(n)}${spec.path}`, {
          method: spec.method,
          ...(spec.body ? { headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(spec.body) } : {}),
        });
        ok++;
      } catch (e) {
        if (e.absent) absentActs.add(act); else errs.push(`${n}: ${e.message}`);
      }
    }
    const summary = t('accounts.batch.done', { ok, total: names.length });
    window.showNotification(errs.length ? `${summary} · ${errs.slice(0, 3).join(' · ')}` : summary,
      errs.length ? 'error' : 'success');
    reload();
  }

  function confirmDelete(a) {
    const key = a.source === 'panel' ? 'accounts.act.confirmDeletePanel' : 'accounts.act.confirmDelete';
    window.Modal.confirm(t(key, { name: a.name }), { danger: true }).then((ok) => {
      if (ok) runMutation('delete', `${BASE}/${encodeURIComponent(a.name)}`, 'DELETE');
    });
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
            <div class="acct-form-row"><input class="form-input acct-form-mono" data-e="token" spellcheck="false" autocomplete="off"></div>
            <div class="acct-form-row"><input class="form-input acct-form-mono" data-e="credFile" spellcheck="false" autocomplete="off" hidden></div>
            <div class="acct-form-row acct-form-duo">
              <input class="form-input acct-form-mono" data-e="priority" type="number" min="0" step="1" spellcheck="false" autocomplete="off">
              <input class="form-input acct-form-mono" data-e="maxRpm" type="number" min="0" step="1" spellcheck="false" autocomplete="off">
            </div>
            <div class="acct-form-row"><input class="form-input acct-form-mono" data-e="notes" spellcheck="false" autocomplete="off"></div>
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
      if (e.target.closest('[data-m="close"],[data-m="cancel"]')) closeEditModal();
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
    editCtx = {
      name: a.name,
      source: a.source,
      priority: Number(a.priority) || 0,
      max_rpm: Number(a.max_rpm) || 0,
      notes: typeof a.notes === 'string' ? a.notes : '',
    };
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
    el.querySelector('[data-e="priority"]').placeholder = t('accounts.f.priority');
    el.querySelector('[data-e="maxRpm"]').placeholder = t('accounts.f.maxRpm');
    el.querySelector('[data-e="notes"]').placeholder = t('accounts.f.notes');
    el.querySelector(`input[name="acct-edit-kind"][value="${kind}"]`).checked = true;
    el.querySelector('[data-e="token"]').value = '';
    el.querySelector('[data-e="credFile"]').value = '';
    el.querySelector('[data-e="priority"]').value = editCtx.priority || '';
    el.querySelector('[data-e="maxRpm"]').value = editCtx.max_rpm || '';
    el.querySelector('[data-e="notes"]').value = editCtx.notes;
    setFormErr(el, '');
    syncEditKind();
    window.Modal.open(el, {
      focus: `[data-e="${kind}"]`,
      onClose: () => { editCtx = null; },
    });
  }

  function closeEditModal() {
    if (!editModal) return;
    window.Modal.close(editModal);
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
    // priority/max_rpm/notes 指针语义：只在值变化时进 body（缺席=不变），
    // 空输入归一为 0/''——0 即「继承全局」的显式写法，notes '' 即清空。
    const numVal = (sel) => {
      const v = editModal.querySelector(sel).value.trim();
      return v === '' ? 0 : Math.max(0, parseInt(v, 10) || 0);
    };
    const pv = numVal('[data-e="priority"]');
    const mv = numVal('[data-e="maxRpm"]');
    const nv = editModal.querySelector('[data-e="notes"]').value.trim();
    if (pv !== editCtx.priority) body.priority = pv;
    if (mv !== editCtx.max_rpm) body.max_rpm = mv;
    if (nv !== editCtx.notes) body.notes = nv;
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

  // ---- 加号表单：modal 宿主与空态内嵌共用 mountForm 渲染 ----
  // 页首 #accounts-add 是「+ 添加账号」钮（mountAddButton），点开 addModal；
  // 空态 #accounts-empty-add 保留内嵌表单。两处的 form.acct-form[data-add]
  // 同构，visibleAddForm 优先取当前可见者（CLI 导入/duplicate 预填落点）。
  function mountForm(el) {
    if (!el) return;
    formMounts.set(el, 'form');
    if (supported === false) {
      el.innerHTML = '';
      return;
    }
    const uid = ++formSeq;
    el.innerHTML = `
      <form class="acct-form" data-add>
        <div class="acct-form-row">
          <input class="form-input acct-form-mono" data-f="name" placeholder="${esc(t('accounts.add.name'))}" aria-label="${esc(t('accounts.add.name'))}" required maxlength="32" pattern="[A-Za-z0-9_-]{1,32}" spellcheck="false" autocomplete="off">
        </div>
        <div class="acct-form-row">
          <label class="acct-form-radio"><input type="radio" name="acct-kind-${uid}" value="token" checked> ${esc(t('accounts.add.token'))}</label>
          <label class="acct-form-radio"><input type="radio" name="acct-kind-${uid}" value="credFile"> ${esc(t('accounts.add.credFile'))}</label>
          <label class="acct-form-radio"><input type="radio" name="acct-kind-${uid}" value="credContent"> ${esc(t('accounts.add.credContent'))}</label>
        </div>
        <div class="acct-form-row"><input class="form-input acct-form-mono" data-f="token" placeholder="${esc(t('accounts.add.token'))}" aria-label="${esc(t('accounts.add.token'))}" required spellcheck="false" autocomplete="off"></div>
        <div class="acct-form-row"><input class="form-input acct-form-mono" data-f="credFile" placeholder="${esc(t('accounts.add.credFile'))}" aria-label="${esc(t('accounts.add.credFile'))}" hidden spellcheck="false" autocomplete="off"></div>
        <div class="acct-form-row"><textarea class="form-input acct-form-mono" data-f="credContent" placeholder="${esc(t('accounts.add.credContentHint'))}" aria-label="${esc(t('accounts.add.credContent'))}" hidden spellcheck="false" autocomplete="off"></textarea></div>
        <div class="acct-form-row acct-form-duo">
          <input class="form-input acct-form-mono" data-f="priority" type="number" min="0" step="1" placeholder="${esc(t('accounts.f.priority'))}" aria-label="${esc(t('accounts.f.priority'))}" spellcheck="false" autocomplete="off">
          <input class="form-input acct-form-mono" data-f="notes" placeholder="${esc(t('accounts.f.notes'))}" aria-label="${esc(t('accounts.f.notes'))}" spellcheck="false" autocomplete="off">
        </div>
        <div class="acct-form-row">
          <label class="acct-form-check"><input type="checkbox" data-f="verify"> ${esc(t('accounts.add.verify'))}</label>
        </div>
        <div class="acct-form-err" data-err hidden></div>
        <div class="acct-form-row"><button type="submit" class="btn btn-primary">${esc(t('accounts.add.submit'))}</button></div>
      </form>`;
    const form = el.querySelector('form');
    const inputs = {
      token: form.querySelector('[data-f="token"]'),
      credFile: form.querySelector('[data-f="credFile"]'),
      credContent: form.querySelector('[data-f="credContent"]'),
    };
    form.addEventListener('change', (e) => {
      if (e.target.name !== `acct-kind-${uid}`) return;
      const kind = e.target.value;
      for (const [k, input] of Object.entries(inputs)) {
        input.hidden = k !== kind;
        input.required = k === kind;
      }
    });
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      setFormErr(form, '');
      const submitBtn = form.querySelector('button[type="submit"]');
      submitBtn.disabled = true;
      try {
        const kind = form.querySelector(`input[name="acct-kind-${uid}"]:checked`).value;
        const credKey = kind === 'token' ? 'token' : kind === 'credFile' ? 'credentials_file' : 'credentials_content';
        const priority = parseInt(form.querySelector('[data-f="priority"]').value.trim(), 10) || 0;
        const notes = form.querySelector('[data-f="notes"]').value.trim();
        const resp = await apiCall(BASE, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            name: form.querySelector('[data-f="name"]').value.trim(),
            [credKey]: inputs[kind].value.trim(),
            ...(form.querySelector('[data-f="verify"]').checked ? { verify: true } : {}),
            ...(priority > 0 ? { priority } : {}),
            ...(notes ? { notes } : {}),
          }),
        });
        closeAddModal();
        // verify 结构化结果挂在 data.verification：chat 过=已建行，seat/mint
        // 受限只做提示级回填（individual plan 号可服役但无配额/自愈）。
        const ver = resp && resp.verification;
        const vnotes = [];
        if (ver && ver.seat_gated) vnotes.push(t('accounts.st.seatGated'));
        else if (ver && ver.seat === false && ver.seat_error) vnotes.push(String(ver.seat_error));
        if (ver && ver.mint === false) vnotes.push(t('accounts.st.mintUnavailable'));
        window.showNotification(t('accounts.add.success') + (vnotes.length ? ` · ${vnotes.join(' · ')}` : ''), 'success');
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

  function mountAddButton(el) {
    if (!el) return;
    formMounts.set(el, 'button');
    el.innerHTML = supported === false ? '' :
      `<button type="button" class="btn btn-primary" data-act="add-open">${esc(t('accounts.add.open'))}</button>`;
  }

  // 加号 modal：表单宿主每次 open 重挂（i18n/探测态跟随最新），
  // 提交成功统一 closeAddModal（对空态内嵌表单是无害 no-op）。
  let addModal = null;

  function buildAddModal() {
    const el = document.createElement('div');
    el.id = 'acct-add-modal';
    el.className = 'modal';
    el.setAttribute('role', 'dialog');
    el.setAttribute('aria-modal', 'true');
    el.setAttribute('aria-hidden', 'true');
    el.innerHTML = `
      <div class="modal-content modal-content--sm">
        <div class="modal-header">
          <h2 class="modal-title">${esc(t('accounts.add.title'))}</h2>
          <button type="button" class="close-btn" data-m="close" aria-label="${esc(t('common.close'))}">&times;</button>
        </div>
        <div class="modal-body"><div data-m="formhost"></div></div>
      </div>`;
    document.body.appendChild(el);
    el.addEventListener('click', (e) => {
      if (e.target.closest('[data-m="close"]')) closeAddModal();
    });
    return el;
  }

  function openAddModal() {
    const el = addModal || (addModal = buildAddModal());
    mountForm(el.querySelector('[data-m="formhost"]'));
    window.Modal.open(el, { focus: '[data-f="name"]' });
  }

  function closeAddModal() {
    if (!addModal) return;
    window.Modal.close(addModal);
  }

  // ---- CLI 凭据导入与复制预填：共用「找可见加号表单 + 切凭据档」两招 ----
  // 无可见表单时（空态未挂/modal 未开）开 modal 再取——调用方拿不到表单即放弃。
  function visibleAddForm() {
    const forms = Array.from(document.querySelectorAll('form.acct-form[data-add]'));
    return forms.find((f) => f.checkVisibility()) || null;
  }

  function ensureAddForm() {
    const f = visibleAddForm();
    if (f) return f;
    openAddModal();
    return visibleAddForm();
  }

  function setAddKind(form, kind) {
    const radio = form.querySelector(`input[type="radio"][value="${kind}"]`);
    if (radio) {
      radio.checked = true;
      radio.dispatchEvent(new Event('change', { bubbles: true }));
    }
  }

  function prefillCredFile(path, suggestedName) {
    const form = ensureAddForm();
    if (!form) return;
    setAddKind(form, 'credFile');
    form.querySelector('[data-f="credFile"]').value = path;
    const nameIn = form.querySelector('[data-f="name"]');
    if (suggestedName) nameIn.value = suggestedName;
    nameIn.focus();
  }

  // duplicate：开加号表单预填除凭据值外的字段——token 永不上 wire 无从预填，
  // credentials_file 路径可带；名取 <name>-copy（撞名由服务端 409 收口）。
  function duplicateAccount(a) {
    const form = ensureAddForm();
    if (!form) return;
    const kind = a.credential === 'credentials_file' && a.credentials_file ? 'credFile' : 'token';
    setAddKind(form, kind);
    if (kind === 'credFile') form.querySelector('[data-f="credFile"]').value = a.credentials_file || '';
    form.querySelector('[data-f="name"]').value = `${a.name}-copy`;
    if (Number(a.priority) > 0) form.querySelector('[data-f="priority"]').value = Number(a.priority);
    if (a.notes) form.querySelector('[data-f="notes"]').value = a.notes;
    (kind === 'token' ? form.querySelector('[data-f="token"]') : form.querySelector('[data-f="name"]')).focus();
  }

  async function maybeCliImport(el) {
    if (!el) return;
    let data = null;
    try {
      data = await apiCall(`${BASE}/cli-credentials`);
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

  // ---- 托盘装饰：core 展开行后把 view.trayBlock 的静态壳交来补动态件——
  // 证据节尾 recover 钮（端点探测口径）+ failover 槽懒拉。托盘在行的下一行
  // 容器里，core 逐块 diff 不碰展开体，扛得住自动刷新。

  function decorateTray(container, a) {
    if (!container || !a) return;
    const evSec = container.querySelector('.acct-sec--evidence');
    if (evSec && supported !== false && a.source !== 'tombstoned'
        && !(absentActs.has('clear-cooldown') && absentActs.has('refresh-quota'))) {
      evSec.insertAdjacentHTML('beforeend', `<div class="acct-drawer-foot">
        <button type="button" class="acct-action-btn" data-act="recover" data-acct="${esc(a.name)}">${esc(t('accounts.act.recover'))}</button>
        <span class="acct-ev-hint">${esc(t('accounts.ev.hint'))}</span>
      </div>`);
    }
    const slot = container.querySelector('[data-tray="failover"]');
    if (slot) fillFailover(slot, a);
  }

  function secLeft(iso) {
    return Math.max(0, Math.round((Date.parse(iso) - Date.now()) / 1000));
  }

  // failover 明细：logs?account=<name> 取 account_switches>0 的近 5 行，
  // 逐行懒拉 meta.json 取 upstream_attempts（被放弃 lane 的有序明细）与
  // pool_candidates（候选序与降级原因）画时间线。matrix 已计 0 次时
  // 短路出空态，省一次请求。
  async function fillFailover(body, a) {
    if (a.matrix && a.matrix.failoverCount === 0) {
      body.innerHTML = `<div class="acct-none">${esc(t('accounts.fo.empty'))}</div>`;
      return;
    }
    const since = encodeURIComponent(new Date(Date.now() - 24 * 3600e3).toISOString());
    let rows;
    try {
      const data = await window.fetchDataWithAuth(
        `/admin/logs?account=${encodeURIComponent(a.name)}&since=${since}&limit=50`);
      rows = (Array.isArray(data) ? data : []).filter((r) => r && r.account_switches > 0).slice(0, 5);
    } catch (e) {
      if (body.isConnected) {
        body.innerHTML = `<div class="acct-form-err">${esc(t('accounts.fo.error'))}: ${esc(e.message || '')}</div>`;
      }
      return;
    }
    if (!body.isConnected) return;
    if (!rows.length) {
      body.innerHTML = `<div class="acct-none">${esc(t('accounts.fo.empty'))}</div>`;
      return;
    }
    const metas = await Promise.allSettled(rows.map((r) =>
      window.fetchDataWithAuth(`/admin/debug-logs/${r.id}/file/meta.json`)
        .then((d) => JSON.parse((d && d.text) || '{}'))));
    if (!body.isConnected) return;
    body.innerHTML = rows.map((r, i) =>
      failoverRow(r, metas[i].status === 'fulfilled' ? metas[i].value : null)).join('');
  }

  const REASON_KEYS = {
    bound: 'bound',
    bound_yield: 'boundYield',
    auth_cooldown: 'authCooldown',
    generic_cooldown: 'genericCooldown',
    gate_latched: 'gateLatched',
    gate_probing: 'gateProbing',
    gate_window_deadzone: 'gateWindowDeadzone',
    gate_window_full: 'gateWindowFull',
    quota_low: 'quotaLow'
  };

  function failoverRow(row, meta) {
    const at = row.time ? new Date(row.time * 1000) : null;
    const head = `<div class="acct-fo-head"><span class="acct-fo-time">${esc(at ? at.toLocaleString() : '')}</span>${row.model ? ` <span class="acct-fo-model">${esc(row.model)}</span>` : ''} <span class="acct-pill acct-pill--warn">${esc(t('accounts.fo.switches', { n: row.account_switches }))}</span></div>`;
    if (!meta) return `<div class="acct-fo">${head}<div class="acct-none">${esc(t('accounts.fo.noMeta'))}</div></div>`;
    const attempts = Array.isArray(meta.upstream_attempts) ? meta.upstream_attempts : [];
    const cands = Array.isArray(meta.pool_candidates) ? meta.pool_candidates : [];
    const final = meta.upstream_account || row.account || '';
    // elapsed_ms 是自请求起算的放弃时刻偏移，不是单次尝试耗时——渲成 +Nms。
    const tl = attempts.map((att) => `<div class="acct-tl-item"><span class="acct-tl-x">✗</span><span class="acct-tl-acct">${esc(att.account || '?')}</span> ${esc(att.code || '?')} · +${num(att.elapsed_ms || 0)}ms${att.message ? `<div class="acct-tl-msg">${esc(att.message)}</div>` : ''}</div>`).join('')
      + (final ? `<div class="acct-tl-item acct-tl-item--ok"><span class="acct-tl-x">→</span><span class="acct-tl-acct">${esc(final)}</span> ${esc(t('accounts.fo.served'))}</div>` : '');
    const candLine = cands.length
      ? `<div class="acct-fo-cands"><span>${esc(t('accounts.fo.candidates'))}:</span> ${cands.map((c, i) => {
          // reason 是逗号连写多因（auth_cooldown,gate_latched…）——逐词翻译。
          const why = c.reason
            ? c.reason.split(',').map((r) => (REASON_KEYS[r] ? t('accounts.reason.' + REASON_KEYS[r]) : r)).join(' · ')
            : (i === 0 ? t('accounts.reason.selected') : '');
          return `<span class="acct-cand${i === 0 ? ' acct-cand--sel' : ''}${c.bound ? ' acct-cand--bound' : ''}">${esc(c.name)}${why ? `<span class="acct-cand-why">${esc(why)}</span>` : ''}</span>`;
        }).join(' ')}</div>`
      : '';
    return `<div class="acct-fo">${head}<div class="acct-tl">${tl}</div>${candLine}</div>`;
  }

  // ---- 测试：POST {name}/test 恒 200 回 {ok,latency_ms,user?,plan?,error?}，
  // 失败是结果不是 HTTP 错（404 才是域错误）。成功时服务端已顺带清冷却+
  // 记配额信号，reload 让行跟上。
  async function testAccount(url, btn) {
    if (btn) {
      btn.disabled = true;
      btn.classList.add('acct-action-btn--busy');
      btn.textContent = t('accounts.act.testing');
    }
    const reset = () => {
      if (!btn) return;
      btn.disabled = false;
      btn.classList.remove('acct-action-btn--busy');
      btn.textContent = t('accounts.act.test');
    };
    try {
      const d = await apiCall(`${url}/test`, { method: 'POST' });
      const pick = (o, keys) => (o && typeof o === 'object'
        ? keys.map((k) => o[k]).find(Boolean)
        : (typeof o === 'string' ? o : null));
      if (d && d.ok) {
        const who = [pick(d.user, ['email', 'name']), pick(d.plan, ['plan_name', 'name', 'tier'])]
          .filter(Boolean).join(' · ');
        // ok 判据是 chat 面；seat_gated/seat_error 是 seat 面受限的附带信号。
        const snote = d.seat_gated ? t('accounts.st.seatGated')
          : (d.seat === false && d.seat_error ? String(d.seat_error) : '');
        window.showNotification(t('accounts.test.ok', { ms: d.latency_ms ?? '—', who: who ? ` · ${who}` : '' }) + (snote ? ` · ${snote}` : ''), 'success');
        reload();
      } else {
        window.showNotification(t('accounts.test.fail', { err: (d && d.error) || t('accounts.st.unknownError') }), 'error');
      }
    } catch (e) {
      if (e.absent) {
        absentActs.add('test');
        reload();
        return;
      }
      window.showNotification(e.message, 'error');
    } finally {
      reset();
    }
  }

  // ---- 恢复 = clear-cooldown + quota/refresh 复合；闩剩余秒数只读提示。
  async function recoverAccount(url, a) {
    const errors = [];
    try { await apiCall(`${url}/clear-cooldown`, { method: 'POST' }); }
    catch (e) { if (e.absent) absentActs.add('clear-cooldown'); else errors.push(e.message); }
    try { await apiCall(`${url}/quota/refresh`, { method: 'POST' }); }
    catch (e) { if (e.absent) absentActs.add('refresh-quota'); else errors.push(e.message); }
    let latch = '';
    const g = a && a.gate;
    if (g && g.latched && g.limited_until) {
      const s = secLeft(g.limited_until);
      if (s > 0) latch = t('accounts.ev.latchLeft', { s });
    }
    if (errors.length) {
      window.showNotification(errors.join(' · '), 'error');
    } else {
      window.showNotification(t('accounts.recover.done') + (latch ? ` · ${latch}` : ''), 'success');
    }
    reload();
  }

  function handle(act, name, a, srcEl) {
    a = a || { name };
    const url = `${BASE}/${encodeURIComponent(name)}`;
    switch (act) {
      case 'add-open':
        openAddModal();
        return;
      case 'kebab':
        if (a && a.name) openKebab(srcEl, a);
        return;
      case 'view-logs':
        window.location.href = logsHref(name);
        return;
      case 'test':
        testAccount(url, srcEl);
        return;
      case 'recover':
        recoverAccount(url, a);
        return;
      case 'duplicate':
        duplicateAccount(a);
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
    reportGetProbe,
    mountAddForm: mountAddButton,
    mountEmptyAdd: mountForm,
    maybeCliImport,
    decorateTray,
    batchRun,
  };
})();
