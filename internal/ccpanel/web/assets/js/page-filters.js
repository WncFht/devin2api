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
        joinClasses(groupClass, 'filter-group--api')
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
        joinClasses(groupClass, 'filter-group--log-source')
      ),
      account: buildFilterGroup(
        `${buildFilterLabel('f_account', 'logs.colAccount', '账号')}
        <div class="filter-combobox-wrapper filter-control--compact filter-control--account">
          <input id="f_account" class="filter-select filter-combobox" type="text" autocomplete="off" spellcheck="false" />
          <div id="f_account_dropdown" class="filter-dropdown" role="listbox"></div>
        </div>`,
        joinClasses(groupClass, 'filter-group--account')
      ),
      hideZeroSuccess,
      filterButton,
      logsActions: `<div class="logs-filter-summary-row"><div class="${joinClasses('filter-actions', 'filter-actions--page', config.actionsClass)}">
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
      authTokenGroupClass: 'logs-filter-group--token',
      actionsClass: 'logs-filter-actions',
      // 单条 flex 流一行排布：先「哪些请求」（范围/入口/模型/令牌/来源）
      // 后「结果如何」（状态码/结果/失败阶段），清空+筛选靠右收尾；
      // 列显隐/导出不参与查询，挪到表格上方工具条（logs.html）。
      items: ['timeRange', 'api', 'modelCombobox', 'authToken', 'account', 'logSource', 'status', 'result', 'errorStage', 'logsActions']
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
    const content = (config.items || [])
      .map((item) => fields[item] || '')
      .filter(Boolean)
      .join('\n');

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
