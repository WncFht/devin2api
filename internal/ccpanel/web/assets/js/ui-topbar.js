// ============================================================

// 共享UI：顶部导航与背景动画（KISS/DRY）

// 使用方式：在页面底部引入本文件，并调用 initTopbar('index'|'configs'|'stats'|'trend'|'errors')

// ============================================================

(function () {

  const NAVS = [

    { key: 'index', labelKey: 'nav.overview', href: '/web/index.html', icon: iconHome },

    { key: 'tokens', labelKey: 'nav.tokens', href: '/web/tokens.html', icon: iconKey },

    { key: 'models', labelKey: 'nav.models', href: '/web/models.html', icon: iconLayers },

    { key: 'stats', labelKey: 'nav.stats', href: '/web/stats.html', icon: iconBars },

    { key: 'trend', labelKey: 'nav.trend', href: '/web/trend.html', icon: iconTrend },

    { key: 'accounts', labelKey: 'nav.accounts', href: '/web/accounts.html', icon: iconUsers },

    { key: 'logs', labelKey: 'nav.logs', href: '/web/logs.html', icon: iconAlert },

    { key: 'settings', labelKey: 'nav.settings', href: '/web/settings.html', icon: iconSettings },

  ];

  const THEME_STORAGE_KEY = 'ccload_theme';

  const THEME_MODES = ['system', 'light', 'dark'];

  let systemThemeQuery = null;

  let currentThemeMode = 'system';



  function h(tag, attrs = {}, children = []) {

    const el = document.createElement(tag);

    Object.entries(attrs).forEach(([k, v]) => {

      if (k === 'class') el.className = v;

      else if (k === 'style') el.style.cssText = v;

      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);

      else el.setAttribute(k, v);

    });

    (Array.isArray(children) ? children : [children]).forEach((c) => {

      if (c == null) return;

      if (typeof c === 'string') el.appendChild(document.createTextNode(c));

      else el.appendChild(c);

    });

    return el;

  }



  function iconHome() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 7v10a2 2 0 002 2h14a2 2 0 002-2V9a2 2 0 00-2-2H5a2 2 0 00-2-2z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 5a2 2 0 012-2h4a2 2 0 012 2v0a2 2 0 01-2 2H10a2 2 0 01-2-2v0z"/>`);

  }

  function iconSettings() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M10.325 4.317c.426-1.756 2.924-1.756 3.35 0a1.724 1.724 0 002.573 1.066c1.543-.94 3.31.826 2.37 2.37a1.724 1.724 0 001.065 2.572c1.756.426 1.756 2.924 0 3.35a1.724 1.724 0 00-1.066 2.573c.94 1.543-.826 3.31-2.37 2.37a1.724 1.724 0 00-2.572 1.065c-.426 1.756-2.924 1.756-3.35 0a1.724 1.724 0 00-2.573-1.066c-1.543.94-3.31-.826-2.37-2.37a1.724 1.724 0 00-1.065-2.572c-1.756-.426-1.756-2.924 0-3.35a1.724 1.724 0 001.066-2.573c-.94-1.543.826-3.31 2.37-2.37.996.608 2.296.07 2.572-1.065z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 12a3 3 0 11-6 0 3 3 0 016 0z"/>`);

  }

  function iconBars() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z"/>`);

  }

  function iconTrend() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M7 12l3-3 3 3 4-4"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M8 21l4-4 4 4"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 4h18"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 4h16v12a1 1 0 01-1 1H5a1 1 0 01-1-1V4z"/>`);

  }

  function iconAlert() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.864-.833-2.634 0L4.18 16.5c-.77.833.192 2.5 1.732 2.5z"/>`);

  }

  function iconKey() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z"/>`);

  }

  function iconTest() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z"/>`);

  }

  function iconLayers() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 2l9 4.5-9 4.5-9-4.5L12 2z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 11.5l9 4.5 9-4.5"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3 16.5l9 4.5 9-4.5"/>`);

  }

  function iconUsers() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M17 20h5v-2a3 3 0 00-5.356-1.857M17 20H7m10 0v-2c0-.656-.126-1.283-.356-1.857M7 20H2v-2a3 3 0 015.356-1.857M7 20v-2c0-.656.126-1.283.356-1.857m0 0a5.002 5.002 0 019.288 0M15 7a3 3 0 11-6 0 3 3 0 016 0zm6 3a2 2 0 11-4 0 2 2 0 014 0zM7 10a2 2 0 11-4 0 2 2 0 014 0z"/>`);

  }

  function iconThemeSystem() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3a9 9 0 100 18 9 9 0 000-18z"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M3.6 9h16.8M3.6 15h16.8"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3c2 2.2 3 5.2 3 9s-1 6.8-3 9c-2-2.2-3-5.2-3-9s1-6.8 3-9z"/>`);

  }

  function iconThemeLight() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 3v2m0 14v2m9-9h-2M5 12H3m15.364-6.364l-1.414 1.414M7.05 16.95l-1.414 1.414m12.728 0l-1.414-1.414M7.05 7.05L5.636 5.636"/><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M16 12a4 4 0 11-8 0 4 4 0 018 0z"/>`);

  }

  function iconThemeDark() {

    return svg(`<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M20.354 15.354A9 9 0 118.646 3.646 7 7 0 0020.354 15.354z"/>`);

  }

  function svg(inner) {

    const el = document.createElementNS('http://www.w3.org/2000/svg', 'svg');

    el.setAttribute('fill', 'none');

    el.setAttribute('stroke', 'currentColor');

    el.setAttribute('viewBox', '0 0 24 24');

    el.classList.add('w-5', 'h-5');

    el.innerHTML = inner;

    return el;

  }



  function getStoredTheme() {

    if (window.ccLoadTheme && typeof window.ccLoadTheme.getStoredTheme === 'function') {

      return window.ccLoadTheme.getStoredTheme();

    }

    try {

      const saved = localStorage.getItem(THEME_STORAGE_KEY);

      const mode = typeof saved === 'string' ? saved.split(':', 1)[0] : null;

      return THEME_MODES.includes(mode) ? mode : 'system';

    } catch (_) {

      return 'system';

    }

  }



  function resolveTheme(mode) {

    if (mode !== 'system') return mode;

    return systemThemeQuery && systemThemeQuery.matches ? 'dark' : 'light';

  }



  function setThemeMetaColor(resolvedTheme) {

    const meta = document.querySelector('meta[name="theme-color"]');

    if (meta) meta.setAttribute('content', resolvedTheme === 'dark' ? '#0f172a' : '#3b82f6');

  }



  function getThemeCssVar(style, name, fallback) {

    const value = style ? style.getPropertyValue(name).trim() : '';

    return value || fallback;

  }



  function getChartTheme() {

    const root = document.documentElement;

    const style = window.getComputedStyle ? getComputedStyle(root) : null;

    const resolvedTheme = root.dataset.resolvedTheme || root.dataset.theme || 'light';

    const isDark = resolvedTheme === 'dark';



    return {

      text: getThemeCssVar(style, '--neutral-700', isDark ? '#e5e7eb' : '#374151'),

      mutedText: getThemeCssVar(style, '--neutral-500', isDark ? '#9ca3af' : '#6b7280'),

      strongText: getThemeCssVar(style, '--neutral-900', isDark ? '#f9fafb' : '#111827'),

      axisLine: getThemeCssVar(style, '--surface-border-strong', isDark ? 'rgba(255, 255, 255, 0.18)' : 'rgba(17, 24, 39, 0.16)'),

      splitLine: getThemeCssVar(style, '--surface-border', isDark ? 'rgba(255, 255, 255, 0.12)' : 'rgba(17, 24, 39, 0.10)'),

      surface: getThemeCssVar(style, '--surface-bg-strong', isDark ? 'rgba(17, 24, 39, 0.94)' : 'rgba(255, 255, 255, 0.98)'),

      surfaceMuted: getThemeCssVar(style, '--surface-bg-muted', isDark ? 'rgba(31, 41, 55, 0.78)' : 'rgba(243, 244, 246, 0.90)'),

      tooltipBg: getThemeCssVar(style, '--surface-bg-strong', isDark ? 'rgba(17, 24, 39, 0.94)' : 'rgba(255, 255, 255, 0.98)'),

      tooltipBorder: getThemeCssVar(style, '--surface-border-strong', isDark ? 'rgba(255, 255, 255, 0.18)' : 'rgba(17, 24, 39, 0.16)'),

      tooltipText: getThemeCssVar(style, '--neutral-900', isDark ? '#f9fafb' : '#111827')

    };

  }



  function refreshThemeSwitcher(root = document) {

    const trigger = root.querySelector ? root.querySelector('.theme-trigger') : null;

    if (trigger) {

      trigger.replaceChildren(getThemeIcon(currentThemeMode));

    }

    root.querySelectorAll && root.querySelectorAll('.theme-option').forEach((option) => {

      const active = option.getAttribute('data-theme-mode') === currentThemeMode;

      option.setAttribute('aria-pressed', active ? 'true' : 'false');

      option.classList.toggle('active', active);

    });

  }



  function applyThemeMode(mode) {

    currentThemeMode = THEME_MODES.includes(mode) ? mode : 'system';

    const resolvedTheme = resolveTheme(currentThemeMode);

    document.documentElement.dataset.theme = currentThemeMode;

    document.documentElement.dataset.resolvedTheme = resolvedTheme;

    document.documentElement.style.colorScheme = resolvedTheme;

    setThemeMetaColor(resolvedTheme);

    refreshThemeSwitcher();

    window.dispatchEvent(new CustomEvent('ccload:themechange', {

      detail: { mode: currentThemeMode, resolvedTheme }

    }));

  }



  function applyStoredTheme() {

    applyThemeMode(getStoredTheme());

  }



  function setThemeMode(mode) {

    if (!THEME_MODES.includes(mode)) return;

    if (window.ccLoadTheme && typeof window.ccLoadTheme.setStoredTheme === 'function') {

      window.ccLoadTheme.setStoredTheme(mode);

    } else {

      try {

        localStorage.setItem(THEME_STORAGE_KEY, `${mode}:${Date.now()}`);

      } catch (_) { /* 存储失败时只应用当前页面 */ }

    }

    applyThemeMode(mode);

  }



  function initTheme() {

    systemThemeQuery = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

    applyStoredTheme();

    if (systemThemeQuery && !systemThemeQuery._ccloadThemeBound) {

      systemThemeQuery.addEventListener('change', applyStoredTheme);

      systemThemeQuery._ccloadThemeBound = true;

    }

  }



  function setThemeSwitcherOpen(switcher, open) {

    if (!switcher) return;

    if (open) switcher.classList.add('open');

    else switcher.classList.remove('open');

    const trigger = switcher.querySelector('.theme-trigger');

    if (trigger) trigger.setAttribute('aria-expanded', open ? 'true' : 'false');

  }



  function closeThemeSwitchers(except = null) {

    document.querySelectorAll('.theme-switcher.open').forEach((switcher) => {

      if (switcher !== except) setThemeSwitcherOpen(switcher, false);

    });

  }



  initTheme();

  document.addEventListener('click', () => closeThemeSwitchers());



  function isLoggedIn() {

    const token = localStorage.getItem('ccload_token');

    const expiry = localStorage.getItem('ccload_token_expiry');

    return token && (!expiry || Date.now() <= parseInt(expiry));

  }



  // GitHub仓库地址

  const GITHUB_REPO_URL = 'https://github.com/WncFht/devin2api';

  const GITHUB_RELEASES_URL = 'https://github.com/WncFht/devin2api/releases';



  // 版本信息

  let versionInfo = null;



  // 获取版本信息（后端已包含新版本检测结果）

  async function fetchVersionInfo() {

    try {

      const res = await fetch('/public/version');

      const resp = await res.json();

      versionInfo = resp.data;

      return versionInfo;

    } catch (e) {

      console.error('Failed to fetch version info:', e);

      return null;

    }

  }



  // 更新版本显示

  function updateVersionDisplay() {

    const versionEl = document.getElementById('version-display');

    const badgeEl = document.getElementById('version-badge');

    if (!versionInfo) return;



    if (versionEl) {

      versionEl.textContent = versionInfo.version;

    }

    if (badgeEl) {

      if (versionInfo.has_update && versionInfo.latest_version) {

        badgeEl.title = t('version.hasUpdate', { version: versionInfo.latest_version });

        badgeEl.classList.add('has-update');

      } else {

        badgeEl.title = t('version.checkUpdate');

        badgeEl.classList.remove('has-update');

      }

    }

  }



  // 初始化版本显示

  function initVersionDisplay() {

    fetchVersionInfo().then(() => updateVersionDisplay());

  }



  // GitHub图标

  function iconGitHub() {

    const el = document.createElementNS('http://www.w3.org/2000/svg', 'svg');

    el.setAttribute('fill', 'currentColor');

    el.setAttribute('viewBox', '0 0 24 24');

    el.classList.add('w-5', 'h-5');

    el.innerHTML = '<path d="M12 0C5.374 0 0 5.373 0 12c0 5.302 3.438 9.8 8.207 11.387.599.111.793-.261.793-.577v-2.234c-3.338.726-4.033-1.416-4.033-1.416-.546-1.387-1.333-1.756-1.333-1.756-1.089-.745.083-.729.083-.729 1.205.084 1.839 1.237 1.839 1.237 1.07 1.834 2.807 1.304 3.492.997.107-.775.418-1.305.762-1.604-2.665-.305-5.467-1.334-5.467-5.931 0-1.311.469-2.381 1.236-3.221-.124-.303-.535-1.524.117-3.176 0 0 1.008-.322 3.301 1.23A11.509 11.509 0 0112 5.803c1.02.005 2.047.138 3.006.404 2.291-1.552 3.297-1.23 3.297-1.23.653 1.653.242 2.874.118 3.176.77.84 1.235 1.911 1.235 3.221 0 4.609-2.807 5.624-5.479 5.921.43.372.823 1.102.823 2.222v3.293c0 .319.192.694.801.576C20.566 21.797 24 17.3 24 12c0-6.627-5.373-12-12-12z"/>';

    return el;

  }





  // ---- 活动请求指示器（favicon 角标 + 标题闪烁）----
  // 全站唯一轮询源：拉取完整 payload 后自己消费 count，同时推送 data 给订阅者（如 logs.js）

  const ACTIVE_POLL_MS = 2000;
  const ACTIVE_TITLE_FLASH_MS = 900;

  let activeTimer = null;
  let faviconBaseImg = null;
  let origFavicons = null;
  let lastBadgeCount = -1;
  let faviconPulseOn = false;
  let activeCount = 0;
  const activeDataListeners = [];
  let lastActiveData = null;
  let titleBase = '';
  let titleFlashTimer = null;
  let titleFlashVisible = false;
  let titleFlashEnabled = false;

  function snapshotFavicons() {
    if (origFavicons) return;
    origFavicons = Array.from(document.querySelectorAll('link[rel~="icon"]'))
      .filter((link) => link.dataset.dynamicFavicon !== '1')
      .map((link) => ({
        rel: link.getAttribute('rel') || 'icon',
        href: link.getAttribute('href') || '',
        type: link.getAttribute('type') || '',
        sizes: link.getAttribute('sizes') || ''
      }));
    if (!origFavicons.length) {
      origFavicons = [{ rel: 'icon', href: '/web/favicon.svg', type: 'image/svg+xml', sizes: '' }];
    }
  }

  function applyFavicons(descriptors, dynamic) {
    document.querySelectorAll('link[rel~="icon"]').forEach((link) => link.remove());
    descriptors.forEach((d) => {
      const link = document.createElement('link');
      link.rel = d.rel;
      if (d.type) link.type = d.type;
      if (d.sizes) link.sizes = d.sizes;
      link.href = d.href;
      if (dynamic) link.dataset.dynamicFavicon = '1';
      document.head.appendChild(link);
    });
  }

  // 底图右上画呼吸色点 + 计数角标（1~9 单字符 / 9+ 双字符两档字号）
  function drawFaviconBadge(count, pulse) {
    if (!faviconBaseImg) return;
    const canvas = document.createElement('canvas');
    canvas.width = 64;
    canvas.height = 64;
    const ctx = canvas.getContext('2d');
    if (!ctx) return;
    ctx.drawImage(faviconBaseImg, 0, 0, 64, 64);
    const cx = 50, cy = 14, r = pulse ? 13 : 11;
    ctx.beginPath();
    ctx.arc(cx, cy, r + 5, 0, Math.PI * 2);
    ctx.fillStyle = pulse ? 'rgba(249,115,22,0.28)' : 'rgba(249,115,22,0.12)';
    ctx.fill();
    ctx.beginPath();
    ctx.arc(cx, cy, r + 2, 0, Math.PI * 2);
    ctx.fillStyle = '#ffffff';
    ctx.fill();
    ctx.beginPath();
    ctx.arc(cx, cy, r, 0, Math.PI * 2);
    ctx.fillStyle = pulse ? '#fb923c' : '#f97316';
    ctx.fill();
    const label = count > 9 ? '9+' : String(count);
    ctx.fillStyle = '#ffffff';
    ctx.font = `bold ${label.length >= 2 ? 14 : 18}px ui-sans-serif, system-ui, sans-serif`;
    ctx.textAlign = 'center';
    ctx.textBaseline = 'middle';
    ctx.fillText(label, cx, cy + 1);
    try {
      applyFavicons([
        { rel: 'shortcut icon', href: canvas.toDataURL('image/png'), type: 'image/png', sizes: '' },
        { rel: 'icon', href: canvas.toDataURL('image/png'), type: 'image/png', sizes: '' }
      ], true);
    } catch (_) { /* 编码失败：保持原 favicon */ }
  }

  function refreshActiveFavicon() {
    if (activeCount <= 0) {
      applyFavicons(origFavicons);
      return;
    }
    if (!faviconBaseImg) {
      const img = new Image();
      img.onload = () => { faviconBaseImg = img; drawFaviconBadge(activeCount, faviconPulseOn); };
      img.src = '/web/favicon.svg';
      return;
    }
    drawFaviconBadge(activeCount, faviconPulseOn);
  }

  function activeTitleLabel() {
    const label = activeCount > 999 ? '999+' : String(activeCount);
    if (typeof t === 'function') {
      const translated = t('nav.activeRequestsTitle', { count: label });
      if (translated && translated !== 'nav.activeRequestsTitle') return translated;
    }
    return `请求中[${label}]-`;
  }

  function stopTitleFlash() {
    if (titleFlashTimer !== null) {
      clearInterval(titleFlashTimer);
      titleFlashTimer = null;
    }
    titleFlashVisible = false;
    if (titleBase) document.title = titleBase;
  }

  function updateActiveIndicator(count, titleEnabled) {
    if (titleFlashTimer === null) {
      titleBase = document.title || titleBase || '';
    }
    activeCount = count;
    titleFlashEnabled = titleEnabled === true;

    // favicon 角标仅在数量变化时重绘，省 toDataURL 开销
    if (count !== lastBadgeCount) {
      lastBadgeCount = count;
      snapshotFavicons();
      faviconPulseOn = false;
      refreshActiveFavicon();
    }

    if (count <= 0) {
      stopTitleFlash();
      return;
    }
    if (titleFlashTimer === null) {
      if (titleFlashEnabled) {
        document.title = activeTitleLabel() + titleBase;
        titleFlashVisible = true;
      }
      titleFlashTimer = setInterval(() => {
        faviconPulseOn = !faviconPulseOn;
        if (faviconBaseImg) drawFaviconBadge(activeCount, faviconPulseOn);
        if (!titleFlashEnabled) return;
        document.title = titleFlashVisible ? titleBase : activeTitleLabel() + titleBase;
        titleFlashVisible = !titleFlashVisible;
      }, ACTIVE_TITLE_FLASH_MS);
      return;
    }
    if (!titleFlashEnabled && titleFlashVisible) {
      document.title = titleBase;
      titleFlashVisible = false;
    }
    if (titleFlashEnabled && titleFlashVisible) {
      document.title = activeTitleLabel() + titleBase;
    }
  }

  async function pollActiveRequests() {
    try {
      const payload = await fetchAPIWithAuth('/admin/active-requests');
      const count = typeof payload.count === 'number' ? payload.count : 0;
      updateActiveIndicator(count, payload.active_request_title_enabled === true);
      const data = (payload.success && Array.isArray(payload.data)) ? payload.data : [];
      lastActiveData = data;
      for (const cb of activeDataListeners) {
        try { cb(data, count); } catch (_) { /* 订阅者异常不影响主逻辑 */ }
      }
    } catch (_) { /* 静默：未登录或网络异常不打断页面 */ }
  }

  function startActiveRequestsPolling() {
    if (activeTimer) return;
    pollActiveRequests();
    activeTimer = setInterval(() => {
      if (!document.hidden) pollActiveRequests();
    }, ACTIVE_POLL_MS);
    document.addEventListener('visibilitychange', onActiveVisibilityChange);
  }

  function stopActiveRequestsPolling() {
    if (activeTimer) {
      clearInterval(activeTimer);
      activeTimer = null;
    }
  }

  function onActiveVisibilityChange() {
    if (document.hidden) stopActiveRequestsPolling();
    else startActiveRequestsPolling();
  }

  // 供其他页面模块（如 logs.js）订阅活动请求数据，避免重复轮询；
  // 已有最近数据则立即回调，规避新订阅者干等一个周期的时序竞争
  function onActiveRequestsData(callback) {
    if (typeof callback !== 'function') return;
    activeDataListeners.push(callback);
    if (lastActiveData !== null) {
      try { callback(lastActiveData); } catch (_) { /* 订阅者异常不影响主逻辑 */ }
    }
  }

  function buildTopbar(active) {

    const bar = h('header', { class: 'topbar' });



    const role = window.getWebRole();

    const visibleNavKeys = new Set(window.WebAuth.filterNavigation(NAVS.map((item) => item.key), role));

    const nav = h('nav', { class: 'topnav' }, [

      ...NAVS.filter((item) => visibleNavKeys.has(item.key)).map(n => h('a', {

        class: `topnav-link ${n.key === active ? 'active' : ''}`,

        href: n.href,

        'data-nav-key': n.key

      }, [n.icon(), h('span', { 'data-i18n': n.labelKey }, t(n.labelKey))]))

    ]);

    const loggedIn = isLoggedIn();



    // 版本信息组件（点击跳转到GitHub releases页面）

    const versionBadge = h('a', {

      id: 'version-badge',

      class: 'version-badge',

      href: GITHUB_RELEASES_URL,

      target: '_blank',

      rel: 'noopener noreferrer',

      title: t('version.checkUpdate')

    }, [

      h('span', { id: 'version-display' }, 'v...')

    ]);



    // GitHub链接

    const githubLink = h('a', {

      href: GITHUB_REPO_URL,

      target: '_blank',

      rel: 'noopener noreferrer',

      class: 'github-link',

      title: t('nav.githubRepo')

    }, [iconGitHub()]);



    // 版本+GitHub组合成一个视觉组

    const versionGroup = h('div', { class: 'version-group' }, [versionBadge, githubLink]);



    // 语言切换器

    const langSwitcher = window.i18n ? window.i18n.createLanguageSwitcher() : null;

    const themeSwitcher = buildThemeSwitcher();



    const right = h('div', { class: 'topbar-right' }, [

      versionGroup,

      themeSwitcher,

      langSwitcher,

      h('button', {

        id: 'auth-btn',

        class: 'btn btn-secondary btn-sm',

        'data-i18n': loggedIn ? 'common.logout' : 'common.login',

        onclick: loggedIn ? onLogout : () => location.href = window.getLoginUrl()

      }, t(loggedIn ? 'common.logout' : 'common.login'))

    ].filter(Boolean));

    bar.appendChild(nav); bar.appendChild(right);

    return bar;

  }



  function buildThemeSwitcher() {

    const modes = [

      { mode: 'system', labelKey: 'theme.system', icon: iconThemeSystem },

      { mode: 'light', labelKey: 'theme.light', icon: iconThemeLight },

      { mode: 'dark', labelKey: 'theme.dark', icon: iconThemeDark },

    ];

    const trigger = h('button', {

      type: 'button',

      class: 'theme-trigger',

      title: t('theme.label'),

      'aria-label': t('theme.label'),

      'aria-expanded': 'false',

      'aria-haspopup': 'menu',

      onclick: (event) => {

        event.stopPropagation();

        const switcher = event.currentTarget.closest('.theme-switcher');

        const nextOpen = !switcher.classList.contains('open');

        closeThemeSwitchers(switcher);

        setThemeSwitcherOpen(switcher, nextOpen);

        if (nextOpen) focusThemeOption(switcher);

      },

      onkeydown: (event) => {

        if (event.key !== 'ArrowDown' && event.key !== 'Enter') return;

        const switcher = event.currentTarget.closest('.theme-switcher');

        if (switcher.classList.contains('open')) return;

        event.preventDefault();

        closeThemeSwitchers(switcher);

        setThemeSwitcherOpen(switcher, true);

        focusThemeOption(switcher);

      }

    }, [getThemeIcon(currentThemeMode)]);

    const menu = h('div', {

      class: 'theme-menu',

      role: 'menu',

      onclick: (event) => event.stopPropagation()

    }, modes.map(({ mode, labelKey, icon }) => h('button', {

      type: 'button',

      class: 'theme-option',

      role: 'menuitemradio',

      'data-theme-mode': mode,

      'aria-pressed': mode === currentThemeMode ? 'true' : 'false',

      onclick: (event) => {

        setThemeMode(mode);

        setThemeSwitcherOpen(event.currentTarget.closest('.theme-switcher'), false);

        trigger.focus();

      }

    }, [icon(), h('span', { 'data-i18n': labelKey }, t(labelKey))])));

    // 菜单键盘导航：方向键循环、Esc 关并回焦 trigger、Tab 关

    menu.addEventListener('keydown', (event) => {

      const options = Array.from(menu.querySelectorAll('.theme-option'));

      const idx = options.indexOf(document.activeElement);

      if (event.key === 'Escape') {

        event.preventDefault();

        setThemeSwitcherOpen(switcher, false);

        trigger.focus();

        return;

      }

      if (event.key === 'Tab') {

        setThemeSwitcherOpen(switcher, false);

        return;

      }

      let next = -1;

      if (event.key === 'ArrowDown') next = idx < 0 ? 0 : (idx + 1) % options.length;

      else if (event.key === 'ArrowUp') next = idx <= 0 ? options.length - 1 : idx - 1;

      else if (event.key === 'Home') next = 0;

      else if (event.key === 'End') next = options.length - 1;

      if (next >= 0) {

        event.preventDefault();

        options[next] && options[next].focus();

      }

    });

    const switcher = h('div', { class: 'theme-switcher' }, [trigger, menu]);

    refreshThemeSwitcher(switcher);

    return switcher;

  }

  function focusThemeOption(switcher) {

    const menu = switcher && switcher.querySelector('.theme-menu');

    if (!menu) return;

    const target = menu.querySelector('.theme-option[aria-pressed="true"]') || menu.querySelector('.theme-option');

    if (target) target.focus();

  }



  function getThemeIcon(mode) {

    if (mode === 'light') return iconThemeLight();

    if (mode === 'dark') return iconThemeDark();

    return iconThemeSystem();

  }



  async function onLogout() {

    if (!(await window.Modal.confirm(t('confirm.logout')))) return;



    // 先清理本地Token，避免后续请求触发token检查

    const token = localStorage.getItem('ccload_token');

    window.WebAuth.clearWebSession(localStorage);



    // 如果有token，尝试调用后端登出接口（使用普通fetch，不触发token检查）

    if (token) {

      try {

        await fetch('/logout', {

          method: 'POST',

          headers: { 'Authorization': `Bearer ${token}` }

        });

      } catch (error) {

        console.error('Logout error:', error);

      }

    }



    // 跳转到登录页

    location.href = '/web/login.html';

  }



  window.initTopbar = function initTopbar(activeKey) {

    document.body.classList.add('top-layout');

    document.body.classList.toggle('web-role-api-token', window.isAPITokenRole());

    const app = document.querySelector('.app-container') || document.body;

    // 隐藏侧边栏与移动按钮

    const sidebar = document.getElementById('sidebar');

    if (sidebar) sidebar.style.display = 'none';

    const mobileBtn = document.getElementById('mobile-menu-btn');

    if (mobileBtn) mobileBtn.style.display = 'none';



    // 插入顶部条

    const topbar = buildTopbar(activeKey);

    document.body.appendChild(topbar);

    const skipLink = document.createElement('a');
    skipLink.className = 'skip-link';
    skipLink.href = '#main';
    skipLink.textContent = t('common.skipToContent');
    document.body.prepend(skipLink);



    // 初始化版本显示

    initVersionDisplay();



    // 启动活动请求指示器轮询

    if (isLoggedIn() && !window.isAPITokenRole()) startActiveRequestsPolling();

  }



  // 供其他模块订阅活动请求数据（全站唯一轮询源，避免重复请求）

  window.onActiveRequestsData = onActiveRequestsData;

  window.getChartTheme = getChartTheme;



})();
