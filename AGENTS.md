# 代码风格规则

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

# 架构原则

# 本机运行约定

本节原则适用于你正在编写或被明确要求重构的代码，不是主动清理现有代码的授权。

结构重构（等价变换）与语义变更（改变行为）必须分离。删除或迁移任何能力前，先确认没有调用者仍然依赖它。

好的重构让下一个读者更快看懂，降低阅读理解成本；如果只是换了个地方藏复杂度，不如不动。若重构过程中涉及行为变更，必须显式指出差异，由用户决定是否接受。

可证明的语义正确性优先于表面整洁。错误应尽早显式暴露，不要静默吞掉让问题漂移到下游。

运用第一性原理 思考，拒绝经验主义和路径盲从，不要假设我完全清楚目标，保持审慎，从原始需求和问题出发，若目标模糊请停下和我讨论，若目标清晰但路径非最优，请直接建议更短、更低成本的办法。

# 回复用户语气

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

`*.md` 提交会走 pre-commit：markdownlint-cli2 --fix 原地修规则 → `autocorrect --stdin | prettier` 经 git-format-staged 只写 index（commit 不被格式化阻断，不碰工作区未暂存内容）；`*.go` 走 gofmt（同机制）。版本以 `package.json` 为准。前置条件：`npm install`、`brew install autocorrect golangci-lint`、`pre-commit install`。markdownlint 原地改写文件时会 fail 一次，重新 `git add` 再提交。

改 Go 代码提交前跑 `golangci-lint run`（规则见 `.golangci.yml`：default:none + 显式启用 bodyclose/errcheck/govet/revive/staticcheck/unused），`golangci-lint fmt` 修 gofmt/goimports；CI golangci job 同配置，本地不过 CI 必挂。全量工具链说明见 `docs/toolchain.md`。

## 版本与发布

- 版本号不写进源码：构建期 `-X main.version=$(git describe --tags --always --dirty)` 注入；运行时解析链见 `resolvedVersion`（ldflags → buildinfo → `vcs.revision` → embed `cmd/devin-2api/VERSION` → `"dev"`）。
- `scripts/release.sh` 发版：dry-run 打印分类 changelog；`--publish` 自动回写 VERSION 并推送 → 轮询该提交的 CI 到绿 → 复查 `origin/main` 未被推进 → `git tag -a --cleanup=verbatim` 推送。tag 注解是 release body 的唯一事实源（release.yml 取 `%(contents)`）。
- tag 只打在已推送 `origin/main` 且 CI 绿的提交上；0.x 阶段 feat/破坏性变更升 minor、其余升 patch。`latest` 镜像 tag 只跟随稳定版。**已推送的 tag 永不重打**——release body 出错用 `gh release edit --notes-file` 原地修（详见 release-runbook skill）。
- 改 `scripts/release.sh` 后必跑 `scripts/release-selftest.sh`：bare origin + stub GitHub API 的离线演练，覆盖 dry-run 版本计算与 publish 全部拒绝分支；CI 的 deploy-assets job 同步跑它。
- `scripts/deploy.sh` 本机升级（`--release <tag>` 可装预编译二进制）。

# 服务排障（对运行中的实例）

本服务为 agent 调试设计：每个 `/v1/*` 响应带 `X-Request-Id` 头，值即本次请求的调试目录名（`logs/<dir>/`）；错误响应体与流式错误事件另含 `debug_ref`（同值），非流式错误体还带 `stage`（写出错误的 HTTP 处理层）。失败的首因分层 stage 以 `error.json`/`index.jsonl` 为准：`devin_transport` 是连接被截断类传输故障（含 connect.Error 包装的 EOF/帧截断），`devin_connect` 是上游语义拒绝（参数/权限/限流）。

工作流：

1. 失败/可疑请求 → 取响应头 `X-Request-Id` 或错误体 `error.debug_ref` 得到 `<dir>`。
2. 读 `logs/<dir>/meta.json`（结果、三段模型、TTFB、token、upstream_request_id）与 `error.json`（首个失败点）。
3. 需要细节再按序读阶段文件：`01-http-request.json`（客户端原文）→ `02-request-messages.json`（中间投影）→ `03-devin-request.json`（上游 wire）→ `04-devin-response.jsonl`（上游原始帧）→ `05/06`（内部事件 / 下发客户端的 SSE）。上游重试（token 自愈/空响应/transport 重开）时每次续试写 `03-devin-request.attemptN.json`，并在 04 中插入 `retry_attempt` 标记行分隔各次尝试的原始帧；次数与原因另落 `index.jsonl` 的 `retries` 与 meta.json 的 `retry_attempts`。
4. 批量检索用 `logs/index.jsonl`（每完成请求一行摘要，含 `error_stage`、`client_request_id`、key 哈希、全部 token 分类、重发次数 `retries`），`grep` 即可；更早历史被 retention 清理后索引仍在。
5. 进程级信号看 `logs/stderr.log`（slog 结构化行，每请求一行摘要 + 拒绝/清理告警）；面板数据可用 `curl -H 'Authorization: Bearer <dashboard.password>' localhost:<port>/panel/api/*` 程序化访问，`/panel/api` 返回端点目录。

