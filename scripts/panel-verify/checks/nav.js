// checks/nav.js — 断点×语言矩阵下的 topnav 完整性：8 项链接全部存在、
// 没有链接被裁出可达区域（>768px nav 允许 overflow-x:auto 滚动，
// 「可达」= 链接右缘不超过 nav.scrollWidth 覆盖范围；≤768px 换行
// 独占一行必须全部落在 nav 矩形内）、页面无横向溢出。
const { firefox, BASE, adminContext, watchErrors, makeReporter } = require('../lib/harness');

const MATRIX = [
  [1440, 'zh-CN'], [1440, 'en'],
  [1280, 'zh-CN'], [1280, 'en'],
  [1024, 'zh-CN'], [900, 'en'],
  [768, 'zh-CN'], [767, 'zh-CN'],
  [390, 'zh-CN'],
];

(async () => {
  const R = makeReporter('nav');
  const browser = await firefox.launch();
  for (const [w, locale] of MATRIX) {
    const ctx = await adminContext(browser, { viewport: { width: w, height: 900 }, locale });
    const p = await ctx.newPage();
    const errs = [];
    watchErrors(p, errs);
    await p.goto(`${BASE}/web/index.html`, { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('.topnav', { timeout: 8000 }).catch(() => {});
    await p.waitForTimeout(800);
    const r = await p.evaluate(() => {
      const n = document.querySelector('.topnav');
      if (!n) return null;
      const nr = n.getBoundingClientRect();
      // scrollWidth 覆盖滚动区全部内容：链接右缘超出它才是真裁掉
      // （不可达）；visible 模式下 scrollWidth == clientWidth，同一式子
      // 两种模式通用。左缘低于 nav.left 同样不可达（safe center 防线）。
      const reachRight = nr.left + n.scrollWidth + 1;
      return {
        keys: [...n.querySelectorAll('.topnav-link')].map((a) => a.dataset.navKey),
        unreachable: [...n.querySelectorAll('.topnav-link')].filter((a) => {
          const r2 = a.getBoundingClientRect();
          return r2.right > reachRight || r2.left < nr.left - 1;
        }).map((a) => a.dataset.navKey),
        scrollable: n.scrollWidth > n.clientWidth + 1,
        docOverflow: document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
      };
    });
    const tag = `${w}-${locale}`;
    R.check(!!r, `${tag}: topnav present`);
    if (r) {
      R.check(r.keys.length === 8, `${tag}: 8 nav items (got ${r.keys.length})`);
      R.check(r.unreachable.length === 0, `${tag}: all links reachable${r.unreachable.length ? ' — ' + r.unreachable : ''}${r.scrollable ? ' (scrollable)' : ''}`);
      R.check(!r.docOverflow, `${tag}: no document horizontal overflow`);
      await R.shotOnFail(p, tag, r.keys.length === 8 && r.unreachable.length === 0 && !r.docOverflow);
    } else {
      await R.shotOnFail(p, tag, false);
    }
    R.check(errs.length === 0, `${tag}: no console errors${errs.length ? ' — ' + errs.join(' | ') : ''}`);
    await ctx.close();
  }
  await browser.close();
  R.finish();
})().catch((e) => { console.log('ERR', e.message); process.exit(1); });
