# 编译器分析参考

Go 编译器提供一组诊断 flag，能揭示优化决策——escape analysis、内联、SSA 中间表示与生成的汇编。它们对理解函数**为什么**发生分配、编译器**为什么**不肯内联它是必需的。

当 pprof 指出某个热点函数、而你需要理解编译器对该函数做的决策时，用编译器诊断。这些工具是免费的（无运行时开销）——它们在编译期分析。

## 目录

- [Escape Analysis](#escape-analysis)
    - [命令](#命令)
    - [读懂输出](#读懂输出)
    - [常见逃逸原因](#常见逃逸原因)
- [内联决策](#内联决策)
    - [命令](#命令-1)
    - [读懂输出](#读懂输出-1)
    - [常见内联阻碍](#常见内联阻碍)
- [SSA Dump](#ssa-dump)
    - [命令](#命令-2)
    - [读懂 ssa.html](#读懂-ssahtml)
- [汇编输出](#汇编输出)
    - [命令](#命令-3)
    - [读懂汇编输出](#读懂汇编输出)
    - [优化前后对比汇编](#优化前后对比汇编)

## Escape Analysis

Escape analysis 决定一个变量能活在栈上（便宜——函数返回即释放）还是必须分配在堆上（昂贵——要 GC 管）。「moved to heap」意味着编译器判定该变量可能比函数活得更久。

### 命令

```bash
# 显示逃逸决策——每个逃逸变量一行
go build -gcflags="-m" ./... 2>&1 | grep "escapes to heap"
go build -gcflags="-m" ./... 2>&1 | grep "moved to heap"

# 详细模式——显示每个逃逸决策的原因
go build -gcflags="-m -m" ./...

# 过滤到指定包
go build -gcflags="-m" ./pkg/parser 2>&1 | grep "escapes"

# 过滤到指定文件
go build -gcflags="-m" ./pkg/parser/parse.go 2>&1

# 对全部依赖也启用（通常太吵，但调试时有用）
go build -gcflags="all=-m" ./...

# 配合 grep 看某个函数
go build -gcflags="-m" ./pkg/parser 2>&1 | grep "Parse"

# 配合 grep 看哪些留在栈上（不逃逸）
go build -gcflags="-m" ./pkg/parser 2>&1 | grep "does not escape"
```

### 读懂输出

```
./pkg/parser/parse.go:15:6: can inline Parse
./pkg/parser/parse.go:42:13: &result escapes to heap
./pkg/parser/parse.go:42:13:   flow: ~r0 = &result:
./pkg/parser/parse.go:42:13:     from &result (address-of) at ./pkg/parser/parse.go:42:13
./pkg/parser/parse.go:42:13:     from return &result (return) at ./pkg/parser/parse.go:42:6
```

`-m -m`（详细）输出显示**逃逸链**——编译器判定变量逃逸的原因。本例中：`result` 被取地址（`&result`），而这个指针被返回，所以 `result` 必须活得比函数久——它逃逸到堆上。

### 常见逃逸原因

| 原因                         | 示例                                  | 为什么逃逸                                           |
| ---------------------------- | ------------------------------------- | ---------------------------------------------------- |
| **返回局部变量的指针**       | `return &result`                      | 局部变量必须比函数调用活得久——调用方持有引用         |
| **接口装箱**                 | `var x any = myStruct`                | 具体类型装进 `interface{}` 时在堆上分配一份拷贝      |
| **闭包捕获局部变量**         | `go func() { use(localVar) }()`       | goroutine 可能在外层函数返回后才运行                 |
| **slice append 超容量**      | `len == cap` 时 `s = append(s, item)` | 触发一次新的底层数组堆分配                           |
| **把指针传给无法分析的函数** | `json.Marshal(&data)`                 | 编译器无法证明指针不会跨包边界被保留                 |
| **存进会逃逸的结构体字段**   | `obj.Field = &local`                  | `obj` 若在堆上分配，它指向的一切也得在堆上           |
| **fmt.Sprintf 一家**         | `fmt.Sprintf("%d", n)`                | 参数被装箱进 `any`（接口装箱）+ 结果字符串在堆上分配 |
| **在 channel 上发指针**      | `ch <- &data`                         | channel 接收方可能是生命周期不同的另一个 goroutine   |

**不是所有逃逸都是问题。** 只排查 pprof 标记为分配大户的函数里的逃逸。启动时只调一次的函数随便逃逸。

## 内联决策

内联把函数调用替换为调用点处的函数体。它消除调用开销，并让更多优化成为可能（escape analysis 变准、死代码消除、常量折叠）。热路径上没被内联的函数可能值得简化。

### 命令

```bash
# 显示哪些函数可以被内联
go build -gcflags="-m" ./... 2>&1 | grep "can inline"

# 显示哪些函数不能被内联（带原因）
go build -gcflags="-m" ./... 2>&1 | grep "cannot inline"

# 显示某个包的内联决策
go build -gcflags="-m" ./pkg/handler 2>&1 | grep "inline"

# 显示内联实际应用的位置（函数被内联进了调用方）
go build -gcflags="-m" ./... 2>&1 | grep "inlining call to"

# 详细模式——显示成本预算与内联被阻止的原因
go build -gcflags="-m -m" ./... 2>&1 | grep "inline"

# 过滤到指定函数
go build -gcflags="-m" ./pkg/handler 2>&1 | grep "HandleRequest"

# 内联与 escape analysis 一起看（两者相互作用）
go build -gcflags="-m" ./pkg/handler 2>&1 | grep -E "(inline|escape|moved to heap)"
```

### 读懂输出

```
./pkg/handler/handler.go:20:6: can inline validateInput
./pkg/handler/handler.go:35:6: cannot inline HandleRequest: function too complex: cost 120 exceeds budget 80
./pkg/handler/handler.go:42:19: inlining call to validateInput
```

内联成本预算约为 80–82 个 AST 节点（Go 1.22+ 时；后续版本有所提高）。成本更高的函数（AST 节点更多、控制流更复杂）不会被内联。实际阈值用 `-gcflags="-m -m"` 查。

### 常见内联阻碍

| 阻碍                           | 为什么阻止内联             | 缓解                                   |
| ------------------------------ | -------------------------- | -------------------------------------- |
| **函数太复杂**                 | 函数体成本超过预算（80）   | 拆成更小的函数；把冷路径摘出去         |
| **`defer` 语句**               | 增加的清理代码让内联变复杂 | 小热函数里去掉 `defer`；直接调清理代码 |
| **`recover()` 调用**           | 强制保留栈帧               | 把 `recover()` 挪到一个 wrapper 函数里 |
| **`go` 语句**                  | goroutine 启动有隐含复杂度 | 把 goroutine 体提取成独立函数          |
| **type switch / 接口方法调用** | 动态分发编译期无法解析     | 热路径用具体类型                       |
| **`select` 语句**              | 复杂的 runtime 交互        | 简化热函数里的 channel 模式            |
| **函数体大**                   | 语句多了成本累加           | 拆小——热内层函数可能就够格内联了       |

**值接收者 vs 指针接收者：** 接收者选择会影响拷贝、别名、escape analysis 与内联，但指针接收者方法同样能内联，值接收者也不保证内联。真实编译器决策用 `-gcflags="-m -m"` 查。

## SSA Dump

SSA（Static Single Assignment）dump 展示编译器在每个优化 pass 之后的中间表示——死代码消除、边界检查消除、常量折叠、寄存器分配。需要精确理解编译器生成什么时用它。

### 命令

```bash
# 为指定函数生成 SSA dump——在当前目录生成 ssa.html
GOSSAFUNC=Parse go build ./pkg/parser
# 浏览器打开 ssa.html——并排显示每个优化 pass

# 为某个类型的方法生成
GOSSAFUNC='(*Parser).Parse' go build ./pkg/parser

# 为指定包中的函数生成（名字撞车时用）
GOSSAFUNC=myapp/pkg/parser.Parse go build ./...

# 配合指定输出目录
GOSSAFUNC=Parse GOSSADIR=/tmp/ssa go build ./pkg/parser
# 生成 /tmp/ssa/ssa.html
```

### 读懂 ssa.html

HTML 文件展示函数代码在每个编译 pass 后的形态：

1. **Source**——原始 Go 代码
2. **AST**——抽象语法树
3. **Start**——初始 SSA 形态
4. **Opt**——优化 pass 之后（死代码、常量传播、边界检查消除）
5. **Lower**——架构相关降级
6. **Regalloc**——寄存器分配之后
7. **Genssa**——最终生成的代码

在任意 pass 中点击一个值，可跨所有 pass 高亮它——看编译器如何变换它。红色的值被消除了（死代码）。绿色的值是新产生的（某个 pass 引入的）。

**看什么：**

- **残留的边界检查**——没被消掉的 `IsInBounds` 或 `IsSliceInBounds` 操作。加显式边界检查或用 `_ = s[n-1]` 提示可能有帮助
- **没消掉的死代码**——算了但没用的值（本应被消掉；没消掉时检查是否有副作用）
- **常量折叠**——常量上的计算应在编译期就求值完
- **寄存器溢出**——寄存器不够用被迫搬到栈上的值；说明寄存器压力大

## 汇编输出

看编译器实际生成的机器码。用于验证 SIMD 指令、边界检查、寄存器分配与微优化决策。

### 命令

```bash
# 整个包的完整汇编输出（非常啰嗦）
go build -gcflags="-S" ./pkg/parser 2>&1 | head -200

# 指定函数的汇编（grep 函数名）
go build -gcflags="-S" ./pkg/parser 2>&1 | grep -A 50 '"".Parse'

# 全部包的汇编（含依赖——非常啰嗦）
go build -gcflags="all=-S" ./... 2>&1 | grep -A 50 'myapp/pkg/parser.Parse'

# 反汇编已编译二进制（-gcflags="-S" 的替代方案）
go build -o myapp ./cmd/server
go tool objdump -s Parse myapp

# 反汇编并交织源码
go tool objdump -S -s Parse myapp

# 反汇编指定符号
go tool objdump -s 'myapp/pkg/parser.Parse' myapp

# 反汇编指定地址区间
go tool objdump -start 0x4a3b00 -end 0x4a3c00 myapp

# 列出二进制里全部符号
go tool nm myapp | grep Parse

# 交叉编译看另一种架构的汇编
GOARCH=arm64 go build -gcflags="-S" ./pkg/parser 2>&1 | head -200
```

### 读懂汇编输出

```asm
"".Parse STEXT size=240 args=0x18 locals=0x48
    0x0000 MOVQ (TLS), CX           ; goroutine 栈检查
    0x0009 LEAQ -64(SP), AX
    0x000e CMPQ AX, 16(CX)          ; 栈溢出检查
    0x0012 JLS  228                  ; 跳到栈扩张
    0x0018 SUBQ $72, SP             ; 分配栈帧
    0x001c MOVQ BP, 64(SP)          ; 保存基址指针
    0x0021 LEAQ 64(SP), BP          ; 设新基址指针
    ; ... 函数体 ...
    0x00e0 CALL runtime.makeslice(SB) ; 堆分配！
```

**看什么：**

- `CALL runtime.makeslice` 或 `CALL runtime.newobject`——热路径上的堆分配
- `CALL runtime.growslice`——slice 超容量，触发拷贝
- `PCDATA` / `FUNCDATA`——GC 元数据（性能分析时忽略）
- 边界检查序列：数组/slice 访问前的 `CMPQ` + `JCC`——有时能消掉
- SIMD 指令：`VMOVDQU`、`VPSHUFB`、`VPADDB` 等——验证自动向量化或手写 SIMD
- `CALL runtime.morestack_noctxt`——栈扩张（正常，但频繁调用说明递归太深）

### 优化前后对比汇编

```bash
# 变更前
go build -gcflags="-S" ./pkg/parser 2>&1 > asm-before.txt

# 变更后
go build -gcflags="-S" ./pkg/parser 2>&1 > asm-after.txt

# diff 汇编
diff asm-before.txt asm-after.txt
```
