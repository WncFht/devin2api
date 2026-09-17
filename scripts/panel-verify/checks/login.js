// checks/login.js — UI 登录链路：admin 密码登录拿满 8 项 nav；
// api_token 登录（auth.api_key 播种行）应得 api_token 角色，
// nav 只剩白名单 4 项，访问受限页被重定向回 index。
const { firefox, BASE, ADMIN_PW, API_TOKEN, API_TOKEN_NAV, watchErrors, makeReporter } = require('../lib/harness');

(async () => {
  const R = makeReporter('login');
  const browser = await firefox.launch();

  // 1. admin 密码登录（走真实表单，不走 addInitScript）
  const ctx = await browser.newContext({ viewport: { width: 1440, height: 900 }, locale: 'zh-CN' });
  const p = await ctx.newPage();
  const errs = [];
  watchErrors(p, errs);
  await p.goto(`${BASE}/web/login.html`);
  await p.fill('#password', ADMIN_PW);
  await p.click('#login-button');
  await p.waitForURL(/\/web\/index\.html/, { timeout: 10000 }).catch(() => {});
  await p.waitForSelector('.topnav', { timeout: 8000 }).catch(() => {});
  const nav = await p.$$eval('.topnav .topnav-link', (els) => els.map((a) => a.dataset.navKey));
  R.check(p.url().includes('/web/'), `admin login lands on panel (url=${p.url()})`);
  R.check(nav.length === 8, `admin nav has 8 items (got ${nav.length}: ${nav})`);
  R.check(errs.length === 0, `no console errors during admin login${errs.length ? ' — ' + errs.join(' | ') : ''}`);
  await R.shotOnFail(p, 'admin', errs.length === 0 && nav.length === 8);
  await ctx.close();

  // 2. api_token 登录 → 受限视图
  const ctx2 = await browser.newContext({ viewport: { width: 1440, height: 900 }, locale: 'zh-CN' });
  const p2 = await ctx2.newPage();
  const errs2 = [];
  watchErrors(p2, errs2);
  await p2.goto(`${BASE}/web/login.html`);
  await p2.click('[data-login-mode="api_token"]');
  await p2.fill('#api-token', API_TOKEN);
  await p2.click('#login-button');
  await p2.waitForURL(/\/web\//, { timeout: 10000 }).catch(() => {});
  await p2.goto(`${BASE}/web/index.html`);
  await p2.waitForSelector('.topnav', { timeout: 8000 }).catch(() => {});
  await p2.waitForTimeout(800); // /dashboard/session 回角色
  const nav2 = await p2.$$eval('.topnav .topnav-link', (els) => els.map((a) => a.dataset.navKey));
  const role = await p2.evaluate(() => localStorage.getItem('ccload_web_role'));
  R.check(role === 'api_token', `api_token login role=api_token (got ${role})`);
  R.check(JSON.stringify(nav2) === JSON.stringify(API_TOKEN_NAV), `api_token nav restricted to ${API_TOKEN_NAV} (got ${nav2})`);

  // 3. api_token 直接访问受限页 → 重定向回 index
  await p2.goto(`${BASE}/web/tokens.html`);
  await p2.waitForTimeout(1500);
  const redirected = !p2.url().includes('tokens.html');
  R.check(redirected, `api_token visiting tokens.html redirected (url=${p2.url()})`);
  await R.shotOnFail(p2, 'apitoken', role === 'api_token' && nav2.length === 4 && redirected);
  await ctx2.close();
  await browser.close();
  R.finish();
})().catch((e) => { console.log('ERR', e.message); process.exit(1); });
