# 排查会话搭建

用于**临时深挖性能问题**的工具与技术——不是日常监控。这些东西在排查某个具体问题时开几小时或几天，用完就关。

## 目录

- [搭建排查会话](#搭建排查会话)
- [Prometheus Go Runtime 收集器](#prometheus-go-runtime-收集器)
    - [关键 series](#关键-series)
- [PromQL 深查查询](#promql-深查查询)
    - [GC 压力](#gc-压力)
    - [内存泄漏检测](#内存泄漏检测)
    - [Goroutine 泄漏检测](#goroutine-泄漏检测)
    - [CPU 饱和](#cpu-饱和)
    - [部署后回归检测](#部署后回归检测)
    - [告警规则示例](#告警规则示例)
- [主机级关联](#主机级关联)
- [成本警示](#成本警示)

## 搭建排查会话

钻进 profile 之前，先把环境搭好以采集高分辨率数据：

1. **把 Prometheus 抓取间隔**在目标实例上降到 <=10s（正常是 15-30s）。排查窗口短，更多数据点能揭示 30s 间隔漏掉的模式。排查完改回去。

2. **启用 pprof**——用环境变量，不用重新编译：

    ```bash
    kubectl set env deployment/my-service PPROF_ENABLED=true
    kubectl rollout restart deployment/my-service
    ```

3. **启用持续 profiling**——只在目标实例上开，不要全舰队开。单实例跑 Pyroscope/Parca 可控；50 个副本会压垮后端。

    ```bash
    kubectl set env deployment/my-service PYROSCOPE_ENABLED=true
    kubectl rollout restart deployment/my-service
    ```

4. **需要时开 debug 日志**——走环境变量，但只在目标实例上开。debug 日志对吞吐有显著影响：

    ```bash
    kubectl set env deployment/my-service LOG_LEVEL=debug
    kubectl rollout restart deployment/my-service
    ```

**关键原则：** 所有有成本的 debug 特性（pprof HTTP、持续 profiling、debug 日志级别、trace 采集）都应该能用环境变量配置。这样才能不重新编译即时开关。从第一天起就按这个要求设计应用。

## Prometheus Go Runtime 收集器

`prometheus/client_golang` 库自动注册暴露 Go runtime 指标的收集器。排查会话期间它们价值很大——提供内存、GC、goroutine、CPU 的时间序列视图，补足单点 profile 的不足。

使用 `prometheus/client_golang` 时，查阅库的官方文档核实收集器配置与可用选项。

### 关键 series

→ 全部 Go runtime 指标的**完整参考**（已从官方来源核实）见 [prometheus-go-metrics.md](./prometheus-go-metrics.md)。**注意：** runtime/metrics 清单随 Go 版本变化——运行时用 `metrics.All()` 查你所用版本的清单。

**性能注意：** `go_memstats_*` 指标内部调 `runtime.ReadMemStats()`，会触发一次短暂的 stop-the-world 暂停。Go 1.17+ 中 runtime/metrics 收集器（`collectors.NewGoCollector()`）改用 `runtime/metrics`，开销更低。高吞吐服务优先用新式收集器：

```go
import "github.com/prometheus/client_golang/prometheus/collectors"

// 用基于 runtime/metrics 的收集器（开销更低）
reg := prometheus.NewRegistry()
reg.MustRegister(collectors.NewGoCollector(
    collectors.WithGoCollectorRuntimeMetrics(collectors.MetricsAll),
))
reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
```

## PromQL 深查查询

排查会话期间配合收紧的抓取间隔使用这些查询。每条都写明看什么、结果意味着什么。

### GC 压力

| PromQL                                                                          | 看什么                                                 |
| ------------------------------------------------------------------------------- | ------------------------------------------------------ |
| `rate(go_gc_duration_seconds_count[5m])`                                        | GC 次数/秒。持续 >2/s = 分配速率过高。减少每请求分配。 |
| `rate(go_gc_duration_seconds_sum[5m]) / rate(go_gc_duration_seconds_count[5m])` | 平均 GC 暂停。趋势上升 = 堆在涨，或要扫描的指针太多。  |
| `go_gc_duration_seconds{quantile="1"}`                                          | 最坏 GC 暂停。这里的尖刺造成尾延迟（P99）。            |

### 内存泄漏检测

| PromQL                                                  | 看什么                                                     |
| ------------------------------------------------------- | ---------------------------------------------------------- |
| `go_memstats_alloc_bytes`                               | 恒定负载下应大致平稳。持续上涨 = 内存泄漏。                |
| `rate(go_memstats_alloc_bytes_total[5m])`               | 分配速率（字节/秒）。对比部署前后——显著上升 = 新分配模式。 |
| `process_resident_memory_bytes - go_memstats_sys_bytes` | 差值 = 非 Go 内存（cgo、mmap）。差值变大 = 非 Go 泄漏。    |

### Goroutine 泄漏检测

| PromQL                     | 看什么                                  |
| -------------------------- | --------------------------------------- |
| `go_goroutines`            | 应与负载相关。脱离流量独立上涨 = 泄漏。 |
| `delta(go_goroutines[1h])` | 1 小时净增。负载没涨却为正 = 泄漏。     |

### CPU 饱和

| PromQL                                               | 看什么                                |
| ---------------------------------------------------- | ------------------------------------- |
| `rate(process_cpu_seconds_total[5m])`                | 消耗的 CPU 核数。与 GOMAXPROCS 对比。 |
| `rate(process_cpu_seconds_total[5m]) / <GOMAXPROCS>` | CPU 利用率。持续 >0.8 = CPU 饱和。    |

### 部署后回归检测

| PromQL                                                                     | 看什么                                                 |
| -------------------------------------------------------------------------- | ------------------------------------------------------ |
| `rate(go_memstats_alloc_bytes_total[5m])`                                  | 对比部署前后窗口。显著上升 = 引入了新分配模式。        |
| `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))` | 部署后 P99 延迟上升 = 性能回归。需要应用层 histogram。 |

### 告警规则示例

```yaml
# GC 耗时过高
- alert: HighGCPauseTime
  expr: rate(go_gc_duration_seconds_sum[5m]) / rate(go_gc_duration_seconds_count[5m]) > 0.01
  for: 10m
  annotations:
      summary: "Average GC pause >10ms — reduce allocations or tune GOGC"

# Goroutine 泄漏
- alert: GoroutineLeak
  expr: go_goroutines > 10000
  for: 5m
  annotations:
      summary: "Goroutine count >10K — check for leaked goroutines"

# 内存逼近容器限额
- alert: MemoryNearLimit
  expr: predict_linear(process_resident_memory_bytes[1h], 3600) > <container_limit_bytes>
  for: 15m
  annotations:
      summary: "RSS projected to exceed container limit within 1h"
```

阈值按你的应用调——数据管道与 API 服务的基线不一样。

## 主机级关联

光有 Go runtime 指标看不到全貌。主机级指标揭示问题在你的应用里还是在基础设施里。

- **`node_exporter`**——主机 CPU、内存、磁盘 I/O、网络。与 Go 应用指标关联：`node_cpu_seconds_total` 高而 `process_cpu_seconds_total` 低 = noisy neighbor，不是你的应用。
- **`process-exporter`**——Linux 上按进程的指标。多个 Go 服务共享一台主机时有用。

## 成本警示

**Profile 与 trace 的采集是有成本的。** 保持短期、局部：

- **pprof CPU profiling**——采集窗口内 CPU 密集。生产上别连着跑 30s profile。错开跑。
- **Pyroscope 持续 profiling**——**每实例常驻**约 2-5% CPU 开销。规模大了（几百实例）算力成本与后端存储会累积。在部分实例上开，或用环境变量按需开。Pyroscope 配置 → 见 `samber/cc-skills-golang@golang-observability` skill。
- **执行 trace**——很快产生大文件（MB/s）。最多采 5-10s。更长的 trace 难处理、分析慢。
- **Debug 日志级别**——分配与 I/O 开销带来显著吞吐影响。绝不长期开着。
- **所有有成本的特性**都应该能用环境变量开关，不重新编译即时切换。从第一天起按此设计。
