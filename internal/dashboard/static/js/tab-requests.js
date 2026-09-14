// 请求页：进行中请求表 + 筛选列表 + 行内详情 + 文件查看 + 导出 + 中断。
// 渲染模型：模块态（lastActive/lastList/expandedDir/detail/openFile/fileText）
// → 整棵 tbody HTML → morph 增量更新。展开详情与文件视图都是状态而非 DOM，
// 轮询重建不再需要 innerHTML 移植 hack；fillDetail 的错行竞态由
// 「detail.dir === expandedDir」同源校验天然消解。
// activeTable 同时被概览页复用渲染在途快照。
// 轮询节奏跟随活跃度：有进行中请求 1s/轮，空闲 5s。

import {
  $, api, apiRaw, esc, debounce, fmtMs, fmtBytes, fmtNum, fmtTime,
  secClass, statusClass, resultBadge, copyText, confirmBox, toast,
  titleBadge, Tabs, Polls, morph, parseHash, writeHash, REJECT_LABELS,
} from './core.js';

let expandedDir = null;  // 展开详情的 dir；详情行随 tbody 一起由 render 产出
let openFile = null;     // {dir, name, merged} 打开中的文件视图；打开时暂停列表自动刷新
let fileText = '';       // 文件视图正文（渲染态文本）
let detail = null;       // /requests/{dir} 响应；detail.dir 需等于 expandedDir 才渲染
let detailErr = null;    // error.json 解析出的首个失败点
let reqLimit = 100;
let lastActive = [];
let lastList = [];
let prevDirs = null;     // 上轮渲染出的 dir 集（null=首轮/刚重置，不做新行闪显）

const FILTER_IDS = ['reqSearch', 'fStatus', 'fResult', 'fReqModel', 'fErrStage', 'fSince', 'fSinceTS', 'fUntilTS'];
const STATE_LABEL = { waiting_upstream: '等上游', receiving_upstream: '收上游', streaming_client: '发客户端' };

// ---------- 静默检测 ----------
// ActiveRequest 没有 last_activity 字段：跨轮询比较 client_bytes 增量，
// 10 分钟无新字节流出即标「静默」——等上游超时/上游挂死都会呈现这个形态。
const SILENCE_MS = 10 * 60000;
const silence = {}; // dir -> {bytes, t: 最后一次字节变化时刻}
function annotateSilence(list) {
  const seen = {};
  list.forEach(a => {
    const s = silence[a.dir] || (silence[a.dir] = { bytes: 0, t: Date.now() });
    if ((a.client_bytes || 0) > s.bytes) { s.bytes = a.client_bytes; s.t = Date.now(); }
    a.silent_ms = Date.now() - s.t;
    seen[a.dir] = 1;
  });
  for (const k in silence) if (!seen[k]) delete silence[k];
}
function silentTag(a) {
  return (a.silent_ms || 0) >= SILENCE_MS
    ? ' <span class="badge badge-high" title="≥10 分钟无字节流出，可能上游挂死或对端僵住">静默 ' + Math.floor(a.silent_ms / 60000) + 'm</span>'
    : '';
}

// ---------- 行模板 ----------
function pendingRowHtml(a) {
  return '<tr class="pending-row" id="p-' + esc(a.dir) + '" data-dir="' + esc(a.dir) + '">' +
    '<td><span class="pulse-dot"></span><span class="mono">' + esc(a.dir) + '</span></td>' +
    '<td>' + esc(a.meta && a.meta.api || '-') + '</td>' +
    '<td><span class="rbadge r-muted">' + esc(STATE_LABEL[a.state] || a.state || '进行中') + '</span>' + silentTag(a) + '</td>' +
    '<td class="mono">' + esc(a.model || '-') + '</td>' +
    '<td class="mono">' + fmtMs(a.elapsed_ms) + '</td>' +
    '<td class="mono">' + fmtMs(a.first_upstream_ms) + '</td>' +
    '<td class="mono muted">已下发 ' + fmtBytes(a.client_bytes) + '</td>' +
    '<td>' + (a.abortable ? '<button type="button" class="file-link" data-abort="' + esc(a.dir) + '">中断</button>' : '') + '</td></tr>';
}

