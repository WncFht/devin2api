# 内存优化

减少分配是多数 Go 程序里回报最高的单项优化。每次分配最终都要由 GC 收拾——减少分配次数与体积直接降低 GC 停顿和 CPU 开销。

## 目录

- [分配模式](#分配模式)
    - [用 append(s[:0], ...) 复用切片](#用-appends0--复用切片)
    - [直接索引与 append](#直接索引与-append)
    - [消除冗余 map 查找](#消除冗余-map-查找)
    - [map 容量提示](#map-容量提示)
    - [哨兵错误与 fmt.Errorf](#哨兵错误与-fmterrorf)
    - [接口装箱](#接口装箱)
- [底层数组泄漏](#底层数组泄漏)
    - [切片再切保留整个底层数组](#切片再切保留整个底层数组)
    - [子串内存泄漏](#子串内存泄漏)
    - [map 永不收缩](#map-永不收缩)
- [字符串与字节优化](#字符串与字节优化)
- [sync.Pool 热路径模式](#syncpool-热路径模式)
- [内存布局](#内存布局)
    - [结构体字段对齐](#结构体字段对齐)
    - [结构体末尾的零尺寸字段](#结构体末尾的零尺寸字段)
    - [大结构体用指针接收者](#大结构体用指针接收者)
    - [频繁更新的大结构体用指针 map](#频繁更新的大结构体用指针-map)

## 分配模式

**诊断：**1- `go tool pprof -alloc_objects`——按堆分配次数给函数排名；预期热路径函数（请求处理器、序列化器）排在前列，alloc/op 上千 2- `go build -gcflags="-m -m"`——详细逃逸分析，显示变量为什么逃逸；在你预期留在栈上的变量上找 `"leaking param"`、`"too large for stack"` 或 `"captured by closure"` 3- `go test -bench -benchmem`——测每个基准的 allocs/op 与 B/op；预期目标函数显示可消除的 >0 allocs/op

### 用 append(s[:0], ...) 复用切片

重切到零长度保留底层数组，把本来的一次新分配变成无操作：

```go
// 差——分配新切片，旧的变成垃圾
mode = []T{item}

// 好——复用现有底层数组（0 次分配）
mode = append(mode[:0], item)
```

### 直接索引与 append

输出尺寸等于输入尺寸时，用 `make([]T, len(input))` 加直接赋值，而不是 `make([]T, 0, len(input))` 加 `append`。直接赋值省掉逐元素的边界检查和长度递增：

```go
// 慢——逐元素吃 append 开销
result := make([]T, 0, len(input))
for i := range input { result = append(result, transform(input[i])) }

// 快——直接赋值
result := make([]T, len(input))
for i := range input { result[i] = transform(input[i]) }
```

结果可能更小（过滤场景）或提前出错返回可能丢弃部分结果时用 append。

### 消除冗余 map 查找

`for k := range m { use(m[k]) }` 每次迭代查两次。从 range 直接拿值：

```go
// 差——每次迭代两次查找
for k := range in { result[k] = fn(in[k]) }

// 好——单次查找
for k, v := range in { result[k] = fn(v) }
```

### map 容量提示

`make(map[K]V)` 起始桶数少，增长时反复 rehash。给容量提示避免 rehash：

```go
m := make(map[string]int, len(items)) // 单次分配，无 rehash
```

### 哨兵错误与 fmt.Errorf

`fmt.Errorf` 每次调用都分配。热路径中可预期的错误用预分配的哨兵：

```go
var ErrNegative = errors.New("value is negative") // 只分配一次

func validate(x int) error {
    if x < 0 { return ErrNegative } // 零分配
    return nil
}
```

只有需要动态上下文（字段名、值）时才用 `fmt.Errorf`。

### 接口装箱

具体类型经 `any`/`interface{}` 传递会强制堆分配做装箱。热路径中用类型化参数或泛型：

```go
// 差——每个 int 装箱，产生分配
func sum(values []any) int { ... }

// 好——无装箱，无分配
func sum(values []int) int { ... }

// 好——泛型，同样无装箱
func sum[T ~int | ~int64](values []T) T { ... }
```

## 底层数组泄漏

**诊断：**1- `go tool pprof -inuse_space`——按分配点显示当前存活堆内存；找意外大的存活对象（MB 级）——本该被 GC 回收却还活着，是底层数组被保留的信号 2- `go tool pprof -alloc_space`——显示累计分配字节；找分配字节远超其最终持有数据的分配点（如为 16 字节结果分配了 100MB）

### 切片再切保留整个底层数组

对大切片切出一小片，会把整个原数组留在内存里：

```go
// 差——留住整个 MB 级底层数组
func getHeader(data []byte) []byte { return data[:16] }

// 好——独立拷贝，原数组可被 GC
func getHeader(data []byte) []byte {
    header := make([]byte, 16)
    copy(header, data[:16])
    return header
}
```

### 子串内存泄漏

子串共享原字符串的底层数组：

```go
// 差——整个 longMsg 留在内存里
func extractID(msg string) string { return msg[:8] }

// 好——独立拷贝（Go 1.20+）
func extractID(msg string) string { return strings.Clone(msg[:8]) }
```

### map 永不收缩

Go 的 map 会增长，但删条目时永不释放桶内存。装过百万条目的 map 永远保留那份分配：

```go
// 定期重建以回收内存
func compact(old map[string]Data) map[string]Data {
    m := make(map[string]Data, len(old))
    for k, v := range old { m[k] = v }
    return m // 旧 map 变为可被 GC
}
```

## 字符串与字节优化

**诊断：**1- `go tool pprof -alloc_objects`——找字符串/字节转换函数（`runtime.stringtoslicebyte`、`runtime.slicebytetostring`）成为分配大户 2- `go test -bench -benchmem`——测 allocs/op；预期反复转换显示每次转换 1+ alloc/op，缓存后可降到零

**缓存 string↔byte 转换**——`string` 与 `[]byte` 互转每次都分配一份拷贝。转一次复用结果。

**直接用 `bytes` 包**——`bytes.Contains`、`bytes.HasPrefix`、`bytes.Split`、`bytes.ToUpper` 等直接操作 `[]byte`，不经字符串转换。`bytes` 包覆盖了 `strings` 的大部分功能。

## sync.Pool 热路径模式

**诊断：**1- `go tool pprof -alloc_objects`——找出反复创建同类型对象的热分配点（如 `[]byte` 缓冲、临时结构体）；预期有一个每秒数千分配的点可以池化

`sync.Pool` 跨 GC 周期回收对象，降低分配压力。用于热路径中频繁分配、生命周期短的对象（HTTP 处理器、序列化、日志）：

```go
var bufPool = sync.Pool{
    New: func() any {
        buf := make([]byte, 0, 4096)
        return &buf
    },
}

func handleRequest(data []byte) []byte {
    bp := bufPool.Get().(*[]byte)
    buf := (*bp)[:0] // 重置长度，保留容量
    defer func() { *bp = buf; bufPool.Put(bp) }()

    // ... 把 data 处理进 buf ...

    result := make([]byte, len(buf))
    copy(result, buf) // 返回拷贝——buf 归还池中
    return result
}
```

**规则：**

- `Put()` 前重置状态——清掉引用，避免大对象图跨 GC 周期被留住
- 返回拷贝而不是池中缓冲——调用方不得持有池化内存的引用
- 不要池化 >32KB 的对象——大分配不走池的 size class，GC 处理它们本就高效
- 不要池化用得少的对象——分配稀少时池化开销超过收益

→ 见 `golang-concurrency` skill：`sync.Pool` API 参考与基础用法。

## 内存布局

**诊断：**1- `fieldalignment ./...`——检测浪费填充字节的结构体；预期警告形如 `"struct of size 40 could be 24"`，列出哪些结构体受益于字段重排 2- `unsafe.Sizeof`/`Alignof`/`Offsetof`——量出精确字节尺寸与字段偏移；用于确认改动前后的收益并写进代码注释

### 结构体字段对齐

Go 为满足对齐要求在字段间加填充。把字段从大到小重排：

```go
// 差——24 字节（7 + 3 字节填充）
type Bad struct {
    a bool    // 1 字节 + 7 填充
    b int64   // 8 字节
    c bool    // 1 字节 + 3 填充
    d int32   // 4 字节
}

// 好——16 字节（2 字节填充）
type Good struct {
    b int64   // 8 字节
    d int32   // 4 字节
    a bool    // 1 字节
    c bool    // 1 字节 + 2 填充
}
```

**对齐要求：**`bool`/`byte` = 1，`int16` = 2，`int32`/`float32` = 4，`int64`/`float64`/`string`/`[]T`/`*T` = 8。

**查看布局：**`unsafe.Sizeof(T{})`、`unsafe.Alignof(T{})`、`unsafe.Offsetof(T{}.field)`

### 结构体末尾的零尺寸字段

最后一个字段是零尺寸（`struct{}`）时，编译器会补一个机器字长的填充，防止指向该字段的指针越出进入下一块内存：

```go
// 差——16 字节（Value 8 + Flag 填充 8）
type Entry struct { Value int64; Flag struct{} }

// 好——8 字节（Flag 0 + Value 8）
type Entry struct { Flag struct{}; Value int64 }
```

结构体里放 `struct{}` 字段很少见，几乎没用。

### 大结构体用指针接收者

值接收者每次方法调用都拷贝整个结构体。大于约 128 字节的结构体用指针接收者。任一方法用了指针接收者，其余方法也该统一。

### 频繁更新的大结构体用指针 map

map 的值不可取址——无法原地修改字段。大结构体频繁更新时，`map[K]*V` 免去「拷贝 - 修改 - 写回」模式：

```go
players := map[string]*Player{"alice": {Score: 100}}
players["alice"].Score += 10 // 直接修改，无需拷贝
```

代价：每个指针是一次独立堆分配，增加 GC 压力。小且以读为主的结构体，`map[K]V`（值）更好。
