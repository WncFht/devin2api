    // 常量定义
    const t = window.t;
    const STATS_TABLE_COLUMNS = 12; // 统计表列数

    let statsData = null;
    let rpmStats = null; // 全局RPM统计（峰值、平均、最近一分钟）
    let isToday = true;  // 是否为本日（本日才显示最近一分钟）
    let durationSeconds = 0; // 时间跨度（秒），用于计算RPM
    let currentStatsCustomTimeRange = null;
    let authTokens = []; // 令牌列表
    let hideZeroSuccess = true; // 是否隐藏0成功的模型（默认开启）
    let statsModelOptions = []; // 从统计数据中提取的模型列表
    let statsModelCombobox = null; // 模型筛选组合框实例
    let usageData = null;      // /admin/usage 规范化快照；null=未拉到
    let usageDisabled = false; // 快照 disabled 标记（调试日志未启用）
    let statsExactModelValue = '';
    let sortState = {
      column: null,
      order: null // null, 'asc', 'desc'
    };

    function normalizeStatsFilterValue(value) {
      return String(value || '').trim().toLowerCase();
    }

    function statsFilterMatchesOption(value, options) {
      const normalizedValue = normalizeStatsFilterValue(value);
      if (!normalizedValue) return false;

      return (Array.isArray(options) ? options : []).some((option) => {
        const candidates = option && typeof option === 'object'
          ? [option.value, option.label]
          : [option];
        return candidates.some((candidate) => normalizeStatsFilterValue(candidate) === normalizedValue);
      });
    }

    function statsFilterMatchesExactValue(value, exactValue) {
      const normalizedValue = normalizeStatsFilterValue(value);
      return Boolean(normalizedValue) && normalizedValue === normalizeStatsFilterValue(exactValue);
    }

    function isExactStatsModelFilter(value) {
      return statsFilterMatchesOption(value, statsModelOptions) ||
        statsFilterMatchesExactValue(value, statsExactModelValue);
    }

    function getStatsModelFilterKey(value, values) {
      return (values && values.modelExact) || isExactStatsModelFilter(value) ? 'model' : 'model_like';
    }

    function rememberExactStatsFilters(filters = {}, urlParams = null) {
      const hasExactModel = urlParams
        ? urlParams.has('model')
        : filters.modelExact === true;

      statsExactModelValue = hasExactModel ? (filters.model || '') : '';
    }

    function normalizeStatsCustomTimeRange(range) {
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

    function appendStatsTimeRangeParams(params, filters) {
      const range = filters?.range || 'today';
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(range, currentStatsCustomTimeRange)
        : `range=${encodeURIComponent(range)}`;
      new URLSearchParams(query).forEach((value, key) => {
        params.set(key, value);
      });
      return params;
    }

    function buildStatsLogLinkParams(baseParams = {}) {
      const params = new URLSearchParams();
      Object.entries(baseParams).forEach(([key, value]) => {
        if (value !== undefined && value !== null && value !== '') {
          params.set(key, value);
        }
      });
      return appendStatsTimeRangeParams(params, getStatsFilters());
    }

    async function loadStats() {
      try {
        renderStatsLoading();

        const params = buildStatsRequestParams();
        // 用量观测是独立失败域：端点未部署/未启用只影响观测卡，不拖垮统计表
        const usagePromise = loadUsageObserv();
        // 后端返回格式: {"success":true,"data":{"stats":[...],"duration_seconds":...,"rpm_stats":{...},"is_today":...}}
        statsData = (await fetchDataWithAuth('/dashboard/stats?' + params.toString())) || { stats: [] };
        durationSeconds = statsData.duration_seconds || 1; // 防止除零
        rpmStats = statsData.rpm_stats || null;
        isToday = statsData.is_today !== false;
        populateStatsComboboxOptions();

        // 初始化时应用默认排序（模型名称）
        applyDefaultSorting();

        renderStatsTable();
        updateRpmHeader(); // 更新表头标题

        // 如果当前是图表视图，同步更新图表
        if (currentView === 'chart') {
          renderCharts();
        }

        await usagePromise;

      } catch (error) {
        console.error('Failed to load stats:', error);
        if (window.showError) try { window.showError(t('stats.noData')); } catch(_){}
        renderStatsError();
      }
    }

    function renderStatsLoading() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = '';
      const row = TemplateEngine.render('tpl-stats-loading', { colspan: STATS_TABLE_COLUMNS });
      if (row) tbody.appendChild(row);
    }

    function renderStatsError() {
      const tbody = document.getElementById('stats_tbody');
      tbody.innerHTML = '';
      const row = TemplateEngine.render('tpl-stats-error', { colspan: STATS_TABLE_COLUMNS });
      if (row) tbody.appendChild(row);
    }

    // 表格排序功能
    function sortTable(column) {
      if (!statsData || !statsData.stats || statsData.stats.length === 0) return;
      
      // 确定排序状态：null -> desc -> asc -> null (三态循环)
      let newOrder;
      if (sortState.column !== column) {
        // 切换到新列，从desc开始
        newOrder = 'desc';
      } else {
        // 同一列循环：null -> desc -> asc -> null
        if (sortState.order === null) {
          newOrder = 'desc';
        } else if (sortState.order === 'desc') {
          newOrder = 'asc';
        } else {
          newOrder = null;
        }
      }
      
      // 更新排序状态
      sortState.column = newOrder ? column : null;
      sortState.order = newOrder;
      
      // 更新表头样式
      updateSortHeaders();
      
      // 执行排序并重新渲染
      applySorting();
      renderStatsTable();
    }

    function updateSortHeaders() {
      // 清除所有列的排序样式
      document.querySelectorAll('.sortable').forEach(th => {
        th.classList.remove('sorted');
        th.removeAttribute('data-sort-order');
      });
      
      // 如果有排序状态，设置当前列的样式
      if (sortState.column && sortState.order) {
        const currentHeader = document.querySelector(`[data-column="${sortState.column}"]`);
        if (currentHeader) {
          currentHeader.classList.add('sorted');
          currentHeader.setAttribute('data-sort-order', sortState.order);
        }
      }
    }

    function applySorting() {
      // 如果没有排序状态，从原始数据恢复默认排序（模型名称）
      if (!sortState.column || !sortState.order) {
        if (statsData && statsData.originalStats) {
          statsData.stats = [...statsData.originalStats];
        }
        return;
      }

      // 保存原始数据（如果还没有保存）
      if (!statsData.originalStats) {
        statsData.originalStats = [...statsData.stats];
      }

      const column = sortState.column;
      const isAsc = sortState.order === 'asc';

      statsData.stats.sort((a, b) => {
        let valueA, valueB;

        switch (column) {
          case 'model':
            valueA = (a.model || '').toLowerCase();
            valueB = (b.model || '').toLowerCase();
            break;
          case 'success':
            valueA = a.success || 0;
            valueB = b.success || 0;
            break;
          case 'error':
            valueA = a.error || 0;
            valueB = b.error || 0;
            break;
          case 'rpm':
            // 使用后端计算的峰值RPM排序
            valueA = a.peak_rpm || 0;
            valueB = b.peak_rpm || 0;
            break;
          case 'success_rate':
            valueA = a.total > 0 ? (a.success / a.total) : 0;
            valueB = b.total > 0 ? (b.success / b.total) : 0;
            break;
          case 'avg_first_byte_time':
            // 优先按平均耗时排序，其次按平均首字时间
            valueA = a.avg_duration_seconds || a.avg_first_byte_time_seconds || 0;
            valueB = b.avg_duration_seconds || b.avg_first_byte_time_seconds || 0;
            break;
          case 'avg_speed':
            valueA = calculateAverageSpeed(a) || 0;
            valueB = calculateAverageSpeed(b) || 0;
            break;
          case 'total_input_tokens':
            valueA = a.total_input_tokens || 0;
            valueB = b.total_input_tokens || 0;
            break;
          case 'total_output_tokens':
            valueA = a.total_output_tokens || 0;
            valueB = b.total_output_tokens || 0;
            break;
          case 'total_cache_read':
            valueA = a.total_cache_read_input_tokens || 0;
            valueB = b.total_cache_read_input_tokens || 0;
            break;
          case 'total_cache_creation':
            valueA = a.total_cache_creation_input_tokens || 0;
            valueB = b.total_cache_creation_input_tokens || 0;
            break;
          case 'total_cost':
            valueA = a.total_cost || 0;
            valueB = b.total_cost || 0;
            break;
          default:
            return 0;
        }

        let result;
        if (typeof valueA === 'string') {
          result = valueA.localeCompare(valueB, 'zh-CN');
        } else {
          result = valueA - valueB;
        }

        return isAsc ? result : -result;
      });
    }

    function calculateAverageSpeed(entry) {
      const successCount = Number(entry?.success);
      if (!Number.isFinite(successCount) || successCount <= 0) {
        return null;
      }

      return calculateTokenSpeed(
        Number(entry?.total_output_tokens) / successCount,
        Number(entry?.avg_duration_seconds),
        Number(entry?.avg_first_byte_time_seconds)
      );
    }

    function buildStatsTimingValue(seconds, color) {
      return `<span class="stats-value-dynamic" style="--stats-accent:${color};">${seconds.toFixed(2)}</span>`;
    }

    function buildStatsTimingText(firstByteSeconds, durationSeconds) {
      const firstByte = Number(firstByteSeconds) || 0;
      const duration = Number(durationSeconds) || 0;
      const parts = [];

      if (firstByte > 0) {
        parts.push(buildStatsTimingValue(firstByte, window.getFirstByteTimingColor(firstByte)));
      }
      if (duration > 0) {
        if (parts.length > 0) parts.push('<span class="stats-timing-separator">/</span>');
        parts.push(buildStatsTimingValue(duration, window.getDurationTimingColor(duration)));
      }

      return parts.join('');
    }

    function buildCacheUtilRate(inputTokens, cacheReadTokens, cacheCreationTokens) {
      const i = Number(inputTokens) || 0;
      const r = Number(cacheReadTokens) || 0;
      const c = Number(cacheCreationTokens) || 0;
      const denom = i + r + c;
      if (denom <= 0 || r <= 0) return '';
      const pct = (r / denom) * 100;
      return `<span class="stats-value-success">${pct.toFixed(1)}%</span>`;
    }

    function buildStatsModelDisplay(entry) {
      if (!entry.model) {
        return `<span class="stats-value-muted">${t('stats.unknownModel')}</span>`;
      }

      const modelLink = `<a href="#" class="model-tag model-link" data-model="${escapeHtml(entry.model)}" title="${t('stats.viewLogsTitle')}">${escapeHtml(entry.model)}</a>`;
      return `<span class="stats-model-cell">${modelLink}</span>`;
    }

    function buildStatsCostDisplay(standardCost, effectiveCost) {
      return buildCostStackHtml(standardCost, effectiveCost, { tone: 'warning' });
    }

    function renderStatsTable() {
      const tbody = document.getElementById('stats_tbody');

      if (!statsData || !statsData.stats || statsData.stats.length === 0) {
        tbody.innerHTML = '';
        const emptyRow = TemplateEngine.render('tpl-stats-empty', { colspan: STATS_TABLE_COLUMNS });
        if (emptyRow) tbody.appendChild(emptyRow);
        return;
      }

      // 根据 hideZeroSuccess 过滤数据
      const filteredStats = hideZeroSuccess
        ? statsData.stats.filter(entry => (entry.success || 0) > 0)
        : statsData.stats;

      if (filteredStats.length === 0) {
        tbody.innerHTML = '';
        const emptyRow = TemplateEngine.render('tpl-stats-empty', { colspan: STATS_TABLE_COLUMNS });
        if (emptyRow) tbody.appendChild(emptyRow);
        return;
      }

      tbody.innerHTML = '';

      // 初始化合计变量
      let totalSuccess = 0;
      let totalError = 0;
      let totalRequests = 0;
      let totalInputTokens = 0;
      let totalOutputTokens = 0;
      let totalCacheRead = 0;
      let totalCacheCreation = 0;
      let totalCost = 0;
      let totalEffectiveCost = 0;
      // 首字/耗时按成功数加权
      let firstByteTimeWeighted = 0;
      let firstByteSuccessSum = 0;
      let durationWeighted = 0;
      let durationSuccessSum = 0;
      // Tok/s 用 总输出 / 总生成秒数
      let speedOutputTokensSum = 0;
      let speedGenerationSecondsSum = 0;

      const fragment = document.createDocumentFragment();

      for (const entry of filteredStats) {
        const successRate = entry.total > 0 ? ((entry.success / entry.total) * 100) : 0;
        const successCountText = formatNumber(entry.success || 0);
        const errorCountText = formatNumber(entry.error || 0);
        const successRateText = formatSuccessRateText(successRate, entry.total || 0);

        // 使用后端返回的 RPM 数据（峰值/平均/最近）
        const rpmHtml = formatEntryRpm(entry, isToday);

        // 根据成功率设置颜色类
        const successRateClass = getSuccessRateClass(successRate);
        const successDisplay = buildSuccessDisplay(successCountText, successRateText, successRateClass);

        const modelDisplay = buildStatsModelDisplay(entry);

        // 格式化平均首字响应时间/平均耗时
        const avgFirstByteTime = entry.avg_first_byte_time_seconds || 0;
        const avgDuration = entry.avg_duration_seconds || 0;
        const avgTimeText = buildStatsTimingText(avgFirstByteTime, avgDuration);

        const avgSpeed = calculateAverageSpeed(entry);
        const avgSpeedText = avgSpeed === null
          ? ''
          : `<span class="stats-value-dynamic" style="--stats-accent:var(--neutral-700);">${avgSpeed >= 100 ? avgSpeed.toFixed(0) : avgSpeed.toFixed(1)}</span>`;

        // 格式化Token数据
        const inputTokensText = entry.total_input_tokens ? formatNumber(entry.total_input_tokens) : '';
        const outputTokensText = entry.total_output_tokens ? formatNumber(entry.total_output_tokens) : '';
        const cacheReadTokensText = entry.total_cache_read_input_tokens ?
          `<span class="stats-value-success">${formatNumber(entry.total_cache_read_input_tokens)}</span>` : '';
        const cacheCreationTokensText = entry.total_cache_creation_input_tokens ?
          `<span class="stats-value-primary">${formatNumber(entry.total_cache_creation_input_tokens)}</span>` : '';
        const cacheUtilText = buildCacheUtilRate(
          entry.total_input_tokens,
          entry.total_cache_read_input_tokens,
          entry.total_cache_creation_input_tokens
        );
        const costText = buildStatsCostDisplay(entry.total_cost, entry.effective_cost);
        const timingCellClass = avgTimeText ? '' : 'mobile-empty-cell';
        const speedCellClass = avgSpeedText ? '' : 'mobile-empty-cell';
        const inputCellClass = inputTokensText ? '' : 'mobile-empty-cell';
        const outputCellClass = outputTokensText ? '' : 'mobile-empty-cell';
        const cacheReadCellClass = cacheReadTokensText ? '' : 'mobile-empty-cell';
        const cacheCreateCellClass = cacheCreationTokensText ? '' : 'mobile-empty-cell';
        const cacheUtilCellClass = cacheUtilText ? '' : 'mobile-empty-cell';
        const costCellClass = costText ? '' : 'mobile-empty-cell';

        // 构建健康状态指示器
        const healthIndicator = buildHealthIndicator(entry.health_timeline, successRate / 100);

        const row = TemplateEngine.render('tpl-stats-row', {
          healthIndicator: healthIndicator,
          modelDisplay: modelDisplay,
          successDisplay: successDisplay,
          errorCount: errorCountText,
          rpm: rpmHtml,
          avgFirstByteTime: avgTimeText,
          timingCellClass: timingCellClass,
          avgSpeed: avgSpeedText,
          speedCellClass: speedCellClass,
          inputTokens: inputTokensText,
          inputCellClass: inputCellClass,
          outputTokens: outputTokensText,
          outputCellClass: outputCellClass,
          cacheReadTokens: cacheReadTokensText,
          cacheReadCellClass: cacheReadCellClass,
          cacheCreationTokens: cacheCreationTokensText,
          cacheCreateCellClass: cacheCreateCellClass,
          cacheUtilText: cacheUtilText,
          cacheUtilCellClass: cacheUtilCellClass,
          costText: costText,
          costCellClass: costCellClass,
          mobileLabelModel: t('common.model'),
          mobileLabelSuccess: t('common.success'),
          mobileLabelError: t('common.failed'),
          mobileLabelTiming: t('stats.avgFirstByte'),
          mobileLabelSpeed: t('stats.avgSpeed'),
          mobileLabelRpm: t('stats.rpm'),
          mobileLabelInput: t('stats.inputTokens'),
          mobileLabelOutput: t('stats.outputTokens'),
          mobileLabelCacheRead: t('stats.cacheRead'),
          mobileLabelCacheCreate: t('stats.cacheCreation'),
          mobileLabelCacheUtil: t('stats.cacheUtil'),
          mobileLabelCost: t('stats.costUsd')
        });
        if (row) fragment.appendChild(row);

        // 累加合计数据
        totalSuccess += entry.success || 0;
        totalError += entry.error || 0;
        totalRequests += entry.total || 0;
        totalInputTokens += entry.total_input_tokens || 0;
        totalOutputTokens += entry.total_output_tokens || 0;
        totalCacheRead += entry.total_cache_read_input_tokens || 0;
        totalCacheCreation += entry.total_cache_creation_input_tokens || 0;
        totalCost += entry.total_cost || 0;
        totalEffectiveCost += (entry.effective_cost !== undefined && entry.effective_cost !== null)
          ? Number(entry.effective_cost) || 0
          : (entry.total_cost || 0);

        const entrySuccess = Number(entry.success) || 0;
        if (entrySuccess > 0) {
          if (avgFirstByteTime > 0) {
            firstByteTimeWeighted += avgFirstByteTime * entrySuccess;
            firstByteSuccessSum += entrySuccess;
          }
          if (avgDuration > 0) {
            durationWeighted += avgDuration * entrySuccess;
            durationSuccessSum += entrySuccess;

            const entryOutput = Number(entry.total_output_tokens) || 0;
            if (entryOutput > 0) {
              let perReqGen = avgDuration;
              if (avgFirstByteTime > 0 && avgFirstByteTime < avgDuration) {
                const gen = avgDuration - avgFirstByteTime;
                if (gen >= 1) perReqGen = gen;
              }
              speedOutputTokensSum += entryOutput;
              speedGenerationSecondsSum += perReqGen * entrySuccess;
            }
          }
        }
      }

      tbody.appendChild(fragment);

      // 追加合计行（使用全局rpm_stats显示峰值/平均/最近）
      const totalSuccessRateVal = totalRequests > 0 ? (totalSuccess / totalRequests) * 100 : 0;
      const totalSuccessRate = formatSuccessRateText(totalSuccessRateVal, totalRequests);
      const totalSuccessDisplay = buildSuccessDisplay(
        formatNumber(totalSuccess),
        totalSuccessRate,
        getSuccessRateClass(totalSuccessRateVal)
      );

      // 使用全局rpm_stats格式化RPM
      const totalRpmHtml = formatGlobalRpm(rpmStats, isToday);

      // 合计行首字/耗时(秒)
      const totalAvgFirstByte = firstByteSuccessSum > 0 ? firstByteTimeWeighted / firstByteSuccessSum : 0;
      const totalAvgDuration = durationSuccessSum > 0 ? durationWeighted / durationSuccessSum : 0;
      const totalTimingText = buildStatsTimingText(totalAvgFirstByte, totalAvgDuration);

      // 合计行 Tok/s
      let totalSpeedText = '';
      if (speedOutputTokensSum > 0 && speedGenerationSecondsSum > 0) {
        const speed = speedOutputTokensSum / speedGenerationSecondsSum;
        totalSpeedText = `<span class="stats-value-dynamic" style="--stats-accent:var(--neutral-700);">${speed >= 100 ? speed.toFixed(0) : speed.toFixed(1)}</span>`;
      }

      const totalRow = TemplateEngine.render('tpl-stats-total', {
        totalLabel: t('stats.total'),
        successDisplay: totalSuccessDisplay,
        errorCount: formatNumber(totalError),
        rpm: totalRpmHtml,
        avgFirstByteTime: totalTimingText,
        timingCellClass: totalTimingText ? '' : 'mobile-empty-cell',
        avgSpeed: totalSpeedText,
        speedCellClass: totalSpeedText ? '' : 'mobile-empty-cell',
        inputTokens: formatNumber(totalInputTokens),
        outputTokens: formatNumber(totalOutputTokens),
        cacheReadTokens: formatNumber(totalCacheRead),
        cacheCreationTokens: formatNumber(totalCacheCreation),
        cacheUtilText: buildCacheUtilRate(totalInputTokens, totalCacheRead, totalCacheCreation),
        costText: buildStatsCostDisplay(totalCost, totalEffectiveCost),
        mobileLabelSummary: t('stats.total'),
        mobileLabelSuccess: t('common.success'),
        mobileLabelError: t('common.failed'),
        mobileLabelTiming: t('stats.avgFirstByte'),
        mobileLabelSpeed: t('stats.avgSpeed'),
        mobileLabelRpm: t('stats.rpm'),
        mobileLabelInput: t('stats.inputTokens'),
        mobileLabelOutput: t('stats.outputTokens'),
        mobileLabelCacheRead: t('stats.cacheRead'),
        mobileLabelCacheCreate: t('stats.cacheCreation'),
        mobileLabelCacheUtil: t('stats.cacheUtil'),
        mobileLabelCost: t('stats.costUsd')
      });
      if (totalRow) tbody.appendChild(totalRow);
    }

    function formatSuccessRateText(successRate, totalRequests) {
      if (!(totalRequests > 0)) return '';
      const text = successRate.toFixed(1) + '%';
      return text.endsWith('.0%') ? text.slice(0, -3) + '%' : text;
    }

    function getSuccessRateClass(successRate) {
      let successRateClass = 'success-rate';
      if (successRate >= 95) successRateClass += ' high';
      else if (successRate < 80) successRateClass += ' low';
      return successRateClass;
    }

    function buildSuccessDisplay(successCountText, successRateText, successRateClass) {
      if (!successRateText) {
        return `<span class="success-count">${successCountText}</span>`;
      }

      return `<span class="stats-success-inline"><span class="success-count">${successCountText}</span><span class="stats-success-separator">/</span><span class="${successRateClass}">${successRateText}</span></span>`;
    }

    function applyFilter() {
      window.persistFilterState({
        key: STATS_FILTER_KEY,
        values: getStatsFilters(),
        search: location.search,
        pathname: location.pathname,
        fields: STATS_FILTER_FIELDS,
        preserveExistingParams: true
      });
      loadStats();
    }

    function getDefaultStatsFilters() {
      if (window.FilterState && typeof window.FilterState.restore === 'function') {
        return window.FilterState.restore({
          search: '',
          savedFilters: null,
          fields: STATS_FILTER_FIELDS
        });
      }

      return STATS_FILTER_FIELDS.reduce((values, field) => {
        values[field.key] = Object.prototype.hasOwnProperty.call(field, 'defaultValue')
          ? field.defaultValue
          : '';
        return values;
      }, {});
    }

    function resetStatsFilters() {
      const defaults = getDefaultStatsFilters();

      currentStatsCustomTimeRange = null;
      hideZeroSuccess = true;
      rememberExactStatsFilters({
        ...defaults,
        modelExact: false
      });

      window.applyFilterControlValues(defaults, {
        range: 'f_hours',
        api: 'f_api',
        authToken: 'f_auth_token'
      });
      statsModelCombobox?.setValue('', t('trend.allModels'));

      const hideZeroCheckbox = document.getElementById('f_hide_zero_success');
      if (hideZeroCheckbox) hideZeroCheckbox.checked = true;

      window.persistFilterState({
        key: STATS_FILTER_KEY,
        values: getStatsFilters(),
        search: location.search,
        pathname: location.pathname,
        fields: STATS_FILTER_FIELDS,
        preserveExistingParams: true,
        historyMethod: 'replaceState'
      });
      loadStats();
    }

    function initStatsModelCombobox(initialValue) {
      statsModelCombobox = window.createSearchableCombobox({
        inputId: 'f_model',
        dropdownId: 'f_model_dropdown',
        attachMode: true,
        allowCustomInput: true,
        commitEmptyAsFirst: true,
        initialValue: initialValue || '',
        initialLabel: initialValue || t('trend.allModels'),
        getOptions: () => [
          { value: '', label: t('trend.allModels') },
          ...statsModelOptions.map(m => ({ value: m, label: m }))
        ],
        onSelect: () => {
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          applyFilter();
        }
      });
    }

    async function loadStatsFilterOptions() {
      try {
        const params = new URLSearchParams();
        appendStatsTimeRangeParams(params, getStatsFilters());
        const data = await fetchDataWithAuth('/dashboard/stats/filter-options?' + params.toString());
        if (data) {
          statsModelOptions = data.models || [];
          if (statsModelCombobox) {
            statsModelCombobox.refresh();
          }
        }
      } catch (error) {
        console.error('[Stats] 加载筛选选项失败:', error);
      }
    }

    function populateStatsComboboxOptions() {
      loadStatsFilterOptions();
    }

    function initFilters(restoredFilters) {
      const range = restoredFilters.range || 'today';
      const model = restoredFilters.model || '';
      const api = restoredFilters.api || '';
      const authToken = restoredFilters.authToken || '';

      window.initSavedDateRangeFilter({
        selectId: 'f_hours',
        defaultValue: 'today',
        restoredValue: range,
        includeCustom: true,
        customRange: currentStatsCustomTimeRange,
        customPickerContainerId: 'f_hours_custom_range_host',
        onChange: (nextRange, customRange) => {
          if (nextRange === 'custom') {
            currentStatsCustomTimeRange = normalizeStatsCustomTimeRange(customRange);
          }
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          loadStatsFilterOptions();
          applyFilter();
        }
      });

      initStatsModelCombobox(model);

      const apiSelect = document.getElementById('f_api');
      if (apiSelect) {
        apiSelect.value = api;
        apiSelect.addEventListener('change', applyFilter);
      }

      window.initAuthTokenFilter({
        selectId: 'f_auth_token',
        value: authToken,
        loadOptions: { tokenPrefix: t('stats.tokenPrefix') },
        onChange: () => {
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          applyFilter();
        }
      }).then((tokens) => {
        authTokens = tokens;
      });

      // 事件监听
      document.getElementById('btn_filter').addEventListener('click', applyFilter);
      document.getElementById('btn_clear_filters')?.addEventListener('click', resetStatsFilters);

      window.bindFilterApplyInputs({
        apply: applyFilter,
        debounceInputIds: [],
        enterInputIds: ['f_hours', 'f_api', 'f_auth_token']
      });
    }

    // 根据是否本日更新RPM表头标题
    function updateRpmHeader() {
      const rpmHeader = document.querySelector('[data-column="rpm"]');
      if (rpmHeader) {
        const span = rpmHeader.querySelector('span[data-i18n]');
        if (span) {
          const key = isToday ? 'stats.rpm' : 'stats.rpmNoRecent';
          const titleKey = isToday ? 'stats.rpmTitle' : 'stats.rpmNoRecentTitle';
          span.textContent = t(key);
          span.setAttribute('data-i18n', key);
          rpmHeader.title = t(titleKey);
          rpmHeader.setAttribute('data-i18n-title', titleKey);
        }
      }
    }

    // 应用默认排序:按模型名称升序
    // 如果用户已选择自定义排序，则保持用户的排序
    function applyDefaultSorting() {
      if (!statsData || !statsData.stats || statsData.stats.length === 0) return;

      // 保存原始数据副本(仅首次)
      if (!statsData.originalStats) {
        statsData.originalStats = [...statsData.stats];
      }

      // 如果用户已选择自定义排序，应用用户的排序而非默认排序
      if (sortState.column && sortState.order) {
        applySorting();
        updateSortHeaders();
        return;
      }

      statsData.stats.sort((a, b) =>
        (a.model || '').toLowerCase().localeCompare((b.model || '').toLowerCase(), 'zh-CN'));
    }

    // 渲染令牌选择器（支持语言切换时重新渲染）
    function renderTokenSelect() {
      const tokenSelect = document.getElementById('f_auth_token');
      if (!tokenSelect) return;

      const currentValue = tokenSelect.value;
      tokenSelect.innerHTML = `<option value="">${t('stats.allTokens')}</option>`;
      authTokens.forEach(token => {
        const option = document.createElement('option');
        option.value = token.id;
        option.textContent = token.description || `${t('stats.tokenPrefix')}${token.id}`;
        tokenSelect.appendChild(option);
      });
      // 恢复之前的选择
      if (currentValue) {
        tokenSelect.value = currentValue;
      }
    }

    // 格式化 RPM（每分钟请求数）带颜色
    function formatRpm(rpm) {
      if (rpm < 0.01) return '';
      const color = getRpmColor(rpm);
      const text = rpm >= 1000 ? (rpm / 1000).toFixed(1) + 'K' : rpm >= 1 ? rpm.toFixed(1) : rpm.toFixed(2);
      return `<span class="stats-rpm-value" style="--stats-rpm-color:${color};">${text}</span>`;
    }

    // 格式化全局RPM（峰值/平均/最近），固定格式，0显示为-
    function formatGlobalRpm(stats, showRecent) {
      if (!stats) return '-/-' + (showRecent ? '/-' : '');

      const formatVal = (v) => {
        const text = (v || 0).toFixed(1);
        return text === '0.0' ? '-' : text;
      };
      const peakText = formatVal(stats.peak_rpm);
      const avgText = formatVal(stats.avg_rpm);

      const parts = [
        {
          text: peakText,
          color: peakText !== '-' ? getRpmColor(stats.peak_rpm) : 'inherit'
        },
        {
          text: avgText,
          color: avgText !== '-' ? getRpmColor(stats.avg_rpm) : 'inherit'
        }
      ];

      if (showRecent) {
        const recentText = formatVal(stats.recent_rpm);
        parts.push({
          text: recentText,
          color: recentText !== '-' ? getRpmColor(stats.recent_rpm) : 'inherit'
        });
      }

      return buildCompactRpmDisplay(parts);
    }

    // 格式化每行的RPM（峰值/平均/最近），固定格式，0显示为-
    function formatEntryRpm(entry, showRecent) {
      const formatVal = (v) => {
        const text = (v || 0).toFixed(1);
        return text === '0.0' ? '-' : text;
      };

      const peakText = formatVal(entry.peak_rpm);
      const avgText = formatVal(entry.avg_rpm);

      const parts = [
        {
          text: peakText,
          color: peakText !== '-' ? getRpmColor(entry.peak_rpm) : 'inherit'
        },
        {
          text: avgText,
          color: avgText !== '-' ? getRpmColor(entry.avg_rpm) : 'inherit'
        }
      ];

      if (showRecent) {
        const recentText = formatVal(entry.recent_rpm);
        parts.push({
          text: recentText,
          color: recentText !== '-' ? getRpmColor(entry.recent_rpm) : 'inherit'
        });
      }

      return buildCompactRpmDisplay(parts);
    }

    function buildCompactRpmDisplay(parts) {
      const html = parts.map((part, index) => {
        const separator = index === 0 ? '' : '<span class="stats-rpm-separator">/</span>';
        return `${separator}<span class="stats-rpm-value" style="--stats-rpm-color:${part.color};">${part.text}</span>`;
      }).join('');

      return `<span class="stats-rpm-inline">${html}</span>`;
    }

    // 构建健康状态指示器 HTML（固定48个方块 + 当前成功率）
    // 性能优化：使用快速时间格式化，避免 toLocaleString 开销
    function buildHealthIndicator(timeline, currentRate) {
      if (!timeline || timeline.length === 0) {
        // 无健康数据时不显示指示器
        return '';
      }

      const fixedBucketCount = 48;
      const normalizedTimeline = timeline.length >= fixedBucketCount
        ? timeline.slice(-fixedBucketCount)
        : [...Array(fixedBucketCount - timeline.length).fill(null), ...timeline];
      const blocks = new Array(fixedBucketCount);

      for (let i = 0; i < fixedBucketCount; i++) {
        const point = normalizedTimeline[i];
        if (!point || point.rate < 0) {
          blocks[i] = `<span class="health-block unknown" title="${t('stats.healthNoData')}"></span>`;
          continue;
        }

        const rate = point.rate;
        const rateLimited = point.rate_limited || 0;
        const realErrors = (point.error || 0) - rateLimited;

        // 配色：所有失败都是限流 → 蓝色；有真实错误 → 按成功率分级(绿/橙/红)
        const className = (realErrors === 0 && rateLimited > 0)
          ? 'rate-limited'
          : rate >= 0.95 ? 'healthy' : rate >= 0.80 ? 'warning' : 'critical';

        // 快速时间格式化（避免 toLocaleString 的性能开销）
        const d = new Date(point.ts);
        const timeStr = `${String(d.getMonth() + 1).padStart(2, '0')}/${String(d.getDate()).padStart(2, '0')} ${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;

        // 构建 tooltip - 使用条件拼接减少数组操作
        let title = `${timeStr}
${t('stats.tooltipSuccess')}: ${point.success || 0} / ${t('stats.tooltipFailed')}: ${point.error || 0}`;
        if (rateLimited > 0) title += ` (${t('stats.tooltipRateLimited')}: ${rateLimited})`;
        if (point.avg_first_byte_time > 0) title += `
${t('stats.tooltipTTFT')}: ${point.avg_first_byte_time.toFixed(2)}s`;
        if (point.avg_duration > 0) title += `
${t('stats.tooltipDuration')}: ${point.avg_duration.toFixed(2)}s`;
        if (point.input_tokens > 0) title += `
${t('stats.tooltipInput')}: ${formatNumber(point.input_tokens)}`;
        if (point.output_tokens > 0) title += `
${t('stats.tooltipOutput')}: ${formatNumber(point.output_tokens)}`;
        if (point.cache_read_tokens > 0) title += `
${t('stats.tooltipCacheRead')}: ${formatNumber(point.cache_read_tokens)}`;
        if (point.cache_creation_tokens > 0) title += `
${t('stats.tooltipCacheWrite')}: ${formatNumber(point.cache_creation_tokens)}`;
        if (point.cost > 0) title += `
${t('stats.tooltipCost')}: $${point.cost.toFixed(4)}`;

        blocks[i] = `<span class="health-block ${className}" title="${escapeHtml(title)}"></span>`;
      }

      // 构建完整 HTML - 成功率颜色：>=95%绿色, >=80%橙色, <80%红色
      const ratePercent = (currentRate * 100).toFixed(1);
      const rateColor = currentRate >= 0.95 ? 'var(--success-600)' :
                        currentRate >= 0.80 ? 'var(--warning-600)' : 'var(--error-600)';
      return `<div class="health-indicator"><span class="health-track">${blocks.join('')}</span><span class="health-rate" style="--health-rate-color:${rateColor};">${ratePercent}%</span></div>`;
    }

    // 注销功能（已由 ui.js 的 onLogout 统一处理）

    // localStorage key for stats page filters
    const STATS_FILTER_KEY = 'stats.filters';
    const STATS_FILTER_FIELDS = [
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
        paramKey: getStatsModelFilterKey,
        requestKey: getStatsModelFilterKey,
        defaultValue: ''
      },
      { key: 'authToken', queryKeys: ['auth_token_id'], defaultValue: '' },
    ];

    function getStatsFilters() {
      const model = statsModelCombobox ? statsModelCombobox.getValue() : '';
      const baseValues = window.readFilterControlValues({
        range: { id: 'f_hours', defaultValue: 'today', trim: true },
        api: { id: 'f_api', trim: true },
        authToken: { id: 'f_auth_token', trim: true }
      });
      const hasCustomRange = baseValues.range === 'custom' && currentStatsCustomTimeRange;
      return {
        ...baseValues,
        customStartTime: hasCustomRange ? String(currentStatsCustomTimeRange.startMs) : '',
        customEndTime: hasCustomRange ? String(currentStatsCustomTimeRange.endMs) : '',
        model,
        modelExact: isExactStatsModelFilter(model),
        hideZeroSuccess: hideZeroSuccess
      };
    }

    function buildStatsRequestParams() {
      const params = window.FilterQuery.buildRequestParams(getStatsFilters(), STATS_FILTER_FIELDS);
      appendStatsTimeRangeParams(params, getStatsFilters());
      return params;
    }

    function bindStatsStaticControls() {
      const viewToggleGroup = document.getElementById('view-toggle-group');
      if (viewToggleGroup && !viewToggleGroup.dataset.bound) {
        viewToggleGroup.addEventListener('click', (e) => {
          const viewBtn = e.target.closest('.view-toggle-btn[data-view]');
          if (!viewBtn) return;

          switchView(viewBtn.dataset.view);
        });
        viewToggleGroup.dataset.bound = '1';
      }

      const thead = document.querySelector('.stats-table thead');
      if (thead && !thead.dataset.bound) {
        thead.addEventListener('click', (e) => {
          const sortable = e.target.closest('.sortable[data-column]');
          if (!sortable) return;

          sortTable(sortable.dataset.column);
        });
        thead.dataset.bound = '1';
      }
    }

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'stats',
      run: async () => {
      bindStatsStaticControls();

      // 优先从 URL 读取，其次从 localStorage 恢复，默认 all
      const u = new URLSearchParams(location.search);
      const hasUrlParams = u.toString().length > 0;
      const savedFilters = window.FilterState.load(STATS_FILTER_KEY);
      const restoredFilters = window.FilterState.restore({
        search: location.search,
        savedFilters,
        fields: STATS_FILTER_FIELDS
      });
      currentStatsCustomTimeRange = restoredFilters.range === 'custom'
        ? normalizeStatsCustomTimeRange(restoredFilters)
        : null;
      if (restoredFilters.range === 'custom' && !currentStatsCustomTimeRange) {
        restoredFilters.range = 'today';
      }
      rememberExactStatsFilters({
        ...restoredFilters,
        modelExact: !hasUrlParams && savedFilters?.modelExact === true
      }, hasUrlParams ? u : null);
      // 恢复隐藏0成功选项状态（从 localStorage 读取，默认 true）
      hideZeroSuccess = savedFilters?.hideZeroSuccess !== false;
      const hideZeroCheckbox = document.getElementById('f_hide_zero_success');
      if (hideZeroCheckbox) {
        hideZeroCheckbox.checked = hideZeroSuccess;
        hideZeroCheckbox.addEventListener('change', (e) => {
          hideZeroSuccess = e.target.checked;
          window.persistFilterState({
            key: STATS_FILTER_KEY,
            getValues: getStatsFilters
          });
          renderStatsTable();
        });
      }

      initFilters(restoredFilters);

      if (!hasUrlParams && savedFilters) {
        window.persistFilterState({
          values: savedFilters,
          pathname: location.pathname,
          fields: STATS_FILTER_FIELDS,
          historyMethod: 'replaceState'
        });
      }

      await loadStats();
      restoreViewState();

      // 注册语言切换回调，重新渲染动态内容
      window.i18n.onLocaleChange(() => {
        renderTokenSelect();
        renderStatsTable();
        updateRpmHeader();
        renderUsageObserv();
        if (currentView === 'chart') {
          renderCharts();
        }
      });

      // 事件委托：处理统计表格中的模型名称点击（跳日志页按模型过滤）
      const statsTableBody = document.getElementById('stats_tbody');
      if (statsTableBody) {
        statsTableBody.addEventListener('click', (e) => {
          const modelLink = e.target.closest('.model-link[data-model]');
          if (modelLink) {
            e.preventDefault();
            const model = modelLink.dataset.model;
            if (model) {
              const params = buildStatsLogLinkParams({ model });
              window.location.href = `/web/logs.html?${params.toString()}`;
            }
            return;
          }
        });
      }

      // 自动刷新（system_settings.auto_refresh_interval_seconds，0=禁用）
      if (typeof window.createAutoRefresh === 'function') {
        window.createAutoRefresh({ load: loadStats }).init();
      }
      }
    });

    // ========== 服务观测（/admin/usage 聚合快照）==========
    // 快照各组时间维不同：SLA 归因卡按页面时间范围用 10 分钟桶（points，
    // 8 天）或日表（days，31 天）在客户端切片；延迟分位是定长蓄水池、
    // 错误阶段与上下文填充是窗口累计，均无范围维，卡片按窗口口径展示。
    const usageT = (key, fallback, params) => window.i18nText(key, fallback, params);

    const USAGE_TOTAL_FIELDS = [
      'requests', 'errors', 'disconnected', 'rate_limited',
      'client_faults', 'upstream_faults',
      'input_tokens', 'output_tokens', 'cache_read_tokens', 'cache_write_tokens',
      'reasoning_tokens', 'total_tokens', 'gen_ms', 'gen_tokens'
    ];

    function sumUsageTotals(list) {
      const totals = {};
      USAGE_TOTAL_FIELDS.forEach(k => { totals[k] = 0; });
      (list || []).forEach(p => USAGE_TOTAL_FIELDS.forEach(k => { totals[k] += Number(p[k]) || 0; }));
      return totals;
    }

    // 形状适配：老面板 {snapshot:{...}, models:[...], est_cost, ...}；
    // snapshot 键缺失时按平铺形状兜底。
    function normalizeUsage(data) {
      if (!data || typeof data !== 'object' || data.disabled) return null;
      const snap = (data.snapshot && typeof data.snapshot === 'object') ? data.snapshot : data;
      const models = Array.isArray(data.models) ? data.models
        : (Array.isArray(snap.models) ? snap.models : []);
      return {
        windowStart: snap.window_start || '',
        entries: Number(snap.entries) || 0,
        points: Array.isArray(snap.points) ? snap.points : [],
        days: Array.isArray(snap.days) ? snap.days : [],
        errorStages: (snap.error_stages && typeof snap.error_stages === 'object') ? snap.error_stages : {},
        duration: snap.duration || null,
        ttfb: snap.ttfb || null,
        models
      };
    }

    // 与后端 resolveRange 同口径（周一为一周起点、非法 custom 回落 today），
    // 返回 [since, until) unix 秒。
    function statsRangeSecs() {
      const now = Date.now();
      const dayMs = 86400000;
      const beginDay = ms => { const d = new Date(ms); d.setHours(0, 0, 0, 0); return d.getTime(); };
      const beginWeek = ms => { const s = beginDay(ms); return s - ((new Date(s).getDay() + 6) % 7) * dayMs; };
      const beginMonth = ms => { const d = new Date(ms); return new Date(d.getFullYear(), d.getMonth(), 1).getTime(); };
      const today = () => [beginDay(now) / 1000, now / 1000];
      const filters = getStatsFilters();
      switch (filters.range || 'today') {
        case 'yesterday': {
          const s = beginDay(now - dayMs);
          return [s / 1000, (s + dayMs) / 1000];
        }
        case 'day_before_yesterday': {
          const s = beginDay(now - 2 * dayMs);
          return [s / 1000, (s + dayMs) / 1000];
        }
        case 'this_week': return [beginWeek(now) / 1000, now / 1000];
        case 'last_week': {
          const s = beginWeek(now - 7 * dayMs);
          return [s / 1000, (s + 7 * dayMs) / 1000];
        }
        case 'this_month': return [beginMonth(now) / 1000, now / 1000];
        case 'last_month': {
          const d = new Date(now);
          return [new Date(d.getFullYear(), d.getMonth() - 1, 1).getTime() / 1000, beginMonth(now) / 1000];
        }
        case 'custom': {
          const s = Number(filters.customStartTime);
          const u = Number(filters.customEndTime);
          return (s > 0 && u > s) ? [s / 1000, Math.min(u, now) / 1000] : today();
        }
        default: return today();
      }
    }

    // 范围切片：≤8 天用 10 分钟桶，更长窗口用日表；细粒度窗外零命中
    // 时回落日表（自定义范围可能整段落在 points 覆盖之前）。
    function usageRangeTotals(u) {
      const [since, until] = statsRangeSecs();
      if (until - since <= 8 * 86400) {
        const t = sumUsageTotals(u.points.filter(p => p.at >= since && p.at < until));
        if (t.requests > 0) return t;
      }
      const days = new Set();
      const d = new Date(since * 1000);
      d.setHours(0, 0, 0, 0);
      while (d.getTime() < until * 1000) {
        days.add(d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0'));
        d.setDate(d.getDate() + 1);
      }
      return sumUsageTotals(u.days.filter(row => days.has(row.date)));
    }

    // SLA 口径成功率：分母剔除客户端责任与 429 后，服务端失分占比取反；
    // 分母为 0（只有客户端/限流流量）返回 null。
    function usageSlaRate(totals) {
      const base = (totals.requests || 0) - (totals.client_faults || 0) - (totals.rate_limited || 0);
      if (base <= 0) return null;
      return (base - (totals.upstream_faults || 0)) / base * 100;
    }

    function usageKpiCard(labelKey, fallback, valueHtml, sub, color) {
      return `<div class="runtime-metric-card">
        <span class="runtime-metric-label">${escapeHtml(usageT(labelKey, fallback))}</span>
        <strong class="runtime-metric-value"${color ? ` style="color:${color};"` : ''}>${valueHtml}</strong>
        ${sub ? `<span class="runtime-metric-sub">${sub}</span>` : ''}
      </div>`;
    }

    async function loadUsageObserv() {
      try {
        const data = await fetchDataWithAuth('/admin/usage');
        usageDisabled = Boolean(data && data.disabled);
        usageData = normalizeUsage(data);
      } catch (error) {
        // 保留上一份快照：自动刷新时单次失败不清空已渲染内容
        console.error('[Stats] 加载服务观测失败:', error);
      }
      try {
        renderUsageObserv();
      } catch (error) {
        console.error('[Stats] 渲染服务观测失败:', error);
      }
    }

    function renderUsageObserv() {
      const section = document.getElementById('usage-observ-section');
      if (!section) return;
      const blocks = ['usage-faults-block', 'usage-latency-block', 'usage-stages-block', 'usage-ctxfill-block'];
      const empty = document.getElementById('usage-observ-empty');
      const footnote = document.getElementById('usage-observ-footnote');
      const note = document.getElementById('usage-observ-note');

      // 拉取失败（端点未部署等）：整卡隐藏，页面维持旧观
      if (!usageData && !usageDisabled) {
        section.hidden = true;
        return;
      }
      section.hidden = false;
      if (note) {
        note.textContent = usageT('stats.usageScopeNote', '归因卡跟随时间范围 · 分位/阶段/填充为窗口累计');
      }

      if (usageDisabled || !usageData) {
        blocks.forEach(id => { const el = document.getElementById(id); if (el) el.hidden = true; });
        if (footnote) footnote.textContent = '';
        if (empty) {
          empty.hidden = false;
          empty.textContent = usageT('stats.usageDisabled', '调试日志未启用，无用量统计。');
        }
        return;
      }
      if (empty) empty.hidden = true;

      renderUsageFaults(usageRangeTotals(usageData));
      renderUsageLatency(usageData);
      renderUsageStages(usageData);
      renderUsageCtxFill(usageData);
      if (footnote) {
        footnote.textContent = usageT('stats.usageFootnote', '窗口起点 {start} · 聚合 {entries} 条请求', {
          start: usageData.windowStart || '-',
          entries: formatNumber(usageData.entries)
        });
      }
    }

    function renderUsageFaults(totals) {
      const block = document.getElementById('usage-faults-block');
      const grid = document.getElementById('usage-faults-grid');
      if (!block || !grid) return;
      const sla = usageSlaRate(totals);
      const slaColor = sla === null ? '' :
        sla >= 99 ? 'var(--success-600)' : sla >= 95 ? 'var(--warning-600)' : 'var(--error-600)';
      grid.innerHTML = [
        usageKpiCard('stats.usageSla', 'SLA 成功率',
          sla === null ? '—' : sla.toFixed(1) + '%',
          escapeHtml(usageT('stats.usageSlaSub', '剔除客户端责任与 429')),
          slaColor),
        usageKpiCard('stats.usageRequests', '请求数',
          formatNumber(totals.requests),
          escapeHtml(usageT('stats.usageRequestsSub', '失败 {errors} · 断连 {disconnected}', {
            errors: formatNumber(totals.errors),
            disconnected: formatNumber(totals.disconnected)
          }))),
        usageKpiCard('stats.usageUpstreamFaults', '服务端失分',
          formatNumber(totals.upstream_faults), '',
          totals.upstream_faults > 0 ? 'var(--error-600)' : ''),
        usageKpiCard('stats.usageClientFaults', '客户端责任',
          formatNumber(totals.client_faults), ''),
        usageKpiCard('stats.usageRateLimited', '限流 429',
          formatNumber(totals.rate_limited), '')
      ].join('');
      block.hidden = false;
    }

    // 分位行：样本为 0 的指标整行不渲染；阈值着色复用 timingColor 口径
    //（TTFB 5s/10s，耗时 30s/60s），与统计表首字/耗时列一致。
    function renderUsageLatency(u) {
      const block = document.getElementById('usage-latency-block');
      const tbody = document.getElementById('usage-latency-tbody');
      if (!block || !tbody) return;
      const row = (labelKey, fallback, st, colorFn) => {
        if (!st || !(Number(st.samples) > 0)) return '';
        const cells = [st.p50, st.p90, st.p95, st.p99, st.max].map(ms => {
          const sec = (Number(ms) || 0) / 1000;
          return `<td><span class="stats-value-dynamic" style="--stats-accent:${colorFn(sec)};">${sec.toFixed(2)}s</span></td>`;
        }).join('');
        return `<tr><td>${escapeHtml(usageT(labelKey, fallback))}</td><td>${formatNumber(st.samples)}</td>${cells}</tr>`;
      };
      const html = row('stats.usageLatencyTtfb', '上游首字', u.ttfb, getFirstByteTimingColor) +
        row('stats.usageLatencyDuration', '总耗时', u.duration, getDurationTimingColor);
      tbody.innerHTML = html;
      block.hidden = !html;
    }

    function renderUsageStages(u) {
      const block = document.getElementById('usage-stages-block');
      const tbody = document.getElementById('usage-stages-tbody');
      if (!block || !tbody) return;
      const stages = Object.entries(u.errorStages)
        .filter(([, n]) => Number(n) > 0)
        .sort((a, b) => b[1] - a[1]);
      if (!stages.length) {
        block.hidden = true;
        return;
      }
      const total = stages.reduce((s, [, n]) => s + Number(n), 0);
      tbody.innerHTML = stages.map(([stage, n]) => {
        const pct = total > 0 ? Number(n) / total * 100 : 0;
        return `<tr>
          <td><a class="model-link" href="/web/logs.html?error_stage=${encodeURIComponent(stage)}">${escapeHtml(stage)}</a></td>
          <td>${formatNumber(n)}</td>
          <td><div style="display:flex;align-items:center;gap:8px;">
            <div class="runtime-transcript-progress" style="flex:1;min-width:64px;">
              <span class="runtime-transcript-progress-bar runtime-transcript-progress-bar--exceeded" style="width:${pct.toFixed(1)}%"></span>
            </div>
            <span class="stats-value-dynamic">${pct.toFixed(1)}%</span>
          </div></td>
        </tr>`;
      }).join('');
      block.hidden = false;
    }

    // 上下文填充率 = 平均单请求占用（输入+两向缓存）÷ 模型窗口上限，
    // 衡量「窗口挤不挤」；按填充率降序取前 8 个模型。
    function renderUsageCtxFill(u) {
      const block = document.getElementById('usage-ctxfill-block');
      const tbody = document.getElementById('usage-ctxfill-tbody');
      if (!block || !tbody) return;
      const rows = u.models
        .filter(m => m && m.context_fill_pct != null)
        .sort((a, b) => b.context_fill_pct - a.context_fill_pct)
        .slice(0, 8);
      if (!rows.length) {
        block.hidden = true;
        return;
      }
      tbody.innerHTML = rows.map(m => {
        const pct = Number(m.context_fill_pct) || 0;
        const tone = pct < 50 ? 'normal' : pct < 80 ? 'warning' : 'exceeded';
        return `<tr>
          <td>${escapeHtml(m.name || usageT('stats.unknownModel', '未知模型'))}</td>
          <td><div style="display:flex;align-items:center;gap:8px;">
            <div class="runtime-transcript-progress" style="flex:1;min-width:64px;">
              <span class="runtime-transcript-progress-bar runtime-transcript-progress-bar--${tone}" style="width:${Math.min(100, pct).toFixed(1)}%"></span>
            </div>
            <span class="stats-value-dynamic">${pct.toFixed(0)}%</span>
          </div></td>
          <td>${formatNumber(Math.round(Number(m.avg_context_tokens) || 0))}/${formatNumber(m.context_tokens)}</td>
        </tr>`;
      }).join('');
      block.hidden = false;
    }

    // ========== 图表视图功能 ==========
    let currentView = 'table'; // 当前视图: 'table' | 'chart'
    let chartInstances = {}; // ECharts 实例缓存

    function getStatsChartTheme() {
      return typeof window.getChartTheme === 'function'
        ? window.getChartTheme()
        : {
          mutedText: '#6b7280',
          strongText: '#111827',
          tooltipBg: 'rgba(255, 255, 255, 0.98)',
          tooltipBorder: 'rgba(17, 24, 39, 0.16)',
          tooltipText: '#111827',
          axisLine: 'rgba(17, 24, 39, 0.16)',
          surface: '#ffffff'
        };
    }

    // 切换视图
    function switchView(view) {
      currentView = view;
      document.documentElement.classList.toggle('stats-view-init-chart', view === 'chart');

      // 持久化视图状态
      try {
        localStorage.setItem('stats.view', view);
      } catch (_) {}

      // 更新按钮状态
      document.querySelectorAll('.view-toggle-btn').forEach(btn => {
        btn.classList.toggle('active', btn.dataset.view === view);
      });

      // 切换显示
      const tableView = document.getElementById('stats-table-view');
      const chartView = document.getElementById('stats-chart-view');

      if (view === 'table') {
        tableView.style.display = 'block';
        chartView.style.display = 'none';
      } else {
        tableView.style.display = 'none';
        chartView.style.display = 'block';
        // 渲染图表
        renderCharts();
      }
    }

    // 恢复视图状态
    function restoreViewState() {
      try {
        const savedView = localStorage.getItem('stats.view');
        if (savedView === 'chart' || savedView === 'table') {
          // 只在需要切换时才调用 switchView，避免不必要的重绘
          if (savedView !== currentView) {
            switchView(savedView);
          }
        }
      } catch (_) {}
    }

    // 渲染所有饼图
    function renderCharts() {
      if (!statsData || !statsData.stats || statsData.stats.length === 0) {
        return;
      }

      // 聚合数据（只统计成功调用）
      const modelCallsMap = {}; // 模型 -> 成功调用次数
      const modelTokensMap = {}; // 模型 -> Token用量
      const modelCostMap = {}; // 模型 -> 成本（美元）

      for (const entry of statsData.stats) {
        const modelName = entry.model || t('stats.unknownModel');
        const successCount = entry.success || 0;
        const totalTokens = (entry.total_input_tokens || 0) + (entry.total_output_tokens || 0) + (entry.total_cache_read_input_tokens || 0) + (entry.total_cache_creation_input_tokens || 0);

        // 只统计成功调用
        if (successCount > 0) {
          // 模型调用次数
          modelCallsMap[modelName] = (modelCallsMap[modelName] || 0) + successCount;
          // 模型Token用量
          modelTokensMap[modelName] = (modelTokensMap[modelName] || 0) + totalTokens;
        }

        // 成本聚合（不依赖 successCount，因为成本可能来自失败请求的部分消耗）
        const cost = entry.total_cost || 0;
        const effectiveCost = (entry.effective_cost !== undefined && entry.effective_cost !== null)
          ? Number(entry.effective_cost) || 0
          : cost;
        if (cost > 0 || effectiveCost > 0) {
          if (!modelCostMap[modelName]) modelCostMap[modelName] = { standard: 0, effective: 0 };
          modelCostMap[modelName].standard += cost;
          modelCostMap[modelName].effective += effectiveCost;
        }
      }

      // 渲染3个模型饼图
      const unitTimes = t('stats.unitTimes');
      renderPieChart('chart-model-calls', modelCallsMap, unitTimes);
      renderPieChart('chart-model-tokens', modelTokensMap, '');
      renderPieChart('chart-model-cost', modelCostMap, '$');
    }

    // 渲染单个饼图
    function renderPieChart(containerId, dataMap, unit) {
      const container = document.getElementById(containerId);
      if (!container) return;

      // 获取或创建 ECharts 实例
      if (!chartInstances[containerId]) {
        chartInstances[containerId] = echarts.init(container);
      }
      const chart = chartInstances[containerId];
      const chartTheme = getStatsChartTheme();

      // 转换数据格式并排序（成本场景的值为 {standard, effective}，其他场景为数字）
      const data = Object.entries(dataMap)
        .map(([name, value]) => {
          if (value && typeof value === 'object') {
            const eff = Number(value.effective) || 0;
            const std = Number(value.standard) || 0;
            return { name, value: eff || std, standard: std };
          }
          return { name, value };
        })
        .sort((a, b) => b.value - a.value);

      // 如果没有数据，显示空状态
      if (data.length === 0) {
        chart.setOption({
          title: {
            text: t('stats.chartNoData'),
            left: 'center',
            top: 'center',
            textStyle: {
              color: chartTheme.mutedText,
              fontSize: 14
            }
          }
        });
        return;
      }

      // 颜色方案
      const colors = [
        '#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#8b5cf6',
        '#06b6d4', '#ec4899', '#84cc16', '#f97316', '#6366f1',
        '#14b8a6', '#a855f7', '#eab308', '#22c55e', '#0ea5e9'
      ];

      // 计算总值用于百分比
      const total = data.reduce((sum, item) => sum + item.value, 0);

      const option = {
        tooltip: {
          trigger: 'item',
          backgroundColor: chartTheme.tooltipBg,
          borderColor: chartTheme.tooltipBorder,
          textStyle: { color: chartTheme.tooltipText, fontSize: 12 },
          formatter: function(params) {
            const value = params.value;
            let formattedValue;
            // 成本特殊处理
            if (unit === '$') {
              const std = params.data && typeof params.data.standard === 'number' ? params.data.standard : value;
              formattedValue = formatCostPair(std, value);
              return `${params.name}<br/>${formattedValue} (${params.percent}%)`;
            }
            // 原有逻辑：大数值缩写
            if (value >= 1000000) {
              formattedValue = (value / 1000000).toFixed(2) + 'M';
            } else if (value >= 1000) {
              formattedValue = (value / 1000).toFixed(2) + 'K';
            } else {
              formattedValue = value.toLocaleString();
            }
            return `${params.name}<br/>${formattedValue}${unit} (${params.percent}%)`;
          }
        },
        legend: {
          type: 'scroll',
          orient: 'vertical',
          right: 10,
          top: 20,
          bottom: 20,
          textStyle: { fontSize: 11, color: chartTheme.mutedText },
          pageIconColor: chartTheme.mutedText,
          pageIconInactiveColor: chartTheme.axisLine,
          pageTextStyle: { color: chartTheme.mutedText },
          formatter: function(name) {
            const item = data.find(d => d.name === name);
            if (item && total > 0) {
              const percent = ((item.value / total) * 100).toFixed(1);
              return `${name} (${percent}%)`;
            }
            return name;
          }
        },
        color: colors,
        series: [{
          type: 'pie',
          radius: ['40%', '70%'],
          center: ['35%', '50%'],
          avoidLabelOverlap: true,
          itemStyle: {
            borderRadius: 4,
            borderColor: chartTheme.surface,
            borderWidth: 2
          },
          label: {
            show: false
          },
          emphasis: {
            label: {
              show: true,
              fontSize: 12,
              fontWeight: 'bold',
              formatter: function(params) {
                return params.percent.toFixed(1) + '%';
              }
            },
            itemStyle: {
              shadowBlur: 10,
              shadowOffsetX: 0,
              shadowColor: 'rgba(0, 0, 0, 0.3)'
            }
          },
          data: data
        }]
      };

      chart.setOption(option, true);
    }

    // 窗口大小变化时重新调整图表
    window.addEventListener('resize', function() {
      Object.values(chartInstances).forEach(chart => {
        if (chart) chart.resize();
      });
    });

    window.addEventListener('ccload:themechange', function() {
      if (currentView === 'chart') {
        renderCharts();
      }
    });
