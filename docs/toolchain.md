# 工具链说明（给 agent 的操作手册）

本仓库的工程化设施分四层：**提交前**（pre-commit 管道）、**本地验证**（lint/test/selftest）、**CI/CD**（三个 workflow）、**发布与部署**（release.sh + deploy 脚本族）。每层都有机械化的检查，原则是能写进脚本/CI 的规则不靠人守。

## 1. 提交前：pre-commit 管道

钩子定义在 `.pre-commit-config.yaml`，经 `pre-commit install` 装入。核心技巧是 **git-format-staged**：格式化结果只写 git index，不碰工作区未暂存内容——你可以只暂存文件的一部分提交，格式化不会污染剩下的工作区改动。

- `*.md`：`markdownlint-cli2 --fix` 原地修可自动修的规则 → `autocorrect --stdin | prettier` 写 index。markdownlint 原地改写文件时会 fail 一次，**重新 `git add` 再提交**即可，不是错误。
- `*.go`：`gofmt` 走同一机制。
- 全补丁：gitleaks 密钥扫描（v8.30.1 上游 hook，自定义规则在 `.gitleaks.toml`——GitHub push protection 只认标准 pattern，`devin-session-token$` 这类自有格式靠它拦）。
- markdownlint / prettier / autocorrect 的版本锁定在 `package.json`（`npm install` + `npm ci` 在 CI 复现），autocorrect 本机经 `brew install autocorrect` 提供。

## 2. 本地验证

| 命令                                 | 覆盖                                                         | 说明                                                                                                                                        |
| ------------------------------------ | ------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `golangci-lint run`                  | bodyclose / errcheck / govet / revive / staticcheck / unused | `.golangci.yml` 用 `default: none` + 显式点名，升级 golangci 不会被新增默认 linter 偷袭。revive 的 `exported`（导出符号注释）按仓库约定关闭 |
| `golangci-lint fmt`                  | gofmt + goimports                                            | goimports 的 `local-prefixes` 是 `github.com/WncFht/devin2api`（**必须是 YAML 数组**，标量写法 `run` 能过但 `config verify` 拒收）          |
| `go vet ./...`                       | 编译期检查                                                   |                                                                                                                                             |
| `go test -race ./...`                | 单测 + race                                                  |                                                                                                                                             |
| `GOOS=windows go build/vet ./...`    | 交叉编译                                                     | 防止引入 unix-only 调用打断其它平台；darwin 同理                                                                                            |
| `bash scripts/deploy-assets.test.sh` | 部署资产断言                                                 | 见 §6                                                                                                                                       |
| `bash scripts/release-selftest.sh`   | release.sh 全流程演练                                        | 见 §5，**改 release.sh 后必跑**                                                                                                             |
| `npm run format:check` / `lint:md`   | markdown 格式/规则                                           | 与 pre-commit 同套版本                                                                                                                      |
| `actionlint`（若装了）               | workflow 语法                                                | CI 不跑它，本地自查                                                                                                                         |

前置条件：`npm install`、`brew install autocorrect golangci-lint`、`pre-commit install`。Linux 无 brew 时的等价装法（archbox 实测）：`go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2 && ln -sf ~/go/bin/golangci-lint ~/.local/bin/`——版本号与 CI 的 `golangci-lint-action@v9` 固定值对齐。

写 shell 脚本注意 macOS 自带 **bash 3.2**：`mapfile`/`declare -A` 不存在；`set -u` 下展开空数组 `"${arr[@]}"` 报 unbound——仓内脚本统一写 `${arr[@]+"${arr[@]}"}`（smoke/release/perf-snapshot/deploy-remote 全是这个写法，新脚本照抄）。

## 3. 版本解析链（4 级 fallback）

`-version` 输出的来源，按优先级（`cmd/devin-2api/main.go` 的 `resolvedVersion`）：

1. **ldflags**：`-X main.version=vX.Y.Z`——release.yml 与 deploy 脚本构建时都注入。
2. **buildinfo**：`go install ...@vX.Y.Z` 装的二进制，`debug.ReadBuildInfo().Main.Version` 给出 module 版本。
3. **vcs.revision**：`go build` 于 git 工作区 → `dev-<sha12>[-dirty]`。
4. **embed VERSION**：`//go:embed cmd/devin-2api/VERSION`——release.sh 发版时回写这个文件，**tag 与文件内容自指**（tag vX.Y.Z 指向的提交里 VERSION == vX.Y.Z），因此 tarball/`go build` 无 ldflags 的产物也能报对版本。兜底 `"dev"`。

## 4. CI（`.github/workflows/ci.yml`）

push 到 main 与 PR 触发。顶层 `permissions: contents: read`；`concurrency` 按 workflow+ref 取消在途（PR 连推不排队）；每个 job 有 `timeout-minutes` 护栏。9 个并行 job：

