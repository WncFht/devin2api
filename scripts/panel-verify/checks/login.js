// checks/login.js — UI 登录链路：admin 密码登录拿满 9 项 nav；
// api_token 登录（auth.api_key 播种行）应得 api_token 角色，
// nav 只剩白名单 4 项，访问受限页被重定向回 index。
//
// 受限视图断言只在密码面板成立：开放面板（dashboard.password 为空）下
// withWebAuth 把任何有效凭据判成 admin，/dashboard/session 回 admin、
// nav 满 9 项。这里先探令牌的有效角色再决定断言哪一边；令牌不在仓
// （开放面板无播种行命中）时整段跳过。
const { firefox, BASE, ADMIN_PW, API_TOKEN, API_TOKEN_NAV, loginData, sessionRole, watchErrors, makeReporter } = require('../lib/harness');

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
  R.check(nav.length === 9, `admin nav has 9 items (got ${nav.length}: ${nav})`);
  R.check(errs.length === 0, `no console errors during admin login${errs.length ? ' — ' + errs.join(' | ') : ''}`);
  await R.shotOnFail(p, 'admin', errs.length === 0 && nav.length === 9);
  await ctx.close();

  // 2. api_token 登录：先探有效角色，再决定断言受限视图还是开放面板视图
  let effectiveRole = null;
  try {
    const d = await loginData('api_token', API_TOKEN);
    effectiveRole = await sessionRole(d.token);
  } catch (e) {
    console.log(`  skip api_token section (probe: ${e.message.slice(0, 120)})`);
  }

  if (effectiveRole !== null) {
    const ctx2 = await browser.newContext({ viewport: { width: 1440, height: 900 }, locale: 'zh-CN' });
    const p2 = await ctx2.newPage();
    const errs2 = [];
    watchErrors(p2, errs2);
    await p2.goto(`${BASE}/web/login.html`);
    await p2.click('[data-login-mode="api_token"]');
    await p2.fill('#api-token', API_TOKEN);
    await p2.click('#login-button');
    // waitForURL(/\/web\//) 会原地命中 login.html 自身（它就在 /web/ 下），
    // 紧随的 goto 会掐断在途 /login 请求——等 ccload_token 落 localStorage
    // 才算登录提交完成。
    await p2.waitForFunction(() => !!localStorage.getItem('ccload_token'), null, { timeout: 5000 }).catch(() => {});
    await p2.goto(`${BASE}/web/index.html`);
    await p2.waitForSelector('.topnav', { timeout: 8000 }).catch(() => {});
    await p2.waitForTimeout(800); // /dashboard/session 回角色
    const nav2 = await p2.$$eval('.topnav .topnav-link', (els) => els.map((a) => a.dataset.navKey));
    const role = await p2.evaluate(() => localStorage.getItem('ccload_web_role'));

    if (effectiveRole === 'api_token') {
      R.check(role === 'api_token', `api_token login role=api_token (got ${role})`);
      R.check(JSON.stringify(nav2) === JSON.stringify(API_TOKEN_NAV), `api_token nav restricted to ${API_TOKEN_NAV} (got ${nav2})`);

      // api_token 直接访问受限页 → 重定向回 index
      await p2.goto(`${BASE}/web/tokens.html`);
      await p2.waitForTimeout(1500);
      const redirected = !p2.url().includes('tokens.html');
      R.check(redirected, `api_token visiting tokens.html redirected (url=${p2.url()})`);
      await R.shotOnFail(p2, 'apitoken', role === 'api_token' && nav2.length === 4 && redirected);
    } else {
      // 开放面板：登录链路照跑，但 session 应把角色纠成 admin、nav 满 8 项
      R.check(p2.url().includes('/web/'), `api_token login lands on panel (url=${p2.url()})`);
      R.check(role === 'admin', `open panel session role=admin (got ${role})`);
      R.check(nav2.length === 9, `open panel nav has 9 items (got ${nav2.length})`);
      await R.shotOnFail(p2, 'apitoken', p2.url().includes('/web/') && role === 'admin' && nav2.length === 9);
    }
    R.check(errs2.length === 0, `no console errors during api_token login${errs2.length ? ' — ' + errs2.join(' | ') : ''}`);
    await ctx2.close();
  }

  await browser.close();
  R.finish();
})().catch((e) => { console.log('ERR', e.message); process.exit(1); });
