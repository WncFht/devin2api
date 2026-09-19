# 缓存模式

最快的代码是不运行的代码。缓存预计算结果、对并发请求去重、避免不必要的工作，往往是杠杆最高的性能改进。

## 目录

- [预编译模式缓存](#预编译模式缓存)
    - [包级 Regexp](#包级-regexp)
    - [模板缓存](#模板缓存)
    - [预计算查找表](#预计算查找表)
- [请求级缓存](#请求级缓存)
    - [用 singleflight 防缓存击穿](#用-singleflight-防缓存击穿)
    - [LRU 缓存](#lru-缓存)
- [算法复杂度](#算法复杂度)
- [避免无谓工作](#避免无谓工作)
    - [用 map 查找代替切片扫描](#用-map-查找代替切片扫描)
    - [提前返回与短路循环](#提前返回与短路循环)
    - [避免迭代器链](#避免迭代器链)
    - [用直接循环代替间接函数调用](#用直接循环代替间接函数调用)

## 预编译模式缓存

**诊断：**1- `go tool pprof`（CPU profile）——在热路径里找 `regexp.Compile`、`regexp.MustCompile` 或 `template.Parse`；它们出现说明模式在每次调用时重编译而不是只编译一次 2- `go test -bench -benchmem`——对比每次调用编译与缓存版本的基准；预期 10-12 倍提升且编译步骤 allocs/op 降到零

### 包级 Regexp

`regexp.Compile` 把模式解析成状态机——每次编译约 5,700ns。编译后 regexp 的匹配操作约 450ns。每次调用都编译浪费 10-12 倍：

```go
// 差——每次调用都编译
func isValid(email string) bool {
    re := regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$`)
    return re.MatchString(email)
}

// 好——只编译一次，并发安全
var emailRegex = regexp.MustCompile(`^[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}$`)

func isValid(email string) bool { return emailRegex.MatchString(email) }
```

注意：`regexp.MustCompile` 对非法模式会 panic——包级常量没问题（启动时就暴露）。用户提供的模式用 `regexp.Compile`。Go 的 regexp 用线性时间匹配（无回溯）。

### 模板缓存

`template.Parse` 同样昂贵。启动时解析一次：

```go
var reportTmpl = template.Must(template.ParseFiles("templates/report.html"))
```

### 预计算查找表

当计算是纯函数（同输入同输出）且输入空间小时，用数组查表代替计算：

```go
var hexDigit = [16]byte{'0','1','2','3','4','5','6','7','8','9','a','b','c','d','e','f'}

func byteToHex(b byte) (byte, byte) {
    return hexDigit[b>>4], hexDigit[b&0x0f] // 两次数组查找，替代分支逻辑
}
```

表能放进 L1/L2 缓存时，查表比简单计算还快。

## 请求级缓存

**诊断：**1- `go tool pprof`（goroutine profile）——找大量 goroutine 阻塞在同一个外部调用上（HTTP 拉取、DB 查询）；这是缓存击穿的信号：N 个 goroutine 同时 miss 缓存 2- `fgprof`——显示 off-CPU 等待时间；若同一个 fetch 函数在大量 goroutine 上主导墙钟时间，确认存在重复的并发工作 3- `go tool pprof -alloc_objects`——检查缓存 miss 处理是否大量分配；fetch 函数上的高分配数确认击穿同时制造 GC 压力

### 用 singleflight 防缓存击穿

缓存条目过期时，许多 goroutine 可能同时发现 miss 并都去请求同一个昂贵计算。`singleflight` 保证只有一个 goroutine 去取，其余等待：

```go
import "golang.org/x/sync/singleflight"

var (
    cache sync.Map
    sf    singleflight.Group
)

func GetWeather(city string) (string, error) {
    if val, ok := cache.Load(city); ok {
        return val.(string), nil
    }

    // 只有一个 goroutine 去取；其余在同 key 上阻塞
    result, err, _ := sf.Do(city, func() (any, error) {
        data, err := fetchFromAPI(city)
        if err == nil { cache.Store(city, data) }
        return data, err
    })
    return result.(string), err
}
```

→ 见 `samber/cc-skills-golang@golang-concurrency` skill：`singleflight` API 细节与 `sync.Map` vs `RWMutex` 选型指导。→ **泛型替代：**用 `github.com/samber/go-singleflightx` 避免 interface{} 装箱开销；结果取回比标准库 `singleflight.Group` 快 2-4 倍。

### LRU 缓存

带淘汰的有界缓存，标准库 `container/list` 能用但缓存局部性差（每个节点是一次独立堆分配）。高性能 LRU：

- **`github.com/hashicorp/golang-lru`**——线程安全、API 简单
- **`github.com/elastic/go-freelru`**——把 hashmap 与 ringbuffer 合并进连续内存，比分片实现快约 37 倍

用第三方缓存库时，API 签名以该库官方文档为准。

## 算法复杂度

**诊断：**1- `go tool pprof`（CPU profile）——找累计时间高且含嵌套循环或反复线性扫描的函数；这些是算法复杂度瓶颈 2- `go test -bench`——用不同输入规模做基准（100、1K、10K、100K）；时间若随输入平方增长（10 倍输入 → 100 倍时间），算法是 O(n²)，需要换掉

微优化之前，先确认算法本身不是瓶颈。O(n²) 算法上的常数因子改进，在规模上输给朴素的 O(n log n) 实现。

**Go 常见复杂度陷阱：**

| 模式                     | 复杂度         | 修法                                                 | 修后复杂度                     |
| ------------------------ | -------------- | ---------------------------------------------------- | ------------------------------ |
| 循环里 `slices.Contains` | O(n·m)         | 先建 `map[T]struct{}` 再查                           | O(n+m)                         |
| 嵌套循环做匹配           | O(n²)          | 用 map 建索引、排序 + 二分、或 `slices.BinarySearch` | O(n log n) 或 O(n)             |
| `append` 不预分配反复用  | O(n²) 摊销拷贝 | `make([]T, 0, n)`                                    | O(n)                           |
| `+=` 拼字符串            | O(n²) 总拷贝   | `strings.Builder`                                    | O(n)                           |
| 线性扫描求 min/max/去重  | 每次查询 O(n)  | 排序一次，查询多次                                   | O(n log n) + 每次查询 O(log n) |

**先想 Big-O，再优化常数。**常数因子 10 倍改进重要；从 O(n²) 换到 O(n) 更重要。

## 避免无谓工作

**诊断：**1- `go tool pprof`（CPU profile）——找热路径中消耗 CPU 的线性扫描函数（`slices.Contains`、`slices.Index`）或迭代器链（`Filter`、`Map`） 2- `go test -bench`——对比当前写法与 map 版或提前返回版的基准；成员测试预期 O(n) → O(1)，短路循环有显著提升

### 用 map 查找代替切片扫描

`Contains(slice, element)` 是 O(n)。map 查找是 O(1)。对同一集合做多次成员测试时，先建一次 map：

```go
// 差——O(n*m)，逐元素查 Contains
for _, item := range subset {
    if !Contains(collection, item) { return false } // 每次检查 O(n)
}

// 好——O(n+m)，建一次 map，O(1) 查找
seen := make(map[T]struct{}, len(collection))
for _, item := range collection { seen[item] = struct{}{} }
for _, item := range subset {
    if _, ok := seen[item]; !ok { return false }
}
```

set 用 `struct{}`（0 字节）而不是 `bool`（1 字节）。

### 提前返回与短路循环

答案已知就立刻返回。1000 次迭代里第 3 次命中，省掉 997 次：

```go
// 差——永远遍历整个集合
found := false
for _, item := range collection {
    if item == target { found = true }
}
return found

// 好——首次命中即返回
for i := range collection {
    if collection[i] == target { return true }
}
return false
```

### 避免迭代器链

迭代器操作链（`Filter → Map → First`）会创建闭包和中间机制。直接循环更简单也更快：

```go
// 差——创建 2 个带闭包的迭代器
result, ok := First(Filter(collection, predicate))

// 好——单趟、提前返回、无闭包
for i := range collection {
    if predicate(collection[i]) { return collection[i], true }
}
```

### 用直接循环代替间接函数调用

函数包函数时（如 `FromSlicePtr` 用闭包调 `Map`），闭包间接层阻碍内联。换成直接循环：

```go
// 差——Map() 带闭包，逐元素函数调用开销
func FromSlicePtr(items []*T) []T {
    return Map(items, func(p *T) T { return *p })
}

// 好——直接循环，可内联，时间 -13% 至 -17%
func FromSlicePtr(items []*T) []T {
    result := make([]T, len(items))
    for i := range items { result[i] = *items[i] }
    return result
}
```
