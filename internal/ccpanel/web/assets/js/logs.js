const t = window.t;
const i18nText = window.i18nText || ((key, fallback) => fallback || key);

// ── 后端契约（ccpanel 迁移路由）─────────────────────────────────
// 列表/筛选选项/指标条走 /dashboard/* 镜像——与 /admin 同一批 handler，
// withWebAuth 两种角色都放行，api_token 身份下按 KeyHash 收敛；否则
// api_token 会话首个抓取 401 就会把整页弹回登录。调试目录文件与服务端
// 合并视图留在 /admin/debug-logs/{id}/*——{id} 是 started_at 的
// epoch 毫秒（日志行 id 同口径），目录无属主校验，不对 api_token 开放。
// 进行中的请求用 active-requests 列表的 start_time（同 UnixMilli）
// 反查目录，FNV 哈希 id 不能解析目录。
const LOGS_LIST_URL = '/dashboard/logs';
const LOGS_BOOTSTRAP_URL = '/dashboard/logs/bootstrap';
const LOGS_MODELS_URL = '/dashboard/models';
const LOGS_EXPORT_URL = '/admin/logs/export';
const LOGS_STATS_URL = '/dashboard/stats';
const debugLogUrl = (id) => `/admin/debug-logs/${encodeURIComponent(id)}`;
const debugLogFileUrl = (id, name) =>
  `${debugLogUrl(id)}/file/${String(name).split('/').map(encodeURIComponent).join('/')}`;
const debugLogMergedUrl = (id) => `${debugLogUrl(id)}/merged`;
const activeDebugLogUrl = (id) => `/admin/active-requests/${encodeURIComponent(id)}/debug-log`;
const activeAbortUrl = (id) => `/admin/active-requests/${encodeURIComponent(id)}/abort`;

// 失败阶段枚举（internal/debuglog/stages.go ErrStage* 快照）：
// 下拉静态候选；新阶段出现时可经 combobox 自定义输入提交。
const LOGS_ERROR_STAGES = [
  'http_read', 'http_decode', 'request_build', 'provider_stream', 'http_stream',
  'response_event', 'http_encode', 'client_disconnected', 'devin_connect',
  'devin_transport', 'rate_gate', 'token_limit', 'model_disabled'
];

// status 筛选是表达式语法（逗号 OR，单项 499|4xx|>=400|<300|!200|!2xx），
// 预设常用档；观察到的具体状态码由 mergeLogsFilterOptions 补进候选尾部。
const LOGS_STATUS_PRESETS = ['2xx', '4xx', '5xx', '499', '!2xx', '>=400'];

// ── 机器值 → 人性化标签（展示层映射，wire 值不变）───────────────
// error_stage 与 internal/debuglog/stages.go 的 ErrStage* 枚举一一对应，
// result 对应 debuglog Completion.Result。未知值兜底显示原值，原值始终
// 进 title 或标签括号供排障核对。
const LOGS_ERROR_STAGE_LABELS = {
  http_read:           ['logs.stage.httpRead',           '读取请求失败'],
  http_decode:         ['logs.stage.httpDecode',         '请求解码失败'],
  request_build:       ['logs.stage.requestBuild',       '请求构建失败'],
  provider_stream:     ['logs.stage.providerStream',     '上游流中断'],
  http_stream:         ['logs.stage.httpStream',         '响应下发失败'],
  response_event:      ['logs.stage.responseEvent',      '响应事件失败'],
  http_encode:         ['logs.stage.httpEncode',         '响应编码失败'],
  client_disconnected: ['logs.stage.clientDisconnected', '客户端断连'],
  devin_connect:       ['logs.stage.devinConnect',       '上游拒绝'],
  devin_transport:     ['logs.stage.devinTransport',     '上游传输中断'],
  rate_gate:           ['logs.stage.rateGate',           '速率闸门拦截'],
  token_limit:         ['logs.stage.tokenLimit',         '令牌准入拒绝'],
  model_disabled:      ['logs.stage.modelDisabled',      '模型已停用']
};

const LOGS_RESULT_LABELS = {
  completed:    ['logs.resultCompleted',    '完成'],
  failed:       ['logs.resultFailed',       '失败'],
  disconnected: ['logs.resultDisconnected', '断连'],
  aborted:      ['logs.resultAborted',      '中断']
};

// 常见状态码的一行含义提示（title 悬浮，不替代数字本体）。
const LOGS_STATUS_HINTS = {
  200: ['logs.statusHint.200', '成功'],
  400: ['logs.statusHint.400', '请求无效'],
  401: ['logs.statusHint.401', '鉴权失败'],
  403: ['logs.statusHint.403', '无权访问'],
  404: ['logs.statusHint.404', '接口不存在'],
  408: ['logs.statusHint.408', '请求超时'],
  409: ['logs.statusHint.409', '请求冲突'],
  413: ['logs.statusHint.413', '请求体超限'],
  422: ['logs.statusHint.422', '参数校验失败'],
  429: ['logs.statusHint.429', '限流拒绝'],
  499: ['logs.statusHint.499', '客户端断连'],
  500: ['logs.statusHint.500', '内部错误'],
  502: ['logs.statusHint.502', '上游网关错误'],
  503: ['logs.statusHint.503', '服务不可用'],
  504: ['logs.statusHint.504', '网关超时']
};

function logsErrorStageLabel(stage) {
  const item = LOGS_ERROR_STAGE_LABELS[stage];
  return item ? i18nText(item[0], item[1]) : String(stage || '');
}

// 筛选下拉里的人性化标签：「中文（原值）」——原值可搜可见，提交仍是原值。
function logsErrorStageOptionLabel(stage) {
  const label = logsErrorStageLabel(stage);
  return label === stage ? label : `${label}（${stage}）`;
}

function logsResultLabel(result) {
  const item = LOGS_RESULT_LABELS[result];
  return item ? i18nText(item[0], item[1]) : String(result || '');
}

function logsStatusHint(code) {
  const item = LOGS_STATUS_HINTS[Number(code)];
  return item ? i18nText(item[0], item[1]) : '';
}

// index.jsonl 的 message 契约：completed → "ok"；否则 "result[: error_stage]"。
function humanizeLogMessage(raw) {
  const text = String(raw || '');
  if (!text) return { text: '', title: '' };
  if (text === 'ok') return { text: logsResultLabel('completed'), title: text };
  const sep = text.indexOf(':');
  const result = (sep === -1 ? text : text.slice(0, sep)).trim();
  const stage = sep === -1 ? '' : text.slice(sep + 1).trim();
  const parts = [logsResultLabel(result)];
  if (stage) parts.push(logsErrorStageLabel(stage));
  return { text: parts.join(' · '), title: text };
}

// key_hash 是 16 位十六进制（SHA-256 前 8 字节）：列表截前 8 位，全量进 title。
function buildKeyHashDisplay(hash) {
  const h = String(hash || '');
  if (!h) return '<span style="color: var(--neutral-500);">-</span>';
  const short = h.length > 10 ? `${h.slice(0, 8)}…` : h;
  return `<code class="logs-api-key-text logs-mono-text" title="${escapeHtml(h)}">${escapeHtml(short)}</code>`;
}

let currentLogsPage = 1;
let logsPageSize = 100;
let totalLogsPages = 1;
let totalLogs = 0;
let currentLogsCustomTimeRange = null;
let authTokens = []; // 令牌列表
let logsModelCombobox = null; // 模型筛选组合框
let logsStatusCombobox = null; // 状态码筛选组合框
let logsErrorStageCombobox = null; // 失败阶段筛选组合框
let lastLogsHintData = null; // 最近一次列表响应的 {rejects}，供语言切换重渲
let lastLogsMetricsData = null; // 最近一次指标条响应（/admin/stats data），供语言切换重渲
window.availableLogsModels = []; // 可用模型列表
window.availableLogsStatusCodes = []; // 可用状态码列表
let logsExactModelValue = '';

let latestActiveRequests = []; // 缓存 ui.js 最近一次推送的活动请求，供 load() 即时刷新
let lastActiveRequestStates = null; // Map<id, fingerprint>：上次活跃请求状态，用于检测请求结束/上游重试
let logsLoadInFlight = false;
let logsLoadPending = false;
// logsLoadScheduled 已被 _scheduleLoadTimer 取代

// === 列显隐 ===
const LOGS_COL_STORAGE_KEY = 'ccload_logs_columns';

const LOG_COLUMNS = [
  { key: 'time',        cls: 'logs-col-time',        i18n: 'logs.colTime' },
  { key: 'ip',          cls: 'logs-col-ip',          i18n: 'logs.colIP' },
  { key: 'tokenDesc',   cls: 'logs-col-token-desc',  i18n: 'logs.colTokenDesc' },
  { key: 'apiKey',      cls: 'logs-col-api-key',     i18n: 'logs.colApiKey' },
  { key: 'model',       cls: 'logs-col-model',       i18n: 'common.model' },
  { key: 'account',     cls: 'logs-col-account',     i18n: 'logs.colAccount' },
  { key: 'status',      cls: 'logs-col-status',      i18n: 'logs.statusCode' },
  { key: 'timing',      cls: 'logs-col-timing',      i18n: 'logs.colTiming' },
  { key: 'speed',       cls: 'logs-col-speed',       i18n: 'logs.colSpeed' },
  { key: 'input',       cls: 'logs-col-input',       i18n: 'logs.colInput' },
  { key: 'output',      cls: 'logs-col-output',      i18n: 'logs.colOutput' },
  { key: 'cacheRead',   cls: 'logs-col-cache-read',  i18n: 'logs.colCacheRead' },
  { key: 'cacheWrite',  cls: 'logs-col-cache-write', i18n: 'logs.colCacheWrite' },
  { key: 'cacheUtil',   cls: 'logs-col-cache-util',  i18n: 'logs.colCacheUtil' },
  { key: 'cost',        cls: 'logs-col-cost',        i18n: 'logs.colCost' },
  { key: 'message',     cls: 'logs-col-message',     i18n: 'logs.colMessage' },
];

let colVisibility = {};
let colStyleEl = null;

function loadColVisibility() {
  try {
    const saved = localStorage.getItem(LOGS_COL_STORAGE_KEY);
    if (saved) {
      colVisibility = JSON.parse(saved);
      return;
    }
  } catch (_) { /* ignore */ }
  colVisibility = {};
}

function saveColVisibility() {
  const toSave = {};
  for (const col of LOG_COLUMNS) {
    if (colVisibility[col.key] === false) toSave[col.key] = false;
  }
  if (Object.keys(toSave).length === 0) {
    localStorage.removeItem(LOGS_COL_STORAGE_KEY);
  } else {
    localStorage.setItem(LOGS_COL_STORAGE_KEY, JSON.stringify(toSave));
  }
}

function isColVisible(key) {
  return colVisibility[key] !== false;
}

function applyColVisibility() {
  if (!colStyleEl) {
    colStyleEl = document.createElement('style');
    colStyleEl.id = 'logs-col-visibility';
    document.head.appendChild(colStyleEl);
  }
  const rules = [];
  for (const col of LOG_COLUMNS) {
    if (!isColVisible(col.key)) {
      rules.push(`.logs-table .${col.cls} { display: none !important; }`);
    }
  }
  colStyleEl.textContent = rules.join('\n');
}

function renderColToggleMenu() {
  const list = document.getElementById('colToggleList');
  if (!list) return;
  list.innerHTML = '';
  for (const col of LOG_COLUMNS) {
    const visible = isColVisible(col.key);
    const item = document.createElement('label');
    item.className = 'logs-col-toggle-item';
    item.dataset.colKey = col.key;
    item.dataset.visible = String(visible);
    item.innerHTML = `<span class="logs-col-toggle-check"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"><polyline points="20 6 9 17 4 12"/></svg></span><span>${escapeHtml(i18nText(col.i18n, col.i18n))}</span>`;
    item.addEventListener('click', (e) => {
      e.preventDefault();
      e.stopPropagation();
      const newVisible = !isColVisible(col.key);
      colVisibility[col.key] = newVisible;
      item.dataset.visible = String(newVisible);
      saveColVisibility();
      applyColVisibility();
    });
    list.appendChild(item);
  }
}

