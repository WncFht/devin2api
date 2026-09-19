// ============================================================
// 页面筛选/引导共享助手 + 协议配置管理
// ============================================================
// ============================================================
// 协议配置管理模块（动态加载配置，单一数据源）
// ============================================================
(function () {
  let protocolsCache = null;

  // 复用公共工具（DRY）：真实实现由下方公共工具模块导出到 window.escapeHtml
  const escapeHtml = (str) => window.escapeHtml(str);

  // 入口端点显示名由 locales 提供；值与 logs 表的 api 字段同口径
  const PROTOCOL_LABEL_KEYS = {
    anthropic: 'modelTest.clientProtocolAnthropic',
    'openai-chat': 'modelTest.clientProtocolOpenAI',
    'openai-responses': 'modelTest.clientProtocolCodex',
    'responses-ws': 'common.apiResponsesWs'
  };

  function protocolDisplayName(value) {
    const key = PROTOCOL_LABEL_KEYS[value];
    const label = key && typeof window.t === 'function' ? window.t(key) : '';
    return label && label !== key ? label : value;
  }

  /**
   * 获取协议列表（带缓存）。后端返回协议名数组，显示名由前端 locales 补齐。
   * @returns {Promise<Array<{value: string, display_name: string}>>}
   */
  async function getProtocols() {
    if (protocolsCache) {
      return protocolsCache;
    }

    const values = await fetchData('/public/protocols');
    protocolsCache = (Array.isArray(values) ? values : []).map(value => ({
      value,
      display_name: protocolDisplayName(value)
    }));
    return protocolsCache;
  }

  /**
   * 渲染协议下拉选择框
   * @param {string} selectId - select元素ID
   * @param {string} selectedValue - 选中的值（默认'anthropic'）
   */
  async function renderProtocolSelect(selectId, selectedValue = 'anthropic') {
    const select = document.getElementById(selectId);
    if (!select) {
      console.error('select element not found:', selectId);
      return;
    }

    const protocols = await getProtocols();

    select.innerHTML = protocols.map(protocol => `
      <option value="${escapeHtml(protocol.value)}"
              ${protocol.value === selectedValue ? 'selected' : ''}>
        ${escapeHtml(protocol.display_name)}
      </option>
    `).join('');
  }

  // 导出到全局作用域
  window.ProtocolManager = {
    getProtocols,
    renderProtocolSelect
  };
})();

