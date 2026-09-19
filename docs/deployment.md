# 部署（launchd / systemd / 裸进程）

三平台拓扑——各平台按自己的目录规范分家（二进制 / 配置 / 状态日志三类不再同居一个运行目录）：

| 平台    | 托管方式                                   | 二进制                                              | 配置                                                   | 状态/日志                                                                | 部署命令                            |
| ------- | ------------------------------------------ | --------------------------------------------------- | ------------------------------------------------------ | ------------------------------------------------------------------------ | ----------------------------------- |
| macOS   | launchd 用户代理 `com.$USER.devin-2api`    | `~/.local/bin/devin-2api`                           | `~/Library/Application Support/devin-2api/config.yaml` | 同配置目录（`logs/` 子目录；macOS 无独立 state 惯例，维持 app 目录模型） | `scripts/deploy/deploy.sh`          |
| Linux   | systemd `--user` unit `devin-2api.service` | `~/.local/bin/devin-2api`                           | `${XDG_CONFIG_HOME:-~/.config}/devin-2api/config.yaml` | `${XDG_STATE_HOME:-~/.local/state}/devin-2api`                           | `scripts/deploy/deploy-linux.sh`    |
| Windows | 无服务化，裸 exe 前台跑                    | `%LOCALAPPDATA%\Programs\devin-2api\devin-2api.exe` | `%APPDATA%\devin-2api\config.yaml`                     | `%LOCALAPPDATA%\devin-2api`                                              | `scripts/deploy/deploy-windows.ps1` |

二进制的路径解析链（服务定义里全部显式传 flag，链只对裸跑生效）：配置文件 `-config` flag → `DEVIN2API_CONFIG` env → `./config.yaml`（存在才选，仓库开发/Windows 解压即跑）→ 上表平台默认；状态目录 `-state-dir` flag → `DEVIN2API_STATE_DIR` env → 上表平台默认。启动日志 `paths resolved` 一行打出实际生效的两个路径。

三个 deploy 脚本（macOS/Linux 共用 `scripts/deploy/lib-deploy.sh`）参数语义一致：`--release <tag|latest>` 装预编译二进制（sha256 校验）、`--no-restart` 只替换不重启、`--check` 对比 已安装/运行中/最新 release 版本、`--uninstall` 停用并移除服务与二进制（保留 config/logs）。服务未安装时首装自动生成服务定义并拉起；`config.yaml` 缺失时从 `config.example.yaml` 生成（随机 `dashboard.password`，tty 下提示粘贴 token；下游 /v1 令牌不入配置，到面板 /web/tokens.html 创建）。开工前的 preflight 拦截 sudo、缺依赖、占位 token、端口冲突、生效 config 里死引用的 `credentials_file`（9-18 断流根因；加载期现已改判该 lane 降级带病服役而非拒载，带病起跑照样拦下）；`/healthz` 版本对上后再打一发 `/v1/models` 验证上游鉴权。最小安装路径：clone 仓库 → `deploy*.sh --release latest`。

开发机侧曾有远程驱动 `scripts/attic/deploy-remote.sh`（SSH 到生产机执行 `deploy.sh`，worktree 推送模式），2026-09-18 随 Mac 生产实例退役、仅留档（运行即 exit 1）；机制细节见 `toolchain.md` §6。部署后的验证步骤（healthz 版本确认 + 面板套件冒烟）见 `post-deploy-verify.md`。

## 跨平台共同约定

