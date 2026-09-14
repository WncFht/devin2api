// 系统页：进程运行指标 + 日志管道自观测 + 速率闸门 + 生效配置 + stderr 日志。
// 计数器为进程内存值，重启清零；用量口径见用量页（index.jsonl 回放不丢）。

const System = (() => {
  let procOffset = 0, procFollow = false, procBuf = '';
  let cfgData = null;    // /panel/api/config 视图（文件值，用于开关口径漂移提示）
  let lastDebug = null;  // 最近一次 debuglog stats，供 config 到达后补渲染口径行
  const PROC_CAP = 256 << 10;
  const PROC_LV = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 };

  async function loadStats() {
    try {
      const d = await api('/stats');
      if (d.version) $('versionTag').textContent = d.version;
      const h = d.http || {}, p = h.process || {}, r = h.rates || {};
      $('sysKpis').innerHTML =
        kpi('运行时长', fmtDuration(h.uptime_seconds), 'PID 内计数器，重启清零') +
        kpi('goroutine', p.goroutines ?? '-', '堆 ' + fmtBytes(p.heap_alloc_bytes) + ' / ' + fmtBytes(p.heap_sys_bytes), 'info') +
        kpi('峰值 RSS', fmtBytes(p.max_rss_bytes), '', 'violet') +
        kpi('CPU', Number(p.cpu_percent || 0).toFixed(1) + '%', '累计 ' + Number(p.cpu_seconds || 0).toFixed(1) + 's', 'warn') +
        kpi('GC', (p.num_gc ?? 0) + ' 次', '暂停 ' + Number(p.gc_pause_total_ms || 0).toFixed(0) + 'ms · CPU ' + (Number(p.gc_cpu_fraction || 0) * 100).toFixed(2) + '%') +
        kpi('当前 QPS', Number(r.qps_current || 0).toFixed(2), 'RPM ' + (r.rpm_current ?? 0) + ' / 峰值 ' + (r.rpm_peak ?? 0), 'cyan') +
        kpi('累计请求', h.completed_requests ?? 0, '2xx ' + (h.ok_responses ?? 0) + ' · 4xx ' + (h.client_error_responses ?? 0) + ' · 5xx ' + (h.server_error_responses ?? 0) + ' · 拒 ' + (h.rejected_requests ?? 0)) +
        kpi('流式/非流式', (h.streaming_requests ?? 0) + ' / ' + (h.non_streaming_requests ?? 0), '上行 ' + fmtBytes(h.request_body_bytes) + ' · 下行 ' + fmtBytes(h.response_body_bytes));
      renderGate(d.gate);
      renderRejects(h.rejects);
      if (d.debuglog) renderPipe(d.debuglog);
    } catch (e) {
      $('sysKpis').innerHTML = '<div class="note status-err">指标拉取失败: ' + esc(String(e)) + '</div>';
    }
  }

  // 速率闸门：闩中状态用 banner 强调（正在对客户端快败 429），
  // 计数器是进程内存值，重启清零。
  function renderGate(g) {
    const body = $('gateBody');
    if (!g) { body.innerHTML = '<div class="mini"><span class="v">无闸门数据（provider adapter 未配置）</span></div>'; return; }
    let html = '';
    if (g.latched) {
      const until = g.limited_until ? fmtTime(g.limited_until) + '（剩 ' + fmtInPrecise(Date.parse(g.limited_until) / 1000) + '）' : '时刻未知';
      html += '<div class="err-banner full">闩中：上游限流冷却至 ' + esc(until) + '，闩内新请求快败 429 + Retry-After，滴灌探针放行探测解闩</div>';
    }
    html += meta('闩态', g.latched ? '闩中' : '未闩') +
      meta('闩截止', g.limited_until ? fmtTime(g.limited_until) : '-') +
      meta('累计上闩', g.latch_count ?? 0) +
      meta('滴灌放行', g.drip_count ?? 0) +
      meta('闩内快败', g.reject_latched_count ?? 0) +
      meta('排队快败', g.reject_hold_count ?? 0) +
      meta('令牌余量', g.capacity ? Number(g.tokens ?? 0).toFixed(1) + ' / ' + g.capacity : '-') +
      meta('排队等待', g.waiters ?? 0) +
      meta('令牌补充', Number(g.refill_per_sec || 0).toFixed(2) + ' req/s');
    // 闩迁移事件环：计数器只说发生过几次，事件表回答「何时闩的、
    // 闩了多久、怎么解的」；概览趋势图的闩时段底色与这份数据同源。
    const GATE_KIND = { latched: '上闩', released: '解闩', expired: '到期失效', restored: '重启恢复' };
    const evs = (g.events || []).slice(0, 20);
    if (evs.length) {
      html += '<div class="tbl-wrap" style="max-height:180px;margin-top:6px"><table><thead><tr><th>时间</th><th>事件</th><th>闩截止</th><th>详情</th></tr></thead><tbody>';
      evs.forEach(e => {
        const at = Date.parse(e.at), until = e.until ? Date.parse(e.until) : 0;
        let label = GATE_KIND[e.kind] || e.kind;
        if (e.kind === 'latched' && e.detail === 'extended') label = '延闩';
        let detail = '';
        if (e.kind === 'latched' && until) detail = '闩长 ' + fmtMs(until - at);
        else if (e.kind === 'released' && until) detail = '提前 ' + fmtMs(Math.max(0, until - at)) + ' 解闩';
        else if (e.kind === 'restored') detail = '自 gate-state.json';
        html += '<tr><td class="mono">' + fmtTime(at) + '</td><td>' + esc(label) + '</td>' +
          '<td class="mono">' + (until ? fmtTime(until) : '-') + '</td><td class="muted">' + esc(detail) + '</td></tr>';
      });
      html += '</tbody></table></div>';
    }
    body.innerHTML = html;
  }

  // 本地拒绝：管线前被拒的请求没有调试目录与 index 行——分原因计数 +
  // 最近事件表是它们唯一的面板足迹；reason 与 stderr.log 的
  // "request rejected" 行同源，重启后可去进程日志按时间对。
  const REJECT_LABELS = {
    draining: '排空',
    concurrency_limit: '并发上限',
    ws_connection_limit: 'WS 连接上限',
    missing_api_key: '缺 API Key',
    invalid_api_key: '错 API Key',
  };
  function renderRejects(rj) {
    const body = $('rejectBody');
    if (!rj) { body.innerHTML = '<div class="mini"><span class="v">无拒绝数据</span></div>'; return; }
    const by = rj.by_reason || {};
    let html = '<div class="grid">';
    const keys = Object.keys(REJECT_LABELS).concat(Object.keys(by).filter(k => !REJECT_LABELS[k]));
    let any = false;
    keys.forEach(k => {
      if (!by[k]) return;
      any = true;
      html += meta(REJECT_LABELS[k] || k, by[k]);
    });
    if (!any) html += meta('分原因计数', '本进程无拒绝');
    html += '</div>';
    const recent = (rj.recent || []).slice(0, 30);
    if (recent.length) {
      html += '<div class="tbl-wrap" style="max-height:220px"><table><thead><tr><th>时间</th><th>原因</th><th>状态</th><th>路径</th><th>客户端</th></tr></thead><tbody>';
      recent.forEach(e => {
        const who = esc(e.ip || '-') + (e.key_hash ? ' <span class="muted" title="key hash">' + esc(e.key_hash) + '</span>' : '');
        const ua = e.user_agent ? '<div class="muted" title="' + qa(e.user_agent) + '">' + esc(e.user_agent.length > 48 ? e.user_agent.slice(0, 48) + '…' : e.user_agent) + '</div>' : '';
        html += '<tr><td class="mono">' + fmtTime(e.at * 1000) + '</td>' +
          '<td><span class="badge badge-medium" title="' + qa(e.reason) + '">' + esc(REJECT_LABELS[e.reason] || e.reason) + '</span></td>' +
          '<td class="mono">' + e.status + '</td><td class="mono">' + esc(e.path || '-') + '</td>' +
          '<td class="mono">' + who + ua + '</td></tr>';
      });
      html += '</tbody></table></div>';
      if ((rj.recent || []).length > recent.length) html += '<div class="note">仅显示最近 ' + recent.length + ' 条；更早的查 stderr.log「request rejected」。</div>';
    }
    body.innerHTML = html;
  }

  // 开关口径：toggle 只改内存，重启后回退到文件值；内存与文件不一致
  // 时显式标出（config 视图拿的是最后一次加载的文件值）。
  function pipeScopeHint(d) {
    const fileEnabled = cfgData && cfgData.config && cfgData.config.debug ? cfgData.config.debug.enabled : null;
    if (fileEnabled != null && !!fileEnabled !== !!d.enabled) {
      return meta('开关口径', '仅运行时生效 · 文件值为' + (fileEnabled ? '开' : '关') + '，重启后回退');
    }
    return meta('开关口径', '仅运行时生效 · 重启后回退到文件值');
  }

  function renderPipe(d) {
    lastDebug = d;
    $('debugPipeBody').innerHTML =
      meta('日志开关', d.enabled ? '开启' : '关闭') +
      pipeScopeHint(d) +
      meta('活跃日志目录', d.active_request_dirs ?? 0) +
      meta('写队列积压', (d.queued_log_events ?? 0) + ' / ' + (d.queue_capacity ?? 0)) +
      meta('丢弃日志事件', d.dropped_log_events ?? 0) +
      meta('IO 写失败', d.io_errors ?? 0) +
      meta('索引大小', fmtBytes(d.index_bytes)) +
      meta('保留天数', d.retention_days ?? '-') +
      meta('容量上限', (d.max_total_mb ?? '-') + ' MB') +
      meta('负载剥离', (d.payload_hours ?? '-') + 'h') +
      meta('保护失败目录', d.keep_error_dirs ?? '-');
    const tg = $('debugToggle');
    tg.style.display = '';
    tg.textContent = '请求日志: ' + (d.enabled ? '开' : '关');
    tg.classList.toggle('on', !!d.enabled);
  }

  async function toggleDebug() {
    const cur = $('debugToggle');
    const on = cur.classList.contains('on');
    try {
      const res = await fetch('/panel/api/debug/toggle', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: !on }) });
      const d = await res.json();
      cur.classList.toggle('on', !!d.enabled);
      cur.textContent = '请求日志: ' + (d.enabled ? '开' : '关');
      if (lastDebug) { lastDebug.enabled = !!d.enabled; renderPipe(lastDebug); }
      toast('请求日志已' + (d.enabled ? '开启' : '关闭'), 'ok');
    } catch (e) { toast('切换失败：' + e, 'err'); }
  }

  // ---------- 生效配置 ----------
  // 文件为事实源：这里展示最后一次加载的脱敏视图；改 config.yaml 后
  // 点「重新加载」热应用，requires_restart 列出的字段需托管重启。
  async function loadConfig() {
    try {
      cfgData = await api('/config');
      renderConfig();
      if (lastDebug) renderPipe(lastDebug);
    } catch (e) {
      $('cfgBody').innerHTML = '<div class="mini"><span class="v">配置端点不可用: ' + esc(String(e)) + '</span></div>';
    }
  }

  function renderConfig() {
    const d = cfgData || {};
    $('cfgBanner').innerHTML = d.stale
      ? '<div class="err-banner" style="margin-bottom:8px">config.yaml 在最后一次加载后被修改——点「重新加载」热应用；requires_restart 字段需托管重启生效</div>'
      : '';
    let html =
      meta('文件', d.path || '-') +
      meta('加载于', fmtTime(d.loaded_at)) +
      meta('文件修改', fmtTime(d.file_mtime)) +
      meta('文件状态', d.stale ? '已修改（待应用）' : '与文件一致');
    const r = d.last_reload;
    if (r) {
      html += meta('上次重载', fmtTime(r.at)) +
        meta('已应用', (r.applied || []).join(', ') || '无') +
        meta('待重启', (r.requires_restart || []).join(', ') || '无');
    }
    $('cfgBody').innerHTML = html;
  }

  async function reloadConfig() {
    const btn = $('cfgReload');
    btn.disabled = true;
    try {
      const res = await fetch('/panel/api/config/reload', { method: 'POST' });
      if (res.status === 401) { location.href = '/panel'; return; }
      const d = await res.json().catch(() => ({}));
      if (!res.ok) { toast('重载失败：' + (d.error || ('HTTP ' + res.status)), 'err'); return; }
      const cold = d.requires_restart || [];
      toast('已应用: ' + ((d.applied || []).join(', ') || '无变更') +
        (cold.length ? ' · 待重启: ' + cold.join(', ') : ''), cold.length ? 'warn' : 'ok');
      // 热字段（debug.enabled 等）可能刚变，连带刷新 stats 与配置视图。
      loadStats();
      loadConfig();
    } catch (e) { toast('重载失败：' + e, 'err'); }
    finally { btn.disabled = false; }
  }

  function toggleCfgView() {
    const v = $('cfgView');
    const show = v.style.display !== 'block';
    v.style.display = show ? 'block' : 'none';
    if (show) v.textContent = cfgData && cfgData.config ? JSON.stringify(cfgData.config, null, 2) : '(无配置数据)';
  }

  async function loadLog(offset) {
    try {
      const d = await api('/logs?offset=' + (offset || 0));
      procBuf = (offset && procOffset) ? procBuf + (d.text || '') : (d.text || '');
      // 缓冲封顶：跟随模式长期运行时 DOM 不无限增长。
      if (procBuf.length > PROC_CAP) procBuf = procBuf.slice(-PROC_CAP);
      procOffset = d.next_offset || 0;
      renderLog();
    } catch (e) { $('processLogView').textContent = '进程日志不可用（stderr.log 缺失或被清理）: ' + String(e); }
  }

  // 级别过滤保留无 level= 的行（堆栈续行、手写输出等），不静默吞内容。
  function renderLog() {
    const view = $('processLogView');
    const min = $('procLevel').value;
    let t = procBuf;
    if (min) {
      const want = PROC_LV[min] || 0;
      t = procBuf.split('\n').filter(l => { const m = /level=(\w+)/.exec(l); return !m || PROC_LV[m[1]] === undefined || PROC_LV[m[1]] >= want; }).join('\n');
    }
    view.textContent = t || '(空)';
    if (procFollow) view.scrollTop = view.scrollHeight;
  }

  $('debugToggle').addEventListener('click', toggleDebug);
  $('cfgReload').addEventListener('click', reloadConfig);
  $('cfgViewBtn').addEventListener('click', toggleCfgView);
  $('procReload').addEventListener('click', () => loadLog(0));
  $('procLevel').addEventListener('change', renderLog);
  $('procFollow').addEventListener('click', e => {
    procFollow = !procFollow;
    e.target.classList.toggle('on', procFollow);
  });

  Tabs.register('system', () => { loadStats(); loadConfig(); loadLog(0); });
  Polls.add('system', loadStats, 10000);
  Polls.add('system', loadConfig, 60000);
  Polls.add('system', () => { if (procFollow) loadLog(procOffset); }, 5000);
  return {};
})();
