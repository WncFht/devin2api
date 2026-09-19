---
name: golang-troubleshooting
description: "系统化排障 Go 程序——找到并修复根因。在 Go 代码遇到 bug、崩溃、死锁、数据竞争或异常行为时使用。涵盖调试方法论、常见 Go 陷阱、测试驱动调试、pprof 配置与采集、Delve、竞态检测、GODEBUG 追踪与生产环境排障。任何「有东西不对」的情况从这里开始。不用于：解读 profile 或基准测试（→ 见 `samber/cc-skills-golang@golang-benchmark` skill）、应用优化模式（→ 见 `samber/cc-skills-golang@golang-performance` skill）、设计新代码（防御式编程见 `samber/cc-skills-golang@golang-safety` skill、并发设计见 `samber/cc-skills-golang@golang-concurrency` skill）。'troubleshoot golang' 'something is wrong' 'deadlock' 'data race' 'debugging'"
user-invocable: true
allowed-tools: Read Edit Write Glob Grep Bash(go:*) Bash(golangci-lint:*) Bash(git:*) Bash(dlv:*) Agent WebFetch WebSearch AskUserQuestion
---

# Golang 排障

**人设：**你是 Go 系统调试器。跟随证据而不是直觉——插桩、复现、系统化地追根因。

**思考模式：**为调试与根因分析做尽可能充分的推理——仓促推理得到的是症状级修复，深入思考才找到真正的根因。在 Claude Code 上用 `ultrathink` 显式触发扩展思考。

**编排模式：**在全仓 bug 搜猎时，按「代码库 bug 搜猎模式」所述散开五个 bug 类目的子代理。单问题调试会话保持顺序执行；只有广撒网找未知 bug 时编排才划算。在 Claude Code 上用 `ultracode` 显式启用多代理编排。

**模式：**

- **单问题调试**（默认）：按顺序执行黄金规则——读错误、复现、一次一个假设。不要起子代理；单一已知症状用聚焦的顺序排查更快。
- **代码库 bug 搜猎**（对大型代码库做显式审计）：最多并行起 5 个子代理，每个负责一类 bug（nil/接口、资源、错误处理、竞态、context/slice/map）。只在用户要求大范围扫描时用此模式，不用于排查某个具体上报的问题。

**依赖：**

- dlv：`go install github.com/go-delve/delve/cmd/dlv@latest`

# Go 排障指南

**先做根因调查，再谈修复。**症状级修复会制造新 bug、浪费时间。这套流程在时间压力下尤其要遵守——赶工会引发级联故障，解决起来更久。

当用户报告 Go 代码中的 bug、崩溃、性能问题或异常行为：

1. **从下面的决策树开始**，识别症状类别并跳到对应小节。
2. **遵守黄金规则**——特别是：先复现再修、一次一个假设、找到根因。
3. **逐步执行通用调试方法论**。不要跳步。
4. **警惕自己推理中的危险信号**。如果发现自己在不理解原因的情况下猜修复方案，停下来收集更多证据。
5. **逐步升级工具。**从最简单的诊断（`fmt.Println`、测试隔离）开始，只有简单工具不够时才上 pprof、Delve 或 GODEBUG。
6. **永远不要提你解释不了的修复。**如果你不理解 bug 为什么发生，直说，继续查。

## 快速决策树

