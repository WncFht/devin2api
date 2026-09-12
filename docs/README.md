# 文档索引

devin-2api 把 Anthropic Messages / OpenAI Responses / Chat Completions 请求转成 Devin Connect `GetChatMessage` 的 Connect-RPC 调用。链路：`客户端 → ccload :49173 → devin-2api :3003 → server.codeium.com`。

文档分两类：**活文档**（本目录无日期前缀，随代码演进持续更新，以它们为准）与**调查档案**（`archive/` 下 `YYYY-MM-DD-` 前缀，某次调研/事故的快照，结论可能已被后来者修正）。

## 活文档

| 文档                              | 用途                                                                                    |
| --------------------------------- | --------------------------------------------------------------------------------------- |
| `client-setup.md`                 | 各客户端接入配置（CC / pi / kimi-code / Codex，含 Codex WS 链路），窗口声明与权限模式   |
| `upstream-debug-playbook.md`      | 排障手册：错误速查表、标准排查流程、已验证 wire 契约、新客户端验证清单、运维坑          |
| `upstream-policy-fingerprints.md` | content-policy 指纹实证：触发器形态、客户端全模板探测结果、`sanitize.go` 新规则维护流程 |
| `upstream-cache.md`               | 上游前缀缓存机制逆向：命中条件、EPHEMERAL 断点、trajectory 稳定性                       |
| `upstream-compaction.md`          | 压缩责任划分：上游不压缩，压缩义务全在客户端；代理侧只需保证窗口声明一致                |
| `harness-verification.md`         | 各 harness 验证矩阵（单轮/多轮/工具/图像/并发/缓存/thinking/压缩）与踩坑记录            |
| `macos-deployment.md`             | launchd 部署：plist、升级流程、进程模型                                                 |

## 调查档案（`archive/`，2026-09-12）

上游逆向分多轮，阅读顺序即编号顺序：

1. `archive/2026-09-12-upstream-gaps.md` — 二轮：proto/strings 静态分析，wire 字段清单（实测结论以 live-probes 为准）
2. `archive/2026-09-12-upstream-live-probes.md` — 三/四轮：`cmd/probe` 直连上游逐字段实测，含 AssignModel 路由链、帧序、错误分类、签名三体制
3. `archive/2026-09-12-premature-endturn.md` — Codex 提前收工事故 postmortem：助手回合拆分形态抬高宣告句 EOS 概率（契约结论已回写 playbook）
4. `archive/2026-09-12-cliproxyapi-issues-survey.md` — CLIProxyAPI 2,878 issue 谱系对本项目的适用性分析（actionable 缺口已落地）
5. `archive/2026-09-12-performance-review.md` — 性能审查：8 个 perf commit 的基线数字、机制与复现命令

## 速查入口

- 客户端报错 → `upstream-debug-playbook.md` 错误速查表 → `logs/<debug_ref>/error.json`
- 新客户端接入 → `client-setup.md` + playbook「新客户端验证清单」
- `permission_denied` + content policy → `upstream-policy-fingerprints.md` 第六节
- feature 没生效（工具不调用/内容丢失）→ 指纹文档第六节 + playbook「已验证 wire 契约」
