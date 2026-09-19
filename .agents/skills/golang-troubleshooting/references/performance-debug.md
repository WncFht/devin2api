# 性能排障

## CPU Profiling

用 pprof CPU profile 采 30s 样本（命令见 [pprof.md](./pprof.md)），然后用 `top`、`web` 或 `list funcName` 查看。

**常见 CPU 大户：**

1. 热路径上的 JSON marshal/unmarshal——预分配 buffer、换更快的库
2. 关键路径上的反射
3. 不必要的分配——用 sync.Pool
4. 藏在嵌套循环里的 O(n^2)
5. 过多 syscall——批量操作

## 内存 Profiling

用 pprof heap profile（见 [pprof.md](./pprof.md)）。用 `go tool pprof -base heap1.prof heap2.prof` 对比不同时间的堆快照找增长。用逃逸分析（见 [diagnostic-tools.md](./diagnostic-tools.md)）找热路径上非预期的堆分配。

**常见内存泄漏：**

1. 没有淘汰的无界缓存
2. 循环里增长的 slice（忘了重置）
3. 永不清空的全局 map
4. 循环里拼字符串（用 `strings.Builder`）
5. 按值传大结构体

## 锁竞争

**症状：**CPU 高但吞吐低、延迟随负载上升、多核没有帮助。

**在代码里启用 profiling：**

```go
runtime.SetMutexProfileFraction(1)
runtime.SetBlockProfileRate(1)
```

然后用 pprof mutex 和 block profile（见 [pprof.md](./pprof.md)）。

**解法：**

1. 缩小临界区——持锁时间最短化
2. 分片——不同数据用不同的锁
3. `sync.Map`——读多写少的负载
4. `atomic`——简单计数器
5. `RWMutex`——读远多于写时
