// 面板核心：DOM/格式化 helpers、hash 路由、按页轮询调度、morph 渲染器、
// 统一 API 出口（api/apiRaw）。约定：每个 tab 模块调用 Tabs.register(name, fn)
// 注册自己；core 负责切页、只在可见页上跑定时器。
// 全部经 ES module 显式导入，不再依赖 script 标签顺序与全局符号。

import morphdom from './vendor/morphdom.js';

export const $ = id => document.getElementById(id);

export function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#39;');
}
export function debounce(fn, ms) { let t; return function () { clearTimeout(t); t = setTimeout(fn, ms || 300); }; }

// morph 用「目标 HTML」外科手术式更新 el 的子树：morphdom 只改动真正变化
// 的节点，未变节点保持身份——展开行、悬停态、选区、滚动位置自然保留，
// 替代原先 innerHTML + 签名比对 + 节点移植的组合。
// _echarts_instance_ 是 echarts.init 打在容器上的标记：这类容器内部由
// echarts 自管（canvas/option），整棵跳过 morph；被移除时（条件性出现的
// 图表消失）顺带 dispose 实例，避免 canvas/监听器泄漏。
export function morph(el, html) {
  const t = document.createElement(el.tagName);
  t.innerHTML = html;
  morphdom(el, t, {
    childrenOnly: true,
    onBeforeElUpdated: fromEl => !fromEl.hasAttribute('_echarts_instance_'),
    onNodeDiscarded(node) {
      if (node.nodeType !== 1 || !window.echarts) return;
      if (node.hasAttribute('_echarts_instance_')) {
        const inst = window.echarts.getInstanceByDom(node);
        if (inst) inst.dispose();
      }
      node.querySelectorAll('[_echarts_instance_]').forEach(d => {
        const inst = window.echarts.getInstanceByDom(d);
        if (inst) inst.dispose();
      });
    },
  });
}

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

// 顶栏网关状态点：跟随最近一次 API 调用结果变色，不额外发心跳。
function gwState(ok) {
  const dot = $('gwDot'), txt = $('gwText');
  if (!dot) return;
  dot.classList.toggle('err', !ok);
  txt.textContent = ok ? '运行中' : '连接异常';
}

// apiRaw 是所有 /panel/api 出口的统一底层：loadingBar/gwState/401 跳登录。
// 需要按 status 分支或读错误体的调用方用它（abort/config reload）。
export async function apiRaw(path, opts) {
  loadingBar(1);
  try {
    const res = await fetch('/panel/api' + path, opts);
    // 会话过期或服务重启（session 是内存表）时回登录页，
    // 比每页各自弹「连接异常」更直接。
    if (res.status === 401) { location.href = '/panel'; throw new Error('unauthorized'); }
    gwState(true);
    return res;
  } catch (e) { gwState(false); throw e; }
  finally { loadingBar(-1); }
}
// api 是 JSON 消费位的常规出口；非 2xx 抛错（要错误体请用 apiRaw）。
export async function api(path, opts) {
  const res = await apiRaw(path, opts);
  if (!res.ok) throw new Error('HTTP ' + res.status);
  return res.json();
}

