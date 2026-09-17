# 上游账号池（devin.accounts）

`devin.accounts` 把多个 Devin 上游账号池化成单个 adapter（`internal/adapter/devin/pool.go`）：每个号是一条完整 lane，app/handler 层对「多账号」零感知。本文档记行为口径；字段示例见 `config.example.yaml`，热更语义见 `config-reload.md`，面板端点契约见 `notes` 里的 accounts schema（冻结稿）。

## 何时启用

空池、单号、多号同一条路径——单号部署就是 `accounts` 里声明一条，空池也合法：不配任何号照常起服务，`/v1` 一律 `unavailable` 定失败，面板 `/web/accounts.html` 引导加号，凭据到位即热上线（「先起服务后配号」）。池解决单号天花板——上游分钟桶限流与日/周配额都按号计费，多号即多倍配额与突发容量，且某号限流、凭据失效或 seat 受限时请求可换号续跑，而不是整体排队等恢复。

## 生效集：config 声明 + 面板覆盖

账号的权威集是两层叠加：config.yaml 的 `devin.accounts` 是声明基座（永不落库、不被播种），面板写操作落 `upstream_accounts` 表成行。生效集 = config 声明 ∪ 活行 − 墓碑；同名时行逐字段覆盖声明值（行 `token`/`credentials_file` 非空即用行值），`disabled` 恒取行值。`/admin/accounts` 把每条生效项标 `source`：`config`（声明项，可有覆盖行）、`panel`（仅面板行）、`tombstoned`（面板删掉的 config 名——`deleted=1` 行压住声明，`POST .../restore` 复活）；config 撤名后残留的死墓碑不进列表，由下次生效集重推顺带 GC。面板改 config 名生成覆盖行（`has_override:true`，面板值恒赢、改空串回落声明值）；`disabled:true` 从 lane 集摘除（lane 异步排空 Close、在途流跑完），身份与闸门/配额历史保留——停用不是删除。面板端点一览：`POST /admin/accounts` 建号、`PUT /admin/accounts/{name}` 改凭据/停启用（指针字段，显式空串=清覆盖）、`DELETE`（config 名→墓碑，panel 名→物理删）、`POST .../restore`、`POST .../clear-cooldown`（清池侧两档冷却）、`POST .../quota/refresh`（单号即采）、`GET .../cli-credentials`（CLI 凭据发现链探针，只报存在性不回内容）。

## 配置与校验

`devin.accounts[]` 每项为 `{name, token, credentials_file}`。`name` 必填、池内唯一、匹配 `^[A-Za-z0-9_-]{1,32}$`——它进 `runtime_state` 表的 `gate:<name>` 键与日志归因字段（号池前的存量日志行 account 为空串，读侧统一折叠进 `default` 桶，故 `default` 不再是保留名）。`token`（字面量）与 `credentials_file`（指向 Devin CLI 的 credentials.toml，解析 windsurf_api_key）至少给一个；都给时 token 是初始凭据、credentials_file 是 unauthenticated 自愈来源。credentials_file 支持 `~/` 展开，相对路径锚定到 config.yaml 所在目录而非进程 CWD（launchd 下 CWD=/，按 CWD 解析必死）；加载期就必须能解出 key，路径笔误直接拒绝启动。两条目引用同一有效 token 或同一 credentials_file 等于同号进池两次（限流簿记各自按满额计数、合并超发），按配置错误拒绝。面板建号/改号对合并后的整表跑同一份校验，重名（含墓碑名，须先 restore）、凭据缺失、撞凭据都在写入前拒绝。其余 `devin.*` 字段（base_url/model/aliases/proxy/force_http1/`client_*`/max_rpm/`gate_*`/`warm_*`）全局生效，各 lane 共享同一份值。

## lane 隔离边界

每条 lane 是一个完整 adapter 实例，各自持有：token 槽与自愈链（TokenSource——字面量来源重解生效集该名的当前凭据，config 改写与面板覆盖都算数；credentials_file 来源重读该文件跟随 CLI 续期）、速率闸门（闩状态持久化在 `runtime_state` 表 `gate:<name>` 键，重启各自恢复，disabled/tombstoned 摘出后键位仍留）、前缀保温簿记与调度、AssignModel 缓存与模型目录缓存。模型目录按 lane 各自缓存（各号 seat/套餐可不同）：`/v1/models` 返回首个健康 lane 的目录，全失败时回最后一个错误，空池显式回 `unavailable`。

## 会话钉选

选号是 rendezvous hashing：对每个 lane 计 `sha256(亲和键|lane名)` 排序取首。亲和键与 trajectory/cascade ID 同种子（SessionKey 优先——CC metadata.user_id、Codex prompt_cache_key；空则回退 system 头 4KB + 首条消息文本头 1KB 的内容哈希），故同一会话恒落同一 lane，trajectory/cascade 派生、warm 谱系、assignment 三个命名空间随会话整体钉在同号上。

