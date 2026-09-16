// 多轮会话测试模态：models 页「会话」与 tokens 页「测试」共用一份实现。
// mode=admin 走 POST /admin/model-chat——服务端用主密钥把整段 messages 打进
// 真实 /v1 管线（与探活同契约），用来验证模型本身能不能聊。
// mode=token 是 Token Playground——浏览器直接 fetch /v1/*，Authorization
// 用用户粘贴的令牌明文，等价于真实客户端调用，用来验证令牌权限与配额。
// 服务端只存令牌 sha256，明文无法代发，所以 playground 必须放在前端。
(function () {
  const t = (key, params) => (typeof window.t === 'function' ? window.t(key, params) : key);
  const esc = (s) => (typeof window.escapeHtml === 'function' ? window.escapeHtml(String(s ?? '')) : String(s ?? ''));

  const IDS = {
    modal: 'modelChatModal',
    title: 'cmTitle',
    tokenGroup: 'cmTokenGroup',
    token: 'cmToken',
    tokenHint: 'cmTokenHint',
    modelGroup: 'cmModelGroup',
    model: 'cmModel',
    modelList: 'cmModelList',
    protocol: 'cmProtocol',
    messages: 'cmMessages',
    input: 'cmInput',
    sendBtn: 'cmSendBtn'
  };

  let injected = false;
  let state = null;
  let pending = false;

  function ensureModal() {
    if (injected) return;
    injected = true;
    const host = document.createElement('div');
    host.innerHTML = `
  <div id="${IDS.modal}" class="modal" role="dialog" aria-modal="true" aria-hidden="true">
    <div class="modal-content test-modal-content">
      <div class="modal-header">
        <h2 class="modal-title" id="${IDS.title}"></h2>
        <button type="button" class="close-btn" data-cm-action="close" data-i18n-aria-label="common.close" aria-label="关闭">&times;</button>
      </div>

      <div class="chat-controls">
        <div id="${IDS.tokenGroup}" style="display:none; flex:1 1 100%;">
          <input type="password" id="${IDS.token}" class="form-input" autocomplete="off"
            data-i18n-placeholder="chat.tokenPlaceholder" placeholder="粘贴令牌明文（sk-...）">
        </div>
        <div id="${IDS.modelGroup}" class="chat-grow" style="display:none;">
          <input type="text" id="${IDS.model}" class="form-input" list="${IDS.modelList}" spellcheck="false"
            data-i18n-placeholder="chat.modelPlaceholder" placeholder="输入或选择模型名">
          <datalist id="${IDS.modelList}"></datalist>
        </div>
        <select id="${IDS.protocol}" class="form-input" data-i18n-aria-label="probe.protocol" aria-label="客户端协议">
          <option value="anthropic">Anthropic (/v1/messages)</option>
          <option value="openai">OpenAI (/v1/chat/completions)</option>
          <option value="codex">Codex (/v1/responses)</option>
        </select>
      </div>
      <p id="${IDS.tokenHint}" class="chat-hint" style="display:none;" data-i18n="chat.tokenHint">明文仅在创建时可见，请重新粘贴</p>

      <div id="${IDS.messages}" class="chat-messages"></div>

      <div class="chat-composer">
        <textarea id="${IDS.input}" class="form-input" rows="2"
          data-i18n-placeholder="chat.inputPlaceholder" placeholder="输入消息，Enter 发送，Shift+Enter 换行"></textarea>
        <button type="button" id="${IDS.sendBtn}" class="btn btn-primary" data-cm-action="send" data-i18n="chat.send">发送</button>
      </div>

      <div class="chat-footer">
        <button type="button" class="btn btn-secondary" data-cm-action="clear" data-i18n="chat.clear">清空会话</button>
        <button type="button" class="btn btn-secondary" data-cm-action="close" data-i18n="common.close">关闭</button>
      </div>
    </div>
  </div>`;
    document.body.appendChild(host.firstElementChild);

    const modal = document.getElementById(IDS.modal);
    modal.addEventListener('click', (e) => {
      const actionEl = e.target.closest('[data-cm-action]');
      const action = actionEl ? actionEl.dataset.cmAction : '';
      if (action === 'send') {
        send();
      } else if (action === 'clear') {
        state.messages = [];
        renderMessages();
      } else if (action === 'close' || e.target === modal) {
        closeModal();
      }
    });
    document.getElementById(IDS.input).addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        send();
      }
    });
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape' && modal.classList.contains('show')) closeModal();
    });
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
    pending = false;
  }

  // openChatModal({mode:'admin'|'token', model?, clientProtocol?})
  // admin：模型锁入口行，协议可选；token：模型与令牌由用户在模态里填。
  function openChatModal(opts) {
    ensureModal();
    opts = opts || {};
    state = { mode: opts.mode === 'token' ? 'token' : 'admin', model: opts.model || '', messages: [] };
    pending = false;

    const tokenMode = state.mode === 'token';
    el(IDS.title).textContent = tokenMode
      ? t('chat.playgroundTitle')
      : t('chat.title') + ' · ' + state.model;
    el(IDS.tokenGroup).style.display = tokenMode ? '' : 'none';
    el(IDS.tokenHint).style.display = tokenMode ? '' : 'none';
    el(IDS.modelGroup).style.display = tokenMode ? '' : 'none';
    el(IDS.protocol).value = ['anthropic', 'openai', 'codex'].includes(opts.clientProtocol)
      ? opts.clientProtocol
      : 'anthropic';
    el(IDS.sendBtn).disabled = false;
    renderMessages();
    if (tokenMode) fillModelList();

    el(IDS.modal).classList.add('show');
    el(IDS.modal).setAttribute('aria-hidden', 'false');
    el(IDS.input).focus();
  }

  // 候选模型名取注册表全量（datalist 只做提示，仍可自由输入目录外名字）。
  async function fillModelList() {
    try {
      const data = await window.fetchDataWithAuth('/admin/model-registry');
      el(IDS.modelList).innerHTML = ((data && data.models) || [])
        .map((r) => `<option value="${esc(r.model)}"></option>`).join('');
    } catch (_) { /* 列表失败不阻塞手输 */ }
  }

  function renderMessages() {
    const box = el(IDS.messages);
    if (!state.messages.length) {
      box.innerHTML = `<div class="chat-empty">${esc(t('chat.emptyHint'))}</div>`;
      return;
    }
    box.innerHTML = state.messages.map((m) => {
      if (m.role === 'error') {
        return `<div class="chat-msg chat-msg--error">${esc(m.content)}</div>`;
      }
      const cls = m.role === 'user' ? 'chat-msg--user' : 'chat-msg--assistant';
      const pendingCls = m.pending ? ' chat-msg--pending' : '';
      const meta = m.meta ? `<div class="chat-msg-meta">${esc(m.meta)}</div>` : '';
      return `<div class="chat-msg ${cls}${pendingCls}">${esc(m.content)}</div>${meta}`;
    }).join('');
    box.scrollTop = box.scrollHeight;
  }

  // token 模式把 messages 折成与后端 probeChatRequestSpec 相同的三种载荷，
  // 差别只在走浏览器 fetch + 用户令牌，不经服务端代发。
  function tokenRequestSpec(protocol, model, msgs) {
    if (protocol === 'openai') {
      return ['/v1/chat/completions', { model, stream: false, max_tokens: 1024, messages: msgs }];
    }
    if (protocol === 'codex') {
      const input = msgs.map((m) => ({
        type: 'message',
        role: m.role,
        content: [{ type: m.role === 'assistant' ? 'output_text' : 'input_text', text: m.content }]
      }));
      return ['/v1/responses', { model, stream: false, max_output_tokens: 1024, input }];
    }
    const system = msgs.filter((m) => m.role === 'system').map((m) => m.content).join('\n\n');
    const body = { model, stream: false, max_tokens: 1024, messages: msgs.filter((m) => m.role !== 'system') };
    if (system) body.system = system;
    return ['/v1/messages', body];
  }

  function extractText(protocol, body) {
    if (!body) return '';
    if (protocol === 'openai') {
      const c = body.choices && body.choices[0];
      return (c && c.message && c.message.content) || '';
    }
    if (protocol === 'codex') {
      if (typeof body.output_text === 'string') return body.output_text;
      return (body.output || [])
        .flatMap((o) => o.content || [])
        .filter((c) => c.type === 'output_text')
        .map((c) => c.text || '')
        .join('');
    }
    return (body.content || [])
      .filter((c) => c.type === 'text')
      .map((c) => c.text || '')
      .join('');
  }

  function extractError(body) {
    if (!body) return '';
    const e = body.error;
    if (typeof e === 'string') return e;
    if (e && (e.message || e.type)) return e.message || e.type;
    return body.message || '';
  }

  async function send() {
    if (!state || pending) return;
    const text = el(IDS.input).value.trim();
    if (!text) return;

    const protocol = el(IDS.protocol).value;
    const model = state.mode === 'token' ? el(IDS.model).value.trim() : state.model;
    const token = state.mode === 'token' ? el(IDS.token).value.trim() : '';
    if (state.mode === 'token' && !token) {
      state.messages.push({ role: 'error', content: t('chat.missingToken') });
      renderMessages();
      return;
    }
    if (!model) {
      state.messages.push({ role: 'error', content: t('chat.missingModel') });
      renderMessages();
      return;
    }

    pending = true;
    el(IDS.sendBtn).disabled = true;
    el(IDS.input).value = '';
    state.messages.push({ role: 'user', content: text });
    state.messages.push({ role: 'assistant', content: t('chat.waiting'), pending: true });
    renderMessages();

    const startedAt = Date.now();
    const turns = state.messages.filter((m) => !m.pending && m.role !== 'error');
    try {
      if (state.mode === 'admin') {
        const r = await window.fetchDataWithAuth('/admin/model-chat', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            model,
            client_protocol: protocol,
            stream: false,
            messages: turns.map((m) => ({ role: m.role, content: m.content }))
          })
        });
        if (r && r.success) {
          commitPending(r.response_text || t('chat.emptyResponse'),
            `${r.duration_ms}ms` + (r.actual_model && r.actual_model !== model ? ` · ${r.actual_model}` : ''));
        } else {
          failPending((r && (r.error || r.response_text)) || t('chat.failed'));
        }
      } else {
        const [path, payload] = tokenRequestSpec(protocol, model, turns);
        const resp = await fetch(path, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', Authorization: 'Bearer ' + token },
          body: JSON.stringify(payload)
        });
        const body = await resp.json().catch(() => null);
        if (!resp.ok) {
          failPending(`HTTP ${resp.status}` + (extractError(body) ? ` · ${extractError(body)}` : ''));
        } else {
          commitPending(extractText(protocol, body) || t('chat.emptyResponse'), `${Date.now() - startedAt}ms`);
        }
      }
    } catch (err) {
      failPending(t('chat.failed') + ': ' + err.message);
    } finally {
      pending = false;
      el(IDS.sendBtn).disabled = false;
      el(IDS.input).focus();
    }
  }

  function commitPending(content, meta) {
    const last = state.messages[state.messages.length - 1];
    if (last && last.pending) {
      last.content = content;
      last.meta = meta;
      delete last.pending;
    }
    renderMessages();
  }

  function failPending(message) {
    state.messages.pop();
    state.messages.push({ role: 'error', content: message });
    renderMessages();
  }

  window.openChatModal = openChatModal;
})();
