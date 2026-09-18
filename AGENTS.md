# AGENTS.md

## 代码风格规则

这是新项目，按照开源和业界规范组织、编写。因为是初期 api key 等敏感信息允许暴露在文件中，无需 env 等方式隐藏。不要无意义的过度 test，test 端到端的也不一定是 tests/的东西。或许是跑起来。

因迭代快，新功能添加如果需要大面积模块层级文件移动是允许的，不允许“补丁”，应重构、合并时必须做到，以避免膨胀、屎山。

不要写冗余代码和过度防御性编程代码，技术品味包括但不限于 (根据 python 语言举例，同样适用其他语言):

1. 不要对已知类型使用 `getattr`、`bool(x)`、`callable(x)` 等防御写法
2. 不要过度 `isinstance` 检查
3. 不要过度 `try/except`，只在真实边界（外部输入、网络、IO）与核心业务逻辑的关键位置捕获异常
4. 所有方法、函数都需要 docstring，如必要抽象简单方法，则一行 docstring 即可；复杂函数则按照最佳实践写
5. 原有代码若跟用户需求的新功能无关，不要“热心”删除任何空行、comments，除非用户强制要求
6. 使用 minimal code——200 行能写成 50 行就重写
7. 不为当前真实调用链上不可达的场景增加处理。新增 guard、`raise`、`.get()`、`getattr()`、默认值、`None` 分支、`try/except`、retry、fallback 或兼容路径，必须有真实调用者、明确契约或已观察故障作为依据。若必要性无法从当前代码确定，且会改变行为、异常时机或兼容性，先说明触发条件和被掩盖的前置问题，由用户决定是否支持、拒绝或清理该场景。
8. 新增的项目自有标识符禁止使用前导下划线命名；仅 Python 协议、框架强制 hook、现有接口的准确 override 可以例外。不得自行发明 dunder 名称，也不得用私有命名隐藏职责不清的代码。
9. 类名和函数名应使用具体、可理解的业务名称，使读者能从调用处判断其对象、动作和职责；避免职责不明的通用命名以及只转发调用的 wrapper。
10. 注释应帮助读者理解非显然的所有权、数据来源、执行顺序和跨模块交接，说明“来自哪里、为什么、交给谁”，不要复述代码本身。
11. 不允许过度抽象，如果一个函数只是转发调用而不封装非平凡逻辑，它就不该存在，一个“核心业务函数”在允许的情况下，写 500 行也是可以的，因为 self-contained；不要为一次性代码创建抽象；十行相似代码也好过一个过早出现的抽象，代码量是负债，不是资产（检验标准：资深工程师会觉得这一大片代码阅读压力大吗？这个抽象是否只是移动了复杂度而非减少？任一为是，简化。）
12. 以认知连贯性为优先：能从上读到下就理解完整流程，胜过结构"整洁"但需要反复跳转的代码。线性控制流不要拆散到多处。（检验标准：读懂这一大段功能所有相关联逻辑需要跳几个文件/函数？）

## 架构原则

本节原则适用于你正在编写或被明确要求重构的代码，不是主动清理现有代码的授权。

结构重构（等价变换）与语义变更（改变行为）必须分离。删除或迁移任何能力前，先确认没有调用者仍然依赖它。

好的重构让下一个读者更快看懂，降低阅读理解成本；如果只是换了个地方藏复杂度，不如不动。若重构过程中涉及行为变更，必须显式指出差异，由用户决定是否接受。

可证明的语义正确性优先于表面整洁。错误应尽早显式暴露，不要静默吞掉让问题漂移到下游。

运用第一性原理 思考，拒绝经验主义和路径盲从，不要假设我完全清楚目标，保持审慎，从原始需求和问题出发，若目标模糊请停下和我讨论，若目标清晰但路径非最优，请直接建议更短、更低成本的办法。

### 复杂性是第一判据

设计的目标是降低复杂性。复杂性只有两个根源：**依赖**（代码无法被独立理解和修改）与**模糊**（重要信息不明显）。日常症状有三种：**变更放大**（小改动要动多处）、**认知负荷**（调用者要背太多知识）、**未知的未知**（不知道该改哪里，危害最大）。复杂性按被接触的频率加权——热路径上的小复杂比冷角落的大复杂更要命。每个模块、抽象、配置项都要求「消除的复杂性大于引入的复杂性」，否则不值得存在。代码为易读而设计，不为易写而设计。（框架出自 Ousterhout《A Philosophy of Software Design》，下面两节同。）

### 危险信号（red flags）

写代码和 review 时对这些信号零容忍，见到就停下来想设计：

- 信息泄漏：同一设计决策散在多个模块里，改一处要同步改多处
- 透传方法：签名照搬下层、不增加功能（调度分发除外）
- 连体方法：必须对照另一段代码才能看懂这一段
- 重复代码；通用机制与专用逻辑混在同一层
- 接口注释被迫描述实现细节——模块太浅
- 一段话说不清职责、起不出准确名字——抽象边界错了
- 评审者说「不明显」——那就是不明显，去澄清，不要争辩