// 菜单用 position:fixed 挂在表格外定位，相对触发按钮右对齐弹出；
// 按钮在工具栏里（不随表格横向滚动），fixed 天然免疫容器裁剪。
function toggleColMenu(trigger) {
  const menu = document.getElementById('colToggleMenu');
  if (!menu) return;
  const isOpen = !menu.hidden;
  if (isOpen) {
    menu.hidden = true;
    return;
  }
  renderColToggleMenu();
  menu.hidden = false;

  if (trigger) {
    const rect = trigger.getBoundingClientRect();
    menu.style.top = (rect.bottom + 4) + 'px';
    menu.style.left = Math.max(8, rect.right - menu.offsetWidth) + 'px';
  }
}

function closeColMenuOnClickOutside(e) {
  const menu = document.getElementById('colToggleMenu');
  if (!menu || menu.hidden) return;
  if (menu.contains(e.target)) return;
  if (e.target.closest('[data-action="toggle-col-menu"]')) return;
  menu.hidden = true;
}

loadColVisibility();

function normalizeLogsFilterValue(value) {
  return String(value || '').trim().toLowerCase();
}

function logsFilterMatchesOption(value, options) {
  const normalizedValue = normalizeLogsFilterValue(value);
  if (!normalizedValue) return false;

  return (Array.isArray(options) ? options : []).some((option) => {
    const candidates = option && typeof option === 'object'
      ? [option.value, option.label]
      : [option];
    return candidates.some((candidate) => normalizeLogsFilterValue(candidate) === normalizedValue);
  });
}

function logsFilterMatchesExactValue(value, exactValue) {
  const normalizedValue = normalizeLogsFilterValue(value);
  return Boolean(normalizedValue) && normalizedValue === normalizeLogsFilterValue(exactValue);
}

function isExactLogsModelFilter(value) {
  return logsFilterMatchesOption(value, window.availableLogsModels || []) ||
    logsFilterMatchesExactValue(value, logsExactModelValue);
}

function getLogsModelFilterKey(value, values) {
  return (values && values.modelExact) || isExactLogsModelFilter(value) ? 'model' : 'model_like';
}

function rememberExactLogsFilters(filters = {}, urlParams = null) {
  const hasExactModel = urlParams
    ? urlParams.has('model')
    : filters.modelExact === true;

  logsExactModelValue = hasExactModel ? (filters.model || '') : '';
}

function normalizeLogsCustomTimeRange(range) {
  if (!range || typeof range !== 'object') return null;

  const startMs = Number(range.startMs ?? range.customStartTime);
  const endMs = Number(range.endMs ?? range.customEndTime);
  if (!Number.isFinite(startMs) || !Number.isFinite(endMs) || endMs <= startMs) {
    return null;
  }
  return {
    startMs: Math.trunc(startMs),
    endMs: Math.trunc(endMs),
    label: range.label || ''
  };
}

function appendLogsTimeRangeParams(params, filters) {
  const range = filters?.range || 'today';
  const query = typeof window.buildDateRangeQuery === 'function'
    ? window.buildDateRangeQuery(range, currentLogsCustomTimeRange)
    : `range=${encodeURIComponent(range)}`;
  new URLSearchParams(query).forEach((value, key) => {
    params.set(key, value);
  });
  return params;
}

let _scheduleLoadTimer = null;
function scheduleLoad() {
  if (_scheduleLoadTimer) clearTimeout(_scheduleLoadTimer);
  _scheduleLoadTimer = setTimeout(() => {
    _scheduleLoadTimer = null;
    load(true); // 自动刷新时跳过 loading 状态，避免闪烁
  }, 2000);
}

function toUnixMs(value) {
  if (value === undefined || value === null) return null;

  if (typeof value === 'number' && Number.isFinite(value)) {
    // 兼容：秒(10位) / 毫秒(13位)
    if (value > 1e12) return value;
    if (value > 1e9) return value * 1000;
    return value;
  }

  if (typeof value === 'string') {
    if (/^\d+$/.test(value)) {
      const n = parseInt(value, 10);
      if (!Number.isFinite(n)) return null;
      return n > 1e12 ? n : n * 1000;
    }
    const parsed = Date.parse(value);
    return Number.isNaN(parsed) ? null : parsed;
  }

  return null;
}

// 格式化字节数为可读形式（K/M/G）- 使用对数优化
function formatBytes(bytes) {
  if (bytes == null || bytes <= 0) return '';
  const UNITS = ['B', 'K', 'M', 'G'];
  const FACTOR = 1024;
  const i = Math.min(Math.floor(Math.log(bytes) / Math.log(FACTOR)), UNITS.length - 1);
  const value = bytes / Math.pow(FACTOR, i);
  return value.toFixed(i > 0 ? 1 : 0) + ' ' + UNITS[i];
}

function buildActiveRequestInfoContent(req) {
  const bytesInfo = formatBytes(req?.bytes_received);
  const hasBytes = !!bytesInfo;
  const infoDisplay = hasBytes
    ? t('logs.receivedBytes', { bytes: bytesInfo })
    : (req?.debug_log_available ? t('logs.upstreamDetails') : '-');
  const infoColor = hasBytes ? 'var(--success-600)' : 'var(--neutral-500)';
  const infoHtml = `<span style="color: ${infoColor};">${escapeHtml(infoDisplay)}</span>`;
  const activeRequestId = Number(req?.id);

  if (!req?.debug_log_available || !Number.isFinite(activeRequestId) || activeRequestId <= 0) {
    return infoHtml;
  }

  return `<span class="debug-log-link has-upstream-detail" data-active-request-id="${activeRequestId}" title="${escapeHtml(t('logs.debugLogTitle'))}">${infoHtml}</span>`;
}

// IP 地址掩码处理（隐藏最后两段）
function maskIP(ip) {
  if (!ip) return '';
  // 短地址（如 ::1 localhost）无需掩码
  if (ip.length <= 3) return ip;
  // IPv4: 192.168.1.100 -> 192.168.*.*
  if (ip.includes('.')) {
    const parts = ip.split('.');
    if (parts.length === 4) {
      return `${parts[0]}.${parts[1]}.*.*`;
    }
  }
  // IPv6: 简化处理，保留前两段
  if (ip.includes(':')) {
    const parts = ip.split(':');
    if (parts.length >= 2) {
      return `${parts[0]}:${parts[1]}::*`;
    }
  }
  return ip;
}

function clearActiveRequestsRows() {
  document.querySelectorAll('tr.pending-row').forEach(el => el.remove());
}

// 单上游下没有渠道/Key 轮换维度：指纹用 start_time，同一 id 重启一次
// 尝试即视为新轮次，触发一次日志刷新（完成的尝试已落索引）。
function activeRequestFingerprint(req) {
  return String(req?.start_time || '');
}

// index.jsonl 的 api 原值 → 探活端点协议（/admin/model-test 的 client_protocol）。
function apiToClientProtocol(api) {
  switch (api) {
    case 'openai-chat':
      return 'openai';
    case 'openai-responses':
    case 'responses-ws':
      return 'codex';
    default:
      return 'anthropic';
  }
}

function activeRequestStatusLabel(req) {
  switch (req?.upstream_status) {
    case 'receiving':
      return t('logs.upstreamStatusReceiving');
    case 'retrying':
      return t('logs.upstreamStatusRetrying');
    case 'requesting':
      return t('logs.upstreamStatusRequesting');
    default:
      return '-';
  }
}

function buildActiveRequestStatusHtml(req) {
  return `<span class="status-pending active-upstream-status">${escapeHtml(activeRequestStatusLabel(req))}</span>`;
}

// 生成流式标志HTML（公共函数，避免重复）
function getStreamFlagHtml(isStreaming) {
  const label = escapeHtml(t('logs.streamFlag'));
  return isStreaming
    ? `<span class="stream-flag" title="${escapeHtml(t('logs.streamFlagTip'))}">${label}</span>`
    : `<span class="stream-flag placeholder">${label}</span>`;
}

function buildTimingSeparatorHtml() {
  return '<span class="log-timing-separator" style="color: var(--neutral-400);">/</span>';
}

function buildFirstByteTimingHtml(seconds, text) {
  return `<span class="log-timing-first-byte" style="color: ${window.getFirstByteTimingColor(seconds)};">${text}</span>`;
}

function buildDurationTimingHtml(seconds, text) {
  return `<span class="log-timing-duration" style="color: ${window.getDurationTimingColor(seconds)};">${text}</span>`;
}

function buildActiveRequestTimingHtml(req, elapsedRaw, elapsedText) {
  if (!Number.isFinite(elapsedRaw)) return '-';

  const durationDisplay = buildDurationTimingHtml(elapsedRaw, `${elapsedText}s...`);
  if (req.is_streaming && req.client_first_byte_time > 0) {
    const firstByte = Number(req.client_first_byte_time);
    return `<span class="log-timing-pair">${buildFirstByteTimingHtml(firstByte, `${firstByte.toFixed(2)}s`)}${buildTimingSeparatorHtml()}${durationDisplay}</span>`;
  }
  return durationDisplay;
}

function normalizeReasoningTokens(value) {
  const n = Number(value);
  return Number.isFinite(n) && n > 0 ? Math.trunc(n) : 0;
}

function buildReasoningTokensBadge(reasoningTokens) {
  const tokens = normalizeReasoningTokens(reasoningTokens);
  if (tokens === 0) return '';
  const title = `${t('logs.tip.reasoningTokens')}: ${tokens}`;
  return `<sup class="thinking-effort-badge" title="${title}">${escapeHtml(String(tokens))}</sup>`;
}

// 判断两个模型名是否只是前缀/后缀写法不同，此类差异不算模型重定向：
// - gemini-3.6-flash-high vs gemini-3.6-flash（变体后缀）
// - vanchin/deepseek-v4-pro-0813 vs deepseek-v4-pro-0813（渠道前缀）
// - deepseek-v4-pro-ga-260813 vs deepseek-v4-pro-0813（共享首尾，中间展开）
function isPrefixOrSuffixVariant(model, actualModel) {
  if (!model || !actualModel) return false;
  const a = String(model).toLowerCase();
  const b = String(actualModel).toLowerCase();
  if (a === b) return true;
  const short = a.length <= b.length ? a : b;
  const long = a.length <= b.length ? b : a;
  if (long.startsWith(short) || long.endsWith(short)) return true;
  let prefixLen = 0;
  while (prefixLen < short.length && short[prefixLen] === long[prefixLen]) prefixLen++;
  let suffixLen = 0;
  while (suffixLen < short.length - prefixLen &&
         short[short.length - 1 - suffixLen] === long[long.length - 1 - suffixLen]) suffixLen++;
  return prefixLen > 0 && suffixLen > 0 && prefixLen + suffixLen === short.length;
}

// 模型列渲染「请求模型」tag；重定向时落点模型直接跟在 ↪ 后可见
// （原名仍进 tag 悬浮提示），WS 传输与推理 token 数以角标呈现。
function buildLogModelDisplay(model, actualModel, reasoningTokens, upstreamWebsocket) {
  if (!model) {
    return '<span style="color: var(--neutral-500);">-</span>';
  }

  const redirected = actualModel && actualModel !== model && !isPrefixOrSuffixVariant(model, actualModel);
  const tokens = normalizeReasoningTokens(reasoningTokens);
  const classes = ['model-tag'];
  const titleParts = [];
  if (redirected) {
    classes.push('model-redirected');
    titleParts.push(`${t('logs.tip.requestedModel')}: ${escapeHtml(model)}`);
    titleParts.push(`${t('logs.tip.actualModel')}: ${escapeHtml(actualModel)}`);
  }
  if (tokens > 0) {
    classes.push('model-thinking');
    titleParts.push(`${t('logs.tip.reasoningTokens')}: ${tokens}`);
  }
  const title = titleParts.length > 0 ? ` title="${titleParts.join('&#10;')}"` : '';
  const redirectTarget = redirected
    ? `<span class="model-redirect-arrow" aria-hidden="true">↪</span><span class="model-text model-text--actual">${escapeHtml(actualModel)}</span>`
    : '';
  const wsBadge = upstreamWebsocket === true
    ? `<sup class="log-channel-badge log-channel-websocket-badge" title="${escapeHtml(i18nText('logs.tip.upstreamWebsocket', '上游走 WebSocket 通道'))}">ws</sup>`
    : '';
  const badgeHtml = wsBadge || tokens > 0
    ? `<span class="model-badges">${wsBadge}${buildReasoningTokensBadge(tokens)}</span>`
    : '';

  return `<span class="model-display">
      <span class="${classes.join(' ')}"${title}>
        <span class="model-text">${escapeHtml(model)}</span>${redirectTarget}
      </span>
      ${badgeHtml}
    </span>`;
}

