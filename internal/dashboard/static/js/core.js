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

async function api(path, opts) {
  const res = await fetch('/panel/api' + path, opts);
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return res.json();
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
  if (code >= 400) return 'status-warn';
  if (code <= 0) return 'muted';
  return 'status-ok';
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
    document.querySelectorAll('#sideNav a').forEach(a => a.classList.toggle('on', a.dataset.tab === name));
    this.handlers[name] && this.handlers[name]();
    // 页切换后已挂起的图表恢复显示，需要按新尺寸重排。
    setTimeout(() => {
      document.querySelectorAll('#page-' + name + ' .chart').forEach(el => {
        const inst = echarts.getInstanceByDom(el);
        if (inst) inst.resize();
      });
    }, 30);
  },
};

// onVisible 以固定间隔调用 fn，但仅在对应页面可见且文档前台时真正执行。
// 每个 tab 模块在初始化时调用一次；定时器常驻、由 Tabs.current 门控。
function onVisible(name, fn, ms) {
  setInterval(() => { if (Tabs.current === name && !document.hidden) fn(); }, ms);
}

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
document.getElementById('sideNav').addEventListener('click', e => {
  const a = e.target.closest('a[data-tab]');
  if (a) Tabs.go(a.dataset.tab);
});

// 跨页联动：把条件填进请求页过滤器并切过去（由各 tab 的表格/chips 调用）。
function jumpRequests(kv) {
  const map = { q: 'reqSearch', status_class: 'fStatusClass', result: 'fResult', model: 'fReqModel', error_stage: 'fErrStage' };
  for (const k in kv) { const el = $(map[k]); if (el) el.value = kv[k]; }
  Tabs.go('requests');
  Requests.resetAndLoad();
}