### 设计动作

- **深模块优先**：简单接口后面藏大量功能。看「接口成本 vs 功能收益」，不看模块大小——「类越小越好」是类炎，只会制造接口堆积。
- **接口有点通用**：功能按当前需求做，接口按多种用途设计；只被一处调用的专用方法是信号。
- **复杂性下拉**：能由模块内部消化的复杂性不推给调用者；简单接口比简单实现重要。配置参数本质是复杂性上移，必须提供时自动算默认值。
- **定义不存在的错误**：优先重定义 API 语义让异常情形变成正常情形（如「确保不存在」之于「找不到则报错」），而不是加分支、抛异常、把处理推给上层。
- **设计两次**：重要接口和模块分解至少想两个根本不同的方案，列出取舍再动手；第一个想法几乎不是最优。
- **增量单位是抽象，不是功能**：每次迭代让系统结构更像「一开始就为这个需求设计的」，而不是最小 diff 糊上去。

## 回复用户语气

少用抽象词、空话、夸张修辞、营销口吻、emoji。不用口癖：来个狠的、给你给狠的、狠一点、我直说、大的、直接点说、我会、我不绕、我走、我直接、我最、我抓、顺、落、压、拍板、说白了、硬、软、补一刀、收口。禁止欧式中文。写中文就用中文语序，不要套英语句式。在重要的术语和概念方面（并延展其相关、对偶或者反向的概念），要进行必要的解释，多用联想、类比、对比等进行生动直观又不失准确，严谨地阐述，如果涉及到计算相关的概念；适当补充介绍用户可能的”不知道自己不知道“，但要结合代码探索事实 add on top(Michael Polanyi 哲学思想)

## Commit Messages

提交信息整体使用英文，并遵循 Conventional Commits 格式：

格式：

`<type>[optional scope][!]: <description>`

`[optional body]`

`[optional footer(s)]`

允许的类型：

- feat: 新增功能
- fix: 修复缺陷
- docs: 仅修改文档
- refactor: 既不新增功能也不修复缺陷的代码变更
- perf: 性能优化
- test: 新增或更新测试
- build: 构建系统或依赖变更
- ci: CI 配置变更
- chore: 仓库维护
- revert: 回退之前的提交

规则：

- 使用祈使语气。
- 描述保持简洁，末尾不要加句号。
- 使用稳定的 scope，例如包、子系统或业务领域。
- 在可行的情况下，将标题控制在 72 个字符以内。
- 对于不明显的变更，在正文中说明为什么需要这项变更。
- 对于破坏性变更，使用 `!`，并添加 `BREAKING CHANGE:` footer。
- 对于破坏性变更，附上迁移说明。
- 能提供有用上下文时，使用 `Refs: #123` 等 trailer。
- 每个提交只聚焦一个逻辑变更。
- 在运行过程中，自主创建提交。

## 文档

文献引用一律用脚注：正文在作者 - 年份或论文名后紧跟 `[^key]`（键为小写 ASCII 与连字符，如 `[^zhang20]`；同一文献可多处引用），行内不放链接；文末设 `### 参考文献` 节，逐行写 `[^key]: 作者. 标题. venue 年份. [arXiv:编号](链接)`，编号由渲染器按首次引用自动生成，venue 只写核实过的。

行内公式用 $，行间公式用 $$，表格中的 LaTeX 使用 \mid、\Vert 等命令，避免裸 |；正确使用 LaTeX 语法，最好不要把数学公式放到代码块里面

- 文档分两处，各守各的规矩：
    - `docs/`：随仓库发布的活文档（索引 `docs/README.md`）。行为变更同 commit 更新对应文档，不留过期描述；新增长期参考进 docs/ 并登记索引；不含真实密钥，端口/渠道 id/路径等部署相关值用占位符或标注「本机示例」；一段一行，不硬折行。
    - `notes/`：整体 gitignore 的私有工作区，只放 `archive/YYYY-MM-DD-<slug>.md` 日期快照（禁用 `article.md`、`周报.md`、`*.zh.md` 别名）；一次性调研/事故记录的归宿，结论被 docs/ 吸收后原文冻结不再改。

## 格式化工具链

`*.md` 提交会走 pre-commit：markdownlint-cli2 --fix 原地修规则 → `autocorrect --stdin | prettier` 经 git-format-staged 只写 index（commit 不被格式化阻断，不碰工作区未暂存内容）；`*.go` 走 gofmt（同机制）；`*.yaml`/`*.yml` 走 `scripts/check-yaml-comments.py`（check 类：纯注释行 ≤80 显示列、CJK 按 2 列计；断点取标点/从句边界是人工活，机械重排用 vim `gq`/VS Code Rewrap）。版本以 `package.json` 为准。前置条件：`npm install`、`brew install autocorrect golangci-lint`、`pre-commit install`、系统 `python3`。markdownlint 原地改写文件时会 fail 一次，重新 `git add` 再提交。

