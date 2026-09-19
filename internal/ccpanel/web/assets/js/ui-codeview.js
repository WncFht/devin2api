// ============================================================
// 上游报文代码视图：高亮、折叠、复制与敏感头脱敏
// ============================================================
(function () {

  /**
   * 复制文本到剪贴板（带降级处理）
   * @param {string} text - 要复制的文本
   * @returns {Promise<void>}
   */

  function copyWithSelection(text) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.style.position = 'fixed';
    ta.style.left = '-9999px';
    ta.tabIndex = -1;
    const dialogs = typeof document.querySelectorAll === 'function'
      ? document.querySelectorAll('dialog[open]')
      : [];
    const host = dialogs.length > 0 ? dialogs[dialogs.length - 1] : document.body;
    const previousFocus = document.activeElement;
    host.appendChild(ta);
    ta.select();
    ta.setSelectionRange?.(0, ta.value.length);

    try {
      return typeof document.execCommand === 'function' && document.execCommand('copy');
    } catch {
      return false;
    } finally {
      host.removeChild(ta);
      previousFocus?.focus?.({ preventScroll: true });
    }
  }

  function copyToClipboard(text) {
    // 必须在点击事件的同步调用栈内执行，异步 Clipboard API 被拒绝后
    // 再降级会丢失 user activation，浏览器仍会拦截复制。
    if (typeof document.execCommand === 'function' && copyWithSelection(text)) {
      return Promise.resolve();
    }
    const clipboard = globalThis.navigator && globalThis.navigator.clipboard;
    if (clipboard && typeof clipboard.writeText === 'function') {
      return clipboard.writeText(text);
    }
    return Promise.reject(new Error('copy failed'));
  }

  function escapeCodeHtml(str) {
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;');
  }

  function wrapHighlightedToken(text, modifier) {
    return `<span class="upstream-token upstream-token--${modifier}">${escapeCodeHtml(text)}</span>`;
  }

  function highlightSyntax(text, language) {
    const highlighter = globalThis.hljs;
    if (!highlighter || typeof highlighter.highlight !== 'function') {
      throw new Error('highlight.js is required for syntax highlighting');
    }

    return highlighter.highlight(String(text || ''), {
      language,
      ignoreIllegals: true
    }).value;
  }

  function classifyStatusModifier(statusCode) {
    const code = Number.parseInt(statusCode, 10);
    if (!Number.isFinite(code)) return 'status-unknown';
    if (code >= 200 && code < 300) return 'status-success';
    if (code >= 400 && code < 500) return 'status-client-error';
    if (code >= 500) return 'status-server-error';
    return 'status-neutral';
  }

  function renderHighlightedLines(text, language) {
    const value = String(text || '');
    return highlightSyntax(value, language).split('\n');
  }

  function renderHeaderLine(line) {
    const match = line.match(/^([^:]+)(:\s*)(.*)$/);
    if (!match) return escapeCodeHtml(line);

    const [, key, separator, value] = match;
    return `${wrapHighlightedToken(key, 'header-key')}${escapeCodeHtml(separator)}${value ? wrapHighlightedToken(value, 'header-value') : ''}`;
  }

  function renderRequestLine(line) {
    const requestMatch = line.match(/^(\s*)([A-Z]+)(\s+)(\S.*)$/);
    if (requestMatch && /^[a-z]+:\/\//i.test(requestMatch[4])) {
      const [, indent, method, gap, url] = requestMatch;
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(method, 'method')}${escapeCodeHtml(gap)}${wrapHighlightedToken(url, 'url')}`;
    }

    const urlMatch = line.match(/^(\s*)([a-z]+:\/\/\S.*)$/i);
    if (urlMatch) {
      const [, indent, url] = urlMatch;
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(url, 'url')}`;
    }

    return escapeCodeHtml(line);
  }

  function renderStatusLine(line) {
    const responseMatch = line.match(/^(\s*)(HTTP)(\s+)(\d{3})(.*)$/i);
    if (responseMatch) {
      const [, indent, protocol, gap, statusCode, rest] = responseMatch;
      const modifier = classifyStatusModifier(statusCode);
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(protocol, 'protocol')}${escapeCodeHtml(gap)}${wrapHighlightedToken(statusCode, modifier)}${escapeCodeHtml(rest)}`;
    }

    const statusMatch = line.match(/^(\s*)(\d{3})(.*)$/);
    if (statusMatch) {
      const [, indent, statusCode, rest] = statusMatch;
      const modifier = classifyStatusModifier(statusCode);
      return `${escapeCodeHtml(indent)}${wrapHighlightedToken(statusCode, modifier)}${escapeCodeHtml(rest)}`;
    }

    return escapeCodeHtml(line);
  }

  function looksLikeJSONBlock(text) {
    const trimmed = text.trim();
    if (!trimmed) return false;
    const startsLikeJSON = (trimmed.startsWith('{') && trimmed.endsWith('}'))
      || (trimmed.startsWith('[') && trimmed.endsWith(']'));
    if (!startsLikeJSON) return false;

    try {
      JSON.parse(trimmed);
      return true;
    } catch {
      return false;
    }
  }

  function looksLikeSSE(text) {
    let hits = 0;
    for (const line of text.split('\n')) {
      if (/^(event|data|id|retry):/.test(line) && ++hits >= 2) return true;
    }
    return false;
  }

  function renderSSELine(line) {
    const fieldMatch = line.match(/^(event|data|id|retry)(:)(.*)/);
    if (fieldMatch) {
      const [, field, colon, value] = fieldMatch;
      const renderedField = wrapHighlightedToken(field + colon, 'sse-field');
      if (field === 'event') return renderedField + wrapHighlightedToken(value, 'sse-event-name');
      if (field === 'data' && value.trim()) {
        const trimmed = value.trim();
        const jsonLike = (trimmed[0] === '{' || trimmed[0] === '[');
        if (jsonLike) return renderedField + highlightSyntax(value, 'json');
      }
      return renderedField + escapeCodeHtml(value);
    }
    if (line.startsWith(':')) return wrapHighlightedToken(line, 'sse-comment');
    return escapeCodeHtml(line);
  }

  function leadingSpaceCount(line) {
    let i = 0;
    while (i < line.length && (line[i] === ' ' || line[i] === '\t')) i++;
    return i;
  }

  // 基于缩进配对识别可折叠区间。
  // rawLines: 字符串数组（折叠分析针对的"逻辑"行，索引与最终渲染行索引一一对应）
  // 返回 Map<startIndex, { endIndex, count }>，startIndex 指向打开 { 或 [ 的行；
  // endIndex 指向对应的 } 或 ] 行；count 为可折叠行数（不含起止行）。

  function computeFoldRegions(rawLines) {
    const regions = new Map();
    const stack = [];
    for (let i = 0; i < rawLines.length; i++) {
      const raw = rawLines[i] || '';
      const trimmedRight = raw.replace(/[,\s]+$/, '');
      const lastChar = trimmedRight.slice(-1);
      const isOpen = lastChar === '{' || lastChar === '[';
      const trimmedLeft = raw.trimStart();
      const firstChar = trimmedLeft[0];
      const isClose = firstChar === '}' || firstChar === ']';
      const indent = leadingSpaceCount(raw);

      if (isClose && stack.length) {
        // 找到匹配的同缩进 open
        for (let s = stack.length - 1; s >= 0; s--) {
          if (stack[s].indent === indent) {
            const opener = stack[s];
            const span = i - opener.index - 1;
            if (span >= 1) {
              regions.set(opener.index, { endIndex: i, count: span });
            }
            stack.length = s;
            break;
          }
        }
      }
      if (isOpen) {
        stack.push({ index: i, indent });
      }
    }
    return regions;
  }

  let foldIdCounter = 0;

  function nextFoldId() {
    foldIdCounter += 1;
    return `f${foldIdCounter}`;
  }

  function renderCodeLines(lines, foldRegions) {
    const contentHtml = (line, suffix = '') => `<span class="code-line-content">${line || ''}${suffix}</span>`;
    if (!foldRegions || foldRegions.size === 0) {
      return lines.map(line => `<span class="code-line">${contentHtml(line)}</span>`).join('');
    }
    // 为每个区间生成 id；保留每行的 ancestor region ids 列表（开区间 s < i < e）。
    const startToId = new Map();
    const regionList = []; // {id, start, end, count}
    for (const [startIdx, info] of foldRegions.entries()) {
      const id = nextFoldId();
      startToId.set(startIdx, { id, count: info.count });
      regionList.push({ id, start: startIdx, end: info.endIndex, count: info.count });
    }
    const ancestorIdsAt = (i) => {
      const ids = [];
      for (const r of regionList) {
        if (r.start < i && i < r.end) ids.push(r.id);
      }
      return ids;
    };

    const out = [];
    for (let i = 0; i < lines.length; i++) {
      const content = lines[i] || '';
      const ancestors = ancestorIdsAt(i);
      const regionAttr = ancestors.length ? ` data-fold-region="${ancestors.join(' ')}"` : '';
      const startMeta = startToId.get(i);
      if (startMeta) {
        const { id, count } = startMeta;
        const summary = `<span class="code-fold-summary" data-fold-summary-for="${id}">…${count} lines</span>`;
        const toggle = `<button type="button" class="code-fold-toggle" data-fold-toggle="${id}" aria-expanded="true" aria-label="toggle code fold"></button>`;
        out.push(`<span class="code-line code-line--foldable" data-fold-id="${id}"${regionAttr}>${toggle}${contentHtml(content, summary)}</span>`);
        continue;
      }
      out.push(`<span class="code-line"${regionAttr}>${contentHtml(content)}</span>`);
    }
    return out.join('');
  }

  function renderUpstreamRequestOrResponse(text, mode) {
    const lines = String(text || '').split('\n');
    if (lines.length === 0) return '';

    const separatorIndex = lines.findIndex(line => line === '');
    const headerEnd = separatorIndex === -1 ? lines.length : separatorIndex;
    const renderedLines = [];
    const rawForFold = []; // 与 renderedLines 同索引，仅用于折叠分析；header 区填空字符串避免参与配对

    renderedLines.push(mode === 'response' ? renderStatusLine(lines[0]) : renderRequestLine(lines[0]));
    rawForFold.push('');

    for (let i = 1; i < headerEnd; i++) {
      renderedLines.push(renderHeaderLine(lines[i]));
      rawForFold.push('');
    }

    if (separatorIndex !== -1) {
      renderedLines.push('');
      rawForFold.push('');
      const bodyLines = lines.slice(separatorIndex + 1);
      const bodyText = bodyLines.join('\n');
      const bodyRenderedLines = looksLikeJSONBlock(bodyText)
        ? renderHighlightedLines(bodyText, 'json')
        : looksLikeSSE(bodyText)
          ? bodyLines.map(renderSSELine)
          : bodyLines.map(escapeCodeHtml);
      bodyRenderedLines.forEach((line, index) => {
        const rawLine = bodyLines[index] || '';
        renderedLines.push(line);
        rawForFold.push(rawLine);
      });
    }

    return renderCodeLines(renderedLines, computeFoldRegions(rawForFold));
  }

  function renderUpstreamCodeBlock(text, mode = 'text') {
    const value = String(text || '');
    if (!value) return '';

    switch (mode) {
      case 'request':
      case 'response':
        return renderUpstreamRequestOrResponse(value, mode);
      case 'json': {
        const rawLines = value.split('\n');
        return renderCodeLines(renderHighlightedLines(value, 'json'), computeFoldRegions(rawLines));
      }
      case 'url':
        return renderCodeLines(value.split('\n').map(renderRequestLine));
      case 'status':
        return renderCodeLines(value.split('\n').map(renderStatusLine));
      default:
        return renderCodeLines(value.split('\n').map(escapeCodeHtml));
    }
  }

  function setHighlightedCodeContent(target, text, mode = 'text') {
    const el = typeof target === 'string' ? document.getElementById(target) : target;
    if (!el) return;
    el._rawText = text || '';
    el.innerHTML = renderUpstreamCodeBlock(text || '', mode);
  }

  // 全局折叠按钮事件委托（仅绑定一次）。
  // 任何使用 setHighlightedCodeContent 渲染的 pre 都自动支持折叠。
  if (typeof document !== 'undefined' && typeof document.addEventListener === 'function'
      && !document.__codeFoldDelegated) {
    document.__codeFoldDelegated = true;
    document.addEventListener('click', (e) => {
      const foldBtn = e.target.closest('.code-fold-toggle');
      if (!foldBtn) return;
      e.preventDefault();
      e.stopPropagation();
      const id = foldBtn.dataset.foldToggle;
      if (!id) return;
      const startLine = foldBtn.closest('.code-line--foldable');
      if (!startLine) return;
      const collapsed = startLine.classList.toggle('code-line--collapsed');
      foldBtn.setAttribute('aria-expanded', collapsed ? 'false' : 'true');
      const pre = startLine.closest('pre');
      const root = pre || document;
      root.querySelectorAll(`[data-fold-region~="${id}"]`).forEach(el => {
        el.classList.toggle('code-line--hidden', collapsed);
      });
    });
  }

  const SENSITIVE_HEADER_RE = /^(authorization|x-refresh-token|x-api-key|api-key|x-goog-api-key|proxy-authorization)$/i;

  function maskHeaderValue(v) {
    if (typeof v !== 'string' || v.length <= 8) return '******';
    return v.slice(0, 4) + '******' + v.slice(-4);
  }

  function maskSensitiveHeaders(headers) {
    if (!headers || typeof headers !== 'object') return headers;
    const out = {};
    for (const [key, value] of Object.entries(headers)) {
      if (SENSITIVE_HEADER_RE.test(key)) {
        out[key] = Array.isArray(value) ? value.map(maskHeaderValue) : maskHeaderValue(value);
      } else {
        out[key] = value;
      }
    }
    return out;
  }

  window.maskSensitiveHeaders = maskSensitiveHeaders;
  window.copyToClipboard = copyToClipboard;
  window.renderUpstreamCodeBlock = renderUpstreamCodeBlock;
  window.setHighlightedCodeContent = setHighlightedCodeContent;

})();
