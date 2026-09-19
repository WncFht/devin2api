# benchstat 参考

`benchstat` 对 Go 基准测试结果做统计摘要与 A/B 对比。单次基准测试运行说明不了任何方差信息——`benchstat` 告诉你两次运行之间的差异是真实的还是噪声。

## 目录

- [安装](#安装)
- [用法](#用法)
- [基本工作流](#基本工作流)
    - [第 0 步：写基准测试](#第-0-步写基准测试)
    - [第 1 步：测量基线](#第-1-步测量基线)
    - [第 2 步：做你的变更](#第-2-步做你的变更)
    - [第 3 步：再次测量](#第-3-步再次测量)
    - [第 4 步：对比](#第-4-步对比)
- [读懂输出](#读懂输出)
    - [单位归一化](#单位归一化)
    - [`~` 符号何时出现](#-符号何时出现)
- [Flag 参考](#flag-参考)
    - [投影 flag](#投影-flag)
    - [过滤 flag](#过滤-flag)
    - [输入打标](#输入打标)
- [过滤表达式语法](#过滤表达式语法)
    - [匹配运算符](#匹配运算符)
    - [逻辑运算符](#逻辑运算符)
    - [过滤键类型](#过滤键类型)
    - [过滤示例](#过滤示例)
- [投影示例](#投影示例)
    - [默认：前后文件对比](#默认前后文件对比)
    - [单文件内对比子基准参数](#单文件内对比子基准参数)
    - [行只保留基准名](#行只保留基准名)
    - [控制列顺序](#控制列顺序)
    - [按 GOMAXPROCS 分组](#按-gomaxprocs-分组)
    - [按包分表](#按包分表)
    - [忽略一个维度](#忽略一个维度)
    - [对比三个版本](#对比三个版本)
    - [跨维度对比](#跨维度对比)
- [单元元数据](#单元元数据)
    - [`assume=exact`](#assumeexact)
    - [`assume=nothing`（默认）](#assumenothing默认)
- [交错运行](#交错运行)
- [要跑多少次](#要跑多少次)
- [单文件摘要](#单文件摘要)
- [常见坑](#常见坑)
- [CI 中的 benchstat](#ci-中的-benchstat)

## 安装

```bash
go install golang.org/x/perf/cmd/benchstat@latest
```

## 用法

```bash
benchstat [flags] inputs...
```

每个输入是一个包含 `go test -bench` 输出的文件。可选地用 `label=path` 语法给输入打标。

## 基本工作流

### 第 0 步：写基准测试

在 `*_test.go` 中使用标准 Go 基准测试函数签名：

### 第 1 步：测量基线

用 `-count=10` 或更多跑基准测试。每次运行产生一个数据点——至少要 10 个才能算出有意义置信区间：

```bash
go test -run='^$' -bench=BenchmarkParse -benchmem -count=10 ./pkg/parser | tee old.txt
```

`-run='^$'` 跳过单元测试，只跑基准测试——测量会话期间不浪费跑测试的时间。

### 第 2 步：做你的变更

编辑你要优化的代码。

### 第 3 步：再次测量

同一条命令、同一批 flag、同一台机器、同样的负载条件：

```bash
go test -run='^$' -bench=BenchmarkParse -benchmem -count=10 ./pkg/parser | tee new.txt
```

### 第 4 步：对比

```bash
benchstat old.txt new.txt
```

输出：

```
goos: linux
goarch: amd64
pkg: myapp/pkg/parser
cpu: AMD Ryzen 9 5950X 16-Core Processor
          │   old.txt   │              new.txt               │
          │   sec/op    │   sec/op     vs base               │
Parse-32    4.592µ ± 2%   3.041µ ± 1%  -33.78% (p=0.000 n=10)

          │  old.txt   │             new.txt              │
          │    B/op    │    B/op     vs base              │
Parse-32    1.024Ki ± 0%   0.512Ki ± 0%  -50.00% (p=0.000 n=10)

          │  old.txt  │            new.txt             │
          │ allocs/op │ allocs/op   vs base            │
Parse-32    12.00 ± 0%   6.000 ± 0%  -50.00% (p=0.000 n=10)
```

## 读懂输出

| 元素                        | 含义                                                   | 看什么                                                                           |
| --------------------------- | ------------------------------------------------------ | -------------------------------------------------------------------------------- |
| **median**（如 `4.592µ`）   | 多次运行的中位值——比均值更稳健，因为离群值不会把它带偏 | 该基准测试的参考数值                                                             |
| **± N%**（如 `± 2%`）       | 95% 置信区间的半宽，占中位数的百分比                   | 低（≤2%）= 测量稳定。高（>5%）= 噪声大——先排查噪声源再信任结果                   |
| **vs base**（如 `-33.78%`） | 从第一个输入（base）到后续输入的百分比变化             | 负数 = 更快/更小。正数 = 更慢/更大                                               |
| **p=N**（如 `p=0.000`）     | Mann-Whitney U 检验（非参数）的 p 值                   | <0.05 = 统计显著。≥0.05 = 差异可能只是噪声                                       |
| **n=N**（如 `n=10`）        | 参与对比的样本数                                       | 通常应与你的 `-count` 一致；不一致时检查每个输入文件是否含相同的基准测试行与单位 |
| **`~`**                     | 未检测到统计显著差异                                   | 不要宣称改进——变化可能为零                                                       |
| **geomean** 行              | 表中全部基准测试变化的几何平均                         | 整体比例变化；一次对比大量基准测试时有用                                         |

### 单位归一化

benchstat 自动归一化显示单位：

- `ns/op` → 显示为 `sec/op`（带 µ、m 前缀），避免出现莫名其妙的 `µns/op`
- `MB/s` → 显示为 `B/s`（带 K、M、G 前缀）

### `~` 符号何时出现

```
Parse-32    4.592µ ± 8%   4.481µ ± 7%  ~ (p=0.089 n=10)
```

这表示 benchstat 无法把该差异与随机噪声区分开。两个宽置信区间（±8%、±7%）互相重叠。不要宣称改进。可选项：

- 把 `-count` 提到 20+（更窄的 CI 可能显出真实差异）
- 减少噪声源（关掉无关应用、接上电源、用专用机器）
- 接受该变更对这个基准测试没有可度量影响

## Flag 参考

### 投影 flag

这些 flag 控制基准测试结果如何分组为表、行、列。

| Flag           | 默认值      | 用途                                          |
| -------------- | ----------- | --------------------------------------------- |
| `-table KEYS`  | `.config`   | 按这些键把结果分组为多个表                    |
| `-row KEYS`    | `.fullname` | 按这些键把结果分组为表内行                    |
| `-col KEYS`    | `.file`     | 按这些键的不同值分列对比                      |
| `-ignore KEYS` | （无）      | 分组时忽略这些键——压制 "benchmarks vary" 警告 |

**可用的键：**

| 键            | 含义                                                         | 示例值                                   |
| ------------- | ------------------------------------------------------------ | ---------------------------------------- |
| `.name`       | 基准测试名本体（不含子基准配置）                             | `BenchmarkParse/size=4k-16` 中的 `Parse` |
| `.fullname`   | 含子基准配置的完整名                                         | `Parse/size=4k-16`                       |
| `.file`       | 输入文件名或自定义标签                                       | `old.txt` 或 `baseline`                  |
| `.config`     | 全部文件级配置键的组合                                       | `goos/goarch/pkg/cpu`                    |
| `.unit`       | 指标单位名                                                   | `sec/op`、`B/op`、`allocs/op`            |
| `/{name-key}` | 每个基准的子名键                                             | `/size` 从 `Parse/size=4k` 提取 `4k`     |
| `/gomaxprocs` | GOMAXPROCS 值——同时识别 `/gomaxprocs=N` 与 `-N` 后缀两种写法 | `Parse-16` 中的 `16`                     |
| `goos`        | 操作系统（取自基准输出头）                                   | `linux`、`darwin`                        |
| `goarch`      | 架构（取自基准输出头）                                       | `amd64`、`arm64`                         |
| `pkg`         | 包路径（取自基准输出头）                                     | `myapp/pkg/parser`                       |
| `cpu`         | CPU 型号（取自基准输出头）                                   | `AMD Ryzen 9 5950X`                      |

**排序修饰符**——追加到任意键上：

| 修饰符             | 含义                                        | 示例                 |
| ------------------ | ------------------------------------------- | -------------------- |
| `@alpha`           | 按字母排序                                  | `/format@alpha`      |
| `@num`             | 按数值排序（理解前缀：2k、1Mi）             | `/size@num`          |
| `@(val1 val2 ...)` | 固定顺序 + 过滤（只保留列出的值，按此顺序） | `/format@(gob json)` |

### 过滤 flag

| Flag           | 用途                                     |
| -------------- | ---------------------------------------- |
| `-filter EXPR` | 在分组与对比之前过滤哪些基准测试参与处理 |

完整细节见下文[过滤表达式语法](#过滤表达式语法)。

### 输入打标

不是 flag 而是语法特性——给输入文件打标让列头更清晰：

```bash
# 默认：文件名成为列头
benchstat old.txt new.txt

# 自定义标签
benchstat baseline=old.txt optimized=new.txt

# 多个版本
benchstat v1=v1.txt v2=v2.txt v3=v3.txt
```

第一个输入始终是对比的 **base**。所有后续输入都与它对比。

## 过滤表达式语法

过滤器在分组与对比之前选择哪些基准测试参与。语法如下：

### 匹配运算符

| 模式                 | 含义                                     | 示例                         |
| -------------------- | ---------------------------------------- | ---------------------------- |
| `key:value`          | 精确匹配                                 | `goos:linux`                 |
| `key:"value"`        | 带引号值的精确匹配（允许空格与特殊字符） | `pkg:"github.com/user/repo"` |
| `key:/regexp/`       | 正则匹配（Go regexp 语法）               | `.name:/Parse\|Encode/`      |
| `key:(val1 OR val2)` | 匹配列出的任意值                         | `goos:(linux OR darwin)`     |
| `*`                  | 匹配一切（全部基准测试）                 | `*`                          |

### 逻辑运算符

| 运算符    | 含义                        | 示例                                          |
| --------- | --------------------------- | --------------------------------------------- |
| `x y`     | AND——两者都必须匹配（隐式） | `goos:linux goarch:amd64`                     |
| `x AND y` | AND——显式形式               | `goos:linux AND goarch:amd64`                 |
| `x OR y`  | OR——任一匹配即可            | `goos:linux OR goos:darwin`                   |
| `-x`      | NOT——不得匹配               | `-goos:windows`                               |
| `(...)`   | 分组 / 子表达式             | `(goos:linux OR goos:darwin) -pkg:/internal/` |

### 过滤键类型

| 键            | 匹配什么             | 示例                         |
| ------------- | -------------------- | ---------------------------- |
| `.name`       | 基准测试名本体       | `.name:Parse`                |
| `.fullname`   | 含子基准配置的完整名 | `.fullname:/Parse\/size=4k/` |
| `/{name-key}` | 子基准参数           | `/size:4k`                   |
| `/gomaxprocs` | GOMAXPROCS 值        | `/gomaxprocs:16`             |
| `.file`       | 输入文件标签         | `.file:old.txt`              |
| `.unit`       | 指标单位             | `.unit:sec/op`               |
| `goos`        | 输出头中的 OS        | `goos:linux`                 |
| `goarch`      | 输出头中的架构       | `goarch:amd64`               |
| `pkg`         | 输出头中的包         | `pkg:/parser/`               |

### 过滤示例

```bash
# 只保留 Parse 基准测试
benchstat -filter '.name:Parse' old.txt new.txt

# 只保留 size=4096 子参数的基准测试
benchstat -filter '/size:4096' old.txt new.txt

# 排除 Parallel 基准测试
benchstat -filter '-.name:/Parallel/' old.txt new.txt

# 只要 linux amd64
benchstat -filter 'goos:linux goarch:amd64' old.txt new.txt

# 多个基准测试名
benchstat -filter '.name:(Parse OR Encode OR Decode)' old.txt new.txt

# 复合：linux 或 darwin，排除 internal 包，只看 sec/op 指标
benchstat -filter '(goos:linux OR goos:darwin) -pkg:/internal/ .unit:sec/op' old.txt new.txt

# 正则：所有以 Bench 开头的基准测试
benchstat -filter '.name:/^Bench/' old.txt new.txt
```

## 投影示例

### 默认：前后文件对比

```bash
benchstat old.txt new.txt
# 等价于：
benchstat -table .config -row .fullname -col .file old.txt new.txt
```

每个基准测试一行，每个文件一列。

### 单文件内对比子基准参数

当单个基准文件含多个子基准（例如 `BenchmarkEncode/format=json` 与 `BenchmarkEncode/format=gob`）：

```bash
benchstat -col /format bench.txt
```

为 `/format` 的每个取值生成一列，让它们互相对比。

### 行只保留基准名

```bash
benchstat -col /format -row .name bench.txt
```

把子基准配置从行名里剥掉，表格更紧凑。

### 控制列顺序

```bash
# 强制 gob 在前、json 在后（而非按字母序）
benchstat -col '/format@(gob json)' bench.txt
```

### 按 GOMAXPROCS 分组

```bash
benchstat -col /gomaxprocs bench.txt
```

在同一文件内对比不同 GOMAXPROCS 值下的性能。

### 按包分表

```bash
benchstat -table pkg old.txt new.txt
```

每个包一张表——跨多个包对比基准测试时有用。

### 忽略一个维度

```bash
# 压制 "benchmarks vary in /gomaxprocs" 警告
benchstat -row .name -ignore /gomaxprocs bench.txt
```

### 对比三个版本

```bash
benchstat v1=v1.txt v2=v2.txt v3=v3.txt
```

显示 v2 vs v1 与 v3 vs v1（第一个输入始终是 base）。

### 跨维度对比

```bash
# 行 = 基准测试名，列 = OS，按架构分表
benchstat -row .name -col goos -table goarch results.txt
```

## 单元元数据

### `assume=exact`

用于运行之间不应有变化的指标（例如二进制大小、生成代码大小）：

```
BenchmarkSize 1 42 custom-bytes/op
Unit custom-bytes/op assume=exact
```

`assume=exact` 时：

- 非参数统计被禁用
- 测量值若有变化 benchstat 会告警
- 单次前后测量也能显示对比（不需要 `-count`）

### `assume=nothing`（默认）

标准行为——使用非参数统计（中位数 + Mann-Whitney U 检验）。需要多个样本。

## 交错运行

顺序运行（先跑完所有 old，再跑所有 new）容易受**系统性偏差**影响——热节流随时间积累、后台进程来来去去、CPU 频率调节在自适应。交错运行能降低这种偏差：

```bash
# 预编译两个版本，避免把编译时间测进去
go test -c -o old.test ./pkg/parser
# ... 做你的变更 ...
go test -c -o new.test ./pkg/parser

# 交错运行——交替降低系统性偏差
for i in $(seq 1 10); do
    ./old.test -test.bench=BenchmarkParse -test.benchmem >> old.txt
    ./new.test -test.bench=BenchmarkParse -test.benchmem >> new.txt
done

benchstat old.txt new.txt
```

用 `go test -c` 预编译是关键——不预编译的话，每次 `go test -bench` 调用都含编译时间，这部分时间有波动，会污染结果。

## 要跑多少次

| 场景              | 最低 `-count` | 原因                                    |
| ----------------- | ------------- | --------------------------------------- |
| 本地快速验证      | 6             | 够算出粗略置信区间；反馈快              |
| 合并前对比        | 10            | 检测中等（>5%）变化的标准配置           |
| 检测小变化（<5%） | 20-30         | 更多样本收窄 CI；信号相对噪声偏小时必需 |
| 噪声大的 CI 环境  | 20+           | 共享 CI runner 方差更大；更多运行弥补   |

**绝不「重试到显著为止」**——反复重跑基准测试直到 `~` 消失会引入选择偏差（p-hacking）。10 次运行显示 `~`，说明变化多半没有意义。**一次性**提高运行次数，然后接受结果。

α=0.05 时，预期约 5% 的基准测试会在无真实变化的情况下随机报出显著（假阳性）。这很正常——别去追它们。

## 单文件摘要

不做对比，只分析单次运行的方差：

```bash
benchstat bench.txt
```

显示每个基准测试的中位数与置信区间。用途：

- 改代码前先检查测量稳定性
- 找出需要更多运行或更好隔离的噪声基准测试
- 快速摘要当前性能

## 常见坑

| 坑                   | 为什么错                                            | 修法                                     |
| -------------------- | --------------------------------------------------- | ---------------------------------------- |
| `-count=1`           | 单次运行没有方差信息；benchstat 算不出置信度        | 至少 `-count=6`，优先 `-count=10`        |
| 笔记本用电池跑       | CPU 为省电降频；方差爆炸                            | 接电源、关省电模式，或用台式机/服务器    |
| 开着浏览器/IDE 跑    | 后台进程偷 CPU 周期；引入噪声                       | 关掉不必要的应用，或接受更宽的 CI        |
| 重跑到 `~` 消失      | 选择偏差（p-hacking）——你在挑那些碰巧显示改进的运行 | 用高 `-count` 跑一次，接受结果           |
| 跨机器对比           | 不同 CPU、内存、OS = 基线不可比                     | 同一台机器、同样条件跑两次               |
| 不交错运行           | 热节流、后台负载漂移带来系统性偏差                  | 用 `go test -c` 预编译两个版本，交替运行 |
| 把编译时间测进去     | `go test -bench` 先编译；启动开销有波动             | 用 `go test -c` 预编译，直接跑二进制     |
| 无视宽 CI（± >5%）   | 结果看似显著但方差太大不可信                        | 先治噪声再对比；或加大 `-count`          |
| 对比不同 `-count` 值 | 样本量不均让对比有偏                                | 所有输入用同一个 `-count`                |

## CI 中的 benchstat

把 benchstat 对比集成进 CI 流水线（benchdiff、cob、gobenchdata），见 [CI 回归检测](./ci-regression.md)。
