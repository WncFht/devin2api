# 管理面板前端调研笔记

调研对象：sub2api、CLIProxyAPI（CPA）、ccLoad、aio-coding-hub（HelpAIO 系）。目的是给 devin-2api 的 `/panel` 管理面板找可借鉴的设计与实现模式。本文记录每个项目的架构、值得参考的具体做法（含源码位置），以及本项目已吸收和暂未吸收的部分。

调研时的本地副本：`/tmp/sub2api`、`/tmp/cliproxyapi-src`、`/tmp/ccload`、`/tmp/aio-coding-hub`（/tmp 可能被清理，届时重新 clone 同名仓库即可）。

## 四个项目的形态对比

| 项目           | 技术栈                               | 交付形态                                            | 与我们的相似度                           |
| -------------- | ------------------------------------ | --------------------------------------------------- | ---------------------------------------- |
| sub2api        | Vue3 + Pinia + 独立 frontend/        | SPA 构建产物                                        | 功能相似（订阅/额度/用量管理），形态不同 |
| CLIProxyAPI    | React + TS + Vite（独立仓库）        | 打包成单个 `management.html`，由 release asset 下发 | 功能最相似（纯代理管理面），形态不同     |
| ccLoad         | 原生 JS + vendored ECharts，Go embed | `web/` 目录直接 embed 进二进制                      | **形态完全一致**，最值得逐文件抄         |
| aio-coding-hub | Tauri + React + shadcn               | 桌面应用                                            | 信息密度和组件设计参考价值最高           |

结论：我们选择的「原生 JS 模块 + vendored ECharts + go:embed + 侧栏 tab」路线与 ccLoad 同构，不需要引入构建链。sub2api 和 aio 的价值在组件设计而非架构。

## ccLoad（形态一致，逐文件可抄）

源码：`/tmp/ccload/web/assets/js/`，重点文件及行数：

- `ui.js`（2586 行）— 共享 UI 工具层：toast、confirm、badge、格式化。我们对应 `core.js`，规模小一个量级，不要照搬其体量，只取模式。
- `trend.js`（1792 行）— 趋势图页。**最值钱的参考**：
    - `markArea` 把连续无请求时段染灰——「没流量」和「正常」一眼可分（已实现到我们 `charts.js` 的 `gapMark`）。
    - 延迟图 `markLine` 均值虚线 + `markPoint` 峰值 pin（已实现）。
    - y 轴 `scale:true` + min/max 各留 8% 边距（已实现）。
    - 指标切换器：同一坐标系切换请求数/RPM/TTFB/耗时/token/成本，我们的趋势图可照此加 metric 切换。
- `filter-state.js`（170 行）+ `filter-query.js`（69 行）— **声明式筛选字段表**：每个字段声明 `restore` 优先级（URL hash > localStorage > 默认值），`persist` 时同步写 hash + localStorage。我们 `tab-requests.js` 手写的 hash 同步可以统一成这个模式，几十行换来所有筛选可分享、可刷新保持。
- `service-health.js`（109 行）— 96 格 DOM 健康网格，429 染黄与真实错误分开。我们已实现 120 格版本（概览页健康时间线）。
- `logs.js`（2827 行）— 日志页大全：进行中请求并入表顶 pending-row（已实现）、按 ID diff 更新、列可见性开关、表达式状态过滤（`>=400`、`!200`）、增量 offset 拉取。
- `date-range-selector.js`（543 行）— 统一时间范围选择器，预设 + 自定义区间。我们目前各页时间窗是写死的，值得引入。
- `searchable-select.js`（241 行）— 可搜索下拉，原生 select 退化为隐藏值载体。模型多了之后筛选框可用。
- `tokens.css` — 设计变量独立文件；ECharts 颜色从 `getComputedStyle` 读 CSS 变量（已实现到我们 `charts.js`）。
- 细节：`document.title` 写在途请求数角标 + favicon 闪烁（角标已实现，favicon 未做）。

