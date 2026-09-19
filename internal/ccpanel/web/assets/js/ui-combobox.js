// ============================================================

// 通用可搜索下拉选择框组件 (SearchableCombobox)

// ============================================================

(function () {

  /**

   * 创建可搜索下拉选择框

   * @param {Object} config - 配置对象

   * @param {HTMLElement|string} [config.container] - 容器元素或ID（生成模式必需）

   * @param {string} config.inputId - input 元素 ID

   * @param {string} config.dropdownId - 下拉框元素 ID

   * @param {Function} config.getOptions - 获取选项列表的函数，返回 [{value, label, className?}]

   * @param {Function} config.onSelect - 选中回调 (value, label) => void

   * @param {Function} [config.onCancel] - 取消选择回调

   * @param {string} [config.placeholder] - placeholder 文本

   * @param {string} [config.initialValue] - 初始值

   * @param {string} [config.initialLabel] - 初始显示文本

   * @param {number} [config.minWidth] - 最小宽度 (px)

   * @param {boolean} [config.attachMode] - 附着模式，使用已存在的 HTML 元素

   * @param {boolean} [config.allowCustomInput] - 允许提交非下拉选项的自定义输入

   * @param {boolean} [config.commitEmptyAsFirst] - 输入为空回车/失焦时提交第一项（通常为“全部”），覆盖默认的取消/恢复行为

   * @param {boolean} [config.showAllOptionsOnOpen] - 打开时先展示完整选项，开始输入后再按关键字过滤

   * @returns {Object} 组件实例

   */

  function createSearchableCombobox(config) {

    const {

      container: containerArg,

      inputId,

      dropdownId,

      getOptions,

      onSelect,

      onCancel,

      placeholder = '',

      initialValue = '',

      initialLabel = '',

      minWidth = 150,

      attachMode = false,

      allowCustomInput = false,

      commitEmptyAsFirst = false,

      showAllOptionsOnOpen = false

    } = config;



    let input, dropdown, wrapper, dropdownHome, container = null;



    if (attachMode) {

      // 附着模式：使用已存在的 HTML 元素

      input = document.getElementById(inputId);

      dropdown = document.getElementById(dropdownId);

      if (!input || !dropdown) {

        console.error('SearchableCombobox: input or dropdown not found in attach mode');

        return null;

      }

      wrapper = input.closest('.filter-combobox-wrapper');

      dropdownHome = dropdown.parentElement;

      if (initialLabel) input.value = initialLabel;

    } else {

      // 生成模式：创建新的 HTML 结构

      container = typeof containerArg === 'string'

        ? document.getElementById(containerArg)

        : containerArg;



      if (!container) {

        console.error('SearchableCombobox: container not found');

        return null;

      }



      container.innerHTML = `

        <div class="filter-combobox-wrapper" style="min-width: ${minWidth}px;">

          <input

            id="${inputId}"

            class="filter-select filter-combobox"

            type="text"

            autocomplete="off"

            spellcheck="false"

            placeholder="${escapeHtml(placeholder)}"

            value="${escapeHtml(initialLabel)}"

          />

          <div id="${dropdownId}" class="filter-dropdown" role="listbox"></div>

        </div>

      `;



      input = document.getElementById(inputId);

      dropdown = document.getElementById(dropdownId);

      wrapper = input.closest('.filter-combobox-wrapper');

      dropdownHome = dropdown.parentElement;

    }



    input.setAttribute('role', 'combobox');

    input.setAttribute('aria-autocomplete', 'list');

    input.setAttribute('aria-haspopup', 'listbox');

    input.setAttribute('aria-controls', dropdown.id);

    input.setAttribute('aria-expanded', 'false');

    dropdown.setAttribute('role', 'listbox');



    let activeIndex = -1;

    let outsideHandler = null;

    let repositionHandler = null;

    let currentValue = initialValue;



    function clearOutsideHandler() {

      if (!outsideHandler) return;

      document.removeEventListener('mousedown', outsideHandler, true);

      outsideHandler = null;

    }



    function clearRepositionHandler() {

      if (!repositionHandler) return;

      window.removeEventListener('resize', repositionHandler, true);

      window.removeEventListener('scroll', repositionHandler, true);

      repositionHandler = null;

    }



    function closeDropdown() {

      dropdown.style.display = 'none';

      dropdown.dataset.open = '0';

      input.setAttribute('aria-expanded', 'false');

      input.removeAttribute('aria-activedescendant');

      activeIndex = -1;

      clearOutsideHandler();

      clearRepositionHandler();

      if (dropdownHome && dropdown.parentElement !== dropdownHome) {

        dropdownHome.appendChild(dropdown);

      }

    }



    function beginPick() {

      if (input.dataset.pickActive === '1') return;

      input.dataset.pickActive = '1';

      input.dataset.prevInputValue = input.value;

      input.dataset.prevValue = currentValue;

      delete input.dataset.pickEdited;

      // 非自定义输入模式始终清空；自定义输入模式下：

      // - 当前值为空（全量态）→ 清空，避免把“所有渠道”这类占位标签当成过滤关键字

      // - 当前值精确命中下拉选项（用户已从下拉选中而非输入自定义词）→ 清空，便于再次浏览全部选项

      // - 其余情况（自定义搜索词）→ 保留以便继续编辑

      let shouldClear = !allowCustomInput;

      if (allowCustomInput) {

        const trimmedCurrent = String(currentValue || '').trim();

        if (!trimmedCurrent) {

          shouldClear = true;

        } else {

          const trimmedLower = trimmedCurrent.toLowerCase();

          const matchesOption = getOptions().some((opt) => {

            const v = String(opt.value || '').trim().toLowerCase();

            const l = String(opt.label || '').trim().toLowerCase();

            return v === trimmedLower || l === trimmedLower;

          });

          if (matchesOption) shouldClear = true;

        }

      }

      if (shouldClear) {

        input.value = '';

      }

      activeIndex = -1;

    }



    function cancelPick() {

      if (input.dataset.pickActive !== '1') {

        closeDropdown();

        return;

      }



      const prevInputValue = input.dataset.prevInputValue ?? '';

      const prevValue = input.dataset.prevValue ?? '';



      input.value = prevInputValue;

      currentValue = prevValue;



      delete input.dataset.pickActive;

      delete input.dataset.prevInputValue;

      delete input.dataset.prevValue;

      delete input.dataset.pickEdited;



      closeDropdown();

      if (onCancel) onCancel();

    }



    function commitValue(value, label) {

      currentValue = value;

      input.value = label;



      delete input.dataset.pickActive;

      delete input.dataset.prevInputValue;

      delete input.dataset.prevValue;

      delete input.dataset.pickEdited;



      closeDropdown();

      if (onSelect) onSelect(value, label);

    }



    function commitOption(option) {

      if (!option || option.disabled === true) return false;

      commitValue(option.value, option.label);

      return true;

    }



    function commitFirstMatchedOrCancel() {

      const keyword = input.value.trim();

      if (!keyword) {

        if (commitEmptyAsFirst) {

          // 空输入回车/失焦时提交第一项（约定为“全部”），无论之前是否有选中值。

          const option = getOptions().find(opt => opt.disabled !== true);

          if (option) {

            commitOption(option);

            return;

          }

        }

        if (allowCustomInput) {

          // 自定义输入模式下，若打开下拉前已存在选中值（即本次仅是浏览/清空显示），

          // 视为取消并恢复之前的选择；只有从空态主动确认空值时才清除筛选。

          const prevInputValue = String(input.dataset.prevInputValue ?? '').trim();

          const prevValue = String(input.dataset.prevValue ?? '').trim();

          if (prevInputValue || prevValue) {

            cancelPick();

            return;

          }

          commitValue('', '');

          return;

        }

        cancelPick();

        return;

      }

      if (allowCustomInput) {

        const normalizedKeyword = keyword.toLowerCase();

        const exactOption = getOptions().find((opt) => {

          if (opt.disabled === true) return false;

          const label = String(opt.label || '').trim().toLowerCase();

          const value = String(opt.value || '').trim().toLowerCase();

          return label === normalizedKeyword || value === normalizedKeyword;

        });

        if (exactOption) {

          commitOption(exactOption);

          return;

        }

        commitValue(keyword, keyword);

        return;

      }

      const items = getDropdownItems();

      const option = items.find(item => item.disabled !== true);

      if (option) {

        commitOption(option);

        return;

      }

      cancelPick();

    }



    function getDropdownItems() {

      const allOptions = getOptions();

      if (showAllOptionsOnOpen && input.dataset.pickActive === '1' && input.dataset.pickEdited !== '1') {

        return allOptions;

      }

      const keyword = input.value.trim().toLowerCase();

      if (!keyword) return allOptions;

      return allOptions.filter(opt =>

        String(opt.label).toLowerCase().includes(keyword) ||

        String(opt.value).toLowerCase().includes(keyword)

      );

    }



    function renderDropdown() {

      if (dropdown.dataset.open !== '1') return;



      const items = getDropdownItems();

      dropdown.innerHTML = '';



      if (activeIndex >= items.length) activeIndex = items.length - 1;

      if (activeIndex < -1) activeIndex = -1;



      items.forEach((item, idx) => {

        const row = document.createElement('div');

        row.className = 'filter-dropdown-item';

        row.setAttribute('role', 'option');

        row.id = `${dropdown.id}-option-${idx}`;

        row.dataset.value = item.value;

        row.dataset.index = String(idx);

        row.textContent = item.label;

        if (item.className) {

          row.classList.add(...String(item.className).split(/\s+/).filter(Boolean));

        }

        if (item.disabled === true) {

          row.classList.add('filter-dropdown-item--disabled');

          row.setAttribute('aria-disabled', 'true');

        }



        const selected = item.value === currentValue;

        row.setAttribute('aria-selected', selected ? 'true' : 'false');

        if (selected) row.classList.add('selected');

        if (idx === activeIndex) row.classList.add('active');



        // Keep the option in the DOM until the click event is dispatched.

        // Firefox retargets a click to the dialog when a mousedown handler

        // removes the clicked node immediately; dialog backdrop handlers then

        // mistake a normal selection for an outside click and close the modal.

        row.addEventListener('mousedown', (e) => {

          e.preventDefault();

          e.stopPropagation();

        });

        row.addEventListener('click', (e) => {

          e.preventDefault();

          e.stopPropagation();

          commitOption(item);

        });



        dropdown.appendChild(row);

      });



      if (activeIndex >= 0 && activeIndex < items.length) {

        input.setAttribute('aria-activedescendant', `${dropdown.id}-option-${activeIndex}`);

      } else {

        input.removeAttribute('aria-activedescendant');

      }

    }



    function positionDropdown() {

      if (dropdown.dataset.open !== '1') return;

      const rect = input.getBoundingClientRect();

      const margin = 6;



      dropdown.style.left = `${Math.round(rect.left)}px`;

      dropdown.style.width = `${Math.round(rect.width)}px`;

      dropdown.style.top = `${Math.round(rect.bottom + margin)}px`;



      const dropdownHeight = dropdown.offsetHeight || 0;

      const viewportBottom = window.innerHeight || 0;

      if (dropdownHeight && rect.bottom + margin + dropdownHeight > viewportBottom && rect.top - margin - dropdownHeight >= 0) {

        dropdown.style.top = `${Math.round(rect.top - margin - dropdownHeight)}px`;

      }

    }



    function openDropdown() {

      const dialog = input.closest('dialog');

      const portalRoot = dialog?.open ? dialog : document.body;

      if (dropdownHome && dropdown.parentElement !== portalRoot) {

        portalRoot.appendChild(dropdown);

      }

      dropdown.style.display = 'block';

      dropdown.dataset.open = '1';

      input.setAttribute('aria-expanded', 'true');

      renderDropdown();

      positionDropdown();



      clearOutsideHandler();

      outsideHandler = (e) => {

        if (!wrapper.contains(e.target) && !dropdown.contains(e.target)) {

          commitFirstMatchedOrCancel();

        }

      };

      document.addEventListener('mousedown', outsideHandler, true);



      clearRepositionHandler();

      repositionHandler = () => positionDropdown();

      window.addEventListener('resize', repositionHandler, true);

      window.addEventListener('scroll', repositionHandler, true);

    }



    function moveActive(delta) {

      const items = getDropdownItems();

      if (items.length <= 0) return;

      let nextIndex = activeIndex;

      if (nextIndex === -1) nextIndex = delta < 0 ? items.length : -1;

      while (true) {

        nextIndex += delta;

        if (nextIndex < 0 || nextIndex >= items.length) return;

        if (items[nextIndex].disabled !== true) break;

      }

      activeIndex = nextIndex;

      renderDropdown();

    }



    // 事件绑定

    input.addEventListener('mousedown', () => {

      beginPick();

      openDropdown();

    });



    input.addEventListener('input', () => {

      const wasClosed = dropdown.dataset.open !== '1';

      if (wasClosed) {

        beginPick();

      }

      input.dataset.pickEdited = '1';

      if (wasClosed) {

        openDropdown();

      }

      activeIndex = -1;

      renderDropdown();

    });



    input.addEventListener('keydown', (e) => {

      if (e.key === 'Escape') {

        if (dropdown.dataset.open === '1') {

          e.preventDefault();

          cancelPick();

        }

        return;

      }



      if (e.key === 'ArrowDown') {

        e.preventDefault();

        if (dropdown.dataset.open !== '1') {

          beginPick();

          openDropdown();

          return;

        }

        moveActive(1);

        return;

      }



      if (e.key === 'ArrowUp') {

        e.preventDefault();

        if (dropdown.dataset.open !== '1') {

          beginPick();

          openDropdown();

          return;

        }

        moveActive(-1);

        return;

      }



      if (e.key === 'Enter') {

        e.preventDefault();

        if (dropdown.dataset.open === '1') {

          const items = getDropdownItems();

          if (activeIndex >= 0 && activeIndex < items.length) {

            commitOption(items[activeIndex]);

            return;

          }

          commitFirstMatchedOrCancel();

          return;

        }

        if (input.dataset.pickActive === '1') {

          commitFirstMatchedOrCancel();

        }

      }

    });



    input.addEventListener('blur', () => {

      if (dropdown.dataset.open !== '1') return;

      commitFirstMatchedOrCancel();

    });



    // 返回组件实例，提供外部控制接口

    return {

      getValue: () => currentValue,

      setValue: (value, label) => {

        currentValue = value;

        input.value = label;

      },

      refresh: () => {

        if (dropdown.dataset.open === '1') {

          renderDropdown();

        }

      },

      getInput: () => input,

      getDropdown: () => dropdown,

      destroy: () => {

        closeDropdown();

        clearOutsideHandler();

        clearRepositionHandler();

        if (!attachMode && container) {

          container.innerHTML = '';

        }

      }

    };

  }



  // 导出到全局作用域

  window.createSearchableCombobox = createSearchableCombobox;

})();