改 Go 代码提交前跑 `golangci-lint run`（规则见 `.golangci.yml`：default:none + 显式启用 bodyclose/errcheck/govet/revive/staticcheck/unused），`golangci-lint fmt` 修 gofmt/goimports；CI golangci job 同配置，本地不过 CI 必挂。全量工具链说明见 `docs/toolchain.md`。

## 版本与发布

- 版本号不写进源码：构建期 `-X main.version=$(git describe --tags --always --dirty)` 注入；运行时解析链见 `resolvedVersion`（ldflags → buildinfo → `vcs.revision` → embed `cmd/devin-2api/VERSION` → `"dev"`）。
- `scripts/release.sh` 发版：dry-run 打印分类 changelog；`--publish` 自动回写 VERSION 并推送 → 轮询该提交的 CI 到绿 → 复查 `origin/main` 未被推进 → `git tag -a --cleanup=verbatim` 推送。tag 注解是 release body 的唯一事实源（release.yml 取 `%(contents)`）。
- tag 只打在已推送 `origin/main` 且 CI 绿的提交上；0.x 阶段 feat/破坏性变更升 minor、其余升 patch。`latest` 镜像 tag 只跟随稳定版。**已推送的 tag 永不重打**——release body 出错用 `gh release edit --notes-file` 原地修（详见 release-runbook skill）。
- 改 `scripts/release.sh` 后必跑 `scripts/release-selftest.sh`：bare origin + stub GitHub API 的离线演练，覆盖 dry-run 版本计算与 publish 全部拒绝分支；CI 的 deploy-assets job 同步跑它。
- `scripts/deploy.sh` 本机升级（`--release <tag>` 可装预编译二进制）。

## 服务排障（对运行中的实例）

本服务为 agent 调试设计：`debug.enabled` 开启时每个 `/v1/*` 响应带 `X-Request-Id` 头，值即本次请求的调试身份 `<dir>`（`YYYYMMDD-HHMMSS(-NN)`，同秒并发加后缀）；错误响应体与流式错误事件另含 `debug_ref`（同值），非流式错误体还带 `stage`（写出错误的 HTTP 处理层）。失败的首因分层 stage 以 `error.json`/`logs` 表为准：`devin_transport` 是连接被截断类传输故障（含 connect.Error 包装的 EOF/帧截断），`devin_connect` 是上游语义拒绝（参数/权限/限流），`rate_gate` 是本地速率闸门快败（未触达上游，含续试重打被闩拦），`request_build` 是本地请求投影失败（tool_choice 指空等参数校验，未触达上游），`token_limit`/`model_disabled` 是解码后的下游准入拒绝（下游令牌并发/费用窗口/模型白名单、注册表停用——未触达上游但留有调试记录，区别于管线前拒绝）；客户端断连记 `client_disconnected`，响应未提交时 status 记 499；排空超时强掐的在途请求记 `drain_timeout`（`result=aborted`，与面板中断同词，区别于真实断连——进程侧掐断不污染客户端断连口径）。stderr `request failed` 行的 `stage=` 是捕获点（哪个错误出口写出的响应）、`error_stage=` 才是归原点（与 `logs` 表同名列）——闸门拒绝常在 `provider_stream` 出口被捕获，两层都写在同一行里；本地闸门快败只记 Info 级（预期整形，非故障）。管线前拒绝（鉴权 401 / 并发 429 / 排空 503 / WS 准入 / 读体中断 `http_read` → 504——传输抖动走可重试档而非 400，避免客户端按致命错误杀轮次）不产生调试目录，但落一条 `log_source='rejected'` 的 `logs` 留存行（`dir` 空、`result='rejected'`、`error_stage='pre_pipeline'`、`error_message` 记拒绝原因）——默认列表与全部聚合口径把它剔除，显式 `log_source=rejected`（或 `all`）才可见；唯一例外是请求体超 32MiB 上限：载荷真实到达，按 413 + `http_read` 留调试行供容量排障。管线前拒绝的实时面是 `/admin/runtime-metrics` 的 `rejects`（分原因计数 + 最近事件环），跨重启痕迹在 `stderr.log` 的 `request rejected` 行（reason 同源）与 rejected 留存行。

