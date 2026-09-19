# I/O 与网络优化

网络和 I/O 瓶颈表现为 goroutine 阻塞在系统调用上或等响应。关键杠杆是连接复用、合理的超时、以及用流式代替缓冲。

## 目录

- [HTTP Transport 配置](#http-transport-配置)
    - [连接池](#连接池)
    - [超时](#超时)
    - [排空响应体以复用连接](#排空响应体以复用连接)
- [流式与缓冲](#流式与缓冲)
    - [大载荷避免 io.ReadAll](#大载荷避免-ioreadall)
    - [流式 JSON](#流式-json)
- [JSON 性能](#json-性能)
- [Cgo 开销](#cgo-开销)
- [缓冲 I/O](#缓冲-io)
- [并发多阶段流水线](#并发多阶段流水线)
    - [特殊场景](#特殊场景)
    - [何时使用（以及何时不用）](#何时使用以及何时不用)
- [批量操作](#批量操作)
    - [数据库：批量插入代替逐行](#数据库批量插入代替逐行)
    - [HTTP：批量 API 调用](#http批量-api-调用)
    - [Channel：流的批量处理](#channel流的批量处理)

## HTTP Transport 配置

**诊断：**1- `go tool pprof`（goroutine + block profile）——找阻塞在 `net/http.(*Transport).dialConn` 或 `net/http.(*persistConn).readLoop` 上的 goroutine；大量 goroutine 等在这里说明连接池耗尽 2- `fgprof`——同时捕获 on-CPU 与 off-CPU 等待时间；CPU profile 里显得便宜的 HTTP 调用若主导墙钟时间就要注意 3- `go tool trace`——可视化 goroutine 生命周期；找 goroutine 等网络 I/O 而非处理的长空隙 4- Prometheus `go_goroutines`——监控生产中 goroutine 数；负载稳定却持续上升，提示 HTTP client 配置错误导致连接或 goroutine 泄漏

### 连接池

默认 `http.Transport` 的池配置保守——`MaxIdleConnsPerHost` 默认 2。高并发下请求排队等连接而不是并行执行：

```go
// 差——默认 transport，每 host 只有 2 条空闲连接
client := &http.Client{}

// 好——为高并发服务间调用调优
var apiClient = &http.Client{
    Timeout: 30 * time.Second,
    Transport: &http.Transport{
        MaxIdleConns:          100,             // 所有 host 的空闲连接总数
        MaxIdleConnsPerHost:   20,              // 每 host 空闲连接（默认是 2！）
        MaxConnsPerHost:       50,              // 每 host 总连接上限（0 = 不限）
        IdleConnTimeout:       90 * time.Second,
        TLSHandshakeTimeout:  5 * time.Second,
        ResponseHeaderTimeout: 10 * time.Second,
    },
}
```

打许多不同 host 的爬虫，关掉 keep-alive 避免堆积空闲连接：

```go
crawlerClient := &http.Client{
    Transport: &http.Transport{DisableKeepAlives: true},
}
```

### 超时

零值 `http.Client` 和 `http.Server` 没有任何超时。慢或对端恶意的 peer 会无限期占住连接，耗尽文件描述符和内存：

```go
// 服务端——永远设置超时，防 Slowloris 攻击
server := &http.Server{
    Addr:         ":8080",
    Handler:      handler,
    ReadTimeout:  5 * time.Second,
    WriteTimeout: 10 * time.Second,
    IdleTimeout:  120 * time.Second,
}
```

### 排空响应体以复用连接

响应体被完整读完后连接才会归还池。即使不需要 body，也要排空：

```go
resp, err := client.Get(url)
if err != nil { return err }
defer resp.Body.Close()
_, _ = io.Copy(io.Discard, resp.Body) // 排空以复用连接
```

## 流式与缓冲

**诊断：**1- `go tool pprof -inuse_space`——找 `io.ReadAll`、`bytes.Buffer.Grow` 或 `json.Unmarshal` 产生的大块单次分配（MB 级）；它们说明在缓冲整个载荷而不是流式处理

### 大载荷避免 io.ReadAll

`io.ReadAll` 把整个流装进内存。大文件或 HTTP 响应会造成巨大的内存尖峰：

```go
// 差——2GB 文件 = 2GB 分配
data, _ := io.ReadAll(f)

// 好——逐行处理，O(1) 内存
scanner := bufio.NewScanner(f)
for scanner.Scan() { processLine(scanner.Bytes()) }

// 好——reader 与 writer 之间流式传输（内部 32KB 缓冲）
io.Copy(w, resp.Body)
```

小而已知边界的载荷（< 1MB）用 `io.ReadAll` 没问题。

### 流式 JSON

大 JSON 载荷用 `json.NewDecoder` 代替 `json.Unmarshal`（后者缓冲整个 body）：

```go
dec := json.NewDecoder(r)
for dec.More() {
    var item Item
    if err := dec.Decode(&item); err != nil { return err }
    process(item) // 一次处理一条
}
```

## JSON 性能

**诊断：**1- `go tool pprof`（CPU profile）——找 `encoding/json.(*Decoder).Decode`、`reflect.Value.*` 或 `encoding/json.Marshal` 消耗大量 CPU；它们说明基于反射的 JSON 是瓶颈 2- `go test -bench -benchmem`——测 marshal/unmarshal 的 ns/op 与 allocs/op；反射带来高分配数是预期；代码生成方案应少 2-5 倍分配

标准库 `encoding/json` 在运行时用反射检查结构体字段。高吞吐服务里这带来明显的 CPU 与分配开销。

**更快的 JSON 选项：**

- **自定义 `MarshalJSON`/`UnmarshalJSON`**——给热路径类型手写方法，消除反射
- **代码生成库**——`easyjson`、`ffjson` 在构建期生成 marshal/unmarshal 方法，运行时零反射
- **直接替换**——`github.com/goccy/go-json`、`github.com/json-iterator/go`、`github.com/bytedance/sonic` 性能好 2-5 倍
- **`encoding/json/v2`**（Go 1.27 起为默认 JSON 实现；Go 1.25 以 `GOEXPERIMENT=jsonv2` 实验引入）——迁移要审慎：它比 v1 严格（拒绝重复对象键与非法 UTF-8），上热路径前先用真实载荷重跑测试

用第三方 JSON 库时，API 签名以该库官方文档为准。

## Cgo 开销

**诊断：**1- `go tool pprof`（CPU profile + threadcreate profile）——找 `runtime.cgocall` 或 `runtime.asmcgocall` 消耗 CPU；threadcreate 数高说明 cgo 调用把 goroutine 钉在 OS 线程上 2- `go test -bench`——对比 cgo 调用循环与纯 Go 等价物的基准；预期每次 cgo 跨界约 50-100ns 开销

每次经 cgo 从 Go 进 C 花约 50-100ns，代价来自栈切换、信号掩码操作与调度器协调：

```go
// 差——紧凑循环里逐元素吃 cgo 开销
for i, v := range values {
    values[i] = float64(C.sqrt(C.double(v))) // 每次调用约 100ns 开销
}

// 好——用纯 Go 标准库（math.Sqrt 与 C 一样快且可内联）
for i, v := range values { values[i] = math.Sqrt(v) }

// 好——C 代码不可避免时走批量
C.batch_sqrt((*C.double)(&values[0]), C.int(len(values))) // 摊薄开销
```

cgo 的额外代价：goroutine 被钉在 OS 线程上、C 代码不可被抢占（可能拖延 GC）、边界处无法内联。

## 缓冲 I/O

**诊断：**1- `go test -bench`——对比缓冲与无缓冲 I/O 的基准；减少系统调用次数预期带来 3-10 倍提升 2- `go tool trace`——找密集连续的短系统调用（`pread`、`pwrite`）；大量碎小 I/O 操作说明是无缓冲访问

无缓冲的文件读写每个操作一次系统调用。`bufio.Reader` 与 `bufio.Writer` 把小操作攒批，系统调用减少 10 倍以上：

```go
// 差——每行一次系统调用
for _, line := range lines { f.WriteString(line + "\n") }

// 好——带缓冲，把写攒成更大的块
w := bufio.NewWriter(f)
for _, line := range lines { w.WriteString(line + "\n") }
w.Flush()
```

## 并发多阶段流水线

**诊断：**1- `go tool trace`——可视化各阶段的资源利用；找顺序执行中 CPU、磁盘或网络闲置而另一资源忙碌的空隙 2- `go tool pprof`（CPU + goroutine profile）——确认每个阶段饱和的是不同资源；多个阶段争同一资源（如都是 CPU 受限）时，并发没有帮助

少见的场景里，流水线每个阶段饱和不同资源（CPU、磁盘 I/O、网络），把阶段并发而非顺序执行能提高吞吐——即使阶段间有攒批。

### 特殊场景

想象处理记录：阶段 A 做压缩（CPU 受限），阶段 B 写磁盘（I/O 受限），阶段 C 上传网络（网络受限）。顺序执行浪费资源：

```
Time:    0       10      20      30      40      50
CPU:     AAAAAAAAAA|..........|..........|..........|
Disk:    ..........|BBBBBBBBBB|..........|..........|
Network: ..........|..........|CCCCCCCCCC|..........|
```

阶段并发让各资源并行工作：

```
Time:    0       10      20      30      40      50
CPU:     AAAAAAAAAA|AA........|
Disk:    ..........|BBBBBBBBBB|BB........|
Network: ..........|..........|CCCCCCCCCC|CC........|
```

**代码模式：**

```go
// 每个阶段跑在自己的 goroutine，channel 缓冲定界
compressedCh := make(chan []byte, 100)    // A → B 缓冲
uploadedCh := make(chan bool, 100)        // B → C 缓冲

// 阶段 A：CPU 受限压缩
go func() {
    for record := range inputCh {
        compressed := compress(record)    // 吃满 CPU
        compressedCh <- compressed
    }
    close(compressedCh)
}()

// 阶段 B：I/O 受限写盘
go func() {
    for compressed := range compressedCh {
        diskFile.Write(compressed)        // 吃满磁盘 I/O
        uploadedCh <- true
    }
    close(uploadedCh)
}()

// 阶段 C：网络受限上传
go func() {
    for <-uploadedCh {
        client.Post(uploadURL, ...)       // 吃满网络
    }
}()
```

每阶段攒批时，总吞吐 = min(A_throughput, B_throughput, C_throughput)。不并发时，吞吐 = 各阶段顺序相加。**只有瓶颈不重叠时并发阶段才有帮助。**

### 何时使用（以及何时不用）

**只在以下全部成立时才用并发流水线：**

1. **资源饱和可预测且不重叠**——你实测过 A 饱和一种资源（如 CPU = 95%）、B 饱和另一种（磁盘 I/O = 90%）、C 饱和第三种（网络 = 85%）。饱和重叠意味着并发无收益。
2. **瓶颈转移不伤延迟**——处理顺序无所谓，或记录可以乱序流过各阶段。
3. **缓冲开销可接受**——阶段间 channel 占内存。大记录下 channel 缓冲可能顶破系统限制。
4. **已对比过替代方案的基准**——顺序版与并发版都 profile。顺序 + 攒批常常更好，因为更简单、没有上下文切换开销。

**以下情况避免并发流水线：**

- **记录必须保序**——并发处理可能重排记录；下游要求顺序时，所需的同步会抹掉加速收益。
- **资源重叠**——A 和 B 都争 CPU（如都做压缩）时，并发只带来上下文切换开销，没有资源利用收益。
- **延迟比吞吐重要**——单条记录现在并行穿过 3 个阶段，单条延迟反而上升。
- **内存紧张**——每个阶段的 channel 缓冲都是内存预算；深缓冲 channel 能耗尽可用 RAM。

→ 见 `samber/cc-skills-golang@golang-concurrency` skill：channel 模式细节与何时该用 worker pool。

## 批量操作

**诊断：**1- `go test -bench`——对比单条与批量操作的基准；摊薄每操作开销（系统调用、往返）后吞吐预期有 N 倍提升 2- `go tool trace`——找反复出现、之间有空隙的短网络/磁盘操作；这些空隙就是攒批能消掉的浪费往返时间

攒批把每操作开销（系统调用、网络往返、事务成本）摊到多条数据上。这个模式到处适用：I/O、数据库、网络、甚至内存处理。

### 数据库：批量插入代替逐行

逐行插 1,000 行意味着 1,000 次往返、1,000 次查询解析、1,000 次事务提交。一次批量插入一个往返搞定：

```go
// 差——1,000 次往返，约 500ms
for _, user := range users {
    db.Exec("INSERT INTO users (name, email) VALUES ($1, $2)", user.Name, user.Email)
}

// 好——多行 VALUES 一次往返，约 5ms
const batchSize = 1000
for i := 0; i < len(users); i += batchSize {
    end := min(i+batchSize, len(users))
    batch := users[i:end]
    // 构造多行 INSERT 或用 COPY 协议
    tx, _ := db.Begin()
    stmt, _ := tx.Prepare(pq.CopyIn("users", "name", "email"))
    for _, u := range batch { stmt.Exec(u.Name, u.Email) }
    stmt.Exec()
    tx.Commit()
}
```

→ 见 `samber/cc-skills-golang@golang-database` skill：批处理模式与连接池配置细节。

### HTTP：批量 API 调用

API 支持时，一次请求带 N 条数据，代替 N 次单独请求：

```go
// 差——100 次 HTTP 往返
for _, id := range ids {
    resp, _ := client.Get(fmt.Sprintf("/api/users/%s", id))
    // ...
}

// 好——一次 HTTP 请求带上全部 ID
resp, _ := client.Post("/api/users/batch", "application/json",
    bytes.NewReader(marshalIDs(ids)))
```

### Channel：流的批量处理

从 channel 攒数据再批量处理，摊薄单条开销：

```go
func batchProcessor(in <-chan Item, batchSize int) {
    batch := make([]Item, 0, batchSize)
    ticker := time.NewTicker(100 * time.Millisecond) // 超时也冲刷
    defer ticker.Stop()
    for {
        select {
        case item, ok := <-in:
            if !ok { flush(batch); return }
            batch = append(batch, item)
            if len(batch) >= batchSize { flush(batch); batch = batch[:0] }
        case <-ticker.C:
            if len(batch) > 0 { flush(batch); batch = batch[:0] }
        }
    }
}
```