- **test**：`go mod tidy -diff`（go.mod 与 import 漂移拦截）→ gofmt 检查 → `go vet` → `go test -race -shuffle=on -covermode=atomic -coverpkg=./...`（覆盖率只观测不设门槛，total 打日志 + profile 存 artifact）→ `go build` → windows/darwin 交叉编译 + vet。
- **golangci**：`golangci-lint-action@v9` 固定 `v2.13.2`，与本地版对齐。
- **actionlint**：workflow 文件自身 lint（钉 v1.7.12，shellcheck 查 run: 块内 bash）。
- **deploy-assets**：`deploy-assets.test.sh` 断言 + `release-selftest.sh` 演练。
- **codegen-drift**：钉版 protoc + protoc-gen-go/connect-go 重跑生成 → `scripts/check-codegen.sh` 与提交的 `outputs/devin-proto-go` 逐字节比对，proto 源改了忘重新生成时拦下。
- **darwin-smoke**（macos-latest）：macOS 平台的真机验证——`go test ./...` + `smoke.sh --no-upstream`（无 token 环境下断言 `/v1/models` 明确 502、SIGTERM 优雅退出），darwin 产物不再只靠交叉编译门禁。
- **windows-smoke**（windows-latest）：全量测试 + 启动冒烟（healthz + `/v1/models` 空 token 502）；不验 SIGTERM 排空——Windows 优雅退出走 Ctrl+C/os.Interrupt，git-bash kill 是 TerminateProcess，无从断言。
- **panel-verify**：`scripts/panel-verify` playwright 套件进 CI（临时实例 + 假上游凭据，不打真上游；firefox 浏览器按 package-lock 缓存）。
- **lint-markdown**：`npm ci` → `format:check` + `lint:md`。

Go 环境统一走复合 action `.github/actions/setup-go`：`actions/setup-go` 读 `go.mod` 定版本，mod 缓存按 `go.sum` 哈希、build 缓存按 job+sha（restore-keys 兜底）。

`.github/workflows/security.yml` 是独立的 govulncheck job（push/PR/每周一）。**release.sh 的 CI 门禁只认名为 `CI` 的 workflow**——Security 红不挡发版（有意的：漏洞扫描是持续观察项，不是单次发布的质量门）。

`.github/workflows/nightly.yml`（每日 cron + 手动 dispatch）：`upstream-probe` 在 `secrets.DEVIN_E2E_TOKEN` 配置后跑 `smoke.sh` 全真模式（真上游 RPC + SIGTERM 排空；未配置则跳过，fork 不红）；`bench` 跑 `bench.sh` 全基准并把结果存 90 天 artifact 做趋势留痕。`.github/dependabot.yml` 周更 gomod（根 + devinproto 生成模块）、github-actions、npm（根 + panel-verify）。

## 5. 发布（`scripts/release.sh` + `release.yml`）

### release.sh 两段式

- **dry-run**（默认）：从 Conventional Commits 算下一版本（0.x：feat/破坏性 → minor，其余 → patch；`--version` 可覆盖），打印分类 changelog（Features / Fixes / Other；`chore(release):` 簿记提交自动过滤）。
- **--publish**：脏工作区/未推送 HEAD 拒绝 → 回写 `cmd/devin-2api/VERSION` 提交 `chore(release): bump` 并推送 → 轮询 `workflow_runs?head_sha=` 到该提交 CI 绿（`CI_WAIT_SECONDS` 默认 1200s、`CI_POLL_INTERVAL` 默认 20s，环境变量可调）→ 重新 fetch 复查 `HEAD == origin/main`（TOCTOU 防护）→ `git tag -a -F notes --cleanup=verbatim` → 推 tag。

两个踩过的坑已机械化防住：`--cleanup=verbatim` 防止 `-F` 默认的 `strip` 吃掉 `##` 开头的 markdown 标题；release.yml 在 tag push 的 checkout 上先 `git fetch --force` 拉注解对象再读 `%(contents)`，否则拿到 lightweight ref 读不出 body。

**铁律：已推送的 tag 永不重打。** release body 坏了用 `gh release edit vX.Y.Z --notes-file <tag 注解>` 原地修。

### release-selftest.sh（ccLoad 模式）

改 release.sh 后的必跑项，也是 CI deploy-assets job 的一步。原理：临时目录建工作仓库 + bare origin，`url."file://<bare>".insteadOf "https://github.com/test/repo.git"` 让 `REPO_SLUG` 解析正常而 fetch/push 全走本地；PATH 前置 stub `curl`（按 `FIXTURE_MODE` 回 canned `workflow_runs` JSON，`CALLS_FILE` 计数验证轮询）和 stub `gh`。

