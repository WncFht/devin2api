// align-shots.js — 对齐统一 + trend 工具栏改版截图（人工目检用，不断言）
const path = require('path');
const fs = require('fs');
const { firefox, BASE, adminContext, watchErrors, SHOTS } = require('../lib/harness');

(async () => {
  const browser = await firefox.launch();
  const out = path.join(SHOTS, 'align');
  fs.mkdirSync(out, { recursive: true });

  const pages = ['trend', 'logs', 'stats', 'tokens', 'models', 'index', 'settings'];
  for (const w of [1440, 1024]) {
    const ctx = await adminContext(browser, { viewport: { width: w, height: 900 } });
    const p = await ctx.newPage();
    const errs = [];
    watchErrors(p, errs);
    for (const pg of pages) {
      await p.goto(`${BASE}/web/${pg}.html`, { waitUntil: 'domcontentloaded' });
      await p.waitForTimeout(2500);
      await p.screenshot({ path: path.join(out, `${pg}-${w}.png`), fullPage: false });
      if (pg === 'trend') {
        // 工具栏行数体检：所有直接子节点应同一条 baseline 带内
        const info = await p.evaluate(() => {
          const tb = document.querySelector('.trend-chart-toolbar');
          if (!tb) return null;
          const tops = [...tb.children].map((c) => c.getBoundingClientRect().top);
          return { rows: new Set(tops.map((t) => Math.round(t))).size, tops };
        });
        console.log(`  trend@${w} toolbar rows: ${info && info.rows}`);
      }
      const errsNow = errs.length;
      if (errsNow) { console.log(`  ${pg}@${w} errors:`, errs.splice(0).join(' | ')); }
    }
    await ctx.close();
  }
  // 移动端一档
  const ctx = await adminContext(browser, { viewport: { width: 390, height: 844 } });
  const p = await ctx.newPage();
  for (const pg of ['trend', 'logs', 'stats']) {
    await p.goto(`${BASE}/web/${pg}.html`, { waitUntil: 'domcontentloaded' });
    await p.waitForTimeout(2500);
    await p.screenshot({ path: path.join(out, `${pg}-390.png`), fullPage: false });
  }
  await ctx.close();
  await browser.close();
  console.log('shots -> ' + out);
})();