function getLogMobileLabels() {
  return {
    time: escapeHtml(t('logs.colTime')),
    ip: escapeHtml(t('logs.colIP')),
    tokenDesc: escapeHtml(t('logs.colTokenDesc')),
    apiKey: escapeHtml(t('logs.colApiKey')),
    model: escapeHtml(t('common.model')),
    account: escapeHtml(t('logs.colAccount')),
    status: escapeHtml(t('logs.statusCode')),
    timing: escapeHtml(t('logs.colTiming')),
    speed: escapeHtml(t('logs.colSpeed')),
    input: escapeHtml(t('logs.colInput')),
    output: escapeHtml(t('logs.colOutput')),
    cacheRead: escapeHtml(t('logs.colCacheRead')),
    cacheWrite: escapeHtml(t('logs.colCacheWrite')),
    cacheUtil: escapeHtml(t('logs.colCacheUtil')),
    cost: escapeHtml(t('logs.colCost')),
    message: escapeHtml(t('logs.colMessage'))
  };
}

function buildActiveRequestTokenDescDisplay(req) {
  const tokenId = Number(req?.token_id);
  if (!Number.isFinite(tokenId) || tokenId <= 0) return '';

  const token = (Array.isArray(authTokens) ? authTokens : [])
    .find(item => Number(item?.id) === tokenId);
  const label = token?.description || `Token #${tokenId}`;
  const labelText = String(label || '');
  const displayLabel = token?.description && labelText.length > 7
    ? `${labelText.slice(0, 3)}.${labelText.slice(-3)}`
    : labelText;
  return `<span title="${escapeHtml(label)}">${escapeHtml(displayLabel)}</span>`;
}

function formatLogTokenDescLabel(label) {
  const text = String(label || '');
  return text.length > 7 ? `${text.slice(0, 3)}.${text.slice(-3)}` : text;
}

function buildLogTokenDescDisplay(label) {
  const text = String(label || '');
  if (!text) return '<span style="color: var(--neutral-500);">-</span>';
  return `<span class="logs-token-desc-text" title="${escapeHtml(text)}">${escapeHtml(formatLogTokenDescLabel(text))}</span>`;
}

// 号池 lane 名归因：account 是终局 lane；switches>0 说明 failover 救回，
// 角标透出换号次数（细节在请求目录 meta.json 的 upstream_attempts）。
function buildAccountDisplay(account, switches) {
  const name = String(account || '');
  if (!name) return '';
  const count = Number(switches) || 0;
  const badge = count > 0
    ? `<sup class="logs-account-switches" title="${escapeHtml(t('logs.accountSwitchesTooltip', { count }))}">+${count}</sup>`
    : '';
  return `<span class="logs-mono-text" title="${escapeHtml(name)}">${escapeHtml(name)}</span>${badge}`;
}

// 后端 log_source 只产出 proxy/manual_test（面板探活行），无其他来源。
function renderLogSourceBadge(logSource) {
  if (logSource === 'manual_test') {
    return `<span class="log-source-badge log-source-badge--manual">${escapeHtml(t('logs.sourceManualTestBadge'))}</span>`;
  }
  return '';
}

function canInspectDebugLog(entry) {
  const isTokenSession = typeof window.isAPITokenRole === 'function' && window.isAPITokenRole();
  return !isTokenSession && Number(entry?.id) > 0;
}

function buildLogMessageContent(entry) {
  const sourceBadge = renderLogSourceBadge(entry.log_source || 'proxy');
  const msg = humanizeLogMessage(entry.message);
  const errorMessage = String(entry.error_message || '').trim();
  if (!sourceBadge && !msg.text && !errorMessage) {
    return '';
  }
  // 机器原文（"failed: devin_connect"）进 title，展示人性化短标签。
  const titleAttr = msg.title && msg.title !== msg.text ? ` title="${escapeHtml(msg.title)}"` : '';

  let inner;
  if (!canInspectDebugLog(entry)) {
    inner = `<span${titleAttr}>${escapeHtml(msg.text)}</span>`;
  } else {
    const logId = Number(entry?.id);
    const logIdAttr = Number.isFinite(logId) && logId > 0 ? ` data-log-id="${logId}"` : '';
    inner = `<span class="debug-log-link has-upstream-detail"${logIdAttr}${titleAttr}>${escapeHtml(msg.text)}</span>`;
  }
  // error_message 是首个失败点的原始错误文案（仅失败行有值）：行内截
  // ~80 字符，悬停 title 看全文，避免把 ~300B 长串铺满单元格。
  const errorHtml = errorMessage
    ? `<span class="log-error-text" title="${escapeHtml(errorMessage)}">${escapeHtml(errorMessage.length > 80 ? `${errorMessage.slice(0, 80)}…` : errorMessage)}</span>`
    : '';
  return `${sourceBadge}${inner}${errorHtml}`;
}

function getLogCostInfo(entry) {
  const standardCost = Number(entry?.cost) || 0;
  if (standardCost <= 0) return null;
  // 目录价即成交价：cost_multiplier 恒 1、effective_cost 不投影，无倍率双价。
  return getCostDisplayInfo(standardCost, standardCost);
}

function formatLogCostFormulaValue(value) {
  const numeric = Number(value);
  if (!Number.isFinite(numeric) || numeric === 0) return '$0';
  return `$${numeric.toFixed(9).replace(/\.?0+$/, '')}`;
}

function buildLogCostTooltip(entry, costInfo) {
  const breakdown = entry?.cost_breakdown;
  if (!costInfo || !breakdown) return '';

  const components = [
    ['logs.costTooltipInput', breakdown.input],
    ['logs.costTooltipOutput', breakdown.output],
    ['logs.costTooltipCacheRead', breakdown.cache_read],
    ['logs.costTooltipCacheWrite', breakdown.cache_write]
  ];
  return components.map(([labelKey, component]) => t('logs.costTooltipLine', {
    label: t(labelKey),
    price: formatLogCostFormulaValue(component?.price_per_million),
    quantity: (Number(component?.quantity) || 0).toLocaleString(),
    cost: formatLogCostFormulaValue(component?.cost)
  })).join('\n');
}

function buildLogCostDisplay(entry, costInfo = getLogCostInfo(entry)) {
  if (!costInfo) return '';
  return `<span class="log-cost"><span class="log-cost-effective">${formatCost(costInfo.standardCost)}</span></span>`;
}

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

function calculateLogSpeed(entry) {
  return calculateTokenSpeed(
    Number(entry?.output_tokens),
    Number(entry?.duration),
    entry?.is_streaming ? Number(entry?.first_byte_time) : 0
  );
}

async function load(skipLoading = false) {
  if (logsLoadInFlight) {
    logsLoadPending = true;
    return;
  }
  logsLoadInFlight = true;
  try {
    if (!skipLoading) {
      renderLogsLoading();
    }
    // 指标条与列表同一节拍刷新（自动刷新/筛选/翻页都经过 load）
    loadLogsMetrics();

    const params = buildLogsRequestParams();
    const response = await fetchAPIWithAuth(LOGS_LIST_URL + '?' + params.toString());
    if (!response.success) throw new Error(response.error || i18nText('logs.loadFailed', '无法加载请求日志'));

    const data = response.data || [];
    // 提示条（管线前拒绝事件环）与列表渲染同源更新。
    updateLogsListHint(response);

    // 把日志中出现的模型/状态码合并进筛选下拉（无需刷新页面）
    mergeLogsFilterOptions(data);

    // 精确计算总页数（基于后端返回的count字段）
    if (typeof response.count === 'number') {
      totalLogs = response.count;
      totalLogsPages = Math.ceil(totalLogs / logsPageSize) || 1;
    } else if (Array.isArray(data)) {
      // 降级方案：后端未返回count时使用旧逻辑
      if (data.length === logsPageSize) {
        totalLogsPages = Math.max(currentLogsPage + 1, totalLogsPages);
      } else if (data.length < logsPageSize && currentLogsPage === 1) {
        totalLogsPages = 1;
      } else if (data.length < logsPageSize) {
        totalLogsPages = currentLogsPage;
      }
    }

    updatePagination();

    // 自动刷新时，保存现有 pending 行以避免闪烁
    const pendingRows = skipLoading ? Array.from(document.querySelectorAll('tr.pending-row')) : [];

    renderLogs(data);

    // 立即恢复 pending 行（后续活动请求推送会再更新）
    if (skipLoading && pendingRows.length > 0) {
      const tbody = document.getElementById('tbody');
      const firstRow = tbody.firstChild;
      const fragment = document.createDocumentFragment();
      pendingRows.forEach(row => fragment.appendChild(row));
      tbody.insertBefore(fragment, firstRow);
    }

    // 第一页时用最近一次推送的数据即时刷新进行中请求（轮询由 ui.js 统一驱动）
    if (currentLogsPage === 1) {
      handleActiveRequestsData(latestActiveRequests);
    } else {
      lastActiveRequestStates = null;
      clearActiveRequestsRows();
    }

  } catch (error) {
    console.error('加载日志失败:', error);
    try { if (window.showError) window.showError(i18nText('logs.loadFailed', '无法加载请求日志')); } catch (_) { }
    renderLogsError();
    updateLogsListHint(null);
  } finally {
    logsLoadInFlight = false;
    if (logsLoadPending) {
      logsLoadPending = false;
      scheduleLoad();
    }
  }
}

// ── 列表提示条（rejects 事件环）────────────────────────────────
// 管线前拒绝（鉴权 401/并发 429/排空 503/读体中断）不产生调试目录、不进
// index.jsonl——用户在列表找这类失败天然扑空，提示条把事件环聚合成一行
// 说明并指向统计页（runtime-metrics 的 rejects 组同源）。
// rejects 作为 envelope 顶层 sibling 捎回（先例：
// /admin/active-requests 的 active_request_title_enabled）。
const LOGS_REJECT_HINT_WINDOW_MS = 15 * 60000;

function summarizeLogsRejects(rejects, windowMs) {
  const labels = {};
  (rejects?.labels || []).forEach((item) => {
    if (item && item.reason) labels[item.reason] = item.label || item.reason;
  });
  const recent = Array.isArray(rejects?.recent) ? rejects.recent : [];
  const sinceMs = Date.now() - windowMs;
  const byReason = {};
  let n = 0;
  recent.forEach((event) => {
    if (Number(event?.at) * 1000 <= sinceMs) return;
    n += 1;
    const reason = String(event?.reason || '');
    byReason[reason] = (byReason[reason] || 0) + 1;
  });
  const parts = Object.keys(byReason).map((key) => `${labels[key] || key} ${byReason[key]}`);
  return { n, parts };
}

function updateLogsListHint(response) {
  const el = document.getElementById('logsListHint');
  if (!el) return;
  const rejects = response?.rejects;
  lastLogsHintData = { rejects };

  const rej = summarizeLogsRejects(rejects, LOGS_REJECT_HINT_WINDOW_MS);
  if (!rej.n) {
    el.hidden = true;
    el.innerHTML = '';
    return;
  }
  const text = i18nText(
    'logs.rejectsHint',
    '近 15 分钟本地拒绝 {count} 条（{detail}）——管线前拒绝不进索引',
    { count: rej.n, detail: rej.parts.join(' · ') }
  );
  const link = i18nText('logs.rejectsGotoStats', '前往统计页');
  el.innerHTML = `<div class="logs-list-hint-item">${escapeHtml(text)} <a class="logs-hint-link" href="/web/stats.html">${escapeHtml(link)}</a></div>`;
  el.hidden = false;
}

