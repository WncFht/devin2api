// 系统设置页面
const t = window.t;
const i18nText = window.i18nText || ((key, fallback) => fallback || key);

let originalSettings = {}; // 保存原始值用于比较
let settingDefinitions = new Map();
let runtimeMetricsLoading = false;
let runtimeMetricsPreviousFocus = null;
let runtimeMetricsRefreshTimer = null;
const RUNTIME_METRICS_REFRESH_MS = 3000;
let multimodalFallbackPreviousFocus = null;
let multimodalFallbackDraft = [];
let multimodalFallbackModelOptions = null;
let customPricingPreviousFocus = null;
let customPricingDraft = [];
let customPricingModelFilter = '';
// 系统分层定价模型的 ID 集合（在草稿内的任何位置都不允许保存）。
// 只存 ID 而非 DOM 状态，重渲染后依然有效。
const customPricingTieredModels = new Set();

let effectiveConfigData = null;
let processLogPreviousFocus = null;
let processLogPollTimer = null;
let processLogOffset = 0;
let processLogBuffer = '';
let processLogPaused = false;
let processLogFollow = true;
let processLogLoading = false;
const PROCESS_LOG_REFRESH_MS = 5000;
// 缓冲封顶：跟随模式长期运行时 DOM 不无限增长。
const PROCESS_LOG_BUFFER_CAP = 256 * 1024;
// 级别过滤保留无 level= 的行（堆栈续行、手写输出等），不静默吞内容。
const PROCESS_LOG_LEVELS = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 };

const modelMultimodalFallbackSettingKey = 'model_multimodal_fallback';
const modelCustomPricingSettingKey = 'model_custom_pricing';
const maxMultimodalFallbackMappings = 64;
const maxCustomPricingBytes = 1024 * 1024;
const maxCustomPricingModels = 512;
const customPricingBasicFields = ['input_price', 'output_price', 'cache_read_price', 'cache_write_price'];
const customPricingHighContextFields = ['input_price_high', 'output_price_high', 'cache_read_price_high', 'cache_write_price_high'];
// 后端契约只接受显式列出的字段；系统价格里仅供内部使用的字段（分层表、
// 缓存读计入分档、图像费率）在预填时必须剔除，否则保存会被拒绝。
const customPricingContractFields = new Set([
  ...customPricingBasicFields, ...customPricingHighContextFields
]);

function projectCustomPricingDefaults(pricing) {
  const projected = {};
  for (const field of customPricingContractFields) {
    if (pricing && Object.prototype.hasOwnProperty.call(pricing, field)) projected[field] = pricing[field];
  }
  return projected;
}

const advancedSettingKeys = new Set([
  'auto_refresh_interval_seconds',
  'active_request_title_enabled',
  'codex_map_429_to_503',
  'model_catalog_sync_interval_hours',
  'model_fuzzy_match'
]);

const byteSettingKeys = new Set([
  'max_body_bytes',
  'max_image_body_bytes',
  'responses_ws_max_transcript_bytes'
]);
const bytesPerMiB = 1024 * 1024;
const maxDurationSeconds = 9223372036;
const maxDurationMinutes = 153722867;
const maxDurationHours = 2562047;

const numericSettingConstraints = new Map([
  ['max_concurrency', { min: 1 }],
  ['max_body_bytes', { min: 1 / bytesPerMiB }],
  ['max_image_body_bytes', { min: 1 / bytesPerMiB }],
  ['http_read_timeout_seconds', { min: 0, max: maxDurationSeconds }],
  ['log_retention_days', { min: -1, max: 365 }],
  ['model_catalog_sync_interval_hours', { min: 0, max: maxDurationHours }],
  ['auto_refresh_interval_seconds', { min: 0, max: maxDurationSeconds }],
  ['warm_prefix_jitter_ratio', { maxExclusive: 1 }],
  ['responses_ws_max_sessions', { min: 0 }],
  ['responses_ws_session_ttl_minutes', { min: 0, max: maxDurationMinutes }],
  ['responses_ws_max_transcript_bytes', { min: 0 }],
  ['responses_ws_max_connections', { min: 0 }],
  ['responses_ws_max_connections_per_token', { min: 0 }]
]);

// json 设置键的顶层形状约束：别名表是对象，工具名表是数组。
// 形状不符在保存前就拦下，其余语义校验（元素非空等）归后端。
const jsonSettingShapes = new Map([
  ['devin_aliases', 'object'],
  ['warm_prefix_blocked_names', 'array'],
  ['warm_prefix_userpaced_names', 'array']
]);

function settingValueForDisplay(key, value) {
  const normalizedValue = String(value ?? '');
  if (!byteSettingKeys.has(key)) return normalizedValue;

  const bytes = Number(normalizedValue);
  return Number.isFinite(bytes) ? String(bytes / bytesPerMiB) : normalizedValue;
}

function settingValueForStorage(key, value) {
  const normalizedValue = String(value ?? '');
  if (!byteSettingKeys.has(key)) return normalizedValue;

  const mebibytes = Number(normalizedValue);
  const bytes = Math.round(mebibytes * bytesPerMiB);
  return Number.isFinite(bytes) ? String(bytes) : normalizedValue;
}

function numericConstraintFor(setting) {
  const configured = numericSettingConstraints.get(setting.key);
  if (configured) return configured;
  if (setting.value_type === 'duration') return { min: 0, max: maxDurationSeconds };
  return null;
}

function numericInputAttributes(setting) {
  const constraint = numericConstraintFor(setting);
  const attributes = ['required'];
  if (constraint?.min !== undefined) attributes.push(`min="${constraint.min}"`);
  if (constraint?.max !== undefined) attributes.push(`max="${constraint.max}"`);
  const acceptsFraction = setting.value_type === 'float' || byteSettingKeys.has(setting.key);
  attributes.push(`step="${acceptsFraction ? 'any' : '1'}"`);
  return attributes.join(' ');
}

function validateSettingInput(setting, value) {
  const normalizedValue = String(value ?? '');
  if (setting.key === modelCustomPricingSettingKey) {
    return validateCustomPricingInput(normalizedValue);
  }

  if (setting.value_type === 'json') {
    const trimmed = normalizedValue.trim();
    if (trimmed === '') return '';
    let parsed;
    try {
      parsed = JSON.parse(trimmed);
    } catch (_) {
      return i18nText('settings.validation.invalidJSON', '必须是合法 JSON');
    }
    const shape = jsonSettingShapes.get(setting.key);
    if (shape === 'object' && (!parsed || typeof parsed !== 'object' || Array.isArray(parsed))) {
      return i18nText('settings.validation.invalidJSONObject', '必须是 JSON 对象，如 {"客户端模型名":"上游UID"}');
    }
    if (shape === 'array' && !Array.isArray(parsed)) {
      return i18nText('settings.validation.invalidJSONArray', '必须是 JSON 数组，如 ["工具名"]');
    }
    return '';
  }

  const numeric = setting.value_type === 'int'
    || setting.value_type === 'float'
    || setting.value_type === 'duration'
    || byteSettingKeys.has(setting.key);
  if (!numeric) return '';

  if (normalizedValue.trim() === '') return t('settings.validation.numberRequired');
  const number = Number(normalizedValue);
  if (!Number.isFinite(number)) return t('settings.validation.finiteNumber');
  if (!byteSettingKeys.has(setting.key) && setting.value_type !== 'float' && !Number.isSafeInteger(number)) {
    return t('settings.validation.wholeNumber');
  }

  const constraint = numericConstraintFor(setting);
  if (constraint?.min !== undefined && number < constraint.min) {
    return t('settings.validation.minimum', { value: constraint.min });
  }
  if (constraint?.max !== undefined && number > constraint.max) {
    return t('settings.validation.maximum', { value: constraint.max });
  }
  if (constraint?.maxExclusive !== undefined && number >= constraint.maxExclusive) {
    return t('settings.validation.maximumExclusive', { value: constraint.maxExclusive });
  }
  if (setting.key === 'log_retention_days' && number !== -1 && (number < 1 || number > 365)) {
    return t('settings.validation.logRetention');
  }

  if (byteSettingKeys.has(setting.key)) {
    const bytes = Math.round(number * bytesPerMiB);
    if (!Number.isSafeInteger(bytes)) return t('settings.validation.smallerSize');
    if (number !== 0 && bytes === 0) return t('settings.validation.zeroOrOneByte');
    if (setting.key !== 'responses_ws_max_transcript_bytes' && bytes < 1) {
      return t('settings.validation.oneByteMinimum');
    }
  }
  return '';
}

function validateCustomPricingInput(value) {
  if (new TextEncoder().encode(value).length > maxCustomPricingBytes) {
    return t('settings.validation.customPricingTooLarge');
  }
  let parsed;
  try {
    parsed = JSON.parse(value);
  } catch (_) {
    return t('settings.validation.customPricingJSON');
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return t('settings.validation.customPricingObject');
  }
  const ids = new Set();
  const validID = /^[a-z0-9][a-z0-9._:/-]*$/;
  const allowedFields = new Set([...customPricingContractFields]);
  const finiteNonNegative = (n) => typeof n === 'number' && Number.isFinite(n) && n >= 0;
  const checkObjectFields = (object, fields) => Object.keys(object).every((key) => fields.has(key));
  const checkPrices = (object) => Object.values(object).every(finiteNonNegative);
  for (const [rawID, object] of Object.entries(parsed)) {
    const id = rawID.trim().toLowerCase();
    if (!validID.test(id) || /\s/.test(id) || ids.has(id)) return t('settings.validation.customPricingModelID');
    ids.add(id);
    if (ids.size > maxCustomPricingModels || !object || typeof object !== 'object' || Array.isArray(object)) {
      return ids.size > maxCustomPricingModels
        ? t('settings.validation.customPricingModelLimit', { max: maxCustomPricingModels })
        : t('settings.validation.customPricingObject');
    }
    if (!checkObjectFields(object, allowedFields) || !checkPrices(object)) return t('settings.validation.customPricingFields');
  }
  return '';
}

