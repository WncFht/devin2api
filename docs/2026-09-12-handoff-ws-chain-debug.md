# Handoff: Codex → ccLoad → devin2api WebSocket 多轮全链路调试

> 2026-09-12 交接。**已完结**：14:54 全链路实测两轮 `responses-ws` 全部 `completed`(logs/20260912-145403、-145409),prev_id 增量展开、sealed 签名回放、session 身份稳定均已在线上 wire 验证。编码器修复 + decoder 合成路径 latent bug 修复已测试并提交。

## 背景：三跳链路

```
Codex CLI --WS--> ccLoad(:49173) --WS--> devin2api(:3003) --Connect-RPC--> Devin
```

- 两个 WS 跳各自做「prev_id → 全量 transcript 展开」，上游都无 prev_id 语义，对齐成立。
- devin2api 新 WS 实现在 `internal/app/websocket.go`(多轮读循环)+ `internal/app/websocket_session.go`(wsSession 状态机，merge/dedupe/prev_id 校验)。调研蓝本：`docs/2026-09-12-websocket-multi-turn-research.md`(ccLoad 为主)。

## 已完成

1. **devin2api WS multi-turn 实现完毕**(上个 session):
    - `internal/app/websocket.go`(~660 行):reader goroutine、per-turn 并发槽、wsConns 256 连接上限、错误分类 (invalid_request/previous_response_not_found/upstream_unavailable/upstream_stream_interrupted/message_too_big)、prewarm `generate:false` 合成 `resp_prewarm_`、ping/idle 保活。
    - `internal/app/websocket_session.go`:七步 normalize 守卫、merge+ 两轮 dedupe、tool-call 配对校验、32MiB transcript 上限。
    - `internal/app/app.go`:WS 路由在 protected group 内但**不经** concurrencyMiddleware(改 per-turn 槽);`wsConns` 字段。
    - `internal/app/app_websocket_test.go`:10 个测试全绿 (wsScriptAdapter 假上游)。
2. **ccLoad 侧配置完毕**:
    - ccLoad 源码在 `/tmp/ccload`(克隆);实际运行在 `~/Desktop/src/ccload`。
    - 上游 WS 门槛 (`proxy_forward.go:2705`):client 必须 WS 到达 + channel `websockets=1` + URL `protocols:["codex"]` + `!NeedsTransform` + RequestFamilyResponses。
    - 已建 channel **294 `devin-ws`**(独立于原 293 `devin` anthropic channel),url `http://127.0.0.1:3003/v1` protocols `["codex"]`,websockets=1,model `swe-2-max-ws` → redirect `swe-2-max`。
    - 管理 API:`POST /login` `{mode:"admin",password:$CCLOAD_PASS}`(密码在 `~/Desktop/src/ccload/.env`,勿外泄)→ Bearer token;`POST /admin/channels` 建渠道。
3. **Codex 侧配置完毕**:
    - `~/.codex/config.toml`:provider `OpenAI`,base_url `http://127.0.0.1:49173/v1`,`model_catalog_json="~/.codex/models.ccload.json"`。
    - `~/.codex/models.ccload.json`:追加了 `swe-2-max-ws` 条目 (`prefer_websockets:true`);备份在 `models.ccload.json.bak-ws-test`。
    - 测试命令：

        ```bash
        codex exec -m swe-2-max-ws \
          -c 'model_providers.OpenAI.supports_websockets=true' \
          --skip-git-repo-check "run echo ws-chain-test"
        ```

        (supports_websockets 也可直接写进 config.toml 的 `[model_providers.OpenAI]`)
4. **首次全链路实测 (14:31-14:32)**:WS 握手 ✓,ccLoad→devin2api upstream WS ✓,跑了 2 轮 (`api:"responses-ws"` at 14:31:58/14:32:03)——**两轮都 failed**:`status=200 result=failed error_stage=http_stream`,ccLoad 随即将该 WS target cool down 到 14:34:03,Codex 重试撞 cooldown 失败。

## 已定位的根因 + 已写好的修复 (未验证)

**Bug**:`logs/20260912-143153/error.json` 与 `-143159/error.json`:

```
{"stage":"http_stream","message":"content index 0 output item is already closed"}
```

上游 Devin 事件序：`thinking_end(0) → toolcall_start(1) → toolcall_delta* → thinking_signature(0) → toolcall_end(1) → done`。**签名帧隔着整个 toolcall 块才到**,而编码器原实现：

- `internal/api/openai/responses/response.go`:`Encode()` 对任何非 signature 事件先 `flushPendingReasoning()` —— toolcall_start 一到就把 pendingDone 的 reasoning item 提前 close → 迟到的 signature 走 `item()` 撞 `already closed` → 整轮 failed → `done` 永远没编码 → 下游无 terminal 事件 → ccLoad 判 upstream_stream_interrupted。
- `internal/api/anthropic/messages/response.go`:同构 bug，不 crash 但签名静默丢失 (`stopThinking` 已发，签名写不进块)→ 回放历史丢 encrypted signature。

**修复 (已写在工作区，`go build`+包测试已通过，未重跑 codex e2e)**:

