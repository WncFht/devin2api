# 诊断工具

## 目录

- [Runtime 诊断（GODEBUG）](#runtime-诊断godebug)
    - [Go 文档命令](#go-文档命令)
    - [GC 追踪](#gc-追踪)
    - [调度器追踪](#调度器追踪)
    - [GOTRACEBACK](#gotraceback)
- [Delve 调试器](#delve-调试器)
    - [安装](#安装)
    - [基本用法](#基本用法)
    - [常用命令](#常用命令)
    - [IDE 集成](#ide-集成)
- [高级分析](#高级分析)

## Runtime 诊断（GODEBUG）

### Go 文档命令

用 `go doc`，不要用 `go tool doc`。Go 1.26 移除了旧的 `cmd/doc` / `go tool doc` 路径。Go 1.27 加了 `package@version` 查询（`go doc golang.org/x/tools/cmd/stringer@v0.30.0`）和列出可执行示例的 `-ex` flag。

### GC 追踪

```bash
GODEBUG=gctrace=1 ./app
```

**输出：**

```
gc 123 @45.67s 4%: 0.8+10+0.3 ms clock, 6+5/10/0 ms cpu, 512->300->150 MB
```

| 字段             | 含义                                 |
| ---------------- | ------------------------------------ |
| 4%               | GC CPU 开销（>10% 说明分配过多）     |
| 512->300->150 MB | GC 开始时堆 -> GC 结束时堆 -> 存活堆 |
| 大停顿           | 分配风暴                             |

### 调度器追踪

```bash
GODEBUG=schedtrace=1000,scheddetail=1 ./app
```

| 信号                 | 含义                       |
| -------------------- | -------------------------- |
| runqueue 高          | CPU 饱和，goroutine 在排队 |
| idleprocs=0          | 满载，到容量上限           |
| spinningthreads      | 锁竞争                     |
| threads > gomaxprocs | 阻塞型 syscall             |

### GOTRACEBACK

panic 时拿完整堆栈：

```bash
GOTRACEBACK=all ./app
```

| 级别     | 显示                         |
| -------- | ---------------------------- |
| `none`   | 无堆栈                       |
| `single` | 仅当前 goroutine（默认）     |
| `all`    | 所有 goroutine（查死锁有用） |
| `system` | 所有 goroutine + runtime 帧  |

---

## Delve 调试器

### 安装

```bash
go install github.com/go-delve/delve/cmd/dlv@latest
```

### 基本用法

```bash
dlv debug ./cmd/myapp          # 调试程序
dlv test ./mypackage           # 调试测试
dlv attach 12345               # attach 到运行中的进程
dlv exec ./myapp -- --flag=v   # 带参数执行二进制
```

### 常用命令

```
break main.main      # 设断点
break file.go:42     # 在某行断
continue             # 继续执行
next                 # 单步跳过（n）
step                 # 单步进入（s）
stepout              # 步出
print variable       # 打印变量
locals               # 打印所有局部变量
args                 # 打印函数参数
goroutines           # 列出所有 goroutine
goroutine 5          # 切到 goroutine 5
stack                # 显示堆栈
```

### IDE 集成

**VS Code:**

```json
// .vscode/launch.json
{
    "version": "0.2.0",
    "configurations": [
        {
            "name": "Launch Package",
            "type": "go",
            "request": "launch",
            "mode": "auto",
            "program": "${workspaceFolder}",
            "env": { "GOTRACEBACK": "all" }
        }
    ]
}
```

**GoLand：**Run -> Edit Configurations -> Go Build。点行号槽设断点。用 Debugger 标签页。

---

## 高级分析

→ 逃逸分析解读、汇编检查与编译器诊断（SSA dump、内联决策）的详细指南见 `golang-benchmark` skill 的 `compiler-analysis.md`；执行 tracer 分析另见其 `trace.md`。
