# 常见 Go bug

## 目录

- [nil 指针解引用](#nil-指针解引用)
- [接口 nil 陷阱](#接口-nil-陷阱)
- [`:=` 造成的变量遮蔽](#-造成的变量遮蔽)
- [Slice 与 Map 陷阱](#slice-与-map-陷阱)
- [Defer 陷阱](#defer-陷阱)
- [错误处理陷阱](#错误处理陷阱)
- [Context 误用](#context-误用)
- [并发 Map 读写（致命）](#并发-map-读写致命)
- [拷贝 sync 类型](#拷贝-sync-类型)
- [在 goroutine 内调 WaitGroup.Add](#在-goroutine-内调-waitgroupadd)
- [HTTP 错误响应后缺 return](#http-错误响应后缺-return)
- [JSON 陷阱](#json-陷阱)
    - [数字进 `interface{}` 变成 `float64`](#数字进-interface-变成-float64)
    - [未导出字段被静默忽略](#未导出字段被静默忽略)
- [`strings.Trim` 与 `strings.TrimPrefix`](#stringstrim-与-stringstrimprefix)
- [字符串长度与索引](#字符串长度与索引)
- [`for` 循环内 `select`/`switch` 里的 `break`](#for-循环内-selectswitch-里的-break)
- [`iota` 枚举的零值问题](#iota-枚举的零值问题)
- [`recover()` 只在同一 goroutine 生效](#recover-只在同一-goroutine-生效)
- [`os.Exit` 跳过 defer 函数](#osexit-跳过-defer-函数)
- [`time.Time` 比较：`==` vs `.Equal()`](#timetime-比较-vs-equal)
- [`sql.Rows` 必须关闭](#sqlrows-必须关闭)
- [向已关闭 channel 写入会 panic](#向已关闭-channel-写入会-panic)
- [`select` 里已关闭 channel 造成忙循环](#select-里已关闭-channel-造成忙循环)
- [带 `default` 的 `select` 会空转 CPU](#带-default-的-select-会空转-cpu)
- [整数转换静默截断](#整数转换静默截断)
- [`filepath.Join` 防不住路径穿越](#filepathjoin-防不住路径穿越)
- [指针接收者的接口满足](#指针接收者的接口满足)
- [热路径上的 `regexp.MustCompile`](#热路径上的-regexpmustcompile)
- [`init()` 顺序很脆弱](#init-顺序很脆弱)
- [map 迭代顺序是随机的](#map-迭代顺序是随机的)
- [`switch` 里 `fallthrough` 无条件执行](#switch-里-fallthrough-无条件执行)

## nil 指针解引用

来自外部的指针解引用前必须检查。

最常见的 Go panic。堆栈会告诉你确切的行号。

```go
// 1. 未初始化的结构体字段
type Server struct {
    logger *log.Logger  // 构造函数没设就是 nil
}

// 2. 未检查的 error 返回——err != nil 时 val 可能是 nil/零值
val, err := doSomething()
val.Method()  // doSomething 返回了带 error 的 nil val 就 panic

// 3. map 查找返回零值
m := map[string]*Config{}
cfg := m["missing"]  // cfg 是 nil
cfg.Timeout  // panic

// 4. 不带 comma-ok 的类型断言
var i interface{} = "hello"
n := i.(int)        // panic
n, ok := i.(int)    // ok == false，不 panic
```

## 接口 nil 陷阱

接口可能装着 typed nil 指针时，永远不要拿它跟 nil 比。

接口里的 typed nil 指针**不是** nil 接口：

```go
type MyError struct{ msg string }
func (e *MyError) Error() string { return e.msg }

func doWork() error {
    var err *MyError  // typed nil 指针
    return err        // 返回的是装着 nil 指针的非 nil 接口！
}

func main() {
    if err := doWork(); err != nil {
        // 这里会执行——接口非 nil
        fmt.Println(err)  // panic：Error() 里 nil 指针
    }
}

// 修：显式返回 nil，别返回 typed nil 变量
func doWork() error {
    return nil
}
```

## `:=` 造成的变量遮蔽

`:=` 短声明在内层作用域创建了新变量，而不是给外层变量赋值。遮蔽 `err` 时尤其危险，因为错误处理会静默失效。

```go
// 坏
func doWork() error {
    var err error
    if condition {
        result, err := someFunc() // BUG：新 err 变量，没赋值给外层
        if err != nil {
            return err
        }
        process(result)
    }
    return err // 永远 nil——内层 err 是另一个变量
}

// 好
func doWork() error {
    var err error
    if condition {
        var result ResultType
        result, err = someFunc() // 赋值给外层 err
        if err != nil {
            return err
        }
        process(result)
    }
    return err
}
```

**检测：**在 lint 配置里跑 `golang.org/x/tools/go/analysis/passes/shadow` 分析器。旧的 shadow flag 不在标准 `go vet` 里。

## Slice 与 Map 陷阱

```go
// 1. 写 nil map 会 panic
var m map[string]int
m["key"] = 1  // panic：assignment to entry in nil map
// 修：m := make(map[string]int)
// 注意：读 nil map 没问题——返回零值

// 2. append 可能共享底层数组
a := []int{1, 2, 3}
b := a[:2]
b = append(b, 99)  // 覆盖了 a[2]！
// 修：全切片表达式——b := a[:2:2] 限制容量

// 3. goroutine 捕获 range 变量（Go < 1.22）
for _, v := range items {
    go func() {
        process(v)  // v 是共享的，很可能拿到最后一个元素
    }()
}
// 修：当参数传进去
for _, v := range items {
    go func(v Item) { process(v) }(v)
}
// Go 1.22+ 循环变量是每次迭代独立的（不用修）
```

## Defer 陷阱

```go
// 1. 参数立即求值
x := 1
defer fmt.Println(x)  // 打 1，不是 2
x = 2

// 2. 循环里 defer——函数返回前不执行
for _, f := range files {
    file, _ := os.Open(f)
    defer file.Close()  // 所有 Close() 堆到返回时才跑
}
// 修：包进闭包
for _, f := range files {
    func() {
        file, _ := os.Open(f)
        defer file.Close()
        // 用 file
    }()
}

// 3. 具名返回值 + defer 交互
func readFile() (err error) {
    f, err := os.Open("file.txt")
    if err != nil { return }
    defer func() {
        if closeErr := f.Close(); err == nil {
            err = closeErr  // 改具名返回值
        }
    }()
    // ...
    return nil
}
```

## 错误处理陷阱

**静默吞错误**是「灵异」bug 最常见的单一来源：

```go
// 坏——静默失败
result, _ := doSomething()
json.Unmarshal(data, &config)
http.ListenAndServe(":8080", nil)

// 好——处理或向上传
result, err := doSomething()
if err != nil {
    return fmt.Errorf("doSomething: %w", err)
}
```

**找被忽略的错误：**

```bash
go vet ./...

# 更彻底
go get -tool github.com/kisielk/errcheck@latest
go tool errcheck ./...
```

**错误包装——用 `%w`，不用 `%v`：**

```go
return fmt.Errorf("reading config from %s: %v", path, err)  // 坏——丢了错误链
return fmt.Errorf("reading config from %s: %w", path, err)  // 好——保留 Is/As

// 查特定错误——用 errors.Is，不用 ==
if err == sql.ErrNoRows { ... }            // 坏——被包装就失效
if errors.Is(err, sql.ErrNoRows) { ... }   // 好——沿链查找

// 提取具体类型的错误
var pathErr *os.PathError
if errors.As(err, &pathErr) { ... }
```

## Context 误用

```go
// 1. 忘了 cancel——泄漏 goroutine
ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
// 缺：defer cancel()

// 2. 该传递时用了 background context
go doWork(context.Background())  // 坏——父级取消不了
go doWork(ctx)                   // 好——尊重父级取消

// 3. 不查 context 错误
err := doWork(ctx)
if err != nil {
    // 区分超时和其他错误
    if ctx.Err() == context.DeadlineExceeded {
        log.Printf("operation timed out")
    } else if ctx.Err() == context.Canceled {
        log.Printf("operation cancelled")
    } else {
        log.Printf("operation failed: %v", err)
    }
}

// 4. 后台任务活过了请求 context
func handler(w http.ResponseWriter, r *http.Request) {
    // 坏——后台任务用请求 context，客户端断连就被取消
    go processAsync(r.Context(), data)

    // 好——为后台任务派生新 context（Go 1.21+）
    bgCtx := context.WithoutCancel(r.Context())
    go processAsync(bgCtx, data)
}
```

## 并发 Map 读写（致命）

不同步就并发访问 map 是禁止的。

跟大多数 Go runtime 错误不同，并发 map 读写是 **fatal error**——`recover()` **抓不住**，整个进程直接崩。测试里难抓，因为它看时序。

```go
// 坏——fatal：concurrent map read and map write
m := make(map[string]int)
go func() { m["key"] = 1 }()  // 并发写
go func() { _ = m["key"] }()  // 并发读——致命！

// 好——mutex 保护
var mu sync.RWMutex
m := make(map[string]int)
go func() { mu.Lock(); m["key"] = 1; mu.Unlock() }()
go func() { mu.RLock(); _ = m["key"]; mu.RUnlock() }()

// 读多写少且键集稳定的场景也可以用 sync.Map
```

**检测：**`go test -race ./...`——CI 里永远跑。

## 拷贝 sync 类型

sync 类型永远不许拷贝——用指针接收者、按指针传。

所有 `sync` 类型（`Mutex`、`RWMutex`、`WaitGroup`、`Once`、`Cond`、`Map`、`Pool`）都不能拷贝。通过值接收者、函数参数或结构体赋值拷贝它们，会静默破坏同步。

```go
// 坏——值接收者拷贝了 Mutex
type Counter struct {
    mu    sync.Mutex
    count int
}

func (c Counter) Increment() { // BUG：每次调用都拷贝 mutex
    c.mu.Lock()
    c.count++
    c.mu.Unlock()
}

// 好——指针接收者
func (c *Counter) Increment() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.count++
}
```

**检测：**`go vet` 能查出 mutex 拷贝。对所有 sync 类型都适用。

## 在 goroutine 内调 WaitGroup.Add

`wg.Add(1)` 在 goroutine 里面调而不是在启动之前调，`wg.Wait()` 可能在所有 goroutine 启动前就返回——一种大多数时候能过测试、偶尔才翻车的竞态。

```go
// 坏
var wg sync.WaitGroup
for i := 0; i < n; i++ {
    go func() {
        wg.Add(1) // BUG：可能在 wg.Wait() 返回后才跑
        defer wg.Done()
        doWork()
    }()
}
wg.Wait()

// 好
var wg sync.WaitGroup
for i := 0; i < n; i++ {
    wg.Add(1) // 在启动 goroutine 之前调
    go func() {
        defer wg.Done()
        doWork()
    }()
}
wg.Wait()
```

## HTTP 错误响应后缺 return

`http.Error()` 写出错误后执行会继续。可能导致重复写响应、响应损坏，或者执行了本该跳过的逻辑。

```go
// 坏
func handler(w http.ResponseWriter, r *http.Request) {
    if !authorized(r) {
        http.Error(w, "Forbidden", http.StatusForbidden)
        // BUG：缺 return——handler 继续往下跑
    }
    doSensitiveAction(r)
}

// 好
func handler(w http.ResponseWriter, r *http.Request) {
    if !authorized(r) {
        http.Error(w, "Forbidden", http.StatusForbidden)
        return
    }
    doSensitiveAction(r)
}
```

## JSON 陷阱

### 数字进 `interface{}` 变成 `float64`

反序列化进 `map[string]interface{}` 或 `interface{}` 时，所有 JSON 数字都变成 `float64`。断言成 `int` 会 panic。大整数（> 2^53）静默丢精度。

```go
// 坏
var result map[string]interface{}
json.Unmarshal([]byte(`{"id": 1234567890123456789}`), &result)
id := result["id"].(int) // PANIC：是 float64，不是 int

// 好——用有类型的结构体（首选）
type Response struct {
    ID int64 `json:"id"`
}

// 好——必须用 interface{} 时用 json.Number
dec := json.NewDecoder(bytes.NewReader(data))
dec.UseNumber()
var result map[string]interface{}
dec.Decode(&result)
id, _ := result["id"].(json.Number).Int64()
```

### 未导出字段被静默忽略

小写开头的字段对 `encoding/json` 不可见。Marshal 产出空输出，Unmarshal 跳过它们——两种情况都不报错。

```go
// 坏
type User struct {
    name  string `json:"name"`  // 未导出——被静默忽略！
    email string `json:"email"` // 未导出——被静默忽略！
}
u := User{name: "Alice", email: "alice@example.com"}
data, _ := json.Marshal(u) // data 是 "{}"——不报错

// 好
type User struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}
```

**检测：**`go vet` 会对带 JSON struct tag 的未导出字段告警。

## `strings.Trim` 与 `strings.TrimPrefix`

`strings.Trim` 把第二个参数当**字符集合**从两端剥离，不是当子串。会发生意外的过度裁剪。

```go
// 坏
s := strings.Trim("application/json", "application/")
// 结果："js"——把集合 {a,p,l,i,c,t,o,n,/} 里的字符从两端全剥了！

// 好
s := strings.TrimPrefix("application/json", "application/")
// 结果："json"
```

去子串用 `strings.TrimPrefix`/`strings.TrimSuffix`。只有真的想剥字符集合时才用 `strings.Trim`。

## 字符串长度与索引

`len()` 对字符串返回字节数，不是字符数。索引返回的是字节。对多字节 UTF-8 字符，这会给出错误的计数，切片时还会损坏数据。

```go
s := "Hello, 世界"
fmt.Println(len(s))    // 13（字节），不是 9（字符）
fmt.Println(s[:8])     // "Hello, \xe4"——坏了！切开了一个多字节 rune

// 修：字符数用 utf8.RuneCountInString
fmt.Println(utf8.RuneCountInString(s)) // 9

// 修：按字符切片先转 []rune
runes := []rune(s)
fmt.Println(string(runes[:8])) // "Hello, 世"

// 修：for-range 按字符（rune）迭代，不按字节
for _, r := range s { ... } // 迭代的是 rune
```

## `for` 循环内 `select`/`switch` 里的 `break`

`for` 循环里的 `select` 或 `switch` 中，裸 `break` 只跳出 `select`/`switch`，不跳出循环。

```go
// 坏
for {
    select {
    case msg := <-ch:
        if msg == "quit" {
            break // BUG：只跳出 select，循环永远继续
        }
        process(msg)
    }
}

// 好——用带标签的 break
loop:
for {
    select {
    case msg := <-ch:
        if msg == "quit" {
            break loop // 跳出 for 循环
        }
        process(msg)
    }
}
```

## `iota` 枚举的零值问题

`iota` 从 0 开始时，类型的零值（未初始化变量、零值结构体字段、缺失的 JSON 字段）跟第一个常量无法区分。

```go
// 坏
type Status int
const (
    Active   Status = iota // 0——跟零值相同！
    Inactive               // 1
)
type User struct {
    Status Status // 零值是 Active——但这是有意的吗？
}

// 好——把 0 留给「未知」
type Status int
const (
    StatusUnknown  Status = iota // 0——显式的未设置哨兵
    StatusActive                 // 1
    StatusInactive               // 2
)
```

## `recover()` 只在同一 goroutine 生效

`recover()` 只能接住它被 defer 的那个 goroutine 里的 panic。子 goroutine 里的 panic 会搞崩整个程序——父 goroutine 接不住。

```go
// 坏——main 里的 recover() 接不住子 goroutine 的 panic
func main() {
    defer func() {
        if r := recover(); r != nil {
            fmt.Println("recovered:", r) // 永远到不了
        }
    }()
    go func() {
        panic("crash!") // 搞崩整个程序
    }()
    time.Sleep(time.Second)
}

// 好——每个 goroutine 自己 recover
func main() {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                log.Printf("goroutine recovered: %v", r)
            }
        }()
        panic("crash!") // 在这个 goroutine 内被接住
    }()
    time.Sleep(time.Second)
}
```

## `os.Exit` 跳过 defer 函数

`os.Exit` 立即终止进程。所有 defer 都不执行——清理、flush、close 全被跳过。`log.Fatal` 内部调 `os.Exit(1)`，有同样的问题。

```go
// 坏——defer 的清理永远不跑
func main() {
    f, _ := os.Create("data.tmp")
    defer f.Close()       // 永不执行
    defer os.Remove(f.Name()) // 永不执行

    if err := process(); err != nil {
        log.Fatal(err) // 调 os.Exit(1)——跳过全部 defer！
    }
}

// 好——从 main 返回，或者重构让 defer 先跑完
func main() {
    if err := run(); err != nil {
        fmt.Fprintf(os.Stderr, "error: %v\n", err)
        os.Exit(1) // run() 返回时它的 defer 已经跑完了
    }
}

func run() error {
    f, _ := os.Create("data.tmp")
    defer f.Close()
    return process()
}
```

## `time.Time` 比较：`==` vs `.Equal()`

`time.Time` 带一个单调时钟读数。两个表示同一时刻的 `time.Time` 如果一个带单调分量另一个不带，`==` 可能不成立（比如一个来自 `time.Now()`，另一个从 JSON/数据库反序列化来）。

```go
// 坏——同一时刻也可能不等
t1 := time.Now()
data, _ := t1.MarshalJSON()
var t2 time.Time
t2.UnmarshalJSON(data)
fmt.Println(t1 == t2) // false！t1 带单调读数，t2 不带

// 好——.Equal() 忽略单调时钟
fmt.Println(t1.Equal(t2)) // true

// 另外：存储/比较前显式剥掉单调读数
t1 = t1.Round(0) // 剥掉单调读数
```

## `sql.Rows` 必须关闭

`sql.Rows` 必须调 `rows.Close()`——永远在查询后立即 defer。

忘了关 `sql.Rows` 会泄漏数据库连接。连接一直被占着直到 `Rows` 被 GC，但负载下连接池先耗尽。

```go
// 坏——rows 不关就泄漏连接
rows, err := db.Query("SELECT id FROM users")
if err != nil { return err }
for rows.Next() {
    // ...
}
// rows 从没关——连接泄漏！

// 好——永远 defer Close
rows, err := db.Query("SELECT id FROM users")
if err != nil { return err }
defer rows.Close()
for rows.Next() {
    // ...
}
if err := rows.Err(); err != nil { // 别忘了查 rows.Err()
    return err
}
```

另外：单行查询用 `db.QueryRow()`，非 SELECT 语句（INSERT、UPDATE、DELETE）用 `db.Exec()`。非 SELECT 用 `db.Query()` 会泄漏连接，因为返回的 `Rows` 没人迭代/关闭。

## 向已关闭 channel 写入会 panic

向已关闭 channel 发送会 panic。从已关闭 channel 读取立即返回零值（`ok == false`）。

```go
// 坏——panic：send on closed channel
ch := make(chan int, 1)
close(ch)
ch <- 1 // panic！

// 好——只有发送方关闭，接收方永远不关
// 用 done channel 或 context 通知完成
func producer(ch chan<- int, done <-chan struct{}) {
    defer close(ch)
    for i := 0; ; i++ {
        select {
        case ch <- i:
        case <-done:
            return
        }
    }
}
```

**经验法则：**只有发送方关 channel。多个发送方时用 `sync.Once` 或 `sync.WaitGroup` 协调。

## `select` 里已关闭 channel 造成忙循环

已关闭 channel 永远可读（返回零值）。在 `select` 里这会让这个 case 不停触发——烧 CPU 的忙循环。

```go
// 坏——ch 关闭后这段以 100% CPU 空转
for {
    select {
    case v := <-ch: // ch 关闭后不停触发
        process(v)   // 永远在处理零值
    case <-done:
        return
    }
}

// 好——channel 关闭后置 nil
for {
    select {
    case v, ok := <-ch:
        if !ok {
            ch = nil // nil channel 在 select 里永远阻塞——禁用这个 case
            continue
        }
        process(v)
    case <-done:
        return
    }
}
```

## 带 `default` 的 `select` 会空转 CPU

带 `default` 的 `select` 永不阻塞。在 `for` 循环里就形成烧 CPU 的忙等。

```go
// 坏——等消息时以 100% CPU 空转
for {
    select {
    case msg := <-ch:
        process(msg)
    default:
        // ch 没东西时立即执行——死循环！
    }
}

// 好——去掉 default，阻塞到消息到达
for {
    select {
    case msg := <-ch:
        process(msg)
    case <-ctx.Done():
        return
    }
}

// 好——确实需要非阻塞检查时，加个小 sleep 或 ticker
for {
    select {
    case msg := <-ch:
        process(msg)
    default:
        time.Sleep(10 * time.Millisecond) // 让出 CPU
    }
}
```

## 整数转换静默截断

Go 整数转换不查溢出——静默截断。转换用户输入或外部数据时尤其危险。

```go
// 坏——静默截断
var big int64 = 256
small := int8(big)
fmt.Println(small) // 0——静默溢出了！

var n int64 = math.MaxInt64
n32 := int32(n)
fmt.Println(n32) // -1——静默回绕了！

// 好——转换前查边界
func safeIntToInt32(n int64) (int32, error) {
    if n < math.MinInt32 || n > math.MaxInt32 {
        return 0, fmt.Errorf("value %d overflows int32", n)
    }
    return int32(n), nil
}
```

## `filepath.Join` 防不住路径穿越

`filepath.Join` 会清理路径（解析 `..`），但不阻止逃出基准目录。用户提供的路径可以穿越到预期根目录之外。

```go
// 坏——用户可以逃出基准目录
base := "/srv/files"
userInput := "../../etc/passwd"
path := filepath.Join(base, userInput)
// path = "/etc/passwd"——逃出去了！

// 好（Go 1.24+）——把访问关在基准目录里
root, err := os.OpenRoot("/srv/files")
if err != nil {
    return err
}
defer root.Close()
file, err := root.Open(userInput)
if err != nil {
    return err
}
defer file.Close()
```

Go <1.24 在没有 `os.Root` 时用纯词法兜底：

```go
func safePath(base, userInput string) (string, error) {
    if userInput == "" || filepath.IsAbs(userInput) || !filepath.IsLocal(userInput) {
        return "", fmt.Errorf("invalid relative path: %q", userInput)
    }

    path := filepath.Join(base, userInput)
    rel, err := filepath.Rel(base, path)
    if err != nil {
        return "", fmt.Errorf("checking path: %w", err)
    }
    if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
        return "", fmt.Errorf("path traversal attempt: %s", userInput)
    }

    return path, nil
}
```

## 指针接收者的接口满足

`T` 类型的值满足不了要求 `*T` 接收者方法的接口。但 `*T` 能满足要求 `T` 或 `*T` 方法的接口。

```go
type Sizer interface {
    Size() int
}

type File struct{ size int }
func (f *File) Size() int { return f.size } // 指针接收者

var s Sizer
s = File{}   // 编译错误：File 没实现 Sizer（*File 实现了）
s = &File{}  // OK——*File 有 Size 方法

// 原因是编译器不是总能对值取地址
//（比如 map 的值、返回值）。指针接收者 = 必须用指针。
```

## 热路径上的 `regexp.MustCompile`

长寿命的正则必须在包级编译一次——不能在反复调用的函数里编译。只用一次的短寿命正则（比如 CLI 或测试里）内联可以接受。

`regexp.MustCompile` 每次调用都编译一遍正则。在热路径（循环、HTTP handler）里又贵又浪费。

```go
// 坏——每次调用都重新编译
func isEmail(s string) bool {
    re := regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
    return re.MatchString(s)
}

// 好——包级编译一次
var emailRe = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)

func isEmail(s string) bool {
    return emailRe.MatchString(s)
}
```

## `init()` 顺序很脆弱

`init()` 函数在包内按源文件顺序跑、跨包按依赖顺序跑。但依赖这个顺序会造出脆弱、难调试的初始化序列。同一文件里多个 `init()` 自上而下跑，但跨文件是按文件名字母序——加个文件就可能改变顺序。

```go
// 坏——init() 依赖另一个 init() 先跑完
var db *sql.DB

func init() {
    // 假设 config 的 init() 已经跑过——脆弱！
    db, _ = sql.Open("postgres", config.DatabaseURL)
}

// 好——显式初始化
func main() {
    cfg := loadConfig()
    db := setupDatabase(cfg)
    startServer(db)
}
```

优先在 `main()` 里显式初始化，少用 `init()`。`init()` 只留给真正自包含的装配（注册 driver、codec）。

## map 迭代顺序是随机的

Go 故意随机化 map 迭代顺序。假设特定顺序的代码会产出不一致的结果。

```go
// 坏——每次跑输出顺序都是随机的
m := map[string]int{"a": 1, "b": 2, "c": 3}
for k, v := range m {
    fmt.Printf("%s=%d ", k, v) // 每次顺序都不同！
}

// 好——要顺序就排序键
keys := make([]string, 0, len(m))
for k := range m {
    keys = append(keys, k)
}
sort.Strings(keys)
for _, k := range keys {
    fmt.Printf("%s=%d ", k, m[k])
}
```

这在测试（输出比较不确定）、序列化（JSON/输出不确定）和日志（diff 混乱）里尤其危险。

## `switch` 里 `fallthrough` 无条件执行

跟 C 不同，Go 的 `switch` case 默认不穿透。但显式写 `fallthrough` 时，它**无条件执行下一个 case 体**——不看下一个 case 的条件。

```go
// 意外点：fallthrough 不检查下一个条件
switch x := 5; {
case x > 10:
    fmt.Println(">10")
    fallthrough
case x > 0:
    fmt.Println(">0")
    fallthrough
case x < 0:
    fmt.Println("<0") // 执行了，尽管 5 并不 < 0！
}
// 输出：>0, <0

// fallthrough 很少真的需要。多值列出来更好：
switch status {
case "active", "enabled":
    enable()
}
```
