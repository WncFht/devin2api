# Channel 与 Select 模式

## 目录

- [Goroutine 生命周期](#goroutine-生命周期)
    - [Goroutine 边界的 Panic 恢复](#goroutine-边界的-panic-恢复)
- [Channel 方向](#channel-方向)
- [Channel 关闭](#channel-关闭)
- [缓冲大小](#缓冲大小)
- [用 Select 做非阻塞通信](#用-select-做非阻塞通信)
- [热循环里避免反复 `time.After`](#热循环里避免反复-timeafter)

## Goroutine 生命周期

绝不在不知道它怎么停的情况下启动 goroutine。每个 goroutine 都必须回答：**它怎么停？**

```go
// ✗ 坏——发后不管，无法停止也无法等待
func startWorker() {
    go func() {
        for {
            doWork() // 永远跑下去，关停时泄漏
        }
    }()
}

// ✓ 好——goroutine 响应 context 取消，调用方能等它
func startWorker(ctx context.Context) *sync.WaitGroup {
    var wg sync.WaitGroup
    wg.Add(1)
    go func() {
        defer wg.Done()
        for {
            select {
            case <-ctx.Done():
                return
            default:
                doWork(ctx)
            }
        }
    }()
    return &wg
}
```

### Goroutine 边界的 Panic 恢复

goroutine 里的 panic 会拖垮整个进程。生产代码永远在 goroutine 边界做 recover：

```go
go func() {
    defer func() {
        if r := recover(); r != nil {
            // ...
        }
    }()
    doWork(ctx)
}()
```

## Channel 方向

在函数签名里指明方向，让误用在编译期被挡住：

```go
// ✗ 坏——调用方可能意外关闭只该接收的 channel，或往里发送
func consume(ch chan int) { ... }

// ✓ 好——编译器强制正确用法
func produce(ch chan<- int) { ... } // 只发
func consume(ch <-chan int) { ... } // 只收
```

## Channel 关闭

channel 必须由发送方（生产方）关闭，绝不许接收方关——发送方在 close 后写入会 panic。

```go
// ✓ 好——生产方做完后关闭
func generate(ctx context.Context) <-chan int {
    ch := make(chan int)
    go func() {
        defer close(ch) // 发送方关闭
        for i := 0; ; i++ {
            select {
            case ch <- i:
            case <-ctx.Done():
                return
            }
        }
    }()
    return ch
}
```

## 缓冲大小

| 大小        | 何时用                                                                               |
| ----------- | ------------------------------------------------------------------------------------ |
| 0（无缓冲） | 默认。同步发送方与接收方——需要交接保证时用                                           |
| 1           | 信号 channel（`done := make(chan struct{}, 1)`），或发送方不能因一条待发数据而阻塞时 |
| N > 1       | 只在有实测依据时——注释里写清为什么选 N、缓冲填满会怎样                               |

```go
// ✓ 好——无缓冲用于同步交接
ch := make(chan Result)

// ✓ 好——缓冲 1 用于信号
done := make(chan struct{}, 1)

// ✗ 可疑——随便给的大缓冲掩盖背压问题
// 注释里给出解释。
ch := make(chan Task, 1000) // 为什么 1000？填满了怎么办？
```

## 用 Select 做非阻塞通信

用 `select` 复用 channel 操作，并永远带上 `ctx.Done()` 防 goroutine 泄漏：

```go
func process(ctx context.Context, in <-chan Task, out chan<- Result) {
    for {
        select {
        case <-ctx.Done():
            return
        case task, ok := <-in:
            if !ok {
                return // channel 已关闭
            }
            result := handle(ctx, task)
            select {
            case out <- result:
            case <-ctx.Done():
                return
            }
        }
    }
}
```

## 热循环里避免反复 `time.After`

```go
// ✗ 坏——每次迭代都新建 timer
for {
    select {
    case msg := <-ch:
        handle(msg)
    case <-time.After(5 * time.Second): // 反复分配/抖动
        handleTimeout()
    }
}

// ✓ 好（Go 1.23+）——复用 timer
timer := time.NewTimer(5 * time.Second)
defer timer.Stop()
for {
    select {
    case msg := <-ch:
        timer.Stop()
        timer.Reset(5 * time.Second)
        handle(msg)
    case <-timer.C:
        handleTimeout()
        timer.Reset(5 * time.Second)
    }
}
```

Go <1.23 时，若 `timer.Stop()` 返回 false，`Reset` 前先排空可能残留的旧值。Go 1.23+ 起，`Stop` 返回后再从 `timer.C` 接收保证是阻塞，而不会收到陈旧值。
