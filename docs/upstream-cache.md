# 上游 Prompt 缓存调研（Devin `GetChatMessage`）

本文记录对 Devin 上游 prompt 缓存机制的逆向与实测结论，以及 devin-2api 的对接方式。调研方法：proto 静态分析 + Devin CLI 真实流量抓包（`api_server_url` 重定向到本地捕获服务器）+ 控制变量 A/B 实测。

## 结论摘要

- **上游对 swe-2 / glm-5-2 等免费档模型提供隐式内容前缀缓存**，按「账号 + 前缀内容」键控，不需要任何标记或会话连续性。
- 免费档是 **best-effort**：暖了以后命中率约 75–90%，偶发 miss/逐出，且响应 usage 里可能不上报 cache 字段（付费档才稳定上报）。
- **命中时延迟差异巨大**：实测 ~2.5s vs 未命中 15–44s（5.4k token 前缀）。长会话可用性基本取决于此。
- 缓存匹配不依赖 `trajectory_id`/`cascade_id`/消息 `message_id` —— 每次随机 ID 的请求暖后照样命中；但稳定 ID 命中率更高（实测稳定 7/8 vs 随机波动），故本代理默认派生稳定会话 ID。

## 协议字段（`GetChatMessageRequest`）

| 字段                                            | 说明                                                        |
| ----------------------------------------------- | ----------------------------------------------------------- |
| `system_prompt_cache_options`                   | system prompt 缓存选项；可设 `CACHE_CONTROL_TYPE_EPHEMERAL` |
| `ChatMessagePrompt.prompt_cache_options`        | 单条消息缓存断点（EPHEMERAL）                               |
| `trajectory_reference` / `cascade_id`           | 上游轨迹标识，多轮复用同一会话 ID                           |
| `message_id`                                    | 每条 ChatMessagePrompt 的标识                               |
| 响应 `cache_read_tokens` / `cache_write_tokens` | 命中/写入计量（`model_usage`）                              |

Cascade 轨迹流（`StartCascade`/`SendUserCascadeMessage`）另有 `cache_breakpoint_indices` 等结构化断点字段——`GetChatMessage` 路线用不到。

## Devin CLI 真实行为（抓包实测）

- CLI 走 `GetChatMessage`，模型如 `swe-2-max`，`requestType=CASCADE`。
- **不发送** `system_prompt_cache_options`/`prompt_cache_options` —— 完全依赖隐式缓存。
- 同一会话内 `trajectory_id`/`cascade_id` 保持稳定（重试间逐字节相同），`execution_id` 每次新随机。
- `configuration`: `maxTokens=128000, temperature=1, topK=40, topP≈0.95`。

## A/B 实测（swe-2-max，~5.4k token 前缀）

| 模式                          | 命中率      | 备注               |
| ----------------------------- | ----------- | ------------------ |
| 稳定 trajectory+cascade+msgid | 7/8         | 首次冷写后近乎必中 |
| 全随机 ID、无 trajectory      | 6/6（暖后） | 内容前缀即可命中   |
| 加 EPHEMERAL 标记             | 6/8         | 标记对免费档无害   |

命中特征：`input_tokens=1, cached_tokens≈5366`（整段前缀 + 历史全命中，仅新 token 计费）。未命中：`input_tokens=5367, cached_tokens=0`。

## devin-2api 的对接

1. `buildRequest` 无条件发送 `system_prompt_cache_options` + 末条消息 `prompt_cache_options`（EPHEMERAL）。免费档下无副作用，付费档可获得完整 write→read 计量。
2. `trajectory_id`/`cascade_id` 由 `deriveSessionIDs` 派生：有 `SessionKey`（`user`/`prompt_cache_key`/`metadata.user_id`，三者均为会话级）时直接以它为种——压缩改写消息后轨迹仍然连续；无 key 时退回「系统提示头 4KB + 首条消息文本头 1KB + 客户端模型名 + 工具声明哈希」内容哈希。种子尾部另折叠客户端声明 marker（`cache_control:<type>`、`anthropic_beta:<flag>`，排序去重的规范集）——断点类型与 beta flag 改变上游特性面，同会话键下声明漂移即换轨迹/lane。`execution_id` 与消息 `message_id` 保持每次随机。
3. 上游 cache 计量按协议透出：Responses `usage.input_tokens_details.cached_tokens`/`cache_write_tokens`、chat `prompt_tokens_details` 同名字段、Anthropic `cache_read_input_tokens`/`cache_creation_input_tokens`。
4. sanitize 改写是确定性的（同输入必同输出），不影响缓存键稳定。

