const t = window.t;
const i18nText = window.i18nText;

// ── 后端契约（ccpanel 迁移路由）─────────────────────────────────
// 列表/筛选选项/指标条走 /dashboard/* 镜像——与 /admin 同一批 handler，
// withWebAuth 两种角色都放行，api_token 身份下按 KeyHash 收敛；否则
// api_token 会话首个抓取 401 就会把整页弹回登录。调试文件与服务端
// 合并视图留在 /admin/debug-logs/{id}/*——{id} 是日志行自增主键（迁移前
// 的 started_at 毫秒戳链接仍由后端兜底解析），记录无属主校验，不对
// api_token 开放。进行中的请求用 active-requests 列表的 start_time
// （UnixMilli）反查目录，FNV 哈希 id 不能解析目录。
const LOGS_LIST_URL = '/dashboard/logs';
const LOGS_BOOTSTRAP_URL = '/dashboard/logs/bootstrap';
const LOGS_MODELS_URL = '/dashboard/models';
const LOGS_EXPORT_URL = '/admin/logs/export';
const LOGS_STATS_URL = '/dashboard/stats';
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
// 括号随 locale：zh 用全角，en 用半角。
function logsErrorStageOptionLabel(stage) {
  const label = logsErrorStageLabel(stage);
  if (label === stage) return label;
  const en = window.i18n && typeof window.i18n.getLocale === 'function' && window.i18n.getLocale() === 'en';
  return en ? `${label} (${stage})` : `${label}（${stage}）`;
}

function logsResultLabel(result) {
  const item = LOGS_RESULT_LABELS[result];
  return item ? i18nText(item[0], item[1]) : String(result || '');
}

function logsStatusHint(code) {
  const item = LOGS_STATUS_HINTS[Number(code)];
  return item ? i18nText(item[0], item[1]) : '';
}

// logs 表的 message 契约：completed → "ok"；否则 "result[: error_stage]"。
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

// key_hash 是 16 位十六进制（SHA-256 前 8 字节）：列表截前 8 位，
// 全量进 title + data-copy，点击复制全量（委托处理在文件尾部）。
function buildKeyHashDisplay(hash) {
  const h = String(hash || '');
  if (!h) return '<span class="logs-dash">—</span>';
  const short = h.length > 10 ? `${h.slice(0, 8)}…` : h;
  return `<code class="logs-api-key-text logs-mono-text logs-copyable" data-copy="${escapeHtml(h)}" title="${escapeHtml(h)} · ${escapeHtml(t('logs.clickToCopy'))}">${escapeHtml(short)}</code>`;
}

let currentLogsPage = 1;
let logsPageSize = 100;
let totalLogsPages = 1;
let totalLogs = 0;
// keyset 游标：页号 → 取该页用的 before_id（上一页最旧行 id）；null=该页按 offset 取。
// 顺序翻页时由 nextLogsPage 播种；跳页/筛选变化时整表重置。
let logsPageCursors = { 1: null };
let logsPageLastId = null; // 当前页最旧行 id，是下一页的 before_id
let logsHasMore = false;   // 响应顶层 has_more：窗外仍有更早历史（count 缺席的深页判尾页用）
let currentLogsCustomTimeRange = null;
let authTokens = []; // 令牌列表
let logsModelCombobox = null; // 模型筛选组合框
let logsStatusCombobox = null; // 状态码筛选组合框
let logsErrorStageCombobox = null; // 失败阶段筛选组合框
let logsAccountCombobox = null; // 上游账号筛选组合框
let lastLogsHintData = null; // 最近一次列表响应的 {rejects}，供语言切换重渲
let lastLogsMetricsData = null; // 最近一次指标条响应（/admin/stats data），供语言切换重渲
window.availableLogsModels = []; // 可用模型列表
window.availableLogsStatusCodes = []; // 可用状态码列表
window.availableLogsAccounts = []; // 可见日志里出现过的上游账号
let logsExactModelValue = '';

let latestActiveRequests = []; // 缓存 ui.js 最近一次推送的活动请求，供 load() 即时刷新
let lastActiveRequestStates = null; // Map<id, fingerprint>：上次活跃请求状态，用于检测请求结束/上游重试
let logsLoadInFlight = false;
let logsLoadPending = false;

