// checks/mobile.js — 移动端 logs 页操作区：无横向溢出、列显隐按钮
// 完整落在视口内（不被右缘裁切）、可点开菜单。覆盖 375/320 两个宽度。
const { firefox, BASE, adminContext, watchErrors, makeReporter } = require('../lib/harness');

(async () => {
  const R = makeReporter('mobile');
  const browser = await firefox.launch();
  for (const w of [375, 320]) {
    const ctx = await adminContext(browser, {
      viewport: { width: w, height: 800 },
      isMobile: true,
      hasTouch: true,
    });
    const p = await ctx.newPage();
    const errs = [];
    watchErrors(p, errs);
    await p.goto(`${BASE}/web/logs.html`, { waitUntil: 'domcontentloaded' });
    await p.waitForSelector('#btn_col_settings', { state: 'attached', timeout: 10000 });
    await p.waitForTimeout(2000);
    const r = await p.evaluate(() => {
      const btn = document.getElementById('btn_col_settings');
      const doc = document.documentElement;
      const br = btn.getBoundingClientRect();
      // 找 filter 区内溢出视口的元素（被祖先 overflow 裁掉的不算，
      // 那属于受控滚动容器而非布局破洞）
      let worst = null;
      for (const el of document.querySelectorAll('.logs-filter-summary-row *')) {
        const er = el.getBoundingClientRect();
        if (!er.width) continue;
        if (er.right > doc.clientWidth + 1) {
          let clipped = false;
          for (let a = el.parentElement; a; a = a.parentElement) {
            if (['auto', 'scroll', 'hidden'].includes(getComputedStyle(a).overflowX)) { clipped = true; break; }
          }
          if (!clipped) { worst = el.className.toString().slice(0, 60); break; }
        }
      }
      return {
        docOverflow: doc.scrollWidth > doc.clientWidth + 1,
        btnInView: br.left >= -1 && br.right <= doc.clientWidth + 1 && br.width > 0,
        btnRect: [Math.round(br.left), Math.round(br.right)],
        worst,
      };
    });
    const tag = `${w}`;
    R.check(!r.docOverflow, `${tag}: no document horizontal overflow`);
    R.check(r.btnInView, `${tag}: col-settings button fully in viewport (rect=${r.btnRect})`);
    R.check(!r.worst, `${tag}: no unclipped overflow in filter row${r.worst ? ' — ' + r.worst : ''}`);
    await p.locator('#btn_col_settings').click({ timeout: 3000 }).catch(() => {});
    await p.waitForTimeout(300);
    const menuOpen = await p.evaluate(() => !document.getElementById('colToggleMenu').hidden);
    R.check(menuOpen, `${tag}: col menu opens on tap`);
    await R.shotOnFail(p, tag, !r.docOverflow && r.btnInView && !r.worst && menuOpen);
    R.check(errs.length === 0, `${tag}: no console errors${errs.length ? ' — ' + errs.join(' | ') : ''}`);
    await ctx.close();
  }
  await browser.close();
  R.finish();
})().catch((e) => { console.log('ERR', e.message); process.exit(1); });
