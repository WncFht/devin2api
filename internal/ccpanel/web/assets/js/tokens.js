    const t = window.t;
    const API_BASE = '/admin';
    let allTokens = [];
    let isToday = true;      // 是否为本日（本日才显示最近一分钟）

    // 当前选中的时间范围(默认为本日)
    let currentTimeRange = 'today';
    let currentCustomTimeRange = null;

    // 模型限制相关状态（2026-01新增）
    let editAllowedModels = [];              // 编辑模态框中当前的模型限制列表
    let selectedAllowedModelIndices = new Set(); // 已选中的模型索引（批量删除用）
    let availableModelsCache = [];           // 可用模型缓存（/admin/model-registry 合并视图）
    let selectedModelsForAdd = new Set();    // 模型选择对话框中已选的模型
    let currentVisibleModels = [];            // 当前可见的模型列表（用于全选功能）
    let currentAllowedModelFilter = '';
    let initialEditExpiryState = { type: 'never', value: '' };

    // 配额字段单一事实源：create/edit 两表单的 markup、读回校验、重置与回显
    // 都由这张表驱动。元素 id = {prefix}{suffix}；wire 是 /admin/auth-tokens 的
    // JSON 键；编辑表单 cost 字段带已用额展示（id 按 suffix→UsedDisplay 推导）。
    const TOKEN_QUOTA_FIELDS = [
      { suffix: '5hCostLimitUSD', wire: 'cost_5h_limit_usd', labelKey: 'tokens.cost5hLimitLabel', cost: true },
      { suffix: 'DailyCostLimitUSD', wire: 'cost_daily_limit_usd', labelKey: 'tokens.dailyCostLimitLabel', cost: true },
      { suffix: 'WeeklyCostLimitUSD', wire: 'cost_weekly_limit_usd', labelKey: 'tokens.weeklyCostLimitLabel', cost: true },
      { suffix: 'MonthlyCostLimitUSD', wire: 'cost_monthly_limit_usd', labelKey: 'tokens.monthlyCostLimitLabel', cost: true },
      { suffix: 'CostLimitUSD', wire: 'cost_limit_usd', labelKey: 'tokens.costLimitLabel', cost: true },
      { suffix: 'MaxConcurrency', wire: 'max_concurrency', labelKey: 'tokens.maxConcurrencyLabel', phKey: 'tokens.maxConcurrencyPlaceholder', intKey: 'tokens.msg.maxConcurrencyInteger' },
      { suffix: 'MaxRPM', wire: 'max_rpm', labelKey: 'tokens.maxRPMLabel', phKey: 'tokens.zeroUnlimitedHint', intKey: 'tokens.msg.maxRPMInteger' }
    ];

    // 按 spec 把配额行写进宿主（data-i18n 随行，translatePage 重扫自动接管多语言）
    function stampTokenQuotaFields(prefix) {
      const host = document.getElementById(prefix + 'QuotaFields');
      if (!host) return;
      const editing = prefix === 'edit';
      host.innerHTML = TOKEN_QUOTA_FIELDS.map((f) => {
        const inputId = prefix + f.suffix;
        const phKey = f.cost ? 'tokens.costLimitPlaceholder' : f.phKey;
        const groupCls = editing ? ` token-edit-field token-edit-field--${f.cost ? 'cost' : (f.suffix === 'MaxConcurrency' ? 'concurrency' : 'rpm')}` : '';
        const usedId = editing && f.cost ? `edit${f.suffix.replace('LimitUSD', 'UsedDisplay')}` : '';
        return `<div class="form-group form-row-inline${groupCls}">
          <label for="${inputId}" class="form-label form-row-inline__label" data-i18n="${f.labelKey}">${t(f.labelKey)}</label>
          <div class="form-row-inline__content token-limit-control${editing && f.cost ? ' token-edit-cost-control' : ''}">
            <div class="token-limit-input-line${editing && f.cost ? ' token-edit-cost-row' : ''}">
              ${f.cost ? '<span class="token-cost-prefix" aria-hidden="true">$</span>' : ''}
              <input type="number" id="${inputId}" class="form-input field-grow" min="0" step="${f.cost ? '0.01' : '1'}" data-i18n-placeholder="${phKey}" placeholder="${t(phKey)}">
              <span class="token-limit-hint token-limit-hint--inline" data-i18n="tokens.zeroUnlimitedHint">${t('tokens.zeroUnlimitedHint')}</span>
            </div>
            ${usedId ? `<div class="token-limit-meta token-edit-cost-meta"><span id="${usedId}" class="token-edit-cost-used"></span></div>` : ''}
          </div>
        </div>`;
      }).join('');
    }

    // 读回校验：顺序 = 费用负值 → 并发整数 → RPM 整数（与原逐行展开同口径）
    function readTokenQuotaFields(prefix) {
      const values = {};
      for (const f of TOKEN_QUOTA_FIELDS) {
        const raw = document.getElementById(prefix + f.suffix).value;
        if (f.cost) {
          const v = parseFloat(raw) || 0;
          if (v < 0) return { error: t('tokens.msg.costLimitNegative') };
          values[f.wire] = v;
        } else {
          const r = parseNonNegativeIntInput(raw, f.intKey);
          if (r.error) return { error: r.error };
          values[f.wire] = r.value;
        }
      }
      return { values };
    }

    // expiryType → {expiresAt, customDate}；custom 未填返回 null（调用方弹提示并中止）
    function resolveTokenExpiry(expiryType, customInputId) {
      if (expiryType === 'never') return { expiresAt: null, customDate: '' };
      if (expiryType === 'custom') {
        const customDate = document.getElementById(customInputId).value;
        if (!customDate) return null;
        return { expiresAt: new Date(customDate).getTime(), customDate };
      }
      return { expiresAt: Date.now() + parseInt(expiryType) * 86400000, customDate: '' };
    }

    function initExpirySelects() {
      const template = document.getElementById('tpl-token-expiry-options');
      if (!template) return;

      const optionsHtml = template.innerHTML.trim();
      document.querySelectorAll('[data-expiry-select]').forEach((select) => {
        const currentValue = select.value;
        select.innerHTML = optionsHtml;
        if (currentValue) {
          select.value = currentValue;
        }
      });
    }

    window.initPageBootstrap({
      topbarKey: 'tokens',
      run: () => {
      initExpirySelects();
      stampTokenQuotaFields('token');
      stampTokenQuotaFields('edit');

      window.bindTimeRangeSelector({
        containerId: 'tokens-time-range',
        values: ['today', 'yesterday', 'day_before_yesterday', 'this_week', 'this_month', 'last_month', 'custom', 'all'],
        includeAll: true,
        initialValue: currentTimeRange,
        customRange: currentCustomTimeRange,
        onChange: (range, customRange) => {
          currentTimeRange = range;
          if (range === 'custom') currentCustomTimeRange = customRange;
          loadTokens();
        }
      });

      // 加载令牌列表(默认显示本日统计)
      loadTokens();

      // 预加载可用模型清单（模型选择弹窗数据源）
      loadAvailableModels();

      initPageActionDelegation();

      // 监听语言切换事件，重新渲染令牌相关动态内容
      window.i18n.onLocaleChange(() => {
        renderAllowedModelsTable();
        renderTokens();
      });

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadTokens }).init();
      }
      }
    });

    function initPageActionDelegation() {
      if (typeof window.initDelegatedActions !== 'function') return;

      window.initDelegatedActions({
        boundKey: 'tokensPageActionsBound',
        click: {
          'show-create-modal': () => showCreateModal(),
          'close-create-modal': () => closeCreateModal(),
          'create-token': () => createToken(),
          'close-token-result-modal': () => closeTokenResultModal(),
          'copy-token-result': () => copyToken(),
          'close-edit-modal': () => closeEditModal(),
          'update-token': () => updateToken(),
          'show-model-select-modal': () => showModelSelectModal(),
          'show-model-import-modal': () => showModelImportModal(),
          'batch-delete-allowed-models': () => batchDeleteSelectedAllowedModels(),
          'close-model-select-modal': () => closeModelSelectModal(),
          'confirm-model-selection': () => confirmModelSelection(),
          'close-model-import-modal': () => closeModelImportModal(),
          'confirm-model-import': () => confirmModelImport(),
          'remove-allowed-model': (actionTarget) => {
            const index = Number(actionTarget.dataset.index);
            if (!Number.isNaN(index)) {
              removeAllowedModel(index);
            }
          },
          'copy-token-hash': (actionTarget) => {
            const hash = actionTarget.dataset.token;
            if (hash) copyTokenToClipboard(hash);
          },
          'play-token': () => {
            if (typeof window.openChatModal === 'function') window.openChatModal({ mode: 'token' });
          },
          'edit-token': (actionTarget) => {
            const id = parseInt(actionTarget.closest('tr').dataset.tokenId);
            if (id) editToken(id);
          },
          'delete-token': (actionTarget) => {
            const id = parseInt(actionTarget.closest('tr').dataset.tokenId);
            if (id) deleteToken(id);
          }
        },
        change: {
          'toggle-custom-expiry': (actionTarget) => {
            document.getElementById('customExpiryContainer').style.display =
              actionTarget.value === 'custom' ? 'block' : 'none';
          },
          'toggle-edit-custom-expiry': (actionTarget) => {
            document.getElementById('editCustomExpiryContainer').style.display =
              actionTarget.value === 'custom' ? 'block' : 'none';
          },
          'toggle-select-all-allowed-models': (actionTarget) => toggleSelectAllAllowedModels(actionTarget.checked),
          'toggle-select-all-models': (actionTarget) => toggleSelectAllModels(actionTarget.checked),
          'toggle-allowed-model': (actionTarget) => {
            const index = Number(actionTarget.dataset.index);
            if (!Number.isNaN(index)) {
              toggleAllowedModelSelection(index, actionTarget.checked);
            }
          }
        },
        input: {
          'filter-available-models': (actionTarget) => filterAvailableModels(actionTarget.value),
          'filter-allowed-models': (actionTarget) => filterAllowedModels(actionTarget.value),
          'update-model-import-preview': () => updateModelImportPreview()
        }
      });
    }

    async function loadTokens() {
      try {
        const query = typeof window.buildDateRangeQuery === 'function'
          ? window.buildDateRangeQuery(currentTimeRange, currentCustomTimeRange)
          : `range=${encodeURIComponent(currentTimeRange)}`;
        const url = `${API_BASE}/auth-tokens?${query}`;

        const data = await fetchDataWithAuth(url);
        allTokens = (data && data.tokens) || [];
        isToday = !!(data && data.is_today);
        renderTokens();
      } catch (error) {

        console.error('Failed to load tokens:', error);
        window.showNotification(t('tokens.msg.loadFailed') + ': ' + error.message, 'error');
      }
    }

    function renderTokens() {
      const container = document.getElementById('tokens-container');
      const emptyState = document.getElementById('empty-state');

      if (allTokens.length === 0) {
        container.innerHTML = '';
        emptyState.style.display = 'block';
        return;
      }

      emptyState.style.display = 'none';

      // 构建表格结构
      const table = document.createElement('table');
      table.className = 'mobile-card-table tokens-table';
      
      table.innerHTML = `
        <colgroup>
          <col class="tokens-colgroup-token">
          <col class="tokens-colgroup-calls">
          <col class="tokens-colgroup-success-rate">
          <col class="tokens-colgroup-rpm">
          <col class="tokens-colgroup-token-usage">
          <col class="tokens-colgroup-cost">
          <col class="tokens-colgroup-concurrency">
          <col class="tokens-colgroup-stream">
          <col class="tokens-colgroup-non-stream">
          <col class="tokens-colgroup-last-used">
          <col class="tokens-colgroup-actions">
        </colgroup>
        <thead>
          <tr>
            <th scope="col">${t('tokens.table.token')}</th>
            <th scope="col" class="tokens-col-calls">${t('tokens.table.callCount')}</th>
            <th scope="col" class="tokens-col-success-rate">${t('tokens.table.successRate')}</th>
            <th scope="col" class="tokens-col-rpm" title="${t('tokens.table.rpmTitle')}">${t('tokens.table.rpm')}</th>
            <th scope="col" class="tokens-col-token-usage">${t('tokens.table.tokenUsage')}</th>
            <th scope="col" class="tokens-col-cost">${t('tokens.table.totalCost')}</th>
            <th scope="col" class="tokens-col-concurrency">${t('tokens.table.concurrency')}</th>
            <th scope="col" class="tokens-col-stream">${t('tokens.table.streamAvg')}</th>
            <th scope="col" class="tokens-col-non-stream">${t('tokens.table.nonStreamAvg')}</th>
            <th scope="col">${t('tokens.table.lastUsed')}</th>
            <th scope="col" class="tokens-actions-col">${t('tokens.table.actions')}</th>
          </tr>
        </thead>
      `;

      const tbody = document.createElement('tbody');

      // 行渲染统一走 tpl-token-row 模板
      allTokens.forEach(token => {
        const row = createTokenRowWithTemplate(token);
        if (row) tbody.appendChild(row);
      });

      table.appendChild(tbody);
      container.innerHTML = '';
      container.appendChild(table);

      // 翻译动态渲染的内容中的 data-i18n 属性（只扫新表，不碰整页）
      if (window.i18n.translatePage) {
        window.i18n.translatePage(table);
      }
    }

    /**
     * 使用模板引擎渲染令牌行
     */
    function createTokenRowWithTemplate(token) {
      
      const locale = window.i18n?.getLocale?.() || 'en';
      const status = getTokenStatus(token);
      const createdAt = new Date(token.created_at).toLocaleString(locale);
      const lastUsed = formatLastUsedHtml(token.last_used_at, locale);
      const expiresAt = token.expires_at ? new Date(token.expires_at).toLocaleString(locale) : t('tokens.expiryNever');

      // 计算统计信息
      const successCount = token.success_count || 0;
      const failureCount = token.failure_count || 0;
      const totalCount = successCount + failureCount;
      const successRate = totalCount > 0 ? ((successCount / totalCount) * 100).toFixed(1) : 0;

      // 预构建各个HTML片段(保留条件逻辑在JS中)
      const callsHtml = buildCallsHtml(successCount, failureCount, totalCount);
      const successRateHtml = buildSuccessRateHtml(successRate, totalCount);
      const rpmHtml = buildRpmHtml(token);
      const tokensHtml = buildTokensHtml(token);
      const costHtml = buildCostHtml(token.total_cost_usd, token.effective_cost_usd);
      const concurrencyHtml = buildConcurrencyHtml(token.max_concurrency);
      const streamAvgHtml = buildResponseTimeHtml(token.stream_avg_ttfb, token.stream_count, window.getFirstByteTimingTone);
      const nonStreamAvgHtml = buildResponseTimeHtml(token.non_stream_avg_rt, token.non_stream_count, window.getDurationTimingTone);
      const costCellClass = token.total_cost_usd > 0 ? '' : 'mobile-empty-cell';
      const streamCellClass = token.stream_count ? '' : 'mobile-empty-cell';
      const nonStreamCellClass = token.non_stream_count ? '' : 'mobile-empty-cell';
      const classBadgeHtml = buildClassBadgeHtml(token.class);

      // 使用模板引擎渲染；匿名通道行没有可出示的凭据，显示固定占位符
      const maskedToken = token.anonymous
        ? t('tokens.anonymousDisplay')
        : token.token.length > 8
          ? token.token.substring(0, 4) + '****' + token.token.slice(-4)
          : token.token;

      return TemplateEngine.render('tpl-token-row', {
        id: token.id,
        description: token.description,
        token: token.anonymous ? '' : token.token,
        maskedToken: maskedToken,
        statusClass: token.anonymous ? 'anonymous' : status.class,
        rowClass: token.anonymous ? 'token-card-row--anonymous' : '',
        createdAt: createdAt,
        createdLabel: t('tokens.createdSuffix'),
        expiresAt: expiresAt,
        callsHtml: callsHtml,
        classBadgeHtml: classBadgeHtml,
        rpmHtml: rpmHtml,
        successRateHtml: successRateHtml,
        tokensHtml: tokensHtml,
        costHtml: costHtml,
        costCellClass: costCellClass,
        concurrencyHtml: concurrencyHtml,
        streamAvgHtml: streamAvgHtml,
        streamCellClass: streamCellClass,
        nonStreamAvgHtml: nonStreamAvgHtml,
        nonStreamCellClass: nonStreamCellClass,
        lastUsed: lastUsed,
        mobileLabelToken: t('tokens.table.token'),
        mobileLabelCalls: t('tokens.table.callCount'),
        mobileLabelSuccessRate: t('tokens.table.successRate'),
        mobileLabelRpm: t('tokens.table.rpm'),
        mobileLabelTokenUsage: t('tokens.table.tokenUsage'),
        mobileLabelCost: t('tokens.table.totalCost'),
        mobileLabelConcurrency: t('tokens.table.concurrency'),
        mobileLabelStream: t('tokens.table.streamAvg'),
        mobileLabelNonStream: t('tokens.table.nonStreamAvg'),
        mobileLabelLastUsed: t('tokens.table.lastUsed'),
        mobileLabelActions: t('tokens.table.actions')
      });
    }

    function formatLastUsedHtml(value, locale) {
      if (!value) {
        return `<span class="token-value-muted">${t('tokens.neverUsed')}</span>`;
      }

      const usedAt = new Date(value);
      if (Number.isNaN(usedAt.getTime())) {
        return '<span class="token-value-muted">—</span>';
      }

      const dateText = usedAt.toLocaleDateString(locale);
      const timeText = usedAt.toLocaleTimeString(locale, { hour12: false });
      return `
        <span class="token-last-used">
          <span class="token-last-used-date">${dateText}</span>
          <span class="token-last-used-time">${timeText}</span>
        </span>
      `;
    }

    /**
     * 构建调用次数HTML
     */
    function buildCallsHtml(successCount, failureCount, totalCount) {
      if (totalCount === 0) {
        return '<span class="token-value-muted">—</span>';
      }

      let html = '<div class="token-call-stats">';
      html += `<span class="stats-badge token-call-badge token-call-badge--success" title="${t('tokens.successCall')}">`;
      html += `<span class="token-call-icon token-call-icon--success">✓</span> ${successCount.toLocaleString()}`;
      html += `</span>`;

      if (failureCount > 0) {
        html += `<span class="stats-badge token-call-badge token-call-badge--failure" title="${t('tokens.failedCall')}">`;
        html += `<span class="token-call-icon token-call-icon--failure">✗</span> ${failureCount.toLocaleString()}`;
        html += `</span>`;
      }

      html += '</div>';
      return html;
    }

    /**
     * 构建请求类徽章：fg/bg 与 X-Gate-Class、闸门词表同词——
     * 徽章直接显示 wire 值，语义解释放 title；空值/未知值按 fg 归一
     * （与服务端 NormalizeClass 同向）。
     */
    function buildClassBadgeHtml(cls) {
      const isBg = cls === 'bg';
      const value = isBg ? 'bg' : 'fg';
      const title = isBg ? t('tokens.classBgTitle') : t('tokens.classFgTitle');
      return `<span class="token-class-badge token-class-badge--${value}" title="${title}">${value}</span>`;
    }

    /**
     * 构建RPM HTML（峰/均/近格式）
     */
    function buildRpmHtml(token) {
      const peakRPM = token.peak_rpm || 0;
      const avgRPM = token.avg_rpm || 0;
      const recentRPM = token.recent_rpm || 0;

      // 如果都是0，返回空
      if (peakRPM < 0.01 && avgRPM < 0.01 && recentRPM < 0.01) {
        return '<span class="token-value-muted">—</span>';
      }

      // 格式化RPM值
      const formatRpm = (rpm) => {
        if (rpm < 0.01) return '—';
        if (rpm >= 1000) return (rpm / 1000).toFixed(1) + 'K';
        if (rpm >= 1) return rpm.toFixed(1);
        return rpm.toFixed(2);
      };

      const peakText = formatRpm(peakRPM);
      const avgText = formatRpm(avgRPM);
      const recentText = isToday ? formatRpm(recentRPM)  : '—';

      let rpmClass = 'token-rpm token-rpm--high';
      if (peakRPM < 10) rpmClass = 'token-rpm token-rpm--low';
      else if (peakRPM < 100) rpmClass = 'token-rpm token-rpm--medium';

      return `<span class="${rpmClass}">${peakText}/${avgText}/${recentText}</span>`;
    }

    /**
     * RPM 颜色：低流量绿色，中等橙色，高流量红色
     */
    /**
     * 构建成功率HTML
     */
    function buildSuccessRateHtml(successRate, totalCount) {
      if (totalCount === 0) {
        return '<span class="token-value-muted">—</span>';
      }

      let className = 'stats-badge';
      if (successRate >= 95) className += ' success-rate-high';
      else if (successRate >= 80) className += ' success-rate-medium';
      else className += ' success-rate-low';

      return `<span class="${className}">${successRate}%</span>`;
    }

    /**
     * 构建Token用量HTML
     */
    function buildTokensHtml(token) {
      const hasTokens = token.prompt_tokens_total > 0 ||
                        token.completion_tokens_total > 0 ||
                        token.cache_read_tokens_total > 0 ||
                        token.cache_creation_tokens_total > 0;

      if (!hasTokens) {
        return '<span class="token-value-muted">—</span>';
      }

      const items = [];
      const pushUsageItem = (variant, label, title, count) => {
        if (!count || count <= 0) return;
        items.push(
          `<span class="token-usage-item token-usage-item--${variant}" title="${title}">` +
            `<span class="token-usage-label">${label}</span>` +
            `<span class="token-usage-value">${formatNumber(count)}</span>` +
          `</span>`
        );
      };

      pushUsageItem('input', t('tokens.input'), t('tokens.inputTokens'), token.prompt_tokens_total || 0);
      pushUsageItem('output', t('tokens.output'), t('tokens.outputTokens'), token.completion_tokens_total || 0);
      pushUsageItem('cache-read', t('tokens.cacheRead'), t('tokens.cacheReadTokens'), token.cache_read_tokens_total || 0);
      pushUsageItem('cache-create', t('tokens.cacheCreate'), t('tokens.cacheCreateTokens'), token.cache_creation_tokens_total || 0);

      return `<div class="token-usage-metrics">${items.join('')}</div>`;
    }

    /**
     * 构建总费用HTML
     */
    function buildCostHtml(totalCostUsd, effectiveCostUsd) {
      if (!totalCostUsd || totalCostUsd <= 0) {
        return '<span class="token-value-muted">—</span>';
      }

      const costStack = buildCostStackHtml(totalCostUsd, effectiveCostUsd);
      return `
        <div class="token-cost">
          ${costStack}
        </div>
      `;
    }

    function buildConcurrencyHtml(maxConcurrency) {
      const limit = Number(maxConcurrency) || 0;
      if (limit <= 0) {
        return '<span class="token-value-muted">∞</span>';
      }
      return `<span class="metric-value">${limit.toLocaleString()}</span>`;
    }

    function parseNonNegativeIntInput(rawValue, errorKey) {
      const normalized = String(rawValue ?? '').trim();
      if (normalized === '') {
        return { value: 0 };
      }

      const parsed = Number(normalized);
      if (!Number.isFinite(parsed) || !Number.isInteger(parsed) || parsed < 0) {
        return { error: t(errorKey) };
      }

      return { value: parsed };
    }

    function fillCostLimitField(inputId, usedDisplayId, limitUSD, usedUSD) {
      const input = document.getElementById(inputId);
      const usedDisplay = document.getElementById(usedDisplayId);
      if (input) {
        input.value = limitUSD || 0;
      }
      if (usedDisplay) {
        const costUsed = Number(usedUSD);
        const used = Number.isFinite(costUsed) ? costUsed : 0;
        usedDisplay.textContent = `${t('tokens.costUsedPrefix')}: $${used.toFixed(4)}`;
      }
    }

    /**
     * 构建响应时间HTML；toneFn 产出 tone 名，着色走 .tone-* 共享类。
     */
    function buildResponseTimeHtml(time, count, toneFn) {
      if (!count || count === 0) {
        return '<span class="token-value-muted">—</span>';
      }

      const num = Number(time);
      if (!Number.isFinite(num) || num <= 0) {
        return '<span class="token-value-muted">—</span>';
      }
      return `<span class="metric-value${window.toneClass(toneFn(num))}">${num.toFixed(2)}s</span>`;
    }

    function getTokenStatus(token) {
      
      if (token.is_expired) return { class: 'expired', text: t('tokens.status.expired') };
      if (!token.is_active) return { class: 'inactive', text: t('tokens.status.inactive') };
      return { class: 'active', text: t('tokens.status.active') };
    }

    function showCreateModal() {
      document.getElementById('tokenDescription').value = '';
      document.getElementById('tokenExpiry').value = 'never';
      TOKEN_QUOTA_FIELDS.forEach((f) => { document.getElementById('token' + f.suffix).value = 0; });
      document.getElementById('tokenAllowedModels').value = '';
      document.getElementById('tokenClass').value = 'fg';
      document.getElementById('tokenActive').checked = true;
      document.getElementById('tokenAnonymous').checked = false;
      document.getElementById('customExpiryContainer').style.display = 'none';
      Modal.open(document.getElementById('createModal'));
    }

    function closeCreateModal() {
      Modal.close(document.getElementById('createModal'));
    }

    async function createToken() {

      const anonymous = document.getElementById('tokenAnonymous').checked;
      // 匿名通道行没有明文可出示，描述缺省成 anonymous 即可
      const description = document.getElementById('tokenDescription').value.trim();
      if (!anonymous && !description) {
        window.showNotification(t('tokens.msg.enterDescription'), 'error');
        return;
      }
      const expiry = resolveTokenExpiry(document.getElementById('tokenExpiry').value, 'customExpiry');
      if (!expiry) {
        window.showNotification(t('tokens.msg.selectExpiry'), 'error');
        return;
      }
      const isActive = document.getElementById('tokenActive').checked;
      const quota = readTokenQuotaFields('token');
      if (quota.error) {
        window.showNotification(quota.error, 'error');
        return;
      }
      const allowedModels = parseModelInput(document.getElementById('tokenAllowedModels').value);
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/auth-tokens`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            description,
            anonymous,
            class: document.getElementById('tokenClass').value,
            expires_at: expiry.expiresAt,
            is_active: isActive,
            allowed_models: allowedModels,
            ...quota.values
          })
        });

        closeCreateModal();
        if (!anonymous && data.token) {
          document.getElementById('newTokenValue').value = data.token;
          Modal.open(document.getElementById('tokenResultModal'), {
            onClose: () => { document.getElementById('newTokenValue').value = ''; },
          });
        }
        loadTokens();
        window.showNotification(t(anonymous ? 'tokens.msg.anonymousCreateSuccess' : 'tokens.msg.createSuccess'), 'success');
      } catch (error) {
        console.error('Failed to create token:', error);
        window.showNotification(t('tokens.msg.createFailed') + ': ' + error.message, 'error');
      }
    }

    function copyToken() {
      const textarea = document.getElementById('newTokenValue');
      window.copyToClipboard(textarea.value).then(() => {
        window.showNotification(t('tokens.msg.copySuccess'), 'success');
      });
    }

    function copyTokenToClipboard(hash) {
      window.copyToClipboard(hash).then(() => {
        window.showNotification(t('tokens.msg.copySuccess'), 'success');
      });
    }

    function closeTokenResultModal() {
      Modal.close(document.getElementById('tokenResultModal'));
    }

    function editToken(id) {
      const token = allTokens.find(tok => tok.id === id);
      if (!token) return;
      document.getElementById('editTokenId').value = id;
      // 匿名通道行没有可出示的凭据，令牌位显示占位符而非存储哈希
      document.getElementById('editTokenValue').value = token.anonymous ? t('tokens.anonymousDisplay') : (token.token || '');
      document.getElementById('editTokenDescription').value = token.description;
      document.getElementById('editTokenActive').checked = token.is_active;
      const expiryTypeInput = document.getElementById('editTokenExpiry');
      const customExpiryInput = document.getElementById('editCustomExpiry');
      if (!token.expires_at) {
        expiryTypeInput.value = 'never';
        customExpiryInput.value = '';
        document.getElementById('editCustomExpiryContainer').style.display = 'none';
      } else {
        expiryTypeInput.value = 'custom';
        document.getElementById('editCustomExpiryContainer').style.display = 'block';
        customExpiryInput.value = TokenExpiry.formatDateTimeLocal(token.expires_at);
      }
      initialEditExpiryState = { type: expiryTypeInput.value, value: customExpiryInput.value };
      // 请求类与服务端 NormalizeClass 同向：空值/未知值回显 fg
      document.getElementById('editTokenClass').value = token.class === 'bg' ? 'bg' : 'fg';

      TOKEN_QUOTA_FIELDS.forEach((f) => {
        if (f.cost) {
          fillCostLimitField('edit' + f.suffix, 'edit' + f.suffix.replace('LimitUSD', 'UsedDisplay'),
            token[f.wire], token[f.wire.replace('_limit_', '_used_')]);
        } else {
          document.getElementById('edit' + f.suffix).value = token[f.wire] || 0;
        }
      });

      // 初始化模型限制状态（2026-01新增）
      editAllowedModels = (token.allowed_models || []).slice();
      selectedAllowedModelIndices.clear();
      currentAllowedModelFilter = '';
      const allowedModelFilterInput = document.getElementById('allowedModelFilterInput');
      if (allowedModelFilterInput) allowedModelFilterInput.value = '';
      renderAllowedModelsTable();

      Modal.open(document.getElementById('editModal'), { onClose: resetEditModalState });
    }

    function resetEditModalState() {
      document.getElementById('editTokenValue').value = '';
      document.getElementById('editCustomExpiry').value = '';
      document.getElementById('editCustomExpiryContainer').style.display = 'none';
      initialEditExpiryState = { type: 'never', value: '' };
      // 清理模型限制状态
      editAllowedModels = [];
      selectedAllowedModelIndices.clear();
      currentAllowedModelFilter = '';
    }

    function closeEditModal() {
      Modal.close(document.getElementById('editModal'));
    }

    async function updateToken() {
      
      const id = document.getElementById('editTokenId').value;
      const description = document.getElementById('editTokenDescription').value.trim();
      const isActive = document.getElementById('editTokenActive').checked;
      const expiryType = document.getElementById('editTokenExpiry').value;
      const quota = readTokenQuotaFields('edit');
      if (quota.error) {
        window.showNotification(quota.error, 'error');
        return;
      }
      const expiry = resolveTokenExpiry(expiryType, 'editCustomExpiry');
      if (!expiry) {
        window.showNotification(t('tokens.msg.selectExpiry'), 'error');
        return;
      }
      const expiryUpdate = TokenExpiry.buildUpdatePayload(
        initialEditExpiryState,
        { type: expiryType, value: expiry.customDate },
        expiry.expiresAt
      );
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'PUT',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            description,
            is_active: isActive,
            ...expiryUpdate,
            class: document.getElementById('editTokenClass').value,
            allowed_models: editAllowedModels,  // 2026-01新增：模型限制
            ...quota.values
          })
        });
        closeEditModal();
        loadTokens();
        window.showNotification(t('tokens.msg.updateSuccess'), 'success');
      } catch (error) {
        console.error('Failed to update token:', error);
        window.showNotification(t('tokens.msg.updateFailed') + ': ' + error.message, 'error');
      }
    }

    async function deleteToken(id) {

      if (!(await Modal.confirm(t('tokens.msg.deleteConfirm'), { danger: true }))) return;
      try {
        await fetchDataWithAuth(`${API_BASE}/auth-tokens/${id}`, {
          method: 'DELETE'
        });
        loadTokens();
        window.showNotification(t('tokens.msg.deleteSuccess'), 'success');
      } catch (error) {
        console.error('Failed to delete token:', error);
        window.showNotification(t('tokens.msg.deleteFailed') + ': ' + error.message, 'error');
      }
    }

    // ============================================================================
    // 模型限制功能（2026-01新增）
    // ============================================================================

    /**
     * 加载可用模型清单（/admin/model-registry 合并视图：目录∪别名∪注册表∪流量）。
     * 单上游无渠道维，模型选择弹窗直接取全量启用模型名。
     */
    async function loadAvailableModels() {
      try {
        const data = await fetchDataWithAuth(`${API_BASE}/model-registry`);
        const rows = (data && data.models) || [];
        availableModelsCache = rows
          .filter(r => r && r.enabled !== false && r.model)
          .map(r => r.model)
          .sort();
      } catch (error) {
        console.error('Failed to load model registry:', error);
      }
    }

    function normalizeRestrictionFilter(value) {
      return String(value || '').trim().toLowerCase();
    }

    function getVisibleAllowedModelEntries() {
      const entries = editAllowedModels.map((model, index) => ({ model, index }));
      const filter = normalizeRestrictionFilter(currentAllowedModelFilter);
      if (!filter) return entries;
      return entries.filter(({ model }) => String(model).toLowerCase().includes(filter));
    }

    function filterAllowedModels(searchText) {
      currentAllowedModelFilter = searchText;
      renderAllowedModelsTable();
    }

    /**
     * 渲染模型限制表格
     */
    function renderAllowedModelsTable() {
      const tbody = document.getElementById('allowedModelsTableBody');
      const countSpan = document.getElementById('editAllowedModelsCount');
      const selectAllCheckbox = document.getElementById('selectAllAllowedModels');
      const mobileLabelModelName = t('tokens.modelName');
      const mobileLabelActions = t('tokens.table.actions');
      const visibleModelEntries = getVisibleAllowedModelEntries();

      if (!tbody) return;

      // 更新计数
      if (countSpan) countSpan.textContent = editAllowedModels.length;

      // 更新批量删除按钮状态
      updateBatchDeleteBtn();

      // 更新全选复选框状态
      if (selectAllCheckbox) {
        const selectedVisibleCount = visibleModelEntries.filter(({ index }) => selectedAllowedModelIndices.has(index)).length;
        selectAllCheckbox.checked = visibleModelEntries.length > 0 && selectedVisibleCount === visibleModelEntries.length;
        selectAllCheckbox.indeterminate = selectedVisibleCount > 0 && selectedVisibleCount < visibleModelEntries.length;
      }

      if (editAllowedModels.length === 0) {
        tbody.innerHTML = `
          <tr class="allowed-models-empty-row">
            <td colspan="3" class="allowed-models-empty-cell">
              ${t('tokens.noModelRestriction')}
            </td>
          </tr>
        `;
        return;
      }
      if (visibleModelEntries.length === 0) {
        tbody.innerHTML = `
          <tr class="allowed-models-empty-row">
            <td colspan="3" class="allowed-models-empty-cell">
              ${t('tokens.noMatchingModel')}
            </td>
          </tr>
        `;
        return;
      }

      tbody.innerHTML = visibleModelEntries.map(({ model, index }) => {
        return `
        <tr class="mobile-inline-row allowed-model-row">
          <td class="allowed-model-col-select mobile-inline-no-label">
            <input type="checkbox" class="allowed-model-checkbox" data-index="${index}"
              data-change-action="toggle-allowed-model"
              ${selectedAllowedModelIndices.has(index) ? 'checked' : ''}
            >
          </td>
          <td class="allowed-model-col-name" data-mobile-label="${mobileLabelModelName}">${escapeHtml(model)}</td>
          <td class="allowed-model-col-actions" data-mobile-label="${mobileLabelActions}">
            <button type="button" class="allowed-model-remove-btn btn btn-secondary btn-sm" data-action="remove-allowed-model" data-index="${index}">${t('common.delete')}</button>
          </td>
        </tr>
      `}).join('');
    }

    /**
     * 切换单个模型的选中状态
     */
    function toggleAllowedModelSelection(index, checked) {
      if (checked) {
        selectedAllowedModelIndices.add(index);
      } else {
        selectedAllowedModelIndices.delete(index);
      }
      updateBatchDeleteBtn();
      updateSelectAllCheckbox();
    }

    /**
     * 全选/取消全选模型
     */
    function toggleSelectAllAllowedModels(checked) {
      if (checked) {
        getVisibleAllowedModelEntries().forEach(({ index }) => selectedAllowedModelIndices.add(index));
      } else {
        getVisibleAllowedModelEntries().forEach(({ index }) => selectedAllowedModelIndices.delete(index));
      }
      renderAllowedModelsTable();
    }

    /**
     * 更新批量删除按钮状态
     */
    function updateBatchDeleteBtn() {
      const btn = document.getElementById('batchDeleteAllowedModelsBtn');
      if (btn) {
        btn.disabled = selectedAllowedModelIndices.size === 0;
      }
    }

    /**
     * 更新全选复选框状态
     */
    function updateSelectAllCheckbox() {
      const checkbox = document.getElementById('selectAllAllowedModels');
      if (checkbox) {
        const visibleModelEntries = getVisibleAllowedModelEntries();
        const selectedVisibleCount = visibleModelEntries.filter(({ index }) => selectedAllowedModelIndices.has(index)).length;
        checkbox.checked = visibleModelEntries.length > 0 && selectedVisibleCount === visibleModelEntries.length;
        checkbox.indeterminate = selectedVisibleCount > 0 && selectedVisibleCount < visibleModelEntries.length;
      }
    }

    /**
     * 删除单个模型
     */
    function removeAllowedModel(index) {
      editAllowedModels.splice(index, 1);
      // 重建选中索引（删除后索引会变化）
      const newIndices = new Set();
      selectedAllowedModelIndices.forEach(i => {
        if (i < index) newIndices.add(i);
        else if (i > index) newIndices.add(i - 1);
      });
      selectedAllowedModelIndices = newIndices;
      renderAllowedModelsTable();
    }

    /**
     * 批量删除选中的模型
     */
    function batchDeleteSelectedAllowedModels() {
      if (selectedAllowedModelIndices.size === 0) return;

      // 从大到小排序，避免删除时索引偏移问题
      const indices = Array.from(selectedAllowedModelIndices).sort((a, b) => b - a);
      indices.forEach(index => {
        editAllowedModels.splice(index, 1);
      });
      selectedAllowedModelIndices.clear();
      renderAllowedModelsTable();
    }

    /**
     * 显示模型选择对话框
     */
    async function showModelSelectModal() {
      if (availableModelsCache.length === 0) {
        await loadAvailableModels();
      }
      selectedModelsForAdd.clear();
      document.getElementById('modelSearchInput').value = '';
      renderAvailableModels('');
      Modal.open(document.getElementById('modelSelectModal'), { onClose: () => selectedModelsForAdd.clear() });
    }

    /**
     * 关闭模型选择对话框
     */
    function closeModelSelectModal() {
      Modal.close(document.getElementById('modelSelectModal'));
    }

    /**
     * 搜索过滤可用模型
     */
    function filterAvailableModels(searchText) {
      renderAvailableModels(searchText);
    }

    /**
     * 渲染可用模型列表
     */
    function renderAvailableModels(searchText) {
      const container = document.getElementById('availableModelsContainer');
      const countSpan = document.getElementById('selectedModelsCount');
      const selectAllContainer = document.getElementById('selectAllContainer');
      const selectAllCheckbox = document.getElementById('selectAllModelsCheckbox');
      const visibleModelsCount = document.getElementById('visibleModelsCount');
      if (!container) return;

      // 过滤已添加的模型
      const existingModels = new Set(editAllowedModels.map(m => m.toLowerCase()));
      const sourceModels = availableModelsCache;
      let models = sourceModels.filter(m => !existingModels.has(m.toLowerCase()));

      // 搜索过滤
      if (searchText) {
        const search = searchText.toLowerCase();
        models = models.filter(m => m.toLowerCase().includes(search));
      }

      // 保存当前可见模型列表（用于全选功能）
      currentVisibleModels = models;

      // 更新选中计数
      if (countSpan) countSpan.textContent = selectedModelsForAdd.size;

      
      if (models.length === 0) {
        const isEmptyCache = sourceModels.length === 0;
        const message = searchText
          ? t('tokens.noMatchingModel')
          : isEmptyCache
            ? t('tokens.noModelsAvailable')
            : t('tokens.allModelsAdded');
        container.innerHTML = `
          <div class="available-models-empty">
            ${message}
          </div>
        `;
        // 隐藏全选容器，恢复列表圆角
        if (selectAllContainer) selectAllContainer.style.display = 'none';
        container.classList.add('available-models-container--standalone');
        container.classList.remove('available-models-container--stacked');
        return;
      }

      // 显示全选容器，调整列表圆角
      if (selectAllContainer) {
        selectAllContainer.style.display = 'block';
      }
      container.classList.add('available-models-container--stacked');
      container.classList.remove('available-models-container--standalone');

      // 更新全选复选框状态
      if (selectAllCheckbox) {
        const allSelected = models.every(m => selectedModelsForAdd.has(m));
        selectAllCheckbox.checked = allSelected;
        selectAllCheckbox.indeterminate = !allSelected && models.some(m => selectedModelsForAdd.has(m));
      }
      if (visibleModelsCount) {
        
        visibleModelsCount.textContent = t('tokens.visibleModelsCount', { count: models.length });
      }

      container.innerHTML = models.map(model => `
        <label class="model-option-item" data-model="${escapeHtml(model)}">
          <input type="checkbox" class="model-option-checkbox" data-model="${escapeHtml(model)}"
            ${selectedModelsForAdd.has(model) ? 'checked' : ''}>
          <span class="model-option-label">${escapeHtml(model)}</span>
        </label>
      `).join('');

      // Event delegation: attach once on container
      if (!container.dataset.delegated) {
        container.addEventListener('change', (e) => {
          const checkbox = e.target.closest('.model-option-checkbox');
          if (checkbox) {
            toggleModelForAdd(checkbox.dataset.model || '', checkbox.checked);
          }
        });
        container.dataset.delegated = '1';
      }
    }

    /**
     * 切换待添加模型的选中状态
     */
    function toggleModelForAdd(model, checked) {
      if (checked) {
        selectedModelsForAdd.add(model);
      } else {
        selectedModelsForAdd.delete(model);
      }
      document.getElementById('selectedModelsCount').textContent = selectedModelsForAdd.size;
      updateSelectAllCheckboxState();
    }

    /**
     * 更新全选复选框状态
     */
    function updateSelectAllCheckboxState() {
      const selectAllCheckbox = document.getElementById('selectAllModelsCheckbox');
      if (!selectAllCheckbox || currentVisibleModels.length === 0) return;

      const allSelected = currentVisibleModels.every(m => selectedModelsForAdd.has(m));
      selectAllCheckbox.checked = allSelected;
      selectAllCheckbox.indeterminate = !allSelected && currentVisibleModels.some(m => selectedModelsForAdd.has(m));
    }

    /**
     * 全选/取消全选当前可见模型
     */
    function toggleSelectAllModels(checked) {
      currentVisibleModels.forEach(model => {
        if (checked) {
          selectedModelsForAdd.add(model);
        } else {
          selectedModelsForAdd.delete(model);
        }
      });
      document.getElementById('selectedModelsCount').textContent = selectedModelsForAdd.size;
      // 重新渲染以更新复选框状态
      const searchText = document.getElementById('modelSearchInput')?.value || '';
      renderAvailableModels(searchText);
    }

    /**
     * 确认添加选中的模型
     */
    function confirmModelSelection() {
      if (selectedModelsForAdd.size === 0) {
        window.showNotification(t('tokens.msg.selectAtLeastOne'), 'warning');
        return;
      }

      const addedCount = selectedModelsForAdd.size;
      // 添加到模型限制列表
      selectedModelsForAdd.forEach(model => {
        if (!editAllowedModels.includes(model)) {
          editAllowedModels.push(model);
        }
      });

      // 排序
      editAllowedModels.sort();

      closeModelSelectModal();
      renderAllowedModelsTable();
      window.showNotification(t('tokens.msg.modelsAdded', { count: addedCount }), 'success');
    }

    // ==================== 模型手动输入 ====================

    /**
     * 解析模型输入，支持逗号和换行分隔
     */
    function parseModelInput(input) {
      return input
        .split(/[,\n]/)
        .map(m => m.trim())
        .filter(m => m);
    }

    // 去重并排除已在 editAllowedModels 中的模型（大小写不敏感比对、保留原大小写写入）
    function newImportableModels(models) {
      const existingModels = new Set(editAllowedModels.map(m => m.toLowerCase()));
      return [...new Set(models)].filter(m => !existingModels.has(m.toLowerCase()));
    }

    /**
     * 显示模型导入对话框
     */
    function showModelImportModal() {
      document.getElementById('tokenModelImportTextarea').value = '';
      document.getElementById('tokenModelImportPreview').style.display = 'none';
      Modal.open(document.getElementById('modelImportModal'), { focus: '#tokenModelImportTextarea' });
    }

    /**
     * 关闭模型导入对话框
     */
    function closeModelImportModal() {
      Modal.close(document.getElementById('modelImportModal'));
    }

    /**
     * 更新模型导入预览
     */
    function updateModelImportPreview() {
      const textarea = document.getElementById('tokenModelImportTextarea');
      const preview = document.getElementById('tokenModelImportPreview');
      const countSpan = document.getElementById('tokenModelImportCount');
      const input = textarea.value.trim();

      if (!input) {
        preview.style.display = 'none';
        return;
      }

      const models = parseModelInput(input);
      const newModels = newImportableModels(models);

      if (newModels.length > 0) {
        countSpan.textContent = newModels.length;
        preview.style.display = 'block';
      } else {
        preview.style.display = 'none';
      }
    }

    /**
     * 确认模型导入
     */
    function confirmModelImport() {
      
      const textarea = document.getElementById('tokenModelImportTextarea');
      const input = textarea.value.trim();

      if (!input) {
        window.showNotification(t('tokens.msg.enterModelName'), 'warning');
        return;
      }

      const models = parseModelInput(input);
      if (models.length === 0) {
        window.showNotification(t('tokens.msg.noValidModel'), 'warning');
        return;
      }

      const newModels = newImportableModels(models);

      if (newModels.length === 0) {
        window.showNotification(t('tokens.msg.allModelsExist'), 'info');
        closeModelImportModal();
        return;
      }

      // 添加新模型
      newModels.forEach(model => editAllowedModels.push(model));
      editAllowedModels.sort();

      closeModelImportModal();
      renderAllowedModelsTable();

      const duplicateCount = models.length - newModels.length;
      const msg = duplicateCount > 0
        ? t('tokens.msg.importSuccessWithDuplicates', { added: newModels.length, duplicates: duplicateCount })
        : t('tokens.msg.importSuccess', { count: newModels.length });
      window.showNotification(msg, 'success');
    }
