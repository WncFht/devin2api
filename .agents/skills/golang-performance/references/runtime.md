# Runtime 调优

Runtime 设置控制 GC 频率、内存上限、CPU 调度与编译器优化。先 profiling 再调——默认值对多数负载已是良选。

## 目录

- [垃圾回收器调优](#垃圾回收器调优)
    - [GOGC（默认：100）](#gogc默认100)
    - [GOMEMLIMIT (Go 1.19+)](#gomemlimit-go-119)
    - [编程式控制](#编程式控制)
    - [Ballast 模式（Go 1.19 之前）](#ballast-模式go-119-之前)
- [GC 剖析与诊断](#gc-剖析与诊断)
    - [GODEBUG=gctrace=1](#godebuggctrace1)
    - [runtime.ReadMemStats](#runtimereadmemstats)
    - [GC 步调控制](#gc-步调控制)
- [降低分配速率](#降低分配速率)
- [容器中的 GOMAXPROCS](#容器中的-gomaxprocs)
- [性能剖析指导优化（PGO）](#性能剖析指导优化pgo)
- [热路径中的日志开销](#热路径中的日志开销)
- [Panic/Recover 开销](#panicrecover-开销)

## 垃圾回收器调优

**诊断：**1- `GODEBUG=gctrace=1`——每个 GC 周期打一行；找 GC 频率高（周期/秒）、CPU% 高（>5% 说明 GC 在抢 CPU）、或堆增长比预期快 2- `runtime.ReadMemStats`——查 `Alloc`、`TotalAlloc`、`NumGC`、`PauseNs`；对比 `Alloc` 与 `Sys` 看 GC 回收了多少、OS 实际给了多少 3- `go tool trace`——可视化 GC stop-the-world 停顿与 GC assist 从应用 goroutine 偷 CPU；找长 STW 条或频繁的 assist 标记 4- `debug.ReadGCStats`——取停顿时间分位数（p50、p95、p99）；p99 高说明堆扫描大或指针太多 5- `runtime/metrics`——程序化访问 GC 统计供仪表盘用；监控 `/gc/cycles/total`、`/gc/heap/allocs`、`/gc/pauses` 6- `GODEBUG=gcpacertrace=1`——跟踪 GC pacer 决策；用于理解 GC 为何比预期早或晚触发 7- Prometheus `rate(go_gc_duration_seconds_count[5m])`——生产中监控 GC 频率；持续 >2 次/秒提示分配速率过高

### GOGC（默认：100）

控制触发下一轮 GC 的堆增长比例。`GOGC=100` 表示堆自上次回收翻倍时跑 GC。值越大 GC 越少但内存越多：

```bash
GOGC=50  ./myapp  # 延迟敏感：GC 更频繁、停顿更短
GOGC=200 ./myapp  # 吞吐优先：GC 更少、内存更多
GOGC=off ./myapp  # 完全关 GC（仅测试用！）
```

### GOMEMLIMIT (Go 1.19+)

软内存上限——runtime 提高 GC 频率把内存压在此限制之下。容器化应用必备：超过容器限额会触发 OOM kill：

```bash
# 512MB 容器：给非堆内存（goroutine 栈、OS 缓冲）留余量
GOMEMLIMIT=450MiB ./myapp

# 1GB 容器
GOMEMLIMIT=900MiB ./myapp
```

GC pacer 同时依据 GOGC 与 GOMEMLIMIT 调整回收时机。堆逼近上限时，无论 GOGC 多少 GC 都会更激进。

### 编程式控制

```go
import "runtime/debug"

debug.SetGCPercent(200)                    // 等价于 GOGC=200
debug.SetMemoryLimit(450 * 1024 * 1024)   // 450 MiB 软上限
```

需要按观测到的负载动态调优，或环境变量设不了时，用编程式控制。

### Ballast 模式（Go 1.19 之前）

GOMEMLIMIT 出现前，团队启动时分配一个大字节数组吹大存活堆，降低 GC 频率：

```go
var ballast [1 << 30]byte // 1 GB——已过时的模式
```

**GOMEMLIMIT 严格更优**——同样的收益（GC 周期更少）且不浪费物理内存。用 GOMEMLIMIT。

## GC 剖析与诊断

### GODEBUG=gctrace=1

每个 GC 周期往 stderr 打一行：

```bash
GODEBUG=gctrace=1 ./myapp 2>&1 | head -20
```

示例输出：

```
gc 5 @1.234s 2%: 0.012+12+0.9 ms clock, 0.25+8.9/20+18 ms cpu, 45->92->50 MB, 200 MB goal, 8 P
```

关键字段：

- `gc 5`——第 5 个 GC 周期
- `@1.234s`——距程序启动的时间
- `2%`——GC 占总 CPU 时间比
- `45->92->50 MB`——回收前 → 回收中峰值 → 回收后的堆
- `200 MB goal`——目标堆大小（基于 GOGC 与 GOMEMLIMIT）
- `8 P`——处理器数

关注：GC 频率（太频繁 = 分配太多）、停顿时间（高 = 堆大或指针多）、CPU%（高 = 调 GOGC 或减少分配）。

### runtime.ReadMemStats

给仪表盘和告警用的程序化监控：

```go
var m runtime.MemStats
runtime.ReadMemStats(&m)

fmt.Printf("Alloc: %d MB\n", m.Alloc/1024/1024)       // 当前已分配
fmt.Printf("TotalAlloc: %d MB\n", m.TotalAlloc/1024/1024) // 累计
fmt.Printf("Sys: %d MB\n", m.Sys/1024/1024)            // 向 OS 申请
fmt.Printf("NumGC: %d\n", m.NumGC)                      // 已完成回收次数
fmt.Printf("LastPause: %d ms\n", m.PauseNs[(m.NumGC+255)%256]/1_000_000)
```

### GC 步调控制

GC pacer 依据以下信息预测何时开始下一轮回收：

1. 上次回收后的**存活堆大小**
2. **GOGC 百分比**——允许多少增长
3. **GOMEMLIMIT**——软上限（若设置）
4. **当前分配速率**——堆涨得多快

pacer 提前开始回收，保证在触及目标前完成。分配速率快则开始更早。

## 降低分配速率

**诊断：**1- `go tool pprof -alloc_objects`——按分配次数给函数排名；头部分配户就是减少分配对 GC 影响最大的地方 2- `GODEBUG=gctrace=1`——减少分配前后监控 GC 频率；分配速率降了，每秒 GC 周期应减少 3- Prometheus `rate(go_memstats_alloc_bytes_total[5m])`——跟踪生产分配速率趋势；部署前后对比抓回归

减少分配比调 GOGC 更有效——它治根因而非管症状：

- 尽量用**值类型而非指针类型**——值留在栈上（不经 GC），指针逃逸到堆
- 用 `sync.Pool` **池化频繁分配的对象**（见 [memory.md](./memory.md)）
- **预分配切片与 map**——→ 见 `samber/cc-skills-golang@golang-data-structures` skill
- 热路径中**避免接口装箱**——用类型化参数或泛型

## 容器中的 GOMAXPROCS

**诊断：**1- `go tool pprof`（CPU profile）——找 `runtime.schedule` 或 `runtime.findRunnable` 高开销；说明 P 太多互相争工作或 P 太少饿死 goroutine 2- `go tool trace`——检查 goroutine 是否均匀分布在各 P 上；分布不均提示 GOMAXPROCS 与容器不匹配 3- `GODEBUG=schedtrace=1000`——每秒打印调度器状态；有工作可做时找 `runqueue` 不均衡或空闲的 P 4- `runtime.GOMAXPROCS(0)`——查当前值；若返回宿主机 CPU 数（如 64）而非容器限额（如 2），runtime 在过度调度 5- Prometheus `rate(process_cpu_seconds_total[5m])`——监控生产中消耗的 CPU 核数；持续贴近 GOMAXPROCS 值说明应用 CPU 饱和

**Go 1.25+** 改进了容器 CPU 检测，特别是 cgroup v2。runtime 依据以下设置 `GOMAXPROCS`：

- 机器的逻辑 CPU 数
- 进程 CPU 亲和掩码
- cgroup CPU 配额（Linux 上）

64 核宿主机上跑 2 核容器、用 Go 1.25+ 且 **cgroup v2** 时，`GOMAXPROCS` 默认被正确设为 2。**cgroup v1** 环境下，启动时校验检测值，并考虑用 `go.uber.org/automaxprocs` 保证正确。

**Go 1.24 及更早**，用 `go.uber.org/automaxprocs` 库处理容器 CPU 检测：

```go
// Go 1.25 之前：显式的容器感知检测
import _ "go.uber.org/automaxprocs"

func main() {
    // GOMAXPROCS 现在被正确设为容器 CPU 限额
    startServer()
}
```

**手动覆盖**（如需要）：

```bash
GOMAXPROCS=2 ./myapp
GODEBUG=updatemaxprocs=0 ./myapp  # 禁用动态更新（Go 1.25+）
```

**已知限制（Go 1.25）**：某些系统上的 cgroup v1（Oracle OCPU）可能无法正确检测 Kubernetes CPU 限额。这些情况手动设 `GOMAXPROCS` 兜底。

## 性能剖析指导优化（PGO）

**诊断：**1- `go tool pprof`（CPU profile）——采一份有代表性的生产 profile（30 秒以上）；找 PGO 能经去虚拟化和内联优化的热接口方法调用与深调用链 2- `go test -bench`——放 `default.pgo` 前后各跑一次基准；接口密集的代码预期提升 2-7%，已优化路径更少

Go 1.21+ 支持 PGO——编译器用生产 CPU profile 做更好的内联与去虚拟化决策。预期提升：几乎不费力就有 2-7%。

**工作流：**

1. 采生产 CPU profile（30 秒以上代表性负载）：

    ```bash
    curl http://localhost:6060/debug/pprof/profile?seconds=60 > cpu.pprof
    ```

2. 放到 main 包目录下，命名 `default.pgo`：

    ```bash
    cp cpu.pprof ./cmd/myapp/default.pgo
    ```

3. 构建——`go build` 自动识别 `default.pgo`：

    ```bash
    go build ./cmd/myapp
    ```

**编译器优化什么：**

- **内联**——热函数调用被更激进地内联
- **去虚拟化**——大概率落到具体类型的接口方法调用变成直接调用

**最见效：**接口调用多、有热内联机会、调用栈深的代码。**最不见效：**已优化代码、内存受限的负载。

重大代码变更后重新采 profile——陈旧 profile 会误导编译器。

## 热路径中的日志开销

**诊断：**1- `go tool pprof`（CPU profile）——找热路径中出现的 `fmt.Sprintf`、`log.Printf` 或 `slog.(*Logger).log`；它们说明日志格式化在耗 CPU——即使级别过滤掉了消息 2- `go build -gcflags="-m"`——检查日志参数是否逃逸到堆；日志函数把参数装进 `any` 接口时预期看到 `"moved to heap"` 3- `go test -bench -benchmem`——开日志与关日志各跑一次基准；allocs/op 不变说明 logger 在级别关闭时也在分配

日志格式化在消息因低于配置级别被丢弃时，照样分配内存、消耗 CPU：

```go
// 差——logger 检查级别之前 fmt.Sprintf 先跑了
logger.Debug(fmt.Sprintf("processing item %d with data %v", item.ID, item.Data))

// 好——slog 把格式化推迟到级别检查之后（Go 1.21+）
slog.Debug("processing item", slog.Int("id", item.ID), slog.Any("data", item.Data))

// 最好——LogAttrs：级别关闭时零分配
slog.LogAttrs(ctx, slog.LevelDebug, "processing item",
    slog.Int("id", item.ID))
```

热路径中连 `slog.Any` 都可能分配。优先用类型化属性：`slog.Int`、`slog.String`、`slog.Bool`。

## Panic/Recover 开销

**诊断：**1- `go tool pprof`（CPU profile）——找 profile 里的 `runtime.gopanic` 或 `runtime.gorecover`；它们出现在热路径说明 panic/recover 被当控制流用了 2- `go test -bench`——对比 panic/recover 与 error 返回版本的基准；栈展开与 defer 执行预期有 10-100 倍开销

`panic` 触发栈展开，一路执行调用栈上所有 defer。`recover` 接住 panic，但展开本身昂贵。永远不要把 panic/recover 当控制流：

```go
// 差——为正常情况付出 panic 开销
defer func() { recover() }()
v, _ := strconv.Atoi(s) // 靠 panic 处理非法输入

// 好——显式错误检查，无 panic 开销
v, err := strconv.Atoi(s)
if err != nil { continue }
```

panic 只适用于真正不可恢复的情形（程序错误、状态损坏）。包边界处永远把 panic 转成 error。