const runtimeMetricDomains = [
  {
    sourceKey: 'process',
    titleKey: 'settings.runtimeMetrics.group.process',
    descriptionKey: 'settings.runtimeMetrics.processNote',
    metrics: [
      { key: 'uptime_seconds', labelKey: 'settings.runtimeMetrics.metric.uptime', format: 'duration' },
      { key: 'concurrency_slots_in_use', labelKey: 'settings.runtimeMetrics.metric.concurrencySlotsInUse' },
      { key: 'max_concurrency', labelKey: 'settings.runtimeMetrics.metric.maxConcurrency' },
      { key: 'goroutines', labelKey: 'settings.runtimeMetrics.metric.goroutines' }
    ]
  },
  {
    sourceKey: 'process',
    titleKey: 'settings.runtimeMetrics.group.resources',
    descriptionKey: 'settings.runtimeMetrics.resourcesNote',
    metrics: [
      { key: 'cpu_usage_percent', labelKey: 'settings.runtimeMetrics.metric.cpuUsagePercent', format: 'percent' },
      { key: 'cpu_user_seconds', labelKey: 'settings.runtimeMetrics.metric.cpuUserSeconds', format: 'seconds' },
      { key: 'cpu_system_seconds', labelKey: 'settings.runtimeMetrics.metric.cpuSystemSeconds', format: 'seconds' },
      { key: 'rss_bytes', labelKey: 'settings.runtimeMetrics.metric.rssBytes', format: 'bytes', zeroUnavailable: true },
      { key: 'max_rss_bytes', labelKey: 'settings.runtimeMetrics.metric.maxRssBytes', format: 'bytes', zeroUnavailable: true },
      { key: 'heap_alloc_bytes', labelKey: 'settings.runtimeMetrics.metric.heapAllocBytes', format: 'bytes' },
      { key: 'heap_sys_bytes', labelKey: 'settings.runtimeMetrics.metric.heapSysBytes', format: 'bytes' },
      { key: 'gc_count', labelKey: 'settings.runtimeMetrics.metric.gcCount' },
      { key: 'gc_pause_total_ns', labelKey: 'settings.runtimeMetrics.metric.gcPauseTotal', format: 'durationNs' },
      { key: 'gc_cpu_percent', labelKey: 'settings.runtimeMetrics.metric.gcCpuPercent', format: 'percent' },
      { key: 'sse_framing_repairs', labelKey: 'settings.runtimeMetrics.metric.sseFramingRepairs' }
    ]
  },
  {
    sourceKey: 'http_proxy',
    titleKey: 'settings.runtimeMetrics.group.httpProxy',
    descriptionKey: 'settings.runtimeMetrics.httpProxyNote',
    metrics: [
      { key: 'active_requests', labelKey: 'settings.runtimeMetrics.metric.httpActiveRequests' },
      { key: 'completed_requests', labelKey: 'settings.runtimeMetrics.metric.httpCompletedRequests' },
      { key: 'non_error_responses', labelKey: 'settings.runtimeMetrics.metric.httpNonErrorResponses' },
      { key: 'client_error_responses', labelKey: 'settings.runtimeMetrics.metric.httpClientErrorResponses' },
      { key: 'server_error_responses', labelKey: 'settings.runtimeMetrics.metric.httpServerErrorResponses' },
      { key: 'streaming_requests', labelKey: 'settings.runtimeMetrics.metric.httpStreamingRequests' },
      { key: 'non_streaming_requests', labelKey: 'settings.runtimeMetrics.metric.httpNonStreamingRequests' },
      { key: 'request_body_bytes', labelKey: 'settings.runtimeMetrics.metric.httpRequestBodyBytes', format: 'bytes' },
      { key: 'response_body_bytes', labelKey: 'settings.runtimeMetrics.metric.httpResponseBodyBytes', format: 'bytes' }
    ]
  },
  {
    sourceKey: 'rates',
    titleKey: 'settings.runtimeMetrics.group.rates',
    descriptionKey: 'settings.runtimeMetrics.ratesNote',
    optional: true,
    metrics: [
      { key: 'rpm_current', labelKey: 'settings.runtimeMetrics.metric.rpmCurrent' },
      { key: 'rpm_peak', labelKey: 'settings.runtimeMetrics.metric.rpmPeak' },
      { key: 'rpm_avg', labelKey: 'settings.runtimeMetrics.metric.rpmAvg', format: 'decimal' },
      { key: 'qps_current', labelKey: 'settings.runtimeMetrics.metric.qpsCurrent', format: 'decimal' }
    ]
  },
  {
    sourceKey: 'trend',
    titleKey: 'settings.runtimeMetrics.group.trend',
    descriptionKey: 'settings.runtimeMetrics.trendNote',
    optional: true,
    allowArray: true,
    metrics: [],
    renderExtra: renderTrendChart
  },
  {
    sourceKey: 'gate',
    titleKey: 'settings.runtimeMetrics.group.gate',
    descriptionKey: 'settings.runtimeMetrics.gateNote',
    optional: true,
    metrics: [
      { key: 'latched', labelKey: 'settings.runtimeMetrics.metric.gateLatched', format: 'boolean' },
      { key: 'limited_until', labelKey: 'settings.runtimeMetrics.metric.gateLimitedUntil', format: 'isoTime' },
      { key: 'sendable', labelKey: 'settings.runtimeMetrics.metric.gateSendable', format: 'boolean' },
      { key: 'window_used', labelKey: 'settings.runtimeMetrics.metric.gateWindowUsed' },
      { key: 'window_quota', labelKey: 'settings.runtimeMetrics.metric.gateWindowQuota' },
      { key: 'waiters', labelKey: 'settings.runtimeMetrics.metric.gateWaiters' },
      { key: 'latch_count', labelKey: 'settings.runtimeMetrics.metric.gateLatchCount' },
      { key: 'drip_count', labelKey: 'settings.runtimeMetrics.metric.gateDripCount' },
      { key: 'reject_latched_count', labelKey: 'settings.runtimeMetrics.metric.gateRejectLatched' },
      { key: 'reject_hold_count', labelKey: 'settings.runtimeMetrics.metric.gateRejectHold' },
      { key: 'window_next', labelKey: 'settings.runtimeMetrics.metric.gateWindowNext', format: 'isoTime' }
    ],
    renderExtra: renderGateExtra
  },
  {
    sourceKey: 'rejects',
    titleKey: 'settings.runtimeMetrics.group.rejects',
    descriptionKey: 'settings.runtimeMetrics.rejectsNote',
    optional: true,
    metrics: [],
    renderExtra: renderRejectsExtra
  },
  {
    sourceKey: 'usage',
    titleKey: 'settings.runtimeMetrics.group.usage',
    descriptionKey: 'settings.runtimeMetrics.usageNote',
    optional: true,
    metrics: [],
    renderExtra: renderUsageLatencyExtra
  },
  {
    sourceKey: 'logs',
    titleKey: 'settings.runtimeMetrics.group.logs',
    descriptionKey: 'settings.runtimeMetrics.logsNote',
    metrics: [
      { key: 'backlog_entries', labelKey: 'settings.runtimeMetrics.metric.logBacklogEntries' },
      { key: 'queue_capacity_entries', labelKey: 'settings.runtimeMetrics.metric.logQueueCapacityEntries' },
      { key: 'dropped_entries', labelKey: 'settings.runtimeMetrics.metric.logDroppedEntries' },
      { key: 'persistence_failed_entries', labelKey: 'settings.runtimeMetrics.metric.logPersistenceFailedEntries' }
    ]
  },
  {
    sourceKey: 'debuglog',
    titleKey: 'settings.runtimeMetrics.group.debuglog',
    descriptionKey: 'settings.runtimeMetrics.debuglogNote',
    optional: true,
    metrics: [
      { key: 'enabled', labelKey: 'settings.runtimeMetrics.metric.debuglogEnabled', format: 'boolean' },
      { key: 'active_request_dirs', labelKey: 'settings.runtimeMetrics.metric.debuglogActiveDirs' },
      { key: 'log_rows', labelKey: 'settings.runtimeMetrics.metric.debuglogLogRows' },
      { key: 'db_bytes', labelKey: 'settings.runtimeMetrics.metric.debuglogDbBytes', format: 'bytes' },
      { key: 'log_row_retention_days', labelKey: 'settings.runtimeMetrics.metric.debuglogLogRowRetentionDays' },
      { key: 'retention_days', labelKey: 'settings.runtimeMetrics.metric.debuglogRetentionDays' },
      { key: 'max_total_mb', labelKey: 'settings.runtimeMetrics.metric.debuglogMaxTotalMb' },
      { key: 'payload_hours', labelKey: 'settings.runtimeMetrics.metric.debuglogPayloadHours' },
      { key: 'keep_error_dirs', labelKey: 'settings.runtimeMetrics.metric.debuglogKeepErrorDirs' },
      { key: 'log_root', labelKey: 'settings.runtimeMetrics.metric.debuglogLogRoot', format: 'text' }
    ],
    renderExtra: renderDebuglogExtra
  },
  {
    sourceKey: 'storage',
    titleKey: 'settings.runtimeMetrics.group.storage',
    descriptionKey: 'settings.runtimeMetrics.storageNote',
    optional: true,
    metrics: [
      { key: 'primary_sync_pending', labelKey: 'settings.runtimeMetrics.metric.primarySyncPending' },
      { key: 'primary_sync_failures', labelKey: 'settings.runtimeMetrics.metric.primarySyncFailures' },
      { key: 'primary_sync_dropped', labelKey: 'settings.runtimeMetrics.metric.primarySyncDropped' },
      { key: 'sqlite_read_failures', labelKey: 'settings.runtimeMetrics.metric.sqliteReadFailures' },
      { key: 'analytics_reads_primary', labelKey: 'settings.runtimeMetrics.metric.analyticsReadsPrimary', format: 'boolean' },
      { key: 'primary_sync_last_success_unix_ms', labelKey: 'settings.runtimeMetrics.metric.primarySyncLastSuccess', format: 'unixMilliseconds' }
    ]
  },
  {
    sourceKey: 'warm',
    titleKey: 'settings.runtimeMetrics.group.warm',
    descriptionKey: 'settings.runtimeMetrics.warmNote',
    optional: true,
    metrics: [
      { key: 'enabled', labelKey: 'settings.runtimeMetrics.metric.warmEnabled', format: 'boolean' },
      { key: 'entries', labelKey: 'settings.runtimeMetrics.metric.warmEntries' },
      { key: 'promoted', labelKey: 'settings.runtimeMetrics.metric.warmPromoted' },
      { key: 'suspects', labelKey: 'settings.runtimeMetrics.metric.warmSuspects' },
      { key: 'retained_bytes', labelKey: 'settings.runtimeMetrics.metric.warmRetainedBytes', format: 'bytes' },
      { key: 'pings_sent', labelKey: 'settings.runtimeMetrics.metric.warmPingsSent' },
      { key: 'ping_hits', labelKey: 'settings.runtimeMetrics.metric.warmPingHits' },
      { key: 'ping_misses', labelKey: 'settings.runtimeMetrics.metric.warmPingMisses' },
      { key: 'ping_hit_rate', labelKey: 'settings.runtimeMetrics.metric.warmPingHitRate', format: 'percent' },
      { key: 'ping_skips', labelKey: 'settings.runtimeMetrics.metric.warmPingSkips' },
      { key: 'ping_errors', labelKey: 'settings.runtimeMetrics.metric.warmPingErrors' },
      { key: 'retired', labelKey: 'settings.runtimeMetrics.metric.warmRetired' }
    ]
  }
];

const responsesRuntimeMetricGroups = [
  {
    titleKey: 'settings.runtimeMetrics.group.sessions',
    metrics: [
      { key: 'sessions', labelKey: 'settings.runtimeMetrics.metric.sessions' },
      { key: 'max_sessions', labelKey: 'settings.runtimeMetrics.metric.maxSessions' },
      { key: 'active_attachments', labelKey: 'settings.runtimeMetrics.metric.activeAttachments' }
    ]
  },
  {
    titleKey: 'settings.runtimeMetrics.group.downstream',
    metrics: [
      { key: 'downstream_connections', labelKey: 'settings.runtimeMetrics.metric.downstreamConnections' },
      { key: 'max_downstream_connections', labelKey: 'settings.runtimeMetrics.metric.maxDownstreamConnections' },
      { key: 'max_downstream_connections_per_token', labelKey: 'settings.runtimeMetrics.metric.maxDownstreamConnectionsPerToken' },
      { key: 'rejected_downstream_connections', labelKey: 'settings.runtimeMetrics.metric.rejectedDownstreamConnections' }
    ]
  },
  {
    titleKey: 'settings.runtimeMetrics.group.upstream',
    metrics: [
      { key: 'upstream_connections', labelKey: 'settings.runtimeMetrics.metric.upstreamConnections' },
      { key: 'upstream_handshakes', labelKey: 'settings.runtimeMetrics.metric.upstreamHandshakes' },
      { key: 'upstream_reuses', labelKey: 'settings.runtimeMetrics.metric.upstreamReuses' },
      { key: 'reconnects', labelKey: 'settings.runtimeMetrics.metric.reconnects' },
      { key: 'upstream_heartbeat_failures', labelKey: 'settings.runtimeMetrics.metric.upstreamHeartbeatFailures' },
      { key: 'upstream_queued_read_bytes', labelKey: 'settings.runtimeMetrics.metric.upstreamQueuedReadBytes', format: 'bytes' },
      { key: 'oldest_upstream_connection_seconds', labelKey: 'settings.runtimeMetrics.metric.oldestUpstreamConnection', format: 'duration' }
    ]
  },
  {
    titleKey: 'settings.runtimeMetrics.group.responsesEvents',
    metrics: [
      { key: 'ttl_expired', labelKey: 'settings.runtimeMetrics.metric.ttlExpired' },
      { key: 'capacity_rejected', labelKey: 'settings.runtimeMetrics.metric.capacityRejected' },
      { key: 'budget_rejected', labelKey: 'settings.runtimeMetrics.metric.budgetRejected' },
      { key: 'previous_response_misses', labelKey: 'settings.runtimeMetrics.metric.previousResponseMisses' }
    ]
  }
];

function bindSettingsPageActions() {
  const saveAllBtn = document.getElementById('save-all-btn');
  if (saveAllBtn && !saveAllBtn.dataset.bound) {
    saveAllBtn.addEventListener('click', () => {
      saveAllSettings();
    });
    saveAllBtn.dataset.bound = '1';
  }

  const runtimeMetricsBtn = document.getElementById('runtime-metrics-btn');
  if (runtimeMetricsBtn && !runtimeMetricsBtn.dataset.bound) {
    runtimeMetricsBtn.addEventListener('click', openRuntimeMetricsModal);
    runtimeMetricsBtn.dataset.bound = '1';
  }

  const processLogBtn = document.getElementById('process-log-btn');
  if (processLogBtn && !processLogBtn.dataset.bound) {
    processLogBtn.addEventListener('click', openProcessLogModal);
    processLogBtn.dataset.bound = '1';
  }

  const configRefreshBtn = document.getElementById('effective-config-refresh-btn');
  if (configRefreshBtn && !configRefreshBtn.dataset.bound) {
    configRefreshBtn.addEventListener('click', loadEffectiveConfig);
    configRefreshBtn.dataset.bound = '1';
  }

  const configReloadBtn = document.getElementById('effective-config-reload-btn');
  if (configReloadBtn && !configReloadBtn.dataset.bound) {
    configReloadBtn.addEventListener('click', reloadEffectiveConfig);
    configReloadBtn.dataset.bound = '1';
  }

  const multimodalFallbackBtn = document.getElementById('model-multimodal-fallback-btn');
  if (multimodalFallbackBtn && !multimodalFallbackBtn.dataset.bound) {
    multimodalFallbackBtn.addEventListener('click', (event) => openMultimodalFallbackModal(event.currentTarget));
    multimodalFallbackBtn.dataset.bound = '1';
  }

  const customPricingBtn = document.getElementById('model-custom-pricing-btn');
  if (customPricingBtn && !customPricingBtn.dataset.bound) {
    customPricingBtn.addEventListener('click', (event) => openCustomPricingModal(event.currentTarget));
    customPricingBtn.dataset.bound = '1';
  }

  const refreshBtn = document.getElementById('refresh-runtime-metrics-btn');
  if (refreshBtn && !refreshBtn.dataset.bound) {
    refreshBtn.addEventListener('click', loadRuntimeMetrics);
    refreshBtn.dataset.bound = '1';
  }

  document.querySelectorAll('[data-action="close-runtime-metrics"]').forEach((btn) => {
    if (btn.dataset.bound) return;
    btn.addEventListener('click', closeRuntimeMetricsModal);
    btn.dataset.bound = '1';
  });

  const modal = document.getElementById('runtimeMetricsModal');
  if (modal && !modal.dataset.bound) {
    modal.addEventListener('click', (event) => {
      if (event.target === modal) closeRuntimeMetricsModal();
    });
    modal.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') closeRuntimeMetricsModal();
    });
    modal.dataset.bound = '1';
  }

  bindMultimodalFallbackModal();
  bindCustomPricingModal();
  bindProcessLogModal();
}

