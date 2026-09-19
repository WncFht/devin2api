// audit-shots.js — 对运行中的实例逐页截图（亮/暗双主题），供设计评审。
// 用法: PV_BASE=http://127.0.0.1:3033 PV_ADMIN_PW=<pw> node audit-shots.js <outdir>
const path = require('path');
const fs = require('fs');
const { firefox, BASE, ADMIN_PW, loginData } = require('./lib/harness');

const OUT = process.argv[2] || path.join(__dirname, '..', '..', 'outputs', 'panel-shots');
const PAGES = ['index', 'tokens', 'models', 'stats', 'trend', 'accounts', 'logs', 'settings'];
const THEMES = ['light', 'dark'];

async function themedContext(browser, data, theme) {
  const ctx = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 900 } });
  await ctx.addInitScript((d) => {
    localStorage.setItem('ccload_token', d.token);
    localStorage.setItem('ccload_token_expiry', String(Date.now() + d.expiresIn * 1000));
    localStorage.setItem('ccload_web_role', d.role || 'admin');
    localStorage.setItem('ccload_theme', `${d.theme}:${Date.now()}`);
  }, { ...data, theme });
  return ctx;
}

async function shoot(page, url, file, errs, tag) {
  try {
    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 20000 });
    await page.waitForSelector('.card, table, .app-container, .login-card, main', { timeout: 8000 }).catch(() => {});
    await page.waitForTimeout(2500);
    await page.screenshot({ path: file, fullPage: true });
    console.log(`shot ${tag} -> ${path.basename(file)}`);
  } catch (e) {
    errs.push(`${tag}: goto failed ${String(e).slice(0, 160)}`);
  }
}

(async () => {
  fs.mkdirSync(OUT, { recursive: true });
  const errs = [];
  const browser = await firefox.launch();
  const data = await loginData('admin', ADMIN_PW);

  for (const theme of THEMES) {
    const ctx = await themedContext(browser, data, theme);
    const page = await ctx.newPage();
    page.on('console', (m) => { if (m.type() === 'error') errs.push(`console(${theme}): ` + m.text().slice(0, 160)); });
    page.on('pageerror', (e) => errs.push(`pageerror(${theme}): ` + String(e.message || e).slice(0, 160)));
    for (const p of PAGES) {
      await shoot(page, `${BASE}/web/${p}.html`, path.join(OUT, `${p}-${theme}.png`), errs, `${p}-${theme}`);
    }
    await ctx.close();
  }

  // 未登录 login 页
  const anon = await browser.newContext({ locale: 'zh-CN', viewport: { width: 1440, height: 900 } });
  const lp = await anon.newPage();
  await shoot(lp, `${BASE}/web/login.html`, path.join(OUT, 'login-light.png'), errs, 'login-light');
  await anon.close();
  await browser.close();

  if (errs.length) { console.log('ERRORS:'); errs.forEach((e) => console.log('  ' + e)); }
  console.log('done -> ' + OUT);
})();
