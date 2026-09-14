# 部署（launchd / systemd / 裸进程）

三平台拓扑：

| 平台    | 托管方式                                   | 运行目录（二进制 + config.yaml + logs/）      | 服务定义位置                                        | 部署命令                     |
| ------- | ------------------------------------------ | --------------------------------------------- | --------------------------------------------------- | ---------------------------- |
| macOS   | launchd 用户代理 `com.$USER.devin-2api`    | `~/Library/Application Support/devin-2api`    | `~/Library/LaunchAgents/com.$USER.devin-2api.plist` | `scripts/deploy.sh`          |
| Linux   | systemd `--user` unit `devin-2api.service` | `${XDG_DATA_HOME:-~/.local/share}/devin-2api` | `${XDG_CONFIG_HOME:-~/.config}/systemd/user/`       | `scripts/deploy-linux.sh`    |
| Windows | 无服务化，裸 exe 前台跑                    | `%LOCALAPPDATA%\Programs\devin-2api`          | —                                                   | `scripts/deploy-windows.ps1` |

三个 deploy 脚本（macOS/Linux 共用 `scripts/lib-deploy.sh`）参数语义一致：`--release <tag|latest>` 装预编译二进制（sha256 校验）、`--no-restart` 只替换不重启、`--check` 对比 已安装/运行中/最新 release 版本、`--uninstall` 停用并移除服务与二进制（保留 config/logs）。服务未安装时首装自动生成服务定义并拉起；`config.yaml` 缺失时从 `config.example.yaml` 生成（随机 `auth.api_key`/`dashboard.password`，tty 下提示粘贴 token）。开工前的 preflight 拦截 sudo、缺依赖、占位 token、端口冲突；`/healthz` 版本对上后再打一发 `/v1/models` 验证上游鉴权。最小安装路径：clone 仓库 → `deploy*.sh --release latest`。

## 跨平台共同约定

- **logs/ 永远在 config.yaml 同目录**：请求级 debug 目录、`index.jsonl`、`quota.jsonl` 都在运行目录的 `logs/` 下；`stdout.log`/`stderr.log` 是进程输出。仓库里的 `logs/` 只是指向本机运行目录的符号链接（开发便利，非必需）。
- **优雅排空是硬要求**：进程实现 `SIGTERM` 优雅退出（`signal.NotifyContext`）——收到信号进入 draining：`/healthz` 继续应答但带 `draining: true`，新的 `/v1/*` 立即 `503 + Retry-After: 1`，在途请求跑完；排空上限 300s，超时强关剩余连接。两个服务定义都给 330s 停止超时覆盖该上限加余量。重启只发 SIGTERM，禁用 `kill -9` 抢时间（Ctrl+C 在 Windows 前台触发同一套排空）。listener 在排空期的行为取决于 `DEVIN2API_REUSEPORT`：未开启时保持打开（新请求拿应用层 503 而非内核拒绝）；开启时立即关闭——reuseport 组内新连接按绑定序（macOS）或哈希（Linux）落到组内其它 socket，旧实例只有让出监听，deploy 预置的交接进程才能接管。
- **重叠交接部署（reuseport handoff）**：`deploy.sh`/`deploy-linux.sh` 的重启路径是「先起交接进程 → 重启托管实例 → 等托管新实例拉起 → 退交接进程」。交接进程是同一二进制的临时副本，带 `DEVIN2API_REUSEPORT=1` 绑定同一端口入队；旧实例 drain 起点即关闭 listener 后它接管全部新连接，直到 KeepAlive/Restart 拉起托管新实例后再 SIGTERM 退场。全程零 503、零拒绝，在途请求只受 300s 排空上限约束，也不再需要等空闲窗口。交接进程 pid 记录在 `<运行目录>/.handoff.pid`；部署中断残留时下次部署自动回收。回退路径：在跑的旧实例没有 reuseport env 时交接进程 bind 失败，自动退化经典「等空闲 + 重启」——每个失败分支都不劣于旧部署语义。注意直接 `launchctl kickstart -k` 不走交接：reuseport 实例 drain 即关 listener，排空期新连接是 refused 而非 503（都失败，但拿不到 Retry-After）。
- **单实例**：托管器（KeepAlive/Restart=always）会与手动起的实例互抢监听端口，交替时全部在途流被掐。所有实例必须经托管器启停；冒烟验证用空闲端口起临时二进制，验证完立即关闭，不留常驻侧实例。
- **版本可见性**：`main.version` 由构建期 `-X` 注入（`git describe --tags --always --dirty` 或 tag 名），`stderr.log` 启动行、`/healthz`、`-version` flag 三处可查。部署后脚本轮询 `/healthz` 直到 version 等于刚部署的版本——排空期旧进程仍在应答旧版本，首次 200 不代表切换完成。
- 重启、换二进制前先确认目标端口上没有遗留测试进程（`lsof -nP -iTCP:<port> -sTCP:LISTEN`，Windows 用 `netstat -ano | findstr <port>`）。

