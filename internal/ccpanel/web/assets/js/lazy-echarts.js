// echarts 按需加载：1.1MB 的 echarts.min.js 不再随页面静态加载，
// 首次调用 ensureECharts() 才注入 script，后续调用复用同一 promise。
(function () {
  let echartsPromise = null;

  window.ensureECharts = function ensureECharts() {
    if (window.echarts) return Promise.resolve(window.echarts);
    if (!echartsPromise) {
      echartsPromise = new Promise((resolve, reject) => {
        const script = document.createElement('script');
        script.src = '/web/assets/js/echarts.min.js';
        script.onload = () => resolve(window.echarts);
        script.onerror = () => {
          echartsPromise = null;
          reject(new Error('echarts load failed'));
        };
        document.head.appendChild(script);
      });
    }
    return echartsPromise;
  };
})();
