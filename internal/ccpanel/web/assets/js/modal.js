// 共享模态栈：统一 .modal/.show 惯例的打开/关闭、ESC 与背景点击关闭、
// Tab 焦点圈定与焦点还原；另提供 Promise 版确认框替代原生 confirm()。
// 页面只需 Modal.open/close，不再各自绑 backdrop/Escape/焦点逻辑。
(function () {
  'use strict';

  const t = (key, params) => (typeof window.t === 'function' ? window.t(key, params) : key);
  const stack = [];

  function resolve(target) {
    return typeof target === 'string' ? document.getElementById(target) : target;
  }

  function focusableItems(el) {
    return Array.from(el.querySelectorAll(
      'a[href], button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
    )).filter((x) => !x.hidden && x.getClientRects().length > 0);
  }

  function open(target, opts) {
    const el = resolve(target);
    if (!el || stack.some((s) => s.el === el)) return;
    stack.push({ el, onClose: (opts && opts.onClose) || null, previousFocus: document.activeElement });
    document.querySelector('.app-container')?.setAttribute('inert', '');
    el.classList.add('show');
    el.setAttribute('aria-hidden', 'false');
    const preferred = opts && opts.focus ? el.querySelector(opts.focus) : null;
    (preferred || el.querySelector('.close-btn') || focusableItems(el)[0])?.focus();
  }

  function close(target) {
    const el = resolve(target);
    const idx = stack.findIndex((s) => s.el === el);
    if (idx === -1) {
      el?.classList.remove('show');
      el?.setAttribute('aria-hidden', 'true');
      return;
    }
    const [entry] = stack.splice(idx, 1);
    entry.el.classList.remove('show');
    entry.el.setAttribute('aria-hidden', 'true');
    if (!stack.length) document.querySelector('.app-container')?.removeAttribute('inert');
    if (entry.previousFocus?.isConnected) entry.previousFocus.focus();
    entry.onClose?.();
  }

  function isOpen(target) {
    const el = resolve(target);
    return !!el && stack.some((s) => s.el === el);
  }

  // 背景点击与键盘只作用栈顶模态
  document.addEventListener('click', (e) => {
    const top = stack[stack.length - 1];
    if (top && e.target === top.el) close(top.el);
  });

  document.addEventListener('keydown', (e) => {
    const top = stack[stack.length - 1];
    if (!top) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      close(top.el);
      return;
    }
    if (e.key !== 'Tab') return;
    const items = focusableItems(top.el);
    if (!items.length) return;
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  });

  // ---- 确认框：Promise<boolean>，ESC/背景/取消都归 false ----
  let confirmState = null;

  function ensureConfirmModal() {
    let el = document.getElementById('modalConfirmDialog');
    if (el) return el;
    el = document.createElement('div');
    el.id = 'modalConfirmDialog';
    el.className = 'modal';
    el.setAttribute('role', 'alertdialog');
    el.setAttribute('aria-modal', 'true');
    el.setAttribute('aria-hidden', 'true');
    el.innerHTML = `
      <div class="modal-content modal-content--sm">
        <div class="modal-header">
          <h2 class="modal-title" data-confirm-title></h2>
          <button type="button" class="close-btn" data-confirm-choice="no">&times;</button>
        </div>
        <div class="modal-body"><p class="modal-confirm-message" data-confirm-message></p></div>
        <div class="modal-footer">
          <button type="button" class="btn btn-secondary" data-confirm-choice="no"></button>
          <button type="button" class="btn" data-confirm-choice="yes"></button>
        </div>
      </div>`;
    el.addEventListener('click', (e) => {
      const btn = e.target.closest('[data-confirm-choice]');
      if (btn && el.contains(btn)) settle(btn.dataset.confirmChoice === 'yes');
    });
    document.body.appendChild(el);
    return el;
  }

  function settle(result) {
    const s = confirmState;
    if (!s) return;
    confirmState = null;
    close(s.el);
    s.resolve(result);
  }

  function confirm(message, opts) {
    const el = ensureConfirmModal();
    const options = opts || {};
    el.querySelector('[data-confirm-title]').textContent = options.title || t('common.confirm');
    el.querySelector('[data-confirm-message]').textContent = message;
    const yesBtn = el.querySelector('[data-confirm-choice="yes"]');
    yesBtn.textContent = options.okText || t('common.confirm');
    yesBtn.className = options.danger ? 'btn btn-danger' : 'btn btn-primary';
    el.querySelector('[data-confirm-choice="no"]').textContent = options.cancelText || t('common.cancel');
    el.querySelector('.close-btn').setAttribute('aria-label', t('common.close'));
    return new Promise((res) => {
      confirmState = { el, resolve: res };
      open(el, { focus: '[data-confirm-choice="yes"]', onClose: () => settle(false) });
    });
  }

  window.Modal = { open, close, isOpen, confirm };
})();
