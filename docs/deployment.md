# 部署（launchd / systemd / 裸进程）

三平台拓扑——各平台按自己的目录规范分家（二进制 / 配置 / 状态日志三类不再同居一个运行目录）：

| 平台    | 托管方式                                   | 二进制                                              | 配置                                                   | 状态/日志                                                                | 部署命令                     |
| ------- | ------------------------------------------ | --------------------------------------------------- | ------------------------------------------------------ | ------------------------------------------------------------------------ | ---------------------------- |
| macOS   | launchd 用户代理 `com.$USER.devin-2api`    | `~/.local/bin/devin-2api`                           | `~/Library/Application Support/devin-2api/config.yaml` | 同配置目录（`logs/` 子目录；macOS 无独立 state 惯例，维持 app 目录模型） | `scripts/deploy.sh`          |
| Linux   | systemd `--user` unit `devin-2api.service` | `~/.local/bin/devin-2api`                           | `${XDG_CONFIG_HOME:-~/.config}/devin-2api/config.yaml` | `${XDG_STATE_HOME:-~/.local/state}/devin-2api`                           | `scripts/deploy-linux.sh`    |
| Windows | 无服务化，裸 exe 前台跑                    | `%LOCALAPPDATA%\Programs\devin-2api\devin-2api.exe` | `%APPDATA%\devin-2api\config.yaml`                     | `%LOCALAPPDATA%\devin-2api`                                              | `scripts/deploy-windows.ps1` |

二进制的路径解析链（服务定义里全部显式传 flag，链只对裸跑生效）：配置文件 `-config` flag → `DEVIN2API_CONFIG` env → `./config.yaml`（存在才选，仓库开发/Windows 解压即跑）→ 上表平台默认；状态目录 `-state-dir` flag → `DEVIN2API_STATE_DIR` env → 上表平台默认。启动日志 `paths resolved` 一行打出实际生效的两个路径。

三个 deploy 脚本（macOS/Linux 共用 `scripts/lib-deploy.sh`）参数语义一致：`--release <tag|latest>` 装预编译二进制（sha256 校验）、`--no-restart` 只替换不重启、`--check` 对比 已安装/运行中/最新 release 版本、`--uninstall` 停用并移除服务与二进制（保留 config/logs）。服务未安装时首装自动生成服务定义并拉起；`config.yaml` 缺失时从 `config.example.yaml` 生成（随机 `auth.api_key`/`dashboard.password`，tty 下提示粘贴 token）。开工前的 preflight 拦截 sudo、缺依赖、占位 token、端口冲突；`/healthz` 版本对上后再打一发 `/v1/models` 验证上游鉴权。最小安装路径：clone 仓库 → `deploy*.sh --release latest`。

另有开发机侧的远程驱动 `scripts/deploy-remote.sh`：免密 SSH 到部署目标（`DEVIN2API_HOST`，本机示例 `fht-mba`）执行 `deploy.sh`——默认 worktree 模式把 git 视角的本地工作树（含未提交改动）连同 `.git` 推流到远端暂存目录构建部署，`config.yaml` 不进 tar，复制远端在跑实例的 live 配置（`DEVIN2API_CONFIG_LIVE`，默认 `~/Library/Application Support/devin-2api/config.yaml`）；`--ref`/`--release` 部署已推送状态或预编译资产，`--check` 并排对比两端实例版本。部署后的验证步骤（healthz 版本确认 + 面板套件冒烟）见 `post-deploy-verify.md`。

## 跨平台共同约定

