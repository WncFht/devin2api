# config.yaml 热重载

`POST /admin/config/reload`（Bearer `dashboard.password`）重读 config.yaml 并尽量热应用；校验失败返回 422、旧配置继续服役（validate-then-commit）。
响应的 `applied` 列已生效字段，`requires_restart` 列要重启进程才生效的字段；两者都只报值发生变化的键。
`GET /admin/config` 返回脱敏后的生效视图，`stale=true` 表示文件在最后一次加载后被改过。

## 当前分界

热键的共同特征：读侧每次请求取快照（model/aliases/client_*）、有专门的运行时 setter（闸门参数、debug 开关与保留策略、auth.api_key、dashboard.password）、或把烤死它的对象整体重建后原子换指针（端点三件套进 adapter 的 upstreamLink 与面板的 panelUpstream，quota ticker 经 SetQuotaInterval 重起，pprof listener 经 applyPprofListen 换绑，max_concurrency 走 CAS 计数器）。`auth.api_key` 另有一条播种语义：每次 reload（不止值变化时）若令牌仓内没有对应哈希行，它被补种成普通令牌行——删掉种子行后 reload/重启会重新长出，彻底移除要清空配置值再删行。
冷键只剩 server.listen：Serve 无法换绑端口，同一问题的更难版本（换进程）已由 reuseport 交接部署解决，进程内换监听收益小、排空语义一样绕不过。

| 热应用（applied）                                                                                 | 需重启（requires_restart） |
| ------------------------------------------------------------------------------------------------- | -------------------------- |
| devin.model / devin.aliases / devin.client_name / client_version / client_os                      | server.listen              |
| devin.base_url / devin.proxy / devin.force_http1                                                  |                            |
| devin.accounts（声明基座；与面板行叠加出的生效集驱动 lane 增删改，含逐号 priority/max_rpm）       |                            |
| devin.max_rpm 及 devin.gate_* 全部闸门参数                                                        |                            |
| devin.session_affinity_ttl_seconds / devin.quota_low_threshold_percent                            |                            |
| devin.warm_prefix_* 全部保温参数（总开关热更即时停/启调度循环）                                   |                            |
| auth.api_key / dashboard.password                                                                 |                            |
| debug.enabled / debug.retention_*（retention_days、max_total_mb、payload_hours、keep_error_dirs） |                            |
| debug.quota_interval_minutes / debug.pprof_listen                                                 |                            |
| server.max_concurrency                                                                            |                            |

端点三件套的热更语义：ApplyConfig 先用新参数构建整个上游调用束（transport + stream/api client + 焐池 warmer），构建失败（如非法 proxy）整单 422、旧配置继续服役；构建成功才换 config 快照并原子换指针。在途调用持旧 link 跑完，旧 transport 只收 idle 池；换 base_url 还会清空 AssignModel 缓存（jwt 绑 cascade_id，旧端点的解析对新上游无效）。面板经 `SetUpstream` 跟随同一端点，面板的展示地址读 `BaseURL()` 同源透出。
注意 `devin.client_*` 只影响 chat 路径：面板自身的 seat 类上游调用固定用 windsurf 身份，不随这个键变。

`devin.accounts` 的 reload 语义建立在生效集上：每次重载先把 config 声明与 `upstream_accounts` 行合并（活行覆盖同名声明、墓碑压住声明、disabled 摘出 lane 集），再对生效 lane 集按名做集合 diff——同名 lane 复用旧 adapter 走 ApplyConfig（token 与凭据来源是热换值字段，保温谱系、assignment 与目录缓存、在途流全保住）；新名 lane 先构建再入列；被删/disabled/墓碑 lane 摘出后异步 Close，只停后台协程、在途流持引用跑完（与端点换绑同一生死模型）。lane 名序变化时报 `devin.accounts`，同名 lane 的字段差集仍按各 lane 差集并集进 `applied`（凭据差集以 `devin.accounts.<name>.token` 名义出现）；空生效集合法（空池）。面板的建/改/删/复活/停启用走同一条「合并 → 校验 → ApplyConfigs」路径（行先落库、重推失败回滚行），reload 成功后顺带 GC config 已撤名的死墓碑。任一 lane 构建/应用失败整单 422、已应用 lane 不回滚，与单 lane ApplyConfig 的失败语义一致。号池行为口径见 `devin-accounts.md`。

## 面板覆盖恒赢文件

`reloadRuntimeConfig` 的顺序是先按文件值 `SetEnabled`/`SetPolicy` 重置，再重放 `settings` 表（`devin-2api.db`）里登记的键（`ApplyAll`）。
所以凡是面板 settings 页暴露过的键（debug_log_enabled、log_retention_days、log_max_total_mb、log_payload_hours、log_keep_error_dirs、auto_refresh_interval_seconds），面板值永远压过 config.yaml——文件改了同名字段也不会生效，直到面板侧 reset。
这个「面板赢」的不变量是给未来加键时的硬约束：新热键若同时进面板设置表，reload 路径必须先文件、后重放，顺序不能反。

## 以后加新热键的步骤

1. 确认字段的运行时持有者可变：快照读取（如 adapter.config）加一对字段比较即可；烤进 transport/ticker/listener 的字段参考既有先例改造持有者——端点三件套是「重建调用束 + 原子换指针」（devin.go 的 upstreamLink、ccpanel 的 panelUpstream），ticker 是「cancel 重起」（SetQuotaInterval），listener 不做（见下）。
2. 在 `reloadRuntimeConfig`（cmd/devin-2api/main.go）里加 prev/next 比较与 setter 调用，字段名进 `applied`。
3. 若字段同时想进面板设置页，在 `internal/ccpanel/settings.go` 的键表登记并在 ApplyAll 里接 setter——登记即获得「面板赢」语义，不需要额外代码。
4. 验证：`curl -X POST -H "Authorization: Bearer <pw>" localhost:<port>/admin/config/reload` 看 applied 列表；`GET /admin/config` 看生效视图。

## 已排除的方向

`server.listen` 热更意味着关旧 listener 开新的——reuseport 交接流程（deploy.sh）已经解决了同一问题的更难版本（换进程），进程内换监听收益小、排空语义却一样绕不过，不做。
