(function () {
  function joinClasses(...classes) {
    return classes.filter(Boolean).join(' ');
  }

  function buildFilterGroup(content, extraClass = '') {
    return `<div class="${joinClasses('filter-group', extraClass)}">${content}</div>`;
  }

  function buildFilterLabel(forId, i18nKey, text) {
    return `<label for="${forId}" class="filter-label" data-i18n="${i18nKey}">${text}</label>`;
  }

  function buildSelect(id, optionsHtml = '', extraClass = '') {
    return `<select id="${id}" class="${joinClasses('filter-select', extraClass)}">${optionsHtml}</select>`;
  }

  function buildInput(type, id, placeholderKey, placeholder, extraClass = '') {
    return `<input type="${type}" id="${id}" class="${joinClasses('filter-input', extraClass)}" data-i18n-placeholder="${placeholderKey}" placeholder="${placeholder}">`;
  }

  function buildSharedFields(config) {
    const groupClass = config.groupClass || '';
    const checkboxGroupClass = config.checkboxGroupClass || groupClass;
    const timeRangeGroupClass = joinClasses(groupClass, config.timeRangeGroupClass);
    const timeRangeControlClass = joinClasses('filter-control--compact', 'filter-control--time-range', config.timeRangeControlClass);
    const authTokenGroupClass = joinClasses(groupClass, 'filter-group--auth-token', config.authTokenGroupClass);
    const authTokenControlClass = joinClasses('filter-control--wide', config.authTokenControlClass);
    const hideZeroSuccess = `<div class="${joinClasses('filter-group', 'filter-group--checkbox', checkboxGroupClass)}">
              <label class="filter-checkbox-label">
                <input type="checkbox" id="f_hide_zero_success" checked>
                <span data-i18n="stats.hideZeroSuccess">隐藏0成功</span>
              </label>
            </div>`;
    const filterButtonControl = '<button id="btn_filter" type="button" class="btn btn-primary filter-btn" data-i18n="common.filter">筛选</button>';
    const clearButtonControl = '<button id="btn_clear_filters" type="button" class="btn btn-secondary filter-btn" data-i18n="common.clear">清空</button>';
    const filterButton = `<div class="${joinClasses('filter-actions', 'filter-actions--page', config.actionsClass)}">
              ${clearButtonControl}
              ${filterButtonControl}
            </div>`;
    return {
      timeRange: buildFilterGroup(
        `${buildFilterLabel('f_hours', 'stats.timeRange', '时间范围')}
        <div id="f_hours_custom_range_host" class="filter-custom-range-host">
          ${buildSelect('f_hours', '\n                <!-- 动态生成选项 by date-range-selector.js -->\n              ', timeRangeControlClass)}
        </div>`,
        timeRangeGroupClass
      ),
      modelText: buildFilterGroup(
        `${buildFilterLabel('f_model', 'common.model', '模型')}
        ${buildInput('text', 'f_model', 'stats.containsTextPlaceholder', '包含文本...')}`,
        groupClass
      ),
      modelSelect: buildFilterGroup(
        `${buildFilterLabel('f_model', 'common.model', '模型')}
        ${buildSelect('f_model', '\n                <option value="" data-i18n="trend.allModels">全部模型</option>\n                <!-- 动态加载模型列表 -->\n              ', 'filter-control--wide')}`,
        groupClass
      ),
      api: buildFilterGroup(
        `${buildFilterLabel('f_api', 'stats.api', '入口')}
        ${buildSelect('f_api', `
                <option value="" data-i18n="stats.allApis">全部入口</option>
                <option value="anthropic">/v1/messages</option>
                <option value="openai-chat">/v1/chat/completions</option>
                <option value="openai-responses">/v1/responses</option>
                <option value="responses-ws">/v1/responses (WS)</option>
              `, 'filter-control--compact')}`,
        groupClass
      ),
      modelCombobox: buildFilterGroup(
        `${buildFilterLabel('f_model', 'common.model', '模型')}
        <div class="filter-combobox-wrapper filter-control--wide filter-control--model">
          <input id="f_model" class="filter-select filter-combobox" type="text" autocomplete="off" spellcheck="false" />
          <div id="f_model_dropdown" class="filter-dropdown" role="listbox"></div>
        </div>`,
        joinClasses(groupClass, 'filter-group--model')
      ),
      authToken: buildFilterGroup(
        `${buildFilterLabel('f_auth_token', 'stats.token', '令牌')}
        ${buildSelect('f_auth_token', '\n                <option value="" data-i18n="stats.allTokens">全部令牌</option>\n                <!-- 动态加载令牌列表 -->\n              ', authTokenControlClass)}`,
        authTokenGroupClass
      ),
      status: buildFilterGroup(
        `${buildFilterLabel('f_status', 'logs.statusCode', '状态码')}
        <div class="filter-combobox-wrapper filter-control--narrow filter-control--status">
          <input id="f_status" class="filter-select filter-combobox" type="text" autocomplete="off" spellcheck="false" />
          <div id="f_status_dropdown" class="filter-dropdown" role="listbox"></div>
        </div>`,
        joinClasses(groupClass, 'filter-group--status')
      ),
      search: buildFilterGroup(
        `${buildFilterLabel('f_q', 'logs.search', '搜索')}
        ${buildInput('text', 'f_q', 'logs.searchPlaceholder', 'dir/模型/路径/请求ID...', 'filter-control--wide')}`,
        groupClass
      ),
      statusClass: buildFilterGroup(
        `${buildFilterLabel('f_status_class', 'logs.statusClass', '状态段')}
        ${buildSelect('f_status_class', `
                <option value="" data-i18n="logs.allStatusClasses">全部状态段</option>
                <option value="2xx">2xx</option>
                <option value="3xx">3xx</option>
                <option value="4xx">4xx</option>
                <option value="5xx">5xx</option>
              `, 'filter-control--compact')}`,
        groupClass
      ),
      result: buildFilterGroup(
        `${buildFilterLabel('f_result', 'logs.result', '结果')}
        ${buildSelect('f_result', `
                <option value="" data-i18n="logs.allResults">全部结果</option>
                <option value="completed" data-i18n="logs.resultCompleted">完成</option>
                <option value="failed" data-i18n="logs.resultFailed">失败</option>
                <option value="disconnected" data-i18n="logs.resultDisconnected">断连</option>
                <option value="aborted" data-i18n="logs.resultAborted">中断</option>
              `, 'filter-control--compact')}`,
        groupClass
      ),
      errorStage: buildFilterGroup(
        `${buildFilterLabel('f_error_stage', 'logs.errorStage', '失败阶段')}
        <div class="filter-combobox-wrapper filter-control--compact filter-control--error-stage">
          <input id="f_error_stage" class="filter-select filter-combobox" type="text" autocomplete="off" spellcheck="false" />
          <div id="f_error_stage_dropdown" class="filter-dropdown" role="listbox"></div>
        </div>`,
        joinClasses(groupClass, 'filter-group--error-stage')
      ),
      logSource: buildFilterGroup(
        `${buildFilterLabel('f_log_source', 'logs.logSource', '日志来源')}
        ${buildSelect('f_log_source', `
                <option value="proxy" data-i18n="logs.sourceProxy">请求日志</option>
                <option value="manual_test" data-i18n="logs.sourceManualTest">手动测试</option>
                <option value="all" data-i18n="logs.sourceAll">全部日志</option>
              `, 'filter-control--compact')}`,
        groupClass
      ),
      hideZeroSuccess,
      filterButton,
      logsSummary: `<div class="logs-filter-summary-row"><div class="${joinClasses('filter-actions', 'filter-actions--page', config.actionsClass)}">
              <button id="btn_col_settings" type="button" class="btn btn-secondary filter-btn" data-action="toggle-col-menu" data-i18n-title="logs.colSettings" data-i18n-aria-label="logs.colSettings" aria-label="列显隐设置" title="列显隐设置"><svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><line x1="9" y1="4" x2="9" y2="20"/><line x1="15" y1="4" x2="15" y2="20"/></svg><span data-i18n="logs.colVisibility">列显隐</span></button>
              <button id="btn_export_csv" type="button" class="btn btn-secondary filter-btn" data-i18n="logs.exportCsv">导出CSV</button>
              <button id="btn_export_json" type="button" class="btn btn-secondary filter-btn" data-i18n="logs.exportJson">导出JSON</button>
              ${clearButtonControl}
              ${filterButtonControl}
            </div></div>`,
      statsSummary: `<div class="stats-filter-summary-row">${hideZeroSuccess}<div class="${joinClasses('filter-actions', 'filter-actions--page', config.actionsClass)}">
              ${clearButtonControl}
              ${filterButtonControl}
            </div></div>`
    };
  }

  const LAYOUTS = {
    stats: {
      barClass: 'filter-bar stats-filter-bar mt-2',
      controlsClass: 'filter-controls stats-filter-controls',
      groupClass: 'stats-filter-group',
      checkboxGroupClass: 'stats-filter-group stats-filter-group--checkbox',
      actionsClass: 'stats-filter-actions',
      items: ['timeRange', 'api', 'modelCombobox', 'authToken', 'statsSummary']
    },
    logs: {
      barClass: 'filter-bar logs-filter-bar mt-2',
      controlsClass: 'filter-controls logs-filter-controls',
      groupClass: 'logs-filter-group',
      timeRangeGroupClass: 'logs-filter-group--range',
      timeRangeControlClass: 'logs-filter-control--range',
      authTokenGroupClass: 'logs-filter-group--token',
      authTokenControlClass: 'logs-filter-control--token',
      actionsClass: 'logs-filter-actions',
      // 三段分区：时间范围 / 过滤条件 / 行操作，视觉上各成一簇
      sections: [
        { cls: 'logs-filter-section logs-filter-section--range', items: ['timeRange'] },
        { cls: 'logs-filter-section logs-filter-section--filters', items: ['search', 'api', 'modelCombobox', 'status', 'statusClass', 'result', 'errorStage', 'logSource', 'authToken'] },
        { cls: 'logs-filter-section logs-filter-section--actions', items: ['logsSummary'] }
      ]
    },
    trend: {
      barClass: 'filter-bar mt-2',
      controlsClass: 'filter-controls trend-filter-controls',
      groupClass: '',
      actionsClass: '',
      items: ['timeRange', 'api', 'modelSelect', 'authToken', 'filterButton']
    }
  };

  function renderLayout(layoutName) {
    const config = LAYOUTS[layoutName];
    if (!config) {
      console.error(`[PageFilters] Unknown layout: ${layoutName}`);
      return '';
    }

    const fields = buildSharedFields(config);
    const renderItems = (items) => items
      .map((item) => fields[item] || '')
      .filter(Boolean)
      .join('\n');
    const content = config.sections
      ? config.sections.map((section) => `<div class="${section.cls}">${renderItems(section.items)}</div>`).join('\n')
      : renderItems(config.items);

    return `<div class="${config.barClass}">
          <div class="${config.controlsClass}">
            ${content}
          </div>
        </div>`;
  }

  function initPageFilters(root = document) {
    if (!root || typeof root.querySelectorAll !== 'function') return;

    root.querySelectorAll('[data-page-filters]').forEach((container) => {
      const layoutName = container.getAttribute('data-page-filters');
      if (!layoutName) return;
      container.innerHTML = renderLayout(layoutName);
    });
  }

  window.PageFilters = {
    renderLayout,
    initPageFilters
  };

  initPageFilters();
})();
