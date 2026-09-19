# 通用调试方法论

任何 bug 都按这套系统化流程走：

## 目录

- [第 1 步：弄清预期与实际的差异](#第-1-步弄清预期与实际的差异)
- [第 2 步：拿到完整错误](#第-2-步拿到完整错误)
- [第 3 步：隔离问题](#第-3-步隔离问题)
- [第 4 步：检查外部依赖](#第-4-步检查外部依赖)
- [第 5 步：检查可观测性工具](#第-5-步检查可观测性工具)
- [第 6 步：与能工作的代码对比](#第-6-步与能工作的代码对比)
- [第 7 步：提出假设并检验](#第-7-步提出假设并检验)
- [第 8 步：追到根因](#第-8-步追到根因)
- [第 9 步：修复并验证](#第-9-步修复并验证)
- [第 10 步：纵深防御](#第-10-步纵深防御)
- [卡住时：升级协议](#卡住时升级协议)

## 第 1 步：弄清预期与实际的差异

动代码之前先说清楚：

- **应该**发生什么？
- **实际**发生了什么？
- 最近**改了**什么？

```bash
# 最近改了什么？
git log --oneline -20
git diff HEAD~5

# 二分查找引入 bug 的提交
git bisect start
git bisect bad          # 当前提交是坏的
git bisect good abc123  # 这个提交是好的
# git bisect 会带你走到引入 bug 的提交
```

## 第 2 步：拿到完整错误

```bash
# 完整构建错误
go build ./... 2>&1

# 详细测试输出
go test ./... -v 2>&1

# 静态分析
go vet ./...

# 跑 linter——配置见 golang-lint skill
golangci-lint run ./...
```

在调试工作流早期就跑 `golangci-lint`。它能抓出未检查的错误、可疑构造和许多读代码容易漏掉的问题。配置与用法见 `samber/cc-skills-golang@golang-lint` skill。

## 第 3 步：隔离问题

深入之前先收窄范围：

```bash
# 单个测试失败吗？
go test -run TestSpecificName -v ./pkg/...

# 不用缓存还失败吗？
go test -count=1 -run TestSpecificName ./pkg/...

# 是特定包的问题吗？
go build ./pkg/suspect/...

# 不稳定吗？多跑几次
go test -count=10 -run TestSuspect ./pkg/...
```

怀疑缺测试用例或需要在不同条件下验证时，多写测试。

## 第 4 步：检查外部依赖

有时 bug 不在你的代码里。深入之前先验证外部组件行为符合预期：

```bash
# 在应用之外复现一次 API 调用
curl -v -X POST https://api.example.com/endpoint \
  -H "Content-Type: application/json" \
  -d '{"key": "value"}'

# 直接查数据库内容
psql -h localhost -U myuser -d mydb -c "SELECT * FROM orders WHERE id = 123"
# 或：mysql、mongosh、redis-cli 等
# 或用数据库 MCP server 交互查询

# 测连通性与 DNS 解析
dig api.example.com
nc -zv api.example.com 443

# 外部服务到底有没有响应
curl -o /dev/null -s -w "HTTP %{http_code} in %{time_total}s\n" https://api.example.com/health

# 检查消息队列状态
rabbitmqctl list_queues
# 或：kafka-console-consumer、redis-cli LLEN 等

# 查证书有效期
openssl s_client -connect api.example.com:443 -brief

# 核对环境变量与配置
env | grep DATABASE
env | grep API_KEY
```

**常见外部原因：**

- API 契约变了（新的必填字段、响应结构变了、endpoint 被废弃）
- 数据库 schema 漂移（缺列、类型变了、新约束、migration 没执行）
- 凭据、token 或证书过期/轮换了
- DNS 解析失败或 DNS 缓存过期
- 限流或配额耗尽
- 外部服务降级（响应慢、部分失败、5xx）
- 消息队列满了、消费滞后或正在 rebalance
- 环境间行为不同（staging vs production 配置、feature flag）
- 时钟偏移影响 JWT 校验、缓存 TTL 或定时任务
- TLS/mTLS 配置错误或 CA bundle 不匹配
- 网络策略或防火墙规则变更挡住了流量
- 代理或负载均衡配置错误（后端指错、sticky session、健康检查）
- 磁盘满或文件系统只读
- 文件权限变了
- OOM killer 杀了某个依赖（数据库、缓存、sidecar）
- Docker/K8s：镜像 tag 错了、缺环境变量、资源限额、liveness probe 配置错误
- 第三方 SDK 或库升级带来行为层面的破坏性变更
- 系统间 locale、时区或编码不一致
- 连接池耗尽（数据库、HTTP、gRPC）
- 上游返回了缓存/陈旧数据
- 网络问题。Webhook 或回调 URL 变了或不可达

## 第 5 步：检查可观测性工具

生产排障必须从可观测性数据开始。项目可能已经在用能直接给出答案的可观测性工具——在代码库里找 `prometheus`、`opentelemetry`、`datadog`、`sentry`、`elastic/apm` 这类 import 或依赖。即使代码里看不到，开发者也可能单独部署了。

如果信息缺失，**问用户**他们在用什么监控和可观测性工具。常见栈：

- **Prometheus + Grafana**——面板上可能已经有错误率尖峰、延迟变化、资源饱和。查询示例：

    ```promql
    rate(http_requests_total{status=~"5.."}[5m])           # 错误率
    histogram_quantile(0.99, rate(http_duration_seconds_bucket[5m]))  # p99 延迟
    go_goroutines                                           # 随时间变化的 goroutine 数
    go_memstats_alloc_bytes                                 # 堆分配
    rate(go_gc_duration_seconds_sum[5m])                    # GC 压力
    ```

- **Datadog**——有 APM trace、错误跟踪与基础设施指标。查询示例：

    ```
    avg:trace.http.request.duration{service:myapp} by {resource_name}
    sum:trace.http.request.errors{service:myapp}.as_count()
    avg:runtime.go.num_goroutine{service:myapp}
    ```

- **Sentry**——有捕获到的异常、breadcrumb 与错误分组。Sentry 通常能抓到首次出现时的完整堆栈与上下文。
- **ELK（Elasticsearch + Logstash + Kibana）**——结构化日志可以按错误模式检索：

    ```
    level:error AND service:myapp AND @timestamp:[now-1h TO now]
    ```

- **OpenTelemetry / Jaeger / Zipkin**——分布式 trace 展示跨服务的延迟分解、失败的 span 与传播问题。

如果用户有这些工具的 MCP server（Datadog MCP、Grafana MCP 等），也许可以通过它做交互式查询。

## 第 6 步：与能工作的代码对比

形成假设之前，先找到**能工作**的相似代码：

- 在代码库里搜功能类似但没有这个 bug 的实现
- **完整**读能工作的参考实现——别扫一眼就过
- 列出能工作的代码与坏代码之间的**每处差异**
- 查：依赖一样吗？配置一样吗？初始化顺序一样吗？错误处理一样吗？

看到能工作的版本做法哪里不同时，bug 往往就变得明显了。

## 第 7 步：提出假设并检验

- 提出**单一、具体**、推理清晰的假设
- 加针对性的日志或聚焦的测试
- 改**一处**，观察，确认或推翻
- 假设错了就**回退这次改动**——不要把新修复叠在失败的尝试上

## 第 8 步：追到根因

症状出现在调用栈深处时，不要在错误暴露的地方修。往回追：

1. **找直接原因**——哪一行 panic 或返回了错误值？
2. **问「谁调的它」**——沿调用链往上追一层
3. **继续追**——重复，直到找到非法数据**产生**的地方，而不是它被**消费**的地方
4. **在源头修**——修复属于坏值被造出来的地方，不是它引发崩溃的地方

```go
// 例子：handler 里 panic——但 bug 在构造函数
// ✗ 坏——在症状处修
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
    if s.db == nil {  // nil 检查掩盖了真正的 bug
        http.Error(w, "db unavailable", 500)
        return
    }
    // ...
}

// ✓ 好——在源头修
func NewServer(db *sql.DB) *Server {
    if db == nil {
        panic("NewServer: db must not be nil")  // 构造时快速失败
    }
    return &Server{db: db}
}
```

手动追不动时，加临时插桩：

```go
// 在危险操作前打完整调用链
func suspectFunction(val string) {
    fmt.Fprintf(os.Stderr, "DEBUG suspectFunction: val=%q\n%s\n", val, debug.Stack())
    // ...
}
```

## 第 9 步：修复并验证

- 修根因，不修症状
- 第 1 步写的失败测试现在应该通过
- 跑全量测试查回归

## 第 10 步：纵深防御

修完一个 bug 后问一句：「怎么让这个 bug 在结构上不可能再发生？」单层的单点修复可能被别的代码路径或未来的重构绕过。在多层加校验：

1. **入口**——在公开 API 边界拒绝非法输入（`New*` 构造函数、导出函数）
2. **业务逻辑**——在接收数据的内部函数里断言前置条件
3. **运行时护栏**——用 build tag 或环境检查在测试里抓危险操作（比如拒绝写临时目录之外的路径）
4. **可观测性**——加结构化日志或指标，让同类 bug 复发时立刻可见

不是每个修复都需要全部四层——自己判断。但 bug 可能造成数据丢失、损坏或安全问题时，多层防御值得这个成本。

## 卡住时：升级协议

修不好时：

- **失败 < 3 次：**回到第 1 步。你认错了根因。收集更多证据。
- **失败 >= 3 次：**停止修复。问题多半是架构性的，不是简单 bug。退后一步，质疑你对系统工作方式的假设。问：「设计本身是健全的，还是我在给一个坏抽象打补丁？」
- **每修一处冒一个新问题：**你在追症状，不是根因。见 [SKILL.md](./SKILL.md) 的危险信号一节。
