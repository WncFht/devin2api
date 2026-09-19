---
name: golang-benchmark
description: "Golang 基准测试、profiling 与性能度量。当编写、运行或比较 Go 基准测试，用 pprof 分析热点路径，解读 CPU/内存/trace profile，用 benchstat 分析结果，搭建 CI 基准回归检测，或用 Prometheus runtime 指标排查生产性能时使用。当开发者需要对某个性能指标做深入分析时也使用——本 skill 提供测量方法论，优化模式由 `samber/cc-skills-golang@golang-performance` 提供。'golang benchmark' 'go benchmarking' 'benchstat' 'benchmark regression' 'cpu profile' 'memory profile'"
user-invocable: true
allowed-tools: Read Edit Write Glob Grep Bash(go:*) Bash(golangci-lint:*) Bash(git:*) Agent WebFetch Bash(benchstat:*) Bash(benchdiff:*) Bash(cob:*) Bash(gobenchdata:*) Bash(curl:*) mcp__context7__resolve-library-id mcp__context7__query-docs WebSearch AskUserQuestion EnterWorktree ExitWorktree
---

# Golang Benchmark

**角色设定：** 你是一名 Go 性能度量工程师。绝不凭单次基准测试运行下结论——统计严谨性与受控条件是任何优化决策的前提。

**思考模式：** 对基准测试分析、profile 解读与性能对比任务尽可能深入地推理——深度推理能防止误读 profiling 数据，保证结论在统计上站得住。在 Claude Code 上，用 `ultrathink` 显式触发扩展思考。

**依赖：**

- benchstat: `go install golang.org/x/perf/cmd/benchstat@latest`

# Go 基准测试与性能度量

没有度量就没有性能改进——能度量，才能改进。

本 skill 覆盖完整度量工作流：写基准测试、跑基准测试、对结果做 profile、以统计严谨性做前后对比、在 CI 中跟踪回归。测量之后要应用的优化模式，→ 见 `samber/cc-skills-golang@golang-performance` skill。在运行中服务上配置 pprof，→ 见 `samber/cc-skills-golang@golang-troubleshooting` skill。

## 编写基准测试

### 文件与排序约定

基准测试函数放在以被测源文件命名的 `_bench_test.go` 文件中，而不是以单个函数命名——`parser.go` -> `parser_bench_test.go`，内含 `BenchmarkParse`、`BenchmarkEncode` 等，而不是每个函数一个 `benchmarkparse_test.go`。

- 把基准测试放在独立文件（而不是混进 `parser_test.go`）能让 `go test -bench=. ./pkg/parser` 的输出不夹杂无关的 `Test*` 噪声。
- 它把为度量设计的 fixture（大输入、长生命周期 setup）与为正确性设计的 fixture 分开——两者很少共用同一种形态。
- 该文件仍遵循 Go 的「一个源文件对应一个测试文件」约定（→ 见 `samber/cc-skills-golang@golang-testing` skill），只是用 `_bench` 后缀标明其更窄的用途。

`parser_bench_test.go` 内的 `Benchmark*` 函数顺序应与 `parser.go` 中被测函数/方法的顺序一致——读者自上而下对照两个文件时，`BenchmarkParse` 应处在与 `Parse` 相同的相对位置。

### `b.Loop()`（Go 1.24+）——首选

Go 1.24+ 的新基准测试优先用 `b.Loop()`。它只对循环体计时，并保持函数参数与结果存活，从而减少 dead-code-elimination 类错误。

```go
func BenchmarkParse(b *testing.B) {
    data := loadFixture("large.json") // setup——不计入计时
    for b.Loop() {
        Parse(data)  // 编译器无法消除这个调用
    }
}
```

旧的 `b.N` 循环依然能编译，保留现有基准测试或支持 Go <1.24 时继续用没问题。但它更容易写错：setup 可能需要 `b.ResetTimer()`，结果可能需要 sink 防止编译器把计算消除掉。Go 1.26 修复了早前 `b.Loop()` 的内联限制——1.24–1.25 上的基准测试已能从 `b.Loop()` 受益，但可能错过 1.26 带来的内联优化。

Go 1.27 的按尺寸特化分配器改变了分配密集型基准测试的基线（80 字节以下分配更快，二进制更大），与任何代码变更无关。跨 Go 1.26→1.27 工具链边界的 `benchstat` 对比应视为在测量工具链而非代码——先把「before」基准在同一工具链上重跑，再相信这个 delta。