// ── 当前总况指标条（/admin/stats 窗口聚合）─────────────────────
// 与统计页同源的 rollup 格子聚合，覆盖整个时间窗（不受列表索引尾部
// 读取窗限制）。筛选只透传端点支持的 api/model(_like)/auth_token_id——
// q/status/status_class/result/error_stage/log_source 是列表行级条件，
// 统计侧不生效；model 精确匹配只看生效模型，不含 response_model 维。
function buildLogsMetricsParams() {
  const filters = getLogsFilters();
  const params = new URLSearchParams();
  appendLogsTimeRangeParams(params, filters);
  // api=all 在列表侧是通配语义；stats 端点按精确匹配，传它会聚出空集
  if (filters.api && filters.api !== 'all') params.set('api', filters.api);
  if (filters.authToken) params.set('auth_token_id', filters.authToken);
  const model = String(filters.model || '').trim();
  if (model) params.set(getLogsModelFilterKey(model, filters), model);
  return params;
}

// 速率类数值：>=1000 走 K/M 缩写，小值留两位小数；0/非法显示占位符
function formatLogsMetricRate(value) {
  const n = Number(value);
  if (!Number.isFinite(n) || n <= 0) return '-';
  if (n >= 1000) return formatNumber(Math.round(n));
  if (n >= 10) return n.toFixed(1);
  return n.toFixed(2);
}

function logsMetricCard(label, value, sub, title, color) {
  return `<div class="runtime-metric-card" title="${escapeHtml(title)}">` +
    `<span class="runtime-metric-label">${escapeHtml(label)}</span>` +
    `<strong class="runtime-metric-value"${color ? ` style="color:${color};"` : ''}>${escapeHtml(value)}</strong>` +
    (sub ? `<span class="runtime-metric-sub">${escapeHtml(sub)}</span>` : '') +
    `</div>`;
}

function renderLogsMetrics(data) {
  const el = document.getElementById('logsMetrics');
  if (!el) return;
  lastLogsMetricsData = data;
  const stats = Array.isArray(data?.stats) ? data.stats : [];
  const durationSec = Number(data?.duration_seconds) || 0;
  const rpm = data?.rpm_stats || {};
  const recent = data?.recent || {};

  // 四张卡的副行统一为短窗均值：10s 与 1m 两个实时窗口（recent 环聚合，
  // 与主值同口径）。窗口内补充信息（峰值/均耗时/读写量）进 tooltip。
  const fmtSec = (v) => { const n = Number(v); return n > 0 ? n.toFixed(2) + 's' : '-'; };
  const fmtPct = (v) => { const n = Number(v); return n > 0 ? n.toFixed(1) + '%' : '-'; };
  const winSub = (key, fmt) => i18nText('logs.metricWinSub', '10s {a} · 1m {b}', {
    a: fmt(recent?.s10?.[key]),
    b: fmt(recent?.s60?.[key])
  });
  const titleExtra = (base, extra) => extra ? base + ' · ' + extra : base;

  // 逐模型行累计 token 总量；首字/耗时按成功数加权，gen_ms 是
  // Σ生成时长（stream 扣首字）——TPS 分母，同表格速度列合计口径。
  let inTok = 0, outTok = 0, crTok = 0, cwTok = 0, genMS = 0;
  let ttfbSum = 0, ttfbN = 0, durSum = 0, durN = 0;
  for (const e of stats) {
    inTok += Number(e?.total_input_tokens) || 0;
    outTok += Number(e?.total_output_tokens) || 0;
    crTok += Number(e?.total_cache_read_input_tokens) || 0;
    cwTok += Number(e?.total_cache_creation_input_tokens) || 0;
    genMS += Number(e?.gen_ms) || 0;
    const ok = Number(e?.success) || 0;
    const fbt = Number(e?.avg_first_byte_time_seconds) || 0;
    const dur = Number(e?.avg_duration_seconds) || 0;
    if (ok > 0 && fbt > 0) { ttfbSum += fbt * ok; ttfbN += ok; }
    if (ok > 0 && dur > 0) { durSum += dur * ok; durN += ok; }
  }

  const avgRpm = Number(rpm.avg_rpm) || 0;
  const rpmTitle = titleExtra(
    i18nText('logs.metricRpmTitle', '窗口内平均每分钟请求数（不含 499 断连）'),
    Number(rpm.peak_rpm) > 0
      ? i18nText('logs.metricPeakSub', '峰值 {value}', { value: formatLogsMetricRate(rpm.peak_rpm) })
      : ''
  );

  const tps = genMS > 0 ? outTok * 1000 / genMS : 0;
  const outRate = durationSec > 0 ? outTok / durationSec : 0;
  const tpsTitle = titleExtra(
    i18nText('logs.metricTpsTitle', 'Σ输出 token ÷ Σ生成时长（流式扣首字）——同表格 Tok/s 列口径'),
    outRate > 0
      ? i18nText('logs.metricOutputSub', '输出 {value}/s', { value: formatLogsMetricRate(outRate) })
      : ''
  );

  const ttfb = ttfbN > 0 ? ttfbSum / ttfbN : 0;
  const avgDur = durN > 0 ? durSum / durN : 0;
  const ttfbTitle = titleExtra(
    i18nText('logs.metricTtfbTitle', '流式 2xx 请求平均首字时间（按成功数加权）'),
    avgDur > 0
      ? i18nText('logs.metricAvgDurationSub', '均耗时 {value}s', { value: avgDur.toFixed(1) })
      : ''
  );

  const cacheDenom = inTok + crTok + cwTok;
  const cachePct = cacheDenom > 0 && crTok > 0 ? (crTok / cacheDenom) * 100 : 0;
  const cacheTitle = titleExtra(
    i18nText('logs.metricCacheTitle', '缓存读 ÷（输入+缓存读+缓存建），同表格缓存命中列口径'),
    (crTok > 0 || cwTok > 0)
      ? i18nText('logs.metricCacheSub', '读 {read} · 建 {write}', {
          read: formatNumber(crTok),
          write: formatNumber(cwTok)
        })
      : ''
  );

  el.innerHTML = `<div class="runtime-metrics-grid">` +
    logsMetricCard(
      i18nText('trend.typeRpm', 'RPM'),
      formatLogsMetricRate(avgRpm),
      winSub('rpm', formatLogsMetricRate),
      rpmTitle,
      avgRpm > 0 ? window.getRpmColor(avgRpm) : ''
    ) +
    logsMetricCard(
      i18nText('logs.metricTps', 'TPS'),
      formatLogsMetricRate(tps),
      winSub('tps', formatLogsMetricRate),
      tpsTitle,
      ''
    ) +
    logsMetricCard(
      i18nText('probe.firstByte', '首字'),
      ttfb > 0 ? ttfb.toFixed(2) + 's' : '-',
      winSub('ttfb_s', fmtSec),
      ttfbTitle,
      ttfb > 0 ? window.getFirstByteTimingColor(ttfb) : ''
    ) +
    logsMetricCard(
      i18nText('trend.cacheHitRate', '缓存命中率'),
      cachePct > 0 ? cachePct.toFixed(1) + '%' : '-',
      winSub('cache_pct', fmtPct),
      cacheTitle,
      ''
    ) +
    `</div>`;
  el.title = i18nText(
    'logs.metricsScopeTitle',
    '窗口聚合：时间范围与入口/模型/令牌筛选生效；搜索、状态码、结果、失败阶段、日志来源不参与统计'
  );
  el.hidden = false;
}

async function loadLogsMetrics() {
  if (!document.getElementById('logsMetrics')) return;
  try {
    const data = await fetchDataWithAuth(LOGS_STATS_URL + '?' + buildLogsMetricsParams().toString());
    renderLogsMetrics(data || {});
  } catch (error) {
    console.error('加载指标条失败:', error);
    // 保留上一帧数据，下个节拍自动重试
  }
}

// ── 导出 ──────────────────────────────────────────────────────
// /admin/logs/export 直出 CSV/JSON 文件（非 envelope）：响应头
// X-Truncated: true 表示命中超过单次扫描上限（2000 行）。
async function exportLogs(format) {
  const params = buildLogsRequestParams();
  params.delete('limit');
  params.delete('offset');
  params.set('format', format);
  try {
    const res = await fetchWithAuth(`${LOGS_EXPORT_URL}?${params.toString()}`);
    if (!res.ok) {
      let message = `HTTP ${res.status}`;
      try {
        const payload = await res.json();
        if (payload && payload.error) message = payload.error;
      } catch (_) { /* 非 JSON 错误体 */ }
      throw new Error(message);
    }
    const truncated = res.headers.get('X-Truncated') === 'true';
    const blob = await res.blob();
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = format === 'csv' ? 'requests.csv' : 'requests.json';
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
    if (truncated && window.showWarning) {
      window.showWarning(i18nText(
        'logs.exportTruncated',
        '导出结果已截断：命中超过扫描上限，请缩小时间窗或筛选条件'
      ));
    }
  } catch (error) {
    if (window.showError) {
      window.showError(i18nText('logs.exportFailed', '导出失败：{message}', { message: error.message || error }));
    }
  }
}

// 根据当前筛选条件过滤活跃请求
function filterActiveRequests(requests) {
  const filters = getLogsFilters();
  const model = normalizeLogsFilterValue(filters.model);
  const modelExact = filters.modelExact;
  const api = normalizeLogsFilterValue(filters.api);
  const tokenId = (document.getElementById('f_auth_token')?.value || '').trim();

  return requests.filter(req => {
    if (model) {
      const reqModel = normalizeLogsFilterValue(req.model || '');
      if (modelExact ? reqModel !== model : !reqModel.includes(model)) return false;
    }
    if (api && normalizeLogsFilterValue(req.api) !== api) return false;
    // 令牌ID精确匹配
    if (tokenId) {
      if (req.token_id === undefined || req.token_id === null || req.token_id === 0) return false;
      if (String(req.token_id) !== tokenId) return false;
    }
    return true;
  });
}

// 进行中的请求没有最终状态码/结果/失败阶段——这些维度任一命中时不再把
// 活跃行混进列表；model/api/token 仍由 filterActiveRequests 客户端侧过滤。
function shouldSkipActiveRequestsFetch(filters) {
  if (filters.range && filters.range !== 'today') return true;
  if (filters.status || filters.result || filters.errorStage) return true;
  return filters.logSource !== 'proxy' && filters.logSource !== 'all';
}

// 处理从 ui.js 推送的活动请求数据（不再自行发起网络请求）
function handleActiveRequestsData(rawActiveRequests) {
  latestActiveRequests = Array.isArray(rawActiveRequests) ? rawActiveRequests : [];

  // 非第一页不展示进行中请求
  if (currentLogsPage !== 1) {
    if (lastActiveRequestStates !== null) {
      lastActiveRequestStates = null;
      clearActiveRequestsRows();
    }
    return;
  }

  // 筛选条件不匹配时跳过
  if (shouldSkipActiveRequestsFetch(getLogsFilters())) {
    clearActiveRequestsRows();
    lastActiveRequestStates = null;
    return;
  }

  // 进行中的请求（尚未落库）的模型/状态码也补充进筛选下拉
  mergeLogsFilterOptions(latestActiveRequests);

  // 检测"需要刷新日志"：ID 消失（请求结束）或 fingerprint 变化（上游重试 → 上次尝试已写入日志）
  const currentStates = new Map();
  for (const req of latestActiveRequests) {
    if (req && (req.id !== undefined && req.id !== null)) {
      currentStates.set(String(req.id), activeRequestFingerprint(req));
    }
  }
  if (lastActiveRequestStates !== null) {
    let needRefresh = false;
    for (const [id, lastFp] of lastActiveRequestStates) {
      const currentFp = currentStates.get(id);
      if (currentFp === undefined) {
        needRefresh = true; // 请求消失 = 已结束
        break;
      }
      if (lastFp && currentFp && lastFp !== currentFp) {
        needRefresh = true; // 同 ID 的 start_time 变了 = 上次尝试已写日志
        break;
      }
    }
    if (needRefresh && currentLogsPage === 1) {
      scheduleLoad();
    }
  }
  lastActiveRequestStates = currentStates;

  // 根据当前筛选条件过滤（只影响展示，不影响完成检测）
  const activeRequests = filterActiveRequests(latestActiveRequests);

  renderActiveRequests(activeRequests);
}

