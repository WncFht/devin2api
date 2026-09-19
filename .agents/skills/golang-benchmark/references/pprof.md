# pprof 参考

`go tool pprof` 是理解 Go 程序中 CPU 时间、内存与争用去向的主力工具。本文讲怎么**用** CLI 以及怎么**解读**输出。在运行中服务上启用 pprof 端点（net/http/pprof import、认证、安全）→ 见 `golang-troubleshooting` skill。

## 目录

- [Profile 类型](#profile-类型)
    - [alloc_objects 与 alloc_space 怎么选](#alloc_objects-与-alloc_space-怎么选)
    - [inuse_space 与 alloc_space 怎么选](#inuse_space-与-alloc_space-怎么选)
    - [启用 mutex 与 block profile](#启用-mutex-与-block-profile)
- [生成 profile](#生成-profile)
    - [从基准测试生成（不需要 HTTP 服务）](#从基准测试生成不需要-http-服务)
    - [从运行中服务生成](#从运行中服务生成)
    - [从代码生成（编程方式）](#从代码生成编程方式)
- [交互式 CLI 命令](#交互式-cli-命令)
    - [`top`——自身耗时排名（从这里开始）](#top自身耗时排名从这里开始)
    - [`top -cum`——累计耗时排名](#top--cum累计耗时排名)
    - [`list funcName`——带标注的源码](#list-funcname带标注的源码)
    - [`peek funcName`——调用方与被调方](#peek-funcname调用方与被调方)
    - [`tree`——层级调用树](#tree层级调用树)
    - [`traces`——原始栈踪迹](#traces原始栈踪迹)
    - [`web` / `svg`——图形化调用图](#web--svg图形化调用图)
    - [`disasm funcName`——指令级](#disasm-funcname指令级)
    - [`weblist funcName`——浏览器中带标注的源码](#weblist-funcname浏览器中带标注的源码)
    - [`tags`——profile 标签分解](#tagsprofile-标签分解)
    - [`tagroot` 与 `tagleaf`——按标签分组](#tagroot-与-tagleaf按标签分组)
    - [`granularity`——控制分组粒度](#granularity控制分组粒度)
    - [`sort`——改排序方式](#sort改排序方式)
    - [`source`——显示匹配正则的源码](#source显示匹配正则的源码)
    - [`focus`、`ignore`、`hide`、`show`——过滤](#focusignorehideshow过滤)
    - [`normalize`——对基准 profile 归一化](#normalize对基准-profile-归一化)
    - [`sample_index`——在多指标 profile 中切换指标](#sample_index在多指标-profile-中切换指标)
    - [`unit`——改显示单位](#unit改显示单位)
    - [`callgrind`——导出给 KCachegrind](#callgrind导出给-kcachegrind)
    - [`proto`——保存处理后的 profile](#proto保存处理后的-profile)
    - [`help`——列出全部命令](#help列出全部命令)
    - [`show_from=regex`——裁掉匹配之上的调用方](#show_fromregex裁掉匹配之上的调用方)
    - [`noinlines`——压平内联函数](#noinlines压平内联函数)
    - [完整命令参考](#完整命令参考)
- [图形 / Web UI](#图形--web-ui)
- [对比 profile](#对比-profile)
    - [用 `-base` 检测内存泄漏](#用--base-检测内存泄漏)
    - [跨代码版本对比 CPU profile](#跨代码版本对比-cpu-profile)
- [常见模式](#常见模式)
    - [flat 高 + cum 高](#flat-高--cum-高)
    - [flat 低 + cum 高](#flat-低--cum-高)
    - [`alloc_objects` 高、`inuse_space` 低](#alloc_objects-高inuse_space-低)
    - [`inuse_space` 随时间增长](#inuse_space-随时间增长)
    - [Mutex/block profile 热](#mutexblock-profile-热)
    - [大量 goroutine 阻塞在同一 channel/mutex](#大量-goroutine-阻塞在同一-channelmutex)
    - [`runtime.mallocgc` 占据 CPU profile](#runtimemallocgc-占据-cpu-profile)
    - [`runtime.memmove` 在 CPU profile 中偏高](#runtimememmove-在-cpu-profile-中偏高)
    - [`runtime.scanobject` 在 CPU profile 中偏高](#runtimescanobject-在-cpu-profile-中偏高)
- [什么症状用什么 profile](#什么症状用什么-profile)

## Profile 类型

每种 profile 类型回答一个不同的性能问题。选错 profile 类型会浪费排查时间——采集前先把症状对到 profile 上。

| Profile                  | Flag / 端点                                        | 什么时候用                                        | 为什么是它而不是别的                                                                  |
| ------------------------ | -------------------------------------------------- | ------------------------------------------------- | ------------------------------------------------------------------------------------- |
| **CPU**                  | `-cpuprofile` 或 `/debug/pprof/profile?seconds=30` | CPU 占用高、函数慢                                | 以 100Hz 采样哪些函数在 CPU 上；漏掉 off-CPU 时间（I/O、sleep）                       |
| **Heap (alloc_objects)** | `-memprofile` 然后 `pprof -alloc_objects`          | GC 压力大、分配太多                               | 统计分配事件次数不看大小；分配频率与对象搅动是主因时用                                |
| **Heap (alloc_space)**   | `pprof -alloc_space`                               | 按体积找最大分配点                                | 测量总分配字节数；要降峰值内存而非只降 GC 频率时用                                    |
| **Heap (inuse_space)**   | `pprof -inuse_space`                               | 内存随时间上涨、疑似泄漏                          | 显示当前存活的堆对象；对比两个快照定位泄漏源                                          |
| **Heap (inuse_objects)** | `pprof -inuse_objects`                             | 对象数上涨、疑似小对象泄漏                        | 统计存活对象个数不看大小；泄漏是大量小对象、在 inuse_space 里不明显时用               |
| **Goroutine**            | `/debug/pprof/goroutine`                           | I/O 阻塞、goroutine 泄漏、池耗尽                  | 快照全部 goroutine 栈；找在同一调用点堆积的 goroutine                                 |
| **Mutex**                | `/debug/pprof/mutex`                               | goroutine 间锁争用                                | 测量 goroutine 等锁的累计耗时。须先启用：`runtime.SetMutexProfileFraction(5)`         |
| **Block**                | `/debug/pprof/block`                               | goroutine 阻塞在 channel、mutex、timer、select 上 | 测量 goroutine 阻塞在同步原语上的累计耗时。须先启用：`runtime.SetBlockProfileRate(1)` |
| **Threadcreate**         | `/debug/pprof/threadcreate`                        | OS 线程创建过多                                   | 显示创建新 OS 线程的栈踪迹；通常来自 cgo 调用或钉住线程的阻塞型 syscall               |

### alloc_objects 与 alloc_space 怎么选

- **alloc_objects**——「我在哪里分配得最频繁？」——分配频率与对象搅动驱动 GC 工作时用
- **alloc_space**——「我在哪里分配的字节最多？」——要降峰值内存与 RSS 时用
- 实践中先从 `alloc_objects` 开始，因为 GC 搅动是 Go 里最常见的分配类瓶颈。

### inuse_space 与 alloc_space 怎么选

- **alloc_space** 是从程序启动累计的——包含已被 GC 释放的对象
- **inuse_space** 是时间点快照——只有当前存活的对象
- 用 `alloc_space` 找要优化的分配热点。用 `inuse_space` 排查内存泄漏。

### 启用 mutex 与 block profile

这两类 profile 默认关闭，因为有开销。采集前先启用：

```go
import "runtime"

// Mutex profiling：记录的 mutex 争用事件比例。
// 5 表示每 5 个事件记录 1 个。越大开销越小但细节越少。
runtime.SetMutexProfileFraction(5)

// Block profiling：按时间的采样率。
// 1 = 记录全部阻塞事件。更大的值约每 rate 纳秒阻塞采一个事件。
// 调试用 1，生产用更大的值（如 1000000 = 1ms）。
runtime.SetBlockProfileRate(1)
```

排查完关掉以消除开销：

```go
runtime.SetMutexProfileFraction(0)
runtime.SetBlockProfileRate(0)
```

## 生成 profile

### 从基准测试生成（不需要 HTTP 服务）

```bash
# CPU profile——测量基准测试执行期间计算时间花在哪
go test -bench=BenchmarkParse -cpuprofile=cpu.prof ./pkg/parser

# 内存 profile——捕捉基准测试期间的分配模式
go test -bench=BenchmarkParse -memprofile=mem.prof ./pkg/parser

# 两个一起——但注意 CPU profiling 约 5% 开销会带偏内存结果
go test -bench=BenchmarkParse -cpuprofile=cpu.prof -memprofile=mem.prof ./pkg/parser
```

### 从运行中服务生成

需要 `import _ "net/http/pprof"`（安全配置见 `golang-troubleshooting` skill）：

```bash
# CPU profile——采集 30 秒 CPU 样本
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# 堆 profile——快照当前堆状态
go tool pprof -alloc_objects http://localhost:6060/debug/pprof/heap

# Goroutine profile——快照全部 goroutine 栈
go tool pprof http://localhost:6060/debug/pprof/goroutine

# Mutex profile——上次重置以来的争用数据
go tool pprof http://localhost:6060/debug/pprof/mutex

# Block profile——上次重置以来的阻塞数据
go tool pprof http://localhost:6060/debug/pprof/block
```

### 从代码生成（编程方式）

```go
import "runtime/pprof"

// CPU profile
f, _ := os.Create("cpu.prof")
pprof.StartCPUProfile(f)
defer pprof.StopCPUProfile()

// 在某个点做堆快照
f, _ := os.Create("heap.prof")
pprof.WriteHeapProfile(f)
f.Close()

// 具名 profile（goroutine、threadcreate 等）
pprof.Lookup("goroutine").WriteTo(f, 0)
```

## 交互式 CLI 命令

以交互模式打开一个 profile：

```bash
go tool pprof cpu.prof
# 或从 URL：
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30
```

### `top`——自身耗时排名（从这里开始）

第一个要跑的命令。按函数自身花费的时间（或分配）排名：

```
(pprof) top
Showing nodes accounting for 4.2s, 84% of 5s total
      flat  flat%   sum%        cum   cum%
     1.50s 30.00% 30.00%      2.80s 56.00%  encoding/json.Marshal
     0.80s 16.00% 46.00%      0.80s 16.00%  runtime.mallocgc
     0.60s 12.00% 58.00%      0.60s 12.00%  runtime.memmove
     0.50s 10.00% 68.00%      0.50s 10.00%  runtime.scanobject
     0.40s  8.00% 76.00%      1.90s 38.00%  myapp/pkg/parser.Parse
     0.30s  6.00% 82.00%      0.30s  6.00%  syscall.syscall
     0.10s  2.00% 84.00%      0.10s  2.00%  runtime.futex
```

| 列        | 含义                                  | 怎么读                                          |
| --------- | ------------------------------------- | ----------------------------------------------- |
| **flat**  | 花在函数自身的时间，不含被调方        | flat 高 = 函数自己的代码贵                      |
| **flat%** | flat 占总采样时间的百分比             | 快速看相对成本                                  |
| **sum%**  | flat% 自上而下的累计                  | 「前 3 个函数占了总时间的 58%」                 |
| **cum**   | 函数 + 它调用的全部函数的时间（累计） | cum 高 flat 低 = 函数把活儿委派给了昂贵的被调方 |
| **cum%**  | cum 占总时间的百分比                  | 与 flat% 对比——差得大说明成本在被调方           |

**限制输出：**

```
(pprof) top 5              # 只看前 5 个函数
(pprof) top -cum 10        # 按累计时间取前 10
(pprof) top -flat 20       # 按 flat 时间取前 20（默认排序）
```

### `top -cum`——累计耗时排名

当 `top` 显示 runtime 函数（`runtime.mallocgc`、`runtime.memmove`、`runtime.scanobject`）占主导时至关重要。它们是症状不是病因。`top -cum` 揭示是哪些**应用**函数触发了它们：

```
(pprof) top -cum
      flat  flat%   sum%        cum   cum%
     0.40s  8.00%  8.00%      3.80s 76.00%  myapp/pkg/handler.HandleRequest
     0.10s  2.00% 10.00%      2.80s 56.00%  myapp/pkg/handler.serializeResponse
     1.50s 30.00% 40.00%      2.80s 56.00%  encoding/json.Marshal
```

现在能看到 `HandleRequest` → `serializeResponse` → `json.Marshal` 是热路径。优化目标是 `serializeResponse`，不是 `runtime.mallocgc`。

### `list funcName`——带标注的源码

显示函数源码并标注每行成本。用它定位造成瓶颈的**确切行**：

```
(pprof) list serializeResponse
Total: 5s
ROUTINE ======================== myapp/pkg/handler.serializeResponse
     0.10s      2.80s (flat, cum) 56.00% of Total
         .          .     38:func serializeResponse(w http.ResponseWriter, data any) {
         .      0.20s     39:    w.Header().Set("Content-Type", "application/json")
     0.10s      2.60s     40:    buf, err := json.Marshal(data)
         .          .     41:    if err != nil {
         .          .     42:        http.Error(w, err.Error(), 500)
         .          .     43:        return
         .          .     44:    }
         .      0.20s     45:    w.Write(buf)
         .          .     46:}
```

- 左列 = **flat** 时间（这一行自己做的功）
- 右列 = **cumulative** 时间（这一行 + 它调用的一切）
- 第 40 行 cum 占 2.60s，因为 `json.Marshal` 贵

**`list` 支持正则**找全部匹配函数：

```
(pprof) list Parse.*       # 所有以 Parse 开头的函数
(pprof) list \.Handle      # 跨包的全部 Handle 方法
```

### `peek funcName`——调用方与被调方

显示谁调用了这个函数、它调用了谁——调用图里的一跳邻域。函数看着热但不确定问题在上游（被调太多次）还是下游（被调方贵）时，用它追责任链：

```
(pprof) peek json.Marshal
Showing nodes accounting for 5s, 100% of 5s total
----------------------------------------------+-------------
                                               |      flat  flat%   sum%        cum   cum%
 myapp/pkg/handler.serializeResponse 2.60s     |
 myapp/pkg/api.buildResponse         0.20s     |     1.50s 30.00% 30.00%      2.80s 56.00%  encoding/json.Marshal
----------------------------------------------+-------------
                                               |
 reflect.Value.MapRange              0.40s     |
 encoding/json.(*encodeState).marshal 0.30s    |
 runtime.mallocgc                     0.80s    |
```

上半部分 = 调用方（谁调 json.Marshal）。下半部分 = 被调方（json.Marshal 内部调了什么）。

### `tree`——层级调用树

显示完整调用树，每层带累计成本。需要比 `peek` 更多上下文时用：

```
(pprof) tree
     0.40s  8.00%  8.00%      3.80s 76.00%  myapp/pkg/handler.HandleRequest
              0.10s  myapp/pkg/handler.serializeResponse
                     1.50s  encoding/json.Marshal
                            0.80s  runtime.mallocgc
              0.20s  myapp/pkg/handler.validateInput
              0.10s  myapp/pkg/handler.fetchData
```

### `traces`——原始栈踪迹

转储全部原始采样栈踪迹。每条栈踪迹显示被采样那一刻程序在干什么：

```
(pprof) traces
-----------+-------------------------------------------------------
     bytes:  1.5MB
     1.50s   encoding/json.Marshal
             myapp/pkg/handler.serializeResponse
             myapp/pkg/handler.HandleRequest
             net/http.(*ServeMux).ServeHTTP
-----------+-------------------------------------------------------
```

用于发现意料之外的调用路径（例如没想到某个函数会从热路径被调）。

### `web` / `svg`——图形化调用图

`web` 在浏览器里打开调用图。`svg` 存成文件。两者都需要装 graphviz（`brew install graphviz` 或 `apt install graphviz`）。

视觉编码：

- **边越粗** = 流过该调用的时间越多
- **节点越大** = 函数自身耗时越多
- **红/深色节点** = 热点（flat 时间高）
- **边标签** = 流过该调用路径的时间

当文本命令看不清全貌时用——视觉布局常能暴露文本里难发现的调用模式。

### `disasm funcName`——指令级

显示生成的汇编并标注每指令成本。微优化用：验证 SIMD 指令、边界检查消除、指令级内联：

```
(pprof) disasm Parse
Total: 5s
ROUTINE ======================== myapp/pkg/parser.Parse
     0.40s      1.90s (flat, cum) 38.00% of Total
     0.10s      0.10s    4a3b20: MOVQ 0x8(SP), AX          ;parser.go:15
     0.20s      0.20s    4a3b28: CMPQ AX, $0x100           ;parser.go:16
         .      0.10s    4a3b2f: JGE 0x4a3b80              ;parser.go:16
     0.10s      1.50s    4a3b35: CALL runtime.makeslice(SB) ;parser.go:17
```

### `weblist funcName`——浏览器中带标注的源码

类似 `list`，但在浏览器里打开带颜色标注的源码。每行从白（无成本）到红（热）渐变。比文本版更直观：

```
(pprof) weblist serializeResponse
```

需要浏览器。没有浏览器时退回 `list`。

### `tags`——profile 标签分解

显示 profile 中存在的 tag 值。Go runtime profile 自带 `thread_id` 等 tag；自定义 profile 可以通过 `pprof.Do()` 加任意标签：

```go
labels := pprof.Labels("request_type", "api", "endpoint", "/users")
pprof.Do(ctx, labels, func(ctx context.Context) {
    handleRequest(ctx)
})
```

```
(pprof) tags
request_type: api (85%), batch (15%)
endpoint: /users (40%), /orders (35%), /products (25%)
```

### `tagroot` 与 `tagleaf`——按标签分组

按 tag 值给 profile 数据分组，生成一棵以 tag 名为根的虚拟调用树：

```
(pprof) tagroot request_type    # 先按 request_type 全量分组
(pprof) top                     # 现在按 request_type 分开展示
(pprof) tagleaf endpoint        # 加 endpoint 作为叶子分组
```

多租户 profiling 或不改代码按请求类型拆分时有用。

### `granularity`——控制分组粒度

改变样本的聚合方式：

```
(pprof) granularity=functions    # 默认——按函数名分组
(pprof) granularity=filefunctions # 按 file:function 分组
(pprof) granularity=files        # 只按文件分组
(pprof) granularity=lines        # 按确切源码行分组
(pprof) granularity=addresses    # 按指令地址分组（最细）
```

`lines` 在单个函数有多个热点时特别有用——不用 `list` 就能看出哪几行贵。

### `sort`——改排序方式

```
(pprof) sort=flat     # 按 flat 时间排序（top 的默认）
(pprof) sort=cum      # 按累计时间排序（同 top -cum）
```

### `source`——显示匹配正则的源码

类似 `list`，但搜索所有匹配某模式的函数并显示其标注源码：

```
(pprof) source handler   # 显示所有匹配 "handler" 的函数的标注源码
```

### `focus`、`ignore`、`hide`、`show`——过滤

把分析收窄到指定函数或排除噪声。它们是有状态的——跨命令持续生效，直到显式清除：

```
(pprof) focus=myapp            # 只显示经过 "myapp" 的调用路径
(pprof) ignore=runtime         # 从显示中移除 runtime 函数
(pprof) hide=testing           # 在图里隐藏测试框架噪声
(pprof) show=handler           # 只显示匹配 "handler" 的函数
(pprof) tagfocus=endpoint=/users  # 只显示带此 tag 值的样本
(pprof) tagignore=request_type=batch  # 排除带此 tag 值的样本
```

**`focus`、`show`、`hide`、`ignore` 的区别：**

- `focus`——只保留包含匹配函数的调用路径；其余全部丢弃
- `ignore`——把匹配函数从图里彻底移除；其成本归到调用方
- `show`——类似 `focus` 但只影响显示，不改成本归属
- `hide`——类似 `ignore` 但只是不显示，不改成本归属

**清除全部过滤：**

```
(pprof) reset
```

### `normalize`——对基准 profile 归一化

用 `-base` 对比两个 profile 时，值默认是 delta。`normalize` 把基准 profile 缩放到与主 profile 总量一致，即使两次运行时长不同，比例也可比：

```
(pprof) normalize
```

### `sample_index`——在多指标 profile 中切换指标

堆 profile 含多个指标（alloc_objects、alloc_space、inuse_objects、inuse_space）。不用重载即可切换：

```
(pprof) sample_index=alloc_objects
(pprof) top                       # 现在显示分配次数
(pprof) sample_index=inuse_space
(pprof) top                       # 现在显示存活内存
```

### `unit`——改显示单位

```
(pprof) unit=ms         # 时间按毫秒显示
(pprof) unit=seconds    # 按秒显示
(pprof) unit=MB         # 内存按 MB 显示
(pprof) unit=auto       # 自动（默认）
```

### `callgrind`——导出给 KCachegrind

把 profile 导出为 callgrind 格式，可在 KCachegrind 或 QCachegrind 中打开做高级可视化：

```
(pprof) callgrind
Generating report in callgrind format
```

### `proto`——保存处理后的 profile

把当前 profile（过滤之后）以 protobuf 格式保存，便于分享或后续分析：

```
(pprof) proto > filtered.pb.gz
```

### `help`——列出全部命令

```
(pprof) help             # 全部命令及说明
(pprof) help top         # 某个命令的详细帮助
```

### `show_from=regex`——裁掉匹配之上的调用方

隐藏第一个匹配函数之上的所有帧。只关心某个子系统、想把它上面的框架/路由噪声去掉时用：

```
(pprof) show_from=handler.Handle   # 图从 Handle 开始，隐藏上方全部调用方
```

### `noinlines`——压平内联函数

把内联函数归到其第一个非内联调用方。内联函数在图里造成混乱调用链时用：

```
(pprof) noinlines
```

### 完整命令参考

下面每个命令既能作为独立 shell 命令跑，也能在交互式 `(pprof)` 提示符里用。交互形式省略 `go tool pprof` 与 profile 路径——例如 `go tool pprof -top cpu.prof` 在提示符里就是 `top`。

**报告命令：**

```bash
# 按自身（flat）成本排名的头部函数——第一个要跑的命令
go tool pprof -top cpu.prof

# 按累计成本（自身 + 被调方）排名的前 20 个函数
go tool pprof -cum -top -nodecount=20 cpu.prof

# 指定函数的标注源码——定位确切昂贵行
go tool pprof -list=json.Marshal cpu.prof

# 函数的调用方与被调方——追责任链
go tool pprof -peek=serializeResponse cpu.prof

# 层级调用树，每层带成本
go tool pprof -tree cpu.prof

# 原始采样栈踪迹——发现意外调用路径
go tool pprof -traces cpu.prof

# 每指令汇编成本——验证 SIMD、边界检查、内联
go tool pprof -disasm=Parse cpu.prof

# 匹配正则的全部函数的标注源码
go tool pprof -source='handler\..*' cpu.prof

# 文本输出（flat 表，-top 的替代）
go tool pprof -text cpu.prof
```

**图形/导出命令：**

```bash
# SVG 调用图（任何浏览器可看，不需要 graphviz 服务）
go tool pprof -svg cpu.prof > cpu.svg

# 只导出匹配正则的子图
go tool pprof -svg -focus=handler cpu.prof > handler.svg

# PDF 调用图
go tool pprof -pdf cpu.prof > cpu.pdf

# PNG 调用图
go tool pprof -png cpu.prof > cpu.png

# GIF 调用图
go tool pprof -gif cpu.prof > cpu.gif

# DOT 格式（自定义 graphviz 处理：dot -Tsvg cpu.dot > cpu.svg）
go tool pprof -dot cpu.prof > cpu.dot

# Callgrind 格式（用 KCachegrind / QCachegrind 打开）
go tool pprof -callgrind cpu.prof > cpu.callgrind

# 以 protobuf 格式保存当前 profile（带过滤）
go tool pprof -proto -focus=handler cpu.prof > handler-only.pb.gz

# 浏览器打开标注源码，每行成本颜色编码
go tool pprof -weblist=serializeResponse cpu.prof
```

**过滤 flag**——把分析收窄到相关函数：

```bash
# focus：只保留经过匹配函数的调用路径
go tool pprof -focus=myapp/pkg/handler -top cpu.prof

# ignore：移除匹配函数——其成本归到调用方
go tool pprof -ignore=runtime -top cpu.prof

# show：只显示匹配函数（仅显示，不改成本归属）
go tool pprof -show=handler -top cpu.prof

# hide：从显示中隐藏匹配函数（不改成本归属）
go tool pprof -hide=testing -svg cpu.prof > clean.svg

# show_from：裁掉第一个匹配函数之上的所有帧——隐藏框架/路由调用方
go tool pprof -show_from=handler.Handle -top cpu.prof

# noinlines：内联函数归到其第一个非内联调用方
go tool pprof -noinlines -top cpu.prof

# 组合多个过滤
go tool pprof -cum -top -nodecount=10 -focus=handler -ignore=runtime cpu.prof
```

**按 tag 过滤**——用于带标签的 profile（经 `pprof.Do()`）：

```bash
# 显示全部 tag 键及其取值分布
go tool pprof -tags cpu.prof

# 只保留带指定 key=value 的样本
go tool pprof -tagfocus=endpoint=/users -top cpu.prof

# 排除带指定 tag 的样本
go tool pprof -tagignore=request_type=batch -top cpu.prof

# 按 tag 分组——在根部插入伪帧，按 tag 值拆分
go tool pprof -tagroot=request_type -top cpu.prof

# 按 tag 作叶子分组——每个函数按 tag 值拆分
go tool pprof -tagleaf=endpoint -top cpu.prof

# 在图输出中把 tag 显示/隐藏为标注
go tool pprof -tagshow=endpoint -svg cpu.prof > tagged.svg
go tool pprof -taghide=thread_id -svg cpu.prof > clean.svg
```

**粒度与显示控制：**

```bash
# 按源码行而非函数分组——揭示多热点函数中的热行
go tool pprof -granularity=lines -top cpu.prof

# 按 file:function 分组
go tool pprof -granularity=filefunctions -top cpu.prof

# 只按文件分组
go tool pprof -granularity=files -top cpu.prof

# 按指令地址分组（最细）
go tool pprof -granularity=addresses -top cpu.prof

# 改显示单位
go tool pprof -unit=ms -top cpu.prof

# 边/节点占比阈值——在图中隐藏小贡献
go tool pprof -edgefraction=0.01 -nodefraction=0.005 -svg cpu.prof > clean.svg

# 关闭裁剪——显示完整图含微小节点
go tool pprof -trim=false -svg cpu.prof > full.svg
```

**堆 profile 命令：**

```bash
# 按对象数排名的分配点——诊断 GC 搅动
go tool pprof -top -alloc_objects mem.prof

# 按字节数排名的分配点——诊断峰值内存
go tool pprof -top -alloc_space mem.prof

# 当前存活对象——诊断内存泄漏
go tool pprof -top -inuse_space mem.prof

# 当前存活对象数——诊断大量小对象泄漏
go tool pprof -top -inuse_objects mem.prof

# 按对象数显示分配点的标注源码
go tool pprof -alloc_objects -list=Parse mem.prof

# 按分配对象数着色的 SVG 调用图
go tool pprof -alloc_objects -svg mem.prof > allocs.svg

# 对比两个堆快照——只显示增长（内存泄漏检测）
go tool pprof -top -base heap-baseline.prof heap-after.prof

# 归一化 diff——两次采集时长不同时让比例可比
go tool pprof -normalize -top -base heap-baseline.prof heap-after.prof

# SVG 形式 diff——可视化涨了什么
go tool pprof -base heap-baseline.prof -svg heap-after.prof > leak.svg

# diff 并对指定函数显示标注源码
go tool pprof -base heap-baseline.prof -list=handleRequest heap-after.prof
```

**从运行中服务抓取 profile：**

```bash
# CPU profile——抓 30 秒样本并进交互模式
go tool pprof http://localhost:6060/debug/pprof/profile?seconds=30

# CPU profile——抓完直接生成 SVG（不进交互模式）
go tool pprof -svg http://localhost:6060/debug/pprof/profile?seconds=10 > cpu.svg

# CPU profile——带超时抓取
go tool pprof -timeout=60 "http://localhost:6060/debug/pprof/profile?seconds=30"

# 堆 profile——抓并显示头部分配点
go tool pprof -top -alloc_objects http://localhost:6060/debug/pprof/heap

# Goroutine profile——抓并显示头部 goroutine 栈
go tool pprof -top http://localhost:6060/debug/pprof/goroutine

# Mutex profile——抓争用数据
go tool pprof -top http://localhost:6060/debug/pprof/mutex

# Block profile——抓阻塞数据
go tool pprof -top http://localhost:6060/debug/pprof/block

# 只抓存文件不分析（用 curl）
curl -o heap.prof http://localhost:6060/debug/pprof/heap

# 人可读的 goroutine 转储（不需要 go tool pprof）
curl http://localhost:6060/debug/pprof/goroutine?debug=1

# 带完整栈踪迹、创建点与标签的 goroutine 转储
curl http://localhost:6060/debug/pprof/goroutine?debug=2

# 人可读的堆统计
curl http://localhost:6060/debug/pprof/heap?debug=1

# 带客户端证书的 TLS 抓取
go tool pprof -tls_cert=client.crt -tls_key=client.key -tls_ca=ca.crt https://myservice:6060/debug/pprof/profile?seconds=30

# 跳过服务端证书校验的 TLS 抓取
go tool pprof https+insecure://myservice:6060/debug/pprof/profile?seconds=30
```

**对比命令（diff 两个 profile）：**

```bash
# diff：source 减 base——所有值变成 delta
go tool pprof -base cpu-before.prof cpu-after.prof

# diff base：百分比相对基准 profile 显示
go tool pprof -diff_base=cpu-before.prof cpu-after.prof

# 归一化 diff——把 base 缩放到与 source 总量一致
go tool pprof -normalize -base heap-before.prof heap-after.prof

# top 报告形式的 diff
go tool pprof -top -base cpu-before.prof cpu-after.prof

# SVG 图形式的 diff
go tool pprof -svg -base cpu-before.prof cpu-after.prof > diff.svg
```

**Web UI:**

```bash
# 打开交互式 web UI，含火焰图、调用图、源码与反汇编视图
go tool pprof -http=:8080 cpu.prof

# 换个端口打开
go tool pprof -http=:9090 mem.prof

# 打开时预选某个样本类型
go tool pprof -http=:8080 -alloc_objects mem.prof

# 打开时预应用过滤
go tool pprof -http=:8080 -focus=handler cpu.prof

# 在 web UI 里打开 diff 视图
go tool pprof -http=:8080 -base heap-baseline.prof heap-after.prof

# 打开但不自动启动浏览器（只起服务）
go tool pprof -http=:8080 -no_browser cpu.prof
```

**符号化 flag：**

```bash
# 关闭符号化（显示裸地址）
go tool pprof -symbolize=none cpu.prof

# 只用本地二进制做符号化（不连远端）
go tool pprof -symbolize=local cpu.prof

# 连运行中服务取符号信息
go tool pprof -symbolize=remote http://localhost:6060/debug/pprof/profile?seconds=10

# 显示 mangled C++ 名（cgo profile 相关）
go tool pprof -symbolize=demangle=none cpu.prof

# 不做简化的完整 demangle
go tool pprof -symbolize=demangle=full cpu.prof
```

**环境变量：**

| 变量                | 用途                                                                                                            |
| ------------------- | --------------------------------------------------------------------------------------------------------------- |
| `PPROF_BINARY_PATH` | 符号化用的本地二进制搜索路径（默认 `$HOME/pprof/binaries`）。对远程服务器做 profile、二进制不在默认路径时设置。 |
| `PPROF_TOOLS`       | binutils 工具（`addr2line`、`nm`、`objdump`）所在目录。这些工具不在 `$PATH` 时设置。                            |

## 图形 / Web UI

CLI 输出不够、需要交互式探索时：

```bash
# 打开浏览器进交互式 UI
go tool pprof -http=:8080 cpu.prof

# 8080 被占时换端口
go tool pprof -http=:9090 mem.prof

# 打开时预选样本类型
go tool pprof -http=:8080 -alloc_objects mem.prof

# 打开时预应用过滤
go tool pprof -http=:8080 -focus=handler cpu.prof

# 对比两个 profile——用 -base 打开
go tool pprof -http=:8080 -base heap-baseline.prof heap-after.prof
```

web UI 提供：

- **Flamegraph**（最直观）——水平宽度正比于成本；点击下钻子树；另有反向火焰图（icicle graph）
- **Graph**——带边权的有向调用图；节点与边按成本定大小/颜色；可交互缩放与点击聚焦
- **Top**——同 `top` 命令但列可排序，点击可跳源码
- **Source**——带每行成本的标注源码；可跨全部函数浏览
- **Disassembly**——同 `disasm` 但可跨函数浏览
- **Peek**——交互式 peek 视图，调用方/被调方可展开

快速诊断默认用 CLI 命令——探索陌生调用图、可视化对比 profile、或向他人展示结论时用 web UI。

## 对比 profile

### 用 `-base` 检测内存泄漏

对比两个堆 profile，隔离两者之间涨了什么：

```bash
# 第 1 步：取基线快照
curl http://localhost:6060/debug/pprof/heap > heap-baseline.prof

# 第 2 步：等疑似泄漏积累（几分钟到几小时）

# 第 3 步：取第二个快照
curl http://localhost:6060/debug/pprof/heap > heap-after.prof

# 第 4 步：diff——只显示两个快照之间涨了什么
go tool pprof -base heap-baseline.prof heap-after.prof
# 然后照常 top、list、peek——所有值都是 delta
```

### 跨代码版本对比 CPU profile

```bash
# 变更前
go test -bench=BenchmarkParse -cpuprofile=cpu-before.prof ./pkg/parser

# 变更后
go test -bench=BenchmarkParse -cpuprofile=cpu-after.prof ./pkg/parser

# 可视化对比——两个浏览器标签页分别打开
go tool pprof -http=:8080 cpu-before.prof
go tool pprof -http=:8081 cpu-after.prof
```

基准测试数值（非 profile）的统计对比改用 [benchstat](./benchstat.md)。

## 常见模式

学会辨认这些反复出现的形态——动手修之前它们就告诉你面对的是什么类别的问题。

### flat 高 + cum 高

函数自身就是瓶颈。它直接做昂贵的功（紧循环、重计算、复杂字符串处理）。优化函数自身代码——算法、数据结构或实现。

### flat 低 + cum 高

函数调了慢的东西但自己不怎么干活——它是协调者或分发者。用 `list` 或 `peek` 钻进被调方。修法通常在被调函数里，或减少调用次数。

### `alloc_objects` 高、`inuse_space` 低

短生命周期分配造成 GC 搅动——对象快速分配又快速释放，单个都便宜但总量触发频繁 GC 周期。常见来源：热路径里的 `fmt.Errorf`（每次调用都分配）、接口装箱（`any` 参数）、string 与 byte 互转、未预分配的 slice 增长。分配削减模式 → 见 `golang-performance` skill。

### `inuse_space` 随时间增长

内存泄漏。隔几分钟取两个堆快照用 `-base` 对比（见上文对比 profile）——增长的类型揭示泄漏源。常见原因：无界缓存、只增不减的 map（Go map 删除时不释放桶内存）、持有引用的 goroutine 泄漏。

### Mutex/block profile 热

是争用不是 CPU——goroutine 全在等同一把锁或读同一个 channel 而不是干活。缩小临界区、把锁分片到多个 mutex，或用无锁结构（`sync/atomic`，读多场景用 `sync.Map`）。→ 见 `golang-concurrency` skill。

### 大量 goroutine 阻塞在同一 channel/mutex

串行化瓶颈——所有工作汇过同一个点，吞吐天花板就是那个单点的速度。考虑多独立队列的 worker pool、工作分片，或缓冲 channel 削峰。

### `runtime.mallocgc` 占据 CPU profile

瓶颈是分配速率而不是计算。Go runtime 花在分配与收垃圾上的时间比跑你的代码还多。切到 `alloc_objects` 堆 profile 找哪些函数分配最多，然后 → 见 `golang-performance` skill 找削减模式。

### `runtime.memmove` 在 CPU profile 中偏高

大块内存拷贝——通常是 slice `append` 超容量、大 slice 的 `copy()`，或 string-byte 转换。把 slice 预分配到最终容量、复用 buffer，或直接用 `[]byte`。

### `runtime.scanobject` 在 CPU profile 中偏高

GC 指针扫描。堆里指针太多，GC 必须逐一追踪。降低指针密度：slice/map 里用值类型代替指针、拍平嵌套结构、热结构体里用 `[N]byte` 数组代替 `string`。

## 什么症状用什么 profile

| 症状                     | Profile                   | Flag/命令                                         |
| ------------------------ | ------------------------- | ------------------------------------------------- |
| CPU 高、函数慢           | CPU                       | `-cpuprofile` 或 `pprof/profile`                  |
| 分配太多（GC 压力）      | Heap (alloc_objects)      | `-memprofile` 然后 `pprof -alloc_objects`         |
| 分配太大（内存占用）     | Heap (alloc_space)        | `pprof -alloc_space`                              |
| 内存随时间上涨（泄漏）   | Heap (inuse_space)        | `pprof -inuse_space`，配合 `-base` 对比           |
| 锁争用                   | Mutex                     | `pprof/mutex`（先启用 `SetMutexProfileFraction`） |
| goroutine 阻塞在同步原语 | Block                     | `pprof/block`（先启用 `SetBlockProfileRate`）     |
| goroutine 太多 / 泄漏    | Goroutine                 | `pprof/goroutine`                                 |
| 延迟高但 CPU 低          | Goroutine + Block + Trace | 调度延迟、I/O 等待——见 [Trace 参考](./trace.md)   |
| 线程创建过多             | Threadcreate              | `pprof/threadcreate`                              |