// ---------- 格式化 ----------
export function fmtNum(v) {
  const n = Number(v) || 0;
  if (n >= 1e6) return (n / 1e6).toFixed(2) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
  return String(n);
}
export function fmtBytes(v) {
  const n = Number(v) || 0;
  if (n >= 1073741824) return (n / 1073741824).toFixed(1) + 'GB';
  if (n >= 1048576) return (n / 1048576).toFixed(1) + 'MB';
  if (n >= 1024) return (n / 1024).toFixed(1) + 'KB';
  return n + 'B';
}
export function fmtMs(v) { return v == null ? '-' : (v >= 1000 ? (v / 1000).toFixed(1) + 's' : Math.round(v) + 'ms'); }
export function fmtDuration(sec) {
  sec = Number(sec) || 0;
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm ' + sec % 60 + 's';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h ' + Math.floor(sec % 3600 / 60) + 'm';
  return Math.floor(sec / 86400) + 'd ' + Math.floor(sec % 86400 / 3600) + 'h';
}
// toLocaleString 系列每次调用都新建 Intl.DateTimeFormat（内部查 locale
// 数据表）——请求表/矩阵每秒上百次调用时构造开销不可忽略，缓存两个实例。
const TIME_FMT = new Intl.DateTimeFormat('zh-CN', { hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
const DATETIME_FMT = new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false });
export function fmtTime(iso) {
  const d = new Date(iso);
  if (isNaN(d)) return iso || '-';
  const t = TIME_FMT.format(d);
  if (d.toDateString() === new Date().toDateString()) return t;
  return String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0') + ' ' + t;
}
export function fmtUnix(v) {
  if (v == null || v === '' || Number(v) === 0) return '-';
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return String(v);
  const d = new Date(n * 1000);
  return isNaN(d) ? String(v) : DATETIME_FMT.format(d);
}
// fmtUnixShort 输出 M/D HH:mm 短形式，用于 KPI 卡等窄位。
export function fmtUnixShort(v) {
  const n = Number(v);
  if (!Number.isFinite(n) || n <= 0) return '-';
  const d = new Date(n * 1000);
  return (d.getMonth() + 1) + '/' + d.getDate() + ' ' + String(d.getHours()).padStart(2, '0') + ':' + String(d.getMinutes()).padStart(2, '0');
}
export function fmtQuota(v) {
  if (v == null || v === '' || v === undefined) return '-';
  if (Number(v) === -1) return '不限';
  return String(v);
}
export function money(v) {
  if (v == null || v === undefined || v === '') return '<span class="muted">—</span>';
  const n = Number(v);
  if (Number.isNaN(n)) return '<span class="muted">—</span>';
  return '<span class="num">$' + n.toFixed(n >= 10 ? 1 : n >= 1 ? 2 : 3) + '</span>';
}
export function statusClass(code) {
  if (code >= 500) return 'status-err';
  if (code === 429) return 'status-rl';
  if (code >= 400) return 'status-warn';
  if (code <= 0) return 'muted';
  return 'status-ok';
}
// 结果 badge：语义分层——failed 红、aborted/disconnected 琥珀（非错误）、
// completed 绿、进行中 accent。中断/断连不算失败是重要区分。
export function resultBadge(result) {
  const map = {
    completed: ['ok', '成功'], failed: ['err', '失败'],
    aborted: ['warn', '已中断'], disconnected: ['warn', '断连'],
  };
  const m = map[result] || ['muted', result || '-'];
  return '<span class="rbadge r-' + m[0] + '">' + esc(m[1]) + '</span>';
}
// 阈值着色：秒值与百分比分档上色的统一口径。
export function secClass(v, okLim, warnLim) {
  const s = Number(v) / 1000;
  return s < okLim ? 'status-ok' : s < warnLim ? 'status-warn' : 'status-err';
}
export function rateClass(pct, okLim, warnLim) {
  const p = Number(pct);
  return p >= (okLim ?? 95) ? 'status-ok' : p >= (warnLim ?? 80) ? 'status-warn' : 'status-err';
}
// fmtRel 相对时间（"3 分钟前"），title 里放绝对时间由调用方决定。
export function fmtRel(v) {
  const t = typeof v === 'number' ? v * 1000 : new Date(v).getTime();
  if (!Number.isFinite(t) || t <= 0) return '-';
  const d = (Date.now() - t) / 1000;
  if (d < 60) return Math.max(0, Math.floor(d)) + ' 秒前';
  if (d < 3600) return Math.floor(d / 60) + ' 分钟前';
  if (d < 86400) return Math.floor(d / 3600) + ' 小时前';
  return Math.floor(d / 86400) + ' 天前';
}
// fmtIn 倒计时（"2h 34m 后"），用于配额重置等未来时刻。
export function fmtIn(v) {
  const t = typeof v === 'number' ? v * 1000 : new Date(v).getTime();
  if (!Number.isFinite(t) || t <= 0) return '-';
  const d = (t - Date.now()) / 1000;
  if (d <= 0) return '已重置';
  if (d < 60) return '<1m 后';
  if (d < 3600) return Math.floor(d / 60) + 'm 后';
  if (d < 86400) return Math.floor(d / 3600) + 'h ' + Math.floor(d % 3600 / 60) + 'm 后';
  return Math.floor(d / 86400) + 'd ' + Math.floor(d % 86400 / 3600) + 'h 后';
}
// fmtInPrecise 是闩倒计时的精细版：剩余 <1h 时显示到秒（"12m 34s"），
// 跨小时回退 fmtIn——解闩前的最后几十分钟里秒级读数才有意义。
export function fmtInPrecise(v) {
  const t = typeof v === 'number' ? v * 1000 : new Date(v).getTime();
  if (!Number.isFinite(t) || t <= 0) return '-';
  const d = (t - Date.now()) / 1000;
  if (d <= 0) return '已到期';
  if (d < 3600) return Math.floor(d / 60) + 'm ' + Math.floor(d % 60) + 's';
  return fmtIn(t / 1000);
}