function resetLogsPagination() {
  currentLogsPage = 1;
  totalLogsPages = 1;
  logsPageCursors = { 1: null };
  logsPageLastId = null;
  logsHasMore = false;
}

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
    const item = document.createElement('label');
    item.className = 'logs-col-toggle-item';
    item.dataset.colKey = col.key;
    item.innerHTML = `<input type="checkbox"${isColVisible(col.key) ? ' checked' : ''}><span>${escapeHtml(i18nText(col.i18n, col.i18n))}</span>`;
    item.querySelector('input').addEventListener('change', (e) => {
      colVisibility[col.key] = e.target.checked;
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

// 信息列固定「详情」入口；已接收字节数是进度读数，降为副文本不承担链接职责。
function buildActiveRequestInfoContent(req) {
  const bytesInfo = formatBytes(req?.bytes_received);
  const bytesHtml = bytesInfo
    ? `<span class="logs-bytes-received">${escapeHtml(t('logs.receivedBytes', { bytes: bytesInfo }))}</span>`
    : '';
  const activeRequestId = Number(req?.id);

  if (!req?.debug_log_available || !Number.isFinite(activeRequestId) || activeRequestId <= 0) {
    return bytesHtml || '<span class="logs-dash">—</span>';
  }

  const link = `<span class="debug-log-link has-upstream-detail" data-action="open-active-debug" data-active-request-id="${activeRequestId}" title="${escapeHtml(t('logs.debugLogTitle'))}">${escapeHtml(t('logs.detail'))}</span>`;
  return link + bytesHtml;
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

// logs 表的 api 原值 → 探活端点协议（/admin/model-test 的 client_protocol）。
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
      return '—';
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
  return '<span class="log-timing-separator">/</span>';
}

function buildFirstByteTimingHtml(seconds, text) {
  return `<span class="log-timing-first-byte${window.toneClass(window.getFirstByteTimingTone(seconds))}">${text}</span>`;
}

function buildDurationTimingHtml(seconds, text) {
  return `<span class="log-timing-duration${window.toneClass(window.getDurationTimingTone(seconds))}">${text}</span>`;
}

function buildActiveRequestTimingHtml(req, elapsedRaw, elapsedText) {
  if (!Number.isFinite(elapsedRaw)) return '—';

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
    return '<span class="logs-dash">—</span>';
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

let cachedLogMobileLabels = null;

function getLogMobileLabels() {
  if (cachedLogMobileLabels !== null) return cachedLogMobileLabels;
  cachedLogMobileLabels = {
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
  return cachedLogMobileLabels;
}

// 行单元格统一构造：mobile-empty-cell 与 nowrap 都收口到 CSS 类。
function logRowCell(cls, label, html, opts = {}) {
  const empty = opts.empty !== undefined ? opts.empty : !html;
  const classes = cls + (empty ? ' mobile-empty-cell' : '') + (opts.nowrap === false ? '' : ' logs-nowrap');
  return `<td class="${classes}" data-mobile-label="${label}"${opts.attrs || ''}>${html || ''}</td>`;
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
  if (!text) return '<span class="logs-dash">—</span>';
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
    const logIdAttr = Number.isFinite(logId) && logId > 0 ? ` data-action="open-debug-log" data-log-id="${logId}"` : '';
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
    logsHasMore = response.has_more === true;
    logsPageLastId = (Array.isArray(data) && data.length)
      ? Number(data[data.length - 1].id) || null
      : null;
    // 提示条（管线前拒绝事件环）与列表渲染同源更新。
    updateLogsListHint(response);

    // 把日志中出现的模型/状态码合并进筛选下拉（无需刷新页面）
    mergeLogsFilterOptions(data);

    // 总页数：count 只在首页返回精确值；深页用 has_more 判尾页
    if (typeof response.count === 'number') {
      totalLogs = response.count;
      totalLogsPages = Math.ceil(totalLogs / logsPageSize) || 1;
    } else if (logsHasMore) {
      totalLogsPages = Math.max(currentLogsPage + 1, totalLogsPages);
    } else {
      totalLogsPages = currentLogsPage;
    }

    updatePagination();

    // pending 行不参与 keyed diff，renderLogs 不再动它们
    renderLogs(data);

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
      // 在途期间攒下的加载意图立即排空（skipLoading 保行不闪），不再过 debounce
      logsLoadPending = false;
      load(true);
    }
  }
}

// ── 列表提示条（rejects 事件环）────────────────────────────────
// 管线前拒绝（鉴权 401/并发 429/排空 503/读体中断）不产生调试记录、不进
// logs 表——用户在列表找这类失败天然扑空，提示条把事件环聚合成一行
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
  if (!Number.isFinite(n) || n <= 0) return '—';
  if (n >= 1000) return formatNumber(Math.round(n));
  if (n >= 10) return n.toFixed(1);
  return n.toFixed(2);
}

function logsMetricCard(label, value, sub, title, tone) {
  return `<div class="kpi-card" title="${escapeHtml(title)}">` +
    `<span class="kpi-label">${escapeHtml(label)}</span>` +
    `<strong class="kpi-value${window.toneClass(tone)}">${escapeHtml(value)}</strong>` +
    (sub ? `<span class="kpi-sub">${escapeHtml(sub)}</span>` : '') +
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
  const fmtSec = (v) => { const n = Number(v); return n > 0 ? n.toFixed(2) + 's'  : '—'; };
  const fmtPct = (v) => { const n = Number(v); return n > 0 ? n.toFixed(1) + '%'  : '—'; };
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

  el.innerHTML = `<div class="kpi-grid">` +
    logsMetricCard(
      i18nText('trend.typeRpm', 'RPM'),
      formatLogsMetricRate(avgRpm),
      winSub('rpm', formatLogsMetricRate),
      rpmTitle,
      ''
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
      ttfb > 0 ? ttfb.toFixed(2) + 's'  : '—',
      winSub('ttfb_s', fmtSec),
      ttfbTitle,
      ttfb > 0 ? window.getFirstByteTimingTone(ttfb) : ''
    ) +
    logsMetricCard(
      i18nText('trend.cacheHitRate', '缓存命中率'),
      cachePct > 0 ? cachePct.toFixed(1) + '%'  : '—',
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
  const label = pending ? t('logs.aborting') : t('logs.abort');
  return `<button type="button" class="logs-abort-btn" data-action="abort-active-request" data-abort-request-id="${escapeHtml(id)}"`
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
    const elapsed = elapsedRaw !== null ? elapsedRaw.toFixed(1)  : '—';
    const streamFlag = getStreamFlagHtml(req.is_streaming);
    // 行级点击开 debug 详情与「信息」列链接同条件（debug_log_available + 有效 id）
    const activeNumId = Number(req?.id);
    const debugActiveId = (req?.debug_log_available && Number.isFinite(activeNumId) && activeNumId > 0)
      ? String(activeNumId)
      : '';

    const durationDisplay = startMs ? buildActiveRequestTimingHtml(req, elapsedRaw, elapsed)  : '—';

    const statusDisplay = buildActiveRequestStatusHtml(req);
    const modelDisplay = buildLogModelDisplay(req.model, '', req.reasoning_tokens, req.upstream_websocket);
    const tokenDescDisplay = buildActiveRequestTokenDescDisplay(req);
    const abortDisplay = buildActiveRequestAbortHtml(req, id, startMs);
    const accountDisplay = buildAccountDisplay(req.account, req.account_switches);

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
      if (debugActiveId) existingRow.dataset.debugActiveId = debugActiveId;
      else delete existingRow.dataset.debugActiveId;
    } else {
      // 创建新行
      const row = document.createElement('tr');
      row.className = 'mobile-card-row pending-row';
      row.setAttribute('data-req-id', id);
      if (debugActiveId) row.dataset.debugActiveId = debugActiveId;
      if (totalCols < 8) {
        row.innerHTML = `
            <td colspan="${totalCols}" class="logs-compact-cell">
              ${statusDisplay}
              <span>${formatTime(req.start_time)}</span>
              <span class="logs-mono-text" title="${escapeHtml(req.client_ip || '')}">${escapeHtml(maskIP(req.client_ip) || '-')}</span>
              <span>${modelDisplay}</span>
              <span class="active-account-slot">${accountDisplay}</span>
              <span>${durationDisplay} ${streamFlag}</span>
              <span>${infoContent}</span>
              <span class="active-abort-slot">${abortDisplay}</span>
            </td>
          `;
      } else {
        row.innerHTML =
          logRowCell('logs-col-time', logMobileLabels.time, formatTime(req.start_time), { empty: false })
          + logRowCell('logs-col-ip logs-mono-text', logMobileLabels.ip, escapeHtml(maskIP(req.client_ip) || '-'), { empty: false, attrs: ` title="${escapeHtml(req.client_ip || '')}"` })
          + logRowCell('logs-col-token-desc', logMobileLabels.tokenDesc, tokenDescDisplay)
          + logRowCell('logs-col-api-key', logMobileLabels.apiKey, keyDisplay, { empty: false })
          + logRowCell('logs-col-model', logMobileLabels.model, modelDisplay, { empty: false, nowrap: false })
          + logRowCell('logs-col-account', logMobileLabels.account, accountDisplay)
          + logRowCell('logs-col-status', logMobileLabels.status, statusDisplay, { empty: false, nowrap: false })
          + logRowCell('logs-col-timing', logMobileLabels.timing, `${durationDisplay} ${streamFlag}`, { empty: false })
          + logRowCell('logs-col-speed', logMobileLabels.speed, abortDisplay)
          + logRowCell('logs-col-input', logMobileLabels.input, '')
          + logRowCell('logs-col-output', logMobileLabels.output, '')
          + logRowCell('logs-col-cache-read', logMobileLabels.cacheRead, '')
          + logRowCell('logs-col-cache-write', logMobileLabels.cacheWrite, '')
          + logRowCell('logs-col-cache-util', logMobileLabels.cacheUtil, '')
          + logRowCell('logs-col-cost', logMobileLabels.cost, '')
          + logRowCell('logs-col-message', logMobileLabels.message, infoContent, { empty: false, nowrap: false });
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

  const confirmMsg = t('logs.abortConfirm');
  if (!(await Modal.confirm(confirmMsg, { danger: true }))) return;

  abortingActiveRequests.set(id, Number(button.dataset.abortStart) || 0);
  button.disabled = true;
  button.textContent = t('logs.aborting');

  try {
    const { payload } = await fetchAPIWithAuthRaw(activeAbortUrl(id), { method: 'POST' });
    if (!payload.success) throw new Error(payload.error || i18nText('logs.abortFailed', '中断失败'));
  } catch (e) {
    // 中断没打出去就恢复按钮，否则这一行会永远卡在「中断中」
    abortingActiveRequests.delete(id);
    if (window.showError) window.showError(e.message || i18nText('logs.abortFailed', '中断失败'));
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
  return `<span class="token-metric-value token-metric-value--success">${pct.toFixed(1)}%</span>`;
}

// buildCacheCreationDisplay 渲染缓存建列，5m 分桶角标按实际数据判定，不看
// 模型名或协议——走 codex 协议的 gpt 模型同样会上报分桶，用模型名判断会把
// 真实分桶吞掉。1h 分桶投影端从不赋值（恒 0），不渲染。
function buildCacheCreationDisplay(entry) {
  const total = entry.cache_creation_input_tokens || 0;
  if (total <= 0) return '';

  const badge = (entry.cache_5m_input_tokens || 0) > 0
    ? ' <sup class="cache-5m-badge">5m</sup>'
    : '';
  return `<span class="token-metric-value token-metric-value--primary">${total.toLocaleString()}${badge}</span>`;
}

// ── tbody keyed diff ──────────────────────────────────────────
// 行 key = logs 行 id（自增主键），缺 id 退 dir 再退行序号；行 DOM 的
// _sig 存上次写入的单元格 HTML——相同跳过、不同只刷该行内层，序变只做
// insertBefore 搬位。pending 行（进行中请求）由 renderActiveRequests
// 管理、恒在列表顶部，不参与对账，也不再被整表重建误伤。
function logRowKey(entry, index) {
  return 'r' + (entry?.id ?? entry?.dir ?? index);
}

function clearLogRows(tbody) {
  for (const child of Array.from(tbody.children)) {
    if (!child.matches('tr.pending-row')) child.remove();
  }
}

function syncLogRows(tbody, keys, htmlParts, debugIds) {
  const wanted = new Set(keys);
  const pool = new Map();
  for (const child of Array.from(tbody.children)) {
    if (child.matches('tr.pending-row')) continue;
    const key = child.dataset ? child.dataset.rowKey : null;
    if (key && wanted.has(key)) {
      pool.set(key, child);
    } else {
      child.remove();
    }
  }

  // 插入基准：首个非 pending 子节点（pending 行恒在其前）
  let ref = null;
  for (const child of tbody.children) {
    if (!child.matches('tr.pending-row')) { ref = child; break; }
  }

  for (let i = 0; i < keys.length; i++) {
    let row = pool.get(keys[i]);
    if (row) {
      if (row._sig !== htmlParts[i]) {
        row.innerHTML = htmlParts[i];
        row._sig = htmlParts[i];
      }
      if (debugIds[i]) row.dataset.debugLogId = debugIds[i];
      else delete row.dataset.debugLogId;
    } else {
      row = document.createElement('tr');
      row.className = 'mobile-card-row logs-table-row';
      row.dataset.rowKey = keys[i];
      if (debugIds[i]) row.dataset.debugLogId = debugIds[i];
      row.innerHTML = htmlParts[i];
      row._sig = htmlParts[i];
    }
    if (row === ref) {
      ref = ref.nextElementSibling;
      continue;
    }
    tbody.insertBefore(row, ref);
  }
}

function renderLogsLoading() {
  displayedLogs = null;
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const loadingRow = TemplateEngine.render('tpl-log-loading', { colspan });
  clearLogRows(tbody);
  if (loadingRow) tbody.appendChild(loadingRow);
}

function renderLogsError() {
  displayedLogs = null;
  const tbody = document.getElementById('tbody');
  const colspan = getTableColspan();
  const errorRow = TemplateEngine.render('tpl-log-error', { colspan });
  clearLogRows(tbody);
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
    clearLogRows(tbody);
    if (emptyRow) tbody.appendChild(emptyRow);
    return;
  }

  // 性能优化：直接拼接 HTML 字符串，避免逐行调用 TemplateEngine.render
  const htmlParts = new Array(data.length);
  const rowKeys = new Array(data.length);
  const rowDebugIds = new Array(data.length);

  for (let i = 0; i < data.length; i++) {
    const entry = data[i];
    // === 预处理数据：构建复杂HTML片段 ===

    // 0. 客户端IP显示（掩码处理，hover显示完整IP）
    const clientIPDisplay = entry.client_ip ?
      `<span title="${escapeHtml(entry.client_ip)}">${escapeHtml(maskIP(entry.client_ip))}</span>` :
      '<span class="logs-dash-faint">—</span>';

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
      ? `<button type="button" class="test-key-btn" data-action="probe-model" data-probe-model="${escapeHtml(entry.model)}" data-probe-api="${escapeHtml(entry.api || '')}" title="${escapeHtml(t('logs.probeModel'))}"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" xmlns="http://www.w3.org/2000/svg" aria-hidden="true" focusable="false"><path d="M13 2L4 14H11L9 22L20 10H13L13 2Z" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"/></svg></button>`
      : '';

    // 4. 响应时间显示(流式/非流式)
    const hasDuration = entry.duration !== undefined && entry.duration !== null;
    const durationDisplay = hasDuration ?
      buildDurationTimingHtml(entry.duration, entry.duration.toFixed(2)) :
      '<span class="logs-dash">—</span>';

    const streamFlag = getStreamFlagHtml(entry.is_streaming);

    let responseTimingDisplay;
    if (entry.is_streaming) {
      const hasFirstByte = entry.first_byte_time !== undefined && entry.first_byte_time !== null;
      const firstByteDisplay = hasFirstByte ?
        buildFirstByteTimingHtml(entry.first_byte_time, entry.first_byte_time.toFixed(2)) :
        '<span class="log-timing-first-byte logs-dash">—</span>';
      responseTimingDisplay = `<span class="log-timing-pair">${firstByteDisplay}${buildTimingSeparatorHtml()}${durationDisplay}</span>${streamFlag}`;
    } else {
      responseTimingDisplay = `<span class="log-timing-pair">${durationDisplay}</span>${streamFlag}`;
    }

    const logSpeed = calculateLogSpeed(entry);
    const speedDisplay = logSpeed === null
      ? ''
      : `<span class="token-metric-value token-metric-value--neutral">${logSpeed.toFixed(1)}</span>`;

    // 5. Key 哈希显示（本服务 api_key_used 就是 key_hash，截断 + title 全量）
    const apiKeyDisplay = buildKeyHashDisplay(entry.api_key_used);

    // 6. Token统计显示(0值为空)
    const tokenValue = (value, modifier) => {
      if (value === undefined || value === null || value === 0) return '';
      return `<span class="token-metric-value token-metric-value--${modifier}">${value.toLocaleString()}</span>`;
    };
    const inputTokensDisplay = tokenValue(entry.input_tokens, 'neutral');
    const outputTokensDisplay = tokenValue(entry.output_tokens, 'neutral');
    const cacheReadDisplay = tokenValue(entry.cache_read_input_tokens, 'success');

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

    // === 拼接行内单元格（tr 壳与 key 由 keyed diff 管理）===
    rowKeys[i] = logRowKey(entry, i);
    rowDebugIds[i] = canInspectDebugLog(entry) ? String(Number(entry.id)) : '';
    htmlParts[i] =
      logRowCell('logs-col-time', logMobileLabels.time, formatTime(entry.time), { empty: false })
      + logRowCell('logs-col-ip logs-mono-text', logMobileLabels.ip, clientIPDisplay, { empty: false })
      + logRowCell('logs-col-token-desc', logMobileLabels.tokenDesc, tokenDescDisplay, { empty: false })
      + logRowCell('logs-col-api-key', logMobileLabels.apiKey, apiKeyDisplay, { empty: false })
      + logRowCell('logs-col-model', logMobileLabels.model, `${modelDisplay} ${probeDisplay}`, { empty: false, nowrap: false })
      + logRowCell('logs-col-account', logMobileLabels.account, accountDisplay)
      + logRowCell('logs-col-status', logMobileLabels.status, `<span class="${statusClass}"${statusTitleAttr}>${escapeHtml(statusCode)}</span>`, { empty: false, nowrap: false })
      + logRowCell('logs-col-timing', logMobileLabels.timing, responseTimingDisplay, { empty: false })
      + logRowCell('logs-col-speed', logMobileLabels.speed, speedDisplay)
      + logRowCell('logs-col-input', logMobileLabels.input, inputTokensDisplay)
      + logRowCell('logs-col-output', logMobileLabels.output, outputTokensDisplay)
      + logRowCell('logs-col-cache-read', logMobileLabels.cacheRead, cacheReadDisplay)
      + logRowCell('logs-col-cache-write', logMobileLabels.cacheWrite, cacheCreationDisplay)
      + logRowCell('logs-col-cache-util', logMobileLabels.cacheUtil, cacheUtilDisplay)
      + logRowCell('logs-col-cost', logMobileLabels.cost, costDisplay, { attrs: costTitleAttr })
      + logRowCell('logs-col-message', logMobileLabels.message, messageContent, { nowrap: false });
  }

  syncLogRows(tbody, rowKeys, htmlParts, rowDebugIds);
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
    if (logsPageLastId) logsPageCursors[currentLogsPage + 1] = logsPageLastId;
    currentLogsPage++;
    load();
  }
}

function lastLogsPage() {
  if (currentLogsPage < totalLogsPages) {
    delete logsPageCursors[totalLogsPages]; // 跳页按 offset 语义，不用陈旧锚点
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
        window.showError(t('pagination.invalidPage', { total: totalLogsPages }));
      } catch (_) { }
    }
    return;
  }

  // 跳转到目标页（按 offset 语义，不用陈旧锚点）
  if (targetPage !== currentLogsPage) {
    delete logsPageCursors[targetPage];
    currentLogsPage = targetPage;
    load();
  }

  // 清空输入框
  jumpPageInput.value = '';
}

