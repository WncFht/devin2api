// 防闪烁预显：同步脚本在视图节点解析前执行，按持久化的视图偏好提前把
// 根类与按钮激活态落好；stats.js 接管后 switchView 移除根类并改走内联 display。
(function () {
  try {
    if (localStorage.getItem('stats.view') !== 'chart') return;
    document.documentElement.classList.add('stats-view-init-chart');
    document.querySelectorAll('#view-toggle-group .seg-btn').forEach(function (btn) {
      const active = btn.dataset.view === 'chart';
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-pressed', active ? 'true' : 'false');
    });
  } catch (_) {}
})();