```
你看到了什么？

「编译不过」
  → go build ./... 2>&1, go vet ./...
  → 见 [compilation.md](./references/compilation.md)

「输出不对 / 逻辑 bug」
  → 写一个失败测试 → 查错误处理、nil、差一错误
  → 见 [common-go-bugs.md](./references/common-go-bugs.md)、[testing-debug.md](./references/testing-debug.md)

「随机崩溃 / panic」
  → GOTRACEBACK=all ./app → go test -race ./...
  → 见 [common-go-bugs.md](./references/common-go-bugs.md)、[diagnostic-tools.md](./references/diagnostic-tools.md)

「时好时坏」
  → go test -race ./...
  → 见 [concurrency-debug.md](./references/concurrency-debug.md)、[testing-debug.md](./references/testing-debug.md)

「程序挂死 / 冻结」
  → curl localhost:6060/debug/pprof/goroutine?debug=2
  → 见 [concurrency-debug.md](./references/concurrency-debug.md)、[pprof.md](./references/pprof.md)

「CPU 占用高」
  → pprof CPU profiling
  → 见 [performance-debug.md](./references/performance-debug.md)、[pprof.md](./references/pprof.md)

「内存随时间增长」
  → pprof heap profiling
  → 见 [performance-debug.md](./references/performance-debug.md)、[concurrency-debug.md](./references/concurrency-debug.md)

「慢 / 高延迟 / p99 毛刺」
  → CPU + mutex + block profiles
  → 见 [performance-debug.md](./references/performance-debug.md)、[diagnostic-tools.md](./references/diagnostic-tools.md)

「简单 bug，容易复现」
  → 写个测试，加 fmt.Println / log.Debug
  → 见 [testing-debug.md](./references/testing-debug.md)
```

**记住：**读错误 → 复现 → 一次测一件事 → 修 → 验证

大多数 Go bug 是：漏掉的错误检查、nil 指针、忘掉的 context cancel、没关的资源、数据竞争，或者静默吞错误。

## 黄金规则

### 1. 先读错误信息

Go 的错误信息是精确的。做任何事之前先完整读它：

- **文件与行号** → 直接过去看
- **类型不匹配** → 查函数签名、接口满足
- **"undefined"** → 查 import、导出名、build tag
- **"cannot use X as Y"** → 查具体类型 vs 接口

### 2. 先复现再修

永远不要靠猜调试——先复现。永远：

- 写一个能抓住这个 bug 的失败测试
- 让它确定性复现
- 隔离出最小失败样例
- 用 `git bisect` 找引入 bug 的提交

### 3. 不测就是猜

性能或并发 bug 永远不要靠直觉：

- **pprof 胜过直觉**
- **竞态检测器胜过推理**
- **基准测试胜过假设**

### 4. 一次一个假设

改一处，测量，确认。一次改三处就什么都学不到。

### 5. 找根因——不许绕过

写修复前必须弄清 bug **为什么**发生。遮住症状的创可贴把缺陷留在原地，它会在别处再冒头——通常离原因更远，第二次更难追。

不理解问题时：

- **沿数据流回溯**，从症状追到它的源头。
- **质疑你的假设。**你信任的代码可能就是错的。
- **连问五个「为什么」。**一直追到真正的根因。
- **多做排查动作。**更多 fmt.Println、更多输出检查……

### 6. 调研代码库，不只看 diff

标记 bug 或提修复之前，先追数据流、查上游处理。孤立看有问题的函数在上下文里可能是对的——调用方可能校验了输入，中间件可能维持了不变量，周围代码可能保证了函数依赖的条件。

1. **追调用方**——谁调这个函数、传的什么值？调用点可以用代码搜索工具找。→ 见 `samber/cc-skills-golang@golang-gopls` skill，它能穿过接口与嵌套解析真实符号——找到间接调用点，跳过纯 grep 会漏或误配的同名标识符。
2. **查上游校验**——链上更早的输入解析、类型转换或 guard 子句可能让这个「bug」不可达。
3. **读周围代码**——中间件、拦截器或 init 函数可能建立了函数依赖的状态。

**当上下文降低了严重度但没消除问题时：**仍然上报，按降低后的优先级，并注明是哪些上游保证在保护它。加一行简短的内联注释（例如 `// note: safe because caller validates via parseID() which returns uint`），把推理留下来给后来的评审者。

### 7. 从简单开始

有时 `fmt.Println` 就是本地调试的正确工具。只在简单方法失效时才升级工具。永远不要把 `fmt.Println` 用于生产排障——用 `slog`。

## 危险信号：你调试错了

出现以下任何一条，停下来回到第 1 步：

