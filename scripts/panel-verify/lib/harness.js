// harness.js — panel-verify 各检查共用的最小设施：
// playwright 加载、登录（UI 与编程式两种）、console/pageerror 收集、
// 失败截图、断言计数。所有路径/凭据走 PV_* 环境变量，由 run.sh 注入。
const path = require('path');
const fs = require('fs');
const { firefox } = require('playwright');

const BASE = process.env.PV_BASE || 'http://127.0.0.1:3461';
const ADMIN_PW = process.env.PV_ADMIN_PW || 'testpw';
const API_TOKEN = process.env.PV_API_TOKEN || 'testkey';
const SHOTS = process.env.PV_SHOTS || path.join(__dirname, '..', 'shots');

const PAGES = ['index', 'tokens', 'models', 'stats', 'trend', 'quota', 'logs', 'settings'];
// api_token 角色可见的 nav 子集；其余页面会被重定向回 index。
const API_TOKEN_NAV = ['index', 'stats', 'trend', 'logs'];

// loginData 调 POST /login 拿 {token, expiresIn, role}，绕过 UI 表单——
// UI 登录链路本身由 checks/login.js 单独覆盖，其它检查不需要重复走。
async function loginData(mode, credential) {
  const resp = await fetch(`${BASE}/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(mode === 'api_token' ? { mode, token: credential } : { mode, password: credential }),
  });
  const body = await resp.json();
  if (!body.success) throw new Error(`login ${mode} failed: ${resp.status} ${JSON.stringify(body).slice(0, 200)}`);
  return body.data;
}

// sessionRole 探某凭据经 /dashboard/session 判出的有效角色：POST /login
// 回包里的 role 是登录入口视角，api_token 登录在开放面板下也回 api_token；
// session 才是 withWebAuth 的真实判定（开放面板一切凭据归 admin）。
// checks/login.js 用它区分「受限视图可断言」与「开放面板该改断言」。
async function sessionRole(token) {
  const resp = await fetch(`${BASE}/dashboard/session`, { headers: { Authorization: `Bearer ${token}` } });
  const body = await resp.json();
  if (!body.success) throw new Error(`session probe failed: ${resp.status} ${JSON.stringify(body).slice(0, 200)}`);
  return body.data.role;
}

// adminContext 返回已种好 ccload 三件套的 browser context，面板跳过登录页。
async function adminContext(browser, opts = {}) {
  const data = await loginData('admin', ADMIN_PW);
  const ctx = await browser.newContext({ locale: 'zh-CN', ...opts });
  await ctx.addInitScript((d) => {
    localStorage.setItem('ccload_token', d.token);
    localStorage.setItem('ccload_token_expiry', String(Date.now() + d.expiresIn * 1000));
    localStorage.setItem('ccload_web_role', d.role || 'admin');
  }, data);
  return ctx;
}

// watchErrors 收集 console error / pageerror / 4xx+ 响应进 errs 数组。
function watchErrors(page, errs) {
  page.on('console', (m) => { if (m.type() === 'error') errs.push('console: ' + m.text().slice(0, 200)); });
  page.on('pageerror', (e) => errs.push('pageerror: ' + String(e.message || e).slice(0, 200)));
  page.on('response', (r) => { if (r.status() >= 400) errs.push(`http ${r.status()}: ${r.url()}`); });
}

// check/fail 计数 + finish 收口：每条断言一行输出，失败才截图。
function makeReporter(name) {
  let failed = 0;
  return {
    check(cond, label) {
      if (cond) console.log(`  ok ${label}`);
      else { failed++; console.log(`  FAIL ${label}`); }
      return !!cond;
    },
    async shotOnFail(page, tag, cond) {
      if (cond) return;
      fs.mkdirSync(SHOTS, { recursive: true });
      const file = path.join(SHOTS, `${name}-${tag}.png`);
      await page.screenshot({ path: file, fullPage: true }).catch(() => {});
      console.log(`  shot -> ${file}`);
    },
    finish() {
      console.log(`${failed ? 'FAIL' : 'PASS'} ${name}${failed ? ` (${failed} failed)` : ''}`);
      process.exit(failed ? 1 : 0);
    },
  };
}

module.exports = { firefox, BASE, ADMIN_PW, API_TOKEN, SHOTS, PAGES, API_TOKEN_NAV, loginData, sessionRole, adminContext, watchErrors, makeReporter };