- **运行时状态归 SQLite**：`<state-dir>/devin-2api.db`（WAL，伴生 `-wal`/`-shm`）装全部持久化状态——`logs` 请求行、`debug_files`/`debug_chunks` 调试 payload、`debug_blobs`/`debug_chunk_refs`（payload 跨目录 CAS 共享层）、`log_cells`/`log_err_cells`（logs 的 600s 预聚合 rollup，usage 聚合整格读）、`auth_tokens`、`model_registry`、`settings`、`quota_samples`、`gate_windows`（闸门分钟窗口聚合账）、`lane_attempt_causes`（号池被放弃 lane 尝试的日粒度聚合账）、`detached_events`/`detached_blobs`（脱钩流完成缓存台账与跨进程种子）、`upstream_accounts`（号池账号行）、`store_opens`（开库台账）、`schema_migrations`（版本化迁移登记）、`runtime_state`（闸门闩态等）。`<state-dir>/logs/` 只剩进程输出（`stdout.log`/`stderr.log`）与启动早期取证标记（`bind-failure.json` 端口争夺、`config-fallback.json` 兜底服役——见下）。仓库里的 `logs/` 只是指向本机状态目录的符号链接（开发便利，非必需）。旧版「单运行目录」布局由部署脚本自动迁移（`migrate_legacy_runtime`）：config/logs 挪到平台目录、删旧二进制，目标已存在时不覆盖。文件时代的 `index.jsonl`/`auth_tokens.json`/`models.json`/`panel-settings.json`/`quota.jsonl`/`gate-state*.json` 由启动导入器搬进库后改名 `<name>.migrated`（回滚 = 旧二进制 + 改回原文件名，见 AGENTS.md「服务排障」节）。`<state-dir>/last-good-config.yaml` 是最近一次成功加载的自包含配置投影（accounts 的 credentials_file 已物化成 token 并摘除——兜底服役的重校验不再触外部文件）：boot 加载失败且缓存存在时按它降级服役而非退出（与 reload 校验失败保留旧配置同一判据：坏的新配置永不顶替最近一次的好配置），兜底期 `logs/config-fallback.json` 记次数与恢复边界，`/admin/config` 强制 `stale` 并透 `served_from`/`cached_at`，`/healthz` 带 `config_last_good`；首装无缓存仍照旧退出。
- **优雅排空是硬要求**：进程实现 `SIGTERM` 优雅退出（`signal.NotifyContext`）——收到信号进入 draining：`/healthz` 继续应答但带 `draining: true`，新的 `/v1/*` 立即 `503 + Retry-After: 1`，在途请求跑完；排空上限 600s，超时先对在途请求做带因取消——被掐请求走正常收尾管道落 `result=aborted`/`error_stage=drain_timeout` 的 logs 行与 error.json（进程侧掐断，与客户端断连分开归因）——再强关剩余连接，另留 5s 宽限等收尾 bookkeeping 落盘。两个服务定义都给 660s 停止超时覆盖该上限加余量。重启只发 SIGTERM，禁用 `kill -9` 抢时间（Ctrl+C 在 Windows 前台触发同一套排空）。listener 在排空期的行为取决于 `DEVIN2API_REUSEPORT`：未开启时保持打开（新请求拿应用层 503 而非内核拒绝）；开启时立即关闭——reuseport 组内新连接按绑定序（macOS）或哈希（Linux）落到组内其它 socket，旧实例只有让出监听，deploy 预置的交接进程才能接管。排空起点同时 `SetKeepAlivesEnabled(false)`：drain 前已 accept 的 keep-alive 连接关 listener 管不着，会一直被钉在旧实例上整窗吃 503；关掉 keep-alive 后这些连接在下一个响应带 `Connection: close` 收尾，客户端重连即落到接替者——陈旧连接最多吃一次 503。
- **重叠交接部署（reuseport handoff）**：`deploy.sh`/`deploy-linux.sh` 的重启路径是「先起交接进程 → 重启托管实例 → 等托管新实例拉起 → 退交接进程」。交接进程是同一二进制的临时副本，带 `DEVIN2API_REUSEPORT=1` 绑定同一端口入队；旧实例 drain 起点即关闭 listener 后它接管全部新连接，直到 KeepAlive/Restart 拉起托管新实例后再 SIGTERM 退场。全程零 503、零拒绝，在途请求只受 600s 排空上限约束，也不再需要等空闲窗口。reuseport 是准入制：并组不撞 EADDRINUSE（内核语义就是同端口多 socket 共存），所以二进制发现 `DEVIN2API_REUSEPORT` 开启时要求进程持有托管出处——systemd 的 `INVOCATION_ID`、交接进程的 `DEVIN2API_HANDOFF`、或服务定义注入的 `DEVIN2API_MANAGED`（plist/unit 都写），三者皆无直接拒绝启动——手动带 env 裸跑从此是明确报错而非静默并组成影子实例。交接进程 pid 记录在 `<状态目录>/.handoff.pid`；部署中断残留时下次部署自动回收。交接进程同时是新实例的金丝雀（同二进制同 config 走真实启动路径）：spawn 失败按新鲜 stderr 段分诊——`port already in use` 证明旧实例没开 reuseport，回退经典「等空闲 + 重启」；其余死因（config 校验失败、缺凭据文件、panic、静默早夭、超时）说明新实例当前起不来，直接中止部署而不触碰旧实例——9-18 事故正是把 config 校验死误诊为缺 reuseport 并回退重启，把可失败的部署变成 2h55m 断流。同样，托管新实例有 MainPID 不等于已接管：杀桥前先证接管（Linux 轮询 healthz 直到应答 pid==托管 pid；macOS 新连接只派给最先存活 socket，退以 stderr 新 listening 行 + 进程存活作证），120s 证不出则交接进程保留降级服役。收尾除 version 匹配外再断言 healthz 应答 pid==托管 MainPID，堵死交接/野实例答出新版本的假绿。服务定义变更（plist/unit 重写）走同一套交接：重启动词换成「载入新定义」的那个（macOS `bootout+bootstrap`，Linux `systemctl restart` 随已 daemon-reload 的新 unit 生效），交接桥照样盖住整段排空窗口。注意直接 `launchctl kickstart -k` 不走交接：reuseport 实例 drain 即关 listener，排空期新连接是 refused 而非 503（都失败，但拿不到 Retry-After）。
- **单实例**：托管器（KeepAlive/Restart=always）会与手动起的实例互抢监听端口，交替时全部在途流被掐。所有实例必须经托管器启停；冒烟验证用空闲端口起临时二进制，验证完立即关闭，不留常驻侧实例。
- **版本可见性**：`main.version` 由构建期 `-X` 注入（`git describe --tags --always --dirty` 或 tag 名），`stderr.log` 启动行、`/healthz`、`-version` flag 三处可查。部署后脚本轮询 `/healthz` 直到 version 等于刚部署的版本——排空期旧进程仍在应答旧版本，首次 200 不代表切换完成。
- 重启、换二进制前先确认目标端口上没有遗留测试进程（`lsof -nP -iTCP:<port> -sTCP:LISTEN`，Windows 用 `netstat -ano | findstr <port>`）。

