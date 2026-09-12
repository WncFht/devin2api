# 性能与内存审查：方法、基线与已落地优化

> 2026-09-12。对全仓做了一轮「读代码 + profile + 受控实验」的性能审查，落地 8 个 perf commit。本文记录测量方法、基线数字、每项优化的机制与收益、被实验推翻的假设，以及下一轮候选方向。后续实验请复用本文的基线与命令。

## 一、环境与方法

- 机器：macOS / Apple M4 / 10 核；Go 1.27.1（`go.mod` 声明 1.26.3）。
- 运行实例：launchd `com.fanghaotian.devin-2api` 监听 :3003，审查时 heap ~20MB、RSS 峰值 ~66MB，`logs/` ~1GB / 570 个请求目录。实例负载很轻，内存压力主要来自**每请求的瞬时分配速率**而非驻留堆。
- 方法：`go test -bench -benchmem` 拿基线 → `go test -memprofile/-cpuprofile` 定位 → 读代码确认机制 → 最小改动 → 复测。所有结论以 benchmark 或 profile 为准，不做推测性重写。
- 噪声提示：`BenchmarkStreamEndToEndDebugLog` 写真实文件（worker 刷盘），ns/op 方差大；跨次对比以 `B/op` 和 `allocs/op` 为准，ns/op 只做同环境内相对比较。

## 二、测量基线（改动前）

| Benchmark                           | 场景                 | 时间        | 内存/op | 分配/op |
| ----------------------------------- | -------------------- | ----------- | ------: | ------: |
| `BenchmarkStreamEndToEnd`           | SSE 全链路，debug 关 | ~666µs      |   886KB |   4,913 |
| `BenchmarkStreamEndToEndDebugLog`   | 同上，debug 开       | ~2,243µs    | 1,480KB |  10,604 |
| `BenchmarkRecvDeltaStream`          | adapter 2000 帧解码  | ~840µs      |  2.59MB |   4,055 |
| `BenchmarkWSValidatePairing`        | 200 item 配对校验    | ~220µs      |   171KB |   2,632 |
| `BenchmarkSanitizeRequest`          | 400 消息请求脱敏     | ~5,268µs    |    92KB |     801 |
| `BenchmarkSanitizeText`             | 148KB 干净文本       | ~1,756µs    |   155KB |       1 |
| `BenchmarkDecodeRequestToolHistory` | 工具历史解码         | 0.76–2.06ms |  ~1.6MB |  ~8,000 |

profile 关键发现（改动前）：

- adapter 流路径 `decode` flat 分配占 43%；每帧 `make([]llm.ResponseEvent, 0, N)` + 各子解码器各自分配事件切片是主因。`decodeText` 疑似 O(n²) 逐帧拼接被证伪——`responseDecoder` 已用 `strings.Builder`。
- debug-on 路径 `Manager.Start` 的 `make(chan writeTask, 4096)`（每请求 ~32KB）与 `rawNeedsSanitize` 的全量归一化拷贝显著。
- `randid.UUID` 的 `fmt.Sprintf` 在 buildRequest 每条消息上可见。

## 三、已落地优化（commit 顺序即实施顺序）

### 1. `089ece5` perf(app): skip debug projections when request logging is off

`recorder.WriteJSON(name, httpRequestProjection(...))` 的参数表达式在调用前求值——`WriteJSON` 内部的 nil 检查挡不住投影建树。debug 关闭时每请求仍多做：一次全量 generic unmarshal、完整消息投影树、逐事件投影。改为外层 `if recorder != nil` 门控。

- `StreamEndToEnd`：666µs→460µs，4913→3515 allocs，886KB→747KB。

### 2. `32dfadb` perf(chat): encode stream deltas as structs instead of maps

Chat delta chunk 从 `map[string]any` 换成 `chatChunk/chatChoice/chatDelta/chatToolCall` 结构体。字段声明顺序刻意对齐 map marshal 的字母序（`content < reasoning_content < role < tool_calls`），保住字节级输出不变。`chunk` 签名改为收 `[]chatChoice`，usage-only chunk 传 `[]chatChoice{}`（nil slice 会 marshal 成 `null` 而非 `[]`，语义会变）。

### 3. `04de919` perf(app): append SSE frames into the write batch, reuse batch buffer

`protocolEncoder` 接口从「返回新分配帧」改为 `AppendSSE(dst []byte, ...) []byte`，三个实现直接往批缓冲追加；`streamWriter` flush 后 `batch = batch[:0]` 复用底层数组（约定 `ResponseWriter.Write` 不留存切片）。

- 累计：`StreamEndToEnd` 886KB→390KB（-56%），4913→1850 allocs（-62%）。

### 4. `22c012f` perf(devin): share one events slice across stream sub-decoders

各子解码器（text/thinking/tool_call 等）从各自 `make(0,2)` 改为接收并扩展同一个 `events` 切片。

- `RecvDeltaStream`：2.59MB→1.37MB（-47%），4055→2052 allocs（-49%）。

### 5. `9621f1c` perf(randid): format UUIDs with fixed-size hex instead of Sprintf

`UUID`/`uuidFromBytes` 改手写定长 hex 编码。buildRequest 每条消息一个 UUID，measure 到 ~34% 的该路径分配下降。

### 6. `c70cff5` perf(devin): single-pass bucket scan for sanitize triggers

先试了正则 alternation，实测灾难性退化（见 §四）。最终实现：触发词按小写首字节分桶，干净文本单趟扫描 + `strings.EqualFold` 候选匹配，未命中零分配返回原串。