(function () {

  function bindFilterApplyInputs(options = {}) {
    const apply = typeof options.apply === 'function' ? options.apply : null;
    if (!apply) return;

    const debounceMs = Number.isFinite(options.debounceMs) ? options.debounceMs : 500;
    const debouncedApply = debounce(apply, debounceMs);

    (Array.isArray(options.debounceInputIds) ? options.debounceInputIds : []).forEach((id) => {
      const el = document.getElementById(id);
      if (!el) return;
      el.addEventListener('input', debouncedApply);
    });

    (Array.isArray(options.enterInputIds) ? options.enterInputIds : []).forEach((id) => {
      const el = document.getElementById(id);
      if (!el) return;
      el.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') {
          apply();
        }
      });
    });
  }

  const delegatedActionConfig = {
    click: {
      selector: '[data-action]',
      datasetKey: 'action'
    },
    change: {
      selector: '[data-change-action]',
      datasetKey: 'changeAction'
    },
    input: {
      selector: '[data-input-action]',
      datasetKey: 'inputAction'
    }
  };

  function initDelegatedActions(options = {}) {
    const root = options.root || document;
    const boundElement = options.boundElement || document.body;
    const boundKey = options.boundKey;

    if (!root || !boundElement || !boundElement.dataset || !boundKey) {
      return false;
    }

    if (boundElement.dataset[boundKey]) {
      return false;
    }

    Object.entries(delegatedActionConfig).forEach(([eventType, config]) => {
      const handlers = options[eventType];
      if (!handlers || typeof handlers !== 'object') return;

      root.addEventListener(eventType, (event) => {
        const eventTarget = event.target;
        if (!eventTarget || typeof eventTarget.closest !== 'function') return;

        const actionTarget = eventTarget.closest(config.selector);
        if (!actionTarget) return;

        const actionName = actionTarget.dataset[config.datasetKey];
        const handler = handlers[actionName];
        if (typeof handler === 'function') {
          handler(actionTarget, event);
        }
      });
    });

    boundElement.dataset[boundKey] = '1';
    return true;
  }

  function initPageBootstrap(options = {}) {
    const run = typeof options.run === 'function' ? options.run : () => {};

    const execute = async () => {
	  const session = await window.fetchDataWithAuth('/dashboard/session');
	  if (session && session.role) localStorage.setItem(window.WebAuth.ROLE_KEY, session.role);
	  const restrictedPages = new Set(['tokens', 'settings', 'models', 'accounts']);
	  if (window.isAPITokenRole() && restrictedPages.has(options.topbarKey)) {
	    window.location.replace('/web/index.html');
	    return;
	  }
      if (options.translate !== false && window.i18n && typeof window.i18n.translatePage === 'function') {
        window.i18n.translatePage();
      }

      if (options.topbarKey && typeof window.initTopbar === 'function') {
        window.initTopbar(options.topbarKey);
      }

      await run();
    };

    if (document.readyState === 'loading') {
      document.addEventListener('DOMContentLoaded', () => {
        void execute();
      }, { once: true });
      return;
    }

    void execute();
  }

  function getFilterControlConfig(config) {
    if (typeof config === 'string') {
      return { id: config, defaultValue: '', trim: false };
    }
    return {
      id: config && config.id ? config.id : '',
      defaultValue: config && config.defaultValue !== undefined ? config.defaultValue : '',
      trim: Boolean(config && config.trim)
    };
  }

  function readFilterControlValues(fieldMap = {}) {
    const values = {};
    Object.entries(fieldMap).forEach(([key, config]) => {
      const { id, defaultValue, trim } = getFilterControlConfig(config);
      const rawValue = document.getElementById(id)?.value;
      const normalizedValue = typeof rawValue === 'string' && trim ? rawValue.trim() : rawValue;
      values[key] = normalizedValue || defaultValue;
    });
    return values;
  }

  function applyFilterControlValues(values = {}, fieldMap = {}) {
    Object.entries(fieldMap).forEach(([key, config]) => {
      const { id, defaultValue } = getFilterControlConfig(config);
      const el = document.getElementById(id);
      if (!el) return;
      el.value = values[key] || defaultValue;
    });
  }

  function persistFilterState(options = {}) {
    const values = options.values !== undefined
      ? options.values
      : (typeof options.getValues === 'function' ? options.getValues() : {});

    if (!window.FilterState) {
      return values;
    }

    if (options.key) {
      window.FilterState.save(options.key, values);
    }

    if (options.fields) {
      const historyOptions = {
        values,
        fields: options.fields
      };

      ['search', 'pathname', 'preserveExistingParams', 'historyMethod'].forEach((key) => {
        if (options[key] !== undefined) {
          historyOptions[key] = options[key];
        }
      });

      window.FilterState.writeHistory(historyOptions);
    }

    return values;
  }

  function initSavedDateRangeFilter(options = {}) {
    if (typeof window.initDateRangeSelector !== 'function') return null;

    const selectId = options.selectId;
    if (!selectId) return null;

    const defaultValue = typeof options.defaultValue === 'string' && options.defaultValue
      ? options.defaultValue
      : 'today';
    const restoredValue = typeof options.restoredValue === 'string' && options.restoredValue
      ? options.restoredValue
      : defaultValue;
    const onChange = typeof options.onChange === 'function' ? options.onChange : () => {};
    const selectorOptions = {};
    if (options.includeCustom === true) {
      selectorOptions.includeCustom = true;
    }
    if (options.includeAll === true) {
      selectorOptions.includeAll = true;
    }
    if (Array.isArray(options.values)) {
      selectorOptions.values = options.values;
    }
    if (typeof options.restoredValue === 'string' && options.restoredValue) {
      selectorOptions.restoredValue = restoredValue;
    }
    if (options.customRange) {
      selectorOptions.customRange = options.customRange;
    }
    if (options.customPickerContainerId) {
      selectorOptions.customPickerContainerId = options.customPickerContainerId;
    }

    window.initDateRangeSelector(selectId, defaultValue, onChange, selectorOptions);

    const el = document.getElementById(selectId);
    if (el) {
      el.value = restoredValue;
    }
    return el;
  }

  function fillAuthTokenSelect(selectId, tokens, opts) {
    const o = opts || {};
    const select = document.getElementById(selectId);
    if (select && tokens.length > 0) {
      select.innerHTML = `<option value="">${window.t('stats.allTokens')}</option>`;
      tokens.forEach(token => {
        const option = document.createElement('option');
        option.value = token.id;
        option.textContent = token.description || `${o.tokenPrefix || 'Token #'}${token.id}`;
        select.appendChild(option);
      });
      if (o.restoreValue) select.value = o.restoreValue;
    }
  }

  async function initAuthTokenFilter(options = {}) {
    const selectId = options.selectId;
    if (!selectId) return [];

    if (window.isAPITokenRole()) {
      return [];
    }

    if (Array.isArray(options.preloadedTokens)) {
      fillAuthTokenSelect(selectId, options.preloadedTokens, options.loadOptions);
      const el = document.getElementById(selectId);
      if (!el) return options.preloadedTokens;
      el.value = options.value || '';
      if (typeof options.onChange === 'function') {
        el.addEventListener('change', options.onChange);
      }
      return options.preloadedTokens;
    }

    if (typeof window.loadAuthTokensIntoSelect !== 'function') return [];

    const tokens = await window.loadAuthTokensIntoSelect(selectId, options.loadOptions);
    const el = document.getElementById(selectId);
    if (!el) return tokens;

    el.value = options.value || '';
    if (typeof options.onChange === 'function') {
      el.addEventListener('change', options.onChange);
    }

    return tokens;
  }

  async function loadAuthTokensIntoSelect(selectId, opts) {
    const o = opts || {};
	if (window.isAPITokenRole()) return [];
    try {
      const data = await fetchDataWithAuth('/admin/auth-tokens');
      const tokens = (data && data.tokens) || [];
      window.fillAuthTokenSelect(selectId, tokens, o);
      return tokens;
    } catch (error) {
      console.error('Failed to load auth tokens:', error);
      return [];
    }
  }

  // 重绑同组按钮时摘除旧 handler 的登记表（元素级，随 DOM 回收）

  const timeRangeClickHandlers = new WeakMap();

  /**
   * 初始化时间范围按钮选择器
   * @param {function(string)} onRangeChange - 范围变更回调，参数为 range 值
   * @param {ParentNode} [scope] - 限定按钮查找范围；缺省为整文档（旧行为）。
   *   页面有多个 pill 组时必须传容器，否则后绑定者劫持全部组。
   */

  function initTimeRangeSelector(onRangeChange, scope = document) {
    const buttons = scope.querySelectorAll('.time-range-btn');
    buttons.forEach(btn => {
      const prev = timeRangeClickHandlers.get(btn);
      if (prev) btn.removeEventListener('click', prev);

      const handleClick = function () {
        const result = onRangeChange(this.dataset.range, this);
        if (result === false) return;

        buttons.forEach(b => b.classList.remove('active'));
        this.classList.add('active');
      };

      timeRangeClickHandlers.set(btn, handleClick);
      btn.addEventListener('click', handleClick);
    });
  }

  // 渲染日期按钮 + 绑定切换回调 + 监听 i18n 重渲染

  function bindTimeRangeSelector(options = {}) {
    const { containerId, values, includeAll = false, initialValue, customRange, onChange } = options;
    let currentValue = initialValue;
    let currentCustomRange = customRange || null;

    const render = () => {
      if (typeof window.renderDateRangeButtons !== 'function') return;
      const cfg = { values, activeValue: currentValue };
      if (includeAll) cfg.includeAll = true;
      window.renderDateRangeButtons(containerId, cfg);
    };

    const bind = () => {
      const scope = document.getElementById(containerId) || document;
      initTimeRangeSelector((range, button) => {
        if (range === 'custom' && typeof window.openCustomDateRangePicker === 'function') {
          window.openCustomDateRangePicker({
            containerId,
            range: currentCustomRange,
            onConfirm: (confirmedRange) => {
              currentValue = 'custom';
              currentCustomRange = confirmedRange;
              scope.querySelectorAll('.time-range-btn').forEach(b => b.classList.remove('active'));
              if (button) button.classList.add('active');
              if (button && confirmedRange.label) button.title = confirmedRange.label;
              if (typeof onChange === 'function') onChange('custom', confirmedRange);
            }
          });
          return false;
        }

        currentValue = range;
        if (typeof onChange === 'function') onChange(range);
      }, scope);
    };

    render();
    bind();

    if (window.i18n && typeof window.i18n.onLocaleChange === 'function') {
      window.i18n.onLocaleChange(() => {
        render();
        bind();
      });
    }
  }

  // 页面筛选组合框的统一形态：attach 到 page-filters 渲染的 {inputId}_dropdown，
  // 允许自由输入、空提交取首项（allLabel）。输入元素缺席（布局未渲染该字段）
  // 返回 null。opts.initialLabel 缺省取 initialValue || allLabel。
  function initFilterCombobox(opts) {
    if (typeof window.createSearchableCombobox !== 'function') return null;
    if (!document.getElementById(opts.inputId)) return null;
    return window.createSearchableCombobox({
      inputId: opts.inputId,
      dropdownId: opts.inputId + '_dropdown',
      attachMode: true,
      allowCustomInput: true,
      commitEmptyAsFirst: true,
      initialValue: opts.initialValue || '',
      initialLabel: opts.initialLabel !== undefined ? opts.initialLabel : (opts.initialValue || opts.allLabel),
      getOptions: () => [{ value: '', label: opts.allLabel }, ...opts.getOptions()],
      onSelect: opts.onSelect
    });
  }

  window.bindFilterApplyInputs = bindFilterApplyInputs;
  window.initDelegatedActions = initDelegatedActions;
  window.initPageBootstrap = initPageBootstrap;
  window.readFilterControlValues = readFilterControlValues;
  window.applyFilterControlValues = applyFilterControlValues;
  window.persistFilterState = persistFilterState;
  window.initSavedDateRangeFilter = initSavedDateRangeFilter;
  window.fillAuthTokenSelect = fillAuthTokenSelect;
  window.initAuthTokenFilter = initAuthTokenFilter;
  window.loadAuthTokensIntoSelect = loadAuthTokensIntoSelect;
  window.initTimeRangeSelector = initTimeRangeSelector;
  window.bindTimeRangeSelector = bindTimeRangeSelector;
  window.initFilterCombobox = initFilterCombobox;

})();
