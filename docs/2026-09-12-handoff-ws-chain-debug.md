# Handoff: Codex → ccLoad → devin2api WebSocket 多轮全链路调试

> 2026-09-12 交接。目标：让 Codex CLI 通过 ccLoad 走 devin2api 的 codex(responses) 链路，并发挥 WS multi-turn(单连接多轮 + previous_response_id 增量)。**链路已打通到「WS 握手 ✓ → 双跳 upstream WS ✓ → 上游返回完整流 ✓」，最后一公里挂在 devin2api 的 SSE 编码器 bug(已定位、已改、未验证上线)。**

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

## 待做 (按序)

1. **复查 decoder 侧一个疑似 latent bug**(我还没来得及看完):`internal/adapter/devin/response_decoder.go:391` `decodeLateSignature` 的合成路径 (上游从无 thinking 块、签名裸到时) 会 emit `ThinkingStart`(此时 Partial 里的 ThinkingContent 已含 signature)+ `ThinkingSignature`(Delta=同一签名)→ `startReasoning` 已 set `item.encryptedContent`,`reasoningSignature` 又 `+=` → **签名翻倍**;且该合成 item 可能永远收不到 `thinking_end` → `pendingDone`/`done()` "open item" 报错。查 `endThinking` 调用点和真实 openai-style(signature-only) 上游流再定。
2. **加回归测试**:responses `thinking_end(0)→toolcall→signature(0)→toolcall_end→done` 序列断言 reasoning item done 含 encrypted_content 且 `response.completed` 成功；anthropic 侧同序断言 `signature_delta`+thinking block 带 signature。放 `internal/api/openai/responses/response_test.go` / `messages/response_test.go`。
3. **跑全量测试** `go test ./...`(工作区有大量未提交改动，见下)。
4. **重跑 codex e2e**:重新 `go build -o devin-2api`,重启 :3003 进程 (当前进程是 14:10 构建的旧二进制，**不含本次修复**),再跑上面那条 `codex exec`。观察点：`logs/index.jsonl` 里 `api:"responses-ws"` `result:"success"`;第二轮应走 prev_id 增量 (input 只有 tool output,index.jsonl 里 input_tokens 应主要靠 cache_read);ccLoad 侧不应再 cool down。
5. **多轮验证**:确认同一 WS 连接上第二轮 create 带 `previous_response_id` 正常合并且 `cache_read` 命中 (上轮 143159 已见 `cache_read:7830`,说明 transcript 重放本身就是通的——只是编码器挂了)。
6. **收尾清理**:测试完可禁用/删除 ccLoad channel 294 `devin-ws`;`~/.codex/models.ccload.json` 可用 `.bak-ws-test` 还原。

## 工作区状态 (重要)

`git status` 显示**一大批未提交改动**,其中 WS multi-turn 主体 + 本次编码器修复都还没 commit。之前 session 的提交边界在 `git log` 里未必清晰——先 `git diff` 梳理再决定怎么切 commit。docs 目录下还有几篇调研文档未提交。

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