// 已触发中断但尚未看到上游换轮的请求：id -> 触发时的 start_time（毫秒）。
// 中断生效后代理会切换渠道并重置 start_time，届时按钮恢复可点。
const abortingActiveRequests = new Map();

function buildActiveRequestAbortHtml(req, id, startMs) {
  if (!req.abortable) return '';
  const pending = abortingActiveRequests.has(id);
  const label = pending
    ? (typeof t === 'function' ? t('logs.aborting') : '中断中')
    : (typeof t === 'function' ? t('logs.abort') : '中断');
  return `<button type="button" class="logs-abort-btn" data-abort-request-id="${escapeHtml(id)}"`
    + ` data-abort-start="${startMs || 0}"${pending ? ' disabled' : ''}>${escapeHtml(label)}</button>`;
}

// 渲染进行中的请求（按 ID diff 更新，避免无意义的 DOM churn）
function renderActiveRequests(activeRequests) {
  const tbody = document.getElementById('tbody');
  if (!tbody) return;

  const activeIds = new Set();
  const totalCols = getTableColspan();
  const logMobileLabels = getLogMobileLabels();
  const firstNonPending = tbody.querySelector('tr:not(.pending-row)');

  for (const req of (activeRequests || [])) {
    const id = String(req.id);
    activeIds.add(id);

    const startMs = toUnixMs(req.start_time);
    // 中断已生效（上游换了一轮）就撤掉「中断中」，让操作员能中断新的尝试
    if (abortingActiveRequests.has(id) && abortingActiveRequests.get(id) !== startMs) {
      abortingActiveRequests.delete(id);
    }
    const elapsedRaw = startMs ? Math.max(0, (Date.now() - startMs) / 1000) : null;
    const elapsed = elapsedRaw !== null ? elapsedRaw.toFixed(1) : '-';
    const streamFlag = getStreamFlagHtml(req.is_streaming);

    const durationDisplay = startMs ? buildActiveRequestTimingHtml(req, elapsedRaw, elapsed) : '-';

    const statusDisplay = buildActiveRequestStatusHtml(req);
    const modelDisplay = buildLogModelDisplay(req.model, '', req.reasoning_tokens, req.upstream_websocket);
    const tokenDescDisplay = buildActiveRequestTokenDescDisplay(req);
    const tokenDescCellClass = `logs-col-token-desc${tokenDescDisplay ? '' : ' mobile-empty-cell'}`;
    const abortDisplay = buildActiveRequestAbortHtml(req, id, startMs);
    const speedCellClass = `logs-col-speed${abortDisplay ? '' : ' mobile-empty-cell'}`;
    const accountDisplay = buildAccountDisplay(req.account, req.account_switches);
    const accountCellClass = `logs-col-account${accountDisplay ? '' : ' mobile-empty-cell'}`;

    // Key显示（key_hash 截断 + title 全量，与完成行同口径）
    const keyDisplay = buildKeyHashDisplay(req.api_key_used);

    const infoContent = buildActiveRequestInfoContent(req);

    let existingRow = tbody.querySelector(`tr.pending-row[data-req-id="${id}"]`);

    if (existingRow) {
      // 更新现有行的动态字段
      const timingCell = existingRow.querySelector('.logs-col-timing');
      if (timingCell) timingCell.innerHTML = `${durationDisplay} ${streamFlag}`;
      const statusCell = existingRow.querySelector('.logs-col-status');
      if (statusCell) statusCell.innerHTML = statusDisplay;
      // failover 换号会改写 req.account，每轮同步
      const accountCell = existingRow.querySelector('.logs-col-account');
      if (accountCell) {
        accountCell.innerHTML = accountDisplay;
        accountCell.classList.toggle('mobile-empty-cell', !accountDisplay);
      }
      const compactStatus = existingRow.querySelector('.active-upstream-status');
      if (compactStatus && !statusCell) compactStatus.textContent = activeRequestStatusLabel(req);
      const msgCell = existingRow.querySelector('.logs-col-message');
      if (msgCell) msgCell.innerHTML = infoContent;
      // 中断入口随上游重试与中断状态变化，必须每轮重画
      const speedCell = existingRow.querySelector('.logs-col-speed');
      if (speedCell) {
        speedCell.innerHTML = abortDisplay;
        speedCell.classList.toggle('mobile-empty-cell', !abortDisplay);
      } else {
        const compactAbort = existingRow.querySelector('.active-abort-slot');
        if (compactAbort) compactAbort.innerHTML = abortDisplay;
      }
      const compactAccount = existingRow.querySelector('.active-account-slot');
      if (compactAccount) compactAccount.innerHTML = accountDisplay;
    } else {
      // 创建新行
      const row = document.createElement('tr');
      row.className = 'mobile-card-row pending-row';
      row.setAttribute('data-req-id', id);
      if (totalCols < 8) {
        row.innerHTML = `
            <td colspan="${totalCols}">
              ${statusDisplay}
              <span style="margin-left: 8px;">${formatTime(req.start_time)}</span>
              <span class="logs-mono-text" style="margin-left: 8px;" title="${escapeHtml(req.client_ip || '')}">${escapeHtml(maskIP(req.client_ip) || '-')}</span>
              <span style="margin-left: 8px;">${modelDisplay}</span>
              <span class="active-account-slot" style="margin-left: 8px;">${accountDisplay}</span>
              <span style="margin-left: 8px;">${durationDisplay} ${streamFlag}</span>
              <span style="margin-left: 8px;">${infoContent}</span>
              <span class="active-abort-slot" style="margin-left: 8px;">${abortDisplay}</span>
            </td>
          `;
      } else {
        row.innerHTML = `
            <td class="logs-col-time" data-mobile-label="${logMobileLabels.time}" style="white-space: nowrap;">${formatTime(req.start_time)}</td>
            <td class="logs-col-ip logs-mono-text" data-mobile-label="${logMobileLabels.ip}" style="white-space: nowrap;" title="${escapeHtml(req.client_ip || '')}">${escapeHtml(maskIP(req.client_ip) || '-')}</td>
            <td class="${tokenDescCellClass}" data-mobile-label="${logMobileLabels.tokenDesc}" style="white-space: nowrap;">${tokenDescDisplay}</td>
            <td class="logs-col-api-key" data-mobile-label="${logMobileLabels.apiKey}" style="text-align: center; white-space: nowrap;">${keyDisplay}</td>
            <td class="logs-col-model" data-mobile-label="${logMobileLabels.model}">${modelDisplay}</td>
            <td class="${accountCellClass}" data-mobile-label="${logMobileLabels.account}" style="white-space: nowrap;">${accountDisplay}</td>
            <td class="logs-col-status" data-mobile-label="${logMobileLabels.status}">${statusDisplay}</td>
            <td class="logs-col-timing" data-mobile-label="${logMobileLabels.timing}" style="text-align: right; white-space: nowrap;">${durationDisplay} ${streamFlag}</td>
            <td class="${speedCellClass}" data-mobile-label="${logMobileLabels.speed}" style="text-align: right; white-space: nowrap;">${abortDisplay}</td>
            <td class="logs-col-input mobile-empty-cell" data-mobile-label="${logMobileLabels.input}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-output mobile-empty-cell" data-mobile-label="${logMobileLabels.output}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cache-read mobile-empty-cell" data-mobile-label="${logMobileLabels.cacheRead}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cache-write mobile-empty-cell" data-mobile-label="${logMobileLabels.cacheWrite}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cache-util mobile-empty-cell" data-mobile-label="${logMobileLabels.cacheUtil}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-cost mobile-empty-cell" data-mobile-label="${logMobileLabels.cost}" style="text-align: right; white-space: nowrap;"></td>
            <td class="logs-col-message" data-mobile-label="${logMobileLabels.message}">${infoContent}</td>
          `;
      }
      tbody.insertBefore(row, firstNonPending);
    }
  }

  // 移除已消失的 pending 行
  tbody.querySelectorAll('tr.pending-row').forEach(row => {
    if (!activeIds.has(row.getAttribute('data-req-id'))) {
      row.remove();
    }
  });
  // 请求结束后清理中断标记，避免 Map 无限增长
  for (const id of abortingActiveRequests.keys()) {
    if (!activeIds.has(id)) abortingActiveRequests.delete(id);
  }
}

// 中断运行中请求的当前上游尝试（服务端按上游连接重置处理，随后正常故障切换）
async function abortActiveRequest(button) {
  const id = button.dataset.abortRequestId;
  if (!id || abortingActiveRequests.has(id)) return;

  const confirmMsg = (typeof t === 'function' ? t('logs.abortConfirm') : '') || '确定中断这个进行中的请求吗？将按上游网络故障处理。';
  if (!confirm(confirmMsg)) return;

  abortingActiveRequests.set(id, Number(button.dataset.abortStart) || 0);
  button.disabled = true;
  button.textContent = (typeof t === 'function' ? t('logs.aborting') : '中断中') || '中断中';

  try {
    const { payload } = await fetchAPIWithAuthRaw(activeAbortUrl(id), { method: 'POST' });
    if (!payload.success) throw new Error(payload.error || i18nText('logs.abortFailed', '中断失败'));
  } catch (e) {
    // 中断没打出去就恢复按钮，否则这一行会永远卡在「中断中」
    abortingActiveRequests.delete(id);
    alert(e.message || i18nText('logs.abortFailed', '中断失败'));
  }
}

// ✅ 动态计算列数（避免硬编码维护成本）——按可见列计，列显隐与
// colspan/紧凑布局保持同步。
function getTableColspan() {
  return LOG_COLUMNS.reduce((n, col) => n + (isColVisible(col.key) ? 1 : 0), 0) || 16;
}

function formatCacheUtilRate(inputTokens, cacheReadTokens, cacheCreationTokens) {
  const i = Number(inputTokens) || 0;
  const r = Number(cacheReadTokens) || 0;
  const c = Number(cacheCreationTokens) || 0;
  const denom = i + r + c;
  if (denom <= 0 || r <= 0) return '';
  const pct = (r / denom) * 100;
  return `<span class="token-metric-value" style="color: var(--success-600);">${pct.toFixed(1)}%</span>`;
}

// buildCacheCreationDisplay 渲染缓存建列，5m 分桶角标按实际数据判定，不看
// 模型名或协议——走 codex 协议的 gpt 模型同样会上报分桶，用模型名判断会把
// 真实分桶吞掉。1h 分桶投影端从不赋值（恒 0），不渲染。
function buildCacheCreationDisplay(entry) {
  const total = entry.cache_creation_input_tokens || 0;
  if (total <= 0) return '';

  const badge = (entry.cache_5m_input_tokens || 0) > 0
    ? ' <sup style="color: var(--primary-500); font-size: 0.75em; font-weight: 600;">5m</sup>'
    : '';
  return `<span class="token-metric-value" style="color: var(--primary-600);">${total.toLocaleString()}${badge}</span>`;
}

function renderLogsLoading() {
  displayedLogs = null;
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const loadingRow = TemplateEngine.render('tpl-log-loading', { colspan });
  tbody.innerHTML = '';
  if (loadingRow) tbody.appendChild(loadingRow);
}

function renderLogsError() {
  displayedLogs = null;
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const errorRow = TemplateEngine.render('tpl-log-error', { colspan });
  tbody.innerHTML = '';
  if (errorRow) tbody.appendChild(errorRow);
}

let displayedLogs = null;

