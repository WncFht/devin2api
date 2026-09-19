# 并发调试

## 目录

- [Goroutine 泄漏](#goroutine-泄漏)
- [数据竞争](#数据竞争)
- [死锁](#死锁)

## Goroutine 泄漏

**症状：**内存缓慢上涨、goroutine 数量增长、没有明显的 CPU 尖峰。

**诊断：**

用 pprof goroutine profile（见 [pprof.md](./pprof.md)）加 `?debug=2` 拿人可读的输出，然后找卡在 `chan receive` 的 goroutine。

goroutine 泄漏 profile（Go 1.26 里 `GOEXPERIMENT=goroutineleakprofile` 之后的实验特性）自 Go 1.27 起正式可用——不需要 build flag，跟其他标准 profile 一样挂在 `/debug/pprof/goroutineleak`。

```bash
curl http://localhost:6060/debug/pprof/goroutineleak?debug=2
go tool pprof http://localhost:6060/debug/pprof/goroutineleak
```

原有工具继续保留：测试里用 `go.uber.org/goleak`，粗粒度监控用 `runtime.NumGoroutine()`，堆栈 dump 用 `/debug/pprof/goroutine?debug=2`，竞态检查用 `go test -race ./...`。

```go
// 程序化监控——打 goroutine 数来发现泄漏
go func() {
    for {
        log.Printf("goroutines: %d", runtime.NumGoroutine())
        time.Sleep(3 * time.Second)
    }
}()

// 测试里用 goleak 检测 goroutine 泄漏
// import "go.uber.org/goleak"
// func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }
```

**常见原因：**

```go
// 1. channel 没关——goroutine 永远阻塞
// 坏
for {
    job := <-jobs
    process(job)
}
// 好
for {
    select {
    case job, ok := <-jobs:
        if !ok { return }
        process(job)
    case <-ctx.Done():
        return
    }
}

// 2. 忘了关 response body——泄漏 HTTP 连接
// HTTP 调用后永远 defer resp.Body.Close()。
// 正确模式见 production-debug.md。

// 3. 热循环里的 time.After——每次迭代都新分配一个 timer
// 坏
for {
    select {
    case <-time.After(time.Second):
        do()
    }
}
// 好——重复间隔复用 ticker
ticker := time.NewTicker(time.Second)
defer ticker.Stop()
for {
    select {
    case <-ticker.C:
        do()
    case <-ctx.Done():
        return
    }
}
```

## 数据竞争

**症状：**间歇性失败、「时好时坏」、不同机器结果不同。

**诊断：**数据竞争必须用 `-race` flag 测：

```bash
go test -race ./...
go run -race main.go
# 竞态检测器让代码慢 ~10 倍，但能可靠地找出数据竞争
```

**常见模式：**

- 共享 map 没有 mutex
- 共享变量没有 atomic
- 初始化完成前就发布了引用
- go func() 访问外层变量不同步

## 死锁

**症状：**程序挂起、goroutine 卡在 "chan receive" 或 "mutex lock"。

**诊断：**

```bash
curl http://localhost:6060/debug/pprof/goroutine?debug=2

# 或程序化
runtime.Stack(buf, true)
```

**常见模式：**

1. **循环等待**——A 等 B，B 等 A
2. **忘了的 channel 发送**——发送方 goroutine 已经退出
3. **锁顺序错了**——永远按同一顺序拿锁
