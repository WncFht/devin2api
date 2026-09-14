# 文档索引

devin-2api 把 Anthropic Messages / OpenAI Responses / Chat Completions 请求转成 Devin Connect `GetChatMessage` 的 Connect-RPC 调用。

本目录是**活文档**：随代码演进持续更新、以现状为准、随仓库发布。一次性的调查快照（探测记录、事故 postmortem）在本机的 `notes/archive/`（gitignore，不发布）——活文档引用档案时路径以 `notes/archive/` 标注。

## 上游协议与行为（逆向结论）

| 文档                              | 用途                                                                                             |
| --------------------------------- | ------------------------------------------------------------------------------------------------ |
| `upstream-protocol.md`            | 上游协议逆向参考：请求/响应字段契约、帧形态与签名体制、工具调用矩阵、错误分类、RPC 面            |
| `upstream-policy-fingerprints.md` | content-policy 指纹实证：触发器形态、客户端全模板探测结果、`sanitize.go` 新规则维护流程          |
| `upstream-cache.md`               | 上游前缀缓存机制逆向：命中条件、EPHEMERAL 断点、trajectory 稳定性                                |
| `upstream-compaction.md`          | 压缩责任划分：上游不压缩，压缩义务全在客户端；代理侧只需保证窗口声明一致                         |
| `upstream-rate-limit.md`          | 上游消息限流（429）模型：分钟桶量化 + 概率执行，本地滴灌闩的设计依据与实现状态，整形语义选型决策 |
| `quota-billing.md`                | 配额计费模型反推：日/周额度大小、cache_write 按 input 价计费、est_cost 口径                      |

## 排障与接入

| 文档                         | 用途                                                                           |
| ---------------------------- | ------------------------------------------------------------------------------ |
| `upstream-debug-playbook.md` | 排障手册：错误速查表、标准排查流程、已验证 wire 契约、新客户端验证清单、运维坑 |
| `client-setup.md`            | 各客户端接入配置（CC / pi / kimi-code / Codex，含 Codex WS 链路）与权限模式    |
| `harness-verification.md`    | 各 harness 验证矩阵（单轮/多轮/工具/图像/并发/缓存/thinking/压缩）与踩坑记录   |

## 部署与工程

| 文档            | 用途                                                                                         |
| --------------- | -------------------------------------------------------------------------------------------- |
| `deployment.md` | 部署：macOS launchd / Linux systemd --user / Windows 裸进程，运行目录、优雅排空、单实例约定  |
| `toolchain.md`  | 工程设施手册：pre-commit 管道、本地验证命令、版本解析链、CI/CD、发布与部署脚本族             |
| `perf.md`       | 性能工作流：pprof/fgprof 端点、loadtest+upstreamstub 压测、延迟分解字段、benchstat 验收、PGO |

## 速查入口

- 客户端报错 → `upstream-debug-playbook.md` 错误速查表 → `logs/<debug_ref>/error.json`
- 新客户端接入 → `client-setup.md` + playbook「新客户端验证清单」
- `permission_denied` + content policy → `upstream-policy-fingerprints.md`
- feature 没生效（工具不调用/内容丢失）→ 指纹文档 + playbook「已验证 wire 契约」
- 上游 429 频发 → `upstream-rate-limit.md`（模型）+ `rategate.go`（实现）
- 发版/格式化/CI → `toolchain.md`
- 延迟异常/吞吐瓶颈 → `perf.md`（剖析工具链）+ `logs/<debug_ref>/meta.json` 延迟分解字段
