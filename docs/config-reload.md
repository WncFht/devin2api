# config.yaml 热重载

`POST /panel/api/config/reload`（Bearer `dashboard.password`）重读 config.yaml 并尽量热应用；校验失败返回 422、旧配置继续服役（validate-then-commit）。
响应的 `applied` 列已生效字段，`requires_restart` 列要重启进程才生效的字段；两者都只报值发生变化的键。
`GET /panel/api/config` 返回脱敏后的生效视图，`stale=true` 表示文件在最后一次加载后被改过。

## 当前分界

热键的共同特征：读侧每次请求取快照（model/aliases/client_*）、或有专门的运行时 setter（token、闸门参数、debug 开关与保留策略、auth.api_key、dashboard.password）。
冷键的共同特征：值在启动时烤进了不在重载面上的对象——transport（base_url/proxy/force_http1 进了 http.Client）、listener（server.listen）、srv 配置（max_concurrency 是 HTTPServer 字段）、定时器周期（quota_interval_minutes 是已启动的 ticker）。

| 热应用（applied）                                                                                 | 需重启（requires_restart）                        |
| ------------------------------------------------------------------------------------------------- | ------------------------------------------------- |
| devin.model / devin.aliases / devin.client_name / client_version / client_os                      | devin.base_url / devin.proxy / devin.force_http1  |
| devin.token                                                                                       | server.listen / server.max_concurrency            |
| devin.max_rpm 及 devin.gate_* 全部闸门参数                                                        | debug.quota_interval_minutes / debug.pprof_listen |
| devin.warm_prefix_* 全部保温参数（总开关热更即时停/启调度循环）                                   |                                                   |
| auth.api_key / dashboard.password                                                                 |                                                   |
| debug.enabled / debug.retention_*（retention_days、max_total_mb、payload_hours、keep_error_dirs） |                                                   |

注意 `devin.client_*` 只影响 chat 路径：面板自身的 seat 类上游调用固定用 windsurf 身份，不随这个键变。

## 面板覆盖恒赢文件

`reloadRuntimeConfig` 的顺序是先按文件值 `SetEnabled`/`SetPolicy` 重置，再重放 `panel-settings.json` 里登记的键（`ApplyAll`）。
所以凡是移植面板 settings 页暴露过的键（debug_log_enabled、log_retention_days、log_max_total_mb、log_payload_hours、log_keep_error_dirs、auto_refresh_interval_seconds），面板值永远压过 config.yaml——文件改了同名字段也不会生效，直到面板侧 reset。
这个「面板赢」的不变量是给未来加键时的硬约束：新热键若同时进面板设置表，reload 路径必须先文件、后重放，顺序不能反。

## 以后加新热键的步骤

1. 确认字段的运行时持有者可变：快照读取（如 adapter.config）加一对字段比较即可；烤进 transport/listener/ticker 的字段要么改造持有者（重建 transport 涉及在途连接与连接池排空，成本高），要么老实进 requires_restart。
2. 在 `reloadRuntimeConfig`（cmd/devin-2api/main.go）里加 prev/next 比较与 setter 调用，字段名进 `applied`。
3. 若字段同时想进面板设置页，在 `internal/ccpanel/settings.go` 的键表登记并在 ApplyAll 里接 setter——登记即获得「面板赢」语义，不需要额外代码。
4. 验证：`curl -X POST -H "Authorization: Bearer <pw>" localhost:<port>/panel/api/config/reload` 看 applied 列表；`GET /panel/api/config` 看生效视图。

## 已排除的方向

`server.listen` 热更意味着关旧 listener 开新的——reuseport 交接流程（deploy.sh）已经解决了同一问题的更难版本（换进程），进程内换监听收益小、排空语义却一样绕不过，不做。
transport 三件套（base_url/proxy/force_http1）热更要做连接池迁移，同理搁置；真改就重启。
