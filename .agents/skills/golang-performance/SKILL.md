---
name: golang-performance
description: "Go 性能优化模式与方法论——瓶颈是 X 就应用 Y。覆盖减少分配、CPU 效率、内存布局、GC 调优、对象池、缓存与热路径优化。当 profiling 或基准测试已定位瓶颈、需要正确的优化模式来修复时使用；做性能代码评审、提出改进建议、或评估哪些基准测试能快速发现性能收益时也可使用。不用于测量方法论（→ 见 `golang-benchmark` skill）或调试工作流（→ 见 `golang-troubleshooting` skill）。'if X bottleneck then apply Y' 'allocation reduction' 'GC tuning' 'hot-path optimization' 'performance code review'"
user-invocable: true
allowed-tools: Read Edit Write Glob Grep Bash(go:*) Bash(golangci-lint:*) Bash(git:*) Agent WebFetch Bash(benchstat:*) Bash(fieldalignment:*) Bash(staticcheck:*) Bash(curl:*) Bash(fgprof:*) Bash(perf:*) WebSearch AskUserQuestion EnterWorktree ExitWorktree
---

# Golang Performance

**人设：**你是一名 Go 性能工程师。没有 profiling 绝不优化——先测量、提出假设、一次只改一处、再测量。

**思考模式：**对性能优化做尽可能深入的推理——浅层分析会误判瓶颈，深入推理才能保证把正确的优化用在正确的问题上。在 Claude Code 上，用 `ultrathink` 显式触发扩展思考。

**编排模式：**把评审模式（架构）里描述的三个 sub-agent（分配与内存布局、I/O 与并发、算法复杂度与缓存）发散出去，做一轮全面的架构级性能评审。单条热路径的评审保持顺序执行；只有到包/服务粒度，发散才划算。在 Claude Code 上，用 `ultracode` 显式启用多代理编排。

**模式：**

- **评审模式（架构）**——对一个包或服务做结构性反模式广扫（缺连接池、goroutine 无界、数据结构选错）。最多用 3 个并行 sub-agent 按关注点拆分：(1) 分配与内存布局，(2) I/O 与并发，(3) 算法复杂度与缓存。
- **评审模式（热路径）**——针对调用方指认的单个函数或紧凑循环做聚焦分析。顺序执行，一个 sub-agent 足够。
- **优化模式**——profiling 已定位瓶颈。按迭代循环（定义指标 → 基线 → 诊断 → 改进 → 对比）顺序执行——一次只改一处是纪律。

**依赖：**

- benchstat：`go install golang.org/x/perf/cmd/benchstat@latest`

# Go 性能优化

## 核心理念

1. **先 profiling 再优化**——对瓶颈位置的直觉约 80% 是错的。用 pprof 找真正的热点（→ 见 `golang-troubleshooting` skill）
2. **减少分配回报最大**——Go 的 GC 快但不是免费。减少每请求分配常常比微调 CPU 更值得
3. **优化要写文档**——加注释说明为什么这个模式更快，有基准数据就附上数字。后来的读者需要这些上下文，才不会把「没必要」的优化回滚掉

## 先排除外部瓶颈

优化 Go 代码之前，先确认瓶颈在你的进程里——如果 90% 的延迟来自一条慢 DB 查询或 API 调用，减少分配无济于事。

**诊断：**1- `fgprof`——同时捕获 on-CPU 与 off-CPU（I/O 等待）时间；off-CPU 占主导说明瓶颈在外部 2- `go tool pprof`（goroutine profile）——大量 goroutine 阻塞在 `net.(*conn).Read` 或 `database/sql` 就是外部等待 3- 分布式追踪（OpenTelemetry）——span 分解能看出哪个上游慢

**确认是外部瓶颈时：**去优化那个组件——查询调优、缓存、连接池、熔断器（见 [缓存模式](references/caching.md) 与 [I/O 与网络](references/io-networking.md)）。

## 迭代优化方法论

### 循环：定义目标 → 基准测试 → 诊断 → 改进 → 基准测试

1. **定义指标**——延迟、吞吐、内存还是 CPU？没有目标的优化是乱枪打鸟
2. **写原子化基准测试**——每个基准测试只隔离一个函数，避免结果互相污染（→ 见 `golang-benchmark` skill）
3. **测基线**——`go test -bench=BenchmarkMyFunc -benchmem -count=6 ./pkg/... | tee /tmp/report-1.txt`
4. **诊断**——用各深入章节的 **诊断** 行选工具
5. **改进**——一次只应用一个优化，并加注释说明
6. **对比**——`benchstat /tmp/report-1.txt /tmp/report-2.txt` 确认统计显著性
7. **提交**——把 benchstat 输出贴进 commit body，让 reviewer 和后来的读者看到确切改进；commit 类型用 `perf(scope): summary`
8. **重复**——报告编号递增，处理下一个瓶颈