覆盖：dry-run 的 minor/patch/破坏性/override/chore 过滤/无提交拒绝/tag 冲突拒绝；publish 的 green 全流程（断言 tag、VERSION 回写、注解含 `## Features`——verbatim 回归）、pending→green 轮询、CI 失败拒绝、未推送拒绝、等 CI 期间 origin 被推进的 TOCTOU 拒绝。

### release.yml（tag push 触发，或 workflow_dispatch 在 tag ref 上手动重发）

- **validate**：dispatch 防呆——断言 ref 是 `refs/tags/v*`、是 annotated tag（lightweight 取不出 notes）、tag 落点提交 == 本次运行提交。
- **test**：同 CI 的测试。
- **binaries**：6 个 matrix 资产（darwin/linux × amd64/arm64 裸二进制，windows 打 zip 含 exe+config.example.yaml+LICENSE），`-ldflags "-s -w -X main.version=${GITHUB_REF_NAME}"`。linux 资产过 `readelf -l` 断言无 program interpreter（CGO_ENABLED=0 的产物若有解释器说明意外引入 cgo，alpine 里跑不起来）。
- **publish**：先下载全部二进制产物 → buildx（**无 QEMU**——`Dockerfile.release` 直接 `COPY dist/devin-2api-linux-${TARGETARCH}`，镜像字节 = release 字节，不在镜像里重编）→ GHCR 登录 + DockerHub 条件登录（secrets 配了才推）→ 推 `:<version>` `:<minor>` `:<major>`，稳定版另推 `:latest` → **digest 验证**（imagetools inspect 回读远端 manifest，浮动 tag 必须与精确版本同 digest，串线当场失败）→ `sha256sum` 生成 `checksums.txt` → 取 tag 注解作 release body → `action-gh-release` 建 Release 上传 7 个资产。

## 6. 部署脚本族 + 资产断言

- `scripts/deploy.sh`（macOS launchd `com.$USER.devin-2api`，监听端口取 `server.listen`、缺省 :3003）、`scripts/deploy-linux.sh`（systemd `--user`）共享 `scripts/lib-deploy.sh`：release 资产下载 + `checksums.txt` 校验、`wait_healthz_version` 部署后版本轮询、stray 进程检查（`pgrep -x` 精确名匹配——`pgrep -f` 会把命令行里含 devin-2api 的无关进程误报成 stray）。两脚本另把 `scripts/rotate-logs.sh` 装成 `~/.local/bin/devin-2api-logrotate` 并登记每日驱动（launchd StartInterval agent / systemd timer），轮转 stderr/stdout.log。三平台部署细节见 `deployment.md`。
- ~~`scripts/deploy-remote.sh`~~（2026-09-18 退役留档，运行即 exit 1）曾是开发机侧的远程驱动：经免密 SSH 到生产机执行 `deploy.sh`——默认 worktree 模式把 git 视角的本地工作树（含未提交改动）连同 `.git` 推流到远端 staging 构建部署（`config.yaml` 不进 tar）；`--ref`/`--release` 部署已推送状态或预编译资产，`--check` 并排对比生产与验证实例。三种模式部署前都把 live 配置（`DEVIN2API_CONFIG_LIVE`，默认 `~/Library/Application Support/devin-2api/config.yaml`）刷进 staging——live 是权威副本，`deploy.sh` 预检读的也是它。生产迁 archbox 后部署唯一路径是 `scripts/deploy-linux.sh`。
- `scripts/deploy-assets.test.sh` 是对这些资产的**字符串断言套件**：plist 必须有 KeepAlive/ExitTimeOut/`kickstart -k`、unit 必须有 Restart=always/TimeoutStopSec、进度输出必须 `>&2`（`$()` 捕获会把 stdout 噪音混进变量）、禁 `kill -9`，外加所有 shell 脚本 `bash -n` 与 `fit.py` 的 `compile()` 语法检查。风格：逐条 `check`/`has` 断言、最后统一退出码——新增断言照抄这个模式。
- Windows 无服务化：裸 exe 前台跑，Ctrl+C 走同一套优雅排空。

## 7. 其它设施

- **Issue 模板** `.github/ISSUE_TEMPLATE/bug_report.yml`：要 `X-Request-Id`/`debug_ref`（调试身份 dir 名）、`meta.json`/`error.json`、版本、平台——与服务排障工作流（AGENTS.md「服务排障」节）对应。
- **Skills**：`.claude/skills/<name>/` 与 `.agents/skills/<name>/` 是**逐字节相同的镜像**（`SKILL.md` + `agents/openai.yaml`），新增 skill 两边一起放。现有 12 个：`codebase-design`、`diagnosing-bugs`、`extract-embedded-protos`、`fix-it-never-work-around-it`、`go-comment-conventions`、`golang-pro`、`improve-codebase-architecture`、`llm-core-types`、`observability-first-debugging`、`orchestrating-agents`、`protocol-drift`、`release-runbook`。
- **文档**：README（EN + zh-CN）、`CONTRIBUTING.md`（架构与贡献）、`AGENTS.md`/`CLAUDE.md`（同一文件，agent 行为规则）、`docs/`（活文档目录，索引 `docs/README.md`：上游协议逆向、排障手册、客户端接入、配额计费、部署、本文档）。`notes/` 是本机私有工作区（gitignore），只放 `archive/` 日期快照。