## macOS（launchd）

```
launchd (gui/<uid> 用户域, 无需 sudo)
  └─ devin-2api -config $RT/config.yaml   ($RT = ~/Library/Application Support/devin-2api)
       ├─ config.yaml 同目录 logs/         请求级 debug 目录 + index.jsonl
       ├─ logs/stdout.log                 面板渲染等 fmt 输出
       └─ logs/stderr.log                 slog 结构化进程日志
```

**运行目录与仓库分离**：launchd 拉起的进程对 TCC 保护目录（`~/Desktop`、`~/Documents` 等）的每次 `open()` 都会进入授权判定——未授权时内核挂起 syscall，表现为进程在 dyld/读 config 阶段永久卡死（授权还按 cdhash 记，每次重建二进制即失效）。因此二进制、`config.yaml`、`logs/` 都放在 `~/Library/Application Support/devin-2api/`（不受 TCC 保护）。`config.yaml` 的权威副本仍是仓库里那份，`deploy.sh` 每次部署同步到运行目录；单改配置可 `cp config.yaml "$RT/" && launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api`。

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
		<string>/Users/<user>/Library/Application Support/devin-2api/devin-2api</string>
		<string>-config</string>
		<string>/Users/<user>/Library/Application Support/devin-2api/config.yaml</string>
	</array>
	<key>WorkingDirectory</key><string>/Users/<user>/Library/Application Support/devin-2api</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>DEVIN2API_REUSEPORT</key><string>1</string>
	</dict>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>ExitTimeOut</key><integer>330</integer>
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
| `ExitTimeOut`          | 330                     | SIGTERM 后最多等 330s 再 SIGKILL；覆盖二进制 300s 排空上限 + 退出余量                                                       |
| `EnvironmentVariables` | `DEVIN2API_REUSEPORT=1` | 注入 SO_REUSEPORT——重叠交接部署的前提；裸跑二进制没有它，仍会撞单实例端口冲突保护                                           |
| `StandardErrorPath`    | logs/stderr.log         | slog 输出落盘；**没有轮转**，见下节                                                                                         |

### stderr 日志轮转（可选）

debug 请求日志有 retention，但 `stderr.log`（slog 进程日志）只会增长。用系统自带 newsyslog 管即可，`/etc/newsyslog.d/devin-2api.conf`（需 sudo）：

```
~/Library/Application\ Support/devin-2api/logs/stderr.log $USER:staff 644 5 10240 * J
```

含义：超 10MB 轮转、保留 5 份、bzip2 压缩（`J`）。`stdout.log` 同理可加。

### 常用命令

```bash
launchctl print gui/$(id -u)/com.$USER.devin-2api | grep -E 'state|pid'   # 状态
launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api                 # 重启（发 SIGTERM 再拉起）
launchctl bootout gui/$(id -u)/com.$USER.devin-2api                      # 停止
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.$USER.devin-2api.plist  # 重新加载
tail -f logs/stderr.log                                                       # 进程日志
```

### 可选增强