## macOS（launchd）

```
launchd (gui/<uid> 用户域, 无需 sudo)
  └─ ~/.local/bin/devin-2api -config $RT/config.yaml -state-dir $RT   ($RT = ~/Library/Application Support/devin-2api)
       ├─ $RT/devin-2api.db               SQLite 状态库（logs/debug/auth_tokens/quota 等全部表）
       └─ $RT/logs/                       stdout.log（面板渲染等 fmt 输出）+ stderr.log（slog 进程日志）+ bind-failure.json
```

**与仓库分离的平台目录**：launchd 拉起的进程对 TCC 保护目录（`~/Desktop`、`~/Documents` 等）的每次 `open()` 都会进入授权判定——未授权时内核挂起 syscall，表现为进程在 dyld/读 config 阶段永久卡死（授权还按 cdhash 记，每次重建二进制即失效）。`~/.local/bin` 与 `~/Library/Application Support` 都不受 TCC 保护：二进制入前者（可直接调用），配置与状态目录沿用后者不变（`os.UserConfigDir` 的 darwin 返回即 Application Support）。`config.yaml` 的权威副本是 `$RT` 里那份（live）：`install_binary` 只在 live 缺失时从仓库副本恢复、存在且不一致时保留 live 并告警（已退役的 deploy-remote 当年还在各模式部署前把它刷进 staging 供 `deploy.sh` 预检读取）。改配置直接编辑 `$RT/config.yaml` 后 `POST /admin/config/reload` 热应用；仅 `server.listen` 等冷键需 `launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api`。

