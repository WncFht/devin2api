# macOS 部署（launchd）

本机主实例由 launchd 用户代理管理，Label `com.$USER.devin-2api`，
plist 位于 `~/Library/LaunchAgents/com.$USER.devin-2api.plist`。
本文说明该配置的含义、日常管理命令、升级流程和可选项。重启、换二进制
前先确认目标端口上没有遗留测试进程（`lsof -nP -iTCP:<port> -sTCP:LISTEN`）。

## 进程模型

```
launchd (gui/<uid> 用户域, 无需 sudo)
  └─ devin-2api -config $RT/config.yaml   监听 :3003   ($RT = ~/Library/Application Support/devin-2api)
       ├─ config.yaml 同目录 logs/         请求级 debug 目录 + index.jsonl
       ├─ logs/stdout.log                 面板渲染等 fmt 输出
       └─ logs/stderr.log                 slog 结构化进程日志
```

**运行目录与仓库分离**：仓库在 `~/Desktop` 下，而 launchd 拉起的进程对
Desktop 的每次 `open()` 都会进入 TCC「桌面文件夹」授权判定——未授权时
内核挂起 syscall，表现为进程在 dyld/读 config 阶段永久卡死（授权还按
cdhash 记，每次重建二进制即失效）。因此二进制、`config.yaml`、`logs/`
都放在 `~/Library/Application Support/devin-2api/`（不受 TCC 保护）；
仓库里的 `logs/` 是指向该目录的符号链接，`logs/<dir>/`、`index.jsonl`
等排障路径照旧可用。`config.yaml` 的权威副本仍是仓库里那份，
`deploy.sh` 每次部署同步到运行目录；单改配置可
`cp config.yaml "$RT/" && launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api`。

- 进程实现 `SIGTERM` 优雅重启（`signal.NotifyContext`）：收到信号后进入
  draining——监听器保持打开，`/healthz` 继续应答但带 `draining: true`，
  新的 `/v1/*` 请求立即得到 `503 + Retry-After: 1`（客户端可重试），
  已在途的请求继续跑完；排空上限 50s，超时强关剩余连接再退出。
  `ExitTimeOut=60` 覆盖排空上限加余量。
- 请求级 debug 日志的生命周期由 `debug.retention_days` /
  `debug.max_total_mb` / `debug.payload_hours` / `debug.keep_error_dirs`
  自管；launchd 侧无需额外配置。

## 当前 plist

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
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>ExitTimeOut</key><integer>60</integer>
	<key>StandardOutPath</key><string>/Users/<user>/Library/Application Support/devin-2api/logs/stdout.log</string>
	<key>StandardErrorPath</key><string>/Users/<user>/Library/Application Support/devin-2api/logs/stderr.log</string>