持久化归 SQLite：状态目录根的 `devin-2api.db`（WAL，伴生 `-wal`/`-shm`）装全部运行时状态——`logs`（每完成请求一行，原 `index.jsonl` 的继任）、`debug_files`+`debug_chunks`（调试 payload，键是 `<dir>` 而非 logs.id——飞行中请求与 413 等提前终断有调试行但无 logs 行，管线前拒绝反过来只落 logs 行、dir 为空）、`log_cells`+`log_err_cells`（logs 的 600s 预聚合 rollup：格子键 (slot,day,api,emodel,key_hash) 带 40 个可加指标列与 min_time/last_key 两个非可加列，err 表是 (slot,error_stage) 稀疏计数；InsertLog/WriteDebugBatch 与 logs 行同事务双写并推进 `runtime_state` 的 `log_cells_covered_id` 水位线——id ≤ 水位的非 rejected 行均已记账，rejected 行内建剔除，`Store.Open` 每次启动跑 `ReconcileCells` 补记任何绕过双写的写入（无 cells 码的旧二进制、外部工具、importIndex），迁移 0006 一次性回填存量）、`lane_attempt_causes`（号池被放弃 lane 尝试的日粒度聚合账，主键 (day,lane,cause)：写方是 logs 行同事务展开——meta.json 的 `upstream_attempts` 明细随目录淘汰后，「为什么换号」只剩这里的口径；cause 词表 `local_gate[:reason]`=本地闸门幻影换号（零上游发送）、connect code=真实 failover 发送、`nocode`=无 code 传输断裂）、`auth_tokens`、`model_registry`、`settings`、`quota_samples`、`runtime_state`、`upstream_accounts`、`schema_migrations`。仍留文件的只有 `logs/stderr.log`/`stdout.log`（进程日志）与 `logs/bind-failure.json`（启动早期端口争夺取证不该依赖 db 健康）；`rejects` 环与进程指标是纯内存瞬态。排障直查用 `sqlite3`——WAL 下并发读不干扰运行实例（习惯上加 `-readonly` 防误写）；payload 导出用 `writefile()`（见工作流第 3 步）。文件时代的 `index.jsonl`/`auth_tokens.json`/`models.json`/`panel-settings.json`/`quota.jsonl`/`gate-state*.json` 在首次启动被一次性导入后改名 `<name>.migrated`，旧调试目录由后台导入器搬入 `debug_*` 表后删除；回滚 = 装回旧二进制 + 把 `.migrated` 文件改回原文件名（`devin-2api.db` 旧二进制不读，可留可删）。

工作流：

1. 失败/可疑请求 → 取响应头 `X-Request-Id` 或错误体 `error.debug_ref` 得到 `<dir>`。
2. 读 meta/error 两条证据：`meta.json`（结果、三段模型、延迟分解 `request_ready/upstream_sent/upstream_open/first_upstream/upstream_done/first_client_ms`、token、upstream_request_id；号池下另有 `upstream_account` 终局 lane、`upstream_attempts` 被放弃 lane 的有序明细与 `pool_candidates` 选号时刻候选序快照（name/healthy/bound/reason））与 `error.json`（首个失败点——号池 failover 救回的请求也会留有首失败 lane 的 error.json，它描述第一次失败而非最终下发结果）。读法二选一：端点 `GET /admin/debug-logs/{id}/file/meta.json`（`{id}` 是 `logs` 表自增主键——先 `GET /admin/logs?q=<dir>` 或 `sqlite3 devin-2api.db "SELECT id FROM logs WHERE dir='<dir>'"` 换出数字 id）；或进程外直查 `sqlite3 devin-2api.db "SELECT content FROM debug_files WHERE dir='<dir>' AND name='meta.json'"`。延迟分解字段的段语义见 `docs/perf.md`；`repairs` 是投影/sanitize 修复计数——CC 流量有 ~15 hits/req 的基线，异常信号是命中规则 id 集合的漂移而非总数涨落。
3. 需要细节再按序读阶段记录：`01-http-request.json`（客户端原文）→ `02-request-messages.json`（中间投影）→ `03-devin-request.json`（上游 wire）→ `04-devin-response.jsonl`（上游原始帧）→ `05/06`（内部事件 / 下发客户端的 SSE）——01/02/03 与 meta/error/attachments 在 `debug_files`（name 存相对路径），04/05/06 流式 JSONL 在 `debug_chunks`（按 seq 拼接）。端点侧 `GET /admin/debug-logs/{id}` 把整套投影成 ccLoad detail（01→`original_*`、03（含 attemptN 重试分片依序拼接）→`req_*`、04→`resp_body`、06→`translated_*`），`/file/{name}` 读单名（超 4MB 截断，`?raw=1` 原样回字节带 CSP sandbox），`/merged` 服务端合并 06 成可读正文。进程外导出用 `writefile()`：`sqlite3 devin-2api.db "SELECT writefile('/tmp/03.json', content) FROM debug_files WHERE dir='<dir>' AND name='03-devin-request.json'"`；分块文件按 seq 拼接后再写：`SELECT writefile('/tmp/04.jsonl', group_concat(data,'')) FROM (SELECT data FROM debug_chunks WHERE dir='<dir>' AND name='04-devin-response.jsonl' ORDER BY seq)`。注意 `02-request-messages.json` 与 `03-devin-request*`（含 attemptN/searchN 分片）入库可为 zstd delta 帧（magic `28B52FFD`，字典是同 dir 01 的解后明文）——writefile() 直出的是未解码库存字节，这两个名字首选 `/file/{name}?raw=1` 端点取解后内容；必须读裸行时用 `store.DecodePayloadFile` 解（`cmd/probe rerun` 的 `--dict` 走同一魔数分派，01 的 gzip 库存形态可直接作 dict 喂入）。01/meta/error/attachments 与全部 `debug_chunks` 名永不 delta，writefile() 配方照旧。`03-devin-request` 词干的 attemptN 序号按 dir 计「第 N 次上游发送」且跨 lane 共享：首个发送占基座名，同 lane 续试（token 自愈/空响应/transport 重开）与号池 failover 后新 lane 的首发都续占 `03-devin-request.attemptN.json` 分片——各次 wire 体不再互覆（服务端托管搜索调用用 `03-devin-request.searchN` 词干另起编号，不占 chat 发送序号）；同 lane 续试在 04 中插入 `retry_attempt` 标记行分隔各次尝试的原始帧，次数与原因另落 `logs` 表的 `retries` 列与 meta.json 的 `retry_attempts`（failover 首发不算续试、不占 retries——换号痕迹走 `account_switches`/`upstream_attempts` 口径）。号池下 04 另插 `account_attempt` 分界行——每条 lane 开流/接管前写一行 `{account}`，同 dir 内多个 lane 的帧据此归属。脱钩缓存（客户端断开后上游流续命进完成缓存，见 upstream-debug-playbook 的脱钩段）留标记行：原 dir 04 的 `detached`（登记时刻 + 已缓冲事件数）与缓冲越 8MiB 预算时的 `detached_truncated`（截断条目对重试一律未命中），重试 dir 04 的 `detached_attach`（`origin_dir` 回指原 dir + 命中时条目态与缓冲量）——挂接请求没有自己的上游帧，其完整响应取证要回 `origin_dir` 读；原 dir 完结后盘上不再追写，后台泵后续帧只在条目缓冲里。
4. 批量检索用 `GET /admin/logs` 的过滤参数或 `sqlite3` 直查 `logs` 表（每完成请求一行摘要，含 `error_stage`/`error_message`（终结性失败才落；调试 payload 被保留策略淘汰后仍可归因）、`conn_reused`/`conn_idle_ms`（成功建流的连接画像）、`client_request_id`、`key_hash`、全部 token 分类、重发次数 `retries`、号池归因 `account`（终局 lane）与 `account_switches`（被放弃 lane 数）、`affinity_hash`（会话谱系亲和键，与 meta.json 同源——谱系/绑定/warm 救援的 GROUP BY 维））——例：找上游拒绝类失败 `sqlite3 devin-2api.db "SELECT dir,error_message FROM logs WHERE error_stage='devin_connect' ORDER BY time DESC LIMIT 20"`。`logs` 行自身受 `debug.log_row_retention_days`（默认 90 天）约束，独立于 payload 保留。
5. 进程级信号看 `logs/stderr.log`（slog 结构化行，每请求一行摘要 + 拒绝/清理告警）；面板数据可用 `curl -H 'Authorization: Bearer <dashboard.password>' localhost:<port>/admin/*` 程序化访问，`/admin/api` 返回端点目录。