function trapModalFocus(modal, event) {
  const focusable = Array.from(modal.querySelectorAll(
    'button:not([disabled]), input:not([disabled]):not([type="hidden"]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
  )).filter((element) => !element.hidden && element.offsetParent !== null);
  if (focusable.length === 0) return;
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

// ===== 多模态回退模型映射编辑器 =====
// 草稿三段式：打开时从 hidden input 解析进 DOM，编辑只改对话框 DOM，
// 取消即丢弃；应用时才收集 DOM 写回 hidden input。

function parseMultimodalFallback(value) {
  try {
    const parsed = JSON.parse(String(value || '{}'));
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return [];
    return Object.entries(parsed).map(([from, to]) => ({ from, to: String(to ?? '') }));
  } catch (_) {
    return [];
  }
}

function multimodalFallbackCount(value) {
  return parseMultimodalFallback(value).length;
}

function updateMultimodalFallbackSummary(value) {
  const summary = document.getElementById('model-multimodal-fallback-summary');
  if (!summary) return;
  summary.textContent = t('settings.multimodalFallback.ruleCount', {
    count: multimodalFallbackCount(value)
  });
}

// 与后端 RoutingModelName 近似：剥掉尾部思考后缀（如 (max)）并统一小写，
// 仅用于编辑器的重复/自映射提示，权威校验在后端。
function normalizeModelKey(value) {
  return String(value || '').trim().toLowerCase().replace(/\s*\([^()]*\)\s*$/, '');
}

function multimodalFallbackOptionSources() {
  if (Array.isArray(multimodalFallbackModelOptions)) return multimodalFallbackModelOptions;
  // 加载失败时至少保留当前草稿里出现的模型，已配值不至于从下拉里消失。
  const seen = new Set();
  for (const row of multimodalFallbackDraft) {
    for (const name of [row.from, row.to]) {
      if (name) seen.add(name);
    }
  }
  return Array.from(seen).sort();
}

function multimodalFallbackOptionsHtml(selected) {
  const options = multimodalFallbackOptionSources();
  if (selected && !options.includes(selected)) options.unshift(selected);
  return options.map((name) => (
    `<option value="${escapeHtml(name)}"${name === selected ? ' selected' : ''}>${escapeHtml(name)}</option>`
  )).join('');
}

function renderMultimodalFallbackRow(pair) {
  return `
    <div class="multimodal-fallback-row">
      <select class="form-input multimodal-fallback-select" data-field="from" aria-label="${escapeHtml(t('settings.multimodalFallback.fromModel'))}">
        ${multimodalFallbackOptionsHtml(pair.from)}
      </select>
      <span class="multimodal-fallback-arrow" aria-hidden="true">&rarr;</span>
      <select class="form-input multimodal-fallback-select" data-field="to" aria-label="${escapeHtml(t('settings.multimodalFallback.fallbackModel'))}">
        ${multimodalFallbackOptionsHtml(pair.to)}
      </select>
      <button type="button" class="btn-icon multimodal-fallback-remove-btn" data-action="remove-multimodal-fallback-row"
        aria-label="${escapeHtml(t('settings.multimodalFallback.removeRow'))}">&times;</button>
    </div>`;
}

function renderMultimodalFallbackDraft() {
  const container = document.getElementById('multimodalFallbackRows');
  const empty = document.getElementById('multimodalFallbackEmpty');
  if (!container) return;
  container.innerHTML = multimodalFallbackDraft.map(renderMultimodalFallbackRow).join('');
  if (empty) empty.hidden = multimodalFallbackDraft.length > 0;
}

async function loadMultimodalFallbackModelOptions() {
  if (multimodalFallbackModelOptions !== null) return;
  try {
    const data = await fetchDataWithAuth('/admin/models');
    multimodalFallbackModelOptions = Array.isArray(data?.models) ? data.models : [];
  } catch (err) {
    console.error('加载模型候选失败:', err);
    multimodalFallbackModelOptions = [];
  }
}

function showMultimodalFallbackError(message) {
  const error = document.getElementById('multimodalFallbackError');
  if (!error) return;
  error.textContent = message || '';
  error.hidden = !message;
}

async function openMultimodalFallbackModal(trigger) {
  const modal = document.getElementById('multimodalFallbackModal');
  const input = document.getElementById(modelMultimodalFallbackSettingKey);
  if (!modal || !input) return;

  multimodalFallbackPreviousFocus = trigger || document.activeElement;
  multimodalFallbackDraft = parseMultimodalFallback(input.value);
  showMultimodalFallbackError(null);
  await loadMultimodalFallbackModelOptions();
  renderMultimodalFallbackDraft();
  document.querySelector('.app-container')?.setAttribute('inert', '');
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  modal.querySelector('.close-btn')?.focus();
}

function closeMultimodalFallbackModal() {
  const modal = document.getElementById('multimodalFallbackModal');
  if (!modal) return;

  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.querySelector('.app-container')?.removeAttribute('inert');
  if (multimodalFallbackPreviousFocus?.isConnected) multimodalFallbackPreviousFocus.focus();
  multimodalFallbackPreviousFocus = null;
  multimodalFallbackDraft = [];
}

function addMultimodalFallbackRow() {
  // 现有行的选择值由 DOM 持有；追加后会全量重绘，先把用户编辑同步回草稿，
  // 否则重绘会用打开弹窗时的旧值覆盖现有行。
  const container = document.getElementById('multimodalFallbackRows');
  if (container) multimodalFallbackDraft = collectMultimodalFallbackDraft();
  if (multimodalFallbackDraft.length >= maxMultimodalFallbackMappings) {
    showMultimodalFallbackError(t('settings.multimodalFallback.errorLimit', { max: maxMultimodalFallbackMappings }));
    return;
  }
  multimodalFallbackDraft.push({ from: '', to: '' });
  renderMultimodalFallbackDraft();
}

function removeMultimodalFallbackRow(row) {
  const container = document.getElementById('multimodalFallbackRows');
  if (container) multimodalFallbackDraft = collectMultimodalFallbackDraft();
  const index = Array.from(row.parentNode?.children || []).indexOf(row);
  if (index >= 0) multimodalFallbackDraft.splice(index, 1);
  row.remove();
  const empty = document.getElementById('multimodalFallbackEmpty');
  if (empty) empty.hidden = multimodalFallbackDraft.length > 0;
}

// 收集对话框 DOM 中的映射。select 值由 searchable-select 增强层同步回原生
// select（dispatchSelectionEvents），DOM 就是当前草稿的真源。
function collectMultimodalFallbackDraft() {
  const rows = [];
  const container = document.getElementById('multimodalFallbackRows');
  if (container) {
    container.querySelectorAll('.multimodal-fallback-row').forEach((row) => {
      rows.push({
        from: String(row.querySelector('select[data-field="from"]')?.value || '').trim(),
        to: String(row.querySelector('select[data-field="to"]')?.value || '').trim()
      });
    });
  }
  return rows;
}

function validateMultimodalFallbackRows(rows) {
  if (rows.length > maxMultimodalFallbackMappings) {
    return t('settings.multimodalFallback.errorLimit', { max: maxMultimodalFallbackMappings });
  }
  const seen = new Set();
  for (const row of rows) {
    if (!row.from || !row.to) return t('settings.multimodalFallback.errorBlank');
    const fromKey = normalizeModelKey(row.from);
    const toKey = normalizeModelKey(row.to);
    if (fromKey === toKey) return t('settings.multimodalFallback.errorSelf', { model: row.from });
    if (seen.has(fromKey)) return t('settings.multimodalFallback.errorDuplicate', { model: row.from });
    seen.add(fromKey);
  }
  return null;
}

async function applyMultimodalFallback() {
  const rows = collectMultimodalFallbackDraft();
  const error = validateMultimodalFallbackRows(rows);
  if (error) {
    showMultimodalFallbackError(error);
    return;
  }
  const mapping = {};
  for (const row of rows) mapping[row.from] = row.to;

  const value = JSON.stringify(mapping);
  if (value === originalSettings[modelMultimodalFallbackSettingKey]) {
    closeMultimodalFallbackModal();
    return;
  }

  const modal = document.getElementById('multimodalFallbackModal');
  const applyButton = modal?.querySelector('[data-action="apply-multimodal-fallback"]');
  if (applyButton?.disabled) return;
  if (applyButton) {
    applyButton.disabled = true;
    applyButton.setAttribute('aria-busy', 'true');
  }
  showMultimodalFallbackError(null);

  try {
    const result = await fetchDataWithAuth('/admin/settings/batch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ [modelMultimodalFallbackSettingKey]: value })
    });
    syncSettingState(modelMultimodalFallbackSettingKey, value);
    closeMultimodalFallbackModal();
    showSuccess(result?.message || t('settings.msg.savedCount', { count: 1 }));
  } catch (err) {
    console.error('保存多模态回退映射异常:', err);
    showMultimodalFallbackError(t('settings.msg.saveFailed') + ': ' + err.message);
  } finally {
    if (applyButton) {
      applyButton.disabled = false;
      applyButton.removeAttribute('aria-busy');
    }
  }
}

function bindMultimodalFallbackModal() {
  const modal = document.getElementById('multimodalFallbackModal');
  if (!modal || modal.dataset.bound) return;

  modal.addEventListener('click', (event) => {
    if (event.target === modal) {
      closeMultimodalFallbackModal();
      return;
    }
    const button = event.target.closest('[data-action]');
    if (!button) return;
    switch (button.dataset.action) {
      case 'close-multimodal-fallback':
        closeMultimodalFallbackModal();
        break;
      case 'apply-multimodal-fallback':
        applyMultimodalFallback();
        break;
      case 'add-multimodal-fallback-row':
        addMultimodalFallbackRow();
        break;
      case 'remove-multimodal-fallback-row':
        removeMultimodalFallbackRow(button.closest('.multimodal-fallback-row'));
        break;
    }
  });
  modal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeMultimodalFallbackModal();
      return;
    }
    if (event.key === 'Tab') trapModalFocus(modal, event);
  });
  modal.dataset.bound = '1';
}

// ===== 自定义模型价格编辑器 =====
// 价格设置在这里以结构化表格编辑，提交时仍复用 model_custom_pricing 设置接口。

const customPricingFieldLabelKeys = {
  input_price: 'settings.customPricing.inputPrice',
  output_price: 'settings.customPricing.outputPrice',
  cache_read_price: 'settings.customPricing.cacheReadPrice',
  cache_write_price: 'settings.customPricing.cacheWritePrice',
  cache_write_price_high: 'settings.customPricing.cacheWritePriceHigh',
  cache_read_price_high: 'settings.customPricing.cacheReadPriceHigh',
  input_price_high: 'settings.customPricing.inputPriceHigh',
  output_price_high: 'settings.customPricing.outputPriceHigh'
};

function parseCustomPricing(value) {
  try {
    const parsed = JSON.parse(String(value || '{}'));
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return [];
    return Object.entries(parsed).map(([model, pricing]) => ({
      model,
      pricing: pricing && typeof pricing === 'object' && !Array.isArray(pricing) ? pricing : {}
    }));
  } catch (_) {
    return [];
  }
}

function updateCustomPricingSummary(value) {
  const summary = document.getElementById('model-custom-pricing-summary');
  if (!summary) return;
  summary.textContent = t('settings.customPricing.modelCount', {
    count: parseCustomPricing(value).length
  });
}

function customPricingDisplayNumber(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '';
  return String(value);
}

function customPricingNumberControl(field, value) {
  const label = escapeHtml(t(customPricingFieldLabelKeys[field] || field));
  return `<input type="number" class="form-input custom-pricing-number" data-cp-field="${field}"
    min="0" step="any" inputmode="decimal"
    value="${escapeHtml(customPricingDisplayNumber(value))}"
    aria-label="${label}" title="${label}">`;
}

