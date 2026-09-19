# 生产排障

## 目录

- [生产排障清单](#生产排障清单)
    - [第 1 步：立即采集（别重启！）](#第-1-步立即采集别重启)
    - [第 2 步：系统指标](#第-2-步系统指标)
    - [第 3 步：本地分析](#第-3-步本地分析)
- [日志与可观测性](#日志与可观测性)
    - [有策略地放日志](#有策略地放日志)
    - [结构化日志（Go 1.21+）](#结构化日志go-121)
    - [Request ID 追踪](#request-id-追踪)
- [网络与 HTTP 调试](#网络与-http-调试)
    - [HTTP 客户端问题](#http-客户端问题)

## 生产排障清单

被 oncall 叫起来查生产问题时：

### 第 1 步：立即采集（别重启！）

重启进程前先把所有 profile 采下来。[pprof.md](./pprof.md) 里的 curl 命令可以直接指向你的生产服务器地址。至少采：goroutine dump（`?debug=2`）、heap、CPU（30s）和 mutex profile。

### 第 2 步：系统指标

```bash
ps aux | grep myapp
lsof -p PID | wc -l       # 文件描述符
ss -s                       # socket 汇总
netstat -an | grep ESTABLISHED | wc -l
```

### 第 3 步：本地分析

把采到的 `.prof` 文件下载下来，用 `go tool pprof` 分析（见 [pprof.md](./pprof.md)）。

---

## 日志与可观测性

### 有策略地放日志

日志放在**组件边界**，不要随意撒。目标是看到数据进出每一层，这样能精确定位是哪个组件损坏或丢掉了它：

```go
// 1. 函数出入口带关键参数
func ProcessOrder(ctx context.Context, orderID string) error {
    log.Printf("ProcessOrder: start orderID=%s", orderID)
    defer log.Printf("ProcessOrder: done orderID=%s", orderID)
    // ...
}

// 2. 外部调用前后
log.Printf("calling payment API for order %s", orderID)
resp, err := paymentClient.Charge(ctx, req)
if err != nil {
    log.Printf("payment API: err=%v", err)
} else {
    log.Printf("payment API: status=%d", resp.StatusCode)
}

// 3. 决策点
if user.IsAdmin {
    log.Printf("admin path for user %s", user.ID)
}
```

### 结构化日志（Go 1.21+）

```go
import "log/slog"

slog.Info("processing request",
    "method", r.Method,
    "path", r.URL.Path,
    "user_id", userID,
)

slog.Error("database query failed",
    "err", err,
    "query", query,
    "duration_ms", elapsed.Milliseconds(),
)
```

### Request ID 追踪

```go
type ctxKey string

func WithRequestID(ctx context.Context, id string) context.Context {
    return context.WithValue(ctx, ctxKey("request_id"), id)
}

func RequestID(ctx context.Context) string {
    id, _ := ctx.Value(ctxKey("request_id")).(string)
    return id
}
```

---

## 网络与 HTTP 调试

### HTTP 客户端问题

```go
// 1. HTTP client 必须设超时——默认 http.Client 没有超时
client := &http.Client{
    Timeout: 30 * time.Second,
    Transport: &http.Transport{
        DialContext:          (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
        TLSHandshakeTimeout: 5 * time.Second,
        IdleConnTimeout:     90 * time.Second,
        MaxIdleConns:        100,
        MaxIdleConnsPerHost: 10,
    },
}

// 2. response body 必须关
resp, err := client.Do(req)
if err != nil {
    return err
}
defer resp.Body.Close()

// 3. 错误状态码时读 body（拿服务端的错误信息）
if resp.StatusCode >= 400 {
    body, _ := io.ReadAll(resp.Body)
    return fmt.Errorf("API error %d: %s", resp.StatusCode, body)
}

// 4. dump 完整请求/响应做调试
import "net/http/httputil"
dump, _ := httputil.DumpRequestOut(req, true)
log.Printf("request:\n%s", dump)
dump, _ = httputil.DumpResponse(resp, true)
log.Printf("response:\n%s", dump)
```