聚合与生命周期：

- `GET /admin/usage` 是 `logs` 表的 SQL 聚合（今日/窗口累计、按模型/按 key、错误阶段、8 天 10 分钟粒度趋势、最近 4096 条延迟 p50/p90/p95/p99、错误责任归因 `client_faults`/`upstream_faults` 原始计数（成功率口径由消费方推导）、按模型 token 分位数与目录价估算成本）——聚合读 = 水位内整格 `log_cells`/`log_err_cells` SUM ∪ 水位外行与窗缘不满格原始行补尾的 UNION（语句内水位子查询保证两侧无重无漏，account 维无格子列时回退原始行），与全扫口径等价；快照另带两个非 logs 源段：`sends_per_row`（逐日 `gate_windows` 放行数 used_fg+used_bg ÷ 当日 logs 行——内层 connect 重试对日志不可见，本段是唯一活探针；行内 retry_admits 是放行中同 lane 续试重发的日合计——quota<=0 闸门不记窗行时分子随无窗期缺记、纯探针日 rows=0 ratio 缺省）与 `attempt_causes`（`lane_attempt_causes` 31 天窗直读——区分真实 failover 发送与本地闸门幻影换号）；直查实时库，进程重启不丢口径。
- `GET /admin/runtime-metrics` 的 `process`（运行时长/并发槽/goroutine/堆/GC/CPU/RSS）与 `http_proxy`（活跃/完成/错误/流式计数、收发字节）区分「代理自身瓶颈」与「上游/客户端慢」（RPM/QPS 趋势另见 `GET /admin/stats` 的 `rpm_stats`）；`rejects` 段暴露管线前拒绝（`by_reason` 分原因计数 + `recent` 最近 256 条事件环）；`logs` 段暴露日志管道自观测（写队列积压、在飞字节水位 `pending_bytes` 与其进程期峰值 `pending_bytes_max`、硬顶 `pending_bytes_cap`、丢弃数与丢弃体积 `dropped_payload_bytes`、迟到写计数 `late_writes`——写面已拆后的门口拒收与真丢弃分账、写库失败数、errors_only 开关态）；`gate` 段暴露速率闸门状态（闩态/闩截止/滴灌与快败计数/分钟窗口配额与已放行数/可发区间/排队数 + `events` 闩迁移事件环——上闩/延闩/解闩/到期/恢复——冷却闩截止时刻持久化在 `runtime_state` 表 `gate:<lane>` 键，重启后未过期的闩自动恢复；分钟窗口聚合另落 `gate_windows` 表逐 lane 逐窗留存；注意闸门拒绝计数按每次 gate.wait 评估计——failover/resend/托管搜索多次过闸各记一次，与 `logs` 行每请求一行不同单位，实测约 14×）；`warm` 段投前缀保温簿记（entries/promoted/suspects/ping 收发与命中率）；号池下 `accounts` 段按 lane 名给出各号自己的 gate/warm/lane/detached 快照（`lane` 含 healthy 近似、两档冷却截止与最近换号失败归因；顶层 gate/warm 是首 lane 的后兼容视图，detached 为全 lane 聚合——计数求和、事件环按时刻归并带回填 lane 字段），`/web/accounts.html` 是逐号观测页；`debuglog.last_bind_failure` 与落盘的 `logs/bind-failure.json` 记录最近一次监听端口争夺（`first_at`/`last_at`/`count`/`holder`），是重启风暴的取证入口。
- `GET /admin/quota` 读 `quota_samples` 表（每 `debug.quota_interval_minutes` 一条快照），返回日/周配额曲线与按燃烧速率外推的耗尽时刻。
- `GET /admin/process-log?offset=` 增量拉取 `stderr.log`；`POST /admin/active-requests/{id}/abort` 中断进行中请求（`id` 取 `GET /admin/active-requests` 列表项，取消上游 ctx，结果记为 `aborted`，区别于客户端断连的 `disconnected`）；`PUT /admin/settings/debug_log_enabled`（body `{"value":"true"|"false"}`）热切换请求日志。
- `GET /admin/config` 返回脱敏后的生效配置视图（`devin.accounts[].token`/`dashboard.password` 以 `sha256:` 前缀代替明文，`devin.proxy` 的 userinfo 整段剔除，可与 `logs` 表 `key_hash` 列对照；`stale=true` 表示文件在最后一次加载后被改过）。`POST /admin/config/reload` 重读 config.yaml 并热应用，返回 `applied`（已生效字段）与 `requires_restart`（要重启才生效：仅 `server.listen`）；校验失败 422、旧配置继续服役。注意 `devin.client_*` 只作用于 adapter 发出的上游调用（chat/AssignModel/模型目录/托管搜索）——面板自身的 seat 类上游调用固定用 windsurf 身份，不随该配置走。
- `GET /admin/logs` 支持双词汇筛选——ccLoad 侧 `range`/`start_time`/`end_time`/`status_code`/`api`/`upstream_protocol`/`model_like`/`log_source`/`auth_token_id` + `limit`/`offset` 分页（ccLoad 信封 `{success,data,count}`，行内 `id` 是 `logs` 表自增主键）；`before_id` 是 keyset 翻页游标（传上一页最旧行的 `id`，深页不走 OFFSET 线性退化；与 offset 并存时谓词取交，前端只传其一），`log_source=rejected` 取出默认视图剔除的管线前拒绝留存行（`all` 含全部来源）；旧面板侧 `q`（子串，覆盖 dir/model/key_hash/client_request_id/path/error_message）/`status`（表达式 `499`/`!200`/`>=400`/`4xx`，逗号 OR）/`status_class`/`result`/`model`（精确，覆盖三段模型名）/`error_stage`/`since`/`until`（RFC3339，逐侧优先于 range 窗）。响应顶层另带 `has_more`（窗口外仍有更早历史，`EXISTS` 实查）与 `rejects`（管线前拒绝环，同 runtime-metrics rejects 形状；仅 admin 身份附带——api_token 只读身份的数据范围限定自己令牌的行）。`GET /admin/logs/matrix?since=` 是健康矩阵紧凑条目（`started_at`/`model`/`requested_model`/`status_code`/`result`/`error_stage`/`error_message`（截断）/`owner`/`duration_ms`/`first_upstream_ms`/`rate_limited` + `total` 与 `truncated` 覆盖位）；`/admin/logs/export?format=csv|json` 导出（触及条目上限时带 `X-Truncated: true` 头）。`GET /admin/debug-logs/{id}` 的 `{id}` 即 `logs` 主键（旧 started_at 毫秒伪 id 已退役）；`GET /admin/debug-logs/{id}/file/{name}` 读该 dir 的调试行（`debug_files` 先查、`debug_chunks` 按 seq 拼接，超 4MB 截断，`?raw=1` 原样回字节带 CSP sandbox）；`GET /admin/debug-logs/{id}/merged` 服务端直接合并 06 的 SSE 帧成可读正文（区别于 `POST /admin/debug-logs/merged-response` 的上传体语义——body `{resp_body}`，前端可 gzip 上传）；无 logs 行的调试 dir（413/飞行中）按 dir 名直查 `debug_*` 表。
- 保留策略分层：`debug.retention_days`（按 dir 名内嵌日期整批 DELETE `debug_*` 行）与 `debug.max_total_mb`（按 dir 聚合 payload 字节量的容量淘汰：超限先把最旧目录剥到 meta/error 归因锚点，剥载仍回不到限内才整目录删除）之外，`debug.payload_hours` 超时剥离大 payload（03/04/06/attachments 名下的行），`debug.keep_error_dirs` 在容量淘汰时保护最近 N 个含 `error.json` 行的 dir，`debug.log_row_retention_days`（默认 90 天）独立管 `logs` 行批删——摘要行比 payload 活得久，payload 淘汰后仍可归因；`debug.errors_only`（面板键 `debug_log_errors_only`）开启时干净完成的请求完结即剥 payload（只留 meta/error 锚点防目录名复用），失败与 premature_end_turn 可疑成功照常留全量——`logs` 摘要行不受影响。
- 管理面板只有一套（ccLoad 契约，`internal/ccpanel`）：`/web/*` 静态入口、`/login`/`/logout`、`/public/*` 公开，`/dashboard/*` 认两类 Bearer（面板密码 → admin、下游令牌 → api_token 只读身份），`/admin/*`（active-requests、debug-logs、settings、auth-tokens、model-registry、quota、status、model-test 等）只认面板密码 Bearer。它管理的运行时状态在 `devin-2api.db` 三张表：`auth_tokens`（下游令牌仓，/v1 准入唯一判定源：并发槽/RPM/5h·日·周·月费用窗口/模型白名单/`class` 请求类（fg/bg，决定速率闸门准入口径，语义见 docs/gate-classes.md）/匿名通道行，拒绝记 `token_limit`；令牌只由面板管理、明文一次性出示、仓内只存哈希——配置里零数据面凭据）、`model_registry`（模型注册表：停用记 `model_disabled`，`redirect_model` 在别名解析前改写请求模型名）、`settings`（面板侧改的 debug 开关/保留策略等覆盖键——对 config.yaml 恒赢，config reload 后重放压回文件值）。