function renderCustomPricingModel(entry, modelIndex) {
  const pricing = entry.pricing || {};
  const highContextDescription = escapeHtml(t('settings.customPricing.highContextDescription'));

  // 高上下文四项与基础四价同列同序，标签由表头承担。
  // 关键：输入框必须落在真正的 <td> 里，列宽由表格列算法统一决定。
  // 用 grid/flex 容器会算出与列宽无关的宽度，两行输入框永远对不齐。
  const highContextCells = customPricingHighContextFields
    .map((field) => `<td>${customPricingNumberControl(field, pricing[field])}</td>`)
    .join('');

  return `
    <tr class="custom-pricing-model-row" data-model-index="${modelIndex}">
      <td>
        <div class="custom-pricing-model-cell">
          <input type="text" class="form-input custom-pricing-model-id-input" data-cp-field="model_id" value="${escapeHtml(entry.model || '')}" spellcheck="false" required aria-label="${escapeHtml(t('settings.customPricing.modelId'))}" title="${escapeHtml(t('settings.customPricing.modelId'))}">
          <span class="custom-pricing-model-status" data-cp-status role="status" hidden></span>
        </div>
      </td>
      <td>${customPricingNumberControl('input_price', pricing.input_price)}</td>
      <td>${customPricingNumberControl('output_price', pricing.output_price)}</td>
      <td>${customPricingNumberControl('cache_read_price', pricing.cache_read_price)}</td>
      <td>${customPricingNumberControl('cache_write_price', pricing.cache_write_price)}</td>
      <td><button type="button" class="btn-icon" data-action="remove-custom-pricing-model" data-model-index="${modelIndex}" aria-label="${escapeHtml(t('settings.customPricing.removeModel'))}">&times;</button></td>
    </tr>
    <tr class="custom-pricing-high-row" data-model-index="${modelIndex}">
      <td>
        <span class="custom-pricing-high-label" title="${highContextDescription}">${escapeHtml(t('settings.customPricing.highContextPricing'))}</span>
      </td>
      ${highContextCells}
      <td></td>
    </tr>`;
}

function readCustomPricingNumber(input) {
  const value = String(input?.value ?? '').trim();
  if (value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) ? number : NaN;
}

// 一个模型占两行：基础价行 + 高上下文值行。
// 两行靠 data-model-index 配对，不再靠 nextElementSibling 猜位置——
// 行数或顺序一变，猜测就会静默错位（漏读价格、删错行）。
function customPricingHighRow(modelRow) {
  const index = modelRow?.dataset.modelIndex;
  if (index === undefined) return null;
  for (let row = modelRow.nextElementSibling; row; row = row.nextElementSibling) {
    // 本组两行相邻且 index 相同；遇到下个模型的行说明本组已结束。
    if (row.dataset.modelIndex !== index) break;
    if (row.classList.contains('custom-pricing-high-row')) return row;
  }
  return null;
}

function collectCustomPricingEntries() {
  const entries = [];
  document.querySelectorAll('#customPricingRows .custom-pricing-model-row').forEach((row) => {
    const high = customPricingHighRow(row);
    const pricing = {};
    for (const field of customPricingBasicFields) {
      const value = readCustomPricingNumber(row.querySelector(`[data-cp-field="${field}"]`));
      if (value !== null) pricing[field] = value;
    }
    if (high) {
      for (const field of customPricingHighContextFields) {
        const value = readCustomPricingNumber(high.querySelector(`[data-cp-field="${field}"]`));
        if (value !== null) pricing[field] = value;
      }
    }
    entries.push({
      model: String(row.querySelector('[data-cp-field="model_id"]')?.value || '').trim(),
      pricing
    });
  });
  return entries;
}

function validateCustomPricingEntries(entries) {
  if (entries.length > maxCustomPricingModels) {
    return t('settings.validation.customPricingModelLimit', { max: maxCustomPricingModels });
  }
  const seen = new Set();
  const validID = /^[a-z0-9][a-z0-9._:/-]*$/;
  for (const entry of entries) {
    const id = entry.model.toLowerCase();
    if (!validID.test(id) || /\s/.test(id)) return t('settings.validation.customPricingModelID');
    if (seen.has(id)) return t('settings.validation.customPricingModelID');
    if (customPricingTieredModels.has(id)) return t('settings.validation.customPricingTiered', { model: entry.model });
    seen.add(id);
  }
  const payload = {};
  for (const entry of entries) payload[entry.model] = entry.pricing;
  return validateCustomPricingInput(JSON.stringify(payload));
}

function customPricingEntriesToJSON(entries) {
  const payload = {};
  for (const entry of entries) payload[entry.model] = entry.pricing;
  return JSON.stringify(payload);
}

function canonicalizeCustomPricing(value) {
  if (Array.isArray(value)) return value.map(canonicalizeCustomPricing);
  if (value && typeof value === 'object') {
    return Object.keys(value).sort().reduce((result, key) => {
      result[key] = canonicalizeCustomPricing(value[key]);
      return result;
    }, {});
  }
  return value;
}

function customPricingEquivalent(first, second) {
  const toMap = (value) => parseCustomPricing(value).reduce((result, entry) => {
    result[entry.model.trim().toLowerCase()] = canonicalizeCustomPricing(entry.pricing);
    return result;
  }, {});
  return JSON.stringify(canonicalizeCustomPricing(toMap(first))) === JSON.stringify(canonicalizeCustomPricing(toMap(second)));
}

function renderCustomPricingDraft() {
  const rows = document.getElementById('customPricingRows');
  const empty = document.getElementById('customPricingEmpty');
  if (!rows) return;
  rows.innerHTML = customPricingDraft.map(renderCustomPricingModel).join('');
  if (empty) empty.hidden = customPricingDraft.length > 0;
  applyCustomPricingFilter();
}

function applyCustomPricingFilter() {
  const search = document.getElementById('customPricingSearch');
  if (search) customPricingModelFilter = String(search.value || '').trim().toLowerCase();
  document.querySelectorAll('#customPricingRows .custom-pricing-model-row').forEach((row) => {
    const model = String(row.querySelector('[data-cp-field="model_id"]')?.value || '').toLowerCase();
    const hidden = customPricingModelFilter !== '' && !model.includes(customPricingModelFilter);
    const high = customPricingHighRow(row);
    row.hidden = hidden;
    if (high) high.hidden = hidden;
  });
}

function setCustomPricingModelStatus(row, message, state = '') {
  const status = row?.querySelector('[data-cp-status]');
  if (!status) return;
  status.textContent = message || '';
  status.hidden = !message;
  status.dataset.state = state;
}

async function loadCustomPricingDefaults(input) {
  const row = input?.closest('.custom-pricing-model-row');
  const modelID = String(input?.value || '').trim().toLowerCase();
  if (!row || !modelID || row.dataset.defaultModelId === modelID) return;

  const modelIndex = Number(row.dataset.modelIndex);
  const entries = collectCustomPricingEntries();
  const entry = entries[modelIndex];
  if (!entry) return;
  row.dataset.defaultModelId = modelID;

  // 已有值时不自动改写，避免覆盖用户正在编辑的配置。
  if (Object.keys(entry.pricing || {}).length > 0) return;

  setCustomPricingModelStatus(row, t('settings.customPricing.loadingDefaults'), 'loading');
  try {
    const result = await fetchDataWithAuth(`/admin/model-pricing?model=${encodeURIComponent(modelID)}`);
    if (!row.isConnected || String(row.querySelector('[data-cp-field="model_id"]')?.value || '').trim().toLowerCase() !== modelID) return;
    if (!result?.found || !result.pricing || typeof result.pricing !== 'object') {
      setCustomPricingModelStatus(row, t('settings.customPricing.defaultNotFound'), 'empty');
      return;
    }
    const latestEntries = collectCustomPricingEntries();
    if (latestEntries[modelIndex] && Object.keys(latestEntries[modelIndex].pricing || {}).length > 0) {
      setCustomPricingModelStatus(row, '', '');
      return;
    }

    // 系统分层定价（Qwen 全系价格只存在分层表里）无法用自定义价格完整表达，
    // 预填会丢掉中间档导致静默少计费，这里拒绝预填并保留系统价格。
    const systemTiers = result.pricing.token_pricing_tiers;
    if (Array.isArray(systemTiers) && systemTiers.length > 0) {
      customPricingTieredModels.add(modelID);
      setCustomPricingModelStatus(row, t('settings.customPricing.tieredNotOverridable'), 'empty');
      return;
    }

    latestEntries[modelIndex].pricing = projectCustomPricingDefaults(result.pricing);
    customPricingDraft = latestEntries;
    renderCustomPricingDraft();
    const renderedRow = document.querySelector(`#customPricingRows .custom-pricing-model-row[data-model-index="${modelIndex}"]`);
    setCustomPricingModelStatus(renderedRow, t('settings.customPricing.defaultLoaded'), 'success');
    renderedRow?.querySelector('[data-cp-field="model_id"]')?.focus();
  } catch (err) {
    console.warn('加载系统模型价格失败:', err);
    if (row.isConnected) setCustomPricingModelStatus(row, t('settings.customPricing.defaultLoadFailed'), 'error');
  }
}

function showCustomPricingError(message) {
  const error = document.getElementById('customPricingError');
  if (!error) return;
  error.textContent = message || '';
  error.hidden = !message;
}

function openCustomPricingModal(trigger) {
  const modal = document.getElementById('customPricingModal');
  const input = document.getElementById(modelCustomPricingSettingKey);
  if (!modal || !input) return;
  customPricingPreviousFocus = trigger || document.activeElement;
  customPricingDraft = parseCustomPricing(input.value);
  customPricingModelFilter = '';
  customPricingTieredModels.clear();
  const search = document.getElementById('customPricingSearch');
  if (search) search.value = '';
  showCustomPricingError(null);
  renderCustomPricingDraft();
  document.querySelector('.app-container')?.setAttribute('inert', '');
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  modal.querySelector('.close-btn')?.focus();
}

function closeCustomPricingModal() {
  const modal = document.getElementById('customPricingModal');
  if (!modal) return;
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.querySelector('.app-container')?.removeAttribute('inert');
  if (customPricingPreviousFocus?.isConnected) customPricingPreviousFocus.focus();
  customPricingPreviousFocus = null;
  customPricingDraft = [];
}

function addCustomPricingModel() {
  const search = document.getElementById('customPricingSearch');
  if (search && search.value) {
    search.value = '';
    customPricingModelFilter = '';
  }
  customPricingDraft = collectCustomPricingEntries();
  if (customPricingDraft.length >= maxCustomPricingModels) {
    showCustomPricingError(t('settings.validation.customPricingModelLimit', { max: maxCustomPricingModels }));
    return;
  }
  customPricingDraft.push({ model: '', pricing: {} });
  renderCustomPricingDraft();
  const modelRows = document.querySelectorAll('#customPricingRows .custom-pricing-model-row');
  modelRows[modelRows.length - 1]?.querySelector('[data-cp-field="model_id"]')?.focus();
}

function removeCustomPricingModel(button) {
  const row = button?.closest('.custom-pricing-model-row');
  if (!row) return;
  const high = customPricingHighRow(row);
  row.remove();
  high?.remove();
  customPricingDraft = collectCustomPricingEntries();
  renderCustomPricingDraft();
}

async function applyCustomPricing() {
  const entries = collectCustomPricingEntries();
  const validationError = validateCustomPricingEntries(entries);
  if (validationError) {
    showCustomPricingError(validationError);
    return;
  }
  const value = customPricingEntriesToJSON(entries);
  const current = document.getElementById(modelCustomPricingSettingKey)?.value || '{}';
  if (customPricingEquivalent(current, value)) {
    closeCustomPricingModal();
    return;
  }

  const modal = document.getElementById('customPricingModal');
  const applyButton = modal?.querySelector('[data-action="apply-custom-pricing"]');
  if (applyButton?.disabled) return;
  if (applyButton) {
    applyButton.disabled = true;
    applyButton.setAttribute('aria-busy', 'true');
  }
  showCustomPricingError(null);
  try {
    const result = await fetchDataWithAuth('/admin/settings/batch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ [modelCustomPricingSettingKey]: value })
    });
    syncSettingState(modelCustomPricingSettingKey, value);
    closeCustomPricingModal();
    showSuccess(result?.message || t('settings.msg.savedCount', { count: 1 }));
  } catch (err) {
    console.error('保存自定义模型价格异常:', err);
    showCustomPricingError(t('settings.msg.saveFailed') + ': ' + err.message);
  } finally {
    if (applyButton) {
      applyButton.disabled = false;
      applyButton.removeAttribute('aria-busy');
    }
  }
}

function bindCustomPricingModal() {
  const modal = document.getElementById('customPricingModal');
  if (!modal || modal.dataset.bound) return;
  modal.addEventListener('click', (event) => {
    if (event.target === modal) {
      closeCustomPricingModal();
      return;
    }
    const button = event.target.closest('[data-action]');
    if (!button) return;
    switch (button.dataset.action) {
      case 'close-custom-pricing': closeCustomPricingModal(); break;
      case 'apply-custom-pricing': applyCustomPricing(); break;
      case 'add-custom-pricing-model': addCustomPricingModel(); break;
      case 'remove-custom-pricing-model': removeCustomPricingModel(button); break;
    }
  });
  modal.addEventListener('input', (event) => {
    if (event.target.id === 'customPricingSearch' || event.target.matches('[data-cp-field="model_id"]')) applyCustomPricingFilter();
  });
  modal.addEventListener('focusout', (event) => {
    if (event.target.matches('[data-cp-field="model_id"]')) loadCustomPricingDefaults(event.target);
  });
  modal.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && event.target.matches('[data-cp-field="model_id"]')) {
      event.preventDefault();
      loadCustomPricingDefaults(event.target);
      return;
    }
    if (event.key === 'Escape') {
      event.preventDefault();
      closeCustomPricingModal();
      return;
    }
    if (event.key === 'Tab') trapModalFocus(modal, event);
  });
  modal.dataset.bound = '1';
}