function applyFilter() {
  resetLogsPagination();

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
  resetLogsPagination();
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

  if (logsAccountCombobox && filters.account !== undefined) {
    const account = String(filters.account || '').trim();
    logsAccountCombobox.setValue(account, account || t('logs.allAccounts'));
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
  const accounts = Array.isArray(window.availableLogsAccounts) ? window.availableLogsAccounts : [];
  const knownAccounts = new Set(accounts);
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
    const account = String(entry?.account || '').trim();
    if (account && !knownAccounts.has(account)) {
      knownAccounts.add(account);
      accounts.push(account);
      changed = true;
    }
  }

  if (!changed) return;
  window.availableLogsModels = models;
  window.availableLogsStatusCodes = statusCodes.sort((a, b) => a - b);
  window.availableLogsAccounts = accounts.sort();
  if (logsModelCombobox) logsModelCombobox.refresh();
  if (logsStatusCombobox) logsStatusCombobox.refresh();
  if (logsAccountCombobox) logsAccountCombobox.refresh();
}

function initLogsModelCombobox(initialValue) {
  logsModelCombobox = window.initFilterCombobox({
    inputId: 'f_model',
    allLabel: t('trend.allModels'),
    initialValue,
    getOptions: () => (window.availableLogsModels || []).map(m => ({ value: m, label: m })),
    onSelect: applyFilter
  });
}

