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
  const num = window.formatNumber || ((v) => String(v));
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
    // failover 徽标走 logs/debug-logs 端点，与 /admin/accounts 可用性无关——
    // supported 探测失败时也照常渲染（抽屉内容懒拉，失败在行内报错）。
    const failover = a.matrix && a.matrix.failoverCount;
    if (a.source !== 'tombstoned' && failover > 0) {
      btns.push(`<button type="button" class="acct-action-btn" data-act="failover" data-acct="${esc(name)}">${esc(t('accounts.drawer.failover'))} · ${failover}</button>`);
    }
    if (supported !== false) {
      if (a.source === 'tombstoned') {
        // 墓碑卡动作面只剩 restore（+上面恒有的 view-logs）
        btns.push(btn('restore', 'accounts.act.restore'));
      } else {
        btns.push(btn('test', 'accounts.act.test'));
        btns.push(btn('edit', 'accounts.act.edit'));
        btns.push(btn('duplicate', 'accounts.act.duplicate'));
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
            <div class="acct-form-row acct-form-duo">
              <input class="acct-form-input" data-e="priority" type="number" min="0" step="1" spellcheck="false" autocomplete="off">
              <input class="acct-form-input" data-e="maxRpm" type="number" min="0" step="1" spellcheck="false" autocomplete="off">
            </div>
            <div class="acct-form-row"><input class="acct-form-input" data-e="notes" spellcheck="false" autocomplete="off"></div>
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
          <label class="acct-form-radio"><input type="radio" name="acct-kind-${uid}" value="credContent"> ${esc(t('accounts.add.credContent'))}</label>
        </div>
        <div class="acct-form-row"><input class="acct-form-input" data-f="token" placeholder="${esc(t('accounts.add.token'))}" required spellcheck="false" autocomplete="off"></div>
        <div class="acct-form-row"><input class="acct-form-input" data-f="credFile" placeholder="${esc(t('accounts.add.credFile'))}" hidden spellcheck="false" autocomplete="off"></div>
        <div class="acct-form-row"><textarea class="acct-form-input" data-f="credContent" placeholder="${esc(t('accounts.add.credContentHint'))}" hidden spellcheck="false" autocomplete="off"></textarea></div>
        <div class="acct-form-row acct-form-duo">
          <input class="acct-form-input" data-f="priority" type="number" min="0" step="1" placeholder="${esc(t('accounts.f.priority'))}" spellcheck="false" autocomplete="off">
          <input class="acct-form-input" data-f="notes" placeholder="${esc(t('accounts.f.notes'))}" spellcheck="false" autocomplete="off">
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
        await apiCall(BASE, {
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

  // ---- CLI 凭据导入与复制预填：共用「找可见加号表单 + 切凭据档」两招 ----
  function visibleAddForm() {
    const forms = Array.from(document.querySelectorAll('form.acct-form[data-add]'));
    return forms.find((f) => f.offsetParent !== null) || forms[0] || null;
  }

  function setAddKind(form, kind) {
    const radio = form.querySelector(`input[type="radio"][value="${kind}"]`);
    if (radio) {
      radio.checked = true;
      radio.dispatchEvent(new Event('change', { bubbles: true }));
    }
  }

  function prefillCredFile(path, suggestedName) {
    const form = visibleAddForm();
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
    const form = visibleAddForm();
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

  // ---- 抽屉壳：failover 明细与「为什么病了」证据共用 ----
  // 挂在卡的 [data-slot=drawer] 槽里；core 逐块 diff 不碰此槽，开着的抽屉
  // 扛得住自动刷新（auto-refresh 另有 .acct-drawer[open] 跳本轮兜底）。
  function relText(iso) {
    const ms = Date.now() - Date.parse(iso);
    if (!Number.isFinite(ms)) return '';
    const m = Math.floor(ms / 60000);
    if (m < 1) return t('accounts.justNow');
    if (m < 60) return t('accounts.minAgo', { m });
    const h = Math.floor(m / 60);
    if (h < 24) return t('accounts.hourAgo', { h, m: m % 60 });
    return t('accounts.dayAgo', { d: Math.floor(h / 24), h: h % 24 });
  }

  function leftText(iso) {
    const ms = Date.parse(iso) - Date.now();
    if (!Number.isFinite(ms) || ms <= 0) return t('accounts.now');
    const m = Math.ceil(ms / 60000);
    if (m >= 1440) return t('accounts.inDaysHours', { d: Math.floor(m / 1440), h: Math.floor((m % 1440) / 60) });
    if (m >= 60) return t('accounts.inHoursMinutes', { h: Math.floor(m / 60), m: m % 60 });
    return t('accounts.inMinutes', { m });
  }

  function secLeft(iso) {
    return Math.max(0, Math.round((Date.parse(iso) - Date.now()) / 1000));
  }

  // 闩事件 kind+detail → i18n 文案；未知 kind 用服务端产出 label 兜底。
  function gateEventText(ev) {
    const key = ev.kind === 'latched' && ev.detail === 'extended' ? 'extended' : ev.kind;
    const known = { latched: 1, extended: 1, released: 1, expired: 1, restored: 1 };
    return known[key] ? t('accounts.gate.ev.' + key) : (ev.label || ev.kind || '');
  }

  function openDrawer(srcEl, kind, a) {
    const card = srcEl && srcEl.closest('.acct-card');
    const slot = card && card.querySelector('[data-slot="drawer"]');
    if (!slot || !a || !a.name) return;
    const titleKey = kind === 'failover' ? 'accounts.drawer.failover' : 'accounts.drawer.evidence';
    slot.innerHTML = `<details class="acct-drawer" open>
      <summary>${esc(t(titleKey))} · ${esc(a.name)}</summary>
      <div class="acct-drawer-body">${kind === 'failover'
        ? `<div class="acct-none">${esc(t('accounts.drawer.loading'))}</div>`
        : evidenceHTML(a)}</div>
    </details>`;
    if (kind === 'failover') fillFailover(slot.querySelector('.acct-drawer-body'), a);
  }

  // 证据抽屉：lane.last_failure_* + 两档冷却 + fail_streak/bound_sessions +
  // gate 闩与 events 环 + 最近配额采样，全部现有快照源合成，不发请求。
  // 「恢复」= 清池侧冷却 + 立即刷配额；闩是上游真值只读展示，不提供清闩。
  function evidenceHTML(a) {
    const lane = a.lane || {};
    const gate = a.gate || {};
    const now = Date.now();
    const future = (iso) => iso && Date.parse(iso) > now;
    const row = (label, val) => (val
      ? `<div class="acct-kv"><span class="acct-kv-k">${esc(label)}</span><span class="acct-kv-v">${val}</span></div>` : '');
    const secs = [];
    // 无 lane（disabled/tombstoned 或非 lane 集成员）不假装健康——显式 noData。
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
      secs.push(row(t('accounts.ev.failure'), `${esc(relText(lane.last_failure_at))} · ${esc(code)}${msg}`));
    }
    const cds = [];
    if (future(lane.auth_cooldown_until)) cds.push(`${esc(t('accounts.ev.credCooldown'))} ${esc(leftText(lane.auth_cooldown_until))}`);
    if (future(lane.unhealthy_until)) cds.push(`${esc(t('accounts.ev.failCooldown'))} ${esc(leftText(lane.unhealthy_until))}`);
    if (cds.length) secs.push(row(t('accounts.ev.cooldowns'), cds.join(' · ')));
    if (gate.latched) {
      secs.push(row(t('accounts.ev.latch'), esc(gate.limited_until
        ? t('accounts.ev.latchLeft', { s: secLeft(gate.limited_until) })
        : t('accounts.st.latched'))));
    }
    const pts = (a.quota && Array.isArray(a.quota.points)) ? a.quota.points : [];
    const last = pts.length ? pts[pts.length - 1] : null;
    if (last && (last.daily_remaining !== undefined || last.weekly_remaining !== undefined)) {
      const pct = (v) => (v === null || v === undefined ? '—' : `${Number(v).toFixed(0)}%`);
      secs.push(row(t('accounts.ev.quota'),
        esc(`${t('accounts.f.daily')} ${pct(last.daily_remaining)} · ${t('accounts.f.weekly')} ${pct(last.weekly_remaining)}`)
        + (last.at ? ` <span class="acct-ev-at">${esc(new Date(last.at * 1000).toLocaleTimeString())}</span>` : '')));
    }
    const events = Array.isArray(gate.events) ? gate.events.slice(0, 8) : [];
    const evRows = events.map((ev) => `<div class="acct-ev-ev"><span class="acct-ev-kind">${esc(gateEventText(ev))}</span> ${esc(relText(ev.at))}${ev.until && Date.parse(ev.until) > now ? ` · ${esc(leftText(ev.until))}` : ''}</div>`).join('');
    const evSec = `<div class="acct-ev-events"><h5>${esc(t('accounts.ev.events'))}</h5>${evRows || `<div class="acct-none">${esc(t('accounts.ev.noEvents'))}</div>`}</div>`;
    const body = (secs.length > 1 || evRows)
      ? `<div class="acct-kv-grid">${secs.join('')}</div>${evSec}`
      : `<div class="acct-none">${esc(t('accounts.ev.none'))}</div>`;
    const canRecover = supported !== false
      && !(absentActs.has('clear-cooldown') && absentActs.has('refresh-quota'));
    const foot = canRecover ? `<div class="acct-drawer-foot">
      <button type="button" class="acct-action-btn" data-act="recover" data-acct="${esc(a.name)}">${esc(t('accounts.act.recover'))}</button>
      <span class="acct-ev-hint">${esc(t('accounts.ev.hint'))}</span>
    </div>` : '';
    return body + foot;
  }

  // failover 抽屉：logs?account=<name> 取 account_switches>0 的近 5 行，
  // 逐行懒拉 meta.json 取 upstream_attempts（被放弃 lane 的有序明细）与
  // pool_candidates（候选序与降级原因）画时间线。
  async function fillFailover(body, a) {
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
  // 记配额信号，reload 让卡片跟上。
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
        window.showNotification(t('accounts.test.ok', { ms: d.latency_ms ?? '—', who: who ? ` · ${who}` : '' }), 'success');
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
      case 'view-logs':
        window.location.href = logsHref(name);
        return;
      case 'failover':
      case 'evidence':
        openDrawer(srcEl, act, a);
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
    detect,
    mountAddForm: mountForm,
    mountEmptyAdd: mountForm,
    maybeCliImport,
  };
})();