function rowHtml(e) {
  const resolved = (e.model && e.model !== e.requested_model) ? ' → ' + esc(e.model) : '';
  const mismatch = e.model_mismatch ? ' <span class="badge badge-high">错配</span>' : '';
  const premature = e.premature_end_turn ? ' <span class="badge badge-medium" title="工具结果之后模型直接 end_turn，未继续调用工具">早停</span>' : '';
  const stream = e.stream ? ' <span class="badge badge-sse">SSE</span>' : '';
  const stage = e.error_stage ? '<div><span class="badge badge-high">' + esc(e.error_stage) + '</span></div>' : '';
  // 流内下发的限流 HTTP 状态仍是 200——rate_limited 标记是唯一能认出它的字段。
  const rl = e.rate_limited && e.status_code !== 429 ? ' <span class="badge badge-medium" title="限流语义经流内错误事件下发（HTTP 200 + error event）">流内429</span>' : '';
  const retry = e.retries ? ' <span class="badge badge-sse" title="上游重发 ' + e.retries + ' 次（attempt 文件与 retry_attempt 分界行见详情）">重试' + e.retries + '</span>' : '';
  const cache = e.cache_read_tokens ? '<div class="muted">缓存读 ' + fmtNum(e.cache_read_tokens) + '</div>' : '';
  const keyh = e.key_hash ? '<div class="muted" title="key hash">' + esc(e.key_hash) + '</div>' : '';
  return '<tr id="r-' + esc(e.dir) + '" data-dir="' + esc(e.dir) + '">' +
    '<td class="mono" title="' + esc(e.started_at || '') + '">' + fmtTime(e.started_at) + '</td>' +
    '<td>' + esc(e.api || '-') + '</td>' +
    '<td><span class="' + statusClass(e.status_code) + ' mono">' + e.status_code + '</span>' + resultBadge(e.result) + stream + retry + rl + stage + '</td>' +
    '<td>' + esc(e.requested_model || '-') + resolved + mismatch + premature + '</td>' +
    '<td class="mono ' + secClass(e.duration_ms, 30000, 60000) + '">' + fmtMs(e.duration_ms) + '</td>' +
    '<td class="mono ' + secClass(e.first_upstream_ms, 5000, 10000) + '">' + fmtMs(e.first_upstream_ms) + '</td>' +
    '<td class="mono">↓' + fmtNum(e.input_tokens) + ' ↑' + fmtNum(e.output_tokens) + cache + '</td>' +
    '<td class="mono muted">' + esc(e.client_ip || '') + keyh + '</td></tr>';
}

function detailRowHtml() {
  const inner = (detail && detail.dir === expandedDir) ? detailInnerHtml(detail) : '<div class="loading sm">加载中...</div>';
  return '<tr class="detail-row" id="d-' + esc(expandedDir) + '"><td colspan="8">' + inner + '</td></tr>';
}