function initLogsStatusCombobox(initialValue) {
  logsStatusCombobox = window.initFilterCombobox({
    inputId: 'f_status',
    allLabel: t('logs.allStatusCodes'),
    initialValue,
    getOptions: () => {
      const seen = new Set(LOGS_STATUS_PRESETS);
      return [
        ...LOGS_STATUS_PRESETS.map(v => ({ value: v, label: v })),
        ...(window.availableLogsStatusCodes || [])
          .map(String)
          .filter(code => !seen.has(code))
          .map(code => ({ value: code, label: code }))
      ];
    },
    onSelect: applyFilter
  });
}

// 失败阶段筛选：候选来自 ErrStage 枚举快照（LOGS_ERROR_STAGES），
// allowCustomInput 让尚未进枚举的新阶段也能直接输入提交。
function initLogsErrorStageCombobox(initialValue) {
  logsErrorStageCombobox = window.initFilterCombobox({
    inputId: 'f_error_stage',
    allLabel: i18nText('logs.allErrorStages', '全部阶段'),
    initialValue,
    initialLabel: initialValue
      ? logsErrorStageOptionLabel(initialValue)
      : i18nText('logs.allErrorStages', '全部阶段'),
    getOptions: () => LOGS_ERROR_STAGES.map(stage => ({ value: stage, label: logsErrorStageOptionLabel(stage) })),
    onSelect: applyFilter
  });
}

