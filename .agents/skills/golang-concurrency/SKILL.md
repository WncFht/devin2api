---
name: golang-concurrency
description: "Go 并发设计——goroutine 生命周期与泄漏预防、channel 与 `select`、channel 所有权与方向、`sync.Mutex`/`RWMutex`/`sync.Map`/`sync.Once`/atomic、`errgroup`、`singleflight`、worker pool、fan-out/fan-in 流水线。编写或评审并发 Go 代码、在 channel 与 mutex 之间取舍、保护共享 map 或计数器、或 goroutine 没有明确退出路径时使用。不用于与并发无关的防御性编程，如 nil panic、slice 别名、数值溢出（→ 见 `samber/cc-skills-golang@golang-safety` skill），也不用于事后调试某个挂起、崩溃或数据竞争的具体程序（→ 见 `samber/cc-skills-golang@golang-troubleshooting` skill）。'writing or reviewing concurrent Go code' 'choosing between channels and mutexes' 'goroutine has no clear exit'"
user-invocable: true
allowed-tools: Read Edit Write Glob Grep Bash(go:*) Bash(golangci-lint:*) Bash(git:*) Agent AskUserQuestion
---

# Go 并发

**人设：** 你是一名 Go 并发工程师。每个 goroutine 在被证明必要之前一律按负债对待——正确性与不泄漏优先于性能。

**编排模式：** 在大型代码库上审计并发代码时，按「并行化并发审计」一节发散 5 个 sub-agent，把它们的发现汇总成一份报告。在 Claude Code 上，用 `ultracode` 显式启用多代理编排。

**模式：**

- **写模式**——实现并发代码（goroutine、channel、sync 原语、worker pool、流水线）。按下面的顺序指引执行。
- **评审模式**——评审 PR 中的并发代码改动。聚焦 diff：查 goroutine 泄漏、缺失的 context 传播、所有权违规、未保护的共享状态。顺序执行。
- **审计模式**——审计代码库中已有的并发代码。按「并行化并发审计」一节所述，最多用 5 个并行 sub-agent。

> **社区默认值。** 显式声明取代 `samber/cc-skills-golang@golang-concurrency` 的公司内部 skill 优先。

# Go 并发最佳实践

Go 的并发模型建立在 goroutine 与 channel 之上。goroutine 便宜但不免费——你启动的每个 goroutine 都是必须管理的资源。目标是结构化并发：每个 goroutine 有明确的所有者、可预期的退出、正确的错误传播。

## 核心原则

1. **每个 goroutine 必须有明确的退出路径**——没有关停机制（context、done channel、WaitGroup），它们会泄漏、堆积，直到进程崩溃
2. **通过通信共享内存**——channel 显式转移所有权；mutex 保护共享状态，但所有权变成隐式
3. **channel 上发副本，不发指针**——发指针会制造不可见的共享内存，抵消 channel 的意义
4. **只有发送方关闭 channel**——接收方关闭时，发送方在 close 后写入会 panic
5. **指明 channel 方向**（`chan<-`、`<-chan`）——编译器在构建期挡住误用
6. **默认用无缓冲 channel**——大缓冲掩盖背压；只在有实测依据时才用
7. **select 里永远带上 `ctx.Done()`**——没有它，调用方取消后 goroutine 会泄漏
8. **热循环里避免反复 `time.After`**——每次调用都分配 timer，造成无谓抖动；长期运行的循环用 `time.NewTimer` + `Reset`
9. **在测试里跟踪 goroutine 泄漏**，用 `go.uber.org/goleak`

channel/select 的详细代码示例见 [Channel 与 Select 模式](references/channels-and-select.md)。

## Channel vs Mutex vs Atomic

| 场景                    | 用                            | 为什么                                      |
| ----------------------- | ----------------------------- | ------------------------------------------- |
| goroutine 之间传数据    | Channel                       | 传达所有权转移                              |
| 协调 goroutine 生命周期 | Channel + context             | 用 select 干净关停                          |
| 保护共享 struct 字段    | `sync.Mutex` / `sync.RWMutex` | 简单临界区                                  |
| 简单计数器、标志位      | `sync/atomic`                 | 无锁，开销更低                              |
| 多读少写的 map          | `sync.Map`                    | 为读多负载优化。**并发读写 map 会直接崩溃** |
| 缓存昂贵计算            | `sync.Once` / `singleflight`  | 执行一次或去重                              |

## WaitGroup vs errgroup

| 需求                          | 用                     | 为什么             |
| ----------------------------- | ---------------------- | ------------------ |
| 等 goroutine 结束，不需要错误 | `sync.WaitGroup`       | 发后等待           |
| 等待 + 收集第一个错误         | `errgroup.Group`       | 错误传播           |
| 等待 + 首个错误时取消兄弟任务 | `errgroup.WithContext` | 出错时取消 context |
| 等待 + 限制并发度             | `errgroup.SetLimit(n)` | 内建 worker pool   |

## Sync 原语速查

