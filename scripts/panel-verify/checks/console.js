// checks/console.js — 零 console 错误巡检：admin 身份逐页打开全部
// 8 个面板页，收集 console error / pageerror / 4xx+ 响应；任何一页
// 有错误即失败。每页独立 context 隔离 localStorage 副作用。
const { firefox, BASE, PAGES, adminContext, watchErrors, makeReporter } = require('../lib/harness');

(async () => {
  const R = makeReporter('console');
  const browser = await firefox.launch();
  const ctx = await adminContext(browser, { viewport: { width: 1440, height: 900 } });
  for (const name of PAGES) {
    const p = await ctx.newPage();
    const errs = [];
    watchErrors(p, errs);
    await p.goto(`${BASE}/web/${name}.html`, { waitUntil: 'networkidle', timeout: 20000 })
      .catch((e) => errs.push('goto: ' + e.message.slice(0, 120)));
    await p.waitForTimeout(1500);
    const topbar = await p.$('header.topbar') !== null;
    R.check(topbar, `${name}: topbar rendered`);
    R.check(errs.length === 0, `${name}: clean${errs.length ? ' — ' + errs.slice(0, 3).join(' | ') : ''}`);
    await R.shotOnFail(p, name, topbar && errs.length === 0);
    await p.close();
  }
  await browser.close();
  R.finish();
})().catch((e) => { console.log('ERR', e.message); process.exit(1); });