### 内存跟踪

```go
func BenchmarkAlloc(b *testing.B) {
    b.ReportAllocs() // 或者用 -benchmem flag 运行
    var sink []byte
    for b.Loop() {
        sink = make([]byte, 1024)
    }
    _ = sink
}
```

`b.ReportMetric()` 添加自定义指标（例如吞吐率）：

```go
b.ReportMetric(float64(totalBytes)/b.Elapsed().Seconds(), "bytes/s") // b.Elapsed() 只在 b.Loop() 内有效
```

### 子基准测试与表驱动

```go
func BenchmarkEncode(b *testing.B) {
    for _, size := range []int{64, 256, 4096} {
        b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
            data := make([]byte, size)
            for b.Loop() {
                Encode(data)
            }
        })
    }
}
```

## 运行基准测试

```bash
go test -bench=BenchmarkEncode -benchmem -count=10 ./pkg/... | tee bench.txt
```

| Flag                   | 用途                              |
| ---------------------- | --------------------------------- |
| `-bench=.`             | 运行全部基准测试（正则过滤）      |
| `-benchmem`            | 报告分配（B/op、allocs/op）       |
| `-count=10`            | 运行 10 次以获得统计显著性        |
| `-benchtime=3s`        | 每个基准测试的最短时长（默认 1s） |
| `-cpu=1,2,4`           | 用不同 GOMAXPROCS 值运行          |
| `-cpuprofile=cpu.prof` | 写出 CPU profile                  |
| `-memprofile=mem.prof` | 写出内存 profile                  |
| `-trace=trace.out`     | 写出执行 trace                    |

**输出格式：** `BenchmarkEncode/size=64-8  5000000  230.5 ns/op  128 B/op  2 allocs/op`——`-8` 后缀是 GOMAXPROCS，`ns/op` 是每次操作耗时，`B/op` 是每次操作分配字节数，`allocs/op` 是每次操作的堆分配次数。

## 并行比较多个优化变体

同一瓶颈存在多个候选优化假设时，把每个变体放在独立 worktree 中由独立 sub-agent 实现，让它们的代码改动不在共享工作树里相互碰撞。

**基准测试要串行跑，不要并发跑。** 并发跑基准测试共享同一 CPU——noisy-neighbor 效应会污染 `ns/op`，把 `-count` 和 `benchstat` 本来要消除的统计噪声重新引入。并行实现是安全的（隔离 worktree，无文件争用）；并行测量不安全（共享硬件，真实争用）。每个变体的基准测试一次只跑一个，回主树跑或按 worktree 依次跑。

把每个变体的 `benchstat` 输出与**同一份**基线报告对比，留下胜者，删掉其余 worktree。

## 在提交中记录结果

当变更带来可度量的性能影响时，把 benchstat 输出贴进 commit body。这记录了优化_为什么_做，防止后来的读者把它回退掉，也让评审者不用重跑基准测试就能验证这个说法。

提交格式：

```
perf(parser): reduce Parse allocations 50% with sync.Pool

Replace per-call []byte allocation with a pooled buffer.

goos: linux / goarch: amd64 / cpu: AMD Ryzen 9 5950X
          │    old     │              new               │
          │  sec/op    │  sec/op     vs base            │
Parse-32    4.592µ ± 2%  3.041µ ± 1%  -33.78% (p=0.000 n=10)

          │   old    │             new              │
          │   B/op   │   B/op     vs base           │
Parse-32   1.024Ki ± 0%  0.512Ki ± 0%  -50.00% (p=0.000 n=10)

          │ old  │            new             │
          │ allocs/op │ allocs/op  vs base    │
Parse-32   12.00 ± 0%   6.000 ± 0%  -50.00% (p=0.000 n=10)
```

**规则：**

- 只放直接受影响的基准测试——删掉无关行
- 绝不贴带 `~` 的结果（无统计显著性）——改进不能成立
- 带上硬件上下文行（`goos/goarch/cpu`）让结果可复现
- 纯性能变更使用 `perf(scope):` 提交类型

## 从基准测试生成 profile

直接从基准测试运行生成 profile——不需要 HTTP 服务：