function openRuntimeMetricsModal() {
  const modal = document.getElementById('runtimeMetricsModal');
  if (!modal) return;

  runtimeMetricsPreviousFocus = document.activeElement;
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  modal.querySelector('.close-btn')?.focus();
  loadRuntimeMetrics();
  if (runtimeMetricsRefreshTimer === null) {
    runtimeMetricsRefreshTimer = setInterval(() => loadRuntimeMetrics({ silent: true }), RUNTIME_METRICS_REFRESH_MS);
  }
}

function closeRuntimeMetricsModal() {
  const modal = document.getElementById('runtimeMetricsModal');
  if (!modal) return;

  if (runtimeMetricsRefreshTimer !== null) {
    clearInterval(runtimeMetricsRefreshTimer);
    runtimeMetricsRefreshTimer = null;
  }
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  if (runtimeMetricsPreviousFocus?.isConnected) runtimeMetricsPreviousFocus.focus();
  runtimeMetricsPreviousFocus = null;
}

function normalizeRuntimeMetric(value) {
  if (value === null || value === undefined || value === '') return null;
  const numeric = Number(value);
  return Number.isFinite(numeric) && numeric >= 0 ? numeric : null;
}

function runtimeMetricsLocale() {
  return window.i18n?.getLocale?.() || document.documentElement.lang || 'zh-CN';
}

function formatRuntimeInteger(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  return new Intl.NumberFormat(runtimeMetricsLocale(), { maximumFractionDigits: 0 }).format(numeric);
}

function formatRuntimeDecimal(value, maximumFractionDigits = 1) {
  return new Intl.NumberFormat(runtimeMetricsLocale(), { maximumFractionDigits }).format(value);
}

function formatRuntimeBytes(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';

  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let unitIndex = 0;
  let amount = numeric;
  while (amount >= 1024 && unitIndex < units.length - 1) {
    amount /= 1024;
    unitIndex++;
  }
  const digits = unitIndex === 0 ? 0 : 1;
  return `${formatRuntimeDecimal(amount, digits)} ${units[unitIndex]}`;
}

function formatRuntimeDuration(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';

  const seconds = Math.round(numeric);
  const hours = Math.floor(seconds / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (hours > 0) return t('common.timeHM', { h: hours, m: minutes });
  if (minutes > 0) return t('common.timeMS', { m: minutes, s: seconds % 60 });
  return t('common.timeS', { s: seconds });
}

function formatRuntimeBoolean(value) {
  if (typeof value !== 'boolean') return '—';
  return t(value ? 'common.yes' : 'common.no');
}

function formatRuntimeTimestamp(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null || numeric <= 0) return '—';
  const date = new Date(numeric);
  return Number.isNaN(date.getTime()) ? '—' : date.toLocaleString(runtimeMetricsLocale());
}

function formatRuntimePercent(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  return `${formatRuntimeDecimal(numeric, 1)}%`;
}

function formatRuntimeSeconds(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  if (numeric < 60) return t('common.timeS', { s: formatRuntimeDecimal(numeric, 1) });
  return formatRuntimeDuration(numeric);
}

function formatRuntimeDurationNs(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  if (numeric < 1e9) return `${formatRuntimeDecimal(numeric / 1e6, 1)} ms`;
  return formatRuntimeDuration(numeric / 1e9);
}

// gate/usage 的时间字段是 RFC3339 字符串（ISO），与 unixMilliseconds 不同源。
function formatRuntimeISOTime(value) {
  if (typeof value !== 'string' || value.trim() === '') return '—';
  const time = Date.parse(value);
  return Number.isNaN(time) ? '—' : new Date(time).toLocaleString(runtimeMetricsLocale());
}

function formatRuntimeNumber(value, digits = 2) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  return formatRuntimeDecimal(numeric, digits);
}

// 毫秒级时长：usage 分位数（int64 ms）与闩事件时长（at/until 差值）共用。
function formatRuntimeMilliseconds(value) {
  const numeric = normalizeRuntimeMetric(value);
  if (numeric === null) return '—';
  if (numeric < 1000) return `${formatRuntimeDecimal(numeric, 0)} ms`;
  return formatRuntimeSeconds(numeric / 1000);
}

function formatRuntimeMetric(metric, stats) {
  if (metric.zeroUnavailable && normalizeRuntimeMetric(stats[metric.key]) === 0) return '—';
  if (metric.format === 'bytes') return formatRuntimeBytes(stats[metric.key]);
  if (metric.format === 'duration') return formatRuntimeDuration(stats[metric.key]);
  if (metric.format === 'seconds') return formatRuntimeSeconds(stats[metric.key]);
  if (metric.format === 'durationNs') return formatRuntimeDurationNs(stats[metric.key]);
  if (metric.format === 'milliseconds') return formatRuntimeMilliseconds(stats[metric.key]);
  if (metric.format === 'percent') return formatRuntimePercent(stats[metric.key]);
  if (metric.format === 'decimal') return formatRuntimeNumber(stats[metric.key]);
  if (metric.format === 'boolean') return formatRuntimeBoolean(stats[metric.key]);
  if (metric.format === 'unixMilliseconds') return formatRuntimeTimestamp(stats[metric.key]);
  if (metric.format === 'isoTime') return formatRuntimeISOTime(stats[metric.key]);
  if (metric.format === 'text') {
    const value = stats[metric.key];
    return value === null || value === undefined || String(value).trim() === '' ? '—' : String(value);
  }
  return formatRuntimeInteger(stats[metric.key]);
}

function renderRuntimeMetricCard(metric, stats) {
  return `
    <div class="runtime-metric-card">
      <span class="runtime-metric-label">${escapeHtml(t(metric.labelKey))}</span>
      <strong class="runtime-metric-value">${escapeHtml(formatRuntimeMetric(metric, stats))}</strong>
      <code class="runtime-metric-key">${escapeHtml(metric.key)}</code>
    </div>`;
}

function renderRuntimeMetricDomain(domain, payload) {
  const stats = payload[domain.sourceKey];
  // trend 组载荷是桶数组而非对象，allowArray 放行给 renderExtra 自己处理。
  const hasData = domain.allowArray
    ? Array.isArray(stats)
    : stats !== null && stats !== undefined && typeof stats === 'object' && !Array.isArray(stats);
  if (!hasData) {
    return domain.optional ? '' : `
      <section class="runtime-metrics-section">
        <div class="runtime-metrics-section-header">
          <h3>${escapeHtml(t(domain.titleKey))}</h3>
        </div>
        <p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.groupUnavailable'))}</p>
      </section>`;
  }
  const grid = domain.metrics.length
    ? `<div class="runtime-metrics-grid">${domain.metrics.map((metric) => renderRuntimeMetricCard(metric, stats)).join('')}</div>`
    : '';
  const extra = typeof domain.renderExtra === 'function' ? domain.renderExtra(stats, payload) : '';
  return `
    <section class="runtime-metrics-section">
      <div class="runtime-metrics-section-header">
        <h3>${escapeHtml(t(domain.titleKey))}</h3>
      </div>
      <p class="runtime-metrics-section-description">${escapeHtml(t(domain.descriptionKey))}</p>
      ${grid}
      ${extra}
    </section>`;
}

function renderRuntimeMetricGroup(group, stats) {
  return `
    <section class="runtime-metrics-subsection">
      <div class="runtime-metrics-subsection-header">
        <h4>${escapeHtml(t(group.titleKey))}</h4>
      </div>
      <div class="runtime-metrics-grid">
        ${group.metrics.map((metric) => renderRuntimeMetricCard(metric, stats)).join('')}
      </div>
    </section>`;
}

function renderRuntimeMetricTable(headers, rowsHtml) {
  return `
    <div class="table-container">
      <table class="modern-table">
        <thead><tr>${headers.map((header) => `<th>${escapeHtml(header)}</th>`).join('')}</tr></thead>
        <tbody>${rowsHtml}</tbody>
      </table>
    </div>`;
}

// 速率闸门附加块：闩中横幅 + 闩迁移事件表（新在前，label 由服务端随事件下发）。
function renderGateExtra(stats) {
  let html = '';
  if (stats.latched) {
    html += `<div class="custom-rules-error" role="alert">${escapeHtml(t('settings.runtimeMetrics.gateLatchedBanner', { until: formatRuntimeISOTime(stats.limited_until) }))}</div>`;
  }
  const events = Array.isArray(stats.events) ? stats.events.slice(0, 20) : [];
  if (!events.length) return html;

  const rows = events.map((event) => {
    const at = Date.parse(event.at);
    const until = event.until ? Date.parse(event.until) : NaN;
    let detail = '';
    if (event.kind === 'latched' && Number.isFinite(at) && Number.isFinite(until)) {
      detail = t('settings.runtimeMetrics.gateDetailLatch', { duration: formatRuntimeMilliseconds(until - at) });
    } else if (event.kind === 'released' && Number.isFinite(at) && Number.isFinite(until)) {
      detail = t('settings.runtimeMetrics.gateDetailReleased', { duration: formatRuntimeMilliseconds(Math.max(0, until - at)) });
    } else if (event.kind === 'restored') {
      detail = t('settings.runtimeMetrics.gateDetailRestored');
    }
    return `<tr>
      <td>${escapeHtml(Number.isFinite(at) ? new Date(at).toLocaleString(runtimeMetricsLocale()) : '—')}</td>
      <td>${escapeHtml(event.label || event.kind || '—')}</td>
      <td>${escapeHtml(formatRuntimeISOTime(event.until))}</td>
      <td>${escapeHtml(detail || '—')}</td>
    </tr>`;
  }).join('');

  html += `
    <div class="runtime-metrics-subsection-header">
      <h4>${escapeHtml(t('settings.runtimeMetrics.gateEvents'))}</h4>
    </div>
    ${renderRuntimeMetricTable([
      t('settings.runtimeMetrics.eventColTime'),
      t('settings.runtimeMetrics.gateEventColKind'),
      t('settings.runtimeMetrics.gateEventColUntil'),
      t('settings.runtimeMetrics.gateEventColDetail')
    ], rows)}`;
  return html;
}

// 管线前拒绝：分原因计数卡 + 最近事件表——这类请求不产生调试记录与
// logs 行，这里是唯一结构化足迹。reason 显示名取服务端下发的 labels。
function renderRejectsExtra(stats) {
  const byReason = stats.by_reason && typeof stats.by_reason === 'object' ? stats.by_reason : {};
  const labels = {};
  for (const entry of stats.labels || []) {
    if (entry && entry.reason) labels[entry.reason] = entry.label;
  }
  const ordered = Object.keys(labels).concat(Object.keys(byReason).filter((k) => !(k in labels)));
  const cards = ordered
    .filter((reason) => (normalizeRuntimeMetric(byReason[reason]) || 0) > 0)
    .map((reason) => `
      <div class="runtime-metric-card">
        <span class="runtime-metric-label">${escapeHtml(labels[reason] || reason)}</span>
        <strong class="runtime-metric-value">${escapeHtml(formatRuntimeInteger(byReason[reason]))}</strong>
        <code class="runtime-metric-key">${escapeHtml(reason)}</code>
      </div>`);

  let html = cards.length
    ? `<div class="runtime-metrics-grid">${cards.join('')}</div>`
    : `<p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.rejectsNone'))}</p>`;

  const recent = Array.isArray(stats.recent) ? stats.recent.slice(0, 30) : [];
  if (recent.length) {
    const rows = recent.map((event) => {
      const at = normalizeRuntimeMetric(event.at);
      const ua = event.user_agent ? String(event.user_agent) : '';
      const who = escapeHtml(event.ip || '—')
        + (event.key_hash ? ` <code class="runtime-metric-key">${escapeHtml(event.key_hash)}</code>` : '')
        + (ua ? `<div title="${escapeHtml(ua)}">${escapeHtml(ua.length > 48 ? ua.slice(0, 48) + '…' : ua)}</div>` : '');
      return `<tr>
        <td>${escapeHtml(at !== null ? new Date(at * 1000).toLocaleString(runtimeMetricsLocale()) : '—')}</td>
        <td><span class="runtime-transcript-status runtime-transcript-status--warning" title="${escapeHtml(event.reason || '')}">${escapeHtml(labels[event.reason] || event.reason || '—')}</span></td>
        <td>${escapeHtml(String(event.status ?? '—'))}</td>
        <td>${escapeHtml(event.path || '—')}</td>
        <td>${who}</td>
      </tr>`;
    }).join('');
    html += `
      <div class="runtime-metrics-subsection-header">
        <h4>${escapeHtml(t('settings.runtimeMetrics.rejectsRecent'))}</h4>
      </div>
      ${renderRuntimeMetricTable([
        t('settings.runtimeMetrics.eventColTime'),
        t('settings.runtimeMetrics.rejectColReason'),
        t('settings.runtimeMetrics.rejectColStatus'),
        t('settings.runtimeMetrics.rejectColPath'),
        t('settings.runtimeMetrics.rejectColClient')
      ], rows)}`;
    if (stats.recent.length > recent.length) {
      html += `<p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.rejectsTruncated', { count: recent.length }))}</p>`;
    }
  }
  return html;
}

