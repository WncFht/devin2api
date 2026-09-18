    // 统计数据管理
    let statsData = { by_api: {} };

    // 四个入口端点卡片；key 与 logs 表的 api 字段同口径，
    // 卡片 DOM 由 tpl-channel-card 渲染，type-{key}-* id 供 updateOverviewCard 填充。
    const ICON_OPENAI = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M22.2819 9.8211a5.9847 5.9847 0 0 0-.5157-4.9108 6.0462 6.0462 0 0 0-6.5098-2.9A6.0651 6.0651 0 0 0 4.9807 4.1818a5.9847 5.9847 0 0 0-3.9977 2.9 6.0462 6.0462 0 0 0 .7427 7.0966 5.98 5.98 0 0 0 .511 4.9107 6.051 6.051 0 0 0 6.5146 2.9001A5.9847 5.9847 0 0 0 13.2599 24a6.0557 6.0557 0 0 0 5.7718-4.2058 5.9894 5.9894 0 0 0 3.9977-2.9001 6.0557 6.0557 0 0 0-.7475-7.0729zm-9.022 12.6081a4.4755 4.4755 0 0 1-2.8764-1.0408l.1419-.0804 4.7783-2.7582a.7948.7948 0 0 0 .3927-.6813v-6.7369l2.02 1.1686a.071.071 0 0 1 .038.052v5.5826a4.504 4.504 0 0 1-4.4945 4.4944zm-9.6607-4.1254a4.4708 4.4708 0 0 1-.5346-3.0137l.142.0852 4.783 2.7582a.7712.7712 0 0 0 .7806 0l5.8428-3.3685v2.3324a.0804.0804 0 0 1-.0332.0615L9.74 19.9502a4.4992 4.4992 0 0 1-6.1408-1.6464zM2.3408 7.8956a4.485 4.485 0 0 1 2.3655-1.9728V11.6a.7664.7664 0 0 0 .3879.6765l5.8144 3.3543-2.0201 1.1685a.0757.0757 0 0 1-.071 0l-4.8303-2.7865A4.504 4.504 0 0 1 2.3408 7.872zm16.5963 3.8558L13.1038 8.364 15.1192 7.2a.0757.0757 0 0 1 .071 0l4.8303 2.7913a4.4944 4.4944 0 0 1-.6765 8.1042v-5.6772a.79.79 0 0 0-.407-.667zm2.0107-3.0231l-.142-.0852-4.7735-2.7818a.7759.7759 0 0 0-.7854 0L9.409 9.2297V6.8974a.0662.0662 0 0 1 .0284-.0615l4.8303-2.7866a4.4992 4.4992 0 0 1 6.6802 4.66zM8.3065 12.863l-2.02-1.1638a.0804.0804 0 0 1-.038-.0567V6.0742a4.4992 4.4992 0 0 1 7.3757-3.4537l-.142.0805L8.704 5.459a.7948.7948 0 0 0-.3927.6813zm1.0976-2.3654l2.602-1.4998 2.6069 1.4998v2.9994l-2.5974 1.4997-2.6067-1.4997Z"/></svg>';
    const CHANNEL_CARDS = Object.freeze([
      { key: 'anthropic', iconClass: 'anthropic', label: 'Anthropic · /v1/messages',
        icon: '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M12 2L2 22h20L12 2zm0 4.5L18.5 20h-13L12 6.5z"/></svg>' },
      { key: 'openai-chat', iconClass: 'openai', label: 'OpenAI Chat · /v1/chat/completions', icon: ICON_OPENAI },
      { key: 'openai-responses', iconClass: 'codex', label: 'Responses · /v1/responses',
        icon: '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M16 18l6-6-6-6"/><path d="M8 6l-6 6 6 6"/></svg>' },
      { key: 'responses-ws', iconClass: 'api', label: 'Responses WS · /v1/responses',
        icon: '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor"><path d="M13 2 3 14h7l-1 8 10-12h-7l1-8z"/></svg>' },
    ]);
    const ENDPOINT_CARD_KEYS = Object.freeze(CHANNEL_CARDS.map(c => c.key));

    // 模块顶层渲染（defer 后 DOM 已就绪），bootstrap 的 translatePage 会接管 data-i18n
    (function renderChannelCards() {
      const grid = document.getElementById('channel-grid');
      if (!grid || typeof TemplateEngine === 'undefined') return;
      CHANNEL_CARDS.forEach(c => {
        const el = TemplateEngine.render('tpl-channel-card', { type: c.key, iconClass: c.iconClass, label: c.label, icon: c.icon });
        if (el) grid.appendChild(el);
      });
    })();

    // 当前选中的时间范围
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;
    let serviceHealthModel = null;
    let dashboardLoadGeneration = 0;

    function buildCurrentDateRangeQuery() {
      return typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
        : `range=${encodeURIComponent(currentTimeRange)}`;
    }

    function currentRangeHours() {
      if (currentTimeRange === 'custom' && currentCustomTimeRange) {
        const startMs = Number(currentCustomTimeRange.startMs);
        const endMs = Number(currentCustomTimeRange.endMs);
        if (Number.isFinite(startMs) && Number.isFinite(endMs) && endMs > startMs) {
          return Math.max((endMs - startMs) / 3600000, 1 / 60);
        }
      }
      return typeof window.getRangeHours === 'function'
        ? window.getRangeHours(currentTimeRange)
        : 24;
    }

    function serviceHealthText(key, fallback, params) {
      if (typeof window.i18nText === 'function') return window.i18nText(key, fallback, params);
      const translated = typeof window.t === 'function' ? window.t(key, params) : key;
      return translated === key ? fallback : translated;
    }

    function serviceHealthLocale() {
      return window.i18n && typeof window.i18n.getLocale === 'function' && window.i18n.getLocale() === 'en'
        ? 'en-US'
        : 'zh-CN';
    }

    function serviceHealthTimeFormatter() {
      return new Intl.DateTimeFormat(serviceHealthLocale(), {
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        hourCycle: 'h23'
      });
    }

    function serviceHealthPeriodText() {
      return typeof window.getRangeLabel === 'function'
        ? window.getRangeLabel(currentTimeRange)
        : currentTimeRange;
    }

    function hideServiceHealthTooltip() {
      const tooltip = document.getElementById('service-health-tooltip');
      if (tooltip) tooltip.hidden = true;
    }

    function showServiceHealthTooltip(cell, point, formatter, bucketMs) {
      const plot = cell.closest('.service-health-plot');
      const card = plot && plot.closest('.service-health-card');
      const tooltip = document.getElementById('service-health-tooltip');
      const timeElement = document.getElementById('service-health-tooltip-time');
      const successElement = document.getElementById('service-health-tooltip-success');
      const errorElement = document.getElementById('service-health-tooltip-error');
      const rateElement = document.getElementById('service-health-tooltip-rate');
      if (!plot || !card || !tooltip || !timeElement || !successElement || !errorElement || !rateElement) return;

      const intervalMs = bucketMs || 15 * 60 * 1000;
      timeElement.textContent = `${formatter.format(new Date(point.ts))} – ${formatter.format(new Date(point.ts + intervalMs))}`;
      successElement.textContent = formatNumber(point.success);
      errorElement.textContent = formatNumber(point.error);
      rateElement.textContent = point.rate === null ? '--' : `(${(point.rate * 100).toFixed(1)}%)`;

      tooltip.hidden = false;
      tooltip.dataset.placement = 'top';

      const plotRect = plot.getBoundingClientRect();
      const cellRect = cell.getBoundingClientRect();
      const tooltipRect = tooltip.getBoundingClientRect();
      const cellCenter = cellRect.left - plotRect.left + cellRect.width / 2;
      const inset = 8;
      const maxLeft = Math.max(inset, plotRect.width - tooltipRect.width - inset);
      const left = Math.min(Math.max(cellCenter - tooltipRect.width / 2, inset), maxLeft);
      const roomAbove = cellRect.top - card.getBoundingClientRect().top;
      let top = cellRect.top - plotRect.top - tooltipRect.height - 12;

      if (roomAbove < tooltipRect.height + 16) {
        top = cellRect.bottom - plotRect.top + 12;
        tooltip.dataset.placement = 'bottom';
      }

      tooltip.style.left = `${left}px`;
      tooltip.style.top = `${top}px`;
      const arrowX = Math.min(Math.max(cellCenter - left, 12), tooltipRect.width - 12);
      tooltip.style.setProperty('--service-health-tooltip-arrow-x', `${arrowX}px`);
    }

    function renderServiceHealth(model) {
      const grid = document.getElementById('service-health-grid');
      const rateElement = document.getElementById('service-health-rate');
      const message = document.getElementById('service-health-message');
      if (!grid || !rateElement || !message || !model) return;

      hideServiceHealthTooltip();
      const timeFormatter = serviceHealthTimeFormatter();
      const fragment = document.createDocumentFragment();
      for (const [index, point] of model.points.entries()) {
        const cell = document.createElement('span');
        cell.className = `service-health-cell ${point.state}`;
        cell.setAttribute('aria-hidden', 'true');
        cell.dataset.index = String(index);
        fragment.appendChild(cell);
      }
      grid.replaceChildren(fragment);
      grid.onmouseover = event => {
        const cell = event.target.closest('.service-health-cell');
        if (!cell || !grid.contains(cell)) return;
        showServiceHealthTooltip(cell, model.points[Number(cell.dataset.index)], timeFormatter, model.bucketMs);
      };
      grid.onmouseleave = hideServiceHealthTooltip;

      const hasData = model.rate !== null;
      const rate = hasData ? `${(model.rate * 100).toFixed(1)}%` : '--';
      const period = serviceHealthPeriodText();
      rateElement.textContent = rate;
      rateElement.dataset.state = model.state;
      const periodElement = document.getElementById('service-health-period');
      if (periodElement) periodElement.textContent = period;
      const earlierElement = document.getElementById('service-health-earlier');
      const latestElement = document.getElementById('service-health-latest');
      if (earlierElement) {
        earlierElement.textContent = model.points.length > 0
          ? timeFormatter.format(new Date(model.points[0].ts))
          : '--';
      }
      if (latestElement) {
        latestElement.textContent = model.points.length > 0
          ? timeFormatter.format(new Date(model.points.at(-1).ts))
          : '--';
      }
      grid.setAttribute('aria-label', hasData
        ? serviceHealthText(
          'index.health.summary',
          `${period}服务成功率 ${rate}，成功 ${model.success} 次，失败 ${model.error} 次`,
          {
            period,
            rate,
            success: formatNumber(model.success),
            error: formatNumber(model.error)
          }
        )
        : serviceHealthText('index.health.noData', `${period}暂无请求数据`, { period }));
      message.hidden = true;
      message.textContent = '';
    }

    function renderServiceHealthUnavailable() {
      const message = document.getElementById('service-health-message');
      const rateElement = document.getElementById('service-health-rate');
      if (rateElement) {
        rateElement.textContent = '--';
        rateElement.dataset.state = 'unknown';
      }
      if (message) {
        message.hidden = false;
        message.textContent = serviceHealthText(
          'index.health.unavailable',
          '健康数据暂时无法加载，将在下次刷新时重试。'
        );
      }
    }

    async function loadDashboard() {
      const generation = ++dashboardLoadGeneration;
      const dateRangeQuery = buildCurrentDateRangeQuery();
      const grid = document.getElementById('service-health-grid');
      const loadingElements = document.querySelectorAll('.metric-number');
      loadingElements.forEach(element => element.classList.add('animate-pulse'));
      if (grid) grid.setAttribute('aria-busy', 'true');

      const healthRequest = window.ServiceHealth
        ? window.ServiceHealth.buildRequest(dateRangeQuery, currentRangeHours())
        : null;
      // 用量概览两路聚合只对 admin 身份取数：api_token 只读身份访问
      // /admin/* 恒 403，跳过而不是打一串必然失败的请求。
      const adminViewer = !(window.isAPITokenRole && window.isAPITokenRole());
      const [statsResult, healthResult, usageResult, quotaResult] = await Promise.allSettled([
        fetchDataWithAuth(`/dashboard/summary?${dateRangeQuery}`),
        healthRequest
          ? fetchDataWithAuth(`/dashboard/metrics?${healthRequest.query}`)
          : Promise.reject(new Error('ServiceHealth unavailable')),
        adminViewer ? fetchDataWithAuth('/admin/usage') : Promise.resolve(null),
        adminViewer ? fetchDataWithAuth('/admin/quota') : Promise.resolve(null)
      ]);

      if (generation !== dashboardLoadGeneration) return;

      if (statsResult.status === 'fulfilled') {
        statsData = statsResult.value || statsData;
        updateStatsDisplay();
      } else {
        console.error('Failed to load stats:', statsResult.reason);
        showError(window.t('index.statsLoadFailed'));
      }

      if (healthResult.status === 'fulfilled') {
        serviceHealthModel = window.ServiceHealth.buildModel(
          healthResult.value,
          healthRequest.bucketMinutes
        );
        renderServiceHealth(serviceHealthModel);
      } else {
        console.error('Failed to load service health:', healthResult.reason);
        renderServiceHealthUnavailable();
      }

      if (usageResult.status === 'fulfilled') usagePayload = usageResult.value;
      if (quotaResult.status === 'fulfilled') quotaPayload = quotaResult.value;
      if (adminViewer && usageResult.status === 'rejected' && quotaResult.status === 'rejected') {
        console.error('Failed to load usage cards:', usageResult.reason || quotaResult.reason);
      }
      renderUsageCards();

      loadingElements.forEach(element => element.classList.remove('animate-pulse'));
      if (grid) grid.setAttribute('aria-busy', 'false');
    }

    // 更新统计显示：四张入口端点卡片，缺席的 api 显示 0
    function updateStatsDisplay() {
      const byApi = statsData.by_api || {};
      ENDPOINT_CARD_KEYS.forEach(api => updateOverviewCard(api, byApi[api]));
    }

    // 更新单个概览卡片的统计
    function updateOverviewCard(type, data) {
      const card = document.getElementById(`type-${type}-card`);
      if (!card) return;

      // 如果没有数据，显示默认值
      const totalRequests = data ? (data.total_requests || 0) : 0;
      const successRequests = data ? (data.success_requests || 0) : 0;
      const errorRequests = data ? (data.error_requests || 0) : 0;

      const successRate = totalRequests > 0
        ? ((successRequests / totalRequests) * 100).toFixed(1)
        : '0.0';

      // 更新基础统计（总请求、成功、失败、成功率）
      document.getElementById(`type-${type}-requests`).textContent = formatNumber(totalRequests);
      document.getElementById(`type-${type}-success`).textContent = formatNumber(successRequests);
      document.getElementById(`type-${type}-error`).textContent = formatNumber(errorRequests);
      const rateEl = document.getElementById(`type-${type}-rate`);
      rateEl.textContent = successRate + '%';
      // 成功率按 service-health 同口径分档（≥95 healthy / ≥80 warning / 其余 critical）；
      // 无请求时不着色，避免 0 流量被误读为故障
      if (totalRequests > 0) {
        const rate = successRequests / totalRequests;
        rateEl.dataset.state = rate >= 0.95 ? 'healthy' : rate >= 0.8 ? 'warning' : 'critical';
      } else {
        delete rateEl.dataset.state;
      }

      const inputTokens = data ? (data.total_input_tokens || 0) : 0;
      const outputTokens = data ? (data.total_output_tokens || 0) : 0;
      const totalCost = data ? (data.total_cost || 0) : 0;
      const effectiveCost = data && data.effective_cost !== undefined && data.effective_cost !== null
        ? Number(data.effective_cost) || 0
        : totalCost;

      document.getElementById(`type-${type}-input`).textContent = formatNumber(inputTokens);
      document.getElementById(`type-${type}-output`).textContent = formatNumber(outputTokens);
      document.getElementById(`type-${type}-cost`).innerHTML = buildCostStackHtml(totalCost, effectiveCost, { inline: true });

      const cacheReadTokens = data ? (data.total_cache_read_tokens || 0) : 0;
      const cacheReadEl = document.getElementById(`type-${type}-cache-read`);
      if (cacheReadEl) cacheReadEl.textContent = formatNumber(cacheReadTokens);

      const cacheCreateEl = document.getElementById(`type-${type}-cache-create`);
      if (cacheCreateEl) {
        const cacheCreateTokens = data ? (data.total_cache_creation_tokens || 0) : 0;
        cacheCreateEl.textContent = formatNumber(cacheCreateTokens);
      }
    }

    // ===== 用量概览：/admin/usage 与 /admin/quota 聚合快照 =====
    // 两种 payload 独立保鲜：单边失败时另一边照常渲染，重试交给下一轮
    // 自动刷新；双双为空或 api_token 身份时整块隐藏。
    let usagePayload = null;
    let quotaPayload = null;

    function usageText(key, fallback, params) {
      return typeof window.i18nText === 'function' ? window.i18nText(key, fallback, params) : fallback;
    }

    // 配额剩余量分档配色，与 accounts 页 toneFor 同阈值。
    function usageTone(pct) {
      return pct > 50 ? 'var(--success-600)' : pct > 20 ? 'var(--warning-600)' : 'var(--error-600)';
    }

    // forecast 一行燃烧文案；键位复用 accounts.burn.*（语义完全一致）。
    function quotaBurnLine(f) {
      if (!f || f.burn_per_hour == null) return '';
      const rate = Number(f.burn_per_hour);
      if (!(rate > 0)) return usageText('accounts.burn.refilled', '窗口内有回充或重置，暂不外推');
      const burn = usageText('accounts.burn.rate', '燃烧 {rate}%/h', { rate: rate.toFixed(2) });
      if (f.survives_until_reset) return usageText('accounts.burn.survives', '按当前速率可撑到重置') + ' · ' + burn;
      if (f.exhausted_at) {
        return usageText('accounts.burn.exhaust', '约 {h}h 后耗尽', { h: Number(f.hours_left || 0).toFixed(1) }) + ' · ' + burn;
      }
      return burn;
    }

    function quotaRowHtml(label, f) {
      const rem = f && f.remaining != null ? Math.max(0, Math.min(100, Number(f.remaining))) : null;
      const width = rem === null ? 0 : rem;
      const tone = rem === null ? 'var(--color-text-secondary)' : usageTone(rem);
      const val = rem === null ? '--' : `${rem.toFixed(0)}%`;
      return `<div class="usage-quota-row">
        <span class="usage-quota-label">${escapeHtml(label)}</span>
        <div class="usage-quota-track"><div class="usage-quota-fill" style="width:${width}%;background:${tone};"></div></div>
        <span class="usage-quota-val" style="color:${tone};">${val}</span>
      </div>`;
    }

    function laneUsageCardHtml(name, report) {
      const daily = report && report.daily;
      const weekly = report && report.weekly;
      const rem = daily && daily.remaining != null ? Math.max(0, Math.min(100, Number(daily.remaining))) : null;
      const tone = rem === null ? 'var(--color-text-secondary)' : usageTone(rem);
      const sub = quotaBurnLine(daily) || quotaBurnLine(weekly);
      return `<div class="card channel-card">
        <div class="channel-card-header">
          <div class="channel-card-title">${escapeHtml(name)}</div>
          <div class="channel-cost">
            <span class="cost-label">${escapeHtml(usageText('accounts.f.daily', '日配额'))}</span>
            <span class="cost-value" style="color:${tone};">${rem === null ? '--' : `${rem.toFixed(0)}%`}</span>
          </div>
        </div>
        <div class="usage-quota-rows">
          ${quotaRowHtml(usageText('accounts.f.daily', '日配额'), daily)}
          ${quotaRowHtml(usageText('accounts.f.weekly', '周配额'), weekly)}
        </div>
        ${sub ? `<div class="usage-quota-sub">${escapeHtml(sub)}</div>` : ''}
      </div>`;
    }

    // 今日 sends/row：sends_per_row 末日行是闸门放行数 ÷ logs 行的探针
    // （内层 connect 重试的唯一活指标）；date 不是本地今日时视为缺测。
    function todaySendsRatio(snap) {
      const spr = Array.isArray(snap.sends_per_row) ? snap.sends_per_row : [];
      const last = spr.length ? spr[spr.length - 1] : null;
      const now = new Date();
      const today = `${now.getFullYear()}-${String(now.getMonth() + 1).padStart(2, '0')}-${String(now.getDate()).padStart(2, '0')}`;
      if (!last || last.date !== today || last.ratio == null) return '--';
      return `${Number(last.ratio).toFixed(2)}×`;
    }

    function todayUsageCardHtml(snap) {
      const t0 = snap.today;
      if (!t0) return '';
      const req = t0.requests || 0;
      const err = t0.errors || 0;
      const rate = req > 0 ? (((req - err) / req) * 100).toFixed(1) : '0.0';
      const rateState = req > 0 ? (req - err) / req : null;
      const rateTone = rateState === null ? '' : ` data-state="${rateState >= 0.95 ? 'healthy' : rateState >= 0.8 ? 'warning' : 'critical'}"`;
      const credits = t0.credit_cost || 0;
      return `<div class="card channel-card">
        <div class="channel-card-header">
          <div class="channel-card-title">${escapeHtml(usageText('index.usage.today', '今日用量'))}</div>
          <div class="channel-cost">
            <span class="cost-label">${escapeHtml(usageText('index.usage.credits', '计费点数'))}</span>
            <span class="cost-value">${credits > 0 ? formatNumber(credits) : '--'}</span>
          </div>
        </div>
        <div class="channel-metrics">
          <div class="metric-item">
            <div class="metric-value metric-total">${formatNumber(req)}</div>
            <div class="metric-label">${escapeHtml(usageText('index.metrics.totalRequests', '总请求'))}</div>
          </div>
          <div class="metric-item">
            <div class="metric-value metric-success">${formatNumber(req - err)}</div>
            <div class="metric-label">${escapeHtml(usageText('index.metrics.success', '成功'))}</div>
          </div>
          <div class="metric-item">
            <div class="metric-value metric-error">${formatNumber(err)}</div>
            <div class="metric-label">${escapeHtml(usageText('index.metrics.failed', '失败'))}</div>
          </div>
          <div class="metric-item">
            <div class="metric-value metric-rate"${rateTone}>${rate}%</div>
            <div class="metric-label">${escapeHtml(usageText('index.metrics.successRate', '成功率'))}</div>
          </div>
        </div>
        <div class="token-stats">
          <div class="token-item">
            <span class="token-label">${escapeHtml(usageText('common.input', '输入'))}</span>
            <span class="token-value">${formatNumber(t0.input_tokens || 0)}</span>
          </div>
          <div class="token-item">
            <span class="token-label">${escapeHtml(usageText('common.output', '输出'))}</span>
            <span class="token-value">${formatNumber(t0.output_tokens || 0)}</span>
          </div>
          <div class="token-item">
            <span class="token-label">${escapeHtml(usageText('common.cacheRead', '缓存读'))}</span>
            <span class="token-value">${formatNumber(t0.cache_read_tokens || 0)}</span>
          </div>
          <div class="token-item" title="${escapeHtml(usageText('index.usage.sendsPerRowHint', '闸门放行数 ÷ 日志行：内层重试探针'))}">
            <span class="token-label">${escapeHtml(usageText('index.usage.sendsPerRow', '发送/请求'))}</span>
            <span class="token-value">${todaySendsRatio(snap)}</span>
          </div>
        </div>
      </div>`;
    }

    function topKeysCardHtml(snap) {
      const keys = Array.isArray(snap.keys) ? snap.keys.slice(0, 4) : [];
      if (!keys.length) return '';
      const rows = keys.map(k => {
        const full = String(k.name || '');
        const short = full.length > 12 ? `${full.slice(0, 12)}…` : full || '--';
        return `<div class="usage-key-row">
          <span class="usage-key-name" title="${escapeHtml(full)}">${escapeHtml(short)}</span>
          <span class="usage-key-req">${formatNumber(k.requests || 0)}</span>
          <span class="usage-key-tok">${formatNumber(k.total_tokens || 0)} tok</span>
        </div>`;
      }).join('');
      return `<div class="card channel-card">
        <div class="channel-card-header">
          <div class="channel-card-title">${escapeHtml(usageText('index.usage.topKeys', '高频令牌'))}</div>
          <div class="channel-cost">
            <span class="cost-label">${escapeHtml(usageText('index.usage.window', '窗口'))}</span>
            <span class="cost-value">${formatNumber((snap.days || []).length)}d</span>
          </div>
        </div>
        <div class="usage-key-rows">${rows}</div>
      </div>`;
    }

    function renderUsageCards() {
      const section = document.getElementById('usage-section');
      const grid = document.getElementById('usage-grid');
      if (!section || !grid) return;
      const snap = usagePayload && !usagePayload.disabled ? usagePayload.snapshot : null;
      const accounts = (quotaPayload && quotaPayload.accounts) || {};
      const parts = [];
      if (snap) {
        parts.push(todayUsageCardHtml(snap), topKeysCardHtml(snap));
      }
      Object.keys(accounts).sort().forEach(name => parts.push(laneUsageCardHtml(name, accounts[name])));
      const html = parts.filter(Boolean).join('');
      if (!html) {
        section.hidden = true;
        return;
      }
      grid.innerHTML = html;
      section.hidden = false;
    }

    // 通知系统统一由 ui.js 提供（showSuccess/showError/showNotification）

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // 自动刷新由 createAutoRefresh 统一管理（system_settings.auto_refresh_interval_seconds）

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'index',
      run: () => {
      window.bindTimeRangeSelector({
        containerId: 'index-time-range',
        values: ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'last_week', 'this_month', 'last_month', 'custom'],
        initialValue: currentTimeRange,
        customRange: currentCustomTimeRange,
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          loadDashboard();
        }
      });

      // 费用与服务健康检测共用同一日期范围快照。
      loadDashboard();

      if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
        window.i18n.onLocaleChange(() => {
          updateStatsDisplay();
          if (serviceHealthModel) renderServiceHealth(serviceHealthModel);
          renderUsageCards();
        });
      }

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadDashboard }).init();
      }

      // 添加页面动画
      document.querySelectorAll('.animate-slide-up').forEach((el, index) => {
        el.style.animationDelay = `${index * 0.1}s`;
      });
      }
    });
