# 代码评审危险信号

评审时看到这些就标出来：

| 模式                                | 为什么坏                                                |
| ----------------------------------- | ------------------------------------------------------- |
| `result, _ := doSomething()`        | 静默吞错误——日后灵异 bug                                |
| `go func() { }()` 不带 context      | 取消不了，泄漏 goroutine                                |
| Channel 没有 close                  | 发送方退出后 goroutine 泄漏                             |
| 热循环里的 `time.After`             | 反复分配/churn timer；需要 reset 语义时该用可复用 timer |
| 全局 map 没有 mutex                 | 数据竞争                                                |
| 热循环里的 `defer`                  | defer 的调用堆到函数返回才跑                            |
| 热路径上的 `json.Marshal`           | 贵，制造 GC 压力                                        |
| `for range` 不查 `ok`               | 错过 channel 关闭                                       |
| `var err *MyError; return err`      | 接口 nil 陷阱                                           |
| `http.Get` 不设超时                 | 默认 client 没有超时                                    |
| `fmt.Errorf("...: %v", err)`        | 用 `%w` 保留错误链                                      |
| `:=` 遮蔽外层 `err`                 | 内层 err 是新变量，外层一直 nil                         |
| `func (c Counter) Lock()`           | 值接收者拷贝 sync 类型                                  |
| goroutine 内调 `wg.Add(1)`          | 竞态：Wait() 可能在 Add() 前返回                        |
| `http.Error(...)` 后缺 return       | 错误后 handler 继续执行                                 |
| 枚举 `iota` 从 0 开始               | 零值与第一个常量分不开                                  |
| `strings.Trim(s, "prefix")`         | 剥的是字符集合，不是子串                                |
| 有 defer 的函数里 `log.Fatal(err)`  | `os.Exit` 跳过全部 defer 清理                           |
| `time.Time` 用 `t1 == t2`           | 用 `.Equal()`——单调时钟不一样                           |
| `rows, _ := db.Query(...)` 不 Close | 泄漏数据库连接                                          |
| `close(ch)` 后 `ch <- val`          | panic——只有发送方该关                                   |
| 循环里 `select { default: }`        | 忙循环——不阻塞地烧 CPU                                  |
| `int32(bigInt64)`                   | 静默截断——不查溢出                                      |
| `filepath.Join(base, userInput)`    | 防不住 `../` 路径穿越                                   |
| handler 里 `regexp.MustCompile`     | 每次调用都重编译——挪到包级变量                          |
| switch 里 `fallthrough`             | 无条件执行下一个 case                                   |
