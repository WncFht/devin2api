# CPU 优化

CPU 受限的瓶颈表现为某些函数主导 CPU profile。下面的模式针对最常见的成因：错失的内联机会、糟糕的缓存利用、不必要的计算。

## 目录

- [函数内联](#函数内联)
    - [值接收者利于内联](#值接收者利于内联)
- [缓存局部性](#缓存局部性)
    - [行主序遍历](#行主序遍历)
    - [连续二维分配](#连续二维分配)
    - [数组结构体（SoA）与结构体数组（AoS）](#数组结构体soa与结构体数组aos)
    - [指针密集与值密集数据](#指针密集与值密集数据)
- [伪共享](#伪共享)
- [指令级并行](#指令级并行)
- [SIMD（单指令多数据）](#simd单指令多数据)
    - [处理 CPU 专属指令集](#处理-cpu-专属指令集)
- [紧凑循环与调度器](#紧凑循环与调度器)
- [反射与类型断言](#反射与类型断言)
- [单调时钟](#单调时钟)

## 函数内联

**诊断：**1- `go tool pprof`（CPU profile）——找累计 CPU 时间高的热函数；若一个小 helper 主导 profile，多半是没被内联 2- `go build -gcflags="-m"`——对热路径函数 grep `"cannot inline"`；原因（如 `"function too complex"`、`"unhandled op"`）告诉你该简化什么

Go 编译器内联小函数，消除调用开销。太复杂的函数（含循环、语句太多、或调用了不可内联的函数）不会被内联——在被调用数百万次的紧凑循环里这很关键。

```go
// 差——日志调用阻碍内联
func abs(x int) int {
    if x < 0 {
        log.Printf("negative: %d", x) // 阻碍内联
        return -x
    }
    return x
}

// 好——简单到足以内联
func abs(x int) int {
    if x < 0 { return -x }
    return x
}
```

**检查内联决策：**

```bash
go build -gcflags="-m" ./... 2>&1 | grep "can inline"
go build -gcflags="-m" ./... 2>&1 | grep "inlining call"
```

把副作用（日志、指标）挪到热路径函数之外，或用条件判断守住。

### 值接收者利于内联

值接收者让编译器能完整内联流式方法链。指针接收者多一层间接，阻碍内联：

```go
// 指针接收者——间接层阻碍内联，每次调用固定开销
func (c *config) WithTimeout(d time.Duration) *config { c.timeout = d; return c }

// 值接收者——完全内联，流式链中时间 -80%
func (c config) WithTimeout(d time.Duration) config { c.timeout = d; return c }
```

## 缓存局部性

**诊断：**1- `go tool pprof`（CPU profile）——找消耗 CPU 不成比例的切片/矩阵遍历循环；缓存 miss 密集的代码表现为 `runtime.memmove` 高或简单索引操作占大量 flat 时间 2- `go test -bench`——对比行优先与列优先遍历的基准；大矩阵上纯缓存效应就能差 10-50 倍

现代 CPU 按 64 字节缓存行取数据。顺序内存访问远快于随机访问，因为预取器能在你需要之前把下一行装好。

### 行主序遍历

Go 按行主序存二维数组。列优先遍历在内存里跳来跳去，造成缓存 miss：

```go
// 差——列优先，跨内存跳跃（约 10M 次缓存 miss）
for col := 0; col < 1024; col++ {
    for row := 0; row < 1024; row++ {
        sum += matrix[row][col]
    }
}

// 好——行优先，顺序访问（约 125K 次缓存 miss）
for row := 0; row < 1024; row++ {
    for col := 0; col < 1024; col++ {
        sum += matrix[row][col]
    }
}
```

性能差距：纯缓存效应 10-50 倍。

### 连续二维分配

每行单独分配会把数据打散到堆上各处：

```go
// 差——N 次独立分配，缓存局部性差
matrix := make([][]float64, rows)
for i := range matrix { matrix[i] = make([]float64, cols) }

// 好——单次连续分配，缓存友好
data := make([]float64, rows*cols)
matrix := make([][]float64, rows)
for i := range matrix { matrix[i] = data[i*cols : (i+1)*cols] }
```

### 数组结构体（SoA）与结构体数组（AoS）

只遍历结构体单个字段时，AoS 把用不到的字段也装进缓存，浪费缓存空间：

```go
// AoS——为读 x（8 字节）装入整个 Point（24 字节）= 66% 缓存浪费
type Point struct { x, y, z float64 }
points := make([]Point, n)
for i := range points { sum += points[i].x }

// SoA——所有 x 值连续，缓存利用率 100%
type Points struct { xs, ys, zs []float64 }
for i := range ps.xs { sum += ps.xs[i] }
```

只遍历字段子集时用 SoA（物理、图形、分析场景）。所有字段一起访问或结构体很小时，AoS 没问题。

### 指针密集与值密集数据

基于索引的数据结构（节点存在连续数组里、用下标引用）比基于指针的结构缓存局部性更好：

```go
// 指针树——每个节点散落在堆上，随机缓存 miss
type Node struct { value int; left, right *Node }

// 索引树——节点在连续数组里，缓存友好
type Tree struct { nodes []Node }
type Node struct { value int; left, right int } // nodes 的下标
```

## 伪共享

**诊断：**1- `go tool pprof`（CPU profile + mutex profile）——找原子操作或计数器更新消耗异常高的 CPU；mutex profile 里找本不该需要锁的变量上的竞争 2- `go test -bench`——对并发计数器自增做基准；加 goroutine 反而更慢，多半就是伪共享

多个 goroutine 更新落在同一条 64 字节 CPU 缓存行上的变量时，每次写都让另一核的缓存失效，性能严重劣化：

```go
// 差——a 和 b 在同一缓存行，两核互相争抢
type Counters struct { a, b int64 }

// 好——各自独占缓存行，互不干扰
type Counters struct {
    a int64    // 8 字节
    _ [56]byte // 64 - 8 = 56 字节填充
    b int64    // 8 字节
}
```

只有 profiling 确认并发计数器/标志位上有竞争时才加缓存行填充。

## 指令级并行

**诊断：**1- `go tool pprof`（CPU profile）——找循环体本身主导 CPU 的紧凑算术循环（求和、点积）；它们是多累加器优化的候选 2- `go test -bench`——对比单累加器与多累加器版本的基准；循环真正 CPU 受限且存在依赖链时，预期 2-4 倍提升

现代 CPU 同时执行多条相互独立的指令。单累加器构成依赖链——每次加法都要等上一次：

```go
// 差——顺序依赖，CPU 流水线停顿
var total int64
for _, v := range data { total += v }

// 好——4 个独立累加器，CPU 并行流水 4 路
var s0, s1, s2, s3 int64
limit := len(data) - len(data)%4
for i := 0; i < limit; i += 4 {
    s0 += data[i]; s1 += data[i+1]; s2 += data[i+2]; s3 += data[i+3]
}
for i := limit; i < len(data); i++ { s0 += data[i] }
total := s0 + s1 + s2 + s3
```

紧凑算术循环预期 2-4 倍提升。只在 profiling 显示该循环是瓶颈时用。

## SIMD（单指令多数据）

**诊断：**1- `go tool pprof`（CPU profile）——确认数值内层循环消耗 >20% CPU；SIMD 只对 CPU 受限的数值工作有帮助，对分配或 I/O 瓶颈无效 2- `go test -bench`——测循环基线 ns/op；为验证 SIMD 收益提供参照点 3- `go build -gcflags="-d=ssa/prove/debug=2"`——检查编译器是否已自动向量化该循环；找使向量化成为可能的 `"Proved"` 边界检查消除 4- `GOSSAFUNC=MyFunc go build`——生成 SSA dump（`ssa.html`），检查编译器是否为热循环生成向量指令 5- `go tool objdump -s MyFunc ./binary`——验证最终汇编含 SIMD 指令（如 amd64 上的 `VMOVAPD`、`VADDPD`）而非标量等价物

Go 1.26+ 含实验性 `simd/archsimd` 包（需 `GOEXPERIMENT=simd`），提供底层、架构专属的 SIMD intrinsics——amd64 支持 128/256/512 位向量，Go 1.27 起 arm64/wasm 支持 128 位。Go 1.27 另加可移植 `simd` 包（`Int8s`、`Float32s` 等向量类型），跨架构工作、无硬件支持时回退标量代码。更广的可移植性方面，编译器会自动向量化简单循环，另有几种策略。

**Go 显式 SIMD 的选项：**

- **实验性 `simd`/`simd/archsimd`（Go 1.26+，未稳定）**——通过向量类型加 CPU 特性检测直接调 SIMD intrinsics。`simd`（Go 1.27+）跨架构可移植；`simd/archsimd` 架构专属（amd64，Go 1.27 起另含 arm64/wasm）。谨慎使用：两者都是实验性、开发中的 API（`GOEXPERIMENT=simd`），包路径和类型名在稳定前可能变动。不受 Go 1 兼容性承诺保护，绝不该暴露在公开 API 里。实际 import 路径和 API 以你用的 Go 工具链为准。

    ```go
    // 需要：GOEXPERIMENT=simd go build
    // 警告：实验性 API——包路径与类型可能变动
    import "simd/archsimd"

    v := archsimd.Int32x4{1, 2, 3, 4}
    ```

- **让编译器代劳**——在 `[]float64`/`[]int32` 切片上写简单、地道的循环。检查自动向量化：`go build -gcflags="-d=ssa/prove/debug=2" ./...`
- **`math/bits`**——`OnesCount`、`LeadingZeros`、`RotateLeft` 等操作直接映射到硬件指令（POPCNT、CLZ、ROL）
- **手写汇编**——关键内层循环用 `.s` 文件写 AVX2/NEON 指令。`klauspost/compress`、`minio/sha256-simd` 等库走这条路
- **第三方向量化库**——常见操作（哈希、压缩、编码）用已有优化 SIMD 实现的库，不要自己写

### 处理 CPU 专属指令集

手写汇编解锁更高性能，但把代码绑死在特定 CPU 特性（AVX2、NEON 等）上。有三种策略：

**1. 在与生产相似的机器上编译**

在与部署目标匹配的硬件上构建二进制，让编译器针对运行时实际可用的 CPU 指令集生成代码：

```bash
# 在生产硬件上编译，保证为该 CPU 架构与代次
# 生成最优代码
ssh prod-server "cd /path && go build -o app ."
```

**取舍：**最简单的方案，但需要能访问生产硬件，且每种 CPU 类型（Intel vs AMD vs Apple Silicon）要出不同二进制。破坏 CI/CD 可移植性。

**2. 运行时 CPU 特性检测 + 多份实现**

把函数实现多份——每种 CPU 能力一份——运行时分发：

```go
// dispatch.go
var sumImpl func([]int64) int64

func init() {
    if cpu.X86.HasAVX2 {
        sumImpl = sumAVX2
    } else {
        sumImpl = sumGeneric
    }
}

func Sum(data []int64) int64 {
    return sumImpl(data)
}

// sum_generic.go
func sumGeneric(data []int64) int64 {
    var total int64
    for _, v := range data { total += v }
    return total
}

// sum_amd64.s
TEXT ·sumAVX2(SB), NOSPLIT, $0-32
    // AVX2 实现
    VMOVAPD (SI), Y0
    // ...
```

**取舍：**单个二进制处处可跑；用一次函数调用分发开销换完整的 CPU 特性利用。`encoding/base64`、`sha256` 等库用这个模式。

**3. `//go:build` tag 编译期选择**

用条件编译在构建时为每个目标生成不同代码：

```go
// sum_fast.go
//go:build amd64 && !nosimd

package mylib

// 经 cgo 或内联使用 AVX2 汇编
func Sum(data []int64) int64 {
    return sumAVX2(data) // 或调用 .s 文件
}

// sum_generic.go
//go:build !amd64 || nosimd

package mylib

func Sum(data []int64) int64 {
    var total int64
    for _, v := range data { total += v }
    return total
}
```

每个目标构建不同二进制：

```bash
GOOS=linux GOARCH=amd64 go build -o app-avx2 .     # 用 sum_fast.go
GOOS=darwin GOARCH=arm64 go build -o app-neon .    # 用 sum_generic.go
go build -tags=nosimd -o app-safe .                # 处处可用的兜底
```

**取舍：**零运行时开销；每个二进制都为自己的目标完全优化。代价是要发布多个二进制并协调哪个二进制跑在哪。

**什么时候不值得上 SIMD：**

- Go 没有 intrinsics，SIMD 意味着写汇编——维护负担高、平台专属、更难调试
- 自动向量化已覆盖最常见情形（简单数值循环）
- 瓶颈若在分配或 I/O，SIMD 帮不上忙

**建议：**

- 从自动向量化开始。
- Go 1.27+ 评估跨架构代码用可移植 `simd` 包、架构专属调优用 `simd/archsimd`（amd64、arm64、wasm）——记住两者都是实验性的。
- profiling 显示瓶颈且代码要跑在异构硬件上时，转到运行时检测（上文方案 2）。
- 只有你能控制部署环境并能逐个测试每个二进制变体时，才用编译期选择（方案 3）。

只有 profiling 显示数值内层循环消耗 >20% CPU 且编译器没有自动向量化时，才值得投入手写 SIMD。

## 紧凑循环与调度器

**诊断：**1- `go tool pprof`（goroutine profile）——找大量停在 `"runnable"` 状态（等 CPU）的 goroutine，而某个 goroutine 独占执行 2- `go tool trace`——随时间可视化 goroutine 调度；找一个 goroutine 长时间不间断执行、其他 goroutine 出现调度空隙 3- `GODEBUG=schedtrace=1000`——每秒打印调度器状态；找各 P 的 `runqueue` 不均衡，说明某个 P 被饿死 4- `runtime/metrics`（`/sched/latencies:seconds`）——测量 goroutine 拿到 CPU 前要等多久；p99 延迟高确认饥饿 5- Prometheus `rate(process_cpu_seconds_total[2m])`——监控 CPU 用量是否顶到 GOMAXPROCS 上限；饱和而其他 goroutine 挨饿时，说明紧凑循环霸占了 P

跑 CPU 密集紧凑循环且没有函数调用的 goroutine 可能不让出调度器，饿死其他 goroutine。Go 1.14+ 有异步抢占，但完全内联操作的极紧凑循环仍可能出问题：

```go
// 可能饿死别人——纯计算，无函数调用
for { x = x*a + b }

// 安全——不可内联的调用触发抢占检查
for item := range work {
    processBatch(item) // 函数调用 = 抢占点
}
```

**何时用不可内联调用帮助调度：**满足以下条件时用不可内联函数调用：

- 循环运行时间长（数百毫秒以上的不间断计算）
- 有其他 goroutine 在等运行（如处理请求、I/O 完成、channel 操作）
- 循环里只有算术或内存操作，没有函数调用

短促计算（< 10ms）抢占不关键，为 CPU 效率内联优先。

**检测调度器饥饿：**用这些工具确认 goroutine 被饿死：

- **`go tool pprof` goroutine profile**——显示停在 "runnable" 状态（等 CPU）的 goroutine。大量 goroutine runnable 而某一个独占 CPU，就是饥饿
- **`go tool trace`**——随时间可视化 goroutine 调度。找 goroutine 因某个 goroutine 霸占调度器而不运行的空隙
- **`runtime/metrics`（Go 1.19+）**——测 `/sched/latencies:seconds`，量化 goroutine 等 CPU 的时长
- **可观测症状**——响应延迟高、请求超时、请求分布不均、goroutine 数攀升

**用 `//go:noinline` 阻止内联：**若某函数本来可内联（小、热）但你特意不想让它内联、以强制调度器抢占检查，用 `//go:noinline` 编译指令：

```go
//go:noinline
func processBatch(item WorkItem) {
    // CPU 密集工作在这里
    // 这个调用点不会被内联，即使函数很小
    // 函数调用本身成为调度器的抢占点
}

// 紧凑循环中
for item := range work {
    processBatch(item) // 保证是抢占点
}
```

**取舍：**`//go:noinline` 阻止内联，意味着：

- **好处：**保证调度器抢占检查；防 goroutine 饥饿
- **代价：**增加函数调用开销（约 10-30 个 CPU 周期）；降低调用方的指令级并行（ILP）

只有 profiling 显示调度器抢占饥饿确实阻塞了其他 goroutine 时才用 `//go:noinline`。不必要的 `//go:noinline` 指令损害吞吐和延迟。

## 反射与类型断言

**诊断：**1- `go tool pprof`（CPU profile）——找热路径中出现的 `reflect.Value.*`、`reflect.DeepEqual` 或 `fmt.Sprintf`（内部用 reflect）2- `go test -bench`——对比反射版与类型化版本；视反射操作不同，预期差 10-200 倍

- **热路径中的 `reflect`**——类型内省和装箱让它慢 10-100 倍。换泛型或类型化代码
- **`reflect.DeepEqual`**——比类型化比较慢 50-200 倍。用 `slices.Equal`、`maps.Equal`、`bytes.Equal`（Go 1.21+）
- **type switch vs 重复断言**——type switch 一次求值完成分发：

```go
// 差——多次求值接口
if s, ok := v.(string); ok { return s }
if i, ok := v.(int); ok { return strconv.Itoa(i) }

// 好——单次分发
switch v := v.(type) {
case string: return v
case int:    return strconv.Itoa(v)
}
```

## 单调时钟

**诊断：**1- `go test -bench`——对比 `time.Since(start)` 与 `time.Now().Sub(start)` 的基准；单调时钟避开墙钟系统调用，有虽小但稳定的改进

`time.Since(start)` 用单调时钟，不受墙钟调整（NTP、夏令时）影响，还略快：

```go
var appStart = time.Now() // 程序启动时捕获单调时间 + 墙钟

func myFunc() {
    // 比较时长，不比较墙钟时刻
    elapsed := time.Since(appStart)
    if elapsed > threshold { ... }
}
```