- `responses/response.go`:flush 只在 `ResponseEventDone` 前执行 (等待窗口延到流终止);`reasoningSignature` 重写为容忍三种形态——item 缺失/非 reasoning → 静默丢；item 已 closed → 把 signature 补丁写进 `encoder.output[i]` 的 `encrypted_content`(保证 `response.completed.output` 仍带签名);item pendingDone → 正常 `reasoningDone`。
- `messages/response.go`:`startText`/`startThinking`/`startToolUse` 里的 `flushPendingThinking()` 删掉，flush 保留在 `finish`/`failed`;`thinkingSignature` 原逻辑已容忍 pending 路径，无需再改。
- **注意 syntax 坑**:改 messages/response.go 时留下过 `}))}` 残留，已修，两个包 `go test` 全绿 (1.13s/1.69s)。

## 复核出的第二个 bug(已修)

`response_decoder.go` `decodeLateSignature` 合成路径 (上游从无 thinking 块、签名裸到，openai-style signature-only 形态):原实现 emit `ThinkingStart`+`ThinkingSignature`。事件共享同一 `Partial` 指针，`ThinkingStart` 时块内已含全量签名——responses 编码器 `startReasoning` 播种 `encryptedContent` 后 `reasoningSignature` 又 `+=` 同一 `Delta` → **签名翻倍**;且合成块永不置 `thinkingOpen`、`ThinkingEnd` 不到 → item 悬挂，`done()` 报 `open reasoning item`。anthropic 侧同害：块永远不收尾，`message_stop` 前留未关闭 thinking block。

修复：合成路径改 emit `ThinkingStart`+`ThinkingEnd`,签名只经 `Partial` 传递 (两编码器的 start/end 边界都读块内签名，幂等);后续裸签名帧仍走 merge 路径 (块已在 partial.Content 里)。当日日志无此形态实流 (所有含 `deltaSignature` 的请求都有 `deltaThinking`),但该路径是注释明示支持的 openai 体制，触发即翻车。

回归测试：`TestResponseDecoderSynthesizesThinkingForBareSignature`(decoder 合成 + 二次 merge)、`TestStreamEncoderHoldsReasoningForLateSignature` + `TestStreamEncoderEncodesSignatureOnlyReasoning`(responses)、`TestStreamEncoderHoldsThinkingForLateSignature`(anthropic)。

## 实测结果 (14:54)

- `codex exec -m swe-2-max-ws -c 'model_providers.OpenAI.supports_websockets=true' --skip-git-repo-check "run echo ws-chain-test"` 一次跑通，exec_command 执行并收尾。
- `index.jsonl`:`responses-ws` ×2 均 `completed`,无重试，ccLoad 未再 cool down。
- turn2 (145409) wire 证据：`chatMessagePrompts` 4 条 = 2 user + assistant 回放 (带 `signature: sealed.v1.…`、`signatureType: sealed`、toolCalls)+ tool result;`trajectoryId`/`cascadeId` 与 turn1 相同。turn2 内部 POST 的 `input` 8 项含回传的 reasoning `encrypted_content`(与上游 signature 逐字一致，无翻倍) 与 `exec_command_0` 配对的 function_call/output。
- `cache_read=0`:上游 FIREWORKS_DEVIN usage 帧本轮未上报 cache tokens。早上 `cr=7830` 那次是 Codex 对**同一请求的重试**(两轮 chatMessagePrompts 完全相同，各 2 条 user),整段命中;本轮是真续链 (4 条消息),缓存上报是 provider 侧行为，非链路问题。
- turn2 带 `premature_end_turn` 标记：观测性启发式 (tool result 后纯文本 end_turn),本例模型正确收尾，非故障。

## 收尾状态

- ccLoad channel 294 `devin-ws` 与 `~/.codex/models.ccload.json` 的 `swe-2-max-ws` 条目**保留**——WS 链路是现行可用配置;还原备份在 `models.ccload.json.bak-ws-test`。
- :3003 已跑含全部修复的新二进制。

## 关键文件/位置速查

- devin2api WS 入口：`internal/app/websocket.go` `responsesWebSocket`(wsConns admission→upgrade→reader goroutine→session loop→runWSTurn 内发 POST /v1/responses)
- 请求规范化：`internal/app/websocket_session.go` `normalizeRequest`
- 本次 bug 现场：`internal/api/openai/responses/response.go:118`(flush 时机)、`:275`(`reasoningSignature` 容忍逻辑)
- 调试产物：`logs/20260912-143153/`、`logs/20260912-143159/`(error.json / meta.json / 05-response-events.jsonl / 06-http-response.jsonl)
- ccLoad 关键代码：`/tmp/ccload/internal/app/codex_upstream_websocket.go`(s.dial:646、reconnectWithReplay:1193、isCodexWebsocketPreviousResponseNotFound:1374)、`responses_websocket_session.go`(normalizeRequests 双请求/commit:184)、`proxy_responses_websocket.go`(executeResponsesWebsocketTurn:~390)
- devin2api 配置：`config.yaml`(auth.api_key `240127`,devin.token 是 JWT，敏感);服务 :3003,debug 目录 `logs/`。
- codex 跑法注意：macOS 无 `timeout` 命令；`codex exec` 直接跑，`-c` 支持 TOML dotted override。

## 敏感信息

- `config.yaml` devin token(JWT),`~/Desktop/src/ccload/.env` CCLOAD_PASS、ccLoad admin session token(24h 有效)——都别进 commit/外发。