// 详情内容：失败请求的 error.json 提到最上方；上游重发链路次之；meta 表格、
// 文件清单、文件视图依次排开。openFile/fileText 决定文件视图的显隐与内容。
function detailInnerHtml(d) {
  const m = d.meta || {};
  let html = '';
  if (detailErr) {
    html += '<div class="err-banner">失败阶段 ' + esc(detailErr.stage || '-') + ' · ' + esc(detailErr.message || '') + ' · +' + fmtMs(detailErr.elapsed_ms) + '</div>';
  } else if (m.status_code >= 400 || m.result === 'failed') {
    html += '<div class="err-banner">' + esc((m.status_code || '') + ' ' + (m.result || '')) + '</div>';
  }
  if (m.retry_attempts && m.retry_attempts.length) {
    const hops = ['<strong>attempt 1</strong>'];
    m.retry_attempts.forEach(a => {
      hops.push('attempt ' + a.attempt + '<span class="muted">（' + esc(a.cause || '-') + ' · +' + fmtMs(a.elapsed_ms) + '）</span>');
    });
    html += '<div class="retry-chain">上游重发链路：' + hops.join(' → ') + '</div>';
  }
  html += '<div class="meta-grid">';
  [['目录', d.dir], ['API', m.api], ['路径', (m.method || '') + ' ' + (m.path || '')], ['状态', (m.status_code || '-') + ' ' + (m.result || '')],
  ['流式', m.stream === true ? 'SSE' : m.stream === false ? '否' : null], ['提供方', m.provider],
  ['请求模型', m.requested_model], ['实际模型', m.model], ['响应模型', m.response_model],
  ['模型错配', m.model_mismatch ? '是' : null], ['可疑早停', m.premature_end_turn ? '工具结果后纯文本 end_turn' : null],
  ['开始', m.started_at], ['完成', m.finished_at], ['耗时', fmtMs(m.duration_ms)], ['上游TTFB', fmtMs(m.first_upstream_ms)], ['客户端TTFB', fmtMs(m.first_client_ms)],
  ['上游请求ID', m.upstream_request_id], ['限流标记', m.rate_limited ? '是（含流内下发）' : null], ['重试等待', m.retry_after_seconds != null ? m.retry_after_seconds + 's' : null], ['客户端IP', m.client && m.client.ip], ['UA', m.client && m.client.user_agent], ['Key哈希', m.client && m.client.key_hash],
  ['客户端请求ID', m.client && m.client.request_id],
  ['Tokens', m.usage ? (m.usage.input + ' in / ' + m.usage.output + ' out / ' + m.usage.cache_read + ' cached') : null],
  ['丢弃事件', m.dropped_events]].forEach(kv => {
    if (kv[1] == null || kv[1] === '') return;
    html += '<div><span class="k">' + esc(kv[0]) + '</span> <span class="v">' + esc(String(kv[1])) + '</span></div>';
  });
  html += '</div><div class="file-list">';
  html += '<button type="button" class="file-link" data-copydir="' + esc(d.dir) + '" title="dir 即响应头 X-Request-Id">复制 dir</button>';
  (d.files || []).forEach(f => {
    const on = openFile && openFile.dir === d.dir && openFile.name === f.name && !openFile.merged;
    html += '<button type="button" class="file-link' + (on ? ' on' : '') + '" data-f="' + esc(f.name) + '" data-dir="' + esc(d.dir) + '">' + esc(f.name) + ' <span class="muted">' + fmtBytes(f.size) + '</span></button>';
    if (f.name === '06-http-response.jsonl') {
      html += '<button type="button" class="file-link ok' + (openFile && openFile.merged ? ' on' : '') + '" data-merged="' + esc(d.dir) + '">合并视图</button>';
    }
  });
  html += '</div>';
  if (openFile) html += '<div class="file-view">' + esc(fileText) + '</div>';
  if (!d.meta) html += '<div class="note">meta.json 缺失或已损坏</div>';
  return html;
}

// render 由模块态整建 tbody 字符串并 morph 增量更新：
// 未变行不动（hover/选中保留），新增行打 row-new 闪显。
function render() {
  const tbody = $('reqBody');
  const dirs = new Set();
  lastActive.forEach(a => dirs.add(a.dir));
  lastList.forEach(e => dirs.add(e.dir));
  // 展开的行从当前数据消失（请求完成且被过滤条件挡在列表外）→ 收起，
  // 避免挂着无宿主行的幽灵详情。
  if (expandedDir && !dirs.has(expandedDir)) {
    expandedDir = null; detail = null; detailErr = null; openFile = null; fileText = '';
  }
  let html = '';
  lastActive.forEach(a => {
    html += pendingRowHtml(a);
    if (expandedDir === a.dir) html += detailRowHtml();
  });
  lastList.forEach(e => {
    html += rowHtml(e);
    if (expandedDir === e.dir) html += detailRowHtml();
  });
  if (!html) html = '<tr><td colspan="8" class="loading">暂无请求记录</td></tr>';
  morph(tbody, html);
  if (prevDirs) {
    tbody.querySelectorAll('tr[data-dir]').forEach(tr => {
      if (!prevDirs.has(tr.dataset.dir)) tr.classList.add('row-new');
    });
  }
  prevDirs = dirs;
}

// ---------- 进行中请求 ----------
// activeTable 返回完整表格 HTML，概览页在途快照与本页 pending 行共用渲染口径。
export function activeTable(list) {
  annotateSilence(list);
  let html = '<table><thead><tr><th>目录</th><th>API</th><th>模型</th><th>阶段</th><th>已耗时</th><th>上游TTFB</th><th>已下发</th><th>队列/丢弃</th><th></th></tr></thead><tbody>';
  list.forEach(a => {
    html += '<tr><td class="mono">' + esc(a.dir) + '</td><td>' + esc(a.meta && a.meta.api || '-') + '</td>' +
      '<td class="mono">' + esc(a.model || '-') + '</td>' +
      '<td>' + esc(STATE_LABEL[a.state] || a.state || '-') + silentTag(a) + '</td>' +
      '<td class="mono">' + fmtMs(a.elapsed_ms) + '</td>' +
      '<td class="mono">' + fmtMs(a.first_upstream_ms) + '</td>' +
      '<td class="mono">' + fmtBytes(a.client_bytes) + '</td>' +
      '<td class="mono">' + (a.queued_events || 0) + ' / ' + (a.dropped_events || 0) + '</td>' +
      '<td>' + (a.abortable ? '<button type="button" class="file-link" data-abort="' + esc(a.dir) + '">中断</button>' : '') + '</td></tr>';
  });
  return html + '</tbody></table>';
}