聚合与生命周期：

- `GET /panel/api/usage` 是 index.jsonl 的内存聚合（今日/窗口累计、按模型/按 key、错误阶段、8 天 10 分钟粒度趋势、最近 4096 条延迟 p50/p90/p95/p99、错误责任归因 `client_faults`/`upstream_faults` 与 SLA 口径成功率、按模型 token 分位数与目录价估算成本）；启动时回放索引尾部（≤64MB）重建，进程重启不丢口径。
- `GET /panel/api/stats` 的 `http.process`（goroutine/堆/GC/CPU/RSS）与 `http.rates`（RPM/QPS）区分「代理自身瓶颈」与「上游/客户端慢」；`debuglog` 段暴露日志管道自观测（开关、写队列积压、丢弃数、IO 失败数）；`gate` 段暴露速率闸门状态（闩态/闩截止/滴灌与快败计数——冷却闩截止时刻另落盘 `logs/gate-state.json`，重启后未过期的闩自动恢复）。
- `GET /panel/api/quota` 读 `logs/quota.jsonl`（每 `debug.quota_interval_minutes` 一条快照），返回日/周配额曲线与按燃烧速率外推的耗尽时刻。
- `GET /panel/api/logs?offset=` 增量拉取 `stderr.log`；`POST /panel/api/requests/{dir}/abort` 中断进行中请求（取消上游 ctx，结果记为 `aborted`，区别于客户端断连的 `disconnected`）；`POST /panel/api/debug/toggle` 热切换请求日志。
- `GET /panel/api/config` 返回脱敏后的生效配置视图（`devin.token`/`auth.api_key`/`dashboard.password` 以 `sha256:` 前缀代替明文，可与日志 `key_hash` 对照；`stale=true` 表示文件在最后一次加载后被改过）。`POST /panel/api/config/reload` 重读 config.yaml 并热应用，返回 `applied`（已生效字段）与 `requires_restart`（要重启才生效：`server.listen`/`max_concurrency`/`devin.base_url`/`proxy`/`force_http1`/`debug.quota_interval_minutes`）；校验失败 422、旧配置继续服役。注意 `devin.client_*` 只影响 chat 路径——面板自身的 seat 类上游调用固定用 windsurf 身份。
- `GET /panel/api/requests` 支持结构化筛选（`status_class`/`result`/`model`/`error_stage`/`since`/`until`）与 `has_more` 截断信号；`/panel/api/requests/export?format=csv|json` 导出；`/panel/api/requests/{dir}/merged` 把 `06` 的 SSE 帧合并成可读正文。
- 保留策略分层：`debug.retention_days`（目录整删）与 `debug.max_total_mb`（容量淘汰）之外，`debug.payload_hours` 超时剥离大文件（03/04/06/attachments），`debug.keep_error_dirs` 在容量淘汰时保护最近 N 个含 `error.json` 的失败目录。

注意：请求体可能含用户隐私内容；日志目录与 API 均不落明文凭据（`key_hash` 是 SHA-256 截断），但内容字段未脱敏——对外分享前先读 `meta.json` 再决定是否给全量。

## 部署（单实例约定）

本机只维护一个实例：launchd 用户代理 `com.$USER.devin-2api` 监听 :3003（config 的 `server.listen`，作者本机取值），plist 与原理见 docs/deployment.md。

- 启停一律经 launchd；部署统一 `scripts/deploy.sh`（构建 → 装入运行目录并同步 config → `kickstart -k` → healthz 校验版本）。
- 运行目录是 `~/Library/Application Support/devin-2api/`（二进制+config.yaml+logs），不是仓库：launchd 子进程对 ~/Desktop 的 open 会被 TCC 授权判定永久挂起。仓库 `logs/` 是指向运行目录的符号链接，排障路径照旧。
- **不要手动跑 `./devin-2api` 占端口**：KeepAlive 会与手动实例互抢 :3003，交替时全部在途流被掐。
- 优雅是硬要求：重启只发 SIGTERM（`kickstart -k`，`ExitTimeOut=60`，在途流跑完再退），禁用 `kill -9` 抢时间。
- 冒烟用 `scripts/smoke.sh`（空闲端口起临时实例，healthz + `/v1/models` 真实上游探针后自动关闭）；不保留常驻侧实例。
- `devin-2api.new` 构建产物若部署中断残留，直接删除即可。

其它平台的对应物：Linux 用 `scripts/deploy-linux.sh`（systemd --user，运行目录 `${XDG_DATA_HOME:-~/.local/share}/devin-2api`，unit 生成在 `~/.config/systemd/user/`）；Windows 不做服务化，裸 exe 前台跑（Ctrl+C 触发同一套优雅排空）。两平台脚本与 macOS 版共享 `scripts/lib-deploy.sh`（release 下载/校验、healthz 版本轮询、stray 检查）。
