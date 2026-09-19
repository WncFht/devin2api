# Sync 原语深入

## 目录

- [sync.Mutex](#syncmutex)
    - [内嵌约定](#内嵌约定)
- [sync.RWMutex](#syncrwmutex)
- [sync/atomic](#syncatomic)
- [sync.Map](#syncmap)
- [sync.Pool](#syncpool)
- [sync.Once](#synconce)
- [sync.WaitGroup](#syncwaitgroup)
    - [Go 1.25+: `wg.Go`](#go-125wggo)
    - [Go <1.25 回退](#go-125-回退)
- [golang.org/x/sync/singleflight](#golangorgxsyncsingleflight)
- [golang.org/x/sync/errgroup](#golangorgxsyncerrgroup)
    - [用 SetLimit 做有界并发](#用-setlimit-做有界并发)

## sync.Mutex

以独占访问保护共享状态。持锁时间必须尽可能短——绝不跨 I/O、网络调用或 channel 操作持有 mutex。

```go
type SafeCache struct {
    mu    sync.Mutex
    items map[string]string
}

func (c *SafeCache) Get(key string) (string, bool) {
    c.mu.Lock()
    defer c.mu.Unlock()
    v, ok := c.items[key]
    return v, ok
}

func (c *SafeCache) Set(key, value string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.items[key] = value
}
```

### 内嵌约定

把 mutex 作为非导出字段内嵌，直接放在它保护的字段上方：

```go
type Registry struct {
    mu      sync.Mutex // 保护 entries
    entries map[string]Entry
}
```

## sync.RWMutex

读远多于写时应使用。多个 goroutine 可同时持有 `RLock`；`Lock` 是独占的。

```go
type Config struct {
    mu     sync.RWMutex
    values map[string]string
}

func (c *Config) Get(key string) string {
    c.mu.RLock()
    defer c.mu.RUnlock()
    return c.values[key]
}

func (c *Config) Set(key, value string) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.values[key] = value
}
```

**陷阱**：不要把 RLock 升级成 Lock——这会死锁。先放 RLock，再取 Lock。

## sync/atomic

简单值的无锁操作。简单计数器操作应优先于 Mutex。低竞争的计数器与标志位上比 mutex 更快。

```go
// ✓ 好——简单计数器用 atomic
var requestCount atomic.Int64

func handleRequest() {
    requestCount.Add(1)
}

func getCount() int64 {
    return requestCount.Load()
}
```

```go
// ✓ 好——关停标志用 atomic.Bool
var shuttingDown atomic.Bool

func shutdown() {
    shuttingDown.Store(true)
}

func isRunning() bool {
    return !shuttingDown.Load()
}
```

Go 1.19+ 提供类型化 atomic（`atomic.Int64`、`atomic.Bool`、`atomic.Pointer[T]`）——优先用它们，不用裸 `atomic.AddInt64`/`atomic.LoadInt64`。

## sync.Map

只该用于写一次读多次的场景。为两种常见场景优化：(1) key 写一次读多次；(2) 多个 goroutine 读写不相交的 key 集合。其它场景下普通 `map` + `sync.RWMutex` 更快。

```go
var cache sync.Map

func Get(key string) (any, bool) {
    return cache.Load(key)
}

func Set(key string, value any) {
    cache.Store(key, value)
}

func GetOrSet(key string, compute func() any) any {
    if v, ok := cache.Load(key); ok {
        return v
    }
    v, _ := cache.LoadOrStore(key, compute())
    return v
}
```

**何时不用 `sync.Map`**：需要遍历、取长度，或写频繁且 key 大量重叠时。改用 `sync.RWMutex` + `map`。

## sync.Pool

复用临时对象降低 GC 压力。不得存放指向栈分配对象的指针。池里的对象在任何 GC 周期都可能被回收——不要存持久状态。

```go
var bufPool = sync.Pool{
    New: func() any {
        return new(bytes.Buffer)
    },
}

func process(data []byte) string {
    buf := bufPool.Get().(*bytes.Buffer)
    defer func() {
        buf.Reset()
        bufPool.Put(buf)
    }()

    buf.Write(data)
    // ... 变换 ...
    return buf.String()
}
```

**规则**：

- `Put()` 前永远 `Reset()`——还回脏对象会出 bug
- 别假设 `Get()` 拿到的是零值对象——`New` 只在池空时才跑
- 最适合短命、频繁分配的对象（buffer、编码器、临时 struct）

## sync.Once

一次性初始化必须用它。无论多少 goroutine 并发调用，都只执行一次。设计上即线程安全。

```go
type DBClient struct {
    initOnce  sync.Once
    closeOnce sync.Once
    conn      *sql.DB
}

func (c *DBClient) getConn() *sql.DB {
    c.initOnce.Do(func() {
        var err error
        c.conn, err = sql.Open("postgres", dsn)
        if err != nil {
            panic(fmt.Sprintf("db init: %v", err))
        }
    })
    return c.conn
}

func (c *DBClient) Close() error {
    var err error
    c.closeOnce.Do(func() {
        err = c.conn.Close()
    })
    return err
}
```

Go 1.21+ 还提供 `sync.OnceFunc`、`sync.OnceValue`、`sync.OnceValues`，覆盖更简单的场景：

```go
var loadConfig = sync.OnceValue(func() *Config {
    cfg, err := parseConfig("config.yaml")
    if err != nil {
        panic(err)
    }
    return cfg
})

// 用法：cfg := loadConfig()
```

## sync.WaitGroup

只需要等一组 goroutine 结束时用 `sync.WaitGroup`。

### Go 1.25+:`wg.Go`

`WaitGroup.Go` 启动一个 goroutine，把它加入组，函数返回时把它移出组。

```go
func processAll(items []Item) {
    var wg sync.WaitGroup

    for _, item := range items {
        // 模块声明 `go 1.22+` 时，循环变量按迭代独立。
        // 现代模块里不要专为闭包捕获加 `item := item`。
        wg.Go(func() {
            process(item)
        })
    }

    wg.Wait()
}
```

规则：

- `WaitGroup.Go` 是 Go 1.25+，不是 Go 1.24。
- 传给 `wg.Go` 的函数不许 panic。
- `WaitGroup` 不传播错误，也不取消兄弟任务。
- 要首错即返、取消、并发上限或返回值，用 `golang.org/x/sync/errgroup`。

**`wg.Go()` 的好处**：

- 不用手动记 `Add`/`Done` 账
- `Add`/`Wait` 顺序错误的风险更低
- 简单发后等待任务的 API 更干净

**何时用**：Go 1.25+ 项目里必须全部跑完、不返回错误、不需要取消、不会 panic 的简单 goroutine。任务返回错误、需要取消、限并发或首错行为时用 `errgroup`。

### Go <1.25 回退

```go
func processAll(ctx context.Context, items []Item) {
    var wg sync.WaitGroup
    for _, item := range items {
        wg.Add(1) // Add 在 go 之前
        go func(item Item) {
            defer wg.Done()
            process(ctx, item)
        }(item)
    }
    wg.Wait() // 阻塞到全部 goroutine 结束
}
```

```go
// ✗ 坏——Add 写在 goroutine 里（竞争：Wait 可能在 Add 执行前返回）
go func() {
    wg.Add(1)
    defer wg.Done()
    process(item)
}()
```

## golang.org/x/sync/singleflight

对同一 key 的并发调用去重。多个 goroutine 同时请求同一资源时只有一个执行，其余等待并共享结果。

```go
var group singleflight.Group

func GetUser(ctx context.Context, id string) (*User, error) {
    v, err, _ := group.Do(id, func() (any, error) {
        // 对给定 id 只有一个 goroutine 执行这里
        return db.QueryUser(ctx, id)
    })
    if err != nil {
        return nil, err
    }
    return v.(*User), nil
}
```

**用途**：防缓存击穿、昂贵查询去重（DB、API）、限流的外部服务调用。

## golang.org/x/sync/errgroup

带错误传播的 goroutine 组。返回任意 goroutine 的第一个错误。配 `WithContext` 时，首个错误取消其余 goroutine。

```go
func fetchAll(ctx context.Context, urls []string) ([]Response, error) {
    g, ctx := errgroup.WithContext(ctx) // 首错时取消兄弟任务
    results := make([]Response, len(urls))

    for i, url := range urls {
        g.Go(func() error {
            resp, err := fetch(ctx, url)
            if err != nil {
                return fmt.Errorf("fetching %s: %w", url, err)
            }
            results[i] = resp // 安全：每个 goroutine 只写自己的下标
            return nil
        })
    }

    if err := g.Wait(); err != nil {
        return nil, err
    }
    return results, nil
}
```

### 用 SetLimit 做有界并发

应该用 `SetLimit` 限制并发，避免无界起 goroutine。

```go
g, ctx := errgroup.WithContext(ctx)
g.SetLimit(10) // 最多 10 个 goroutine 并发运行

for _, task := range tasks {
    g.Go(func() error {
        return process(ctx, task)
    })
}
return g.Wait()
```

多数场景下它取代手写 worker pool。

→ 高层模式与决策树见 [SKILL.md](../SKILL.md)。
