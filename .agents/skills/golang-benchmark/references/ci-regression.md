# CI 基准回归检测

> **这些工具只在 CI 里跑，别在本地机器上跑。** 本地基准测试结果噪声大——后台进程、热节流、CPU 频率不一致——本地测出的回归不可靠，只会浪费开发者时间。即使共享 CI runner 也会有显著波动（5-10%）；用 `benchstat` 这类统计方法配合多次迭代与相对对比来滤掉噪声，或为关键路径投资专用基准 runner。

## 目录

- [benchdiff](#benchdiff)
- [cob](#cob)
- [gobenchdata](#gobenchdata)
    - [CLI 命令](#cli-命令)
    - [GitHub Action 配置](#github-action-配置)
    - [PR 上的回归门禁](#pr-上的回归门禁)
    - [面板配置](#面板配置)
- [工具选型指南](#工具选型指南)
- [Noisy Neighbor 缓解](#noisy-neighbor-缓解)
    - [为什么 CI 基准测试噪声大](#为什么-ci-基准测试噪声大)
    - [策略](#策略)
- [Self-Hosted Runner 系统调优](#self-hosted-runner-系统调优)
    - [禁用 CPU 频率调节](#禁用-cpu-频率调节)
    - [禁用 Turbo Boost](#禁用-turbo-boost)
    - [把基准测试绑到指定 CPU 核](#把基准测试绑到指定-cpu-核)
    - [禁用 SMT（Hyper-Threading）](#禁用-smthyper-threading)
    - [CI 组合设置脚本](#ci-组合设置脚本)

## benchdiff

在两个 git ref 上跑 Go 基准测试，用 `benchstat` 显示 delta。对非 worktree 的 ref 会缓存结果，重跑很快。基准测试期间阻止 macOS 睡眠。

```bash
go install filippo.io/mostly-harmless/benchdiff@latest
```

```bash
# 当前 worktree 对比 HEAD（默认）
benchdiff -- -benchmem

# 对比两个指定 ref
benchdiff -base-ref main -head-ref feature-branch

# 对比某个 commit 或 tag
benchdiff -base-ref v1.2.0

# 给 go test 传额外 flag——-- 之后的内容全部传给 go test
benchdiff -- -benchmem -count=10 -benchtime=3s

# 过滤到指定基准测试
benchdiff -- -benchmem -count=10 -bench=BenchmarkParse

# 指定某个包
benchdiff -- -benchmem -count=10 ./pkg/parser/...

# 清缓存（rebase 之后或缓存过期时用）
benchdiff -clear-cache

# 组合：对比 main，10 次迭代，只跑关键基准测试
benchdiff -base-ref main -- -benchmem -count=10 -bench='BenchmarkParse|BenchmarkEncode'
```

适用场景：git 工作流中快速的 PR-vs-base 对比。借助 `benchstat` 获得统计严谨性，非 worktree 的 ref 有缓存，重跑只需重测 worktree。

## cob

对比 HEAD 与 HEAD~1 之间的基准测试，性能退化超过可配置阈值（默认 20%）就让 CI job 失败。

```bash
go install github.com/knqyf263/cob@latest
```

```bash
# 默认 20% 阈值——对比 HEAD vs HEAD~1
cob

# 关键路径用更严阈值（10% 退化即失败）
cob -threshold 10

# 对比指定 base commit
cob -base main

# 只报告退化（忽略改进）
cob -only-degression

# 选择对比哪些指标（默认：ns/op,B/op）
cob -compare "ns/op,B/op,allocs/op"

# 自定义 go test 参数
cob -bench-args "test -run '^$' -bench BenchmarkParse -benchmem ./pkg/parser/..."

# 加长基准时长换取更稳结果
cob -bench-args "test -run '^$' -bench . -benchmem -benchtime=3s ./..."

# 某个 commit 跳过 cob：commit message 里包含 [skip cob]
```

**注意：** `cob` 内部使用 `git reset`，有未提交变更时可能丢数据——运行前先提交。

- 为安全起见只在 CI 流水线里跑，别在本地跑。
- `cob` 要求全部基准测试通过；任何一个失败它就跳过 CI 门禁。
- `cob` 只对比单次运行、没有 `benchstat` 式统计，比 `benchdiff` 更容易受噪声影响。

适用场景：提交后快速回归门禁，统计严谨性让位于反馈速度的 CI 场景。

## gobenchdata

GitHub Action + CLI：收集基准测试结果，以 JSON 发布到 gh-pages，用交互式 web 面板可视化。展示性能随时间的趋势。

```bash
go install go.bobheadxi.dev/gobenchdata@latest
```

### CLI 命令

```bash
# 把 go test -bench 输出解析成 JSON
go test -bench=. -benchmem -count=5 ./... | gobenchdata --json bench.json

# 从文件解析
gobenchdata --json bench.json < bench.txt

# 给这次基准运行打 tag（比如 git commit）
gobenchdata --json bench.json --tag "$(git rev-parse --short HEAD)" < bench.txt

# 按 checks 配置评估回归检查
gobenchdata checks eval bench.txt --checks-config .gobenchdata-checks.yml

# 生成 web 面板应用（静态 Vue.js 站点）
gobenchdata web generate ./dashboard-app

# 本地起服务预览面板
gobenchdata web serve ./dashboard-app

# 合并多个基准 JSON 文件
gobenchdata merge old-bench.json new-bench.json > combined.json

# 裁剪旧条目（保留最近 30 次运行）
gobenchdata prune --count 30 bench.json
```

### GitHub Action 配置

```yaml
# .github/workflows/benchmark.yml
name: Benchmark
on: [push]
jobs:
    benchmark:
        runs-on: ubuntu-latest
        steps:
            - uses: actions/checkout@v4
            - uses: actions/setup-go@v5
              with:
                  go-version: stable
            - name: Run benchmarks
              run: go test -bench=. -benchmem -count=5 ./... | tee bench.txt
            - uses: bobheadxi/gobenchdata@v1
              with:
                  PRUNE_COUNT: 30
                  GO_TEST_PKGS: ./...
                  BENCHMARKS_OUT: bench.txt
                  PUBLISH: true
                  PUBLISH_BRANCH: gh-pages
              env:
                  GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

### PR 上的回归门禁

```yaml
- name: Check for regressions
  run: gobenchdata checks eval bench.txt --checks-config .gobenchdata-checks.yml
```

```yaml
# .gobenchdata-checks.yml
checks:
    - name: "No major regressions"
      package: ./...
      benchmarks: [".*"]
      thresholds:
          - metric: NsPerOp
            max: 1.2 # 慢超 20% 即失败
          - metric: AllocedBytesPerOp
            max: 1.3 # 分配多超 30% 即失败
    - name: "Critical path stability"
      package: ./pkg/parser
      benchmarks: ["BenchmarkParse.*"]
      thresholds:
          - metric: NsPerOp
            max: 1.1 # 更严：慢超 10% 即失败
```

### 面板配置

```yaml
# gobenchdata-web.yml——配置 Vue.js 面板
title: "My Project Benchmarks"
description: "Performance tracking dashboard"
chartGroups:
    - name: Parser
      charts:
          - name: Parse Performance
            package: myapp/pkg/parser
            benchmarks: ["BenchmarkParse.*"]
            metrics: [NsPerOp, AllocedBytesPerOp, AllocsPerOp]
    - name: Encoding
      charts:
          - name: Encode/Decode
            package: myapp/pkg/encoding
            benchmarks: ["Benchmark(Encode|Decode).*"]
            metrics: [NsPerOp, MBPerS]
```

适用场景：长期趋势跟踪与可视化；与 benchdiff/cob 的即时门禁互补。

## 工具选型指南

| 工具                | 统计严谨性         | 面板                     | 适用场景                 |
| ------------------- | ------------------ | ------------------------ | ------------------------ |
| **benchdiff**       | 高（用 benchstat） | 无                       | 本地开发 + CI PR 对比    |
| **cob**             | 低（单次对比）     | 无                       | 快速 CI 门禁，配置简单   |
| **gobenchdata**     | 中（可配置检查）   | 有（gh-pages 上 Vue.js） | 长期趋势跟踪             |
| **benchstat**（裸） | 高                 | 无（CSV 导出）           | 最大控制度，自定义工作流 |

## Noisy Neighbor 缓解

云 CI 环境与其他 job 共享硬件。即使在安静的机器上也要有 5-10% 波动的心理预期。

### 为什么 CI 基准测试噪声大

- **共享 CPU/内存**——其他 CI job 在争资源
- **热节流**——持续负载降低时钟频率
- **每次运行硬件不同**——CI runner 规格可能不一样
- **内核调度**——上下文切换引入不可预测的延迟
- **磁盘 I/O 争用**——共享存储影响 I/O 密集型基准测试

### 策略

**统计严谨**——用 `-count=10` 或更多跑，用 `benchstat` 对比。单次运行没有意义。benchstat 的 p 值检验能滤掉噪声引起的假阳性。

**同一 job 内相对对比**——base 与 head 的基准测试在同一个 CI job、同一台机器上跑，而不是与历史绝对值对比。这样能消掉机器与机器之间的差异。`benchdiff` 这类工具通过 checkout 两个 git ref 自动做到这一点。

**专用基准 runner**——关键路径基准测试用不跑其他负载的 self-hosted CI runner。彻底消除 noisy neighbor，但基础设施成本更高。

**保守阈值**——共享 CI 上的回归阈值（20%+）要比专用 runner（10%）更宽。噪声环境里收紧阈值会制造假阳性，侵蚀信任。GitHub 托管 runner 最好情况下变异系数约 2-3%；要保证 <1% 假阳性率，需要 7%+ 的性能门禁。

**绝不「重试到通过为止」**——反复重跑基准测试直到通过会引入选择偏差。基准测试不稳定时，去修噪声源（更多迭代、专用 runner、更宽阈值）而不是重试。

## Self-Hosted Runner 系统调优

> **警告：这些命令修改内核与 CPU 设置。只用在专用 CI runner 上，绝不用在开发者机器或共享服务器上。**

当你能控制 CI 硬件时，这些设置通过消除主要的非确定性来源，大幅降低基准测试方差。

### 禁用 CPU 频率调节

CPU 频率可变让基准测试时长失去意义——同一份代码随负载与温度不同跑出不同速度：

```bash
# 把全部 CPU 设为 "performance" 调度器（固定最高频率）
echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor
```

### 禁用 Turbo Boost

Turbo Boost 临时拉高时钟频率，但在持续负载下会节流，在基准测试运行的开头与结尾之间制造方差：

```bash
# Intel
echo 1 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo

# AMD
echo 0 | sudo tee /sys/devices/system/cpu/cpufreq/boost
```

### 把基准测试绑到指定 CPU 核

防止 OS 在核之间迁移基准测试进程——迁移会导致缓存抖动（L1/L2 缓存是每核私有的）：

```bash
# 绑到核 2、3（核 0-1 留给 OS 与其他进程）
taskset -c 2,3 go test -bench=. -count=10 ./...
```

### 禁用 SMT（Hyper-Threading）

SMT 让同一物理核上的两个逻辑核共享执行单元，造成不可预测的争用：

```bash
# 全系统禁用 SMT
echo off | sudo tee /sys/devices/system/cpu/smt/control

# 或禁用单个兄弟核（查 /sys/devices/system/cpu/cpu*/topology/thread_siblings_list）
echo 0 | sudo tee /sys/devices/system/cpu/cpu1/online  # cpu0 与 cpu1 是兄弟核时
```

### CI 组合设置脚本

```bash
#!/bin/bash
# benchmark-setup.sh——在 self-hosted CI runner 上跑基准测试前执行
set -euo pipefail

echo "=== Configuring CPU for stable benchmarks ==="
echo performance | sudo tee /sys/devices/system/cpu/cpu*/cpufreq/scaling_governor
echo 1 | sudo tee /sys/devices/system/cpu/intel_pstate/no_turbo 2>/dev/null || true
echo off | sudo tee /sys/devices/system/cpu/smt/control 2>/dev/null || true

echo "=== Running benchmarks on isolated cores ==="
taskset -c 2,3 go test -bench=. -benchmem -count=10 ./... | tee bench.txt
```