function initLogsAccountCombobox(initialValue) {
  logsAccountCombobox = window.initFilterCombobox({
    inputId: 'f_account',
    allLabel: t('logs.allAccounts'),
    initialValue,
    getOptions: () => (window.availableLogsAccounts || []).map(name => ({ value: name, label: name })),
    onSelect: applyFilter
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
      await loadLogsFilterOptions(nextRange);
      applyFilter();
    }
  });

  initLogsModelCombobox(restoredFilters.model || '');
  initLogsStatusCombobox(restoredFilters.status || '');
  initLogsErrorStageCombobox(restoredFilters.errorStage || '');
  initLogsAccountCombobox(restoredFilters.account || '');
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
        resetLogsPagination();
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
    enterInputIds: ['f_hours', 'f_api', 'f_auth_token', 'f_log_source', 'f_result', 'f_account']
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
        'toggle-col-menu': (el) => toggleColMenu(el),
        'abort-active-request': (el) => abortActiveRequest(el),
        'open-active-debug': (el) => {
          const activeRequestId = parseInt(el.dataset.activeRequestId, 10);
          if (Number.isFinite(activeRequestId) && activeRequestId > 0) {
            window.showActiveDebugLogModal(activeRequestId);
          }
        },
        'open-debug-log': (el) => {
          const logId = parseInt(el.dataset.logId, 10);
          if (Number.isFinite(logId) && logId > 0) {
            window.showDebugLogModal(logId);
          }
        },
        'probe-model': (el) => {
          if (typeof window.openModelTestModal === 'function') {
            window.openModelTestModal({
              model: el.dataset.probeModel || '',
              clientProtocol: apiToClientProtocol(el.dataset.probeApi)
            });
          }
        }
      }
    });
  }

  // 整行可点开 debug 详情：交互子元素（链接/按钮/输入框/可复制文本）与
  // 文本选区上的点击不触发——「信息」列链接与探活按钮走 data-action 委托，
  // 行点击只兜其余区域。
  const tbody = document.getElementById('tbody');
  if (tbody && !tbody.dataset.rowClickBound) {
    tbody.dataset.rowClickBound = '1';
    tbody.addEventListener('click', (e) => {
      const row = e.target.closest('tr[data-debug-log-id], tr[data-debug-active-id]');
      if (!row || !tbody.contains(row)) return;
      if (e.target.closest('a, button, input, select, textarea, code, [data-action], [data-copy]')) return;
      if (window.getSelection && String(window.getSelection() || '') !== '') return;
      const logId = parseInt(row.dataset.debugLogId || '', 10);
      if (Number.isFinite(logId) && logId > 0) {
        window.showDebugLogModal(logId);
        return;
      }
      const activeId = parseInt(row.dataset.debugActiveId || '', 10);
      if (Number.isFinite(activeId) && activeId > 0) {
        window.showActiveDebugLogModal(activeId);
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
    if (!ts) return '—';

    const d = new Date(ts);
    if (isNaN(d.getTime()) || d.getFullYear() < 2020) {
      return '—';
    }

    // 手动格式化：MM-DD HH:mm:ss
    const M = String(d.getMonth() + 1).padStart(2, '0');
    const D = String(d.getDate()).padStart(2, '0');
    const h = String(d.getHours()).padStart(2, '0');
    const m = String(d.getMinutes()).padStart(2, '0');
    const s = String(d.getSeconds()).padStart(2, '0');
    return `${M}-${D} ${h}:${m}:${s}`;
  } catch (e) {
    return '—';
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
  { key: 'authToken', queryKeys: ['auth_token_id'], defaultValue: '' },
  { key: 'account', queryKeys: ['account'], defaultValue: '' }
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
  const account = logsAccountCombobox
    ? String(logsAccountCombobox.getValue() || '').trim()
    : (document.getElementById('f_account')?.value || '').trim();
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
    account,
    modelExact: isExactLogsModelFilter(model),
    logSource
  };
}

function buildLogsRequestParams() {
  const filters = getLogsFilters();
  // 顺序翻页命中有播种的 keyset 游标 → before_id 恒定成本；其余路径（跳页/
  // 首页/筛选后）走 offset。两参数后端取交，前端只发其一。
  const cursor = logsPageCursors[currentLogsPage];
  const baseParams = { limit: logsPageSize.toString() };
  if (cursor) {
    baseParams.before_id = String(cursor);
  } else {
    baseParams.offset = ((currentLogsPage - 1) * logsPageSize).toString();
  }
  const params = window.FilterQuery.buildRequestParams(filters, LOGS_FILTER_FIELDS, { baseParams });
  appendLogsTimeRangeParams(params, filters);
  return params;
}

// 列显隐的绑定不依赖网络，提到 bootstrap 之外：run() 要等
// /dashboard/session 返回才执行，慢会话期间按钮曾是死的。
initLogsPageActions();
applyColVisibility();
document.addEventListener('click', closeColMenuOnClickOutside);

// ESC键关闭列显隐菜单——同在 bootstrap 之前绑定，慢会话下也可用
// （.modal 的 ESC/背景关闭由 modal.js 的共享栈统一处理）
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') {
    const colMenu = document.getElementById('colToggleMenu');
    if (colMenu && !colMenu.hidden) {
      colMenu.hidden = true;
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
  const restoredFilters = restoredLogsFilters(location.search, savedFilters);
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

  }
});