// ---------- 错误归因（与后端 debuglog.errorOwner 同口径） ----------
// client=调用方责任（断连/中断/请求体阶段失败）；business_limited=429
// 配额动作（HTTP 429 或流内限流——后者 HTTP 仍是 200，靠 rate_limited
// 标记认出）；upstream=服务端失分（上游错误与代理自身失败），SLA 只算它。
export function errorOwner(e) {
  if (e.status_code === 429 || e.rate_limited) return 'business_limited';
  if (e.result === 'disconnected' || e.result === 'aborted') return 'client';
  if (e.status_code < 400 && e.result !== 'failed') return '';
  if (e.error_stage === 'http_read' || e.error_stage === 'http_decode') return 'client';
  return 'upstream';
}
// slaRate 服务端口径成功率：分母剔除客户端责任与 429 条目后，
// upstream 失分占比取反；分母为 0（只有客户端/限流流量）返回 null。
export function slaRate(t) {
  const base = (t.requests || 0) - (t.client_faults || 0) - (t.rate_limited || 0);
  if (base <= 0) return null;
  return (base - (t.upstream_faults || 0)) / base * 100;
}
export function hitRate(t) {
  const dd = (t.cache_read_tokens || 0) + (t.input_tokens || 0);
  return dd > 0 ? (100 * t.cache_read_tokens / dd).toFixed(1) + '%' : '-';
}
export function avgTps(t) {
  return (t.gen_ms > 0) ? (t.gen_tokens / (t.gen_ms / 1000)).toFixed(1) + ' tok/s' : '-';
}

// REJECT_LABELS 是管线前拒绝原因的中文标签：请求页提示与系统页拒绝表共用一份。
export const REJECT_LABELS = {
  draining: '排空',
  concurrency_limit: '并发上限',
  ws_connection_limit: 'WS连接上限',
  missing_api_key: '缺API Key',
  invalid_api_key: '错API Key',
};
// summarizeRejects 把 rejects.recent 事件环聚合成「窗口内条数 + 分原因明细」：
// 概览判词（10 分钟窗、≥3 条才告警）与请求页提示（15 分钟窗、≥1 条即提示）
// 共用同一聚合口径，只是阈值与包裹文案不同。
export function summarizeRejects(recent, windowMs) {
  const list = (recent || []).filter(e => e.at * 1000 > Date.now() - windowMs);
  const byReason = {};
  list.forEach(e => { byReason[e.reason] = (byReason[e.reason] || 0) + 1; });
  return { n: list.length, parts: Object.keys(byReason).map(k => (REJECT_LABELS[k] || k) + ' ' + byReason[k]) };
}
// gateLatchUntil 是闩截止时刻的统一文案（"14:03:22（剩 9m 41s）"）：
// 概览判词、告警横幅与系统页闸门卡三处共用。
export function gateLatchUntil(g) {
  return g.limited_until ? fmtTime(g.limited_until) + '（剩 ' + fmtInPrecise(g.limited_until) + '）' : '时刻未知';
}