// 60 分钟逐 10 秒请求/错误趋势的 SVG 迷你图；闩时段（gate.latch_ranges）
// 叠琥珀底色，拒绝风暴/流量塌陷与「当时在闩内」直接对得上。
function renderTrendChart(trend, payload) {
  if (!trend.length) return '';
  let totalRequests = 0;
  let totalErrors = 0;
  let peak = 0;
  for (const point of trend) {
    const requests = normalizeRuntimeMetric(point.requests) || 0;
    const errors = normalizeRuntimeMetric(point.errors) || 0;
    totalRequests += requests;
    totalErrors += errors;
    peak = Math.max(peak, requests, errors);
  }

  const width = 720;
  const height = 72;
  const count = trend.length;
  const xAt = (index) => (count > 1 ? (index / (count - 1)) * width : width / 2);
  const yAt = (value) => height - (peak > 0 ? (value / peak) * (height - 4) : 0) - 2;
  const pathOf = (field) => trend
    .map((point, index) => `${index === 0 ? 'M' : 'L'}${xAt(index).toFixed(1)},${yAt(normalizeRuntimeMetric(point[field]) || 0).toFixed(1)}`)
    .join(' ');
  const requestPath = pathOf('requests');
  const errorPath = pathOf('errors');
  const areaPath = `${requestPath} L${width},${height} L0,${height} Z`;

  let latchRects = '';
  const t0 = (trend[0].at || 0) * 1000;
  const t1 = (trend[count - 1].at || 0) * 1000;
  if (t1 > t0 && Array.isArray(payload?.gate?.latch_ranges)) {
    latchRects = payload.gate.latch_ranges.map((range) => {
      const start = range.start ? Date.parse(range.start) : t0;
      const end = Date.parse(range.end);
      if (!Number.isFinite(end)) return '';
      const x0 = Math.max(0, ((Math.max(start, t0) - t0) / (t1 - t0)) * width);
      const x1 = Math.min(width, ((Math.min(end, t1) - t0) / (t1 - t0)) * width);
      if (x1 <= x0) return '';
      return `<rect x="${x0.toFixed(1)}" y="0" width="${(x1 - x0).toFixed(1)}" height="${height}" style="fill:var(--warning-500, #f59e0b); opacity:0.18"></rect>`;
    }).join('');
  }

  return `
    <svg viewBox="0 0 ${width} ${height}" preserveAspectRatio="none" role="img"
      aria-label="${escapeHtml(t('settings.runtimeMetrics.group.trend'))}"
      style="display:block; width:100%; height:${height}px; margin:4px 0 8px">
      ${latchRects}
      <path d="${areaPath}" style="fill:var(--primary-500, #3b82f6); opacity:0.18"></path>
      <path d="${requestPath}" style="fill:none; stroke:var(--primary-500, #3b82f6); stroke-width:1.5" vector-effect="non-scaling-stroke"></path>
      <path d="${errorPath}" style="fill:none; stroke:var(--error-500, #ef4444); stroke-width:1.5" vector-effect="non-scaling-stroke"></path>
    </svg>
    <p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.trendSummary', {
      requests: formatRuntimeInteger(totalRequests),
      errors: formatRuntimeInteger(totalErrors)
    }))}</p>`;
}

// logs 表蓄水池的全局延迟分位数：ttfb（流式首字节）与 duration
// （总时长）两行，值均为毫秒。
function renderUsageLatencyExtra(stats) {
  const series = [
    { key: 'ttfb', labelKey: 'settings.runtimeMetrics.usageTtfb' },
    { key: 'duration', labelKey: 'settings.runtimeMetrics.usageDuration' }
  ];
  const rows = series.map(({ key, labelKey }) => {
    const latency = stats[key];
    if (!latency || typeof latency !== 'object' || Array.isArray(latency)) return '';
    const cell = (value) => `<td>${escapeHtml(formatRuntimeMilliseconds(value))}</td>`;
    return `<tr>
      <td>${escapeHtml(t(labelKey))}</td>
      <td>${escapeHtml(formatRuntimeInteger(latency.samples))}</td>
      ${cell(latency.p50)}${cell(latency.p90)}${cell(latency.p95)}${cell(latency.p99)}${cell(latency.max)}
    </tr>`;
  }).join('');
  if (!rows) return '';
  return renderRuntimeMetricTable([
    t('settings.runtimeMetrics.usageColMetric'),
    t('settings.runtimeMetrics.usageColSamples'),
    'p50', 'p90', 'p95', 'p99', 'max'
  ], rows);
}

// last_bind_failure 是最近一次监听端口争夺的取证记录（EADDRINUSE
// 重启风暴）；缺失即未发生过，不透出。
function renderDebuglogExtra(stats) {
  const failure = stats.last_bind_failure;
  if (!failure || typeof failure !== 'object' || Array.isArray(failure)) return '';
  return `<div class="custom-rules-error" role="alert">${escapeHtml(t('settings.runtimeMetrics.bindFailure', {
    count: failure.count ?? '—',
    addr: failure.addr || '—',
    holder: failure.holder || '—',
    time: formatRuntimeISOTime(failure.last_at)
  }))}</div>`;
}

function renderTranscriptUsage(stats) {
  const used = normalizeRuntimeMetric(stats.transcript_bytes);
  const budget = normalizeRuntimeMetric(stats.max_transcript_bytes);
  const percent = used !== null && budget !== null && budget > 0
    ? (used / budget) * 100
    : null;

  let state = 'unavailable';
  if (percent !== null) {
    if (percent > 100) state = 'exceeded';
    else if (percent >= 80) state = 'warning';
    else state = 'normal';
  }

  const stateLabel = t(`settings.runtimeMetrics.transcriptStatus.${state}`);
  const percentLabel = percent === null ? '—' : `${formatRuntimeDecimal(percent, 1)}%`;
  const progressValue = percent === null ? 0 : Math.min(100, Math.max(0, percent));
  const progressAria = percent === null
    ? ''
    : `aria-valuenow="${Math.round(progressValue)}"`;

  return `
    <section class="runtime-transcript-card">
      <div class="runtime-metrics-subsection-header">
        <h4>${escapeHtml(t('settings.runtimeMetrics.group.transcript'))}</h4>
        <span class="runtime-transcript-status runtime-transcript-status--${state}">${escapeHtml(stateLabel)}</span>
      </div>
      <div class="runtime-transcript-summary">
        <strong>${escapeHtml(formatRuntimeBytes(used))} / ${escapeHtml(formatRuntimeBytes(budget))}</strong>
        <span>${escapeHtml(percentLabel)}</span>
      </div>
      <div class="runtime-transcript-progress" role="progressbar" aria-label="${escapeHtml(t('settings.runtimeMetrics.group.transcript'))}" aria-valuemin="0" aria-valuemax="100" ${progressAria}>
        <span class="runtime-transcript-progress-bar runtime-transcript-progress-bar--${state}" style="width: ${progressValue}%"></span>
      </div>
      <div class="runtime-transcript-details">
        <span><b>${escapeHtml(t('settings.runtimeMetrics.metric.transcriptBytes'))}: ${escapeHtml(formatRuntimeBytes(used))}</b><code>transcript_bytes</code></span>
        <span><b>${escapeHtml(t('settings.runtimeMetrics.metric.maxTranscriptBytes'))}: ${escapeHtml(formatRuntimeBytes(budget))}</b><code>max_transcript_bytes</code></span>
      </div>
      <p class="runtime-metrics-note">${escapeHtml(t('settings.runtimeMetrics.transcriptNote'))}</p>
    </section>`;
}

function renderRuntimeMetrics(stats) {
  const content = document.getElementById('runtime-metrics-content');
  if (!content) return;

  const domains = runtimeMetricDomains.map((domain) => renderRuntimeMetricDomain(domain, stats)).join('');
  const responses = stats.responses_websocket;
  let responsesSection = '';
  if (responses && typeof responses === 'object' && !Array.isArray(responses)) {
    const groups = responsesRuntimeMetricGroups.map((group) => renderRuntimeMetricGroup(group, responses)).join('');
    responsesSection = `
    <section class="runtime-metrics-section runtime-responses-section">
      <div class="runtime-metrics-section-header">
        <h3>${escapeHtml(t('settings.runtimeMetrics.group.responses'))}</h3>
      </div>
      <p class="runtime-metrics-section-description">${escapeHtml(t('settings.runtimeMetrics.responsesNote'))}</p>
      ${renderTranscriptUsage(responses)}
      ${groups}
      <p class="runtime-metrics-note runtime-metrics-note--footer">${escapeHtml(t('settings.runtimeMetrics.cumulativeNote'))}</p>
    </section>`;
  }

  content.innerHTML = `
    ${domains}
    ${responsesSection}`;
}

function renderRuntimeMetricsLoading() {
  const content = document.getElementById('runtime-metrics-content');
  if (!content) return;
  content.innerHTML = `
    <div class="runtime-metrics-message">
      <span class="loading-spinner" aria-hidden="true"></span>
      <span>${escapeHtml(t('settings.runtimeMetrics.loading'))}</span>
    </div>`;
}

function renderRuntimeMetricsError(error) {
  const content = document.getElementById('runtime-metrics-content');
  if (!content) return;
  const message = error?.message || t('settings.runtimeMetrics.loadFailed');
  content.innerHTML = `
    <div class="runtime-metrics-message runtime-metrics-message--error" role="alert">
      <strong>${escapeHtml(t('settings.runtimeMetrics.loadFailed'))}</strong>
      <span>${escapeHtml(message)}</span>
    </div>`;
}

async function loadRuntimeMetrics(options) {
  if (runtimeMetricsLoading) return;
  // 手动刷新按钮的 click 事件会把 MouseEvent 传进来,此时 silent 恒为 false
  const silent = options?.silent === true;

  const content = document.getElementById('runtime-metrics-content');
  const refreshBtn = document.getElementById('refresh-runtime-metrics-btn');
  const updatedAt = document.getElementById('runtime-metrics-updated-at');
  runtimeMetricsLoading = true;
  if (content) content.setAttribute('aria-busy', 'true');
  if (!silent) {
    if (refreshBtn) refreshBtn.disabled = true;
    if (updatedAt) updatedAt.textContent = '';
    renderRuntimeMetricsLoading();
  }

  try {
    const data = await fetchDataWithAuth('/admin/runtime-metrics');
    if (!data || typeof data !== 'object' || Array.isArray(data)) {
      throw new Error(t('settings.runtimeMetrics.invalidResponse'));
    }

    renderRuntimeMetrics(data);
    if (updatedAt) {
      const time = new Date().toLocaleString(runtimeMetricsLocale());
      updatedAt.textContent = t('settings.runtimeMetrics.updatedAt', { time });
    }
  } catch (error) {
    console.error('Failed to load runtime metrics:', error);
    renderRuntimeMetricsError(error);
  } finally {
    runtimeMetricsLoading = false;
    if (content) content.setAttribute('aria-busy', 'false');
    if (refreshBtn) refreshBtn.disabled = false;
  }
}

// ===== 进程 stderr 日志查看器 =====
// offset 增量拉取；next_offset 回缩说明 stderr.log 已被新进程重写——
// 拿到的是新文件 tail 而非增量，往旧缓冲追加会重复整段尾巴，整换。

function openProcessLogModal() {
  const modal = document.getElementById('processLogModal');
  if (!modal) return;

  processLogPreviousFocus = document.activeElement;
  processLogOffset = 0;
  processLogBuffer = '';
  processLogPaused = false;
  updateProcessLogPauseButton();
  const follow = document.getElementById('process-log-follow');
  processLogFollow = follow ? follow.checked : true;
  document.querySelector('.app-container')?.setAttribute('inert', '');
  modal.classList.add('show');
  modal.setAttribute('aria-hidden', 'false');
  modal.querySelector('.close-btn')?.focus();
  loadProcessLogDebugState();
  loadProcessLog(0);
  startProcessLogPolling();
}

function closeProcessLogModal() {
  const modal = document.getElementById('processLogModal');
  if (!modal) return;

  stopProcessLogPolling();
  modal.classList.remove('show');
  modal.setAttribute('aria-hidden', 'true');
  document.querySelector('.app-container')?.removeAttribute('inert');
  if (processLogPreviousFocus?.isConnected) processLogPreviousFocus.focus();
  processLogPreviousFocus = null;
}

function startProcessLogPolling() {
  if (processLogPollTimer !== null || processLogPaused || !processLogFollow) return;
  processLogPollTimer = setInterval(() => {
    if (document.hidden) return;
    loadProcessLog(processLogOffset, { silent: true });
  }, PROCESS_LOG_REFRESH_MS);
}

function stopProcessLogPolling() {
  if (processLogPollTimer !== null) {
    clearInterval(processLogPollTimer);
    processLogPollTimer = null;
  }
}

function updateProcessLogPauseButton() {
  const btn = document.getElementById('process-log-pause-btn');
  if (btn) btn.textContent = t(processLogPaused ? 'settings.processLog.resume' : 'settings.processLog.pause');
}

function setProcessLogPaused(paused) {
  processLogPaused = paused;
  updateProcessLogPauseButton();
  if (paused) {
    stopProcessLogPolling();
    return;
  }
  loadProcessLog(processLogOffset, { silent: true });
  startProcessLogPolling();
}

