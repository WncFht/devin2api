// 全局通知系统：右上角滑入 toast，自动消退。
// 从 ui-topbar.js 拆出，login 等不需要顶栏的页面可单独引入。
(function () {
  function ensureNotifyHost() {
    let host = document.getElementById('notify-host');
    if (!host) {
      host = document.createElement('div');
      host.id = 'notify-host';
      host.style.cssText = 'position:fixed;top:var(--space-6);right:var(--space-6);display:flex;flex-direction:column;gap:var(--space-2);z-index:9999;pointer-events:none;';
      document.body.appendChild(host);
    }
    return host;
  }

  window.ensureNotifyHost = ensureNotifyHost;

  window.showNotification = function (message, type = 'info') {
    const el = document.createElement('div');
    el.className = `notification notification-${type}`;
    el.style.cssText = `
      background: var(--surface-bg-strong);
      backdrop-filter: blur(16px);
      border: 1px solid var(--surface-border);
      border-radius: var(--radius-lg);
      padding: var(--space-4) var(--space-6);
      color: var(--neutral-900);
      font-weight: var(--font-medium);
      opacity: 0;
      transform: translateX(20px);
      transition: all var(--duration-normal) var(--timing-function);
      max-width: 360px;
      box-shadow: 0 10px 25px rgba(0,0,0,0.12);
      overflow: hidden;
      overflow-wrap: anywhere;
      white-space: pre-wrap;
      isolation: isolate;
      pointer-events: auto;
    `;

    if (type === 'success') {
      el.style.background = 'var(--notification-success-bg)';
      el.style.color = 'var(--notification-success-fg)';
      el.style.borderColor = 'var(--notification-success-border)';
      el.style.boxShadow = '0 6px 28px rgba(16,185,129,0.18)';
    } else if (type === 'error') {
      el.style.background = 'var(--notification-error-bg)';
      el.style.color = 'var(--notification-error-fg)';
      el.style.borderColor = 'var(--notification-error-border)';
      el.style.boxShadow = '0 6px 28px rgba(239,68,68,0.18)';
    } else if (type === 'warning') {
      el.style.background = 'var(--notification-warning-bg)';
      el.style.color = 'var(--notification-warning-fg)';
      el.style.borderColor = 'var(--notification-warning-border)';
      el.style.boxShadow = '0 6px 28px rgba(245,158,11,0.18)';
    } else {
      el.style.background = 'var(--notification-info-bg)';
      el.style.color = 'var(--notification-info-fg)';
      el.style.borderColor = 'var(--notification-info-border)';
    }

    el.textContent = message;
    el.setAttribute('role', type === 'error' ? 'alert' : 'status');
    ensureNotifyHost().appendChild(el);
    requestAnimationFrame(() => { el.style.opacity = '1'; el.style.transform = 'translateX(0)'; });
    setTimeout(() => {
      el.style.opacity = '0';
      el.style.transform = 'translateX(20px)';
      setTimeout(() => el.remove(), 320);
    }, 3600);
  };

  window.showSuccess = (msg) => window.showNotification(msg, 'success');
  window.showError = (msg) => window.showNotification(msg, 'error');
  window.showWarning = (msg) => window.showNotification(msg, 'warning');
  window.showInfo = (msg) => window.showNotification(msg, 'info');
})();