- **logs/ 永远在状态目录下**：请求级 debug 目录、`index.jsonl`、`quota.jsonl`、`gate-state.json` 都在 `<state-dir>/logs/` 下；`stdout.log`/`stderr.log` 是进程输出。仓库里的 `logs/` 只是指向本机状态目录的符号链接（开发便利，非必需）。旧版「单运行目录」布局由部署脚本自动迁移（`migrate_legacy_runtime`）：config/logs 挪到平台目录、删旧二进制，目标已存在时不覆盖。
- **优雅排空是硬要求**：进程实现 `SIGTERM` 优雅退出（`signal.NotifyContext`）——收到信号进入 draining：`/healthz` 继续应答但带 `draining: true`，新的 `/v1/*` 立即 `503 + Retry-After: 1`，在途请求跑完；排空上限 600s，超时强关剩余连接。两个服务定义都给 660s 停止超时覆盖该上限加余量。重启只发 SIGTERM，禁用 `kill -9` 抢时间（Ctrl+C 在 Windows 前台触发同一套排空）。listener 在排空期的行为取决于 `DEVIN2API_REUSEPORT`：未开启时保持打开（新请求拿应用层 503 而非内核拒绝）；开启时立即关闭——reuseport 组内新连接按绑定序（macOS）或哈希（Linux）落到组内其它 socket，旧实例只有让出监听，deploy 预置的交接进程才能接管。排空起点同时 `SetKeepAlivesEnabled(false)`：drain 前已 accept 的 keep-alive 连接关 listener 管不着，会一直被钉在旧实例上整窗吃 503；关掉 keep-alive 后这些连接在下一个响应带 `Connection: close` 收尾，客户端重连即落到接替者——陈旧连接最多吃一次 503。
- **重叠交接部署（reuseport handoff）**：`deploy.sh`/`deploy-linux.sh` 的重启路径是「先起交接进程 → 重启托管实例 → 等托管新实例拉起 → 退交接进程」。交接进程是同一二进制的临时副本，带 `DEVIN2API_REUSEPORT=1` 绑定同一端口入队；旧实例 drain 起点即关闭 listener 后它接管全部新连接，直到 KeepAlive/Restart 拉起托管新实例后再 SIGTERM 退场。全程零 503、零拒绝，在途请求只受 600s 排空上限约束，也不再需要等空闲窗口。交接进程 pid 记录在 `<状态目录>/.handoff.pid`；部署中断残留时下次部署自动回收。回退路径：在跑的旧实例没有 reuseport env 时交接进程 bind 失败，自动退化经典「等空闲 + 重启」——每个失败分支都不劣于旧部署语义。服务定义变更（plist/unit 重写）走同一套交接：重启动词换成「载入新定义」的那个（macOS `bootout+bootstrap`，Linux `systemctl restart` 随已 daemon-reload 的新 unit 生效），交接桥照样盖住整段排空窗口。注意直接 `launchctl kickstart -k` 不走交接：reuseport 实例 drain 即关 listener，排空期新连接是 refused 而非 503（都失败，但拿不到 Retry-After）。
- **单实例**：托管器（KeepAlive/Restart=always）会与手动起的实例互抢监听端口，交替时全部在途流被掐。所有实例必须经托管器启停；冒烟验证用空闲端口起临时二进制，验证完立即关闭，不留常驻侧实例。
- **版本可见性**：`main.version` 由构建期 `-X` 注入（`git describe --tags --always --dirty` 或 tag 名），`stderr.log` 启动行、`/healthz`、`-version` flag 三处可查。部署后脚本轮询 `/healthz` 直到 version 等于刚部署的版本——排空期旧进程仍在应答旧版本，首次 200 不代表切换完成。
- 重启、换二进制前先确认目标端口上没有遗留测试进程（`lsof -nP -iTCP:<port> -sTCP:LISTEN`，Windows 用 `netstat -ano | findstr <port>`）。

## macOS（launchd）

```
launchd (gui/<uid> 用户域, 无需 sudo)
  └─ ~/.local/bin/devin-2api -config $RT/config.yaml -state-dir $RT   ($RT = ~/Library/Application Support/devin-2api)
       ├─ $RT/logs/                          请求级 debug 目录 + index.jsonl
       ├─ logs/stdout.log                 面板渲染等 fmt 输出
       └─ logs/stderr.log                 slog 结构化进程日志
```

**与仓库分离的平台目录**：launchd 拉起的进程对 TCC 保护目录（`~/Desktop`、`~/Documents` 等）的每次 `open()` 都会进入授权判定——未授权时内核挂起 syscall，表现为进程在 dyld/读 config 阶段永久卡死（授权还按 cdhash 记，每次重建二进制即失效）。`~/.local/bin` 与 `~/Library/Application Support` 都不受 TCC 保护：二进制入前者（可直接调用），配置与状态目录沿用后者不变（`os.UserConfigDir` 的 darwin 返回即 Application Support）。`config.yaml` 的权威副本是 `$RT` 里那份（live）：deploy-remote 各模式部署前把它刷进 staging 供 `deploy.sh` 预检读取，`install_binary` 只在 live 缺失时从仓库副本恢复、存在且不一致时保留 live 并告警。改配置直接编辑 `$RT/config.yaml` 后 `POST /admin/config/reload` 热应用；仅 `server.listen` 等冷键需 `launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api`。

请求级 debug 日志的生命周期由 `debug.retention_days` / `debug.max_total_mb` / `debug.payload_hours` / `debug.keep_error_dirs` 自管；launchd 侧无需额外配置。

### 当前 plist

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

| 键                     | 当前值                  | 说明                                                                                                                        |
| ---------------------- | ----------------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `RunAtLoad`            | true                    | 登录即启动                                                                                                                  |
| `KeepAlive`            | true                    | 任何退出都重拉——含 `bootout` 外的主动 `kill`。若想「干净退出不复活」，改为 `<dict><key>SuccessfulExit</key><false/></dict>` |
| `ThrottleInterval`     | 5                       | 崩溃循环时每 5 秒才重试，防止拉满 CPU                                                                                       |
| `ExitTimeOut`          | 660                     | SIGTERM 后最多等 660s 再 SIGKILL；覆盖二进制 600s 排空上限 + 退出余量                                                       |
| `EnvironmentVariables` | `DEVIN2API_REUSEPORT=1` | 注入 SO_REUSEPORT——重叠交接部署的前提；裸跑二进制没有它，仍会撞单实例端口冲突保护                                           |
| `StandardErrorPath`    | logs/stderr.log         | slog 输出落盘；轮转由配套 logrotate agent 每日执行，见下节                                                                  |