async function loadActive() {
  try {
    const d = await api('/requests/active');
    lastActive = d.active || [];
    annotateSilence(lastActive);
    titleBadge(lastActive.length);
  } catch (e) { /* 静默 */ }
}

async function abort(dir) {
  if (!await confirmBox('中断请求', dir + ' — 上游与客户端连接都会被取消。', true)) return;
  try {
    const res = await apiRaw('/requests/' + encodeURIComponent(dir) + '/abort', { method: 'POST' });
    if (res.ok) { toast('已中断 ' + dir, 'ok'); load(); } else { toast('中断失败：' + await res.text(), 'err'); }
  } catch (e) { toast('中断失败：' + e, 'err'); }
}

// ---------- 筛选与列表 ----------
// 过滤器状态同步到 location.hash（#requests&model=x）：刷新/分享链接后现场不丢。
function saveFilterHash() {
  if (Tabs.current !== 'requests') return;
  const p = new URLSearchParams();
  FILTER_IDS.forEach(id => { const el = $(id); if (el && el.value) p.set(id, el.value); });
  writeHash('requests', p);
}
function restoreFilterHash() {
  const h = parseHash();
  FILTER_IDS.forEach(id => { const v = h.params.get(id); if (v) { const el = $(id); if (el) el.value = v; } });
  // #requests&dir=X 深链：直接展开该请求详情（对应同类面板的渠道深链）。
  const dir = h.params.get('dir');
  if (dir) expandedDir = dir;
}

function reqQuery() {
  const p = new URLSearchParams();
  const q = $('reqSearch').value.trim(); if (q) p.set('q', q);
  const sc = $('fStatus').value.trim(); if (sc) p.set('status', sc);
  const rs = $('fResult').value; if (rs) p.set('result', rs);
  const md = $('fReqModel').value.trim(); if (md) p.set('model', md);
  const es = $('fErrStage').value.trim(); if (es) p.set('error_stage', es);
  const since = $('fSince').value;
  if (since) {
    const ms = { '1h': 36e5, '24h': 864e5, '7d': 6048e5 }[since] || 0;
    if (ms) p.set('since', new Date(Date.now() - ms).toISOString());
  }
  // 隐藏 ISO 窗字段由矩阵下钻写入，存在即优先于下拉相对窗。
  const sts = $('fSinceTS').value.trim(), uts = $('fUntilTS').value.trim();
  if (sts) p.set('since', sts);
  if (uts) p.set('until', uts);
  return p;
}

async function load() {
  try {
    const p = reqQuery(); p.set('limit', reqLimit);
    saveFilterHash();
    const data = await api('/requests?' + p.toString());
    const reqCount = $('reqCount'), moreBtn = $('reqMore');
    if (data.disabled) {
      lastList = []; prevDirs = null;
      morph($('reqBody'), '<tr><td colspan="8" class="loading">调试日志未启用（config: debug.enabled）</td></tr>');
      reqCount.textContent = ''; moreBtn.style.display = 'none'; return;
    }
    lastList = data.requests || [];
    render();
    reqCount.textContent = '显示 ' + lastList.length + ' / 命中 ' + (data.total ?? lastList.length) + ' 条' +
      ' · 更新于 ' + new Date().toLocaleTimeString('zh-CN', { hour12: false });
    moreBtn.style.display = (lastList.length < data.total && reqLimit < 500) ? '' : 'none';
    let hint = '';
    // 管线前拒绝（排空/并发/鉴权）不进 index——用户在请求页找这类 503/429
    // 天然扑空，看到提示才知道去系统页查拒绝事件环。
    const recentRejects = (data.rejects && data.rejects.recent || []).filter(e => e.at * 1000 > Date.now() - 15 * 60000);
    if (recentRejects.length) {
      const byReason = {};
      recentRejects.forEach(e => { byReason[e.reason] = (byReason[e.reason] || 0) + 1; });
      const parts = Object.keys(byReason).map(k => (REJECT_LABELS[k] || k) + ' ' + byReason[k]);
      hint += '近 15 分钟本地拒绝 ' + recentRejects.length + ' 条（' + esc(parts.join(' · ')) + '）——管线前拒绝不进索引，<button type="button" class="lnk" data-gotosys="1">去系统页</button>。 ';
    }
    const sts = $('fSinceTS').value.trim(), uts = $('fUntilTS').value.trim();
    if (sts || uts) {
      hint = '时间窗锁定 ' + (sts ? fmtTime(sts) : '最早') + ' ~ ' + (uts ? fmtTime(uts) : '现在') +
        '（矩阵下钻）· <button type="button" class="lnk" data-clrwin="1">清除窗口</button> ';
    }
    if (data.has_more) hint += '更早历史在扫描窗口之外，可缩小筛选或 grep index.jsonl。';
    if (reqLimit >= 500 && lastList.length < data.total) hint += ' 已达 500 条单页上限，用导出查看全部。';
    const hintEl = $('reqHint');
    if (hint) { hintEl.style.display = ''; hintEl.innerHTML = hint; } else { hintEl.style.display = 'none'; }
    if (expandedDir) fillDetail(expandedDir);
  } catch (e) {
    const h = $('reqHint');
    h.style.display = ''; h.textContent = '请求列表刷新失败：' + String(e) + '（保留旧数据，下轮自动重试）';
  }
}