- `SanitizeText`：~1.76ms→~1ms，155KB→**0 B/op, 0 allocs/op**。
- `SanitizeRequest`：~5.27ms→~2.1–2.4ms。

### 7. `6976dc4` perf(app): parse websocket transcript items once per turn

修了一个 benchmark bug：`BenchmarkWSMergeInput` 用 `json.Marshal(map{"input": []byte})` 把 input 编成 base64 字符串，400 条历史 item 从未进合并——真实成本是 ~12.5K allocs/轮而非报告的 44。

重构：`wsSession` 缓存解析态——`stagedTop`（top 字段 map，payload 只解一次）+ `stagedItems`/`lastItems`/`lastOutputItems`（`[]wsItem{raw, fields wsItemFields}`）。commit 转正后下一轮历史零重解析；merge/dedupe/pairing 全部读同一份探针。`marshalWSItems` 手工拼接已合法 raw 数组，避开 `json.Marshal([]RawMessage)` 的逐条 compact 校验。

- `BenchmarkWSNormalizeTurn`（首轮建态 + 续轮规范化全程，400 item 历史）：**1,558 allocs**，旧实现仅 merge 一步 ~12.5K。
- `BenchmarkWSValidatePairing`：220µs/2,632 allocs → **11µs/15 allocs**。

### 8. `8a0d4b9` perf(debuglog): marshal proto frames and prescreen keys without copies

两件事：

- `protoJSON` 包装器让 `protojson.Marshal` 推迟到日志 worker 执行（上游泵 goroutine 只携带消息引用）。marshal 失败从 `devin_proto_encode` error stage 变为记录文件内 `serialization_error` 条目（实际不可达）。
- `rawNeedsSanitize` 从「全量小写 + 去下划线拷贝再 14 个 `bytes.Contains`」改为零分配引号区间扫描：敏感键只在 `"key":` 位置匹配，值里的 `"token"` 文本不再误进慢路径。脱敏语义不变（`sanitizeValue` 本来就只对 map 键脱敏）。

- `StreamEndToEndDebugLog`：1,480KB→~1.05MB，10,604→~8,200 allocs。

## 四、被实验推翻的假设（教训存档）

1. **「decodeText 每帧做全量拼接，O(n²)」——证伪。** profile 里 `strings.Builder.WriteString` 占比高是因为解码器本来就用 Builder 累积，不是 bug。读代码优先于按 profile 脑补。
2. **「正则 alternation 合并触发词更快」——证伪。** `(?i)` 大小写不敏感 alternation 让 RE2 失去字面量优化，148KB 文本 ~50ms，比原方案慢 28 倍；去掉 `(?i)` 扫小写文本也要 ~38ms。手写首字节分桶（~630µs）才是正解。
3. **「WS merge 只有 44 allocs」——benchmark 本身错了。** `json.Marshal` 对 `[]byte` 输出 base64，构造的 input 不是数组。教训：验证 benchmark 确实测到了目标路径（比如用 `testing.AllocsPerRun` 探针交叉验证数量级）。

## 五、下一轮候选方向（按预期收益排序）

1. **`Manager.Start` 的 `make(chan writeTask, 4096)`** —— debug 开启时每请求固定 ~32KB。可按需缩队列（如 256）或分段写盘；要权衡丢帧阈值。
2. **debug-on 剩余成本**：AppendJSONL worker 内的 sanitize+marshal 仍占大头（~280MB cum profile）。方向：投影侧少建 map（事件投影直接产 RawMessage）、JSONLRecord 的 Data 换 `json.RawMessage` 免二次 marshal。
3. **`buildRequest`/`promptForContent`/`convertMessage` 的字符串拼装**：profile 可见但量级中等，可用 `strings.Builder` 复用或预分配。
4. **WS `marshalWSItems` 的字节拷贝**：merged 数组整体再拷一份进 top map；理论上可让 stagedItems 的 raw 直接指向 marshal 产物（已在实现中做了一半），进一步可让 `top["input"]` 与 stagedItems 共享底层缓冲省一次 110KB 拷贝——收益有限，优先级低。
5. **index.jsonl / usage 回放**：启动回放尾部 ≤64MB，索引单调增长；长期看可加分层压缩或周期截断。
6. **日志总量**：`logs/` 已 ~1GB。payload_hours 剥离已存在；可观察 `04`/`06` 的体积占比决定是否对 SSE 帧做更激进的采样。

## 六、复现命令

```bash
# 全量测试与静态检查（cmd/protocensus 是未跟踪 WIP，vet 会报 undefined: join，不属于本工作）
go test ./...
go vet ./internal/... ./cmd/devin-2api ./cmd/protoextract

# 核心基准
go test ./internal/app -run=^$ -bench='StreamEndToEnd|WSNormalizeTurn|WSValidatePairing|WSWriteFrame' -benchmem -count=3
go test ./internal/adapter/devin -run=^$ -bench='RecvDeltaStream|Sanitize' -benchmem -count=3
go test ./internal/api/openai/chat -run=^$ -bench=. -benchmem -count=3

# profile
go test ./internal/app -run=^$ -bench=StreamEndToEnd -memprofile=/tmp/app-mem.out -cpuprofile=/tmp/app-cpu.out
go tool pprof -top -alloc_space /tmp/app-mem.out
```

环境约定（详 `AGENTS.md`）：不要手动起 :3003 实例；临时实验用 :3005；部署走 `scripts/deploy.sh` + launchd；重启只发 SIGTERM。
