// panel-qa.js — ccpanel 视觉/交互 QA：截图、溢出检测、i18n 泄漏、console 错误。
//
// 用法:
//   node scripts/panel-qa.js shot [page ...]      截图 + console 断言（默认全部页面）
//   node scripts/panel-qa.js overflow <page> [w]  元素级溢出检测（含祖先裁剪链判断）
//   node scripts/panel-qa.js sweep [w ...]        页×宽矩阵：console + 溢出 + i18n 泄漏
//
// 环境变量:
//   PANEL_QA_BASE    面板地址（默认 http://127.0.0.1:3033）
//   PANEL_QA_TOKEN   dashboard.password（默认从 ./config.yaml 提取）
//   PANEL_QA_OUT     截图输出目录（默认 /tmp/panel-qa-<ts>/）
//   PLAYWRIGHT_PATH  playwright 模块路径（默认依次试 playwright →
//                    playwright-core → npm root -g 下的 playwright）
//
// 面板沿用了 ccload 的 localStorage 三件套做免登录：
// ccload_token / ccload_token_expiry / ccload_web_role。
const path = require('path');
const fs = require('fs');
const { execSync } = require('child_process');

function loadPlaywright() {
  const home = process.env.HOME || '';
  const cands = [
    process.env.PLAYWRIGHT_PATH,
    'playwright',
    'playwright-core',
    // archbox 上 playwright 寄生在 taac2026-cli 的 node_modules 里
    path.join(home, '.local/lib/node_modules/taac2026-cli/node_modules/playwright'),
  ].filter(Boolean);
  for (const c of cands) {
    try { return require(c); } catch {}
  }
  try {
    const root = execSync('npm root -g').toString().trim();
    return require(path.join(root, 'playwright'));
  } catch {}
  console.error('playwright 不可用：npm i -g playwright，或 PLAYWRIGHT_PATH=<path> 指定');
  process.exit(2);
}

const { chromium } = loadPlaywright();
const BASE = process.env.PANEL_QA_BASE || 'http://127.0.0.1:3033';
const OUT = process.env.PANEL_QA_OUT || `/tmp/panel-qa-${Date.now()}`;
const PAGES = ['index', 'stats', 'trend', 'accounts', 'logs', 'models', 'tokens', 'settings'];

