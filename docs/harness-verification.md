# 各 Harness 验证状态

截至 commit `07a4b9d` 的实测记录。链路:客户端 → ccload:49173 → devin-2api:3003 → Devin `GetChatMessage`(swe-2-max)。

## 验证矩阵

| 场景 | Claude Code | pi | kimi-code | Codex |
|---|---|---|---|---|
| 单轮问答 | ✅ | ✅ | ✅ | ✅ |
| 多轮记忆 | ✅ `--continue` 回忆 codeword | ✅ `--continue` 回忆 `JAGUAR-77` | ✅ `-r <session>` 回忆 `MANTIS-33` | ✅ |
| 单工具调用(写+读文件) | ✅ Write/Read | ✅ write/read | ✅ write/read | ✅ |
| 并行工具调用 | ✅ | ✅ 同轮读两文件 | ✅ 同轮读两文件 | ✅ 2 call + 2 result |
| 流式 | ✅ | ✅ | ✅ | ✅ |
| 前缀缓存 | ✅ 后续轮 `cache_read` ~25.5k | ✅ `cache_read` 53k–111k | ✅ `cache_read` ~17.7k | ✅ |
| thinking/签名 | ✅ 尾随签名已合并修复 | ✅ `thinking:enabled,8192` 正常 | ✅ thinking 输出正常 | ✅ |
| 客户端压缩 | CC 自带 | pi 自带(16k reserve/20k recent) | kimi-code 自带 | codex 自带(`auto_compact`) |

## 各客户端接入时踩过的坑(已修)

### Claude Code(2.1.236 / 2.1.258)

- 本地模型白名单拦截 `swe-2-max` → env flag / ccload redirect / `modelOverrides` 三解法
- `settings.json` env 覆盖 shell 变量
- 7 条新指纹行触发 `permission_denied` → `sanitize.go` 加改写规则
- 尾随 `DeltaSignature` 落成独立空 thinking 块 → decoder/encoder 修复,`content_block_stop` 延迟等签名
- ccload `protocol_transform_mode` 必须 `local`,`auto` 的 codex→anthropic 转换会产生同款畸形签名块

### pi(0.73.1)

- 零修改直接通。anthropic-messages provider 伪装完整 CC 提示词(订阅兼容),指纹规则自动覆盖
- 真实系统提示词走 user 消息 `[System Instructions]` 前缀
- `PI_CACHE_RETENTION` 可拉长缓存保留窗口

### kimi-code(0.42.0)

- 零修改直接通。同样伪装 CC 请求封套(`claude-cli` UA、`X-Claude-Code-Session-Id`、CC beta 头)
- `metadata.user_id` 带 device_id JSON,被代理用作 SessionKey → 会话 ID 稳定,利于缓存
- `max_tokens` 发得很大(≈context size),透传无碍
- 注意区分:kimi-cli(旧 Python 版)已官方弃用,未测不测

### Codex

- 93KB 真实请求(2 call + 2 result)验证通过
- `apply_patch` FREEFORM 裸词、reasoning item、`custom`/`web_search` 工具类型上游不认会被静默丢弃——行为偏差已知
- 工具调用历史曾触发 `invalid_argument` → 代理已做 call→result 配对重排

## 已知边界

- 免费档模型(swe-2-*)缓存是 best-effort 前缀匹配(~75-90% 命中),偶发逐出属正常
- 重启 devin-2api 会掐断在途请求,ccload 会把渠道打冷却——重启后检查 `channels.cooldown_until`,必要时清零
- 上游流式中途失败(HTTP 200 + `response.failed`)算真失败,会正常计入 ccload 冷却统计