```bash
# CPU profile
go test -bench=BenchmarkParse -cpuprofile=cpu.prof ./pkg/parser
go tool pprof cpu.prof

# 内存 profile（alloc_objects 看 GC 搅动，inuse_space 看泄漏）
go test -bench=BenchmarkParse -memprofile=mem.prof ./pkg/parser
go tool pprof -alloc_objects mem.prof

# 执行 trace
go test -bench=BenchmarkParse -trace=trace.out ./pkg/parser
go tool trace trace.out
```

完整 pprof CLI 参考（全部命令、非交互模式、profile 解读）见 [pprof 参考](./references/pprof.md)。执行 trace 解读见 [Trace 参考](./references/trace.md)。统计对比见 [benchstat 参考](./references/benchstat.md)。

## 参考文件

- **[pprof 参考](./references/pprof.md)**——CPU、内存与 goroutine profile 的交互式与非交互式分析。完整 CLI 命令、profile 类型（CPU vs alloc_objects vs inuse_space）、web UI 导航与解读模式。用它深入挖掘代码中时间与内存花在_哪里_。

- **[benchstat 参考](./references/benchstat.md)**——带严格置信区间与 p 值检验的基准测试结果统计对比。涵盖输出读法、过滤旧基准、交错运行以获得直观效果、回归检测。需要证明变更带来真实性能差异而非走运的一次运行时用它。

- **[Trace 参考](./references/trace.md)**——执行追踪器，用于理解代码_何时_以及_为什么_运行。可视化 goroutine 调度、垃圾回收阶段、网络阻塞与自定义 span 标注。当 pprof（展示 CPU 去了_哪里_）不够用时用它——你需要看到事情发生的时间线。

- **[诊断工具](./references/tools.md)**——辅助工具速查：fieldalignment（结构体填充浪费）、GODEBUG（runtime 日志 flag）、fgprof（frame graph profile）、竞态检测器（并发 bug）等。有特定症状需要针对性诊断时用它——如果一个更简单的工具已能回答你的问题，就别动用 pprof。

- **[编译器分析](./references/compiler-analysis.md)**——底层编译器优化洞察：escape analysis（值何时搬到堆上）、内联决策（哪些函数调用被消除）、SSA dump（中间表示）与汇编输出。当基准测试出现意料之外的分配，或想验证编译器是否如你所愿时用它。

- **[CI 回归检测](./references/ci-regression.md)**——CI 流水线中的自动化性能回归门禁。涵盖三个工具（benchdiff 做快速 PR 对比、cob 做严格阈值门禁、gobenchdata 做长期趋势面板）、noisy neighbor 缓解策略（为什么云 CI 基准测试即使在空闲机器上也有 5-10% 波动），以及让基准测试可复现的 self-hosted runner 调优。想确保 pull request 不悄悄拖慢代码库时用它——尽早发现回归能避免把性能债带上线。

- **[排查会话](./references/investigation-session.md)**——生产性能排障工作流，组合 Prometheus runtime 指标（堆大小、GC 频率、goroutine 数）、把指标与代码变更关联起来的 PromQL 查询、runtime 配置 flag（GODEBUG 环境变量开启 GC 日志），以及成本警示（何时在吃性能税）。当基准测试很好看但真实流量表现不一样时用它。

- **[Prometheus Go 指标参考](./references/prometheus-go-metrics.md)**——`prometheus/client_golang` 实际暴露为 Prometheus 指标的 Go runtime 指标完整清单。涵盖 30 个默认指标、40+ 可选指标（Go 1.17+）、进程指标与常用 PromQL 查询。区分 `runtime/metrics`（Go 内部数据）与 Prometheus 指标（你从 `/metrics` 抓到的）。搭监控面板或为生产告警写 PromQL 时用它。

## 交叉引用

- → 测量之后要应用的优化模式（「X 瓶颈就用 Y」）见 `samber/cc-skills-golang@golang-performance` skill
- → 运行中服务上的 pprof 配置（启用、加固、采集）、Delve 调试器、GODEBUG flag、根因方法论见 `samber/cc-skills-golang@golang-troubleshooting` skill
- → 日常常驻监控、持续 profiling（Pyroscope）、分布式追踪（OpenTelemetry）见 `samber/cc-skills-golang@golang-observability` skill
- → 通用测试实践见 `samber/cc-skills-golang@golang-testing` skill
- → 在生产环境查询 Prometheus runtime 指标以验证基准测试结论，见 `samber/cc-skills@promql-cli` skill
