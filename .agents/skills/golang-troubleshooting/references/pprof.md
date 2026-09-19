# pprof 参考

## 目录

- [启用 pprof HTTP 服务](#启用-pprof-http-服务)
    - [快速配置（开发）](#快速配置开发)
    - [安全配置（生产）](#安全配置生产)
- [Profile 类型](#profile-类型)
- [采集 Profile](#采集-profile)
- [分析与解读 Profile](#分析与解读-profile)
- [远程 Profiling（生产）](#远程-profiling生产)

## 启用 pprof HTTP 服务

pprof 端点必须用 basic auth 保护——永远不要公开暴露。它们泄漏敏感的 runtime 信息（goroutine 堆栈、内存内容），还可能被用来 DoS 你的服务（CPU profiling 很贵）。pprof 应该通过 `PPROF_ENABLED` 环境变量开关。

### 快速配置（开发）

```go
import _ "net/http/pprof"

func main() {
    go func() {
        log.Println(http.ListenAndServe("localhost:6060", nil))
    }()
    // ... 应用其余部分
}
```

### 安全配置（生产）

生产环境用 basic auth 保护端点：

```go
import "net/http/pprof"

func setupPprof(mux *http.ServeMux) {
    if os.Getenv("PPROF_ENABLED") != "true" {
        return
    }

    // 用 basic auth 保护 pprof 端点——永远不要无认证暴露
    username := os.Getenv("PPROF_USERNAME")
    password := os.Getenv("PPROF_PASSWORD")
    if username == "" || password == "" {
        panic("PPROF_USERNAME and PPROF_PASSWORD must be set when pprof is enabled")
    }
    auth := basicAuth(username, password)

    mux.Handle("/debug/pprof/", auth(http.HandlerFunc(pprof.Index)))
    mux.Handle("/debug/pprof/cmdline", auth(http.HandlerFunc(pprof.Cmdline)))
    mux.Handle("/debug/pprof/profile", auth(http.HandlerFunc(pprof.Profile)))
    mux.Handle("/debug/pprof/symbol", auth(http.HandlerFunc(pprof.Symbol)))
    mux.Handle("/debug/pprof/trace", auth(http.HandlerFunc(pprof.Trace)))

    slog.Info("pprof endpoints enabled (basic auth required)")
}

// basicAuth 给 http.Handler 包一层 HTTP Basic Authentication。
func basicAuth(username, password string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            u, p, ok := r.BasicAuth()
            if !ok || u != username || subtle.ConstantTimeCompare([]byte(p), []byte(password)) != 1 {
                w.Header().Set("WWW-Authenticate", `Basic realm="pprof"`)
                http.Error(w, "unauthorized", http.StatusUnauthorized)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

## Profile 类型

| Profile       | 命令                              | 展示什么                               |
| ------------- | --------------------------------- | -------------------------------------- |
| **CPU**       | `go tool pprof profile`           | CPU 时间花在哪                         |
| **Heap**      | `go tool pprof heap`              | 内存分配、存活对象                     |
| **Goroutine** | `go tool pprof goroutine`         | 所有 goroutine 的堆栈                  |
| **Block**     | `go tool pprof block`             | 阻塞操作（需要 SetBlockProfileRate）   |
| **Mutex**     | `go tool pprof mutex`             | 锁竞争（需要 SetMutexProfileFraction） |
| **Alloc**     | `go tool pprof -alloc_space heap` | 累计分配（不是当前堆）                 |

## 采集 Profile

```bash
# CPU profile 至少应采 30 秒才有意义（默认 30s）。
# 确保 HTTP server 的请求超时大于采集时长。
curl http://localhost:6060/debug/pprof/profile?seconds=30 > cpu.prof

# 堆快照
curl http://localhost:6060/debug/pprof/heap > heap.prof

# goroutine dump（人可读）
curl http://localhost:6060/debug/pprof/goroutine?debug=2 > goroutines.txt

# goroutine profile（供 pprof 分析）
curl http://localhost:6060/debug/pprof/goroutine > goroutine.prof

# goroutine 泄漏 profile——自 Go 1.27 正式可用（不需要 GOEXPERIMENT）
curl http://localhost:6060/debug/pprof/goroutineleak?debug=2
go tool pprof http://localhost:6060/debug/pprof/goroutineleak

# mutex 竞争
curl http://localhost:6060/debug/pprof/mutex > mutex.prof

# block profile
curl http://localhost:6060/debug/pprof/block > block.prof
```

## 分析与解读 Profile

→ 解读 profile——`top`、`list`、`peek`、常见 profile 模式（flat vs cum、GC 抖动、内存泄漏）与编译器诊断——见 `samber/cc-skills-golang@golang-benchmark` skill（pprof.md）。逃逸分析与内联决策另见 compiler-analysis.md。

**快速上手：**

```bash
go tool pprof cpu.prof          # 交互式分析
go tool pprof -http=:8080 cpu.prof  # 图形化火焰图
go tool pprof -base heap1.prof heap2.prof  # 对比堆快照
```

## 远程 Profiling（生产）

生产服务器把 `localhost:6060` 换成你的服务器地址，并带上 basic auth 凭据。

**安全注意：**空闲的 pprof 端点开销很低，但采集 profile 不是免费的。CPU profiling 按请求时长采样，heap profile 可能触发额外工作，block/mutex profile 开启后会增加 runtime 开销。

---

→ 用 Pyroscope 做持续 profiling 见 `samber/cc-skills-golang@golang-observability` skill。调查会话搭建与基于 Prometheus 的性能跟踪见 `samber/cc-skills-golang@golang-benchmark` skill。