## 8. 运维与实验脚本（`scripts/`）

会话排障与上游调研沉淀下来的手工工具，不进 CI：

| 脚本                              | 干什么                                                                                                                                                                                                               |
| --------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `reqprobe.sh <label> <ep> <body>` | 打一发请求到 `REQPROBE_BASE`（默认 127.0.0.1:3033，key 自动读 config.yaml），打印 X-Request-Id、02 的 Dropped/tool_choice/IR 序列、03 wire 名、SSE/终态 JSON 形态——新客户端/新字段冒烟的第一步                       |
| `index-stream-stats.py`           | join `logs` 表与 debug_files/debug_chunks payload 出流画像：sid/psid 派生、`cc_is_subagent` 标记、gap→hit% 分桶、miss 归因、warm/cold TTFB；`--db` 默认按平台探测 devin-2api.db（只读连接）                          |
| `cache-probe.py`                  | 缓存受控实验骨架：arm（独立 user_id + padded system）× 绝对偏移时刻表，ThreadPoolExecutor 调度、逐行 JSONL 落盘；`--plan` 或 `--keepalive` 模式                                                                      |
| `drift-corpus-scan.py`            | 扫 db 里 01 语料统计各协议的漂移形状分布（`--db` 默认按平台探测，`--since/--until YYYYMMDD` 限窗；语料为空 exit 2 防空转误读）                                                                                       |
| `panel-qa.js`                     | ccpanel 前端走查：`shot`/`overflow`/`sweep` 子命令，playwright 无头截图 + 元素级溢出检测 + i18n 泄漏检查；token 自动读 config.yaml dashboard.password                                                                |
| `panel-verify/`                   | ccpanel 断言式验证套件：`./run.sh` 自带临时实例（空闲端口 + 临时 config/state，假上游凭据）跑 playwright 检查——登录角色、断点 nav 裁切、列显隐（含死窗回归）、移动端溢出、零 console 错误；截图只在失败时写 `shots/` |
| `remote-logs.sh`                  | 远端实例日志分诊（`tail`/`fails`/`dir`/`grep`/`stderr`），内部 `ssh host bash -s` 绕 fish——原为 fht-mba 生产而写，生产已迁 archbox 本机（直读 `~/.local/state/devin-2api/logs/`），脚本留作远端目标通用工具          |
| `repo-survey.sh`                  | 一台机器 `~/src/*` 全部 git 仓体检表（branch/dirty/ahead/behind/stash/最后提交），可 `--host` 走 ssh                                                                                                                 |
| `toolalign/`                      | 客户端工具声明对齐矩阵：`run_matrix.py <cc\|codex>`（逐工具强制调用 + tool_result 回环）、`run_edges.py`（流式/none/image-error/并行配对/namespace 展平边界）                                                        |

脏树时拿干净构建验证的配方：`git worktree add $W/wt HEAD && go build -C $W/wt -o $W/devin-2api ./cmd/devin-2api` 出 HEAD 态二进制 → scratch 目录备一份 `config.yaml`（`listen` 换空闲端口、`debug.enabled: true`）→ `-state-dir .` 让 logs 落本地 → `(nohup … &)` 起 → 测完 `git worktree remove --force` 收尾。多人共用工作树时这是不动主树的验证通道。

## 9. 速查

```bash
# 提交前
golangci-lint run && golangci-lint fmt   # Go lint + 格式化
go test -race ./...                      # 测试
npm run format:check && npm run lint:md  # markdown

# 发版（详见 release-runbook skill）
scripts/release.sh                       # dry-run
scripts/release.sh --publish             # VERSION 回写→等 CI 绿→打 tag
bash scripts/release-selftest.sh         # 改 release.sh 后必跑

# 部署与排障（生产 = archbox 本机 :3033）
scripts/deploy-linux.sh [--release vX.Y.Z|--check]  # 生产部署唯一路径（零停机交接）
scripts/deploy.sh [--release vX.Y.Z]     # macOS 本机升级（非生产拓扑）
# scripts/deploy-remote.sh               # 2026-09-18 退役：运行即 exit 1，仅留档
bash scripts/deploy-assets.test.sh       # 部署资产断言
```