- **config 改动自动重启**：plist 加 `WatchPaths` 指向运行目录的 `config.yaml`，保存即触发重启。代价是任何 mtime 变化（包括编辑器误触、`deploy.sh` 的同步）都会重启。
- **新编译二进制立刻 kickstart 的注意**：`go build` 覆盖二进制后立刻 kickstart，dyld 可能卡在 Gatekeeper 检查（进程 `S` 态、无监听、无日志）。`sample <pid>` 看栈确认后 `kill -9` 等 KeepAlive 重拉即可；稳妥做法是先 build 再停旧进程。

## Linux（systemd --user）

`scripts/deploy-linux.sh` 生成的 unit（`~/.config/systemd/user/devin-2api.service`）：

```ini
[Unit]
Description=devin-2api — OpenAI/Anthropic-compatible proxy for Devin
After=network-online.target

[Service]
ExecStart=<运行目录>/devin-2api -config <运行目录>/config.yaml
WorkingDirectory=<运行目录>
Environment=DEVIN2API_REUSEPORT=1
Restart=always
RestartSec=5
TimeoutStopSec=330
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ReadWritePaths=<运行目录>
StandardOutput=append:<运行目录>/logs/stdout.log
StandardError=append:<运行目录>/logs/stderr.log

[Install]
WantedBy=default.target
```

与 macOS 版的对应关系：`Restart=always` + `RestartSec=5` ≈ `KeepAlive` + `ThrottleInterval`，`TimeoutStopSec=330` ≈ `ExitTimeOut`，`Environment=DEVIN2API_REUSEPORT=1` ≈ `EnvironmentVariables`，stdout/stderr 同样落运行目录文件（不走 journal，排障路径与 macOS 一致）。

常用命令：

```bash
systemctl --user status devin-2api          # 状态（MainPID、内存）
systemctl --user restart devin-2api         # 重启（SIGTERM → 60s 内强杀）
systemctl --user stop devin-2api            # 停止
journalctl --user -u devin-2api -f          # unit 事件日志（slog 在 logs/stderr.log）
tail -f logs/stderr.log                     # 进程日志
```

两个 Linux 特有注意点：

- **user manager 生命周期**：`systemctl --user` 服务默认随最后一个登录会话退出。要未登录也常驻：`loginctl enable-linger $USER`（免 root，部分发行版经 polkit 弹授权；`deploy-linux.sh` 每次跑完会探测并提示）。
- **无 systemd 的环境**（容器、WSL1 等）：直接前台跑二进制，等价于 Windows 模式。

## Windows（裸进程）

不做服务化：`devin-2api.exe` 与 `config.yaml` 放同一目录，前台启动。Ctrl+C 触发与其它平台相同的优雅排空（SIGTERM 路径）；关窗、`taskkill /F`、`Stop-Process` 都是强杀。`logs/` 落在 config.yaml 同目录。release zip 内含 exe + `config.example.yaml` + LICENSE。

`scripts/deploy-windows.ps1` 与 bash 版同语义：`-Release latest` 下载 zip 校验 sha256、缺失时生成 config.yaml（随机 `auth.api_key`/`dashboard.password`、`127.0.0.1`+空闲端口、交互粘贴 token）、独立控制台窗口启动、healthz + `/v1/models` 冒烟；`-Check`/`-Uninstall`/`-NoStart`/`-Force`（允许强杀运行中实例，等价关窗）/`-RuntimeDir`。经 SSH 远程执行时实例会随会话结束被系统回收——脚本面向本机交互会话。

## 面板与 agent 访问

`/panel` 是人看板的入口；其下 API 同时面向 agent 程序化消费。`dashboard.password` 非空时除 cookie 登录外，可直接 `Authorization: Bearer <面板密码>` 访问（免去 cookie 交互）：

```bash
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/panel/api/stats
curl -s -H 'Authorization: Bearer <password>' 'localhost:<port>/panel/api/requests?limit=20&q=failed'
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/panel/api/requests/active
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/panel/api/requests/<dir>
curl -s -H 'Authorization: Bearer <password>' localhost:<port>/panel/api/requests/<dir>/file/04-devin-response.jsonl
```

`password` 为空时面板及 API 开放访问——本机自用可接受，暴露到局域网前务必配置。