## 受控实测的缓存语义（2026-09-15，archbox :3033 暖臂实验）

用受控探针臂（独立 `metadata.user_id` + padded system prompt，绝对偏移时刻表发 seed/ping/probe，`scripts/cache-probe.py` 为该实验骨架的入库形态）测出的机制级结论：

- **滑动 TTL 标称 ~780s**：每次命中把寿命续满（滑动窗而非固定过期），≥840s 静默后死透；另有 **~10% 逐请求早夭 lottery**——未到期也偶发 miss，属上游逐出噪声。
- **复用规则是逐字前缀**：已缓存的整条存储区间必须是新请求 token-0 起的逐字前缀才命中；中间改写一个 token，其后部分整体失效。
- **命名空间按 SessionKey→trajectoryID 隔离**：不同 SessionKey 派生的轨迹互不可见缓存。
- **死后 verbatim 重写只恢复 ~7%**：缓存过期后原样重发同一前缀，命中率恢复极低——等 TTL 自然死亡的 lineage 基本救不回，要靠保温维持。
- **`cache_creation` 恒 0**：免费档响应不写 create 计量，判活只能看 `cache_read`。
- **keepalive 可续命**：180s 间隔、`max_tokens=1` 的 verbatim ping（与 seed 完全同前缀）实证把 lineage 续过 3.2×TTL，对照组全死。这是「subagent 等待期缓存不失温」的可用手段。
- **上游模型相位漂移**：wire 恒发 `swe-2-max` 时，响应侧模型署名以 ~30s 相位在 swe-2-max 与 opus 间交替；翻转对命中率 52% vs 同模型对 98%。直发真实 uid `claude-opus-4-6` 被接受且**跨 uid 命中**——请求里的 model uid 不进缓存键，但响应署名漂移会影响按 model 分桶的命中率统计口径。

## 前缀保温（prefix warming）

「keepalive 可续命」的实测结论已落成代理内建调度器（`internal/adapter/devin/prefixwarm.go`，总开关 `devin.warm_prefix_enabled`，默认关、灰度放出）：每条会话谱系留存最近一次 sanitize 后的客户端请求体，静默期按 `warm_prefix_interval_seconds`（默认 180s）加 ±`warm_prefix_jitter_ratio`（默认 0.15）抖动的节拍，重放 `max_tokens=1` 的逐字 ping 给上游滑动 TTL（标称 ~780s）续命，压住 subagent 长等待、用户离开后首轮的冷 prefill。ping 拿 retained 走 buildRequest 原路径重建 wire 体——token、message_id、step_index、execution_id、assignment jwt 全部新鲜，只压 MaxTokens；这些字段本就不进缓存 token 流。调度由 30s 清扫节拍驱动：interval 只决定条目到期时刻，实际 ping 周期被清扫节拍向下取整，观测端至少等 ~35s 才能看到第一发 ping。

谱系键 warmLineageKey 是「前缀逐字相等」的最小判据：SessionKey、system 头 4KB、工具声明全量、首条消息头 1KB、解析后 wire uid 五维，任一维漂移即换键。条目「晋升」为保温对象需同时满足：第 2 发真追加的成功上行（逐字重发/探针不计，microcompact 类原地改写重置计数）与前缀达 `warm_prefix_min_prefix_tokens`（默认 8192 token；观测过 usage 用实测 input+cache_read，未观测按 retained 字节/4 估）。同 SessionKey 内与新到谱系恰好一维相异的旧条目标 suspect——compaction 换首消息、auto-update 改 system 头、模型漂移这类「旧流从此永久静默」的形态；宽限 2×Interval 无真实上行即退役。

ping 语义有三条硬边界。其一，只续命不复活：TTL 死透的谱系 verbatim 重发也救不回（实测恢复 ~7%），故退役只认四类证据——客户端可归因上行静默超时、suspect 宽限期满、容量淘汰、自愈后仍语义错误；单发 ping 的 cache_read=0 永不作退役证据（相位 miss≠冷 miss，miss 请求本身已完成重写兜底），但 K=4 连 miss（≈12min 不沾）说明锚反复丢失——降级停 ping（demote≠retire：条目留表照 maxIdle 退役，retain 真流量免费重武装；hit 清连击，发送错误不清）。其二，准入过闸门 `tryAdmit`：闩内一律拒，闩外只在可发区间、配额有余、无排队者时放行——不排队不偷槽，被拒跳过本轮（计 `ping_skips`）；ping 撞 resource_exhausted 照喂冷却闩，它常最先发现上游饱和。其三，错误分类：凭证味失败（unauthenticated/permission_denied）自愈重发一次，仍 ClientFixable（invalid_argument/ContextLength/permission_denied 等）才退役，传输/限流/超时类只跳本轮。ping 是内部流量：直连 streamClient、绕过 app/recorder，不进 `logs` 表、调试记录与面板请求列表。