// 打开文件时列表自动刷新暂停（避免详情 DOM 被重建）。
function tick() {
  if (openFile) {
    const h = $('reqHint');
    h.style.display = ''; h.textContent = '正在查看文件，自动刷新已暂停（再点一次文件名或收起详情后恢复）。';
    return;
  }
  // 先拉在途再渲染列表：pending 行用本轮数据，不滞后一个周期。
  loadActive().then(load);
}

export function resetAndLoad() { reqLimit = 100; prevDirs = null; tick(); }

// ---------- 行内详情 ----------
function toggleDetail(dir) {
  if (expandedDir === dir) {
    expandedDir = null; detail = null; detailErr = null; openFile = null; fileText = '';
    render(); return;
  }
  expandedDir = dir;
  detail = null; detailErr = null; openFile = null; fileText = '';
  render();
  fillDetail(dir);
}

async function fillDetail(dir) {
  try {
    const d = await api('/requests/' + encodeURIComponent(dir));
    // 用户已收起或换行——晚到的响应直接丢弃，不污染新展开行的占位。
    if (dir !== expandedDir) return;
    detail = d;
    detailErr = null;
    // error.json 记的是首个失败点，提到最上方比埋在 meta 表格里更先被看到。
    if ((d.files || []).some(f => f.name === 'error.json')) {
      try { detailErr = JSON.parse((await api('/requests/' + encodeURIComponent(dir) + '/file/error.json')).text); } catch (e) {}
    }
    render();
  } catch (e) { /* 详情拉取失败：保留旧渲染，下轮重试 */ }
}

async function openFileView(dir, name, merged) {
  // 再点同一个文件名 = 关闭查看区，恢复列表自动刷新。
  if (openFile && openFile.dir === dir && openFile.name === name && !openFile.merged && !merged) {
    openFile = null; fileText = ''; render(); return;
  }
  const mine = { dir, name, merged: !!merged };
  openFile = mine;
  fileText = merged ? '合并 06-http-response.jsonl ...' : '加载 ' + name + ' ...';
  render();
  try {
    let text;
    if (merged) {
      const d = await api('/requests/' + encodeURIComponent(dir) + '/merged');
      text = '== 正文 ==\n' + (d.text || '(空)');
      if (d.reasoning) text += '\n\n== 推理 ==\n' + d.reasoning;
      if (d.tool_input) text += '\n\n== 工具调用参数 ==\n' + d.tool_input;
      if (d.usage) text += '\n\n== usage ==\n' + JSON.stringify(JSON.parse(d.usage), null, 2);
      text += '\n\n— 合并自 ' + d.events + ' 帧' + (d.finish_reason ? (' · finish=' + d.finish_reason) : '');
    } else {
      const d = await api('/requests/' + encodeURIComponent(dir) + '/file/' + name.split('/').map(encodeURIComponent).join('/'));
      text = d.text || '';
      if (name.endsWith('.json')) {
        try { text = JSON.stringify(JSON.parse(text), null, 2); } catch (e) {}
      } else if (name.endsWith('.jsonl')) {
        text = text.split('\n').filter(Boolean).map(line => {
          try {
            const o = JSON.parse(line);
            const head = (o.seq ? '#' + o.seq + ' ' : '') + (o.elapsed_ms != null ? '+' + o.elapsed_ms + 'ms ' : '') + (o.event || '');
            return head + '  ' + JSON.stringify(o.data !== undefined ? o.data : o, null, 0).slice(0, 2000);
          } catch (e) { return line; }
        }).join('\n\n');
      }
      if (d.truncated) text += '\n\n... 已截断（原始 ' + d.size + ' 字节）';
    }
    // 期间用户换了文件/收起详情——晚到的内容直接丢弃。
    if (openFile !== mine) return;
    fileText = text;
    render();
  } catch (e) {
    if (openFile !== mine) return;
    fileText = '读取失败: ' + String(e);
    render();
  }
}