注意：请求体可能含用户隐私内容；`devin-2api.db` 与 API 均不落明文凭据（`key_hash` 是 SHA-256 截断、`auth_tokens.token` 存全 hex 哈希），但 payload 内容未脱敏——对外分享前先读 meta.json 再决定是否给全量。

## 开发拓扑（archbox 开发，双机部署）

开发以 archbox 为准：`ssh archbox` → `~/src/devin-2api`（clone 自 GitHub，origin 走 SSH 直连可用）。gitignore 的开发依赖（`config.yaml`、`notes/`、`data/`、`node_modules`）已 rsync 对齐；本机新增私有文件时同步过去，反向同理。

Mac 侧到 GitHub 的直连 SSH（22 与 ssh.github.com:443）被 GFW 注入 RST（对端伪地址 `2001:2::4`）；Mac `~/.ssh/config` 的 github.com 块已配 `ProxyCommand nc -X connect -x 127.0.0.1:7893 %h %p` 走 mihomo 混合端口——依赖 mihomo 在跑且选中节点可用，代理挂时退回 HTTPS origin + gh 凭据（`credential.https://github.com.helper`）。

生产实例自 2026-09-18 起在 archbox 本机（开发与生产同机，Mac 实例已退役，全量 db 随迁移带过来）：

- archbox 生产实例：`scripts/deploy-linux.sh` 维护的 systemd --user 服务 `devin-2api.service`，监听 `:3033`（config 的 `server.listen`）。XDG 布局：二进制 `~/.local/bin/devin-2api`、权威 config `~/.config/devin-2api/config.yaml`、state 与 logs `~/.local/state/devin-2api/`（stderr/stdout.log 由 `devin-2api-logrotate.timer` 轮转）。上游 `server.codeium.com` 走 mihomo `DOMAIN-SUFFIX,codeium.com,DIRECTLY` 直连，不经代理。OOM 防护两层：earlyoom `--avoid` 名单收 `devin-2api`（`/etc/systemd/system/earlyoom.service.d/args.conf`——内存紧张期内核 OOM 曾一日三杀该服务），unit drop-in `oom-protect.conf` 把旧的 `OOMScoreAdjust=+200` 归零（user 服务写不了负值，earlyoom 名单才是真保护）。
- Mac 实例已退役：launchd 任务已 bootout，plist/旧 db/logs 均已清理，`~/Library/Application Support/devin-2api/` 只剩 `config.yaml` 作回滚种子（回滚 = 仓库 `deploy.sh` 重装 + 客户端指回，不再有可 bootstrap 的现成 plist）。`scripts/deploy-remote.sh`（Mac 目标 worktree→staging 流程）同步退役、仅留档——`~/.cache/devin-2api-staging` 不再使用。Mac 端运维备忘仍有效：**fht-mba 登录 shell 是 fish**，ad-hoc `ssh fht-mba 'VAR=x; for ...'` 一律语法炸，远端命令统一 `ssh fht-mba bash -s <<'EOF' … EOF`；Mac 上递归 grep `~/.claude`/`~/.codex` 超 120s 会被挪后台，定点文件列表逐个查。
- `:3003` 端点由两台转发 shim 继续兜住（下游零改动）：fht-mba 上 launchd `com.fanghaotian.devin-2api-forwarder`（`~/.local/bin/devin-2api-forwarder.py`）绑 `*:3003` → `100.121.76.120:3033`，loopback/tailnet/LAN 入向全覆盖；archbox 上 systemd --user `devin-2api-compat-3003.service`（`~/.local/bin/tcp-forwarder.py`）绑 `:3003` → `127.0.0.1:3033`，兜本机陈旧配置。两者都不带 SO_REUSEPORT（绝不与真实例共绑），后端拨号重试 90s 扛目标重启；脚本与两端 unit 模板收在 `scripts/compat-forwarder/`。

