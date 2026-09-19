// 版本更新卡片：GET /admin/update/status 渲染进度徽标与版本格，
// POST check/start/rollback 驱动自更新。在途时 1s 轮询——重启窗口内
// fetch 失败按「等待服务恢复」处理，编排进程接管后继续读同一条 db
// 记录，done/failed 终态停轮询并 toast。
(function () {
  const t = window.t;
  const i18nText = window.i18nText;

  const els = {};
  const IN_FLIGHT = new Set(['downloading', 'verifying', 'spawning', 'swapping', 'restarting']);
  let pollTimer = null;
  let pollFailures = 0;
  let lastStatus = null;   // 最近一次 status 视图（latest 渲染要参考 supported）
  let lastCheck = null;    // 最近一次 check 视图
  let terminalSeen = false; // 本次轮询已 toast 过终态，防重复

  function phaseText(phase) {
    return i18nText('settings.update.phase.' + phase, phase);
  }

  function setBadge(variant, text) {
    const b = els.badge;
    if (!variant) {
      b.hidden = true;
      return;
    }
    b.hidden = false;
    b.className = 'runtime-transcript-status runtime-transcript-status--' + variant;
    b.textContent = text;
  }

  function setError(text) {
    if (!text) {
      els.error.hidden = true;
      els.error.textContent = '';
      return;
    }
    els.error.hidden = false;
    els.error.textContent = text;
  }

  function inFlight(view) {
    return !!(view && view.update && IN_FLIGHT.has(view.update.phase));
  }

  function render(view) {
    lastStatus = view;
    els.current.textContent = view.current || '—';
    const upd = view.update;
    const flying = inFlight(view);

    els.startBtn.disabled = !view.supported || flying;
    els.checkBtn.disabled = !view.supported;
    els.rollbackBtn.hidden = !view.rollback_available;
    els.rollbackBtn.disabled = flying;

    if (!view.supported) {
      setBadge('unavailable', t('settings.update.unsupported'));
      setError(view.unsupported_reason || t('settings.update.unsupported'));
      els.progressCell.hidden = true;
      renderCheck(lastCheck);
      return;
    }

    if (!upd) {
      setBadge(null);
      els.progressCell.hidden = true;
      setError(lastCheck && lastCheck.error ? lastCheck.error : null);
      renderCheck(lastCheck);
      return;
    }

    els.progressCell.hidden = false;
    const route = (upd.rollback ? t('settings.update.rollbackTag') + ' ' : '') +
      (upd.from || '?') + ' → ' + (upd.to || '?');
    if (flying) {
      setBadge('warning', phaseText(upd.phase) + (view.stale ? ' · ' + t('settings.update.stale') : ''));
      els.progress.textContent = pollFailures > 5
        ? t('settings.update.reconnecting')
        : phaseText(upd.phase) + ' · ' + route;
      setError(null);
    } else if (upd.phase === 'done') {
      setBadge('normal', t('settings.update.phase.done'));
      els.progress.textContent = route;
      setError(lastCheck && lastCheck.error ? lastCheck.error : null);
    } else if (upd.phase === 'failed') {
      setBadge('exceeded', t('settings.update.phase.failed'));
      els.progress.textContent = route;
      setError(upd.error || t('settings.update.phase.failed'));
    }
    renderCheck(lastCheck);
  }

  function renderCheck(data) {
    if (!data) return;
    lastCheck = data;
    els.latest.textContent = data.latest || '—';
    if (data.error && !inFlight(lastStatus)) {
      setError(data.error);
    } else if (!data.error && lastStatus && !lastStatus.update) {
      setError(null);
    }
    if (lastStatus && lastStatus.supported && !inFlight(lastStatus)) {
      els.startBtn.disabled = false;
    }
  }

  async function refresh() {
    try {
      render(await window.fetchDataWithAuth('/admin/update/status'));
    } catch (e) {
      setError(e.message);
    }
  }

  async function refreshCheck() {
    try {
      renderCheck(await window.fetchDataWithAuth('/admin/update/check', { method: 'POST' }));
    } catch (e) {
      setError(e.message);
    }
  }

  async function poll() {
    let view;
    try {
      view = await window.fetchDataWithAuth('/admin/update/status');
      pollFailures = 0;
    } catch (e) {
      // 重启窗口：旧实例已退、新实例未绑——保持轮询不报错。
      pollFailures++;
      if (els.progressCell && !els.progressCell.hidden) {
        els.progress.textContent = t('settings.update.reconnecting');
      }
      return;
    }
    render(view);
    const upd = view.update;
    if (!upd || !IN_FLIGHT.has(upd.phase)) {
      stopPolling();
      if (!terminalSeen && upd) {
        if (upd.phase === 'done') {
          window.showSuccess(t('settings.update.toastDone', { to: upd.to || '' }));
        } else if (upd.phase === 'failed') {
          window.showError(upd.error || t('settings.update.toastFailed'));
        }
      }
      refreshCheck();
    }
  }

  function startPolling() {
    terminalSeen = false;
    pollFailures = 0;
    if (!pollTimer) {
      pollTimer = setInterval(poll, 1000);
    }
  }

  function stopPolling() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  async function startUpdate(tag) {
    const ok = await window.Modal.confirm(
      t('settings.update.confirmStart', { to: tag || t('settings.update.latestUnknown') }),
      { okText: t('settings.update.start') }
    );
    if (!ok) return;
    els.startBtn.disabled = true;
    try {
      const body = tag ? JSON.stringify({ tag }) : undefined;
      await window.fetchDataWithAuth('/admin/update', {
        method: 'POST',
        headers: body ? { 'Content-Type': 'application/json' } : undefined,
        body,
      });
      startPolling();
      poll();
    } catch (e) {
      window.showError(e.message);
      refresh();
    }
  }

  async function startRollback() {
    const ok = await window.Modal.confirm(
      t('settings.update.confirmRollback'),
      { okText: t('settings.update.rollback') }
    );
    if (!ok) return;
    els.rollbackBtn.disabled = true;
    try {
      await window.fetchDataWithAuth('/admin/update/rollback', { method: 'POST' });
      startPolling();
      poll();
    } catch (e) {
      window.showError(e.message);
      refresh();
    }
  }

  async function forceCheck() {
    els.checkBtn.disabled = true;
    try {
      renderCheck(await window.fetchDataWithAuth('/admin/update/check?force=1', { method: 'POST' }));
      if (lastCheck && !lastCheck.error && lastCheck.latest) {
        window.showInfo(lastCheck.update_available
          ? t('settings.update.toastNew', { to: lastCheck.latest })
          : t('settings.update.toastLatest'));
      }
    } catch (e) {
      window.showError(e.message);
    } finally {
      els.checkBtn.disabled = false;
    }
  }

  document.addEventListener('DOMContentLoaded', () => {
    for (const [k, id] of Object.entries({
      current: 'update-current',
      latest: 'update-latest',
      progressCell: 'update-progress-cell',
      progress: 'update-progress',
      badge: 'update-phase-badge',
      error: 'update-error',
      checkBtn: 'update-check-btn',
      startBtn: 'update-start-btn',
      rollbackBtn: 'update-rollback-btn',
    })) {
      els[k] = document.getElementById(id);
    }
    if (!els.current) return;

    els.checkBtn.addEventListener('click', forceCheck);
    els.startBtn.addEventListener('click', () => startUpdate(lastCheck && lastCheck.update_available ? lastCheck.latest : ''));
    els.rollbackBtn.addEventListener('click', startRollback);

    refresh().then(() => {
      if (inFlight(lastStatus)) startPolling();
    });
    refreshCheck();
  });
})();
