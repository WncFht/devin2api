// 请求页：进行中请求表 + 筛选列表 + 行内详情 + 文件查看 + 导出 + 中断。
// activeTable 同时被概览页复用渲染在途快照。

const Requests = (() => {
  let expandedDir = null;
  let openFile = null;
  let reqLimit = 100;

  const FILTER_IDS = ['reqSearch', 'fStatusClass', 'fResult', 'fReqModel', 'fErrStage', 'fSince'];

  // ---------- 进行中请求 ----------
  function activeTable(list) {
    const stateMap = { waiting_upstream: '等上游', receiving_upstream: '收上游', streaming_client: '发客户端' };
    let html = '<table><thead><tr><th>目录</th><th>API</th><th>模型</th><th>阶段</th><th>已耗时</th><th>上游TTFB</th><th>已下发</th><th>队列/丢弃</th><th></th></tr></thead><tbody>';
    list.forEach(a => {
      html += '<tr><td class="mono">' + esc(a.dir) + '</td><td>' + esc(a.meta && a.meta.api || '-') + '</td>' +
        '<td class="mono">' + esc(a.model || '-') + '</td>' +
        '<td>' + esc(stateMap[a.state] || a.state || '-') + '</td>' +
        '<td class="mono">' + fmtMs(a.elapsed_ms) + '</td>' +
        '<td class="mono">' + fmtMs(a.first_upstream_ms) + '</td>' +
        '<td class="mono">' + fmtBytes(a.client_bytes) + '</td>' +
        '<td class="mono">' + (a.queued_events || 0) + ' / ' + (a.dropped_events || 0) + '</td>' +
        '<td>' + (a.abortable ? '<span class="file-link" data-abort="' + qa(a.dir) + '">中断</span>' : '') + '</td></tr>';
    });
    return html + '</tbody></table>';
  }

  async function loadActive() {
    try {
      const d = await api('/requests/active');
      const list = d.active || [];
      const panel = $('rqActivePanel');
      if (!list.length) { panel.style.display = 'none'; return; }
      panel.style.display = '';
      $('rqActiveBody').innerHTML = activeTable(list);
    } catch (e) { /* 静默 */ }
  }

  async function abort(dir) {
    if (!confirm('中断请求 ' + dir + '？上游与客户端连接都会被取消。')) return;
    try {
      const res = await fetch('/panel/api/requests/' + encodeURIComponent(dir) + '/abort', { method: 'POST' });
      if (res.ok) { loadActive(); } else { alert('中断失败：' + await res.text()); }
    } catch (e) { alert('中断失败：' + e); }
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
  }

  function reqQuery() {
    const p = new URLSearchParams();
    const q = $('reqSearch').value.trim(); if (q) p.set('q', q);
    const sc = $('fStatusClass').value; if (sc) p.set('status_class', sc);
    const rs = $('fResult').value; if (rs) p.set('result', rs);
    const md = $('fReqModel').value.trim(); if (md) p.set('model', md);
    const es = $('fErrStage').value.trim(); if (es) p.set('error_stage', es);
    const since = $('fSince').value;
    if (since) {
      const ms = { '1h': 36e5, '24h': 864e5, '7d': 6048e5 }[since] || 0;
      if (ms) p.set('since', new Date(Date.now() - ms).toISOString());
    }
    return p;
  }

  async function load() {
    try {
      const p = reqQuery(); p.set('limit', reqLimit);
      saveFilterHash();
      const data = await api('/requests?' + p.toString());
      const tbody = $('reqBody');
      const list = data.requests || [];
      const reqCount = $('reqCount'), moreBtn = $('reqMore');
      if (data.disabled) {
        tbody.innerHTML = '<tr><td colspan="8" class="loading">调试日志未启用（config: debug.enabled）</td></tr>';
        reqCount.textContent = ''; moreBtn.style.display = 'none'; return;
      }
      if (!list.length) {
        tbody.innerHTML = '<tr><td colspan="8" class="loading">暂无请求记录</td></tr>';
        reqCount.textContent = '命中 0 条'; moreBtn.style.display = 'none'; return;
      }
      let html = '';
      list.forEach(e => {
        const resolved = (e.model && e.model !== e.requested_model) ? ' → ' + esc(e.model) : '';
        const mismatch = e.model_mismatch ? ' <span class="badge badge-high">错配</span>' : '';
        const premature = e.premature_end_turn ? ' <span class="badge badge-medium" title="工具结果之后模型直接 end_turn，未继续调用工具">早停</span>' : '';
        const stream = e.stream ? ' <span class="badge badge-sse">SSE</span>' : '';
        const stage = e.error_stage ? '<div><span class="badge badge-high">' + esc(e.error_stage) + '</span></div>' : '';
        const cache = e.cache_read_tokens ? '<div class="muted">缓存读 ' + fmtNum(e.cache_read_tokens) + '</div>' : '';
        const keyh = e.key_hash ? '<div class="muted" title="key hash">' + esc(e.key_hash) + '</div>' : '';
        html += '<tr data-dir="' + qa(e.dir) + '">' +
          '<td class="mono">' + fmtTime(e.started_at) + '</td>' +
          '<td>' + esc(e.api || '-') + '</td>' +
          '<td class="' + statusClass(e.status_code) + '">' + e.status_code + ' ' + esc(e.result || '') + stream + stage + '</td>' +
          '<td>' + esc(e.requested_model || '-') + resolved + mismatch + premature + '</td>' +
          '<td class="mono">' + fmtMs(e.duration_ms) + '</td>' +
          '<td class="mono">' + fmtMs(e.first_upstream_ms) + '</td>' +
          '<td class="mono">' + fmtNum(e.input_tokens) + '/' + fmtNum(e.output_tokens) + cache + '</td>' +
          '<td class="mono muted">' + esc(e.client_ip || '') + keyh + '</td></tr>';
        if (expandedDir === e.dir) {
          html += '<tr class="detail-row"><td colspan="8"><div class="loading" style="padding:8px">加载中...</div></td></tr>';
        }
      });
      tbody.innerHTML = html;
      reqCount.textContent = '显示 ' + list.length + ' / 命中 ' + (data.total ?? list.length) + ' 条';
      moreBtn.style.display = (list.length < data.total && reqLimit < 500) ? '' : 'none';
      let hint = '';
      if (data.has_more) hint = '更早历史在扫描窗口之外，可缩小筛选或 grep index.jsonl。';
      if (reqLimit >= 500 && list.length < data.total) hint += ' 已达 500 条单页上限，用导出查看全部。';
      const hintEl = $('reqHint');
      if (hint) { hintEl.style.display = ''; hintEl.textContent = hint; } else { hintEl.style.display = 'none'; }
      if (expandedDir) fillDetail(expandedDir);
    } catch (e) {
      const h = $('reqHint');
      h.style.display = ''; h.textContent = '请求列表刷新失败：' + String(e) + '（保留旧数据，10s 后重试）';
    }
  }

  // 打开文件时列表自动刷新暂停（避免详情 DOM 被重建）。
  function tick() {
    if (openFile) {
      const h = $('reqHint');
      h.style.display = ''; h.textContent = '正在查看文件，自动刷新已暂停（再点一次文件名或收起详情后恢复）。';
      return;
    }
    load(); loadActive();
  }

  function resetAndLoad() { reqLimit = 100; tick(); }

  // ---------- 行内详情 ----------
  function toggleDetail(dir) {
    if (expandedDir === dir) { expandedDir = null; openFile = null; load(); return; }
    expandedDir = dir;
    load();
  }

  async function fillDetail(dir) {
    const row = document.querySelector('.detail-row td');
    if (!row) return;
    try {
      const d = await api('/requests/' + encodeURIComponent(dir));
      const m = d.meta || {};
      // 失败请求：error.json 记的是首个失败点，提到最上方比埋在 meta 表格里更先被看到。
      let banner = '';
      if ((d.files || []).some(f => f.name === 'error.json')) {
        try {
          const er = await fetch('/panel/api/requests/' + encodeURIComponent(dir) + '/file/error.json');
          const eo = JSON.parse((await er.json()).text);
          banner = '<div class="err-banner">失败阶段 ' + esc(eo.stage || '-') + ' · ' + esc(eo.message || '') + ' · +' + fmtMs(eo.elapsed_ms) + '</div>';
        } catch (e) {}
      }
      if (!banner && (m.status_code >= 400 || m.result === 'failed')) {
        banner = '<div class="err-banner">' + esc((m.status_code || '') + ' ' + (m.result || '')) + '</div>';
      }
      let html = banner + '<div class="meta-grid">';
      [['目录', d.dir], ['API', m.api], ['路径', (m.method || '') + ' ' + (m.path || '')], ['状态', (m.status_code || '-') + ' ' + (m.result || '')],
      ['流式', m.stream === true ? 'SSE' : m.stream === false ? '否' : null], ['提供方', m.provider],
      ['请求模型', m.requested_model], ['实际模型', m.model], ['响应模型', m.response_model],
      ['模型错配', m.model_mismatch ? '是' : null], ['可疑早停', m.premature_end_turn ? '工具结果后纯文本 end_turn' : null],
      ['开始', m.started_at], ['完成', m.finished_at], ['耗时', fmtMs(m.duration_ms)], ['上游TTFB', fmtMs(m.first_upstream_ms)], ['客户端TTFB', fmtMs(m.first_client_ms)],
      ['上游请求ID', m.upstream_request_id], ['客户端IP', m.client && m.client.ip], ['UA', m.client && m.client.user_agent], ['Key哈希', m.client && m.client.key_hash],
      ['客户端请求ID', m.client && m.client.request_id],
      ['Tokens', m.usage ? (m.usage.input + ' in / ' + m.usage.output + ' out / ' + m.usage.cache_read + ' cached') : null],
      ['丢弃事件', m.dropped_events]].forEach(kv => {
        if (kv[1] == null || kv[1] === '') return;
        html += '<div><span class="k">' + esc(kv[0]) + '</span> <span class="v">' + esc(String(kv[1])) + '</span></div>';
      });
      html += '</div><div class="file-list">';
      html += '<span class="file-link" data-copydir="' + qa(d.dir) + '" title="dir 即响应头 X-Request-Id">复制 dir</span>';
      (d.files || []).forEach(f => {
        html += '<span class="file-link" data-f="' + esc(f.name) + '" data-dir="' + qa(d.dir) + '">' + esc(f.name) + ' <span class="muted">' + fmtBytes(f.size) + '</span></span>';
        if (f.name === '06-http-response.jsonl') {
          html += '<span class="file-link" style="border-color:#34d399" data-merged="' + qa(d.dir) + '">合并视图</span>';
        }
      });
      html += '</div><div class="file-view" id="fileView" style="display:none"></div>';
      if (!d.meta) html += '<div class="note">meta.json 缺失或已损坏</div>';
      row.innerHTML = html;
      if (openFile && openFile.dir === d.dir) {
        const o = openFile; openFile = null;
        o.merged ? loadMerged(d.dir) : loadFile(d.dir, o.name);
      }
    } catch (e) { row.innerHTML = '<div class="note">详情拉取失败: ' + esc(String(e)) + '</div>'; }
  }

  function markFile(name) {
    document.querySelectorAll('.detail-row .file-link[data-f]').forEach(x => x.classList.toggle('on', x.dataset.f === name));
  }

  async function loadFile(dir, name) {
    const view = $('fileView');
    if (!view) return;
    // 再点同一个文件名 = 关闭查看区，恢复列表自动刷新。
    if (openFile && openFile.dir === dir && openFile.name === name && !openFile.merged) {
      openFile = null; markFile(null); view.style.display = 'none'; view.textContent = ''; return;
    }
    openFile = { dir, name };
    markFile(name);
    view.style.display = 'block';
    view.textContent = '加载 ' + name + ' ...';
    try {
      const d = await api('/requests/' + encodeURIComponent(dir) + '/file/' + name.split('/').map(encodeURIComponent).join('/'));
      let text = d.text || '';
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
      view.textContent = text + (d.truncated ? '\n\n... 已截断（原始 ' + d.size + ' 字节）' : '');
    } catch (e) { view.textContent = '读取失败: ' + String(e); }
  }

  async function loadMerged(dir) {
    const view = $('fileView');
    if (!view) return;
    openFile = { dir, name: '06-http-response.jsonl', merged: true };
    markFile('06-http-response.jsonl');
    view.style.display = 'block';
    view.textContent = '合并 06-http-response.jsonl ...';
    try {
      const d = await api('/requests/' + encodeURIComponent(dir) + '/merged');
      let out = '== 正文 ==\n' + (d.text || '(空)');
      if (d.reasoning) out += '\n\n== 推理 ==\n' + d.reasoning;
      if (d.tool_input) out += '\n\n== 工具调用参数 ==\n' + d.tool_input;
      if (d.usage) out += '\n\n== usage ==\n' + JSON.stringify(JSON.parse(d.usage), null, 2);
      out += '\n\n— 合并自 ' + d.events + ' 帧' + (d.finish_reason ? (' · finish=' + d.finish_reason) : '');
      view.textContent = out;
    } catch (e) { view.textContent = '合并失败: ' + String(e); }
  }

  // ---------- 事件委托与注册 ----------
  function bind() {
    $('reqSearch').addEventListener('input', debounce(resetAndLoad, 300));
    $('fReqModel').addEventListener('input', debounce(resetAndLoad, 300));
    $('fErrStage').addEventListener('input', debounce(resetAndLoad, 300));
    $('fStatusClass').addEventListener('change', resetAndLoad);
    $('fResult').addEventListener('change', resetAndLoad);
    $('fSince').addEventListener('change', resetAndLoad);
    $('reqMore').addEventListener('click', () => { reqLimit = Math.min(500, reqLimit + 100); load(); });
    $('reqExportJson').addEventListener('click', () => exportReq('json'));
    $('reqExportCsv').addEventListener('click', () => exportReq('csv'));
    document.getElementById('page-requests').addEventListener('click', e => {
      const ab = e.target.closest('[data-abort]');
      if (ab) { e.stopPropagation(); abort(ab.dataset.abort); return; }
      const cp = e.target.closest('[data-copydir]');
      if (cp) { navigator.clipboard && navigator.clipboard.writeText(cp.dataset.copydir); return; }
      const mg = e.target.closest('[data-merged]');
      if (mg) { loadMerged(mg.dataset.merged); return; }
      const fl = e.target.closest('.file-link[data-f]');
      if (fl) { loadFile(fl.dataset.dir, fl.dataset.f); return; }
      const tr = e.target.closest('.req-table tbody tr[data-dir]');
      if (tr) toggleDetail(tr.dataset.dir);
    });
  }
  function exportReq(fmt) {
    const p = reqQuery(); p.set('format', fmt);
    window.open('/panel/api/requests/export?' + p.toString(), '_blank');
  }

  bind();
  Tabs.register('requests', () => { restoreFilterHash(); tick(); });
  onVisible('requests', tick, 10000);

  return { activeTable, resetAndLoad };
})();
