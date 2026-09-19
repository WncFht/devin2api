# 生产环境性能可观测性

第三方监控工具补足本地 profiling（pprof、基准测试），在生产中提供持续监控、历史趋势与回归检测。

## 目录

- [Go 的 Prometheus 指标](#go-的-prometheus-指标)
    - [性能诊断 PromQL 查询](#性能诊断-promql-查询)
    - [告警规则（示例）](#告警规则示例)
    - [Grafana 仪表盘](#grafana-仪表盘)
- [持续性能分析](#持续性能分析)
    - [Pyroscope 推送模式](#pyroscope-推送模式)
    - [Pyroscope 拉取模式（经 Grafana Alloy）](#pyroscope-拉取模式经-grafana-alloy)
- [实时可视化（开发）](#实时可视化开发)

## Go 的 Prometheus 指标

**接入：**`github.com/prometheus/client_golang`——用 `promhttp.Handler()` 暴露 `/metrics` 端点。默认 collector 自动导出 Go runtime 指标（`go_goroutines`、`go_memstats_*`、`go_gc_duration_seconds`、`process_cpu_seconds_total` 等）。

→ 见 `golang-benchmark` skill（investigation-session.md）：完整 runtime 指标表、调查会话搭建（抓取间隔调优、环境变量开关）与 profiling 工具成本警告。

### 性能诊断 PromQL 查询

#### GC 压力

| PromQL                                                                          | 看什么                                     |
| ------------------------------------------------------------------------------- | ------------------------------------------ |
| `rate(go_gc_duration_seconds_count[5m])`                                        | 每秒 GC 次数——持续 >2/s 提示分配速率过高   |
| `rate(go_gc_duration_seconds_sum[5m]) / rate(go_gc_duration_seconds_count[5m])` | 平均 GC 停顿——上升趋势说明堆在涨或指针太多 |
| `go_gc_duration_seconds{quantile="1"}`                                          | 最坏 GC 停顿——这里的尖峰造成尾延迟         |

#### 内存泄漏

| PromQL                                                  | 看什么                                                |
| ------------------------------------------------------- | ----------------------------------------------------- |
| `go_memstats_alloc_bytes`                               | 恒定负载下应大致平稳；持续上涨 = 内存泄漏             |
| `rate(go_memstats_alloc_bytes_total[5m])`               | 分配速率（字节/秒）——驱动 GC 频率；部署前后对比查回归 |
| `process_resident_memory_bytes - go_memstats_sys_bytes` | 差值 = 非 Go 内存（cgo、mmap）；差值变大 = 非 Go 泄漏 |

#### goroutine 泄漏

| PromQL                     | 看什么                                |
| -------------------------- | ------------------------------------- |
| `go_goroutines`            | 应与负载相关；脱离流量独立上涨 = 泄漏 |
| `delta(go_goroutines[1h])` | 1 小时净变化；负载没涨却为正 = 泄漏   |

#### CPU 饱和

| PromQL                                               | 看什么                                    |
| ---------------------------------------------------- | ----------------------------------------- |
| `rate(process_cpu_seconds_total[5m])`                | 消耗的 CPU 核数；对照 GOMAXPROCS 判断饱和 |
| `rate(process_cpu_seconds_total[5m]) / <GOMAXPROCS>` | CPU 利用率；持续 >0.8 = CPU 饱和          |

#### 回归检测（部署后）

| PromQL                                                                     | 看什么                                             |
| -------------------------------------------------------------------------- | -------------------------------------------------- |
| `rate(go_memstats_alloc_bytes_total[5m])`                                  | 部署前后对比；显著上升 = 引入了新分配模式          |
| `histogram_quantile(0.99, rate(http_request_duration_seconds_bucket[5m]))` | 部署后 p99 延迟上升 = 回归（需要应用层 histogram） |

### 告警规则（示例）

[示例告警规则](../assets/prometheus-alerts.yml)——阈值按你的应用调整；高吞吐数据管道与轻量 API server 的基线不同。

→ 这些 PromQL 表达式可直接在 Prometheus UI 上交互验证，或用 `promtool query instant <prometheus-url> '<expr>'` 从 CLI 执行。

### Grafana 仪表盘

→ Grafana 仪表盘市场（grafana.com/dashboards）有现成的 Go runtime 社区仪表盘可导入，按上面的指标清单挑覆盖面即可。

## 持续性能分析

持续性能分析在生产中以低开销采样并存档供历史对比。用它跨部署抓回归、比较不同时间的火焰图、喂给 PGO（见 [Runtime 调优](./runtime.md#性能剖析指导优化pgo)）。

| 工具                            | 模式                        | 开销  | 最适合                           |
| ------------------------------- | --------------------------- | ----- | -------------------------------- |
| **Grafana Pyroscope**           | 推送 SDK 或拉取（经 Alloy） | ~2-5% | Grafana 生态、历史火焰图对比     |
| **Parca**（Polar Signals）      | eBPF 拉取                   | <1%   | 全基础设施 profiling，零代码改动 |
| **Datadog Continuous Profiler** | 推送（agent）               | ~1-2% | 已有 Datadog 用户                |
| **Google Cloud Profiler**       | 推送（agent）               | ~1-2% | GCP 上的 Go 服务                 |

### Pyroscope 推送模式

```go
import "github.com/grafana/pyroscope-go"

pyroscope.Start(pyroscope.Config{
    ApplicationName: "myapp",
    ServerAddress:   "http://pyroscope:4040",
    ProfileTypes: []pyroscope.ProfileType{
        pyroscope.ProfileCPU,
        pyroscope.ProfileAllocObjects,
        pyroscope.ProfileAllocSpace,
        pyroscope.ProfileInuseObjects,
        pyroscope.ProfileInuseSpace,
        pyroscope.ProfileGoroutines,
    },
})
```

### Pyroscope 拉取模式（经 Grafana Alloy）

无需改代码——Alloy 定期抓取 `/debug/pprof/*` 端点。把 Alloy 配置指向你服务的 pprof 端点即可。

用第三方 profiling 库时，API 签名以该库官方文档为准。

## 实时可视化（开发）

| 工具                                      | 作用                                                                                                                     |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| **statsviz**（`github.com/arl/statsviz`） | `/debug/statsviz` 上的实时浏览器仪表盘——堆、GC 停顿、goroutine、调度器。用 `statsviz.Register(mux)` 注册。本地开发很好用 |
| **expvar**（标准库 `expvar`）             | `/debug/vars` 输出 JSON 指标——轻量、零依赖。可接 Netdata、Telegraf 或自建仪表盘                                          |