静默分级只决定「最多保多久」——resume 越不可能，烧 ping 越不值：

| 档位      | 判定（按最后一发干净收尾响应的 pending）                                       | 默认最长静默 | 配置键                                   |
| --------- | ------------------------------------------------------------------------------ | ------------ | ---------------------------------------- |
| blocked   | pending 含阻塞派发或普通工具（权限提示与普通工具 wire 不可分，catch-all 统归） | 4h           | `warm_prefix_blocked_max_idle_seconds`   |
| userpaced | 无 pending（轮结束等用户）或 pending 全为提问类工具                            | 45min        | `warm_prefix_userpaced_max_idle_seconds` |
| subdone   | 带 subagent 标记的流已跑完（SendMessage/agentId 复活长尾）                     | 10min        | `warm_prefix_subdone_max_idle_seconds`   |
| unknown   | 无 SessionKey 或尚无已完成响应可分类，兜底档                                   | 30min        | `warm_prefix_unknown_max_idle_seconds`   |

两档工具名表 `warm_prefix_blocked_names`/`warm_prefix_userpaced_names` 可配（默认 {Agent, Task, Workflow, wait_agent} 与 {AskUserQuestion, ExitPlanMode, request_user_input}）；名表主要为可观测性存在，catch-all 条款已把一切非 userpaced pending 归 blocked。

资源与生命周期：谱系数与 retained 字节双帽 `warm_prefix_max_streams`（默认 256）/`warm_prefix_max_retained_mb`（默认 96），触顶先挤 suspect 再按 lastTouch LRU 挤。`retained_bytes` 只计 prompt 内容字段（system+ 消息文本 + 工具声明），不含 JSON 包装，比完整请求体小是正常口径。簿记全在内存，重启即清空——存活流的第一发真实请求自然重暖。排空（BeginDrain）后停发 ping，条目表留作观测，退役与淘汰判定照常。`warm_prefix_*` 全部参数热重载（热键清单见 config-reload.md）；enabled 热关掉即停调度并清空条目表，释放 retained 内存。

观测面：`/admin/runtime-metrics` 的 `warm` 段透出 `enabled`、`entries`（留存谱系）、`promoted`（保温中）、`demoted`（连 miss 降级停 ping 现值）、`suspects`、`retained_bytes`、`pings_sent`、`ping_hits`/`ping_misses`、`ping_skips`（闸门拒）、`ping_errors`、`retired`、`retired_by_cause`（idle/suspect/semantic/capacity 四退役桶 + miss_demote 降级累计——降级不删条目故不进 `retired`）、`ping_miss_prefill_tokens`（miss 轮 prefill 成本账）、`ping_hit_cache_read_tokens`（hit 轮上游实报 cache_read 累计——两侧合计即 ping 燃烧的完整口径）、`failover_suspects`（号池换 lane 标 suspect 的累计实绩——与 `retired_by_cause.suspect` 退役账对照看跨 lane 孤儿比重），另派生 `ping_hit_rate`。命中率口径只算 ping 自身、不含真实流量；面板「趋势」页顶部状态条与「设置」页运行指标组有同名展示。开启方式：config.yaml 置 `devin.warm_prefix_enabled: true` 后 POST `/admin/config/reload` 即时生效。

## 已知边界

- 免费档命中非保证：偶发 miss 是上游逐出/冷启动，非代理问题。
- `permission_denied`（含内容策略拦截）在免费档表现非确定性——同一 prompt 可能先封后放（WindsurfAPI 亦记录此现象）。
- 多 token 轮换会破坏按账号键控的缓存（粘账号才有意义）；当前单 token 无此问题。
- 命中率统计口径：`logs` 表聚合时必须过滤 `result='completed' AND input_tokens+cache_read_tokens>0`——rate_gate 快败与断开请求的 0-token 行会被误算成 miss；`scripts/index-stream-stats.py` 实现了这套口径（流画像 + gap→hit% 分桶 + miss 归因）。
