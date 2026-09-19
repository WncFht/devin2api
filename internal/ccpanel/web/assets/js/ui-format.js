// ============================================================
// 共享格式化/通用小工具：数字、成本、耗时配色、HTML 转义、debounce
// ============================================================
(function () {

  /**
   * 防抖函数
   * @param {Function} func - 要防抖的函数
   * @param {number} wait - 等待时间(ms)
   * @returns {Function} 防抖后的函数
   */

  function debounce(func, wait) {
    let timeout;
    return function executedFunction(...args) {
      const later = () => {
        clearTimeout(timeout);
        func(...args);
      };
      clearTimeout(timeout);
      timeout = setTimeout(later, wait);
    };
  }

  function calculateTokenSpeed(outputTokens, durationSeconds, firstByteSeconds) {
    const output = Number(outputTokens);
    const duration = Number(durationSeconds);
    if (!Number.isFinite(output) || output <= 0 || !Number.isFinite(duration) || duration <= 0) {
      return null;
    }

    let tokenDuration = duration;
    const firstByte = Number(firstByteSeconds);
    if (Number.isFinite(firstByte) && firstByte > 0 && firstByte < duration) {
      const generationDuration = duration - firstByte;
      if (generationDuration >= 1) {
        tokenDuration = generationDuration;
      }
    }

    return output / tokenDuration;
  }

  function timingColor(seconds, greenThreshold, warningThreshold) {
    const value = Number(seconds);
    if (!Number.isFinite(value) || value <= 0) return 'var(--neutral-600)';
    if (value <= greenThreshold) return 'var(--success-600)';
    if (value <= warningThreshold) return 'var(--warning-600)';
    return 'var(--error-600)';
  }

  function getFirstByteTimingColor(seconds) {
    return timingColor(seconds, 5, 10);
  }

  function getDurationTimingColor(seconds) {
    return timingColor(seconds, 30, 60);
  }

  /**
   * 格式化成本（美元）
   * @param {number} cost - 成本值
   * @param {number} [decimalPlaces=3] - 小数位数
   * @returns {string} 格式化后的字符串
   */

  function formatCost(cost, decimalPlaces) {
    const value = Number(cost);
    if (!Number.isFinite(value)) return '';
    const hasExplicitDecimalPlaces = Number.isInteger(decimalPlaces);
    const places = hasExplicitDecimalPlaces
      ? Math.max(0, Math.min(6, decimalPlaces))
      : 3;
    if (value === 0) return hasExplicitDecimalPlaces && places > 0 ? '$0.' + '0'.repeat(places) : '$0';
    return '$' + value.toFixed(places);
  }

  /**
   * 格式化标准成本/倍率后成本对
   * 倍率为 1 或两值相等时仅显示标准成本，否则显示 "标准/倍率后"
   * @param {number} standard - 标准成本
   * @param {number|null|undefined} effective - 倍率后成本
   * @returns {string}
   */

  function formatCostPair(standard, effective) {
    const s = Number(standard) || 0;
    const e = (effective === undefined || effective === null) ? s : (Number(effective) || 0);
    if (Math.abs(e - s) < 1e-9) return formatCost(s);
    return formatCost(s) + '/' + formatCost(e);
  }

  /**
   * 格式化倍率文本
   * @param {number} multiplier - 倍率
   * @returns {string}
   */

  function formatCostMultiplier(multiplier) {
    const value = Number(multiplier);
    if (!Number.isFinite(value) || value < 0 || Math.abs(value - 1) < 1e-9) return '';
    return formatCostMultiplierValue(value);
  }

  // 区间端点必须显式显示 1x；单值倍率为 1 时才由上层整体隐藏。

  function formatCostMultiplierValue(multiplier) {
    const value = Number(multiplier);
    if (!Number.isFinite(value) || value < 0) return '';
    return `${Number(value.toFixed(2)).toString()}x`;
  }

  /**
   * 解析标准成本/倍率后成本显示信息
   * @param {number} standard - 标准成本
   * @param {number|null|undefined} effective - 倍率后成本
   * @returns {{standardCost:number,effectiveCost:number,hasMultiplier:boolean,multiplier:number,multiplierText:string}}
   */

  function getCostDisplayInfo(standard, effective) {
    const standardCost = Number(standard) || 0;
    if (!(standardCost > 0)) {
      return {
        standardCost: 0,
        effectiveCost: 0,
        hasMultiplier: false,
        multiplier: 1,
        multiplierText: ''
      };
    }

    const hasExplicitEffectiveCost = effective !== undefined && effective !== null;
    const effectiveValue = hasExplicitEffectiveCost ? (Number(effective) || 0) : standardCost;
    const effectiveCost = hasExplicitEffectiveCost ? effectiveValue : standardCost;
    const hasMultiplier = Math.abs(effectiveCost - standardCost) >= 1e-9;
    const multiplier = hasMultiplier ? (effectiveCost / standardCost) : 1;

    return {
      standardCost,
      effectiveCost,
      hasMultiplier,
      multiplier,
      multiplierText: formatCostMultiplier(multiplier)
    };
  }

  /**
   * 构建两行成本显示HTML
   * @param {number} standard - 标准成本
   * @param {number|null|undefined} effective - 倍率后成本
   * @param {{inline?: boolean, decimalPlaces?: number}} options - 样式配置
   * @returns {string}
   */

  function buildCostStackHtml(standard, effective, options = {}) {
    const info = getCostDisplayInfo(standard, effective);
    if (!(info.standardCost > 0)) return '';

    const inline = options.inline === true;
    const classes = ['cost-stack'];
    if (inline) {
      classes.push('cost-stack--inline');
    }

    const format = cost => formatCost(cost, options.decimalPlaces);

    if (!info.hasMultiplier) {
      return `<span class="${classes.join(' ')}"><span class="cost-stack-effective">${format(info.effectiveCost)}</span></span>`;
    }

    if (inline) {
      return `<span class="${classes.join(' ')}"><span class="cost-stack-standard">${format(info.standardCost)}</span><span class="cost-stack-effective">${format(info.effectiveCost)}</span></span>`;
    }

    return `<span class="${classes.join(' ')}"><span class="cost-stack-standard">${format(info.standardCost)}</span><span class="cost-stack-effective">${format(info.effectiveCost)}</span></span>`;
  }

  // 格式化数字显示（通用：K/M缩写）

  function formatNumber(num) {
    const n = Number(num);
    if (!Number.isFinite(n)) return '0';
    if (n >= 1e9) return (n / 1e9).toFixed(1) + 'B';
    if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
    if (n >= 1e3) return (n / 1e3).toFixed(1) + 'K';
    return Number.isInteger(n) ? String(n) : n.toFixed(2);
  }

  // RPM 颜色：低流量绿色，中等橙色，高流量红色

  function getRpmColor(rpm) {
    const n = Number(rpm);
    if (!Number.isFinite(n)) return 'var(--neutral-600)';
    if (n < 10) return 'var(--success-600)';
    if (n < 100) return 'var(--warning-600)';
    return 'var(--error-600)';
  }

  // RPM 数值文本（1000+ 缩写 K、1+ 一位小数、其余两位）；<0.01 的兜底展示由调用方定

  function formatRpmValue(rpm) {
    if (rpm >= 1000) return (rpm / 1000).toFixed(1) + 'K';
    if (rpm >= 1) return rpm.toFixed(1);
    return rpm.toFixed(2);
  }

  /**
   * HTML转义（防XSS）
   * @param {string} str - 需要转义的字符串
   * @returns {string} 转义后的安全字符串
   */

  function escapeHtml(str) {
    if (str == null) return '';
    return String(str)
      .replace(/&/g, '&amp;')
      .replace(/</g, '&lt;')
      .replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;')
      .replace(/'/g, '&#39;');
  }

  // 简单显示/隐藏切换（用于日志/测试响应块等）

  function toggleResponse(elementId) {
    const el = document.getElementById(elementId);
    if (!el) return;
    el.style.display = el.style.display === 'none' ? 'block' : 'none';
  }

  // 导出到全局作用域

  window.debounce = debounce;
  window.calculateTokenSpeed = calculateTokenSpeed;
  window.formatCost = formatCost;
  window.formatCostPair = formatCostPair;
  window.getCostDisplayInfo = getCostDisplayInfo;
  window.buildCostStackHtml = buildCostStackHtml;
  window.getFirstByteTimingColor = getFirstByteTimingColor;
  window.getDurationTimingColor = getDurationTimingColor;
  window.formatNumber = formatNumber;
  window.getRpmColor = getRpmColor;
  window.formatRpmValue = formatRpmValue;
  window.escapeHtml = escapeHtml;
  window.toggleResponse = toggleResponse;

})();
