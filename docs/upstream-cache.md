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
3. 上游 cache 计量透传到 `usage.input_tokens_details.cached_tokens` / `cache_write_tokens`。
4. sanitize 改写是确定性的（同输入必同输出），不影响缓存键稳定。

## 已知边界

- 免费档命中非保证：偶发 miss 是上游逐出/冷启动，非代理问题。
- `permission_denied`（含内容策略拦截）在免费档表现非确定性——同一 prompt 可能先封后放（WindsurfAPI 亦记录此现象）。
- 多 token 轮换会破坏按账号键控的缓存（粘账号才有意义）；当前单 token 无此问题。