- **「先快速修一下，回头再查」**——没有「回头」。找根因。
- **同时改多处**——一次一个假设。
- **不理解原因就提修复**——「也许在这加个 nil 检查……」是猜，不是调试。
- **每修一处冒一个新问题**——你在治症状。真 bug 在别处。
- **同一问题尝试修复 3 次以上**——你的心智模型错了。重读代码，从头追数据流。
- **「我机器上是好的」**——你没有隔离出环境差异。
- **怪框架/标准库/编译器**——几乎从来不是 Go 的 bug。先验证你自己的代码。

## 参考文件

- **[通用调试方法论](./references/methodology.md)**——系统化 10 步流程：定义症状、隔离复现、提出一个假设、检验它、验证根因、防回归。升级指南：何时从 `fmt.Println` 升级到日志、pprof、Delve，以及如何避免「同时改多处」的陷阱。

- **[常见 Go bug](./references/common-go-bugs.md)**——搞崩 Go 代码的那些 bug：nil 指针解引用、接口 nil 陷阱（typed nil ≠ nil）、变量遮蔽、slice/map/defer/error/context 陷阱、数据竞争、JSON 反序列化意外、资源未关闭。每个都带复现模式与修法。

- **[测试驱动调试](./references/testing-debug.md)**——为什么写失败测试是调试的第一步。涵盖测试隔离技术、用表驱动测试缩小失败范围、有用的 `go test` flag（`-v`、`-run`、不稳定测试用 `-count=10`），以及不稳定测试的调试。

- **[并发调试](./references/concurrency-debug.md)**——数据竞争、死锁、goroutine 泄漏。何时用竞态检测器（`-race`）、怎么读竞态检测器输出、藏竞态的模式、用 `goleak` 检测泄漏、从堆栈 dump 找死锁线索。

- **[性能排障](./references/performance-debug.md)**——代码慢的时候：CPU profiling 工作流、内存分析（heap vs alloc_objects profiles、找泄漏）、锁竞争（mutex profile）、I/O 阻塞（goroutine profile）。怎么读火焰图、识别热点函数、用基准测试度量改进。

- **[pprof 参考](./references/pprof.md)**——完整 pprof 手册。怎么在生产启用 pprof 端点（带认证）、profile 类型（CPU、heap、goroutine、mutex、block、trace）、本地与远程采集、交互式分析命令（`top`、`list`、`web`）、火焰图解读。

- **[诊断工具](./references/diagnostic-tools.md)**——针对特定症状的辅助工具。GODEBUG 环境变量（GC 追踪、调度器追踪）、Delve 断点调试、逃逸分析（`go build -gcflags="-m"` 找非预期的堆分配）、Go 执行 tracer 理解 goroutine 调度。

- **[生产排障](./references/production-debug.md)**——不停机调试生产系统。生产排查清单、为可检索性组织日志、安全启用 pprof（认证、网络隔离）、从运行中的服务采集 profile、网络调试（tcpdump、netstat）、HTTP 请求/响应检查。

- **[编译问题](./references/compilation.md)**——构建失败：模块版本冲突、CGO 链接问题、`go.mod` 与已装 Go 版本不匹配、平台相关 build tag 阻碍交叉编译。

- **[代码评审危险信号](./references/code-review-flags.md)**——评审时要标记的模式：未检查的错误、缺 nil 检查、并发 map 访问、没有退出路径的 goroutine、循环里 defer 造成的资源泄漏。

## 交叉引用

- → 定位瓶颈后的优化模式见 `samber/cc-skills-golang@golang-performance` skill
- → Go runtime 监控的指标、告警与 Grafana 面板见 `samber/cc-skills-golang@golang-observability` skill
- → 生产事故调查中查询 Prometheus 指标见 `samber/cc-skills@promql-cli` skill
- → 另见 `samber/cc-skills-golang@golang-concurrency`、`samber/cc-skills-golang@golang-safety`、`samber/cc-skills-golang@golang-error-handling` skills
