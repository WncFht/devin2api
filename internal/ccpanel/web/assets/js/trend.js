    // 全局变量
    const t = window.t;

    window.trendData = null;
    window.currentRange = 'today'; // 默认"本日"
    window.currentTrendType = 'first_byte'; // 默认显示首字响应趋势 (count/rpm/tps/error_rate/first_byte/duration/tokens/cost/cache_hit)
    window.currentTrendChartType = 'line'; // 默认使用折线图，可切换为柱状图
    window.currentModel = ''; // 当前选中的模型（空字符串表示全部模型）
    window.currentAuthToken = ''; // 当前选中的令牌（空字符串表示全部令牌）
    window.currentAPI = ''; // 当前选中的入口端点（logs 表 api 原值）
    let currentTrendCustomTimeRange = null;
    window.chartInstance = null;
    window.visibleModels = new Set(); // 可见模型序列集合
    window.availableModels = []; // 可用模型列表
    window.authTokens = []; // 令牌列表

    function getTrendChartTheme() {
      return typeof window.getChartTheme === 'function'
        ? window.getChartTheme()
        : {
          text: '#374151',
          mutedText: '#6b7280',
          strongText: '#111827',
          axisLine: '#e5e7eb',
          splitLine: 'rgba(148, 163, 184, 0.25)',
          surface: '#ffffff',
          surfaceMuted: 'rgba(148, 163, 184, 0.10)',
          tooltipBg: 'rgba(255, 255, 255, 0.98)',
          tooltipBorder: 'rgba(17, 24, 39, 0.16)',
          tooltipText: '#111827'
        };
    }

    const TREND_FILTER_KEY = 'trend.filters';
    const TREND_FILTER_FIELDS = [
      {
        key: 'range',
        queryKeys: ['range'],
        defaultValue: 'today',
        includeInQuery(value) {
          return Boolean(value) && value !== 'today';
        }
      },
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
      {
        key: 'trendType',
        queryKeys: ['type'],
        defaultValue: 'first_byte',
        includeInQuery(value) {
          return Boolean(value) && value !== 'first_byte';
        },
        includeInRequest() {
          return false;
        }
      },
      { key: 'api', queryKeys: ['api'], defaultValue: '' },
      { key: 'model', queryKeys: ['model'], defaultValue: '' },
      { key: 'authToken', queryKeys: ['token'], requestKey: 'auth_token_id', defaultValue: '' }
    ];
    const TREND_MODELS_REQUEST_FIELDS = TREND_FILTER_FIELDS.filter((field) => field.key === 'range');

    function getTrendFilters() {
      const range = window.currentRange || 'today';
      const hasCustomRange = range === 'custom' && currentTrendCustomTimeRange;
      return {
        range,
        customStartTime: hasCustomRange ? String(currentTrendCustomTimeRange.startMs) : '',
        customEndTime: hasCustomRange ? String(currentTrendCustomTimeRange.endMs) : '',
        trendType: window.currentTrendType || 'first_byte',
        api: window.currentAPI || '',
        model: window.currentModel || '',
        authToken: window.currentAuthToken || ''
      };
    }

    function loadSavedTrendFilters(storage = window.localStorage) {
      return window.FilterState.load(TREND_FILTER_KEY, storage);
    }

    function normalizeTrendCustomTimeRange(range) {
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

    function appendTrendTimeRangeParams(params, filters) {
      const range = filters?.range || 'today';
      const query = typeof window.buildDateRangeQuery === 'function'
        ? window.buildDateRangeQuery(range, currentTrendCustomTimeRange)
        : `range=${encodeURIComponent(range)}`;
      new URLSearchParams(query).forEach((value, key) => {
        params.set(key, value);
      });
      return params;
    }

    function getTrendRangeHours(range) {
      if (range === 'custom' && currentTrendCustomTimeRange) {
        return Math.max((currentTrendCustomTimeRange.endMs - currentTrendCustomTimeRange.startMs) / 3600000, 1 / 60);
      }
      return window.getRangeHours ? getRangeHours(range) : 24;
    }

    function buildTrendRequestParams(baseParams = {}) {
      const params = window.FilterQuery.buildRequestParams(getTrendFilters(), TREND_FILTER_FIELDS, {
        baseParams
      });
      appendTrendTimeRangeParams(params, getTrendFilters());
      return params;
    }

    // 加载当前时间范围内的可用模型列表
    async function loadModels(range) {
      try {
        const filters = {
          ...getTrendFilters(),
          range: range || window.currentRange || 'today'
        };
        const params = window.FilterQuery.buildRequestParams(filters, TREND_MODELS_REQUEST_FIELDS);
        appendTrendTimeRangeParams(params, filters);
        const url = `/dashboard/models?${params.toString()}`;

        const resp = await fetchDataWithAuth(url) || {};
        const rawModels = Array.isArray(resp.models) ? resp.models : [];

        // 去重：使用 Set 确保模型名称唯一
        window.availableModels = [...new Set(rawModels)];

        // 填充模型选择器
        const modelSelect = document.getElementById('f_model');
        if (modelSelect) {
          // 保留"全部模型"选项
          modelSelect.innerHTML = `<option value="">${t('trend.allModels')}</option>`;
          window.availableModels.forEach(model => {
            const option = document.createElement('option');
            option.value = model;
            option.textContent = model;
            modelSelect.appendChild(option);
          });

          // 恢复之前选择的模型（如果仍在列表中）
          if (window.currentModel && window.availableModels.includes(window.currentModel)) {
            modelSelect.value = window.currentModel;
          } else {
            // 模型不在新列表中，重置为"全部"
            window.currentModel = '';
            modelSelect.value = '';
          }
        }
      } catch (error) {
        console.error('加载模型列表失败:', error);
      }
    }

    let trendLoadInFlight = false;
    let trendLoadPending = false;

    async function loadData(skipLoading) {
      // 与 logs.js 同款：在途时记 pending 而不是丢——加载中改筛选或点
      // 应用的意图不该被静默吞掉
      if (trendLoadInFlight) {
        trendLoadPending = true;
        return;
      }
      trendLoadInFlight = true;
      try {
        if (!skipLoading) renderTrendLoading();

        // 从 DOM 元素读取当前选择的时间范围和模型
        const rangeSelect = document.getElementById('f_hours');
        const currentRange = rangeSelect ? rangeSelect.value : (window.currentRange || 'today');
        window.currentRange = currentRange; // 同步到全局变量
        if (currentRange !== 'custom') {
          currentTrendCustomTimeRange = null;
        }

        const modelSelect = document.getElementById('f_model');
        if (modelSelect) {
          window.currentModel = modelSelect.value || '';
        }

        const apiSelect = document.getElementById('f_api');
        if (apiSelect) {
          window.currentAPI = apiSelect.value || '';
        }

        const tokenSelect = document.getElementById('f_auth_token');
        if (tokenSelect) {
          window.currentAuthToken = tokenSelect.value || '';
        }

        const hours = getTrendRangeHours(currentRange);
        window.currentHours = hours; // 同步到全局变量，供 renderChart 使用
        const reqBucketSec = effectiveBucketSec(hours);

        const metricsParams = buildTrendRequestParams({
          bucket_sec: reqBucketSec
        });
        const metrics = await fetchAPIWithAuthRaw('/dashboard/metrics?' + metricsParams.toString());

        if (!metrics.payload.success) {
          throw new Error(metrics.payload.error || t('trend.fetchDataFailed'));
        }

        window.trendData = metrics.payload.data || [];
        // 后端桶时间字段有 ts/Ts 两种大小写——入库即归一到 ts，
        // 渲染路径不再逐点回退
        window.trendData.forEach(p => { if (p.ts == null) p.ts = p.Ts; });

        // 构建模型数据缓存（一次遍历，供后续 hasModelData 使用）
        buildModelDataCache(window.trendData);

        // 首次访问默认只显示总数；已有选择时剔除数据里已不存在的模型。
        if (window.visibleModels.size > 0) {
          const validModels = new Set();
          window.visibleModels.forEach(modelName => {
            if (hasModelData(modelName, window.trendData)) {
              validModels.add(modelName);
            }
          });
          window.visibleModels = validModels;
          persistModelState();
        }

        const debugTotal = metrics.res.headers.get('X-Debug-Total');
        // 后端可能因点数上限把桶宽抬档；RPM/TPS/间隔片统一吃回传的生效值
        window.currentBucketSec = Number(metrics.res.headers.get('X-Bucket-Sec')) || reqBucketSec;

        updateModelFilter();
        renderChart();

        // 更新分桶提示：尾部带本次拉取时刻，自动刷新时看数据新不新
        const iv = document.getElementById('bucket-interval');
        if (iv) {
          iv.textContent = t('trend.dataInterval', {
            interval: formatInterval(window.currentBucketSec),
            points: trendData.length,
            total: debugTotal || t('trend.unknown')
          }) + ' · ' + t('trend.updatedAt', { time: fmtBucketTime(Date.now(), false, true) });
        }

      } catch (error) {
        console.error('加载趋势数据失败:', error);
        try { if (window.showError) window.showError(t('trend.loadDataFailed')); } catch(_){}
        // 已有数据时保留旧图，只提示错误；首载失败才切错误视图
        if (!window.trendData || !window.trendData.length) renderTrendError();
      } finally {
        trendLoadInFlight = false;
        if (trendLoadPending) {
          trendLoadPending = false;
          void loadData(true);
        }
      }
    }

    // 保温机制摘要：复用 /admin/runtime-metrics 的 warm 组；api_token 身份
    // 无 admin 权限——端点回 401 会被 fetchWithAuth 踢去登录页，这里按
    // 角色直接跳过；组未投影（未接线）或失败时同样保持隐藏。
    async function loadWarmStatus() {
      const item = document.getElementById('warm-status-item');
      const label = document.getElementById('warm-status');
      if (!item || !label) return;
      if (window.isAPITokenRole && window.isAPITokenRole()) return;
      try {
        const data = await fetchDataWithAuth('/admin/runtime-metrics');
        const warm = data ? data.warm : null;
        if (!warm) {
          item.style.display = 'none';
          return;
        }
        if (!warm.enabled) {
          label.textContent = t('trend.warmStatusDisabled');
        } else {
          const total = (warm.ping_hits || 0) + (warm.ping_misses || 0);
          const rate = total > 0 ? ((warm.ping_hits / total) * 100).toFixed(1) + '%' : '—';
          label.textContent = t('trend.warmStatus', {
            entries: warm.promoted || 0,
            pings: warm.pings_sent || 0,
            rate: rate
          });
        }
        item.style.display = '';
      } catch (_) {
        item.style.display = 'none';
      }
    }

    // 自动分桶（秒）：按窗口长度分档，目标 ~300 点——短窗给到 10s
    // 近实时粒度，长窗放宽到 5/15/60 分钟。后端点数上限仍会兜底，
    // 实际生效值以响应头 X-Bucket-Sec（window.currentBucketSec）为准。
    function computeBucketSec(hours) {
      if (hours <= 1) return 10;
      if (hours <= 3) return 30;
      if (hours <= 6) return 60;
      if (hours <= 24) return 300;
      if (hours <= 72) return 900;
      return 3600;
    }

    // 请求分桶：用户在工具栏选的粒度（秒）优先，0/未选 = 自动分档
    function effectiveBucketSec(hours) {
      const override = Number(localStorage.getItem(TREND_BUCKET_KEY));
      return override > 0 ? override : computeBucketSec(hours);
    }

    function renderTrendLoading() {
      document.getElementById('chart-loading').style.display = 'flex';
      document.getElementById('chart-error').classList.add('hidden');
      document.getElementById('chart').classList.add('hidden');
    }

    function renderTrendError() {
      document.getElementById('chart-loading').style.display = 'none';
      document.getElementById('chart-error').classList.remove('hidden');
      document.getElementById('chart').classList.add('hidden');
    }

    const positiveOrNull = (v) => (v != null && v > 0) ? v : null;

    // 趋势视图规格表：每种 trendType 声明总量线（totals）与模型线（models）
    // 的取值口径，renderChart 统一展开成 ECharts series——新增视图只加表项，
    // 不再复制整段 series 模板。
    // totals.value(point, bucketSec)：point 是聚合桶；模型线 value(m, bucketSec)
    // 的 m 是桶内该模型子对象（缺失时按 zeroFill 给 0，否则给 null）。
    const TREND_SERIES_SPECS = {
      count: {
        totals: [
          { nameKey: 'trend.totalSuccess', color: '#10b981', area: 0.22, value: (p) => p.success || 0 },
          { nameKey: 'trend.totalFailed', color: '#ef4444', area: 0.12, value: (p) => p.error || 0 }
        ],
        models: [
          { nameKey: 'trend.modelSuccess', lineType: 'solid', zeroFill: true, value: (m) => m.success || 0 },
          { nameKey: 'trend.modelFailed', lineType: 'dashed', zeroFill: true, value: (m) => m.error || 0 }
        ]
      },
      first_byte: {
        totals: [
          { nameKey: 'trend.avgFirstByteTime', color: '#0ea5e9', area: 0.18, value: (p) => positiveOrNull(p.avg_first_byte_time_seconds) }
        ],
        models: [{ value: (m) => positiveOrNull(m.avg_first_byte_time_seconds) }]
      },
      duration: {
        totals: [
          { nameKey: 'trend.avgDuration', color: '#a855f7', area: 0.16, value: (p) => positiveOrNull(p.avg_duration_seconds) }
        ],
        models: [{ value: (m) => positiveOrNull(m.avg_duration_seconds) }]
      },
      tokens: {
        totals: [
          { nameKey: 'trend.inputTokens', color: '#3b82f6', value: (p) => p.input_tokens || 0 },
          { nameKey: 'trend.outputTokens', color: '#10b981', value: (p) => p.output_tokens || 0 },
          { nameKey: 'trend.cacheRead', color: '#f97316', value: (p) => p.cache_read_tokens || 0 },
          { nameKey: 'trend.cacheCreate', color: '#a855f7', value: (p) => p.cache_creation_tokens || 0 }
        ],
        models: [{ value: (m) => positiveOrNull((m.input_tokens || 0) + (m.output_tokens || 0)) }]
      },
      cost: {
        totals: [
          { nameKey: 'trend.totalCost', color: '#f97316', area: 0.16, value: (p) => p.total_cost || 0 }
        ],
        models: [{ value: (m) => positiveOrNull(m.total_cost) }]
      },
      rpm: {
        totals: [
          { name: 'RPM', color: '#3b82f6', area: 0.16, value: (p, bs) => {
            const total = (p.success || 0) + (p.error || 0);
            return total > 0 ? total * 60 / bs : 0;
          } }
        ],
        models: [{ value: (m, bs) => {
          const total = (m.success || 0) + (m.error || 0);
          return total > 0 ? total * 60 / bs : null;
        } }]
      },
      tps: {
        totals: [
          { nameKey: 'trend.inputTokens', color: '#3b82f6', value: (p, bs) => (Number(p.input_tokens) || 0) / bs },
          { nameKey: 'trend.outputTokens', color: '#10b981', value: (p, bs) => (Number(p.output_tokens) || 0) / bs },
          { nameKey: 'trend.cacheRead', color: '#f97316', value: (p, bs) => (Number(p.cache_read_tokens) || 0) / bs },
          { nameKey: 'trend.cacheCreate', color: '#a855f7', value: (p, bs) => (Number(p.cache_creation_tokens) || 0) / bs }
        ],
        models: [{ value: (m, bs) => {
          const total = (m.input_tokens || 0) + (m.output_tokens || 0);
          return total > 0 ? total / bs : null;
        } }]
      },
      cache_hit: {
        totals: [
          { nameKey: 'trend.cacheHitRate', color: '#f97316', area: 0.18, value: (p) => {
            const hit = p.cache_read_tokens || 0;
            const total = hit + (p.input_tokens || 0);
            return total > 0 ? (hit / total) * 100 : null;
          } }
        ],
        models: [{ value: (m) => {
          const hit = m.cache_read_tokens || 0;
          const total = hit + (m.input_tokens || 0);
          return total > 0 ? (hit / total) * 100 : null;
        } }]
      },
      error_rate: {
        totals: [
          { nameKey: 'trend.errorRate', color: '#ef4444', area: 0.16, value: (p) => {
            const total = (p.success || 0) + (p.error || 0);
            return total > 0 ? ((p.error || 0) / total) * 100 : null;
          } }
        ],
        models: [{ value: (m) => {
          const total = (m.success || 0) + (m.error || 0);
          return total > 0 ? ((m.error || 0) / total) * 100 : null;
        } }]
      }
    };

    function hexRgba(hex, alpha) {
      const n = parseInt(hex.slice(1), 16);
      return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha.toFixed(2)})`;
    }

    async function renderChart() {
      if (!window.trendData || !window.trendData.length) {
        renderTrendError();
        return;
      }

      // echarts 按需加载；老页面头仍直引 echarts.min.js 时 ensureECharts 缺席，
      // 此时 window.echarts 已就位直接放行，两头都断才落错误视图
      try { await window.ensureECharts?.(); } catch (_) {}
      if (!window.echarts) {
        renderTrendError();
        return;
      }

      // 显示图表容器
      document.getElementById('chart-loading').style.display = 'none';
      document.getElementById('chart-error').classList.add('hidden');
      document.getElementById('chart').classList.remove('hidden');

      // 初始化或获取 ECharts 实例
      const chartDom = document.getElementById('chart');
      if (!window.chartInstance) {
        window.chartInstance = echarts.init(chartDom, null, {
          renderer: 'canvas'
        });
        attachChartResizeObserver(chartDom);
      }

      // 时间轴用真实毫秒戳：刻度由 ECharts 贴整点、缩放后密度自适应，
      // 稀疏数据的位置也不再被 category 等距化失真
      const trendData = window.trendData;
      const dataLen = trendData.length;
      const bucketMs = (window.currentBucketSec || 600) * 1000;
      const tsMs = new Array(dataLen);
      for (let i = 0; i < dataLen; i++) {
        tsMs[i] = new Date(trendData[i].ts).getTime();
      }

      const noRequestRanges = computeNoRequestRanges(trendData);
      const markAreaData = noRequestRanges
        .filter(([start, end]) => (end - start + 1) >= 3) // 太短的空窗不要标，避免噪音
        .map(([start, end]) => ([
          { xAxis: tsMs[start] },
          { xAxis: tsMs[end] + bucketMs }
        ]));

      // 为每个可见模型序列生成颜色
      const modelColors = generateModelColors(window.visibleModels);

      // 准备series数据
      const series = [];
      const trendType = window.currentTrendType;

      // 保留旧缩放窗口跨全量重绘：setOption(notMerge) 会重置 dataZoom，
      // 这里在重建前读回上次的 ms 窗口；窗口贴着数据尾端视为"追最新"，
      // 数据变长后继续保持贴尾，否则按原窗口恢复（出界则丢弃）
      const showSlider = window.currentHours > 24;
      let zoomRestore = null;
      if (window.chartInstance && tsMs.length) {
        const dz = window.chartInstance.getOption()?.dataZoom?.[0];
        const dataStart = tsMs[0];
        const dataEnd = tsMs[dataLen - 1] + bucketMs;
        if (dz && Number.isFinite(dz.startValue) && Number.isFinite(dz.endValue)
            && dz.endValue > dataStart && dz.startValue < dataEnd) {
          const prevEnd = window._trendDataEnd || 0;
          const followTail = prevEnd > 0 && dz.endValue >= prevEnd - bucketMs * 0.5;
          zoomRestore = {
            startValue: Math.max(dz.startValue, dataStart),
            endValue: followTail ? dataEnd : Math.min(dz.endValue, dataEnd)
          };
        }
      }
      window._trendDataEnd = tsMs.length ? tsMs[dataLen - 1] + bucketMs : 0;

      // 按规格表展开总量线
      const spec = TREND_SERIES_SPECS[trendType];
      const bucketSec = window.currentBucketSec || 600;
      if (spec) {
        spec.totals.forEach(def => {
          const item = {
            name: def.nameKey ? t(def.nameKey) : def.name,
            type: 'line',
            smooth: 0.25,
            symbol: 'circle',
            symbolSize: 4,
            showSymbol: false,
            sampling: 'lttb',
            connectNulls: false,
            emphasis: { focus: 'series', showSymbol: true },
            itemStyle: { color: def.color },
            lineStyle: { width: 2, color: def.color, cap: 'round', join: 'round' },
            data: trendData.map((point, i) => [tsMs[i], def.value(point, bucketSec)])
          };
          if (def.area) {
            item.areaStyle = {
              color: new echarts.graphic.LinearGradient(0, 0, 0, 1, [
                { offset: 0, color: hexRgba(def.color, def.area) },
                { offset: 1, color: hexRgba(def.color, 0) }
              ])
            };
          }
          series.push(item);
        });
      }

      // 为每个可见模型添加对应趋势线（同一张规格表的模型口径）
      const visibleModelsArray = Array.from(window.visibleModels);

      if (spec) {
        for (let ci = 0; ci < visibleModelsArray.length; ci++) {
          const modelName = visibleModelsArray[ci];
          const color = modelColors[modelName];

          spec.models.forEach(def => {
            const data = new Array(dataLen);
            let hasData = false;
            let total = 0;

            for (let i = 0; i < dataLen; i++) {
              const models = trendData[i].models;
              const modelData = models ? models[modelName] : null;
              const v = modelData ? def.value(modelData, bucketSec) : (def.zeroFill ? 0 : null);
              data[i] = [tsMs[i], v];
              if (def.zeroFill) total += v;
              else if (v != null) hasData = true;
            }

            if (def.zeroFill ? total <= 0 : !hasData) return;

            series.push({
              name: def.nameKey ? t(def.nameKey, { model: modelName }) : modelName,
              drillModel: modelName,
              type: 'line',
              smooth: 0.25,
              symbol: 'none',
              sampling: 'lttb',
              connectNulls: false,
              emphasis: { focus: 'series' },
              itemStyle: { color: color },
              lineStyle: {
                width: 1.5, color: color, cap: 'round', join: 'round',
                ...(def.lineType ? { type: def.lineType } : {})
              },
              data: data
            });
          });
        }
      }


      // 首字响应/总耗时：加参考线（P50/P90）和极值标记，便于读趋势/看尖峰
      if (trendType === 'first_byte' || trendType === 'duration') {
        enhanceLatencySeries(series);
      }

      series.forEach(s => {
        // 系列点可点击钻取日志页——指针样式给提示
        s.cursor = 'pointer';
        // connectNulls:false 下孤立点渲染为空——序列有空洞时把点显出来
        if (Array.isArray(s.data) && s.data.some(d => d && d[1] == null)) {
          s.showSymbol = true;
          if (s.symbol === 'none') s.symbol = 'circle';
          s.symbolSize = s.symbolSize || 4;
        }
      });

      // ECharts 配置
      const legendHeight = 28;
      const gridTopPx = legendHeight + 18;
      const gridBottomPx = showSlider ? 70 : 48;
      const gridRightPx = (trendType === 'first_byte' || trendType === 'duration') ? 44 : 28;
      const multiDay = window.currentHours > 24;
      const showSec = (window.currentBucketSec || 600) < 60;
      const yAxisScale = (trendType === 'first_byte' || trendType === 'duration');
      const useLatencyAxis = (trendType === 'first_byte' || trendType === 'duration');
      const yAxisMin = useLatencyAxis ? latencyAxisMin : 0;
      const yAxisMax = useLatencyAxis ? latencyAxisMax : (trendType === 'cache_hit' ? 100 : null);
      const chartTheme = getTrendChartTheme();
      const hasRequests = trendData.some(p => (p.success || 0) + (p.error || 0) > 0);

      const chartType = window.currentTrendChartType === 'bar' ? 'bar' : 'line';
      const zoomSpan = zoomRestore
        ? { startValue: zoomRestore.startValue, endValue: zoomRestore.endValue }
        : {};
      const option = {
        // 首渲保留一次淡入，数据刷新/切图即时到位——自动刷新下重画不扫动
        animation: !window._trendChartPainted,
        backgroundColor: 'transparent',
        title: {
          show: false
        },
        tooltip: {
          trigger: 'axis',
          confine: true,
          backgroundColor: chartTheme.tooltipBg,
          borderColor: chartTheme.tooltipBorder,
          borderWidth: 1,
          textStyle: {
            color: chartTheme.tooltipText,
            fontSize: 12
          },
          axisPointer: {
            type: chartType === 'bar' ? 'shadow' : 'cross',
            crossStyle: {
              color: chartTheme.mutedText,
              width: 1,
              type: 'dashed'
            }
          },
          formatter: function(params) {
            const dataIndex = params && params.length ? params[0].dataIndex : null;
            const point = (dataIndex != null && window.trendData && window.trendData[dataIndex]) ? window.trendData[dataIndex] : null;
            const totalReq = point ? ((point.success || 0) + (point.error || 0)) : null;

            let html = `<div style="font-weight: 600; margin-bottom: 6px;">${fmtBucketTime(params[0].axisValue, multiDay, showSec, false)}</div>`;
            if (totalReq != null) {
              const hint = totalReq === 0
                ? `<span style="color: ${chartTheme.mutedText};">${t('trend.noRequestInPeriod')}</span>`
                : '';
              html += `<div style="margin-bottom: 8px; color: ${chartTheme.text}; font-size: 12px;">${t('trend.requestCount')}: ${totalReq}${hint}</div>`;
            }
            params.forEach(param => {
              const color = param.color;
              const value = Array.isArray(param.value) ? param.value[1] : param.value;
              if (value == null) return; // 无数据序列不打 N/A 行

              html += `
                <div style="display: flex; align-items: center; gap: 8px; margin: 4px 0;">
                  <span style="display: inline-block; width: 10px; height: 10px; background: ${color}; border-radius: 50%;"></span>
                  <span>${escapeHtml(param.seriesName)}: ${formatTrendValue(window.currentTrendType, value)}</span>
                </div>
              `;
            });
            html += `<div style="margin-top: 6px; color: ${chartTheme.mutedText}; font-size: 11px;">${t('trend.drillHint')}</div>`;
            return html;
          }
        },
        legend: {
          data: series.map(s => s.name),
          // 回灌图例开关状态；多余的键（其他视图/旧语言名）无害
          selected: loadLegendState(),
          top: 10,
          left: 16,
          right: 48, // 右上角留给导出按钮
          textStyle: {
            color: chartTheme.mutedText,
            fontSize: 11
          },
          itemWidth: 20,
          itemHeight: 8,
          itemGap: 12,
          type: 'scroll',
          pageIconColor: chartTheme.mutedText,
          pageIconInactiveColor: chartTheme.axisLine,
          pageIconSize: 12,
          pageTextStyle: {
            color: chartTheme.mutedText,
            fontSize: 10
          }
        },
        toolbox: {
          right: 10,
          top: 6,
          feature: {
            saveAsImage: {
              title: t('trend.saveAsImage'),
              name: 'devin-trend',
              backgroundColor: chartTheme.surface,
              pixelRatio: 2,
              iconStyle: { borderColor: chartTheme.mutedText }
            }
          }
        },
        grid: {
          left: 16,
          right: gridRightPx,
          bottom: gridBottomPx,
          top: gridTopPx,
          containLabel: true
        },
        xAxis: {
          type: 'time',
          boundaryGap: chartType === 'bar',
          min: tsMs[0],
          max: tsMs[dataLen - 1] + bucketMs,
          axisLine: {
            lineStyle: {
              color: chartTheme.axisLine
            }
          },
          axisLabel: {
            color: chartTheme.mutedText,
            fontSize: 11,
            rotate: window.innerWidth < 640 ? 45 : 0,
            hideOverlap: true,
            formatter: (v) => fmtBucketTime(v, multiDay, showSec || v % 60000 !== 0, 'boundary')
          },
          splitLine: {
            show: true,
            lineStyle: {
              color: chartTheme.splitLine,
              type: 'dashed'
            }
          }
        },
        yAxis: {
          type: 'value',
          scale: yAxisScale,
          min: yAxisMin,
          max: yAxisMax,
          axisLine: {
            lineStyle: {
              color: chartTheme.axisLine
            }
          },
          axisLabel: {
            color: chartTheme.mutedText,
            fontSize: 11,
            formatter: (value) => formatTrendValue(trendType, value, true)
          },
          splitLine: {
            lineStyle: {
              color: chartTheme.splitLine,
              type: 'dashed'
            }
          }
        },
        series: applyNoRequestMarkArea(applyTrendChartType(series, chartType), markAreaData),
        // inside 缩放常驻（滚轮/拖拽），滑块只在跨天长窗出现——短窗用不上还吃高度；
        // zoomSpan 回写上次缩放窗口，自动刷新/切图不丢视野
        dataZoom: [
          {
            type: 'inside',
            minValueSpan: bucketMs * 5,
            ...zoomSpan
          },
          ...(showSlider ? [{
            show: true,
            type: 'slider',
            bottom: 18,
            height: 20,
            minValueSpan: bucketMs * 5,
            ...zoomSpan,
            borderColor: chartTheme.axisLine,
            backgroundColor: chartTheme.surfaceMuted,
            fillerColor: 'rgba(59, 130, 246, 0.16)',
            handleStyle: {
              color: '#3b82f6',
              borderColor: '#3b82f6'
            },
            textStyle: {
              color: chartTheme.mutedText,
              fontSize: 10
            }
          }] : [])
        ],
        // 全空窗口直接给文字占位，不留一张"零值平线"的假图
        graphic: hasRequests ? [] : [{
          type: 'text',
          left: 'center',
          top: '55%',
          silent: true,
          style: {
            text: t('trend.noDataInRange'),
            fill: chartTheme.mutedText,
            fontSize: 13
          }
        }],
        animationDuration: 450,
        animationEasing: 'cubicInOut'
      };

      // 设置配置并渲染
      window.chartInstance.setOption(option, true); // true 表示不合并，全量更新
      window._trendChartPainted = true;
      window._trendSeriesDefs = series; // 点击钻取时回查 drillModel
      bindTrendDrill(window.chartInstance);
      bindTrendLegendState(window.chartInstance);
    }

    function applyTrendChartType(series, chartType) {
      if (chartType !== 'bar') return series;

      return series.map(item => {
        const isDashedLine = item.lineStyle && item.lineStyle.type === 'dashed';
        const next = {
          ...item,
          type: 'bar',
          barMaxWidth: 18,
          itemStyle: {
            ...(item.itemStyle || {}),
            opacity: isDashedLine ? 0.48 : 0.78,
            borderRadius: [3, 3, 0, 0]
          }
        };

        delete next.smooth;
        delete next.symbol;
        delete next.symbolSize;
        delete next.showSymbol;
        delete next.sampling;
        delete next.connectNulls;
        delete next.lineStyle;
        delete next.areaStyle;
        return next;
      });
    }

    function setTrendChartType(chartType) {
      if (chartType !== 'line' && chartType !== 'bar') return;
      if (window.currentTrendChartType === chartType) return;

      window.currentTrendChartType = chartType;
      updateTrendChartTypeButtons();
      renderChart();
    }

    function updateTrendChartTypeButtons() {
      document.querySelectorAll('.trend-chart-type-btn').forEach(button => {
        const active = button.dataset.chartType === window.currentTrendChartType;
        button.classList.toggle('active', active);
        button.setAttribute('aria-pressed', active ? 'true' : 'false');
      });
    }

    function attachChartResizeObserver(chartDom) {
      if (!chartDom) return;
      if (window.chartResizeObserver) return;
      if (typeof ResizeObserver === 'undefined') return;

      let raf = 0;
      window.chartResizeObserver = new ResizeObserver(() => {
        if (!window.chartInstance) return;
        if (raf) cancelAnimationFrame(raf);
        raf = requestAnimationFrame(() => {
          try { window.chartInstance.resize(); } catch (_) {}
        });
      });

      window.chartResizeObserver.observe(chartDom);
    }

    // 标注无请求区间：视觉上解释“断线/空窗”，同时不篡改数据语义
    function computeNoRequestRanges(trendData) {
      const ranges = [];
      let start = -1;
      for (let i = 0; i < trendData.length; i++) {
        const p = trendData[i] || {};
        const total = (p.success || 0) + (p.error || 0);
        if (total === 0) {
          if (start === -1) start = i;
        } else if (start !== -1) {
          ranges.push([start, i - 1]);
          start = -1;
        }
      }
      if (start !== -1) ranges.push([start, trendData.length - 1]);
      return ranges;
    }

    function applyNoRequestMarkArea(series, markAreaData) {
      if (!markAreaData || markAreaData.length === 0) return series;
      if (!series || series.length === 0) return series;

      // 只挂在第一条 series 上，避免重复渲染造成性能和视觉噪音
      const first = { ...series[0] };
      first.markArea = {
        silent: true,
        itemStyle: {
          color: 'rgba(148, 163, 184, 0.08)'
        },
        label: {
          show: false
        },
        data: markAreaData
      };
      return [first, ...series.slice(1)];
    }

    function latencyAxisMin(value) {
      if (!value) return 0;
      const min = Number.isFinite(value.min) ? value.min : 0;
      const max = Number.isFinite(value.max) ? value.max : 0;
      const range = Math.max(0, max - min);
      const pad = range > 0 ? range * 0.08 : max * 0.08;
      return Math.max(0, min - pad);
    }

    function latencyAxisMax(value) {
      if (!value) return null;
      const min = Number.isFinite(value.min) ? value.min : 0;
      const max = Number.isFinite(value.max) ? value.max : 0;
      const range = Math.max(0, max - min);
      const pad = range > 0 ? range * 0.08 : Math.max(10, max * 0.08);
      return max + pad;
    }

    function enhanceLatencySeries(series) {
      if (!series || series.length === 0) return;
      const base = series[0];
      if (!base || !Array.isArray(base.data)) return;
      const chartTheme = getTrendChartTheme();

      const values = base.data
        .filter(d => Array.isArray(d) && typeof d[1] === 'number' && Number.isFinite(d[1]) && d[1] > 0)
        .map(d => d[1]);
      if (values.length < 5) return;

      const p50 = percentile(values, 0.50);
      const p90 = percentile(values, 0.90);

      base.markLine = {
        silent: true,
        symbol: 'none',
        lineStyle: {
          width: 1,
          type: 'dashed',
          color: 'rgba(100, 116, 139, 0.55)'
        },
        label: {
          color: chartTheme.strongText,
          fontSize: 11,
          position: 'insideEndTop',
          padding: [2, 6],
          borderRadius: 4,
          backgroundColor: chartTheme.surface,
          borderColor: chartTheme.axisLine,
          borderWidth: 1,
          formatter: (p) => {
            const v = p && p.value != null ? p.value : null;
            if (v == null) return '';
            return `${p.name}: ${Number(v).toFixed(1)}s`;
          }
        },
        data: [
          { name: 'P50', yAxis: p50 },
          { name: 'P90', yAxis: p90 }
        ]
      };

      base.markPoint = {
        symbol: 'pin',
        symbolSize: 34,
        label: {
          color: chartTheme.strongText,
          fontSize: 10,
          formatter: (p) => (p && p.value != null ? `${Number(p.value).toFixed(1)}s` : '')
        },
        itemStyle: {
          color: 'rgba(14, 165, 233, 0.85)'
        },
        data: [
          { type: 'max', name: 'MAX' }
        ]
      };
    }

    function percentile(values, p) {
      if (!values || values.length === 0) return 0;
      const sorted = values.slice().sort((a, b) => a - b);
      const clamped = Math.min(1, Math.max(0, p));
      const idx = (sorted.length - 1) * clamped;
      const lo = Math.floor(idx);
      const hi = Math.ceil(idx);
      if (lo === hi) return sorted[lo];
      const w = idx - lo;
      return sorted[lo] * (1 - w) + sorted[hi] * w;
    }

    // 间隔标签：入参是秒，>=1min 折算成一位小数的分/时（整齐档整除后无小数）
    function formatInterval(sec) {
      if (sec >= 3600) return Math.round(sec / 360) / 10 + ' ' + t('trend.hour');
      if (sec >= 60) return Math.round(sec / 6) / 10 + ' ' + t('trend.minute');
      return sec + ' ' + t('trend.second');
    }

    // fmtBucketTime：时间轴刻度与 tooltip 头的桶时刻文本。跨天时轴刻度
    // 只在 0 点露日期（dateMode='boundary'），tooltip 头总是带日期（'always'）。
    function fmtBucketTime(ms, multiDay, withSec, dateMode) {
      const d = new Date(ms);
      let hm = pad(d.getHours()) + ':' + pad(d.getMinutes());
      if (withSec) hm += ':' + pad(d.getSeconds());
      if (!multiDay) return hm;
      const date = (d.getMonth() + 1) + '/' + d.getDate();
      if (dateMode === 'boundary') {
        return (d.getHours() === 0 && d.getMinutes() === 0 && d.getSeconds() === 0) ? date : hm;
      }
      return date + ' ' + hm;
    }

    // formatTrendValue：tooltip 行值与 y 轴刻度共用一套数值格式；
    // compact=true 给轴刻度（count 用 K/M、rpm 不带后缀），防两处漂移。
    function formatTrendValue(trendType, value, compact) {
      if (trendType === 'first_byte' || trendType === 'duration') {
        return value.toFixed(1) + 's';
      }
      if (trendType === 'cost') {
        if (value >= 1) return '$' + value.toFixed(2);
        if (value >= 0.01) return '$' + value.toFixed(4);
        return value > 0 ? '$' + value.toFixed(6) : '$0.00';
      }
      if (trendType === 'tokens') {
        if (value >= 1000000) return (value / 1000000).toFixed(1) + 'M';
        if (value >= 1000) return (value / 1000).toFixed(1) + 'K';
        return String(value);
      }
      if (trendType === 'rpm') {
        return compact ? value.toFixed(1) : value.toFixed(1) + '/min';
      }
      if (trendType === 'tps') {
        if (value >= 1000000) return (value / 1000000).toFixed(1) + 'M/s';
        if (value >= 1000) return (value / 1000).toFixed(1) + 'K/s';
        return value.toFixed(1) + '/s';
      }
      if (trendType === 'cache_hit' || trendType === 'error_rate') {
        return value.toFixed(1) + '%';
      }
      if (compact) {
        if (value >= 1000000) return (value / 1000000) + 'M';
        if (value >= 1000) return (value / 1000) + 'K';
      }
      return String(Math.round(value));
    }

    // 点击数据点 → 新窗口打开该桶时间窗的日志页；模型序列构建时挂
    // drillModel（seriesName 是格式化文案不能反解），总量线只带时间窗
    function bindTrendDrill(chart) {
      if (chart._trendDrillBound) return;
      chart._trendDrillBound = true;
      chart.on('click', (params) => {
        if (params.componentType !== 'series' || !Array.isArray(params.value)) return;
        const startMs = Math.floor(params.value[0]);
        if (!Number.isFinite(startMs)) return;
        const q = new URLSearchParams({
          range: 'custom',
          start_time: String(startMs),
          end_time: String(startMs + (window.currentBucketSec || 600) * 1000)
        });
        const def = (window._trendSeriesDefs || [])[params.seriesIndex];
        if (def && def.drillModel) q.set('model', def.drillModel);
        if (window.currentAPI) q.set('api', window.currentAPI);
        window.open('/web/logs.html?' + q.toString(), '_blank');
      });
    }

    // 工具函数
    function pad(n) {
      return (n < 10 ? '0' : '') + n;
    }
    
    // ===== 模型序列数据缓存（避免重复遍历 trendData）=====
    // 缓存结构: { modelName: { success, error, hasData } }
    window._modelDataCache = null;

    // 构建模型数据缓存：一次遍历 trendData，统计所有模型
    function buildModelDataCache(trendData) {
      const cache = {};
      if (!trendData || !trendData.length) {
        window._modelDataCache = cache;
        return cache;
      }

      // 单次遍历：收集所有模型的统计数据
      for (let i = 0, len = trendData.length; i < len; i++) {
        const models = trendData[i].models;
        if (!models) continue;

        const names = Object.keys(models);
        for (let j = 0, nLen = names.length; j < nLen; j++) {
          const name = names[j];
          const mData = models[name];
          if (!cache[name]) {
            cache[name] = { success: 0, error: 0 };
          }
          cache[name].success += mData.success || 0;
          cache[name].error += mData.error || 0;
        }
      }

      // 计算 hasData 标记
      const cacheNames = Object.keys(cache);
      for (let i = 0, len = cacheNames.length; i < len; i++) {
        const name = cacheNames[i];
        cache[name].hasData = (cache[name].success + cache[name].error) > 0;
      }

      window._modelDataCache = cache;
      return cache;
    }

    // 检查模型是否有数据（使用缓存）
    function hasModelData(modelName, trendData) {
      // 如果缓存不存在或为空，先构建缓存
      if (!window._modelDataCache) {
        buildModelDataCache(trendData);
      }

      const cached = window._modelDataCache[modelName];
      return cached ? cached.hasData : false;
    }

    // 生成模型序列颜色（避免与总体趋势线颜色冲突）
    // 总体趋势线保留颜色: #10b981(绿), #ef4444(红), #0ea5e9(天蓝), #a855f7(紫), #f97316(橙)
    // 暗色主题整体提亮一档：同序号保持同色系，模型↔颜色对应关系跨主题不漂移
    const MODEL_COLORS_LIGHT = [
      '#3b82f6', // 蓝色
      '#06b6d4', // 青色
      '#14b8a6', // 绿松色
      '#84cc16', // 黄绿色
      '#eab308', // 黄色
      '#fb923c', // 浅橙色
      '#ec4899', // 粉色
      '#6366f1', // 靛蓝色
      '#8b5cf6', // 淡紫色
      '#22c55e', // 亮绿色
      '#f43f5e', // 玫红色
      '#0891b2', // 深青色
      '#65a30d', // 橄榄绿
      '#ca8a04', // 金黄色
      '#dc2626'  // 深红色
    ];
    const MODEL_COLORS_DARK = [
      '#60a5fa', // 蓝
      '#22d3ee', // 青
      '#2dd4bf', // 绿松
      '#a3e635', // 黄绿
      '#facc15', // 黄
      '#fdba74', // 浅橙
      '#f472b6', // 粉
      '#818cf8', // 靛蓝
      '#a78bfa', // 淡紫
      '#4ade80', // 亮绿
      '#fb7185', // 玫红
      '#06b6d4', // 深青→青
      '#84cc16', // 橄榄→黄绿
      '#eab308', // 金黄→黄
      '#ef4444'  // 深红→红
    ];

    function generateModelColors(models) {
      const colors = document.documentElement.dataset.resolvedTheme === 'dark'
        ? MODEL_COLORS_DARK
        : MODEL_COLORS_LIGHT;

      const modelColors = {};
      const modelArray = Array.from(models);
      const colorsLen = colors.length;

      for (let i = 0, len = modelArray.length; i < len; i++) {
        modelColors[modelArray[i]] = colors[i % colorsLen];
      }

      return modelColors;
    }

    // 更新模型筛选器 - 显示所有有数据的模型
    // 优化：直接使用缓存获取有数据的模型，避免重复遍历 trendData
    function updateModelFilter() {
      const filterList = document.getElementById('model-filter-list');
      if (!filterList) return;

      // 直接从缓存获取所有有数据的模型名称
      const allModelNames = new Set();

      // 使用缓存：O(1) 查找
      if (window._modelDataCache) {
        const cachedNames = Object.keys(window._modelDataCache);
        for (let i = 0, len = cachedNames.length; i < len; i++) {
          const name = cachedNames[i];
          if (window._modelDataCache[name].hasData) {
            allModelNames.add(name);
          }
        }
      }

      // 生成颜色映射
      const modelColors = generateModelColors(allModelNames);

      // 使用 DocumentFragment 批量插入 DOM
      const fragment = document.createDocumentFragment();
      const sortedNames = Array.from(allModelNames).sort();

      for (let i = 0, len = sortedNames.length; i < len; i++) {
        const modelName = sortedNames[i];
        const isVisible = window.visibleModels.has(modelName);
        const displayName = modelName === '' ? t('trend.unknown') : modelName;

        const item = TemplateEngine.render('tpl-model-filter-item', {
          checkedClass: isVisible ? 'checked' : '',
          color: modelColors[modelName],
          displayName: displayName
        });
        if (item) {
          item.addEventListener('click', () => {
            toggleModel(modelName);
          });
          fragment.appendChild(item);
        }
      }

      filterList.innerHTML = '';
      filterList.appendChild(fragment);
    }

    // 切换模型序列显示/隐藏
    function toggleModel(modelName) {
      if (window.visibleModels.has(modelName)) {
        window.visibleModels.delete(modelName);
      } else {
        window.visibleModels.add(modelName);
      }

      updateModelFilter();
      renderChart();
      persistModelState();
    }

    // 全选模型 - 选择所有有数据的模型
    // 优化：直接使用缓存获取有数据的模型
    function selectAllModels() {
      if (window._modelDataCache) {
        const names = Object.keys(window._modelDataCache);
        for (let i = 0, len = names.length; i < len; i++) {
          const name = names[i];
          if (window._modelDataCache[name].hasData) {
            window.visibleModels.add(name);
          }
        }
      }

      updateModelFilter();
      renderChart();
      persistModelState();
    }

    // 清空选择
    function clearAllModels() {
      window.visibleModels.clear();

      updateModelFilter();
      renderChart();
      persistModelState();
    }

    // 切换模型筛选器显示/隐藏
    function toggleModelFilter() {
      const dropdown = document.getElementById('model-filter-dropdown');
      if (!dropdown) return;

      const isVisible = !dropdown.classList.contains('hidden');
      dropdown.classList.toggle('hidden', isVisible);

      if (!isVisible) {
        // 点击外部关闭
        setTimeout(() => {
          document.addEventListener('click', closeModelFilter, true);
        }, 10);
      }
    }

    function bindModelFilterControls() {
      const modelFilterToggle = document.getElementById('btn-model-filter-toggle');
      if (modelFilterToggle) {
        modelFilterToggle.addEventListener('click', () => {
          toggleModelFilter();
        });
      }

      const selectAllBtn = document.getElementById('btn-select-all-models');
      if (selectAllBtn) {
        selectAllBtn.addEventListener('click', () => {
          selectAllModels();
        });
      }

      const clearAllBtn = document.getElementById('btn-clear-all-models');
      if (clearAllBtn) {
        clearAllBtn.addEventListener('click', () => {
          clearAllModels();
        });
      }
    }

    function closeModelFilter(event) {
      const dropdown = document.getElementById('model-filter-dropdown');
      const container = document.querySelector('.channel-filter-container');

      if (!dropdown || !container) return;

      if (!container.contains(event.target)) {
        dropdown.classList.add('hidden');
        document.removeEventListener('click', closeModelFilter, true);
      }
    }

    // 持久化模型序列可见状态
    function persistModelState() {
      try {
        const visibleArray = Array.from(window.visibleModels);
        localStorage.setItem('trend.visibleModels', JSON.stringify(visibleArray));
      } catch (_) {}
    }

    // 恢复模型序列可见状态
    function restoreModelState() {
      try {
        const saved = localStorage.getItem('trend.visibleModels');
        if (saved) {
          const visibleArray = JSON.parse(saved);
          window.visibleModels = new Set(visibleArray);
        }
      } catch (_) {}
    }

    // 图例开关状态持久化：ECharts legend 点击只在实例内生效，而 setOption
    // (notMerge) 每次重绘都把 selected 重置——自动刷新/切图/刷新页面都会把
    // 点掉的线弹回来。这里把 selected map 落 localStorage，渲染时回灌。
    const TREND_LEGEND_KEY = 'trend.legendSelected';

    function loadLegendState() {
      try {
        const saved = localStorage.getItem(TREND_LEGEND_KEY);
        return saved ? JSON.parse(saved) : null;
      } catch (_) {
        return null;
      }
    }

    function persistLegendState(selected) {
      try {
        // legendselectchanged 只携带当前视图内的序列名，必须合并而非覆盖——
        // 否则在 tokens 视图点一次图例会丢掉 count 视图藏的 "X 成功/失败"
        const merged = { ...(loadLegendState() || {}), ...selected };
        localStorage.setItem(TREND_LEGEND_KEY, JSON.stringify(merged));
      } catch (_) {}
    }

    function bindTrendLegendState(chart) {
      if (chart._trendLegendBound) return;
      chart._trendLegendBound = true;
      chart.on('legendselectchanged', (params) => {
        persistLegendState(params.selected || {});
      });
    }

    // 自动刷新：工具栏 select 控制间隔（关闭/10s/30s/1min/5min），默认 60s，
    // 选择存 localStorage，切换即重建定时器；页面隐藏时跳过 tick。
    const TREND_REFRESH_KEY = 'trend.refreshSec';
    const TREND_REFRESH_OPTIONS = [0, 10, 30, 60, 300];
    const TREND_REFRESH_DEFAULT = 60;
    let trendRefreshTimer = null;

    // 数据粒度手动档：localStorage 存秒数，0/未存 = 自动分档
    const TREND_BUCKET_KEY = 'trend.bucketSec';
    const TREND_BUCKET_OPTIONS = [10, 30, 60, 300, 600, 1800, 3600, 7200, 21600];

    function initTrendBucketControl() {
      const select = document.getElementById('f_bucket_sec');
      if (!select) return;
      try { localStorage.removeItem('trend.bucketMin'); } catch (_) {}
      const saved = Number(localStorage.getItem(TREND_BUCKET_KEY));
      select.value = TREND_BUCKET_OPTIONS.includes(saved) ? String(saved) : '0';
      select.addEventListener('change', () => {
        try { localStorage.setItem(TREND_BUCKET_KEY, select.value); } catch (_) {}
        loadData();
      });
    }

    function currentTrendRefreshSec() {
      try {
        const raw = localStorage.getItem(TREND_REFRESH_KEY);
        if (raw === null) return TREND_REFRESH_DEFAULT;
        const v = Number(raw);
        if (TREND_REFRESH_OPTIONS.includes(v)) return v;
      } catch (_) {}
      return TREND_REFRESH_DEFAULT;
    }

    function startTrendRefresh() {
      if (trendRefreshTimer !== null) {
        clearInterval(trendRefreshTimer);
        trendRefreshTimer = null;
      }
      const sec = currentTrendRefreshSec();
      if (sec <= 0) return;
      trendRefreshTimer = setInterval(() => {
        if (document.hidden) return;
        loadData(true);
        loadWarmStatus();
      }, sec * 1000);
    }

    function initTrendRefreshControl() {
      const select = document.getElementById('f_refresh_interval');
      if (select) {
        select.value = String(currentTrendRefreshSec());
        select.addEventListener('change', () => {
          try { localStorage.setItem(TREND_REFRESH_KEY, select.value); } catch (_) {}
          startTrendRefresh();
        });
      }
      startTrendRefresh();
    }

    // 页面初始化
    window.initPageBootstrap({
      topbarKey: 'trend',
      run: async () => {
      restoreState();
      restoreModelState();
      applyRangeUI();

      bindToggles();
      bindModelFilterControls();

      // 模型选项与令牌选项互不依赖；保温摘要走 /admin 端点，失败自行隐藏
      const [, authTokens] = await Promise.all([
        loadModels(),
        window.initAuthTokenFilter({
          selectId: 'f_auth_token',
          value: window.currentAuthToken,
          loadOptions: {
            tokenPrefix: t('trend.tokenPrefix'),
            restoreValue: window.currentAuthToken
          }
        }),
        loadWarmStatus()
      ]);
      window.authTokens = authTokens;

      // 数据加载（依赖 model/token select 已填充）
      loadData();

      // 修复：全局注册resize监听器（仅一次，避免内存泄漏）
      window.addEventListener('resize', () => {
        if (window.chartInstance) {
          window.chartInstance.resize();
        }
      });

      window.addEventListener('ccload:themechange', () => {
        if (window.chartInstance && window.trendData && window.trendData.length) {
          updateModelFilter();
          renderChart();
        }
      });

      // 定期刷新数据（间隔由工具栏 select 控制，默认 60s）
      initTrendRefreshControl();
      initTrendBucketControl();
      }
    });

    function bindToggles() {
      const trendChartTypeGroup = document.getElementById('trend-chart-type-group');
      updateTrendChartTypeButtons();
      trendChartTypeGroup?.addEventListener('click', (event) => {
        const button = event.target.closest('.trend-chart-type-btn');
        if (!button || !trendChartTypeGroup.contains(button)) return;
        setTrendChartType(button.dataset.chartType);
      });

      // 指标切换（下拉，searchable-select 已增强；回填在 applyRangeUI）
      const trendTypeSelect = document.getElementById('f_trend_type');
      if (trendTypeSelect) {
        trendTypeSelect.addEventListener('change', (e) => {
          window.currentTrendType = e.target.value || 'first_byte';
          persistState();
          renderChart();
        });
      }

      // 模型选择器
      const modelSelect = document.getElementById('f_model');
      if (modelSelect) {
        modelSelect.addEventListener('change', (e) => {
          window.currentModel = e.target.value || '';
          persistState();
          loadData();
        });
      }

      const apiSelect = document.getElementById('f_api');
      if (apiSelect) {
        apiSelect.addEventListener('change', (e) => {
          window.currentAPI = e.target.value || '';
          persistState();
          loadData();
        });
      }

      // 令牌选择器
      const tokenSelect = document.getElementById('f_auth_token');
      if (tokenSelect) {
        tokenSelect.addEventListener('change', (e) => {
          window.currentAuthToken = e.target.value || '';
          persistState();
          loadData();
        });
      }

      // 筛选按钮
      const btnFilter = document.getElementById('btn_filter');
      if (btnFilter) {
        btnFilter.addEventListener('click', () => {
          loadData();
        });
      }

      document.getElementById('btn_clear_filters')?.addEventListener('click', resetTrendFilters);
    }

    async function resetTrendFilters() {
      window.currentModel = '';
      window.currentAPI = '';
      window.currentAuthToken = '';
      window.applyFilterControlValues({ range: 'today' }, {
        range: 'f_hours',
        model: 'f_model',
        api: 'f_api',
        authToken: 'f_auth_token'
      });
      await handleTrendRangeChange('today');
    }

    async function handleTrendRangeChange(nextRange, customRange) {
      const range = nextRange || 'today';
      window.currentRange = range;
      if (range === 'custom') {
        currentTrendCustomTimeRange = normalizeTrendCustomTimeRange(customRange);
      } else {
        currentTrendCustomTimeRange = null;
      }
      persistState();
      await loadModels(range);
      loadData();
    }

    function persistState() {
      try {
        window.persistFilterState({
          key: TREND_FILTER_KEY,
          values: getTrendFilters(),
          pathname: location.pathname,
          fields: TREND_FILTER_FIELDS,
          historyMethod: 'replaceState'
        });
      } catch (_) {}
    }

    function restoreState() {
      try {
        const savedFilters = loadSavedTrendFilters();
        const restoredFilters = window.FilterState.restore({
          search: location.search,
          savedFilters,
          fields: TREND_FILTER_FIELDS
        });

        // 恢复时间范围 (默认"本日")
        const validRanges = window.getDateRangePresets
          ? window.getDateRangePresets({ includeCustom: true }).map((range) => range.value)
          : ['today'];
        window.currentRange = validRanges.includes(restoredFilters.range) ? restoredFilters.range : 'today';
        currentTrendCustomTimeRange = window.currentRange === 'custom'
          ? normalizeTrendCustomTimeRange(restoredFilters)
          : null;
        if (window.currentRange === 'custom' && !currentTrendCustomTimeRange) {
          window.currentRange = 'today';
        }

        // 恢复趋势类型
        window.currentTrendType = 'first_byte';
        if (['count', 'rpm', 'tps', 'error_rate', 'first_byte', 'duration', 'tokens', 'cost', 'cache_hit'].includes(restoredFilters.trendType)) {
          window.currentTrendType = restoredFilters.trendType;
        }

        // 恢复模型选择
        window.currentModel = restoredFilters.model || '';

        // 恢复入口端点
        window.currentAPI = restoredFilters.api || '';
        const apiSelect = document.getElementById('f_api');
        if (apiSelect) {
          apiSelect.value = window.currentAPI;
        }

        // 恢复令牌选择
        window.currentAuthToken = restoredFilters.authToken || '';
      } catch (_) {}
    }

    function applyRangeUI() {
      window.initSavedDateRangeFilter({
        selectId: 'f_hours',
        defaultValue: 'today',
        restoredValue: window.currentRange,
        includeCustom: true,
        customRange: currentTrendCustomTimeRange,
        customPickerContainerId: 'f_hours_custom_range_host',
        onChange: handleTrendRangeChange
      });

      // 指标下拉回填恢复值；change 事件让 searchable-select 同步显示
      // （此刻 bindToggles 还没挂监听，dispatch 不会触发 persist/render）
      const trendTypeSelect = document.getElementById('f_trend_type');
      if (trendTypeSelect) {
        trendTypeSelect.value = window.currentTrendType;
        trendTypeSelect.dispatchEvent(new Event('change', { bubbles: true }));
      }
    }

    window.i18n?.onLocaleChange?.(() => {
      if (window.chartInstance && !document.getElementById('chart').classList.contains('hidden')) renderChart();
    });

    // 注销功能（已由 ui.js 的 onLogout 统一处理）
