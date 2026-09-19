// ============================================================
// Debug 日志模态框：上游报文 / 翻译投影 / 调试文件 / 合并视图
// ============================================================
(function () {

  const t = window.t;
  const i18nText = window.i18nText || ((key, fallback) => fallback || key);

  const debugLogUrl = (id) => `/admin/debug-logs/${encodeURIComponent(id)}`;
  const debugLogFileUrl = (id, name) =>
    `${debugLogUrl(id)}/file/${String(name).split('/').map(encodeURIComponent).join('/')}`;
  const debugLogMergedUrl = (id) => `${debugLogUrl(id)}/merged`;
  const activeDebugLogUrl = (id) => `/admin/active-requests/${encodeURIComponent(id)}/debug-log`;

  function formatDebugSettingValue(setting) {
    if (!setting || setting.value === undefined || setting.value === null || setting.value === '') {
      return '-';
    }

    const rawValue = String(setting.value).trim();
    switch (setting.key) {
      case 'debug_log_enabled':
        return (rawValue === 'true' || rawValue === '1')
          ? t('logs.debugSettingEnabledOn')
          : t('logs.debugSettingEnabledOff');
      case 'debug_log_retention_minutes':
        return t('logs.debugSettingRetentionMinutes', { minutes: rawValue });
      default:
        return rawValue;
    }
  }

  function buildDebugLogUnavailableHtml(data) {
    const enabledSetting = data?.debug_log_enabled || null;
    const retentionSetting = data?.debug_log_retention_minutes || null;
    const enabledValue = String(enabledSetting?.value || '').trim().toLowerCase();
    const isDebugEnabled = enabledValue === 'true' || enabledValue === '1';
    const hasExplicitEnabledValue = enabledValue !== '';
    const hintKey = hasExplicitEnabledValue
      ? (isDebugEnabled ? 'logs.debugUnavailableHintExpired' : 'logs.debugUnavailableHintDisabled')
      : 'logs.debugUnavailableHintGeneric';

    return `
      <div class="debug-log-unavailable">
        <div class="debug-log-unavailable__title">${escapeHtml(t('logs.debugUnavailableTitle'))}</div>
        <div class="debug-log-unavailable__hint">${escapeHtml(t(hintKey))}</div>
        <div class="debug-log-unavailable__settings-title">${escapeHtml(t('logs.debugUnavailableSettingsTitle'))}</div>
        <div class="debug-log-unavailable__settings">
          <div class="debug-log-unavailable__row">
            <span class="debug-log-unavailable__label">${escapeHtml(t('settings.desc.debug_log_enabled'))}</span>
            <span class="debug-log-unavailable__value">${escapeHtml(formatDebugSettingValue(enabledSetting))}</span>
          </div>
          <div class="debug-log-unavailable__row">
            <span class="debug-log-unavailable__label">${escapeHtml(t('settings.desc.debug_log_retention_minutes'))}</span>
            <span class="debug-log-unavailable__value">${escapeHtml(formatDebugSettingValue(retentionSetting))}</span>
          </div>
        </div>
      </div>
    `;
  }

  function formatJsonSafe(str) {
    if (!str) return '';
    try {
      return JSON.stringify(JSON.parse(str), null, 2);
    } catch {
      return str;
    }
  }

  function formatHeaderLines(headers) {
    if (!headers) return '';
    if (typeof headers === 'string') {
      try { headers = JSON.parse(headers); } catch { return headers; }
    }
    if (typeof headers !== 'object') return '';
    headers = window.maskSensitiveHeaders(headers);
    const lines = [];
    for (const [key, value] of Object.entries(headers)) {
      if (Array.isArray(value)) {
        value.forEach(v => lines.push(`${key}: ${v}`));
      } else {
        lines.push(`${key}: ${value}`);
      }
    }
    return lines.join('\n');
  }

  function composeDebugRequest(method, url, headerData, bodyData) {
    const parts = [];
    parts.push(`${method || 'POST'} ${url || ''}`);
    const headers = formatHeaderLines(headerData);
    if (headers) parts.push(headers);
    const body = formatJsonSafe(bodyData);
    if (body) {
      parts.push('');
      parts.push(body);
    }
    return parts.join('\n');
  }

  function composeDebugResponse(status, headerData, bodyData, upstreamError) {
    const parts = [];
    if (status) parts.push('HTTP ' + status);
    if (upstreamError) {
      if (!status) parts.push('UPSTREAM TRANSPORT ERROR (no HTTP response)');
      parts.push(String(upstreamError));
    }
    const headers = formatHeaderLines(headerData);
    if (headers) parts.push(headers);
    const body = formatJsonSafe(bodyData);
    if (body) {
      parts.push('');
      parts.push(body);
    }
    return parts.join('\n');
  }

  function composeDebugRawRequest(data) {
    if (data?.protocol_transformed) {
      return composeDebugRequest(data.req_method, data.original_req_url, data.original_req_headers, data.original_req_body);
    }
    return composeDebugRequest(data?.req_method, data?.req_url, data?.req_headers, data?.req_body);
  }

  function composeDebugRawResponse(data) {
    return composeDebugResponse(data?.resp_status, data?.resp_headers, data?.resp_body, data?.upstream_error);
  }

  function composeDebugTranslatedRequest(data) {
    return composeDebugRequest(data?.req_method, data?.req_url, data?.req_headers, data?.req_body);
  }

  function composeDebugTranslatedResponse(data) {
    return composeDebugResponse(data?.translated_resp_status, data?.translated_resp_headers, data?.translated_resp_body);
  }

  function setDebugTabLabel(buttonId, key, fallback) {
    const button = document.getElementById(buttonId);
    if (!button) return;
    button.dataset.i18n = key;
    button.textContent = (typeof t === 'function' ? t(key) : '') || fallback;
  }

  function activateDebugTab(target) {
    const modal = document.getElementById('debugLogModal');
    if (!modal) return;
    modal.querySelectorAll('.upstream-tab').forEach(tab => {
      tab.classList.toggle('active', tab.dataset.tab === target);
    });
    modal.querySelectorAll('.upstream-tab-panel').forEach(panel => {
      panel.classList.toggle('active', panel.dataset.tab === target);
    });
    updateDebugResponseActionButtons();
  }

  function configureDebugProtocolTabs(data) {
    const transformed = !!data?.protocol_transformed;
    const translatedRequestTab = document.getElementById('debugTranslatedRequestTabBtn');
    const translatedResponseTab = document.getElementById('debugTranslatedResponseTabBtn');
    if (translatedRequestTab) translatedRequestTab.hidden = !transformed;
    if (translatedResponseTab) translatedResponseTab.hidden = !transformed;

    setDebugTabLabel('debugRequestTabBtn', transformed ? 'logs.debugOriginalRequest' : 'logs.debugRequest', transformed ? '原始请求' : '请求');
    setDebugTabLabel('debugTranslatedRequestTabBtn', 'logs.debugTranslatedRequest', '转换后请求');
    setDebugTabLabel('debugResponseTabBtn', transformed ? 'logs.debugOriginalResponse' : 'logs.debugResponse', transformed ? '原始响应' : '响应');
    setDebugTabLabel('debugTranslatedResponseTabBtn', 'logs.debugTranslatedResponse', '转换后响应');

    const activeTab = document.querySelector('#debugLogModal .upstream-tab.active');
    if (!activeTab || activeTab.hidden) activateDebugTab('request');
  }

  const ACTIVE_DEBUG_LOG_REFRESH_INTERVAL_MS = 1500;
  let activeDebugLogRefreshTimer = null;
  let activeDebugLogRefreshInFlight = false;
  let debugLogWrapEnabled = true;
  let currentDebugLogData = null;
  const debugResponseViews = {
    response: {
      rawId: 'debugRespRaw',
      mergedId: 'debugRespMerged',
      bodyKey: 'resp_body'
    },
    'translated-response': {
      rawId: 'debugTranslatedRespRaw',
      mergedId: 'debugTranslatedRespMerged',
      bodyKey: 'translated_resp_body'
    }
  };
  const debugMergedStates = {
    response: { visible: false, sourceBody: null, loading: false },
    'translated-response': { visible: false, sourceBody: null, loading: false }
  };

  // debugFileContext 记录当前模态框对应的可解析目录 id（files/merged 端点
  // 的 {id}：日志行自增 id，或历史链接的 started_at 毫秒戳）与已打开文件
  // 名/大小；活跃请求模态框的 log_id 是 FNV 哈希不能解析目录，fileId 由
  // 活跃列表 start_time 反查。
  let debugFileContext = null;

  async function showDebugLogModal(logId) {
    return showDebugLogModalFromUrl(debugLogUrl(logId), { activeRequestId: 0, fileId: logId });
  }

  async function showActiveDebugLogModal(activeRequestId) {
    return showDebugLogModalFromUrl(activeDebugLogUrl(activeRequestId), { activeRequestId });
  }

  async function showDebugLogModalFromUrl(url, opts = {}) {
    const modal = document.getElementById('debugLogModal');
    const loading = document.getElementById('debugLogLoading');
    const error = document.getElementById('debugLogError');
    const content = document.getElementById('debugLogContent');

    // 若上一次模态框未清理，先停掉旧的轮询
    stopActiveDebugLogPolling();

    const requestedActiveId = Number(opts.activeRequestId) || 0;
    debugFileContext = {
      fileId: opts.fileId ? String(opts.fileId)
        : (requestedActiveId > 0 ? resolveActiveDebugFileId(requestedActiveId) : ''),
      activeRequestId: requestedActiveId,
      openName: null,
      openSize: null
    };

    loading.style.display = '';
    error.style.display = 'none';
    error.innerHTML = '';
    error.textContent = '';
    content.style.display = 'none';
    setDebugLogStatus(null);
    currentDebugLogData = null;
    Modal.open(modal, { onClose: cleanupDebugLogModal });

    // Reset tabs
    configureDebugProtocolTabs(null);
    activateDebugTab('request');
    resetDebugMergedResponses();
    resetDebugFileView();
    renderDebugFileList(null);
    applyDebugLogWrapMode();
    updateDebugResponseActionButtons();

    try {
      const { res, payload } = await fetchAPIWithAuthRaw(url);
      if (!payload.success) {
        if (res.status === 404) {
          loading.style.display = 'none';
          error.innerHTML = buildDebugLogUnavailableHtml(payload.data || null);
          error.style.display = '';
          return;
        }
        throw new Error(payload.error || i18nText('logs.debugLoadFailed', '加载失败'));
      }

      const data = payload.data || {};
      currentDebugLogData = data;
      loading.style.display = 'none';
      content.style.display = 'flex';

      configureDebugProtocolTabs(data);
      window.setHighlightedCodeContent('debugReqRaw', composeDebugRawRequest(data), 'request');
      window.setHighlightedCodeContent('debugTranslatedReqRaw', composeDebugTranslatedRequest(data), 'request');
      window.setHighlightedCodeContent('debugRespRaw', composeDebugRawResponse(data), 'response');
      window.setHighlightedCodeContent('debugTranslatedRespRaw', composeDebugTranslatedResponse(data), 'response');
      renderDebugFileList(data);
      resetDebugMergedResponses();

      // 如果是实时活跃请求，启动轮询
      const activeRequestId = Number(opts.activeRequestId);
      if (Number.isFinite(activeRequestId) && activeRequestId > 0) {
        startActiveDebugLogPolling(activeRequestId);
      }
    } catch (e) {
      loading.style.display = 'none';
      error.textContent = e.message || i18nText('logs.debugLoadFailed', '加载失败');
      error.style.display = '';
    }
  }

  function setDebugLogStatus(kind) {
    const el = document.getElementById('debugLogStatus');
    if (!el) return;
    el.classList.remove('debug-log-status--refreshing', 'debug-log-status--finished');
    if (!kind) {
      el.hidden = true;
      el.textContent = '';
      return;
    }
    if (kind === 'refreshing') {
      el.classList.add('debug-log-status--refreshing');
      el.textContent = (typeof t === 'function' ? t('logs.debugRefreshing') : '正在更新…') || '正在更新…';
    } else if (kind === 'finished') {
      el.classList.add('debug-log-status--finished');
      el.textContent = (typeof t === 'function' ? t('logs.debugRequestFinished') : '请求已结束') || '请求已结束';
    }
    el.hidden = false;
  }

  function startActiveDebugLogPolling(activeRequestId) {
    stopActiveDebugLogPolling();
    setDebugLogStatus('refreshing');
    activeDebugLogRefreshTimer = setInterval(() => {
      refreshActiveDebugLogOnce(activeRequestId);
    }, ACTIVE_DEBUG_LOG_REFRESH_INTERVAL_MS);
  }

  function stopActiveDebugLogPolling() {
    if (activeDebugLogRefreshTimer) {
      clearInterval(activeDebugLogRefreshTimer);
      activeDebugLogRefreshTimer = null;
    }
    activeDebugLogRefreshInFlight = false;
  }

  async function refreshActiveDebugLogOnce(activeRequestId) {
    if (activeDebugLogRefreshInFlight) return;
    // 模态框已关闭则停止
    const modal = document.getElementById('debugLogModal');
    if (!modal || !modal.classList.contains('show')) {
      stopActiveDebugLogPolling();
      return;
    }
    activeDebugLogRefreshInFlight = true;
    try {
      const { res, payload } = await fetchAPIWithAuthRaw(activeDebugLogUrl(activeRequestId));
      if (!payload.success) {
        if (res.status === 404) {
          // 请求已结束，停止轮询并提示，保留最后一次成功拉到的快照
          stopActiveDebugLogPolling();
          setDebugLogStatus('finished');
          return;
        }
        // 其他错误：保持现状，下个 tick 再试
        return;
      }
      const data = payload.data || {};
      currentDebugLogData = data;
      updateDebugLogContentPreserveScroll(data);
    } catch (_) {
      // 网络抖动：忽略，下个 tick 继续
    } finally {
      activeDebugLogRefreshInFlight = false;
    }
  }

  function updateDebugLogContentPreserveScroll(data) {
    configureDebugProtocolTabs(data);
    updateDebugPanePreserveScroll('debugReqRaw', composeDebugRawRequest(data), 'request');
    updateDebugPanePreserveScroll('debugTranslatedReqRaw', composeDebugTranslatedRequest(data), 'request');
    updateDebugPanePreserveScroll('debugRespRaw', composeDebugRawResponse(data), 'response');
    updateDebugPanePreserveScroll('debugTranslatedRespRaw', composeDebugTranslatedResponse(data), 'response');
    // 上游重试会换目录（start_time 变）：轮询时向活跃列表重解析 fileId，
    // 让 files/merged 始终指向当前目录。
    if (debugFileContext?.activeRequestId) {
      const resolved = resolveActiveDebugFileId(debugFileContext.activeRequestId);
      if (resolved) debugFileContext.fileId = resolved;
    }
    renderDebugFileList(data);
    // 进行中请求仍在写文件：清单里已打开文件的大小变了才重拉内容
    if (debugFileContext?.openName) {
      const entry = (Array.isArray(data?.files) ? data.files : [])
        .find(f => String(f?.name) === debugFileContext.openName);
      if (!entry) {
        resetDebugFileView();
      } else if (Number(entry.size) !== debugFileContext.openSize) {
        void loadDebugFile(debugFileContext.openName);
      }
    }
    for (const tab of Object.keys(debugResponseViews)) {
      if (debugMergedStates[tab].visible) {
        void refreshDebugMergedResponse(data, tab);
      }
    }
  }

  function updateDebugPanePreserveScroll(targetId, text, mode) {
    const pre = document.getElementById(targetId);
    if (!pre) return;
    // 内容未变化则跳过，避免破坏选区与滚动
    const prevText = pre._rawText || '';
    const nextText = mode === 'markdown' ? mergedResponseRawText(text) : (text || '');
    if (prevText === nextText) return;

    const stickToBottom = isScrolledToBottom(pre);
    const prevScrollTop = pre.scrollTop;

    if (mode === 'markdown') {
      window.MarkdownRenderer.renderResponse(targetId, text || { reasoning: '', content: '' });
    } else {
      window.setHighlightedCodeContent(targetId, text || '', mode);
    }

    if (stickToBottom) {
      pre.scrollTop = pre.scrollHeight;
    } else {
      pre.scrollTop = prevScrollTop;
    }
  }

  function mergedResponseRawText(response) {
    if (response && typeof response === 'object' && !Array.isArray(response)) {
      return [
        response.reasoning || response.thinking || '',
        response.content ?? response.text ?? '',
        response.tools ?? response.toolCalls ?? response.functionCalls ?? ''
      ]
        .map(value => String(value || '').trim())
        .filter(Boolean)
        .join('\n\n');
    }
    return String(response || '');
  }

  function isScrolledToBottom(el) {
    if (!el) return false;
    const threshold = 8; // 像素容差
    return el.scrollHeight - el.scrollTop - el.clientHeight <= threshold;
  }

  function cleanupDebugLogModal() {
    stopActiveDebugLogPolling();
    setDebugLogStatus(null);
    currentDebugLogData = null;
    resetDebugMergedResponses();
    resetDebugFileView();
    debugFileContext = null;
  }

  function closeDebugLogModal() {
    Modal.close(document.getElementById('debugLogModal'));
  }

  function updateDebugWrapButton() {
    const wrapBtn = document.getElementById('debugWrapBtn');
    if (!wrapBtn) return;
    wrapBtn.classList.toggle('active', debugLogWrapEnabled);
    wrapBtn.setAttribute('aria-pressed', debugLogWrapEnabled ? 'true' : 'false');
    wrapBtn.dataset.i18n = debugLogWrapEnabled ? 'logs.debugWrap' : 'logs.debugNoWrap';
    wrapBtn.textContent = (typeof t === 'function' ? t(wrapBtn.dataset.i18n) : '') ||
      (debugLogWrapEnabled ? '换行' : '不换行');
  }

  function applyDebugLogWrapMode() {
    document.querySelectorAll('#debugLogModal .upstream-pre').forEach(pre => {
      pre.classList.toggle('upstream-pre--nowrap', !debugLogWrapEnabled);
    });
    document.querySelectorAll('#debugLogModal .upstream-merged-markdown').forEach(merged => {
      merged.classList.toggle('upstream-merged-markdown--nowrap', !debugLogWrapEnabled);
    });
    updateDebugWrapButton();
  }

  function setDebugLogWrapEnabled(enabled) {
    debugLogWrapEnabled = !!enabled;
    applyDebugLogWrapMode();
  }

  function updateDebugResponseActionButtons() {
    const activeTab = document.querySelector('#debugLogModal .upstream-tab.active')?.dataset.tab || 'request';
    const responseView = debugResponseViews[activeTab];
    const mergedVisible = responseView ? debugMergedStates[activeTab].visible : false;
    const copyTargets = {
      request: 'debugReqRaw',
      'translated-request': 'debugTranslatedReqRaw',
      response: debugMergedStates.response.visible ? 'debugRespMerged' : 'debugRespRaw',
      'translated-response': debugMergedStates['translated-response'].visible
        ? 'debugTranslatedRespMerged'
        : 'debugTranslatedRespRaw',
      files: 'debugFileRaw'
    };
    const copyBtn = document.querySelector('#debugLogModal .upstream-copy-btn--tabs');
    if (copyBtn) {
      copyBtn.dataset.copyTarget = copyTargets[activeTab] || 'debugReqRaw';
    }

    const mergeBtn = document.getElementById('debugMergeBtn');
    if (mergeBtn) {
      mergeBtn.hidden = !responseView;
      const key = mergedVisible ? 'logs.debugRaw' : 'logs.debugMerge';
      mergeBtn.classList.toggle('active', mergedVisible);
      mergeBtn.setAttribute('aria-pressed', mergedVisible ? 'true' : 'false');
      mergeBtn.dataset.i18n = key;
      mergeBtn.textContent = (typeof t === 'function' ? t(key) : '') || (mergedVisible ? '原始' : '合并');
    }
  }

  function activeDebugResponseTab() {
    const tab = document.querySelector('#debugLogModal .upstream-tab.active')?.dataset.tab;
    return debugResponseViews[tab] ? tab : '';
  }

  function setDebugResponseMergedVisible(visible, tab = activeDebugResponseTab()) {
    const view = debugResponseViews[tab];
    const state = debugMergedStates[tab];
    if (!view || !state) return;
    state.visible = !!visible;
    const raw = document.getElementById(view.rawId);
    const merged = document.getElementById(view.mergedId);
    if (raw) raw.hidden = state.visible;
    if (merged) merged.hidden = !state.visible;
    updateDebugResponseActionButtons();

    if (state.visible) {
      void refreshDebugMergedResponse(currentDebugLogData, tab);
    }
  }

  function resetDebugMergedResponses() {
    for (const [tab, view] of Object.entries(debugResponseViews)) {
      const state = debugMergedStates[tab];
      state.visible = false;
      state.sourceBody = null;
      state.loading = false;
      const raw = document.getElementById(view.rawId);
      const merged = document.getElementById(view.mergedId);
      if (raw) raw.hidden = false;
      if (merged) merged.hidden = true;
      window.MarkdownRenderer.renderResponse(view.mergedId, { reasoning: '', content: '' });
    }
    const note = document.getElementById('debugMergedNote');
    if (note) {
      note.hidden = true;
      note.textContent = '';
    }
    updateDebugResponseActionButtons();
  }

  function showDebugMergedNote(truncated) {
    const note = document.getElementById('debugMergedNote');
    if (!note) return;
    if (truncated) {
      note.textContent = i18nText(
        'logs.mergedTruncated',
        '响应流超过读取上限，仅合并了前段帧——后半可能缺失'
      );
      note.hidden = false;
    } else {
      note.hidden = true;
      note.textContent = '';
    }
  }

  async function refreshDebugMergedResponse(data, tab) {
    const view = debugResponseViews[tab];
    const state = debugMergedStates[tab];
    if (!data || !view || !state || state.loading) return;
    const sourceBody = String(data[view.bodyKey] || '');
    if (state.sourceBody === sourceBody) return;
    state.loading = true;
    window.MarkdownRenderer.renderResponse(view.mergedId, {
      reasoning: '',
      content: (typeof t === 'function' ? t('common.loading') : '加载中...') || '加载中...',
    });
    try {
      // translated-response 的源是 06（客户端线上帧）：目录 id 可解析时走
      // 服务端合并 GET /admin/debug-logs/{id}/merged——与 POST
      // merged-response 共用后端 mergeResponseBody，省去把 06 原文上送
      // 一趟，并能拿到 truncated 标记（>4MB 只合并前段）标注在视图上方。
      // response 页签（04 上游帧）与活跃请求无目录 id 时仍走 POST 上传。
      const fileId = tab === 'translated-response' ? (debugFileContext?.fileId || '') : '';
      let merged;
      if (fileId) {
        const resp = await fetchDataWithAuth(debugLogMergedUrl(fileId)) || {};
        merged = { reasoning: resp.reasoning, content: resp.content, tools: resp.tools };
        showDebugMergedNote(resp.truncated === true);
      } else {
        merged = await window.MergedResponseClient.mergeUpstreamResponse(sourceBody);
        showDebugMergedNote(false);
      }
      state.sourceBody = sourceBody;
      updateDebugPanePreserveScroll(view.mergedId, merged, 'markdown');
    } catch (e) {
      window.MarkdownRenderer.renderResponse(view.mergedId, {
        reasoning: '',
        content: e?.message || '合并响应失败',
      });
    } finally {
      state.loading = false;
    }
  }

  // ── Files 页签：调试记录文件清单 ──────────────────────────────────
  // 详情响应的 files[]（{name,size}）列出目录内全部留痕文件（01-06 阶段、
  // error.json、attachments/…）；点击经 /file/{name} 读取——JSON 美化、
  // JSONL 逐行加「#seq +ms event」头注，二进制走 ?raw=1 原始字节预览/打开。
  function resolveActiveDebugFileId(activeRequestId) {
    const req = latestActiveRequests.find(r => String(r?.id) === String(activeRequestId));
    const startMs = Number(req?.start_time);
    return Number.isFinite(startMs) && startMs > 0 ? String(Math.trunc(startMs)) : '';
  }

  function resetDebugFileView() {
    if (debugFileContext) {
      debugFileContext.openName = null;
      debugFileContext.openSize = null;
    }
    const view = document.getElementById('debugFileView');
    if (view) view.hidden = true;
    const pre = document.getElementById('debugFileRaw');
    if (pre) {
      pre._rawText = '';
      pre.innerHTML = '';
      pre.hidden = false;
    }
    const binary = document.getElementById('debugFileBinary');
    if (binary) {
      binary.hidden = true;
      binary.innerHTML = '';
    }
  }

  function renderDebugFileList(data) {
    const tabBtn = document.getElementById('debugFilesTabBtn');
    const list = document.getElementById('debugFileList');
    if (!tabBtn || !list) return;
    const files = Array.isArray(data?.files) ? data.files : [];
    tabBtn.hidden = files.length === 0;
    if (files.length === 0) {
      list.innerHTML = '';
      if (debugFileContext?.openName) resetDebugFileView();
      if (document.querySelector('#debugLogModal .upstream-tab.active')?.dataset.tab === 'files') {
        activateDebugTab('request');
      }
      return;
    }
    list.innerHTML = files.map((file) => {
      const name = String(file?.name || '');
      const open = debugFileContext && debugFileContext.openName === name;
      return `<button type="button" class="debug-file-item${open ? ' active' : ''}" data-action="toggle-debug-file" data-debug-file="${escapeHtml(name)}">`
        + `<span class="debug-file-item-name">${escapeHtml(name)}</span>`
        + `<span class="debug-file-item-size">${escapeHtml(formatBytes(Number(file?.size) || 0))}</span>`
        + '</button>';
    }).join('');
  }

  // JSONL 逐行加「#seq +elapsed_ms event」头注（沿用旧面板口径）；
  // data 截断 2000 字符避免单行撑爆视图。
  function formatLogsJsonlLines(text) {
    return String(text || '').split('\n').filter(Boolean).map((line) => {
      try {
        const o = JSON.parse(line);
        const head = (o.seq ? '#' + o.seq + ' ' : '')
          + (o.elapsed_ms != null ? '+' + o.elapsed_ms + 'ms ' : '')
          + (o.event || '');
        return head + '  ' + JSON.stringify(o.data !== undefined ? o.data : o).slice(0, 2000);
      } catch (e) {
        return line;
      }
    }).join('\n\n');
  }

  async function toggleDebugFile(name) {
    if (!debugFileContext) return;
    // 再点同一个文件名 = 收起查看区
    if (debugFileContext.openName === name) {
      resetDebugFileView();
      renderDebugFileList(currentDebugLogData);
      return;
    }
    debugFileContext.openName = name;
    renderDebugFileList(currentDebugLogData);
    await loadDebugFile(name);
  }

  async function loadDebugFile(name) {
    const view = document.getElementById('debugFileView');
    const nameEl = document.getElementById('debugFileViewName');
    const binaryEl = document.getElementById('debugFileBinary');
    if (!view) return;
    view.hidden = false;
    if (binaryEl) {
      binaryEl.hidden = true;
      binaryEl.innerHTML = '';
    }
    const pre = document.getElementById('debugFileRaw');
    if (pre) pre.hidden = false;
    if (nameEl) nameEl.textContent = name;
    updateDebugFileRawButtons(false);
    window.setHighlightedCodeContent('debugFileRaw', i18nText('common.loading', '加载中...'), 'text');

    const fileId = debugFileContext?.fileId;
    if (!fileId) {
      window.setHighlightedCodeContent(
        'debugFileRaw',
        i18nText('logs.debugFileNoDir', '无法定位调试记录（请求可能刚结束或已清理）'),
        'text'
      );
      return;
    }
    try {
      const data = await fetchDataWithAuth(debugLogFileUrl(fileId, name));
      // 期间用户切换/收起了文件——晚到的内容直接丢弃
      if (debugFileContext?.openName !== name) return;
      // 0 字节文件要存 0 而非 null——null 会让轮询判成「大小变了」每拍重拉
      const openSize = Number(data?.size);
      debugFileContext.openSize = Number.isFinite(openSize) ? openSize : null;
      if (data?.binary) {
        renderDebugFileBinary(name, data);
        return;
      }
      let text = String(data?.text ?? '');
      let mode = 'text';
      if (/\.json$/i.test(name)) {
        text = formatJsonSafe(text);
        mode = 'json';
      } else if (/\.jsonl$/i.test(name)) {
        text = formatLogsJsonlLines(text);
      }
      if (data?.truncated) {
        text += `\n\n${i18nText('logs.fileTruncated', '… 已截断（文件超过读取上限，仅显示前段）')}`;
      }
      window.setHighlightedCodeContent('debugFileRaw', text, mode);
    } catch (e) {
      if (debugFileContext?.openName !== name) return;
      window.setHighlightedCodeContent('debugFileRaw', e?.message || i18nText('logs.fileReadFailed', '读取失败'), 'text');
    }
  }

  // 二进制附件（图片等）：JSON 文本视图装不下字节，给「打开原始内容」
  // 入口；图片扩展名额外经 ?raw=1 拉 objectURL 预览。
  function renderDebugFileBinary(name, data) {
    const binaryEl = document.getElementById('debugFileBinary');
    const pre = document.getElementById('debugFileRaw');
    if (!binaryEl) return;
    if (pre) {
      pre._rawText = '';
      pre.innerHTML = '';
      pre.hidden = true;
    }
    updateDebugFileRawButtons(true);
    binaryEl.innerHTML = `<div class="debug-file-binary-info">${escapeHtml(i18nText(
      'logs.debugFileBinary',
      '二进制文件（{size}）——用「打开原始内容」查看',
      { size: formatBytes(Number(data?.size) || 0) }
    ))}</div>`;
    binaryEl.hidden = false;
    if (/\.(png|jpe?g|gif|webp|bmp|svg|ico)$/i.test(name)) {
      void previewDebugFileImage(name, binaryEl);
    }
  }

  async function previewDebugFileImage(name, container) {
    const fileId = debugFileContext?.fileId;
    if (!fileId) return;
    try {
      const res = await fetchWithAuth(`${debugLogFileUrl(fileId, name)}?raw=1`);
      if (!res.ok || debugFileContext?.openName !== name) return;
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const img = document.createElement('img');
      img.className = 'debug-file-preview';
      img.alt = name;
      img.src = url;
      img.onload = () => URL.revokeObjectURL(url);
      img.onerror = () => URL.revokeObjectURL(url);
      container.appendChild(img);
    } catch (_) { /* 预览失败仅保留信息行 */ }
  }

  function updateDebugFileRawButtons(isBinary) {
    // 二进制内容复制成文本是乱码——复制原始按钮只对文本文件有意义
    const copyBtn = document.getElementById('debugFileRawBtn');
    if (copyBtn) copyBtn.hidden = !!isBinary;
  }

  async function copyDebugFileRaw(btn) {
    const name = debugFileContext?.openName;
    const fileId = debugFileContext?.fileId;
    if (!name || !fileId) return;
    try {
      const res = await fetchWithAuth(`${debugLogFileUrl(fileId, name)}?raw=1`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const text = await res.text();
      await window.copyToClipboard(text);
      if (btn) {
        const orig = btn.textContent;
        btn.textContent = '✓';
        btn.classList.add('copied');
        setTimeout(() => { btn.textContent = orig; btn.classList.remove('copied'); }, 1500);
      }
    } catch (e) {
      if (window.showError) window.showError(e?.message || i18nText('logs.fileReadFailed', '读取失败'));
    }
  }

  async function openDebugFileRaw() {
    const name = debugFileContext?.openName;
    const fileId = debugFileContext?.fileId;
    if (!name || !fileId) return;
    try {
      const res = await fetchWithAuth(`${debugLogFileUrl(fileId, name)}?raw=1`);
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      window.open(url, '_blank', 'noopener');
      setTimeout(() => URL.revokeObjectURL(url), 60000);
    } catch (e) {
      if (window.showError) window.showError(e?.message || i18nText('logs.fileReadFailed', '读取失败'));
    }
  }

  // 模态框内交互统一走 data-action 委托；测试桩只有最小 DOM 时静默跳过。
  if (typeof window.initDelegatedActions === 'function') {
    window.initDelegatedActions({
      boundKey: 'debugLogModalActionsBound',
      click: {
        'switch-debug-tab': (el) => activateDebugTab(el.dataset.tab),
        'merge-debug-response': () => {
          const tab = activeDebugResponseTab();
          if (tab) setDebugResponseMergedVisible(!debugMergedStates[tab].visible, tab);
        },
        'toggle-debug-wrap': () => setDebugLogWrapEnabled(!debugLogWrapEnabled),
        'toggle-debug-file': (el) => void toggleDebugFile(el.dataset.debugFile),
        'copy-debug-file-raw': (el) => void copyDebugFileRaw(el),
        'open-debug-file-raw': () => void openDebugFileRaw(),
        'copy-debug-pane': (el) => {
          const pre = document.getElementById(el.dataset.copyTarget);
          if (!pre) return;
          const text = pre._rawText || pre.textContent || '';
          window.copyToClipboard(text).then(() => {
            const orig = el.textContent;
            el.textContent = '\u2713';
            el.classList.add('copied');
            setTimeout(() => { el.textContent = orig; el.classList.remove('copied'); }, 1500);
          }).catch(() => {});
        }
      }
    });
  }

  window.showDebugLogModal = showDebugLogModal;
  window.showActiveDebugLogModal = showActiveDebugLogModal;
  window.closeDebugLogModal = closeDebugLogModal;
})();