</dict>
</plist>
```

各键的含义与取舍：

| 键                  | 当前值          | 说明                                                                                                                        |
| ------------------- | --------------- | --------------------------------------------------------------------------------------------------------------------------- |
| `RunAtLoad`         | true            | 登录即启动                                                                                                                  |
| `KeepAlive`         | true            | 任何退出都重拉——含 `bootout` 外的主动 `kill`。若想「干净退出不复活」，改为 `<dict><key>SuccessfulExit</key><false/></dict>` |
| `ThrottleInterval`  | 5               | 崩溃循环时每 5 秒才重试，防止拉满 CPU                                                                                       |
| `ExitTimeOut`       | 60              | SIGTERM 后最多等 60s 再 SIGKILL；默认 20s 也够                                                                              |
| `StandardErrorPath` | logs/stderr.log | slog 输出落盘；**没有轮转**，见下节                                                                                         |

## stderr 日志轮转（可选）

debug 请求日志有 retention，但 `stderr.log`（slog 进程日志）只会增长。
用系统自带 newsyslog 管即可，`/etc/newsyslog.d/devin-2api.conf`（需 sudo）：

```
~/Library/Application\ Support/devin-2api/logs/stderr.log $USER:staff 644 5 10240 * J
```

含义：超 10MB 轮转、保留 5 份、bzip2 压缩（`J`）。`stdout.log` 同理可加。

## 常用命令

```bash
launchctl print gui/$(id -u)/com.$USER.devin-2api | grep -E 'state|pid'   # 状态
launchctl kickstart -k gui/$(id -u)/com.$USER.devin-2api                 # 重启（发 SIGTERM 再拉起）
launchctl bootout gui/$(id -u)/com.$USER.devin-2api                      # 停止
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.$USER.devin-2api.plist  # 重新加载
tail -f logs/stderr.log                                                       # 进程日志
```

## 首装与升级流程

```bash
scripts/deploy.sh                    # 构建 → 替换二进制 → kickstart → 校验 healthz 版本
scripts/deploy.sh --no-restart       # 只构建替换，不重启
scripts/deploy.sh --release v0.2.0   # 下载 GitHub Release 预编译二进制（sha256 校验）后替换
scripts/deploy.sh --release latest   # 同上，装最新 release
scripts/deploy.sh --check            # 对比 已安装/运行中/最新 release 版本，与 latest 不一致时 exit 1
```

首装不需要手工处理 launchd：服务未加载时 `deploy.sh` 会按本文「当前 plist」
一节的内容生成 `~/Library/LaunchAgents/com.$USER.devin-2api.plist` 并
`launchctl bootstrap`（刚 bootstrap 的进程直接跑新二进制，跳过 redundant
kickstart）。因此新机器的最小安装路径是：同步仓库（含 `config.yaml`）→
`scripts/deploy.sh --release latest`。

脚本做四件事：以 `git describe --tags --always --dirty` 注入
`main.version` 构建新二进制、`-version` 自检、安装到运行目录并同步
`config.yaml`、kickstart 后轮询 `/healthz` 直到 **version 字段等于刚构建
的版本**——排空期旧进程仍在应答旧版本，首次 200 不代表切换完成（版本一直
不变说明端口被其它实例抢占）。排空在途请求期间 `healthz`/`/v1/*` 全程可达，
新连接只会短暂收到 `503 + Retry-After`，无 connection refused 窗口；
真正无进程应答的空窗是「旧进程排空退出 → launchd 拉起新进程」之间。
`git describe` 输出形如 `f43a8f7`（无 tag 时的短 SHA）或
`v0.1.0-3-gabc1234`（tag 之后第 3 个提交），工作区有未提交改动带
`-dirty` 后缀。

冒烟验证**不要用 :3003/:3004**——用空闲端口起临时二进制，验证完再决定
替换（这两个端口曾有旧构建残留导致误判的历史）。

## 可选增强

- **config 改动自动重启**：plist 加 `WatchPaths` 指向运行目录的
  `config.yaml`，保存即触发重启。代价是任何 mtime 变化（包括编辑器误触、
  `deploy.sh` 的同步）都会重启。
- **单实例约定**：本机只维护这一个实例。冒烟验证用空闲端口（如 :3005）
  起临时二进制，验证完立即 `kill <pid>`（SIGTERM 会走优雅退出）；不留
  常驻 side 实例，也不要手动占 :3003/:3004——与 KeepAlive 互抢端口时
  全部在途流都会被掐。
- **版本可见性**：已实现——`main.version` 由构建期 `-X` 注入（见升级命令），
  `stderr.log` 启动行、`/healthz`、`-version` flag 三处可查。

## 面板与 agent 访问

`/panel` 是人看板的入口；其下 API 同时面向 agent 程序化消费。
`dashboard.password` 非空时除 cookie 登录外，可直接
`Authorization: Bearer <面板密码>` 访问（免去 cookie 交互）：

```bash
curl -s -H 'Authorization: Bearer <password>' localhost:3003/panel/api/stats
curl -s -H 'Authorization: Bearer <password>' 'localhost:3003/panel/api/requests?limit=20&q=failed'
curl -s -H 'Authorization: Bearer <password>' localhost:3003/panel/api/requests/active
curl -s -H 'Authorization: Bearer <password>' localhost:3003/panel/api/requests/<dir>
curl -s -H 'Authorization: Bearer <password>' localhost:3003/panel/api/requests/<dir>/file/04-devin-response.jsonl
```

`password` 为空时面板及 API 开放访问——本机自用可接受，暴露到局域网前
务必配置。