async function loadProcessLog(offset, options) {
  if (processLogLoading) return;
  const silent = options?.silent === true;
  processLogLoading = true;
  const refreshBtn = document.getElementById('process-log-refresh-btn');
  const updatedAt = document.getElementById('process-log-updated-at');
  if (!silent && refreshBtn) refreshBtn.disabled = true;

  try {
    const data = await fetchDataWithAuth('/admin/process-log?offset=' + (offset || 0));
    const next = Number(data?.next_offset) || 0;
    const isDelta = offset > 0 && next >= processLogOffset;
    processLogBuffer = isDelta ? processLogBuffer + (data?.text || '') : String(data?.text || '');
    if (processLogBuffer.length > PROCESS_LOG_BUFFER_CAP) {
      processLogBuffer = processLogBuffer.slice(-PROCESS_LOG_BUFFER_CAP);
    }
    processLogOffset = next;
    renderProcessLog();
    if (updatedAt) {
      updatedAt.textContent = t('settings.runtimeMetrics.updatedAt', { time: new Date().toLocaleString(runtimeMetricsLocale()) });
    }
  } catch (err) {
    console.error('Failed to load process log:', err);
    // 缓冲已有内容时保留视图只在底栏记错误；空缓冲才把错误写进视图。
    const view = document.getElementById('process-log-view');
    if (view && !processLogBuffer) {
      view.textContent = t('settings.processLog.unavailable') + ': ' + err.message;
    }
    if (updatedAt) {
      updatedAt.textContent = t('settings.processLog.fetchFailed', { message: err.message });
    }
  } finally {
    processLogLoading = false;
    if (refreshBtn) refreshBtn.disabled = false;
  }
}

function renderProcessLog() {
  const view = document.getElementById('process-log-view');
  const content = document.getElementById('process-log-content');
  if (!view) return;

  const level = document.getElementById('process-log-level')?.value || '';
  let text = processLogBuffer;
  if (level) {
    const min = PROCESS_LOG_LEVELS[level] ?? 0;
    text = processLogBuffer.split('\n').filter((line) => {
      const match = /level=(\w+)/.exec(line);
      return !match || PROCESS_LOG_LEVELS[match[1]] === undefined || PROCESS_LOG_LEVELS[match[1]] >= min;
    }).join('\n');
  }
  view.textContent = text || t('settings.processLog.empty');
  if (processLogFollow && content) content.scrollTop = content.scrollHeight;
}

function bindProcessLogModal() {
  const modal = document.getElementById('processLogModal');
  if (!modal || modal.dataset.bound) return;

  modal.addEventListener('click', (event) => {
    if (event.target === modal) {
      closeProcessLogModal();
      return;
    }
    const button = event.target.closest('[data-action]');
    if (button?.dataset.action === 'close-process-log') closeProcessLogModal();
  });
  modal.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      closeProcessLogModal();
      return;
    }
    if (event.key === 'Tab') trapModalFocus(modal, event);
  });

  const pauseBtn = document.getElementById('process-log-pause-btn');
  if (pauseBtn) pauseBtn.addEventListener('click', () => setProcessLogPaused(!processLogPaused));

  const refreshBtn = document.getElementById('process-log-refresh-btn');
  if (refreshBtn) refreshBtn.addEventListener('click', () => loadProcessLog(processLogOffset));

  const clearBtn = document.getElementById('process-log-clear-btn');
  if (clearBtn) {
    clearBtn.addEventListener('click', () => {
      // 只清视图：offset 不动，下一轮增量只追加新行。
      processLogBuffer = '';
      renderProcessLog();
    });
  }

  // procFollow 语义：跟随开关是拉取总开关——勾选=5s 轮询+滚底，取消=停拉。
  const follow = document.getElementById('process-log-follow');
  if (follow) {
    follow.addEventListener('change', () => {
      processLogFollow = follow.checked;
      if (!processLogFollow) {
        stopProcessLogPolling();
        return;
      }
      renderProcessLog();
      loadProcessLog(processLogOffset, { silent: true });
      startProcessLogPolling();
    });
  }

  const debugToggle = document.getElementById('process-log-debug-toggle');
  if (debugToggle) debugToggle.addEventListener('change', () => setProcessLogDebugEnabled(debugToggle));

  const level = document.getElementById('process-log-level');
  if (level) level.addEventListener('change', renderProcessLog);

  modal.dataset.bound = '1';
}

// 请求日志开关复用 settings 键仓：GET/PUT /admin/settings/debug_log_enabled，
// 与设置表里的同一行共享状态，写成功后同步原值避免该行被误判为脏。
async function loadProcessLogDebugState() {
  const toggle = document.getElementById('process-log-debug-toggle');
  if (!toggle) return;
  try {
    const row = await fetchDataWithAuth('/admin/settings/debug_log_enabled');
    toggle.checked = row?.value === 'true';
  } catch (err) {
    console.error('Failed to load debug_log_enabled:', err);
  }
}

async function setProcessLogDebugEnabled(toggle) {
  const next = toggle.checked;
  toggle.disabled = true;
  try {
    await fetchDataWithAuth('/admin/settings/debug_log_enabled', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ value: String(next) })
    });
    syncSettingState('debug_log_enabled', String(next));
    // 开启后立刻补拉一轮：debug 关闭时 process-log 端点 404，开关是恢复入口。
    if (next) loadProcessLog(processLogOffset, { silent: true });
  } catch (err) {
    toggle.checked = !next;
    showError(t('settings.processLog.debugToggleFailed', { message: err.message }));
  } finally {
    toggle.disabled = false;
  }
}

// ===== 生效配置 =====
// 文件为事实源：展示最后一次加载的脱敏视图；改 config.yaml 后点
// 「重新加载」热应用，requires_restart 列出的字段需托管重启。

async function loadEffectiveConfig() {
  const banner = document.getElementById('effective-config-banner');
  try {
    effectiveConfigData = await fetchDataWithAuth('/admin/config');
    renderEffectiveConfig();
  } catch (err) {
    console.error('Failed to load effective config:', err);
    effectiveConfigData = null;
    if (banner) {
      banner.hidden = false;
      banner.textContent = t('settings.effectiveConfig.loadFailed') + ': ' + err.message;
    }
  }
}

function renderEffectiveConfig() {
  const data = effectiveConfigData || {};
  const staleBadge = document.getElementById('effective-config-stale');
  const banner = document.getElementById('effective-config-banner');
  const meta = document.getElementById('effective-config-meta');
  const isStale = data.stale === true;

  if (staleBadge) staleBadge.hidden = !isStale;
  if (banner) {
    banner.hidden = !isStale;
    banner.textContent = isStale ? t('settings.effectiveConfig.staleBanner') : '';
  }

  if (meta) {
    const rows = [
      ['settings.effectiveConfig.path', data.path],
      ['settings.effectiveConfig.loadedAt', formatRuntimeISOTime(data.loaded_at)],
      ['settings.effectiveConfig.fileMtime', formatRuntimeISOTime(data.file_mtime)],
      ['settings.effectiveConfig.state', isStale ? t('settings.effectiveConfig.stateStale') : t('settings.effectiveConfig.stateFresh')]
    ];
    const reload = data.last_reload;
    if (reload && typeof reload === 'object' && !Array.isArray(reload)) {
      rows.push(['settings.effectiveConfig.lastReload', formatRuntimeISOTime(reload.at)]);
      rows.push(['settings.effectiveConfig.applied', (reload.applied || []).join(', ') || t('settings.effectiveConfig.none')]);
      rows.push(['settings.effectiveConfig.requiresRestart', (reload.requires_restart || []).join(', ') || t('settings.effectiveConfig.none')]);
    }
    meta.innerHTML = rows.map(([labelKey, value]) => `
      <div class="quota-kv">
        <span class="quota-kv-k">${escapeHtml(t(labelKey))}</span>
        <span class="quota-kv-v">${escapeHtml(value === null || value === undefined || value === '' ? '—' : String(value))}</span>
      </div>`).join('');
  }

  const view = data.config && typeof data.config === 'object'
    ? JSON.stringify(data.config, null, 2)
    : String(data.error || t('settings.effectiveConfig.notLoaded'));
  setHighlightedCodeContent('effective-config-json', view, 'json');
}

async function reloadEffectiveConfig() {
  const btn = document.getElementById('effective-config-reload-btn');
  if (btn?.disabled) return;
  if (btn) {
    btn.disabled = true;
    btn.setAttribute('aria-busy', 'true');
  }

  try {
    const report = await fetchDataWithAuth('/admin/config/reload', { method: 'POST' });
    const applied = Array.isArray(report?.applied) ? report.applied : [];
    const cold = Array.isArray(report?.requires_restart) ? report.requires_restart : [];
    const appliedText = applied.join(', ') || t('settings.effectiveConfig.reloadedNoChanges');
    if (cold.length) {
      window.showWarning(t('settings.effectiveConfig.reloadedWithRestart', { applied: appliedText, keys: cold.join(', ') }));
    } else {
      showSuccess(t('settings.effectiveConfig.reloaded', { keys: appliedText }));
    }
    // 热字段（debug.enabled 等）可能刚变，连带刷新配置视图。
    await loadEffectiveConfig();
  } catch (err) {
    console.error('配置重载异常:', err);
    showError(t('settings.effectiveConfig.reloadFailed') + ': ' + err.message);
  } finally {
    if (btn) {
      btn.disabled = false;
      btn.removeAttribute('aria-busy');
    }
  }
}

function getSettingGroupInfo(key) {
  const k = String(key || '').toLowerCase();

  const defs = [
    { id: 'advanced', nameKey: 'settings.group.advanced', order: 70, match: () => advancedSettingKeys.has(k) },

    // devin_* 匹配讲究先后：max_rpm 属闸门、client_* 属身份，须先排掉，
    // 余下的 devin_* 才归上游端点组。
    { id: 'gate', nameKey: 'settings.group.gate', order: 22, fb: '速率闸门', match: () => k === 'devin_max_rpm' || k.startsWith('gate_') },
    { id: 'warm', nameKey: 'settings.group.warm', order: 23, fb: '前缀保温', match: () => k.startsWith('warm_prefix_') },
    { id: 'identity', nameKey: 'settings.group.identity', order: 15, fb: '客户端身份', match: () => k.startsWith('devin_client_') },
    { id: 'upstream', nameKey: 'settings.group.upstream', order: 10, fb: '上游端点', match: () => k.startsWith('devin_') },

    { id: 'websocket', nameKey: 'settings.group.websocket', order: 25, match: () => k.startsWith('responses_ws_') },
    { id: 'stream-timeout', nameKey: 'settings.group.streamTimeout', order: 20, match: () => k === 'stream_timeout' || k.endsWith('_first_byte_timeout') },
    { id: 'non-stream-timeout', nameKey: 'settings.group.nonStreamTimeout', order: 21, match: () => k === 'non_stream_timeout' || k.endsWith('_non_stream_timeout') },
    { id: 'limits', nameKey: 'settings.group.limits', order: 26, match: () => k === 'max_concurrency' || k.endsWith('_body_bytes') || k === 'http_read_timeout_seconds' },
    { id: 'billing', nameKey: 'settings.group.billing', order: 35, match: () => k === modelCustomPricingSettingKey },
    { id: 'log', nameKey: 'settings.group.log', order: 50, match: () => k.startsWith('log_') || k.startsWith('debug_') },
    { id: 'access', nameKey: 'settings.group.access', order: 60, match: () => k.includes('auth_') },
  ];

  for (const d of defs) {
    if (d.match()) return { ...d, name: i18nText(d.nameKey, d.fb || d.nameKey) };
  }
  return { id: 'advanced', nameKey: 'settings.group.advanced', name: i18nText('settings.group.advanced', '高级'), order: 70 };
}

function getSettingOrder(key) {
  const orders = {
    devin_base_url: 10,
    devin_proxy: 11,
    devin_force_http1: 12,
    devin_model: 13,
    devin_aliases: 14,
    devin_client_name: 30,
    devin_client_version: 31,
    devin_client_os: 32,
    devin_max_rpm: 40,
    gate_max_hold_seconds: 41,
    gate_drip_interval_seconds: 42,
    gate_default_latch_seconds: 43,
    gate_window_offset_seconds: 44,
    gate_window_guard_seconds: 45,
    warm_prefix_enabled: 50,
    warm_prefix_interval_seconds: 51,
    warm_prefix_jitter_ratio: 52,
    warm_prefix_max_streams: 53,
    warm_prefix_max_retained_mb: 54,
    warm_prefix_min_prefix_tokens: 55,
    warm_prefix_blocked_max_idle_seconds: 56,
    warm_prefix_userpaced_max_idle_seconds: 57,
    warm_prefix_subdone_max_idle_seconds: 58,
    warm_prefix_unknown_max_idle_seconds: 59,
    warm_prefix_blocked_names: 60,
    warm_prefix_userpaced_names: 61,
    upstream_first_byte_timeout: 100,
    stream_timeout: 101,
    non_stream_timeout: 102,
    anthropic_first_byte_timeout: 110,
    anthropic_non_stream_timeout: 111,
    codex_first_byte_timeout: 120,
    codex_non_stream_timeout: 121,
    openai_first_byte_timeout: 130,
    openai_non_stream_timeout: 131,
    gemini_first_byte_timeout: 140,
    gemini_non_stream_timeout: 141,
    max_concurrency: 200,
    max_body_bytes: 201,
    max_image_body_bytes: 202,
    http_read_timeout_seconds: 203,
    debug_log_enabled: 300,
    log_retention_days: 301,
    log_max_total_mb: 302,
    log_payload_hours: 303,
    log_keep_error_dirs: 304,
    debug_quota_interval_minutes: 305,
    debug_pprof_listen: 306,
    model_custom_pricing: 710
  };
  const normalizedKey = String(key || '').toLowerCase();
  return orders[normalizedKey] ?? 1000;
}

