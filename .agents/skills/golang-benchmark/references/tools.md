# 诊断工具速查

应用任何优化之前，先用这些工具验证变慢的真正根因。不要用自动修复 flag（如 `--fix`）——让编码 agent 解读结果并手动改代码、附上解释性注释。

各工具的详细用法见专项参考文件：

- [pprof 参考](./pprof.md)——profiling（CPU、堆、goroutine、mutex、block）
- [benchstat 参考](./benchstat.md)——基准测试统计对比
- [Trace 参考](./trace.md)——执行追踪器
- [编译器分析](./compiler-analysis.md)——escape analysis、内联、SSA、汇编

## GC 与 Runtime 诊断

用环境变量配置——不用重新编译。

| 命令                                              | 用途                                                    |
| ------------------------------------------------- | ------------------------------------------------------- |
| `GODEBUG=gctrace=1 ./app`                         | GC 频率、暂停时长、堆大小、CPU%——每个 GC 周期一行       |
| `GODEBUG=gcpacertrace=1 ./app`                    | GC 为什么在此刻触发——pacer 决策（触发比率、堆目标）     |
| `GODEBUG=schedtrace=1000 ./app`                   | 负载均衡、P 上的 goroutine 分布——每 1000ms 打一行       |
| `GODEBUG=schedtrace=1000,scheddetail=1 ./app`     | 在 schedtrace 之上加每个 goroutine 的状态明细           |
| 堆/分配 profile（`go tool pprof -alloc_objects`） | 分配点与对象搅动；替代已移除/过期的分配 trace flag 使用 |

→ 详细的 GODEBUG 用法与解读见 `samber/cc-skills-golang@golang-troubleshooting` skill。

### 编程式 API

- **`runtime.ReadMemStats`**——堆大小、NumGC、暂停时长（PauseNs 环形缓冲）、TotalAlloc（累计）。用于面板、堆增长告警。
- **`debug.ReadGCStats`**——GC 专项统计：暂停分位数、暂停时间线、总暂停时长。比 ReadMemStats 更聚焦。
- **`runtime/metrics`（Go 1.16+）**——稳定 API、并发读安全、开销比 ReadMemStats 低。键：`/gc/cycles/total:gc-cycles`、`/gc/heap/allocs:bytes`、`/gc/pauses:seconds`、`/sched/latencies:seconds`、`/memory/classes/heap/released:bytes`。
- **`debug.FreeOSMemory()`**——强制 GC 并把内存还给 OS。只在大块临时分配之后偶尔用（不要常规用——交给 runtime 自己管）。
- **`expvar`**——stdlib 指标，在 `/debug/vars` 以 JSON 暴露。`import _ "expvar"` 自动注册。轻量、无依赖。可接 Netdata、Telegraf 或自研面板。

## 静态分析

| 命令                                     | 用途                                                                                        |
| ---------------------------------------- | ------------------------------------------------------------------------------------------- |
| `fieldalignment ./...`                   | 检测次优结构体字段排序（填充浪费）。不要用 `-fix` flag——让编码 agent 手动改并附解释性注释。 |
| `unsafe.Sizeof` / `Alignof` / `Offsetof` | 编译期检查结构体内存布局——重排前后对比量化节省。                                            |
| `go vet ./...`                           | 可疑构造：printf 格式不匹配、不可达代码、未使用的结果、可疑移位。                           |
| `staticcheck ./...`                      | 高级 linter：性能陷阱（SA9003：空分支、SA4006：未使用的值、SA1019：已废弃 API）。           |
| `go test -race ./...`                    | 运行期数据竞争检测——也可用来确认 false sharing。                                            |

## 第三方 Profiling

| 工具                                                    | 提供什么                                                                                                                      | 什么时候用                                                                                   |
| ------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| **fgprof**（`github.com/felixge/fgprof`）               | 完整 goroutine profiler——单个 profile 同时捕捉 on-CPU 与 off-CPU（I/O 等待）时间。标准 pprof CPU profile 只显示 on-CPU 时间。 | pprof CPU profile 显示 CPU% 低但延迟高时。                                                   |
| **Pyroscope / Parca**                                   | 持续 profiling 平台——按时间聚合 pprof profile、跨部署对比、检测回归。                                                         | 生产性能监控、历史趋势分析。配置 → 见 `samber/cc-skills-golang@golang-observability` skill。 |
| **Linux perf**（`perf record -g ./app && perf report`） | 硬件性能计数器：cache miss、分支误预测、TLB miss。转 pprof 格式需要 `perf_data_converter`。                                   | pprof 粒度不够时做 CPU 微架构级分析。                                                        |
