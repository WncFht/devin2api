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
2. `trajectory_id`/`cascade_id` 由 `deriveSessionIDs` 派生：有 `SessionKey`（`user`/`prompt_cache_key`/`metadata.user_id`，三者均为会话级）时直接以它为种——压缩改写消息后轨迹仍然连续；无 key 时退回「系统提示头 4KB + 首条消息文本头 1KB」内容哈希。`execution_id` 与消息 `message_id` 保持每次随机。
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

## 已知边界

- 免费档命中非保证：偶发 miss 是上游逐出/冷启动，非代理问题。
- `permission_denied`（含内容策略拦截）在免费档表现非确定性——同一 prompt 可能先封后放（WindsurfAPI 亦记录此现象）。
- 多 token 轮换会破坏按账号键控的缓存（粘账号才有意义）；当前单 token 无此问题。
- 命中率统计口径：`index.jsonl` 聚合时必须过滤 `result=="completed" && input_tokens+cache_read_tokens>0`——rate_gate 快败与断开请求的 0-token 行会被误算成 miss；`scripts/index-stream-stats.py` 实现了这套口径（流画像 + gap→hit% 分桶 + miss 归因）。