请求级 debug 日志的生命周期由 `debug.retention_days` / `debug.max_total_mb` / `debug.payload_hours` / `debug.keep_error_dirs` / `debug.errors_only` 自管；launchd 侧无需额外配置。

### plist 示例

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>com.$USER.devin-2api</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/<user>/.local/bin/devin-2api</string>
		<string>-config</string>
		<string>/Users/<user>/Library/Application Support/devin-2api/config.yaml</string>
		<string>-state-dir</string>
		<string>/Users/<user>/Library/Application Support/devin-2api</string>
	</array>
	<key>WorkingDirectory</key><string>/Users/<user>/Library/Application Support/devin-2api</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>DEVIN2API_REUSEPORT</key><string>1</string>
		<key>DEVIN2API_MANAGED</key><string>1</string>
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>ExitTimeOut</key><integer>660</integer>
	<key>StandardOutPath</key><string>/Users/<user>/Library/Application Support/devin-2api/logs/stdout.log</string>
	<key>StandardErrorPath</key><string>/Users/<user>/Library/Application Support/devin-2api/logs/stderr.log</string>
</dict>
</plist>
```

各键的含义与取舍：

| 键                     | 示例值                                         | 说明                                                                                                                                                                       |
| ---------------------- | ---------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `RunAtLoad`            | true                                           | 登录即启动                                                                                                                                                                 |
| `KeepAlive`            | true                                           | 任何退出都重拉——含 `bootout` 外的主动 `kill`。若想「干净退出不复活」，改为 `<dict><key>SuccessfulExit</key><false/></dict>`                                                |
| `ThrottleInterval`     | 5                                              | 崩溃循环时每 5 秒才重试，防止拉满 CPU                                                                                                                                      |
| `ExitTimeOut`          | 660                                            | SIGTERM 后最多等 660s 再 SIGKILL；覆盖二进制 600s 排空上限 + 退出余量                                                                                                      |
| `EnvironmentVariables` | `DEVIN2API_REUSEPORT=1`、`DEVIN2API_MANAGED=1` | 前者注入 SO_REUSEPORT——重叠交接部署的前提；后者是 reuseport 准入的出处声明（launchd 无 `INVOCATION_ID`，缺了它二进制拒绝并组）；两者皆无的裸跑二进制仍撞单实例端口冲突保护 |
| `StandardErrorPath`    | logs/stderr.log                                | slog 输出落盘；轮转由配套 logrotate agent 每日执行，见下节                                                                                                                 |

### stderr/stdout 日志轮转

debug 请求日志有 retention，但 `stderr.log`（slog 进程日志）与 `stdout.log` 只会增长。这两个文件的写 fd 归 launchd 持有、进程无法 reopen，rename 类轮转会让 fd 跟着旧 inode 走、新写全丢——所以轮转用 copytruncate：`scripts/deploy/rotate-logs.sh` 对超 50MB 的 `*.log` 复制后原地截断并 gzip，保留 `.1`–`.3.gz` 三代（截断瞬间并发写入的一行仍可能丢，是该语义的最小代价）。

`deploy.sh` 把它装成 `~/.local/bin/devin-2api-logrotate` 并加载配套 agent `com.$USER.devin-2api.logrotate`（`StartInterval=86400` 每天一次，输出落 `logs/logrotate.out`/`.err`）；plist 漂移会自动重写并 bootout+bootstrap，`--uninstall` 一并移除。与主服务完全解耦，轮转动作不影响在途请求。注意系统 newsyslog 是 rename+signal 语义，对 launchd 持有的 fd 不适用——这里没有等价替代品，用这个脚本。

### 常用命令

```bash
launchctl print gui/$(id -u)/com.$USER.devin-2api | grep -E 'state|pid'   # 状态
launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api                 # 重启（发 SIGTERM 再拉起）
launchctl bootout gui/$(id -u)/com.$USER.devin-2api                      # 停止
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.$USER.devin-2api.plist  # 重新加载
tail -f logs/stderr.log                                                       # 进程日志
```

### 可选增强

- **config 改动自动重启**：plist 加 `WatchPaths` 指向配置目录的 `config.yaml`，保存即触发重启。代价是任何 mtime 变化（包括编辑器误触、`deploy.sh` 的同步）都会重启。
- **新编译二进制立刻 kickstart 的注意**：`go build` 覆盖二进制后立刻 kickstart，dyld 可能卡在 Gatekeeper 检查（进程 `S` 态、无监听、无日志）。`sample <pid>` 看栈确认后 `kill -9` 等 KeepAlive 重拉即可；稳妥做法是先 build 再停旧进程。

## Linux（systemd --user）

`scripts/deploy/deploy-linux.sh` 生成的 unit（`~/.config/systemd/user/devin-2api.service`）：

```ini
[Unit]
Description=devin-2api — OpenAI/Anthropic-compatible proxy for Devin
After=network-online.target