function renderLogs(data) {
  displayedLogs = data;
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const logMobileLabels = getLogMobileLabels();

  if (data.length === 0) {
    const emptyRow = TemplateEngine.render('tpl-log-empty', { colspan });
    tbody.innerHTML = '';
    if (emptyRow) tbody.appendChild(emptyRow);
    return;
  }

  // 性能优化：直接拼接 HTML 字符串，避免逐行调用 TemplateEngine.render
  const htmlParts = new Array(data.length);

  for (let i = 0; i < data.length; i++) {
    const entry = data[i];
    // === 预处理数据：构建复杂HTML片段 ===

    // 0. 客户端IP显示（掩码处理，hover显示完整IP）
    const clientIPDisplay = entry.client_ip ?
      `<span title="${escapeHtml(entry.client_ip)}">${escapeHtml(maskIP(entry.client_ip))}</span>` :
      '<span style="color: var(--neutral-400);">-</span>';

    // 0.5. API访问令牌描述
    const tokenDescDisplay = buildLogTokenDescDisplay(entry.auth_token_description);

    // 2. 状态码样式（数字本体不动，含义进 title）
    const statusClass = (entry.status_code >= 200 && entry.status_code < 300) ?
      'status-success' : 'status-error';
    const statusCode = entry.status_code;
    const statusHint = logsStatusHint(statusCode);
    const statusTitleAttr = statusHint ? ` title="${escapeHtml(statusHint)}"` : '';

    // 3. 模型显示（重定向落点在 tag 悬浮提示里）；非 2xx 行给探活入口——
    // /admin/model-test 仅 admin 可达且消耗上游配额，api_token 会话不渲染入口
    const displayedActualModel = entry.actual_model || entry.response_model;
    const modelDisplay = buildLogModelDisplay(entry.model, displayedActualModel, entry.reasoning_tokens, entry.upstream_websocket);
    const isTokenSession = typeof window.isAPITokenRole === 'function' && window.isAPITokenRole();
    const probeDisplay = !(statusCode >= 200 && statusCode < 300) && entry.model && !isTokenSession
      ? `<button type="button" class="test-key-btn" data-probe-model="${escapeHtml(entry.model)}" data-probe-api="${escapeHtml(entry.api || '')}" title="${escapeHtml(t('logs.probeModel'))}"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><path d="M13 2L4 14H11L9 22L20 10H13L13 2Z" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg></button>`
      : '';

    // 4. 响应时间显示(流式/非流式)
    const hasDuration = entry.duration !== undefined && entry.duration !== null;
    const durationDisplay = hasDuration ?
      buildDurationTimingHtml(entry.duration, entry.duration.toFixed(2)) :
      '<span style="color: var(--neutral-500);">-</span>';

    const streamFlag = getStreamFlagHtml(entry.is_streaming);

    let responseTimingDisplay;
    if (entry.is_streaming) {
      const hasFirstByte = entry.first_byte_time !== undefined && entry.first_byte_time !== null;
      const firstByteDisplay = hasFirstByte ?
        buildFirstByteTimingHtml(entry.first_byte_time, entry.first_byte_time.toFixed(2)) :
        '<span class="log-timing-first-byte" style="color: var(--neutral-500);">-</span>';
      responseTimingDisplay = `<span class="log-timing-pair">${firstByteDisplay}${buildTimingSeparatorHtml()}${durationDisplay}</span>${streamFlag}`;
    } else {
      responseTimingDisplay = `<span class="log-timing-pair">${durationDisplay}</span>${streamFlag}`;
    }

    const logSpeed = calculateLogSpeed(entry);
    const speedDisplay = logSpeed === null
      ? ''
      : `<span class="token-metric-value" style="color: var(--neutral-700);">${logSpeed.toFixed(1)}</span>`;

    // 5. Key 哈希显示（本服务 api_key_used 就是 key_hash，截断 + title 全量）
    const apiKeyDisplay = buildKeyHashDisplay(entry.api_key_used);

    // 6. Token统计显示(0值为空)
    const tokenValue = (value, color) => {
      if (value === undefined || value === null || value === 0) return '';
      return `<span class="token-metric-value" style="color: ${color};">${value.toLocaleString()}</span>`;
    };
    const inputTokensDisplay = tokenValue(entry.input_tokens, 'var(--neutral-700)');
    const outputTokensDisplay = tokenValue(entry.output_tokens, 'var(--neutral-700)');
    const cacheReadDisplay = tokenValue(entry.cache_read_input_tokens, 'var(--success-600)');

    // 缓存建列
    const cacheCreationDisplay = buildCacheCreationDisplay(entry);

    // 7. 成本显示
    const costInfo = getLogCostInfo(entry);
    const costDisplay = buildLogCostDisplay(entry, costInfo);
    const costTitle = buildLogCostTooltip(entry, costInfo);
    const costTitleAttr = costTitle ? ` title="${escapeHtml(costTitle)}"` : '';
    const cacheUtilDisplay = formatCacheUtilRate(
      entry.input_tokens,
      entry.cache_read_input_tokens,
      entry.cache_creation_input_tokens
    );
    const messageContent = buildLogMessageContent(entry);
    const accountDisplay = buildAccountDisplay(entry.account, entry.account_switches);

    // === 直接拼接行 HTML ===
    htmlParts[i] = `<tr class="mobile-card-row logs-table-row">
          <td class="logs-col-time" data-mobile-label="${logMobileLabels.time}" style="white-space: nowrap;">${formatTime(entry.time)}</td>
          <td class="logs-col-ip logs-mono-text" data-mobile-label="${logMobileLabels.ip}" style="white-space: nowrap;">${clientIPDisplay}</td>
          <td class="logs-col-token-desc" data-mobile-label="${logMobileLabels.tokenDesc}" style="white-space: nowrap;">${tokenDescDisplay}</td>
          <td class="logs-col-api-key" data-mobile-label="${logMobileLabels.apiKey}" style="text-align: center; white-space: nowrap;">${apiKeyDisplay}</td>
          <td class="logs-col-model" data-mobile-label="${logMobileLabels.model}">${modelDisplay} ${probeDisplay}</td>
          <td class="logs-col-account${accountDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.account}" style="white-space: nowrap;">${accountDisplay}</td>
          <td class="logs-col-status" data-mobile-label="${logMobileLabels.status}"><span class="${statusClass}"${statusTitleAttr}>${statusCode}</span></td>
          <td class="logs-col-timing" data-mobile-label="${logMobileLabels.timing}" style="text-align: right; white-space: nowrap;">${responseTimingDisplay}</td>
          <td class="logs-col-speed${speedDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.speed}" style="text-align: right; white-space: nowrap;">${speedDisplay}</td>
          <td class="logs-col-input${inputTokensDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.input}" style="text-align: right; white-space: nowrap;">${inputTokensDisplay}</td>
          <td class="logs-col-output${outputTokensDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.output}" style="text-align: right; white-space: nowrap;">${outputTokensDisplay}</td>
          <td class="logs-col-cache-read${cacheReadDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cacheRead}" style="text-align: right; white-space: nowrap;">${cacheReadDisplay}</td>
          <td class="logs-col-cache-write${cacheCreationDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cacheWrite}" style="text-align: right; white-space: nowrap;">${cacheCreationDisplay}</td>
          <td class="logs-col-cache-util${cacheUtilDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cacheUtil}" style="text-align: right; white-space: nowrap;">${cacheUtilDisplay}</td>
          <td class="logs-col-cost${costDisplay ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.cost}"${costTitleAttr} style="text-align: right; white-space: nowrap;">${costDisplay}</td>
          <td class="logs-col-message${messageContent ? '' : ' mobile-empty-cell'}" data-mobile-label="${logMobileLabels.message}" style="max-width: 300px; word-break: break-word;">${messageContent}</td>
        </tr>`;
  }

  // 一次性替换 tbody 内容
  tbody.innerHTML = htmlParts.join('');
}

function updatePagination() {
  // 更新页码显示（只更新底部分页）
  const currentPage2El = document.getElementById('logs_current_page2');
  const totalPages2El = document.getElementById('logs_total_pages2');
  const first2El = document.getElementById('logs_first2');
  const prev2El = document.getElementById('logs_prev2');
  const next2El = document.getElementById('logs_next2');
  const last2El = document.getElementById('logs_last2');
  const jumpPageInput = document.getElementById('logs_jump_page');

  if (currentPage2El) currentPage2El.textContent = currentLogsPage;
  if (totalPages2El) totalPages2El.textContent = totalLogsPages;

  // 更新跳转输入框的max属性
  if (jumpPageInput) {
    jumpPageInput.max = totalLogsPages;
    jumpPageInput.placeholder = `1-${totalLogsPages}`;
  }

  // 更新按钮状态（只更新底部分页）
  const prevDisabled = currentLogsPage <= 1;
  const nextDisabled = currentLogsPage >= totalLogsPages;

  if (first2El) first2El.disabled = prevDisabled;
  if (prev2El) prev2El.disabled = prevDisabled;
  if (next2El) next2El.disabled = nextDisabled;
  if (last2El) last2El.disabled = nextDisabled;
}

function firstLogsPage() {
  if (currentLogsPage > 1) {
    currentLogsPage = 1;
    load();
  }
}

function prevLogsPage() {
  if (currentLogsPage > 1) {
    currentLogsPage--;
    load();
  }
}

function nextLogsPage() {
  if (currentLogsPage < totalLogsPages) {
    currentLogsPage++;
    load();
  }
}

function lastLogsPage() {
  if (currentLogsPage < totalLogsPages) {
    currentLogsPage = totalLogsPages;
    load();
  }
}

function jumpToPage() {
  const jumpPageInput = document.getElementById('logs_jump_page');
  if (!jumpPageInput) return;

  const targetPage = parseInt(jumpPageInput.value);

  // 输入验证
  if (isNaN(targetPage) || targetPage < 1 || targetPage > totalLogsPages) {
    jumpPageInput.value = ''; // 清空无效输入
    if (window.showError) {
      try {
        window.showError(t('logs.invalidPage', { total: totalLogsPages }));
      } catch (_) { }
    }
    return;
  }

  // 跳转到目标页
  if (targetPage !== currentLogsPage) {
    currentLogsPage = targetPage;
    load();
  }

  // 清空输入框
  jumpPageInput.value = '';
}

function applyFilter() {
  currentLogsPage = 1;
  totalLogsPages = 1;

  window.persistFilterState({
    key: LOGS_FILTER_KEY,
    values: getLogsFilters(),
    search: location.search,
    pathname: location.pathname,
    fields: LOGS_FILTER_FIELDS,
    preserveExistingParams: true
  });
  load();
}

function getDefaultLogsFilters() {
  if (window.FilterState && typeof window.FilterState.restore === 'function') {
    return window.FilterState.restore({
      search: '',
      savedFilters: null,
      fields: LOGS_FILTER_FIELDS
    });
  }

  return LOGS_FILTER_FIELDS.reduce((values, field) => {
    values[field.key] = Object.prototype.hasOwnProperty.call(field, 'defaultValue')
      ? field.defaultValue
      : '';
    return values;
  }, {});
}

async function resetLogsFilters() {
  const defaults = getDefaultLogsFilters();

  currentLogsCustomTimeRange = null;
  currentLogsPage = 1;
  totalLogsPages = 1;
  rememberExactLogsFilters({
    ...defaults,
    modelExact: false
  });

  applyLogsFilterValues(defaults);
  await loadLogsFilterOptions(defaults.range || 'today');
  syncLogSourceVisibility();

  window.persistFilterState({
    key: LOGS_FILTER_KEY,
    values: getLogsFilters(),
    search: location.search,
    pathname: location.pathname,
    fields: LOGS_FILTER_FIELDS,
    preserveExistingParams: true,
    historyMethod: 'replaceState'
  });
  load();
}

function applyLogsFilterValues(filters) {
  window.applyFilterControlValues(filters, {
    range: 'f_hours',
    api: 'f_api',
    logSource: 'f_log_source',
    authToken: 'f_auth_token',
    result: 'f_result'
  });

  // 模型通过 combobox 恢复
  if (logsModelCombobox && filters.model !== undefined) {
    logsModelCombobox.setValue(filters.model || '', filters.model || t('trend.allModels'));
  }

  if (logsStatusCombobox && filters.status !== undefined) {
    logsStatusCombobox.setValue(filters.status || '', filters.status || t('logs.allStatusCodes'));
  }

  if (logsErrorStageCombobox && filters.errorStage !== undefined) {
    const stage = String(filters.errorStage || '').trim();
    // 已选阶段显示人性化标签（原值在括号里），非枚举的自定义输入原样显示。
    logsErrorStageCombobox.setValue(
      stage,
      stage ? logsErrorStageOptionLabel(stage) : i18nText('logs.allErrorStages', '全部阶段')
    );
  }

}

