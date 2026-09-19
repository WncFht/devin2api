# 流水线与 Worker Pool

## 目录

- [流水线模式](#流水线模式)
- [Fan-Out / Fan-In](#fan-out--fan-in)
- [用 errgroup 做 Worker Pool](#用-errgroup-做-worker-pool)
- [用信号量做有界并发](#用信号量做有界并发)
- [流水线替代方案](#流水线替代方案)
    - [Go 1.23+ 迭代器（range-over-func）](#go-123-迭代器range-over-func)
    - [samber/ro](#samberro)
- [Goroutine 泄漏检测](#goroutine-泄漏检测)
- [流水线常见错误](#流水线常见错误)

## 流水线模式

流水线是用 channel 串起来的一串阶段，每个阶段是一个（或一组）goroutine，它：

1. 从上游 channel 接收值
2. 处理每个值
3. 把结果发到下游 channel

```go
// 阶段 1：生成整数
func generate(ctx context.Context, nums ...int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for _, n := range nums {
            select {
            case out <- n:
            case <-ctx.Done():
                return
            }
        }
    }()
    return out
}

// 阶段 2：对每个整数求平方
func square(ctx context.Context, in <-chan int) <-chan int {
    out := make(chan int)
    go func() {
        defer close(out)
        for n := range in {
            select {
            case out <- n * n:
            case <-ctx.Done():
                return
            }
        }
    }()
    return out
}

// 用法
func main() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    ch := generate(ctx, 2, 3, 4)
    results := square(ctx, ch)

    for v := range results {
        fmt.Println(v) // 4, 9, 16
    }
}
```

**流水线的关键规则**：

- 流水线阶段必须接受并响应 context 取消——每个阶段都要 select `ctx.Done()`，否则提前取消时 goroutine 泄漏
- 生产方（第一个阶段）关闭自己的输出 channel；后续每个阶段关闭自己的输出
- 绝不在流水线阶段里无界起 goroutine
- 除非有实测吞吐需求，否则用无缓冲 channel

## Fan-Out / Fan-In

**Fan-out**：多个 goroutine 读同一个 channel，把 CPU 密集工作并行化。**Fan-in**：多个 channel 合并成一个输出 channel。

```go
// Fan-out：N 个 worker 读同一个输入 channel
func fanOut(ctx context.Context, in <-chan Task, workers int) <-chan Result {
    out := make(chan Result)
    var wg sync.WaitGroup

    for i := 0; i < workers; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for {
                select {
                case task, ok := <-in:
                    if !ok {
                        return
                    }
                    select {
                    case out <- process(ctx, task):
                    case <-ctx.Done():
                        return
                    }
                case <-ctx.Done():
                    return
                }
            }
        }()
    }

    go func() {
        wg.Wait()
        close(out)
    }()
    return out
}
```

```go
// Fan-in：把多个 channel 合并成一个
func fanIn(ctx context.Context, channels ...<-chan Result) <-chan Result {
    out := make(chan Result)
    var wg sync.WaitGroup

    for _, ch := range channels {
        wg.Add(1)
        go func(c <-chan Result) {
            defer wg.Done()
            for v := range c {
                select {
                case out <- v:
                case <-ctx.Done():
                    return
                }
            }
        }(ch)
    }

    go func() {
        wg.Wait()
        close(out)
    }()
    return out
}
```

## 用 errgroup 做 Worker Pool

fan-out worker 应该用 `errgroup.SetLimit` 做有界并发。多数场景下，`errgroup.SetLimit` 取代手写 worker pool：

```go
func processAll(ctx context.Context, tasks []Task) error {
    g, ctx := errgroup.WithContext(ctx)
    g.SetLimit(10) // 最多 10 个并发 worker

    for _, task := range tasks {
        g.Go(func() error {
            return process(ctx, task)
        })
    }
    return g.Wait()
}
```

只在需要以下能力时才手写 worker pool：

- 每 worker 的状态（连接、缓冲）
- 自定义背压或优先级调度
- 优雅排空、在途任务跑完再退

## 用信号量做有界并发

不用 errgroup 但需要细粒度并发控制时：

```go
func processAll(ctx context.Context, items []Item) error {
    sem := make(chan struct{}, 10) // 容量 10 的信号量
    var wg sync.WaitGroup

    for _, item := range items {
        wg.Add(1)
        sem <- struct{}{} // 获取
        go func(item Item) {
            defer wg.Done()
            defer func() { <-sem }() // 释放
            process(ctx, item)
        }(item)
    }
    wg.Wait()
    return nil
}
```

需要错误传播时优先 `errgroup.SetLimit`，不用这个模式。

## 流水线替代方案

### Go 1.23+ 迭代器（range-over-func）

不需要并发的进程内数据变换，用迭代器可以避开 goroutine 与 channel 的开销：

```go
func Filter[T any](seq iter.Seq[T], pred func(T) bool) iter.Seq[T] {
    return func(yield func(T) bool) {
        for v := range seq {
            if pred(v) {
                if !yield(v) {
                    return
                }
            }
        }
    }
}

func Map[T, U any](seq iter.Seq[T], f func(T) U) iter.Seq[U] {
    return func(yield func(U) bool) {
        for v := range seq {
            if !yield(f(v)) {
                return
            }
        }
    }
}
```

何时用迭代器：

- 处理是 CPU 密集、并行无收益
- 想要惰性求值但不想要 goroutine 开销
- 数据源本身就是顺序的（slice、数据库游标）

何时用 goroutine+channel 流水线：

- 阶段涉及 I/O（网络、磁盘），并发有收益
- 需要跨 CPU 核的真并行
- 各阶段吞吐特征不同

### samber/ro

`samber/ro` 为只读集合提供流式、类型安全的流水线 API：

```go
import "github.com/samber/ro"

emails, _ := ro.Collect( // 忽略错误
    ro.Pipe(
        ro.FromSlice(users),
        ro.Filter(func(u User) bool { return u.Active }),
        ro.Map(func(u User) string { return u.Email }),
    ),
)

```

顺序数据变换且受益于流式 API 时用 `samber/ro`。需要时它也支持并行处理。

## Goroutine 泄漏检测

goroutine 泄漏应该在测试里用 goleak 检测。在 `TestMain` 中用 `go.uber.org/goleak` 抓跨全部测试的泄漏 goroutine：

```go
func TestMain(m *testing.M) {
    goleak.VerifyTestMain(m)
}
```

## 流水线常见错误

| 错误                      | 修法                                         |
| ------------------------- | -------------------------------------------- |
| 流水线阶段缺 `ctx.Done()` | 永远在 select 里带上 context 以便取消        |
| 不关闭输出 channel        | 生产方必须 `defer close(out)`                |
| 无界 goroutine 启动       | 用 `errgroup.SetLimit` 或信号量              |
| 经 channel 发送可变数据   | 发送副本或不可变值                           |
| 不带 select 的阻塞发送    | channel 发送包在带 `ctx.Done()` 的 select 里 |

→ sync 原语与 channel 模式见 [sync-primitives.md](sync-primitives.md) 与 [channels-and-select.md](channels-and-select.md)。
