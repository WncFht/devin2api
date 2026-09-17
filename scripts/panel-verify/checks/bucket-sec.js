// bucket-sec.js — /dashboard/metrics 秒级桶宽：
// API 侧断言 X-Bucket-Sec 回传与点数封顶，UI 侧断言粒度下拉选 10 秒
// 后请求带 bucket_sec=10、间隔片显示「10 秒」。
const { firefox, BASE, adminContext, loginData, watchErrors, makeReporter } = require('../lib/harness');

(async () => {
  const rep = makeReporter('bucket-sec');
  const { token } = await loginData('admin', process.env.PV_ADMIN_PW || 'testpw');
  const auth = { Authorization: `Bearer ${token}` };

  async function probe(qs) {
    const res = await fetch(`${BASE}/dashboard/metrics?${qs}`, { headers: auth });
    const body = await res.json();
    return { sec: Number(res.headers.get('X-Bucket-Sec')), points: (body.data || []).length, status: res.status };
  }

  const now = Date.now();
  const h1 = `range=custom&start_time=${now - 3600e3}&end_time=${now}`;

  let r = await probe(`${h1}&bucket_sec=10`);
  rep.check(r.sec === 10, `1h window bucket_sec=10 -> X-Bucket-Sec 10 (got ${r.sec})`);
  rep.check(r.points > 300 && r.points <= 362, `1h@10s point count ~361 (got ${r.points})`);

  r = await probe(`${h1}&bucket_sec=5`);
  rep.check(r.sec === 10, `bucket_sec=5 floored to 10 (got ${r.sec})`);

  r = await probe(`${h1}`);
  rep.check(r.sec === 600, `absent bucket_sec defaults 600 (got ${r.sec})`);

  r = await probe(`range=last_month&bucket_sec=10`);
  rep.check(r.sec >= 900 && r.sec <= 86400, `last_month@10s raised into ladder (got ${r.sec})`);
  rep.check(r.points <= 2882, `last_month points capped <=2882 (got ${r.points})`);

  r = await probe(`${h1}&bucket_sec=99999999999`);
  rep.check(r.sec === 86400, `huge bucket_sec clamped to 1d (got ${r.sec})`);

  // UI：粒度下拉选 10 秒后，metrics 请求带 bucket_sec=10，间隔片显示秒级
  const browser = await firefox.launch();
  const ctx = await adminContext(browser, { viewport: { width: 1440, height: 900 } });
  const p = await ctx.newPage();
  const errs = [];
  watchErrors(p, errs);
  await p.goto(`${BASE}/web/trend.html`, { waitUntil: 'domcontentloaded' });
  await p.waitForSelector('#f_bucket_sec', { state: 'attached', timeout: 8000 });

  const reqSeen = p.waitForRequest((req) => req.url().includes('/dashboard/metrics?') && req.url().includes('bucket_sec=10'), { timeout: 10000 });
  await p.selectOption('#f_bucket_sec', '10');
  const req = await reqSeen;
  rep.check(req.url().includes('bucket_sec=10'), 'selecting 10s sends bucket_sec=10');

  await p.waitForFunction(() => {
    const el = document.getElementById('bucket-interval');
    return el && el.textContent.includes('秒');
  }, { timeout: 10000 });
  const chip = await p.textContent('#bucket-interval');
  rep.check(/秒/.test(chip), `interval chip shows seconds: "${chip.trim()}"`);

  const stored = await p.evaluate(() => localStorage.getItem('trend.bucketSec'));
  rep.check(stored === '10', `localStorage trend.bucketSec persisted (got ${stored})`);

  await p.screenshot({ path: require('path').join(require('../lib/harness').SHOTS, 'bucket-sec-trend.png') });
  rep.check(errs.length === 0, `no page errors${errs.length ? ': ' + errs.join(' | ') : ''}`);

  await browser.close();
  rep.finish();
})().catch((e) => { console.error(e); process.exit(1); });
