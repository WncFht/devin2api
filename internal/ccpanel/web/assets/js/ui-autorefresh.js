// ============================================================
// 页面自动刷新（基于 system_settings.auto_refresh_interval_seconds）
// ============================================================
(function () {

  const AUTO_REFRESH_CACHE_KEY = '__autoRefreshIntervalSec';

  const AUTO_REFRESH_CACHE_TTL_MS = 60 * 1000;

  async function fetchAutoRefreshIntervalSec() {
    if (window.isAPITokenRole()) return 0;
    try {
      const cached = window.sessionStorage?.getItem(AUTO_REFRESH_CACHE_KEY);
      if (cached) {
        const parsed = JSON.parse(cached);
        if (parsed && typeof parsed.value === 'number' && Date.now() - parsed.ts < AUTO_REFRESH_CACHE_TTL_MS) {
          return parsed.value;
        }
      }
    } catch (_) { /* 忽略 sessionStorage 异常 */ }

    let seconds = 0;
    try {
      const fetcher = window.fetchDataWithAuth || window.fetchData;
      if (typeof fetcher !== 'function') return 0;
      const data = await fetcher('/admin/settings');
      if (Array.isArray(data)) {
        const item = data.find(s => s && s.key === 'auto_refresh_interval_seconds');
        const n = item ? Number(item.value) : 0;
        if (Number.isFinite(n) && n > 0) seconds = Math.floor(n);
      }
    } catch (_) { /* 拉取失败：不刷新 */ }

    try {
      window.sessionStorage?.setItem(AUTO_REFRESH_CACHE_KEY, JSON.stringify({ value: seconds, ts: Date.now() }));
    } catch (_) { /* 忽略 */ }
    return seconds;
  }

  function createAutoRefresh(options = {}) {
    const load = typeof options.load === 'function' ? options.load : null;
    if (!load) {
      return { init: async () => {}, stop: () => {} };
    }

    let intervalId = null;
    let intervalMs = 0;
    let visibilityHandler = null;

    function shouldSkip() {
      if (typeof document === 'undefined') return true;
      if (document.hidden) return true;
      if (document.querySelector('.modal.show')) return true;
      // options.skip 让页面加自己的暂停条件（如 accounts 页抽屉打开）。
      if (typeof options.skip === 'function' && options.skip()) return true;
      return false;
    }

    function tick() {
      if (shouldSkip()) return;
      try {
        const result = load();
        if (result && typeof result.catch === 'function') {
          result.catch(() => { /* 单次失败不影响后续轮询 */ });
        }
      } catch (_) { /* 同步异常吞掉 */ }
    }

    function startTimer() {
      if (intervalId !== null || intervalMs <= 0) return;
      intervalId = setInterval(tick, intervalMs);
    }

    function stopTimer() {
      if (intervalId !== null) {
        clearInterval(intervalId);
        intervalId = null;
      }
    }

    function onVisibilityChange() {
      if (intervalMs <= 0) return;
      if (document.hidden) {
        stopTimer();
      } else {
        tick();
        startTimer();
      }
    }

    async function init() {
      const seconds = await fetchAutoRefreshIntervalSec();
      if (!seconds || seconds <= 0) return;
      intervalMs = seconds * 1000;
      visibilityHandler = onVisibilityChange;
      document.addEventListener('visibilitychange', visibilityHandler);
      startTimer();
    }

    function stop() {
      stopTimer();
      if (visibilityHandler) {
        document.removeEventListener('visibilitychange', visibilityHandler);
        visibilityHandler = null;
      }
      intervalMs = 0;
    }

    return { init, stop };
  }

  window.createAutoRefresh = createAutoRefresh;

})();