### stderr/stdout 日志轮转

debug 请求日志有 retention，但 `stderr.log`（slog 进程日志）与 `stdout.log` 只会增长。这两个文件的写 fd 归 launchd 持有、进程无法 reopen，rename 类轮转会让 fd 跟着旧 inode 走、新写全丢——所以轮转用 copytruncate：`scripts/rotate-logs.sh` 对超 50MB 的 `*.log` 复制后原地截断并 gzip，保留 `.1`–`.3.gz` 三代（截断瞬间并发写入的一行仍可能丢，是该语义的最小代价）。

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

`scripts/deploy-linux.sh` 生成的 unit（`~/.config/systemd/user/devin-2api.service`）：

```ini
[Unit]
Description=devin-2api — OpenAI/Anthropic-compatible proxy for Devin
After=network-online.target

[Service]
ExecStart=~/.local/bin/devin-2api -config ~/.config/devin-2api/config.yaml -state-dir ~/.local/state/devin-2api
WorkingDirectory=~/.local/state/devin-2api
Environment=DEVIN2API_REUSEPORT=1
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

与 macOS 版的对应关系：`Restart=always` + `RestartSec=5` ≈ `KeepAlive` + `ThrottleInterval`，`TimeoutStopSec=660` ≈ `ExitTimeOut`，`Environment=DEVIN2API_REUSEPORT=1` ≈ `EnvironmentVariables`，stdout/stderr 同样落状态目录文件（不走 journal，排障路径与 macOS 一致）。注意 systemd `--user` 上下文里 `XDG_CONFIG_HOME`/`XDG_STATE_HOME` 通常不设，unit 一律用部署期展开的绝对路径。

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

`scripts/deploy-windows.ps1` 与 bash 版同语义：`-Release latest` 下载 zip 校验 sha256、缺失时生成 config.yaml（随机 `auth.api_key`/`dashboard.password`、`127.0.0.1`+空闲端口、交互粘贴 token）、独立控制台窗口启动、healthz + `/v1/models` 冒烟；`-Check`/`-Uninstall`/`-NoStart`/`-Force`（允许强杀运行中实例，等价关窗）/`-RuntimeDir`（覆盖 exe 安装目录；配置/状态目录由 `DEVIN2API_CONFIG_DIR`/`DEVIN2API_STATE_DIR` env 覆盖）。旧版「exe 同目录放 config/logs」布局由 `Move-LegacyLayout` 自动迁移。经 SSH 远程执行时实例会随会话结束被系统回收——脚本面向本机交互会话。

## 面板与 agent 访问

`/web/index.html` 是人看板的入口；其下 API 同时面向 agent 程序化消费：`/admin/*` 只认面板密码 Bearer（admin 身份）、`/dashboard/*` 认两类 Bearer——面板密码 → admin、下游令牌 → api_token 只读身份（数据按该令牌收敛）、`/public/*` 公开。全程无 cookie——`POST /login` body `{mode:"admin"|"api_token", password|token}` 返回的 token 就是凭据本身。`dashboard.password` 非空时可直接 `Authorization: Bearer <面板密码>` 访问：

```bash
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/admin/runtime-metrics
curl -s -H 'Authorization: Bearer <password>' 'localhost:<port>/admin/logs?limit=20&status_code=500'
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/admin/active-requests
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/admin/debug-logs/<id>
```

`password` 为空时面板及 API 开放访问——本机自用可接受，暴露到局域网前务必配置。

面板是移植自 ccLoad（MIT）的唯一管理面。它带来的状态文件都落在状态目录根：`auth_tokens.json`（下游多 key：描述/过期/allowed_models/RPM 与 5h/日/周/月费用窗口及并发限额，是 /v1 准入的唯一判定源——`auth.api_key` 只是播种源，启动与 reload 时被写成一条普通令牌行；仓空（零行）时 /v1 开放准入，匿名通道行（空明文哈希）是无凭据流量的准入载体、至多一行）、`models.json`（模型注册表：停用 → 404、redirect → 先注册表再 config 别名链）、`panel-settings.json`（运行设置覆盖：`debug_log_enabled` 与 `log_retention_days`/`log_max_total_mb`/`log_payload_hours`/`log_keep_error_dirs` 等日志保留策略，覆盖项在启动与 config reload 后重放、恒赢 config.yaml；`auto_refresh_interval_seconds` 仅前端消费）。
