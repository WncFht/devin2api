// 系统页：进程运行指标 + 日志管道自观测 + stderr 日志查看 + 请求日志开关。
// 计数器为进程内存值，重启清零；用量口径见用量页（index.jsonl 回放不丢）。

const System = (() => {
  let procOffset = 0, procFollow = false, procBuf = '';
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
      if (d.debuglog) renderPipe(d.debuglog);
    } catch (e) {
      $('sysKpis').innerHTML = '<div class="note" style="color:var(--err)">指标拉取失败: ' + esc(String(e)) + '</div>';
    }
  }

  function renderPipe(d) {
    $('debugPipeBody').innerHTML =
      meta('日志开关', d.enabled ? '开启' : '关闭') +
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
      toast('请求日志已' + (d.enabled ? '开启' : '关闭'), 'ok');
    } catch (e) { toast('切换失败：' + e, 'err'); }
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
  $('procReload').addEventListener('click', () => loadLog(0));
  $('procLevel').addEventListener('change', renderLog);
  $('procFollow').addEventListener('click', e => {
    procFollow = !procFollow;
    e.target.classList.toggle('on', procFollow);
  });

  Tabs.register('system', () => { loadStats(); loadLog(0); });
  onVisible('system', loadStats, 10000);
  onVisible('system', () => { if (procFollow) loadLog(procOffset); }, 5000);
  return {};
})();
