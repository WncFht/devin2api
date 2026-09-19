# Prometheus Go Runtime 指标参考

`prometheus/client_golang` 库**实际暴露为 Prometheus 指标**的 Go runtime 指标完整清单。

---

## 目录

- [重要澄清](#重要澄清)
- [速查](#速查)
    - [带标签的指标](#带标签的指标)
    - [其余全部指标](#其余全部指标)
- [默认 Go 指标（始终暴露）](#默认-go-指标始终暴露)
    - [内存分配](#内存分配)
    - [堆状态](#堆状态)
    - [栈与元数据](#栈与元数据)
    - [分配与释放计数器](#分配与释放计数器)
    - [GC 配置与计时](#gc-配置与计时)
    - [GC 暂停时长（带标签）](#gc-暂停时长带标签)
    - [Runtime 状态](#runtime-状态)
    - [版本信息（带标签）](#版本信息带标签)
- [可选 Go 指标（需启用，Go 1.17+）](#可选-go-指标需启用go-117)
    - [GC 周期](#gc-周期)
    - [更多堆指标](#更多堆指标)
    - [GC 暂停分布](#gc-暂停分布)
    - [CPU 分类](#cpu-分类)
    - [内存分类](#内存分类)
    - [调度器指标](#调度器指标)
    - [CGO 指标](#cgo-指标)
- [进程指标](#进程指标)
    - [CPU 与内存](#cpu-与内存)
    - [文件描述符](#文件描述符)
    - [进程信息](#进程信息)
    - [缺页](#缺页)
- [常用 PromQL 查询](#常用-promql-查询)
    - [内存泄漏检测](#内存泄漏检测)
    - [GC 压力](#gc-压力)
    - [Goroutine 泄漏](#goroutine-泄漏)
    - [CPU 使用](#cpu-使用)
    - [文件描述符泄漏](#文件描述符泄漏)
- [参考资料](#参考资料)

## 重要澄清

**`runtime/metrics` 不是 Prometheus 指标。** 它们是 Go runtime 的数据结构。

Prometheus Go 客户端库（`prometheus/client_golang`）**有选择地把一部分** `runtime/metrics` 转成 Prometheus 格式。默认只暴露传统的 `go_memstats_*` 与 `go_gc_*` 指标，以控制基数。

**本文只列 Prometheus 指标**（你实际从 `/metrics` 端点抓到的那些）。

---

## 速查

### 带标签的指标

| 指标                     | 标签       | 取值                  |
| ------------------------ | ---------- | --------------------- |
| `go_gc_duration_seconds` | `quantile` | 0, 0.25, 0.5, 0.75, 1 |
| `go_info`                | `version`  | 如 "go1.21.3"         |

### 其余全部指标

其余全部指标**没有标签**。

---

## 默认 Go 指标（始终暴露）

以下由 `prometheus/client_golang` 默认暴露。

### 内存分配

| 指标                            | 类型    | 说明                 |
| ------------------------------- | ------- | -------------------- |
| `go_memstats_alloc_bytes`       | gauge   | 当前堆上已分配字节数 |
| `go_memstats_alloc_bytes_total` | counter | 累计分配字节数       |
| `go_memstats_sys_bytes`         | gauge   | 向 OS 申请的总字节数 |

### 堆状态

| 指标                              | 类型  | 说明                 |
| --------------------------------- | ----- | -------------------- |
| `go_memstats_heap_alloc_bytes`    | gauge | 已分配堆字节数       |
| `go_memstats_heap_idle_bytes`     | gauge | 空闲堆字节数         |
| `go_memstats_heap_inuse_bytes`    | gauge | 使用中堆字节数       |
| `go_memstats_heap_objects`        | gauge | 堆对象数             |
| `go_memstats_heap_released_bytes` | gauge | 已归还 OS 的堆字节数 |
| `go_memstats_heap_sys_bytes`      | gauge | 向 OS 预留的堆字节数 |

### 栈与元数据

| 指标                              | 类型  | 说明                     |
| --------------------------------- | ----- | ------------------------ |
| `go_memstats_stack_inuse_bytes`   | gauge | 使用中栈字节数           |
| `go_memstats_stack_sys_bytes`     | gauge | 预留栈字节数             |
| `go_memstats_mspan_inuse_bytes`   | gauge | 使用中 mspan 字节数      |
| `go_memstats_mspan_sys_bytes`     | gauge | 预留 mspan 字节数        |
| `go_memstats_mcache_inuse_bytes`  | gauge | 使用中 mcache 字节数     |
| `go_memstats_mcache_sys_bytes`    | gauge | 预留 mcache 字节数       |
| `go_memstats_other_sys_bytes`     | gauge | 其他 runtime 字节数      |
| `go_memstats_gc_sys_bytes`        | gauge | GC 内部字节数            |
| `go_memstats_buck_hash_sys_bytes` | gauge | profiling 桶哈希表字节数 |

### 分配与释放计数器

| 指标                        | 类型    | 说明          |
| --------------------------- | ------- | ------------- |
| `go_memstats_mallocs_total` | counter | malloc 总次数 |
| `go_memstats_frees_total`   | counter | free 总次数   |

### GC 配置与计时

| 指标                               | 类型  | 说明                        |
| ---------------------------------- | ----- | --------------------------- |
| `go_gc_gogc_percent`               | gauge | GOGC 目标百分比             |
| `go_gc_gomemlimit_bytes`           | gauge | GOMEMLIMIT 软内存上限       |
| `go_memstats_last_gc_time_seconds` | gauge | 上次 GC 结束时刻（Unix 秒） |
| `go_memstats_next_gc_bytes`        | gauge | 下次 GC 的堆大小目标        |

### GC 暂停时长（带标签）

| 指标                           | 类型    | 标签                                | 说明             |
| ------------------------------ | ------- | ----------------------------------- | ---------------- |
| `go_gc_duration_seconds`       | summary | `quantile`（0, 0.25, 0.5, 0.75, 1） | 分位 GC 暂停时长 |
| `go_gc_duration_seconds_count` | counter | —                                   | GC 暂停次数      |
| `go_gc_duration_seconds_sum`   | counter | —                                   | GC 暂停总时长    |

### Runtime 状态

| 指标                          | 类型  | 说明               |
| ----------------------------- | ----- | ------------------ |
| `go_goroutines`               | gauge | 当前 goroutine 数  |
| `go_threads`                  | gauge | 当前 OS 线程数     |
| `go_sched_gomaxprocs_threads` | gauge | 当前 GOMAXPROCS 值 |

### 版本信息（带标签）

| 指标      | 类型  | 标签      | 说明          |
| --------- | ----- | --------- | ------------- |
| `go_info` | gauge | `version` | Go 版本字符串 |

---

## 可选 Go 指标（需启用，Go 1.17+）

启用方式：

```go
prometheus.NewRegistry().MustRegister(
    collectors.NewGoCollector(
        collectors.WithGoCollectorRuntimeMetrics(
            collectors.MetricsAll,
        ),
    ),
)
```

### GC 周期

| 指标                                     | 类型    | 说明                         |
| ---------------------------------------- | ------- | ---------------------------- |
| `go_gc_cycles_automatic_gc_cycles_total` | counter | 自动 GC 周期（堆增长）       |
| `go_gc_cycles_forced_gc_cycles_total`    | counter | 强制 GC 周期（runtime.GC()） |

### 更多堆指标

| 指标                              | 类型    | 说明                 |
| --------------------------------- | ------- | -------------------- |
| `go_gc_heap_allocs_bytes_total`   | counter | 累计堆分配（字节）   |
| `go_gc_heap_allocs_objects_total` | counter | 累计堆分配（个数）   |
| `go_gc_heap_frees_bytes_total`    | counter | 累计堆释放（字节）   |
| `go_gc_heap_frees_objects_total`  | counter | 累计堆释放（个数）   |
| `go_gc_heap_goal_bytes`           | gauge   | 下次 GC 的堆大小目标 |
| `go_gc_heap_live_bytes`           | gauge   | 存活堆字节数         |
| `go_gc_heap_objects_objects`      | gauge   | 堆对象总数           |

### GC 暂停分布

| 指标                   | 类型         | 说明        |
| ---------------------- | ------------ | ----------- |
| `go_gc_pauses_seconds` | distribution | GC 暂停时长 |

### CPU 分类

| 指标                                                   | 类型    | 说明                    |
| ------------------------------------------------------ | ------- | ----------------------- |
| `go_cpu_classes_gc_mark_assist_cpu_seconds_total`      | counter | GC 标记辅助 CPU 时间    |
| `go_cpu_classes_gc_mark_dedicated_cpu_seconds_total`   | counter | GC 专职 worker CPU 时间 |
| `go_cpu_classes_gc_mark_idle_cpu_seconds_total`        | counter | GC 空闲 worker CPU 时间 |
| `go_cpu_classes_gc_pause_cpu_seconds_total`            | counter | GC 暂停 CPU 时间        |
| `go_cpu_classes_gc_total_cpu_seconds_total`            | counter | GC 总 CPU 时间          |
| `go_cpu_classes_idle_cpu_seconds_total`                | counter | 空闲 CPU 时间           |
| `go_cpu_classes_scavenge_assist_cpu_seconds_total`     | counter | 清扫辅助 CPU 时间       |
| `go_cpu_classes_scavenge_background_cpu_seconds_total` | counter | 后台清扫 CPU 时间       |
| `go_cpu_classes_scavenge_total_cpu_seconds_total`      | counter | 清扫总 CPU 时间         |
| `go_cpu_classes_total_cpu_seconds_total`               | counter | 总 CPU 时间（全类）     |
| `go_cpu_classes_user_cpu_seconds_total`                | counter | 用户态 CPU 时间         |

### 内存分类

| 指标                                            | 类型  | 说明               |
| ----------------------------------------------- | ----- | ------------------ |
| `go_memory_classes_heap_free_bytes`             | gauge | 空闲堆内存         |
| `go_memory_classes_heap_objects_bytes`          | gauge | 已分配堆对象       |
| `go_memory_classes_heap_released_bytes`         | gauge | 已释放堆内存       |
| `go_memory_classes_heap_stacks_bytes`           | gauge | 栈内存             |
| `go_memory_classes_heap_unused_bytes`           | gauge | 未用堆内存         |
| `go_memory_classes_metadata_mcache_free_bytes`  | gauge | 空闲 mcache 内存   |
| `go_memory_classes_metadata_mcache_inuse_bytes` | gauge | 使用中 mcache 内存 |
| `go_memory_classes_metadata_mspan_free_bytes`   | gauge | 空闲 mspan 内存    |
| `go_memory_classes_metadata_mspan_inuse_bytes`  | gauge | 使用中 mspan 内存  |
| `go_memory_classes_other_bytes`                 | gauge | 其他内存           |
| `go_memory_classes_total_bytes`                 | gauge | 总内存             |

### 调度器指标

| 指标                                           | 类型         | 说明                           |
| ---------------------------------------------- | ------------ | ------------------------------ |
| `go_sched_goroutines_running_goroutines`       | gauge        | 运行中 goroutine               |
| `go_sched_goroutines_runnable_goroutines`      | gauge        | 等待中的可运行 goroutine       |
| `go_sched_goroutines_goroutines`               | gauge        | 当前 goroutine 总数            |
| `go_sched_goroutines_created_goroutines_total` | counter      | 累计创建过的 goroutine         |
| `go_sched_goroutines_waiting_goroutines`       | gauge        | 等待中的 goroutine（不可运行） |
| `go_sched_latencies_seconds`                   | distribution | goroutine 调度延迟             |
| `go_sched_pauses_stopping_gc_seconds`          | distribution | STW 暂停时长（GC 停止）        |
| `go_sched_pauses_stopping_other_seconds`       | distribution | STW 暂停时长（其他停止）       |
| `go_sched_pauses_total_gc_seconds`             | distribution | GC 暂停总时长                  |
| `go_sched_pauses_total_other_seconds`          | distribution | 其他暂停总时长                 |
| `go_sched_threads_total_threads`               | counter      | 累计创建过的 OS 线程           |
| `go_sync_mutex_wait_total_seconds_total`       | counter      | goroutine 等 mutex 总时长      |

### CGO 指标

| 指标                         | 类型    | 说明           |
| ---------------------------- | ------- | -------------- |
| `go_cgo_go_to_c_calls_total` | counter | Go 调 C 总次数 |

---

## 进程指标

由 Prometheus `process` 收集器暴露（非 Go 特有）：

### CPU 与内存

| 指标                               | 类型    | 说明                      |
| ---------------------------------- | ------- | ------------------------- |
| `process_cpu_seconds_total`        | counter | CPU 总时间（用户 + 系统） |
| `process_resident_memory_bytes`    | gauge   | RSS（实际使用物理内存）   |
| `process_virtual_memory_bytes`     | gauge   | 已分配虚拟内存            |
| `process_virtual_memory_max_bytes` | gauge   | 虚拟内存上限              |

### 文件描述符

| 指标               | 类型  | 说明               |
| ------------------ | ----- | ------------------ |
| `process_open_fds` | gauge | 已打开文件描述符数 |
| `process_max_fds`  | gauge | 文件描述符上限     |

### 进程信息

| 指标                         | 类型  | 说明                    |
| ---------------------------- | ----- | ----------------------- |
| `process_start_time_seconds` | gauge | 进程启动时刻（Unix 秒） |

### 缺页

| 指标                              | 类型    | 说明     |
| --------------------------------- | ------- | -------- |
| `process_page_faults_total`       | counter | 缺页总数 |
| `process_page_faults_minor_total` | counter | 次要缺页 |
| `process_page_faults_major_total` | counter | 主要缺页 |

---

## 常用 PromQL 查询

### 内存泄漏检测

```promql
# 当前堆分配（恒定负载下应平稳）
go_memstats_alloc_bytes

# 存活堆字节数（可选指标）
go_gc_heap_live_bytes

# 堆增长速率
rate(go_memstats_alloc_bytes_total[5m])
```

### GC 压力

```promql
# 最坏 GC 暂停（quantile 1 = 最大值）
go_gc_duration_seconds{quantile="1"}

# 平均 GC 暂停
rate(go_gc_duration_seconds_sum[5m]) / rate(go_gc_duration_seconds_count[5m])

# GC 频率（周期/秒）
rate(go_gc_duration_seconds_count[5m])
```

### Goroutine 泄漏

```promql
# 当前 goroutine 数
go_goroutines

# goroutine 增长（泄漏信号）
delta(go_goroutines[1h])
```

### CPU 使用

```promql
# 消耗的 CPU 时间
rate(process_cpu_seconds_total[5m])

# CPU 利用率（0-1）
rate(process_cpu_seconds_total[5m]) / <GOMAXPROCS>
```

### 文件描述符泄漏

```promql
# FD 增长
delta(process_open_fds[1h])

# FD 饱和度
process_open_fds / process_max_fds
```

→ 这些查询可直接在 Prometheus UI 上执行，或用 `promtool query instant <prometheus-url> '<expr>'` 从 CLI 对你的 Prometheus 实例验证。

## 参考资料

- [prometheus/client_golang collectors](https://github.com/prometheus/client_golang/tree/main/prometheus/collectors)
- [Go runtime/metrics package](https://pkg.go.dev/runtime/metrics)