// 跨页联动：把条件填进本页过滤器并切过来（各 tab 的表格/chips 调用）。
// since/until 是隐藏时间窗字段（矩阵格子下钻用 ISO 时刻钉窗口）：
// 不带窗参数的跳转总是清掉旧锁，否则一次下钻后所有跳转都被钉住。
export function jumpRequests(kv) {
  const map = { q: 'reqSearch', status: 'fStatus', result: 'fResult', model: 'fReqModel', error_stage: 'fErrStage', since: 'fSinceTS', until: 'fUntilTS' };
  for (const k in kv) { const el = $(map[k]); if (el) el.value = kv[k]; }
  if (!('since' in kv)) { const el = $('fSinceTS'); if (el) el.value = ''; }
  if (!('until' in kv)) { const el = $('fUntilTS'); if (el) el.value = ''; }
  Tabs.go('requests');
  resetAndLoad();
}

// ---------- 事件委托与注册 ----------
function bind() {
  $('reqSearch').addEventListener('input', debounce(resetAndLoad, 300));
  $('fStatus').addEventListener('input', debounce(resetAndLoad, 300));
  $('fReqModel').addEventListener('input', debounce(resetAndLoad, 300));
  $('fErrStage').addEventListener('input', debounce(resetAndLoad, 300));
  $('fResult').addEventListener('change', resetAndLoad);
  $('fSince').addEventListener('change', resetAndLoad);
  $('reqMore').addEventListener('click', () => { reqLimit = Math.min(500, reqLimit + 100); load(); });
  $('reqExportJson').addEventListener('click', () => exportReq('json'));
  $('reqExportCsv').addEventListener('click', () => exportReq('csv'));
  document.getElementById('page-requests').addEventListener('click', e => {
    // 中断按钮在 document 级处理（概览页也用）——此处提前放行，
    // 否则中断按钮落在 tr[data-dir] 内会先触发展开详情。
    if (e.target.closest('[data-abort]')) return;
    const cp = e.target.closest('[data-copydir]');
    if (cp) { copyText(cp.dataset.copydir, '已复制 ' + cp.dataset.copydir); return; }
    const mg = e.target.closest('[data-merged]');
    if (mg) { openFileView(mg.dataset.merged, '06-http-response.jsonl', true); return; }
    const fl = e.target.closest('.file-link[data-f]');
    if (fl) { openFileView(fl.dataset.dir, fl.dataset.f, false); return; }
    const cw = e.target.closest('[data-clrwin]');
    if (cw) { $('fSinceTS').value = ''; $('fUntilTS').value = ''; resetAndLoad(); return; }
    const gs = e.target.closest('[data-gotosys]');
    if (gs) { Tabs.go('system'); return; }
    const tr = e.target.closest('.req-table tbody tr[data-dir]');
    if (tr) toggleDetail(tr.dataset.dir);
  });
  // 中断按钮挂 document 级：概览页在途表（activeTable 复用）与本页
  // pending 行都会渲染它，页面级委托够不着概览。
  document.addEventListener('click', e => {
    const ab = e.target.closest('[data-abort]');
    if (ab) { e.stopPropagation(); abort(ab.dataset.abort); }
  });
}
function exportReq(fmt) {
  const p = reqQuery(); p.set('format', fmt);
  window.open('/panel/api/requests/export?' + p.toString(), '_blank');
}

bind();
Tabs.register('requests', () => { restoreFilterHash(); tick(); });
Polls.add('requests', tick, () => lastActive.length ? 1000 : 5000);
