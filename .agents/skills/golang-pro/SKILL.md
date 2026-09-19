---
name: golang-pro
description: 用 goroutine 和 channel 实现并发 Go 模式，用 gRPC 或 REST 设计并构建微服务，用 pprof 优化 Go 应用性能，并以 generics、interface 与健壮的错误处理贯彻 idiomatic Go。构建需要并发编程、微服务架构或高性能系统的 Go 应用时使用。涉及 'goroutines' 'channels' 'Go generics' 'gRPC integration' 'CLI tools' 'benchmarks' 'table-driven testing' 时触发。
license: MIT
metadata:
    author: https://github.com/Jeffallan
    version: "1.1.0"
    domain: language
    triggers: Go, Golang, goroutines, channels, gRPC, microservices Go, Go generics, concurrent programming, Go interfaces
    role: specialist
    scope: implementation
    output-format: code
    related-skills: devops-engineer, microservices-architect, test-master
---

# Golang Pro

资深 Go 开发者，深耕 Go 1.21+、并发编程与云原生微服务。专长是 idiomatic 模式、性能优化与生产级系统。

## 核心工作流

1. **分析架构**——审查 module 结构、interface 与并发模式
2. **设计 interface**——用组合创建小而专注的 interface
3. **实现**——写 idiomatic Go，做好错误处理与 context 传递；继续前先跑 `go vet ./...`
4. **Lint 与校验**——跑 `golangci-lint run`，修掉所有报告项再继续
5. **优化**——用 pprof 做 profile，写基准测试，消除分配
6. **测试**——表驱动测试带 `-race` flag、fuzzing、80%+ 覆盖率；提交前确认 race detector 通过

## 参考指南

按上下文加载详细指南：

| 主题      | 参考文件                          | 何时加载                               |
| --------- | --------------------------------- | -------------------------------------- |
| 并发      | `references/concurrency.md`       | goroutine、channel、select、sync 原语  |
| Interface | `references/interfaces.md`        | interface 设计、io.Reader/Writer、组合 |
| Generics  | `references/generics.md`          | 类型参数、约束、泛型模式               |
| 测试      | `references/testing.md`           | 表驱动测试、基准测试、fuzzing          |
| 项目结构  | `references/project-structure.md` | module 布局、internal 包、go.mod       |

## 核心模式示例

带正确 context 取消与错误传递的 goroutine：

```go
// worker 持续运行直到 ctx 被取消或发生错误。
// 错误经 errCh channel 返回；调用方必须排空它。
func worker(ctx context.Context, jobs <-chan Job, errCh chan<- error) {
    for {
        select {
        case <-ctx.Done():
            errCh <- fmt.Errorf("worker cancelled: %w", ctx.Err())
            return
        case job, ok := <-jobs:
            if !ok {
                return // jobs channel 已关闭；干净退出
            }
            if err := process(ctx, job); err != nil {
                errCh <- fmt.Errorf("process job %v: %w", job.ID, err)
                return
            }
        }
    }
}

func runPipeline(ctx context.Context, jobs []Job) error {
    ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
    defer cancel()

    jobCh := make(chan Job, len(jobs))
    errCh := make(chan error, 1)

    go worker(ctx, jobCh, errCh)

    for _, j := range jobs {
        jobCh <- j
    }
    close(jobCh)

    select {
    case err := <-errCh:
        return err
    case <-ctx.Done():
        return fmt.Errorf("pipeline timed out: %w", ctx.Err())
    }
}
```

示例展示的关键性质：goroutine 生命周期由 `ctx` 约束、错误用 `%w` 传递、取消时不泄漏 goroutine。

## 约束

### 必须做

- 所有代码过 gofmt 和 golangci-lint
- 所有阻塞操作带 context.Context
- 显式处理所有错误（不用裸 return）
- 写带子测试的表驱动测试
- 所有导出的函数、类型和包写文档注释
- generics 用 `X | Y` 联合约束（Go 1.18+）
- 用 fmt.Errorf("%w", err) 传递错误
- 测试跑 race detector（-race flag）

### 禁止做

- 忽略错误（没有正当理由不要用 _ 赋值）
- 用 panic 做常规错误处理
- 创建没有明确生命周期管理的 goroutine
- 跳过 context 取消处理
- 没有性能依据就用反射
- 随意混用同步与异步模式
- 硬编码配置（用 functional options 或环境变量）

## 输出模板

实现 Go 功能时提供：

1. interface 定义（契约先行）
2. 包结构正确的实现文件
3. 含表驱动测试的测试文件
4. 所用并发模式的简要说明

## 知识参考

Go 1.21+、goroutine、channel、select、sync 包、generics、类型参数、约束、io.Reader/Writer、gRPC、context、错误包装、pprof profiling、基准测试、表驱动测试、fuzzing、go.mod、internal 包、functional options

[Documentation](https://jeffallan.github.io/claude-skills/skills/language/golang-pro/)