## 部署（单实例约定）

本机（archbox）只维护一个实例：systemd --user 服务 `devin-2api.service` 监听 :3033（config 的 `server.listen`），unit 与原理见 docs/deployment.md。

- 启停一律经 systemd；部署统一 `scripts/deploy-linux.sh`（构建 → 装入 `~/.local/bin` 并同步 config 到 `~/.config/devin-2api/` → reuseport 交接进程预接管 → `systemctl --user restart` → 托管新实例拉起后退交接 → healthz 校验版本）。
- Linux 布局：二进制 `~/.local/bin/devin-2api`，config.yaml 在 `~/.config/devin-2api/`、state 与 logs/ 在 `~/.local/state/devin-2api/`（XDG 三目录）。stderr/stdout.log 由 `devin-2api-logrotate.timer` 每日轮转（copytruncate ≥50MB、留 `.1`–`.3.gz`）。macOS 对应布局语义一致（launchd `com.$USER.devin-2api`、config 与 logs 在 `~/Library/Application Support/devin-2api/`）——平台支持仍在（`scripts/deploy.sh`），但 Mac 生产实例已退役。
- **不要手动跑 `./devin-2api` 占端口**：Restart=always 会与手动实例互抢 :3033，交替时全部在途流被掐。
- 优雅是硬要求：重启只发 SIGTERM（`TimeoutStopSec=660` 覆盖 600s 排空上限，在途流跑完再退），禁用 `kill -9` 抢时间。部署走 `deploy-linux.sh` 的 reuseport 重叠交接才是零停机；直接 `systemctl --user restart` 时排空期新连接是 refused（reuseport 实例 drain 即关 listener）。排空起点对已有连接关 keep-alive（响应带 `Connection: close`），陈旧复用连接最多吃一次 503 即重连到接替者。
- 冒烟用 `scripts/smoke.sh`（空闲端口起临时实例，healthz + `/v1/models` 真实上游探针后自动关闭）；不保留常驻侧实例。
- `devin-2api.new` 构建产物若部署中断残留，直接删除即可。
- 多个会话可能共用同一工作树：`deploy-linux.sh` 构建的就是工作树现状（tracked 含脏改 + **未跟踪非忽略文件**，即他人未提交 WIP 与本地新脚本原样上生产），脏树部署前先确认树上文件的归属与可编译性。
- `pkill -f <pattern>` 的模式会匹配发起者自己的 shell 命令行 → 整条 shell 被杀（exit 144，踩过多次）。用自排除正则（`pkill -f 'devin-2api-v[0-9]'`、`pkill -f 'state-dir /tmp/d2api-[0-9]'`——`[0-9]`/`[.]` 字面不匹配模式串自身）或先 `pgrep` 拿 pid 再 `kill -TERM`。
- 提交/部署命令不要把 `cmd | tail` 接进 `&&` 链：管道洗掉退出码，曾把「nothing to commit」当成可重试错误反复触发部署（25 分钟 20+ 次生产重启）。

其它平台的对应物：Linux 用 `scripts/deploy-linux.sh`（systemd --user，XDG 三目录：bin `~/.local/bin`、config `${XDG_CONFIG_HOME:-~/.config}/devin-2api`、state `${XDG_STATE_HOME:-~/.local/state}/devin-2api`，unit 生成在 `~/.config/systemd/user/`）；Windows 不做服务化，裸 exe 前台跑（Ctrl+C 触发同一套优雅排空；exe 在 `%LOCALAPPDATA%\Programs\devin-2api`，config 在 `%APPDATA%\devin-2api`，state 在 `%LOCALAPPDATA%\devin-2api`）。两平台脚本与 macOS 版共享 `scripts/lib-deploy.sh`（release 下载/校验、healthz 版本轮询、stray 检查）。二进制自身的路径解析链：`-config` > `DEVIN2API_CONFIG` > `./config.yaml` > 平台默认；`-state-dir` > `DEVIN2API_STATE_DIR` > 平台默认。
