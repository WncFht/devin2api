# 文档索引

devin-2api 把 Anthropic Messages / OpenAI Responses / Chat Completions 请求转成 Devin Connect `GetChatMessage` 的 Connect-RPC 调用。链路：`客户端 → ccload :49173 → devin-2api :3003 → server.codeium.com`。

文档分两类：**活文档**（无日期前缀，随代码演进持续更新）与**调查档案**（`YYYY-MM-DD-` 前缀，某次调研/事故的快照，结论可能已被后来者修正——以活文档为准）。

## 活文档

| 文档                         | 用途                                                                           |
| ---------------------------- | ------------------------------------------------------------------------------ |
| `client-setup.md`            | 各客户端接入配置（CC / pi / kimi-code / Codex），窗口声明与权限模式            |
| `upstream-debug-playbook.md` | 排障手册：错误速查表、标准排查流程、已验证 wire 契约、新客户端验证清单、运维坑 |
| `upstream-cache.md`          | 上游前缀缓存机制逆向：命中条件、EPHEMERAL 断点、trajectory 稳定性              |
| `upstream-compaction.md`     | 压缩责任划分：上游不压缩，压缩义务全在客户端；代理侧只需保证窗口声明一致       |
| `harness-verification.md`    | 各 harness 验证矩阵（单轮/多轮/工具/图像/并发/缓存/thinking/压缩）与踩坑记录   |
| `macos-deployment.md`        | launchd 部署：plist、升级流程、进程模型                                        |

## 调查档案（2026-09-12 逆向系列）

上游逆向分多轮，阅读顺序即编号顺序：

1. `2026-09-12-upstream-gaps.md` — 二轮：proto/strings 静态分析，列出我们没用/没消费的 wire 字段
2. `2026-09-12-upstream-live-probes.md` — 三轮：`cmd/probe` 直连上游逐字段实测，含 AssignModel 路由链、帧序、错误分类
3. `2026-09-12-upstream-policy-fingerprints.md` — content-policy 指纹逆向：触发器形态（整句/句对/共现）、CC 2.1.236 与 Codex 0.153.3 全模板探测结果、feature 级限制实测、新规则维护流程

其它档案：

- `2026-09-12-premature-endturn.md` — Codex 提前收工事故：上游缺 stopReason 的干净 EOF 曾是截断而非正常结束
- `2026-09-12-websocket-multi-turn-research.md` — Responses WS 多轮调研：ccLoad 会话状态机是蓝本
- `2026-09-12-cliproxyapi-issues-survey.md` — CLIProxyAPI 2,878 issue 谱系对本项目的适用性分析

## 速查入口

- 客户端报错 → `upstream-debug-playbook.md` 错误速查表 → `logs/<debug_ref>/error.json`
- 新客户端接入 → `client-setup.md` + playbook「新客户端验证清单」
- `permission_denied` + content policy → `2026-09-12-upstream-policy-fingerprints.md` 第六节
- feature 没生效（工具不调用/内容丢失）→ 指纹文档第六节 + playbook「已验证 wire 契约」