[Service]
ExecStart=~/.local/bin/devin-2api -config ~/.config/devin-2api/config.yaml -state-dir ~/.local/state/devin-2api
WorkingDirectory=~/.local/state/devin-2api
Environment=DEVIN2API_REUSEPORT=1
Environment=DEVIN2API_MANAGED=1
Restart=always
RestartSec=5
TimeoutStopSec=660
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=~/.local/state/devin-2api
StandardOutput=append:~/.local/state/devin-2api/logs/stdout.log
StandardError=append:~/.local/state/devin-2api/logs/stderr.log

[Install]
WantedBy=default.target
```

与 macOS 版的对应关系：`Restart=always` + `RestartSec=5` ≈ `KeepAlive` + `ThrottleInterval`，`TimeoutStopSec=660` ≈ `ExitTimeOut`，`Environment=` 两键 ≈ `EnvironmentVariables`（`DEVIN2API_MANAGED` 在 systemd 下本有 `INVOCATION_ID` 作证，显式声明让服务定义自描述），stdout/stderr 同样落状态目录文件（不走 journal，排障路径与 macOS 一致）。注意 systemd `--user` 上下文里 `XDG_CONFIG_HOME`/`XDG_STATE_HOME` 通常不设，unit 一律用部署期展开的绝对路径。

stderr/stdout 轮转与 macOS 同脚本同语义：`deploy-linux.sh` 安装 `~/.local/bin/devin-2api-logrotate` 并生成 `devin-2api-logrotate.service`（oneshot）+ `devin-2api-logrotate.timer`（`OnCalendar=daily`，`Persistent=true`）两个 unit，`--uninstall` 一并移除。

常用命令：

```bash
systemctl --user status devin-2api          # 状态（MainPID、内存）
systemctl --user restart devin-2api         # 重启（SIGTERM → TimeoutStopSec=660 排空窗口后强杀）
systemctl --user stop devin-2api            # 停止
journalctl --user -u devin-2api -f          # unit 事件日志（slog 在 logs/stderr.log）
tail -f logs/stderr.log                     # 进程日志
```

两个 Linux 特有注意点：

- **user manager 生命周期**：`systemctl --user` 服务默认随最后一个登录会话退出。要未登录也常驻：`loginctl enable-linger $USER`（免 root，部分发行版经 polkit 弹授权；`deploy-linux.sh` 每次跑完会探测并提示）。
- **无 systemd 的环境**（容器、WSL1 等）：直接前台跑二进制，等价于 Windows 模式。

## Windows（裸进程）

不做服务化：`devin-2api.exe` 前台启动，Ctrl+C 触发与其它平台相同的优雅排空（SIGTERM 路径）；关窗、`taskkill /F`、`Stop-Process` 都是强杀。部署布局按 Microsoft 惯例拆开：exe 在 `%LOCALAPPDATA%\Programs\devin-2api`，`config.yaml` 在 `%APPDATA%\devin-2api`（roaming），`logs\` 在 `%LOCALAPPDATA%\devin-2api`（machine-local）。不用部署脚本直接跑 zip 里的 exe 也可以——`./config.yaml` 存在即被选中（解析链见上），但状态目录仍回落 `%LOCALAPPDATA%\devin-2api`。release zip 内含 exe + `config.example.yaml` + LICENSE。

`scripts/deploy/deploy-windows.ps1` 与 bash 版同语义：`-Release latest` 下载 zip 校验 sha256、缺失时生成 config.yaml（随机 `dashboard.password`、`127.0.0.1`+空闲端口、交互粘贴 token；下游 /v1 令牌不入配置，到面板 /web/tokens.html 创建）、独立控制台窗口启动、healthz + `/v1/models` 冒烟；`-Check`/`-Uninstall`/`-NoStart`/`-Force`（允许强杀运行中实例，等价关窗）/`-RuntimeDir`（覆盖 exe 安装目录；配置/状态目录由 `DEVIN2API_CONFIG_DIR`/`DEVIN2API_STATE_DIR` env 覆盖）。旧版「exe 同目录放 config/logs」布局由 `Move-LegacyLayout` 自动迁移。经 SSH 远程执行时实例会随会话结束被系统回收——脚本面向本机交互会话。

## 面板与 agent 访问

`/web/index.html` 是人看板的入口；其下 API 同时面向 agent 程序化消费：`/admin/*` 只认面板密码 Bearer（admin 身份）、`/dashboard/*` 认两类 Bearer——面板密码 → admin、下游令牌 → api_token 只读身份（数据按该令牌收敛）、`/public/*` 公开。全程无 cookie——`POST /login` body `{mode:"admin"|"api_token", password|token}` 返回的 token 就是凭据本身。`dashboard.password` 非空时可直接 `Authorization: Bearer <面板密码>` 访问：

```bash
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/admin/runtime-metrics
curl -s -H 'Authorization: Bearer <password>' 'localhost:<port>/admin/logs?limit=20&status_code=500'
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/admin/active-requests
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/admin/debug-logs/<id>
```

`password` 为空时面板及 API 开放访问——本机自用可接受，暴露到局域网前务必配置。

面板是移植自 ccLoad（MIT）的唯一管理面。它管理的运行时状态都在 `devin-2api.db` 的三张表：`auth_tokens`（下游多 key：描述/过期/allowed_models/RPM 与 5h/日/周/月费用窗口及并发限额，是 /v1 准入的唯一判定源，只由面板管理、明文一次性出示；仓空（零行）时 /v1 开放准入，匿名通道行（空明文哈希）是无凭据流量的准入载体、至多一行）、`model_registry`（模型注册表：停用 → 404、redirect → 先注册表再 config 别名链）、`settings`（运行设置覆盖：`debug_log_enabled`/`debug_log_errors_only` 与 `log_retention_days`/`log_max_total_mb`/`log_payload_hours`/`log_keep_error_dirs`/`log_row_retention_days` 等日志保留策略，覆盖项在启动与 config reload 后重放、恒赢 config.yaml；`auto_refresh_interval_seconds` 仅前端消费）。

## 本机示例：作者的生产拓扑

本节是作者本机部署的拓扑形态（2026-09-18 起生效，地址用占位符），供对照参考，不是部署规范的一部分。

生产实例在 Linux 生产机本机：systemd `--user` 服务 `devin-2api.service` 监听 `:3033`，由 `scripts/deploy/deploy-linux.sh` 维护；原 Mac 生产实例已退役，`scripts/attic/deploy-remote.sh` 仅留档。各机 `:3003` 端点由转发 shim 兜住继续可用（旧 Mac launchd `com.devin2api.forwarder` → `<tailnet-ip>:3033`；生产机 systemd --user `devin-2api-compat-3003.service` → `127.0.0.1:3033`；脚本与 unit 模板见 `scripts/deploy/compat-forwarder/`），下游客户端无需改动。其它机器经 tailnet `http://<tailnet-ip>:3033` 访问该实例。