### ccLoad 未吸收但值得做的

1. **统一时间范围选择器**——预设 1h/6h/24h/7d + 自定义，用量/趋势/日志页共用。
2. **趋势图指标切换**——一个图切多个指标，比并排多图省空间。
3. **表达式状态过滤**——`>=400`、`!200`、`499`，解析器就几十行。
4. **列可见性开关**——请求表列越来越多，让用户自选。
5. **声明式 filter-state**——替换现在手写的 hash 同步，之后加筛选字段不用写样板。

## sub2api（组件与表格设计最深）

源码：`/tmp/sub2api/frontend/src/`，Vue3 组件。值得看的文件：

- `components/user/monitor/MonitorTimeline.vue`（115 行）— 60 格 flex 细条健康时间线，高度 + 颜色双编码。我们概览页已实现同款（120 格）。
- `components/admin/usage/UsageTable.vue`（766 行）— 用量表标杆：
    - token 列 `↓输入 ↑输出` 同行 + cache 副行（已实现）。
    - 耗时列左侧 1px 双色条按 TTFB/总耗时分档着色（我们用了文字阈值色，未做色条）。
    - donut 图旁配滚动明细表 + `Σ Other` 补差行，top-N 与总量口径对齐（已实现）。
- `components/common/BaseDialog.vue`、`ConfirmDialog.vue` — 弹窗基座：Esc/遮罩取消、danger 主键（已实现到我们 `confirmBox`）。
- `api/admin/*.ts` — 每个资源一个 API 模块，错误统一归一化。我们 `core.js` 的 `api()` 是简化版。
- 布局：固定侧栏 256px ↔ 72px 折叠、sticky 页头、路由 meta 带页标题 + 一行描述。我们侧栏不能折叠，窄屏直接收顶部——够用，不抄。
- KPI 卡：icon 块 + 大数字 + muted 标签 + 环比/提示副行。环比已实现（今日 vs 昨日）。

### sub2api 未吸收但值得做的

1. **KPI 卡 icon 块**——现在只有数字，加个色块 icon 更扫读。
2. **耗时列左侧双色条**——1px 竖条比整行着色更克制。
3. **搜索/筛选输入 300ms debounce**——筛选多了以后要。
4. **侧栏折叠**——如果以后导航项变多再考虑。

## CLIProxyAPI / CPA（状态模型与交付方式）

管理面源码不在主仓库：主仓库 `internal/managementasset/updater.go` 负责从 release 下载预构建的 `management.html` 单文件。这个交付方式本身是个模式：**前端独立仓库用完整工具链开发，产物单文件下发**，二进制不带源码。我们暂时不需要（go:embed 已够），但前端复杂到原生 JS 撑不住时这是退路。

值得吸收的模式（从调研报告归纳）：

- **三层状态分离**：开关状态（disabled 灰）/ 运行状态 badge / quota 冷却 chip + tooltip 展示 `LastError`。一个对象的状态不要用单 badge 表达，拆成「用户配置态 / 运行态 / 限制态」三轴。
- **数据新鲜度**：状态都带 `ObservedAt` 之类时间戳，UI 显示「x 分钟前」，不假装实时。
- **危险操作统一确认 + toast**（已实现）。
- **敏感字段遮罩**：api_key/OAuth 永远 mask；`localStorage` 里的 `enc::v1::` 只是混淆不是加密——我们面板不落明文凭据，保持。
- **API 用相对路径**——反代兼容，我们已遵守。
- **隐藏管理流量开关**——日志页可勾选不显示面板自身产生的请求，我们如果以后把 panel API 也计入 index.jsonl 才需要。

## aio-coding-hub / HelpAIO（信息密度最高）

源码：`/tmp/aio-coding-hub/src/`，Tauri + React + shadcn。值得看的文件：

