# macOS 部署（launchd）

本机主实例由 launchd 用户代理管理，Label `com.devinuser.devin-2api`，
plist 位于 `~/Library/LaunchAgents/com.devinuser.devin-2api.plist`。
本文说明该配置的含义、日常管理命令、升级流程和可选项。重启、换二进制
前先确认目标端口上没有遗留测试进程（`lsof -nP -iTCP:<port> -sTCP:LISTEN`）。

## 进程模型

```
launchd (gui/<uid> 用户域, 无需 sudo)
  └─ devin-2api -config .../config.yaml   监听 :3003
       ├─ config.yaml 同目录 logs/         请求级 debug 目录 + index.jsonl
       ├─ logs/stdout.log                 面板渲染等 fmt 输出
       └─ logs/stderr.log                 slog 结构化进程日志
```

- 进程实现 `SIGTERM` 优雅退出（`signal.NotifyContext`）：停服会先 flush
  日志索引、排空异步写队列，再退出。`ExitTimeOut=60` 给了充足余量。
- 请求级 debug 日志的生命周期由 `debug.retention_days` /
  `debug.max_total_mb` 自管；launchd 侧无需额外配置。

## 当前 plist

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>com.devinuser.devin-2api</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/devinuser/Desktop/src/devin-2api/devin-2api</string>
		<string>-config</string>
		<string>/Users/devinuser/Desktop/src/devin-2api/config.yaml</string>
	</array>
	<key>WorkingDirectory</key><string>/Users/devinuser/Desktop/src/devin-2api</string>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><true/>
	<key>ThrottleInterval</key><integer>5</integer>
	<key>ExitTimeOut</key><integer>60</integer>
	<key>StandardOutPath</key><string>/Users/devinuser/Desktop/src/devin-2api/logs/stdout.log</string>
	<key>StandardErrorPath</key><string>/Users/devinuser/Desktop/src/devin-2api/logs/stderr.log</string>
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
/Users/devinuser/Desktop/src/devin-2api/logs/stderr.log devinuser:staff 644 5 10240 * J
```

含义：超 10MB 轮转、保留 5 份、bzip2 压缩（`J`）。`stdout.log` 同理可加。

## 常用命令

```bash
launchctl print gui/$(id -u)/com.devinuser.devin-2api | grep -E 'state|pid'   # 状态
launchctl kickstart -k gui/$(id -u)/com.devinuser.devin-2api                 # 重启（发 SIGTERM 再拉起）
launchctl bootout gui/$(id -u)/com.devinuser.devin-2api                      # 停止
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.devinuser.devin-2api.plist  # 重新加载
tail -f logs/stderr.log                                                       # 进程日志
```

## 升级流程

```bash
scripts/deploy.sh                # 构建 → 替换二进制 → kickstart → 校验 healthz 版本
scripts/deploy.sh --no-restart   # 只构建替换，不重启
```

脚本做四件事：以 `git describe --tags --always --dirty` 注入
`main.version` 构建新二进制、`-version` 自检、原地替换、kickstart 后轮询
`/healthz` 确认线上版本与刚构建的一致（不一致说明端口被其它实例抢占）。
launchd 发 SIGTERM 后进程优雅退出立即拉起，停机约一秒。`git describe`
输出形如 `f43a8f7`（无 tag 时的短 SHA）或 `v0.1.0-3-gabc1234`（tag 之后
第 3 个提交），工作区有未提交改动带 `-dirty` 后缀。

冒烟验证**不要用 :3003/:3004**——用空闲端口起临时二进制，验证完再决定
替换（这两个端口曾有旧构建残留导致误判的历史）。

## 可选增强

- **config 改动自动重启**：plist 加 `WatchPaths` 指向 `config.yaml`，保存即
  触发重启。代价是任何 mtime 变化（包括编辑器误触）都会重启。
- **多实例**：side 测试实例（如 :3004）若要常态化，用不同 Label + 不同
  config 另起一个 plist；不要放 `/tmp` 裸跑，`/tmp` 重启即丢。
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