动手造方案前先查库文档里的已知模式。保留所有 `/tmp/report-*.txt` 文件作为审计轨迹。

当多个候选优化方案竞争同一个瓶颈时，把每个方案放进独立 worktree、由各自的 sub-agent 实现——然后 → 见 `golang-benchmark` skill 对比各变体，并注意其串行测量警告（共享 CPU 上并发跑基准测试会污染结果，即使实现本身是并行构建的）。

## 决策树：时间花在哪？

| 瓶颈             | 信号（来自 pprof）                 | 动作                                                                       |
| ---------------- | ---------------------------------- | -------------------------------------------------------------------------- |
| 分配过多         | heap profile 中 `alloc_objects` 高 | [内存优化](references/memory.md)                                           |
| CPU 受限的热循环 | 某函数主导 CPU profile             | [CPU 优化](references/cpu.md)                                              |
| GC 停顿 / OOM    | GC% 高、容器限额                   | [Runtime 调优](references/runtime.md)                                      |
| 网络 / I/O 延迟  | goroutine 阻塞在 I/O 上            | [I/O 与网络](references/io-networking.md)                                  |
| 重复的昂贵工作   | 同一计算/拉取执行多次              | [缓存模式](references/caching.md)                                          |
| 算法选错         | 存在 O(n) 却用了 O(n²)             | [算法复杂度](references/caching.md#算法复杂度)                             |
| 锁竞争           | mutex/block profile 热             | → 见 `golang-concurrency` skill                                            |
| 慢查询           | trace 中 DB 时间占主导             | 查询调优、索引、攒批；重复查询结果缓存见 [缓存模式](references/caching.md) |

## 常见错误

| 错误                              | 修法                                                                     |
| --------------------------------- | ------------------------------------------------------------------------ |
| 没 profiling 就优化               | 先用 pprof——直觉约 80% 是错的                                            |
| 默认 `http.Client` 不配 Transport | `MaxIdleConnsPerHost` 默认只有 2；按并发量设置                           |
| 热循环里打日志                    | 日志调用阻碍内联，级别关闭也照样分配。用 `slog.LogAttrs`                 |
| 拿 `panic`/`recover` 当控制流     | panic 要分配栈追踪并展开栈；用 error 返回                                |
| 没有基准证据就上 `unsafe`         | 仅当 profiling 显示已验证热路径有 >10% 提升时才值得                      |
| 容器里不调 GC                     | 把 `GOMEMLIMIT` 设为容器内存的 80-90%，防 OOM kill                       |
| 生产代码用 `reflect.DeepEqual`    | 比类型化比较慢 50-200 倍；用 `slices.Equal`、`maps.Equal`、`bytes.Equal` |

## 深入专题

- [内存优化](references/memory.md)——分配模式、底层数组泄漏、sync.Pool、结构体对齐
- [CPU 优化](references/cpu.md)——内联、缓存局部性、伪共享、ILP、避免反射
- [I/O 与网络](references/io-networking.md)——HTTP transport 配置、流式处理、JSON 性能、cgo、批量操作
- [Runtime 调优](references/runtime.md)——GOGC、GOMEMLIMIT、GC 诊断、GOMAXPROCS、PGO
- [缓存模式](references/caching.md)——算法复杂度、预编译模式、singleflight、避免无谓工作
- [生产可观测性](references/observability.md)——Prometheus 指标、PromQL 查询、持续性能分析、告警规则

## CI 回归检测

在 CI 里自动化基准测试对比，在回归进入生产前抓住它。`benchdiff` 与 `cob` 的配置 → 见 `golang-benchmark` skill。

## 交叉引用

- → 见 `golang-benchmark` skill：基准测试方法论、`benchstat` 与 `b.Loop()`（Go 1.24+）
- → 见 `golang-troubleshooting` skill：pprof 工作流、逃逸分析诊断与性能调试
- → 见 [内存优化](references/memory.md)：切片/map 预分配与 `strings.Builder`
- → 见 `golang-concurrency` skill：worker pool、`sync.Pool` API、goroutine 生命周期与锁竞争
- → 循环中的 defer 见 `golang-troubleshooting` skill（common-go-bugs.md）；切片底层数组别名见 [内存优化](references/memory.md)
- → 见 [I/O 与网络](references/io-networking.md)：连接池调优与批处理
- → 见 [生产可观测性](references/observability.md)：生产环境持续性能分析
