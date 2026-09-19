# 执行 Trace 参考

`go tool trace` 显示 pprof 看不到的东西：**调度延迟**、GC stop-the-world 阶段、goroutine 状态转换，以及 goroutine 为什么**没**在跑。pprof 采样谁在 CPU 上；trace 以纳秒精度记录每一次状态转换。

什么时候用执行追踪器：

- pprof 显示 CPU% 低但延迟高（goroutine 在等，没在干活）
- 怀疑 GC 暂停造成尾延迟尖刺
- 需要理解 goroutine 调度与争用
- 想看并发操作的墙钟时间线

## 目录

- [生成 trace](#生成-trace)
    - [从基准测试](#从基准测试)
    - [从运行中服务](#从运行中服务)
    - [从测试](#从测试)
    - [从代码（编程方式）](#从代码编程方式)
- [完整命令参考](#完整命令参考)
    - [打开 trace](#打开-trace)
    - [从 trace 提取 pprof profile](#从-trace-提取-pprof-profile)
    - [完整采集到分析工作流](#完整采集到分析工作流)
    - [`go tool trace` flag 汇总](#go-tool-trace-flag-汇总)
    - [web UI 提供的 HTTP 端点](#web-ui-提供的-http-端点)
- [Web UI](#web-ui)
    - [主视图](#主视图)
    - [操作 trace 查看器](#操作-trace-查看器)
    - [读时间线](#读时间线)
- [看什么](#看什么)
    - [Goroutine 状态](#goroutine-状态)
    - [GC 阶段](#gc-阶段)
    - [调度延迟](#调度延迟)
    - [网络/同步阻塞](#网络同步阻塞)
    - [Goroutine 创建与销毁](#goroutine-创建与销毁)
- [自定义标注](#自定义标注)
    - [任务](#任务)
    - [区域](#区域)
    - [日志消息](#日志消息)
    - [何时用标注](#何时用标注)
- [Flight Recorder (Go 1.25+)](#flight-recorder-go-125)
    - [配置](#配置)
    - [出错时快照](#出错时快照)
    - [触发模式](#触发模式)
    - [分析快照](#分析快照)
    - [限制](#限制)
    - [flight recorder 与常规 trace 怎么选](#flight-recorder-与常规-trace-怎么选)
- [开销与实际限制](#开销与实际限制)
- [trace 与 pprof 怎么选](#trace-与-pprof-怎么选)

## 生成 trace

### 从基准测试

```bash
go test -bench=BenchmarkParse -trace=trace.out ./pkg/parser
go tool trace trace.out
```

### 从运行中服务

需要 `import _ "net/http/pprof"`：

```bash
# 采集 5 秒 trace 数据（按需调时长）
curl -o trace.out http://localhost:6060/debug/pprof/trace?seconds=5
go tool trace trace.out
```

**警告：** trace 以 MB/s 的速度产生数据。采集要短——典型是 5-10 秒。更长的 trace 难处理、解析慢，打开时还可能吃很多内存。

### 从测试

```bash
go test -trace=trace.out ./pkg/parser
go tool trace trace.out
```

### 从代码（编程方式）

```go
import "runtime/trace"

f, _ := os.Create("trace.out")
trace.Start(f)
defer trace.Stop()
```

或只采集感兴趣的一段：

```go
import "runtime/trace"

// 只在需要时开始 trace
f, _ := os.Create("trace.out")
trace.Start(f)

doExpensiveWork()

trace.Stop()
f.Close()
```

## 完整命令参考

### 打开 trace

```bash
# 在浏览器打开 trace（默认——起 HTTP 服务并开浏览器）
go tool trace trace.out

# 指定端口打开
go tool trace -http=:8080 trace.out

# 指定 host:port 打开（比如要远程访问）
go tool trace -http=0.0.0.0:8080 trace.out
```

### 从 trace 提取 pprof profile

`go tool trace` 能把 trace 数据转成 pprof 兼容的 profile。这把两个工具接起来——你用追踪器采集（纳秒级事件），用 pprof 分析（`top`、`list`、`peek` 的统计聚合）：

```bash
# 网络阻塞 profile——goroutine 在哪里等网络 I/O
go tool trace -pprof=net trace.out > net.prof
go tool pprof -top net.prof

# 同步阻塞 profile——mutex、channel、wait group
go tool trace -pprof=sync trace.out > sync.prof
go tool pprof -top sync.prof

# syscall 阻塞 profile——阻塞 goroutine 的系统调用
go tool trace -pprof=syscall trace.out > syscall.prof
go tool pprof -top syscall.prof

# 调度延迟 profile——从可运行到真正运行之间的时间
go tool trace -pprof=sched trace.out > sched.prof
go tool pprof -top sched.prof
```

可以接任意 pprof 命令——例如某个阻塞函数的标注源码：

```bash
go tool trace -pprof=sync trace.out > sync.prof
go tool pprof -list=handleRequest sync.prof
go tool pprof -svg sync.prof > sync-blocking.svg
```

### 完整采集到分析工作流

```bash
# 工作流 1：基准测试 trace——采集、查看、提取阻塞 profile
go test -bench=BenchmarkParse -trace=trace.out ./pkg/parser
go tool trace trace.out                                         # 可视化时间线
go tool trace -pprof=sync trace.out > sync.prof                 # 提取同步阻塞
go tool pprof -top -cum sync.prof                               # 找最严重的同步阻塞者
go tool pprof -list=processOrder sync.prof                      # 标注源码

# 工作流 2：生产 trace——从运行中服务采集，分析调度
curl -o trace.out http://localhost:6060/debug/pprof/trace?seconds=5
go tool trace trace.out                                         # 可视化时间线
go tool trace -pprof=sched trace.out > sched.prof               # 提取调度延迟
go tool pprof -top sched.prof                                   # 调度延迟最严重的 goroutine
go tool pprof -svg sched.prof > sched.svg                       # 调度瓶颈图

# 工作流 3：测试 trace——测试运行期间采集
go test -trace=trace.out -run=TestSlowIntegration ./pkg/api
go tool trace trace.out                                         # 可视化时间线
go tool trace -pprof=net trace.out > net.prof                   # 提取网络阻塞
go tool pprof -top net.prof                                     # 找网络等待点
```

### `go tool trace` flag 汇总

| Flag          | 示例                                            | 用途                                                                 |
| ------------- | ----------------------------------------------- | -------------------------------------------------------------------- |
| （无）        | `go tool trace trace.out`                       | 在浏览器打开 trace（默认）                                           |
| `-http=:PORT` | `go tool trace -http=:9090 trace.out`           | 设置 web UI 的 HTTP 服务地址                                         |
| `-pprof=TYPE` | `go tool trace -pprof=net trace.out > net.prof` | 从 trace 提取 pprof profile。类型：`net`、`sync`、`syscall`、`sched` |

### web UI 提供的 HTTP 端点

`go tool trace trace.out` 启动 HTTP 服务后暴露这些页面：

| 端点              | 显示什么                                                      |
| ----------------- | ------------------------------------------------------------- |
| `/`               | 索引页，链到全部视图                                          |
| `/trace`          | 交互式时间线查看器（Chrome trace viewer）——主可视化           |
| `/goroutines`     | Goroutine 分析——全部 goroutine 类型的汇总表，含数量与执行统计 |
| `/goroutine/<id>` | 单个 goroutine 的详细视图——完整生命周期时间线                 |

在 `/goroutines` 里点击某个 goroutine 类型看全部实例及其执行统计（总时间、被调度时间、阻塞时间）。点击单个 goroutine 看它的时间线。

## Web UI

### 主视图

web UI（`go tool trace trace.out` 打开）显示一条时间线，每条水平泳道代表一个处理器（P）、一个 goroutine 或一类系统事件：

- **Trace 查看器**（`/trace`）——交互式时间线，含：
    - **P 泳道**——每个逻辑处理器（GOMAXPROCS）一条，显示每个时刻哪个 goroutine 在哪个 P 上跑
    - **Goroutine 泳道**——每个 goroutine 的生命周期：创建 → 可运行 → 运行 → 等待 → 运行 → …
    - **GC 事件**——mark 阶段、sweep、STW 暂停以横跨全部 P 泳道的色带显示
    - **系统事件**——syscall、网络 I/O、定时器事件
    - **用户标注**——来自 `runtime/trace` API 的任务、区域与日志消息

- **Goroutine 分析**（`/goroutines`）——汇总表：
    - 按创建栈踪迹（类型）给 goroutine 分组
    - 显示数量、总执行时间、总调度等待、总阻塞时间
    - 点类型看单个 goroutine 统计
    - 点单个 goroutine 看它的时间线

### 操作 trace 查看器

trace 查看器用的是 Chrome tracing UI（Chrome DevTools 同款）：

| 键/操作       | 效果                                         |
| ------------- | -------------------------------------------- |
| `W` / 上滚    | 放大（时间轴）                               |
| `S` / 下滚    | 缩小（时间轴）                               |
| `A`           | 左移                                         |
| `D`           | 右移                                         |
| 点击事件      | 底部显示详情面板——goroutine ID、时长、栈踪迹 |
| `Shift+click` | 选中时间区间——高亮窗口内全部事件             |
| `M`           | 标记当前选中                                 |
| `/`           | 按名字搜索事件                               |
| `?`           | 显示键盘快捷键                               |

### 读时间线

**颜色编码：**

- P 泳道上的**绿色块** = goroutine 正在执行
- **蓝色块** = syscall（goroutine 钉在 OS 线程上）
- **橙/黄色标记** = 调度事件（goroutine 变为可运行）
- 横跨全部 P 泳道的**红色带** = GC stop-the-world 暂停
- **浅蓝带** = GC 并发 mark 阶段
- **紫色** = 用户自定义区域（来自 `trace.WithRegion`）

**P 泳道上的空隙** = 该处理器空闲（没有可运行 goroutine，或 goroutine 都阻塞了）。空隙多同时还有可运行 goroutine 积压，说明存在调度争用。

## 看什么

### Goroutine 状态

trace 时间线用颜色标出 goroutine 状态：

| 颜色      | 状态      | 含义                                         | 说明什么                                        |
| --------- | --------- | -------------------------------------------- | ----------------------------------------------- |
| **绿**    | Running   | 正在 P 上执行                                | 正常——在做有用的功                              |
| **黄/橙** | Runnable  | 就绪但在等 P                                 | CPU 饱和——可运行 goroutine 太多，抢太少的处理器 |
| **红/粉** | Waiting   | 阻塞在 I/O、channel、mutex、sleep、select 上 | I/O 密集或争用——查它在等什么                    |
| **蓝**    | GC assist | 被 GC 征用去帮忙 mark/sweep                  | GC 压力——分配太多迫使 goroutine 帮回收器干活    |

### GC 阶段

GC 事件以横跨全部 P 泳道的色带显示：

- **Mark assist**——goroutine 被征用帮 GC 扫堆。表现为应用 goroutine 执行中的空隙。runtime 按 goroutine 的分配速率比例强制它们协助 GC——分配大户被征税更多。
- **STW（stop-the-world）**——所有 goroutine 停下的短暂阶段（mark setup、mark termination）。它们造成延迟尖刺，在时间线上显示为横跨全部泳道的竖带。
- **Sweep**——并发清扫不可达对象。通常开销低，但堆大时会累积。

**从 trace 诊断 GC 问题：**

- GC 周期频繁且 mark assist 长 = 分配太多（降分配速率）
- STW 阶段长 = 要扫的指针太多（降指针密度）
- GC 周期聚集在特定操作之后 = 那些操作分配量大

### 调度延迟

goroutine 从**可运行**到真正**运行**之间的时间。调度延迟高意味着：

- 太多 goroutine 抢 GOMAXPROCS 个处理器
- OS 调度干扰（noisy neighbor、CPU 节流）
- goroutine 被 cgo 或长 syscall 钉在忙碌线程上

**看什么：**

- 绿色（运行）段之前的黄色（可运行）空隙——黄隙越长，调度延迟越高
- 大量 goroutine 同时处于可运行态——说明 CPU 饱和
- P 之间分布不均——一个 P 过载其他空闲，说明工作不均衡

### 网络/同步阻塞

- goroutine 上**长段红/粉** = 它在阻塞等待。点阻塞事件看它在等什么（channel 接收、mutex 加锁、网络读等）
- **大量 goroutine 阻塞在同一 channel 或 mutex** = 串行化瓶颈。所有工作汇过一个点。
- **goroutine 阻塞在网络 I/O** = 外部依赖延迟。Go 代码没法更快——瓶颈在上游。用 `-pprof=net` 生成网络等待点的 pprof profile。

### Goroutine 创建与销毁

trace 显示 goroutine 生命周期事件。找：

- **无界循环里创建 goroutine** = 潜在 goroutine 泄漏
- **创建了但永不结束的 goroutine** = 泄漏——随时间累积
- **反复创建的超短命 goroutine** = goroutine 创建/调度开销高（考虑批处理或 worker pool）

## 自定义标注

给 trace 加应用层上下文，把 runtime 事件与业务操作关联起来。

### 任务

任务代表一个可能跨多个 goroutine 的逻辑操作：

```go
import "runtime/trace"

func processOrder(ctx context.Context, order Order) error {
    ctx, task := trace.NewTask(ctx, "processOrder")
    defer task.End()

    // 这个 ctx 下的全部 trace 事件都归到该任务
    validate(ctx, order)
    charge(ctx, order)
    fulfill(ctx, order)
    return nil
}
```

任务在 trace 时间线上显示为具名分组。可以过滤 trace 视图只显示属于某个任务的事件。

### 区域

区域代表任务或 goroutine 内的一个阶段：

```go
func validate(ctx context.Context, order Order) {
    trace.WithRegion(ctx, "validateAddress", func() {
        // 这一块被标注为区域
        validateAddress(order.Address)
    })

    trace.WithRegion(ctx, "validatePayment", func() {
        validatePayment(order.Payment)
    })
}
```

区域在 goroutine 时间线上显示为带标签的 span，一眼看出处理的哪个阶段占了最多墙钟时间。

### 日志消息

给 trace 加点状日志消息：

```go
trace.Log(ctx, "orderID", order.ID)
trace.Log(ctx, "status", "payment_verified")
```

日志在时间线上显示为标记——把 trace 事件与具体数据关联起来时有用。

### 何时用标注

- **始终**在 server 请求处理器里用——每个请求包一个任务
- **性能关键路径**——给想测墙钟时间的阶段加区域
- **排查间歇性延迟**——在关键决策点加日志，看慢的那次 trace 里发生了什么

trace 未开启时标注开销可忽略（查个标志位就返回）。

## Flight Recorder (Go 1.25+)

flight recorder 解决了长运行服务中执行 trace 的根本难题：问题发生时（超时、健康检查失败），再调 `trace.Start()` 已经晚了。flight recorder 在内存里维护一个最近 trace 数据的环形缓冲，出事时把它快照到磁盘——就像飞机的黑匣子。

### 配置

```go
import "runtime/trace"

fr := trace.NewFlightRecorder(trace.FlightRecorderConfig{
    MinAge:   10 * time.Second, // 至少保留 10s 数据
    MaxBytes: 5 << 20,          // 上限 5 MiB，控制内存占用
})
if err := fr.Start(); err != nil {
    return err
}
```

**尺寸建议：**

- **MinAge**——设为问题窗口的约 2 倍。排查 5 秒超时就用 10 秒。MaxBytes 允许时 runtime 保留的数据可能多于 MinAge。
- **MaxBytes**——繁忙服务每秒产生约 1-10 MB trace 数据。从 1-5 MiB 起步再调。MaxBytes 优先于 MinAge——缓冲一满，旧数据不论年龄都被丢弃。

### 出错时快照

发生意外时把 trace 缓冲抓下来。用 `sync.Once` 防止多个快照互相覆盖：

```go
var snapshotOnce sync.Once

func captureSnapshot(fr *trace.FlightRecorder) {
    snapshotOnce.Do(func() {
        f, err := os.Create("snapshot.trace")
        if err != nil {
            log.Printf("snapshot file: %v", err)
            return
        }
        defer f.Close()

        if _, err := fr.WriteTo(f); err != nil {
            log.Printf("snapshot write: %v", err)
            return
        }
        fr.Stop()
        log.Printf("captured snapshot to %s", f.Name())
    })
}
```

### 触发模式

```go
// 模式 1：慢请求检测
http.HandleFunc("/api/order", func(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    // ... 处理器逻辑 ...

    if fr.Enabled() && time.Since(start) > 100*time.Millisecond {
        go captureSnapshot(fr)
    }
})

// 模式 2：健康检查失败
if !healthCheck() && fr.Enabled() {
    go captureSnapshot(fr)
}

// 模式 3：HTTP 端点按需抓取
http.HandleFunc("/debug/flightrecorder", func(w http.ResponseWriter, r *http.Request) {
    if !fr.Enabled() {
        http.Error(w, "flight recorder not active", http.StatusServiceUnavailable)
        return
    }
    w.Header().Set("Content-Type", "application/octet-stream")
    w.Header().Set("Content-Disposition", "attachment; filename=trace.out")
    fr.WriteTo(w)
})
```

### 分析快照

```bash
go tool trace snapshot.trace
```

快照与常规 trace 数据相同——所有分析技术都适用（时间线查看器、goroutine 分析、pprof 提取）。flight recorder 的 flow 事件对诊断造成异常的锁争用与 goroutine 停滞尤其有用。

### 限制

- **最多一个 flight recorder** 同时激活（未来 Go 版本可能放宽）
- flight recorder **可以与 `trace.Start` 并发运行**——两者可同时激活
- 同一时刻只有一个 goroutine 能调 `WriteTo`——`sync.Once` 模式天然处理这点
- `Stop()` 会阻塞到并发的 `WriteTo` 完成

### flight recorder 与常规 trace 怎么选

| 场景                   | 工具                                                        | 为什么                                     |
| ---------------------- | ----------------------------------------------------------- | ------------------------------------------ |
| 排查已知的慢操作       | `go test -trace` 或 `trace.Start`/`Stop`                    | 你知道何时开始何时结束                     |
| 生产中的间歇延迟尖刺   | Flight recorder                                             | 不知道尖刺何时来——缓冲区事后回溯捕捉       |
| 超时或崩溃后的事后分析 | Flight recorder                                             | 问题已经发生；常规 trace 会错过            |
| 持续性能监控           | `samber/cc-skills-golang@golang-observability`（Pyroscope） | Flight recorder 是一次性诊断，不是持续采集 |

## 开销与实际限制

| 顾虑             | 指导                                                               |
| ---------------- | ------------------------------------------------------------------ |
| **运行时开销**   | 采集期间约 1-2% CPU；不采集时忽略不计                              |
| **数据量**       | trace 以 MB/s 产生数据。繁忙服务 10 秒 trace 可达 50-100MB         |
| **采集时长**     | 典型 5-10 秒。更长的 trace 打开慢、难浏览                          |
| **查看所需内存** | `go tool trace` 把整个 trace 读进内存。大 trace 可能需要 1GB+ 内存 |
| **浏览器性能**   | web UI 对 >100MB 的 trace 会吃力。采集保持短                       |
| **生产使用**     | 单实例短时采集是安全的。不要持续采集                               |

## trace 与 pprof 怎么选

| 问题                           | 工具                      | 为什么                                     |
| ------------------------------ | ------------------------- | ------------------------------------------ |
| CPU 时间花在哪？               | pprof CPU profile         | 统计采样、低开销、适合聚合视图             |
| 为什么延迟高但 CPU 低？        | go tool trace             | 显示 goroutine 等待态——I/O、channel、mutex |
| 分配发生在哪？                 | pprof 堆 profile          | 按函数的分配次数与大小                     |
| 为什么 GC 暂停长？             | go tool trace             | 显示 STW 阶段、mark assist、GC 时间线      |
| 有锁争用吗？                   | pprof mutex/block + trace | pprof 量化；trace 显示时间线               |
| goroutine 在泄漏吗？           | pprof goroutine + trace   | pprof 显示栈；trace 显示创建/生命周期      |
| 哪些 goroutine 在抢 CPU？      | go tool trace             | 显示全部 P 上的可运行 vs 运行状态          |
| 一个请求的墙钟时间怎么分布的？ | go tool trace（配标注）   | 带任务与区域的时间线视图                   |

拿不准时先用 pprof（开销更低、输出更简单）。pprof 解释不了延迟、或需要墙钟时间线视图时再用 trace。
