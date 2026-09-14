// 启动入口：import 各 tab 模块（模块顶层副作用完成 Tabs.register /
// Polls.add / 事件绑定），再按 hash 落到对应页（默认概览）。
// ES module 的执行顺序由依赖图决定，不再依赖 script 标签排列。

import './tab-requests.js';
import './tab-overview.js';
import './tab-usage.js';
import './tab-quota.js';
import './tab-models.js';
import './tab-system.js';
import { Tabs, parseHash } from './core.js';

Tabs.apply(parseHash().tab);
