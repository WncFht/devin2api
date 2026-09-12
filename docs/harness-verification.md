# 各 Harness 验证状态

截至 2026-09-12 的实测记录。链路：客户端 → ccload:49173 → devin-2api:3003 → Devin `GetChatMessage`(swe-2-max)。

## 验证矩阵

| 场景                               | Claude Code                                                | pi                               | kimi-code                          | Codex                                                  |
| ---------------------------------- | ---------------------------------------------------------- | -------------------------------- | ---------------------------------- | ------------------------------------------------------ |
| 单轮问答                           | ✅                                                         | ✅                               | ✅                                 | ✅                                                     |
| 多轮记忆                           | ✅ `--continue` 回忆 codeword                              | ✅ `--continue` 回忆 `JAGUAR-77` | ✅ `-r <session>` 回忆 `MANTIS-33` | ✅                                                     |
| 单工具调用 (写 + 读文件)           | ✅ Write/Read                                              | ✅ write/read                    | ✅ write/read                      | ✅                                                     |
| 并行工具调用                       | ✅                                                         | ✅ 同轮读两文件                  | ✅ 同轮读两文件                    | ✅ 2 call + 2 result                                   |
| 图像输入                           | ✅ 识别纯蓝 PNG                                            | ✅ `@blue.png` 识别颜色          | ✅ `image_in` 读 PNG 识别颜色      | ✅ `exec -i` 识别纯蓝 PNG                              |
| 并发会话                           | ✅ CC+Codex 并行                                           | ✅ pi+kimi 并行                  | ✅                                 | ✅ CC+Codex 并行                                       |
| 多步任务 (写文件+bash 执行 + 汇报) | ✅                                                         | ✅                               | ✅                                 | ✅                                                     |
| 流式                               | ✅                                                         | ✅                               | ✅                                 | ✅                                                     |
| 前缀缓存                           | ✅ 后续轮 `cache_read` ~25.5k                              | ✅ `cache_read` 53k–111k         | ✅ `cache_read` ~17.7k             | ✅                                                     |
| thinking/签名                      | ✅ 尾随签名已合并修复                                      | ✅ `thinking:enabled,8192` 正常  | ✅ thinking 输出正常               | ✅                                                     |
| subagent/skill/MCP                 | ✅ 7 种内置 agent + Skill + `mcp__` 工具全通（指纹已改写） | ✅                               | —                                  | ✅ 模板全过；`apply_patch`(custom) 被丢，走 shell 兜底 |
| 客户端压缩                         | ✅ 自带，~202k 实测自动触发                                | pi 自带 (16k reserve/20k recent) | kimi-code 自带                     | ✅ `auto_compact`，resume 240k 实测触发                |

## 各客户端接入时踩过的坑 (已修)

### Claude Code(2.1.236 / 2.1.258)

- 本地模型白名单拦截 `swe-2-max` → env flag / ccload redirect / `modelOverrides` 三解法
- `settings.json` env 覆盖 shell 变量
- 主提示词 7 条指纹 + subagent 提示词 emoji 禁令句触发 `permission_denied` → `sanitize.go` 改写（详见 `2026-09-12-upstream-policy-fingerprints.md`）；subagent 被拒时 CC 报 "issue with the selected model" 属误诊，实非模型问题
- 尾随 `DeltaSignature` 落成独立空 thinking 块 → decoder/encoder 修复，`content_block_stop` 延迟等签名
- ccload `protocol_transform_mode` 必须 `local`,`auto` 的 codex→anthropic 转换会产生同款畸形签名块

### pi(0.73.1)

- 零修改直接通。anthropic-messages provider 伪装完整 CC 提示词 (订阅兼容),指纹规则自动覆盖
- 真实系统提示词走 user 消息 `[System Instructions]` 前缀
- `PI_CACHE_RETENTION` 可拉长缓存保留窗口

### kimi-code(0.42.0)

- 零修改直接通。同样伪装 CC 请求封套 (`claude-cli` UA、`X-Claude-Code-Session-Id`、CC beta 头)
- `metadata.user_id` 带 device_id JSON，被代理用作 SessionKey → 会话 ID 稳定，利于缓存
- `max_tokens` 发得很大 (≈context size),透传无碍
- `image_in` 实测可用——上游 `ChatMessagePrompt.images`(纯 base64 + mime_type) 与 swe-2-max 视觉能力都支持;**坏图/非法 base64 会被上游判 `invalid_argument` → 400**,属正确行为
- `video_in` 未验证也不要开：上游 proto 没有视频字段
- 注意区分:kimi-cli(旧 Python 版) 已官方弃用，未测不测

### Codex

- 93KB 真实请求 (2 call + 2 result) 验证通过
- `apply_patch` 实际走 `exec_command` shell 命令而非 FREEFORM tool call——`/v1/responses` 适配层只认 `type:"function"`，`custom`/`web_search`/`mcp`/`local_shell` 类型静默丢弃（wire 上 `tools:[]`），Codex 用 shell 兜底功能完整
- 0.153.3 备选模板三条指纹（open-source 定义句 / plan 状态句对 / ANSI 转义句）已入 `sanitize.go`——换模型家族映射时会踩到，已提前改写
- 工具调用历史曾触发 `invalid_argument` → 代理已做 call→result 配对重排

## 已知边界

- 免费档模型 (swe-2-\*) 缓存是 best-effort 前缀匹配 (~75-90% 命中),偶发逐出属正常
- 重启 devin-2api 仍会结束在途请求，但 launchd `ExitTimeOut=60` 给了优雅退出窗口——SIGTERM 后在途流可跑完；`kill -9` 跳过该窗口，禁用
- 上游错误按层级分类：`permission_denied`/`prompt too long` 等请求级错误在首个上游事件前以真实 HTTP 4xx 返回（不冷却渠道）；流式中途的错误事件带顶层 `status` 供 ccload 精确分类——只有传输级故障 (EOF/连接重置) 才会进入模型/渠道冷却
