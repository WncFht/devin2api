---
name: protocol-drift
description: 对照已落库的 wire 流量与提取的 protobuf descriptor，探测上游 Devin Connect 协议漂移与 adapter 字段覆盖缺口。Devin CLI 升级后、logs/index.jsonl 里 error_stage 形态变化时、发布前、代理行为与官方客户端出现分叉时，或需要决定是否重跑 proto 提取与 binding 生成时使用。
---

# 协议漂移

代理维护着上游 Devin Connect 协议的两个证据面。`cmd/protocensus` 负责对账；本 skill 讲怎么读它的报告以及怎么处置。

## 证据模型及其极限

- `outputs/devin-proto/descriptors.pb`——提取出的 FileDescriptorSet，静态契约：客户端*可能*发什么。原始名称、包名与字段号的权威。
- `devin-2api.db` 调试表里的 `03-devin-request*.json`——adapter 实际发往上流的 protojson 请求（我们*实际*发了什么）。chat 与服务端托管搜索（GetWebSearchResults）共用此文件名空间，普查按载荷判别类型。
- 同表 `04-devin-response.jsonl`——上游返回帧。行是 `{seq,time,elapsed_ms,event,data}` 信封：真实上游帧固定 `event="frame"`、protojson 在 `data` 里；其余 event 名（account_attempt/retry_attempt/detached 等）是 adapter 标记行，不参与普查。

日志能证明的东西有硬上限：`03`/`04` 经生成的 binding 重新编码过，`protojson.Marshal` 会丢掉生成代码不认识的字段。**上游全新字段在旧日志里不可见。**仍然能浮出水面的有：

- 新枚举成员以裸数字形式出现在本该是名字的位置。
- 已知字段上的字段级漂移（填没填）仍可见。
- 新字段/消息/service 需要从更新的上游二进制重新提取 descriptor 再 diff（见下）。

## 跑 census

需要生成的 binding（`outputs/devin-proto-go/`）；缺失时先跑 `task generate`。

```sh
go run ./cmd/protocensus census -db ~/.local/state/devin-2api/devin-2api.db                # 全部请求目录
go run ./cmd/protocensus census -db ~/.local/state/devin-2api/devin-2api.db -max-dirs 500  # 只要最新 N 个目录
```

默认 `-db ./devin-2api.db`；指向平台 state 目录的生产库即查生产流量。WAL 下并发只读扫描不干扰运行实例（Open 只多落一行 `store_opens` 台账）。

## 读报告

JSON 报告分 `request` 与 `response` 两节，各含：

- `messages`——按消息类型给 `occurrences` 与逐字段命中计数。这是覆盖基线。
- `fields_never_seen`——schema 里有定义但**在确实出现过的消息类型内**零命中的字段。解读需要判断力：正当可选字段（如 `images`、`num_tokens`、`custom_tool_grammar`）是噪声；官方客户端会发而我们 adapter 从不填的字段是真缺口。拿嫌疑字段对照 `all-protos.proto` 注释与 `internal/adapter/devin` 的填充代码复核。
- `unknown_keys`——descriptor 解析不了的 JSON 键。应为空；非空意味着日志路径变了或手写投影泄漏——按 adapter bug 查，不算上游漂移。
- `enum_anomalies`——超出已知成员集的枚举值。`number:N` 表示上游返回了我们 schema 不认识的成员：**按确认漂移处理**。不在成员表里的字符串名则说明 binding 过期或不匹配。

每个异常最多带 3 个 `examples`（请求目录名）；按 dir 查 `debug_files`/`debug_chunks` 表里的 meta.json 与被引用阶段文件（`sqlite3` 直查或 `GET /admin/debug-logs/{id}/file/{name}`）。

## CLI 升级后探测字段级漂移

判定 schema 增删改的权威检查：

```sh
go run ./cmd/protoextract <new-upstream-binary> outputs/devin-proto-new
go run ./cmd/protocensus diff outputs/devin-proto/descriptors.pb outputs/devin-proto-new/descriptors.pb
```

新版上游二进制（Go 系，含可提取描述符）的获取链：`https://devin.ai/download` 页按钮 → `https://windsurf.com/api/windsurf/download-redirect?build=<platform>&isNext=false` 307 到 `windsurf-stable.codeiumdata.com/<platform>/<channel>/<commit>/Devin-<platform>-<ver>.<ext>`；`linux-x64` 是 tar.gz，语言服务器在包内 `Devin/resources/app/extensions/windsurf/bin/language_server_linux_x64`。注意 GitHub `Exafunction/codeium` 的公开 language-server release 明显落后于 Devin 桌面版内嵌构建（枚举表更短），不是合格的提取源；`devin` CLI 是 Rust 编译，无 Go 描述符。

`diff` 按类型、字段（号 + 类型+label）、枚举值和 RPC 方法报告 `added`/`removed`/`changed`。复查 diff 之后才能把新集合提升进 `outputs/devin-proto/`；然后 `task generate` 并重跑 `task census`。

读 diff 的注意点：

- 重命名呈现为 `- old_field` + `+ new_field`——查字段号；同号是改名，不同号是真变更。
- `type_name` 差异只是把 `exa.codeium_common_pb.X` 换成 `exa.api_server_pb.ExaCodeiumCommonPb_X` 的，说明拿原始名提取结果跟扁平化结果在比——无意义；要 diff 两个 `descriptors.pb`（两者都保留原始名）。

## 活行为检查

`go run ./cmd/probe rerun -file logs/<dir>/03-devin-request.json -n 8` 把抓到的请求对活上游重放，逐次打印停止原因、工具调用计数与文本尾巴。CLI 升级后或 `index.jsonl` 出现新的 `error_stage` 簇时用它：schema 没变但多次重放结果分叉，说明上游发生了行为漂移。

## 判定规则

- `enum_anomalies` 非空，或 `diff` 显示 `GetChatMessage*` 上有增删 `rpc`/`type`——升级处理：重新提取、重生成 binding、审计 `internal/adapter/devin` 中变动的符号。
- `fields_never_seen` 条目对应用户可见特性（图片、缓存、自定义工具）——多半是 adapter 覆盖缺口；先验证 adapter 确实从不填它，再按特性缺口而非漂移归档。
- schema 干净但重放分叉——上游行为漂移；把差异写成文档并调整 adapter 的解读，不动 schema。