function getLogSourceFilterElements() {
  const select = document.getElementById('f_log_source');
  if (!select) {
    return { group: null, select: null };
  }

  let group = null;
  if (typeof select.closest === 'function') {
    group = select.closest('.filter-group');
  }
  if (!group) {
    group = select.parentElement || null;
  }

  return { group, select };
}

function syncLogSourceVisibility() {
  const { group, select } = getLogSourceFilterElements();
  if (!group || !select) return false;

  if (window.isAPITokenRole()) {
    group.hidden = true;
    select.value = 'proxy';
    return false;
  }

  group.hidden = false;
  return true;
}

async function loadLogsFilterOptions(range) {
  try {
    const params = new URLSearchParams();
    const r = range || document.getElementById('f_hours')?.value || 'today';
    appendLogsTimeRangeParams(params, { range: r });
    const resp = await fetchDataWithAuth(LOGS_MODELS_URL + '?' + params.toString()) || {};
    const rawModels = Array.isArray(resp.models) ? resp.models : [];
    const rawStatusCodes = Array.isArray(resp.status_codes) ? resp.status_codes : [];

    window.availableLogsModels = [...new Set(rawModels)];
    window.availableLogsStatusCodes = [...new Set(rawStatusCodes
      .map(Number)
      .filter(code => Number.isInteger(code) && code >= 100 && code <= 999))];
    if (logsModelCombobox) logsModelCombobox.refresh();
    if (logsStatusCombobox) logsStatusCombobox.refresh();
  } catch (error) {
    console.error('加载日志筛选选项失败:', error);
  }
}

// 从日志/活跃请求数据中提取请求模型与状态码，去重合并进筛选下拉。
// 根因：bootstrap 的 distinct 集合滞后于刚落库或进行中的请求，
// 导致列表里能看到的模型/状态码在下拉里缺失，必须刷新页面才更新。
// 此处做到“所见即可筛选”，无需刷新。
function mergeLogsFilterOptions(entries) {
  if (!Array.isArray(entries) || entries.length === 0) return;

  const models = Array.isArray(window.availableLogsModels) ? window.availableLogsModels : [];
  const knownModels = new Set(models);
  const statusCodes = Array.isArray(window.availableLogsStatusCodes) ? window.availableLogsStatusCodes : [];
  const knownStatusCodes = new Set(statusCodes);
  let changed = false;

  for (const entry of entries) {
    const model = String(entry?.model || '').trim();
    if (model && !knownModels.has(model)) {
      knownModels.add(model);
      models.push(model);
      changed = true;
    }
    const statusCode = Number(entry?.status_code);
    if (Number.isInteger(statusCode) && statusCode >= 100 && statusCode <= 999 && !knownStatusCodes.has(statusCode)) {
      knownStatusCodes.add(statusCode);
      statusCodes.push(statusCode);
      changed = true;
    }
  }

  if (!changed) return;
  window.availableLogsModels = models;
  window.availableLogsStatusCodes = statusCodes.sort((a, b) => a - b);
  if (logsModelCombobox) logsModelCombobox.refresh();
  if (logsStatusCombobox) logsStatusCombobox.refresh();
}

function initLogsModelCombobox(initialValue) {
  if (typeof window.createSearchableCombobox !== 'function') return;
  if (!document.getElementById('f_model')) return;
  logsModelCombobox = window.createSearchableCombobox({
    inputId: 'f_model',
    dropdownId: 'f_model_dropdown',
    attachMode: true,
    initialValue: initialValue || '',
    initialLabel: initialValue || t('trend.allModels'),
    allowCustomInput: true,
    commitEmptyAsFirst: true,
    getOptions: () => [
      { value: '', label: t('trend.allModels') },
      ...(window.availableLogsModels || []).map(m => ({ value: m, label: m }))
    ],
    onSelect: () => {
      applyFilter();
    }
  });
}

function initLogsStatusCombobox(initialValue) {
  if (typeof window.createSearchableCombobox !== 'function') return;
  if (!document.getElementById('f_status')) return;
  logsStatusCombobox = window.createSearchableCombobox({
    inputId: 'f_status',
    dropdownId: 'f_status_dropdown',
    attachMode: true,
    initialValue: initialValue || '',
    initialLabel: initialValue || t('logs.allStatusCodes'),
    // 状态表达式（200 / 4xx / >=400 / !2xx / 逗号 OR）——自定义输入放行
    allowCustomInput: true,
    commitEmptyAsFirst: true,
    getOptions: () => {
      const seen = new Set(LOGS_STATUS_PRESETS);
      return [
        { value: '', label: t('logs.allStatusCodes') },
        ...LOGS_STATUS_PRESETS.map(v => ({ value: v, label: v })),
        ...(window.availableLogsStatusCodes || [])
          .map(String)
          .filter(code => !seen.has(code))
          .map(code => ({ value: code, label: code }))
      ];
    },
    onSelect: () => {
      applyFilter();
    }
  });
}

// 失败阶段筛选：候选来自 ErrStage 枚举快照（LOGS_ERROR_STAGES），
// allowCustomInput 让尚未进枚举的新阶段也能直接输入提交。
function initLogsErrorStageCombobox(initialValue) {
  if (typeof window.createSearchableCombobox !== 'function') return;
  if (!document.getElementById('f_error_stage')) return;
  logsErrorStageCombobox = window.createSearchableCombobox({
    inputId: 'f_error_stage',
    dropdownId: 'f_error_stage_dropdown',
    attachMode: true,
    initialValue: initialValue || '',
    initialLabel: initialValue
      ? logsErrorStageOptionLabel(initialValue)
      : i18nText('logs.allErrorStages', '全部阶段'),
    allowCustomInput: true,
    commitEmptyAsFirst: true,
    getOptions: () => [
      { value: '', label: i18nText('logs.allErrorStages', '全部阶段') },
      ...LOGS_ERROR_STAGES.map(stage => ({ value: stage, label: logsErrorStageOptionLabel(stage) }))
    ],
    onSelect: () => {
      applyFilter();
    }
  });
}

async function initFilters(restoredFilters, preloaded) {
  const range = restoredFilters.range || 'today';
  const authToken = restoredFilters.authToken || '';

  window.initSavedDateRangeFilter({
    selectId: 'f_hours',
    defaultValue: 'today',
    restoredValue: range,
    includeCustom: true,
    customRange: currentLogsCustomTimeRange,
    customPickerContainerId: 'f_hours_custom_range_host',
    onChange: async (nextRange, customRange) => {
      if (nextRange === 'custom') {
        currentLogsCustomTimeRange = normalizeLogsCustomTimeRange(customRange);
      } else {
        currentLogsCustomTimeRange = null;
      }
      currentLogsPage = 1;
      totalLogsPages = 1;
      await loadLogsFilterOptions(nextRange);
      applyFilter();
    }
  });

  initLogsModelCombobox(restoredFilters.model || '');
  initLogsStatusCombobox(restoredFilters.status || '');
  initLogsErrorStageCombobox(restoredFilters.errorStage || '');
  applyLogsFilterValues(restoredFilters);
  const apiSelect = document.getElementById('f_api');
  if (apiSelect) {
    apiSelect.addEventListener('change', applyFilter);
  }
  document.getElementById('f_result')?.addEventListener('change', applyFilter);
  document.getElementById('btn_export_csv')?.addEventListener('click', () => exportLogs('csv'));
  document.getElementById('btn_export_json')?.addEventListener('click', () => exportLogs('json'));
  syncLogSourceVisibility();
  const [tokens] = await Promise.all([
    window.initAuthTokenFilter({
      selectId: 'f_auth_token',
      value: authToken,
      onChange: () => {
        window.persistFilterState({
          key: LOGS_FILTER_KEY,
          getValues: getLogsFilters
        });
        currentLogsPage = 1;
        load();
      },
      ...(preloaded ? { preloadedTokens: preloaded.authTokens } : {})
    }),
    preloaded ? Promise.resolve() : loadLogsFilterOptions(range)
  ]);
  authTokens = tokens;

  // 事件监听
  document.getElementById('btn_filter').addEventListener('click', applyFilter);
  document.getElementById('btn_clear_filters')?.addEventListener('click', resetLogsFilters);
  document.getElementById('f_log_source')?.addEventListener('change', applyFilter);

  window.bindFilterApplyInputs({
    apply: applyFilter,
    enterInputIds: ['f_hours', 'f_api', 'f_auth_token', 'f_log_source', 'f_result']
  });
}

function initLogsPageActions() {
  if (typeof window.initDelegatedActions === 'function') {
    window.initDelegatedActions({
      boundKey: 'logsPageActionsBound',
      click: {
        'first-logs-page': () => firstLogsPage(),
        'prev-logs-page': () => prevLogsPage(),
        'next-logs-page': () => nextLogsPage(),
        'last-logs-page': () => lastLogsPage(),
        'close-debug-log-modal': () => closeDebugLogModal(),
        'toggle-col-menu': (el) => toggleColMenu(el)
      }
    });
  }

  const jumpPageInput = document.getElementById('logs_jump_page');
  if (jumpPageInput && !jumpPageInput.dataset.bound) {
    jumpPageInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        jumpToPage();
      }
    });
    jumpPageInput.dataset.bound = '1';
  }
}

// 性能优化：避免 toLocaleString 的开销，使用手动格式化
function formatTime(timeStr) {
  try {
    const ts = toUnixMs(timeStr);
    if (!ts) return '-';

    const d = new Date(ts);
    if (isNaN(d.getTime()) || d.getFullYear() < 2020) {
      return '-';
    }

    // 手动格式化：MM-DD HH:mm:ss
    const M = String(d.getMonth() + 1).padStart(2, '0');
    const D = String(d.getDate()).padStart(2, '0');
    const h = String(d.getHours()).padStart(2, '0');
    const m = String(d.getMinutes()).padStart(2, '0');
    const s = String(d.getSeconds()).padStart(2, '0');
    return `${M}-${D} ${h}:${m}:${s}`;
  } catch (e) {
    return '-';
  }
}

// 注销功能（已由 ui.js 的 onLogout 统一处理）

// localStorage key for logs page filters
const LOGS_FILTER_KEY = 'logs.filters';
const LOGS_FILTER_FIELDS = [
  { key: 'range', queryKeys: ['range'], defaultValue: 'today' },
  {
    key: 'customStartTime',
    queryKeys: ['start_time'],
    defaultValue: '',
    includeInQuery(value, values) {
      return values?.range === 'custom' && Boolean(value);
    },
    includeInRequest() {
      return false;
    }
  },
  {
    key: 'customEndTime',
    queryKeys: ['end_time'],
    defaultValue: '',
    includeInQuery(value, values) {
      return values?.range === 'custom' && Boolean(value);
    },
    includeInRequest() {
      return false;
    }
  },
  { key: 'api', queryKeys: ['api'], defaultValue: '' },
  {
    key: 'model',
    queryKeys: ['model', 'model_like'],
    paramKey: getLogsModelFilterKey,
    requestKey: getLogsModelFilterKey,
    defaultValue: ''
  },
  { key: 'logSource', queryKeys: ['log_source'], requestKey: 'log_source', defaultValue: 'proxy' },
  // status 是表达式参数（499|4xx|>=400|!2xx 逗号 OR）；status_code/status_class
  // 仅作旧链接/旧本地存档的恢复入口，请求一律发 status——同给时后端 status 赢。
  { key: 'status', queryKeys: ['status', 'status_code', 'status_class'], defaultValue: '' },
  { key: 'result', queryKeys: ['result'], defaultValue: '' },
  { key: 'errorStage', queryKeys: ['error_stage'], defaultValue: '' },
  { key: 'authToken', queryKeys: ['auth_token_id'], defaultValue: '' }
];