function isModalSettingKey(key) {
  return key === modelMultimodalFallbackSettingKey || key === modelCustomPricingSettingKey;
}

function groupSettings(settings) {
  const groupsById = new Map();

  for (const s of settings) {
    const g = getSettingGroupInfo(s.key);
    if (!groupsById.has(g.id)) {
      groupsById.set(g.id, { id: g.id, name: g.name, order: g.order, settings: [] });
    }
    groupsById.get(g.id).settings.push(s);
  }

  const groups = Array.from(groupsById.values())
    .sort((a, b) => a.order - b.order || a.name.localeCompare(b.name));

  for (const g of groups) {
    g.settings.sort((a, b) => {
      const orderDiff = getSettingOrder(a.key) - getSettingOrder(b.key);
      if (orderDiff !== 0) return orderDiff;
      return String(a.key).localeCompare(String(b.key));
    });
  }

  return groups;
}

function renderGroupNav(groups) {
  const nav = document.getElementById('settings-group-nav');
  const navSection = document.getElementById('settings-group-nav-section');
  if (!nav) return;

  nav.innerHTML = '';
  const hasMultipleGroups = Array.isArray(groups) && groups.length > 1;
  if (navSection) navSection.hidden = !hasMultipleGroups;
  if (!hasMultipleGroups) return;

  for (let i = 0; i < groups.length; i++) {
    const g = groups[i];
    const btn = document.createElement('button');
    btn.className = 'time-range-btn' + (i === 0 ? ' active' : '');
    btn.dataset.group = g.id;
    btn.textContent = i18nText(`settings.nav.${g.id}`, g.name);
    btn.title = g.name;
    btn.addEventListener('click', () => {
      // 移除所有按钮的 active 状态
      nav.querySelectorAll('.time-range-btn').forEach(b => b.classList.remove('active'));
      btn.classList.add('active');
      // 滚动到对应分组
      const target = document.getElementById(`settings-group-${g.id}`);
      if (target) target.scrollIntoView({ behavior: 'smooth', block: 'start' });
    });
    nav.appendChild(btn);
  }
}

function refreshSettingsTranslations() {
  const groups = groupSettings(Array.from(settingDefinitions.values()))
    .map((group) => ({ ...group, settings: group.settings.filter((setting) => !isModalSettingKey(setting.key)) }))
    .filter((group) => group.settings.length > 0);
  for (const group of groups) {
    const button = document.querySelector(`#settings-group-nav [data-group="${group.id}"]`);
    if (button) {
      button.textContent = i18nText(`settings.nav.${group.id}`, group.name);
      button.title = group.name;
    }
    const title = document.querySelector(`#settings-group-${group.id} .setting-group-title`);
    if (title) title.textContent = group.name;
  }
  for (const setting of settingDefinitions.values()) {
    const row = document.querySelector(`.setting-data-row[data-key="${setting.key}"]`);
    if (!row) continue;
    const description = row.querySelector('.setting-col-description');
    description.textContent = i18nText(`settings.desc.${setting.key}`, setting.description);
    description.dataset.mobileLabel = t('settings.configItem');
    row.querySelector('.setting-col-value').dataset.mobileLabel = t('settings.currentValue');
    row.querySelector('.setting-col-actions').dataset.mobileLabel = t('common.actions');
  }
  updateMultimodalFallbackSummary(document.getElementById(modelMultimodalFallbackSettingKey)?.value || '');
  updateCustomPricingSummary(document.getElementById(modelCustomPricingSettingKey)?.value || '');
  if (effectiveConfigData) renderEffectiveConfig();
  updateProcessLogPauseButton();
}

async function loadSettings() {
  try {
    const data = await fetchDataWithAuth('/admin/settings');
    if (!Array.isArray(data)) throw new Error(t('settings.msg.invalidResponse'));
    renderSettings(data);
  } catch (err) {
    console.error('Failed to load settings:', err);
    showError(t('settings.msg.loadFailed') + ': ' + err.message);
  }
}

function renderSettings(settings) {
  const tbody = document.getElementById('settings-tbody');
  originalSettings = {};
  settingDefinitions = new Map(settings.map((setting) => [setting.key, setting]));
  tbody.innerHTML = '';

  // 初始化事件委托（仅一次）
  initSettingsEventDelegation();

  for (const s of settings) {
    const displayValue = settingValueForDisplay(s.key, s.value);
    originalSettings[s.key] = displayValue;
    if (!isModalSettingKey(s.key)) continue;
    const target = document.getElementById(s.key);
    if (target) target.value = displayValue;
    const buttonID = s.key === modelMultimodalFallbackSettingKey
      ? 'model-multimodal-fallback-btn'
      : 'model-custom-pricing-btn';
    const button = document.getElementById(buttonID);
    if (button) button.disabled = s.editable === false;
    if (s.key === modelMultimodalFallbackSettingKey) updateMultimodalFallbackSummary(displayValue);
    else updateCustomPricingSummary(displayValue);
  }

  const groups = groupSettings(settings)
    .map((group) => ({ ...group, settings: group.settings.filter((setting) => !isModalSettingKey(setting.key)) }))
    .filter((group) => group.settings.length > 0);
  renderGroupNav(groups);

  for (const g of groups) {
    const groupRow = TemplateEngine.render('tpl-setting-group-row', {
      groupId: g.id,
      groupName: g.name
    });
    if (groupRow) tbody.appendChild(groupRow);

    for (const s of g.settings) {
      const displayValue = settingValueForDisplay(s.key, s.value);
      // 优先使用语言包中的描述，若没有则回退到后端返回的描述
      const description = i18nText(`settings.desc.${s.key}`, s.description);
      const row = TemplateEngine.render('tpl-setting-row', {
        key: s.key,
        description: description,
        inputHtml: renderInput({ ...s, value: displayValue }),
        resetDisabledAttributes: settingDisabledAttributes(s),
        mobileLabelDescription: t('settings.configItem'),
        mobileLabelValue: t('settings.currentValue'),
        mobileLabelActions: t('common.actions')
      });
      if (row) tbody.appendChild(row);
    }
  }
}

function settingDisabledAttributes(setting) {
  return setting.editable === false ? 'disabled' : '';
}

// 初始化事件委托（替代 inline onclick）
function initSettingsEventDelegation() {
  const tbody = document.getElementById('settings-tbody');
  if (!tbody || tbody.dataset.delegated) return;
  tbody.dataset.delegated = 'true';

  // 重置按钮点击
  tbody.addEventListener('click', (e) => {
    const resetBtn = e.target.closest('.setting-reset-btn');
    if (resetBtn) {
      resetSetting(resetBtn.dataset.key);
    }
  });

  // 输入变更
  tbody.addEventListener('change', (e) => {
    const input = e.target.closest('input, select, textarea');
    if (input) markChanged(input);
  });
}

function renderInput(setting) {
  const safeKey = escapeHtml(setting.key);
  const safeValue = escapeHtml(setting.value);
  const disabledAttributes = settingDisabledAttributes(setting);
  const numericAttributes = numericInputAttributes(setting);

  if (byteSettingKeys.has(setting.key)) {
    return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${numericAttributes} ${disabledAttributes}>`;
  }

  switch (setting.value_type) {
    case 'bool':
      const isTrue = setting.value === 'true' || setting.value === '1';
      return `
        <div class="settings-bool-group">
          <label class="settings-bool-option">
            <input type="radio" name="${safeKey}" value="true" ${isTrue ? 'checked' : ''} ${disabledAttributes}> <span data-i18n="common.enable">${t('common.enable')}</span>
          </label>
          <label class="settings-bool-option">
            <input type="radio" name="${safeKey}" value="false" ${!isTrue ? 'checked' : ''} ${disabledAttributes}> <span data-i18n="common.disable">${t('common.disable')}</span>
          </label>
        </div>`;
    case 'int':
    case 'duration':
      return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${numericAttributes} ${disabledAttributes}>`;
    case 'float':
      return `<input type="number" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--number" ${numericAttributes} ${disabledAttributes}>`;
    case 'json':
      return `<textarea id="${safeKey}" class="settings-input settings-input--json" rows="3" spellcheck="false" ${disabledAttributes}>${safeValue}</textarea>`;
    default:
      return `<input type="text" id="${safeKey}" value="${safeValue}" class="settings-input settings-input--text" ${disabledAttributes}>`;
  }
}

function markChanged(input) {
  input.removeAttribute?.('aria-invalid');
  const row = input.closest('tr');
  if (!row) return; // 常驻控制区（如多模态回退 hidden input）没有表格行可高亮
  let key, currentValue;

  if (input.type === 'radio') {
    key = input.name;
    const checkedRadio = row.querySelector(`input[name="${key}"]:checked`);
    currentValue = checkedRadio ? checkedRadio.value : '';
  } else {
    key = input.id;
    currentValue = input.value;
  }

  if (currentValue !== originalSettings[key]) {
    row.style.background = 'rgba(59, 130, 246, 0.08)';
  } else {
    row.style.background = '';
  }
}

function setSettingInvalid(key, invalid) {
  const control = getSettingControl(key);
  if (!control) return;
  const inputs = control.radios ? Array.from(control.radios) : [control.input];
  for (const input of inputs) {
    if (invalid) input.setAttribute?.('aria-invalid', 'true');
    else input.removeAttribute?.('aria-invalid');
  }
  if (invalid) control.input?.focus?.();
}

function getSettingControl(key) {
  const input = document.getElementById(key);
  if (input) {
    return {
      input,
      row: input.closest('tr'),
      value: input.value
    };
  }

  const radios = document.querySelectorAll(`input[name="${key}"]`);
  if (radios.length === 0) return null;

  const checkedRadio = document.querySelector(`input[name="${key}"]:checked`);
  return {
    input: radios[0],
    radios,
    row: radios[0].closest('tr'),
    value: checkedRadio ? checkedRadio.value : ''
  };
}

function setSettingControlValue(key, value) {
  const normalizedValue = settingValueForDisplay(key, value);
  const control = getSettingControl(key);

  if (control?.radios) {
    for (const radio of control.radios) {
      radio.checked = radio.value === normalizedValue
        || (normalizedValue === '1' && radio.value === 'true')
        || (normalizedValue === '0' && radio.value === 'false');
    }
  } else if (control?.input) {
    control.input.value = normalizedValue;
  }

  if (key === modelMultimodalFallbackSettingKey) updateMultimodalFallbackSummary(normalizedValue);
  if (key === modelCustomPricingSettingKey) updateCustomPricingSummary(normalizedValue);
  return control;
}

function syncSettingState(key, value) {
  const normalizedValue = settingValueForDisplay(key, value);
  const control = setSettingControlValue(key, value);

  originalSettings[key] = normalizedValue;
  if (control?.row) {
    control.row.style.background = '';
  }
}

async function saveAllSettings() {
  // 收集所有变更
  const updates = {};

  for (const key of Object.keys(originalSettings)) {
    const control = getSettingControl(key);
    if (!control) continue;

    const currentValue = control.value;
    if (currentValue !== originalSettings[key]) {
      const setting = settingDefinitions.get(key);
      const validationError = setting ? validateSettingInput(setting, currentValue) : '';
      if (validationError) {
        setSettingInvalid(key, true);
        showError(t('settings.msg.invalidValue', { key, reason: validationError }));
        return;
      }
      setSettingInvalid(key, false);
      updates[key] = settingValueForStorage(key, currentValue);
    }
  }

  if (Object.keys(updates).length === 0) {
    window.showNotification(t('settings.msg.noChanges'), 'info');
    return;
  }

  if (!confirm(t('settings.msg.confirmSave'))) return;

  // 使用批量更新接口（单次请求，事务保护）
  try {
    const result = await fetchDataWithAuth('/admin/settings/batch', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(updates)
    });

    for (const [key, value] of Object.entries(updates)) {
      syncSettingState(key, value);
    }

    showSuccess(result?.message || t('settings.msg.savedCount', { count: Object.keys(updates).length }));
  } catch (err) {
    console.error('保存异常:', err);
    showError(t('settings.msg.saveFailed') + ': ' + err.message);
  }
}

function resetSetting(key) {
  const setting = settingDefinitions.get(key);
  if (!setting) {
    showError(t('settings.msg.resetUnavailable', { key }));
    return;
  }

  const control = setSettingControlValue(key, setting.default_value ?? '');
  if (control?.input) markChanged(control.input);
}

window.i18n?.onLocaleChange?.(refreshSettingsTranslations);

window.initPageBootstrap({
  topbarKey: 'settings',
  run: () => {
    bindSettingsPageActions();
    loadSettings();
    loadEffectiveConfig();
  }
});