function token() {
  if (process.env.PANEL_QA_TOKEN) return process.env.PANEL_QA_TOKEN;
  try {
    const cfg = fs.readFileSync('./config.yaml', 'utf8');
    const m = cfg.match(/dashboard:[\s\S]*?password:\s*['"]?([^'"\s]+)/);
    if (m) return m[1];
  } catch {}
  return '';
}

async function newContext(browser, width, opts = {}) {
  const ctx = await browser.newContext({
    viewport: { width, height: opts.height || Math.max(660, Math.round(width * 0.66)) },
    deviceScaleFactor: opts.dsf || 1,
    ...(opts.locale ? { locale: opts.locale } : {}),
    ...(opts.colorScheme ? { colorScheme: opts.colorScheme } : {}),
  });
  await ctx.addInitScript((tk) => {
    localStorage.setItem('ccload_token', tk);
    localStorage.setItem('ccload_token_expiry', String(Date.now() + 86400e3));
    localStorage.setItem('ccload_web_role', 'admin');
  }, token());
  return ctx;
}

function watchErrors(page, errs) {
  page.on('console', (m) => { if (m.type() === 'error') errs.push(m.text().slice(0, 300)); });
  page.on('pageerror', (e) => errs.push('PAGEERROR: ' + String(e).slice(0, 300)));
}

const OVERFLOW_EXPR = `(() => {
  const docW = document.documentElement.clientWidth;
  const bad = [];
  document.querySelectorAll('body *').forEach((el) => {
    const r = el.getBoundingClientRect();
    if (r.width === 0) return;
    const overSelf = el.scrollWidth > el.clientWidth + 2 && getComputedStyle(el).overflowX === 'visible';
    const overDoc = r.right > docW + 2;
    if (!overSelf && !overDoc) return;
    let clipped = false, p = el.parentElement;
    while (p) { const ox = getComputedStyle(p).overflowX; if (ox === 'auto' || ox === 'scroll' || ox === 'hidden') { clipped = true; break; } p = p.parentElement; }
    if (clipped) return;
    const cls = (el.className && el.className.baseVal === undefined) ? String(el.className).slice(0, 80) : el.tagName;
    bad.push(el.tagName + '.' + cls + ' sw=' + el.scrollWidth + ' cw=' + el.clientWidth + ' right=' + Math.round(r.right) + ' text=' + (el.textContent || '').trim().slice(0, 40));
  });
  return bad.slice(0, 15);
})()`;

const I18N_LEAK_EXPR = `(() => {
  const leaks = [];
  document.querySelectorAll('[data-i18n]').forEach((el) => {
    const v = el.textContent.trim();
    if (/^[a-z_]+\\.[a-zA-Z_.]+$/.test(v)) leaks.push(v);
  });
  return leaks.slice(0, 10);
})()`;

async function cmdShot(pages) {
  fs.mkdirSync(OUT, { recursive: true });
  const browser = await chromium.launch();
  const ctx = await newContext(browser, 1500, { dsf: 2 });
  const page = await ctx.newPage();
  const errors = [];
  watchErrors(page, errors);
  for (const p of pages) {
    errors.length = 0;
    await page.goto(`${BASE}/web/${p}.html`, { waitUntil: 'networkidle', timeout: 30000 })
      .catch((e) => errors.push('GOTO: ' + e));
    await page.waitForTimeout(1200);
    await page.screenshot({ path: path.join(OUT, `${p}.png`), fullPage: true });
    const m = await page.evaluate(() => ({
      sw: document.documentElement.scrollWidth,
      cw: document.documentElement.clientWidth,
      overflowX: document.documentElement.scrollWidth > document.documentElement.clientWidth + 1,
    }));
    console.log(`${p}: scrollW=${m.sw} clientW=${m.cw} overflowX=${m.overflowX}${errors.length ? ' CONSOLE:' + JSON.stringify(errors.slice(0, 5)) : ''}`);
  }
  await browser.close();
  console.log('shots ->', OUT);
}

async function cmdOverflow(pageName, width) {
  const browser = await chromium.launch();
  const ctx = await newContext(browser, width, { height: 900 });
  const page = await ctx.newPage();
  const errs = [];
  watchErrors(page, errs);
  await page.goto(`${BASE}/web/${pageName}.html`, { waitUntil: 'networkidle', timeout: 40000 }).catch(() => {});
  await page.waitForTimeout(1500);
  const found = await page.evaluate(OVERFLOW_EXPR);
  console.log(found.join('\n') || 'clean');
  if (errs.length) console.log('console:', errs.slice(0, 5).join(' | '));
  await browser.close();
}

async function cmdSweep(widths) {
  const browser = await chromium.launch();
  const results = [];
  for (const vw of widths) {
    for (const pg of PAGES) {
      const ctx = await newContext(browser, vw, { height: 900 });
      const page = await ctx.newPage();
      const errs = [];
      watchErrors(page, errs);
      await page.goto(`${BASE}/web/${pg}.html`, { waitUntil: 'networkidle', timeout: 40000 })
        .catch((e) => errs.push('NAV: ' + e.message.slice(0, 80)));
      await page.waitForTimeout(1600);
      const over = await page.evaluate(OVERFLOW_EXPR);
      const leaks = await page.evaluate(I18N_LEAK_EXPR);
      if (errs.length || over.length || leaks.length)
        results.push(`[${pg}@${vw}] console=${errs.length} overflow=${over.length} i18nleak=${leaks.length}\n  errs: ${errs.slice(0, 3).join(' | ')}\n  over: ${over.slice(0, 5).join(' | ')}\n  leak: ${leaks.join(', ')}`);
      else results.push(`[${pg}@${vw}] clean`);
      await ctx.close();
    }
  }
  console.log(results.join('\n'));
  await browser.close();
}

(async () => {
  const [cmd, ...rest] = process.argv.slice(2);
  if (cmd === 'shot') await cmdShot(rest.length ? rest : PAGES);
  else if (cmd === 'overflow') await cmdOverflow(rest[0] || 'logs', parseInt(rest[1] || '1280', 10));
  else if (cmd === 'sweep') await cmdSweep(rest.length ? rest.map(Number) : [1280, 390]);
  else {
    console.error('usage: panel-qa.js shot [page...] | overflow <page> [w] | sweep [w...]');
    process.exit(2);
  }
})();
