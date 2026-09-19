// 保持公共 UI 在独立加载或旧缓存混用时可用；完整实现由 web-auth.js 覆盖。

window.WebAuth = window.WebAuth || {

  ROLE_KEY: 'ccload_web_role',

  clearWebSession(storage) {

    storage.removeItem('ccload_token');

    storage.removeItem('ccload_token_expiry');

    storage.removeItem('ccload_web_role');

  },

  getWebRole() { return 'admin'; },

  isAPITokenRole() { return false; },

  filterNavigation(keys) { return [...keys]; }

};



// ============================================================

// Token认证工具（统一API调用，替代Cookie Session）

// ============================================================

(function () {

  /**

   * 生成带redirect参数的登录页URL

   * @returns {string}

   */

  function getLoginUrl() {

    const currentPath = window.location.pathname + window.location.search;

    // 排除登录页本身

    if (currentPath.includes('/web/login.html')) {

      return '/web/login.html';

    }

    return '/web/login.html?redirect=' + encodeURIComponent(currentPath);

  }



  // 导出到全局作用域

  window.getLoginUrl = getLoginUrl;



  /**

   * 带Token认证的fetch封装

   * @param {string} url - 请求URL

   * @param {Object} options - fetch选项

   * @returns {Promise<Response>}

   */

  async function fetchWithAuth(url, options = {}) {

    const token = localStorage.getItem('ccload_token');

    const expiry = localStorage.getItem('ccload_token_expiry');



    // 检查Token过期（静默跳转，不显示错误提示）

    if (!token || (expiry && Date.now() > parseInt(expiry))) {

      window.WebAuth.clearWebSession(localStorage);

      window.location.href = getLoginUrl();

      throw new Error('Token expired');

    }



    // 合并Authorization头

    const headers = {

      ...options.headers,

      'Authorization': `Bearer ${token}`,

    };



    const response = await fetch(url, { ...options, headers });



    // 处理401未授权（静默跳转，不显示错误提示）

    if (response.status === 401) {

      window.WebAuth.clearWebSession(localStorage);

      window.location.href = getLoginUrl();

      throw new Error('Unauthorized');

    }



    return response;

  }



  // 导出到全局作用域

  window.fetchWithAuth = fetchWithAuth;

  window.getWebRole = () => window.WebAuth.getWebRole(localStorage);

  window.isAPITokenRole = () => window.WebAuth.isAPITokenRole(localStorage);

})();



// ============================================================

// API响应解析（统一后端返回格式：{success,data,error,count}）

// ============================================================

(function () {

  async function parseAPIResponse(res) {

    const text = await res.text();

    if (!text) {

      throw new Error(t('error.emptyResponse') + ` (HTTP ${res.status})`);

    }



    let payload;

    try {

      payload = JSON.parse(text);

    } catch (e) {

      throw new Error(t('error.invalidJson') + ` (HTTP ${res.status})`);

    }



    if (!payload || typeof payload !== 'object' || typeof payload.success !== 'boolean') {

      throw new Error(t('error.invalidFormat') + ` (HTTP ${res.status})`);

    }



    return payload;

  }



  async function fetchAPI(url, options = {}) {

    const res = await fetch(url, options);

    return parseAPIResponse(res);

  }



  async function fetchAPIWithAuth(url, options = {}) {

    const res = await fetchWithAuth(url, options);

    return parseAPIResponse(res);

  }



  // 需要同时读取响应头（如 X-Debug-*）的场景：返回 { res, payload }

  async function fetchAPIWithAuthRaw(url, options = {}) {

    const res = await fetchWithAuth(url, options);

    const payload = await parseAPIResponse(res);

    return { res, payload };

  }



  async function fetchData(url, options = {}) {

    const resp = await fetchAPI(url, options);

    if (!resp.success) throw new Error(resp.error || t('error.requestFailed'));

    return resp.data;

  }



  async function fetchDataWithAuth(url, options = {}) {

    const resp = await fetchAPIWithAuth(url, options);

    if (!resp.success) throw new Error(resp.error || t('error.requestFailed'));

    return resp.data;

  }



  window.fetchAPI = fetchAPI;

  window.parseAPIResponse = parseAPIResponse;

  window.fetchAPIWithAuth = fetchAPIWithAuth;

  window.fetchAPIWithAuthRaw = fetchAPIWithAuthRaw;

  window.fetchData = fetchData;

  window.fetchDataWithAuth = fetchDataWithAuth;

})();
