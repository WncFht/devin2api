// checks/columns.js — logs 页列显隐菜单：开合、隐藏列生效、菜单
// 保持打开、外部点击关闭、刷新后 localStorage 持久化；外加死窗回归——
// /dashboard/session 被拖慢期间按钮已绑定（委托监听在模块顶层挂，
// 不等 bootstrap），点击应立即开菜单。
const { firefox, BASE, adminContext, watchErrors, makeReporter } = require('../lib/harness');

(async () => {
  const R = makeReporter('columns');
  const browser = await firefox.launch();
  const ctx = await adminContext(browser, { viewport: { width: 1440, height: 900 } });
  const p = await ctx.newPage();
  const errs = [];
  watchErrors(p, errs);
  await p.goto(`${BASE}/web/logs.html`, { waitUntil: 'domcontentloaded' });
  await p.waitForSelector('#btn_col_settings', { timeout: 10000 });
  await p.waitForTimeout(1500);

  // 开菜单 → 切 ip 列 → 校验隐藏生效且菜单仍开
  await p.click('#btn_col_settings');
  await p.waitForTimeout(300);
  let st = await p.evaluate(() => ({
    open: !document.getElementById('colToggleMenu').hidden,
    items: document.querySelectorAll('.logs-col-toggle-item').length,
  }));
  R.check(st.open, 'menu opens on click');
  R.check(st.items > 0, `menu has column items (got ${st.items})`);

  await p.click('.logs-col-toggle-item[data-col-key="ip"]');
  await p.waitForTimeout(300);
  st = await p.evaluate(() => {
    const ths = [...document.querySelectorAll('.logs-table th.logs-col-ip')];
    return {
      menuOpen: !document.getElementById('colToggleMenu').hidden,
      thHidden: ths.length > 0 && ths.every((e) => getComputedStyle(e).display === 'none'),
      saved: (localStorage.getItem('ccload_logs_columns') || '').includes('"ip"'),
    };
  });
  R.check(st.thHidden, 'ip column hidden after toggle');
  R.check(st.menuOpen, 'menu stays open after toggle');
  R.check(st.saved, 'visibility persisted to localStorage');

  // 外部点击关闭
  await p.mouse.click(700, 600);
  await p.waitForTimeout(300);
  st = await p.evaluate(() => ({ open: !document.getElementById('colToggleMenu').hidden }));
  R.check(!st.open, 'outside click closes menu');

  // 刷新持久化
  await p.reload({ waitUntil: 'domcontentloaded' });
  await p.waitForSelector('.logs-table', { timeout: 10000 }).catch(() => {});
  await p.waitForTimeout(1200);
  const thHiddenAfter = await p.evaluate(() => {
    const ths = [...document.querySelectorAll('.logs-table th.logs-col-ip')];
    return ths.length > 0 && ths.every((e) => getComputedStyle(e).display === 'none');
  });
  R.check(thHiddenAfter, 'ip column still hidden after reload');
  await R.shotOnFail(p, 'toggle', thHiddenAfter);
  await ctx.close();

  // 死窗回归：拖慢 /dashboard/session，窗内按钮必须可用
  const ctx2 = await adminContext(browser, { viewport: { width: 1440, height: 900 } });
  const p2 = await ctx2.newPage();
  await p2.route('**/dashboard/session**', async (route) => {
    await new Promise((r) => setTimeout(r, 3000));
    route.continue();
  });
  await p2.goto(`${BASE}/web/logs.html`, { waitUntil: 'domcontentloaded' });
  await p2.waitForSelector('#btn_col_settings', { state: 'attached', timeout: 8000 });
  const inWindow = await p2.evaluate(() => document.body.dataset.logsPageActionsBound === '1');
  R.check(inWindow, 'delegated actions bound before session resolves');
  await p2.click('#btn_col_settings', { timeout: 3000 });
  const openInWindow = await p2.evaluate(() => !document.getElementById('colToggleMenu').hidden);
  R.check(openInWindow, 'menu opens during dead window');
  await R.shotOnFail(p2, 'deadwindow', openInWindow);
  await ctx2.close();
  await browser.close();
  R.finish();
})().catch((e) => { console.log('ERR', e.message); process.exit(1); });