## 健康分层与 failover

健康判定只降权不剔除：闸门闩内、分钟桶可发区间外或已满、凭据失效冷却中、非凭据失败短冷却中的 lane 排到候选序尾部但仍在序内——判定是选中前一刻的近似快照，全不健康时回到钉选序，由 lane 自己的闸门走等待或快败（客户端拿 429 + Retry-After，与单号一致）。

failover 只发生在 `lane.Stream` 返回 error 的边界（该边界保证未向客户端提交任何内容，换号重试安全）；流建立后的错误走事件流上报，不换号。可换号的词表按「换号能否改变结果」划分：本地闸门快败、上游限流、传输断裂、`unauthenticated`、`permission_denied` 与一切非客户端可修的错误都换；客户端取消、确定性的请求形状错误（client-fixable）与无 code 的本地确定性失败（投影/参数校验在触达上游之前就炸，换号只会逐 lane 复现同一拒绝）不换。

## lane 失败冷却

lane 内自愈（重读凭据 + 重试）也救不回的 `unauthenticated` 会把该 lane 当前 token 的哈希标记冷却 10 分钟，期间该 lane 在选号中降权。冷却是惰性解禁：TokenSource 重读出不同凭据（CLI 续期、面板改号、config.yaml 被改）即提前解封——10 分钟只是凭据源永不更新时的兜底解封点，同刻并发标死也不会卡着新 token 不放行。面板 `POST /admin/accounts/{name}/clear-cooldown` 人工清掉两档冷却与判死键立回候选（保留 `last_failure_*` 证据，不动 gate 闩）。

其余可换号失败（`permission_denied`、传输断裂、本地闸门快败等）不判死凭据，只把 lane 压进 90 秒短冷却：没有它，钉选到惯犯 lane 的会话每个请求都先烧一次注定失败的上游调用再换号。短冷却同样只降权不剔除，到期自动解封、不要求凭据变化信号；同 lane 连续失败只延长不缩短。

## 观测字段

`logs` 表每行带 `account`（产出终局结果的 lane 名——成功开流、终审拒绝或换号穷尽时的最后一号都算，凡触达 lane 的请求恒有值）与 `account_switches`（被试过又放弃的 lane 数，0 即首号出终局）；`/admin/logs?account=<name>` 与逐号聚合按 `COALESCE(NULLIF(account,''),'default')` 折叠——`default` 参数命中号池前时代的空串行与真名 default 行两群。同 dir 的 `meta.json` 带 `upstream_account`（同口径）与 `upstream_attempts`（被放弃 lane 的有序尝试：account/elapsed_ms/code/message）——failover 救回的请求没有 error.json，这份尝试表是换号归因的唯一痕迹。多 lane 模式下 `04-devin-response.jsonl` 内插 `account_attempt` 分界行：各 lane 的上游帧续写同一流，分界行标明一段帧属于哪号。`quota_samples` 表每号独立成行、带 `account` 列；`GET /admin/quota` 返回 `accounts` 组按名给各自曲线与耗尽预测 + `user` 身份快照（采样顺带取回的 name/email/plan_name，内存态、重启后首个采样点前缺席），顶层 points/daily/weekly 镜像最新采样（freshest）的账号序列保持后兼容。`GET /admin/runtime-metrics` 的 `accounts` 组按名给每号 `gate`、`warm` 与 `lane` 快照——`lane` 是池侧状态：`healthy`（选中判定近似快照）、`auth_cooldown_until`/`unhealthy_until`（两档冷却截止，凭据换新可提前解禁）与 `last_failure_at/code/message`（最近一次换号失败归因）；顶层 `gate`/`warm` 仍是首 lane 快照。stderr 在换号时打 `devin account lane failed, failing over` 告警行（带 account 与 error）。`GET /admin/accounts` 是上述数据源的聚合视图：逐号给身份（source/disabled/凭据种类/token_sha 指纹——明文永不出端点）、lane/gate/warm 快照、在途数与配额摘要；disabled/tombstoned 项 lane/gate/warm 为 null 但 inflight 仍报实际值。

## 面板边界

面板自身的 seat 类上游调用固定绑配置序首条活 lane（TokenFunc/GateStats/WarmStats/CurrentConfig 同口径；空池回零值，seat 调用拿 401 即「还没配号」的引导态）；全部 lane 的凭据源已喂给 recentTokens 脱敏环与配额采样。`/web/accounts.html` 是完整管理面——建号、改凭据、停启用、删除/复活、清冷却、单号刷配额都走它，观测则同页汇入 runtime-metrics 的 accounts 组 + quota 的逐号曲线与身份 + matrix 的逐请求归因。