- `components/home/HomeRequestLogsPanel.tsx`（873 行）— **请求日志卡片流**：状态 badge + tag badges 一行，下面 4×2 指标网格（输入/缓存写/首字/花费 × 输出/缓存读/耗时/速率），带「简洁模式」开关存 localStorage。信息密度比我们平铺表格高一个量级。
- `components/home/LogBadges.tsx`（83 行）— 状态 badge 语义分层样板：**499/中断用琥珀不用红**，中断不算错误（已实现）。
- `components/home/RequestLogDetailDialog.tsx`（216 行）+ `*SummaryTab` / `*ChainTab` / `*RawTab` — 详情弹窗三 tab：概览 / **决策链**（重试 attempt 垂直时间线）/ 原始数据。决策链需要后端记 attempt 明细，我们的 debug 目录已有阶段文件，理论上可以映射成「客户端请求 → 投影 → 上游 wire → 上游响应 → 下发」五节点链。
- `components/UsageHeatmap15d.tsx`（312 行）— GitHub 风格 15 天用量热力图。
- `components/usage/UsageAvailabilityPanel.tsx` — 可用率面板：颜色=可用率、大小=请求量。
- `components/home/RealtimeTraceCards.tsx`（542 行）— 实时 trace 卡片流。
- `ui/` — 一整套手写基础组件（Dialog/Popover/Toast/Skeleton/EmptyState/QueryStateView），`QueryStateView` 统一包 loading/error/empty 三态。
- 侧栏分组 MAIN/TOOLS/SETTINGS + 底部常驻「网关状态点 + 端口 pill」（已实现到我们侧栏）。
- 状态过滤表达式 `499`、`!200`、`>=400`（同 ccLoad）。
- stale-while-revalidate：轮询保留旧数据 + 顶部细 loading 条（已实现顶部条）。

### aio 未吸收但值得做的

1. **请求详情三 tab 化**——概览/决策链/原始数据，比现在展开行更能装。
2. **用量热力图**——15 天或更长跨度的活动热力，概览页可以再放一块。
3. **卡片流日志视图**——替代表格或作为可选视图。
4. **`QueryStateView` 三态封装**——loading/error/empty 统一处理入口。

## 已吸收清单（2026-09-12 前完成）

见 commit `e5fee9a`：toast、复制降级 + 反馈、confirmBox、顶部加载条、侧栏状态点 + 端口、title 角标、badge 语义分层（中断琥珀）、状态码色阶、耗时/TTFB 阈值色、↓↑ token、pending-row 并入表顶、`#requests&dir=` 深链、markArea 空窗、markLine/markPoint、y 轴留白、confine tooltip、图表空态、配色读 CSS 变量、120 格健康时间线、KPI 今日 vs 昨日、模型表三态排序 + 加权合计行+Σ Other、配额重置倒计时。

## 明确不做的

- 明暗主题切换、i18n、移动端卡片视图、虚拟滚动、WebSocket 推送——单用户自用，收益不抵复杂度。
- IP 属地查询——把用户 IP 发给第三方。
- 分页器 + 服务端排序——「加载更多」够用，全局排序要改 `/panel/api/requests` 加 `order_by`。
- React/Vue 构建链——ccLoad 证明原生 JS 模块撑得住这个规模；真撑不住时走 CPA 的「独立仓库 + 单文件产物」路线。
- localStorage `enc::` 混淆存储敏感物——混淆不是加密，不要学。

## 后续若继续改，优先级建议

1. ccLoad `filter-state.js` 声明式筛选表 → 统一请求页筛选 + 给用量页加时间范围。
2. ccLoad `date-range-selector.js` → 统一时间窗。
3. ccLoad `trend.js` 指标切换 → 趋势图省空间。
4. aio 详情弹窗三 tab → 请求详情装下更多诊断信息（决策链可映射 debug 阶段文件）。
5. aio `UsageHeatmap15d` → 概览加长期活动热力。
