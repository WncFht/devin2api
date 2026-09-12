// 面板核心：DOM/格式化 helpers、hash 路由、按页轮询调度。
// 约定：每个 tab 模块调用 Tabs.register(name, {refresh}) 注册自己；
// core 负责切页、只在可见页上跑定时器、以及「点模型跳到请求页」这类跨页联动。

const $ = id => document.getElementById(id);

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
// qa 用于 onclick="fn('...')" 这类「双引号属性内套单引号 JS 字符串」的场景：
// 先对 JS 层转义反斜杠与单引号，再过 esc 保证属性安全。
function qa(s) { return esc(String(s == null ? '' : s).replace(/\\/g, '\\\\').replace(/'/g, "\\'")); }
function debounce(fn, ms) { let t; return function () { clearTimeout(t); t = setTimeout(fn, ms || 300); }; }

// 顶部 2px 加载条：任何 /panel/api 在途请求期间显示（stale-while-revalidate
// 的可见信号——刷新时旧数据保留，靠这条线表达"正在更新"）。
let inflight = 0;
function loadingBar(delta) {
  inflight = Math.max(0, inflight + delta);
  let bar = $('topLoading');
  if (!bar) {
    bar = document.createElement('div');
    bar.id = 'topLoading';
    document.body.appendChild(bar);
  }
  bar.classList.toggle('on', inflight > 0);
}

// 侧栏底部网关状态点：跟随最近一次 API 调用结果变色，不额外发心跳。
function gwState(ok) {
  const dot = $('gwDot'), txt = $('gwText');
  if (!dot) return;
  dot.classList.toggle('err', !ok);
  txt.textContent = ok ? '运行中' : '连接异常';
}

async function api(path, opts) {
  loadingBar(1);
  try {
    const res = await fetch('/panel/api' + path, opts);
    if (!res.ok) throw new Error('HTTP ' + res.status);
    gwState(true);
    return res.json();
  } catch (e) { gwState(false); throw e; }
  finally { loadingBar(-1); }
}

// ---------- 格式化 ----------
function fmtNum(v) {
  const n = Number(v) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}
function fmtBytes(v) {
  const n = Number(v) || 0;
  if (n >= 1073741824) return (n / 1073741824).toFixed(1) + 'GB';
  if (n >= 1048576) return (n / 1048576).toFixed(1) + 'MB';
  if (n >= 1024) return (n / 1024).toFixed(1) + 'KB';
  return n + 'B';
}
function fmtMs(v) { return v == null ? '-' : (v >= 1000 ? (v / 1000).toFixed(1) + 's' : Math.round(v) + 'ms'); }
function fmtDuration(sec) {
  sec = Number(sec) || 0;
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm ' + sec % 60 + 's';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h ' + Math.floor(sec % 3600 / 60) + 'm';
  return Math.floor(sec / 86400) + 'd ' + Math.floor(sec % 86400 / 3600) + 'h';
}
function fmtTime(iso) {
  try {
    const d = new Date(iso);
    if (isNaN(d)) return iso || '-';
    const t = d.toLocaleTimeString('zh-CN', { hour12: false });
    if (d.toDateString() === new Date().toDateString()) return t;
    return String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0') + ' ' + t;
  } catch (e) { return iso || '-'; }
}
function fmtUnix(v) {
  if (v == null || v === '' || Number(v) === 0) return '-';
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return String(v);
  try { return new Date(n * 1000).toLocaleString('zh-CN', { hour12: false }); } catch (e) { return String(v); }
}
// fmtUnixShort 输出 M/D HH:mm 短形式，用于 KPI 卡等窄位。
function fmtUnixShort(v) {
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return '-';
  const d = new Date(n * 1000);
  return (d.getMonth() + 1) + '/' + d.getDate() + ' ' + String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}
function fmtQuota(v) {
  if (v == null || v === '' || v === undefined) return '-';
  if (Number(v) === -1) return '不限';
  return String(v);
}
function money(v) {
  if (v == null || v === undefined || v === '') return '<span class="muted">—</span>';
  const n = Number(v);
  if (Number.isNaN(n)) return '<span class="muted">—</span>';
  return '<span class="num">$' + n.toFixed(n >= 10 ? 1 : n >= 1 ? 2 : 3) + '</span>';
}
function statusClass(code) {
  if (code >= 500) return 'status-err';
  if (code === 429) return 'status-rl';
  if (code >= 400) return 'status-warn';
  if (code <= 0) return 'muted';
  return 'status-ok';
}
// 结果 badge：语义分层——failed 红、aborted/disconnected 琥珀（非错误）、
// completed 绿、进行中 accent。中断/断连不算失败是重要区分。
function resultBadge(result) {
  const map = {
    completed: ['ok', '成功'], failed: ['err', '失败'],
    aborted: ['warn', '已中断'], disconnected: ['warn', '断连'],
  };
  const m = map[result] || ['muted', result || '-'];
  return '<span class="rbadge r-' + m[0] + '">' + esc(m[1]) + '</span>';
}
// 阈值着色：秒值与百分比分档上色的统一口径（参照同类面板的 timingColor）。
function secClass(v, okLim, warnLim) {
  const s = Number(v) / 1000;
  return s < okLim ? 'status-ok' : s < warnLim ? 'status-warn' : 'status-err';
}
function rateClass(pct, okLim, warnLim) {
  const p = Number(pct);
  return p >= (okLim ?? 95) ? 'status-ok' : p >= (warnLim ?? 80) ? 'status-warn' : 'status-err';
}
// fmtRel 相对时间（"3 分钟前"），title 里放绝对时间由调用方决定。
function fmtRel(v) {
  const t = typeof v === 'number' ? v * 1000 : new Date(v).getTime();
  if (!Number.isFinite(t) || t <= 0) return '-';
  const d = (Date.now() - t) / 1000;
  if (d < 60) return Math.max(0, Math.floor(d)) + ' 秒前';
  if (d < 3600) return Math.floor(d / 60) + ' 分钟前';
  if (d < 86400) return Math.floor(d / 3600) + ' 小时前';
  return Math.floor(d / 86400) + ' 天前';
}
// fmtIn 倒计时（"2h 34m 后"），用于配额重置等未来时刻。
function fmtIn(v) {
  const t = typeof v === 'number' ? v * 1000 : new Date(v).getTime();
  if (!Number.isFinite(t) || t <= 0) return '-';
  const d = (t - Date.now()) / 1000;
  if (d <= 0) return '已重置';
  if (d < 60) return '<1m 后';
  if (d < 3600) return Math.floor(d / 60) + 'm 后';
  if (d < 86400) return Math.floor(d / 3600) + 'h ' + Math.floor(d % 3600 / 60) + 'm 后';
  return Math.floor(d / 86400) + 'd ' + Math.floor(d % 86400 / 3600) + 'h 后';
}
function hitRate(t) {
  const dd = (t.cache_read_tokens || 0) + (t.input_tokens || 0);
  return dd > 0 ? (100 * t.cache_read_tokens / dd).toFixed(1) + '%' : '-';
}
function avgTps(t) {
  return (t.gen_ms > 0) ? (t.gen_tokens / (t.gen_ms / 1000)).toFixed(1) + ' tok/s' : '-';
}

// ---------- 组件 ----------
// kpi 卡片：label + 大数字 + 副行。tone 控制左侧色点。
function kpi(label, value, sub, tone) {
  return '<div class="kpi' + (tone ? ' tone-' + tone : '') + '">' +
    '<div class="k-label">' + (tone ? '<i></i>' : '') + esc(label) + '</div>' +
    '<div class="k-value">' + value + '</div>' +
    (sub ? '<div class="k-sub">' + sub + '</div>' : '') + '</div>';
}
function meta(k, v) {
  return '<div class="mini"><span class="k">' + esc(k) + '</span><span class="v">' + esc(String(v)) + '</span></div>';
}
// 配额条：label + 剩余% + 副信息（耗尽预测/重置）。
function qbar(label, pct, sub) {
  const p = Number(pct);
  if (!Number.isFinite(p)) return '';
  const tone = p > 50 ? 'ok' : p > 20 ? 'warn' : 'err';
  const shown = p <= 0 ? '已用尽' : p.toFixed(1) + '%';
  return '<div class="qbar"><div class="qb-head"><span class="t">' + esc(label) + '</span><span class="v">' + shown + '</span></div>' +
    '<div class="qb-track"><div class="qb-fill ' + tone + '" style="width:' + Math.max(0, Math.min(100, p)) + '%"></div></div>' +
    (sub ? '<div class="qb-sub">' + esc(sub) + '</div>' : '') + '</div>';
}
function fillSelect(id, values) {
  const el = $(id);
  const cur = el.value;
  const keep = el.options[0].outerHTML;
  el.innerHTML = keep + [...values].filter(Boolean).sort().map(v => '<option value="' + esc(v) + '">' + esc(v) + '</option>').join('');
  if ([...el.options].some(o => o.value === cur)) el.value = cur;
}
function sumTotals(list) {
  const t = { requests: 0, errors: 0, disconnected: 0, rate_limited: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, total_tokens: 0, gen_ms: 0, gen_tokens: 0 };
  list.forEach(p => { for (const k in t) t[k] += p[k] || 0; });
  return t;
}

// ---------- 反馈组件 ----------
// toast：右上角堆叠，success 3s / error 5s 自动消失。
function toast(msg, type) {
  let box = $('toastBox');
  if (!box) {
    box = document.createElement('div');
    box.id = 'toastBox';
    document.body.appendChild(box);
  }
  const el = document.createElement('div');
  el.className = 'toast t-' + (type || 'info');
  el.textContent = msg;
  box.appendChild(el);
  setTimeout(() => el.classList.add('out'), type === 'err' ? 5000 : 3000);
  setTimeout(() => el.remove(), type === 'err' ? 5400 : 3400);
}

// copyText 带 execCommand 降级（非 HTTPS 内网下 clipboard API 不存在）。
async function copyText(text, hint) {
  let ok = false;
  try {
    if (navigator.clipboard) { await navigator.clipboard.writeText(text); ok = true; }
  } catch (e) { ok = false; }
  if (!ok) {
    const ta = document.createElement('textarea');
    ta.value = text; ta.style.position = 'fixed'; ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    try { ok = document.execCommand('copy'); } catch (e) { ok = false; }
    ta.remove();
  }
  toast(ok ? (hint || '已复制') : '复制失败', ok ? 'ok' : 'err');
  return ok;
}

// confirmBox：窄弹窗替代原生 confirm，danger 时确认键红底。
// 返回 Promise<boolean>；Esc/遮罩点击 = 取消。
function confirmBox(title, msg, danger) {
  return new Promise(resolve => {
    const ov = document.createElement('div');
    ov.className = 'dlg-mask';
    ov.innerHTML = '<div class="dlg"><div class="dlg-t">' + esc(title) + '</div>' +
      '<div class="dlg-m">' + esc(msg) + '</div>' +
      '<div class="dlg-b"><button class="btn" data-a="no">取消</button>' +
      '<button class="btn ' + (danger ? 'btn-danger-solid' : 'btn-on-solid') + '" data-a="yes">确认</button></div></div>';
    const done = v => { ov.remove(); document.removeEventListener('keydown', onKey); resolve(v); };
    const onKey = e => { if (e.key === 'Escape') done(false); };
    ov.addEventListener('click', e => {
      if (e.target === ov) return done(false);
      const b = e.target.closest('[data-a]');
      if (b) done(b.dataset.a === 'yes');
    });
    document.addEventListener('keydown', onKey);
    document.body.appendChild(ov);
    ov.querySelector('[data-a=no]').focus();
  });
}

// 页面偏好持久化（localStorage，键空间 panel.页面.字段）。
function loadPref(key, def) {
  try { const v = localStorage.getItem('panel.' + key); return v === null ? def : JSON.parse(v); }
  catch (e) { return def; }
}
function savePref(key, v) {
  try { localStorage.setItem('panel.' + key, JSON.stringify(v)); } catch (e) {}
}

// titleBadge：在途请求数写到标签页标题，后台也能瞥见。
const baseTitle = 'Devin API - 管理面板';
function titleBadge(n) {
  document.title = (n > 0 ? '(' + n + ') ' : '') + baseTitle;
}

// ---------- 路由与轮询 ----------
// hash 形如 #requests&model=x&result=failed：首段是 tab 名，其余是页面内过滤参数。
const Tabs = {
  handlers: {},
  current: null,
  register(name, fn) { this.handlers[name] = fn; },
  go(name) { location.hash = '#' + name; },
  apply(name) {
    if (!this.handlers[name]) name = 'overview';
    this.current = name;
    document.querySelectorAll('.page').forEach(p => p.classList.toggle('on', p.id === 'page-' + name));
    document.querySelectorAll('#topNav a').forEach(a => a.classList.toggle('on', a.dataset.tab === name));
    this.handlers[name] && this.handlers[name]();
    Polls.reset(name);
    // 页切换后已挂起的图表恢复显示，需要按新尺寸重排。
    setTimeout(() => {
      document.querySelectorAll('#page-' + name + ' .chart').forEach(el => {
        const inst = echarts.getInstanceByDom(el);
        if (inst) inst.resize();
      });
    }, 30);
  },
};

// Polls 是面板的轮询调度器（对齐同类面板自动刷新的行为）：
// - setTimeout 链而非 setInterval——fn 落地才排下一轮，慢请求不会在飞叠加；
// - 某轮若页面非当前页 / 浏览器后台 / 确认弹窗打开则跳过，下轮照常；
// - interval 可为固定 ms 或 ()=>ms（按当前状态算节奏，如请求页活跃加速）；
// - 浏览器标签回前台时对当前页 kick 一轮，不空等下个周期；
// - Tabs.apply 切页时 reset 该页定时器，让下一次轮询从手动加载之后起算。
const Polls = {
  items: [],
  add(name, fn, interval) {
    const it = { name, fn, interval, timer: null };
    const wait = () => typeof it.interval === 'function' ? it.interval() : it.interval;
    const loop = async () => {
      if (Tabs.current === it.name && !document.hidden && !document.querySelector('.dlg-mask')) {
        try { await it.fn(); } catch (e) { /* 单次失败不挡后续轮询 */ }
      }
      it.timer = setTimeout(loop, wait());
    };
    it.kick = () => { clearTimeout(it.timer); loop(); };
    it.reset = () => { clearTimeout(it.timer); it.timer = setTimeout(loop, wait()); };
    it.timer = setTimeout(loop, wait());
    this.items.push(it);
    return it;
  },
  kick(name) { this.items.forEach(it => { if (it.name === name) it.kick(); }); },
  reset(name) { this.items.forEach(it => { if (it.name === name) it.reset(); }); },
};
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && Tabs.current) Polls.kick(Tabs.current);
});

function parseHash() {
  const raw = location.hash.slice(1);
  const [tab, ...rest] = raw.split('&');
  return { tab: tab || 'overview', params: new URLSearchParams(rest.join('&')) };
}
// writeHash 保留 tab 段，重写过滤参数段。
function writeHash(tab, params) {
  const s = params.toString();
  history.replaceState(null, '', location.pathname + '#' + tab + (s ? '&' + s : ''));
}
window.addEventListener('hashchange', () => {
  const h = parseHash();
  if (h.tab !== Tabs.current) Tabs.apply(h.tab);
});
document.getElementById('topNav').addEventListener('click', e => {
  const a = e.target.closest('a[data-tab]');
  if (a) Tabs.go(a.dataset.tab);
});

// 跨页联动：把条件填进请求页过滤器并切过去（由各 tab 的表格/chips 调用）。
function jumpRequests(kv) {
  const map = { q: 'reqSearch', status: 'fStatus', result: 'fResult', model: 'fReqModel', error_stage: 'fErrStage' };
  for (const k in kv) { const el = $(map[k]); if (el) el.value = kv[k]; }
  Tabs.go('requests');
  Requests.resetAndLoad();
}
