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

  // combobox 骨架 = input + 附着 dropdown（createSearchableCombobox attachMode 约定：
  // 输入 id 与下拉 id 以 _dropdown 后缀配对），四处候选字段同形不同 id/宽度类。
  function buildCombobox(id, controlClass = '') {
    return `<div class="${joinClasses('filter-combobox-wrapper', controlClass)}">
          <input id="${id}" class="filter-select filter-combobox" type="text" autocomplete="off" spellcheck="false" />
          <div id="${id}_dropdown" class="filter-dropdown" role="listbox"></div>
        </div>`;
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
        ${buildCombobox('f_model', 'filter-control--wide filter-control--model')}`,
        joinClasses(groupClass, 'filter-group--model')
      ),
      authToken: buildFilterGroup(
        `${buildFilterLabel('f_auth_token', 'stats.token', '令牌')}
        ${buildSelect('f_auth_token', '\n                <option value="" data-i18n="stats.allTokens">全部令牌</option>\n                <!-- 动态加载令牌列表 -->\n              ', authTokenControlClass)}`,
        authTokenGroupClass
      ),
      status: buildFilterGroup(
        `${buildFilterLabel('f_status', 'logs.statusCode', '状态码')}
        ${buildCombobox('f_status', 'filter-control--narrow filter-control--status')}`,
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
        ${buildCombobox('f_error_stage', 'filter-control--compact filter-control--error-stage')}`,
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
        ${buildCombobox('f_account', 'filter-control--compact filter-control--account')}`,
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

  // primary 常驻一行；secondary 收进「更多筛选」披露区（display:contents 参与
  // 同一 flex 流，不包额外盒子）；actions 永远排在末尾。所有字段元素始终在 DOM
  // 中——页面脚本按 id 绑定，披露只是视觉折叠。
  const LAYOUTS = {
    stats: {
      barClass: 'filter-bar stats-filter-bar mt-2',
      controlsClass: 'filter-controls stats-filter-controls',
      groupClass: 'stats-filter-group',
      checkboxGroupClass: 'stats-filter-group stats-filter-group--checkbox',
      actionsClass: 'stats-filter-actions',
      primary: ['timeRange', 'modelCombobox'],
      secondary: ['api', 'authToken'],
      actions: ['statsSummary']
    },
    logs: {
      barClass: 'filter-bar logs-filter-bar mt-2',
      controlsClass: 'filter-controls logs-filter-controls',
      groupClass: 'logs-filter-group',
      timeRangeGroupClass: 'logs-filter-group--range',
      authTokenGroupClass: 'logs-filter-group--token',
      actionsClass: 'logs-filter-actions',
      // 列显隐/导出不参与查询，挪到表格上方工具条（logs.html）。
      primary: ['timeRange', 'modelCombobox', 'status'],
      secondary: ['api', 'authToken', 'account', 'logSource', 'result', 'errorStage'],
      actions: ['logsActions']
    },
    trend: {
      barClass: 'filter-bar mt-2',
      controlsClass: 'filter-controls trend-filter-controls',
      groupClass: '',
      actionsClass: '',
      primary: ['timeRange'],
      secondary: ['api', 'modelSelect', 'authToken'],
      actions: ['filterButton']
    }
  };

  function renderLayout(layoutName) {
    const config = LAYOUTS[layoutName];
    if (!config) {
      console.error(`[PageFilters] Unknown layout: ${layoutName}`);
      return '';
    }

    const fields = buildSharedFields(config);
    const pick = (keys) => (keys || [])
      .map((item) => fields[item] || '')
      .filter(Boolean)
      .join('\n');
    const secondaryHtml = pick(config.secondary);
    const disclosureHtml = secondaryHtml
      ? `<button type="button" class="filter-more-toggle" data-filter-more-toggle aria-expanded="false">
            <span data-i18n="common.moreFilters">更多筛选</span>
            <svg class="filter-more-chevron" viewBox="0 0 20 20" fill="none" aria-hidden="true"><path d="M6 8l4 4 4-4" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"/></svg>
          </button>
          <div class="filter-secondary" data-filter-secondary>
            ${secondaryHtml}
          </div>`
      : '';

    return `<div class="${config.barClass}">
          <div class="${config.controlsClass}">
            ${pick(config.primary)}
            ${disclosureHtml}
            ${pick(config.actions)}
          </div>
          <div class="filter-chips" data-filter-chips hidden></div>
        </div>`;
  }

  // ============================================================
  // 已选条件 chips：折叠后仍给出「当前筛了什么」的可视回执，
  // 点 chip 上的 × 把该字段复位到首项/默认并立即应用。
  // ============================================================

  function escapeChipText(str) {
    return String(str).replace(/[&<>"']/g, (ch) => ({
      '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'
    }[ch]));
  }

  function initFilterChips(container) {
    const bar = container.querySelector('.filter-bar');
    const chipsRow = bar && bar.querySelector('[data-filter-chips]');
    if (!bar || !chipsRow) return;

    const secondary = bar.querySelector('[data-filter-secondary]');
    const toggle = bar.querySelector('[data-filter-more-toggle]');
    if (secondary && toggle) {
      toggle.addEventListener('click', () => {
        toggle.setAttribute('aria-expanded', String(secondary.classList.toggle('open')));
      });
    }

    let chipSpecs = new Map();

    // 每个 .filter-group 按其控件形态推导：是否激活（≠默认值）、展示文案、
    // 复位动作。applyViaButton=true 的控件没有原生 apply 事件，复位后需代点筛选按钮。
    function specFor(group) {
      const labelEl = group.querySelector('.filter-label');
      const label = labelEl ? labelEl.textContent.trim() : '';

      const checkbox = group.querySelector('input[type="checkbox"]');
      if (checkbox) {
        const text = group.querySelector('.filter-checkbox-label span');
        return {
          key: checkbox.id,
          label: text ? text.textContent.trim() : label,
          display: '',
          active: checkbox.checked !== checkbox.defaultChecked,
          applyViaButton: false,
          reset() {
            checkbox.checked = checkbox.defaultChecked;
            checkbox.dispatchEvent(new Event('change', { bubbles: true }));
          }
        };
      }

      // searchable-select 给每个 select 挂了影子 combobox input（无 filterAllLabel），
      // 必须先查 select——它才是语义控件；select 缺席的组才是真 combobox 字段。
      const select = group.querySelector('select');
      if (select) {
        if (!select.options.length) return null;
        const defaultValue = select.options[0].value;
        const selected = select.options[select.selectedIndex];
        return {
          key: select.id,
          label,
          display: selected ? selected.textContent.trim() : select.value,
          active: select.value !== defaultValue,
          applyViaButton: true,
          reset() {
            select.value = defaultValue;
            select.dispatchEvent(new Event('change', { bubbles: true }));
          }
        };
      }

      const combo = group.querySelector('input.filter-combobox');
      if (combo) {
        const allLabel = (combo.dataset.filterAllLabel || '').trim();
        const value = combo.value.trim();
        return {
          key: combo.id,
          label,
          display: value,
          active: value !== '' && value !== allLabel,
          applyViaButton: true,
          reset() {
            const reset = window.FilterControlResets && window.FilterControlResets.get(combo.id);
            if (reset) reset();
          }
        };
      }

      const input = group.querySelector('input.filter-input');
      if (input) {
        const value = input.value.trim();
        return {
          key: input.id,
          label,
          display: value,
          active: value !== '',
          applyViaButton: true,
          reset() {
            input.value = '';
            input.dispatchEvent(new Event('input', { bubbles: true }));
            input.dispatchEvent(new Event('change', { bubbles: true }));
          }
        };
      }

      return null;
    }

    function computeChips() {
      const specs = Array.from(bar.querySelectorAll('.filter-group'))
        .filter((group) => !group.hidden)
        .map(specFor)
        .filter((spec) => spec && spec.active);
      chipSpecs = new Map(specs.map((spec) => [spec.key, spec]));
      chipsRow.innerHTML = specs.map((spec) => {
        const name = escapeChipText(spec.label);
        const value = spec.display
          ? `<span class="filter-chip-value">${escapeChipText(spec.display)}</span>`
          : '';
        return `<button type="button" class="filter-chip" data-chip-key="${escapeChipText(spec.key)}"><span class="filter-chip-name">${name}</span>${value}<svg class="filter-chip-x" viewBox="0 0 20 20" fill="none" aria-hidden="true"><path d="M6 6l8 8M14 6l-8 8" stroke="currentColor" stroke-width="2" stroke-linecap="round"/></svg></button>`;
      }).join('');
      chipsRow.hidden = specs.length === 0;
    }

    chipsRow.addEventListener('click', (e) => {
      const chip = e.target.closest('[data-chip-key]');
      if (!chip) return;
      const spec = chipSpecs.get(chip.dataset.chipKey);
      if (!spec) return;
      spec.reset();
      if (spec.applyViaButton) bar.querySelector('#btn_filter')?.click();
      computeChips();
    });

    bar.addEventListener('change', computeChips);
    bar.addEventListener('input', computeChips);
    bar.addEventListener('filter:change', computeChips);
    bar.addEventListener('click', (e) => {
      if (e.target.closest('#btn_filter, #btn_clear_filters')) setTimeout(computeChips, 0);
    });

    if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
      window.i18n.onLocaleChange(() => setTimeout(computeChips, 0));
    }

    // 恢复值/选项列表/角色显隐都在渲染后异步落地，分档补算
    [0, 400, 1500].forEach((delay) => setTimeout(computeChips, delay));
  }

  function initPageFilters(root = document) {
    if (!root || typeof root.querySelectorAll !== 'function') return;

    root.querySelectorAll('[data-page-filters]').forEach((container) => {
      const layoutName = container.getAttribute('data-page-filters');
      if (!layoutName) return;
      container.innerHTML = renderLayout(layoutName);
      initFilterChips(container);
    });
  }

  window.PageFilters = {
    renderLayout,
    initPageFilters
  };

  initPageFilters();
})();