function getLogsFilters() {
  const { group: logSourceGroup, select: logSourceSelect } = getLogSourceFilterElements();
  const logSource = !logSourceSelect || (logSourceGroup && logSourceGroup.hidden)
    ? 'proxy'
    : (logSourceSelect.value || 'proxy').trim();
  const model = logsModelCombobox ? logsModelCombobox.getValue() : (document.getElementById('f_model')?.value || '').trim();
  const status = logsStatusCombobox ? logsStatusCombobox.getValue() : (document.getElementById('f_status')?.value || '').trim();
  const errorStage = logsErrorStageCombobox
    ? String(logsErrorStageCombobox.getValue() || '').trim()
    : (document.getElementById('f_error_stage')?.value || '').trim();
  const baseValues = window.readFilterControlValues({
    range: { id: 'f_hours', defaultValue: 'today', trim: true },
    api: { id: 'f_api', trim: true },
    authToken: { id: 'f_auth_token', trim: true },
    result: { id: 'f_result', trim: true }
  });
  const hasCustomRange = baseValues.range === 'custom' && currentLogsCustomTimeRange;

  return {
    ...baseValues,
    customStartTime: hasCustomRange ? String(currentLogsCustomTimeRange.startMs) : '',
    customEndTime: hasCustomRange ? String(currentLogsCustomTimeRange.endMs) : '',
    model,
    status,
    errorStage,
    modelExact: isExactLogsModelFilter(model),
    logSource
  };
}

function buildLogsRequestParams() {
  const params = window.FilterQuery.buildRequestParams(getLogsFilters(), LOGS_FILTER_FIELDS, {
    baseParams: {
      limit: logsPageSize.toString(),
      offset: ((currentLogsPage - 1) * logsPageSize).toString()
    }
  });
  appendLogsTimeRangeParams(params, getLogsFilters());
  return params;
}

// 列显隐的绑定不依赖网络，提到 bootstrap 之外：run() 要等
// /dashboard/session 返回才执行，慢会话期间按钮曾是死的。
initLogsPageActions();
applyColVisibility();
document.addEventListener('click', closeColMenuOnClickOutside);

// ESC键关闭模态框与列显隐菜单——同在 bootstrap 之前绑定，慢会话下也可用
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const colMenu = document.getElementById('colToggleMenu');
    if (colMenu && !colMenu.hidden) {
      colMenu.hidden = true;
      return;
    }
    const debugModal = document.getElementById('debugLogModal');
    if (debugModal && debugModal.classList.contains('show')) {
      closeDebugLogModal();
      return;
    }
    // 探活模态 DOM 懒注入：未打开过时不存在，直接调 close 会 null.classList。
    const modelTestModal = document.getElementById('modelTestModal');
    if (modelTestModal?.classList.contains('show') && typeof window.closeModelTestModal === 'function') {
      window.closeModelTestModal();
    }
  }
});

// 页面初始化
window.initPageBootstrap({
  topbarKey: 'logs',
  run: async () => {
  // 优先从 URL 读取，其次从 localStorage 恢复，默认 all
  const u = new URLSearchParams(location.search);
  const hasUrlParams = u.toString().length > 0;
  const savedFilters = window.FilterState.load(LOGS_FILTER_KEY);
  const restoredFilters = window.FilterState.restore({
    search: location.search,
    savedFilters,
    fields: LOGS_FILTER_FIELDS
  });
  currentLogsCustomTimeRange = restoredFilters.range === 'custom'
    ? normalizeLogsCustomTimeRange(restoredFilters)
    : null;
  if (restoredFilters.range === 'custom' && !currentLogsCustomTimeRange) {
    restoredFilters.range = 'today';
  }
  rememberExactLogsFilters({
    ...restoredFilters,
    modelExact: !hasUrlParams && savedFilters?.modelExact === true
  }, hasUrlParams ? u : null);
  // 构造 bootstrap 请求参数（和 loadLogsFilterOptions 一致）
  const bootstrapParams = new URLSearchParams();
  appendLogsTimeRangeParams(bootstrapParams, { range: restoredFilters.range || 'today' });

  // Wave 1：bootstrap 合并页面初始化请求
  const bootstrap = await fetchDataWithAuth(LOGS_BOOTSTRAP_URL + '?' + bootstrapParams.toString()).catch(() => null);

  // 从 bootstrap 数据应用设置（bootstrap 失败时各字段回退到原有 fetch 路径）
  if (bootstrap) {
    window.availableLogsModels = [...new Set(bootstrap.models || [])];
    window.availableLogsStatusCodes = [...new Set((bootstrap.status_codes || [])
      .map(Number)
      .filter(code => Number.isInteger(code) && code >= 100 && code <= 999))];
    if (logsModelCombobox) logsModelCombobox.refresh();
    if (logsStatusCombobox) logsStatusCombobox.refresh();
  }

  // Wave 2：initFilters（有预加载则跳过内部 fetch）
  await initFilters(restoredFilters, bootstrap ? {
    authTokens: bootstrap.auth_tokens || []
  } : undefined);

  if (!hasUrlParams && savedFilters) {
    window.persistFilterState({
      values: getLogsFilters(),
      pathname: location.pathname,
      fields: LOGS_FILTER_FIELDS,
      historyMethod: 'replaceState'
    });
  }

  load();

  // 订阅 ui.js 的活动请求推送（全站唯一轮询源，可见性由 ui.js 统一管理）
  if (typeof window.onActiveRequestsData === 'function') {
    window.onActiveRequestsData(handleActiveRequestsData);
  }

  // 列表自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用；
  // 隐藏/弹窗时跳过，回前台补一轮）——与 index/stats 同一 helper。
  if (typeof window.createAutoRefresh === 'function') {
    window.createAutoRefresh({ load: () => load(true) }).init();
  }

  // 事件委托：处理日志表格中的按钮点击
  const tbody = document.getElementById('tbody');
  if (tbody) {
    tbody.addEventListener('click', (e) => {
      // 运行中请求手动中断
      const abortBtn = e.target.closest('.logs-abort-btn[data-abort-request-id]');
      if (abortBtn) {
        abortActiveRequest(abortBtn);
        return;
      }

      // 运行中请求 Debug log 查看
      const activeDebugLink = e.target.closest('.debug-log-link[data-active-request-id]');
      if (activeDebugLink) {
        const activeRequestId = parseInt(activeDebugLink.dataset.activeRequestId, 10);
        if (Number.isFinite(activeRequestId) && activeRequestId > 0) {
          showActiveDebugLogModal(activeRequestId);
        }
        return;
      }

      // Debug log 查看
      const debugLink = e.target.closest('.debug-log-link[data-log-id]');
      if (debugLink) {
        const logId = parseInt(debugLink.dataset.logId, 10);
        if (Number.isFinite(logId) && logId > 0) {
          showDebugLogModal(logId);
        }
        return;
      }

      // 非 2xx 行的模型探活入口（按该行实际入口协议预填）
      const probeBtn = e.target.closest('[data-probe-model]');
      if (probeBtn) {
        if (typeof window.openModelTestModal === 'function') {
          window.openModelTestModal({
            model: probeBtn.dataset.probeModel || '',
            clientProtocol: apiToClientProtocol(probeBtn.dataset.probeApi)
          });
        }
        return;
      }
    });
  }
  }
});

// 处理 bfcache（后退/前进缓存）：页面从缓存恢复时重新加载筛选条件
window.addEventListener('pageshow', async function (event) {
  if (event.persisted) {
    // 页面从 bfcache 恢复，重新同步筛选器状态
    const savedFilters = window.FilterState.load(LOGS_FILTER_KEY);
    if (savedFilters) {
      const restoredFilters = window.FilterState.restore({
        search: '',
        savedFilters,
        fields: LOGS_FILTER_FIELDS
      });
      currentLogsCustomTimeRange = restoredFilters.range === 'custom'
        ? normalizeLogsCustomTimeRange(restoredFilters)
        : null;
      if (restoredFilters.range === 'custom' && !currentLogsCustomTimeRange) {
        restoredFilters.range = 'today';
      }
      rememberExactLogsFilters({
        ...restoredFilters,
        modelExact: savedFilters.modelExact === true
      });

      // 重新加载令牌列表并设置值
      authTokens = await window.loadAuthTokensIntoSelect('f_auth_token');
      if (restoredFilters.authToken) {
        document.getElementById('f_auth_token').value = restoredFilters.authToken;
      }

      document.getElementById('f_hours').value = restoredFilters.range || 'today';
      await loadLogsFilterOptions(restoredFilters.range || 'today');
      applyLogsFilterValues(restoredFilters);
      syncLogSourceVisibility();

      // 重新加载数据
      currentLogsPage = 1;
      load();
    }
  }
});


// ============================================================================
// Debug Log Modal
// ============================================================================

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

// debugFileContext 记录当前模态框对应的可解析目录 id（started_at epoch
// 毫秒——files/merged 端点的 {id}）与已打开文件名/大小；活跃请求模态框的
// log_id 是 FNV 哈希不能解析目录，fileId 由活跃列表 start_time 反查。
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
  modal.classList.add('show');

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

function closeDebugLogModal() {
  stopActiveDebugLogPolling();
  setDebugLogStatus(null);
  currentDebugLogData = null;
  resetDebugMergedResponses();
  resetDebugFileView();
  debugFileContext = null;
  document.getElementById('debugLogModal').classList.remove('show');
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

// ── Files 页签：调试目录文件清单 ──────────────────────────────────
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
    return `<button type="button" class="debug-file-item${open ? ' active' : ''}" data-debug-file="${escapeHtml(name)}">`
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
      i18nText('logs.debugFileNoDir', '无法定位调试目录（请求可能刚结束或已清理）'),
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

// Tab switch + copy button delegation for debug log modal.
// 部分测试桩只提供最小 document API，这里避免在脚本加载阶段就假定完整 DOM 存在。
if (typeof document !== 'undefined' && typeof document.addEventListener === 'function') {
  document.addEventListener('click', (e) => {
    const tab = e.target.closest('#debugLogModal .upstream-tab');
    if (tab) {
      activateDebugTab(tab.dataset.tab);
      return;
    }

    const mergeBtn = e.target.closest('#debugLogModal [data-action="merge-debug-response"]');
    if (mergeBtn) {
      const tab = activeDebugResponseTab();
      if (tab) setDebugResponseMergedVisible(!debugMergedStates[tab].visible, tab);
      return;
    }

    const wrapBtn = e.target.closest('#debugLogModal [data-action="toggle-debug-wrap"]');
    if (wrapBtn) {
      setDebugLogWrapEnabled(!debugLogWrapEnabled);
      return;
    }

    const fileItem = e.target.closest('#debugLogModal [data-debug-file]');
    if (fileItem) {
      void toggleDebugFile(fileItem.dataset.debugFile);
      return;
    }

    const fileRawBtn = e.target.closest('#debugLogModal [data-action="copy-debug-file-raw"]');
    if (fileRawBtn) {
      void copyDebugFileRaw(fileRawBtn);
      return;
    }

    const fileOpenBtn = e.target.closest('#debugLogModal [data-action="open-debug-file-raw"]');
    if (fileOpenBtn) {
      void openDebugFileRaw();
      return;
    }

    const copyBtn = e.target.closest('#debugLogModal .upstream-copy-btn');
    if (copyBtn) {
      const targetId = copyBtn.dataset.copyTarget;
      const pre = document.getElementById(targetId);
      if (!pre) return;
      const text = pre._rawText || pre.textContent || '';
      window.copyToClipboard(text).then(() => {
        const orig = copyBtn.textContent;
        copyBtn.textContent = '\u2713';
        copyBtn.classList.add('copied');
        setTimeout(() => { copyBtn.textContent = orig; copyBtn.classList.remove('copied'); }, 1500);
      }).catch(() => {});
    }
  });
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { isPrefixOrSuffixVariant, buildLogModelDisplay, buildCacheCreationDisplay };
}

if (typeof window !== 'undefined') {
  window.i18n?.onLocaleChange?.(() => {
    if (displayedLogs !== null) {
      renderLogs(displayedLogs);
      window.i18n.translatePage();
    }
    if (lastLogsHintData) updateLogsListHint(lastLogsHintData);
    if (lastLogsMetricsData) renderLogsMetrics(lastLogsMetricsData);
  });
}