// ---------- 组件 ----------
// kpi 卡片：label + 大数字 + 副行。tone 控制左侧色点；value/sub 允许内嵌 HTML
// （money()/fmtNum()+delta 等已自带转义或纯数字），label 纯文本。
// 两者都是 nowrap+ellipsis 截断——title 兜底完整文本，被截的关键信息
// （往往在副行后半段）悬停仍可读。
const plainText = s => String(s).replace(/<[^>]+>/g, '');
export function kpi(label, value, sub, tone) {
  return '<div class="kpi' + (tone ? ' tone-' + tone : '') + '">' +
    '<div class="k-label">' + (tone ? '<i></i>' : '') + esc(label) + '</div>' +
    '<div class="k-value" title="' + esc(plainText(value)) + '">' + value + '</div>' +
    (sub ? '<div class="k-sub" title="' + esc(plainText(sub)) + '">' + sub + '</div>' : '') + '</div>';
}
export function meta(k, v) {
  return '<div class="mini"><span class="k">' + esc(k) + '</span><span class="v">' + esc(String(v)) + '</span></div>';
}
// 配额条：label + 剩余% + 副信息（耗尽预测/重置）。
export function qbar(label, pct, sub) {
  const p = Number(pct);
  if (!Number.isFinite(p)) return '';
  const tone = p > 50 ? 'ok' : p > 20 ? 'warn' : 'err';
  const shown = p <= 0 ? '已用尽' : p.toFixed(1) + '%';
  return '<div class="qbar"><div class="qb-head"><span class="t">' + esc(label) + '</span><span class="v">' + shown + '</span></div>' +
    '<div class="qb-track"><div class="qb-fill ' + tone + '" style="width:' + Math.max(0, Math.min(100, p)) + '%"></div></div>' +
    (sub ? '<div class="qb-sub">' + esc(sub) + '</div>' : '') + '</div>';
}
export function fillSelect(id, values) {
  const el = $(id);
  const cur = el.value;
  const keep = el.options[0].outerHTML;
  el.innerHTML = keep + [...values].filter(Boolean).sort().map(v => '<option value="' + esc(v) + '">' + esc(v) + '</option>').join('');
  if ([...el.options].some(o => o.value === cur)) el.value = cur;
}
export function sumTotals(list) {
  const t = { requests: 0, errors: 0, disconnected: 0, rate_limited: 0, client_faults: 0, upstream_faults: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_write_tokens: 0, reasoning_tokens: 0, total_tokens: 0, gen_ms: 0, gen_tokens: 0 };
  list.forEach(p => { for (const k in t) t[k] += p[k] || 0; });
  return t;
}

// ---------- 反馈组件 ----------
// toast：右上角堆叠，success 3s / error 5s 自动消失。
export function toast(msg, type) {
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
export async function copyText(text, hint) {
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
export function confirmBox(title, msg, danger) {
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
export function loadPref(key, def) {
  try { const v = localStorage.getItem('panel.' + key); return v === null ? def : JSON.parse(v); }
  catch (e) { return def; }
}
export function savePref(key, v) {
  try { localStorage.setItem('panel.' + key, JSON.stringify(v)); } catch (e) {}
}

// titleBadge：在途请求数写到标签页标题，后台也能瞥见。
const baseTitle = 'Devin API - 管理面板';
export function titleBadge(n) {
  document.title = (n > 0 ? '(' + n + ') ' : '') + baseTitle;
}

// ---------- 路由与轮询 ----------
// hash 形如 #requests&model=x&result=failed：首段是 tab 名，其余是页面内过滤参数。
export const Tabs = {
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
      if (!window.echarts) return;
      document.querySelectorAll('#page-' + name + ' .chart').forEach(el => {
        const inst = window.echarts.getInstanceByDom(el);
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
export const Polls = {
  items: [],
  add(name, fn, interval) {
    const it = { name, fn, interval, timer: null, running: false };
    const wait = () => typeof it.interval === 'function' ? it.interval() : it.interval;
    const loop = async () => {
      it.running = true;
      if (Tabs.current === it.name && !document.hidden && !document.querySelector('.dlg-mask')) {
        try { await it.fn(); } catch (e) { /* 单次失败不挡后续轮询 */ }
      }
      it.running = false;
      it.timer = setTimeout(loop, wait());
    };
    // kick/reset 只改「等待中」的排程：fn 在途时它落地后自会重排，
    // 此时再启新 loop 会分裂出第二条定时链——两链各自续命，请求量翻倍。
    it.kick = () => { if (!it.running) { clearTimeout(it.timer); loop(); } };
    it.reset = () => { if (!it.running) { clearTimeout(it.timer); it.timer = setTimeout(loop, wait()); } };
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

export function parseHash() {
  const raw = location.hash.slice(1);
  const [tab, ...rest] = raw.split('&');
  return { tab: tab || 'overview', params: new URLSearchParams(rest.join('&')) };
}
// writeHash 保留 tab 段，重写过滤参数段。
export function writeHash(tab, params) {
  const s = params.toString();
  history.replaceState(null, '', location.pathname + '#' + tab + (s ? '&' + s : ''));
}
window.addEventListener('hashchange', () => {
  const h = parseHash();
  if (h.tab !== Tabs.current) Tabs.apply(h.tab);
});
