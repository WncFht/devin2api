# 上游账号池（devin.accounts）

`devin.accounts` 把多个 Devin 上游账号池化成单个 adapter（`internal/adapter/devin/pool.go`）：每个号是一条完整 lane，app/handler 层对「多账号」零感知。本文档记行为口径；字段示例见 `config.example.yaml`，热更语义见 `config-reload.md`。

## 何时启用

单号 `devin.token`（env/credentials.toml 自动发现链）覆盖大多数部署；号池解决单号天花板——上游分钟桶限流与日/周配额都按号计费，多号即多倍配额与突发容量，且某号限流、凭据失效或 seat 受限时请求可换号续跑，而不是整体排队等恢复。

## 配置与校验

`devin.accounts[]` 每项为 `{name, token, credentials_file}`。`accounts` 与 `devin.token` 互斥：accounts 非空时 token 必须留空，且自动发现链（DEVIN_TOKEN / WINDSURF_API_KEY 环境变量、credentials.toml 探测）整体关闭，凭据来源只剩各条目自己。`name` 必填、池内唯一、匹配 `^[A-Za-z0-9_-]{1,32}$`——它进 `runtime_state` 表的 `gate:<name>` 键与日志归因字段；`default` 是保留名（隐式单号 lane 的身份）：若允许复用，模式互转时 reload 差集会把异号同名当「同一 lane 换 token」复用，warm 谱系与 assignment 跨号渗漏。`token`（字面量）与 `credentials_file`（指向 Devin CLI 的 credentials.toml，解析 windsurf_api_key）至少给一个；都给时 token 是初始凭据、credentials_file 是 unauthenticated 自愈来源。credentials_file 支持 `~/` 展开，相对路径锚定到 config.yaml 所在目录而非进程 CWD（launchd 下 CWD=/，按 CWD 解析必死）；加载期就必须能解出 key，路径笔误直接拒绝启动。两条目引用同一有效 token 或同一 credentials_file 等于同号进池两次（限流簿记各自按满额计数、合并超发），按配置错误拒绝。其余 `devin.*` 字段（base_url/model/aliases/proxy/force_http1/`client_*`/max_rpm/`gate_*`/`warm_*`）全局生效，各 lane 共享同一份值。

## lane 隔离边界

每条 lane 是一个完整 adapter 实例，各自持有：token 槽与自愈链（TokenSource——字面量 token 账号重读 config.yaml 按名取值，credentials_file 账号重读该文件跟随 CLI 续期）、速率闸门（闩状态持久化在 `runtime_state` 表 `gate:<name>` 键，重启各自恢复）、前缀保温簿记与调度、AssignModel 缓存与模型目录缓存。隐式单号模式等价于一条名为 `default` 的 lane，沿用 `gate:default` 键保持存量闩状态连续。模型目录按 lane 各自缓存（各号 seat/套餐可不同）：`/v1/models` 返回首个健康 lane 的目录，全失败时回最后一个错误。

## 会话钉选

选号是 rendezvous hashing：对每个 lane 计 `sha256(亲和键|lane名)` 排序取首。亲和键与 trajectory/cascade ID 同种子（SessionKey 优先——CC metadata.user_id、Codex prompt_cache_key；空则回退 system 头 4KB + 首条消息文本头 1KB 的内容哈希），故同一会话恒落同一 lane，trajectory/cascade 派生、warm 谱系、assignment 三个命名空间随会话整体钉在同号上。

## 健康分层与 failover

健康判定只降权不剔除：闸门闩内、分钟桶可发区间外或已满、凭据失效冷却中、非凭据失败短冷却中的 lane 排到候选序尾部但仍在序内——判定是选中前一刻的近似快照，全不健康时回到钉选序，由 lane 自己的闸门走等待或快败（客户端拿 429 + Retry-After，与单号一致）。

failover 只发生在 `lane.Stream` 返回 error 的边界（该边界保证未向客户端提交任何内容，换号重试安全）；流建立后的错误走事件流上报，不换号。可换号的词表按「换号能否改变结果」划分：本地闸门快败、上游限流、传输断裂、`unauthenticated`、`permission_denied` 与一切非客户端可修的错误都换；客户端取消、确定性的请求形状错误（client-fixable）与无 code 的本地确定性失败（投影/参数校验在触达上游之前就炸，换号只会逐 lane 复现同一拒绝）不换。

## lane 失败冷却

lane 内自愈（重读凭据 + 重试）也救不回的 `unauthenticated` 会把该 lane 当前 token 的哈希标记冷却 10 分钟，期间该 lane 在选号中降权。冷却是惰性解禁：TokenSource 重读出不同凭据（CLI 续期、config.yaml 被改）即提前解封——10 分钟只是凭据源永不更新时的兜底解封点，同刻并发标死也不会卡着新 token 不放行。

其余可换号失败（`permission_denied`、传输断裂、本地闸门快败等）不判死凭据，只把 lane 压进 90 秒短冷却：没有它，钉选到惯犯 lane 的会话每个请求都先烧一次注定失败的上游调用再换号。短冷却同样只降权不剔除，到期自动解封、不要求凭据变化信号；同 lane 连续失败只延长不缩短。

## 观测字段

`logs` 表每行带 `account`（产出终局结果的 lane 名——成功开流、终审拒绝或换号穷尽时的最后一号都算，凡触达 lane 的请求恒有值；单号部署恒为 `default`）与 `account_switches`（被试过又放弃的 lane 数，0 即首号出终局）。同 dir 的 `meta.json` 带 `upstream_account`（同口径）与 `upstream_attempts`（被放弃 lane 的有序尝试：account/elapsed_ms/code/message）——failover 救回的请求没有 error.json，这份尝试表是换号归因的唯一痕迹。多 lane 模式下 `04-devin-response.jsonl` 内插 `account_attempt` 分界行：各 lane 的上游帧续写同一流，分界行标明一段帧属于哪号。`quota_samples` 表每号独立成行、带 `account` 列；`GET /admin/quota` 返回 `accounts` 组按名给各自曲线与耗尽预测 + `user` 身份快照（采样顺带取回的 name/email/plan_name，内存态、重启后首个采样点前缺席），顶层 points/daily/weekly 镜像最新采样（freshest）的账号序列保持后兼容。`GET /admin/runtime-metrics` 的 `accounts` 组按名给每号 `gate`、`warm` 与 `lane` 快照——`lane` 是池侧状态：`healthy`（选中判定近似快照）、`auth_cooldown_until`/`unhealthy_until`（两档冷却截止，凭据换新可提前解禁）与 `last_failure_at/code/message`（最近一次换号失败归因）；顶层 `gate`/`warm` 仍是首 lane 快照。stderr 在换号时打 `devin account lane failed, failing over` 告警行（带 account 与 error）。管理面板的 `/web/accounts.html` 页把上述三组数据源汇成逐号视图：全池脉冲条、逐 lane 状态徽章（倒计时语义）、闸门/保温/配额/近 24h 健康格四区。

## 面板边界

面板自身的 seat 类上游调用固定绑配置序首号（TokenFunc/GateStats/WarmStats/CurrentConfig 同口径）；全部 lane 的凭据源已喂给 recentTokens 脱敏环与配额采样。逐号观测走 `/web/accounts.html`（只读）：runtime-metrics 的 accounts 组 + quota 的逐号曲线与身份 + matrix 的逐请求归因。