| 原语                  | 用途                   | 要点                                                                                                                                                   |
| --------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `sync.Mutex`          | 保护共享状态           | 临界区保持短；绝不跨 I/O 持锁                                                                                                                          |
| `sync.RWMutex`        | 多读少写               | 绝不把 RLock 升级成 Lock（死锁）                                                                                                                       |
| `sync/atomic`         | 简单计数器、标志位     | 优先类型化 atomic（Go 1.19+）：`atomic.Int64`、`atomic.Bool`                                                                                           |
| `sync.Map`            | 并发 map，读多         | 无需显式加锁；写主导时用 `RWMutex`+map                                                                                                                 |
| `sync.Pool`           | 复用临时对象           | `Put()` 前永远 `Reset()`；降低 GC 压力                                                                                                                 |
| `sync.Once`           | 一次性初始化           | Go 1.21+：`OnceFunc`、`OnceValue`、`OnceValues`                                                                                                        |
| `sync.WaitGroup`      | 等待一组简单 goroutine | Go 1.25+：发后等待、不 panic、不需要错误传播的任务优先 `wg.Go(func(){ ... })`。Go <1.25 用 `Add`/`Done`。要错误/取消/限并发，用 `errgroup` + context。 |
| `x/sync/singleflight` | 并发调用去重           | 防缓存击穿                                                                                                                                             |
| `x/sync/errgroup`     | goroutine 组 + 错误    | `SetLimit(n)` 取代手写 worker pool                                                                                                                     |

详细示例与反模式见 [Sync 原语深入](references/sync-primitives.md)。

## 并发清单

启动 goroutine 前先回答：

- [ ] **它怎么退出？**——context 取消、channel 关闭或显式信号
- [ ] **我能给它发停止信号吗？**——传 `context.Context` 或 done channel
- [ ] **我能等它结束吗？**——`sync.WaitGroup` 或 `errgroup`
- [ ] **谁拥有这些 channel？**——创建方/发送方拥有并负责关闭
- [ ] **这里该用同步吗？**——没有实测需要就别上并发

## 流水线与 Worker Pool

流水线模式（fan-out/fan-in、有界 worker、生成器链、Go 1.23+ 迭代器、`samber/ro`）见 [流水线与 Worker Pool](references/pipelines.md)。

## 并行化并发审计

在大型代码库上审计并发时，最多用 5 个并行 sub-agent：

1. 找出所有 goroutine 启动点（`go func`、`go method`），核验关停机制
2. 找出可变全局变量与无同步保护的共享状态
3. 审计 channel 用法——所有权、方向、关闭、缓冲大小
4. 找循环里的 `time.After`、select 里缺失的 `ctx.Done()`、无界启动
5. 检查 mutex 用法、`sync.Map`、atomic 与线程安全文档

## 常见错误

| 错误                    | 修法                                             |
| ----------------------- | ------------------------------------------------ |
| 发后不管的 goroutine    | 提供停止机制（context、done channel）            |
| 接收方关闭 channel      | 只有发送方关闭                                   |
| 热循环里的 `time.After` | 复用 `time.NewTimer` + `Reset`                   |
| select 缺 `ctx.Done()`  | 永远在 select 里带上 context 以便取消            |
| 无界 goroutine 启动     | 用 `errgroup.SetLimit(n)` 或信号量               |
| 经 channel 共享指针     | 发送副本或不可变值                               |
| goroutine 内部 `wg.Add` | `Add` 在 `go` 之前调用——否则 `Wait` 可能提前返回 |
| CI 里忘开 `-race`       | 永远跑 `go test -race ./...`                     |
| 跨 I/O 持有 mutex       | 临界区保持短                                     |

## 交叉引用

- → false sharing、cache-line padding、`sync.Pool` 热路径模式见 `samber/cc-skills-golang@golang-performance` skill
- → 取消传播与超时模式见 `samber/cc-skills-golang@golang-context` skill
- → 并发 map 访问与数据竞争防护见 `samber/cc-skills-golang@golang-safety` skill
- → goroutine 泄漏与死锁调试见 `samber/cc-skills-golang@golang-troubleshooting` skill
- → 优雅关停模式见 `samber/cc-skills-golang@golang-design-patterns` skill
- → 按上述准则在 CI 中做 AI 驱动的自动代码评审见 `samber/cc-skills-golang@golang-continuous-integration` skill

### Goroutine 泄漏 profile

goroutine 泄漏 profile（Go 1.26 中 `GOEXPERIMENT=goroutineleakprofile` 实验特性）自 Go 1.27 起在 `runtime/pprof` 中 GA——不需要构建 flag。它与下面列出的现有工具并列，是面向生产的泄漏信号。

```bash
curl http://localhost:6060/debug/pprof/goroutineleak?debug=2
go tool pprof http://localhost:6060/debug/pprof/goroutineleak
```

保留现有工具：

- 测试：`go.uber.org/goleak`
- 运行时计数：`runtime.NumGoroutine()`
- 栈 dump：`/debug/pprof/goroutine?debug=2`
- 竞争检查：`go test -race ./...`

## 参考资料

- [Go Concurrency Patterns: Pipelines](https://go.dev/blog/pipelines)
- [Effective Go: Concurrency](https://go.dev/doc/effective_go#concurrency)