// 筛选恢复在 bootstrap（location.search）与 bfcache 还原（空 search）间共享。
function restoredLogsFilters(search, savedFilters) {
  const restored = window.FilterState.restore({ search, savedFilters, fields: LOGS_FILTER_FIELDS });
  currentLogsCustomTimeRange = restored.range === 'custom'
    ? normalizeLogsCustomTimeRange(restored)
    : null;
  if (restored.range === 'custom' && !currentLogsCustomTimeRange) {
    restored.range = 'today';
  }
  return restored;
}

// 处理 bfcache（后退/前进缓存）：页面从缓存恢复时重新加载筛选条件
window.addEventListener('pageshow', async function (event) {
  if (event.persisted) {
    // 页面从 bfcache 恢复，重新同步筛选器状态
    const savedFilters = window.FilterState.load(LOGS_FILTER_KEY);
    if (savedFilters) {
      const restoredFilters = restoredLogsFilters('', savedFilters);
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
      resetLogsPagination();
      load();
    }
  }
});


if (typeof module !== 'undefined' && module.exports) {
  module.exports = { isPrefixOrSuffixVariant, buildLogModelDisplay, buildCacheCreationDisplay };
}

// data-copy 委托：表格里截断显示的值（key_hash 等）点击复制全量。
if (typeof document !== 'undefined') {
  document.addEventListener('click', (e) => {
    const el = e.target.closest('[data-copy]');
    if (!el || typeof window.copyToClipboard !== 'function') return;
    window.copyToClipboard(el.dataset.copy).then(() => {
      el.classList.add('logs-copied');
      setTimeout(() => el.classList.remove('logs-copied'), 800);
    }).catch(() => {});
  });
}

if (typeof window !== 'undefined') {
  window.i18n?.onLocaleChange?.(() => {
    cachedLogMobileLabels = null;
    if (displayedLogs !== null) {
      renderLogs(displayedLogs);
      window.i18n.translatePage();
    }
    if (lastLogsHintData) updateLogsListHint(lastLogsHintData);
    if (lastLogsMetricsData) renderLogsMetrics(lastLogsMetricsData);
  });
}
